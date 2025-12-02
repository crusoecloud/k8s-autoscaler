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
	"fmt"
	"sync"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"go.uber.org/multierr"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/klog/v2"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	defaultMinNodePoolSize = 1
	defaultMaxNodePoolSize = 254

	instanceBatchSize = 50 // page instance fetch by this size
)

// crusoeNodeGroup implements cloudprovider.NodeGroup interface. It contains
// configuration info and functions to control a CrusoeCloud Managed Kubernetes (CMK)
// NodePool, which is a set of nodes that have the same capacity and set of labels.
type crusoeNodeGroup struct {
	manager *crusoeManager

	pool                      *crusoeapi.KubernetesNodePool
	nodeGroupRWMutex          sync.RWMutex
	nodes                     map[string]*crusoeapi.InstanceV1Alpha5
	deletionInProgressNodeSet map[string]struct{}
	targetSize                int

	scalingMutex sync.Mutex

	spec *dynamic.NodeGroupSpec
}

// MaxSize returns maximum size of the node group.
func (ng *crusoeNodeGroup) MaxSize() int {
	if ng.spec != nil {
		return ng.spec.MaxSize
	}
	return int(defaultMaxNodePoolSize)
}

// MinSize returns minimum size of the node group.
func (ng *crusoeNodeGroup) MinSize() int {
	if ng.spec != nil {
		return ng.spec.MinSize
	}
	return int(defaultMinNodePoolSize)
}

// TargetSize returns the current target size of the node group. It is possible that the
// number of nodes in Kubernetes is different at the moment but should be equal
// to Size() once everything stabilizes (new nodes finish startup and registration or
// removed nodes are deleted completely).
func (ng *crusoeNodeGroup) TargetSize() (int, error) {
	return ng.targetSize, nil
}

// IncreaseSize increases the size of the node group. To delete a node you need
// to explicitly name it and use DeleteNode. This function should wait until
// node group size is updated.
func (ng *crusoeNodeGroup) IncreaseSize(delta int) error {
	ctx := context.Background()

	if delta <= 0 {
		return fmt.Errorf("delta must be strictly positive, have: %d", delta)
	}

	ng.scalingMutex.Lock()
	defer ng.scalingMutex.Unlock()

	klog.V(4).Infof("IncreaseSize (delta = %d) for node pool with id %s", delta, ng.pool.ClusterId)

	targetSize := int64(ng.targetSize + delta)
	if targetSize > int64(ng.MaxSize()) {
		return fmt.Errorf("size increase is too large. current: %d desired: %d max: %d",
			ng.targetSize, targetSize, ng.MaxSize())
	}
	err := ng.refresh()
	if err != nil {
		klog.Errorf("IncreaseSize,PoolID=%s, failed to refresh node group before attempting to increase size: %v", ng.pool.Id, err)
		return fmt.Errorf("failed to refresh node group before attempting to increase size: %v", err)
	}
	if targetSize < ng.pool.Count {
		klog.Errorf("IncreaseSize,PoolID=%s, aborting IncreaseSize. "+
			"Current node pool count on Crusoe Cloud already exceeds node group's target size", ng.Id())
		return nil
	}

	op, err := ng.manager.UpdateNodePool(ctx, ng.pool.Id, targetSize)
	if err != nil {
		klog.Errorf("IncreaseSize,PoolID=%s, failed trying to set target nodepool size to %d: %v", ng.pool.Id, targetSize, err)
		return err
	}

	refreshErr := ng.refresh()
	if refreshErr != nil {
		klog.Errorf("IncreaseSize (background),PoolID=%s, failed to refresh node group: %v", ng.Id(), refreshErr)
	}

	// target size has already updated so waiting for vms to be created can happen asynchronously
	go ng.trackIncreaseSizeAsync(ng.pool.Id, op)

	return nil
}

func (ng *crusoeNodeGroup) trackIncreaseSizeAsync(poolID string, op *crusoeapi.Operation) {
	ctx := context.Background()
	klog.V(5).Infof("IncreaseSize (background): waiting for opID=%s on poolID=%s", op.OperationId, poolID)

	finalOp, waitErr := ng.manager.WaitForNodePoolOperationComplete(ctx, op)
	if waitErr != nil {
		klog.Errorf("IncreaseSize (background),PoolID=%s, failed waiting for opID=%s: %v", poolID, op.OperationId, waitErr)
	}

	if finalOp.State == string(opFailed) {
		klog.Errorf("IncreaseSize (background),PoolID=%s, opID=%s failed: %v", poolID, op.OperationId, finalOp.Result)
	}
}

