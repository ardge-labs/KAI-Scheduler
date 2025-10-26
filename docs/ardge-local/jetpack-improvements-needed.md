# JetPack Improvements Needed for Kubernetes GPU Scheduling

This document lists improvements NVIDIA should make to JetPack (Tegra/Jetson software stack) to achieve parity with x86 NVIDIA GPU support in Kubernetes environments.

## Current State: What Works on x86 but Fails on Tegra

On **x86 + NVIDIA GPU**:
- ✅ nvidia-container-toolkit automatically mounts libraries
- ✅ GPU device files are automatically injected
- ✅ NVML initialization works seamlessly
- ✅ No code changes needed in schedulers/operators

On **Tegra/Jetson Thor**:
- ❌ nvidia-container-toolkit doesn't mount libraries despite correct CSV configuration
- ❌ Requires manual hostPath volume mounts
- ❌ Requires hardcoded paths in application code
- ❌ Requires privileged security context workarounds

---

## Required JetPack Improvements

### 1. Fix nvidia-container-toolkit Runtime Hook for Tegra

**Problem**: The nvidia-container-runtime hook doesn't automatically mount libraries on Tegra systems even with:
- `runtimeClassName: nvidia` set
- `NVIDIA_DRIVER_CAPABILITIES=utility` environment variable
- Correct CSV mount specifications in `/etc/nvidia-container-runtime/host-files-for-container.d/drivers.csv`

**Required Fix**:
```
When nvidia-container-runtime hook detects:
- Tegra platform (via /etc/nv_tegra_release or similar)
- Container with runtimeClassName: nvidia
- NVIDIA_DRIVER_CAPABILITIES environment variable

THEN automatically mount:
- Libraries from /usr/lib/aarch64-linux-gnu/nvidia/
- Device files from /dev/nvidia*, /dev/nvmap, /dev/nvhost-*
- Set LD_LIBRARY_PATH appropriately
```

**Test Cases**:
- Container with `runtimeClassName: nvidia` + `NVIDIA_DRIVER_CAPABILITIES=utility` should have libnvidia-ml.so.1 accessible without manual mounts
- Container should be able to call `nvmlInit()` successfully
- Should work on k3s, k8s, containerd, Docker

**Impact**: Eliminates need for hardcoded volume mounts in scheduler code

---

### 2. Standardize Library Paths or Fix ldconfig Integration

**Problem**: NVML libraries on Tegra are in `/usr/lib/aarch64-linux-gnu/nvidia/` which is not in the standard linker search path.

**Option A: Move to Standard Path** (Breaking change)
```bash
# Install libraries directly in standard path
/usr/lib/aarch64-linux-gnu/libnvidia-ml.so.1
# Not in subdirectory: /usr/lib/aarch64-linux-gnu/nvidia/
```

**Option B: Fix ldconfig Configuration** (Preferred - backward compatible)
```bash
# Add ldconfig configuration
echo "/usr/lib/aarch64-linux-gnu/nvidia" > /etc/ld.so.conf.d/nvidia-tegra.conf
ldconfig

# Verify
ldconfig -p | grep nvidia-ml
# Should show: libnvidia-ml.so.1 => /usr/lib/aarch64-linux-gnu/nvidia/libnvidia-ml.so.1
```

**Option C: Fix nvidia-container-runtime to Set LD_LIBRARY_PATH**
- Runtime hook should automatically add `/usr/lib/aarch64-linux-gnu/nvidia` to LD_LIBRARY_PATH
- Similar to how it handles CUDA library paths

**Impact**: Applications wouldn't need to manually set `LD_LIBRARY_PATH`

---

### 3. Improve GPU Feature Discovery (GFD) for Tegra

**Problem**: GPU Feature Discovery doesn't correctly label Tegra nodes with required metadata.

**Missing Labels on Tegra**:
- `nvidia.com/gpu.memory` - GPU memory in MB (critical for schedulers)
- `nvidia.com/gpu.count` - Physical GPU count (critical for time-slicing VRAM accounting)

