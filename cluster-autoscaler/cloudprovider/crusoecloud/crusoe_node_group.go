/*
Copyright 2022 The Kubernetes Authors.

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
	"fmt"
	"time"

	crusoego "github.com/crusoecloud/client-go/swagger/v1alpha5"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/klog/v2"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	Min_NodePool_Size = 1
	Max_NodePool_Size = 32
)

// NodeGroup implements cloudprovider.NodeGroup interface.
// it is used to resize a Crusoe Managed Kubernetes (CMK) Pool which is a group of nodes with the same capacity.
type NodeGroup struct {
	*crusoego.APIClient
	pool *crusoego.KubernetesNodePool
}

// MaxSize returns maximum size of the node group.
func (ng *NodeGroup) MaxSize() int {
	klog.V(6).Info("MaxSize,called")

	return int(Max_NodePool_Size)
}

// MinSize returns minimum size of the node group.
func (ng *NodeGroup) MinSize() int {
	klog.V(6).Info("MinSize,called")

	return int(Min_NodePool_Size)
}

// TargetSize returns the current target size of the node group. It is possible that the
// number of nodes in Kubernetes is different at the moment but should be equal
// to Size() once everything stabilizes (new nodes finish startup and registration or
// removed nodes are deleted completely).
func (ng *NodeGroup) TargetSize() (int, error) {
	klog.V(6).Info("TargetSize,called")
	return int(ng.pool.Count), nil
}

// IncreaseSize increases the size of the node group. To delete a node you need
// to explicitly name it and use DeleteNode. This function should wait until
// node group size is updated.
func (ng *NodeGroup) IncreaseSize(delta int) error {

	klog.V(4).Infof("IncreaseSize,ClusterID=%s,delta=%d", ng.pool.ClusterId, delta)

	if delta <= 0 {
		return fmt.Errorf("delta must be strictly positive, have: %d", delta)
	}

	targetSize := ng.pool.Count + int64(delta)

	if targetSize > int64(ng.MaxSize()) {
		return fmt.Errorf("size increase is too large. current: %d desired: %d max: %d",
			ng.pool.Count, targetSize, ng.MaxSize())
	}

	ctx := context.Background()
	poolUpdateResp, _, err := ng.KubernetesNodePoolsApi.UpdateNodePool(ctx, crusoego.KubernetesNodePoolPatchRequest{
		Count: int64(targetSize),
	}, ng.pool.ProjectId, ng.pool.Id)
	if err != nil {
		return err
	}
	op := poolUpdateResp.Operation

	for op.State == string(opInProgress) {
		updatedOp, _, err := ng.KubernetesNodePoolOperationsApi.GetKubernetesNodePoolsOperation(ctx, ng.pool.ProjectId, poolUpdateResp.Operation.OperationId)
		if err != nil {
			return fmt.Errorf("failed waiting for nodepool operation: %w", err)
		}
		time.Sleep(2 * time.Second) // TODO: implement backoff

		op = &updatedOp
	}

	if op.State == string(opFailed) {
		// result := op.Result. .. need to marshal
		return fmt.Errorf("couldn't increase pool size to %d.",
			targetSize)
	}

	// fetch actual size?
	// if pool.Count != targetSize {
	// 	return fmt.Errorf("couldn't increase size to %d. Current size is: %d",
	// 		targetSize, pool.Size)
	// }

	ng.pool.Count = targetSize
	return nil
}

// AtomicIncreaseSize is not implemented.
func (ng *NodeGroup) AtomicIncreaseSize(delta int) error {
	return cloudprovider.ErrNotImplemented
}

// DeleteNodes deletes nodes from this node group. Error is returned either on
// failure or if the given node doesn't belong to this node group. This function
// should wait until node group size is updated.
func (ng *NodeGroup) DeleteNodes(nodes []*apiv1.Node) error {
	// ctx := context.Background()
	klog.V(4).Info("DeleteNodes,", len(nodes), " nodes to reclaim")
	// for _, n := range nodes {

	// 	node, ok := ng.nodes[n.Spec.ProviderID]
	// 	if !ok {
	// 		klog.Errorf("DeleteNodes,ProviderID=%s,PoolID=%s,node marked for deletion not found in pool", n.Spec.ProviderID, ng.p.ID)
	// 		continue
	// 	}

	// 	updatedNode, _, err := ng.VMsApi.DeleteInstance(ctx, ng.pool.ProjectId, node.ID)
	// 	if err != nil || updatedNode.Status != crusoego.NodeStatusDeleting {
	// 		return err
	// 	}

	// 	ng.p.Size--
	// 	ng.nodes[n.Spec.ProviderID].Status = crusoego.NodeStatusDeleting
	// }

	return nil
}

// DecreaseTargetSize decreases the target size of the node group. This function
// doesn't permit to delete any existing node and can be used only to reduce the
// request for new nodes that have not been yet fulfilled. Delta should be negative.
// It is assumed that cloud provider will not delete the existing nodes when there
// is an option to just decrease the target.
func (ng *NodeGroup) DecreaseTargetSize(delta int) error {

	klog.V(4).Infof("DecreaseTargetSize,ClusterID=%s,delta=%d", ng.pool.ClusterId, delta)

	if delta >= 0 {
		return fmt.Errorf("delta must be strictly negative, have: %d", delta)
	}

	targetSize := ng.pool.Count + int64(delta)
	if int(targetSize) < ng.MinSize() {
		return fmt.Errorf("size decrease is too large. current: %d desired: %d min: %d",
			ng.pool.Count, targetSize, ng.MinSize())
	}

	ctx := context.Background()
	_ /*resp*/, _, err := ng.KubernetesNodePoolsApi.UpdateNodePool(ctx, crusoego.KubernetesNodePoolPatchRequest{
		Count: targetSize,
	}, ng.pool.ProjectId, ng.pool.Id)
	if err != nil {
		return err
	}

	// check for completion ..
	// return fmt.Errorf("couldn't decrease size to %d. Current size is: %d",
	// 	targetSize, pool.Size)

	ng.pool.Count = targetSize
	return nil
}