// AtomicIncreaseSize is not implemented.
func (ng *crusoeNodeGroup) AtomicIncreaseSize(delta int) error {
	return cloudprovider.ErrNotImplemented
}

// DeleteNodes deletes nodes from this node group. Error is returned either on
// failure or if the given node doesn't belong to this node group. This function
// should wait until node group size is updated.
func (ng *crusoeNodeGroup) DeleteNodes(nodes []*apiv1.Node) error {
	ctx := context.Background()

	ng.scalingMutex.Lock()
	scalingMutexUnlocked := false
	defer func() {
		if !scalingMutexUnlocked {
			ng.scalingMutex.Unlock()
		}
	}()

	err := ng.refresh()
	if err != nil {
		klog.Errorf("DeleteNodes,PoolID=%s, failed to refresh node group before attempting to delete nodes: %v", ng.pool.Id, err)
		return fmt.Errorf("failed to refresh node group before attempting to delete nodes: %v", err)
	}

	nodeIDsToDelete := []string{}
	ng.nodeGroupRWMutex.RLock()
	for _, n := range nodes {
		nodeInfo, ok := ng.nodes[toNodeID(n.Spec.ProviderID)]
		if !ok {
			klog.Errorf("DeleteNodes,Name=%s,PoolID=%s,node marked for deletion not found in pool", n.Name, ng.pool.Id)
			return fmt.Errorf("failed to find node %s (id=%s) in the node group's nodes cache", n.Name, toNodeID(n.Spec.ProviderID))
		}

		nodeIDsToDelete = append(nodeIDsToDelete, nodeInfo.Id)
	}
	ng.nodeGroupRWMutex.RUnlock()

	targetSize := min(ng.targetSize-len(nodeIDsToDelete), int(ng.pool.Count))
	klog.V(4).Infof("DeleteNodes,%d nodes to reclaim (%d target size); ng=%v, pool id=%v", len(nodes), targetSize, ng, ng.pool.Id)
	if targetSize >= int(ng.pool.Count) {
		klog.V(4).Infof("DeleteNodes,PoolID=%s, new target size (%d) greater than or equal to the desired count (%d), skip updating desired count",
			ng.pool.Id, targetSize, ng.pool.Count,
		)
	} else {
		klog.V(4).Infof("DeleteNodes,PoolID=%s, new target size (%d) lower than desired count (%d), setting desired count to match target size",
			ng.pool.Id, targetSize, ng.pool.Count,
		)
		ngOp, err := ng.manager.UpdateNodePool(ctx, ng.pool.Id, int64(targetSize))
		if err != nil {
			klog.Errorf("DeleteNodes,PoolID=%s, failed trying to set target nodepool size to %d: %v", ng.pool.Id, targetSize, err)
			return err
		}

		ngOp, err = ng.manager.WaitForNodePoolOperationComplete(ctx, ngOp)
		if err != nil {
			klog.Errorf("DeleteNodes,PoolID=%s, failed waiting to set target nodepool size to %d: %v", ng.pool.Id, targetSize, err)
			return fmt.Errorf("couldn't decrease pool size to %d: %w", targetSize, err)
		}
		if ngOp.State == string(opFailed) {
			klog.Errorf("DeleteNodes,PoolID=%s, failed to set target nodepool size to %d: %v", ng.pool.Id, targetSize, ngOp.Result)
			return fmt.Errorf("couldn't decrease pool size to %d: operation failed with %v", targetSize, ngOp.Result)
		}
	}

	// group errors onward into a multiErr to try to wait until vm operation(s) complete before removing
	// nodes from the deletion in progress set
	var multiErr error

	vmOps := make([]*crusoeapi.Operation, 0, len(nodeIDsToDelete))
	nodesInDeletionSet := make([]string, len(nodeIDsToDelete))
	for _, id := range nodeIDsToDelete {
		op, err := ng.manager.DeleteVMInstance(ctx, id)
		if err != nil {
			klog.Errorf("DeleteNodes,PoolID=%s, failed to delete node %s: %v",
				ng.pool.Id, id, err)
			multiErr = multierr.Append(multiErr, fmt.Errorf("failed to delete node %s: %v", id, err))
			continue
		}
		ng.addNodeToDeletionInProgressSet(id)
		if op != nil {
			vmOps = append(vmOps, op)
		} else {
			klog.Errorf("DeleteNodes,PoolID=%s, returned delete operation is nil for instance with id %s", ng.pool.Id, id)
		}
		nodesInDeletionSet = append(nodesInDeletionSet, id)
	}

	err = ng.refresh()
	if err != nil {
		klog.Errorf("DeleteNodes,PoolID=%s, failed to refresh node group after delete nodes: %v", ng.pool.Id, err)
		multiErr = multierr.Append(multiErr, fmt.Errorf("failed to refresh node group after delete nodes: %v", err))
	}

	scalingMutexUnlocked = true
	ng.scalingMutex.Unlock()

	go func() {
		// target size has already updated so waiting for vm operations can happen asynchronously
		_, err = ng.manager.WaitForVMOperationListComplete(ctx, vmOps)
		if err != nil {
			klog.Errorf("DeleteNodes (background),failed to delete one or more nodes: %v", err)
		}
		for _, id := range nodesInDeletionSet {
			ng.removeNodeFromDeletionInProgressSet(id)
		}
	}()

	return multiErr
}

