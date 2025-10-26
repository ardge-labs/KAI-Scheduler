# KAI-Scheduler Setup for NVIDIA Jetson (Tegra Thor) Systems

This document describes the configuration changes needed to run KAI-Scheduler on NVIDIA Jetson (Tegra Thor) systems with k3s.

## System Configuration Changes

### 1. NVIDIA Container Runtime Configuration

**File**: `/etc/nvidia-container-runtime/config.toml`

**Change Made**:
```toml
mode = "legacy"
```

**Original Value**: `mode = "auto"`

**Reason**:
On Jetson (Tegra Thor) systems, the nvidia-container-runtime's automatic mode detection doesn't properly mount NVML libraries and device files. Setting it to "legacy" mode was attempted but ultimately the automatic library mounting didn't work correctly on this platform. The solution was to explicitly mount required paths in the code (see below).

### 2. K3s Restart After Configuration Change

After modifying the nvidia-container-runtime config, k3s was restarted:
```bash
sudo systemctl restart k3s
```

## Why Tegra Requires Code Changes (vs x86 NVIDIA GPUs)

### The Problem: nvidia-container-toolkit Doesn't Work on Tegra + k3s

On standard **x86 systems with NVIDIA GPUs**, the `nvidia-container-toolkit` automatically:
- Detects `runtimeClassName: nvidia` in pod specs
- Reads `NVIDIA_DRIVER_CAPABILITIES` environment variable
- Automatically mounts NVML libraries from CSV mount specifications
- Injects GPU device files (`/dev/nvidia*`)
- Configures library paths

**On Tegra/Jetson Thor systems**, even though:
- ✅ CSV file lists the NVML library path: `/usr/lib/aarch64-linux-gnu/nvidia/libnvidia-ml.so.1`
- ✅ `runtimeClassName: nvidia` is set
- ✅ `NVIDIA_DRIVER_CAPABILITIES=utility` is set

**The nvidia-container-runtime hook does NOT mount the libraries.**

### Root Causes

1. **Library path differences**: Tegra uses `/usr/lib/aarch64-linux-gnu/nvidia/` subdirectory (not in standard linker search path)
2. **Additional Tegra devices**: Requires `/dev/nvmap`, `/dev/nvhost-*` in addition to standard `/dev/nvidia*`
3. **k3s + containerd + Tegra**: This combination may not be fully tested/supported by NVIDIA's container toolkit
4. **Platform detection**: Runtime may not correctly identify Tegra Thor platform

### Evidence from Testing

- **Before manual mounts**: `ERROR_LIBRARY_NOT_FOUND` - libraries not accessible
- **After env var only**: Still `ERROR_LIBRARY_NOT_FOUND` - automation doesn't trigger
- **After manual hostPath mounts**: ✅ SUCCESS - bypassing nvidia-container-runtime

## Code Changes Required (Already Committed)

To work around the nvidia-container-runtime issues on Tegra, the following changes were made to `pkg/binder/binding/resourcereservation/resource_reservation.go`:

### 1. Environment Variables
- `NVIDIA_DRIVER_CAPABILITIES=utility` - Enables NVML library access
- `LD_LIBRARY_PATH=/usr/lib/aarch64-linux-gnu/nvidia` - Adds NVIDIA library path to linker search

### 2. Volume Mounts
- **NVIDIA Libraries**: Mount `/usr/lib/aarch64-linux-gnu/nvidia` (read-only)
  - Contains `libnvidia-ml.so.1` and other NVIDIA libraries
- **Device Files**: Mount `/dev`
  - Provides access to NVIDIA device files like `/dev/nvidia0`, `/dev/nvidiactl`, `/dev/nvmap`, etc.

### 3. Security Context
- **Privileged mode**: Required for GPU device access on Jetson systems

### Important Notes

**These hardcoded paths are a workaround** for nvidia-container-toolkit not functioning correctly on Tegra + k3s.

On x86 systems with NVIDIA GPUs, these manual mounts are **not needed** because the nvidia-container-runtime automatically handles everything when `runtimeClassName: nvidia` is set.

**Future improvements**:
- Make the Tegra-specific mounts conditional (detect platform automatically)
- Report to NVIDIA as a potential bug in nvidia-container-toolkit for Jetson
- Add configuration option to enable/disable Tegra mode
- Investigate if newer GPU Operator versions resolve this issue

