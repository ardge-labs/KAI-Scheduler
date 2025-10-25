# KAI Scheduler Dependencies on NVIDIA GPU Operator

## Summary

The KAI scheduler does **NOT require the full NVIDIA GPU Operator** to function. It only depends on specific **node labels and Kubernetes resources** that the GPU operator provides. These can be provided by alternative methods.

## What KAI Scheduler Actually Uses

### 1. Node Labels (Critical)

The scheduler reads these labels from GPU nodes:

| Label | Purpose | File Reference |
|-------|---------|----------------|
| **`nvidia.com/gpu.count`** | Physical GPU count on node (used for time-slicing VRAM accounting) | `pkg/scheduler/api/node_info/node_info.go:600-610` |
| **`nvidia.com/gpu.memory`** | GPU memory in MiB per GPU | `pkg/scheduler/api/node_info/node_info.go:48-62` |
| **`nvidia.com/mig.strategy`** | MIG mode: `none`, `single`, or `mixed` | `pkg/common/constants/constants.go:44` |

**Who provides these labels:**
- Normally: **NVIDIA GPU Feature Discovery (GFD)** - a component of GPU Operator
- Alternative: **fake-gpu-operator** (used in KAI tests) or manually set labels

### 2. Kubernetes Resources

The scheduler schedules pods requesting:

| Resource | Purpose | Usage |
|----------|---------|-------|
| **`nvidia.com/gpu`** | Whole GPU allocation | Standard Kubernetes resource |
| **`nvidia.com/mig-*`** | MIG profile resources (e.g., `nvidia.com/mig-1g.10gb`) | For MIG-enabled GPUs |

**Who provides these resources:**
- Normally: **NVIDIA Device Plugin** - a component of GPU Operator
- Alternative: **fake-gpu-operator** or any device plugin implementation

### 3. ClusterPolicy CRD (Optional - Binder Only)

The **binder** component (not the scheduler) queries the `ClusterPolicy` CRD to detect CDI (Container Device Interface) configuration:

```go
// File: pkg/operator/operands/binder/resources.go:147-176
func isCdiEnabled(ctx context.Context, readerClient client.Reader) (bool, error) {
    nvidiaClusterPolicies := &nvidiav1.ClusterPolicyList{}
    err := readerClient.List(ctx, nvidiaClusterPolicies)

    // If CRD doesn't exist, gracefully defaults to CDI disabled
    if meta.IsNoMatchError(err) || kerrors.IsNotFound(err) {
        return false, nil
    }

    // Check if CDI is enabled in the ClusterPolicy
    if len(nvidiaClusterPolicies.Items) > 0 {
        cp := nvidiaClusterPolicies.Items[0]
        if cp.Spec.CDI.Enabled != nil && *cp.Spec.CDI.Enabled {
            if cp.Spec.CDI.Default != nil && *cp.Spec.CDI.Default {
                return true, nil
            }
        }
    }

    return false, nil
}
```

**What it does:**
- Queries `clusterpolicies.nvidia.com/v1` CRD
- Checks if `spec.cdi.enabled` and `spec.cdi.default` are both true
- If CRD doesn't exist or CDI is disabled, the binder works fine (CDI is optional)

