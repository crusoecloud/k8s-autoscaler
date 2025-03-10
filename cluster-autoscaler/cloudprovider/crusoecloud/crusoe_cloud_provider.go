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
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	ca_errors "k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
)

var _ cloudprovider.CloudProvider = &crusoeCloudProvider{}

const (
	// GPULabel is the label added to nodes with GPU resource.
	GPULabel = "crusoe.ai/accelerator"

	crusoeProviderIDPrefix = "crusoe://"
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
}

func newCrusoeCloudProvider(manager *crusoeManager, rl *cloudprovider.ResourceLimiter) *crusoeCloudProvider {
	return &crusoeCloudProvider{
		manager:         manager,
		resourceLimiter: rl,
	}
}

// Name returns the name of the cloud provider.
func (*crusoeCloudProvider) Name() string {
	return cloudprovider.CrusoeCloudProviderName
}

// NodeGroups returns all node groups configured for this cloud provider.
func (ccp *crusoeCloudProvider) NodeGroups() []cloudprovider.NodeGroup {
	nodeGroups := make([]cloudprovider.NodeGroup, len(ccp.manager.NodeGroups()))
	for i, ng := range ccp.manager.NodeGroups() {
		nodeGroups[i] = ng
	}
	return nodeGroups
}

// NodeGroupForNode returns the node group for the given node, nil if the node
// should not be processed by cluster autoscaler, or non-nil error if such
// occurred. Must be implemented.
func (ccp *crusoeCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	for _, ng := range ccp.manager.NodeGroups() {
		if _, ok := ng.nodes[toNodeID(node.Spec.ProviderID)]; ok {
			return ng, nil
		}
	}
	return nil, nil
}

// HasInstance returns whether a given node has a corresponding instance in this cloud provider
func (ccp *crusoeCloudProvider) HasInstance(node *apiv1.Node) (bool, error) {
	for _, ng := range ccp.manager.NodeGroups() {
		if _, ok := ng.nodes[toNodeID(node.Spec.ProviderID)]; ok {
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
	klog.V(4).Infof("Refresh,ProjectID=%s,ClusterID=%s", ccp.manager.projectID, ccp.manager.clusterID)

	err := ccp.manager.Refresh()
	if err != nil {
		klog.Errorf("Refresh failed: %v", err)
	}

	return err
}

// toProviderID returns a provider ID from the given node ID.
func toProviderID(nodeID string) string {
	return fmt.Sprintf("%s%s", crusoeProviderIDPrefix, nodeID)
}

// toNodeID returns a node or droplet ID from the given provider ID.
func toNodeID(providerID string) string {
	return strings.TrimPrefix(providerID, crusoeProviderIDPrefix)
}

// BuildCrusoeCloud returns CloudProvider implementation for CrusoeCloud.
func BuildCrusoeCloud(
	opts config.AutoscalingOptions,
	do cloudprovider.NodeGroupDiscoveryOptions,
	rl *cloudprovider.ResourceLimiter,
) cloudprovider.CloudProvider {
	var configFile io.ReadCloser

	if opts.CloudConfig != "" {
		var err error
		configFile, err = os.Open(opts.CloudConfig)
		if err != nil {
			klog.Fatalf("Could not open cloud provider configuration file %q, error: %v", opts.CloudConfig, err)
		}

		defer configFile.Close()
	} else {
		klog.Warningf("No config file provided, using environment only")
	}

	manager, err := newManager(configFile, do, opts.UserAgent)
	if err != nil {
		klog.Fatalf("Failed to create CrusoeCloud Manager: %v", err)
	}

	// the cloud provider automatically uses all node pools in CrusoeCloud for
	// the specified clusterID. The cloudprovider.NodeGroupDiscoveryOptions
	// flags (which can be set via '--node-group-auto-discovery' or '-nodes') are
	// used to specify min and max pool sizes.
	provider := newCrusoeCloudProvider(manager, rl)
	return provider
}
