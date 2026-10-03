package timeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

func plain(s string) string { return ansi.Strip(s) }

// fakeEvents is a minimal EventsReader test double — unlike tasks/events'
// own of the same name (which just returns two independent canned slices),
// this one filters a single event list by involvedObject the way
// kube.Cluster's real ObjectEvents does, so eventsForScope's owned-pods
// fan-out (ObjectEvents for the primary object + NamespaceEvents filtered to
// owned pods) can be exercised realistically.
type fakeEvents struct {
	events []kube.Event
	err    error
}

func (f fakeEvents) NamespaceEvents(context.Context, string) ([]kube.Event, error) {
	return f.events, f.err
}

func (f fakeEvents) ObjectEvents(_ context.Context, _ string, kind kube.ResourceKind, name string) ([]kube.Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]kube.Event, 0, len(f.events))
	for _, e := range f.events {
		if e.Object == string(kind)+"/"+name {
			out = append(out, e)
		}
	}
	return out, nil
}

// fakeLister mirrors tasks/poddetail's/nodedetail's own of the same name —
// ignores the namespace argument, same as those.
type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objs[kind], nil
}

// readSyncedLister models the real lazy cache contract: a kind is not synced
// until ListRaw has started/read it. Event is pre-set by the test because the
// separate EventsReader seam performs that read in production.
type readSyncedLister struct {
	fakeLister
	read map[kube.ResourceKind]bool
}

func (l *readSyncedLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	l.read[kind] = true
	return l.fakeLister.ListRaw(ctx, kind, namespace)
}

func (l *readSyncedLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return l.read[kind]
}

func newSession() *tui.Session {
	return &tui.Session{
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "default"},
		Theme:    tui.Dark(),
	}
}

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
	if cmd != nil {
		return step(t, next, cmd())
	}
	return next
}

func testPod(name, node string, terminatedAgo time.Duration) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: corev1.PodSpec{NodeName: node}}
	if terminatedAgo > 0 {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "app",
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(time.Now().Add(-terminatedAgo)),
			}},
		}}
	}
	return pod
}

func TestNamespaceScopedLoadMergesEventsAndRestarts(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "restarting", Count: 1, LastSeen: time.Now()},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {testPod("worker-0", "node-a", 5*time.Minute)},
	}}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	if len(m.rows) != 2 {
		t.Fatalf("rows = %d, want 2 (1 event + 1 restart)", len(m.rows))
	}
	// Newest-first: the just-loaded event (LastSeen ~now) sorts before the
	// 5-minutes-ago restart.
	if m.rows[0].Kind != kube.TimelineEvent || m.rows[1].Kind != kube.TimelineRestart {
		t.Fatalf("rows not newest-first by kind: %+v", m.rows)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "BackOff") {
		t.Fatalf("expected the event's reason in view:\n%s", view)
	}
	if len(m.rail) != 0 {
		t.Fatalf("expected no revision rail in 16a namespace-scoped mode, got %+v", m.rail)
	}
}

// TestOwnerlessPodStartsEveryRequiredCache guards the lazy-informer edge
// where a Pod has no ReplicaSet owner. Timeline still gates its empty/ready
// claim on Deployment, so the load must start that cache even though there
// is no rollout rail whose status would otherwise read it.
func TestOwnerlessPodStartsEveryRequiredCache(t *testing.T) {
	pod := testPod("standalone", "node-a", 0)
	lister := &readSyncedLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {pod}}},
		read:       map[kube.ResourceKind]bool{kube.KindEvent: true},
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: pod.Name,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty after every required cache settles", m.state)
	}
	for _, kind := range []kube.ResourceKind{kube.KindEvent, kube.KindReplicaSet, kube.KindDeployment, kube.KindPod} {
		if !lister.read[kind] {
			t.Errorf("required %s cache was never read", kind)
		}
	}
}

// TestFilterQueryShowsHiddenNotice pins the cross-cutting fix (docs/design
// system-wide interactions: "items never silently disappear"): once '/'
// narrows the merged feed, the strip must say how many entries the query
// itself hid, not just show a bare matched count.
func TestFilterQueryShowsHiddenNotice(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "restarting", Count: 1, LastSeen: time.Now()},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {testPod("worker-0", "node-a", 5*time.Minute)},
	}}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	if !m.filterActive {
		t.Fatal("expected / to activate the filter")
	}
	for _, r := range "BackOff" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	if len(m.rows) != 1 {
		t.Fatalf("expected the filter to narrow to 1 row, got %d", len(m.rows))
	}
	view := plain(m.Render())
	if !strings.Contains(view, "hidden by filter") {
		t.Fatalf("expected the 'hidden by filter' notice:\n%s", view)
	}
}

func TestObjectScopedPodResolvesRevisionRail(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker-abc123", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": "4"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: "nva-worker"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "nginx:1.26"}},
		}}},
	}
	pod := testPod("nva-worker-9k2ss", "node-a", 0)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "nva-worker-abc123"}}

	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:        {pod},
		kube.KindReplicaSet: {rs},
	}}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: "nva-worker-9k2ss",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rail) != 1 || m.railDeployment != "nva-worker" {
		t.Fatalf("expected a 1-entry rail for nva-worker, got rail=%+v deployment=%q", m.rail, m.railDeployment)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "ROLLOUT HISTORY") || !strings.Contains(view, "nginx:1.26") {
		t.Fatalf("expected the revision rail in view:\n%s", view)
	}
}

// TestRevisionRailShowsProgressingWhileDeploymentRollsOut is the regression
// test for the bug report "rollout history always shows 'stable'": a
// Deployment whose Generation hasn't been observed yet (a just-applied image
// tag change, still pulling — the same "generation not yet observed" case
// resources.TestProjectDeploymentClassifiesRollout covers for the browse
// ROLLOUT column) must not have its current-revision rail card say "stable"
// just because no container has restarted yet.
func TestRevisionRailShowsProgressingWhileDeploymentRollsOut(t *testing.T) {
	lister, podName := railFixture()
	replicas := int32(3)
	lister.objs[kube.KindDeployment] = []runtime.Object{&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "default", Generation: 6},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3, ObservedGeneration: 5,
		},
	}}

	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rail) == 0 || m.rail[0].LiveStatusText == "" || m.rail[0].LiveStatusBad {
		t.Fatalf("expected rail[0] to carry a non-bad live progressing override, got %+v", safeRail0(m.rail))
	}
	view := plain(m.Render())
	if !strings.Contains(view, "progressing") {
		t.Fatalf("expected the rail's current-revision card to show 'progressing':\n%s", view)
	}
	if strings.Contains(view, "stable 2m") {
		t.Fatalf("did not expect the mid-rollout current revision to still say 'stable':\n%s", view)
	}
}

// TestRevisionRailShowsDegradedWhenRolloutStalled covers the companion
// stalled-rollout case (ProgressDeadlineExceeded) — the rail should read
// "degraded" (red), matching the browse ROLLOUT column, not "stable".
func TestRevisionRailShowsDegradedWhenRolloutStalled(t *testing.T) {
	lister, podName := railFixture()
	replicas := int32(3)
	lister.objs[kube.KindDeployment] = []runtime.Object{&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "default", Generation: 6},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			Replicas: 3, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ObservedGeneration: 6,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"},
			},
		},
	}}

	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rail) == 0 || m.rail[0].LiveStatusText == "" || !m.rail[0].LiveStatusBad {
		t.Fatalf("expected rail[0] to carry a bad (degraded) live override, got %+v", safeRail0(m.rail))
	}
	view := plain(m.Render())
	if !strings.Contains(view, "degraded") {
		t.Fatalf("expected the rail's current-revision card to show 'degraded':\n%s", view)
	}
}