// DecreaseTargetSize decreases the target size of the node group. This function
// doesn't permit to delete any existing node and can be used only to reduce the
// request for new nodes that have not been yet fulfilled. Delta should be negative.
// It is assumed that cloud provider will not delete the existing nodes when there
// is an option to just decrease the target.
func (ng *crusoeNodeGroup) DecreaseTargetSize(delta int) error {
	klog.V(4).Infof("DecreaseTargetSize,ClusterID=%s,delta=%d", ng.pool.ClusterId, delta)

	if delta >= 0 {
		return fmt.Errorf("delta must be strictly negative, have: %d", delta)
	}

	ng.scalingMutex.Lock()
	defer ng.scalingMutex.Unlock()

	klog.V(4).Infof("DecreaseTargetSize (delta = %d) for node pool with id %s", delta, ng.pool.ClusterId)

	targetSize := int64(ng.targetSize + delta)
	if int(targetSize) < ng.MinSize() {
		return fmt.Errorf("size decrease is too large. current: %d desired: %d min: %d",
			ng.targetSize, targetSize, ng.MinSize())
	}

	ctx := context.Background()

	err := ng.refresh()
	if err != nil {
		klog.Errorf("DecreaseTargetSize,PoolID=%s, failed to refresh node group before attempting to decrease target size: %v", ng.pool.Id, err)
		return fmt.Errorf("failed to refresh node group before attempting to decrease target size: %v", err)
	}
	if targetSize > ng.pool.Count {
		klog.Errorf("DecreaseTargetSize,PoolID=%s, aborting DecreaseTargetSize. "+
			"The existing node pool size (%d) is already lower than or equal to the requested target (%d).",
			ng.Id(), ng.pool.Count, targetSize)
		return nil
	}

	ngOp, err := ng.manager.UpdateNodePool(ctx, ng.pool.Id, targetSize)
	if err != nil {
		klog.Errorf("DecreaseTargetSize,PoolID=%s, failed trying to set target nodepool size to %d: %v", ng.pool.Id, targetSize, err)
		return err
	}

	ngOp, err = ng.manager.WaitForNodePoolOperationComplete(ctx, ngOp)
	if err != nil {
		klog.Errorf("DecreaseTargetSize,PoolID=%s, failed waiting to set target nodepool size to %d: %v", ng.pool.Id, targetSize, err)
		return fmt.Errorf("couldn't decrease pool size to %d: %w", targetSize, err)
	}
	if ngOp.State == string(opFailed) {
		klog.Errorf("DecreaseTargetSize,PoolID=%s, failed to set target nodepool size to %d: operation failed with %v", ng.pool.Id, targetSize, ngOp.Result)
		return fmt.Errorf("couldn't decrease pool size to %d: operation failed with %v", targetSize, ngOp.Result)
	}

	err = ng.refresh()
	if err != nil {
		klog.Errorf("DecreaseTargetSize,PoolID=%s, failed to refresh node group after delete nodes: %v", ng.pool.Id, err)
		return fmt.Errorf("failed to refresh node group after delete nodes: %v", err)
	}

	return nil
}

// Id returns an unique identifier of the node group.
func (ng *crusoeNodeGroup) Id() string {
	return ng.pool.Id
}

// Debug returns a string containing all information regarding this node group.
func (ng *crusoeNodeGroup) Debug() string {
	return fmt.Sprintf("node group %s: min=%d max=%d target=%d", ng.Id(), ng.MinSize(), ng.MaxSize(), ng.targetSize)
}

