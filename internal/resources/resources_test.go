package resources

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// fakeLister returns preset objects per kind, ignoring namespace.
type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
	err  error
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.objs[kind], nil
}

func ptr32(v int32) *int32 { return &v }

// sampleObjects returns one representative object per registered kind, used to
// assert the column/cell invariant.
func sampleObjects() map[kube.ResourceKind]runtime.Object {
	created := metav1.NewTime(time.Now().Add(-90 * time.Minute))
	meta := metav1.ObjectMeta{Name: "sample", Namespace: "observability", CreationTimestamp: created}
	return map[kube.ResourceKind]runtime.Object{
		kube.KindPod:                   &corev1.Pod{ObjectMeta: meta, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true, RestartCount: 2}}}},
		kube.KindDeployment:            &appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Replicas: ptr32(3)}, Status: appsv1.DeploymentStatus{ReadyReplicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3}},
		kube.KindDaemonSet:             &appsv1.DaemonSet{ObjectMeta: meta, Status: appsv1.DaemonSetStatus{NumberReady: 2, DesiredNumberScheduled: 2, NumberAvailable: 2}},
		kube.KindStatefulSet:           &appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Replicas: ptr32(2)}, Status: appsv1.StatefulSetStatus{ReadyReplicas: 2}},
		kube.KindReplicaSet:            &appsv1.ReplicaSet{ObjectMeta: meta, Spec: appsv1.ReplicaSetSpec{Replicas: ptr32(1)}, Status: appsv1.ReplicaSetStatus{ReadyReplicas: 1, Replicas: 1}},
		kube.KindJob:                   &batchv1.Job{ObjectMeta: meta, Spec: batchv1.JobSpec{Completions: ptr32(1)}, Status: batchv1.JobStatus{Succeeded: 1}},
		kube.KindCronJob:               &batchv1.CronJob{ObjectMeta: meta, Spec: batchv1.CronJobSpec{Schedule: "*/5 * * * *"}},
		kube.KindService:               &corev1.Service{ObjectMeta: meta, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.1", ExternalIPs: []string{"203.0.113.10"}, Ports: []corev1.ServicePort{{Port: 80}}}},
		kube.KindIngress:               &networkingv1.Ingress{ObjectMeta: meta, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "sample.local"}}}},
		kube.KindConfigMap:             &corev1.ConfigMap{ObjectMeta: meta, Data: map[string]string{"a": "b"}},
		kube.KindSecret:                &corev1.Secret{ObjectMeta: meta, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"a": []byte("b")}},
		kube.KindPersistentVolumeClaim: &corev1.PersistentVolumeClaim{ObjectMeta: meta, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}},
		kube.KindNode:                  &corev1.Node{ObjectMeta: meta, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.35.0"}}},
		kube.KindNamespace:             &corev1.Namespace{ObjectMeta: meta, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}},
		kube.KindEvent:                 &corev1.Event{ObjectMeta: meta, Type: "Warning", Reason: "FailedScheduling", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api"}},
		kube.KindCustomResourceDefinition: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1",
			"kind":       "CustomResourceDefinition",
			"metadata":   map[string]any{"name": "widgets.example.com", "creationTimestamp": created.UTC().Format(time.RFC3339)},
			"spec": map[string]any{
				"group": "example.com",
				"names": map[string]any{"kind": "Widget", "plural": "widgets"},
				"scope": "Namespaced",
				"versions": []any{
					map[string]any{"name": "v1", "served": true, "storage": true},
				},
			},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Established", "status": "True"}},
			},
		}},
		kube.KindForward: &kube.ForwardObject{Session: kube.ForwardSession{
			ID: "fwd-1", Target: kube.ForwardTarget{Kind: kube.KindPod, Namespace: "observability", Name: "api"},
			ResolvedPod: "api", LocalPort: 8080, RemotePort: 80, State: kube.ForwardActive,
			StartedAt: created.Time, LastActivityAt: time.Now(),
		}},
		kube.KindHelmRelease: kube.NewHelmReleaseObject(kube.HelmRelease{
			Name: "sample", Namespace: "observability", Chart: "sample-chart", ChartVersion: "1.0.0",
			AppVersion: "1.0.0", Revision: 1, Status: "deployed", Updated: created.Time,
		}),
	}
}

