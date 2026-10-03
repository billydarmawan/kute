package overview

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

func plain(s string) string { return ansi.Strip(s) }

func newSession() *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster"}, Registry: resources.DefaultRegistry()}
}

// fakeLister is a minimal resources.RawLister keyed by kind — every fixture
// case populates only the kinds it needs, mirroring browse's own
// fakeLister test helper.
type fakeLister struct {
	objects map[kube.ResourceKind][]runtime.Object
}

func (f *fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objects[kind], nil
}

type fakeNodeMetrics struct {
	metrics map[string]kube.NodeMetric
	err     error
}

func (f *fakeNodeMetrics) NodeMetrics(_ context.Context) (map[string]kube.NodeMetric, error) {
	return f.metrics, f.err
}

// metricsGone is what the metrics client returns when the metrics.k8s.io
// API isn't served at all — a 404, i.e. no metrics-server.
var metricsGone = apierrors.NewNotFound(schema.GroupResource{Group: "metrics.k8s.io", Resource: "nodes"}, "")

func testNode(name string, ready, cordoned bool, cpuMilli, memBytes, pods int64) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI),
				corev1.ResourcePods:   *resource.NewQuantity(pods, resource.DecimalSI),
			},
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.30.2"},
		},
	}
}

func testPod(ns, name string, phase corev1.PodPhase) *corev1.Pod {
	// projectPod (internal/resources/projections.go) only reads StatusOK for
	// a Running pod when every container's ContainerStatuses entry is
	// Ready:true — an empty ContainerStatuses (0/1 ready) projects as Warn
	// regardless of Phase, so a "healthy" fixture needs one to actually
	// read as OK.
	var statuses []corev1.ContainerStatus
	if phase == corev1.PodRunning {
		statuses = []corev1.ContainerStatus{{Name: "app", Ready: true}}
	}
	// Every phase but Pending has been scheduled; a Pending fixture stands
	// for the unscheduled kind, which holds no node's pod slot.
	node := "node-a"
	if phase == corev1.PodPending {
		node = ""
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app"}}},
		Status:     corev1.PodStatus{Phase: phase, ContainerStatuses: statuses},
	}
}

func testReplicaSet(ns, deployment string, revision int, age time.Duration) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         ns,
			Name:              deployment + "-abc123",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: deployment}},
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": itoa(revision)},
		},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func baseLister() *fakeLister {
	return &fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {
			testNode("node-a", true, false, 4000, 16*1024*1024*1024, 110),
			testNode("node-b", false, false, 4000, 16*1024*1024*1024, 110),
			testNode("node-c", true, true, 4000, 16*1024*1024*1024, 110),
		},
		kube.KindPod: {
			testPod("ns1", "web-1", corev1.PodRunning),
			testPod("ns1", "worker-1", corev1.PodFailed),
			testPod("ns2", "cache-1", corev1.PodPending),
		},
		kube.KindNamespace: {
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns2"}},
		},
		kube.KindReplicaSet: {
			testReplicaSet("ns1", "web", 3, 5*time.Minute),
			testReplicaSet("ns1", "old-app", 7, 2*time.Hour),
		},
	}}
}

// step drains a tea.BatchMsg fan-out synchronously (mirrors whocan/timeline's
// own test helper of the same name).
func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				m = step(t, m, c())
			}
		}
		return m
	}
	updated, cmd := m.Update(msg)
	next := *updated.(*Model)
	if _, ok := msg.(metricsLoadedMsg); ok {
		// Its Cmd is the poll chain's next tick — following it would keep
		// step polling forever. One reading is enough.
		return next
	}
	if cmd != nil {
		return step(t, next, cmd())
	}
	return next
}

