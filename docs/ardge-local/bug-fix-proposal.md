# KAI Scheduler Bug Fix: Time-Sliced GPU Handling

## Root Cause Analysis

### The Bug
With NVIDIA GPU time-slicing (replicas=99), KAI scheduler incorrectly allows scheduling more pods than the physical GPU VRAM can support.

### How Time-Slicing Works
- Physical GPU: 1x 32GB GPU
- NVIDIA GPU Operator with `replicas: 99` creates 100 GPU "devices" in Kubernetes
- All 100 devices are time-sliced access to the SAME physical 32GB GPU
- Node reports:
  - `nvidia.com/gpu.count` label: `1` (physical GPU count)
  - `nvidia.com/gpu` allocatable: `100` (time-sliced device count)

### The Broken Logic

**File**: `pkg/scheduler/api/node_info/gpu_sharing_node_info.go:121-128`

```go
default:
    ni.AllocatedSharedGPUsMemory[gpuGroup] += ni.GetResourceGpuMemory(task.ResReq)

    if ni.UsedSharedGPUsMemory[gpuGroup] <= ni.GetResourceGpuMemory(task.ResReq) {
        // no other fractional was allocated here yet
        if int(ni.GetNumberOfGPUsInNode()) < int(ni.Idle.GPUs())+ni.getNumberOfUsedGPUs() {
            ni.Idle.SubGPUs(1)  // <-- Line 126: Only decrements when FIRST pod on GPU
        }
    }
```

**Line 125 condition breakdown**:
```
GetNumberOfGPUsInNode() < Idle.GPUs() + UsedGPUs
```

With time-slicing:
- `GetNumberOfGPUsInNode()` returns `1` (from `nvidia.com/gpu.count` label)
- `Idle.GPUs()` starts at `100` (from node allocatable)
- `UsedGPUs` starts at `0`

**Pod 1 allocation**:
- Check: `1 < 100 + 0` → `TRUE` ✓
- Action: `Idle.SubGPUs(1)` → Idle becomes 99

**Pod 2-4 allocation**:
- Check: `1 < 99 + 1` → `TRUE` ✓
- But line 123: `UsedSharedGPUsMemory[gpu] > RequestedMemory` → **Condition on line 123 is FALSE**
- Action: **Idle NOT decremented** (stays at 99)

**Pod 5 allocation**:
- GPU group `8d580ab0-5642-4fd8-8c19-b34e71f03415` is full (32GB/32GB)
- But `Idle.GPUs() == 99`
- Scheduler sees 99 "whole GPUs" available
- Allocates Pod 5 to new GPU group `dae21df0-bc84-41cb-a3f3-ce4b5afe8982`
- **No VRAM checking occurs for "whole GPU" allocation**
- Result: 40GB allocated on 32GB physical GPU

### Why This Happens

The condition on **line 125** is designed for scenarios where:
- Each GPU in the node is a separate physical device
- `nvidia.com/gpu.count` == allocatable GPU count
- When all physical GPUs are used, idle should be 0

But with time-slicing:
- `nvidia.com/gpu.count` (1) < allocatable GPU count (100)
- The check `GetNumberOfGPUsInNode() < Idle.GPUs() + UsedGPUs` **incorrectly assumes** separate physical GPUs
- It fails to recognize that all 100 time-sliced devices map to the SAME physical GPU

## The Fix

### Option 1: Detect Time-Slicing and Adjust Idle Count (Recommended)

When time-slicing is detected, treat ALL time-sliced devices as belonging to a single shared GPU for VRAM purposes.

