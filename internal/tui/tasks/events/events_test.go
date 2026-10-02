package events

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

func plain(s string) string { return ansi.Strip(s) }

// fakeEvents is a minimal EventsReader test double.
type fakeEvents struct {
	namespaceEvents []kube.Event
	objectEvents    []kube.Event
	err             error
}

func (f fakeEvents) NamespaceEvents(context.Context, string) ([]kube.Event, error) {
	return f.namespaceEvents, f.err
}

func (f fakeEvents) ObjectEvents(context.Context, string, kube.ResourceKind, string) ([]kube.Event, error) {
	return f.objectEvents, f.err
}

// nsAwareEvents is a namespace-aware EventsReader test double, for tests
// that need to tell one namespace's reply apart from another's.
type nsAwareEvents struct {
	byNamespace map[string][]kube.Event
}

func (f *nsAwareEvents) NamespaceEvents(_ context.Context, namespace string) ([]kube.Event, error) {
	return f.byNamespace[namespace], nil
}

func (f *nsAwareEvents) ObjectEvents(context.Context, string, kube.ResourceKind, string) ([]kube.Event, error) {
	return nil, nil
}

func newSession() *tui.Session {
	return &tui.Session{
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "default"},
		Theme:    tui.Dark(),
	}
}

// step applies one message and returns the updated Model, draining a
// tea.BatchMsg fan-out like browse's own test helper does. A
// tea.Sequence-returning Cmd (openSelectedObject's ↵ navigation) yields an
// unexported bubbletea type this package can't type-switch on; passing it
// straight to Update is harmless (no case matches, so it's a no-op) — the
// real *tea.Program is what actually drains a sequence in production.
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

func TestNamespaceScopedLoadDedupesAndFoldsNormal(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "back-off restarting", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "back-off restarting", Count: 1, LastSeen: time.Now()},
		{Type: "Normal", Reason: "Pulled", Object: "Pod/worker-0", Message: "pulled image", Count: 1, LastSeen: time.Now()},
		{Type: "Normal", Reason: "Created", Object: "Pod/worker-0", Message: "created container", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	// Two warning events dedupe into one group; two normal events fold into
	// one summary row — three display rows total.
	if len(m.rows) != 2 {
		t.Fatalf("rows = %d, want 2 (1 warning group + 1 folded normal row)", len(m.rows))
	}
	if m.rows[0].kind != rowGroup || m.rows[0].group.Reason != "BackOff" {
		t.Fatalf("rows[0] = %+v, want the deduped BackOff warning", m.rows[0])
	}
	if m.rows[1].kind != rowFolded || len(m.rows[1].folded) != 2 {
		t.Fatalf("rows[1] = %+v, want a folded row with 2 normal groups", m.rows[1])
	}

	view := plain(m.Render())
	// docs/design README.md §9b's grid puts the "×" in the column header
	// (columnHeaderLines) and the bare count in the cell (renderGroupRow).
	if !strings.Contains(view, "BackOff") || !strings.Contains(view, "×") || !strings.Contains(view, "2") {
		t.Fatalf("expected deduped BackOff with a × count column in view:\n%s", view)
	}
	// docs/design README.md §9b's mockup renders "normal" and "N events —
	// reasons" as separate spans with a plain space between (no "·"), unlike
	// the summary strip's own "N warnings · N normal" separator.
	if !strings.Contains(view, "normal") || !strings.Contains(view, "2 events") {
		t.Fatalf("expected folded normal summary line in view:\n%s", view)
	}
}

