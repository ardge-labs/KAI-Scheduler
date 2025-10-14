# Gang Scheduling Example: Application with Dependent Models

## Overview

This example demonstrates a real-world scenario where an inference application depends on two ML model serving pods. All three pods must start together to ensure a consistent deployment.

## Use Case

Imagine you're deploying an AI-powered service where:
- Your **application server** handles user requests
- It requires **two ML models** to be loaded and ready before it can serve traffic
- If models aren't available, the application can't function properly

Gang scheduling solves this by ensuring all components start simultaneously.

## Architecture

```
┌─────────────────┐
│   app-server    │  (4GB VRAM)
│  Port: 8080     │
└────────┬────────┘
         │
         ├──────────┐
         │          │
    ┌────▼────┐  ┌──▼──────┐
    │ model-1 │  │ model-2 │
    │ 8GB VRAM│  │ 8GB VRAM│
    │Port:5000│  │Port:5001│
    └─────────┘  └─────────┘
```

**Total Resources**: 20GB VRAM across 3 GPUs

## Components

### 1. PodGroup
- **minMember**: 3 (all pods must be scheduled together)
- **priorityClassName**: Inference (appropriate for serving workloads)
- **queue**: default

### 2. Application Server Pod
- **Name**: `app-server`
- **Role**: Main application handling user requests
- **GPU Memory**: 4GB
- **Behavior**: Waits for model pods to be ready before serving

### 3. Model Serving Pods
- **model-1**: First ML model (8GB VRAM)
- **model-2**: Second ML model (8GB VRAM)
- **Purpose**: Provide inference capabilities to the application

## How to Use

### 1. Apply the Manifest

```bash
kubectl apply -f app-with-models.yaml
```

### 2. Check PodGroup Status

```bash
# View PodGroup
kubectl get podgroup app-with-models

# Detailed information
kubectl describe podgroup app-with-models
```

**Expected Output** (when scheduled):
```
Name:         app-with-models
Namespace:    default
Spec:
  Min Member:        3
  Priority Class Name: Inference
  Queue:             default
Status:
  Phase:             Running
  Scheduled:         3
```

### 3. Monitor Pod Status

```bash
# Check all pods in the gang
kubectl get pods -l kai.scheduler/podgroup=app-with-models

# Watch pods come up together
kubectl get pods -l kai.scheduler/podgroup=app-with-models -w
```

**Expected Output** (when scheduled):
```
NAME         READY   STATUS    RESTARTS   AGE
app-server   1/1     Running   0          10s
model-1      1/1     Running   0          10s
model-2      1/1     Running   0          10s
```

**Note**: All pods will have the same AGE, confirming they started simultaneously!

### 4. Check Pod Logs

```bash
# Application server logs
kubectl logs app-server

# Model logs
kubectl logs model-1
kubectl logs model-2
```

### 5. Clean Up

```bash
kubectl delete -f app-with-models.yaml
```

## Gang Scheduling Behavior

### Scenario 1: Sufficient Resources Available
**Available**: 32GB GPU with time-slicing

✅ **Result**: All 3 pods are scheduled simultaneously
- app-server: 4GB allocated
- model-1: 8GB allocated
- model-2: 8GB allocated
- Total: 20GB used

### Scenario 2: Insufficient Resources
**Available**: 32GB GPU, but 25GB already in use

❌ **Result**: NO pods are scheduled
- All pods remain in `Pending` state
- PodGroup shows "Unschedulable" with explanation
- Pods wait until 20GB becomes available
- Once available, all 3 start together

### Scenario 3: Partial Resources Available
**Available**: 32GB GPU, only 15GB free (enough for app + 1 model)

❌ **Result**: NO pods are scheduled (gang scheduling prevents partial deployment)
- Without gang scheduling: app-server and model-1 would start, model-2 would wait
- With gang scheduling: All wait together for full resources
- Ensures consistent deployment state

## Key Benefits

1. **Atomic Deployment**: All components start together or not at all
2. **Consistency**: No partial deployments with missing dependencies
3. **Resource Efficiency**: Prevents wasted resources on incomplete deployments
4. **Predictable Behavior**: Clear understanding of deployment state

## Testing Gang Scheduling

To verify gang scheduling is working:

```bash
# 1. Fill up GPU memory with another workload
kubectl apply -f ../gang-example1/large-gang-job.yaml

# 2. Try to deploy this app (should stay Pending)
kubectl apply -f app-with-models.yaml

# 3. Check that NO pods are scheduled
kubectl get pods -l kai.scheduler/podgroup=app-with-models
# All should be Pending

# 4. Delete the large job to free resources
kubectl delete -f ../gang-example1/large-gang-job.yaml

# 5. Watch all 3 pods start simultaneously
kubectl get pods -l kai.scheduler/podgroup=app-with-models -w
```

## Real-World Customization

Replace the example images with your actual services:

```yaml
# Application server
containers:
- name: app
  image: your-registry/app:v1.0
  command: ["./app-server"]
  env:
  - name: MODEL_1_URL
    value: "http://model-1:5000"
  - name: MODEL_2_URL
    value: "http://model-2:5001"

# Model pods
containers:
- name: model
  image: your-registry/model-server:v1.0
  command: ["python", "serve_model.py"]
  env:
  - name: MODEL_PATH
    value: "/models/model.pt"
```

## Using Deployments Instead of Pods

If you need independent scaling or lifecycle management for each component, you can use separate Deployments that share a single PodGroup. See `deployments-shared-podgroup.yaml` for a complete example.

### How It Works

```yaml
# 1. Create a shared PodGroup
apiVersion: scheduling.kai.run/v1alpha2
kind: PodGroup
metadata:
  name: shared-app-podgroup
spec:
  minMember: 3  # Total pods across all Deployments

# 2. Each Deployment references the same PodGroup
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-server
spec:
  replicas: 1
  template:
    metadata:
      labels:
        kai.scheduler/podgroup: shared-app-podgroup  # Link to shared PodGroup
```

### Setup Steps

1. **Manually create a shared PodGroup** with `minMember` set to total initial pod count
2. **Add the `kai.scheduler/podgroup: <name>` label** to each Deployment's pod template
3. **Deploy all Deployments** - their pods will be gang-scheduled together

### Example Deployment

```bash
# Apply the Deployment-based example
kubectl apply -f deployments-shared-podgroup.yaml

# Verify all pods start together
kubectl get pods -l kai.scheduler/podgroup=shared-app-podgroup -w
```

### Important Limitations

⚠️ **Gang scheduling works for INITIAL deployment only**

**What happens with scaling:**

```bash
# Initial state: 3 Deployments, 1 replica each = 3 pods (gang-scheduled ✓)
kubectl get deployment
# app-server   1/1     1            1
# model-1      1/1     1            1
# model-2      1/1     1            1

# Scale up model-1
kubectl scale deployment model-1 --replicas=2

# Result: 4th pod schedules INDEPENDENTLY (NOT gang-scheduled)
# - PodGroup still says minMember: 3
# - New model-1 pod doesn't wait for others
# - Gang scheduling contract is broken
```

**Why this happens:**
- PodGroup `minMember` is static (set at creation time)
- Kubernetes doesn't automatically update PodGroup when Deployments scale
- The pod-grouper doesn't reconcile PodGroup changes for manual PodGroups

### When to Use This Pattern

✅ **Good Use Cases:**
- **Initial atomic deployment** of microservices that must start together
- **Static configurations** where replica counts don't change after deployment
- **Independent lifecycle management** (restart one component without affecting others)
- **Separate update strategies** for different components

❌ **Not Recommended For:**
- **Dynamic scaling scenarios** where replicas change frequently
- **Auto-scaling** with HPA (Horizontal Pod Autoscaler)
- **Rolling updates** that change total pod count
- **Development environments** with frequent scale up/down

### Alternatives for Dynamic Scaling

If you need gang scheduling AND dynamic scaling, consider:

**Option 1: Single Deployment with Multiple Containers**
```yaml
apiVersion: apps/v1
kind: Deployment
spec:
  replicas: 1  # Scale the entire pod group together
  template:
    spec:
      containers:
      - name: app
      - name: model-1
      - name: model-2
```
**Pros**: Scales atomically, gang scheduling always respected
**Cons**: All containers restart together, shared resource limits

**Option 2: StatefulSet with Predictable Scaling**
```yaml
apiVersion: apps/v1
kind: StatefulSet
spec:
  replicas: 3
  # Pod 0: app-server
  # Pod 1: model-1
  # Pod 2: model-2
```
**Pros**: Predictable pod names, ordered scaling
**Cons**: Complex configuration, may need custom logic

**Option 3: External Controller**
- Build a custom controller that watches Deployments
- Automatically updates PodGroup `minMember` when replicas change
- Most flexible but requires development effort

### Best Practice Recommendation

For production deployments with gang scheduling requirements:

1. **If scaling is needed**: Use a single Deployment with multiple containers
2. **If independent lifecycle is crucial**: Accept the initial-deployment-only limitation
3. **If both are required**: Implement an external controller or use a service mesh with startup dependencies

## Troubleshooting

### Pods Stuck in Pending

```bash
# Check PodGroup for scheduling explanation
kubectl describe podgroup app-with-models

# Look for unschedulableExplanation field
```

### One Pod Fails to Start

If one pod fails after scheduling:
- Other pods will continue running (gang scheduling ensures scheduling, not runtime)
- Consider adding readiness probes for runtime dependencies
- Use init containers for startup dependencies

### Wrong GPU Memory Allocation

```bash
# Check actual GPU memory usage
kubectl exec -it model-1 -- nvidia-smi

# Verify annotation matches actual usage
kubectl get pod model-1 -o yaml | grep gpu-memory
```