// TestDescriptorColumnsMatchCells enforces the core invariant: a projected row
// has exactly as many cells as the descriptor has columns. This catches the
// most common mistake when adding or editing a kind.
func TestDescriptorColumnsMatchCells(t *testing.T) {
	reg := DefaultRegistry()
	samples := sampleObjects()
	for kind, obj := range samples {
		d, ok := reg.Descriptor(kind)
		if !ok {
			t.Fatalf("kind %s has a sample but no descriptor", kind)
		}
		row := d.Project(obj)
		if len(row.Cells) != len(d.Columns) {
			t.Errorf("%s: %d cells, want %d columns (%v vs %v)", kind, len(row.Cells), len(d.Columns), row.Cells, d.Columns)
		}
	}
}

func TestEveryRegisteredKindHasASample(t *testing.T) {
	samples := sampleObjects()
	for _, g := range DefaultGroups() {
		for _, kind := range g.Kinds {
			if _, ok := samples[kind]; !ok {
				t.Errorf("kind %s in group %s has no sample object in the test", kind, g.ID)
			}
		}
	}
}

func TestGroupsReferenceRegisteredKinds(t *testing.T) {
	reg := DefaultRegistry()
	for _, g := range DefaultGroups() {
		for _, kind := range g.Kinds {
			d, ok := reg.Descriptor(kind)
			if !ok {
				t.Errorf("group %s lists unregistered kind %s", g.ID, kind)
				continue
			}
			if d.Group != g.ID {
				t.Errorf("kind %s is in group %s but its descriptor says %s", kind, g.ID, d.Group)
			}
		}
	}
}

func TestProjectPodClassifiesHealth(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true}, {Ready: false}}},
	}
	row := projectPod(pod)
	if row.Name != "api" || row.Cells[1] != "1/2" {
		t.Fatalf("unexpected pod row: %+v", row)
	}
	if row.Status != StatusWarn {
		t.Fatalf("1/2 ready pod should be Warn, got %s", row.Status)
	}
}

func TestProjectPodTagsEphemeralContainerWithFlag(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase:                      corev1.PodRunning,
			ContainerStatuses:          []corev1.ContainerStatus{{Ready: true}},
			EphemeralContainerStatuses: []corev1.ContainerStatus{{Name: "debugger-7xk2", Ready: true}},
		},
	}
	if row := projectPod(pod); row.NameSuffix != " ⚑" {
		t.Fatalf("NameSuffix = %q, want %q for a pod with an ephemeral container", row.NameSuffix, " ⚑")
	}
}

func TestProjectPodNoEphemeralContainerNoFlag(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true}}},
	}
	if row := projectPod(pod); row.NameSuffix != "" {
		t.Fatalf("NameSuffix = %q, want empty — 13d: no chrome until earned", row.NameSuffix)
	}
}

func TestProjectPodHonorsFalseReadyConditionOverContainerReady(t *testing.T) {
	// All containers report Ready, but the pod's own Ready condition is
	// False (e.g. a readiness-gate check failing). k9s colors this red;
	// kute previously ignored the condition and re-derived health purely
	// from container ready counts, so it showed green.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "redis", Namespace: "harbor-internal"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "redis"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
	row := projectPod(pod)
	if row.Cells[1] != "1/1" {
		t.Fatalf("unexpected ready cell: %q", row.Cells[1])
	}
	if row.Status != StatusFail || row.Glyph != "✕" {
		t.Fatalf("pod with False Ready condition should be StatusFail/✕ despite 1/1 container ready, got %s/%s", row.Status, row.Glyph)
	}
	if row.Cells[2] != "CrashLoopBackOff" {
		t.Fatalf("STATUS cell must not fall back to the phase-derived %q for a red row, got %q", "Running", row.Cells[2])
	}
}

func TestProjectPodShowsTerminatingWhileDeleting(t *testing.T) {
	// The API leaves phase at its last real value (often still "Running")
	// after a delete is issued — deletionTimestamp is the only signal a
	// terminating pod carries, and kute must derive "Terminating" from it
	// itself rather than echoing the stale phase back.
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "redis", Namespace: "harbor-internal", DeletionTimestamp: &now},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "redis"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
	row := projectPod(pod)
	if row.Cells[2] != "Terminating" {
		t.Fatalf("expected STATUS cell %q, got %q", "Terminating", row.Cells[2])
	}
	if row.Status != StatusWarn || row.Glyph != "◌" {
		t.Fatalf("terminating pod should be StatusWarn/◌, got %s/%s", row.Status, row.Glyph)
	}
}

