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
	"testing"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
)

func testNodeGroupForPool(mgr *crusoeManager, poolID string, nodeIDs ...string) *crusoeNodeGroup {
	nodes := map[string]*crusoeapi.InstanceV1Alpha5{}
	for _, id := range nodeIDs {
		nodes[id] = &crusoeapi.InstanceV1Alpha5{Id: id}
	}
	return &crusoeNodeGroup{
		manager: mgr,
		pool: &crusoeapi.KubernetesNodePool{
			Id:        poolID,
			ProjectId: testProjectID,
			ClusterId: testClusterID,
			Count:     int64(len(nodeIDs)),
		},
		spec:                      testNodeSpec(),
		nodes:                     nodes,
		deletionInProgressNodeSet: map[string]struct{}{},
		targetSize:                len(nodeIDs),
	}
}

func TestCloudProvider_NodeGroupForNode(t *testing.T) {
	mgr, _ := testManagerWithMocks()
	ngA := testNodeGroupForPool(mgr, "pool-a", "node-a1")
	ngB := testNodeGroupForPool(mgr, "pool-b", "node-b1")
	mgr.nodeGroups = []*crusoeNodeGroup{ngA, ngB}
	ccp := newCrusoeCloudProvider(mgr, nil)

	t.Run("real node resolves through the cache", func(t *testing.T) {
		node := &apiv1.Node{Spec: apiv1.NodeSpec{ProviderID: toProviderID("node-b1")}}
		got, err := ccp.NodeGroupForNode(node)
		assert.NoError(t, err)
		if assert.NotNil(t, got) {
			assert.Equal(t, "pool-b", got.Id())
		}
	})

	t.Run("synthetic instance resolves to its owning group", func(t *testing.T) {
		// The cleanup path (deleteCreatedNodesWithErrors) resolves the fake
		// node via NodeGroupForNode before calling DeleteNodes. A nil result
		// makes the core error out and skip every RunOnce iteration while the
		// failure signal is up, so this mapping is load-bearing.
		node := &apiv1.Node{Spec: apiv1.NodeSpec{ProviderID: toProviderID(failedScaleUpInstanceID("pool-b"))}}
		got, err := ccp.NodeGroupForNode(node)
		assert.NoError(t, err)
		if assert.NotNil(t, got, "synthetic instance did not resolve to a node group") {
			assert.Equal(t, "pool-b", got.Id(), "synthetic instance must map to its own pool, not another group")
		}
	})

	t.Run("unknown node resolves to nil without error", func(t *testing.T) {
		node := &apiv1.Node{Spec: apiv1.NodeSpec{ProviderID: toProviderID("some-other-vm")}}
		got, err := ccp.NodeGroupForNode(node)
		assert.NoError(t, err)
		assert.Nil(t, got)
	})
}
