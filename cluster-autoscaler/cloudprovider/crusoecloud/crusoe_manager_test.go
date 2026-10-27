/*
Copyright 2025 The Kubernetes Authors.

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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
	"k8s.io/klog/v2"
)

func TestManager_ListNodePools(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()

	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{
					{
						Name:  "abcd",
						State: stateRunning,
					},
					{
						Name:  "efgh",
						State: stateRunning,
					},
				},
			}, httpSuccessResponse(), nil,
		)

	pools, err := mgr.ListNodePools(ctx)
	assert.NoError(t, err)
	assert.Len(t, pools, 2)
}

func TestManager_ListNodePoolsNonrunning(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()

	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{
					{
						Name:  "abcd",
						State: stateDeleting,
					},
					{
						Name:  "efgh",
						State: stateRunning,
					},
				},
			}, httpSuccessResponse(), nil,
		)

	pools, err := mgr.ListNodePools(ctx)
	assert.NoError(t, err)
	assert.Len(t, pools, 1)
	assert.Equal(t, pools[0].Name, "efgh")
}

func TestManager_BuildTemplateNodeFromNodePool_AMDGpu(t *testing.T) {
	ctx := context.Background()
	mgr, _ := testManagerWithMocks()

	// Prevent cache refresh from calling GetVMTypes (set to far future time)
	mgr.instanceTypeRefreshLastRefresh = time.Now().Add(10 * 365 * 24 * time.Hour)

	// Pre-populate instance type detail map to avoid mocking complexity
	mgr.instanceTypeDetailMap = map[string]*crusoeInstanceTypeDetail{
		"mi300x-192gb-ib.8x": {
			ProductName: "mi300x-192gb-ib.8x",
			CpuCores:    240,
			CpuType:     "vCPU",
			NumGpu:      8,
			MemoryGb:    2000,
		},
	}

	nodePool := &crusoeapi.KubernetesNodePool{
		Type_: "mi300x-192gb-ib.8x",
		NodeLabels: map[string]string{
			nodeLabelGPUKey: "amd-mi300x-192gb",
		},
	}

	node, err := mgr.buildTemplateNodeFromNodePool(ctx, nodePool)
	assert.NoError(t, err)
	assert.NotNil(t, node)

	assert.Equal(t, "amd-mi300x-192gb", node.Labels[nodeLabelGPUKey])

	pods := node.Status.Capacity[apiv1.ResourcePods]
	cpu := node.Status.Capacity[apiv1.ResourceCPU]
	mem := node.Status.Capacity[apiv1.ResourceMemory]
	gpuAmd := node.Status.Capacity[gpu.ResourceAMDGPU]
	vnicAmd := node.Status.Capacity[amdVNICResourceName]

	// Ensure Cpacity is set correctly
	assert.Equal(t, int64(110), pods.Value())
	assert.Equal(t, int64(240), cpu.Value())
	assert.Equal(t, int64(2000*1024*1024*1024), mem.Value())
	assert.Equal(t, int64(8), gpuAmd.Value())
	assert.Equal(t, int64(8), vnicAmd.Value())
}

func TestManager_BuildTemplateNodeFromNodePool_NvidiaGpu(t *testing.T) {
	ctx := context.Background()
	mgr, _ := testManagerWithMocks()

	// Prevent cache refresh from calling GetVMTypes (set to far future time)
	mgr.instanceTypeRefreshLastRefresh = time.Now().Add(10 * 365 * 24 * time.Hour)

	// Pre-populate instance type detail map
	mgr.instanceTypeDetailMap = map[string]*crusoeInstanceTypeDetail{
		"l40s-48gb.1x": {
			ProductName: "l40s-48gb.1x",
			CpuCores:    48,
			CpuType:     "vCPU",
			NumGpu:      1,
			MemoryGb:    192,
		},
	}

	nodePool := &crusoeapi.KubernetesNodePool{
		Type_: "l40s-48gb.1x",
		NodeLabels: map[string]string{
			nodeLabelGPUKey: "nvidia-l40s-48gb",
		},
	}

	node, err := mgr.buildTemplateNodeFromNodePool(ctx, nodePool)
	assert.NoError(t, err)
	assert.NotNil(t, node)

	assert.Equal(t, "nvidia-l40s-48gb", node.Labels[nodeLabelGPUKey])

	pods := node.Status.Capacity[apiv1.ResourcePods]
	cpu := node.Status.Capacity[apiv1.ResourceCPU]
	mem := node.Status.Capacity[apiv1.ResourceMemory]
	gpuNv := node.Status.Capacity[gpu.ResourceNvidiaGPU]
	_, hasAmdVnic := node.Status.Capacity[amdVNICResourceName]

	assert.Equal(t, int64(110), pods.Value())
	assert.Equal(t, int64(48), cpu.Value())
	assert.Equal(t, int64(192*1024*1024*1024), mem.Value())
	assert.Equal(t, int64(1), gpuNv.Value())
	assert.False(t, hasAmdVnic)
}

func TestManager_BuildTemplateNodeFromNodePool_NvidiaGpuFallback(t *testing.T) {
	// Targets Node Group before we started adding GPU labels to nodes on create operation
	ctx := context.Background()
	mgr, _ := testManagerWithMocks()

	// Prevent cache refresh from calling GetVMTypes (set to far future time)
	mgr.instanceTypeRefreshLastRefresh = time.Now().Add(10 * 365 * 24 * time.Hour)

	// Pre-populate instance type detail map
	mgr.instanceTypeDetailMap = map[string]*crusoeInstanceTypeDetail{
		"h200-141gb-sxm-ib.8x": {
			ProductName: "h200-141gb-sxm-ib.8x",
			CpuCores:    176,
			CpuType:     "vCPU",
			NumGpu:      8,
			MemoryGb:    1960,
		},
	}

	// Old node without GPU label - should auto-generate
	nodePool := &crusoeapi.KubernetesNodePool{
		Type_:      "h200-141gb-sxm-ib.8x",
		NodeLabels: map[string]string{},
	}

	node, err := mgr.buildTemplateNodeFromNodePool(ctx, nodePool)
	assert.NoError(t, err)
	assert.NotNil(t, node)

	assert.Equal(t, "nvidia-h200-141gb-sxm-ib", node.Labels[nodeLabelGPUKey])

	pods := node.Status.Capacity[apiv1.ResourcePods]
	cpu := node.Status.Capacity[apiv1.ResourceCPU]
	mem := node.Status.Capacity[apiv1.ResourceMemory]
	gpuNv := node.Status.Capacity[gpu.ResourceNvidiaGPU]
	_, hasAmdVnic := node.Status.Capacity[amdVNICResourceName]

	assert.Equal(t, int64(110), pods.Value())
	assert.Equal(t, int64(176), cpu.Value())
	assert.Equal(t, int64(1960*1024*1024*1024), mem.Value())
	assert.Equal(t, int64(8), gpuNv.Value())
	assert.False(t, hasAmdVnic)
}

func TestManager_UpdateNodePool_PreservesExistingFields(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()

	pool := &crusoeapi.KubernetesNodePool{
		Id:                            testNodePoolID,
		ProjectId:                     testProjectID,
		ClusterId:                     testClusterID,
		Count:                         3,
		EphemeralStorageForContainerd: true,
		ReservationId:                 "reservation-123",
		SshPublicKey:                  testSSHPublicKey,
		NodeTaints:                    []crusoeapi.KubernetesNodeTaint{{Key: "gpu", Value: "true", Effect: "NoSchedule"}},
		NodeLabels: map[string]string{
			"team":                  "platform",
			"env":                   "prod",
			crusoePrefix + "system": "should-be-stripped",
		},
	}

	expectedReq := crusoeapi.KubernetesNodePoolPatchRequest{
		Action:                        "UPDATE",
		Count:                         5,
		EphemeralStorageForContainerd: true,
		ReservationId:                 "reservation-123",
		SshPublicKey:                  testSSHPublicKey,
		// NodeTaints stays nil: null leaves the pool's taints unchanged.
		NodeLabels: map[string]string{
			"team": "platform",
			"env":  "prod",
		},
	}

	mocks.nodePoolsApi.
		On("UpdateNodePool", ctx, expectedReq, testProjectID, testNodePoolID).
		Return(
			crusoeapi.AsyncOperationResponse{
				Operation: &crusoeapi.Operation{OperationId: "op-1", State: string(opInProgress)},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	op, err := mgr.UpdateNodePool(ctx, pool, 5)
	assert.NoError(t, err)
	assert.NotNil(t, op)
	assert.Equal(t, "op-1", op.OperationId)
	mocks.nodePoolsApi.AssertExpectations(t)
}

// TestManager_UpdateNodePool_NoLabelsOrReservation verifies that pools without
// labels or reservations produce a minimal request.
func TestManager_UpdateNodePool_NoLabelsOrReservation(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()

	pool := &crusoeapi.KubernetesNodePool{
		Id:        testNodePoolID,
		ProjectId: testProjectID,
		ClusterId: testClusterID,
		Count:     2,
	}

	expectedReq := crusoeapi.KubernetesNodePoolPatchRequest{
		Action: "UPDATE",
		Count:  4,
	}

	mocks.nodePoolsApi.
		On("UpdateNodePool", ctx, expectedReq, testProjectID, testNodePoolID).
		Return(
			crusoeapi.AsyncOperationResponse{
				Operation: &crusoeapi.Operation{OperationId: "op-2", State: string(opInProgress)},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	_, err := mgr.UpdateNodePool(ctx, pool, 4)
	assert.NoError(t, err)
	mocks.nodePoolsApi.AssertExpectations(t)
}

func TestManager_RefreshSkipsNodePoolsWithoutSpec(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
		"configured-pool": {Name: "configured-pool", MinSize: 1, MaxSize: 5},
	}

	configuredPool := crusoeapi.KubernetesNodePool{
		Id:        "configured-pool-id",
		Name:      "configured-pool",
		ClusterId: testClusterID,
		State:     stateRunning,
	}
	staticPool := crusoeapi.KubernetesNodePool{
		Id:        "static-pool-id",
		Name:      "static-pool",
		ClusterId: testClusterID,
		State:     stateRunning,
	}

	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{configuredPool, staticPool},
			}, httpSuccessResponse(), nil,
		)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, "configured-pool-id").
		Return(configuredPool, httpSuccessResponse(), nil)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, "static-pool-id").
		Return(staticPool, httpSuccessResponse(), nil)

	err := mgr.Refresh()
	assert.NoError(t, err)

	nodeGroups := mgr.NodeGroups()
	assert.Len(t, nodeGroups, 1)
	assert.Equal(t, "configured-pool", nodeGroups[0].pool.Name)
}

const testSSHPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExampleExampleExampleExample user@host"

// listPoolsOnce makes the next ListNodePools call return pools.
func listPoolsOnce(mocks *crusoeMocks, pools ...crusoeapi.KubernetesNodePool) {
	mocks.nodePoolsApi.On("ListNodePools", context.Background(), testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(crusoeapi.ListKubernetesNodePoolsResponse{Items: pools}, httpSuccessResponse(), nil).
		Once()
}

func testPool(name string, ac *crusoeapi.KubernetesNodePoolAutoscalingConfig) crusoeapi.KubernetesNodePool {
	return crusoeapi.KubernetesNodePool{
		Id:                name + "-id",
		Name:              name,
		ClusterId:         testClusterID,
		State:             stateRunning,
		AutoscalingConfig: ac,
	}
}

func enabledConfig(minSize, maxSize int64) *crusoeapi.KubernetesNodePoolAutoscalingConfig {
	return &crusoeapi.KubernetesNodePoolAutoscalingConfig{Enabled: true, MinNodeSize: minSize, MaxNodeSize: maxSize}
}

func disabledConfig(minSize, maxSize int64) *crusoeapi.KubernetesNodePoolAutoscalingConfig {
	return &crusoeapi.KubernetesNodePoolAutoscalingConfig{Enabled: false, MinNodeSize: minSize, MaxNodeSize: maxSize}
}

func TestManager_RefreshBoundsFromAutoscalingConfig(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	// The pools also have --nodes entries with other bounds: the API wins.
	mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
		"listed-pool": {Name: "listed-pool", MinSize: 1, MaxSize: 2},
	}
	listed := testPool("listed-pool", enabledConfig(0, 5))
	// Not listed in --nodes at all: still autoscaled from the API.
	unlisted := testPool("unlisted-pool", enabledConfig(2, 7))

	listPoolsOnce(mocks, listed, unlisted)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, listed.Id).Return(listed, httpSuccessResponse(), nil)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, unlisted.Id).Return(unlisted, httpSuccessResponse(), nil)

	require.NoError(t, mgr.Refresh())

	nodeGroups := mgr.NodeGroups()
	require.Len(t, nodeGroups, 2)
	assert.Equal(t, "listed-pool", nodeGroups[0].pool.Name)
	assert.Equal(t, 0, nodeGroups[0].MinSize())
	assert.Equal(t, 5, nodeGroups[0].MaxSize())
	assert.Equal(t, "unlisted-pool", nodeGroups[1].pool.Name)
	assert.Equal(t, 2, nodeGroups[1].MinSize())
	assert.Equal(t, 7, nodeGroups[1].MaxSize())
}

// A disabled block is treated like no block: the pool follows --nodes, and any
// bounds the block retains are ignored.
func TestManager_RefreshDisabledFallsBackToFlags(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
		"disabled-pool":         {Name: "disabled-pool", MinSize: 1, MaxSize: 5},
		"disabled-at-zero-pool": {Name: "disabled-at-zero-pool", MinSize: 1, MaxSize: 10},
	}
	// Listed in --nodes, with retained bounds that differ from the flags.
	retained := testPool("disabled-pool", disabledConfig(2, 3))
	// Listed in --nodes, disabled at [0, 0].
	atZero := testPool("disabled-at-zero-pool", disabledConfig(0, 0))
	// Not listed in --nodes: not managed.
	unlisted := testPool("disabled-unlisted-pool", disabledConfig(1, 4))

	listPoolsOnce(mocks, retained, atZero, unlisted)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, retained.Id).Return(retained, httpSuccessResponse(), nil)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, atZero.Id).Return(atZero, httpSuccessResponse(), nil)

	require.NoError(t, mgr.Refresh())

	nodeGroups := mgr.NodeGroups()
	require.Len(t, nodeGroups, 2)
	assert.Equal(t, "disabled-pool", nodeGroups[0].pool.Name)
	assert.Equal(t, 1, nodeGroups[0].MinSize())
	assert.Equal(t, 5, nodeGroups[0].MaxSize())
	assert.Equal(t, "disabled-at-zero-pool", nodeGroups[1].pool.Name)
	assert.Equal(t, 1, nodeGroups[1].MinSize())
	assert.Equal(t, 10, nodeGroups[1].MaxSize())
}

func TestManager_RefreshWithoutAutoscalingConfigFallsBackToFlags(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
		"legacy-pool": {Name: "legacy-pool", MinSize: 1, MaxSize: 4},
	}
	legacy := testPool("legacy-pool", nil)
	unlisted := testPool("unlisted-pool", nil)

	listPoolsOnce(mocks, legacy, unlisted)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, legacy.Id).Return(legacy, httpSuccessResponse(), nil)

	require.NoError(t, mgr.Refresh())

	nodeGroups := mgr.NodeGroups()
	require.Len(t, nodeGroups, 1)
	assert.Equal(t, "legacy-pool", nodeGroups[0].pool.Name)
	assert.Equal(t, 1, nodeGroups[0].MinSize())
	assert.Equal(t, 4, nodeGroups[0].MaxSize())
}

func TestManager_RefreshSkipsInvalidAutoscalingConfig(t *testing.T) {
	mgr, mocks := testManagerWithMocks()
	// The --nodes entry does not rescue a pool with invalid enabled bounds.
	mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
		"min-above-max-pool": {Name: "min-above-max-pool", MinSize: 1, MaxSize: 4},
	}
	listPoolsOnce(mocks,
		testPool("min-above-max-pool", enabledConfig(5, 2)),
		testPool("negative-min-pool", enabledConfig(-1, 2)),
	)

	// Invalid bounds skip the pool but do not fail the refresh.
	require.NoError(t, mgr.Refresh())
	assert.Empty(t, mgr.NodeGroups())
}

// A pool enabled at [0, 0] registers as a node group pinned at zero rather
// than being skipped. CA keeps it inert: a scale-up needs target < MaxSize.
func TestManager_RefreshEnabledAtZeroRegistersInert(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	pool := testPool("parked-pool", enabledConfig(0, 0))

	listPoolsOnce(mocks, pool)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, pool.Id).Return(pool, httpSuccessResponse(), nil)

	require.NoError(t, mgr.Refresh())

	nodeGroups := mgr.NodeGroups()
	require.Len(t, nodeGroups, 1)
	assert.Equal(t, 0, nodeGroups[0].MinSize())
	assert.Equal(t, 0, nodeGroups[0].MaxSize())
	assert.Error(t, nodeGroups[0].IncreaseSize(1))
}

func TestManager_RefreshUpdatesBoundsAcrossRefreshes(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	before := testPool("autoscaled-pool", enabledConfig(1, 3))
	after := testPool("autoscaled-pool", enabledConfig(2, 6))

	listPoolsOnce(mocks, before)
	listPoolsOnce(mocks, after)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, before.Id).Return(before, httpSuccessResponse(), nil)

	require.NoError(t, mgr.Refresh())
	nodeGroups := mgr.NodeGroups()
	require.Len(t, nodeGroups, 1)
	assert.Equal(t, 1, nodeGroups[0].MinSize())
	assert.Equal(t, 3, nodeGroups[0].MaxSize())

	require.NoError(t, mgr.Refresh())
	refreshed := mgr.NodeGroups()
	require.Len(t, refreshed, 1)
	// Same cached node group, new bounds.
	assert.Same(t, nodeGroups[0], refreshed[0])
	assert.Equal(t, 2, refreshed[0].MinSize())
	assert.Equal(t, 6, refreshed[0].MaxSize())
}

// A cached node group is dropped when its pool is no longer managed: its
// enabled block turns invalid (even though the pool is listed in --nodes,
// since the API owns it), or its block is disabled and the pool is not listed
// in --nodes.
func TestManager_RefreshDropsCachedNodeGroup(t *testing.T) {
	for name, tc := range map[string]struct {
		next   *crusoeapi.KubernetesNodePoolAutoscalingConfig
		listed bool
	}{
		"enabled to invalid, listed":      {next: enabledConfig(4, 2), listed: true},
		"enabled to disabled, not listed": {next: disabledConfig(1, 3), listed: false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			mgr, mocks := testManagerWithMocks()
			if tc.listed {
				mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
					"autoscaled-pool": {Name: "autoscaled-pool", MinSize: 1, MaxSize: 4},
				}
			}
			pool := testPool("autoscaled-pool", enabledConfig(1, 3))

			listPoolsOnce(mocks, pool)
			listPoolsOnce(mocks, testPool("autoscaled-pool", tc.next))
			mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, pool.Id).Return(pool, httpSuccessResponse(), nil)

			require.NoError(t, mgr.Refresh())
			require.Len(t, mgr.NodeGroups(), 1)

			require.NoError(t, mgr.Refresh())
			assert.Empty(t, mgr.NodeGroups())
		})
	}
}

// When the block is disabled or the API stops serving it, a pool listed in
// --nodes falls back to its entry: the cached node group is kept and takes the
// flag bounds, not any bounds the disabled block retains.
func TestManager_RefreshFallsBackToFlagsWhenBlockDisabledOrRemoved(t *testing.T) {
	for name, next := range map[string]*crusoeapi.KubernetesNodePoolAutoscalingConfig{
		"block disabled": disabledConfig(0, 8),
		"block removed":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			mgr, mocks := testManagerWithMocks()
			mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
				"autoscaled-pool": {Name: "autoscaled-pool", MinSize: 1, MaxSize: 4},
			}
			withBlock := testPool("autoscaled-pool", enabledConfig(0, 8))

			listPoolsOnce(mocks, withBlock)
			listPoolsOnce(mocks, testPool("autoscaled-pool", next))
			mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, withBlock.Id).Return(withBlock, httpSuccessResponse(), nil)

			require.NoError(t, mgr.Refresh())
			nodeGroups := mgr.NodeGroups()
			require.Len(t, nodeGroups, 1)
			assert.Equal(t, 0, nodeGroups[0].MinSize())
			assert.Equal(t, 8, nodeGroups[0].MaxSize())

			require.NoError(t, mgr.Refresh())
			refreshed := mgr.NodeGroups()
			require.Len(t, refreshed, 1)
			assert.Same(t, nodeGroups[0], refreshed[0])
			assert.Equal(t, 1, refreshed[0].MinSize())
			assert.Equal(t, 4, refreshed[0].MaxSize())
		})
	}
}

// The invalid-bounds warning is logged when a pool's invalid bounds are first
// seen or change, not on every loop.
func TestManager_RefreshWarnsOncePerInvalidBoundsChange(t *testing.T) {
	// Only the WARNING output is captured: klog also copies each warning to
	// the INFO output, which would count it twice.
	var logs bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)
	klog.SetOutputBySeverity("WARNING", &logs)
	t.Cleanup(func() {
		klog.SetOutput(os.Stderr)
		klog.LogToStderr(true)
	})
	warnings := func() int {
		klog.Flush()
		n := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.HasPrefix(line, "W") && strings.Contains(line, "invalid autoscaling_config bounds") {
				n++
			}
		}
		return n
	}

	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	invalid := testPool("bad-pool", enabledConfig(5, 2))
	valid := testPool("bad-pool", enabledConfig(1, 2))
	listPoolsOnce(mocks, invalid)
	listPoolsOnce(mocks, invalid)
	listPoolsOnce(mocks, testPool("bad-pool", enabledConfig(6, 2)))
	listPoolsOnce(mocks, valid)
	listPoolsOnce(mocks, invalid)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, valid.Id).Return(valid, httpSuccessResponse(), nil)

	require.NoError(t, mgr.Refresh())
	assert.Equal(t, 1, warnings(), "first sighting")
	require.NoError(t, mgr.Refresh())
	assert.Equal(t, 1, warnings(), "unchanged bounds")
	require.NoError(t, mgr.Refresh())
	assert.Equal(t, 2, warnings(), "changed bounds")
	require.NoError(t, mgr.Refresh())
	assert.Equal(t, 2, warnings(), "valid bounds")
	assert.Len(t, mgr.NodeGroups(), 1)
	require.NoError(t, mgr.Refresh())
	assert.Equal(t, 3, warnings(), "invalid again after being valid")
	assert.Empty(t, mgr.NodeGroups())
}

// patchRecorder serves one node pool over HTTP and records the body of each
// PATCH it receives.
type patchRecorder struct {
	t    *testing.T
	path string
	pool []byte

	mu      sync.Mutex
	patches [][]byte
}

func (f *patchRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != f.path {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		_, _ = w.Write(f.pool)
	case http.MethodPatch:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("reading PATCH body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.patches = append(f.patches, body)
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"operation":{"operation_id":"resize-op","state":"IN_PROGRESS"}}`)
	default:
		f.t.Errorf("unexpected method %s", r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// TestManager_UpdateNodePoolRequestBody reads a pool through the generated
// client, as the node group's refresh does, resizes it, and asserts the exact
// JSON body of the PATCH.
func TestManager_UpdateNodePoolRequestBody(t *testing.T) {
	ctx := context.Background()
	// The pool as the API serves it, with every field the PATCH model
	// serializes without omitempty set away from its zero value.
	served := `{
		"id": "` + testNodePoolID + `",
		"project_id": "` + testProjectID + `",
		"cluster_id": "` + testClusterID + `",
		"name": "gpu-pool",
		"count": 3,
		"state": "STATE_RUNNING",
		"type": "h100-80gb-sxm-ib.8x",
		"image_id": "an-image-id",
		"instance_ids": ["vm1", "vm2", "vm3"],
		"ephemeral_storage_for_containerd": true,
		"node_labels": {"team": "ml", "env": "prod", "crusoe.ai/nodepool.id": "` + testNodePoolID + `"},
		"node_taints": [{"key": "gpu", "value": "true", "effect": "NoSchedule"}, {"key": "dedicated", "effect": "NoExecute"}],
		"reservation_id": "a-reservation-id",
		"ssh_public_key": "` + testSSHPublicKey + `",
		"autoscaling_config": {"enabled": true, "min_node_size": 1, "max_node_size": 10}
	}`
	fake := &patchRecorder{
		t:    t,
		path: "/v1alpha5/projects/" + testProjectID + "/kubernetes/nodepools/" + testNodePoolID,
		pool: []byte(served),
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg := crusoeapi.NewConfiguration()
	cfg.BasePath = srv.URL + "/v1alpha5"
	cfg.HTTPClient = srv.Client()
	mgr := &crusoeManager{
		nodePoolsApi: crusoeapi.NewAPIClient(cfg).KubernetesNodePoolsApi,
		projectID:    testProjectID,
		clusterID:    testClusterID,
	}

	pool, err := mgr.GetNodePool(ctx, testNodePoolID)
	require.NoError(t, err)
	require.Len(t, pool.NodeTaints, 2)

	op, err := mgr.UpdateNodePool(ctx, pool, 5)
	require.NoError(t, err)
	require.NotNil(t, op)
	assert.Equal(t, "resize-op", op.OperationId)

	require.Len(t, fake.patches, 1)
	assert.JSONEq(t, `{
		"action": "UPDATE",
		"count": 5,
		"ephemeral_storage_for_containerd": true,
		"node_labels": {"team": "ml", "env": "prod"},
		"node_pool_version": "",
		"node_taints": null,
		"reservation_id": "a-reservation-id",
		"ssh_public_key": "`+testSSHPublicKey+`"
	}`, string(fake.patches[0]))

	// autoscaling_config, consent_mode and update_settings are omitempty and
	// must never be sent by the autoscaler.
	var sent map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fake.patches[0], &sent))
	assert.NotContains(t, sent, "autoscaling_config")
	assert.NotContains(t, sent, "consent_mode")
	assert.NotContains(t, sent, "update_settings")
}
