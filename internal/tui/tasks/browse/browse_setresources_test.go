package browse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	kubefake "github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// resourcesDeployment is "nva-worker": a single "worker" container with
// cpu/mem request+limit already set, so 25a's panel has real CURRENT values
// to prefill and nudge from.
func resourcesDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1, CreationTimestamp: setImageAge(30 * 24 * time.Hour)},
		Spec: appsv1.DeploymentSpec{Replicas: replicasPtr(2), Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "worker", Image: "nva-worker:1.0", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
				}},
			}},
		}},
		Status: appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2, ObservedGeneration: 1},
	}
}

// resourcesOwnerChain builds the ReplicaSet + one live Pod (owned by that
// ReplicaSet, in turn owned by deployment) workloadPods needs to walk —
// the Pod's worker container carries an OOMKilled LastTerminationState so
// the strip's failure callout has something real to render.
func resourcesOwnerChain(ns, deployment string) (*appsv1.ReplicaSet, *corev1.Pod) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: deployment + "-abc123", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: deployment}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: deployment + "-abc123-xyz", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "worker",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", FinishedAt: setImageAge(4 * time.Minute),
				}},
			}},
		},
	}
	return rs, pod
}

func resourcesLister(dep *appsv1.Deployment, rs *appsv1.ReplicaSet, pod *corev1.Pod) fakeLister {
	return fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep},
		kube.KindReplicaSet: {rs},
		kube.KindPod:        {pod},
	}}
}

func TestBeginSetResourcesPrefillsFieldsFromContainerSpec(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if !m.beginSetResources() {
		t.Fatal("beginSetResources returned false")
	}
	f := m.pendingSetResources.fields
	if f[fieldCPURequest].current != "250m" || f[fieldCPURequest].input.Value() != "250m" {
		t.Errorf("cpu request = %+v, want current/buffer 250m", f[fieldCPURequest])
	}
	if f[fieldCPULimit].current != "1" {
		t.Errorf("cpu limit current = %q, want 1", f[fieldCPULimit].current)
	}
	if f[fieldMEMRequest].current != "512Mi" || f[fieldMEMLimit].current != "512Mi" {
		t.Errorf("mem request/limit = %+v / %+v, want 512Mi/512Mi", f[fieldMEMRequest], f[fieldMEMLimit])
	}
	if !m.pendingSetResources.oomOK {
		t.Error("expected the OOMKilled fact to be found for the worker container")
	}
}

func TestNudgeAdjustsBySaneStepsAndClampsAtZero(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	// cpu request field is selected by default (fieldIdx 0): +50m from 250m.
	m = step(t, m, tea.KeyPressMsg{Text: "+"})
	if got := m.pendingSetResources.fields[fieldCPURequest].input.Value(); got != "300m" {
		t.Fatalf("cpu request after +: %q, want 300m", got)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "-"})
	m = step(t, m, tea.KeyPressMsg{Text: "-"})
	if got := m.pendingSetResources.fields[fieldCPURequest].input.Value(); got != "200m" {
		t.Fatalf("cpu request after +/--: %q, want 200m", got)
	}

	// Move to mem request (fieldIdx 2) and nudge by 64Mi.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Text: "+"})
	if got := m.pendingSetResources.fields[fieldMEMRequest].input.Value(); got != "576Mi" {
		t.Fatalf("mem request after +: %q, want 576Mi", got)
	}

	// Clamp at zero: nudge cpu request down from 200m by 50m four times.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	for range 6 {
		m = step(t, m, tea.KeyPressMsg{Text: "-"})
	}
	if got := m.pendingSetResources.fields[fieldCPURequest].input.Value(); got != "0" {
		t.Fatalf("cpu request after repeated -: %q, want clamped to 0", got)
	}
}

func TestUnsetFieldThenTypingClearsUnset(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	m = step(t, m, tea.KeyPressMsg{Text: "u"})
	if f := m.pendingSetResources.fields[fieldCPURequest]; !f.unset || f.input.Value() != "" {
		t.Fatalf("after u: %+v, want unset with empty buffer", f)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "1"})
	if f := m.pendingSetResources.fields[fieldCPURequest]; f.unset || f.input.Value() != "1" {
		t.Fatalf("after typing post-unset: %+v, want unset cleared, buffer '1'", f)
	}
}