func TestProjectPodPendingShowsWaitingReason(t *testing.T) {
	// A Pending phase alone is what k9s calls "ContainerCreating" once the
	// kubelet has actually started pulling/creating the container — kute
	// must surface that (and other waiting reasons) instead of the bare
	// phase, or every not-yet-running pod looks identical.
	cases := []struct {
		name       string
		reason     string
		wantStatus string
	}{
		{"container creating", "ContainerCreating", "ContainerCreating"},
		{"image pull backoff", "ImagePullBackOff", "ImagePullBackOff"},
		{"err image pull", "ErrImagePull", "ErrImagePull"},
		{"create container config error", "CreateContainerConfigError", "CreateContainerConfigError"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}}},
				Status: corev1.PodStatus{
					Phase:             corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: tc.reason}}}},
				},
			}
			row := projectPod(pod)
			if row.Cells[2] != tc.wantStatus {
				t.Fatalf("expected STATUS cell %q, got %q", tc.wantStatus, row.Cells[2])
			}
			if row.Status != StatusWarn || row.Glyph != "▲" {
				t.Fatalf("pending pod should stay StatusWarn/▲, got %s/%s", row.Status, row.Glyph)
			}
		})
	}
}

func TestProjectPodPendingShowsInitContainerProgress(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}}, InitContainers: []corev1.Container{{Name: "init-a"}, {Name: "init-b"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{
				{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}},
				{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
			},
		},
	}
	row := projectPod(pod)
	if row.Cells[2] != "Init:1/2" {
		t.Fatalf("expected STATUS cell %q, got %q", "Init:1/2", row.Cells[2])
	}
}

func TestProjectPodFailedShowsTerminatedReason(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}},
		},
	}
	row := projectPod(pod)
	if row.Cells[2] != "OOMKilled" {
		t.Fatalf("expected STATUS cell %q, got %q", "OOMKilled", row.Cells[2])
	}
	if row.Status != StatusFail || row.Glyph != "✕" {
		t.Fatalf("failed pod should be StatusFail/✕, got %s/%s", row.Status, row.Glyph)
	}
}

func TestProjectPodFailedShowsPodReasonOverContainerReason(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"},
	}
	row := projectPod(pod)
	if row.Cells[2] != "Evicted" {
		t.Fatalf("expected STATUS cell %q, got %q", "Evicted", row.Cells[2])
	}
}

// TestProjectJobSuspendedIsNeutral pins the fix for a deliberately-paused
// Job (browse's 's' verb) rendering as a stuck yellow warning: suspended
// sets Row.Suspended (the same field §30a's Flux rows use — needed at
// keypress time to pick suspend vs. resume) and StatusNeutral, matching the
// app's "parked, benign, nothing to see" hue for a Completed pod.
func TestProjectJobSuspendedIsNeutral(t *testing.T) {
	suspend := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-1", Namespace: "default"},
		Spec:       batchv1.JobSpec{Completions: ptr32(3), Suspend: &suspend},
		Status:     batchv1.JobStatus{Succeeded: 1}, // incomplete — would be StatusWarn if not suspended
	}
	row := projectJobFallback(job)
	if !row.Suspended {
		t.Fatalf("expected Row.Suspended = true, got false")
	}
	if row.Status != StatusNeutral {
		t.Fatalf("expected StatusNeutral for a suspended, incomplete Job, got %s", row.Status)
	}
}

// TestProjectJobFailedOverridesSuspended guards against Suspended's neutral
// status masking a real, already-recorded failure — the Job's own Failed
// condition, not the bare Status.Failed attempt counter (§37a: "a running
// job with 1 failed is retrying and probably fine" — only a real Failed
// condition, e.g. backoffLimit exceeded, turns the whole row red).
func TestProjectJobFailedOverridesSuspended(t *testing.T) {
	suspend := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-1", Namespace: "default"},
		Spec:       batchv1.JobSpec{Completions: ptr32(3), Suspend: &suspend},
		Status: batchv1.JobStatus{
			Succeeded:  1,
			Failed:     1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}},
		},
	}
	row := projectJobFallback(job)
	if row.Status != StatusFail {
		t.Fatalf("expected StatusFail to win over Suspended's neutral, got %s", row.Status)
	}
}