**Detection Method**:
```go
// In node_info.go NewNodeInfo() function
func NewNodeInfo(node *v1.Node, podAffinityInfo pod_affinity.NodePodAffinityInfo) *NodeInfo {
    gpuMemory, exists := getNodeGpuMemory(node)

    nodeInfo := &NodeInfo{
        Name: node.Name,
        Node: node,

        Releasing: resource_info.EmptyResource(),
        Idle:      resource_info.ResourceFromResourceList(node.Status.Allocatable),
        Used:      resource_info.EmptyResource(),

        // ... rest of initialization
    }

    // FIX: Detect time-slicing and adjust idle GPU count
    physicalGPUCount := nodeInfo.GetNumberOfGPUsInNode()  // From nvidia.com/gpu.count label
    allocatableGPUCount := int64(nodeInfo.Allocatable.GPUs())

    if allocatableGPUCount > physicalGPUCount {
        // Time-slicing detected: allocatable > physical
        // Set idle to physical GPU count for proper VRAM accounting
        log.InfraLogger.V(2).Infof("Node <%s>: Time-slicing detected (allocatable=%d, physical=%d). Adjusting idle GPU count to physical count.",
            node.Name, allocatableGPUCount, physicalGPUCount)
        nodeInfo.Idle.SetGPUs(float64(physicalGPUCount))
    }

    return nodeInfo
}
```

**Changes Needed**:
1. **File**: `pkg/scheduler/api/node_info/node_info.go` (function `NewNodeInfo`)
   - After line 113, add time-slicing detection
   - Adjust `Idle.GPUs()` to physical GPU count when time-slicing is detected

2. **File**: `pkg/scheduler/api/node_info/gpu_sharing_node_info.go` (lines 121-128)
   - Keep existing logic (no changes needed with Option 1)

**Pros**:
- Simple, localized fix
- Prevents "phantom GPUs" at initialization
- All downstream logic works correctly
- No changes to allocation/deallocation paths

**Cons**:
- Assumes `nvidia.com/gpu.count` label is always present and accurate
- Requires GPU operator to set the label correctly

### Option 2: Remove the Broken Check

Simply remove or fix the condition on line 125.

**File**: `pkg/scheduler/api/node_info/gpu_sharing_node_info.go:121-128`

```go
default:
    ni.AllocatedSharedGPUsMemory[gpuGroup] += ni.GetResourceGpuMemory(task.ResReq)

    if ni.UsedSharedGPUsMemory[gpuGroup] <= ni.GetResourceGpuMemory(task.ResReq) {
        // no other fractional was allocated here yet
        // FIX: Always decrement idle when creating a new GPU group
        ni.Idle.SubGPUs(1)
    }
```

**Pros**:
- Simple change
- Doesn't rely on labels

**Cons**:
- Doesn't address the root issue (Idle starts at 100 instead of 1)
- May cause `Idle.GPUs()` to go negative
- Doesn't work if pods are scheduled on different GPU groups initially

### Option 3: Track Time-Sliced Devices Explicitly

Add explicit tracking of which GPU groups belong to the same physical GPU.

**Changes**:
1. Add field to `GpuSharingNodeInfo`:
   ```go
   type GpuSharingNodeInfo struct {
       PhysicalGPUMapping map[string]string  // GPU group ID -> Physical GPU UUID
       // ... existing fields
   }
   ```

2. When creating GPU groups, assign them to physical GPUs
3. Decrement idle only once per physical GPU, not per group

**Pros**:
- Most correct solution
- Handles complex scenarios (multiple physical GPUs, each with time-slicing)

**Cons**:
- Significant code changes
- Requires GPU UUID information from device plugin
- More complex implementation

## Recommended Fix: Option 1

Implement Option 1 for the following reasons:

1. **Minimal code changes**: Single location, well-isolated
2. **Leverages existing infrastructure**: Uses `nvidia.com/gpu.count` label that GPU operator already provides
3. **Prevents the bug at the source**: Fixes idle count at initialization
4. **Safe**: Doesn't break non-time-sliced scenarios (physicalCount == allocatableCount)
5. **No downstream impacts**: All existing allocation logic continues to work

## Testing Plan

1. **Before fix**: Reproduce bug with 5 pods requesting 8GB each on 32GB GPU with time-slicing
2. **After fix**: Verify only 4 pods are scheduled, 5th remains pending
3. **Verify logs**: Check that `[GPU_FILTER] IdleGPUs=<1>` after fix (not 99)
4. **Non-time-sliced nodes**: Verify normal GPUs still work correctly
5. **Multiple physical GPUs**: Test node with 2 physical GPUs, each with time-slicing

## Implementation

See next file: `bug-fix-implementation.patch`
