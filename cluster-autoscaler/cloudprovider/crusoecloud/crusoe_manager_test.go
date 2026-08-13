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
	"context"
	"testing"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/stretchr/testify/assert"

	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
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

func TestManager_RefreshBoundsFromScalingConfig(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	// pool also has a flag entry with different bounds: the API bounds must win
	mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
		"autoscaled-pool": {Name: "autoscaled-pool", MinSize: 1, MaxSize: 2},
	}

	pool := crusoeapi.KubernetesNodePool{
		Id:        "autoscaled-pool-id",
		Name:      "autoscaled-pool",
		ClusterId: testClusterID,
		State:     stateRunning,
		ScalingConfig: &crusoeapi.KubernetesNodePoolAutoscalingConfig{
			Enabled:     true,
			MinNodeSize: 0,
			MaxNodeSize: 5,
		},
	}

	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{pool},
			}, httpSuccessResponse(), nil,
		)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, "autoscaled-pool-id").
		Return(pool, httpSuccessResponse(), nil)

	err := mgr.Refresh()
	assert.NoError(t, err)

	nodeGroups := mgr.NodeGroups()
	assert.Len(t, nodeGroups, 1)
	assert.Equal(t, 0, nodeGroups[0].MinSize())
	assert.Equal(t, 5, nodeGroups[0].MaxSize())
}

func TestManager_RefreshSkipsPausedNodePools(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	// a flag entry must not resurrect a paused pool
	mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
		"paused-pool":        {Name: "paused-pool", MinSize: 1, MaxSize: 5},
		"parked-then-paused": {Name: "parked-then-paused", MinSize: 1, MaxSize: 10},
	}

	pausedPool := crusoeapi.KubernetesNodePool{
		Id:        "paused-pool-id",
		Name:      "paused-pool",
		ClusterId: testClusterID,
		State:     stateRunning,
		ScalingConfig: &crusoeapi.KubernetesNodePoolAutoscalingConfig{
			Enabled:     false,
			MinNodeSize: 2,
			MaxNodeSize: 3,
		},
	}
	// paused at [0, 0]: block presence alone marks it as configured — it must
	// be skipped, not mistaken for never-configured and handed to the flags
	// fallback (which would scale the deliberately parked pool back up)
	parkedThenPausedPool := crusoeapi.KubernetesNodePool{
		Id:        "parked-then-paused-id",
		Name:      "parked-then-paused",
		ClusterId: testClusterID,
		State:     stateRunning,
		ScalingConfig: &crusoeapi.KubernetesNodePoolAutoscalingConfig{
			Enabled:     false,
			MinNodeSize: 0,
			MaxNodeSize: 0,
		},
	}

	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{pausedPool, parkedThenPausedPool},
			}, httpSuccessResponse(), nil,
		)

	err := mgr.Refresh()
	assert.NoError(t, err)
	assert.Empty(t, mgr.NodeGroups())
}

func TestManager_RefreshNeverConfiguredFallsBackToFlags(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()
	mgr.nodeGroupSpecs = map[string]*dynamic.NodeGroupSpec{
		"legacy-pool": {Name: "legacy-pool", MinSize: 1, MaxSize: 4},
	}

	// never-configured pool as served by the gateway: no scaling_config block
	// at all (the gateway omits it when the pool's bounds were never set)
	legacyPool := crusoeapi.KubernetesNodePool{
		Id:        "legacy-pool-id",
		Name:      "legacy-pool",
		ClusterId: testClusterID,
		State:     stateRunning,
	}

	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{legacyPool},
			}, httpSuccessResponse(), nil,
		)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, "legacy-pool-id").
		Return(legacyPool, httpSuccessResponse(), nil)

	err := mgr.Refresh()
	assert.NoError(t, err)

	nodeGroups := mgr.NodeGroups()
	assert.Len(t, nodeGroups, 1)
	assert.Equal(t, 1, nodeGroups[0].MinSize())
	assert.Equal(t, 4, nodeGroups[0].MaxSize())
}

