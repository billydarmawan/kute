package yamlview

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

func plain(s string) string { return ansi.Strip(s) }

type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objs[kind], nil
}

type fakeYAML struct {
	text            string
	resourceVersion string
	err             error
}

func (f fakeYAML) GetYAML(context.Context, kube.ResourceKind, string, string) (string, string, error) {
	return f.text, f.resourceVersion, f.err
}

func newSession() *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster"}}
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

func testPod(name, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}, {Manager: "kubelet"}},
		},
	}
}

func newModel(text string) (Model, *fakeLister) {
	lister := &fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {testPod("worker-0", "default")},
	}}
	m := New(Config{
		Session: newSession(), Lister: lister,
		YAML:      fakeYAML{text: text, resourceVersion: "12345"},
		Kind:      kube.KindPod,
		Namespace: "default", Name: "worker-0",
	})
	m.SetSize(120, 40)
	return m, lister
}

func TestLoadRendersResourceVersionAndFoldedStatusSummary(t *testing.T) {
	m, _ := newModel(fixtureYAML)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if !m.folded["status"] {
		t.Fatal("expected status to start folded")
	}

	view := plain(m.Render())
	for _, want := range []string{"12345", "lines folded", "apiVersion", "worker"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "phase: Running") {
		t.Fatalf("expected the folded status block's content hidden:\n%s", view)
	}
}

func TestManagedFieldsStartsFoldedAndUnfoldsToRealContent(t *testing.T) {
	m, _ := newModel(fixtureYAML)
	m = step(t, m, m.Init()())

	if len(m.managedFieldsLines) != 2 {
		t.Fatalf("managedFieldsLines = %+v, want 2 entries (from the test pod's ManagedFields)", m.managedFieldsLines)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "managedFields (2 lines folded)") {
		t.Fatalf("expected the managedFields fold summary line:\n%s", view)
	}
	if strings.Contains(view, "manager: kubectl") {
		t.Fatalf("expected managedFields content hidden while folded:\n%s", view)
	}

	// 'f' unfolds everything, including managedFields — docs/design
	// README.md §8a: "f show all".
	m = step(t, m, tea.KeyPressMsg{Text: "f"})
	view = plain(m.Render())
	if !strings.Contains(view, "manager: kubectl") || !strings.Contains(view, "manager: kubelet") {
		t.Fatalf("expected real managedFields content visible after 'f':\n%s", view)
	}

	// Tab on the expanded header re-folds it.
	rendered := m.rendered()
	for i, rl := range rendered {
		if rl.FoldableKey == "managedFields" {
			m.cursor = i
		}
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if !m.folded["managedFields"] {
		t.Fatal("expected tab on the expanded managedFields header to re-fold it")
	}
	if strings.Contains(plain(m.Render()), "manager: kubectl") {
		t.Fatal("expected managedFields content hidden again after re-folding")
	}
}

func TestTabTogglesFoldAtCursor(t *testing.T) {
	m, _ := newModel(fixtureYAML)
	m = step(t, m, m.Init()())

	// Move the cursor onto the "status:" fold-summary line.
	rendered := m.rendered()
	for i, rl := range rendered {
		if rl.FoldKey == "status" {
			m.cursor = i
		}
	}

	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if m.folded["status"] {
		t.Fatal("expected tab on the fold-summary line to unfold status")
	}
	if !strings.Contains(plain(m.Render()), "phase: Running") {
		t.Fatalf("expected status content visible after unfolding:\n%s", plain(m.Render()))
	}
}

func TestFKeyUnfoldsEverything(t *testing.T) {
	m, _ := newModel(fixtureYAML)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "f"})
	if m.folded["status"] {
		t.Fatal("expected 'f' to unfold status")
	}
}

