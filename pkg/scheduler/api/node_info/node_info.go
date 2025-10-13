/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Copyright 2025 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package node_info

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.uber.org/multierr"
	"golang.org/x/exp/maps"
	v1 "k8s.io/api/core/v1"

	commonconstants "github.com/NVIDIA/KAI-scheduler/pkg/common/constants"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/common_info"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/common_info/resources"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/pod_affinity"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/pod_status"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/podgroup_info"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/resource_info"
	sc_info "github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/storagecapacity_info"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/storageclaim_info"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/conf"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/k8s_utils"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/log"
)

const (
	DefaultGpuMemory = 100 // The default value is 100 because it allows all the calculation of (memory = fractional * GpuMemory) to work, if it was 0 the result will always be zero too
	GpuMemoryLabel   = "nvidia.com/gpu.memory"
	GpuCountLabel    = "nvidia.com/gpu.count"
	MbToBRatio       = 1000000
	BitToMib         = 1024 * 1024
	TibInMib         = 1024 * 1024
)

type MigStrategy string

const (
	MigStrategyNone   MigStrategy = "none"
	MigStrategySingle MigStrategy = "single"
	MigStrategyMixed  MigStrategy = "mixed"
	migResourcePrefix             = "nvidia.com/mig-"
)

// NodeInfo is node level aggregated information.
type NodeInfo struct {
	Name string
	Node *v1.Node

	// The releasing resource on that node (excluding shared GPUs)
	Releasing *resource_info.Resource
	// The idle resource on that node (excluding shared GPUs)
	Idle *resource_info.Resource
	// The used resource on that node, including running and terminating
	// pods (excluding shared GPUs)
	Used *resource_info.Resource

	Allocatable *resource_info.Resource

	AccessibleStorageCapacities map[common_info.StorageClassID][]*sc_info.StorageCapacityInfo

	PodInfos               map[common_info.PodID]*pod_info.PodInfo
	MaxTaskNum             int
	MemoryOfEveryGpuOnNode int64
	GpuMemorySynced        bool
	LegacyMIGTasks         map[common_info.PodID]string

	PodAffinityInfo pod_affinity.NodePodAffinityInfo

	GpuSharingNodeInfo
}

func NewNodeInfo(node *v1.Node, podAffinityInfo pod_affinity.NodePodAffinityInfo) *NodeInfo {
	gpuMemory, exists := getNodeGpuMemory(node)

	nodeInfo := &NodeInfo{
		Name: node.Name,
		Node: node,

		Releasing: resource_info.EmptyResource(),
		Idle:      resource_info.ResourceFromResourceList(node.Status.Allocatable),
		Used:      resource_info.EmptyResource(),

		Allocatable: resource_info.ResourceFromResourceList(node.Status.Allocatable),

		AccessibleStorageCapacities: map[common_info.StorageClassID][]*sc_info.StorageCapacityInfo{},

		PodInfos:               make(map[common_info.PodID]*pod_info.PodInfo),
		MemoryOfEveryGpuOnNode: gpuMemory,
		GpuMemorySynced:        exists,
		LegacyMIGTasks:         map[common_info.PodID]string{},

		GpuSharingNodeInfo: *newGpuSharingNodeInfo(),

		PodAffinityInfo: podAffinityInfo,
	}
	numTasks := node.Status.Allocatable[v1.ResourcePods]
	nodeInfo.MaxTaskNum = int(numTasks.Value())

	capacity := resource_info.ResourceFromResourceList(node.Status.Capacity)
	if capacity.GPUs() != nodeInfo.Allocatable.GPUs() {
		log.InfraLogger.V(2).Warnf(
			"For node %s, the capacity and allocatable are different. Capacity %v, Allocatable %v",
			node.Name, capacity.DetailedString(), nodeInfo.Allocatable.DetailedString())
	}

	// FIX: Detect GPU time-slicing and adjust idle GPU count to physical GPU count
	// With time-slicing, node.Allocatable.GPUs() reports the time-sliced device count (e.g., 100),
	// but all devices share the same physical GPU(s) VRAM. We must use the physical GPU count
	// for proper VRAM accounting.
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

	return nodeInfo
}