// Nodes returns a list of all nodes that belong to this node group.  It is
// required that Instance objects returned by this method have ID field set.
// Other fields are optional.
func (ng *crusoeNodeGroup) Nodes() ([]cloudprovider.Instance, error) {
	var nodes []cloudprovider.Instance

	klog.V(4).Info("Nodes,PoolID=", ng.pool.Id)

	for _, node := range ng.nodes {
		nodes = append(nodes, cloudprovider.Instance{
			Id:     toProviderID(node.Id),
			Status: fromCrusoeStatus(node.State),
		})
	}

	return nodes, nil
}

// TemplateNodeInfo returns a schedulerframework.NodeInfo structure of an empty
// (as if just started) node. This will be used in scale-up simulations to
// predict what would a new node look like if a node group was expanded. The returned
// NodeInfo is expected to have a fully populated Node object, with all of the labels,
// capacity and allocatable information as well as all pods that are started on
// the node by default, using manifest (most likely only kube-proxy).
func (ng *crusoeNodeGroup) TemplateNodeInfo() (*schedulerframework.NodeInfo, error) {
	node, err := ng.manager.buildTemplateNodeFromNodePool(context.Background(), ng.pool)
	if err != nil {
		klog.Errorf("Failed to construct template node info for node group %s: %v", ng.pool.Id, err)
	}

	nodeInfo := schedulerframework.NewNodeInfo(cloudprovider.BuildKubeProxy(ng.pool.Name))
	nodeInfo.SetNode(node)
	return nodeInfo, nil
}

// Exist checks if the node group really exists on the cloud provider side. Allows to tell the
// theoretical node group from the real one.
func (ng *crusoeNodeGroup) Exist() bool {
	resp, err := ng.manager.GetNodePool(context.Background(), ng.pool.Id)
	if err != nil {
		klog.Errorf("NodeGroup:Exist,PoolID=%s, failed trying to get nodepool: %v", ng.pool.Id, err)
	}
	return err == nil && resp != nil && resp.Id != "" &&
		resp.State != stateDeleted && resp.State != stateDeleting
}

// Pool Autoprovision feature is not supported by Crusoe cloud yet

// Create creates the node group on the cloud provider side.
func (ng *crusoeNodeGroup) Create() (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrNotImplemented
}

// Delete deletes the node group on the cloud provider side.
func (ng *crusoeNodeGroup) Delete() error {
	return cloudprovider.ErrNotImplemented
}

// Autoprovisioned returns true if the node group is autoprovisioned.
func (ng *crusoeNodeGroup) Autoprovisioned() bool {
	return false
}

// GetOptions returns nil which means 'use defaults options'
func (ng *crusoeNodeGroup) GetOptions(defaults config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	return nil, cloudprovider.ErrNotImplemented
}

func fromCrusoeStatus(status string) *cloudprovider.InstanceStatus {
	st := &cloudprovider.InstanceStatus{}
	switch status {
	case "RUNNING", "RUNNING_DEGRADED":
		st.State = cloudprovider.InstanceRunning
	case "BLOCKED":
		st.ErrorInfo = &cloudprovider.InstanceErrorInfo{
			ErrorCode:    "STATE_BLOCKED",
			ErrorMessage: "crusoe node creation blocked on resources",
		}
	case "DEFINING", "PAUSED":
		st.State = cloudprovider.InstanceCreating
	case "SHUTDOWN":
		st.State = cloudprovider.InstanceDeleting
	case "SHUTOFF":
		st.ErrorInfo = &cloudprovider.InstanceErrorInfo{
			ErrorCode:    "STATE_SHUTOFF",
			ErrorMessage: "crusoe node has been shut off",
		}
	case "CRASHED":
		st.ErrorInfo = &cloudprovider.InstanceErrorInfo{
			ErrorCode:    "STATE_CRASHED",
			ErrorMessage: "crusoe node has crashed",
		}
	case "PMSUSPENDED":
		st.ErrorInfo = &cloudprovider.InstanceErrorInfo{
			ErrorCode:    "STATE_PMSUSPENDED",
			ErrorMessage: "crusoe node has been suspended for power management",
		}
	default: // includes UNSPECIFIED
		st.ErrorInfo = &cloudprovider.InstanceErrorInfo{
			ErrorCode:    status,
			ErrorMessage: "unknown state",
		}
	}

	return st
}

