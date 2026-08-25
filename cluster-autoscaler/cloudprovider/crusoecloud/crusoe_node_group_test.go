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
	"testing"
	"time"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
)

func testNodeGroupWithMocks(count int) (*crusoeNodeGroup, *crusoeMocks) {
	mgr, mocks := testManagerWithMocks()
	return &crusoeNodeGroup{
		manager: mgr,
		pool: &crusoeapi.KubernetesNodePool{
			Id:        testNodePoolID,
			ProjectId: testProjectID,
			ClusterId: testClusterID,
			Count:     int64(count),
		},
		spec:                      testNodeSpec(),
		nodes:                     map[string]*crusoeapi.InstanceV1Alpha5{},
		deletionInProgressNodeSet: map[string]struct{}{},
		targetSize:                count,
	}, mocks
}

func testNodeSpec() *dynamic.NodeGroupSpec {
	return &dynamic.NodeGroupSpec{MinSize: 1, MaxSize: 10}
}

func TestNodeGroup_Debug(t *testing.T) {
	ng, _ := testNodeGroupWithMocks(3)

	d := ng.Debug()
	exp := "node group " + testNodePoolID + ": min=1 max=10 target=3"
	assert.Equal(t, exp, d, "debug string does not match")
}

func TestNodeGroup_TargetSize(t *testing.T) {
	nodes := 3
	ng, _ := testNodeGroupWithMocks(nodes)

	size, err := ng.TargetSize()
	assert.NoError(t, err)
	assert.Equal(t, nodes, size, "target size is wrong")
}