func (ni *NodeInfo) NonAllocatedResources() *resource_info.Resource {
	nonAllocatedResource := resource_info.EmptyResource()
	nonAllocatedResource.Add(ni.Idle)
	nonAllocatedResource.Add(ni.Releasing)
	return nonAllocatedResource
}

func (ni *NodeInfo) NonAllocatedResource(resourceType v1.ResourceName) float64 {
	return ni.Idle.Get(resourceType) + ni.Releasing.Get(resourceType)
}

func (ni *NodeInfo) IsTaskAllocatable(task *pod_info.PodInfo) bool {
	if isBestEffortJob := task.ResReq.IsEmpty() &&
		(len(task.GetAllStorageClaims()) == 0) && !task.IsMemoryRequest(); isBestEffortJob {
		return true
	}

	if allocatable := ni.isTaskAllocatableOnNonAllocatedResources(task, ni.Idle); !allocatable {
		log.InfraLogger.V(7).Infof("Task GPU %s/%s is not allocatable on node %s",
			task.Namespace, task.Name, ni.Name)
		return false
	}

	storageAllocatable, err := ni.isTaskStorageAllocatable(task)
	if !storageAllocatable {
		log.InfraLogger.V(7).Infof("Task storage %s/%s is not allocatable on node %s, error: %v",
			task.Namespace, task.Name, ni.Name, err)
		return false
	}

	return true
}

func (ni *NodeInfo) IsTaskAllocatableOnReleasingOrIdle(task *pod_info.PodInfo) bool {
	nodeNonAllocatedResources := ni.NonAllocatedResources()

	if allocatable := ni.isTaskAllocatableOnNonAllocatedResources(task, nodeNonAllocatedResources); !allocatable {
		log.InfraLogger.V(7).Infof("Task GPU %s/%s is not allocatable on node %s",
			task.Namespace, task.Name, ni.Name)
		return false
	}

	if allocatable, err := ni.isTaskStorageAllocatableOnReleasingOrIdle(task); !allocatable {
		log.InfraLogger.V(7).Infof("Task storage %s/%s is not allocatable on node %s, error: %v",
			task.Namespace, task.Name, ni.Name, err)
		return false
	}

	return true
}

// isTaskStorageAllocatable iterates over a pod's volumes. For all unbound PVCs, which use a CSI storage, we check the
// node's ability to access this StorageCapacity, and calls ArePVCsAllocatable. For all other types of storage, we rely
// on the predicates.
func (ni *NodeInfo) isTaskStorageAllocatable(task *pod_info.PodInfo) (bool, error) {
	if deletedStorageClaims := task.GetDeletedStorageClaimsNames(); len(deletedStorageClaims) > 0 {
		return false, fmt.Errorf("task has deleted storage claims: %s", deletedStorageClaims)
	}

	pendingPVCsByStorageClass := task.GetUnboundOrReleasingStorageClaimsByStorageClass()
	var err error

	for storageClass, pvcs := range pendingPVCsByStorageClass {
		capacities, found := ni.AccessibleStorageCapacities[storageClass]
		if !found {
			err = multierr.Append(err, fmt.Errorf("no allocatable storage from storageclass %s found",
				storageClass))
			continue
		}

		if !isTaskStorageAllocatableOnCapacities(pvcs, capacities) {
			err = multierr.Append(err, fmt.Errorf("node(s) did not have enough free storage "+
				"for storageclass %s", storageClass))
		}
	}

	allocatable := err == nil
	return allocatable, err
}

func isTaskStorageAllocatableOnCapacities(pvcs []*storageclaim_info.StorageClaimInfo,
	capacities []*sc_info.StorageCapacityInfo) bool {
	for _, capacity := range capacities {
		// We don't know on each capacity the pvcs will allocate, so we check that it's possible on any of the capacities
		err := capacity.ArePVCsAllocatable(pvcs)
		if err == nil {
			return true
		}

		log.InfraLogger.V(7).Debugf("PVCs not allocatable on capacity %s due to errors: %v\n",
			capacity.Name, err)
	}

	return false
}