func TestLoadAggregatesNodesPodsAndRecentChanges(t *testing.T) {
	m := New(Config{Session: newSession(), Lister: baseLister(), NodeMetrics: &fakeNodeMetrics{err: metricsGone}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	if m.nodeCount != 3 || m.podCount != 3 || m.nsCount != 2 {
		t.Fatalf("counts = (nodes=%d pods=%d ns=%d), want (3,3,2)", m.nodeCount, m.podCount, m.nsCount)
	}
	if len(m.nodeTrouble) != 2 {
		t.Fatalf("nodeTrouble = %d, want 2 (NotReady node-b + cordoned node-c)", len(m.nodeTrouble))
	}
	// CAPACITY's pods bar counts what occupies a node's pod slots: web-1
	// only — worker-1 is terminal and cache-1 was never scheduled.
	if m.capPodsUsed != 1 {
		t.Fatalf("capPodsUsed = %d, want 1 (scheduled, non-terminal pods only)", m.capPodsUsed)
	}
	if len(m.podTrouble) != 2 {
		t.Fatalf("podTrouble = %d, want 2 (Failed worker-1 + Pending cache-1)", len(m.podTrouble))
	}
	// Only the 5m-old ReplicaSet falls inside the fixed 30m window — the
	// 2h-old one must not appear in RECENT CHANGES.
	if len(m.changes) != 1 || m.changes[0].Object != "Deployment/web" {
		t.Fatalf("changes = %+v, want exactly the recent Deployment/web rollout", m.changes)
	}
	// The fake NodeMetrics reader answers 404 here — no metrics-server,
	// so no poll ever lands a reading, so CAPACITY's cpu/mem bars must be
	// omitted rather than showing a flatlined 0-used bar.
	if m.metricsAvailable {
		t.Fatalf("metricsAvailable = true, want false with no metrics polled")
	}

	view := plain(m.Render())
	if !strings.Contains(view, "Cluster Overview") || !strings.Contains(view, "cluster-scoped") {
		t.Fatalf("expected cluster-scoped breadcrumb in view:\n%s", view)
	}
	if !strings.Contains(view, "no metrics-server installed") {
		t.Fatalf("expected the no-metrics-server note in view:\n%s", view)
	}
	if !strings.Contains(view, "node-b") || !strings.Contains(view, "node-c") {
		t.Fatalf("expected both trouble nodes in view:\n%s", view)
	}
	if !strings.Contains(view, "worker-1") || !strings.Contains(view, "cache-1") {
		t.Fatalf("expected both trouble pods in view:\n%s", view)
	}
}

// TestReloadRefreshesTroublePanelAfterExternalChange exercises tui.Reloader:
// the root shell calls Reload when BackMsg restores this screen from the
// stack, because it missed every kube.ResourceChangedMsg while parked
// underneath whatever it pushed (drilling into a TROUBLE row jumps to
// browse, which is what actually receives the delete's change event). Without
// Reload, a pod deleted from that pushed screen keeps showing here until some
// unrelated kind change happens to land while this screen is active again —
// which for a quiet cluster can be a long wait, matching the reported bug.
func TestReloadRefreshesTroublePanelAfterExternalChange(t *testing.T) {
	lister := baseLister()
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if !strings.Contains(plain(m.Render()), "worker-1") {
		t.Fatalf("expected worker-1 in the initial TROUBLE panel:\n%s", plain(m.Render()))
	}

	// The pod is deleted while this screen sits parked in the stack (e.g.
	// the user drilled into it via TROUBLE's ↵, deleted it from browse, then
	// pressed esc) — no ResourceChangedMsg reaches this Model in the
	// meantime, only the lister's cache itself changes underneath it.
	lister.objects[kube.KindPod] = []runtime.Object{
		testPod("ns1", "web-1", corev1.PodRunning),
		testPod("ns2", "cache-1", corev1.PodPending),
	}

	cmd := m.Reload()
	if cmd == nil {
		t.Fatal("Reload returned a nil command with a non-nil lister")
	}
	m = step(t, m, cmd())

	view := plain(m.Render())
	if strings.Contains(view, "worker-1") {
		t.Fatalf("Reload did not refresh the TROUBLE panel — deleted pod still showing:\n%s", view)
	}
	if len(m.podTrouble) != 1 || m.podTrouble[0].Name != "cache-1" {
		t.Fatalf("podTrouble = %+v, want only cache-1 after reload", m.podTrouble)
	}
}

func TestNMetricsSentinelDoesNotCountAsAvailable(t *testing.T) {
	// kube.NodeMetric{CPU:"n/a"} is the fake cluster's own no-metrics-server
	// sentinel (kube/fake.Cluster.NodeMetrics) — a non-empty map full of
	// sentinels must still read as "no metrics", not a real (zero-usage)
	// reading.
	metrics := map[string]kube.NodeMetric{
		"node-a": {CPU: "n/a", MEM: "n/a"},
		"node-c": {CPU: "n/a", MEM: "n/a"},
	}
	m := New(Config{Session: newSession(), Lister: baseLister(), NodeMetrics: &fakeNodeMetrics{metrics: metrics}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.metricsAvailable {
		t.Fatalf("metricsAvailable = true, want false when every node reports the n/a sentinel")
	}
}

// TestCapacityKeepsLastReadingOnFailedPoll: one timed-out or Forbidden poll
// says nothing about usage, so CAPACITY's bars stay up instead of flipping
// to "no metrics-server installed" until the next poll — and a read that
// never succeeded says why rather than claiming nothing is installed.
func TestCapacityKeepsLastReadingOnFailedPoll(t *testing.T) {
	src := &fakeNodeMetrics{metrics: map[string]kube.NodeMetric{
		"node-a": {CPU: "500m", MEM: "1Gi", CPUMilli: 500, MemBytes: 1 << 30},
	}}
	m := New(Config{Session: newSession(), Lister: baseLister(), NodeMetrics: src})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, m.loadMetrics()())
	if !m.metricsAvailable {
		t.Fatalf("metricsAvailable = false after a good poll")
	}

	src.err = context.DeadlineExceeded
	m = step(t, m, m.loadMetrics()())
	view := plain(m.Render())
	if !m.metricsAvailable || !strings.Contains(view, "500m /") {
		t.Fatalf("a failed poll dropped the last good reading:\n%s", view)
	}

	fresh := New(Config{Session: newSession(), Lister: baseLister(), NodeMetrics: &fakeNodeMetrics{
		err: apierrors.NewForbidden(schema.GroupResource{Group: "metrics.k8s.io", Resource: "nodes"}, "", errors.New("nope")),
	}})
	fresh.SetSize(120, 36)
	fresh = step(t, fresh, fresh.Init()())
	fresh = step(t, fresh, fresh.loadMetrics()())
	if view := plain(fresh.Render()); !strings.Contains(view, "not permitted to read metrics-server") {
		t.Fatalf("a Forbidden read didn't say so:\n%s", view)
	}
}

// forbiddenNodeLister simulates a Node reflector that came back Forbidden:
// ListRaw still returns an empty, error-free slice — the real informer-
// backed behavior (CLAUDE.md: "Informer-backed ListRaw returns an empty
// cache when its reflector is forbidden") — and KindForbidden reports the
// denial the way *kube.Cluster does.
type forbiddenNodeLister struct {
	fakeLister
	err error
}

func (f *forbiddenNodeLister) KindSynced(kube.ResourceKind, string) bool { return true }

func (f *forbiddenNodeLister) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == kube.KindNode {
		return f.err
	}
	return nil
}

// TestDeniedNodeCacheRendersPermissionDeniedNotZeroes pins the fix for §5 of
// docs/lazy-informers.md §5.6: loadOverview's ListRaw calls
// never surface a Forbidden reflector as an error (it just returns an empty
// cache), so without an explicit KindsSynced/KindsError gate this rendered
// as a healthy-looking cluster overview with 0 nodes instead of a
// permission-denied card.
func TestDeniedNodeCacheRendersPermissionDeniedNotZeroes(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("nope"))
	lister := &forbiddenNodeLister{
		fakeLister: fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
			kube.KindPod: {testPod("ns1", "web-1", corev1.PodRunning)},
		}},
		err: denied,
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStatePermissionDenied {
		t.Fatalf("state = %s, want permission-denied — the Node cache is Forbidden, not empty", m.state)
	}
}

