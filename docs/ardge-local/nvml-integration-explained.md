# NVML Integration in KAI-Scheduler: Why Jetson/Thor Hacks Are Necessary

## Overview

This document explains how KAI-Scheduler uses NVIDIA Management Library (NVML) for GPU reservation and why explicit volume mounts and environment variables are required on Jetson Tegra/Thor platforms.

## What is NVML?

**NVML (NVIDIA Management Library)** is a C library (`libnvidia-ml.so`) that provides programmatic access to NVIDIA GPU devices. Applications use it to:
- Query GPU information (temperature, memory, utilization)
- Get GPU device IDs and UUIDs
- Enumerate available GPUs
- Monitor GPU health and performance

## How KAI-Scheduler Uses NVML

### The Problem KAI-Scheduler Solves

When multiple pods share a single GPU (fractional GPU scheduling), the scheduler needs to:
1. Identify which specific GPU device a pod should use
2. Track which GPUs are already reserved
3. Get the GPU's unique identifier (UUID) for proper device allocation

### The GPU Reservation Pod Workflow

```
1. Scheduler decides pod needs fractional GPU
   ↓
2. Binder creates "GPU Reservation Pod"
   ↓
3. Reservation Pod runs `resource-reservation` binary
   ↓
4. `resource-reservation` calls NVML to get GPU UUID
   ↓
5. Pod annotates itself with GPU UUID
   ↓
6. Scheduler uses this UUID for workload placement
```

## Complete NVML Call Chain

### 1. Scheduler Creates BindRequest
**File**: `pkg/scheduler/framework/session.go:118`
```go
Session.BindPod() → Cache.Bind()
```
Creates a `BindRequest` Kubernetes custom resource.

### 2. BindRequest Controller Reconciles
**File**: `pkg/binder/controllers/bindrequest_controller.go:155`
```go
BindRequestReconciler.Reconcile() → binder.Bind()
```
Watches for `BindRequest` CRDs and triggers binding process.

### 3. Binder Reserves GPU
**File**: `pkg/binder/binding/binder.go:101`
```go
Binder.reserveGPUs() → resourceReservationService.ReserveGpuDevice()
```
For fractional GPU workloads, reserves GPU resources.

### 4. Resource Reservation Service Creates Pod
**File**: `pkg/binder/binding/resourcereservation/resource_reservation.go:377-540`
```go
service.createGPUReservationPod() → createResourceReservationPod()
```

**This is where the Jetson/Thor-specific NVML configuration happens:**

```go
// Lines 465-489: Environment variables for NVML
Env: []v1.EnvVar{
    {
        Name:  "NVIDIA_DRIVER_CAPABILITIES",
        Value: "utility",  // Enables NVML library access
    },
    {
        Name:  "LD_LIBRARY_PATH",
        Value: "/usr/lib/aarch64-linux-gnu/nvidia",  // Tell linker where to find NVML
    },
},

// Lines 491-501: Volume mounts
VolumeMounts: []v1.VolumeMount{
    {
        Name:      "nvidia-libs",
        MountPath: "/usr/lib/aarch64-linux-gnu/nvidia",  // NVML library location
        ReadOnly:  true,
    },
    {
        Name:      "dev-nvidia",
        MountPath: "/dev",  // GPU device files
    },
},

// Lines 504-523: Host path volumes
Volumes: []v1.Volume{
    {
        Name: "nvidia-libs",
        VolumeSource: v1.VolumeSource{
            HostPath: &v1.HostPathVolumeSource{
                Path: "/usr/lib/aarch64-linux-gnu/nvidia",  // Host NVML library path
            },
        },
    },
    {
        Name: "dev-nvidia",
        VolumeSource: v1.VolumeSource{
            HostPath: &v1.HostPathVolumeSource{
                Path: "/dev",  // Host GPU device files
            },
        },
    },
},
```

