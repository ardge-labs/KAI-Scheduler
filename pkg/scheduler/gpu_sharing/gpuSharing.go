// Copyright 2025 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package gpu_sharing

import (
	"k8s.io/apimachinery/pkg/util/uuid"

	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/node_info"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/api/pod_info"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/framework"
	"github.com/NVIDIA/KAI-scheduler/pkg/scheduler/log"
)

type nodeGpuForSharing struct {
	Groups      []string
	IsReleasing bool
}

func AllocateFractionalGPUTaskToNode(ssn *framework.Session, stmt *framework.Statement, pod *pod_info.PodInfo,
	node *node_info.NodeInfo, isPipelineOnly bool) bool {
	log.InfraLogger.V(4).Infof("[GPU_ALLOCATE] Pod <%s/%s> on Node <%s>: Starting allocation, requested gpu-memory=<%d MB>, isPipelineOnly=<%v>",
		pod.Namespace, pod.Name, node.Name, pod.ResReq.GpuMemory(), isPipelineOnly)

	fittingGPUs := ssn.FittingGPUs(node, pod)
	log.InfraLogger.V(4).Infof("[GPU_ALLOCATE] Pod <%s/%s> on Node <%s>: FittingGPUs=<%v>",
		pod.Namespace, pod.Name, node.Name, fittingGPUs)

	gpuForSharing := getNodePreferableGpuForSharing(fittingGPUs, node, pod, isPipelineOnly)
	if gpuForSharing == nil {
		log.InfraLogger.V(4).Infof("[GPU_ALLOCATE] Pod <%s/%s> on Node <%s>: No preferable GPU found for sharing",
			pod.Namespace, pod.Name, node.Name)
		return false
	}

	log.InfraLogger.V(4).Infof("[GPU_ALLOCATE] Pod <%s/%s> on Node <%s>: Selected GPU groups=<%v>, IsReleasing=<%v>",
		pod.Namespace, pod.Name, node.Name, gpuForSharing.Groups, gpuForSharing.IsReleasing)

	pod.GPUGroups = gpuForSharing.Groups

	isPipelineOnly = isPipelineOnly || gpuForSharing.IsReleasing
	log.InfraLogger.V(4).Infof("[GPU_ALLOCATE] Pod <%s/%s> on Node <%s>: Final isPipelineOnly=<%v> (original=<%v>, gpuIsReleasing=<%v>)",
		pod.Namespace, pod.Name, node.Name, isPipelineOnly, isPipelineOnly && !gpuForSharing.IsReleasing, gpuForSharing.IsReleasing)

	success := allocateSharedGPUTask(ssn, stmt, node, pod, isPipelineOnly)
	if !success {
		log.InfraLogger.V(4).Infof("[GPU_ALLOCATE] Pod <%s/%s> on Node <%s>: Allocation failed, clearing GPU groups",
			pod.Namespace, pod.Name, node.Name)
		pod.GPUGroups = nil
	} else {
		log.InfraLogger.V(4).Infof("[GPU_ALLOCATE] Pod <%s/%s> on Node <%s>: Allocation successful",
			pod.Namespace, pod.Name, node.Name)
	}
	return success
}