func TestDistinctFailedMessagesRemainVisible(t *testing.T) {
	now := time.Now()
	detailed := `Failed to pull image "registry/software-escrow:1.0.0": rpc error: no match for platform in manifest: not found`
	events := []kube.Event{
		{Type: "Warning", Reason: "Failed", Object: "Pod/software-escrow", Message: detailed, Count: 5, LastSeen: now.Add(-time.Minute)},
		{Type: "Warning", Reason: "Failed", Object: "Pod/software-escrow", Message: "Error: ErrImagePull", Count: 5, LastSeen: now.Add(-30 * time.Second)},
		{Type: "Warning", Reason: "Failed", Object: "Pod/software-escrow", Message: "Error: ImagePullBackOff", Count: 15, LastSeen: now},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{objectEvents: events}, Namespace: "default", ObjectKind: kube.KindPod, ObjectName: "software-escrow"})
	m.SetSize(160, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 3 {
		t.Fatalf("rows = %d, want all three distinct Failed messages", len(m.rows))
	}
	view := plain(m.Render())
	for _, want := range []string{"no match for platform in manifest", "Error: ErrImagePull", "Error: ImagePullBackOff"} {
		if !strings.Contains(view, want) {
			t.Fatalf("events view missing %q:\n%s", want, view)
		}
	}
}

// TestAllNamespacesKeepsCrossNamespaceObjectsSeparateAndLabelsNamespace is
// 9b's all-namespaces mode (browse's 'e' with Namespace == "", mirroring
// 6b): two different namespaces each have their own "cache-0" pod hitting
// the same FailedScheduling reason — one is actively failing
// (CrashLoopBackOff), the other merely pending. Regression coverage for two
// bugs a bare-name key would cause: the rows wrongly folding into one, and
// the healthy namespace's warning wrongly rendering red because a
// same-named pod elsewhere is crashlooping.
func TestAllNamespacesKeepsCrossNamespaceObjectsSeparateAndLabelsNamespace(t *testing.T) {
	failingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "cache-0", Namespace: "shop-checkout"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "cache", RestartCount: 9,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
	healthyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "cache-0", Namespace: "shop-payments"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "cache", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}},
		},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {failingPod, healthyPod},
	}}

	events := []kube.Event{
		{Type: "Warning", Reason: "FailedScheduling", Object: "Pod/cache-0", Namespace: "shop-checkout", Message: "0/5 nodes available", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "FailedScheduling", Object: "Pod/cache-0", Namespace: "shop-payments", Message: "0/5 nodes available", Count: 1, LastSeen: time.Now()},
	}
	sess := newSession()
	sess.Registry = resources.DefaultRegistry()
	m := New(Config{
		Session:   sess,
		Events:    fakeEvents{namespaceEvents: events},
		Lister:    lister,
		Namespace: "", // all-namespaces
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 2 {
		t.Fatalf("rows = %d, want 2 (same reason+object, different namespaces must stay separate), rows=%+v", len(m.rows), m.rows)
	}
	if !m.failing["shop-checkout/cache-0"] {
		t.Fatalf("expected shop-checkout/cache-0 marked failing, failing=%+v", m.failing)
	}
	if m.failing["shop-payments/cache-0"] {
		t.Fatalf("shop-payments/cache-0 wrongly marked failing off a same-named pod in another namespace, failing=%+v", m.failing)
	}

	// The REASON·OBJECT column is a fixed 20 chars wide, so
	// "shop-checkout/Pod/cache-0" ellipsizes — check the namespace-prefixed
	// start survives, not the untruncated string.
	view := plain(m.Render())
	if !strings.Contains(view, "shop-checkout/Pod") {
		t.Fatalf("expected the OBJECT line namespace-prefixed for all-namespaces mode:\n%s", view)
	}
	if !strings.Contains(view, "shop-payments/Pod") {
		t.Fatalf("expected the OBJECT line namespace-prefixed for all-namespaces mode:\n%s", view)
	}
}

// TestAKeySwitchesToAllNamespaces covers 9b's "a" key (verbs.AllNamespaces,
// browse's own idiom) re-scoping the screen to all namespaces and
// re-fetching.
func TestAKeySwitchesToAllNamespaces(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Namespace: "shop-checkout", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "shop-checkout"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if !strings.Contains(plain(m.Render()), "shop-checkout") {
		t.Fatalf("expected the namespace-scoped breadcrumb before 'a':\n%s", plain(m.Render()))
	}

	m = step(t, m, tea.KeyPressMsg{Text: "a"})

	if m.namespace != "" {
		t.Fatalf("namespace = %q, want \"\" (all namespaces) after 'a'", m.namespace)
	}
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready after the reload settles", m.state)
	}
	if !strings.Contains(plain(m.Render()), "all namespaces") {
		t.Fatalf("expected the all-namespaces breadcrumb after 'a':\n%s", plain(m.Render()))
	}
}

