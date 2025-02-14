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
