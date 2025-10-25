# KAI Scheduler GPU VRAM Debugging Session

## Problem Statement

A mysterious bug in the KAI scheduler where **pods are still getting scheduled even when the available VRAM is less than what the pod requests** (via the `gpu-memory` annotation).

## Debug Messages Added

To help trace the scheduling decision factors, comprehensive debug messages were added at key decision points in the GPU VRAM scheduling logic. All messages use log level **V=4** and are tagged with prefixes for easy filtering.

### Files Modified

#### 1. `pkg/scheduler/framework/session.go`

**Function: `filterGpusByEnoughResources` (lines 164-191)**

Added logging to show:
- Requested GPU memory for the pod
- For each GPU being evaluated:
  - Used memory
  - Allocated memory
  - Releasing memory
  - Total GPU memory
  - Whether the GPU fits
- Final filtered GPU list

**Log Prefix**: `[GPU_FILTER]`

```go
log.InfraLogger.V(4).Infof("[GPU_FILTER] Node <%s>: Filtering GPUs for pod <%s/%s>, requested gpu-memory: <%d MB>",
    node.Name, pod.Namespace, pod.Name, pod.ResReq.GpuMemory())
```

#### 2. `pkg/scheduler/api/node_info/gpu_sharing_node_info.go`

**Function: `IsTaskFitOnGpuGroup` (lines 346-356)**

Shows the three conditions that determine if a GPU fits:
- UsedMemory != 0 (GPU is in use)
- EnoughResources (including releasing memory)
- Not all released

**Log Prefix**: `[GPU_FIT_CHECK]`

**Function: `EnoughIdleResourcesOnGpu` (lines 358-374)**

Shows if there's enough **allocated** (non-releasing) memory available:
- Total GPU memory
- Allocated memory
- Requested memory
- Available memory
- Whether there's enough idle resources

**Log Prefix**: `[IDLE_CHECK]`

**Function: `enoughResourcesOnGpu` (lines 376-390)**

**CRITICAL**: Shows if there's enough memory **including releasing memory** (which will become available):
- Total memory
- Allocated memory
- Releasing memory
- Requested memory
- Available memory calculation: `Total - Allocated + Releasing - Requested`
- Whether there are enough resources

**Log Prefix**: `[RESOURCE_CHECK]`

#### 3. `pkg/scheduler/gpu_sharing/gpuSharing.go`

**Function: `AllocateFractionalGPUTaskToNode` (lines 20-55)**

Main allocation entry point showing:
- Requested GPU memory
- isPipelineOnly flag
- Fitting GPUs list
- Selected GPU groups
- Whether allocation succeeded

**Log Prefix**: `[GPU_ALLOCATE]`

**Function: `getNodePreferableGpuForSharing` (lines 57-101)**

Shows the selection logic for choosing which GPU(s) to use:
- Required device count
- For each GPU being processed:
  - Whether it's a whole GPU or shared GPU
  - EnoughIdle status
  - TaskAllocatable status
  - Whether it will be releasing
- Final selected GPU groups

**Log Prefix**: `[GPU_SELECT]`

## How to Use Debug Messages

Run the scheduler with log level V=4:
```bash
--v=4
```

Filter logs by prefix:
```bash
# See all GPU-related debug messages
kubectl logs <scheduler-pod> | grep "\[GPU_"

# See only filtering decisions
kubectl logs <scheduler-pod> | grep "\[GPU_FILTER\]"

# See resource availability checks
kubectl logs <scheduler-pod> | grep "\[RESOURCE_CHECK\]"
```

## Key Insight: Potential Root Cause

The bug might be explained by the **releasing memory** logic in `enoughResourcesOnGpu()`:

```go
availableMemory = totalMemory - allocatedMemory + releasingMemory
```

This calculation **counts releasing memory as available** even though pods using that memory haven't been terminated yet. This could cause incorrect scheduling if:

1. Scheduler sees pods marked as "Releasing"
2. Counts their memory as available
3. Schedules a new pod based on this calculation
4. But the releasing pods haven't actually freed their VRAM yet

The debug messages will reveal this by showing when `ReleasingMemory > 0` in the `[RESOURCE_CHECK]` logs.

## Changes in v0.9.4 That Could Cause the Bug

### 1. **DefaultNodePoolLabelKey Changed to Empty String** ⚠️ HIGH RISK

**Location**: `pkg/common/constants/constants.go:21`

```go
DefaultNodePoolLabelKey = ""  // Was: "kai.scheduler/node-pool"
```

**Impact**: If node pool filtering was previously used to partition nodes, disabling it (empty string) means the scheduler now considers **all nodes** instead of a filtered subset. This could lead to:
- Scheduling on nodes with stale GPU memory information
- Cross-node-pool scheduling with incorrect state
- Bypassed node filtering logic

### 2. **New PrePredicateFn Hook Added** ⚠️ MEDIUM RISK

**Location**: `pkg/scheduler/actions/common/allocate.go:50`

**What changed**:
- New `PrePredicateFn` hook runs **before** node selection
- Used by 3 plugins: `dynamicresources`, `topology`, and `predicates`

**Potential issue**: PrePredicate functions could be incorrectly allowing pods through when they should fail GPU memory checks.

### 3. **CleanAllocationAttemptCacheFns Added** ⚠️ MEDIUM RISK

**Location**: `pkg/scheduler/actions/common/allocate.go:30`

```go
defer ssn.CleanAllocationAttemptCache(job)  // Runs AFTER allocation
```

**Potential issue**: Cache cleanup might be:
- Clearing GPU memory allocation tracking prematurely
- Resetting node GPU state between check and allocate phases
- Creating race conditions where memory appears available when it's not

## Investigation Strategy

With the debug messages enabled (--v=4), look for:

1. **Memory value changes**: Check if `AllocatedMemory` or `ReleasingMemory` values suddenly change between `[GPU_FILTER]` and `[GPU_ALLOCATE]` logs for the same pod

2. **Releasing memory abuse**: Look for cases where:
   ```
   [RESOURCE_CHECK] ReleasingMemory=<large value>
   ```
   This indicates the scheduler is counting soon-to-be-freed memory as available

3. **Node pool issues**: Check if pods are being scheduled on unexpected nodes (if you were previously using node pools)

4. **Cache cleanup timing**: Watch for allocation state changes after cache cleanup operations

## Next Steps

1. Enable V=4 logging on the scheduler
2. Reproduce the bug and collect logs
3. Search for the pod that was incorrectly scheduled in the logs
4. Trace backwards through the `[GPU_*]` prefixed messages to see:
   - What memory values were used in the decision
   - Whether releasing memory was counted as available
   - If any values changed unexpectedly during allocation
5. Compare `AllocatedMemory`, `ReleasingMemory`, and `AvailableMemory` values against actual pod requests

## Code Locations Reference

- GPU filtering: `pkg/scheduler/framework/session.go:164-191`
- Fit checking: `pkg/scheduler/api/node_info/gpu_sharing_node_info.go:346-390`
- Allocation logic: `pkg/scheduler/gpu_sharing/gpuSharing.go:20-101`
- PrePredicate hook: `pkg/scheduler/actions/common/allocate.go:47-59`
- Cache cleanup: `pkg/scheduler/actions/common/allocate.go:30`