func TestNodeGroup_IncreaseSize(t *testing.T) {
	ctx := context.Background()
	curNumNodes := 3
	delta := 2
	ng, mocks := testNodeGroupWithMocks(curNumNodes)

	newNumNodes := int64(curNumNodes + delta)

	// The first refresh (before we call UpdateNodePool)
	// plus the second refresh (after the operation completes)
	// means we expect TWO calls to GetNodePool:
	mocks.nodePoolsApi.
		On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(
			crusoeapi.KubernetesNodePool{
				Id:          testNodePoolID,
				ProjectId:   testProjectID,
				ClusterId:   testClusterID,
				Count:       int64(curNumNodes), // current count before resize
				State:       stateRunning,
				InstanceIds: []string{"nodeId1", "nodeId2", "nodeId3"},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	mocks.nodePoolsApi.
		On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(
			crusoeapi.KubernetesNodePool{
				Id:          testNodePoolID,
				ProjectId:   testProjectID,
				ClusterId:   testClusterID,
				Count:       int64(newNumNodes), // current count before resize
				State:       stateRunning,
				InstanceIds: []string{"nodeId1", "nodeId2", "nodeId3", "nodeId4", "nodeId5"},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	// If refresh also calls ListInstances each time to gather node details,
	// then we expect TWO ListInstances calls, too:
	mocks.vmApi.
		On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
			Ids: optional.NewString("nodeId1,nodeId2,nodeId3"),
		}).
		Return(
			crusoeapi.ListInstancesResponseV1Alpha5{
				Items: []crusoeapi.InstanceV1Alpha5{
					{Id: "nodeId1", Name: "node1", ProjectId: testProjectID},
					{Id: "nodeId2", Name: "node2", ProjectId: testProjectID},
					{Id: "nodeId3", Name: "node3", ProjectId: testProjectID},
				},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()
	mocks.vmApi.
		On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
			Ids: optional.NewString("nodeId1,nodeId2,nodeId3,nodeId4,nodeId5"),
		}).
		Return(
			crusoeapi.ListInstancesResponseV1Alpha5{
				Items: []crusoeapi.InstanceV1Alpha5{
					{Id: "nodeId1", Name: "node1", ProjectId: testProjectID},
					{Id: "nodeId2", Name: "node2", ProjectId: testProjectID},
					{Id: "nodeId3", Name: "node3", ProjectId: testProjectID},
					{Id: "nodeId4", Name: "node4", ProjectId: testProjectID},
					{Id: "nodeId5", Name: "node5", ProjectId: testProjectID},
				},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	// The actual resize
	mocks.nodePoolsApi.
		On("UpdateNodePool",
			ctx,
			crusoeapi.KubernetesNodePoolPatchRequest{Count: newNumNodes},
			testProjectID,
			testNodePoolID,
		).
		Return(
			crusoeapi.AsyncOperationResponse{
				Operation: &crusoeapi.Operation{
					OperationId: "opId",
					State:       string(opInProgress),
				},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	// Waiting for the resize to finish. The operation is polled asynchronously
	// after IncreaseSize returns, with a timeout-wrapped context, so match the
	// context loosely and signal the test once the poll happens.
	opPolled := make(chan struct{})
	mocks.nodePoolOpsApi.
		On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").
		Run(func(args mock.Arguments) { close(opPolled) }).
		Return(
			crusoeapi.Operation{
				OperationId: "opId",
				State:       string(opSucceeded),
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	// Now actually call IncreaseSize:
	err := ng.IncreaseSize(delta)
	assert.NoError(t, err)

	select {
	case <-opPolled:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the async operation poll")
	}

	// Make sure all expectations were met
	mocks.nodePoolsApi.AssertExpectations(t)
	mocks.nodePoolOpsApi.AssertExpectations(t)
	mocks.vmApi.AssertExpectations(t)
}

func TestNodeGroup_IncreaseNegativeDelta(t *testing.T) {
	nodes := 3
	delta := -2
	ng, _ := testNodeGroupWithMocks(nodes)

	err := ng.IncreaseSize(delta)
	assert.Error(t, err)
}

func TestNodeGroup_IncreaseAboveMaximum(t *testing.T) {
	nodes := 3
	delta := 10
	ng, _ := testNodeGroupWithMocks(nodes)

	err := ng.IncreaseSize(delta)
	assert.Error(t, err)
}

func TestNodeGroup_DecreaseTargetSize(t *testing.T) {
	ctx := context.Background()
	nodes := 5
	delta := -4
	ng, mocks := testNodeGroupWithMocks(nodes)

	mocks.nodePoolsApi.
		On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(
			crusoeapi.KubernetesNodePool{
				Id:          testNodePoolID,
				ProjectId:   testProjectID,
				ClusterId:   testClusterID,
				Count:       int64(nodes), // current count before resize
				State:       stateRunning,
				InstanceIds: []string{"nodeId1", "nodeId2", "nodeId3", "nodeId4", "nodeId5"},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()
	mocks.nodePoolsApi.
		On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(
			crusoeapi.KubernetesNodePool{
				Id:          testNodePoolID,
				ProjectId:   testProjectID,
				ClusterId:   testClusterID,
				Count:       int64(nodes) + int64(delta),
				State:       stateRunning,
				InstanceIds: []string{"nodeId1", "nodeId2", "nodeId3", "nodeId4", "nodeId5"},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	mocks.vmApi.
		On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
			Ids: optional.NewString("nodeId1,nodeId2,nodeId3,nodeId4,nodeId5"),
		}).
		Return(
			crusoeapi.ListInstancesResponseV1Alpha5{
				Items: []crusoeapi.InstanceV1Alpha5{
					{Id: "nodeId1", Name: "node1", ProjectId: testProjectID},
					{Id: "nodeId2", Name: "node2", ProjectId: testProjectID},
					{Id: "nodeId3", Name: "node3", ProjectId: testProjectID},
					{Id: "nodeId4", Name: "node4", ProjectId: testProjectID},
					{Id: "nodeId5", Name: "node5", ProjectId: testProjectID},
				},
			},
			httpSuccessResponse(),
			nil,
		).
		Twice()

	newSize := int64(nodes + delta)
	mocks.nodePoolsApi.On("UpdateNodePool",
		ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{
			Count: newSize,
		},
		testProjectID,
		testNodePoolID,
	).Return(
		crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{
				OperationId: "opId",
				State:       string(opInProgress),
			},
		}, httpSuccessResponse(), nil,
	).Once()

	// The operation is polled with a timeout-wrapped context, so match it loosely.
	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").Return(
		crusoeapi.Operation{
			OperationId: "opId",
			State:       string(opSucceeded),
		}, httpSuccessResponse(), nil,
	).Once()

	err := ng.DecreaseTargetSize(delta)
	assert.NoError(t, err)
}

func TestNodeGroup_DecreaseTargetSizePositiveDelta(t *testing.T) {
	nodes := 5
	delta := 2
	ng, _ := testNodeGroupWithMocks(nodes)

	err := ng.DecreaseTargetSize(delta)
	assert.Error(t, err)
}

func TestNodeGroup_DecreaseBelowMinimum(t *testing.T) {
	nodes := 3
	delta := -3
	ng, _ := testNodeGroupWithMocks(nodes)

	err := ng.DecreaseTargetSize(delta)
	assert.Error(t, err)
}

func TestNodeGroup_DeleteNodes(t *testing.T) {
	ctx := context.Background()
	nodeCount := 3
	delta := -3
	ng, mocks := testNodeGroupWithMocks(nodeCount)
	ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
		"6852824b-e409-4c77-94df-819629d135b9": {Name: "np-12345-1", Id: "6852824b-e409-4c77-94df-819629d135b9"},
		"84acb1a6-0e14-4j36-8b32-71bf7b328c22": {Name: "np-12345-2", Id: "84acb1a6-0e14-4j36-8b32-71bf7b328c22"},
		"5c4d832a-d964-4c64-9d53-b9295c206cdd": {Name: "np-12345-3", Id: "5c4d832a-d964-4c64-9d53-b9295c206cdd"},
	}

	newSize := int64(nodeCount + delta)

	// The first refresh (before we call UpdateNodePool)
	// plus the second refresh (after the operation completes)
	// means we expect TWO calls to GetNodePool:
	mocks.nodePoolsApi.
		On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(
			crusoeapi.KubernetesNodePool{
				Id:        testNodePoolID,
				ProjectId: testProjectID,
				ClusterId: testClusterID,
				Count:     int64(nodeCount), // current count before resize
				State:     stateRunning,
				InstanceIds: []string{
					"6852824b-e409-4c77-94df-819629d135b9",
					"84acb1a6-0e14-4j36-8b32-71bf7b328c22",
					"5c4d832a-d964-4c64-9d53-b9295c206cdd"},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	mocks.nodePoolsApi.
		On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(
			crusoeapi.KubernetesNodePool{
				Id:          testNodePoolID,
				ProjectId:   testProjectID,
				ClusterId:   testClusterID,
				Count:       0, // current count before resize
				State:       stateRunning,
				InstanceIds: []string{},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	// If refresh also calls ListInstances each time to gather node details,
	// then we expect TWO ListInstances calls, too:
	mocks.vmApi.
		On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
			Ids: optional.NewString("6852824b-e409-4c77-94df-819629d135b9,84acb1a6-0e14-4j36-8b32-71bf7b328c22,5c4d832a-d964-4c64-9d53-b9295c206cdd"),
		}).
		Return(
			crusoeapi.ListInstancesResponseV1Alpha5{
				Items: []crusoeapi.InstanceV1Alpha5{
					{Id: "6852824b-e409-4c77-94df-819629d135b9", Name: "np-12345-1", ProjectId: testProjectID},
					{Id: "84acb1a6-0e14-4j36-8b32-71bf7b328c22", Name: "np-12345-2", ProjectId: testProjectID},
					{Id: "5c4d832a-d964-4c64-9d53-b9295c206cdd", Name: "np-12345-3", ProjectId: testProjectID},
				},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	mocks.nodePoolsApi.On("UpdateNodePool",
		ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{
			Count: newSize,
		},
		testProjectID,
		testNodePoolID,
	).Return(
		crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{
				OperationId: "opId",
				State:       string(opInProgress),
			},
		}, httpSuccessResponse(), nil,
	).Once()

	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").Return(
		crusoeapi.Operation{
			OperationId: "opId",
			State:       string(opSucceeded),
		}, httpSuccessResponse(), nil,
	).Once()

	nodes := []*apiv1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "np-12345-1.region.local"}, Spec: apiv1.NodeSpec{ProviderID: "crusoe://6852824b-e409-4c77-94df-819629d135b9"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "np-12345-2.region.local"}, Spec: apiv1.NodeSpec{ProviderID: "crusoe://84acb1a6-0e14-4j36-8b32-71bf7b328c22"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "np-12345-3.region.local"}, Spec: apiv1.NodeSpec{ProviderID: "crusoe://5c4d832a-d964-4c64-9d53-b9295c206cdd"}},
	}
	mocks.vmApi.On("DeleteInstance", ctx, testProjectID, "6852824b-e409-4c77-94df-819629d135b9").
		Return(crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{
				OperationId: "opId1",
				State:       string(opInProgress),
			},
		}, httpSuccessResponse(), nil,
		).Once()
	mocks.vmApi.On("DeleteInstance", ctx, testProjectID, "84acb1a6-0e14-4j36-8b32-71bf7b328c22").
		Return(crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{
				OperationId: "opId2",
				State:       string(opInProgress),
			},
		}, httpSuccessResponse(), nil,
		).Once()
	mocks.vmApi.On("DeleteInstance", ctx, testProjectID, "5c4d832a-d964-4c64-9d53-b9295c206cdd").
		Return(crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{
				OperationId: "opId3",
				State:       string(opInProgress),
			},
		}, httpSuccessResponse(), nil,
		).Once()

	mocks.vmOpsApi.On("GetComputeVMsInstancesOperation", mock.Anything, testProjectID, "opId1").
		Return(crusoeapi.Operation{OperationId: "opId1", State: string(opSucceeded)},
			httpSuccessResponse(), nil).Once()
	mocks.vmOpsApi.On("GetComputeVMsInstancesOperation", mock.Anything, testProjectID, "opId2").
		Return(crusoeapi.Operation{OperationId: "opId2", State: string(opSucceeded)},
			httpSuccessResponse(), nil).Once()
	mocks.vmOpsApi.On("GetComputeVMsInstancesOperation", mock.Anything, testProjectID, "opId3").
		Return(crusoeapi.Operation{OperationId: "opId3", State: string(opSucceeded)},
			httpSuccessResponse(), nil).Once()

	err := ng.DeleteNodes(nodes)
	assert.NoError(t, err)
	assert.Equal(t, int64(0), ng.pool.Count)
}

func TestNodeGroup_DeleteNodesNonExistent_Fail(t *testing.T) {
	ctx := context.Background()
	nodeCount := 1
	delta := -1
	ng, mocks := testNodeGroupWithMocks(nodeCount)
	ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
		"nonexistent-on-provider-side": {Id: "6852824b-e409-4c77-94df-819629d135b9"},
	}

	mocks.nodePoolsApi.
		On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(
			crusoeapi.KubernetesNodePool{
				Id:          testNodePoolID,
				ProjectId:   testProjectID,
				ClusterId:   testClusterID,
				Count:       0,
				State:       stateRunning,
				InstanceIds: []string{},
			},
			httpSuccessResponse(),
			nil,
		).
		Twice()

	newSize := int64(nodeCount + delta)
	mocks.nodePoolsApi.On("UpdateNodePool",
		ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{
			Count: newSize,
		},
		testProjectID,
		testNodePoolID,
	).Return(
		crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{
				OperationId: "opId",
				State:       string(opInProgress),
			},
		}, httpSuccessResponse(), nil,
	).Once()

	nodes := []*apiv1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "nonexistent-on-provider-side.local"}, Spec: apiv1.NodeSpec{ProviderID: "nonexistent-on-provider-side"}},
	}

	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").Return(
		crusoeapi.Operation{
			OperationId: "opId",
			State:       string(opSucceeded),
		}, httpSuccessResponse(), nil,
	).Once()

	mocks.vmApi.On("DeleteInstance", ctx, testProjectID, "6852824b-e409-4c77-94df-819629d135b9").
		Return(crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{
				OperationId: "opId1",
				State:       string(opInProgress),
			},
		}, httpSuccessResponse(), nil,
		).Once()

	mocks.vmOpsApi.On("GetComputeVMsInstancesOperation", mock.Anything, testProjectID, "opId1").
		Return(crusoeapi.Operation{OperationId: "opId1", State: string(opFailed)},
			httpSuccessResponse(), nil).Once()

	err := ng.DeleteNodes(nodes)
	assert.Error(t, err)
}

