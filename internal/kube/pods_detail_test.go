package kube

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodFromObjectPopulatesDetailFields(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "api"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}},
			Tolerations: []corev1.Toleration{
				{Key: "node-role", Operator: corev1.TolerationOpEqual, Value: "edge", Effect: corev1.TaintEffectNoSchedule},
				{Key: "dedicated", Operator: corev1.TolerationOpExists},
			},
		},
		Status: corev1.PodStatus{
			PodIP:    "10.0.0.5",
			QOSClass: corev1.PodQOSGuaranteed,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", Ready: true, RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}

	got := PodFromObject(pod)
	if got.IP != "10.0.0.5" {
		t.Errorf("IP = %q, want 10.0.0.5", got.IP)
	}
	if got.QoSClass != "Guaranteed" {
		t.Errorf("QoSClass = %q, want Guaranteed", got.QoSClass)
	}
	if got.Labels["app"] != "api" {
		t.Errorf("Labels[app] = %q, want api", got.Labels["app"])
	}
	if len(got.Tolerations) != 2 {
		t.Fatalf("Tolerations = %v, want 2 entries", got.Tolerations)
	}
	if got.Tolerations[0] != "node-role=edge:NoSchedule" {
		t.Errorf("Tolerations[0] = %q, want node-role=edge:NoSchedule", got.Tolerations[0])
	}
	if got.Tolerations[1] != "dedicated (exists):All" {
		t.Errorf("Tolerations[1] = %q, want dedicated (exists):All", got.Tolerations[1])
	}
	if len(got.ContainerInfos) != 1 || got.ContainerInfos[0].State != "Running" || got.ContainerInfos[0].Restarts != 2 {
		t.Errorf("ContainerInfos = %+v, want one Running container with 2 restarts", got.ContainerInfos)
	}
	if got.LastTermination != nil {
		t.Errorf("LastTermination = %+v, want nil (no container has terminated)", got.LastTermination)
	}
	if len(got.EphemeralContainerInfos) != 0 {
		t.Errorf("EphemeralContainerInfos = %+v, want none — 13d: no chrome until earned", got.EphemeralContainerInfos)
	}
}

// TestPodFromObjectPopulatesEphemeralContainerInfos covers §41e: a pod with
// an attached debug container gets one EphemeralContainerInfo row, merging
// spec.ephemeralContainers (name/image/target) with
// status.ephemeralContainerStatuses (state), the same shape
// buildContainerInfos already uses for the ordinary grid.
func TestPodFromObjectPopulatesEphemeralContainerInfos(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}},
			EphemeralContainers: []corev1.EphemeralContainer{
				{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{
						Name:  "debugger-7xk2",
						Image: "nicolaka/netshoot",
					},
					TargetContainerName: "app",
				},
			},
		},
		Status: corev1.PodStatus{
			EphemeralContainerStatuses: []corev1.ContainerStatus{
				{Name: "debugger-7xk2", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}

	got := PodFromObject(pod)
	if len(got.EphemeralContainerInfos) != 1 {
		t.Fatalf("EphemeralContainerInfos = %+v, want 1 entry", got.EphemeralContainerInfos)
	}
	e := got.EphemeralContainerInfos[0]
	if e.Name != "debugger-7xk2" || e.Image != "nicolaka/netshoot" || e.TargetContainer != "app" {
		t.Errorf("EphemeralContainerInfos[0] = %+v, want name/image/target from spec", e)
	}
	if e.State != "Running" || !e.Ready {
		t.Errorf("EphemeralContainerInfos[0] = %+v, want Running and ready from status", e)
	}
}

// TestPodFromObjectEphemeralContainerWithoutStatusYet mirrors
// TestPodFromObjectContainerWithoutStatusYet's ordinary-container coverage:
// a just-created ephemeral container has spec data but no status entry yet.
func TestPodFromObjectEphemeralContainerWithoutStatusYet(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "default"},
		Spec: corev1.PodSpec{
			EphemeralContainers: []corev1.EphemeralContainer{
				{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger-1", Image: "busybox"}},
			},
		},
	}

	got := PodFromObject(pod)
	if len(got.EphemeralContainerInfos) != 1 {
		t.Fatalf("expected one EphemeralContainerInfo even with no status yet, got %d", len(got.EphemeralContainerInfos))
	}
	if got.EphemeralContainerInfos[0].State != "" {
		t.Errorf("State = %q, want empty (no status reported)", got.EphemeralContainerInfos[0].State)
	}
}