func (ni *NodeInfo) isTaskStorageAllocatableOnReleasingOrIdle(task *pod_info.PodInfo) (bool, error) {
	pendingPVCsByStorageClass := task.GetUnboundOrReleasingStorageClaimsByStorageClass()

	for storageClass, pvcs := range pendingPVCsByStorageClass {
		capacities, found := ni.AccessibleStorageCapacities[storageClass]
		if !found {
			return false, sc_info.NewMissingStorageClassError(storageClass)
		}

		for _, capacity := range capacities {
			// We don't know on each capacity the pvcs will allocate, so we check that it's possible on any of the capacities
			err := capacity.ArePVCsAllocatableOnReleasingOrIdle(pvcs, ni.PodInfos)
			if err != nil {
				return false, err
			}
		}
	}

	return true, nil
}

func (ni *NodeInfo) FittingError(task *pod_info.PodInfo, isGangTask bool) *common_info.FitError {
	enoughResources := ni.lessEqualTaskToNodeResources(task.ResReq, ni.Idle)
	if !enoughResources {
		totalUsed := ni.Used.Clone()
		totalUsed.AddGPUs(float64(ni.getNumberOfUsedSharedGPUs()))
		totalCapability := ni.Allocatable.Clone()

		requestedResources := task.ResReq.Clone()
		if requestedResources.GpuMemory() > 0 {
			// This helps to add an appropriate fit error message in case of a gp memory request
			requestedResources.GpuResourceRequirement = *resource_info.NewGpuResourceRequirementWithMultiFraction(
				task.ResReq.GetNumOfGpuDevices(), ni.getResourceGpuPortion(task.ResReq), requestedResources.GpuMemory())
		}

		messageSuffix := ""
		if len(task.Pod.Spec.Overhead) > 0 {
			// Adding to node idle instead of subtracting from pod requested resources
			idleResourcesWithOverhead := ni.Idle.Clone()
			idleResourcesWithOverhead.Add(resource_info.ResourceFromResourceList(task.Pod.Spec.Overhead))
			enoughResourcesWithoutOverhead := ni.lessEqualTaskToNodeResources(task.ResReq, idleResourcesWithOverhead)
			if enoughResourcesWithoutOverhead {
				messageSuffix = fmt.Sprintf("%s. The overhead resources are %v", common_info.OverheadMessage,
					k8s_utils.StringResourceList(task.Pod.Spec.Overhead))
			}
		}

		fitError := common_info.NewFitErrorInsufficientResource(
			task.Name, task.Namespace, ni.Name, task.ResReq, totalUsed, totalCapability, ni.MemoryOfEveryGpuOnNode,
			isGangTask, messageSuffix)

		return fitError
	}

	allocatable, err := ni.isTaskStorageAllocatable(task)
	if !allocatable {
		return common_info.NewFitErrorByReasons(task.Name, task.Namespace, ni.Name, err, err.Error())
	}

	return nil
}

func (ni *NodeInfo) PredicateByNodeResourcesType(task *pod_info.PodInfo) error {
	// Prevents legacy MIG jobs from being scheduled
	if task.IsLegacyMIGtask {
		return common_info.NewFitError(task.Name, task.Namespace, ni.Name,
			fmt.Sprintf("Legacy MIG jobs cannot be scheduled on node %s", ni.Name))
	}

	if task.IsCPUOnlyRequest() {
		return nil
	}

	migNode := ni.IsMIGEnabled()
	if !migNode && task.IsMigCandidate() {
		return common_info.NewFitError(task.Name, task.Namespace, ni.Name,
			"MIG job cannot be scheduled on non-MIG node")
	}
	if migNode {
		// Prevents new MIG jobs from being scheduled on a node with legacy MIG jobs
		if task.IsMigCandidate() && len(ni.LegacyMIGTasks) > 0 {
			return common_info.NewFitError(task.Name, task.Namespace, ni.Name,
				fmt.Sprintf("MIG job cannot be scheduled on node with legacy MIG jobs: <%v>",
					maps.Values(ni.LegacyMIGTasks)))
		}

		strategy := ni.GetMigStrategy()
		if strategy == MigStrategySingle && !task.IsRegularGPURequest() {
			return common_info.NewFitError(task.Name, task.Namespace, ni.Name,
				"only whole gpu jobs can run on a node with MigStrategy='single'")
		}
		if strategy == MigStrategyMixed && !task.IsMigCandidate() {
			return common_info.NewFitError(task.Name, task.Namespace, ni.Name,
				"non MIG jobs cant run on a node with MigStrategy='mixed'")
		}
	}
	return nil
}

