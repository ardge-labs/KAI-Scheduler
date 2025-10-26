# Jetson Hardcoded Paths Reasoning

**Date**: 2025-10-26
**Commit**: c0563bc - "fix: add Tegra/Thor NVML support for GPU reservation pods"
**Platform**: NVIDIA Jetson (Jetson Thor) on k3s

## The Question

Why does KAI-Scheduler need hardcoded paths (volume mounts) for Jetson (Tegra) when it works without any code changes on x86 + NVIDIA GPU systems?

## The Answer: nvidia-container-toolkit Automation Failure

### How It's Supposed to Work (x86 + NVIDIA GPU)

On x86 systems with NVIDIA GPUs, the **nvidia-container-toolkit** provides full automation:

1. **Detection**: When containerd sees `runtimeClassName: nvidia` in a pod spec
2. **Hook Invocation**: containerd invokes the nvidia-container-runtime-hook
3. **Environment Reading**: Hook reads `NVIDIA_DRIVER_CAPABILITIES` (e.g., "utility")
4. **Library Mounting**: Hook reads CSV specifications from `/etc/nvidia-container-runtime/host-files-for-container.d/drivers.csv`
5. **Auto-Mount**: Hook automatically bind-mounts libraries listed in CSV into the container
6. **Device Injection**: Hook injects GPU device files (`/dev/nvidia0`, `/dev/nvidiactl`, etc.)
7. **Path Configuration**: Hook sets up library search paths automatically

**Result**: Application code doesn't need to know about library paths or device files. Just set `runtimeClassName: nvidia` and it works.

### What Actually Happens on Jetson Thor

Even though the same infrastructure exists:
- ✅ CSV file exists: `/etc/nvidia-container-runtime/host-files-for-container.d/drivers.csv`
- ✅ CSV correctly lists: `lib, /usr/lib/aarch64-linux-gnu/nvidia/libnvidia-ml.so.1`
- ✅ Pod has: `runtimeClassName: nvidia`
- ✅ Pod has: `NVIDIA_DRIVER_CAPABILITIES=utility`
- ✅ Runtime hook exists: `/usr/local/nvidia/toolkit/nvidia-container-runtime-hook`

**But the libraries are NOT mounted into the container.**

### Evidence from Testing

#### Test 1: Before Any Workarounds
```
Error: ERROR_LIBRARY_NOT_FOUND
```
- Container cannot find `libnvidia-ml.so.1`
- NVML initialization fails immediately
- Reservation pods crash on startup

#### Test 2: After Adding Environment Variables Only
```yaml
env:
- name: NVIDIA_DRIVER_CAPABILITIES
  value: utility
```
**Result**: Still `ERROR_LIBRARY_NOT_FOUND`
- Environment variable alone doesn't trigger the hook
- Libraries still not mounted

#### Test 3: After Adding Manual HostPath Mounts
```yaml
volumeMounts:
- name: nvidia-libs
  mountPath: /usr/lib/aarch64-linux-gnu/nvidia
  readOnly: true

volumes:
- name: nvidia-libs
  hostPath:
    path: /usr/lib/aarch64-linux-gnu/nvidia
```
**Result**: ✅ SUCCESS
- Library is accessible
- But still `Driver Not Loaded` errors

#### Test 4: After Adding Device Mounts + Privileged
```yaml
volumeMounts:
- name: dev-nvidia
  mountPath: /dev

securityContext:
  privileged: true
```
**Result**: ✅ FULL SUCCESS
- NVML initializes successfully
- GPU UUID discovered
- Reservation pods work

## Root Causes: Why Automation Fails on Tegra

### 1. Platform Detection Issues

The nvidia-container-runtime may not correctly identify Jetson Thor as a supported platform:
- Runtime expects standard discrete GPU setup
- Tegra is an integrated GPU with different driver architecture
- Platform detection logic may not handle Tegra SoC

### 2. Library Path Differences

**x86 NVIDIA GPU**:
```bash
/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1  # In standard path
ldconfig -p | grep nvidia-ml  # Found automatically
```

**Jetson Thor**:
```bash
/usr/lib/aarch64-linux-gnu/nvidia/libnvidia-ml.so.1  # In subdirectory
ldconfig -p | grep nvidia-ml  # Found (via config), but in non-standard location
```

The subdirectory location may confuse the runtime hook's mounting logic.

### 3. k3s + containerd + Tegra Combination