// forbiddenKindLister simulates a Forbidden reflector on one chosen kind
// (mirrors forbiddenNodeLister, generalized) — used to pin §5's secondary/
// best-effort caches (Namespace/HelmRelease/ReplicaSet), which must flag only
// the fact/panel they back rather than taking over the whole screen the way
// a denied Node/Pod cache does.
type forbiddenKindLister struct {
	fakeLister
	kind kube.ResourceKind
	err  error
}

func (f *forbiddenKindLister) KindSynced(kube.ResourceKind, string) bool { return true }

func (f *forbiddenKindLister) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == f.kind {
		return f.err
	}
	return nil
}

// TestDeniedNamespaceCacheShowsPermissionDeniedNotZero pins §5: a denied
// Namespace cache used to render as "0 namespaces" (loadOverview's own
// best-effort read swallows the error) — indistinguishable from a genuinely
// empty cluster.
func TestDeniedNamespaceCacheShowsPermissionDeniedNotZero(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("nope"))
	lister := &forbiddenKindLister{
		fakeLister: fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
			kube.KindNode: {testNode("node-a", true, false, 4000, 16*1024*1024*1024, 110)},
			kube.KindPod:  {testPod("ns1", "web-1", corev1.PodRunning)},
		}},
		kind: kube.KindNamespace,
		err:  denied,
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — Node/Pod are healthy, a denied Namespace cache must not take over the screen", m.state)
	}
	if !m.nsDenied {
		t.Fatal("nsDenied = false, want true")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "permission denied") {
		t.Fatalf("expected the namespace count to read 'permission denied' in view:\n%s", view)
	}
}