func TestLeftRightArrowsMoveCursorForMidStringEdits(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	// cpu request prefilled to "250m", cursor parked at the end (len 4).
	f := m.pendingSetResources.fields[fieldCPURequest]
	if f.input.Value() != "250m" || f.input.Position() != 4 {
		t.Fatalf("prefill = %+v, want buffer 250m cursor 4", f)
	}

	// Left three times lands the cursor right after "2", before "50m".
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := m.pendingSetResources.fields[fieldCPURequest].input.Position(); got != 1 {
		t.Fatalf("cursor after 3x left = %d, want 1", got)
	}

	// Typing "9" inserts at the cursor rather than appending at the end.
	m = step(t, m, tea.KeyPressMsg{Text: "9"})
	if got := m.pendingSetResources.fields[fieldCPURequest].input.Value(); got != "2950m" {
		t.Fatalf("buffer after mid-string insert = %q, want 2950m", got)
	}
	if got := m.pendingSetResources.fields[fieldCPURequest].input.Position(); got != 2 {
		t.Fatalf("cursor after insert = %d, want 2", got)
	}

	// Right moves it back toward the end; backspace at that position deletes
	// the rune just before the cursor, not the last rune in the buffer.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := m.pendingSetResources.fields[fieldCPURequest].input.Value(); got != "295m" {
		t.Fatalf("buffer after right,right,backspace = %q, want 295m", got)
	}
}

func TestInvalidQuantityBlocksApply(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	for _, r := range "not-a-quantity" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pendingSetResources == nil {
		t.Fatal("expected the panel to stay open after an invalid quantity blocks apply")
	}
	if !m.pendingSetResources.fields[fieldCPURequest].invalid {
		t.Error("expected the cpu request field to be marked invalid")
	}
	if len(mut.setResources) != 0 {
		t.Fatalf("expected SetResources not called, got %v", mut.setResources)
	}
}