// TestProjectJobNotSuspendedLeavesRowUnset guards against Suspended
// defaulting true on a nil Spec.Suspend.
func TestProjectJobNotSuspendedLeavesRowUnset(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-1", Namespace: "default"},
		Spec:       batchv1.JobSpec{Completions: ptr32(1)},
		Status: batchv1.JobStatus{
			Succeeded:      1,
			CompletionTime: &metav1.Time{Time: time.Now()},
		},
	}
	row := projectJobFallback(job)
	if row.Suspended {
		t.Fatalf("expected Row.Suspended = false for a nil Spec.Suspend")
	}
	if row.Status != StatusOK {
		t.Fatalf("expected StatusOK for a completed, non-suspended Job, got %s", row.Status)
	}
}

// CronJob's own projection (Row.Suspended/Row.Active, the NEXT-cell
// schedule/timezone handling, health tallying) moved to
// internal/resources/cronjobs.go as part of the v0.8.0 Phase 1 CronJob/Job
// aggregation rework — see cronjobs_test.go for its coverage
// (TestProjectCronJob*, TestNextCronRuns*, TestBuildCronJobSummaries*).

func TestProjectIngressBackends(t *testing.T) {
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{"web.local"}, SecretName: "web-tls"}},
			Rules: []networkingv1.IngressRule{{
				Host: "web.local",
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{Path: "/", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}}},
							{Path: "/admin", PathType: &pathType, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "missing", Port: networkingv1.ServiceBackendPort{Number: 80}}}},
						},
					},
				},
			}},
		},
	}

	t.Run("nil lister resolves TLS from spec alone, backends as not-found", func(t *testing.T) {
		row := projectIngress(nil)(ing)
		if row.Cells[3] != "●" {
			t.Fatalf("expected TLS ● cell (spec-only, no lister needed), got %q", row.Cells[3])
		}
		if row.Cells[4] != "0 ok · 2 broken" {
			t.Fatalf("expected every backend to resolve not-found without a lister, got %q", row.Cells[4])
		}
	})

	t.Run("live lister tallies ok/broken unhealthy-first", func(t *testing.T) {
		sel := map[string]string{"app": "web"}
		lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindService: {serviceWithSelector("web", "default", sel)},
			kube.KindPod:     {readyPod("web-1", "default", sel, true)},
		}}
		row := projectIngress(lister)(ing)
		if row.Cells[3] != "●" {
			t.Fatalf("expected TLS ● cell, got %q", row.Cells[3])
		}
		if row.Cells[4] != "1 ok · 1 broken" {
			t.Fatalf("unexpected Backends cell: %q", row.Cells[4])
		}
		if row.Status != StatusFail || row.Glyph != "✕" {
			t.Fatalf("expected unhealthy-first StatusFail/✕, got %s/%s", row.Status, row.Glyph)
		}
	})
}

func TestProjectNodeClassifiesStatus(t *testing.T) {
	ready := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}
	notReady := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}
	memPressure := corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}

	cases := []struct {
		name       string
		node       *corev1.Node
		wantStatus StatusClass
		wantText   string
		wantGlyph  string
		wantSuffix string
	}{
		{
			name:       "ready control-plane",
			node:       &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{ready}}},
			wantStatus: StatusOK, wantText: "Ready", wantGlyph: "●", wantSuffix: " (control-plane)",
		},
		{
			name:       "not ready",
			node:       &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{notReady}}},
			wantStatus: StatusFail, wantText: "NotReady", wantGlyph: "✕",
		},
		{
			name:       "memory pressure",
			node:       &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n3"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{ready, memPressure}}},
			wantStatus: StatusWarn, wantText: "MemPressure", wantGlyph: "▲",
		},
		{
			name:       "cordoned wins over ready",
			node:       &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n4"}, Spec: corev1.NodeSpec{Unschedulable: true}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{ready}}},
			wantStatus: StatusNeutral, wantText: "cordoned", wantGlyph: "◈",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := projectNode(tc.node)
			if row.Status != tc.wantStatus || row.Cells[1] != tc.wantText || row.Glyph != tc.wantGlyph {
				t.Fatalf("projectNode(%s) = status=%s text=%q glyph=%q, want status=%s text=%q glyph=%q",
					tc.name, row.Status, row.Cells[1], row.Glyph, tc.wantStatus, tc.wantText, tc.wantGlyph)
			}
			if row.NameSuffix != tc.wantSuffix {
				t.Fatalf("projectNode(%s).NameSuffix = %q, want %q", tc.name, row.NameSuffix, tc.wantSuffix)
			}
			if row.Cordoned != (tc.wantText == "cordoned") {
				t.Fatalf("projectNode(%s).Cordoned = %v", tc.name, row.Cordoned)
			}
		})
	}
}