// TestRevisionRailKeepsRestartsOverLiveStatus asserts restarts still win over
// the live rollout-progress override: a crashlooping pod during a rollout is
// the more specific/urgent signal (view.go's revisionStatus checks restarts
// first).
func TestRevisionRailKeepsRestartsOverLiveStatus(t *testing.T) {
	lister, podName := railFixture()
	replicas := int32(3)
	lister.objs[kube.KindDeployment] = []runtime.Object{&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "default", Generation: 6},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			Replicas: 3, ReadyReplicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3, ObservedGeneration: 5,
		},
	}}
	pods := lister.objs[kube.KindPod]
	crashingPod := testPod("nva-worker-9k2ss", "node-a", 30*time.Second)
	crashingPod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "nva-worker-abc123"}}
	lister.objs[kube.KindPod] = append(pods[:0:0], crashingPod)

	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	if !strings.Contains(view, "1 restart") {
		t.Fatalf("expected the restart to still take priority over the progressing override:\n%s", view)
	}
}

func safeRail0(rail []kube.TimelineEntry) kube.TimelineEntry {
	if len(rail) == 0 {
		return kube.TimelineEntry{}
	}
	return rail[0]
}

func TestObjectScopedNodeFiltersRestartsByNodeName(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			testPod("on-node", "node-a", 2*time.Minute),
			testPod("off-node", "node-b", 2*time.Minute),
		},
	}}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "", ObjectKind: kube.KindNode, ObjectName: "node-a",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 1 || m.rows[0].Object != "Pod/on-node" {
		t.Fatalf("expected only on-node's restart, got %+v", m.rows)
	}
	if len(m.rail) != 0 {
		t.Fatalf("expected no rail for a Node (no Deployment concept), got %+v", m.rail)
	}
}

func TestNodeScopeJoinsPodEventsByNamespaceAndName(t *testing.T) {
	onNode := testPod("web-0", "node-a", 0)
	onNode.Namespace = "team-a"
	elsewhere := testPod("web-0", "node-b", 0)
	elsewhere.Namespace = "team-b"
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {onNode, elsewhere}}}
	events := []kube.Event{
		{Type: "Warning", Reason: "OnNodeReason", Object: "Pod/web-0", Namespace: "team-a", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "ElsewhereReason", Object: "Pod/web-0", Namespace: "team-b", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{events: events}, Lister: lister,
		Namespace: "", ObjectKind: kube.KindNode, ObjectName: "node-a",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	if !strings.Contains(view, "OnNodeReason") || strings.Contains(view, "ElsewhereReason") {
		t.Fatalf("node-a's feed must hold team-a/web-0's event only, not a same-named pod's on another node:\n%s", view)
	}
}

func TestEmptyStateWhenNothingChanged(t *testing.T) {
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty", m.state)
	}
}

func TestEnterOnRowReturnsNavigationCmd(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	cmd, ok := m.openSelectedObject()
	if !ok || cmd == nil {
		t.Fatal("expected ↵ on a row to produce a navigation command")
	}
}

func TestTimeWindowFiltersOldEntries(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "Recent", Object: "Pod/a", Message: "m", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "Ancient", Object: "Pod/b", Message: "m", Count: 1, LastSeen: time.Now().Add(-48 * time.Hour)},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()()) // default window is 30m

	if len(m.rows) != 1 || m.rows[0].Reason != "Recent" {
		t.Fatalf("expected only the recent event within the default 30m window, got %+v", m.rows)
	}

	// Cycle t: 30m -> 1h -> 6h -> 24h -> all. Starting at 30m, 4 more
	// presses reaches "all".
	for range 4 {
		m = step(t, m, tea.KeyPressMsg{Text: "t"})
	}
	if len(m.rows) != 2 {
		t.Fatalf("expected both events once the window is 'all', got %d", len(m.rows))
	}
}