func getNodePreferableGpuForSharing(fittingGPUsOnNode []string, node *node_info.NodeInfo, pod *pod_info.PodInfo,
	isPipelineOnly bool) *nodeGpuForSharing {
	log.InfraLogger.V(4).Infof("[GPU_SELECT] Pod <%s/%s>: Selecting from fitting GPUs=<%v>, required devices=<%d>",
		pod.Namespace, pod.Name, fittingGPUsOnNode, pod.ResReq.GetNumOfGpuDevices())

	nodeGpusSharing := &nodeGpuForSharing{
		Groups:      []string{},
		IsReleasing: false,
	}

	deviceCounts := pod.ResReq.GetNumOfGpuDevices()
	for _, gpuIdx := range fittingGPUsOnNode {
		if gpuIdx == pod_info.WholeGpuIndicator {
			log.InfraLogger.V(4).Infof("[GPU_SELECT] Pod <%s/%s>: Processing whole GPU indicator",
				pod.Namespace, pod.Name)
			if wholeGpuForSharing := findGpuForSharingOnNode(pod, node, isPipelineOnly); wholeGpuForSharing != nil {
				log.InfraLogger.V(4).Infof("[GPU_SELECT] Pod <%s/%s>: Whole GPU found, groups=<%v>, isReleasing=<%v>",
					pod.Namespace, pod.Name, wholeGpuForSharing.Groups, wholeGpuForSharing.IsReleasing)
				nodeGpusSharing.IsReleasing =
					nodeGpusSharing.IsReleasing || wholeGpuForSharing.IsReleasing
				nodeGpusSharing.Groups = append(nodeGpusSharing.Groups, wholeGpuForSharing.Groups...)
			}
		} else {
			hasEnoughIdle := node.EnoughIdleResourcesOnGpu(pod.ResReq, gpuIdx)
			isTaskAllocatable := node.IsTaskAllocatable(pod)
			gpuIsReleasing := !hasEnoughIdle || !isTaskAllocatable

			log.InfraLogger.V(4).Infof("[GPU_SELECT] Pod <%s/%s>: Processing shared GPU <%s>, EnoughIdle=<%v>, TaskAllocatable=<%v>, WillBeReleasing=<%v>",
				pod.Namespace, pod.Name, gpuIdx, hasEnoughIdle, isTaskAllocatable, gpuIsReleasing)

			nodeGpusSharing.IsReleasing = nodeGpusSharing.IsReleasing || gpuIsReleasing
			nodeGpusSharing.Groups = append(nodeGpusSharing.Groups, gpuIdx)
		}

		if len(nodeGpusSharing.Groups) == int(deviceCounts) {
			log.InfraLogger.V(4).Infof("[GPU_SELECT] Pod <%s/%s>: Required device count reached, selected groups=<%v>, isReleasing=<%v>",
				pod.Namespace, pod.Name, nodeGpusSharing.Groups, nodeGpusSharing.IsReleasing)
			return nodeGpusSharing
		}
	}

	log.InfraLogger.V(4).Infof("[GPU_SELECT] Pod <%s/%s>: Could not satisfy device requirements, collected groups=<%v> (needed <%d>)",
		pod.Namespace, pod.Name, nodeGpusSharing.Groups, deviceCounts)
	return nil
}

func findGpuForSharingOnNode(task *pod_info.PodInfo, node *node_info.NodeInfo, isPipelineOnly bool) *nodeGpuForSharing {
	isReleasing := true
	if !isPipelineOnly {
		if taskAllocatable := node.IsTaskAllocatable(task); taskAllocatable {
			isReleasing = false
		}
	}
	return &nodeGpuForSharing{Groups: []string{string(uuid.NewUUID())}, IsReleasing: isReleasing}
}

func allocateSharedGPUTask(ssn *framework.Session, stmt *framework.Statement, node *node_info.NodeInfo,
	task *pod_info.PodInfo, isPipelineOnly bool) bool {
	if isPipelineOnly {
		log.InfraLogger.V(6).Infof(
			"Pipelining Task <%v/%v> to node <%v> gpuGroup: <%v>, requires: <%v, %v mb> GPUs",
			task.Namespace, task.Name, node.Name,
			task.GPUGroups, task.ResReq.GPUs(), task.ResReq.GpuMemory())
		if err := stmt.Pipeline(task, node.Name, !isPipelineOnly); err != nil {
			log.InfraLogger.V(6).Infof("Failed to pipeline Task: <%s/%s> on Node: <%s>, due to an error: %v",
				task.Namespace, task.Name, node.Name, err)
			return false
		}

		return true
	}

	if err := stmt.Allocate(task, node.Name); err != nil {
		log.InfraLogger.Errorf("Failed to bind Task <%v> on <%v> in Session <%v>, err: <%v>",
			task.UID, node.Name, ssn.UID, err)
		return false
	}

	return true
}
