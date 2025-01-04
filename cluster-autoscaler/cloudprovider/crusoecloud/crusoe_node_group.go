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
	"strings"
	"sync"
	"time"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/klog/v2"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	// TODO: these could be configured defaults
	Min_NodePool_Size = 1
	Max_NodePool_Size = 254
)

// crusoeNodeGroup implements cloudprovider.NodeGroup interface. It contains
// configuration info and functions to control a CrusoeCloud Managed Kubernetes (CMK)
// NodePool, which is a set of nodes that have the same capacity and set of labels.
type crusoeNodeGroup struct {
	manager *crusoeManager
	pool    *crusoeapi.KubernetesNodePool
	nodes   map[string]*crusoeapi.InstanceV1Alpha5
	spec    *dynamic.NodeGroupSpec

	updateMutex sync.Mutex
}

// MaxSize returns maximum size of the node group.
func (ng *crusoeNodeGroup) MaxSize() int {
	if ng.spec != nil {
		return ng.spec.MaxSize
	}
	return int(Max_NodePool_Size)
}

// MinSize returns minimum size of the node group.
func (ng *crusoeNodeGroup) MinSize() int {
	if ng.spec != nil {
		return ng.spec.MinSize
	}
	return int(Min_NodePool_Size)
}

// TargetSize returns the current target size of the node group. It is possible that the
// number of nodes in Kubernetes is different at the moment but should be equal
// to Size() once everything stabilizes (new nodes finish startup and registration or
// removed nodes are deleted completely).
func (ng *crusoeNodeGroup) TargetSize() (int, error) {
	return int(ng.pool.Count), nil
}