**Current State**:
```bash
# What's missing on Tegra Thor:
kubectl get node -o json | jq '.metadata.labels' | grep nvidia.com/gpu.memory
# Returns: nothing (label doesn't exist)

kubectl get node -o json | jq '.metadata.labels' | grep nvidia.com/gpu.count
# Returns: nothing (label doesn't exist)
```

**Required Fix**:
```yaml
# GFD should automatically add these labels on Tegra:
metadata:
  labels:
    nvidia.com/gpu.memory: "131072"  # 128GB in MB
    nvidia.com/gpu.count: "1"        # Physical GPU count
    nvidia.com/gpu.product: "NVIDIA-Thor-SHARED"  # ✅ Already exists
    nvidia.com/gpu.replicas: "100"   # ✅ Already exists (time-slicing)
```

**Detection Logic**:
```go
// Pseudo-code for GFD on Tegra
if isTegraPlatform() {
    // Read from NVML
    nvmlInit()
    count, _ := nvmlDeviceGetCount()
    device, _ := nvmlDeviceGetHandleByIndex(0)

    // Get memory in bytes, convert to MB
    memInfo, _ := device.GetMemoryInfo()
    memoryMB := memInfo.Total / (1024 * 1024)

    // Label node
    node.Labels["nvidia.com/gpu.memory"] = fmt.Sprintf("%d", memoryMB)
    node.Labels["nvidia.com/gpu.count"] = fmt.Sprintf("%d", count)
}
```

**Impact**: Eliminates need for manual node labeling

---

### 4. Fix Device File Permissions for Non-Privileged Containers

**Problem**: Tegra-specific devices (`/dev/nvmap`, `/dev/nvhost-*`) require privileged containers or manual permission configuration.

**Current Errors** (from non-privileged containers):
```
NvRmMemInitNvmap failed: error Permission denied
NvRmMemMgrInit failed: Memory Manager Not supported
```

**Required Fix**:
```bash
# Ensure proper device permissions in udev rules
# /etc/udev/rules.d/99-tegra-devices.rules

# Make Tegra GPU devices accessible to video group
KERNEL=="nvmap", MODE="0666", GROUP="video"
KERNEL=="nvhost-*", MODE="0666", GROUP="video"
KERNEL=="tegra-soc-hwpm", MODE="0666", GROUP="video"
KERNEL=="nvsciipc", MODE="0666", GROUP="video"

# Or update nvidia-container-toolkit to handle Tegra devices
# in nvidia-container-cli device injection
```

**Alternative**: nvidia-container-runtime should inject Tegra device files with proper permissions automatically (similar to how it injects `/dev/nvidia0`, `/dev/nvidiactl` on x86)

**Impact**:
- Containers wouldn't need `privileged: true`
- Better security posture
- Follows least-privilege principle

---

### 5. Update GPU Operator for Tegra Platform Support

**Problem**: GPU Operator assumes x86 GPU architecture and doesn't handle Tegra differences.

**Required Updates**:

#### 5.1. Device Plugin Configuration
```yaml
# gpu-operator should detect Tegra and configure accordingly
kind: ClusterPolicy
spec:
  devicePlugin:
    config:
      # On Tegra, ensure time-slicing config is properly applied
      name: time-slicing-config-tegra
      default: tegra-100-replicas
```

#### 5.2. GFD Configuration
```yaml
  gfd:
    # Enable Tegra-specific discovery
    env:
    - name: GFD_DETECT_TEGRA
      value: "true"
    - name: GFD_TEGRA_MEMORY_DETECTION
      value: "nvml"  # Use NVML to get accurate memory
```

#### 5.3. Container Toolkit Configuration
```yaml
  toolkit:
    # Ensure Tegra mode is enabled
    env:
    - name: NVIDIA_TEGRA_PLATFORM
      value: "true"
    - name: NVIDIA_CONTAINER_RUNTIME_MODE
      value: "tegra-optimized"  # New mode that handles Tegra specifics
```