// TestObjectScopedIgnoresAKey covers the flip side: 9b's object-scoped mode
// (poddetail's/nodedetail's "e") has no namespace to switch, so "a" must be
// a no-op — mirroring poddetail/nodedetail themselves never reacting to a
// namespace switch either.
func TestObjectScopedIgnoresAKey(t *testing.T) {
	objectEvents := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Namespace: "default", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{objectEvents: objectEvents},
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: "worker-0",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "a"})

	if m.namespace != "default" {
		t.Fatalf("namespace = %q, want unchanged \"default\" — 'a' should be a no-op in object-scoped mode", m.namespace)
	}
}

// TestSwitchNamespaceMsgReloadsEvents is the namespace palette's half of the
// same wiring: the root shell's "n" key opens the one namespace palette over
// whatever Screen is active (9b included, since it already satisfies
// Screen/CapturingInput) and forwards tui.SwitchNamespaceMsg on Enter.
// Before this fix, 9b silently ignored that message — selecting a namespace
// from the palette looked like it did nothing.
func TestSwitchNamespaceMsgReloadsEvents(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Namespace: "shop-checkout", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "shop-checkout"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "shop-payments"})

	if m.namespace != "shop-payments" {
		t.Fatalf("namespace = %q, want shop-payments after tui.SwitchNamespaceMsg", m.namespace)
	}
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready after the reload settles", m.state)
	}
	if !strings.Contains(plain(m.Render()), "shop-payments") {
		t.Fatalf("expected the shop-payments breadcrumb after switching:\n%s", plain(m.Render()))
	}
}

// TestSwitchNamespaceMsgNoopInObjectScopedMode: the palette can still be
// opened while looking at object-scoped events (it's a root-shell overlay,
// not gated per-screen), but there's no namespace to switch to — the screen
// is pinned to one object.
func TestSwitchNamespaceMsgNoopInObjectScopedMode(t *testing.T) {
	objectEvents := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Namespace: "default", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{objectEvents: objectEvents},
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: "worker-0",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "shop-payments"})

	if m.namespace != "default" {
		t.Fatalf("namespace = %q, want unchanged \"default\" in object-scoped mode", m.namespace)
	}
}