func TestNodeGroup_ExistRunning(t *testing.T) {
	ctx := context.Background()
	nodes := 2
	ng, mocks := testNodeGroupWithMocks(nodes)

	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).Return(
		crusoeapi.KubernetesNodePool{
			Id:          testNodePoolID,
			ProjectId:   testProjectID,
			ClusterId:   testClusterID,
			State:       stateRunning,
			InstanceIds: []string{"nodeId4", "nodeId5"},
		}, httpSuccessResponse(), nil,
	).Once()

	assert.True(t, ng.Exist())
}

func TestNodeGroup_ExistNotRunning(t *testing.T) {
	ctx := context.Background()
	nodes := 0
	ng, mocks := testNodeGroupWithMocks(nodes)

	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).Return(
		crusoeapi.KubernetesNodePool{
			Id:          testNodePoolID,
			ProjectId:   testProjectID,
			ClusterId:   testClusterID,
			State:       stateDeleting,
			InstanceIds: []string{},
		}, httpSuccessResponse(), nil,
	).Once()

	assert.False(t, ng.Exist())
}

func TestNodeGroup_SetTargetSizeClampsWhenNotConverging(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		wantTarget int
	}{
		{name: "running pool keeps the desired count", state: stateRunning, wantTarget: 10},
		{name: "unhealthy pool (v1) clamps to active nodes", state: stateUnhealthy, wantTarget: 2},
		{name: "degraded pool (v2) clamps to active nodes", state: stateDegraded, wantTarget: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ng, _ := testNodeGroupWithMocks(10)
			ng.pool.State = tt.state
			ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
				"vm-1": {Id: "vm-1", State: "RUNNING"},
				"vm-2": {Id: "vm-2", State: "RUNNING"},
			}

			ng.setTargetSizeLocked()

			assert.Equal(t, tt.wantTarget, ng.targetSize)
		})
	}
}

