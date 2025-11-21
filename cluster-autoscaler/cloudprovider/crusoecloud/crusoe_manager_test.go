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
	"time"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/autoscaler/cluster-autoscaler/utils/gpu"
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

func TestManager_BuildTemplateNodeFromNodePool_AMDGpu(t *testing.T) {
	ctx := context.Background()
	mgr, _ := testManagerWithMocks()

	// Prevent cache refresh from calling GetVMTypes (set to far future time)
	mgr.instanceTypeRefreshLastRefresh = time.Now().Add(10 * 365 * 24 * time.Hour)

	// Pre-populate instance type detail map to avoid mocking complexity
	mgr.instanceTypeDetailMap = map[string]*crusoeInstanceTypeDetail{
		"mi300x-192gb-ib.8x": {
			ProductName: "mi300x-192gb-ib.8x",
			CpuCores:    240,
			CpuType:     "vCPU",
			NumGpu:      8,
			MemoryGb:    2000,
		},
	}

	nodePool := &crusoeapi.KubernetesNodePool{
		Type_: "mi300x-192gb-ib.8x",
		NodeLabels: map[string]string{
			nodeLabelGPUKey: "amd-mi300x-192gb",
		},
	}

	node, err := mgr.buildTemplateNodeFromNodePool(ctx, nodePool)
	assert.NoError(t, err)
	assert.NotNil(t, node)

	assert.Equal(t, "amd-mi300x-192gb", node.Labels[nodeLabelGPUKey])

	pods := node.Status.Capacity[apiv1.ResourcePods]
	cpu := node.Status.Capacity[apiv1.ResourceCPU]
	mem := node.Status.Capacity[apiv1.ResourceMemory]
	gpuAmd := node.Status.Capacity[gpu.ResourceAMDGPU]
	vnicAmd := node.Status.Capacity[amdVNICResourceName]

	// Ensure Cpacity is set correctly
	assert.Equal(t, int64(110), pods.Value())
	assert.Equal(t, int64(240), cpu.Value())
	assert.Equal(t, int64(2000*1024*1024*1024), mem.Value())
	assert.Equal(t, int64(8), gpuAmd.Value())
	assert.Equal(t, int64(8), vnicAmd.Value())
}

func TestManager_BuildTemplateNodeFromNodePool_NvidiaGpu(t *testing.T) {
	ctx := context.Background()
	mgr, _ := testManagerWithMocks()

	// Prevent cache refresh from calling GetVMTypes (set to far future time)
	mgr.instanceTypeRefreshLastRefresh = time.Now().Add(10 * 365 * 24 * time.Hour)

	// Pre-populate instance type detail map
	mgr.instanceTypeDetailMap = map[string]*crusoeInstanceTypeDetail{
		"l40s-48gb.1x": {
			ProductName: "l40s-48gb.1x",
			CpuCores:    48,
			CpuType:     "vCPU",
			NumGpu:      1,
			MemoryGb:    192,
		},
	}

	nodePool := &crusoeapi.KubernetesNodePool{
		Type_: "l40s-48gb.1x",
		NodeLabels: map[string]string{
			nodeLabelGPUKey: "nvidia-l40s-48gb",
		},
	}

	node, err := mgr.buildTemplateNodeFromNodePool(ctx, nodePool)
	assert.NoError(t, err)
	assert.NotNil(t, node)

	assert.Equal(t, "nvidia-l40s-48gb", node.Labels[nodeLabelGPUKey])

	pods := node.Status.Capacity[apiv1.ResourcePods]
	cpu := node.Status.Capacity[apiv1.ResourceCPU]
	mem := node.Status.Capacity[apiv1.ResourceMemory]
	gpuNv := node.Status.Capacity[gpu.ResourceNvidiaGPU]
	_, hasAmdVnic := node.Status.Capacity[amdVNICResourceName]

	assert.Equal(t, int64(110), pods.Value())
	assert.Equal(t, int64(48), cpu.Value())
	assert.Equal(t, int64(192*1024*1024*1024), mem.Value())
	assert.Equal(t, int64(1), gpuNv.Value())
	assert.False(t, hasAmdVnic)
}

func TestManager_BuildTemplateNodeFromNodePool_NvidiaGpuFallback(t *testing.T) {
	// Targets Node Group before we started adding GPU labels to nodes on create operation
	ctx := context.Background()
	mgr, _ := testManagerWithMocks()

	// Prevent cache refresh from calling GetVMTypes (set to far future time)
	mgr.instanceTypeRefreshLastRefresh = time.Now().Add(10 * 365 * 24 * time.Hour)

	// Pre-populate instance type detail map
	mgr.instanceTypeDetailMap = map[string]*crusoeInstanceTypeDetail{
		"h200-141gb-sxm-ib.8x": {
			ProductName: "h200-141gb-sxm-ib.8x",
			CpuCores:    176,
			CpuType:     "vCPU",
			NumGpu:      8,
			MemoryGb:    1960,
		},
	}

	// Old node without GPU label - should auto-generate
	nodePool := &crusoeapi.KubernetesNodePool{
		Type_:      "h200-141gb-sxm-ib.8x",
		NodeLabels: map[string]string{},
	}

	node, err := mgr.buildTemplateNodeFromNodePool(ctx, nodePool)
	assert.NoError(t, err)
	assert.NotNil(t, node)

	assert.Equal(t, "nvidia-h200-141gb-sxm-ib", node.Labels[nodeLabelGPUKey])

	pods := node.Status.Capacity[apiv1.ResourcePods]
	cpu := node.Status.Capacity[apiv1.ResourceCPU]
	mem := node.Status.Capacity[apiv1.ResourceMemory]
	gpuNv := node.Status.Capacity[gpu.ResourceNvidiaGPU]
	_, hasAmdVnic := node.Status.Capacity[amdVNICResourceName]

	assert.Equal(t, int64(110), pods.Value())
	assert.Equal(t, int64(176), cpu.Value())
	assert.Equal(t, int64(1960*1024*1024*1024), mem.Value())
	assert.Equal(t, int64(8), gpuNv.Value())
	assert.False(t, hasAmdVnic)
}