func TestRequestGreaterThanLimitBlocksApply(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	// cpu request field (idx 0), current 250m, limit is 1 (1000m) — push
	// the request past the limit via repeated nudges (14 * 50m = 700m + 250m
	// starting point = 950m, one more push crosses 1000m... use enough steps
	// to clear 1 comfortably).
	for range 20 {
		m = step(t, m, tea.KeyPressMsg{Text: "+"})
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pendingSetResources == nil {
		t.Fatal("expected the panel to stay open after request>limit blocks apply")
	}
	if !m.pendingSetResources.fields[fieldCPURequest].invalid || !m.pendingSetResources.fields[fieldCPULimit].invalid {
		t.Error("expected both cpu request and cpu limit fields marked invalid")
	}
	if len(mut.setResources) != 0 {
		t.Fatalf("expected SetResources not called, got %v", mut.setResources)
	}
}

func TestOnlyChangedFieldsGoIntoEdits(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	// Move to mem limit (idx 3) and change it — the only touched field.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Text: "+"})

	edits := m.pendingSetResources.edits()
	if edits.CPURequest != nil || edits.CPULimit != nil || edits.MEMRequest != nil {
		t.Fatalf("edits = %+v, want only MEMLimit set", edits)
	}
	if edits.MEMLimit == nil || *edits.MEMLimit != "576Mi" {
		t.Fatalf("MEMLimit = %v, want 576Mi", edits.MEMLimit)
	}
}

func TestContainerTabSwitchRecomputesFields(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	dep.Spec.Template.Spec.Containers = append(dep.Spec.Template.Spec.Containers, corev1.Container{
		Name: "metrics-sidecar", Image: "sidecar:0.9.1",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
	})
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	if got := m.pendingSetResources.fields[fieldMEMLimit].current; got != "512Mi" {
		t.Fatalf("worker mem limit = %q, want 512Mi", got)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if got := m.pendingSetResources.fields[fieldMEMLimit].current; got != "128Mi" {
		t.Fatalf("sidecar mem limit after tab = %q, want 128Mi", got)
	}
	if m.pendingSetResources.fields[fieldCPURequest].current != "" {
		t.Fatalf("sidecar cpu request = %q, want empty (unset on the container spec)", m.pendingSetResources.fields[fieldCPURequest].current)
	}
}

func TestSetResourcesCommitsThroughMutatorNonProd(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown}) // cpu limit
	m = step(t, m, tea.KeyPressMsg{Text: "+"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	// One dry-run call (commitSetResources' pre-flight) plus one real call
	// (actions.Controller.execute()'s "set-resources" case, dryRun=false).
	if len(mut.setResources) != 2 {
		t.Fatalf("expected exactly two SetResources calls (dry-run + real), got %v", mut.setResources)
	}
	if mut.dryRun {
		t.Fatal("expected the last recorded call to be the real (non-dry-run) apply")
	}
	if _, ok := mut.setResourcesDryRunCtx.Deadline(); !ok {
		t.Fatal("the dry-run ran without a deadline; want the cluster context with the screen's timeout")
	}
	if m.pendingSetResources == nil {
		t.Fatal("expected the panel to remain open after apply")
	}
	want := "applied cpu.limit=1050m"
	if got := m.pendingSetResources.message; got != want {
		t.Fatalf("result message = %q, want %q", got, want)
	}
	if view := plain(m.Render()); !strings.Contains(view, want) {
		t.Fatalf("result is absent from the still-open panel:\n%s", view)
	}
	if got := m.pendingSetResources.fieldIdx; got != fieldCPULimit {
		t.Fatalf("fieldIdx = %d after apply, want the cpu limit field (%d) to stay selected", got, fieldCPULimit)
	}
	if m.actions.Active() {
		t.Fatal("set-resources is TierNone outside PROD and should execute immediately, not show a confirm")
	}
}

func TestSetResourcesFailedApplyPreservesPanelAndAttempt(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	mut := &fakeMutator{setResourcesApplyErr: errors.New("resource quota changed after dry-run")}
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: resourcesLister(dep, rs, pod), Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m.beginSetResources()

	m = step(t, m, tea.KeyPressMsg{Text: "+"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.pendingSetResources == nil {
		t.Fatal("failed apply closed the resources panel")
	}
	if got := m.pendingSetResources.fields[fieldCPURequest].input.Value(); got != "300m" {
		t.Fatalf("attempted cpu request = %q, want 300m", got)
	}
	if got := m.pendingSetResources.lastError; !strings.Contains(got, "resource quota changed") {
		t.Fatalf("lastError = %q", got)
	}
	if view := plain(m.Render()); !strings.Contains(view, "resource quota changed after dry-run") {
		t.Fatalf("server error is absent from the still-open panel:\n%s", view)
	}
}

// TestContainerUsageJoinsOnNamespacedPodKey pins 25a's USAGE join to the key
// shape both metrics providers actually return — kube.PodKey(namespace,
// name) — and proves a same-named pod in another namespace (a DaemonSet, one
// chart installed twice) never lends its usage to the row being edited.
func TestContainerUsageJoinsOnNamespacedPodKey(t *testing.T) {
	podIn := func(ns string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: ns}}
	}
	metrics := map[string]map[string]kube.PodMetrics{
		kube.PodKey("shop", "web-0"):    {"worker": {CPUMilli: 120, MemBytes: 64}},
		kube.PodKey("billing", "web-0"): {"worker": {CPUMilli: 900, MemBytes: 4096}},
	}

	cpu, mem, ok := containerUsage(metrics, []*corev1.Pod{podIn("shop")}, "worker")
	if !ok || cpu != 120 || mem != 64 {
		t.Fatalf("shop/web-0 usage = (%d, %d, %v), want (120, 64, true)", cpu, mem, ok)
	}
	cpu, mem, ok = containerUsage(metrics, []*corev1.Pod{podIn("billing")}, "worker")
	if !ok || cpu != 900 || mem != 4096 {
		t.Fatalf("billing/web-0 usage = (%d, %d, %v), want (900, 4096, true)", cpu, mem, ok)
	}
	if _, _, ok := containerUsage(metrics, []*corev1.Pod{podIn("other")}, "worker"); ok {
		t.Fatal("other/web-0 has no sample but containerUsage reported one")
	}
}

// TestContainerUsageReadsFakeProviderOutput feeds fake.Cluster's own
// ContainerMetricsByNamespace output straight into containerUsage, cluster
// wide, so the join is checked against a real provider's key shape rather
// than a hand-built map that could drift from it.
func TestContainerUsageReadsFakeProviderOutput(t *testing.T) {
	cluster := kubefake.New("default", "test")
	mkPod := func(ns, cpuLimit string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: ns},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "worker",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpuLimit),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				}},
			}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	shop, billing := mkPod("shop", "1"), mkPod("billing", "8")
	cluster.Seed(kube.KindPod, shop, billing)

	metrics, err := cluster.ContainerMetricsByNamespace(t.Context(), "")
	if err != nil {
		t.Fatalf("ContainerMetricsByNamespace: %v", err)
	}
	for _, pod := range []*corev1.Pod{shop, billing} {
		want := metrics[kube.PodKey(pod.Namespace, pod.Name)]["worker"]
		cpu, mem, ok := containerUsage(metrics, []*corev1.Pod{pod}, "worker")
		if !ok {
			t.Fatalf("%s/%s: containerUsage found no sample in the fake provider's output", pod.Namespace, pod.Name)
		}
		if cpu != want.CPUMilli || mem != want.MemBytes {
			t.Fatalf("%s/%s usage = (%d, %d), want (%d, %d)", pod.Namespace, pod.Name, cpu, mem, want.CPUMilli, want.MemBytes)
		}
	}
}