func TestSearchJumpsToMatchAndWraps(t *testing.T) {
	m, _ := newModel(fixtureYAML)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	if !m.CapturingInput() {
		t.Fatal("expected search mode to capture input")
	}
	for _, r := range "worker" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	rendered := m.rendered()
	if !strings.Contains(strings.ToLower(rendered[m.cursor].Text), "worker") {
		t.Fatalf("expected cursor on a line containing 'worker', got %q", rendered[m.cursor].Text)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.CapturingInput() {
		t.Fatal("expected esc to close search mode")
	}
}

func TestYCopiesFullText(t *testing.T) {
	m, _ := newModel(fixtureYAML)
	m = step(t, m, m.Init()())

	_, cmd := m.Update(tea.KeyPressMsg{Text: "Y"})
	if cmd == nil {
		t.Fatal("expected Y to return a clipboard command")
	}
	// tea.SetClipboard's returned Cmd sends an OSC52 escape sequence as a
	// tea.Msg for the runtime to write — just confirm it doesn't panic and
	// returns a non-nil message.
	if cmd() == nil {
		t.Fatal("expected a non-nil message from the clipboard command")
	}
}

func TestLiveReloadPreservesCursorIndex(t *testing.T) {
	m, lister := newModel(fixtureYAML)
	m = step(t, m, m.Init()())
	m.cursor = 3
	m.offset = 0

	// Simulate a resourceVersion bump on the next fetch.
	lister.objs[kube.KindPod] = []runtime.Object{testPod("worker-0", "default")}
	m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindPod})

	if m.cursor != 3 {
		t.Fatalf("expected cursor to stay at 3 across a reload, got %d", m.cursor)
	}
}

func TestNotFoundIsAnError(t *testing.T) {
	lister := &fakeLister{objs: map[kube.ResourceKind][]runtime.Object{}}
	m := New(Config{
		Session: newSession(), Lister: lister,
		YAML: fakeYAML{text: fixtureYAML}, Kind: kube.KindPod,
		Namespace: "default", Name: "ghost",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateError {
		t.Fatalf("state = %s, want error", m.state)
	}
}

func TestEscSendsBackMsg(t *testing.T) {
	m := New(Config{Session: newSession(), Kind: kube.KindPod, Namespace: "default", Name: "worker-0"})
	_, cmd := m.Update(tea.KeyPressMsg{Text: "esc"})
	if cmd == nil {
		t.Fatal("expected esc to return a command")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg, got %T", cmd())
	}
}

// fakeManagedFields stands in for the live Get 8a does now that informer
// caches strip managedFields.
type fakeManagedFields struct {
	yaml   string
	err    error
	calls  int
	lastNS string
}

func (f *fakeManagedFields) GetManagedFields(_ context.Context, _ kube.ResourceKind, ns, _ string) (string, error) {
	f.calls++
	f.lastNS = ns
	return f.yaml, f.err
}

// TestManagedFieldsComeFromTheLiveReader: caches strip managedFields, so the
// fold's content has to be fetched for the object on screen rather than read
// back off a cached object that no longer carries it.
func TestManagedFieldsComeFromTheLiveReader(t *testing.T) {
	// A cached object with no managedFields — exactly what the transform
	// leaves behind.
	lister := &fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "default"}}},
	}}
	managed := &fakeManagedFields{yaml: "- manager: kubectl\n  operation: Apply\n"}
	m := New(Config{
		Session: newSession(), Lister: lister,
		YAML:          fakeYAML{text: fixtureYAML, resourceVersion: "12345"},
		ManagedFields: managed,
		Kind:          kube.KindPod,
		Namespace:     "default", Name: "worker-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if managed.calls != 1 {
		t.Fatalf("GetManagedFields called %d times, want exactly 1", managed.calls)
	}
	if managed.lastNS != "default" {
		t.Fatalf("fetched managedFields for namespace %q, want default", managed.lastNS)
	}
	if len(m.managedFieldsLines) == 0 {
		t.Fatal("expected the fetched managedFields to populate the fold")
	}
	if !strings.Contains(plain(m.Render()), "managedFields") {
		t.Fatalf("expected the managedFields fold to render:\n%s", plain(m.Render()))
	}
}

