# KAI Scheduler Bug Analysis: Incorrect Idle GPU Count

**Date**: 2025-10-13
**Issue**: Pods are being scheduled even when available VRAM is less than requested

## Bug Reproduction

Applied deployment with 5 replicas requesting 8000 MB GPU memory each on a node with a single 32GB GPU:
- **Expected**: Only 3-4 pods scheduled (32GB / 8GB = 4 pods max)
- **Actual**: All 5 pods were scheduled

## Root Cause Identified

**NVIDIA GPU Time-Slicing Configuration**: The node has 1 physical 32GB GPU configured with time-slicing replicas=99 via NVIDIA GPU Operator. This advertises 99 GPU "devices" to Kubernetes for time-sharing.

**The Bug**: The KAI scheduler is treating these 99 time-sliced GPU devices as "whole GPUs" that don't require memory accounting, when they should ALL be treated as shared access to the SAME 32GB physical GPU with strict VRAM limits.

## Evidence from Debug Logs

### Pods 1-4: Correctly Scheduled on Shared GPU

Pod 1 (ubuntu-test-669b8bf444-d6mr8):
```
[GPU_FILTER] Node <ardge-ms-7e47>: IdleGPUs=<100>, ReleasingGPUs=<0>, adding <100> whole GPU indicators
[GPU_SELECT] Whole GPU found, groups=<[8d580ab0-5642-4fd8-8c19-b34e71f03415]>
[GPU_ALLOCATE] Allocation successful
```

Pod 2 (ubuntu-test-669b8bf444-pt992):
```
[GPU_FILTER] GPU <8d580ab0-5642-4fd8-8c19-b34e71f03415>: UsedMemory=<8000 MB>, AllocatedMemory=<8000 MB>, ReleasingMemory=<0 MB>, TotalGpuMemory=<32600 MB>, Fits=<true>
[GPU_FILTER] IdleGPUs=<99>, ReleasingGPUs=<0>, adding <99> whole GPU indicators
[GPU_SELECT] Processing shared GPU <8d580ab0-5642-4fd8-8c19-b34e71f03415>, EnoughIdle=<true>, TaskAllocatable=<true>
[GPU_ALLOCATE] Allocation successful
```

Pod 3 (ubuntu-test-669b8bf444-pvd9h):
```
[GPU_FILTER] GPU <8d580ab0-5642-4fd8-8c19-b34e71f03415>: UsedMemory=<16000 MB>, AllocatedMemory=<16000 MB>, ReleasingMemory=<0 MB>, TotalGpuMemory=<32600 MB>, Fits=<true>
[GPU_FILTER] IdleGPUs=<99>, ReleasingGPUs=<0>, adding <99> whole GPU indicators
[GPU_ALLOCATE] Allocation successful
```

Pod 4 (ubuntu-test-669b8bf444-pwnds):
```
[GPU_FILTER] GPU <8d580ab0-5642-4fd8-8c19-b34e71f03415>: UsedMemory=<24000 MB>, AllocatedMemory=<24000 MB>, ReleasingMemory=<0 MB>, TotalGpuMemory=<32600 MB>, Fits=<true>
[GPU_FILTER] IdleGPUs=<99>, ReleasingGPUs=<0>, adding <99> whole GPU indicators
[GPU_ALLOCATE] Allocation successful
```

### Pod 5: INCORRECTLY Scheduled on Different GPU

Pod 5 (ubuntu-test-669b8bf444-vf2gm):
```
[GPU_FILTER] GPU <8d580ab0-5642-4fd8-8c19-b34e71f03415>: UsedMemory=<32000 MB>, AllocatedMemory=<32000 MB>, ReleasingMemory=<0 MB>, TotalGpuMemory=<32600 MB>, Fits=<false>
[GPU_FIT_CHECK] GPU <8d580ab0-5642-4fd8-8c19-b34e71f03415>: UsedMemory!=0: <true>, EnoughResources: <false>, AllReleased: <false> -> Fits: <false>
```
✅ **Correctly determined first GPU is full**

```
[GPU_FILTER] IdleGPUs=<99>, ReleasingGPUs=<0>, adding <99> whole GPU indicators
[GPU_SELECT] Whole GPU found, groups=<[dae21df0-bc84-41cb-a3f3-ce4b5afe8982]>
[GPU_ALLOCATE] Allocation successful
```
❌ **INCORRECTLY allocated to a different GPU that shouldn't be available**

## The Problem

**Location**: `pkg/scheduler/framework/session.go:193-198`

```go
if node.Idle.GPUs() > 0 || node.Releasing.GPUs() > 0 {
    log.InfraLogger.V(4).Infof("[GPU_FILTER] Node <%s>: IdleGPUs=<%v>, ReleasingGPUs=<%v>, adding <%d> whole GPU indicators",
        node.Name, node.Idle.GPUs(), node.Releasing.GPUs(), int(node.Idle.GPUs())+int(node.Releasing.GPUs()))
    for range int(node.Idle.GPUs()) + int(node.Releasing.GPUs()) {
        filteredGPUs = append(filteredGPUs, pod_info.WholeGpuIndicator)
    }
}
```

