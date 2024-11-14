/*
Copyright 2022 The Kubernetes Authors.

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

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	ca_errors "k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
	"k8s.io/klog/v2"
)

const (
	// GPULabel is the label added to GPU nodes
	GPULabel = "crusoe.ai/accelerator"
)

var (
	// derived from `crusoe compute vms types | tail +3 | cut -f1 -d. | grep -v "^[cs]1" | sort | uniq`
	availableGPUTypes = map[string]struct{}{
		"nvidia-a40":              {},
		"nvidia-a100":             {},
		"nvidia-a100-80gb":        {},
		"nvidia-a100-80gb-sxm":    {},
		"nvidia-a100-80gb-sxm-ib": {},
		"nvidia-h100-80gb-sxm":    {},
		"nvidia-h100-80gb-sxm-ib": {},
		// "nvidia-h200": {},
		"nvidia-l40s-48gb": {},
	}
)

type crusoeCloudProvider struct {
	// client talks to Crusoecloud API
	client *crusoeapi.APIClient
	// Region is the cloud region where the CMK cluster is located.
	region string
	// ProjectID is the project id containing the CMK cluster.
	projectID string
	// ClusterID is the CMK cluster id where the Autoscaler is running.
	clusterID string
	// nodeGroups is an abstraction around the NodePool object returned by the API.
	nodeGroups []*NodeGroup

	resourceLimiter *cloudprovider.ResourceLimiter
}

type crusoeCloudConfig struct {
	// APIEndpoint is the HTTP API URL
	APIEndpoint string `json:"api_endpoint"`
	// AccessKey is an API access key
	AccessKey string `json:"access_key"`
	// SecretKey is an API secret key
	SecretKey string `json:"secret_key"`
	// Region is the cloud region
	Region string `json:"region"`
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

func newCrusoeCloudProvider(configFile io.Reader, defaultUserAgent string, rl *cloudprovider.ResourceLimiter) *crusoeCloudProvider {
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

	// env takes precedence over config passed by command-line
	cfg.APIEndpoint = getenvOr("CRUSOE_API_URL", cfg.APIEndpoint)
	cfg.AccessKey = getenvOr("CRUSOE_ACCESS_KEY", cfg.AccessKey)
	cfg.SecretKey = getenvOr("CRUSOE_SECRET_KEY", cfg.SecretKey)
	cfg.Region = getenvOr("CRUSOE_REGION", cfg.Region)
	cfg.ProjectID = getenvOr("CRUSOE_PROJECT_ID", cfg.ProjectID)
	cfg.ClusterID = getenvOr("CRUSOE_CLUSTER_ID", cfg.ClusterID)
	klog.V(4).Infof("parsed config vars: %+v", cfg)

	client := NewAPIClient(cfg.APIEndpoint, cfg.AccessKey, cfg.SecretKey, defaultUserAgent)
	klog.V(4).Infof("Crusoe Cloud Provider built; ProjectId=%s;ClusterId=%s,AccessKey=%s-***,Region=%s,ApiURL=%s",
		cfg.ProjectID, cfg.ClusterID, cfg.AccessKey[:8], cfg.Region, cfg.APIEndpoint)

	return &crusoeCloudProvider{
		client:          client,
		region:          cfg.Region,
		projectID:       cfg.ProjectID,
		clusterID:       cfg.ClusterID,
		resourceLimiter: rl,
	}
}

// BuildCrusoeCloud returns CloudProvider implementation for CrusoeCloud.
func BuildCrusoeCloud(
	opts config.AutoscalingOptions,
	do cloudprovider.NodeGroupDiscoveryOptions,
	rl *cloudprovider.ResourceLimiter,
) cloudprovider.CloudProvider {
	var configFile io.Reader

	if opts.CloudConfig != "" {
		configFile, err := os.Open(opts.CloudConfig)

		if err != nil {
			klog.Errorf("could not open crusoecloud configuration %s: %s", opts.CloudConfig, err)
		} else {
			defer func() {
				err = configFile.Close()
				if err != nil {
					klog.Errorf("failed to close crusoecloud config file: %s", err)
				}
			}()
		}
	}
	return newCrusoeCloudProvider(configFile, opts.UserAgent, rl)
}

// Name returns 'crusoecloud'
func (*crusoeCloudProvider) Name() string {
	return cloudprovider.CrusoeCloudProviderName
}

// NodeGroups returns all node groups configured for this cluster.
// critical endpoint, make it fast
func (ccp *crusoeCloudProvider) NodeGroups() []cloudprovider.NodeGroup {

	klog.V(4).Info("NodeGroups,ClusterID=", ccp.clusterID)

	nodeGroups := make([]cloudprovider.NodeGroup, len(ccp.nodeGroups))
	for i, ng := range ccp.nodeGroups {
		nodeGroups[i] = ng
	}
	return nodeGroups
}

// NodeGroupForNode returns the node group for the given node, nil if the node
// should not be processed by cluster autoscaler, or non-nil error if such
// occurred.
// critical endpoint, make it fast
func (ccp *crusoeCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	klog.V(4).Infof("NodeGroupForNode,NodeSpecProviderID=%s", node.Spec.ProviderID)

	for _, ng := range ccp.nodeGroups {
		if _, ok := ng.nodes[node.GetName()]; ok {
			return ng, nil
		}
	}
	return nil, nil
}

// HasInstance returns whether a given node has a corresponding instance in this cloud provider
func (ccp *crusoeCloudProvider) HasInstance(node *apiv1.Node) (bool, error) {

	for _, ng := range ccp.nodeGroups {
		if _, ok := ng.nodes[node.GetName()]; ok {
			return true, nil
		}
	}
	return false, nil
}

// Pricing return pricing model for crusoecloud.
func (ccp *crusoeCloudProvider) Pricing() (cloudprovider.PricingModel, ca_errors.AutoscalerError) {
	klog.V(4).Info("Pricing,called")
	return nil, cloudprovider.ErrNotImplemented
}

// GetAvailableMachineTypes get all machine types that can be requested from crusoecloud.
// Not implemented
func (ccp *crusoeCloudProvider) GetAvailableMachineTypes() ([]string, error) {
	return []string{}, nil
}

func (ccp *crusoeCloudProvider) NewNodeGroup(
	machineType string,
	labels map[string]string,
	systemLabels map[string]string,
	taints []apiv1.Taint,
	extraResources map[string]resource.Quantity,
) (cloudprovider.NodeGroup, error) {
	klog.V(4).Info("NewNodeGroup,called")
	return nil, cloudprovider.ErrNotImplemented
}

// GetResourceLimiter returns struct containing limits (max, min) for resources (cores, memory etc.).
func (ccp *crusoeCloudProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	klog.V(4).Info("GetResourceLimiter,called")
	return ccp.resourceLimiter, nil
}

// GPULabel returns the label added to nodes with GPU resource.
func (ccp *crusoeCloudProvider) GPULabel() string {
	klog.V(6).Info("GPULabel,called")
	return GPULabel
}

// GetAvailableGPUTypes return all available GPU types cloud provider supports.
// not yet implemented.
func (ccp *crusoeCloudProvider) GetAvailableGPUTypes() map[string]struct{} {
	klog.V(4).Info("GetAvailableGPUTypes,called")
	return availableGPUTypes
}

// GetNodeGpuConfig returns the label, type and resource name for the GPU added to node. If node doesn't have
// any GPUs, it returns nil.
func (ccp *crusoeCloudProvider) GetNodeGpuConfig(node *apiv1.Node) *cloudprovider.GpuConfig {
	klog.V(6).Info("GetNodeGpuConfig,called")
	return gpu.GetNodeGPUFromCloudProvider(ccp, node)
}

// Cleanup cleans up open resources before the cloud provider is destroyed, i.e. go routines etc.
func (ccp *crusoeCloudProvider) Cleanup() error {
	klog.V(4).Info("Cleanup,called")
	return nil
}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state.
// In particular the list of node groups returned by NodeGroups can change as a result of CloudProvider.Refresh().
func (ccp *crusoeCloudProvider) Refresh() error {
	klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s", ccp.projectID, ccp.clusterID)

	ctx := context.Background()
	resp, httpResp, err := ccp.client.KubernetesNodePoolsApi.ListNodePools(ctx, ccp.projectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(ccp.clusterID)})

	if err != nil {
		klog.Errorf("Refresh,failed to list pools for cluster %s: %s", ccp.clusterID, err)
		return err
	}
	if httpResp.StatusCode >= 400 {
		klog.Errorf("Refresh,failed to list pools for cluster %s: http error %s",
			ccp.clusterID, httpResp.Status)
		return fmt.Errorf("HTTP %s", httpResp.Status)
	}
	klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s ListNodePools returns %d IDs", ccp.projectID, ccp.clusterID, len(resp.Items))

	var ngs []*NodeGroup

	for _, p := range resp.Items {
		var instances map[string]*crusoeapi.InstanceV1Alpha5

		// this is especially until the rest-gateway change is shipped
		if p.ClusterId != ccp.clusterID {
			klog.Warningf("Skipping unexpected nodepool %s for cluster %s (!= %s)",
				p.Id, p.ClusterId, ccp.clusterID)
			continue
		}

		ng := NodeGroup{
			APIClient: ccp.client,
			pool:      &p,
		}

		// TODO: batch this for very large instance groups? (better: generate a provider ID so we don't need to fetch)
		respI, httpRespI, err := ccp.client.VMsApi.ListInstances(ctx, ccp.projectID, &crusoeapi.VMsApiListInstancesOpts{
			Ids: optional.NewString(strings.Join(p.InstanceIds, ",")),
		})
		if err != nil {
			klog.Errorf("Refresh,failed to list instances for cluster %s nodepool %s: %s", ccp.clusterID, p.Id, err)
			return err
		}
		if httpRespI.StatusCode >= 400 {
			klog.Errorf("Refresh,failed to list instances for cluster %s nodepool %s: http error %s",
				ccp.clusterID, p.Id, httpRespI.Status)
			return fmt.Errorf("HTTP %s", httpRespI.Status)
		}
		klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s,NodepoolID=%s ListInstances returns %d->%d IDs", ccp.projectID, ccp.clusterID, p.Id, len(p.InstanceIds), len(respI.Items))

		for _, instance := range respI.Items {
			instances[instance.Name] = &instance
		}
		ng.nodes = instances
		ngs = append(ngs, &ng)
	}
	klog.V(4).Infof("Refresh,ClusterID=%s,%d pools found", ccp.clusterID, len(ngs))

	ccp.nodeGroups = ngs

	return nil
}