// TestDeniedHelmCacheShowsInlineNoteInTroublePanel pins §5's HelmRelease
// half: a denied cache used to render as if there were simply no outdated
// releases, with no signal the read failed.
func TestDeniedHelmCacheShowsInlineNoteInTroublePanel(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "helmreleases"}, "", errors.New("nope"))
	lister := &forbiddenKindLister{
		fakeLister: fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
			kube.KindNode: {testNode("node-a", true, false, 4000, 16*1024*1024*1024, 110)},
			kube.KindPod:  {testPod("ns1", "web-1", corev1.PodRunning)},
		}},
		kind: kube.KindHelmRelease,
		err:  denied,
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	if !m.helmDenied {
		t.Fatal("helmDenied = false, want true")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "outdated releases: permission denied") {
		t.Fatalf("expected TROUBLE's inline denial note in view:\n%s", view)
	}
}

// TestDeniedReplicaSetCacheShowsInlineNoteInChangesPanel pins §5's
// ReplicaSet half: a denied cache used to render RECENT CHANGES as the
// reassuring "no changes in the last 30m", identical to a genuinely quiet
// cluster.
func TestDeniedReplicaSetCacheShowsInlineNoteInChangesPanel(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "replicasets"}, "", errors.New("nope"))
	lister := &forbiddenKindLister{
		fakeLister: fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
			kube.KindNode: {testNode("node-a", true, false, 4000, 16*1024*1024*1024, 110)},
			kube.KindPod:  {testPod("ns1", "web-1", corev1.PodRunning)},
		}},
		kind: kube.KindReplicaSet,
		err:  denied,
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	if !m.changesDenied {
		t.Fatal("changesDenied = false, want true")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "permission denied: rollout history unavailable") {
		t.Fatalf("expected RECENT CHANGES' inline denial note in view:\n%s", view)
	}
}

// unsyncedKindLister reports one chosen kind as not-yet-synced (KindSynced
// false) while every other kind reads normally — mirrors forbiddenKindLister,
// generalized to the transient (still-filling) case instead of the
// permanent (Forbidden) one.
type unsyncedKindLister struct {
	fakeLister
	kind kube.ResourceKind
}