func TestPodFromObjectDetectsLastTermination(t *testing.T) {
	t.Parallel()
	older := metav1.NewTime(time.Now().Add(-time.Hour))
	newer := metav1.NewTime(time.Now().Add(-time.Minute))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "crash-loop"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", FinishedAt: newer},
					},
				},
				{
					Name: "sidecar",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: older},
					},
				},
			},
		},
	}

	got := PodFromObject(pod)
	if got.LastTermination == nil {
		t.Fatalf("expected a LastTermination")
	}
	if got.LastTermination.Container != "app" {
		t.Errorf("Container = %q, want app (the most recent termination)", got.LastTermination.Container)
	}
	if got.LastTermination.ExitCode != 137 || got.LastTermination.Reason != "OOMKilled" {
		t.Errorf("LastTermination = %+v, want exit 137 OOMKilled", got.LastTermination)
	}
}

// TestPodFromObjectUndatedTerminationHasUnknownAge pins the "106751d ago"
// banner: kubelet's ContainerStatusUnknown record carries no finishedAt, and
// now minus the zero time saturates to the maximum Duration. The container
// has since restarted and is Running, so there is no backoff pending either.
func TestPodFromObjectUndatedTerminationHasUnknownAge(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "litellm"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "litellm"}}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "litellm",
				RestartCount: 1,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"},
				},
			}},
		},
	}

	lt := PodFromObject(pod).LastTermination
	if lt == nil {
		t.Fatal("expected a LastTermination for exit 137")
	}
	if lt.AgeKnown || lt.Age != 0 || !lt.FinishedAt.IsZero() {
		t.Errorf("LastTermination = %+v, want an unknown age (no finishedAt)", lt)
	}
	if lt.BackingOff {
		t.Errorf("BackingOff = true for a Running container, want false")
	}
}

// TestPodFromObjectDatedTerminationBeatsUndated guards the ordering: an
// undated record must not shadow a real, dated crash in another container.
func TestPodFromObjectDatedTerminationBeatsUndated(t *testing.T) {
	t.Parallel()
	finished := metav1.NewTime(time.Now().Add(-time.Hour))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mixed"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "lost"}, {Name: "app"}}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "lost", LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"},
				}},
				{
					Name:         "app",
					RestartCount: 3,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: finished},
					},
				},
			},
		},
	}

	lt := PodFromObject(pod).LastTermination
	if lt == nil || lt.Container != "app" {
		t.Fatalf("LastTermination = %+v, want the dated app crash", lt)
	}
	if !lt.AgeKnown || !lt.BackingOff {
		t.Errorf("LastTermination = %+v, want AgeKnown and BackingOff", lt)
	}
}

// TestPodFromObjectExitCodeZeroIsNotALastTermination pins the fix for a
// completed Job pod rendering a false "Exit code 0" error banner: a clean
// exit was never "why is it broken," so it must not populate
// LastTermination at all.
func TestPodFromObjectExitCodeZeroIsNotALastTermination(t *testing.T) {
	t.Parallel()
	finished := metav1.NewTime(time.Now().Add(-time.Minute))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-1-x7f2k"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", FinishedAt: finished},
					},
				},
			},
		},
	}

	got := PodFromObject(pod)
	if got.LastTermination != nil {
		t.Fatalf("LastTermination = %+v, want nil for a clean exit 0", got.LastTermination)
	}
}

// TestPodFromObjectExitCodeZeroDoesNotMaskRealCrash guards against the exit-0
// exclusion in findLastTermination hiding a genuine crash in another
// container just because it happens to be more recent.
func TestPodFromObjectExitCodeZeroDoesNotMaskRealCrash(t *testing.T) {
	t.Parallel()
	older := metav1.NewTime(time.Now().Add(-time.Hour))
	newer := metav1.NewTime(time.Now().Add(-time.Minute))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mixed"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", FinishedAt: older},
					},
				},
				{
					Name: "sidecar",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", FinishedAt: newer},
					},
				},
			},
		},
	}

	got := PodFromObject(pod)
	if got.LastTermination == nil {
		t.Fatalf("expected the OOMKilled termination to still surface")
	}
	if got.LastTermination.Container != "app" || got.LastTermination.ExitCode != 137 {
		t.Errorf("LastTermination = %+v, want app's exit 137 OOMKilled, not sidecar's clean exit 0", got.LastTermination)
	}
}