// TestBeginSetResourcesShowsLiveUsageFromProviderKeyedMetrics opens the
// panel against a metrics reader returning the providers' real
// kube.PodKey-keyed shape, with a same-named pod in another namespace
// carrying different usage: the panel must find its own pod's sample, not
// read "metrics unavailable" and not borrow the other namespace's.
func TestBeginSetResourcesShowsLiveUsageFromProviderKeyedMetrics(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	metrics := fakeMetrics{containerMetrics: map[string]map[string]kube.PodMetrics{
		kube.PodKey("default", pod.Name): {"worker": {CPUMilli: 182, MemBytes: 300 << 20}},
		kube.PodKey("staging", pod.Name): {"worker": {CPUMilli: 950, MemBytes: 900 << 20}},
	}}
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Metrics: metrics, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if !m.beginSetResources() {
		t.Fatal("beginSetResources returned false")
	}
	m = step(t, m, m.loadSetResourcesMetrics()())
	tgt := m.pendingSetResources
	if !tgt.usageOK {
		t.Fatal("usageOK = false: the panel never joined the pod's metrics sample")
	}
	if tgt.cpuMilli != 182 || tgt.memBytes != 300<<20 {
		t.Errorf("usage = (%dm, %d bytes), want (182m, %d bytes) from default/%s", tgt.cpuMilli, tgt.memBytes, 300<<20, pod.Name)
	}
}

// countingContainerMetrics counts ContainerMetricsByNamespace calls, so a
// test can prove the read never happens inside Update.
type countingContainerMetrics struct {
	fakeMetrics
	calls *int
}

func (c countingContainerMetrics) ContainerMetricsByNamespace(ctx context.Context, ns string) (map[string]map[string]kube.PodMetrics, error) {
	*c.calls++
	return c.fakeMetrics.ContainerMetricsByNamespace(ctx, ns)
}

// TestSetResourcesMetricsReadNeverBlocksUpdate pins review 06 M4: opening
// 25a must not read metrics-server synchronously inside Update (a hung
// metrics.k8s.io APIService froze every key), and a watch event refreshing
// the post-apply panel must reuse the panel's snapshot rather than read it
// again — only the spec changed.
func TestSetResourcesMetricsReadNeverBlocksUpdate(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	lister := resourcesLister(dep, rs, pod)
	calls := 0
	metrics := countingContainerMetrics{calls: &calls, fakeMetrics: fakeMetrics{containerMetrics: map[string]map[string]kube.PodMetrics{
		kube.PodKey("default", pod.Name): {"worker": {CPUMilli: 182, MemBytes: 300 << 20}},
	}}}
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Metrics: metrics, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: verbs.SetResources.Key})
	m = *updated.(*Model)
	if m.pendingSetResources == nil {
		t.Fatal("expected the set resources panel to open")
	}
	if calls != 0 {
		t.Fatalf("Update read container metrics %d times, want 0 — the read belongs in a tea.Cmd", calls)
	}
	if !strings.Contains(plain(m.Render()), "loading…") {
		t.Fatalf("expected USAGE to read loading… while the metrics read is in flight:\n%s", plain(m.Render()))
	}
	if cmd == nil {
		t.Fatal("expected a metrics read command")
	}
	m = step(t, m, cmd())
	if calls != 1 || !m.pendingSetResources.usageOK {
		t.Fatalf("calls = %d, usageOK = %v after the read landed, want 1/true", calls, m.pendingSetResources.usageOK)
	}

	m.pendingSetResources.message = "applied cpu.request=300m"
	updated, _ = m.Update(kube.ResourceChangedMsg{Kind: kube.KindDeployment})
	m = *updated.(*Model)
	if calls != 1 {
		t.Fatalf("a watch-event refresh read container metrics again (calls = %d), want the snapshot reused", calls)
	}
	if !m.pendingSetResources.usageOK || m.pendingSetResources.cpuMilli != 182 {
		t.Fatalf("refresh dropped the usage snapshot: usageOK=%v cpu=%d", m.pendingSetResources.usageOK, m.pendingSetResources.cpuMilli)
	}
}

// TestOnlyRegistryKeyOpensSetResources: set-resources is reachable only via
// verbs.SetResources.Key — no hand-wired alias that would bypass its gates.
func TestOnlyRegistryKeyOpensSetResources(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: resourcesLister(dep, rs, pod), Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Code: 'V', Text: "V"})
	if m.pendingSetResources != nil {
		t.Fatal("V opened set-resources; only verbs.SetResources.Key should")
	}
	m = step(t, m, tea.KeyPressMsg{Code: 'r', Text: verbs.SetResources.Key})
	if m.pendingSetResources == nil {
		t.Fatal("verbs.SetResources.Key did not open set-resources")
	}
}