func (l *unsyncedKindLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return kind != l.kind
}

// TestPendingNamespaceCacheShowsDashNotZero pins the transient half of §5: a
// Namespace cache that's still filling (not yet synced, no error to report)
// used to render as "0 namespaces" — indistinguishable from a genuinely
// empty cluster — because only Forbidden was checked, never KindsSynced.
func TestPendingNamespaceCacheShowsDashNotZero(t *testing.T) {
	lister := &unsyncedKindLister{
		fakeLister: fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
			kube.KindNode: {testNode("node-a", true, false, 4000, 16*1024*1024*1024, 110)},
			kube.KindPod:  {testPod("ns1", "web-1", corev1.PodRunning)},
		}},
		kind: kube.KindNamespace,
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	updated, cmd := m.Update(firstOverviewLoaded(t, m.Init()))
	m = *updated.(*Model)

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — Node/Pod are healthy, a pending Namespace cache must not take over the screen", m.state)
	}
	if !m.nsPending {
		t.Fatal("nsPending = false, want true")
	}
	if cmd == nil {
		t.Fatal("expected a retry command to be scheduled while the Namespace cache is still filling")
	}
	view := plain(m.Render())
	if strings.Contains(view, "0 namespaces") {
		t.Fatalf("expected a dash, not a false '0 namespaces', while the cache is still filling:\n%s", view)
	}
}

// TestPendingHelmCacheShowsLoadingNoteInTroublePanel is
// TestPendingNamespaceCacheShowsDashNotZero's HelmRelease half: a
// still-filling cache used to render as if there were simply no outdated
// releases, identical to a cluster with none.
func TestPendingHelmCacheShowsLoadingNoteInTroublePanel(t *testing.T) {
	lister := &unsyncedKindLister{
		fakeLister: fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
			kube.KindNode: {testNode("node-a", true, false, 4000, 16*1024*1024*1024, 110)},
			kube.KindPod:  {testPod("ns1", "web-1", corev1.PodRunning)},
		}},
		kind: kube.KindHelmRelease,
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	updated, cmd := m.Update(firstOverviewLoaded(t, m.Init()))
	m = *updated.(*Model)

	if !m.helmPending {
		t.Fatal("helmPending = false, want true")
	}
	if cmd == nil {
		t.Fatal("expected a retry command to be scheduled while the HelmRelease cache is still filling")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "outdated releases: loading") {
		t.Fatalf("expected TROUBLE's inline loading note in view:\n%s", view)
	}
}

// TestPendingReplicaSetCacheShowsLoadingNoteInChangesPanel is
// TestPendingNamespaceCacheShowsDashNotZero's ReplicaSet half: a still-
// filling cache used to render RECENT CHANGES as the reassuring "no changes
// in the last 30m", identical to a genuinely quiet cluster.
func TestPendingReplicaSetCacheShowsLoadingNoteInChangesPanel(t *testing.T) {
	lister := &unsyncedKindLister{
		fakeLister: fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
			kube.KindNode: {testNode("node-a", true, false, 4000, 16*1024*1024*1024, 110)},
			kube.KindPod:  {testPod("ns1", "web-1", corev1.PodRunning)},
		}},
		kind: kube.KindReplicaSet,
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	updated, cmd := m.Update(firstOverviewLoaded(t, m.Init()))
	m = *updated.(*Model)

	if !m.changesPending {
		t.Fatal("changesPending = false, want true")
	}
	if cmd == nil {
		t.Fatal("expected a retry command to be scheduled while the ReplicaSet cache is still filling")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "rollout history: loading") {
		t.Fatalf("expected RECENT CHANGES' inline loading note in view:\n%s", view)
	}
}