func TestTabExpandsAndCollapsesFoldedNormalRow(t *testing.T) {
	events := []kube.Event{
		{Type: "Normal", Reason: "Pulled", Object: "Pod/worker-0", Message: "pulled image", Count: 1, LastSeen: time.Now()},
		{Type: "Normal", Reason: "Created", Object: "Pod/worker-0", Message: "created container", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 1 || m.rows[0].kind != rowFolded {
		t.Fatalf("expected one folded row before expand, got %+v", m.rows)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if len(m.rows) != 2 {
		t.Fatalf("expected 2 expanded rows after tab, got %d", len(m.rows))
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if len(m.rows) != 1 || m.rows[0].kind != rowFolded {
		t.Fatalf("expected folded again after second tab, got %+v", m.rows)
	}
}

func TestWKeyTogglesWarningsOnly(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
		{Type: "Normal", Reason: "Pulled", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.rows) != 2 {
		t.Fatalf("expected warning + folded-normal rows, got %d", len(m.rows))
	}
	m = step(t, m, tea.KeyPressMsg{Text: "w"})
	if len(m.rows) != 1 || m.rows[0].kind != rowGroup {
		t.Fatalf("expected only the warning row once warnings-only is on, got %+v", m.rows)
	}
}

func TestTimeWindowFiltersOldEvents(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "Recent", Object: "Pod/a", Message: "m", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "Ancient", Object: "Pod/b", Message: "m", Count: 1, LastSeen: time.Now().Add(-48 * time.Hour)},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()()) // default window is 1h

	if len(m.rows) != 1 || m.rows[0].group.Reason != "Recent" {
		t.Fatalf("expected only the recent event within the default 1h window, got %+v", m.rows)
	}

	// Cycle t: 15m -> 1h -> 6h -> 24h -> all. Starting at 1h, 3 more presses
	// reaches "all".
	for range 3 {
		m = step(t, m, tea.KeyPressMsg{Text: "t"})
	}
	if len(m.rows) != 2 {
		t.Fatalf("expected both events once the window is 'all', got %d", len(m.rows))
	}
}

func TestFilterQueryNarrowsRows(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "restarting", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "FailedScheduling", Object: "Pod/cache-0", Message: "insufficient cpu", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	if !m.filterActive {
		t.Fatal("expected / to activate the filter")
	}
	for _, r := range "worker" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	if len(m.rows) != 1 || m.rows[0].group.Reason != "BackOff" {
		t.Fatalf("expected filter to narrow to the worker-0 event, got %+v", m.rows)
	}
	// docs/design system-wide interactions: "items never silently
	// disappear" — the strip must say a row was hidden by the filter, not
	// just show a bare matched count.
	view := plain(m.Render())
	if !strings.Contains(view, "hidden by filter") {
		t.Fatalf("expected the 'hidden by filter' notice:\n%s", view)
	}
}

func TestFilterCtrlJKMovesSelectionWithoutTyping(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "restarting", Count: 1, LastSeen: time.Now()},
		{Type: "Warning", Reason: "FailedScheduling", Object: "Pod/cache-0", Message: "insufficient cpu", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	if m.selected != 0 {
		t.Fatalf("selected = %d, want 0 before moving", m.selected)
	}

	m = step(t, m, tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	if m.selected != 1 {
		t.Fatalf("selected = %d, want 1 after ctrl+j", m.selected)
	}
	if m.filterInput.Value() != "" {
		t.Fatalf("filterQuery = %q, want empty (ctrl+j must move, not type)", m.filterInput.Value())
	}

	m = step(t, m, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	if m.selected != 0 {
		t.Fatalf("selected = %d, want 0 after ctrl+k", m.selected)
	}
	if m.filterInput.Value() != "" {
		t.Fatalf("filterQuery = %q, want empty (ctrl+k must move, not type)", m.filterInput.Value())
	}
}

func TestObjectScopedLoadUsesObjectEvents(t *testing.T) {
	objectEvents := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{namespaceEvents: nil, objectEvents: objectEvents},
		Namespace: "default", ObjectKind: kube.KindPod, ObjectName: "worker-0",
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady || len(m.rows) != 1 {
		t.Fatalf("expected object-scoped events to load, state=%s rows=%d", m.state, len(m.rows))
	}
	view := plain(m.Render())
	if !strings.Contains(view, "Pod/worker-0") {
		t.Fatalf("expected the object-scoped breadcrumb, got:\n%s", view)
	}
}

func TestEmptyStateWhenNoEvents(t *testing.T) {
	m := New(Config{Session: newSession(), Events: fakeEvents{}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateEmpty {
		t.Fatalf("state = %s, want empty", m.state)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "no events in default") {
		t.Fatalf("expected the empty-state message, got:\n%s", view)
	}
}

func TestEnterOnGroupRowReturnsNavigationCmd(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	cmd, ok := m.openSelectedObject()
	if !ok || cmd == nil {
		t.Fatal("expected ↵ on a real event group to produce a navigation command")
	}
}

func TestEnterOnFoldedRowIsNoop(t *testing.T) {
	events := []kube.Event{
		{Type: "Normal", Reason: "Pulled", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
		{Type: "Normal", Reason: "Created", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if _, ok := m.openSelectedObject(); ok {
		t.Fatal("expected ↵ on the folded summary row to be a no-op")
	}
}

// stubYAMLTask is a minimal tea.Model standing in for tasks/yamlview,
// mirroring browse's own stubTask pattern for OpenYAML-style seams.
type stubYAMLTask struct{}

func (stubYAMLTask) Init() tea.Cmd                       { return nil }
func (stubYAMLTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return stubYAMLTask{}, nil }
func (stubYAMLTask) View() tea.View                      { return tea.NewView("") }

// TestYKeyOnGroupRowOpensYAML covers the system-wide "y opens the YAML view
// on any selected object, any kind" interaction (already implemented by
// browse/poddetail/nodedetail/whocan/objectdetail) applied to 9b: 'y' on a
// real event row resolves the same kind/namespace/name openSelectedObject's
// ↵ does and pushes it through OpenYAML.
func TestYKeyOnGroupRowOpensYAML(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Namespace: "nva-stage", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	var gotKind kube.ResourceKind
	var gotNamespace, gotName string
	m := New(Config{
		Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "nva-stage",
		OpenYAML: func(kind kube.ResourceKind, namespace, name string, w, h int) (tea.Model, tea.Cmd) {
			gotKind, gotNamespace, gotName = kind, namespace, name
			return stubYAMLTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "y"})
	if gotKind != kube.KindPod || gotNamespace != "nva-stage" || gotName != "worker-0" {
		t.Fatalf("openYAML called with (%q, %q, %q), want (Pod, nva-stage, worker-0)", gotKind, gotNamespace, gotName)
	}
	if _, ok := updated.(stubYAMLTask); !ok {
		t.Fatalf("expected Update to return the pushed stub task, got %T", updated)
	}
}

// TestYKeyOnFoldedRowIsNoop mirrors TestEnterOnFoldedRowIsNoop: 'y' has no
// single object to resolve on the folded normal-events summary row.
func TestYKeyOnFoldedRowIsNoop(t *testing.T) {
	events := []kube.Event{
		{Type: "Normal", Reason: "Pulled", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
		{Type: "Normal", Reason: "Created", Object: "Pod/worker-0", Message: "m", Count: 1, LastSeen: time.Now()},
	}
	m := New(Config{
		Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Namespace: "default",
		OpenYAML: func(kind kube.ResourceKind, namespace, name string, w, h int) (tea.Model, tea.Cmd) {
			return stubYAMLTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if _, _, ok := m.openSelectedYAML(); ok {
		t.Fatal("expected 'y' on the folded summary row to be a no-op")
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

// TestStaleNamespaceLoadDoesNotOverwriteNewNamespace pins the fix for the
// same race browse's own version of this test covers: a slow load() from a
// namespace the user has since switched away from must not land and
// overwrite the feed now showing under a different namespace.
func TestStaleNamespaceLoadDoesNotOverwriteNewNamespace(t *testing.T) {
	events := &nsAwareEvents{byNamespace: map[string][]kube.Event{
		"default":   {{Type: "Warning", Reason: "BackOff", Object: "Pod/api-1", Message: "m", Count: 1, LastSeen: time.Now()}},
		"nva-stage": {{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-1", Message: "m", Count: 1, LastSeen: time.Now()}},
	}}
	m := New(Config{Session: newSession(), Events: events, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if m.namespace != "default" {
		t.Fatalf("namespace = %q, want default", m.namespace)
	}

	// Capture "default"'s in-flight load before it resolves.
	staleCmd := m.load()

	// Switch away before that load lands — this issues its own fresh load
	// and bumps loadEpoch.
	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "nva-stage"})
	if m.namespace != "nva-stage" {
		t.Fatalf("namespace = %q, want nva-stage", m.namespace)
	}

	staleMsg, ok := staleCmd().(loadedMsg)
	if !ok {
		t.Fatalf("expected loadedMsg from the stale command")
	}
	updated, cmd := m.applyLoaded(staleMsg)
	m = *updated.(*Model)
	if cmd != nil {
		t.Fatal("a stale reply must not schedule anything either")
	}

	if m.namespace != "nva-stage" {
		t.Fatalf("namespace = %q, want nva-stage — stale reply must not touch it", m.namespace)
	}
	for _, g := range m.groups {
		if g.Object == "Pod/api-1" {
			t.Fatalf("stale default-namespace event leaked into nva-stage's groups: %+v", m.groups)
		}
	}
}

// forbiddenLister extends golden_test.go's fakeLister with the KindForbidden
// health-check seam applyLoaded reads (tui.KindForbiddenReporter) — mirrors
// timeline/whocan's own forbidden fakes for the same
// docs/lazy-informers.md §5.6 symmetry check.
type forbiddenLister struct {
	fakeLister
	denied kube.ResourceKind
	err    error
}

func (f *forbiddenLister) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == f.denied {
		return f.err
	}
	return nil
}

// TestEventCacheDeniedIsVisibleEvenWithGroupsAlreadyRendered tests a
// symmetry case for the KindsError check. timeline and whocan needed
// restructuring to run their KindsError checks unconditionally. events is
// single-kind, so applyLoaded's KindsError check was already unconditional.
// This test confirms that a denial discovered on a background reload still
// wins, even when a group is already rendered. It also guards against a
// future refactor that accidentally makes the check conditional.
func TestEventCacheDeniedIsVisibleEvenWithGroupsAlreadyRendered(t *testing.T) {
	events := []kube.Event{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/worker-0", Namespace: "default", Message: "restarting", Count: 1, LastSeen: time.Now()},
	}
	lister := &forbiddenLister{}
	m := New(Config{Session: newSession(), Events: fakeEvents{namespaceEvents: events}, Lister: lister, Namespace: "default"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready after the first load", m.state)
	}
	if len(m.groups) == 0 {
		t.Fatal("expected groups after the first load")
	}

	lister.denied = kube.KindEvent
	lister.err = errors.New("events is forbidden: user cannot list")
	m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindEvent})

	if m.state != tui.TaskStatePermissionDenied {
		t.Fatalf("state = %s, want permission-denied — a denial found on a background reload must win even over groups already on screen", m.state)
	}
}

// TestEnterOnAFluxHelmReleaseEventOpensTheFluxObject is M10: ↵ built the
// jump from the event's bare "HelmRelease/x", which is §18a's Helm kind, so
// every helm-controller event landed on the Helm releases list where the
// Flux object isn't listed. The involved object's apiVersion resolves it.
func TestEnterOnAFluxHelmReleaseEventOpensTheFluxObject(t *testing.T) {
	fluxHR := kube.DiscoveredKind{Kind: "HelmRelease", Plural: "helmreleases", Group: kube.FluxGroupHelm,
		Versions: []kube.CRDVersion{{Name: "v2", Served: true, Storage: true}}, Established: true}
	sess := newSession()
	sess.Registry, sess.Groups = resources.BuildDiscoveredRegistry([]kube.DiscoveredKind{fluxHR}, nil)

	for _, tc := range []struct {
		apiVersion string
		want       kube.ResourceKind
	}{
		{kube.FluxGroupHelm + "/v2", kube.KindFluxHelmRelease},
		{"", kube.KindHelmRelease}, // nothing to resolve with: the bare Kind
	} {
		events := []kube.Event{{Type: "Warning", Reason: "InstallFailed", Object: "HelmRelease/podinfo",
			ObjectAPIVersion: tc.apiVersion, Namespace: "apps", Count: 1, LastSeen: time.Now()}}
		m := New(Config{Session: sess, Events: fakeEvents{namespaceEvents: events}, Namespace: "apps"})
		m.SetSize(120, 36)
		m = step(t, m, m.Init()())
		cmd, ok := m.openSelectedObject()
		if !ok {
			t.Fatalf("%q: ↵ dispatched nothing", tc.apiVersion)
		}
		if msg, _ := cmd().(tui.GotoResourceMsg); msg.Kind != tc.want || msg.Name != "podinfo" {
			t.Errorf("%q: ↵ sent %#v, want kind %s", tc.apiVersion, cmd(), tc.want)
		}
	}
}