### 5. Reservation Pod Starts and Calls NVML
**File**: `cmd/resourcereservation/app/app.go:61`
```go
gpuDevice, err = discovery.GetGPUDevice(ctx)
```

### 6. NVML Functions Are Called
**File**: `pkg/resourcereservation/discovery/discovery.go:14-49`

This is the **actual code that calls NVML library functions**:

```go
func GetGPUDevice(ctx context.Context) (string, error) {
    // Initialize NVML library
    ret := nvml.Init()  // Line 17
    if ret != nvml.SUCCESS {
        return "", fmt.Errorf("unable to initialize NVML: %v", nvml.ErrorString(ret))
    }
    defer nvml.Shutdown()

    // Count available GPUs
    count, ret := nvml.DeviceGetCount()  // Line 28
    if ret != nvml.SUCCESS {
        return "", fmt.Errorf("unable to get device count: %v", nvml.ErrorString(ret))
    }

    // Get handle to first GPU
    device, ret := nvml.DeviceGetHandleByIndex(0)  // Line 38
    if ret != nvml.SUCCESS {
        return "", fmt.Errorf("unable to get device handle: %v", nvml.ErrorString(ret))
    }

    // Get GPU UUID - this is the critical information!
    uuid, ret := device.GetUUID()  // Line 43
    if ret != nvml.SUCCESS {
        return "", fmt.Errorf("unable to get device uuid: %v", nvml.ErrorString(ret))
    }

    return uuid, nil
}
```

**NVML functions used:**
- `nvml.Init()` - Initialize NVML (requires `libnvidia-ml.so`)
- `nvml.DeviceGetCount()` - Count GPUs visible to pod
- `nvml.DeviceGetHandleByIndex(0)` - Get handle to first GPU
- `device.GetUUID()` - Get GPU's unique identifier
- `nvml.Shutdown()` - Clean shutdown

### 7. UUID Annotation
**File**: `cmd/resourcereservation/app/app.go:67`
```go
patch.PatchDeviceInfo(ctx, gpuDevice)
```
Patches the reservation pod with the GPU UUID as an annotation.

### 8. Binder Reads Annotation
**File**: `pkg/binder/binding/resourcereservation/resource_reservation.go:430`
```go
gpuIndex = pod.Annotations[gpuIndexAnnotationName]
```
Returns GPU index to scheduler for workload placement.

## Why the Jetson/Thor Hacks Are Necessary

### The Problem on Standard x86 Systems

On typical x86 NVIDIA systems:
- NVML libraries are in standard paths like `/usr/lib/x86_64-linux-gnu/`
- The `nvidia-container-runtime` automatically mounts these paths
- Environment variables are set automatically
- Everything "just works"

### The Problem on Jetson Tegra/Thor

On Jetson platforms:
1. **Non-standard library location**: NVML is in `/usr/lib/aarch64-linux-gnu/nvidia/`
2. **Not in linker search path**: The dynamic linker doesn't know to look there
3. **Runtime auto-detection fails**: `nvidia-container-runtime` doesn't properly detect Tegra libraries
4. **CSV mount specs don't work**: Even though Tegra has CSV files listing the libraries, the automatic mounting fails

### What Happens Without These Hacks

**Without the explicit volume mounts and environment variables:**

```bash
# Inside the reservation pod:
$ ./resource-reservation

ERROR: unable to initialize NVML: ERROR_LIBRARY_NOT_FOUND
# The application can't find libnvidia-ml.so

# Or if the library is found but devices aren't mounted:
ERROR: unable to initialize NVML: Driver Not Loaded
# The library can't access /dev/nvidia* device files
```

**Result**: KAI-Scheduler cannot function because it can't discover GPU UUIDs.

### How the Hacks Fix It

**1. `LD_LIBRARY_PATH=/usr/lib/aarch64-linux-gnu/nvidia`**
- Tells the dynamic linker exactly where to find `libnvidia-ml.so`
- Overrides the automatic detection that fails on Tegra