## Node Configuration

### Required Node Labels

The kai-scheduler requires the following labels on GPU nodes. These must be set manually on Jetson systems:

#### 1. GPU Memory Label

```bash
kubectl label node <node-name> nvidia.com/gpu.memory=131072 --overwrite
```

**Note**: The value is in MB. For 128GB GPU memory: 128 × 1024 = 131072

**Reason**: The GPU Feature Discovery (GFD) component of the NVIDIA GPU Operator doesn't correctly detect GPU memory on Tegra Thor systems.

#### 2. Physical GPU Count Label (Required for Time-Slicing)

```bash
kubectl label node <node-name> nvidia.com/gpu.count=1 --overwrite
```

**Note**: This is the number of **physical** GPUs, not the time-sliced replica count.

**Reason**: When GPU time-slicing is enabled (e.g., `nvidia.com/gpu.replicas=100`), the scheduler needs to know the physical GPU count for proper VRAM accounting. All time-sliced replicas share the same physical GPU's VRAM, so the scheduler must use the physical count when calculating available VRAM. See commit [73d04f6](https://github.com/your-repo/commits/73d04f6) for details.

## Build and Deployment

### Image Import to k3s

Since k3s uses containerd as its container runtime (separate from Docker), locally built images must be imported:

The `builddeploy.sh` script handles this:

```bash
#!/bin/sh
make build

# Import all locally built images to k3s containerd
echo "Importing images to k3s..."
docker images --format "{{.Repository}}:{{.Tag}}" | grep "registry/local/kai-scheduler.*:0.0.0" | while read image; do
    echo "  Importing $image"
    docker save "$image" | sudo k3s ctr images import -
done

helm package ./deployments/kai-scheduler -d ./charts
helm uninstall kai-scheduler -n kai-scheduler
kubectl -n kai-scheduler delete jobs crd-upgrader
helm upgrade -i kai-scheduler -n kai-scheduler  --create-namespace \
  --set "global.gpuSharing=true" \
  --set "scheduler.additionalArgs[0]=--v=4" \
  --set "crdupgrader.image.pullPolicy=Never" \
  ./charts/kai-scheduler-0.0.0.tgz
```

**Key Points**:
- Images are built with Docker but must be imported to k3s containerd
- `crdupgrader.image.pullPolicy=Never` prevents k3s from trying to pull images from a remote registry

## Verification

After deployment, verify the scheduler is working:

```bash
# Check all kai-scheduler components are running
kubectl -n kai-scheduler get all

# Check GPU reservation pods can initialize NVML
kubectl -n kai-resource-reservation get pods

# Deploy a test workload
kubectl apply -f multiubuntu.yaml

# Verify pods are scheduled
kubectl get pods -n default -l app=ubuntu-test
```

## Troubleshooting

### Common Issues

1. **"ERROR_LIBRARY_NOT_FOUND"**
   - Verify `/usr/lib/aarch64-linux-gnu/nvidia/libnvidia-ml.so.1` exists on the host
   - Check the volume mount is configured correctly in the code

2. **"Driver Not Loaded" or "Permission denied"**
   - Ensure privileged security context is enabled
   - Verify `/dev` volume mount is configured
   - Check device files exist: `ls -la /dev | grep nvidia`

3. **Image Pull Errors**
   - Verify images are imported to k3s: `sudo k3s ctr images list | grep kai-scheduler`
   - Check `imagePullPolicy` is set to `Never` for local images

4. **Pods not being scheduled**
   - Verify GPU memory label: `kubectl describe node <node-name> | grep nvidia.com/gpu.memory`
   - Check scheduler logs: `kubectl logs -n kai-scheduler deployment/scheduler`

## System Information

This configuration was tested on:
- **Platform**: NVIDIA Jetson/Tegra Thor
- **OS**: Linux 6.8.12-tegra
- **K3s Version**: Using containerd runtime
- **NVIDIA Driver**: Tegra-based drivers with NVML support in CUDA Toolkit 13.0+

## References

- NVIDIA Tegra Thor NVML Support: https://developer.nvidia.com/blog/whats-new-in-cuda-toolkit-13-0-for-jetson-thor-unified-arm-ecosystem-and-more/
- NVIDIA Container Toolkit: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/
