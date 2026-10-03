package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func ctr(name, cpu, mem string) corev1.Container {
	return corev1.Container{
		Name: name,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
	}
}

func sidecar(name, cpu, mem string) corev1.Container {
	c := ctr(name, cpu, mem)
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

// TestPodEffectiveRequests pins the scheduler's rule, which is what
// `kubectl describe node` reports under "Allocated resources" — the number
// 11b's REQUESTED / ALLOCATABLE block has to agree with. A plain sum over
// spec.containers gets every case below except the first one wrong.
func TestPodEffectiveRequests(t *testing.T) {
	mi := int64(1024 * 1024)

	tests := []struct {
		name    string
		spec    corev1.PodSpec
		status  corev1.PodStatus
		wantCPU int64
		wantMem int64
	}{
		{
			name:    "regular containers only",
			spec:    corev1.PodSpec{Containers: []corev1.Container{ctr("a", "100m", "128Mi"), ctr("b", "250m", "256Mi")}},
			wantCPU: 350,
			wantMem: 384 * mi,
		},
		{
			name: "a plain init container smaller than the regular sum doesn't raise it",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{ctr("setup", "50m", "64Mi")},
				Containers:     []corev1.Container{ctr("a", "300m", "512Mi")},
			},
			wantCPU: 300,
			wantMem: 512 * mi,
		},
		{
			name: "a heavy init container sets the floor",
			spec: corev1.PodSpec{
				// The node must hold 2000m at some point, even though the
				// regular containers only ever want 300m.
				InitContainers: []corev1.Container{ctr("migrate", "2", "4Gi")},
				Containers:     []corev1.Container{ctr("a", "300m", "512Mi")},
			},
			wantCPU: 2000,
			wantMem: 4096 * mi,
		},
		{
			name: "a native sidecar adds to the regular sum",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{sidecar("istio-proxy", "100m", "128Mi")},
				Containers:     []corev1.Container{ctr("app", "200m", "256Mi")},
			},
			wantCPU: 300,
			wantMem: 384 * mi,
		},
		{
			name: "a plain init running after a sidecar is charged for both",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					sidecar("istio-proxy", "100m", "128Mi"),
					// Runs with the sidecar already up: 100m + 900m = 1000m,
					// which beats the 300m steady state.
					ctr("migrate", "900m", "1Gi"),
				},
				Containers: []corev1.Container{ctr("app", "200m", "256Mi")},
			},
			wantCPU: 1000,
			wantMem: 128*mi + 1024*mi,
		},
		{
			name: "a plain init declared before the sidecar is not charged for it",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					ctr("migrate", "900m", "1Gi"),
					sidecar("istio-proxy", "100m", "128Mi"),
				},
				Containers: []corev1.Container{ctr("app", "200m", "256Mi")},
			},
			wantCPU: 900,
			wantMem: 1024 * mi,
		},
		{
			name: "spec.overhead is added on top",
			spec: corev1.PodSpec{
				Containers: []corev1.Container{ctr("app", "200m", "256Mi")},
				Overhead: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
			wantCPU: 250,
			wantMem: 384 * mi,
		},
		{
			name: "pod-level requests replace the container aggregate",
			spec: corev1.PodSpec{
				Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				}},
				Containers: []corev1.Container{ctr("app", "200m", "256Mi")},
				Overhead:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			},
			wantCPU: 1050,
			wantMem: 2048 * mi,
		},
		{
			name: "a pod-level request only overrides the resource it sets",
			spec: corev1.PodSpec{
				Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"),
				}},
				Containers: []corev1.Container{ctr("app", "200m", "256Mi")},
			},
			wantCPU: 1000,
			wantMem: 256 * mi,
		},
		{
			name: "an in-place resize still in flight holds the larger of spec and status",
			spec: corev1.PodSpec{
				InitContainers: []corev1.Container{sidecar("proxy", "100m", "128Mi")},
				// Resized down in spec; the kubelet hasn't caught up.
				Containers: []corev1.Container{ctr("app", "200m", "256Mi")},
			},
			status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{{
					Name:               "proxy",
					AllocatedResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
				}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:               "app",
					AllocatedResources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
					Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					}},
				}},
			},
			wantCPU: 800,
			wantMem: 128*mi + 1024*mi,
		},
		{
			name: "an infeasible resize counts only what the node granted",
			spec: corev1.PodSpec{Containers: []corev1.Container{ctr("app", "8", "64Gi")}},
			status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{Type: corev1.PodResizePending, Status: corev1.ConditionTrue, Reason: corev1.PodReasonInfeasible}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "app",
					AllocatedResources: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				}},
			},
			wantCPU: 500,
			wantMem: 1024 * mi,
		},
		{
			name:    "no requests at all",
			spec:    corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			wantCPU: 0,
			wantMem: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, mem := PodEffectiveRequests(&corev1.Pod{Spec: tt.spec, Status: tt.status})
			if cpu != tt.wantCPU || mem != tt.wantMem {
				t.Errorf("PodEffectiveRequests = %dm/%d bytes, want %dm/%d bytes", cpu, mem, tt.wantCPU, tt.wantMem)
			}
		})
	}
}

// TestPodFromObjectKeepsPlainContainerSum guards the deliberate split:
// kube.Pod's own request fields describe the pod as written, so they must
// not quietly become the effective-request number.
func TestPodFromObjectKeepsPlainContainerSum(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{sidecar("istio-proxy", "100m", "128Mi")},
		Containers:     []corev1.Container{ctr("app", "200m", "256Mi")},
	}}
	if got := PodFromObject(pod).CPURequestMilli; got != 200 {
		t.Errorf("Pod.CPURequestMilli = %d, want 200 (spec.containers only)", got)
	}
	if cpu, _ := PodEffectiveRequests(pod); cpu != 300 {
		t.Errorf("PodEffectiveRequests = %d, want 300 (sidecar included)", cpu)
	}
}