// TestEOpensEventsNamespaceScoped covers 16a's 'e': the global Events verb
// must push 9b scoped to the same namespace timeline itself is showing,
// with no object kind/name (this bug: 'e' was wired for every other list/
// detail screen but never for tasks/timeline).
func TestEOpensEventsNamespaceScoped(t *testing.T) {
	var gotKind kube.ResourceKind
	var gotNamespace, gotName string
	m := New(Config{
		Session:   newSession(),
		Events:    fakeEvents{},
		Namespace: "default",
		OpenEvents: func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
			gotKind, gotNamespace, gotName = kind, namespace, name
			return &Model{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	_, cmd := m.Update(tea.KeyPressMsg{Text: "e"})
	if cmd != nil {
		t.Fatal("openEvents returns no cmd in this test double, expected nil")
	}
	if gotNamespace != "default" || gotKind != "" || gotName != "" {
		t.Fatalf("expected namespace-scoped events push (kind=%q name=%q namespace=%q), want kind=\"\" name=\"\" namespace=default", gotKind, gotName, gotNamespace)
	}
}

// TestEOpensEventsObjectScoped covers 16b's 'e': object-scoped timelines
// must push 9b scoped to that same object.
func TestEOpensEventsObjectScoped(t *testing.T) {
	var gotKind kube.ResourceKind
	var gotNamespace, gotName string
	m := New(Config{
		Session:    newSession(),
		Events:     fakeEvents{},
		Namespace:  "default",
		ObjectKind: kube.KindPod,
		ObjectName: "worker-0",
		OpenEvents: func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
			gotKind, gotNamespace, gotName = kind, namespace, name
			return &Model{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m.Update(tea.KeyPressMsg{Text: "e"})
	if gotKind != kube.KindPod || gotNamespace != "default" || gotName != "worker-0" {
		t.Fatalf("expected object-scoped events push, got kind=%q namespace=%q name=%q", gotKind, gotNamespace, gotName)
	}
}

func TestEscSendsBackMsg(t *testing.T) {
	m := New(Config{Session: newSession(), Events: fakeEvents{}})
	m.SetSize(120, 36)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc produced no command")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatal("esc did not send BackMsg")
	}
}

func TestSlowEarlierLoadCannotOverwriteANewerOne(t *testing.T) {
	ev := func(reason string) kube.Event {
		return kube.Event{Type: "Warning", Reason: reason, Object: "Pod/web-1", Namespace: "default", Count: 1, LastSeen: time.Now()}
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: []kube.Event{ev("Initial")}}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m.events = fakeEvents{events: []kube.Event{ev("StaleReason")}}
	updated, stale := m.Update(kube.ResourceChangedMsg{Kind: kube.KindEvent})
	m = *updated.(*Model)
	m.events = fakeEvents{events: []kube.Event{ev("FreshReason")}}
	updated, fresh := m.Update(kube.ResourceChangedMsg{Kind: kube.KindEvent})
	m = *updated.(*Model)

	// The newer load lands first; the slow earlier one arrives after it.
	m = step(t, m, fresh())
	m = step(t, m, stale())

	view := plain(m.Render())
	if !strings.Contains(view, "FreshReason") || strings.Contains(view, "StaleReason") {
		t.Fatalf("a superseded load overwrote the newer feed:\n%s", view)
	}
}

// TestRolloutDividerChangeOffsetAndCallout covers 16a's full-width rollout
// divider, the +CHANGE column's offset-since-rollout ("+4m"), and the
// summary strip's "first sign of trouble after rollout" correlation
// callout (docs/design README.md §16a) — all three read the same
// firstTroubleAfterRollout so they can never disagree.
func TestRolloutDividerChangeOffsetAndCallout(t *testing.T) {
	now := time.Now()
	entries := []kube.TimelineEntry{
		{Time: now.Add(-90 * time.Second), Kind: kube.TimelineEvent, Object: "Pod/api-gateway-0", Namespace: "default", Severity: "Warning", Reason: "BackOff", Message: "restarting"},
		{Time: now.Add(-6 * time.Minute), Kind: kube.TimelineRollout, Object: "Deployment/api-gateway", Namespace: "default", Reason: "Rollout", Message: "revision 5", Revision: 5, Image: "api-gateway:2.3.1"},
		{Time: now.Add(-10 * time.Minute), Kind: kube.TimelineRestart, Object: "Pod/api-gateway-0", Namespace: "default", Reason: "Restarted", Message: "app · OOMKilled · exit 137"},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(120, 36)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	view := plain(m.Render())
	if !strings.Contains(view, "ROLLOUT deploy/api-gateway · rev 5 · image api-gateway:2.3.1") {
		t.Fatalf("expected the full-width rollout divider text:\n%s", view)
	}
	if !strings.Contains(view, "+4m") {
		t.Fatalf("expected the BackOff row's +CHANGE offset (+4m since the rollout):\n%s", view)
	}
	if !strings.Contains(view, "before") {
		t.Fatalf("expected the pre-rollout restart's +CHANGE to read 'before':\n%s", view)
	}
	if !strings.Contains(view, "first BackOff 4m after rollout of api-gateway") {
		t.Fatalf("expected the summary strip's correlation callout:\n%s", view)
	}
}

// TestObjectScopedRolloutDividerIsCompact covers 16b's own abbreviation of
// the rollout divider: unlike 16a's "ROLLOUT deploy/x · rev A → B · image a
// → b [· by author]" (the object isn't named since many Deployments are
// interleaved there), 16b already names the object in the header/breadcrumb
// — so its divider drops "deploy/x", the "image" label, and any "by"
// attribution, and abbreviates the image transition down to tag-only on
// both sides ("ROLLOUT rev A → B · :a → :b").
func TestObjectScopedRolloutDividerIsCompact(t *testing.T) {
	now := time.Now()
	entries := []kube.TimelineEntry{
		{Time: now.Add(-2 * time.Minute), Kind: kube.TimelineRollout, Object: "Deployment/nva-worker", Namespace: "default", Reason: "Rollout", Message: "revision 43", Revision: 43, Image: "nva-worker:3.4.1", By: "ci@github"},
		{Time: now.Add(-2 * time.Hour), Kind: kube.TimelineRollout, Object: "Deployment/nva-worker", Namespace: "default", Reason: "Rollout", Message: "revision 42", Revision: 42, Image: "nva-worker:3.4.0"},
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Namespace: "default",
		ObjectKind: kube.KindDeployment, ObjectName: "nva-worker",
	})
	m.SetSize(120, 36)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	view := plain(m.Render())
	if !strings.Contains(view, "ROLLOUT rev 42 → 43 · :3.4.0 → :3.4.1") {
		t.Fatalf("expected the compact 16b rollout divider text:\n%s", view)
	}
	if strings.Contains(view, "deploy/nva-worker") || strings.Contains(view, "image ") || strings.Contains(view, "by ci@github") {
		t.Fatalf("expected the 16b divider to drop deploy/name, the 'image' label, and 'by' attribution:\n%s", view)
	}
}

// TestWarningsOnlyDropsNormalEventsKeepsRestartsAndRollouts covers 16a's
// 'w' toggle (docs/design README.md §16a) — Normal-severity events are
// hard-excluded, but a restart and a rollout (never "normal" chatter)
// always survive it.
func TestWarningsOnlyDropsNormalEventsKeepsRestartsAndRollouts(t *testing.T) {
	now := time.Now()
	entries := []kube.TimelineEntry{
		{Time: now.Add(-1 * time.Minute), Kind: kube.TimelineEvent, Severity: "Normal", Reason: "Pulled", Object: "Pod/a", Namespace: "default"},
		{Time: now.Add(-2 * time.Minute), Kind: kube.TimelineEvent, Severity: "Warning", Reason: "BackOff", Object: "Pod/a", Namespace: "default"},
		{Time: now.Add(-3 * time.Minute), Kind: kube.TimelineRestart, Reason: "Restarted", Object: "Pod/a", Namespace: "default"},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(120, 36)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	if len(m.rows) != 2 || len(m.foldedNormal) != 1 {
		t.Fatalf("expected 2 significant rows + 1 folded normal before 'w', got rows=%d folded=%d", len(m.rows), len(m.foldedNormal))
	}

	m = step(t, m, tea.KeyPressMsg{Text: "w"})
	if !m.warningsOnly {
		t.Fatal("expected 'w' to set warningsOnly")
	}
	if len(m.foldedNormal) != 0 {
		t.Fatalf("expected 'w' to drop the normal event entirely (not just fold it), got %+v", m.foldedNormal)
	}
	if len(m.rows) != 2 {
		t.Fatalf("expected the restart and warning to survive 'w', got %d rows", len(m.rows))
	}
}

// TestFoldedNormalFooterAndTabExpand covers 16a's collapsed "normal events"
// footer line and 'tab' expand/collapse (docs/design README.md §16a: "9b's
// own idioms... normals collapsed into one group line").
func TestFoldedNormalFooterAndTabExpand(t *testing.T) {
	now := time.Now()
	entries := []kube.TimelineEntry{
		{Time: now.Add(-1 * time.Minute), Kind: kube.TimelineEvent, Severity: "Warning", Reason: "BackOff", Object: "Pod/a", Namespace: "default"},
		{Time: now.Add(-2 * time.Minute), Kind: kube.TimelineEvent, Severity: "Normal", Reason: "Pulled", Object: "Pod/a", Namespace: "default"},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(120, 36)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	if len(m.rows) != 1 || len(m.foldedNormal) != 1 {
		t.Fatalf("expected 1 significant row + 1 folded normal, got rows=%d folded=%d", len(m.rows), len(m.foldedNormal))
	}
	view := plain(m.Render())
	if !strings.Contains(view, "1 normal events — Pulled") {
		t.Fatalf("expected the folded footer line:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if !m.normalExpanded || len(m.rows) != 2 {
		t.Fatalf("expected tab to expand the normal entry into m.rows, expanded=%v rows=%d", m.normalExpanded, len(m.rows))
	}

	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if m.normalExpanded || len(m.rows) != 1 {
		t.Fatalf("expected a second tab to re-collapse, expanded=%v rows=%d", m.normalExpanded, len(m.rows))
	}
}

// fakeRolloutMutator is a kube.Mutator test double recording RolloutUndo
// calls — every other method is a no-op stub (16b's own rollback flow is
// the only mutating verb this package's tests exercise).
type fakeRolloutMutator struct {
	called          bool
	namespace, name string
	revision        int
	err             error
}

func (f *fakeRolloutMutator) DeleteResource(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeRolloutMutator) DeleteResourceForced(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeRolloutMutator) RolloutRestart(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeRolloutMutator) Cordon(context.Context, string, bool) error { return nil }
func (f *fakeRolloutMutator) Drain(context.Context, string) (int, error) { return 0, nil }
func (f *fakeRolloutMutator) HelmRollback(context.Context, string, string, int) error {
	return nil
}
func (f *fakeRolloutMutator) RolloutUndo(_ context.Context, namespace, name string, toRevision int) error {
	f.called = true
	f.namespace, f.name, f.revision = namespace, name, toRevision
	return f.err
}
func (f *fakeRolloutMutator) Scale(context.Context, kube.ResourceKind, string, string, int32) error {
	return nil
}
func (f *fakeRolloutMutator) SetImage(context.Context, kube.ResourceKind, string, string, string, string, bool) error {
	return nil
}
func (f *fakeRolloutMutator) SetResources(context.Context, kube.ResourceKind, string, string, string, kube.ResourceEdits, bool) error {
	return nil
}
func (f *fakeRolloutMutator) PatchMeta(context.Context, kube.ResourceKind, string, string, bool, string, string, bool) error {
	return nil
}
func (f *fakeRolloutMutator) PatchSecretData(context.Context, string, string, string, string, string, bool) (string, error) {
	return "", nil
}
func (f *fakeRolloutMutator) PatchConfigMapData(context.Context, string, string, string, string, string, bool) (string, error) {
	return "", nil
}

func (f *fakeRolloutMutator) SetFluxSuspend(_ context.Context, kind kube.ResourceKind, namespace, name string, suspend bool) error {
	return nil
}

func (f *fakeRolloutMutator) RequestFluxReconcile(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	return nil
}
func (f *fakeRolloutMutator) RequestArgoRefresh(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	return nil
}
func (f *fakeRolloutMutator) RequestArgoSync(_ context.Context, kind kube.ResourceKind, namespace, name, revision, initiator string) error {
	return nil
}
func (f *fakeRolloutMutator) RenewCertificate(_ context.Context, namespace, name string) error {
	return nil
}
func (f *fakeRolloutMutator) RetryJob(_ context.Context, namespace, name, newName, creator string, at time.Time) error {
	return nil
}
func (f *fakeRolloutMutator) ReplaceJob(_ context.Context, namespace, name string) error {
	return nil
}
func (f *fakeRolloutMutator) SetJobSuspend(_ context.Context, namespace, name string, suspend bool) error {
	return nil
}
func (f *fakeRolloutMutator) TriggerCronJob(_ context.Context, namespace, name, newJobName, creator string, at time.Time) error {
	return nil
}
func (f *fakeRolloutMutator) SetCronJobSuspend(_ context.Context, namespace, name string, suspend bool, resourceVersion string, currentGeneration int64, at time.Time) error {
	return nil
}
func (f *fakeRolloutMutator) SetCronJobSchedule(_ context.Context, namespace, name string, edit kube.CronJobScheduleEdit) (kube.CronJobScheduleResult, error) {
	return kube.CronJobScheduleResult{}, nil
}

// railFixture builds an object-scoped Pod owned (via its ReplicaSet) by
// Deployment "nva-worker" with two rollout revisions (5 current, 4 prior) —
// shared by the rail-focus and rollback tests below.
func railFixture() (fakeLister, string) {
	rsCurrent := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker-abc123", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": "5"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: "nva-worker"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "nginx:1.27"}},
		}}},
	}
	rsPrev := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker-old1", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": "4"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: "nva-worker"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "nginx:1.26"}},
		}}},
	}
	pod := testPod("nva-worker-9k2ss", "node-a", 0)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "nva-worker-abc123"}}
	return fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:        {pod},
		kube.KindReplicaSet: {rsCurrent, rsPrev},
	}}, "nva-worker-9k2ss"
}

