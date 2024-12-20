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
	"io"
	"os"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	ca_errors "k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
	"k8s.io/klog/v2"
)

var _ cloudprovider.CloudProvider = &crusoeCloudProvider{}

const (
	// GPULabel is the label added to nodes with GPU resource.
	GPULabel = "crusoe.ai/accelerator"

	instanceBatchSize = 50 // page instance fetch by this size
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
	manager         *crusoeManager
	resourceLimiter *cloudprovider.ResourceLimiter
	nodeGroups      []*crusoeNodeGroup
}

func newCrusoeCloudProvider(manager *crusoeManager, rl *cloudprovider.ResourceLimiter) *crusoeCloudProvider {
	return &crusoeCloudProvider{
		manager:         manager,
		resourceLimiter: rl,
		nodeGroups:      []*crusoeNodeGroup{},
	}
}

// Name returns the name of the cloud provider.
func (*crusoeCloudProvider) Name() string {
	return cloudprovider.CrusoeCloudProviderName
}

// NodeGroups returns all node groups configured for this cloud provider.
func (ccp *crusoeCloudProvider) NodeGroups() []cloudprovider.NodeGroup {
	nodeGroups := make([]cloudprovider.NodeGroup, len(ccp.nodeGroups))
	for i, ng := range ccp.nodeGroups {
		nodeGroups[i] = ng
	}
	return nodeGroups
}

// NodeGroupForNode returns the node group for the given node, nil if the node
// should not be processed by cluster autoscaler, or non-nil error if such
// occurred. Must be implemented.
func (ccp *crusoeCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
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

// Pricing returns pricing model for this cloud provider or error if not available.
// Implementation optional.
func (ccp *crusoeCloudProvider) Pricing() (cloudprovider.PricingModel, ca_errors.AutoscalerError) {
	return nil, cloudprovider.ErrNotImplemented
}

// GetAvailableMachineTypes get all machine types that can be requested from crusoecloud.
// Implementation optional.
func (ccp *crusoeCloudProvider) GetAvailableMachineTypes() ([]string, error) {
	return []string{}, nil
}

// NewNodeGroup builds a theoretical node group based on the node definition
// provided. The node group is not automatically created on the cloud provider
// side. The node group is not returned by NodeGroups() until it is created.
// Implementation optional.
func (ccp *crusoeCloudProvider) NewNodeGroup(machineType string, labels map[string]string, systemLabels map[string]string, taints []apiv1.Taint, extraResources map[string]resource.Quantity) (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrNotImplemented
}

// GetResourceLimiter returns struct containing limits (max, min) for resources (cores, memory etc.).
func (ccp *crusoeCloudProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return ccp.resourceLimiter, nil
}

// GPULabel returns the label added to nodes with GPU resource.
func (ccp *crusoeCloudProvider) GPULabel() string {
	return GPULabel
}

// GetAvailableGPUTypes return all available GPU types cloud provider supports.
func (ccp *crusoeCloudProvider) GetAvailableGPUTypes() map[string]struct{} {
	return availableGPUTypes
}

// GetNodeGpuConfig returns the label, type and resource name for the GPU added to node. If node doesn't have
// any GPUs, it returns nil.
func (ccp *crusoeCloudProvider) GetNodeGpuConfig(node *apiv1.Node) *cloudprovider.GpuConfig {
	return gpu.GetNodeGPUFromCloudProvider(ccp, node)
}

// Cleanup cleans up open resources before the cloud provider is destroyed, i.e. go routines etc.
func (ccp *crusoeCloudProvider) Cleanup() error {
	return nil
}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state.
// In particular the list of node groups returned by NodeGroups can change as a result of CloudProvider.Refresh().
func (ccp *crusoeCloudProvider) Refresh() error {
	// TODO: move implementation to manager?
	klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s", ccp.manager.projectID, ccp.manager.clusterID)

	ctx := context.Background()
	pools, err := ccp.manager.ListNodePools(ctx)
	if err != nil {
		klog.Errorf("Refresh failed: %s", err)
		return err
	}
	klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s ListNodePools returns %d IDs",
		ccp.manager.projectID, ccp.manager.clusterID, len(pools))

	var ngs []*crusoeNodeGroup

	for _, p := range pools {
		// just in case
		if p.ClusterId != ccp.manager.clusterID {
			klog.Warningf("Skipping unexpected nodepool %s for cluster %s (!= %s)",
				p.Id, p.ClusterId, ccp.manager.clusterID)
			continue
		}

		ng := crusoeNodeGroup{
			manager: ccp.manager,
			pool:    &p,
			nodes:   make(map[string]*crusoeapi.InstanceV1Alpha5),
		}

		for i := 0; i < len(p.InstanceIds); i += instanceBatchSize {
			end := i + instanceBatchSize
			if end > len(p.InstanceIds) {
				end = len(p.InstanceIds)
			}

			instances, err := ccp.manager.ListVMInstances(ctx, p.InstanceIds[i:end])
			if err != nil {
				klog.Errorf("Refresh failed for nodepool %s: %s", p.Id, err)
				return err
			}
			klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s,NodepoolID=%s ListInstances returns %d->%d IDs",
				ccp.manager.projectID, ccp.manager.clusterID, p.Id, len(p.InstanceIds), len(instances))

			for _, instance := range instances {
				// TODO: change to index by providerID
				ng.nodes[instance.Name] = &instance
			}
		}
		ngs = append(ngs, &ng)
	}
	klog.V(4).Infof("Refresh,ClusterID=%s,%d pools found", ccp.manager.clusterID, len(ngs))

	ccp.nodeGroups = ngs

	return nil
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

	manager := newManager(configFile, opts.UserAgent)
	return newCrusoeCloudProvider(manager, rl)
}