func (ni *NodeInfo) isTaskAllocatableOnNonAllocatedResources(
	task *pod_info.PodInfo, nodeNonAllocatedResources *resource_info.Resource,
) bool {
	if task.IsRegularGPURequest() || task.IsMigProfileRequest() {
		return ni.lessEqualTaskToNodeResources(task.ResReq, nodeNonAllocatedResources)
	}

	if !task.ResReq.BaseResource.LessEqual(&nodeNonAllocatedResources.BaseResource) {
		return false
	}

	if !ni.isValidGpuPortion(task.ResReq) {
		return false
	}
	nodeIdleOrReleasingWholeGpus := int64(math.Floor(nodeNonAllocatedResources.GPUs()))
	nodeNonAllocatedResourcesMatchingSharedGpus := ni.fractionTaskGpusAllocatableDeviceCount(task)
	if nodeIdleOrReleasingWholeGpus+nodeNonAllocatedResourcesMatchingSharedGpus >= task.ResReq.GetNumOfGpuDevices() {
		return true
	}

	return false
}

func (ni *NodeInfo) shouldAddTaskResources(task *pod_info.PodInfo) bool {
	return !pod_info.IsResourceReservationTask(task.Pod)
}

func (ni *NodeInfo) AddTask(task *pod_info.PodInfo) error {
	return ni.addTask(task, false)
}

func (ni *NodeInfo) addTask(task *pod_info.PodInfo, allowTaskToExistOnDifferentGPU bool) error {
	ni.setAcceptedResources(task)

	key := pod_info.PodKey(task.Pod)
	if task.IsSharedGPUAllocation() && allowTaskToExistOnDifferentGPU {
		delete(ni.PodInfos, key)
	} else if _, found := ni.PodInfos[key]; found {
		return fmt.Errorf("task <%v/%v> already on node <%v>",
			task.Namespace, task.Name, ni.Name)
	}

	// Node will hold a copy of task to make sure the status
	// change will not impact resource in node.
	ti := task.Clone()
	ni.PodInfos[key] = ti
	if ni.Node == nil {
		return fmt.Errorf("node is nil during add task, node name: <%v>", ni.Name)
	}

	if task.IsLegacyMIGtask {
		ni.LegacyMIGTasks[task.UID] = string(pod_info.PodKey(task.Pod))
	}

	ni.addTaskResources(task)
	ni.addTaskStorage(task)
	ni.PodAffinityInfo.AddPod(task.Pod)
	return nil
}

func (ni *NodeInfo) AddTasksToNode(podInfos []*pod_info.PodInfo,
	existingPodsMap map[common_info.PodID]*pod_info.PodInfo) (resultPods []*v1.Pod) {
	resultPods = []*v1.Pod{}

	for _, podInfo := range podInfos {
		if pod_status.IsActiveUsedStatus(podInfo.Status) {
			_ = ni.AddTask(podInfo)
		} else {
			log.InfraLogger.V(6).Infof(
				"Pod %s/%s in status %s, not adding to node %s",
				podInfo.Namespace, podInfo.Name, podInfo.Status, ni.Name)
		}
		log.InfraLogger.V(6).Infof("Adding pod %s/%s/%s to existingpods", podInfo.Namespace, podInfo.Name,
			podInfo.UID)
		existingPodsMap[podInfo.UID] = podInfo
		resultPods = append(resultPods, podInfo.Pod)
	}

	return resultPods
}

func (ni *NodeInfo) addTaskStorage(task *pod_info.PodInfo) {
	claims := task.GetUnboundOrReleasingStorageClaimsByStorageClass()
	for storageClass, storageClassClaims := range claims {
		for _, claim := range storageClassClaims {
			capacities, found := ni.AccessibleStorageCapacities[storageClass]
			if !found {
				log.InfraLogger.V(7).Infof(
					"Could not find accessible storage capacities for storage class %s on "+
						"node %s for advanced csi scheduling", storageClass, ni.Name)
				continue
			}

			for _, capacity := range capacities {
				capacity.ProvisionedPVCs[claim.Key] = claim
			}
		}
	}
}