// TestRailWindowFollowsTimeNotRevisionNumber pins the lifetime window after
// a rollback: revision 6 reused revision 4's ReplicaSet, which keeps its old
// creation time, so the rail's revision order and time order disagree. The
// window for revision 5 must still run to the next revision *in time*.
func TestRailWindowFollowsTimeNotRevisionNumber(t *testing.T) {
	now := time.Now()
	restart := kube.TimelineEntry{Time: now.Add(-time.Hour), Kind: kube.TimelineRestart, Object: "Pod/nva-worker-9k2ss", Reason: "Restarted"}
	m := Model{
		fetchedAt: now,
		rail: []kube.TimelineEntry{
			{Time: now.Add(-time.Minute), Kind: kube.TimelineRollout, Object: "Deployment/nva-worker", Revision: 7},
			{Time: now.Add(-21 * 24 * time.Hour), Kind: kube.TimelineRollout, Object: "Deployment/nva-worker", Revision: 6, PriorRevisions: []int{4}},
			{Time: now.Add(-2 * time.Hour), Kind: kube.TimelineRollout, Object: "Deployment/nva-worker", Revision: 5},
		},
		entries:      []kube.TimelineEntry{restart},
		railSelected: 2,
	}
	got, ok := m.railSelectionTarget()
	if !ok || got.Kind != kube.TimelineRestart {
		t.Fatalf("target = %+v, want the restart inside revision 5's lifetime", got)
	}
}