func TestNodeGroup_IncreaseSizeBelowStoredCountReturnsError(t *testing.T) {
	ctx := context.Background()
	ng, mocks := testNodeGroupWithMocks(3)

	// The refresh inside IncreaseSize discovers the pool's stored count is
	// already above the requested target (3+2=5 < 8). The provider must return
	// an error so CA core registers the failed scale-up and backs the group
	// off; returning nil would leave CA waiting max-node-provision-time for
	// nodes that were never requested.
	mocks.nodePoolsApi.
		On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(
			crusoeapi.KubernetesNodePool{
				Id:          testNodePoolID,
				ProjectId:   testProjectID,
				ClusterId:   testClusterID,
				Count:       8,
				State:       stateRunning,
				InstanceIds: []string{},
			},
			httpSuccessResponse(),
			nil,
		).
		Once()

	err := ng.IncreaseSize(2)

	assert.Error(t, err)
	mocks.nodePoolsApi.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func quotaIssueHealth() *crusoeapi.KubernetesNodePoolHealth {
	return &crusoeapi.KubernetesNodePoolHealth{
		Issues: []crusoeapi.KubernetesNodePoolHealthIssue{
			{Code: issueInsufficientQuota, Message: "Scaling up by 8 node(s) was denied by the project's quota"},
		},
	}
}

func TestNodeGroup_NodesSynthesizesPlaceholders(t *testing.T) {
	ng, _ := testNodeGroupWithMocks(10)
	ng.pool.State = stateRunning
	ng.pool.Health = quotaIssueHealth()
	ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
		"vm-1": {Id: "vm-1", State: "RUNNING"},
		"vm-2": {Id: "vm-2", State: "RUNNING"},
	}

	instances, err := ng.Nodes()
	assert.NoError(t, err)
	assert.Len(t, instances, 10, "2 real nodes plus 8 placeholders for the deficit")

	byID := map[string]cloudprovider.Instance{}
	for _, inst := range instances {
		byID[inst.Id] = inst
	}
	for i := 0; i < 8; i++ {
		inst, ok := byID[toProviderID(placeholderNodeID(testNodePoolID, i))]
		assert.True(t, ok, "placeholder %d missing", i)
		assert.Equal(t, cloudprovider.InstanceCreating, inst.Status.State)
		assert.Equal(t, cloudprovider.OutOfResourcesErrorClass, inst.Status.ErrorInfo.ErrorClass)
		assert.Equal(t, issueInsufficientQuota, inst.Status.ErrorInfo.ErrorCode)
		assert.Equal(t, "Scaling up by 8 node(s) was denied by the project's quota", inst.Status.ErrorInfo.ErrorMessage)
	}

	// IDs must be deterministic across refreshes: clusterstate dedups errored
	// instances by ID, and unstable IDs would reset the group's backoff every loop.
	again, err := ng.Nodes()
	assert.NoError(t, err)
	againIDs := map[string]struct{}{}
	for _, inst := range again {
		againIDs[inst.Id] = struct{}{}
	}
	for id := range byID {
		assert.Contains(t, againIDs, id)
	}
}

