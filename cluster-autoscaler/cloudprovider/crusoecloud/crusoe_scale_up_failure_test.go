/*
Copyright 2026 The Kubernetes Authors.

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
	"errors"
	"testing"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
)

func syntheticProviderID() string {
	return toProviderID(failedScaleUpInstanceID(testNodePoolID))
}

func findInstance(instances []cloudprovider.Instance, id string) *cloudprovider.Instance {
	for i := range instances {
		if instances[i].Id == id {
			return &instances[i]
		}
	}
	return nil
}

func TestNodeGroup_NodesEmitsSyntheticInstanceOnFailedScaleUp(t *testing.T) {
	ng, _ := testNodeGroupWithMocks(3)
	ng.pool.State = stateUnhealthy
	ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
		"nodeId1": {Id: "nodeId1", State: "RUNNING"},
		"nodeId2": {Id: "nodeId2", State: "RUNNING"},
	}

	instances, err := ng.Nodes()
	assert.NoError(t, err)
	assert.Len(t, instances, 3, "expected 2 real instances plus the synthetic one")

	synthetic := findInstance(instances, syntheticProviderID())
	if assert.NotNil(t, synthetic, "synthetic errored instance missing") {
		assert.Equal(t, cloudprovider.InstanceCreating, synthetic.Status.State)
		if assert.NotNil(t, synthetic.Status.ErrorInfo) {
			assert.Equal(t, cloudprovider.OutOfResourcesErrorClass, synthetic.Status.ErrorInfo.ErrorClass)
			assert.Equal(t, scaleUpFailureErrorCode, synthetic.Status.ErrorInfo.ErrorCode)
		}
	}

	// The synthetic instance must never land in the node cache.
	assert.Len(t, ng.nodes, 2)
}

func TestNodeGroup_NodesNoSyntheticInstance(t *testing.T) {
	cases := []struct {
		name  string
		count int
		state string
	}{
		{"healthy pool with gap (scale-up in flight)", 3, stateRunning},
		{"unhealthy pool without gap", 2, stateUnhealthy},
		{"healthy pool without gap", 2, stateRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ng, _ := testNodeGroupWithMocks(tc.count)
			ng.pool.State = tc.state
			ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
				"nodeId1": {Id: "nodeId1", State: "RUNNING"},
				"nodeId2": {Id: "nodeId2", State: "RUNNING"},
			}

			instances, err := ng.Nodes()
			assert.NoError(t, err)
			assert.Len(t, instances, 2)
			assert.Nil(t, findInstance(instances, syntheticProviderID()))
		})
	}
}

func TestNodeGroup_TargetSizeNotCollapsedWhenUnhealthy(t *testing.T) {
	ng, _ := testNodeGroupWithMocks(5)
	ng.pool.State = stateUnhealthy
	ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
		"nodeId1": {Id: "nodeId1"},
		"nodeId2": {Id: "nodeId2"},
		"nodeId3": {Id: "nodeId3"},
	}

	ng.setTargetSizeLocked()

	assert.Equal(t, 5, ng.targetSize,
		"target size must stay at pool.Count while the failure is pending, or the core never observes the scale-up")
}

func TestNodeGroup_DeleteNodesSyntheticReconcilesDesiredCount(t *testing.T) {
	ctx := context.Background()
	ng, mocks := testNodeGroupWithMocks(5)

	unhealthyPool := crusoeapi.KubernetesNodePool{
		Id:          testNodePoolID,
		ProjectId:   testProjectID,
		ClusterId:   testClusterID,
		Count:       5,
		State:       stateUnhealthy,
		InstanceIds: []string{"nodeId1", "nodeId2", "nodeId3"},
	}
	// The backend flips the pool HEALTHY asynchronously; right after the write
	// it may still read unhealthy, but the count is already reconciled.
	reconciledPool := unhealthyPool
	reconciledPool.Count = 3

	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(unhealthyPool, httpSuccessResponse(), nil).Once()
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(reconciledPool, httpSuccessResponse(), nil).Once()

	mocks.vmApi.On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString("nodeId1,nodeId2,nodeId3"),
	}).Return(crusoeapi.ListInstancesResponseV1Alpha5{
		Items: []crusoeapi.InstanceV1Alpha5{
			{Id: "nodeId1", State: "RUNNING"},
			{Id: "nodeId2", State: "RUNNING"},
			{Id: "nodeId3", State: "RUNNING"},
		},
	}, httpSuccessResponse(), nil).Twice()

	// The reconcile write: desired count lowered to the 3 nodes that exist.
	mocks.nodePoolsApi.On("UpdateNodePool", ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{Count: 3},
		testProjectID, testNodePoolID,
	).Return(crusoeapi.AsyncOperationResponse{
		Operation: &crusoeapi.Operation{OperationId: "opId", State: string(opInProgress)},
	}, httpSuccessResponse(), nil).Once()

	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").
		Return(crusoeapi.Operation{OperationId: "opId", State: string(opSucceeded)}, httpSuccessResponse(), nil).Once()

	err := ng.DeleteNodes([]*apiv1.Node{{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-scale-up"},
		Spec:       apiv1.NodeSpec{ProviderID: syntheticProviderID()},
	}})

	assert.NoError(t, err)
	assert.Equal(t, int64(3), ng.pool.Count)
	assert.Equal(t, 3, ng.targetSize)
	// The synthetic instance has no VM behind it; nothing may be terminated.
	mocks.vmApi.AssertNotCalled(t, "DeleteInstance", mock.Anything, mock.Anything, mock.Anything)
	mocks.nodePoolsApi.AssertExpectations(t)
}

func TestNodeGroup_DeleteNodesSyntheticPlusRealNode(t *testing.T) {
	ctx := context.Background()
	ng, mocks := testNodeGroupWithMocks(5)

	unhealthyPool := crusoeapi.KubernetesNodePool{
		Id:          testNodePoolID,
		ProjectId:   testProjectID,
		ClusterId:   testClusterID,
		Count:       5,
		State:       stateUnhealthy,
		InstanceIds: []string{"nodeId1", "nodeId2", "nodeId3"},
	}
	reconciledPool := unhealthyPool
	reconciledPool.Count = 2
	reconciledPool.InstanceIds = []string{"nodeId1", "nodeId2"}

	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(unhealthyPool, httpSuccessResponse(), nil).Once()
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(reconciledPool, httpSuccessResponse(), nil).Once()

	mocks.vmApi.On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString("nodeId1,nodeId2,nodeId3"),
	}).Return(crusoeapi.ListInstancesResponseV1Alpha5{
		Items: []crusoeapi.InstanceV1Alpha5{
			{Id: "nodeId1", State: "RUNNING"},
			{Id: "nodeId2", State: "RUNNING"},
			{Id: "nodeId3", State: "RUNNING"},
		},
	}, httpSuccessResponse(), nil).Once()
	mocks.vmApi.On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString("nodeId1,nodeId2"),
	}).Return(crusoeapi.ListInstancesResponseV1Alpha5{
		Items: []crusoeapi.InstanceV1Alpha5{
			{Id: "nodeId1", State: "RUNNING"},
			{Id: "nodeId2", State: "RUNNING"},
		},
	}, httpSuccessResponse(), nil).Once()

	// 3 active nodes minus 1 real node deleted in the same call = desired 2.
	mocks.nodePoolsApi.On("UpdateNodePool", ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{Count: 2},
		testProjectID, testNodePoolID,
	).Return(crusoeapi.AsyncOperationResponse{
		Operation: &crusoeapi.Operation{OperationId: "opId", State: string(opInProgress)},
	}, httpSuccessResponse(), nil).Once()
	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").
		Return(crusoeapi.Operation{OperationId: "opId", State: string(opSucceeded)}, httpSuccessResponse(), nil).Once()

	// The real node is terminated; the synthetic one is not.
	mocks.vmApi.On("DeleteInstance", ctx, testProjectID, "nodeId3").
		Return(crusoeapi.AsyncOperationResponse{
			Operation: &crusoeapi.Operation{OperationId: "vmOpId", State: string(opInProgress)},
		}, httpSuccessResponse(), nil).Once()
	// Polled from a background goroutine after DeleteNodes returns.
	mocks.vmOpsApi.On("GetComputeVMsInstancesOperation", mock.Anything, testProjectID, "vmOpId").
		Return(crusoeapi.Operation{OperationId: "vmOpId", State: string(opSucceeded)}, httpSuccessResponse(), nil).Maybe()

	err := ng.DeleteNodes([]*apiv1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "failed-scale-up"},
			Spec:       apiv1.NodeSpec{ProviderID: syntheticProviderID()},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node3.region.local"},
			Spec:       apiv1.NodeSpec{ProviderID: toProviderID("nodeId3")},
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, int64(2), ng.pool.Count)
	mocks.nodePoolsApi.AssertExpectations(t)
	mocks.vmApi.AssertExpectations(t)
}

func TestNodeGroup_DeleteNodesSyntheticReconcilesBelowMinSize(t *testing.T) {
	// The failure left the pool with zero nodes while MinSize is 1. Reconcile
	// must write the truth (0) rather than clamp up to min — clamping would
	// re-request the exact capacity that just failed.
	ctx := context.Background()
	ng, mocks := testNodeGroupWithMocks(2)

	emptyUnhealthyPool := crusoeapi.KubernetesNodePool{
		Id:          testNodePoolID,
		ProjectId:   testProjectID,
		ClusterId:   testClusterID,
		Count:       2,
		State:       stateUnhealthy,
		InstanceIds: []string{},
	}
	reconciledPool := emptyUnhealthyPool
	reconciledPool.Count = 0

	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(emptyUnhealthyPool, httpSuccessResponse(), nil).Once()
	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(reconciledPool, httpSuccessResponse(), nil).Once()

	mocks.nodePoolsApi.On("UpdateNodePool", ctx,
		crusoeapi.KubernetesNodePoolPatchRequest{Count: 0},
		testProjectID, testNodePoolID,
	).Return(crusoeapi.AsyncOperationResponse{
		Operation: &crusoeapi.Operation{OperationId: "opId", State: string(opInProgress)},
	}, httpSuccessResponse(), nil).Once()
	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").
		Return(crusoeapi.Operation{OperationId: "opId", State: string(opSucceeded)}, httpSuccessResponse(), nil).Once()

	err := ng.DeleteNodes([]*apiv1.Node{{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-scale-up"},
		Spec:       apiv1.NodeSpec{ProviderID: syntheticProviderID()},
	}})

	assert.NoError(t, err)
	assert.Equal(t, int64(0), ng.pool.Count)
	mocks.nodePoolsApi.AssertExpectations(t)
}

func TestNodeGroup_DeleteNodesSyntheticNoopWhenPoolRecovered(t *testing.T) {
	// The pool recovered between the signal being observed and cleanup running
	// (count already matches the active nodes). Cleanup must be a no-op.
	ctx := context.Background()
	ng, mocks := testNodeGroupWithMocks(3)

	healthyPool := crusoeapi.KubernetesNodePool{
		Id:          testNodePoolID,
		ProjectId:   testProjectID,
		ClusterId:   testClusterID,
		Count:       3,
		State:       stateRunning,
		InstanceIds: []string{"nodeId1", "nodeId2", "nodeId3"},
	}

	mocks.nodePoolsApi.On("GetNodePool", ctx, testProjectID, testNodePoolID).
		Return(healthyPool, httpSuccessResponse(), nil).Twice()
	mocks.vmApi.On("ListInstances", ctx, testProjectID, &crusoeapi.VMsApiListInstancesOpts{
		Ids: optional.NewString("nodeId1,nodeId2,nodeId3"),
	}).Return(crusoeapi.ListInstancesResponseV1Alpha5{
		Items: []crusoeapi.InstanceV1Alpha5{
			{Id: "nodeId1", State: "RUNNING"},
			{Id: "nodeId2", State: "RUNNING"},
			{Id: "nodeId3", State: "RUNNING"},
		},
	}, httpSuccessResponse(), nil).Twice()

	err := ng.DeleteNodes([]*apiv1.Node{{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-scale-up"},
		Spec:       apiv1.NodeSpec{ProviderID: syntheticProviderID()},
	}})

	assert.NoError(t, err)
	assert.Equal(t, int64(3), ng.pool.Count)
	mocks.nodePoolsApi.AssertNotCalled(t, "UpdateNodePool", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	mocks.vmApi.AssertNotCalled(t, "DeleteInstance", mock.Anything, mock.Anything, mock.Anything)
}

func TestNodeGroup_TrackIncreaseSizeAsyncWaitErrorDoesNotPanic(t *testing.T) {
	// A failed poll makes WaitForNodePoolOperationComplete return a nil op;
	// trackIncreaseSizeAsync must not dereference it (it runs in a goroutine,
	// so a panic here would crash the whole autoscaler).
	ng, mocks := testNodeGroupWithMocks(3)

	mocks.nodePoolOpsApi.On("GetKubernetesNodePoolsOperation", mock.Anything, testProjectID, "opId").
		Return(crusoeapi.Operation{}, httpSuccessResponse(), errors.New("transient poll failure")).Once()

	op := &crusoeapi.Operation{OperationId: "opId", State: string(opInProgress)}
	assert.NotPanics(t, func() { ng.trackIncreaseSizeAsync(testNodePoolID, op) })
}