func (ni *NodeInfo) addTaskResources(task *pod_info.PodInfo) {
	if !ni.shouldAddTaskResources(task) {
		return
	}

	log.InfraLogger.V(7).Infof("About to add podsInfo: <%v/%v>, status: <%v>, node: <%s>",
		task.Namespace, task.Name, task.Status, ni.Name)
	log.InfraLogger.V(7).Infof("Node info: %+v", ni)

	requestedResourceWithoutSharedGPU := getAcceptedTaskResourceWithoutSharedGPU(task)

	// the added task will be the only one allocated on the GPU
	ni.Used.Add(requestedResourceWithoutSharedGPU)

	switch task.Status {
	case pod_status.Releasing:
		ni.Releasing.Add(requestedResourceWithoutSharedGPU)
		ni.Idle.Sub(requestedResourceWithoutSharedGPU)
	case pod_status.Pipelined:
		ni.Releasing.Sub(requestedResourceWithoutSharedGPU)

	default:
		ni.Idle.Sub(requestedResourceWithoutSharedGPU)
	}

	ni.addSharedTaskResources(task)

	log.InfraLogger.V(8).Infof("Added podsInfo: <%v/%v>, status: <%v>, node: <%+v>",
		task.Namespace, task.Name, task.Status, ni)
}

func (ni *NodeInfo) RemoveTask(ti *pod_info.PodInfo) error {
	key := pod_info.PodKey(ti.Pod)

	task, found := ni.PodInfos[key]
	if !found {
		return fmt.Errorf("failed to find task <%v/%v> on host <%v>",
			ti.Namespace, ti.Name, ni.Name)
	}
	delete(ni.PodInfos, key)
	if ni.Node == nil {
		return fmt.Errorf("node is nil during remove task, node name: <%v>", ni.Name)
	}

	ni.removeTaskStorage(task)
	ni.removeTaskResources(task)
	err := ni.PodAffinityInfo.RemovePod(task.Pod)

	return err
}

func (ni *NodeInfo) removeTaskResources(task *pod_info.PodInfo) {
	if !ni.shouldAddTaskResources(task) {
		return
	}

	log.InfraLogger.V(7).Infof("About to remove podsInfo: <%v/%v>, status: <%v>, node: <%s>",
		task.Namespace, task.Name, task.Status, ni.Name)
	log.InfraLogger.V(7).Infof("NodeInfo: %+v", ni)

	requestedResourceWithoutSharedGPU := getAcceptedTaskResourceWithoutSharedGPU(task)

	// the removed task in the only one currently allocated on the GPU
	ni.Used.Sub(requestedResourceWithoutSharedGPU)

	switch task.Status {
	case pod_status.Releasing:
		ni.Releasing.Sub(requestedResourceWithoutSharedGPU)
		ni.Idle.Add(requestedResourceWithoutSharedGPU)
	case pod_status.Pipelined:
		ni.Releasing.Add(requestedResourceWithoutSharedGPU)
	default:
		ni.Idle.Add(requestedResourceWithoutSharedGPU)
	}

	ni.removeSharedTaskResources(task)

	log.InfraLogger.V(8).Infof("Removed podsInfo: <%v/%v>, status: <%v>, node: <%+v>",
		task.Namespace, task.Name, task.Status, ni)
}

func (ni *NodeInfo) removeTaskStorage(task *pod_info.PodInfo) {
	claims := task.GetUnboundOrReleasingStorageClaimsByStorageClass()
	for storageClass, storageClassClaims := range claims {
		for _, claim := range storageClassClaims {
			capacities, found := ni.AccessibleStorageCapacities[storageClass]
			if !found {
				log.InfraLogger.V(7).Infof("Could not find accessible storage capacities for storage "+
					"class %s on node %s for advanced csi scheduling", storageClass, ni.Name)
				continue
			}
			for _, capacity := range capacities {
				delete(capacity.ProvisionedPVCs, claim.Key)
			}
		}
	}
}

func (ni *NodeInfo) UpdateTask(ti *pod_info.PodInfo) error {
	if err := ni.RemoveTask(ti); err != nil {
		return err
	}

	return ni.addTask(ti, false)
}