**Operator controller watches ClusterPolicy** (but doesn't require it):
```go
// File: pkg/operator/controller/config_controller.go:139
builder := ctrl.NewControllerManagedBy(mgr).
    For(&kaiv1.Config{}).
    Watches(&nvidiav1.ClusterPolicy{}, handler.EnqueueRequestsFromMapFunc(enqueueWatched))
```

The controller triggers a reconcile when ClusterPolicy changes, but handles its absence gracefully.

## What Is NOT Required

### ❌ Not Required from GPU Operator:
1. **GPU Operator Deployment** - Only the labels and resources matter
2. **NVIDIA Driver** - Can use pre-installed drivers or no drivers (for testing)
3. **CUDA Toolkit** - Not used by the scheduler
4. **GPU Monitoring Tools** - Not used by the scheduler
5. **Container Runtime Configuration** - Handled by container runtime directly

## How KAI Scheduler Works Without GPU Operator

### Option 1: Use fake-gpu-operator (Testing)

The KAI project uses a **fake GPU operator** for testing:

```bash
# From: hack/run-e2e-kind.sh:56-57
helm upgrade -i gpu-operator oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator \
    --namespace gpu-operator --create-namespace --version 0.0.62
```

This provides:
- Node labels (`nvidia.com/gpu.count`, `nvidia.com/gpu.memory`)
- Resource allocatable (`nvidia.com/gpu`)
- No actual GPU hardware required

### Option 2: Manual Configuration

For nodes with actual GPUs but no GPU Operator:

```bash
# 1. Label nodes with GPU info
kubectl label node <node-name> nvidia.com/gpu.count=2      # 2 physical GPUs
kubectl label node <node-name> nvidia.com/gpu.memory=40960 # 40GB per GPU in MiB

# 2. Use NVIDIA Device Plugin (standalone)
kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/main/nvidia-device-plugin.yml

# This provides nvidia.com/gpu resource without full GPU Operator
```

### Option 3: Time-Slicing Without GPU Operator

For GPU sharing via time-slicing:

```bash
# 1. Configure device plugin with time-slicing config
apiVersion: v1
kind: ConfigMap
metadata:
  name: nvidia-device-plugin-config
  namespace: kube-system
data:
  config.yaml: |
    sharing:
      timeSlicing:
        replicas: 10  # Creates 10 virtual GPUs per physical GPU

# 2. Apply standalone device plugin with config
# 3. Label nodes
kubectl label node <node-name> nvidia.com/gpu.count=1      # Physical count
kubectl label node <node-name> nvidia.com/gpu.memory=32768 # 32GB
```

The scheduler will see `nvidia.com/gpu: 10` (allocatable) but use `nvidia.com/gpu.count: 1` (label) for VRAM accounting.

## Code References

### Scheduler's GPU Label Usage

**Location**: `pkg/scheduler/api/node_info/node_info.go:127-141`

```go
// FIX: Detect GPU time-slicing and adjust idle GPU count to physical GPU count
physicalGPUCount := nodeInfo.GetNumberOfGPUsInNode() // From nvidia.com/gpu.count label
allocatableGPUCount := int64(nodeInfo.Allocatable.GPUs())

if allocatableGPUCount > physicalGPUCount && physicalGPUCount > 0 {
    // Time-slicing detected: allocatable > physical
    log.InfraLogger.V(2).Infof(
        "Node <%s>: GPU time-slicing detected (allocatable=%d devices, physical=%d GPUs). "+
            "Adjusting idle GPU count to physical count for VRAM accounting.",
        node.Name, allocatableGPUCount, physicalGPUCount)
    nodeInfo.Idle.SetGPUs(float64(physicalGPUCount))
}
```

### Binder's ClusterPolicy Query

**Location**: `pkg/operator/operands/binder/resources.go:147-176`

- Gracefully handles missing ClusterPolicy CRD
- Defaults to CDI disabled if not found
- Only used by **binder** component, not scheduler

### Operator Controller Watch

**Location**: `pkg/operator/controller/config_controller.go:76, 139`

```go
// RBAC permission (but doesn't require it to exist)
// +kubebuilder:rbac:groups="nvidia.com",resources=clusterpolicies,verbs=get;list;watch

// Watch for changes (triggers reconcile, but handles absence)
Watches(&nvidiav1.ClusterPolicy{}, handler.EnqueueRequestsFromMapFunc(enqueueWatched))
```

## Minimum Requirements Summary

### For Scheduler Core Functionality:

**Required**:
1. Node labels: `nvidia.com/gpu.count` and `nvidia.com/gpu.memory`
2. Node resources: `nvidia.com/gpu` in `status.allocatable`
3. Device plugin to report GPU resources

**Optional**:
- ClusterPolicy CRD (for CDI configuration in binder)
- MIG labels (only if using MIG)

### For Full GPU Operator Features:

If you want **all GPU Operator features**, you need:
- Driver installation
- Container runtime configuration
- GPU monitoring
- Dynamic MIG configuration
- DCGM metrics

But for **KAI scheduler to work**, you only need the labels and resources.

## Testing Without Real GPUs

KAI's test suite proves this works:

```yaml
# From: .github/workflows/on-pr.yaml:146-149
- name: Deploy fake gpu operator
  run: |
    helm upgrade -i gpu-operator oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator \
        --namespace gpu-operator --create-namespace --version 0.0.62
```

The fake operator provides:
- ✅ Node labels (gpu.count, gpu.memory)
- ✅ Resource allocatable (nvidia.com/gpu)
- ❌ No actual GPU hardware
- ❌ No GPU drivers
- ❌ No ClusterPolicy CRD

**Result**: KAI scheduler works perfectly for testing and validation.

## Conclusion

The KAI scheduler's "requirement" for NVIDIA GPU Operator is actually a requirement for:
1. **Node labels** (`nvidia.com/gpu.count`, `nvidia.com/gpu.memory`)
2. **GPU resources** (`nvidia.com/gpu` in node allocatable)

These can be provided by:
- Full NVIDIA GPU Operator (production)
- NVIDIA Device Plugin + manual labels (lightweight)
- fake-gpu-operator (testing)
- Any custom solution that provides these labels/resources

The scheduler **does not directly interact** with GPU Operator's core components like driver installer, DCGM, or container runtime configuration.
