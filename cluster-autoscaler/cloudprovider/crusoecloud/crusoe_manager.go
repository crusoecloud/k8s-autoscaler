/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package crusoecloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"

	"k8s.io/klog/v2"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
)

const (
	scaleToZeroSupported = true
	instanceBatchSize    = 50 // page instance fetch by this size
)

type crusoeCloudConfig struct {
	// APIEndpoint is the HTTP API URL
	APIEndpoint string `json:"api_endpoint"`
	// AccessKey is an API access key
	AccessKey string `json:"access_key"`
	// SecretKey is an API secret key
	SecretKey string `json:"secret_key"`
	// ProjectID is the project id containing the CMK cluster.
	ProjectID string `json:"project_id"`
	// ClusterID is the CMK cluster id where the Autoscaler is running.
	ClusterID string `json:"cluster_id"`
}

func readConf(config *crusoeCloudConfig, configFile io.Reader) error {
	body, err := io.ReadAll(configFile)
	if err != nil {
		return err
	}
	err = json.Unmarshal(body, config)
	return err
}

type crusoeManager struct {
	// Client talks to Crusoecloud API
	client *crusoeapi.APIClient
	// ProjectID is the project id containing the CMK cluster.
	projectID string
	// ClusterID is the CMK cluster id where the Autoscaler is running.
	clusterID string
	// Configured set of node groups
	nodeGroupSpecs map[string]*dynamic.NodeGroupSpec
	// Current set of node groups
	nodeGroups []*crusoeNodeGroup
}

func newManager(configFile io.Reader, discoveryOpts cloudprovider.NodeGroupDiscoveryOptions, userAgent string) (*crusoeManager, error) {
	getenvOr := func(key, defaultValue string) string {
		value := os.Getenv(key)
		if value != "" {
			return value
		}
		return defaultValue
	}

	// Config file passed with `cloud-config` flag
	cfg := crusoeCloudConfig{}
	if configFile != nil {
		err := readConf(&cfg, configFile)
		if err != nil {
			klog.Errorf("failed to read/parse crusoecloud config file: %s", err)
		}
	}
	klog.V(4).Infof("parsed config file: %+v", cfg)

	// env takes precedence over config passed by command-line (TODO: probably don't need env for all of these)
	cfg.APIEndpoint = getenvOr("CRUSOE_API_URL", cfg.APIEndpoint)
	cfg.AccessKey = getenvOr("CRUSOE_ACCESS_KEY", cfg.AccessKey)
	cfg.SecretKey = getenvOr("CRUSOE_SECRET_KEY", cfg.SecretKey)
	cfg.ProjectID = getenvOr("CRUSOE_PROJECT_ID", cfg.ProjectID)
	cfg.ClusterID = getenvOr("CRUSOE_CLUSTER_ID", cfg.ClusterID)
	klog.V(4).Infof("parsed config vars: %+v", cfg)

	klog.V(4).Infof("CrusoeCloud Manager built; ProjectId=%s;ClusterId=%s,AccessKey=%s-***,ApiURL=%s",
		cfg.ProjectID, cfg.ClusterID, cfg.AccessKey[:8], cfg.APIEndpoint)

	ngSpecs := make(map[string]*dynamic.NodeGroupSpec)
	for _, nodeGroupSpec := range discoveryOpts.NodeGroupSpecs {
		spec, err := dynamic.SpecFromString(nodeGroupSpec, scaleToZeroSupported)
		if err != nil {
			klog.Fatalf("Could not parse node group spec %s: %v", nodeGroupSpec, err)
		}
		ngSpecs[spec.Name] = spec
	}

	client := NewAPIClient(cfg.APIEndpoint, cfg.AccessKey, cfg.SecretKey, userAgent)
	return &crusoeManager{
		client:         client,
		projectID:      cfg.ProjectID,
		clusterID:      cfg.ClusterID,
		nodeGroupSpecs: ngSpecs,
		nodeGroups:     []*crusoeNodeGroup{},
	}, nil
}

func (mgr *crusoeManager) NodeGroups() []*crusoeNodeGroup {
	return mgr.nodeGroups
}