**Impact**: GPU Operator would "just work" on Tegra like it does on x86

---

### 6. Improve NVML Error Handling and Fallbacks for Tegra

**Problem**: NVML on Tegra shows warnings/errors for Tegra-specific memory management but still works.

**Current Behavior**:
```
NvRmMemInitNvmap failed: error Permission denied
NvRmMemMgrInit failed: Memory Manager Not supported
libnvrm_gpu.so: NvRmGpuLibOpen failed, error=196625
[But then successfully:]
Found 1 GPU devices
Found GPU device GPU-a7c66ad2-6dbb-0ab8-c1a2-37ba6dba3600
```

**Required Fix**:
1. **Suppress misleading errors**: If NVML core functionality works (device enumeration, UUID retrieval), don't print scary ERROR messages
2. **Better error messages**:
   ```
   INFO: Tegra memory management not available (expected on Thor platform)
   INFO: Using alternative device enumeration method
   ```
3. **Documentation**: Explain which NVML features are/aren't available on Tegra

**Impact**: Less confusion, clearer logs, better debugging

---

### 7. Provide Tegra-Specific nvidia-container-toolkit Configuration Template

**Problem**: Users have to manually figure out containerd + k3s + Tegra configuration.

**Required**:
Provide official `/etc/nvidia-container-runtime/config.toml.tegra` template:

```toml
# /etc/nvidia-container-runtime/config.toml
# Tegra/Jetson optimized configuration

disable-require = false
supported-driver-capabilities = "compat32,compute,display,graphics,ngx,utility,video"

[nvidia-container-cli]
environment = []
ldconfig = "@/sbin/ldconfig.real"
load-kmods = true

[nvidia-container-runtime]
log-level = "info"
mode = "tegra"  # New mode specifically for Tegra

  [nvidia-container-runtime.modes.tegra]
    # Tegra-specific library paths
    library-paths = ["/usr/lib/aarch64-linux-gnu/nvidia"]
    # Tegra-specific device files
    device-files = [
      "/dev/nvidia*",
      "/dev/nvmap",
      "/dev/nvhost-*",
      "/dev/nvsciipc",
      "/dev/tegra-soc-hwpm"
    ]
    # Auto-configure LD_LIBRARY_PATH
    auto-library-path = true

[nvidia-container-runtime-hook]
path = "/usr/local/nvidia/toolkit/nvidia-container-runtime-hook"
skip-mode-detection = false  # Let it auto-detect Tegra

[nvidia-ctk]
path = "/usr/local/nvidia/toolkit/nvidia-ctk"
```

**Include in JetPack**:
- Ship this config by default in JetPack
- Provide documentation for k3s integration
- Provide tested k3s containerd config snippets

**Impact**: Users can copy-paste working configuration

---

### 8. Create Tegra Validation Tool

**Problem**: No easy way to validate if Tegra GPU container setup is correct.

**Required**: Ship a validation tool with JetPack:

```bash
# nvidia-tegra-container-validate

#!/bin/bash
echo "NVIDIA Tegra Container Toolkit Validation"
echo "==========================================="

# Test 1: Runtime configuration
echo "[1/5] Checking nvidia-container-runtime configuration..."
if grep -q "mode.*tegra" /etc/nvidia-container-runtime/config.toml; then
    echo "  ✓ Tegra mode enabled"
else
    echo "  ✗ Tegra mode not configured"
fi

# Test 2: Library detection
echo "[2/5] Checking NVML library..."
if ldconfig -p | grep -q nvidia-ml; then
    echo "  ✓ NVML library found in ldconfig"
else
    echo "  ✗ NVML library not in search path"
    echo "    Library location: $(find /usr/lib -name libnvidia-ml.so.1 2>/dev/null)"
fi

# Test 3: Device files
echo "[3/5] Checking GPU device files..."
for dev in /dev/nvidia0 /dev/nvidiactl /dev/nvmap; do
    if [ -e "$dev" ]; then
        echo "  ✓ $dev exists ($(stat -c '%a' $dev))"
    else
        echo "  ✗ $dev missing"
    fi
done

# Test 4: Container runtime test
echo "[4/5] Testing container GPU access..."
if command -v docker &>/dev/null; then
    docker run --rm --runtime=nvidia -e NVIDIA_VISIBLE_DEVICES=all \
        nvcr.io/nvidia/l4t-base:r36.2.0 nvidia-smi -L &>/dev/null
    if [ $? -eq 0 ]; then
        echo "  ✓ Container GPU access working"
    else
        echo "  ✗ Container GPU access failed"
    fi
fi

# Test 5: Kubernetes readiness
echo "[5/5] Checking Kubernetes integration..."
if kubectl get node -o jsonpath='{.items[0].status.capacity.nvidia\.com/gpu}' 2>/dev/null | grep -q .; then
    echo "  ✓ GPU detected in Kubernetes"
else
    echo "  ✗ GPU not visible to Kubernetes"
fi

echo
echo "Validation complete. See above for any issues."
```

**Impact**: Easy troubleshooting, faster issue resolution

---

## Summary: Parity Checklist

To achieve x86 parity, JetPack needs:

| Feature | x86 Status | Tegra Status | Required Fix |
|---------|-----------|--------------|--------------|
| Auto-mount libraries via runtime | ✅ Works | ❌ Broken | Fix #1 |
| Libraries in ld search path | ✅ Yes | ❌ No | Fix #2 |
| GFD auto-labels gpu.memory | ✅ Yes | ❌ No | Fix #3 |
| GFD auto-labels gpu.count | ✅ Yes | ❌ No | Fix #3 |
| Non-privileged GPU access | ✅ Yes | ❌ No | Fix #4 |
| GPU Operator support | ✅ Yes | ⚠️ Partial | Fix #5 |
| Clean NVML errors | ✅ Yes | ⚠️ Noisy | Fix #6 |
| Official k8s config docs | ✅ Yes | ❌ No | Fix #7 |
| Validation tooling | ✅ Yes | ❌ No | Fix #8 |

---

## How to Report These Issues to NVIDIA

### 1. nvidia-container-toolkit Issues
**Repository**: https://github.com/NVIDIA/nvidia-container-toolkit
**Title**: "nvidia-container-runtime hook doesn't mount libraries on Jetson Thor + k3s"
**Include**:
- CSV configuration showing correct paths
- Pod spec with runtimeClassName
- Evidence that libraries aren't mounted
- Request for Tegra-specific mode

### 2. GPU Feature Discovery Issues
**Repository**: https://github.com/NVIDIA/gpu-feature-discovery
**Title**: "GFD doesn't label nvidia.com/gpu.memory and nvidia.com/gpu.count on Jetson Thor"
**Include**:
- Missing labels on Tegra nodes
- Comparison with x86 node labels
- Scheduler failures due to missing labels

### 3. GPU Operator Issues
**Repository**: https://github.com/NVIDIA/gpu-operator
**Title**: "GPU Operator doesn't handle Jetson/Tegra platform differences"
**Include**:
- Differences in device files, library paths
- Request for Tegra-specific ClusterPolicy support

### 4. JetPack Documentation
**Forum**: https://forums.developer.nvidia.com/c/agx-autonomous-machines/jetson-embedded-systems/
**Title**: "Official Kubernetes GPU Operator guide needed for JetPack"
**Request**: Official documentation for k3s/k8s + GPU Operator on Jetson

---

## Workaround Status

Until NVIDIA fixes these issues, the workarounds in KAI-Scheduler commit `c0563bc` are necessary:
- Manual hostPath volume mounts
- Explicit LD_LIBRARY_PATH setting
- Privileged security context
- Manual node labeling

These workarounds should be removed once JetPack achieves parity with x86 GPU support.