**2. Volume Mount: `/usr/lib/aarch64-linux-gnu/nvidia`**
- Makes the NVML library available inside the container
- Required because automatic library mounting doesn't work

**3. Volume Mount: `/dev`**
- Provides access to GPU device files (`/dev/nvidia0`, `/dev/nvidiactl`, etc.)
- NVML needs these to communicate with GPU hardware

**4. `NVIDIA_DRIVER_CAPABILITIES=utility`**
- Tells nvidia-container-runtime to enable NVML access
- Without this, the runtime may block NVML functionality

**5. `Privileged: true`**
- Grants permissions to access `/dev` device files
- Required for NVML to communicate with GPU hardware

## Impact on KAI-Scheduler

### Without NVML Working
- ❌ Cannot identify GPU devices
- ❌ Cannot track GPU reservations
- ❌ Fractional GPU scheduling completely broken
- ❌ Multiple pods would try to use same GPU without coordination

### With NVML Working (via these hacks)
- ✅ GPU UUIDs properly discovered
- ✅ GPU reservation pods can annotate themselves
- ✅ Fractional GPU scheduling works correctly
- ✅ Workloads properly isolated to specific GPUs

## Related Files and Commits

### Key Source Files
- **NVML calls**: `pkg/resourcereservation/discovery/discovery.go`
- **Reservation pod config**: `pkg/binder/binding/resourcereservation/resource_reservation.go:437-540`
- **Reservation binary**: `cmd/resourcereservation/app/app.go`
- **Binder logic**: `pkg/binder/binding/binder.go`
- **Controller**: `pkg/binder/controllers/bindrequest_controller.go`

### Related Commits
- `c0563bc` - "fix: add Tegra/Thor NVML support for GPU reservation pods"

### Related Documentation
- `jetson-hardcoded-paths-reasoning.md` - Detailed reasoning for hardcoded paths
- `jetson-setup.md` - Jetson setup instructions
- `jetpack-improvements-needed.md` - Future improvements for Tegra support

## Why Can't This Be Fixed Upstream?

### Ideal Solution Would Be:
1. NVIDIA updates nvidia-container-runtime to properly detect Tegra library paths
2. NVIDIA updates CSV mount specifications for Tegra
3. JetPack includes NVML libraries in standard linker search paths

### Current Reality:
- Tegra is a specialized embedded platform
- Container support is not the primary use case
- nvidia-container-runtime is optimized for datacenter/x86 workflows
- Jetson containers are an afterthought

### Therefore:
**These "hacks" are necessary workarounds until NVIDIA provides proper Tegra container support.**

## Verification

### How to Test NVML is Working

```bash
# Deploy a GPU reservation pod
kubectl get pods -n kai-system -l app=resource-reservation

# Check logs for successful UUID discovery
kubectl logs -n kai-system <reservation-pod-name>

# Should see:
# "Looking for GPU device id for pod"
# "Found 1 GPU devices"
# "Found GPU device" deviceId="GPU-xxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
# "Pod was updated with GPU device UUID"
```

### How to Test Without the Hacks (will fail)

Remove the volume mounts and environment variables from `resource_reservation.go:465-522`, then:

```bash
# Build and deploy
make build && make deploy

# Check reservation pod logs
kubectl logs -n kai-system <reservation-pod-name>

# Will see:
# ERROR: unable to initialize NVML: ERROR_LIBRARY_NOT_FOUND
```

## Summary

The explicit NVML volume mounts and environment variables in KAI-Scheduler are **not optional workarounds** - they are **essential infrastructure** for the scheduler to function on Jetson Tegra/Thor platforms.

Without them:
- NVML initialization fails
- GPU discovery is impossible
- Fractional GPU scheduling cannot work
- KAI-Scheduler is completely broken on Jetson

These "hacks" are the difference between a working scheduler and a non-functional one on Tegra platforms.