func TestPodFromObjectContainerWithoutStatusYet(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}}},
	}
	got := PodFromObject(pod)
	if len(got.ContainerInfos) != 1 {
		t.Fatalf("expected one ContainerInfo even with no status yet, got %d", len(got.ContainerInfos))
	}
	if got.ContainerInfos[0].State != "" {
		t.Errorf("State = %q, want empty (no status reported)", got.ContainerInfos[0].State)
	}
}

// TestPodFromObjectFlagsNativeSidecars pins 10a (docs/design README.md:141:
// "sidecars labeled sidecar"): a native sidecar (KEP-753's initContainer
// with restartPolicy: Always) must be flagged IsSidecar and appended after
// the regular containers; a plain init container (no RestartPolicy) must be
// projected separately so pod detail can show it without offering it to the
// exec picker.
func TestPodFromObjectFlagsNativeSidecars(t *testing.T) {
	t.Parallel()
	always := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "with-sidecar"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:1.0"}},
			InitContainers: []corev1.Container{
				{Name: "migrate", Image: "migrate:1.0"}, // plain init container, no restart policy
				{Name: "envoy", Image: "envoy:1.0", RestartPolicy: &always},
			},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "migrate", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}},
				{Name: "envoy", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}

	got := PodFromObject(pod)
	if len(got.ContainerInfos) != 2 {
		t.Fatalf("ContainerInfos = %+v, want 2 (app + envoy sidecar, migrate excluded)", got.ContainerInfos)
	}
	if got.ContainerInfos[0].IsSidecar {
		t.Errorf("app should not be flagged IsSidecar")
	}
	sidecar := got.ContainerInfos[1]
	if sidecar.Name != "envoy" || !sidecar.IsSidecar {
		t.Errorf("ContainerInfos[1] = %+v, want envoy flagged IsSidecar", sidecar)
	}
	if sidecar.State != "Running" {
		t.Errorf("sidecar State = %q, want Running (matched from InitContainerStatuses)", sidecar.State)
	}
	if len(got.InitContainerInfos) != 1 {
		t.Fatalf("InitContainerInfos = %+v, want migrate only", got.InitContainerInfos)
	}
	if init := got.InitContainerInfos[0]; init.Name != "migrate" || init.State != "Terminated" || init.Reason != "Completed" {
		t.Errorf("InitContainerInfos[0] = %+v, want completed migrate", init)
	}
}

func TestPodFromObjectSharesListProjectionFields(t *testing.T) {
	t.Parallel()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	got := PodFromObject(pod)
	if got.CPURequestMilli != 100 || got.CPULimitMilli != 200 {
		t.Errorf("CPURequestMilli/CPULimitMilli = %d/%d, want 100/200", got.CPURequestMilli, got.CPULimitMilli)
	}
	if got.Status != "Running" {
		t.Errorf("Status = %q, want Running", got.Status)
	}
}

// TestPodFromObjectHasPreviousInstance pins what podlogs' pre-connect check
// keys on (review 04 H2): a container has a readable previous run only when
// its LastTerminationState is a termination with a container ID — the same
// test the kubelet's log handler applies before falling back to "is waiting
// to start". A synthesized ContainerStatusUnknown record carries no ID and
// the kubelet can't serve it.
func TestPodFromObjectHasPreviousInstance(t *testing.T) {
	t.Parallel()
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "crashing"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "crash"}, {Name: "fresh"}, {Name: "lost"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "crash", State: waiting, RestartCount: 4, LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, ContainerID: "containerd://abc"},
			}},
			{Name: "fresh", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
			{Name: "lost", State: waiting, RestartCount: 1, LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"},
			}},
		}},
	}
	want := map[string]bool{"crash": true, "fresh": false, "lost": false}
	for _, ci := range PodFromObject(pod).ContainerInfos {
		if ci.HasPreviousInstance != want[ci.Name] {
			t.Errorf("%s: HasPreviousInstance = %v, want %v", ci.Name, ci.HasPreviousInstance, want[ci.Name])
		}
	}
}
