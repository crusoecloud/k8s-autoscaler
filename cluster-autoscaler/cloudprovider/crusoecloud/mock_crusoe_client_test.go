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
	"net/http"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/stretchr/testify/mock"
)

type crusoeMocks struct {
	nodePoolsApi   crusoeK8sNodePoolServiceMock
	nodePoolOpsApi crusoeK8sNodePoolOperationServiceMock
	vmApi          crusoeVMServiceMock
	vmOpsApi       crusoeVMOperationServiceMock
}

const (
	testProjectID  = "a_project_id"
	testClusterID  = "a_cluster_id"
	testNodePoolID = "a_nodepool_id"
)

func testManagerWithMocks() (*crusoeManager, *crusoeMocks) {
	client := &crusoeMocks{}
	return &crusoeManager{
		nodePoolsApi:   &client.nodePoolsApi,
		nodePoolOpsApi: &client.nodePoolOpsApi,
		vmApi:          &client.vmApi,
		vmOpsApi:       &client.vmOpsApi,
		waitBackoff:    newDefaultWaitBackoff(),

		projectID: testProjectID,
		clusterID: testClusterID,
	}, client
}

func httpSuccessResponse() *http.Response {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
	}
}

type crusoeK8sNodePoolServiceMock struct {
	mock.Mock
}

func (c *crusoeK8sNodePoolServiceMock) GetNodePool(ctx context.Context, projectId string, nodePoolId string) (crusoeapi.KubernetesNodePool, *http.Response, error) {
	args := c.Called(ctx, projectId, nodePoolId)
	return args.Get(0).(crusoeapi.KubernetesNodePool), args.Get(1).(*http.Response), args.Error(2)
}

func (c *crusoeK8sNodePoolServiceMock) ListNodePools(ctx context.Context, projectId string, listOptions *crusoeapi.KubernetesNodePoolsApiListNodePoolsOpts) (crusoeapi.ListKubernetesNodePoolsResponse, *http.Response, error) {
	args := c.Called(ctx, projectId, listOptions)
	return args.Get(0).(crusoeapi.ListKubernetesNodePoolsResponse), args.Get(1).(*http.Response), args.Error(2)
}

func (c *crusoeK8sNodePoolServiceMock) UpdateNodePool(ctx context.Context, body crusoeapi.KubernetesNodePoolPatchRequest, projectId string, nodePoolId string) (crusoeapi.AsyncOperationResponse, *http.Response, error) {
	args := c.Called(ctx, body, projectId, nodePoolId)
	return args.Get(0).(crusoeapi.AsyncOperationResponse), args.Get(1).(*http.Response), args.Error(2)
}

type crusoeK8sNodePoolOperationServiceMock struct {
	mock.Mock
}

func (c *crusoeK8sNodePoolOperationServiceMock) GetKubernetesNodePoolsOperation(ctx context.Context, projectId string, opId string) (crusoeapi.Operation, *http.Response, error) {
	args := c.Called(ctx, projectId, opId)
	return args.Get(0).(crusoeapi.Operation), args.Get(1).(*http.Response), args.Error(2)
}

type crusoeVMServiceMock struct {
	mock.Mock
}

func (c *crusoeVMServiceMock) DeleteInstance(ctx context.Context, projectId string, vmId string) (crusoeapi.AsyncOperationResponse, *http.Response, error) {
	args := c.Called(ctx, projectId, vmId)
	return args.Get(0).(crusoeapi.AsyncOperationResponse), args.Get(1).(*http.Response), args.Error(2)
}

func (c *crusoeVMServiceMock) ListInstances(ctx context.Context, projectId string, listOptions *crusoeapi.VMsApiListInstancesOpts) (crusoeapi.ListInstancesResponseV1Alpha5, *http.Response, error) {
	args := c.Called(ctx, projectId, listOptions)
	return args.Get(0).(crusoeapi.ListInstancesResponseV1Alpha5), args.Get(1).(*http.Response), args.Error(2)
}

type crusoeVMOperationServiceMock struct {
	mock.Mock
}

func (c *crusoeVMOperationServiceMock) GetComputeVMsInstancesOperation(ctx context.Context, projectId string, opId string) (crusoeapi.Operation, *http.Response, error) {
	args := c.Called(ctx, projectId, opId)
	return args.Get(0).(crusoeapi.Operation), args.Get(1).(*http.Response), args.Error(2)
}