func (ng *crusoeNodeGroup) refresh() error {
	ctx := context.Background()
	ng.nodeGroupRWMutex.Lock()
	defer ng.nodeGroupRWMutex.Unlock()

	currentPool, err := ng.manager.GetNodePool(ctx, ng.pool.Id)
	if err != nil {
		return fmt.Errorf("couldn't fetch node pool with id %s: %w", ng.pool.Id, err)
	}
	ng.pool = currentPool
	err = ng.refreshNodesLocked(ctx, currentPool.InstanceIds)
	if err != nil {
		return fmt.Errorf("couldn't refresh instances for node pool with id %s: %w", ng.pool.Id, err)
	}
	ng.setTargetSizeLocked()

	return nil
}

// refreshNodesLocked is intended to only be called when nodeGroupRWMutex is already held by the caller
func (ng *crusoeNodeGroup) refreshNodesLocked(ctx context.Context, nodeIds []string) error {
	ng.pool.InstanceIds = nodeIds
	newNodes := make(map[string]*crusoeapi.InstanceV1Alpha5)

	for i := 0; i < len(nodeIds); i += instanceBatchSize {
		end := i + instanceBatchSize
		if end > len(nodeIds) {
			end = len(nodeIds)
		}

		instances, err := ng.manager.ListVMInstances(ctx, nodeIds[i:end])
		if err != nil {
			klog.Errorf("Refresh failed for nodepool %s: %s", ng.pool.Id, err)
			return err
		}
		klog.V(6).Infof("Refresh,ProjectID=%s,ClusterID=%s,NodepoolID=%s ListInstances returns %d->%d IDs",
			ng.pool.ProjectId, ng.pool.ClusterId, ng.pool.Id, len(nodeIds), len(instances))

		for _, instance := range instances {
			if instance.State != stateDeleted && instance.State != stateDeleting {
				newNodes[instance.Id] = &instance
			}
		}
	}

	ng.nodes = newNodes
	return nil
}

func (ng *crusoeNodeGroup) addNodeToDeletionInProgressSet(nodeID string) {
	ng.nodeGroupRWMutex.Lock()
	defer ng.nodeGroupRWMutex.Unlock()

	klog.V(4).Infof("Adding node with id %s to deletion in progress set", nodeID)
	ng.deletionInProgressNodeSet[nodeID] = struct{}{}
	ng.setTargetSizeLocked()
}

func (ng *crusoeNodeGroup) removeNodeFromDeletionInProgressSet(nodeID string) {
	ng.nodeGroupRWMutex.Lock()
	defer ng.nodeGroupRWMutex.Unlock()

	klog.V(4).Infof("Removing node with id %s from deletion in progress set", nodeID)
	delete(ng.deletionInProgressNodeSet, nodeID)
	ng.setTargetSizeLocked()
}

// setTargetSizeLocked should only be called when nodeGroupRWMutex is already held by the caller.
// This method sets the target size of the node group based on the desired node count and current active nodes.
// If the node pool is marked unhealthy, the target size defaults to the number of active nodes,
// as the cloud provider will stop trying to fulfill the desired count.
func (ng *crusoeNodeGroup) setTargetSizeLocked() {
	activeNodes := ng.calculateActiveNodesFromCacheLocked()
	ng.targetSize = max(int(ng.pool.Count), activeNodes)
	if ng.pool.State == stateUnhealthy {
		klog.V(4).Infof("node pool with id %s is unhealthy, setting target size "+
			"to the number of active nodes: %d", ng.pool.Id, activeNodes)
		ng.targetSize = activeNodes
	}

	klog.V(4).Infof("current target size for node pool with id %s is %d, "+
		"where node pool's current desired count is %d and it contains %d active nodes",
		ng.pool.Id, ng.targetSize, ng.pool.Count, activeNodes,
	)
}

// calculateActiveNodesFromCacheLocked is intended to only be called when nodeGroupRWMutex is already held by the caller
func (ng *crusoeNodeGroup) calculateActiveNodesFromCacheLocked() int {
	activeNodeCount := 0
	for i, _ := range ng.nodes {
		// do not count nodes where deletion request is already sent
		if _, ok := ng.deletionInProgressNodeSet[ng.nodes[i].Id]; ok {
			klog.V(4).Infof("Found node with id %s in deletion in progress node set", ng.nodes[i].Id)
		} else {
			activeNodeCount++
		}
	}

	return activeNodeCount
}