func TestProjectDeploymentClassifiesRollout(t *testing.T) {
	cases := []struct {
		name       string
		deploy     *appsv1.Deployment
		wantStatus StatusClass
		wantText   string
		wantImage  string
	}{
		{
			name: "stable",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web", Generation: 1},
				Spec: appsv1.DeploymentSpec{Replicas: ptr32(3), Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "api:2.0"}}},
				}},
				Status: appsv1.DeploymentStatus{
					Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3, ObservedGeneration: 1,
				},
			},
			wantStatus: StatusOK, wantText: "stable", wantImage: "api:2.0",
		},
		{
			name: "generation not yet observed",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web", Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr32(3)},
				Status:     appsv1.DeploymentStatus{Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3, ObservedGeneration: 1},
			},
			wantStatus: StatusWarn, wantText: "progressing " + rolloutArrow,
		},
		{
			name: "still rolling out",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web", Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr32(3)},
				Status:     appsv1.DeploymentStatus{Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 1, AvailableReplicas: 2, ObservedGeneration: 2},
			},
			wantStatus: StatusWarn, wantText: "progressing " + rolloutArrow,
		},
		{
			name: "degraded via progress deadline",
			deploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web", Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr32(3)},
				Status: appsv1.DeploymentStatus{
					Replicas: 3, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 2,
					Conditions: []appsv1.DeploymentCondition{
						{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"},
					},
				},
			},
			wantStatus: StatusFail, wantText: "degraded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := projectDeployment(nil)(tc.deploy)
			if row.Status != tc.wantStatus {
				t.Fatalf("Status = %s, want %s", row.Status, tc.wantStatus)
			}
			if row.Cells[2] != tc.wantText {
				t.Fatalf("Rollout cell = %q, want %q", row.Cells[2], tc.wantText)
			}
			if tc.wantImage != "" && row.Cells[3] != tc.wantImage {
				t.Fatalf("Image cell = %q, want %q", row.Cells[3], tc.wantImage)
			}
		})
	}
}

// TestDeploymentImageShowsNewArrowOldDuringRollout pins 9a (docs/design
// README.md:130: "IMAGE shows new ← old during transition"): while
// progressing, the IMAGE cell must append the previous ReplicaSet's image
// (still-live, owned by this Deployment, different image than the current
// template) as "new ← old" — and must NOT do so for a stable Deployment,
// even when a stale-but-still-live old ReplicaSet happens to exist.
func TestDeploymentImageShowsNewArrowOldDuringRollout(t *testing.T) {
	rollingDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "web", Generation: 2},
		Spec: appsv1.DeploymentSpec{Replicas: ptr32(3), Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "api:2.0"}}},
		}},
		Status: appsv1.DeploymentStatus{
			Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 1, AvailableReplicas: 2, ObservedGeneration: 2,
		},
	}
	oldRS := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-abc123", Namespace: "web",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}},
		},
		Spec:   appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "api:1.0"}}}}},
		Status: appsv1.ReplicaSetStatus{Replicas: 2},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindReplicaSet: {oldRS}}}

	row := projectDeployment(lister)(rollingDeploy)
	if row.Cells[3] != "api:2.0 ← api:1.0" {
		t.Fatalf("Image cell = %q, want %q", row.Cells[3], "api:2.0 ← api:1.0")
	}

	stableDeploy := rollingDeploy.DeepCopy()
	stableDeploy.Status = appsv1.DeploymentStatus{
		Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3, ObservedGeneration: 2,
	}
	row = projectDeployment(lister)(stableDeploy)
	if row.Cells[3] != "api:2.0" {
		t.Fatalf("stable Image cell = %q, want plain %q (no old-side lookup once stable)", row.Cells[3], "api:2.0")
	}

	// A nil lister (pre-connect) must never crash and must fall back to the
	// plain image, same as projectIngress's own nil-lister fallback.
	row = projectDeployment(nil)(rollingDeploy)
	if row.Cells[3] != "api:2.0" {
		t.Fatalf("nil-lister Image cell = %q, want plain %q", row.Cells[3], "api:2.0")
	}
}