// TestManagedFieldsFetchFailureStillRendersTheObject: managedFields are
// supplementary. A failed fetch costs the fold, not the whole screen.
func TestManagedFieldsFetchFailureStillRendersTheObject(t *testing.T) {
	lister := &fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {testPod("worker-0", "default")},
	}}
	managed := &fakeManagedFields{err: errors.New("forbidden")}
	m := New(Config{
		Session: newSession(), Lister: lister,
		YAML:          fakeYAML{text: fixtureYAML, resourceVersion: "12345"},
		ManagedFields: managed,
		Kind:          kube.KindPod,
		Namespace:     "default", Name: "worker-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — a managedFields failure must not fail the view (%q)", m.state, m.feedback)
	}
	if len(m.managedFieldsLines) != 0 {
		t.Fatal("expected no managedFields content after a failed fetch")
	}
}

// TestManagedFieldsFallBackToTheCachedObject covers --demo and any seam that
// doesn't provide a live reader: nothing strips anything there, so the
// cached object still carries them.
func TestManagedFieldsFallBackToTheCachedObject(t *testing.T) {
	m, _ := newModel(fixtureYAML)
	m = step(t, m, m.Init()())

	if len(m.managedFieldsLines) == 0 {
		t.Fatal("expected managedFields read back from the cached object when no live reader is wired")
	}
}

// mutableYAML is a YAMLReader whose text/resourceVersion a test can move.
type mutableYAML struct {
	text, rv string
}

func (f *mutableYAML) GetYAML(context.Context, kube.ResourceKind, string, string) (string, string, error) {
	return f.text, f.rv, nil
}

// TestChangeEventsRefetchManagedFieldsOnlyWhenTheObjectMoved: every change
// event for the kind — any object of it — reloads this screen, so the live
// managedFields GET must be skipped while this object's resourceVersion
// hasn't moved, or a busy cluster turns into a stream of GETs.
func TestChangeEventsRefetchManagedFieldsOnlyWhenTheObjectMoved(t *testing.T) {
	lister := &fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {testPod("worker-0", "default")},
	}}
	yaml := &mutableYAML{text: fixtureYAML, rv: "1"}
	managed := &fakeManagedFields{yaml: "- manager: kubectl\n  operation: Apply\n"}
	m := New(Config{
		Session: newSession(), Lister: lister, YAML: yaml, ManagedFields: managed,
		Kind: kube.KindPod, Namespace: "default", Name: "worker-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	for range 5 {
		m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindPod})
	}
	if managed.calls != 1 {
		t.Fatalf("GetManagedFields called %d times across unrelated change events, want 1", managed.calls)
	}
	if len(m.managedFieldsLines) == 0 {
		t.Fatal("a skipped refetch dropped the managedFields already on screen")
	}

	yaml.rv = "2"
	m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindPod})
	if managed.calls != 2 || m.resourceVersion != "2" {
		t.Fatalf("after a resourceVersion bump: calls = %d, rv = %q; want 2, \"2\"", managed.calls, m.resourceVersion)
	}
}

// TestAnOlderLoadNeverOverwritesANewerOne: loads race, so one issued before
// a later change event must be dropped if it lands last.
func TestAnOlderLoadNeverOverwritesANewerOne(t *testing.T) {
	lister := &fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {testPod("worker-0", "default")},
	}}
	yaml := &mutableYAML{text: fixtureYAML, rv: "1"}
	m := New(Config{
		Session: newSession(), Lister: lister, YAML: yaml,
		Kind: kube.KindPod, Namespace: "default", Name: "worker-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	_, older := m.Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
	olderMsg := older()
	yaml.text, yaml.rv = fixtureYAML+"# newer\n", "2"
	_, newer := m.Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
	m = step(t, m, newer())
	m = step(t, m, olderMsg)
	if m.resourceVersion != "2" {
		t.Fatalf("resourceVersion = %q after the older load landed last, want 2", m.resourceVersion)
	}
}