// Id returns an unique identifier of the node group.
func (ng *NodeGroup) Id() string {
	return ng.pool.Id
}

// Debug returns a string containing all information regarding this node group.
func (ng *NodeGroup) Debug() string {
	klog.V(4).Info("Debug,called")
	return fmt.Sprintf("id:%s,status:%s,imageid:%s,size:%d,min_size:%d,max_size:%d", ng.Id(), ng.pool.State, ng.pool.ImageId, ng.pool.Count, ng.MinSize(), ng.MaxSize())
}

// Nodes returns a list of all nodes that belong to this node group.
func (ng *NodeGroup) Nodes() ([]cloudprovider.Instance, error) {
	klog.V(4).Info("Nodes,PoolID=", ng.pool.Id)
	return nil, cloudprovider.ErrNotImplemented
}

// TemplateNodeInfo returns a schedulerframework.NodeInfo structure of an empty
// (as if just started) node. This will be used in scale-up simulations to
// predict what would a new node look like if a node group was expanded. The returned
// NodeInfo is expected to have a fully populated Node object, with all of the labels,
// capacity and allocatable information as well as all pods that are started on
// the node by default, using manifest (most likely only kube-proxy).
func (ng *NodeGroup) TemplateNodeInfo() (*schedulerframework.NodeInfo, error) {
	klog.V(4).Infof("TemplateNodeInfo,PoolID=%s", ng.pool.Id)
	return nil, cloudprovider.ErrNotImplemented
}

// Exist checks if the node group really exists on the cloud provider side. Allows to tell the
// theoretical node group from the real one.
func (ng *NodeGroup) Exist() bool {

	klog.V(4).Infof("Exist,PoolID=%s", ng.pool.Id)

	_, _, err := ng.KubernetesNodePoolsApi.GetNodePool(context.Background(), ng.pool.ProjectId, ng.pool.Id)
	if err != nil {
		// log? any additional check?
		return false
	}
	return true

}

// Pool Autoprovision feature is not supported by Crusoe cloud yet

// Create creates the node group on the cloud provider side.
func (ng *NodeGroup) Create() (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrNotImplemented
}

// Delete deletes the node group on the cloud provider side.
func (ng *NodeGroup) Delete() error {
	return cloudprovider.ErrNotImplemented
}

// Autoprovisioned returns true if the node group is autoprovisioned.
func (ng *NodeGroup) Autoprovisioned() bool {
	return false
}

// GetOptions returns nil which means 'use defaults options'
func (ng *NodeGroup) GetOptions(defaults config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	return nil, cloudprovider.ErrNotImplemented
}