func (ni *NodeInfo) String() string {
	res := ""

	i := 0
	for _, task := range ni.PodInfos {
		res = res + fmt.Sprintf("\n\t %d: %v", i, task)
		i++
	}

	return fmt.Sprintf("Node (%s): idle <%v>, used <%v>, releasing <%v>, taints <%v>%s",
		ni.Name, ni.Idle, ni.Used, ni.Releasing, ni.Node.Spec.Taints, res)

}

func (ni *NodeInfo) GetSumOfIdleGPUs() (float64, int64) {
	sumOfSharedGPUs, sumOfSharedGPUsMemory := ni.getSumOfAvailableSharedGPUs()
	idleGPUs := ni.Idle.GPUs()

	for resourceName, qty := range ni.Idle.ScalarResources() {
		if !isMigResource(resourceName.String()) {
			continue
		}
		gpuPortion, _, err := resources.ExtractGpuAndMemoryFromMigResourceName(resourceName.String())
		if err != nil {
			log.InfraLogger.Errorf("failed to evaluate device portion for resource %v: %v", resourceName, err)
			continue
		}
		idleGPUs += float64(int64(gpuPortion) * qty)
	}

	return sumOfSharedGPUs + idleGPUs, sumOfSharedGPUsMemory + (int64(idleGPUs) * ni.MemoryOfEveryGpuOnNode)
}

func (ni *NodeInfo) GetSumOfReleasingGPUs() (float64, int64) {
	sumOfSharedGPUs, sumOfSharedGPUsMemory := ni.getSumOfReleasingSharedGPUs()
	releasingGPUs := ni.Releasing.GPUs()

	for resourceName, qty := range ni.Releasing.ScalarResources() {
		if !isMigResource(resourceName.String()) {
			continue
		}
		gpuPortion, _, err := resources.ExtractGpuAndMemoryFromMigResourceName(resourceName.String())
		if err != nil {
			log.InfraLogger.Errorf("failed to evaluate device portion for resource %v: %v", resourceName, err)
			continue
		}
		releasingGPUs += float64(int64(gpuPortion) * qty)
	}

	return sumOfSharedGPUs + releasingGPUs, sumOfSharedGPUsMemory + (int64(releasingGPUs) * ni.MemoryOfEveryGpuOnNode)
}

func (ni *NodeInfo) getNodeGpuCountLabelValue() (int, error) {
	gpuCountLabelValue, found := ni.Node.Labels[GpuCountLabel]
	if !found {
		return -1, &common_info.NotFoundError{Name: fmt.Sprintf("node %s nvidia.com/gpu.count label", ni.Name)}
	}

	gpuCount, err := strconv.Atoi(gpuCountLabelValue)
	if err != nil {
		return -1, err
	}

	return gpuCount, nil
}

func (ni *NodeInfo) GetNumberOfGPUsInNode() int64 {
	numberOfGPUs, err := ni.getNodeGpuCountLabelValue()
	if err != nil {
		log.InfraLogger.V(6).Infof("Node: <%v> had no annotations of nvidia.com/gpu.count", ni.Name)
		return int64(ni.Allocatable.GPUs())
	}
	return int64(numberOfGPUs)
}

func (ni *NodeInfo) GetResourceGpuMemory(res *resource_info.ResourceRequirements) int64 {
	if res.GpuMemory() > 0 {
		return res.GpuMemory()
	} else {
		return int64(res.GpuFractionalPortion() * float64(ni.MemoryOfEveryGpuOnNode))
	}
}

func (ni *NodeInfo) getResourceGpuPortion(res *resource_info.ResourceRequirements) float64 {
	if res.GpuMemory() > 0 {
		return ni.getGpuMemoryFractionalOnNode(res.GpuMemory())
	}
	return res.GpuFractionalPortion()
}

func (ni *NodeInfo) isValidGpuPortion(res *resource_info.ResourceRequirements) bool {
	gpuPortion := ni.getResourceGpuPortion(res)
	return gpuPortion <= 1 || gpuPortion == float64(int(gpuPortion))
}