// TestRailFocusAndRollbackFlow covers 16b's rail-focused-by-default model
// (the rail is why you pressed 't' on a workload, so it starts focused —
// 'tab' moves focus onto the feed instead) end-to-end through a non-prod
// rollback.
func TestRailFocusAndRollbackFlow(t *testing.T) {
	lister, podName := railFixture()
	mut := &fakeRolloutMutator{}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister, Mutator: mut,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rail) != 2 {
		t.Fatalf("expected a 2-entry rail, got %+v", m.rail)
	}
	if !m.railFocused {
		t.Fatal("expected the rail to start focused")
	}

	m = step(t, m, tea.KeyPressMsg{Text: "down"})
	if m.railSelected != 1 {
		t.Fatalf("expected down to move the rail cursor to revision 4 (index 1), got railSelected=%d", m.railSelected)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if !m.actionsCtl.Active() {
		t.Fatal("expected R to begin the rollback confirmation")
	}
	pending := m.actionsCtl.Pending()
	if pending == nil || pending.Scope.Verb != "rollout-undo" || pending.Scope.ResourceName != "nva-worker" || pending.Scope.Revision != 4 {
		t.Fatalf("unexpected pending action: %+v", pending)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if !mut.called || mut.namespace != "default" || mut.name != "nva-worker" || mut.revision != 4 {
		t.Fatalf("expected RolloutUndo(default, nva-worker, 4), got called=%v ns=%q name=%q rev=%d", mut.called, mut.namespace, mut.name, mut.revision)
	}
}

// rail[0] is the live revision: R there would only patch the template to
// what it already is, so it's refused with a reason instead of confirmed.
func TestRollbackToTheCurrentRevisionIsRefused(t *testing.T) {
	lister, podName := railFixture()
	mut := &fakeRolloutMutator{}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister, Mutator: mut,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if m.actionsCtl.Active() || mut.called {
		t.Fatal("R on the current revision must not begin a rollback")
	}
	if res := m.ActionResult(); !res.Failed || !strings.Contains(res.Text, "current revision") {
		t.Fatalf("expected a refusal naming the current revision, got %+v", res)
	}
}

// TestRolloutUndoInProdRequiresTypedName mirrors poddetail's own
// TestDeleteInProdRequiresTypedName for 16b's rollback: a PROD context
// escalates to the type-the-deployment-name modal, and a bare "y" types
// into the buffer rather than confirming (docs/design README.md §16b).
func TestRolloutUndoInProdRequiresTypedName(t *testing.T) {
	lister, podName := railFixture()
	mut := &fakeRolloutMutator{}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{
		Session: sess, Events: fakeEvents{}, Lister: lister, Mutator: mut,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "down"})
	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if !m.actionsCtl.Active() || m.actionsCtl.Tier() != actions.TierModal {
		t.Fatalf("expected R in a prod context to open the type-the-name modal, tier=%v", m.actionsCtl.Tier())
	}
	view := plain(m.Render())
	if !strings.Contains(view, `type "nva-worker" to confirm`) || !strings.Contains(view, "rollback (when name matches)") {
		t.Fatalf("expected the type-the-name modal actually rendered in Body():\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if mut.called {
		t.Fatal("expected 'y' to type, not confirm, in the modal")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if mut.called {
		t.Fatal("expected enter to no-op before the name matches")
	}

	// esc cancels the "y"-tainted buffer; re-opening starts fresh so the
	// real name below isn't prefixed with that stray "y".
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.actionsCtl.Active() {
		t.Fatal("expected esc to cancel the modal")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	for _, r := range "nva-worker" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if !mut.called || mut.revision != 4 {
		t.Fatalf("expected RolloutUndo to fire once the name matches, got called=%v rev=%d", mut.called, mut.revision)
	}
}

// TestRailSelectionLiveSyncsFeedCursor covers 16b's rail↔feed live sync: the
// rail starts focused (point 1) with the feed cursor already synced onto the
// current revision, and moving the rail cursor moves the feed cursor onto
// that revision's own ROLLOUT row immediately — no '↵' needed — widening the
// window to "all time" when the target revision's rollout (2h old,
// railFixture) falls outside the default 30m window, and focus stays on the
// rail throughout so ↑↓ keeps driving revision selection.
func TestRailSelectionLiveSyncsFeedCursor(t *testing.T) {
	lister, podName := railFixture()
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if !m.railFocused {
		t.Fatal("expected the rail to start focused")
	}
	got, ok := m.selectedRow()
	if !ok || got.Kind != kube.TimelineRollout || got.Revision != 5 {
		t.Fatalf("expected the feed cursor already on revision 5's rollout row, got %+v ok=%v", got, ok)
	}

	if idx := m.indexOfEntry(m.rail[1]); idx >= 0 {
		t.Fatalf("expected revision 4's rollout outside the default window before moving the rail cursor, found at %d", idx)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "down"})
	if !m.railFocused {
		t.Fatal("expected focus to stay on the rail after moving the cursor")
	}
	if m.railSelected != 1 {
		t.Fatalf("expected the rail cursor on revision 4 (index 1), got %d", m.railSelected)
	}
	if m.window != 0 {
		t.Fatalf("expected moving onto revision 4 to widen the window to all time, got %v", m.window)
	}
	got, ok = m.selectedRow()
	if !ok || got.Kind != kube.TimelineRollout || got.Revision != 4 {
		t.Fatalf("expected the feed cursor to live-sync onto revision 4's rollout row, got %+v ok=%v", got, ok)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "up"})
	got, ok = m.selectedRow()
	if !ok || got.Kind != kube.TimelineRollout || got.Revision != 5 {
		t.Fatalf("expected moving back up to live-sync the feed cursor onto revision 5, got %+v ok=%v", got, ok)
	}
}

// TestRailSelectionSyncsToLatestEventNotJustRollout covers the rest of
// point 2: when the selected revision's own lifetime window contains a real
// entry (a restart, here), the live sync lands the feed cursor on that
// entry — the most recent thing that actually happened under the
// revision — rather than always landing on the revision's own ROLLOUT row.
func TestRailSelectionSyncsToLatestEventNotJustRollout(t *testing.T) {
	lister, podName := railFixture()
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	// A restart landing inside revision 4's own window (its rollout up to
	// revision 5's).
	restart := kube.TimelineEntry{
		Time: m.rail[1].Time.Add(10 * time.Minute), Kind: kube.TimelineRestart,
		Object: "Pod/" + podName, Namespace: "default", Reason: "Restarted", Message: "OOMKilled",
	}
	m.entries = append(m.entries, restart)

	m = step(t, m, tea.KeyPressMsg{Text: "down"})
	got, ok := m.selectedRow()
	if !ok || got.Kind != kube.TimelineRestart {
		t.Fatalf("expected the feed cursor to sync onto the restart under revision 4, not the rollout row, got %+v ok=%v", got, ok)
	}
}

// TestShortImageDropsRegistryPrefix covers point 3: only the trailing
// "name:tag" component of an image reference is ever shown — an internal
// registry host (with its own port, common for self-hosted registries)
// never belongs in a 60-column sidebar.
// TestFeedLinesNeverExceedsRequestedHeight is the overflow-guard regression
// test for feedLines' new variable-height rows: a message long enough that
// its own wrapped line count alone exceeds a tiny height budget must still
// come back trimmed to that budget — 16a's no-rail Body branch joins
// feedLines' return straight into the screen with no further trim (unlike
// 16b's with-rail branch, which pads/truncates to height itself), so a
// wrapped row overflowing its budget would otherwise blow past the body's
// allotted height and misalign the keybar underneath it.
func TestFeedLinesNeverExceedsRequestedHeight(t *testing.T) {
	longMsg := strings.Repeat("word ", 200)
	entries := []kube.TimelineEntry{
		{Time: time.Now(), Kind: kube.TimelineEvent, Object: "Pod/worker-0", Namespace: "default", Severity: "Warning", Reason: "BackOff", Message: longMsg},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(80, 24)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	const height = 2
	lines := m.feedLines(m.Theme(), 80, height)
	if len(lines) > height {
		t.Fatalf("expected feedLines to trim to at most %d lines even when the selected row alone wraps taller than that, got %d", height, len(lines))
	}
}

// TestLongWhatTextWrapsOntoContinuationLine covers the feed's own long-text
// handling: a WHAT message that doesn't fit the column grows the row onto a
// second physical line (wrapText) rather than ellipsizing with "…" the way
// every other fixed-width column (OBJECT, the rail's own rev/image lines)
// still does.
func TestLongWhatTextWrapsOntoContinuationLine(t *testing.T) {
	longMsg := "BackOff · back-off restarting failed container because the readiness probe has been failing continuously for the last several minutes across every replica"
	entries := []kube.TimelineEntry{
		{Time: time.Now(), Kind: kube.TimelineEvent, Object: "Pod/worker-0", Namespace: "default", Severity: "Warning", Reason: "", Message: longMsg},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(80, 24)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	lines := m.renderRow(m.Theme(), m.rows[0], false, 80)
	if len(lines) < 2 {
		t.Fatalf("expected the long WHAT text to wrap onto at least 2 physical lines, got %d: %+v", len(lines), lines)
	}
	joined := plain(strings.Join(lines, " "))
	if !strings.Contains(joined, "every replica") {
		t.Fatalf("expected the full message to survive across wrapped lines (no ellipsis truncation), got:\n%s", joined)
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("did not expect the wrapped WHAT text to still be ellipsized:\n%s", joined)
	}
}

// TestSelectedWrappedRowBarCoversEveryLine is the regression test for a
// selected row whose WHAT text wraps: the "▎" selection bar (the "primary
// cue" per docs/design README.md) must run down every physical line the row
// occupies, not just the first — a bar that stops after line one reads as
// only half the row being selected.
func TestSelectedWrappedRowBarCoversEveryLine(t *testing.T) {
	longMsg := "BackOff · back-off restarting failed container because the readiness probe has been failing continuously for the last several minutes across every replica"
	entries := []kube.TimelineEntry{
		{Time: time.Now(), Kind: kube.TimelineEvent, Object: "Pod/worker-0", Namespace: "default", Severity: "Warning", Reason: "", Message: longMsg},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(80, 24)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	lines := m.renderRow(m.Theme(), m.rows[0], true, 80)
	if len(lines) < 2 {
		t.Fatalf("expected the long WHAT text to wrap onto at least 2 physical lines, got %d", len(lines))
	}
	for i, l := range lines {
		if got := plain(l); !strings.HasPrefix(got, tui.GlyphSelBar) {
			t.Fatalf("expected every wrapped line of a selected row to start with the %q bar, line %d was %q", tui.GlyphSelBar, i, got)
		}
	}
}

// TestRolloutDividerFollowedByHorizontalRule covers the feed's own
// separator between a ROLLOUT divider and the older entries beneath it: a
// full-width rule line, not just a background-color change.
func TestRolloutDividerFollowedByHorizontalRule(t *testing.T) {
	entries := []kube.TimelineEntry{
		{Time: time.Now().Add(-time.Minute), Kind: kube.TimelineRollout, Object: "Deployment/api-gateway", Namespace: "default", Reason: "Rollout", Revision: 5, Image: "api-gateway:2.3.1"},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(80, 24)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	lines := m.renderRow(m.Theme(), m.rows[0], false, 80)
	if len(lines) < 2 {
		t.Fatalf("expected the ROLLOUT divider to carry a trailing rule line, got %d lines: %+v", len(lines), lines)
	}
	rule := plain(lines[len(lines)-1])
	if !strings.Contains(rule, "────") {
		t.Fatalf("expected the divider's last line to be a horizontal rule, got %q", rule)
	}
}

// TestUnselectedRolloutRowHasNoPermanentBackground covers the "same
// background for every item" requirement: unlike the old MarkBg tint that
// used to mark every ROLLOUT row regardless of selection, an unselected
// rollout row's body line must carry no background escape at all — the
// exact same (absent) background a plain unselected feed row has. Only
// selection (theme.SelBg) or the trailing rule (its own foreground-only
// style) may add color.
func TestUnselectedRolloutRowHasNoPermanentBackground(t *testing.T) {
	entries := []kube.TimelineEntry{
		{Time: time.Now().Add(-time.Minute), Kind: kube.TimelineRollout, Object: "Deployment/api-gateway", Namespace: "default", Reason: "Rollout", Revision: 5, Image: "api-gateway:2.3.1"},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(80, 24)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	lines := m.renderRow(m.Theme(), m.rows[0], false, 80)
	if strings.Contains(lines[0], "48;2;") {
		t.Fatalf("expected an unselected ROLLOUT row to carry no background color, got:\n%q", lines[0])
	}
}

// TestSelectedFeedRowShowsAccentBar covers the app-wide "1-cell accent bar
// + SelBg row background — the bar is the primary cue" selection rule
// (docs/design README.md's Mapping HTML → terminal section): the timeline
// feed used to mark selection with background tint alone, unlike every
// other list in the app (components.Table's own SelBarStyle). A selected
// row's marker slot must now render the same "▎" bar every other list
// uses; an unselected row's marker slot must stay blank.
func TestSelectedFeedRowShowsAccentBar(t *testing.T) {
	entries := []kube.TimelineEntry{
		{Time: time.Now(), Kind: kube.TimelineEvent, Object: "Pod/worker-0", Namespace: "default", Severity: "Warning", Reason: "BackOff", Message: "m"},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(80, 24)
	updated, _ := m.Update(loadedMsg{entries: entries})
	m = *updated.(*Model)

	selected := plain(m.renderRow(m.Theme(), m.rows[0], true, 80)[0])
	if !strings.HasPrefix(selected, tui.GlyphSelBar) {
		t.Fatalf("expected a selected feed row to start with the %q selection bar, got %q", tui.GlyphSelBar, selected)
	}
	unselected := plain(m.renderRow(m.Theme(), m.rows[0], false, 80)[0])
	if strings.HasPrefix(unselected, tui.GlyphSelBar) {
		t.Fatalf("expected an unselected feed row to carry no selection bar, got %q", unselected)
	}
}

// lineHasUnstyledSpace parses line's raw SGR escapes and reports whether any
// space character renders with no background color active — a bare `"  "`
// (or even a single un-styled `" "`) string literal concatenated between two
// rendered spans, rather than routed through gap()/gapStyle.Render, punches
// exactly this kind of hole in an otherwise-solid selected background.
// Every span this package renders is a single self-contained
// Style.Render() call (opens the codes it needs, ends with a full "\x1b[0m"
// reset — never a partial/cumulative escape spanning multiple concatenated
// pieces), so tracking "is some `48;` background code currently open"
// sequentially across the whole line is accurate for this codebase's own
// rendering pattern.
func lineHasUnstyledSpace(line string) bool {
	bgActive := false
	for i := 0; i < len(line); i++ {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && line[j] != 'm' {
				j++
			}
			if j >= len(line) {
				break
			}
			codes := line[i+2 : j]
			if codes == "" || codes == "0" {
				bgActive = false
			} else if strings.Contains(codes, "48;") {
				bgActive = true
			}
			i = j
			continue
		}
		if line[i] == ' ' && !bgActive {
			return true
		}
	}
	return false
}

// TestSelectedRowsHaveNoUnstyledBackgroundGap covers the "solid background"
// requirement for both panes: every gap between styled spans on a selected
// row — including the marker slot, the WHEN/glyph/+CHANGE/OBJECT
// separators, and the rail card's own gutter — must be routed through a
// style carrying SelBg, never left as a bare literal that would punch an
// unstyled notch in an otherwise-solid selected background.
func TestSelectedRowsHaveNoUnstyledBackgroundGap(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker-abc123", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": "5"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: "nva-worker"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "nginx:1.27"}},
		}}},
	}
	pod := testPod("nva-worker-9k2ss", "node-a", 0)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "nva-worker-abc123"}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:        {pod},
		kube.KindReplicaSet: {rs},
	}}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: "nva-worker-9k2ss",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if len(m.rail) == 0 {
		t.Fatal("expected a resolved revision rail for this fixture")
	}

	theme := m.Theme()
	// The last line is renderRolloutDivider's own trailing rule (a
	// theme.BorderSubtle foreground-only "─" run, unconditional regardless
	// of selection) — correctly has no background at all, so it's excluded
	// from this check.
	dividerLines := m.renderRolloutDivider(theme, m.rail[0], true, 100)
	for _, line := range dividerLines[:len(dividerLines)-1] {
		if lineHasUnstyledSpace(line) {
			t.Fatalf("selected ROLLOUT divider has an unstyled background gap:\n%q", line)
		}
	}
	entries := []kube.TimelineEntry{
		{Time: time.Now(), Kind: kube.TimelineEvent, Object: "Pod/worker-0", Namespace: "default", Severity: "Warning", Reason: "BackOff", Message: "m"},
	}
	loaded, _ := m.Update(loadedMsg{entries: entries, rail: m.rail, railDeployment: m.railDeployment})
	m = *loaded.(*Model)
	for _, line := range m.renderRow(theme, m.rows[0], true, 100) {
		if lineHasUnstyledSpace(line) {
			t.Fatalf("selected feed row has an unstyled background gap:\n%q", line)
		}
	}
	for _, line := range m.railCardLines(theme, m.rail[0], 0, 30) {
		if lineHasUnstyledSpace(line) {
			t.Fatalf("selected rail card has an unstyled background gap:\n%q", line)
		}
	}
}