**Issue**: `node.Idle.GPUs()` returns **99** for a node with:
- 1 physical GPU (32GB total)
- GPU sharing enabled
- The GPU already in use by 4 pods

**Expected Behavior**: When a GPU is being used for GPU sharing (even if only partially full), it should NOT be counted as an idle whole GPU. The `node.Idle.GPUs()` should return **0** in this case.

## Analysis

The scheduler uses two concepts:
1. **Shared GPUs**: GPUs tracked in `node.UsedSharedGPUsMemory` with VRAM accounting for memory-based sharing
2. **Whole/Idle GPUs**: Represented by `WholeGpuIndicator` (`-2`), intended for time-sliced GPUs without memory constraints

### What's Happening

With NVIDIA GPU time-slicing (replicas=99), the node advertises **100 GPU devices** (the original GPU + 99 time-sliced replicas) to Kubernetes. All 100 devices point to the SAME physical 32GB GPU.

**Pod 1 allocation:**
- `IdleGPUs=<100>` - All 100 time-sliced devices available
- Scheduler creates GPU group `8d580ab0-5642-4fd8-8c19-b34e71f03415` for VRAM tracking
- Allocates 8GB, tracks it in `UsedSharedGPUsMemory`
- **Problem**: `node.Idle.GPUs()` decrements to 99, but should decrement by ALL 100 since they're the same physical GPU!

**Pods 2-4 allocation:**
- Correctly continue sharing GPU `8d580ab0-5642-4fd8-8c19-b34e71f03415`
- VRAM correctly tracked: 8GB → 16GB → 24GB → 32GB (full)

**Pod 5 allocation (BUG OCCURS HERE):**
- First GPU correctly shows `Fits=<false>` (32000/32600 MB used)
- But `IdleGPUs=<99>` still shows 99 "idle whole GPUs" available
- Scheduler allocates Pod 5 to a "whole GPU" `dae21df0-bc84-41cb-a3f3-ce4b5afe8982`
- This is actually the same physical GPU with a different time-slice device ID
- **No VRAM checking occurs** because it's treated as a "whole GPU"
- Result: 40GB allocated on a 32GB GPU → OOM inevitable

## Root Cause

**Location**: `pkg/scheduler/framework/session.go:193-198`

When GPU time-slicing is configured:
- All time-sliced devices point to the SAME physical GPU
- When ANY pod requests GPU memory (via `gpu-memory` annotation), ALL time-sliced devices should be grouped together for VRAM tracking
- Currently: Only the first allocated device is tracked as shared, the other 99 remain as "whole GPU" indicators
- **The scheduler doesn't understand that time-sliced GPUs are the same physical device**

## Proposed Fix

When a pod with `gpu-memory` annotation is being scheduled on a node with time-sliced GPUs:

1. **All time-sliced GPU devices must be grouped together** and tracked as a single shared GPU for VRAM accounting
2. **Once any pod with `gpu-memory` requests GPU on a time-sliced node**, set `node.Idle.GPUs() = 0` to prevent other pods from allocating "whole GPUs"
3. **All subsequent pods on that node** must go through VRAM checking on the same shared GPU group

### Code Changes Needed

**Option 1: Detect time-slicing at allocation time**
- When allocating the first pod with `gpu-memory` annotation
- Check if node has multiple GPU devices (indicating time-slicing)
- If yes, consume ALL idle GPU count and create one shared GPU group
- All future allocations check VRAM on that single group

**Option 2: Pre-configure time-sliced nodes**
- Add node label or annotation to indicate time-slicing configuration
- During node info initialization, if time-slicing detected:
  - Create single shared GPU group for all devices
  - Set idle GPU count based on physical GPU count, not device count

**Option 3: GPU device grouping by UUID**
- Query GPU UUIDs from device plugin or node labels
- Group all devices with the same UUID into one shared GPU
- Only decrement idle count by number of unique physical GPUs

## Next Investigation Steps

1. **How does KAI detect GPU device relationships?**
   - Look for GPU UUID tracking
   - Check if time-sliced devices share identifiers
   - Find where GPU groups are created

2. **When does a GPU move from "idle" to "shared"?**
   - Trace the allocation flow when first pod requests `gpu-memory`
   - Find where `node.Idle.GPUs()` is decremented
   - Check if it should decrement by physical GPU count vs device count

3. **How to identify time-sliced GPUs?**
   - Check node labels/annotations from GPU operator
   - Look for device plugin configuration
   - Verify if GPU UUID is available to the scheduler

## Related Code Locations

- GPU filtering: `pkg/scheduler/framework/session.go:175-202`
- GPU fit checking: `pkg/scheduler/api/node_info/gpu_sharing_node_info.go:346-390`
- GPU allocation: `pkg/scheduler/gpu_sharing/gpuSharing.go:20-101`
- Node info structure: Look for `Idle` field definition and where it's populated

## Debug Log Patterns to Watch For

When investigating, look for:
- How `node.Idle.GPUs()` value changes as pods are scheduled
- Discrepancy between `UsedSharedGPUsMemory` count (1 GPU) and `Idle.GPUs()` (99 GPUs)
- Whether the total GPU count on node initialization shows 100 instead of 1
