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

	// Channel to signal that the async operation polling was invoked at least once.
	done := make(chan struct{})

	// Waiting for the resize to finish
	mocks.nodePoolOpsApi.
		On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").
		Return(
			crusoeapi.Operation{
				OperationId: "opId",
				State:       string(opSucceeded),
			},
			httpSuccessResponse(),
			nil,
		).
		Run(func(args mock.Arguments) {
			close(done)
		}).
		Once()

	// Now actually call IncreaseSize:
	err := ng.IncreaseSize(delta)
	assert.NoError(t, err)

	// Wait deterministically for the async polling goroutine to invoke GetKubernetesNodePoolsOperation.
	select {
	case <-done:
		// ok, background poll was executed
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for GetKubernetesNodePoolsOperation to be called")
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