func TestShortImageDropsRegistryPrefix(t *testing.T) {
	cases := map[string]string{
		"registry.example.com/nva/nva.bp.app:5.31.0.58108": "nva.bp.app:5.31.0.58108",
		"gcr.io/my-project/my-image:tag":                   "my-image:tag",
		"nginx:1.27":                                       "nginx:1.27",
		"checkout-api":                                     "checkout-api",
	}
	for in, want := range cases {
		if got := shortImage(in); got != want {
			t.Errorf("shortImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRailCardShowsShortImage covers point 3 end-to-end: the rail sidebar
// itself renders the shortened image, not the full registry-qualified
// reference.
func TestRailCardShowsShortImage(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nva-worker-abc123", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": "4"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: "nva-worker"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "registry.example.com/nva/nva.bp.app:5.31.0.58108"}},
		}}},
	}
	pod := testPod("nva-worker-9k2ss", "node-a", 0)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "nva-worker-abc123"}}

	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:        {pod},
		kube.KindReplicaSet: {rs},
	}}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: "nva-worker-9k2ss",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	if !strings.Contains(view, "nva.bp.app:5.31.0.58108") {
		t.Fatalf("expected the short image name in view:\n%s", view)
	}
	if strings.Contains(view, "registry.example.com") {
		t.Fatalf("expected the registry host to be dropped from view:\n%s", view)
	}
}

