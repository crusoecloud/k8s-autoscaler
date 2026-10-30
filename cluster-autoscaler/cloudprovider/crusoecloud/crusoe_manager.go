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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"

	"k8s.io/klog/v2"

	apiv1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
)

const (
	defaultApiEndpoint   = "https://api.crusoecloud.com/v1alpha5"
	scaleToZeroSupported = true

	crusoePrefix          = "crusoe.ai/"
	nodeLabelInstanceKey  = crusoePrefix + "instance.class"
	nodeLabelProjectIDKey = crusoePrefix + "project.id"
	nodeLabelGPUKey       = crusoePrefix + "accelerator"

	gpuProductLinePrefixesNvidia                    = "ahl"
	amdVNICResourceName          apiv1.ResourceName = "amd.com/vnics"

	instanceTypeDetailRefreshCoolDown = 6 * time.Hour

	// Where a node group's scaling bounds come from: the nodepool's
	// autoscaling_config served by the API (v2 nodepools), or the --nodes flag
	// (v1 nodepools).
	boundsSourceAPI   = "api"
	boundsSourceFlags = "flags"

	// nodePoolGenerationV2 is the node_pool_generation the API serves for v2
	// nodepools. Any other value, including an unset one, is a v1 nodepool.
	nodePoolGenerationV2 = 2
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

type crusoeInstanceTypeDetail struct {
	ProductName string `json:"product_name"`
	CpuCores    int64  `json:"cpu_cores"`
	CpuType     string `json:"cpu_type"`
	NumGpu      int64  `json:"num_gpu"`
	MemoryGb    int64  `json:"memory_gb"`
}

func readConf(config *crusoeCloudConfig, configFile io.Reader) error {
	body, err := io.ReadAll(configFile)
	if err != nil {
		return err
	}
	err = json.Unmarshal(body, config)
	return err
}

type crusoeK8sNodePoolService interface {
	GetNodePool(ctx context.Context, projectId string, vmId string) (crusoeapi.KubernetesNodePool, *http.Response, error)
	ListNodePools(ctx context.Context, projectId string, listOptions *crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts) (crusoeapi.ListKubernetesNodePoolsResponse, *http.Response, error)
	UpdateNodePool(ctx context.Context, body crusoeapi.KubernetesNodePoolPatchRequest, projectId string, nodePoolId string) (crusoeapi.AsyncOperationResponse, *http.Response, error)
}

type crusoeK8sNodePoolOperationService interface {
	GetKubernetesNodePoolsOperation(ctx context.Context, projectId string, opId string) (crusoeapi.Operation, *http.Response, error)
}

type crusoeVMService interface {
	DeleteInstance(ctx context.Context, projectId string, vmId string) (crusoeapi.AsyncOperationResponse, *http.Response, error)
	ListInstances(ctx context.Context, projectId string, localVarOptionals *crusoeapi.VMsApiListInstancesOpts) (crusoeapi.ListInstancesResponseV1Alpha5, *http.Response, error)
	GetVMTypes(ctx context.Context, projectId string) (crusoeapi.ListTypesResponseV1Alpha5, *http.Response, error)
}

type crusoeVMOperationService interface {
	GetComputeVMsInstancesOperation(ctx context.Context, projectId string, opId string) (crusoeapi.Operation, *http.Response, error)
}

type crusoeManager struct {
	// Service clients to Crusoecloud API
	nodePoolsApi   crusoeK8sNodePoolService
	nodePoolOpsApi crusoeK8sNodePoolOperationService
	vmApi          crusoeVMService
	vmOpsApi       crusoeVMOperationService
	waitBackoff    *waitBackoff

	// ProjectID is the project id containing the CMK cluster.
	projectID string
	// ClusterID is the CMK cluster id where the Autoscaler is running.
	clusterID string
	// Configured set of node groups
	nodeGroupSpecs map[string]*dynamic.NodeGroupSpec
	// Current set of node groups
	nodeGroups []*crusoeNodeGroup
	// map from instance type to its detail
	instanceTypeDetailMap map[string]*crusoeInstanceTypeDetail
	// cool down before crusoeManager can fetch new instance type detail
	instanceTypeRefreshLastRefresh time.Time
	// invalidBounds holds, per nodepool ID, the enabled autoscaling_config
	// bounds that the last Refresh rejected as invalid, so the warning is
	// logged once per change rather than on every loop.
	invalidBounds map[string]autoscalingBounds
	// ignoredFlagPools holds the IDs of v2 nodepools listed in --nodes as of
	// the last Refresh, so the error is logged once per nodepool rather than on
	// every loop.
	ignoredFlagPools map[string]struct{}
}

// autoscalingBounds is a nodepool's [min, max] as served in its autoscaling_config.
type autoscalingBounds struct {
	min, max int64
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

	// apply default if needed
	if cfg.APIEndpoint == "" {
		cfg.APIEndpoint = defaultApiEndpoint
	}

	// env takes precedence over config passed by command-line
	cfg.ProjectID = getenvOr("CRUSOE_PROJECT_ID", cfg.ProjectID)
	cfg.ClusterID = getenvOr("CRUSOE_CLUSTER_ID", cfg.ClusterID)
	cfg.AccessKey = getenvOr("CRUSOE_ACCESS_KEY", cfg.AccessKey)
	cfg.SecretKey = getenvOr("CRUSOE_SECRET_KEY", cfg.SecretKey)
	cfg.APIEndpoint = getenvOr("CRUSOE_API_ENDPOINT", cfg.APIEndpoint)

	klog.V(4).Infof("CrusoeCloud Manager built; ProjectId=%s;ClusterId=%s,AccessKey=%s***,ApiURL=%s",
		cfg.ProjectID, cfg.ClusterID, cfg.AccessKey[:4], cfg.APIEndpoint)

	ngSpecs := make(map[string]*dynamic.NodeGroupSpec)
	for _, nodeGroupSpec := range discoveryOpts.NodeGroupSpecs {
		spec, err := dynamic.SpecFromString(nodeGroupSpec, scaleToZeroSupported)
		if err != nil {
			klog.Fatalf("Could not parse node group spec %s: %v", nodeGroupSpec, err)
		}
		ngSpecs[spec.Name] = spec
	}

	client := NewAPIClient(cfg.APIEndpoint, cfg.AccessKey, cfg.SecretKey, userAgent)

	manager := &crusoeManager{
		nodePoolsApi:   client.KubernetesNodePoolsApi,
		nodePoolOpsApi: client.KubernetesNodePoolOperationsApi,
		vmApi:          client.VMsApi,
		vmOpsApi:       client.VMOperationsApi,
		waitBackoff:    newDefaultWaitBackoff(),
		projectID:      cfg.ProjectID,
		clusterID:      cfg.ClusterID,
		nodeGroupSpecs: ngSpecs,
		nodeGroups:     []*crusoeNodeGroup{},
	}

	instanceTypeDetailMap, err := manager.getInstanceTypeDetailMap(context.Background())
	if err != nil {
		klog.Errorf("failed to get instance type detail for crusoeManager, autoscaler may not be able to fetch node info to build template: %v", err)
	}
	manager.instanceTypeDetailMap = instanceTypeDetailMap

	return manager, nil
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
	invalidBounds := map[string]autoscalingBounds{}
	ignoredFlagPools := map[string]struct{}{}
	cachedPoolIDToNodeGroupsMap := map[string]*crusoeNodeGroup{}
	for _, ng := range mgr.nodeGroups {
		cachedPoolIDToNodeGroupsMap[ng.pool.Id] = ng
	}

	for _, p := range pools {
		// just in case
		if p.ClusterId != mgr.clusterID {
			klog.Warningf("Skipping unexpected nodepool %s for cluster %s (!= %s)",
				p.Id, p.ClusterId, mgr.clusterID)
			continue
		}

		if _, listed := mgr.nodeGroupSpecs[p.Name]; listed && isNodePoolV2(&p) {
			if _, seen := mgr.ignoredFlagPools[p.Id]; !seen {
				klog.Errorf("Ignoring --nodes entry for v2 nodepool %s (%s): v2 nodepools are autoscaled "+
					"only within their autoscaling_config in the Crusoe Cloud API. Set the bounds there "+
					"and remove the --nodes entry.", p.Name, p.Id)
			}
			ignoredFlagPools[p.Id] = struct{}{}
		}

		spec, boundsSource, err := mgr.resolveNodeGroupSpec(&p)
		if err != nil {
			bounds := autoscalingBounds{min: p.AutoscalingConfig.MinNodeSize, max: p.AutoscalingConfig.MaxNodeSize}
			if prev, seen := mgr.invalidBounds[p.Id]; !seen || prev != bounds {
				klog.Warningf("Skipping nodepool %s (%s): %v", p.Name, p.Id, err)
			} else {
				klog.V(4).Infof("Skipping nodepool %s (%s): %v", p.Name, p.Id, err)
			}
			invalidBounds[p.Id] = bounds
			continue
		}
		if spec == nil {
			continue
		}
		klog.V(4).Infof("Nodepool %s (%s): scaling bounds [%d, %d], source=%s",
			p.Name, p.Id, spec.MinSize, spec.MaxSize, boundsSource)

		if cachedNg, ok := cachedPoolIDToNodeGroupsMap[p.Id]; ok {
			// Bounds can change between loops (autoscaling_config edited,
			// or the pool migrating from v1 to v2).
			cachedNg.spec = spec
			if err := cachedNg.refresh(); err != nil {
				klog.Errorf("Refresh failed for nodepool %s: %s", p.Id, err)
			}
			ngs = append(ngs, cachedNg)
		} else {
			ng := crusoeNodeGroup{
				manager:                   mgr,
				pool:                      &p,
				nodes:                     make(map[string]*crusoeapi.InstanceV1Alpha5),
				deletionInProgressNodeSet: map[string]struct{}{},

				spec: spec,
			}
			// refresh to populate nodes and target size information
			if err := ng.refresh(); err != nil {
				klog.Errorf("Refresh failed for new nodepool %s: %s", p.Id, err)
			}
			ngs = append(ngs, &ng)
		}
	}
	klog.V(4).Infof("Refresh,ClusterID=%s,%d pools found", mgr.clusterID, len(ngs))

	mgr.nodeGroups = ngs
	mgr.invalidBounds = invalidBounds
	mgr.ignoredFlagPools = ignoredFlagPools

	return nil
}

// isNodePoolV2 reports whether the API serves pool as a v2 nodepool.
func isNodePoolV2(pool *crusoeapi.KubernetesNodePool) bool {
	return pool.NodePoolGeneration == nodePoolGenerationV2
}

// resolveNodeGroupSpec decides whether this autoscaler manages a nodepool, and
// within which bounds. The nodepool's generation decides where the bounds
// come from, and there is no fallback between the two:
//
//   - v2: only the API. An enabled autoscaling_config autoscales the pool
//     within its bounds; a disabled or absent one leaves the pool unmanaged.
//     A --nodes entry for the pool is ignored (Refresh logs an error).
//   - v1: only --nodes. The pool is autoscaled within its --nodes bounds if
//     listed there, and not managed otherwise. The API does not accept an
//     autoscaling_config for v1 nodepools, so any it serves is ignored.
//
// A nil spec with a nil error means the pool is not managed; the reason is
// logged here. A non-nil error means the v2 pool's block is enabled with
// invalid bounds; the pool is not managed and the caller decides how loudly
// to log.
func (mgr *crusoeManager) resolveNodeGroupSpec(pool *crusoeapi.KubernetesNodePool) (*dynamic.NodeGroupSpec, string, error) {
	if !isNodePoolV2(pool) {
		spec, ok := mgr.nodeGroupSpecs[pool.Name]
		if !ok {
			klog.V(4).Infof("Skipping v1 nodepool %s (%s): not listed in the autoscaler's node group config",
				pool.Name, pool.Id)
			return nil, "", nil
		}
		return spec, boundsSourceFlags, nil
	}

	ac := pool.AutoscalingConfig
	if ac == nil || !ac.Enabled {
		reason := "no autoscaling_config"
		if ac != nil {
			reason = "autoscaling_config is disabled"
		}
		klog.V(4).Infof("Skipping v2 nodepool %s (%s): %s", pool.Name, pool.Id, reason)
		return nil, "", nil
	}

	spec := &dynamic.NodeGroupSpec{
		Name:               pool.Name,
		MinSize:            int(ac.MinNodeSize),
		MaxSize:            int(ac.MaxNodeSize),
		SupportScaleToZero: scaleToZeroSupported,
	}
	// [0, 0] is valid: the group registers pinned at zero, and CA keeps it
	// inert because a scale-up needs the target to be below MaxSize.
	if err := spec.Validate(); err != nil {
		return nil, "", fmt.Errorf("invalid autoscaling_config bounds [%d, %d]: %w",
			ac.MinNodeSize, ac.MaxNodeSize, err)
	}
	return spec, boundsSourceAPI, nil
}

func (mgr *crusoeManager) ListNodePools(ctx context.Context) ([]crusoeapi.KubernetesNodePool, error) {
	resp, httpResp, err := mgr.nodePoolsApi.ListNodePools(ctx, mgr.projectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(mgr.clusterID)})

	if err != nil {
		return nil, fmt.Errorf("failed to list pools for cluster %s: %w", mgr.clusterID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to list pools for cluster %s: http error %s",
			mgr.clusterID, httpResp.Status)
	}

	pools := make([]crusoeapi.KubernetesNodePool, 0, len(resp.Items))
	for _, pool := range resp.Items {
		if pool.State != stateDeleted && pool.State != stateDeleting {
			pools = append(pools, pool)
		}
	}
	return pools, nil
}