func (mgr *crusoeManager) Refresh() error {
	ctx := context.Background()
	pools, err := mgr.ListNodePools(ctx)
	if err != nil {
		klog.Errorf("Refresh failed: %s", err)
		return err
	}
	klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s ListNodePools returns %d IDs",
		mgr.projectID, mgr.clusterID, len(pools))

	var ngs []*crusoeNodeGroup

	for _, p := range pools {
		// just in case
		if p.ClusterId != mgr.clusterID {
			klog.Warningf("Skipping unexpected nodepool %s for cluster %s (!= %s)",
				p.Id, p.ClusterId, mgr.clusterID)
			continue
		}

		ng := crusoeNodeGroup{
			manager: mgr,
			pool:    &p,
			nodes:   make(map[string]*crusoeapi.InstanceV1Alpha5),
			ngSpec:  mgr.nodeGroupSpecs[p.Name], // if empty, use defaults
		}

		for i := 0; i < len(p.InstanceIds); i += instanceBatchSize {
			end := i + instanceBatchSize
			if end > len(p.InstanceIds) {
				end = len(p.InstanceIds)
			}

			instances, err := mgr.ListVMInstances(ctx, p.InstanceIds[i:end])
			if err != nil {
				klog.Errorf("Refresh failed for nodepool %s: %s", p.Id, err)
				return err
			}
			klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s,NodepoolID=%s ListInstances returns %d->%d IDs",
				mgr.projectID, mgr.clusterID, p.Id, len(p.InstanceIds), len(instances))

			for _, instance := range instances {
				// TODO: change to index by providerID
				ng.nodes[instance.Name] = &instance
			}
		}
		ngs = append(ngs, &ng)
	}
	klog.V(4).Infof("Refresh,ClusterID=%s,%d pools found", mgr.clusterID, len(ngs))

	mgr.nodeGroups = ngs

	return nil
}

func (mgr *crusoeManager) ListNodePools(ctx context.Context) ([]crusoeapi.KubernetesNodePool, error) {
	resp, httpResp, err := mgr.client.KubernetesNodePoolsApi.ListNodePools(ctx, mgr.projectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(mgr.clusterID)})

	if err != nil {
		return nil, fmt.Errorf("failed to list pools for cluster %s: %w", mgr.clusterID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to list pools for cluster %s: http error %s",
			mgr.clusterID, httpResp.Status)
	}
	return resp.Items, nil
}

func (mgr *crusoeManager) GetNodePool(ctx context.Context, poolID string) (*crusoeapi.KubernetesNodePool, error) {
	resp, httpResp, err := mgr.client.KubernetesNodePoolsApi.GetNodePool(ctx, mgr.projectID, poolID)

	if err != nil {
		return nil, fmt.Errorf("failed to get nodepool %s: %w", poolID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to get nodepool %s: http error %s", poolID, httpResp.Status)
	}
	return &resp, nil
}

func (mgr *crusoeManager) UpdateNodePool(ctx context.Context, poolID string, targetSize int64) (*crusoeapi.Operation, error) {
	resp, httpResp, err := mgr.client.KubernetesNodePoolsApi.UpdateNodePool(ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{Count: targetSize},
		mgr.projectID, poolID)

	if err != nil {
		return nil, fmt.Errorf("failed to update node pool %s for cluster %s: %w", poolID, mgr.clusterID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to update node pool %s for cluster %s: http error %s",
			poolID, mgr.clusterID, httpResp.Status)
	}
	return resp.Operation, nil
}

func (mgr *crusoeManager) GetNodePoolOperation(ctx context.Context, opID string) (*crusoeapi.Operation, error) {
	resp, httpResp, err := mgr.client.KubernetesNodePoolOperationsApi.GetKubernetesNodePoolsOperation(ctx, mgr.projectID, opID)

	if err != nil {
		return nil, fmt.Errorf("failed to get nodepool operation %s: %w", opID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to get nodepool operation %s: http error %s", opID, httpResp.Status)
	}
	return &resp, nil
}

func (mgr *crusoeManager) ListVMInstances(ctx context.Context, instanceIds []string) ([]crusoeapi.InstanceV1Alpha5, error) {
	resp, httpResp, err := mgr.client.VMsApi.ListInstances(ctx, mgr.projectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString(strings.Join(instanceIds, ",")),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list instances for cluster %s: %w", mgr.clusterID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to list instances for cluster %s: http error %s",
			mgr.clusterID, httpResp.Status)
	}
	return resp.Items, nil
}

func (mgr *crusoeManager) DeleteVMInstance(ctx context.Context, instanceId string) (*crusoeapi.Operation, error) {
	resp, httpResp, err := mgr.client.VMsApi.DeleteInstance(ctx, mgr.projectID, instanceId)
	if err != nil {
		return nil, fmt.Errorf("failed to delete instance %s for cluster %s: %w", instanceId, mgr.clusterID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to delete instance %s for cluster %s: http error %s",
			instanceId, mgr.clusterID, httpResp.Status)
	}
	return resp.Operation, nil
}

func (mgr *crusoeManager) GetVMOperation(ctx context.Context, opID string) (*crusoeapi.Operation, error) {
	resp, httpResp, err := mgr.client.VMOperationsApi.GetComputeVMsInstancesOperation(ctx, mgr.projectID, opID)

	if err != nil {
		return nil, fmt.Errorf("failed to get VM operation %s: %w", opID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to get VM operation %s: http error %s", opID, httpResp.Status)
	}
	return &resp, nil
}
