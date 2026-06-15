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