func (mgr *crusoeManager) GetNodePool(ctx context.Context, poolID string) (*crusoeapi.KubernetesNodePool, error) {
	resp, httpResp, err := mgr.nodePoolsApi.GetNodePool(ctx, mgr.projectID, poolID)

	if err != nil {
		return nil, fmt.Errorf("failed to get nodepool %s: %w", poolID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to get nodepool %s: http error %s", poolID, httpResp.Status)
	}
	return &resp, nil
}

// UpdateNodePool sets the pool's desired node count to targetSize.
//
// The PATCH applies every non-null field and several fields lack omitempty, so
// the request echoes the pool's current value wherever a zero value would
// reset a setting: ephemeral_storage_for_containerd, ssh_public_key, the user
// node labels and reservation_id. node_taints is left null (unchanged), since
// an echo could fail validation or revert a concurrent edit. pool must be
// freshly read.
func (mgr *crusoeManager) UpdateNodePool(ctx context.Context, pool *crusoeapi.KubernetesNodePool, targetSize int64) (*crusoeapi.Operation, error) {
	req := crusoeapi.KubernetesNodePoolPatchRequest{
		Action:                        "UPDATE",
		Count:                         targetSize,
		EphemeralStorageForContainerd: pool.EphemeralStorageForContainerd,
		SshPublicKey:                  pool.SshPublicKey,
	}
	if labels := userNodeLabels(pool.NodeLabels); len(labels) > 0 {
		req.NodeLabels = labels
	}
	if pool.ReservationId != "" {
		req.ReservationId = pool.ReservationId
	}

	resp, httpResp, err := mgr.nodePoolsApi.UpdateNodePool(ctx, req, mgr.projectID, pool.Id)
	if err != nil {
		return nil, fmt.Errorf("failed to update node pool %s for cluster %s: %w", pool.Id, mgr.clusterID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to update node pool %s for cluster %s: http error %s",
			pool.Id, mgr.clusterID, httpResp.Status)
	}
	return resp.Operation, nil
}

// userNodeLabels returns the user-defined subset of pool labels, stripping
// the internal "crusoe.ai/" prefix entries so they aren't echoed back on PATCH.
func userNodeLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if strings.HasPrefix(k, crusoePrefix) {
			continue
		}
		out[k] = v
	}
	return out
}