// TestDeploymentScopedTimelineIncludesPodEvents is a regression test for a
// live bug report: opening 16b's Timeline from a Deployment showed only
// rollouts, because a container-level event like CreateContainerError is
// always emitted with involvedObject == the Pod, never the owning
// Deployment — ObjectEvents(Deployment) alone can never see it.
// eventsForScope's owned-pods fan-out (load.go) is what closes this gap.
func TestDeploymentScopedTimelineIncludesPodEvents(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-api-7d9f6c8b95", Namespace: "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: "checkout-api"}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: "checkout-api:1.9.0"}},
		}}},
	}
	pod := testPod("checkout-api-7d9f6c8b95-k2m9x", "node-a", 0)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "checkout-api-7d9f6c8b95"}}
	otherPod := testPod("other-app-abcde", "node-a", 0)

	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:        {pod, otherPod},
		kube.KindReplicaSet: {rs},
	}}
	events := []kube.Event{
		{Type: "Warning", Reason: "CreateContainerError", Object: "Pod/checkout-api-7d9f6c8b95-k2m9x", Namespace: "default", Message: "container create failed", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "BackOff", Object: "Pod/other-app-abcde", Namespace: "default", Message: "unrelated pod's own event", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{events: events}, Lister: lister,
		Namespace: "default", ObjectKind: kube.KindDeployment, ObjectName: "checkout-api",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	if !strings.Contains(view, "CreateContainerError") {
		t.Fatalf("expected the owned pod's CreateContainerError event in view:\n%s", view)
	}
	if strings.Contains(view, "unrelated pod's own event") {
		t.Fatalf("expected an unrelated pod's event to stay excluded:\n%s", view)
	}
}

// TestFluxCommitSubjectSurvivesTheEventThatCarriedIt pins §32a's
// session-scoped subject cache (docs/flux-plan.md T14). The two facts a
// revision row is built from have different lifetimes on the cluster: the
// Kustomization re-emits its reconcile event — carrying the revision
// annotation — every interval, so the row itself is effectively permanent,
// while source-controller's "stored artifact for commit" event fires once
// per commit and ages out at the ~1h Event TTL. Without retention the
// subject shows for about an hour and then vanishes from a row that stays on
// screen. This drives the exact sequence the cluster produces on its own:
// one load with the subject's event present, a second with only the
// reconcile event left.
func TestFluxCommitSubjectSurvivesTheEventThatCarriedIt(t *testing.T) {
	const (
		revision = "master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef"
		subject  = "openwebui bumped to 16.0.0"
	)
	repo := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "GitRepository",
		"metadata":   map[string]any{"name": "flux-system", "namespace": "default"},
		"status":     map[string]any{"artifact": map[string]any{"revision": revision}},
	}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("GitRepository"): {repo},
	}}
	reconcile := kube.Event{
		Type: "Normal", Reason: "ReconciliationSucceeded", Object: "Kustomization/nva-workers",
		Namespace: "default", Message: "Reconciliation finished in 1.40s, next run in 10m0s",
		Count: 1, LastSeen: time.Now(),
		Annotations: map[string]string{"kustomize.toolkit.fluxcd.io/revision": revision},
	}
	stored := kube.Event{
		Type: "Normal", Reason: "GitOperationSucceeded", Object: "GitRepository/flux-system",
		Namespace: "default", Message: "stored artifact for commit '" + subject + "'",
		Count: 1, LastSeen: time.Now(),
	}

	m := New(Config{
		Session: newSession(), Events: fakeEvents{events: []kube.Event{reconcile, stored}},
		Lister: lister, Namespace: "default",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if got, ok := revisionRow(m); !ok || got.CommitSubject != subject {
		t.Fatalf("first load: revision row = %+v (found %v), want CommitSubject %q", got, ok, subject)
	}

	// The subject's own event ages out; the reconcile event keeps coming.
	m.events = fakeEvents{events: []kube.Event{reconcile}}
	m = step(t, m, m.load()())

	got, ok := revisionRow(m)
	if !ok {
		t.Fatal("second load: expected the revision row to still be there")
	}
	if got.CommitSubject != subject {
		t.Fatalf("second load: CommitSubject = %q, want %q retained from the expired event", got.CommitSubject, subject)
	}
	// Matched on the quoted opening fragment, not the whole subject: the
	// WHAT column wraps, so the full string spans two physical lines.
	if view := plain(m.Render()); !strings.Contains(view, `"openwebui`) {
		t.Fatalf("expected the retained commit subject in view:\n%s", view)
	}

	// Control: a session that never saw the "stored artifact" event has
	// nothing to retain, so the same load renders the bare revision. Without
	// it this test would pass on a per-load map that simply never forgot.
	fresh := New(Config{
		Session: newSession(), Events: fakeEvents{events: []kube.Event{reconcile}},
		Lister: lister, Namespace: "default",
	})
	fresh.SetSize(120, 36)
	fresh = step(t, fresh, fresh.Init()())
	if got, ok := revisionRow(fresh); !ok || got.CommitSubject != "" {
		t.Fatalf("fresh session: revision row = %+v (found %v), want no commit subject", got, ok)
	}
}

// revisionRow returns the feed's first §32a revision entry.
func revisionRow(m Model) (kube.TimelineEntry, bool) {
	for _, e := range m.rows {
		if e.Kind == kube.TimelineRevision {
			return e, true
		}
	}
	return kube.TimelineEntry{}, false
}

func syncRow(m Model) (kube.TimelineEntry, bool) {
	for _, e := range m.rows {
		if e.Kind == kube.TimelineSync {
			return e, true
		}
	}
	return kube.TimelineEntry{}, false
}

// argoApplicationFixture builds an unstructured Application object carrying
// the real status.operationState shape (test/e2e/fixtures/55-argocd-
// objects.yaml's own kute-billing) — the on-cluster source
// argoSyncStatusOf reads since Argo's own OperationCompleted Event carries
// neither the revision nor the initiator.
func argoApplicationFixture(name, ref, sha, username string, automated bool) *unstructured.Unstructured {
	initiatedBy := map[string]any{}
	if username != "" {
		initiatedBy["username"] = username
	}
	if automated {
		initiatedBy["automated"] = true
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"status": map[string]any{
			"operationState": map[string]any{
				"operation":   map[string]any{"sync": map[string]any{"revision": ref}},
				"syncResult":  map[string]any{"revision": sha},
				"initiatedBy": initiatedBy,
			},
		},
	}}
}

// TestArgoSyncRowResolvesFromApplicationStatus pins §34a's core mechanism:
// the OperationCompleted Event carries no revision or initiator of its own,
// so both come from the Application object's own status.operationState.
func TestArgoSyncRowResolvesFromApplicationStatus(t *testing.T) {
	app := argoApplicationFixture("kute-billing", "main", "e41b90cdeadbeef0123456789abcdef012345678", "ci-bot", false)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {app},
	}}
	events := []kube.Event{
		{Type: "Normal", Reason: "OperationCompleted", Object: "Application/kute-billing", Namespace: "default", Message: "application synced successfully", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	got, ok := syncRow(m)
	if !ok {
		t.Fatalf("expected a sync row, rows: %+v", m.rows)
	}
	if got.GitRevision != "main@e41b90c" {
		t.Errorf("GitRevision = %q, want %q", got.GitRevision, "main@e41b90c")
	}
	if got.By != "ci-bot" {
		t.Errorf("By = %q, want %q", got.By, "ci-bot")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "Sync applied") || !strings.Contains(view, "main@e41b90c") || !strings.Contains(view, "ci-bot") {
		t.Fatalf("expected the sync row's text in view:\n%s", view)
	}
}

// OperationCompleted is a generic reason: another controller's event using
// it stays an ordinary row rather than vanishing into the Argo sync path,
// which drops anything that isn't an Application.
func TestNonArgoOperationCompletedEventStaysAPlainRow(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "OperationCompleted", Object: "Backup/nightly", ObjectAPIVersion: "velero.io/v1", Namespace: "default", Message: "backup finished", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "OperationCompleted", Object: "Application/other", ObjectAPIVersion: "app.example.io/v1", Namespace: "default", Message: "other app done", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: fakeLister{}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 2 {
		t.Fatalf("rows = %+v, want both OperationCompleted events as plain rows", m.rows)
	}
	for _, r := range m.rows {
		if r.Kind != kube.TimelineEvent {
			t.Fatalf("row %+v, want a plain event row", r)
		}
	}
}

// TestArgoSyncByFallsBackToAutoSync pins §34a's "was this a human or the
// machine?" distinction: an automated sync reads as "auto-sync", never
// fabricated as anyone's name.
func TestArgoSyncByFallsBackToAutoSync(t *testing.T) {
	app := argoApplicationFixture("kute-frontend", "main", "9d04c7ea0000000000000000000000000000000", "", true)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {app},
	}}
	events := []kube.Event{
		{Type: "Normal", Reason: "OperationCompleted", Object: "Application/kute-frontend", Namespace: "default", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	got, ok := syncRow(m)
	if !ok {
		t.Fatalf("expected a sync row, rows: %+v", m.rows)
	}
	if got.By != "auto-sync" {
		t.Errorf("By = %q, want %q", got.By, "auto-sync")
	}
}

// TestArgoSyncWithNoInitiatorCreditsNobody pins the other half: a manual
// sync that recorded no username — a bare operation patch from another
// tool — is not the machine, so it names nobody rather than
// "auto-sync".
func TestArgoSyncWithNoInitiatorCreditsNobody(t *testing.T) {
	app := argoApplicationFixture("kute-frontend", "main", "9d04c7ea0000000000000000000000000000000", "", false)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {app},
	}}
	events := []kube.Event{
		{Type: "Normal", Reason: "OperationCompleted", Object: "Application/kute-frontend", Namespace: "default", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	got, ok := syncRow(m)
	if !ok {
		t.Fatalf("expected a sync row, rows: %+v", m.rows)
	}
	if got.By != "" {
		t.Errorf("By = %q, want none", got.By)
	}
	if view := plain(m.Render()); strings.Contains(view, "auto-sync") {
		t.Fatalf("a sync with no initiator must not read as auto-sync:\n%s", view)
	}
}

// TestKuteStartedArgoSyncShowsItsInitiator: Argo copies the operation kute
// patched (kube.ArgoSyncPatch) into status.operationState, initiatedBy
// included, so a sync started from 33a's S reads as whoever pressed it.
func TestKuteStartedArgoSyncShowsItsInitiator(t *testing.T) {
	var patch struct {
		Operation map[string]any `json:"operation"`
	}
	if err := json.Unmarshal(kube.ArgoSyncPatch("main", "alice@example.com"), &patch); err != nil {
		t.Fatal(err)
	}
	app := argoApplicationFixture("kute-frontend", "main", "9d04c7ea0000000000000000000000000000000", "", false)
	state := app.Object["status"].(map[string]any)["operationState"].(map[string]any)
	state["operation"] = map[string]any{"sync": patch.Operation["sync"]}
	state["initiatedBy"] = patch.Operation["initiatedBy"]
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {app},
	}}
	events := []kube.Event{
		{Type: "Normal", Reason: "OperationCompleted", Object: "Application/kute-frontend", Namespace: "default", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	got, ok := syncRow(m)
	if !ok {
		t.Fatalf("expected a sync row, rows: %+v", m.rows)
	}
	if got.By != "alice@example.com" {
		t.Errorf("By = %q, want the kute user who synced", got.By)
	}
	if view := plain(m.Render()); !strings.Contains(view, "alice@example.com") {
		t.Fatalf("expected the initiator in the row:\n%s", view)
	}
}

// TestCopyRevisionOnArgoSyncRow pins that 'v' works on a §34a sync row, not
// just a §32a revision row.
func TestCopyRevisionOnArgoSyncRow(t *testing.T) {
	app := argoApplicationFixture("kute-billing", "main", "e41b90cdeadbeef0123456789abcdef012345678", "ci-bot", false)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.ResourceKind("Application"): {app},
	}}
	events := []kube.Event{
		{Type: "Normal", Reason: "OperationCompleted", Object: "Application/kute-billing", Namespace: "default", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if !m.revisionSelected() {
		t.Fatalf("expected the lone sync row to be selected and offer 'v': rows %+v", m.rows)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Text: "v"})
	if cmd == nil {
		t.Fatal("expected 'v' to return a clipboard command on a sync row")
	}
	if cmd() == nil {
		t.Fatal("expected a non-nil message from the clipboard command")
	}
}