func TestDeploymentImageNoContainersIsPlaceholder(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api"}}
	row := projectDeployment(nil)(d)
	if row.Cells[3] != "–" {
		t.Fatalf("Image cell = %q, want placeholder", row.Cells[3])
	}
}

func TestListProjectsAllObjects(t *testing.T) {
	reg := DefaultRegistry()
	d, _ := reg.Descriptor(kube.KindPod)
	src := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true}}}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}},
		},
	}}
	rows, err := List(t.Context(), src, d, "web")
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(rows) != 2 || rows[0].Name != "a" || rows[1].Name != "b" {
		t.Fatalf("unexpected rows: %+v", rows)
	}

	n, err := Count(t.Context(), src, kube.KindPod, "web")
	if err != nil || n != 2 {
		t.Fatalf("Count = %d, %v; want 2", n, err)
	}
}

// TestHealthGlyphOverridesRideOnTheDescriptor pins the per-kind strip
// glyphs that used to be kind-name switches in browse's view: 11a's
// cordoned ◈ for a Node's Neutral class, 18a's pending-upgrade ◌ for a Helm
// release's Warn class. Every other kind falls back to the generic set.
func TestHealthGlyphOverridesRideOnTheDescriptor(t *testing.T) {
	t.Parallel()
	reg := DefaultRegistry()
	tests := []struct {
		kind  kube.ResourceKind
		class StatusClass
		want  string
	}{
		{kube.KindNode, StatusNeutral, "◈"},
		{kube.KindNode, StatusOK, "●"},
		{kube.KindHelmRelease, StatusWarn, "◌"},
		{kube.KindHelmRelease, StatusFail, "✕"},
		{kube.KindPod, StatusNeutral, "○"},
		{kube.KindPod, StatusWarn, "▲"},
	}
	for _, tc := range tests {
		d, ok := reg.Descriptor(tc.kind)
		if !ok {
			t.Fatalf("no descriptor for %s", tc.kind)
		}
		if d.HealthGlyph == nil {
			t.Fatalf("%s: HealthGlyph was not defaulted by the registry", tc.kind)
		}
		if got := d.HealthGlyph(tc.class); got != tc.want {
			t.Errorf("%s/%v glyph = %q, want %q", tc.kind, tc.class, got, tc.want)
		}
	}
}

func TestRBACResourceQualifiesTheAPIGroup(t *testing.T) {
	reg := DefaultRegistry()
	for kind, want := range map[kube.ResourceKind]string{
		kube.KindSecret:     "secrets",
		kube.KindDeployment: "deployments.apps",
		kube.KindIngress:    "ingresses.networking.k8s.io",
	} {
		desc, ok := reg.Descriptor(kind)
		if !ok {
			t.Fatalf("%s not registered", kind)
		}
		if got := RBACResource(desc); got != want {
			t.Errorf("RBACResource(%s) = %q, want %q", kind, got, want)
		}
	}
	crd := Descriptor{Kind: "Certificate", Display: "Certificates", Custom: true, APIGroup: "cert-manager.io"}
	if got := RBACResource(crd); got != "certificates.cert-manager.io" {
		t.Errorf("RBACResource(Certificate) = %q", got)
	}
	// Display is decorated for a colliding discovered kind; RBAC names the
	// discovered plural, never the display text.
	flux := fluxDescriptor(kube.DiscoveredKind{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease", Plural: "helmreleases"})
	if got := RBACResource(flux); got != "helmreleases.helm.toolkit.fluxcd.io" {
		t.Errorf("RBACResource(Flux HelmRelease) = %q (display %q)", got, flux.Display)
	}
	helm, _ := reg.Descriptor(kube.KindHelmRelease)
	if got := RBACResource(helm); got != "secrets" {
		t.Errorf("RBACResource(Helm Releases) = %q, want the release Secrets' resource", got)
	}
}