// firstOverviewLoaded picks the loadedMsg out of Init()'s batch (load +
// spinner tick) — mirrors nodedetail's own firstLoaded test helper.
func firstOverviewLoaded(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	for _, c := range batch {
		if c == nil {
			continue
		}
		if lm, ok := c().(loadedMsg); ok {
			return lm
		}
	}
	t.Fatal("no loadedMsg found in batch")
	return nil
}

func TestAllHealthyRendersGreenAllClear(t *testing.T) {
	lister := &fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a", true, false, 4000, 16*1024*1024*1024, 110)},
		kube.KindPod:  {testPod("ns1", "web-1", corev1.PodRunning)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.nodeTrouble) != 0 || len(m.podTrouble) != 0 {
		t.Fatalf("expected no trouble, got nodes=%v pods=%v", m.nodeTrouble, m.podTrouble)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "nothing unhealthy") {
		t.Fatalf("expected the strip's all-clear line in view:\n%s", view)
	}
	if !strings.Contains(view, "nodes ready") {
		t.Fatalf("expected NODES' all-clear line in view:\n%s", view)
	}
	if !strings.Contains(view, "no changes in the last 30m") {
		t.Fatalf("expected RECENT CHANGES' empty line in view:\n%s", view)
	}
}

func TestTabCyclesFocusAndEnterOpensNodeDetail(t *testing.T) {
	var openedNode string
	m := New(Config{
		Session:     newSession(),
		Lister:      baseLister(),
		NodeMetrics: &fakeNodeMetrics{},
		OpenNodeDetail: func(nodeName string, w, h int) (tea.Model, tea.Cmd) {
			openedNode = nodeName
			return &fakeTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.focus != panelNodes {
		t.Fatalf("initial focus = %v, want panelNodes", m.focus)
	}
	updated, _ := m.Update(tea.KeyPressMsg{Text: "enter"})
	if _, ok := updated.(*fakeTask); !ok {
		t.Fatalf("expected Enter on NODES to push the node-detail task, got %T", updated)
	}
	// sortTrouble ranks Fail before cordoned-only Neutral, so the first
	// selectable row is node-b (NotReady), not node-c (merely cordoned).
	if openedNode != "node-b" {
		t.Fatalf("openNodeDetail called with %q, want node-b", openedNode)
	}

	m.nextPanel()
	if m.focus != panelTrouble {
		t.Fatalf("focus after one tab = %v, want panelTrouble", m.focus)
	}
	m.nextPanel()
	if m.focus != panelChanges {
		t.Fatalf("focus after two tabs = %v, want panelChanges", m.focus)
	}
}

func TestEnterOnTroublePodBacksAndJumps(t *testing.T) {
	m := New(Config{Session: newSession(), Lister: baseLister(), NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m.focus = panelTrouble
	_, cmd := m.Update(tea.KeyPressMsg{Text: "enter"})
	if cmd == nil {
		t.Fatalf("expected Enter on TROUBLE to return a command")
	}
	// tui.GotoResource fires the same navigation the root shell's 'g'
	// palette does — model.go's routeGoto pushes a fresh browse view and
	// keeps overview one esc-back away, rather than this screen needing to
	// pop itself via a BackMsg first.
	gotoMsg, ok := cmd().(tui.GotoResourceMsg)
	if !ok {
		t.Fatalf("expected a GotoResourceMsg, got %T", cmd())
	}
	if gotoMsg.Kind != kube.KindPod || gotoMsg.Name == "" {
		t.Fatalf("GotoResourceMsg = %+v, want a Pod jump", gotoMsg)
	}
}

// outdatedRelease is a deployed release whose chart is behind the local repo
// cache — 18a's outdated signal, as the overview receives it.
func outdatedRelease(ns, name, chart, deployed, available string) *kube.HelmReleaseObject {
	return kube.NewHelmReleaseObject(kube.HelmRelease{
		Namespace: ns, Name: name, Chart: chart, ChartVersion: deployed,
		AppVersion: "1.0.0", Revision: 3, Status: "deployed",
	}.WithLatest(available, "testrepo", false))
}

// TestOutdatedReleasesJoinTheTroublePanel covers the whole path: the load
// reads the Helm kind, only outdated releases make it in, they sort after the
// unhealthy pods, and the panel says what's actually behind.
func TestOutdatedReleasesJoinTheTroublePanel(t *testing.T) {
	lister := baseLister()
	lister.objects[kube.KindHelmRelease] = []runtime.Object{
		outdatedRelease("default", "certs", "cert-manager", "1.14.4", "1.16.2"),
		// Current: must not appear at all.
		kube.NewHelmReleaseObject(kube.HelmRelease{
			Namespace: "default", Name: "redis", Chart: "redis", ChartVersion: "20.1.3",
			Revision: 1, Status: "deployed",
		}.WithLatest("20.1.3", "testrepo", false)),
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.helmOutdated) != 1 || m.helmOutdated[0].Name != "certs" {
		t.Fatalf("helmOutdated = %+v, want just the outdated release", m.helmOutdated)
	}
	entries := m.troubleEntries()
	if len(entries) == 0 {
		t.Fatal("no trouble entries")
	}
	last := entries[len(entries)-1]
	if last.kind != kube.KindHelmRelease {
		t.Errorf("last trouble entry kind = %s, want the release after the unhealthy pods", last.kind)
	}
	for _, e := range entries[:len(entries)-1] {
		if e.kind != kube.KindPod {
			t.Errorf("unhealthy pods must sort before outdated releases, got %s first", e.kind)
		}
	}

	// 60 is the real half-width panel: whatever this says has to survive it.
	body := plain(strings.Join(m.troubleLines(tui.Dark(), 60), "\n"))
	if !strings.Contains(body, "certs") || !strings.Contains(body, "→ 1.16.2") {
		t.Errorf("TROUBLE panel %q doesn't name the release and the version it could move to", body)
	}
}

// TestEnterOnOutdatedReleaseJumpsToTheHelmList is the routing half: the panel
// now aggregates two kinds, and ↵ has to land on the right screen for each.
func TestEnterOnOutdatedReleaseJumpsToTheHelmList(t *testing.T) {
	lister := &fakeLister{objects: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-1", true, false, 4000, 8<<30, 110)},
		// No unhealthy pods, so the release is the only trouble row.
		kube.KindPod:         {testPod("default", "web", corev1.PodRunning)},
		kube.KindHelmRelease: {outdatedRelease("default", "certs", "cert-manager", "1.14.4", "1.16.2")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m.focus = panelTrouble
	_, cmd := m.Update(tea.KeyPressMsg{Text: "enter"})
	if cmd == nil {
		t.Fatal("expected Enter on an outdated release to return a command")
	}
	gotoMsg, ok := cmd().(tui.GotoResourceMsg)
	if !ok {
		t.Fatalf("expected a GotoResourceMsg, got %T", cmd())
	}
	if gotoMsg.Kind != kube.KindHelmRelease || gotoMsg.Name != "certs" {
		t.Fatalf("GotoResourceMsg = %+v, want a HelmRelease jump to certs — a hardcoded Pod kind sends it to the pods table", gotoMsg)
	}
}

// TestHelmChangesReloadTheOverview: a kind the screen displays but doesn't
// reload on goes stale on screen.
func TestHelmChangesReloadTheOverview(t *testing.T) {
	if !isOverviewKind(kube.KindHelmRelease) {
		t.Error("isOverviewKind(HelmRelease) = false — the TROUBLE panel reads it, so it has to reload on it")
	}
}

// TestTroublePanelOrderSurvivesCacheOrder: the panel projects rows straight
// off the informer caches, which hand them back in map-iteration order — a
// different order on every reload. Ranking by status alone left same-status
// rows reshuffling on every watch event, so the panel visibly jumped and the
// selection landed on a different object than the one it was on.
func TestTroublePanelOrderSurvivesCacheOrder(t *testing.T) {
	// Same objects, opposite cache orders — what two consecutive reloads of
	// one unchanged cluster can legitimately return.
	pods := []runtime.Object{
		testPod("ns2", "cache-1", corev1.PodPending),
		testPod("ns1", "worker-1", corev1.PodPending),
		testPod("ns1", "api-1", corev1.PodPending),
		testPod("ns1", "zoo-1", corev1.PodFailed),
		testPod("ns1", "alpha-1", corev1.PodFailed),
	}
	reversed := make([]runtime.Object, len(pods))
	for i, p := range pods {
		reversed[len(pods)-1-i] = p
	}

	order := func(objs []runtime.Object) []string {
		lister := baseLister()
		lister.objects[kube.KindPod] = objs
		lister.objects[kube.KindHelmRelease] = []runtime.Object{
			outdatedRelease("ns2", "certs", "cert-manager", "1.14.4", "1.16.2"),
			outdatedRelease("ns1", "cache", "redis", "18.1.5", "20.1.3"),
		}
		m := New(Config{Session: newSession(), Lister: lister, NodeMetrics: &fakeNodeMetrics{}})
		m.SetSize(120, 36)
		m = step(t, m, m.Init()())
		var names []string
		for _, e := range m.troubleEntries() {
			names = append(names, e.row.Namespace+"/"+e.row.Name)
		}
		for _, r := range m.nodeTrouble {
			names = append(names, "node/"+r.Name)
		}
		return names
	}

	first, second := order(pods), order(reversed)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("trouble order depends on cache order:\n first  = %v\n second = %v", first, second)
	}
	want := []string{
		// Fail before Warn, namespace then name within each rank.
		"ns1/alpha-1", "ns1/zoo-1",
		"ns1/api-1", "ns1/worker-1", "ns2/cache-1",
		// Outdated releases keep their own tail, sorted the same way.
		"ns1/cache", "ns2/certs",
		"node/node-b", "node/node-c",
	}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("trouble order = %v, want %v", first, want)
	}
}

func TestRendersInBothThemes(t *testing.T) {
	for _, theme := range []tui.Theme{tui.Dark(), tui.Light()} {
		sess := newSession()
		sess.Theme = theme
		m := New(Config{Session: sess, Lister: baseLister(), NodeMetrics: &fakeNodeMetrics{}})
		m.SetSize(120, 36)
		m = step(t, m, m.Init()())
		view := plain(m.Render())
		if !strings.Contains(view, "OVERVIEW") || !strings.Contains(view, "CAPACITY") {
			t.Fatalf("theme render missing expected content:\n%s", view)
		}
	}
}

// fakeTask is a minimal tea.Model stand-in for asserting Update pushed a new
// task (mirrors whocan/poddetail's own such-and-such push tests).
type fakeTask struct{}

func (f *fakeTask) Init() tea.Cmd                       { return nil }
func (f *fakeTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return f, nil }
func (f *fakeTask) View() tea.View                      { return tea.NewView("") }

// TestMajorityVersionIsStableAcrossTies pins the tie-break.
//
// The count map is iterated in Go's randomised map order, so an evenly split
// cluster used to pick a different "majority" version on each call — and the
// nodes list marks every node that differs from it, so the markers moved with
// it between redraws. Repeated calls must agree.
func TestMajorityVersionIsStableAcrossTies(t *testing.T) {
	t.Parallel()
	counts := map[string]int{
		"v1.31.0": 2,
		"v1.32.0": 2,
		"v1.30.0": 2,
		"v1.29.0": 1,
	}
	first := majorityVersion(counts)
	if first == "" {
		t.Fatal("majorityVersion returned empty for a populated map")
	}
	for range 200 {
		if got := majorityVersion(counts); got != first {
			t.Fatalf("majorityVersion is unstable: got %q then %q", first, got)
		}
	}
	// A clear winner must still win outright, tie-break or not.
	counts["v1.32.0"] = 9
	if got := majorityVersion(counts); got != "v1.32.0" {
		t.Fatalf("majorityVersion = %q, want the outright majority v1.32.0", got)
	}
}
