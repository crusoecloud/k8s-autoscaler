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
	"testing"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Placeholder fake-nodes must resolve back to their node group: CA core aborts
// its whole loop on a created-node-with-errors it cannot attribute to a group.
func TestCloudProvider_ResolvesPlaceholderNodes(t *testing.T) {
	ng, _ := testNodeGroupWithMocks(5)
	ng.nodes = map[string]*crusoeapi.InstanceV1Alpha5{
		"real-vm": {Id: "real-vm", State: "RUNNING"},
	}
	ng.manager.nodeGroups = []*crusoeNodeGroup{ng}
	ccp := newCrusoeCloudProvider(ng.manager, nil)

	placeholderID := toProviderID(placeholderNodeID(testNodePoolID, 0))
	placeholderNode := &apiv1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: placeholderID},
		Spec:       apiv1.NodeSpec{ProviderID: placeholderID},
	}

	got, err := ccp.NodeGroupForNode(placeholderNode)
	assert.NoError(t, err)
	assert.Equal(t, ng, got)

	has, err := ccp.HasInstance(placeholderNode)
	assert.NoError(t, err)
	assert.True(t, has)

	// A placeholder belonging to a different pool resolves to nothing.
	foreign := &apiv1.Node{
		Spec: apiv1.NodeSpec{ProviderID: toProviderID(placeholderNodeID("some_other_pool", 0))},
	}
	got, err = ccp.NodeGroupForNode(foreign)
	assert.NoError(t, err)
	assert.Nil(t, got)

	has, err = ccp.HasInstance(foreign)
	assert.NoError(t, err)
	assert.False(t, has)
}
