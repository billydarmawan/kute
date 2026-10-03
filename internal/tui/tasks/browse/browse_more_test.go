package browse

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

func crashPod(ns, name string) *corev1.Pod {
	p := pod(ns, name)
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
	}
	return p
}

func pendingPod(ns, name string) *corev1.Pod {
	p := pod(ns, name)
	p.Status.Phase = corev1.PodPending
	p.Status.ContainerStatuses[0].Ready = false
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	}
	return p
}

func TestDefaultSortIsUnhealthyFirst(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			pod("default", "healthy-a"),
			crashPod("default", "crashing"),
			pod("default", "healthy-b"),
			pendingPod("default", "starting"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready", m.state)
	}
	if len(m.visible) != 4 {
		t.Fatalf("visible = %d, want 4", len(m.visible))
	}
	if m.visible[0].row.Name != "crashing" {
		t.Fatalf("first row = %q, want crashing (Fail ranks first)", m.visible[0].row.Name)
	}
	if m.visible[1].row.Name != "starting" {
		t.Fatalf("second row = %q, want starting (Warn ranks second)", m.visible[1].row.Name)
	}
}

func TestFilterNarrowsAndEscClears(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1"), pod("default", "worker-2")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m = *updated.(*Model)
	if !m.filterActive {
		t.Fatal("expected filterActive after '/'")
	}

	for _, r := range "api" {
		updated, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = *updated.(*Model)
	}
	if len(m.visible) != 1 || m.visible[0].row.Name != "api-1" {
		t.Fatalf("filtered visible = %+v, want just api-1", m.visible)
	}
	view := ansi.Strip(m.Render())
	if !strings.Contains(view, "hidden by filter") {
		t.Fatalf("expected hidden-by-filter notice:\n%s", view)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = *updated.(*Model)
	if m.filterActive || m.filterInput.Value() != "" {
		t.Fatalf("esc should clear filter, got active=%v query=%q", m.filterActive, m.filterInput.Value())
	}
	if len(m.visible) != 2 {
		t.Fatalf("visible after clear = %d, want 2", len(m.visible))
	}
}

func TestFilterTypingAcceptsJAndK(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "worker-2"), pod("default", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m = *updated.(*Model)
	for _, r := range "work" {
		updated, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = *updated.(*Model)
	}
	if m.filterInput.Value() != "work" {
		t.Fatalf("filterQuery = %q, want %q ('k' must type, not move selection)", m.filterInput.Value(), "work")
	}
	if len(m.visible) != 1 || m.visible[0].row.Name != "worker-2" {
		t.Fatalf("filtered visible = %+v, want just worker-2", m.visible)
	}
}

func TestFilterCtrlJKMovesSelectionWithoutTyping(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1"), pod("default", "api-2")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m = *updated.(*Model)
	if m.selected != 0 {
		t.Fatalf("selected = %d, want 0 before moving", m.selected)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	m = *updated.(*Model)
	if m.selected != 1 {
		t.Fatalf("selected = %d, want 1 after ctrl+j", m.selected)
	}
	if m.filterInput.Value() != "" {
		t.Fatalf("filterQuery = %q, want empty (ctrl+j must move, not type)", m.filterInput.Value())
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	m = *updated.(*Model)
	if m.selected != 0 {
		t.Fatalf("selected = %d, want 0 after ctrl+k", m.selected)
	}
	if m.filterInput.Value() != "" {
		t.Fatalf("filterQuery = %q, want empty (ctrl+k must move, not type)", m.filterInput.Value())
	}
}

// TestEnterCommitsFilterForKindWithNoDestination covers enter's commit
// behavior on a kind with no enter-destination at all (ConfigMaps —
// openSelectedEnter would return ok=false for it even unfiltered): query/
// rows/strip stay exactly as they were, but keys stop being captured as
// typing, so j/k moves the cursor and ctrl-d runs delete directly on the
// narrowed list.
func TestEnterCommitsFilterForKindWithNoDestination(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindConfigMap: {configMap("default", "app-config"), configMap("default", "other-config")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, tui.GotoKindMsg{Kind: kube.KindConfigMap})

	m = step(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	for _, r := range "app-" {
		m = step(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if len(m.visible) != 1 || m.visible[0].row.Name != "app-config" {
		t.Fatalf("visible = %+v, want just app-config", m.visible)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Text: "enter"})
	m = *updated.(*Model)
	if !m.filterActive || !m.filterListFocused {
		t.Fatalf("filterActive=%v filterListFocused=%v, want both true after enter", m.filterActive, m.filterListFocused)
	}
	if m.filterInput.Value() != "app-" || len(m.visible) != 1 {
		t.Fatalf("expected the filter to stay committed: query=%q visible=%+v", m.filterInput.Value(), m.visible)
	}
	if m.CapturingInput() {
		t.Fatal("expected CapturingInput to release once the filter is list-focused")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "FILTER") {
		t.Fatalf("expected the FILTER pill to stay visible:\n%s", view)
	}
	if strings.Contains(view, "app-"+tui.GlyphSelBar) {
		t.Fatalf("expected the filter strip's typing cursor gone once committed (GlyphSelBar is reused as the table's row-selection marker elsewhere, so check it's not glued to the query):\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() {
		t.Fatalf("expected ctrl-d to open the inline delete confirm while list-focused, got actions.Active()=false")
	}
	kb := m.Keybar()
	if kb.RightNote == "" || !strings.Contains(kb.RightNote, "app-config") {
		t.Fatalf("expected the confirm to target app-config (the filtered cursor row), got %q", kb.RightNote)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.deleted) != 1 || mut.deleted[0] != "app-config" {
		t.Fatalf("deleted = %v, want [app-config]", mut.deleted)
	}
}

// TestSelectionScrollsIntoView pins tableDataRows to the real rendered
// viewport: after walking selection to the last of many rows on a short
// terminal, the selected row must actually be on screen (this once lagged —
// the budget over-counted by the FooterLine row, letting the selection sit
// one row below the visible window).
func TestSelectionScrollsIntoView(t *testing.T) {
	objs := make([]runtime.Object, 0, 12)
	for _, name := range []string{
		"pod-01", "pod-02", "pod-03", "pod-04", "pod-05", "pod-06",
		"pod-07", "pod-08", "pod-09", "pod-10", "pod-11", "pod-12",
	} {
		objs = append(objs, pod("default", name))
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: objs}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 11) // body 5 → table header + footer + 3 data rows
	m = step(t, m, m.Init()())

	for range 11 {
		m = step(t, m, tea.KeyPressMsg{Text: "j"})
	}
	if got := m.selectedName(); got != "pod-12" {
		t.Fatalf("selected = %q, want pod-12", got)
	}
	var selectedLine string
	for line := range strings.SplitSeq(plain(m.Render()), "\n") {
		if strings.HasPrefix(line, "▎") {
			selectedLine = line
		}
	}
	if !strings.Contains(selectedLine, "pod-12") {
		t.Fatalf("selected row must be inside the rendered viewport, got selection line %q in:\n%s", selectedLine, plain(m.Render()))
	}
}

func TestCapturingInputTracksFilterActive(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.CapturingInput() {
		t.Fatal("CapturingInput() should be false before '/' is pressed")
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	m = *updated.(*Model)
	if !m.CapturingInput() {
		t.Fatal("CapturingInput() should be true while filtering")
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = *updated.(*Model)
	if m.CapturingInput() {
		t.Fatal("CapturingInput() should be false after esc clears the filter")
	}
}

type fakeMetrics struct {
	metrics          map[string]kube.PodMetrics
	containerMetrics map[string]map[string]kube.PodMetrics
}

func (f fakeMetrics) PodMetricsByNamespace(context.Context, string) (map[string]kube.PodMetrics, error) {
	return f.metrics, nil
}

func (f fakeMetrics) ContainerMetricsByNamespace(context.Context, string) (map[string]map[string]kube.PodMetrics, error) {
	return f.containerMetrics, nil
}

func TestPodMetricsRenderAsBars(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	metrics := fakeMetrics{metrics: map[string]kube.PodMetrics{
		kube.PodKey("default", "api-1"): {CPU: "45m", MEM: "128Mi", CPUMilli: 45, MemBytes: 128 * 1024 * 1024},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Metrics: metrics})
	m.SetSize(120, 36)
	// Drive the row load and one metrics poll directly rather than through
	// step(m.Init()...): Init's metrics branch also schedules a recurring
	// metricsTickMsg (the real 2s poll loop), which step's cmd-draining
	// would follow forever in this synchronous test harness.
	m = step(t, m, m.load()())
	m = step(t, m, m.loadMetrics(m.metricsEpoch)())

	view := ansi.Strip(m.Render())
	if !strings.Contains(view, "45m") || !strings.Contains(view, "128Mi") {
		t.Fatalf("expected live CPU/MEM values in view:\n%s", view)
	}
}

// TestPodMetricsRenderRoundedRatherThanTruncated pins the pairing between
// kube.FormatMemory and metricCell: the value slot is only
// MetricColumnWidth-barWidth-1 = 5 cells, so an unrounded 174.5Mi used to reach
// the view as "174.…". The MEM string here comes from the real formatter rather
// than a literal precisely so this fails if that rounding is ever relaxed.
func TestPodMetricsRenderRoundedRatherThanTruncated(t *testing.T) {
	const memBytes = 174.5 * 1024 * 1024
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	metrics := fakeMetrics{metrics: map[string]kube.PodMetrics{
		kube.PodKey("default", "api-1"): {
			CPU:      kube.FormatCPU(*resource.NewMilliQuantity(45, resource.DecimalSI)),
			MEM:      kube.FormatMemory(*resource.NewQuantity(memBytes, resource.BinarySI)),
			CPUMilli: 45,
			MemBytes: memBytes,
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Metrics: metrics})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())
	m = step(t, m, m.loadMetrics(m.metricsEpoch)())

	view := ansi.Strip(m.Render())
	if !strings.Contains(view, "175Mi") {
		t.Fatalf("expected 174.5Mi to render rounded as 175Mi:\n%s", view)
	}
	if strings.Contains(view, "174") {
		t.Fatalf("expected no truncated 174.… remnant in view:\n%s", view)
	}
}

// TestUnavailablePodMetricsRenderAsDash pins the 2a fix (docs/design
// README.md §29): both the real cluster and the fake demo cluster report a
// pod with no metrics data as the literal string "n/a" (kube.PodMetrics{CPU:
// "n/a", MEM: "n/a"}), not an empty string — metricCell must still collapse
// that to the spec's clean "–" rather than leaking the raw sentinel,
// mirroring nodeMetricCell's own "n/a" fallback for the same data shape.
func TestUnavailablePodMetricsRenderAsDash(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	metrics := fakeMetrics{metrics: map[string]kube.PodMetrics{
		kube.PodKey("default", "api-1"): {CPU: "n/a", MEM: "n/a"},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Metrics: metrics})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())
	m = step(t, m, m.loadMetrics(m.metricsEpoch)())

	view := ansi.Strip(m.Render())
	if strings.Contains(view, "n/a") {
		t.Fatalf("expected the raw \"n/a\" sentinel never to reach the view:\n%s", view)
	}
	if !strings.Contains(view, "–") {
		t.Fatalf("expected the spec's \"–\" placeholder in view:\n%s", view)
	}
}

func TestLogsKeyOpensLogsScreen(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	opened := false
	m := New(Config{Session: newSession(), Lister: lister, OpenLogs: func(p kube.Pod, _ string, w, h int) (tea.Model, tea.Cmd) {
		opened = true
		if p.Name != "api-1" || p.Namespace != "default" {
			t.Fatalf("unexpected pod passed to OpenLogs: %+v", p)
		}
		return fakeTask{}, nil
	}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if !opened {
		t.Fatal("expected OpenLogs to be called")
	}
	if _, ok := updated.(fakeTask); !ok {
		t.Fatalf("expected Update to return the pushed logs task, got %T", updated)
	}
}

type fakeTask struct{}

func (fakeTask) Init() tea.Cmd                       { return nil }
func (fakeTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return fakeTask{}, nil }
func (fakeTask) View() tea.View                      { return tea.NewView("") }

// TestResourceChangedDebouncesReload asserts the reload really waits out the
// debounce window, not merely that a watch event schedules something.
//
// The old version of this test called the returned command inline, which
// worked — by blocking the test for a real 250ms and then asserting only on
// the message type, never on the timing that is the whole point of a
// debounce. Inside a synctest bubble the clock is fake and advances only once
// every goroutine is blocked, so "has not fired yet at 249ms" and "has fired
// at 251ms" are both exact statements, and the test costs no wall-clock time.
func TestResourceChangedDebouncesReload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindPod: {pod("default", "api-1")},
		}}
		m := New(Config{Session: newSession(), Lister: lister})
		m.SetSize(120, 36)
		m = step(t, m, m.Init()())

		updated, cmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
		m = *updated.(*Model)
		if cmd == nil {
			t.Fatal("expected a scheduled reload command")
		}

		fired := make(chan tea.Msg, 1)
		go func() { fired <- cmd() }()

		time.Sleep(reloadDebounce - time.Millisecond)
		synctest.Wait()
		select {
		case msg := <-fired:
			t.Fatalf("reload fired as %T before the %v debounce elapsed", msg, reloadDebounce)
		default:
		}

		time.Sleep(2 * time.Millisecond)
		synctest.Wait()
		var msg tea.Msg
		select {
		case msg = <-fired:
		default:
			t.Fatalf("reload never fired after the %v debounce elapsed", reloadDebounce)
		}

		due, ok := msg.(reloadDueMsg)
		if !ok {
			t.Fatalf("expected reloadDueMsg, got %T", msg)
		}
		if due.epoch != m.reloadEpoch {
			t.Fatalf("epoch = %d, want %d", due.epoch, m.reloadEpoch)
		}
	})
}

// TestResourceChangedInAnotherNamespaceSkipsReload: under --namespace-scoped
// (and for Helm's per-namespace caches) a change event carries its cache's
// namespace, and one from a namespace the screen isn't showing can't change
// what it reads — so it must not schedule a reload. A cluster-wide event
// ("") still does.
func TestResourceChangedInAnotherNamespaceSkipsReload(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	for _, tc := range []struct {
		namespace string
		reload    bool
	}{
		{"team-a", false},
		{"default", true},
		{"", true},
	} {
		_, cmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindPod, Namespace: tc.namespace})
		if got := cmd != nil; got != tc.reload {
			t.Errorf("Pod change in %q while showing default: reload scheduled = %v, want %v", tc.namespace, got, tc.reload)
		}
	}
}

// TestCapturingInputCoversCronJobPreflights: while §36b's run-now or §36c's
// resume preflight owns the keybar, g/n/c/? must reach it, not the shell.
func TestCapturingInputCoversCronJobPreflights(t *testing.T) {
	m := New(Config{Session: newSession(), Lister: fakeLister{}})
	m.pendingCronJobRun = &cronJobRunTarget{}
	if !m.CapturingInput() {
		t.Fatal("run-now preflight should capture input")
	}
	m.pendingCronJobRun = nil
	m.pendingCronJobResume = &cronJobResumeTarget{}
	if !m.CapturingInput() {
		t.Fatal("resume preflight should capture input")
	}
}