This specific combination may not be well-tested by NVIDIA:
- Most Tegra deployments historically used Docker, not k3s
- k3s uses embedded containerd with specific configuration
- The containerd → runtime hook → CSV mounting chain has multiple integration points that can fail

### 4. Device File Differences

**Standard GPU Devices (x86)**:
```bash
/dev/nvidia0
/dev/nvidiactl
/dev/nvidia-uvm
/dev/nvidia-modeset
```

**Tegra-Specific Additional Devices**:
```bash
/dev/nvmap              # Tegra memory mapping
/dev/nvhost-*           # Tegra host interfaces
/dev/tegra-soc-hwpm     # Tegra performance monitoring
/dev/nvsciipc           # Tegra inter-process communication
```

The runtime hook may not know to inject these Tegra-specific devices.

### 5. Runtime Mode Detection Failure

We tested three modes in `/etc/nvidia-container-runtime/config.toml`:
- `mode = "auto"` - Didn't work
- `mode = "csv"` - Didn't work
- `mode = "legacy"` - Didn't work

None triggered the automatic library mounting, suggesting the runtime hook isn't being invoked correctly or is failing silently.

## The Workaround: Explicit Mounts

Since the nvidia-container-toolkit automation doesn't work, we bypass it entirely with explicit Kubernetes volume mounts:

```go
// pkg/binder/binding/resourcereservation/resource_reservation.go

// 1. Mount NVIDIA libraries explicitly
VolumeMounts: []v1.VolumeMount{
    {
        Name:      "nvidia-libs",
        MountPath: "/usr/lib/aarch64-linux-gnu/nvidia",
        ReadOnly:  true,
    },
    {
        Name:      "dev-nvidia",
        MountPath: "/dev",  // All GPU device files
    },
},

Volumes: []v1.Volume{
    {
        Name: "nvidia-libs",
        VolumeSource: v1.VolumeSource{
            HostPath: &v1.HostPathVolumeSource{
                Path: "/usr/lib/aarch64-linux-gnu/nvidia",
                Type: func() *v1.HostPathType {
                    t := v1.HostPathDirectory
                    return &t
                }(),
            },
        },
    },
    {
        Name: "dev-nvidia",
        VolumeSource: v1.VolumeSource{
            HostPath: &v1.HostPathVolumeSource{
                Path: "/dev",
                Type: func() *v1.HostPathType {
                    t := v1.HostPathDirectory
                    return &t
                }(),
            },
        },
    },
},

// 2. Set library search path explicitly
Env: []v1.EnvVar{
    {
        Name:  "LD_LIBRARY_PATH",
        Value: "/usr/lib/aarch64-linux-gnu/nvidia",
    },
    {
        Name:  "NVIDIA_DRIVER_CAPABILITIES",
        Value: "utility",  // Still needed for documentation
    },
},

// 3. Grant device access permissions
SecurityContext: &v1.SecurityContext{
    Privileged: func() *bool { b := true; return &b }(),
},
```

## Why This Works

1. **Direct Library Access**: Bypasses ldconfig and runtime hook entirely
2. **Device File Access**: `/dev` mount provides all GPU and Tegra-specific devices
3. **Explicit Path**: `LD_LIBRARY_PATH` tells the linker exactly where to find NVML
4. **Privileged Mode**: Grants permissions to access device files like `/dev/nvmap`

## Is This a Hack?

**Yes, but a necessary one.**

### On x86 Systems
- No hardcoded paths needed
- Runtime automation handles everything
- Clean separation of concerns

### On Tegra Systems
- Hardcoded paths required
- Runtime automation broken
- Forced to embed platform-specific knowledge

## Comparison: x86 vs Tegra

| Aspect | x86 + NVIDIA GPU | Jetson Thor | Impact |
|--------|------------------|------------|--------|
| nvidia-container-toolkit | ✅ Works | ❌ Broken | Need manual mounts |
| Library path | Standard location | Subdirectory | Need LD_LIBRARY_PATH |
| Device files | Auto-injected | Not injected | Need /dev mount |
| Permissions | Auto-configured | Need privileged | Security concern |
| Code changes | None needed | Platform-specific | Maintenance burden |

## Why x86 Doesn't Need This

On x86, the exact same pod spec:
```yaml
spec:
  runtimeClassName: nvidia
  containers:
  - name: app
    image: myapp
    env:
    - name: NVIDIA_DRIVER_CAPABILITIES
      value: utility
    resources:
      limits:
        nvidia.com/gpu: 1
```