// forbiddenLister extends fakeLister with the two health-check seams
// timeline's applyLoaded reads (tui.KindSyncChecker/KindForbiddenReporter) —
// mirrors overview_test.go's forbiddenKindLister, generalized with an
// independent unsynced-until-flipped kind so both the stall and denial paths
// can be exercised from the same fake.
type forbiddenLister struct {
	fakeLister
	unsynced kube.ResourceKind
	synced   bool
	denied   kube.ResourceKind
	err      error
}

func (f *forbiddenLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	if kind == f.unsynced && !f.synced {
		return false
	}
	return true
}

func (f *forbiddenLister) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == f.denied {
		return f.err
	}
	return nil
}

// TestPodCacheDeniedPromotesToPermissionDenied pins §5 of
// docs/lazy-informers.md §5.6: Pod backs the merged feed's
// restarts (isTimelineSource already treats Pod as a source), but the health
// checks never asked about it — a denied Pod cache used to render silently
// as a quiet timeline for an object mid-incident, made worse here by the
// Event feed alone already being non-empty.
func TestPodCacheDeniedPromotesToPermissionDenied(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "restarting", Count: 1, LastSeen: time.Now()},
	}
	lister := &forbiddenLister{
		denied: kube.KindPod,
		err:    fmt.Errorf("pods is forbidden: user cannot list"),
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStatePermissionDenied {
		t.Fatalf("state = %s, want permission-denied — a denied Pod cache must promote the whole screen even though the Event feed is non-empty", m.state)
	}
}

// TestEventDeniedIsVisibleEvenWithNonEmptyRail pins §5: timeline has no
// primary/secondary split (every merged kind IS the one answer it presents),
// so a denied Event cache must be visible even when the 16b revision rail
// still resolved from ReplicaSet/Pod alone.
func TestEventDeniedIsVisibleEvenWithNonEmptyRail(t *testing.T) {
	lister, podName := railFixture()
	forbidden := &forbiddenLister{
		fakeLister: lister,
		denied:     kube.KindEvent,
		err:        fmt.Errorf("events is forbidden: user cannot list"),
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{}, Lister: forbidden,
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: podName,
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStatePermissionDenied {
		t.Fatalf("state = %s, want permission-denied — a denied Event cache must be visible even though the revision rail resolved", m.state)
	}
}

// TestStalledCacheOnBackgroundReloadKeepsExistingFeedVisible pins the
// stall-vs-rerender rule: once the feed is already on screen, a background
// reload (e.g. a watch event) finding a required cache merely *stalled* —
// not yet re-synced, no error yet — must not discard the rendered feed back
// into a loading spinner. Only a confirmed denial may do that.
func TestStalledCacheOnBackgroundReloadKeepsExistingFeedVisible(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "restarting", Count: 1, LastSeen: time.Now()},
	}
	lister := &forbiddenLister{}
	m := New(Config{Session: newSession(), Events: fakeEvents{events: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready after the first load", m.state)
	}
	if len(m.rows) == 0 {
		t.Fatal("expected rows after the first load")
	}

	lister.unsynced = kube.KindReplicaSet
	m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindEvent})

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — an already-rendered feed must not be discarded by a background stall", m.state)
	}
	if len(m.rows) == 0 {
		t.Fatal("rows emptied by a background stall — the stall-vs-rerender regression")
	}
}

// TestEnterOnAFluxHelmReleaseRowOpensTheFluxObject is M10: a helm-controller
// event's row — §32a's ◆ revision row included — jumped by its bare
// "HelmRelease" Kind to §18a's Helm list instead of the Flux object.
func TestEnterOnAFluxHelmReleaseRowOpensTheFluxObject(t *testing.T) {
	fluxHR := kube.DiscoveredKind{Kind: "HelmRelease", Plural: "helmreleases", Group: kube.FluxGroupHelm,
		Versions: []kube.CRDVersion{{Name: "v2", Served: true, Storage: true}}, Established: true}
	sess := newSession()
	sess.Registry, sess.Groups = resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{fluxHR}, nil)
	events := []kube.Event{{
		Type: "Normal", Reason: "UpgradeSucceeded", Object: "HelmRelease/podinfo", Message: "upgraded",
		ObjectAPIVersion: kube.FluxGroupHelm + "/v2", Namespace: "default", Count: 1, LastSeen: time.Now(),
		Annotations: map[string]string{"helm.toolkit.fluxcd.io/revision": "6.5.4"},
	}}
	m := New(Config{Session: sess, Events: fakeEvents{events: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) == 0 {
		t.Fatal("no timeline rows")
	}
	for i := range m.rows {
		m.selected = i
		cmd, ok := m.openSelectedObject()
		if !ok {
			t.Fatalf("row %d: ↵ dispatched nothing", i)
		}
		if msg, _ := cmd().(tui.GotoResourceMsg); msg.Kind != kube.KindFluxHelmRelease {
			t.Errorf("row %d (%v): ↵ sent %#v, want the Flux HelmRelease", i, m.rows[i].Kind, cmd())
		}
	}
}

// A Deployment change moves rail[0]'s live status and change-cause, and an
// Argo Application or Flux source change moves sync attribution and
// revision pairing — each must reload the feed, not wait for an unrelated
// Pod or Event change.
func TestDeploymentArgoAndFluxChangesReloadTheTimeline(t *testing.T) {
	argo := kube.DiscoveredKind{Kind: "Application", Plural: "applications", Group: kube.ArgoGroup,
		Versions: []kube.CRDVersion{{Name: "v1alpha1", Served: true, Storage: true}}, Established: true}
	git := kube.DiscoveredKind{Kind: "GitRepository", Plural: "gitrepositories", Group: kube.FluxGroupSource,
		Versions: []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}}, Established: true}
	sess := newSession()
	sess.Registry, sess.Groups = resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{argo, git}, nil)
	m := New(Config{Session: sess, Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	for _, kind := range []kube.ResourceKind{kube.KindDeployment, "Application", "GitRepository"} {
		before := m.loadEpoch
		updated, cmd := m.Update(kube.ResourceChangedMsg{Kind: kind})
		m = *updated.(*Model)
		if cmd == nil || m.loadEpoch == before {
			t.Fatalf("a %s change didn't reload the timeline", kind)
		}
	}
	if _, cmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindConfigMap}); cmd != nil {
		t.Fatal("an unrelated ConfigMap change must not reload the timeline")
	}
}