func getNodeGpuMemory(node *v1.Node) (int64, bool) {
	gpuMemoryLabelValue, err := strconv.ParseInt(node.Labels[GpuMemoryLabel], 10, 64)
	if err != nil {
		log.InfraLogger.V(6).Infof("Could not find gpu memory label %v on node %v", GpuMemoryLabel, node.Name)
		return DefaultGpuMemory, false
	}

	// This code is a fix to cover for the Gpu-feature-discovery bug
	// (issue https://github.com/NVIDIA/gpu-feature-discovery/issues/26)
	if !checkGpuMemoryIsInMib(gpuMemoryLabelValue) {
		gpuMemoryLabelValue = convertBytesToMib(gpuMemoryLabelValue)
	}

	return gpuMemoryLabelValue - (gpuMemoryLabelValue % 100), true // Floor the memory count to make sure its divided by 100 so there will not be 2 jobs that get same bytes
}

func checkGpuMemoryIsInMib(gpuMemoryValue int64) bool {
	return gpuMemoryValue < TibInMib
}

func convertBytesToMib(gpuMemoryValue int64) int64 {
	return gpuMemoryValue / BitToMib
}

func (ni *NodeInfo) IsCPUOnlyNode() bool {
	if ni.IsMIGEnabled() {
		return false
	}
	return ni.Allocatable.GPUs() == 0
}

func (ni *NodeInfo) IsMIGEnabled() bool {
	migWorkerLabelKey := conf.GetConfig().MIGWorkerNodeLabelKey
	enabled, found := ni.Node.Labels[migWorkerLabelKey]
	if found {
		isMig, err := strconv.ParseBool(enabled)
		return err == nil && isMig
	}
	for nodeResource := range ni.Allocatable.ScalarResources() {
		if isMigResource(nodeResource.String()) {
			return true
		}
	}
	return false
}

func (ni *NodeInfo) GetMigStrategy() MigStrategy {
	if !ni.IsMIGEnabled() {
		return MigStrategyNone
	}

	migStrategy, found := ni.Node.Labels[commonconstants.MigStrategyLabel]
	if !found || len(migStrategy) == 0 {
		return MigStrategyNone
	}
	if migStrategy != string(MigStrategyMixed) && migStrategy != string(MigStrategySingle) {
		return MigStrategyNone
	}
	return MigStrategy(migStrategy)
}

func (ni *NodeInfo) GetRequiredInitQuota(pi *pod_info.PodInfo) *podgroup_info.JobRequirement {
	quota := podgroup_info.JobRequirement{}
	if len(pi.ResReq.MigResources()) != 0 {
		quota.GPU = pi.ResReq.GetSumGPUs()
	} else {
		quota.GPU = ni.getGpuMemoryFractionalOnNode(ni.GetResourceGpuMemory(pi.ResReq))
	}
	quota.MilliCPU = pi.ResReq.Cpu()
	quota.Memory = pi.ResReq.Memory()
	return &quota
}

func (ni *NodeInfo) setAcceptedResources(pi *pod_info.PodInfo) {
	if !pod_status.IsActiveUsedStatus(pi.Status) {
		return
	}

	pi.AcceptedResource = pi.ResReq.Clone()
	if pi.IsMigCandidate() {
		pi.ResourceReceivedType = pod_info.ReceivedTypeMigInstance
		pi.AcceptedResource.GpuResourceRequirement =
			*resource_info.NewGpuResourceRequirementWithMig(pi.ResReq.MigResources())
	} else if pi.IsFractionCandidate() {
		pi.ResourceReceivedType = pod_info.ReceivedTypeFraction
		pi.AcceptedResource.GpuResourceRequirement = *resource_info.NewGpuResourceRequirementWithMultiFraction(
			pi.ResReq.GetNumOfGpuDevices(), ni.getResourceGpuPortion(pi.ResReq), ni.GetResourceGpuMemory(pi.ResReq))
	} else {
		pi.ResourceReceivedType = pod_info.ReceivedTypeRegular
		pi.AcceptedResource.GpuResourceRequirement = *resource_info.NewGpuResourceRequirementWithGpus(
			pi.ResReq.GPUs(), 0)
	}
}

func (ni *NodeInfo) lessEqualTaskToNodeResources(
	taskResources *resource_info.ResourceRequirements, nodeResources *resource_info.Resource,
) bool {
	if !ni.isValidGpuPortion(taskResources) {
		return false
	}
	return taskResources.LessEqualResource(nodeResources)
}

func isMigResource(rName string) bool {
	return strings.HasPrefix(rName, migResourcePrefix)
}