func TestNodeGroup_NodesPlaceholderGates(t *testing.T) {
	tests := []struct {
		name   string
		count  int
		state  string
		health *crusoeapi.KubernetesNodePoolHealth
	}{
		{
			name:  "no placeholders while an operation is in flight",
			count: 10, state: "STATE_UPDATING", health: quotaIssueHealth(),
		},
		{
			name:  "no placeholders without a health block",
			count: 10, state: stateRunning, health: nil,
		},
		{
			name:  "no placeholders for codes CA should not act on",
			count: 10, state: stateRunning,
			health: &crusoeapi.KubernetesNodePoolHealth{
				Issues: []crusoeapi.KubernetesNodePoolHealthIssue{
					{Code: "NODE_NOT_READY", Message: "1 node has been NotReady"},
					{Code: "INTERNAL_ERROR", Message: "platform-side failure"},
				},
			},
		},
		{
			name:  "no placeholders without a deficit",
			count: 2, state: stateRunning, health: quotaIssueHealth(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ng, _ := testNodeGroupWithMocks(tt.count)
			ng.pool.State = tt.state
			ng.pool.Health = tt.health
			ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
				"vm-1": {Id: "vm-1", State: "RUNNING"},
				"vm-2": {Id: "vm-2", State: "RUNNING"},
			}

			instances, err := ng.Nodes()
			assert.NoError(t, err)
			assert.Len(t, instances, 2, "only the real nodes must be reported")
		})
	}
}