// IncreaseSize increases the size of the node group. To delete a node you need
// to explicitly name it and use DeleteNode. This function should wait until
// node group size is updated.
func (ng *crusoeNodeGroup) IncreaseSize(delta int) error {
	klog.V(4).Infof("IncreaseSize,ClusterID=%s,delta=%d", ng.pool.ClusterId, delta)

	if delta <= 0 {
		return fmt.Errorf("delta must be strictly positive, have: %d", delta)
	}

	targetSize := ng.pool.Count + int64(delta)
	if targetSize > int64(ng.MaxSize()) {
		return fmt.Errorf("size increase is too large. current: %d desired: %d max: %d",
			ng.pool.Count, targetSize, ng.MaxSize())
	}

	ng.updateMutex.Lock()
	defer ng.updateMutex.Unlock()

	ctx := context.Background()
	op, err := ng.manager.UpdateNodePool(ctx, ng.pool.Id, targetSize)
	if err != nil {
		return err
	}

	// TODO: implement proper wait utility
	for op.State == string(opInProgress) {
		klog.V(4).Infof("IncreaseSize,ClusterID=%s checking op state: %v", ng.pool.ClusterId, op.State)
		updatedOp, err := ng.manager.GetNodePoolOperation(ctx, op.OperationId)
		if err != nil {
			return fmt.Errorf("failed waiting for nodepool operation: %w", err)
		}
		time.Sleep(2 * time.Second) // TODO: implement backoff

		op = updatedOp
	}

	if op.State == string(opFailed) {
		// result := op.Result. .. need to marshal
		return fmt.Errorf("couldn't increase pool size to %d", targetSize)
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
func (ng *crusoeNodeGroup) AtomicIncreaseSize(delta int) error {
	return cloudprovider.ErrNotImplemented
}

// DeleteNodes deletes nodes from this node group. Error is returned either on
// failure or if the given node doesn't belong to this node group. This function
// should wait until node group size is updated.
func (ng *crusoeNodeGroup) DeleteNodes(nodes []*apiv1.Node) error {
	ctx := context.Background()

	targetSize := ng.pool.Count - int64(len(nodes))
	klog.V(4).Infof("DeleteNodes,%d nodes to reclaim (%d target size); ng=%v, pool=%v", len(nodes), targetSize, ng, ng.pool)

	ng.updateMutex.Lock()
	defer ng.updateMutex.Unlock()

	_ /*ngOp*/, err := ng.manager.UpdateNodePool(ctx, ng.pool.Id, targetSize)
	if err != nil {
		klog.Errorf("DeleteNodes,PoolID=%s, failed to set target nodepool size to %d: %v", ng.pool.Id, targetSize, err)
		// for now, ignore error [TODO]
	}

	vmOps := make([]*crusoeapi.Operation, 0, len(nodes))
	for _, n := range nodes {
		node, ok := ng.nodes[nodeIndexFor(n)]
		if !ok {
			klog.Errorf("DeleteNodes,Name=%s,PoolID=%s,node marked for deletion not found in pool", n.Name, ng.pool.Id)
			continue
		}

		op, err := ng.manager.DeleteVMInstance(ctx, node.Id)
		if err != nil {
			klog.Errorf("DeleteNodes,failed to delete node %s: %s",
				node.Id, err)
			return err
		}
		if op.State == string(opFailed) {
			// exit or wait at this point?
			klog.Errorf("DeleteNodes,failed to delete node %s: operation error", node.Id) // TODO: handle result, etc.
		}
		vmOps = append(vmOps, op)

		ng.pool.Count--
		ng.nodes[nodeIndexFor(n)].State = "SHUTDOWN"
	}

	allComplete := false
	for !allComplete {
		inProcess := 0
		for _, op := range vmOps {
			updatedOp, err := ng.manager.GetVMOperation(ctx, op.OperationId)
			klog.V(4).Infof("DeleteNodes,ClusterID=%s checking op %s state: %v", ng.pool.ClusterId, op.OperationId, op.State)
			if err != nil {
				return fmt.Errorf("failed waiting for nodepool operation %s: %w", op.OperationId, err)
			}
			if updatedOp.State == string(opInProgress) {
				inProcess++
			}
			if updatedOp.State == string(opFailed) {
				// exit or wait at this point?
				klog.Errorf("DeleteNodes,failed to delete node with op %s: operation error", op.OperationId) // TODO: handle result, etc.
			}
		}
		if inProcess == 0 {
			allComplete = true
		}
		time.Sleep(2 * time.Second) // TODO: implement backoff
	}
	return nil
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

	targetSize := ng.pool.Count + int64(delta)
	if int(targetSize) < ng.MinSize() {
		return fmt.Errorf("size decrease is too large. current: %d desired: %d min: %d",
			ng.pool.Count, targetSize, ng.MinSize())
	}

	ctx := context.Background()
	_ /*op*/, err := ng.manager.UpdateNodePool(ctx, ng.pool.Id, targetSize)
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
func (ng *crusoeNodeGroup) Id() string {
	return ng.pool.Id
}

// Debug returns a string containing all information regarding this node group.
func (ng *crusoeNodeGroup) Debug() string {
	return fmt.Sprintf("id:%s,status:%s,imageid:%s,size:%d,min_size:%d,max_size:%d", ng.Id(), ng.pool.State, ng.pool.ImageId, ng.pool.Count, ng.MinSize(), ng.MaxSize())
}

// Nodes returns a list of all nodes that belong to this node group.  It is
// required that Instance objects returned by this method have ID field set.
// Other fields are optional.
func (ng *crusoeNodeGroup) Nodes() ([]cloudprovider.Instance, error) {
	var nodes []cloudprovider.Instance

	klog.V(4).Info("Nodes,PoolID=", ng.pool.Id)

	for _, node := range ng.nodes {
		nodes = append(nodes, cloudprovider.Instance{
			Id:     node.Id,
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
	return nil, cloudprovider.ErrNotImplemented
}

// Exist checks if the node group really exists on the cloud provider side. Allows to tell the
// theoretical node group from the real one.
func (ng *crusoeNodeGroup) Exist() bool {
	resp, err := ng.manager.GetNodePool(context.Background(), ng.pool.Id)
	return err == nil && resp != nil && resp.Id != ""
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

func nodeIndexFor(node *apiv1.Node) string {
	return strings.Split(node.Name, ".")[0]
}

func fromCrusoeStatus(status string) *cloudprovider.InstanceStatus {
	st := &cloudprovider.InstanceStatus{}
	switch status {
	case "RUNNING":
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