**Automatically gets**:
- Libraries mounted by runtime hook
- Device files injected
- Paths configured
- No code changes in application

**The KAI-Scheduler resource reservation code has no x86-specific paths because the runtime handles it.**

## Future: How to Eliminate This Hack

### Short Term (KAI-Scheduler)
1. Add platform detection to only apply Tegra mounts when on Tegra
2. Add configuration flag to enable/disable Tegra mode
3. Document this as Tegra-specific workaround

### Long Term (NVIDIA JetPack)
NVIDIA should fix these issues (see `JETPACK_IMPROVEMENTS_NEEDED.md`):
1. Fix nvidia-container-toolkit to work on Tegra + k3s
2. Fix GPU Feature Discovery to auto-label Tegra nodes
3. Provide official Tegra configuration templates
4. Test and support k3s + containerd + Tegra combination

Once NVIDIA fixes the runtime automation on Tegra, these hardcoded paths can be removed.

## Related Issues

### Issue 1: GPU Memory Label
- **Problem**: GFD doesn't auto-label `nvidia.com/gpu.memory` on Tegra
- **Workaround**: Manual labeling `kubectl label node <node> nvidia.com/gpu.memory=131072`
- **Root Cause**: GFD doesn't properly detect Tegra GPU memory via NVML

### Issue 2: Physical GPU Count Label
- **Problem**: GFD doesn't auto-label `nvidia.com/gpu.count` on Tegra
- **Workaround**: Manual labeling `kubectl label node <node> nvidia.com/gpu.count=1`
- **Impact**: Without this, time-slicing VRAM accounting is incorrect

### Issue 3: NVML Warnings
- **Problem**: Tegra shows scary NVML errors but still works
```
NvRmMemInitNvmap failed: error Permission denied
NvRmMemMgrInit failed: Memory Manager Not supported
```
- **Root Cause**: Tegra-specific memory management unavailable in containers
- **Impact**: Confusing logs, but functionality works

## Testing Validation

### Verification That Workaround is Necessary

To confirm the workaround is still needed, one could:

1. **Remove the hardcoded mounts** from `resource_reservation.go`
2. **Rebuild** the binder image
3. **Deploy** to Tegra system
4. **Observe**: Reservation pods will crash with `ERROR_LIBRARY_NOT_FOUND`

This confirms the nvidia-container-toolkit automation is still broken.

### Verification That Workaround Works

Current test results:
```bash
$ kubectl get pods -n default -l app=ubuntu-test
NAME                           READY   STATUS    RESTARTS   AGE
ubuntu-test-6f6fb8fff9-555wk   1/1     Running   0          45s
ubuntu-test-6f6fb8fff9-k7s57   1/1     Running   0          45s
ubuntu-test-6f6fb8fff9-m7cjw   1/1     Running   0          45s
ubuntu-test-6f6fb8fff9-mrfq8   1/1     Running   0          45s
ubuntu-test-6f6fb8fff9-tcwmg   0/1     Pending   0          45s  # Correct: insufficient VRAM
```

- ✅ 4 pods running (requesting 120GB total)
- ✅ 1 pod pending (would need 150GB total, but only 128GB available)
- ✅ VRAM accounting working correctly
- ✅ Scheduler properly using physical GPU count for VRAM calculation

## Conclusion

The hardcoded paths in commit `c0563bc` are:

1. **Necessary**: nvidia-container-toolkit automation doesn't work on Tegra + k3s
2. **Platform-Specific**: Only needed on Tegra, not on x86
3. **A Workaround**: Bypassing broken automation rather than fixing root cause
4. **Temporary**: Should be removed once NVIDIA fixes JetPack + container toolkit

Without these hardcoded paths, KAI-Scheduler cannot function on Jetson systems because the GPU reservation pods cannot initialize NVML.

## References

- Commit: `c0563bc` - fix: add Tegra/Thor NVML support for GPU reservation pods
- Commit: `73d04f6` - fix: fix the available vram miscalculation problem (time-slicing)
- Related Docs:
  - `TEGRA_SETUP.md` - Setup instructions for Tegra
  - `JETPACK_IMPROVEMENTS_NEEDED.md` - What NVIDIA should fix
  - Test file: `multiubuntu.yaml` - GPU memory scheduling validation