func (mgr *crusoeManager) GetNodePoolOperation(ctx context.Context, opID string) (*crusoeapi.Operation, error) {
	resp, httpResp, err := mgr.nodePoolOpsApi.GetKubernetesNodePoolsOperation(ctx, mgr.projectID, opID)

	if err != nil {
		return nil, fmt.Errorf("failed to get nodepool operation %s: %w", opID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to get nodepool operation %s: http error %s", opID, httpResp.Status)
	}
	return &resp, nil
}

func (mgr *crusoeManager) WaitForNodePoolOperationComplete(ctx context.Context, op *crusoeapi.Operation) (*crusoeapi.Operation, error) {
	return mgr.waitBackoff.WaitForOperationComplete(ctx, op, func(ctx context.Context, operationId string) (*crusoeapi.Operation, error) {
		updatedOp, err := mgr.GetNodePoolOperation(ctx, op.OperationId)
		if err != nil {
			return nil, fmt.Errorf("failed to poll for nodepool operation: %w", err)
		}
		return updatedOp, nil
	})
}

func (mgr *crusoeManager) getInstanceTypeDetailMap(ctx context.Context) (map[string]*crusoeInstanceTypeDetail, error) {
	instanceTypeDetailMap := map[string]*crusoeInstanceTypeDetail{}

	typeResp, _, err := mgr.vmApi.GetVMTypes(ctx, mgr.projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list instance types: %w", err)
	}
	mgr.instanceTypeRefreshLastRefresh = time.Now()

	for i := range typeResp.Items {
		instanceTypeDetailMap[typeResp.Items[i].ProductName] = &crusoeInstanceTypeDetail{
			ProductName: typeResp.Items[i].ProductName,
			CpuCores:    typeResp.Items[i].CpuCores,
			CpuType:     typeResp.Items[i].CpuType,
			NumGpu:      typeResp.Items[i].NumGpu,
			MemoryGb:    typeResp.Items[i].MemoryGb,
		}
	}

	return instanceTypeDetailMap, nil
}

func (m *crusoeManager) buildTemplateNodeFromNodePool(ctx context.Context, nodePool *crusoeapi.KubernetesNodePool) (*apiv1.Node, error) {
	if time.Since(m.instanceTypeRefreshLastRefresh) >= instanceTypeDetailRefreshCoolDown {
		instanceTypeDetailMap, err := m.getInstanceTypeDetailMap(ctx)
		if err != nil {
			return nil, err
		}
		m.instanceTypeDetailMap = instanceTypeDetailMap
	}

	instanceTypeDetail, ok := m.instanceTypeDetailMap[nodePool.Type_]
	if !ok {
		return nil, fmt.Errorf("instance type %s not found in instance type detail map", nodePool.Type_)
	}

	node := apiv1.Node{}
	nodeName := ""

	node.ObjectMeta = metav1.ObjectMeta{
		Name:     nodeName,
		SelfLink: fmt.Sprintf("/api/v1/nodes/%s", nodeName),
		Labels:   map[string]string{},
	}

	node.Status = apiv1.NodeStatus{
		Capacity: apiv1.ResourceList{},
	}

	// TODO: get a real value.
	node.Status.Capacity[apiv1.ResourcePods] = *resource.NewQuantity(110, resource.DecimalSI)
	node.Status.Capacity[apiv1.ResourceCPU] = *resource.NewQuantity(instanceTypeDetail.CpuCores, resource.DecimalSI)
	node.Status.Capacity[apiv1.ResourceMemory] = *resource.NewQuantity(instanceTypeDetail.MemoryGb*1024*1024*1024, resource.DecimalSI)

	// Set GPU resource based on the GPU node label
	if instanceTypeDetail.NumGpu > 0 {
		gpuResourceName := getGPUResourceName(nodePool.NodeLabels)
		qty := resource.NewQuantity(instanceTypeDetail.NumGpu, resource.DecimalSI)
		node.Status.Capacity[gpuResourceName] = *qty
		// TODO: Remove after VM API provides VNIC counts.
		if gpuResourceName == gpu.ResourceAMDGPU {
			// VM API doesn't provide VNIC counts; assume symmetric vNICs to GPU count for AMD nodes.
			node.Status.Capacity[amdVNICResourceName] = *qty
		}
	}

	node.Status.Allocatable = node.Status.Capacity

	node.Labels = cloudprovider.JoinStringMaps(node.Labels, m.buildGenericLabels(instanceTypeDetail, nodePool.NodeLabels))

	node.Status.Conditions = cloudprovider.BuildReadyConditions()

	return &node, nil
}

func (mgr *crusoeManager) ListVMInstances(ctx context.Context, instanceIds []string) ([]crusoeapi.InstanceV1Alpha5, error) {
	resp, httpResp, err := mgr.vmApi.ListInstances(ctx, mgr.projectID, &crusoeapi.VMsApiListInstancesOpts{
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
	resp, httpResp, err := mgr.vmApi.DeleteInstance(ctx, mgr.projectID, instanceId)
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
	resp, httpResp, err := mgr.vmOpsApi.GetComputeVMsInstancesOperation(ctx, mgr.projectID, opID)

	if err != nil {
		return nil, fmt.Errorf("failed to get VM operation %s: %w", opID, err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to get VM operation %s: http error %s", opID, httpResp.Status)
	}
	return &resp, nil
}

func (mgr *crusoeManager) WaitForVMOperationListComplete(ctx context.Context, ops []*crusoeapi.Operation) (
	[]*crusoeapi.Operation, error,
) {
	return mgr.waitBackoff.WaitForOperationListComplete(ctx, ops, func(ctx context.Context, operationId string) (*crusoeapi.Operation, error) {
		updatedOp, err := mgr.GetVMOperation(ctx, operationId)
		if err != nil {
			return nil, fmt.Errorf("failed to poll for VM operation: %w", err)
		}
		return updatedOp, nil
	})
}

func (m *crusoeManager) buildGenericLabels(instanceTypeDetail *crusoeInstanceTypeDetail, nodeLabels map[string]string) map[string]string {
	result := make(map[string]string)

	result[apiv1.LabelArchStable] = instanceTypeDetail.CpuType
	result[apiv1.LabelOSStable] = cloudprovider.DefaultOS
	result[apiv1.LabelInstanceTypeStable] = instanceTypeDetail.ProductName

	// Merge node labels from the the crusoe nodepool API response
	for k, v := range nodeLabels {
		result[k] = v
	}

	// Crusoe NodePool creation API adds GPU labels for nodes created after July 15, 2025.
	// If 'crusoe.ai/accelerator' label is not present, it's an older node and we need to manually add labels
	if _, found := nodeLabels[nodeLabelGPUKey]; !found {
		m.addMissingLabels(result, instanceTypeDetail)
	}

	return result
}

func (m *crusoeManager) addMissingLabels(result map[string]string, instanceTypeDetail *crusoeInstanceTypeDetail) {
	productNameSplitted := strings.Split(instanceTypeDetail.ProductName, ".")
	if len(productNameSplitted) != 2 {
		klog.Errorf("unexpected product name format %s when building generic labels", instanceTypeDetail.ProductName)
	} else {
		result[nodeLabelInstanceKey] = productNameSplitted[0]
	}
	result[nodeLabelProjectIDKey] = m.projectID
	if gpuLabelValue := gpuLabelValueFromProductLine(productNameSplitted[0]); gpuLabelValue != "" {
		result[nodeLabelGPUKey] = gpuLabelValue
	}
}

func gpuLabelValueFromProductLine(productLine string) string {
	// check for starting with the Nvidia prefix letters
	if strings.IndexByte(gpuProductLinePrefixesNvidia, productLine[0]) != -1 {
		return "nvidia-" + productLine
	}

	return ""
}

func getGPUResourceName(nodeLabels map[string]string) apiv1.ResourceName {
	// Nodes created post July 15, 2025 will always have the GPU label
	if gpuLabel, hasGPULabel := nodeLabels[nodeLabelGPUKey]; hasGPULabel {
		if strings.HasPrefix(gpuLabel, "amd-") {
			return gpu.ResourceAMDGPU
		}
		if strings.HasPrefix(gpuLabel, "nvidia-") {
			return gpu.ResourceNvidiaGPU
		}
	}
	// Default to NVIDIA GPU
	return gpu.ResourceNvidiaGPU
}