// placeholderTestNode builds the fake node CA core hands to DeleteNodes for an
// errored placeholder instance (name and provider ID are both the instance ID).
func placeholderTestNode(index int) *apiv1.Node {
	id := toProviderID(placeholderNodeID(testNodePoolID, index))
	return &apiv1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: id},
		Spec:       apiv1.NodeSpec{ProviderID: id},
	}
}

func TestNodeGroup_DeleteNodesPlaceholdersLowersCountOnly(t *testing.T) {
	ctx := context.Background()
	ng, mocks := testNodeGroupWithMocks(5)

	// First refresh: the pool still wants 5 but holds 3, with a standing quota
	// issue — the 2-node deficit is what the placeholders represented.
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).Return(
		crusoeapi.KubernetesNodePool{
			Id: testNodePoolID, ProjectId: testProjectID, ClusterId: testClusterID,
			Count: 5, State: stateRunning, Health: quotaIssueHealth(),
			InstanceIds: []string{"r1", "r2", "r3"},
		}, httpSuccessResponse(), nil,
	).Once()
	// Second refresh, after the count write settled.
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).Return(
		crusoeapi.KubernetesNodePool{
			Id: testNodePoolID, ProjectId: testProjectID, ClusterId: testClusterID,
			Count: 3, State: stateRunning,
			InstanceIds: []string{"r1", "r2", "r3"},
		}, httpSuccessResponse(), nil,
	).Once()
	mocks.vmApi.On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString("r1,r2,r3"),
	}).Return(
		crusoeapi.ListInstancesResponseV1Alpha5{
			Items: []crusoeapi.InstanceV1Alpha5{{Id: "r1"}, {Id: "r2"}, {Id: "r3"}},
		}, httpSuccessResponse(), nil,
	).Twice()

	// Deleting the two placeholders gives up on the deficit: count 5 -> 3.
	mocks.nodePoolsApi.On("UpdateNodePool", ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{Count: 3},
		testProjectID, testNodePoolID,
	).Return(
		crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{OperationId: "opId", State: string(opInProgress)},
		}, httpSuccessResponse(), nil,
	).Once()
	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").Return(
		crusoeapi.Operation{OperationId: "opId", State: string(opSucceeded)},
		httpSuccessResponse(), nil,
	).Once()

	err := ng.DeleteNodes([]*apiv1.Node{placeholderTestNode(0), placeholderTestNode(1)})

	assert.NoError(t, err)
	assert.Equal(t, int64(3), ng.pool.Count)
	mocks.vmApi.AssertNotCalled(t, "DeleteInstance", mock.Anything, mock.Anything, mock.Anything)
	mocks.nodePoolsApi.AssertExpectations(t)
}