func TestManager_RefreshSkipsInvalidScalingConfig(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()

	minAboveMaxPool := crusoeapi.KubernetesNodePool{
		Id:        "min-above-max-pool-id",
		Name:      "min-above-max-pool",
		ClusterId: testClusterID,
		State:     stateRunning,
		ScalingConfig: &crusoeapi.KubernetesNodePoolAutoscalingConfig{
			Enabled:     true,
			MinNodeSize: 5,
			MaxNodeSize: 2,
		},
	}
	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{minAboveMaxPool},
			}, httpSuccessResponse(), nil,
		)

	// invalid bounds skip the pool but must not fail the refresh
	err := mgr.Refresh()
	assert.NoError(t, err)
	assert.Empty(t, mgr.NodeGroups())
}

// A pool enabled at [0, 0] is deliberately parked: it registers as a managed
// node group pinned at zero (visible in CA's status) rather than being
// skipped. CA's own math keeps it inert — scale-up requires target < MaxSize.
func TestManager_RefreshParkedAtZeroPoolRegistersInert(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()

	parkedPool := crusoeapi.KubernetesNodePool{
		Id:        "parked-pool-id",
		Name:      "parked-pool",
		ClusterId: testClusterID,
		State:     stateRunning,
		ScalingConfig: &crusoeapi.KubernetesNodePoolAutoscalingConfig{
			Enabled:     true,
			MinNodeSize: 0,
			MaxNodeSize: 0,
		},
	}

	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID,
		&crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{parkedPool},
			}, httpSuccessResponse(), nil,
		)
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, "parked-pool-id").
		Return(parkedPool, httpSuccessResponse(), nil)

	err := mgr.Refresh()
	assert.NoError(t, err)

	nodeGroups := mgr.NodeGroups()
	assert.Len(t, nodeGroups, 1)
	assert.Equal(t, 0, nodeGroups[0].MinSize())
	assert.Equal(t, 0, nodeGroups[0].MaxSize())
}

func TestManager_RefreshUpdatesBoundsAcrossRefreshes(t *testing.T) {
	ctx := context.Background()
	mgr, mocks := testManagerWithMocks()

	poolBefore := crusoeapi.KubernetesNodePool{
		Id:        "autoscaled-pool-id",
		Name:      "autoscaled-pool",
		ClusterId: testClusterID,
		State:     stateRunning,
		ScalingConfig: &crusoeapi.KubernetesNodePoolAutoscalingConfig{
			Enabled:     true,
			MinNodeSize: 1,
			MaxNodeSize: 3,
		},
	}
	poolAfter := poolBefore
	poolAfter.ScalingConfig = &crusoeapi.KubernetesNodePoolAutoscalingConfig{
		Enabled:     true,
		MinNodeSize: 2,
		MaxNodeSize: 6,
	}

	listOpts := &crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts{ClusterId: optional.NewString(testClusterID)}
	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID, listOpts).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{poolBefore},
			}, httpSuccessResponse(), nil,
		).Once()
	mocks.nodePoolsApi.On("ListNodePools", ctx, testProjectID, listOpts).
		Return(
			crusoeapi.ListKubernetesNodePoolsResponse{
				Items: []crusoeapi.KubernetesNodePool{poolAfter},
			}, httpSuccessResponse(), nil,
		).Once()
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, "autoscaled-pool-id").
		Return(poolBefore, httpSuccessResponse(), nil)

	err := mgr.Refresh()
	assert.NoError(t, err)
	nodeGroups := mgr.NodeGroups()
	assert.Len(t, nodeGroups, 1)
	assert.Equal(t, 1, nodeGroups[0].MinSize())
	assert.Equal(t, 3, nodeGroups[0].MaxSize())

	err = mgr.Refresh()
	assert.NoError(t, err)
	refreshedNodeGroups := mgr.NodeGroups()
	assert.Len(t, refreshedNodeGroups, 1)
	// same cached node group, updated bounds
	assert.Same(t, nodeGroups[0], refreshedNodeGroups[0])
	assert.Equal(t, 2, refreshedNodeGroups[0].MinSize())
	assert.Equal(t, 6, refreshedNodeGroups[0].MaxSize())
}
