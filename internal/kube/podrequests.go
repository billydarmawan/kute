package kube

import (
	corev1 "k8s.io/api/core/v1"
)

// isRestartableInit reports whether c is a native sidecar (KEP-753): an
// init container with restartPolicy: Always, which keeps running alongside
// the regular containers instead of finishing before them. Same predicate
// buildContainerInfos uses to decide a container belongs in the running
// grid.
func isRestartableInit(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// PodEffectiveRequests is what a pod actually reserves on a node, by the
// scheduler's own rule — the number `kubectl describe node` reports under
// "Allocated resources".
//
// It is deliberately not the same as Pod.CPURequestMilli/MEMRequestBytes,
// which are the literal sum over spec.containers and describe the pod
// itself. Three things make a node's reservation larger than that sum:
// native sidecars (initContainers with restartPolicy: Always) run for the
// pod's whole life and so add to it; a plain init container can demand more
// on its own than the regular containers ever do together, and the node
// must hold that peak; and spec.overhead is the RuntimeClass's own charge
// for the sandbox. Summing spec.containers alone understates every
// mesh-injected pod, which is what made 11b's panel read low against
// kubectl.
//
// Two newer fields follow upstream's resourcehelper.PodRequests too. An
// in-place resize (KEP-1287) leaves spec and status disagreeing until the
// kubelet catches up, and the node holds the larger of the two — the
// spec's request, what the kubelet allocated, and what the container is
// actually running with — unless the resize is Infeasible, when spec is a
// wish the node will never grant and only status counts. Pod-level
// resources (KEP-2837, spec.resources) replace the container aggregate
// outright for each of cpu/memory they set.
func PodEffectiveRequests(pod *corev1.Pod) (cpuMilli, memBytes int64) {
	infeasible := resizeInfeasible(pod)
	requests := func(c corev1.Container, statuses []corev1.ContainerStatus) (cpu, mem int64) {
		// A linear scan, not a map: this runs for every pod on every
		// node-list reload, and a pod has a handful of containers.
		for i := range statuses {
			if statuses[i].Name == c.Name {
				return containerRequests(c, &statuses[i], infeasible)
			}
		}
		return containerRequests(c, nil, infeasible)
	}

	// Sidecars accumulate as we walk the init list in order: a plain init
	// container at position i runs while every sidecar declared before it
	// is already up, so its peak is its own request plus that running
	// total — not plus every sidecar in the pod.
	var sidecarCPU, sidecarMem int64
	var peakCPU, peakMem int64
	for _, c := range pod.Spec.InitContainers {
		cpu := c.Resources.Requests.Cpu().MilliValue()
		mem := c.Resources.Requests.Memory().Value()
		if isRestartableInit(c) {
			// Only sidecars outlive the init phase, so only they can be
			// resized in place; a plain init container's spec is final.
			cpu, mem = requests(c, pod.Status.InitContainerStatuses)
			sidecarCPU += cpu
			sidecarMem += mem
			peakCPU = max(peakCPU, sidecarCPU)
			peakMem = max(peakMem, sidecarMem)
			continue
		}
		peakCPU = max(peakCPU, sidecarCPU+cpu)
		peakMem = max(peakMem, sidecarMem+mem)
	}

	var regularCPU, regularMem int64
	for _, c := range pod.Spec.Containers {
		cpu, mem := requests(c, pod.Status.ContainerStatuses)
		regularCPU += cpu
		regularMem += mem
	}

	cpuMilli = max(peakCPU, regularCPU+sidecarCPU)
	memBytes = max(peakMem, regularMem+sidecarMem)

	if pod.Spec.Resources != nil {
		if q, ok := pod.Spec.Resources.Requests[corev1.ResourceCPU]; ok {
			cpuMilli = q.MilliValue()
		}
		if q, ok := pod.Spec.Resources.Requests[corev1.ResourceMemory]; ok {
			memBytes = q.Value()
		}
	}

	if pod.Spec.Overhead != nil {
		cpuMilli += pod.Spec.Overhead.Cpu().MilliValue()
		memBytes += pod.Spec.Overhead.Memory().Value()
	}
	return cpuMilli, memBytes
}

// containerRequests is one container's request after an in-place resize:
// the largest of its spec, the kubelet's allocation and the resources it
// is actually running with — or, when the resize is Infeasible, of the
// status figures alone. No status (not started yet) means spec.
func containerRequests(c corev1.Container, cs *corev1.ContainerStatus, infeasible bool) (cpuMilli, memBytes int64) {
	if cs == nil {
		return c.Resources.Requests.Cpu().MilliValue(), c.Resources.Requests.Memory().Value()
	}
	if !infeasible {
		cpuMilli, memBytes = c.Resources.Requests.Cpu().MilliValue(), c.Resources.Requests.Memory().Value()
	}
	lists := []corev1.ResourceList{cs.AllocatedResources}
	if cs.Resources != nil {
		lists = append(lists, cs.Resources.Requests)
	}
	for _, l := range lists {
		cpuMilli = max(cpuMilli, l.Cpu().MilliValue())
		memBytes = max(memBytes, l.Memory().Value())
	}
	return cpuMilli, memBytes
}

// resizeInfeasible reports a pending in-place resize the node has refused
// outright (PodResizePending, reason Infeasible).
func resizeInfeasible(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodResizePending && c.Reason == corev1.PodReasonInfeasible {
			return true
		}
	}
	return false
}