func TestNodeGroup_DeleteNodesStalePlaceholdersOnHealedPool(t *testing.T) {
	ctx := context.Background()
	ng, mocks := testNodeGroupWithMocks(3)

	// The platform filled the deficit between CA loops: the refresh inside
	// DeleteNodes finds no gap, so deleting the now-stale placeholders must
	// not shrink the healed pool.
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).Return(
		crusoeapi.KubernetesNodePool{
			Id: testNodePoolID, ProjectId: testProjectID, ClusterId: testClusterID,
			Count: 3, State: stateRunning,
			InstanceIds: []string{"r1", "r2", "r3"},
		}, httpSuccessResponse(), nil,
	).Twice()
	mocks.vmApi.On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString("r1,r2,r3"),
	}).Return(
		crusoeapi.ListInstancesResponseV1Alpha5{
			Items: []crusoeapi.InstanceV1Alpha5{{Id: "r1"}, {Id: "r2"}, {Id: "r3"}},
		}, httpSuccessResponse(), nil,
	).Twice()

	err := ng.DeleteNodes([]*apiv1.Node{placeholderTestNode(0), placeholderTestNode(1)})

	assert.NoError(t, err)
	assert.Equal(t, int64(3), ng.pool.Count)
	mocks.nodePoolsApi.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mocks.vmApi.AssertNotCalled(t, "DeleteInstance", mock.Anything, mock.Anything, mock.Anything)
}

func TestNodeGroup_DeleteNodesMixedBatch(t *testing.T) {
	ctx := context.Background()
	ng, mocks := testNodeGroupWithMocks(5)

	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).Return(
		crusoeapi.KubernetesNodePool{
			Id: testNodePoolID, ProjectId: testProjectID, ClusterId: testClusterID,
			Count: 5, State: stateRunning, Health: quotaIssueHealth(),
			InstanceIds: []string{"r1", "r2", "r3"},
		}, httpSuccessResponse(), nil,
	).Once()
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).Return(
		crusoeapi.KubernetesNodePool{
			Id: testNodePoolID, ProjectId: testProjectID, ClusterId: testClusterID,
			Count: 2, State: stateRunning,
			InstanceIds: []string{"r2", "r3"},
		}, httpSuccessResponse(), nil,
	).Once()
	mocks.vmApi.On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString("r1,r2,r3"),
	}).Return(
		crusoeapi.ListInstancesResponseV1Alpha5{
			Items: []crusoeapi.InstanceV1Alpha5{{Id: "r1"}, {Id: "r2"}, {Id: "r3"}},
		}, httpSuccessResponse(), nil,
	).Once()
	mocks.vmApi.On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString("r2,r3"),
	}).Return(
		crusoeapi.ListInstancesResponseV1Alpha5{
			Items: []crusoeapi.InstanceV1Alpha5{{Id: "r2"}, {Id: "r3"}},
		}, httpSuccessResponse(), nil,
	).Once()

	// One real node deleted plus the 2-node deficit given up: count 5 -> 2.
	mocks.nodePoolsApi.On("UpdateNodePool", ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{Count: 2},
		testProjectID, testNodePoolID,
	).Return(
		crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{OperationId: "opId", State: string(opInProgress)},
		}, httpSuccessResponse(), nil,
	).Once()
	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").Return(
		crusoeapi.Operation{OperationId: "opId", State: string(opSucceeded)},
		httpSuccessResponse(), nil,
	).Once()
	mocks.vmApi.On("DeleteInstance", ctx, testProjectID, "r1").Return(
		crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{OperationId: "vmOp1", State: string(opInProgress)},
		}, httpSuccessResponse(), nil,
	).Once()
	mocks.vmOpsApi.On("GetComputeVMsInstancesOperation", mock.Anything, testProjectID, "vmOp1").Return(
		crusoeapi.Operation{OperationId: "vmOp1", State: string(opSucceeded)},
		httpSuccessResponse(), nil,
	).Once()

	realNode := &apiv1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1.region.local"},
		Spec:       apiv1.NodeSpec{ProviderID: toProviderID("r1")},
	}

	err := ng.DeleteNodes([]*apiv1.Node{realNode, placeholderTestNode(0), placeholderTestNode(1)})

	assert.NoError(t, err)
	assert.Equal(t, int64(2), ng.pool.Count)
}
