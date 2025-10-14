# Gang Scheduling Examples

This directory contains examples demonstrating gang scheduling with KAI scheduler.

## What is Gang Scheduling?

Gang scheduling ensures that a group of related pods must either be scheduled together or not at all. This is crucial for distributed workloads like machine learning training jobs that require multiple workers to start simultaneously.

## Examples

### 1. Automatic Gang Scheduling with Job
**File**: `distributed-training-job.yaml`

The simplest way to use gang scheduling. The pod-grouper automatically creates a PodGroup based on the Job's `parallelism` setting.

### 2. Manual PodGroup Control
**File**: `manual-podgroup.yaml`

Explicit PodGroup creation for fine-grained control over gang scheduling behavior.

### 3. Testing Gang Scheduling Behavior
**File**: `large-gang-job.yaml`

Example that demonstrates gang scheduling preventing partial allocation when insufficient resources are available.

## How to Use

1. **Apply an example**:
   ```bash
   kubectl apply -f distributed-training-job.yaml
   ```

2. **Check PodGroup status**:
   ```bash
   kubectl get podgroup
   kubectl describe podgroup <podgroup-name>
   ```

3. **Check pod status**:
   ```bash
   kubectl get pods -l app=distributed-training
   ```

## Key Concepts

- **minMember**: Minimum number of pods that must be schedulable before any are scheduled
- **Queue**: Resource allocation queue for the workload
- **Priority Class**: Determines scheduling priority (Train, Inference, etc.)
- **gpu-memory annotation**: Specifies GPU VRAM requirement per pod

## Expected Behavior

**With Sufficient Resources**:
- All pods in the gang are scheduled simultaneously
- PodGroup phase becomes "Running"

**Without Sufficient Resources**:
- NO pods are scheduled (they remain Pending)
- PodGroup shows unschedulable explanation
- Pods wait until resources become available for the entire gang
