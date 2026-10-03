package browse

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// podWithRestartsAndAge builds on the shared pod() fixture with an explicit
// restart count (Restarts column) and creation age (Age column) — the two
// numeric columns this file's tests need distinct, orderable values for.
func podWithRestartsAndAge(ns, name string, restarts int32, age time.Duration) *corev1.Pod {
	p := pod(ns, name)
	p.Status.ContainerStatuses[0].RestartCount = restarts
	p.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
	return p
}

func TestSortFirstPressAscendingOnTextColumn(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "zeta"), pod("default", "alpha"), pod("default", "mid")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "1"}) // column 1 = Name
	if m.sortColumn != 1 || !m.sortAsc {
		t.Fatalf("sortColumn=%d sortAsc=%v, want 1/true", m.sortColumn, m.sortAsc)
	}
	gotNames := displayRowNames(m)
	if want := []string{"alpha", "mid", "zeta"}; !equalStrings(gotNames, want) {
		t.Fatalf("names = %v, want %v (ascending)", gotNames, want)
	}
}

func TestSortFirstPressDescendingOnNumericColumn(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			podWithRestartsAndAge("default", "low", 1, time.Hour),
			podWithRestartsAndAge("default", "high", 9, time.Hour),
			podWithRestartsAndAge("default", "mid", 4, time.Hour),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "4"}) // column 4 = Restarts on Pods
	if m.sortColumn != 4 || m.sortAsc {
		t.Fatalf("sortColumn=%d sortAsc=%v, want 4/false (descending-first)", m.sortColumn, m.sortAsc)
	}
	if want := []string{"high", "mid", "low"}; !equalStrings(displayRowNames(m), want) {
		t.Fatalf("names = %v, want %v (descending restarts)", displayRowNames(m), want)
	}
}

func TestSortSameDigitTogglesDirection(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "zeta"), pod("default", "alpha")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "1"})
	if !m.sortAsc {
		t.Fatalf("first press should be ascending")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "1"})
	if m.sortAsc {
		t.Fatalf("second press on the same column should flip to descending")
	}
	if want := []string{"zeta", "alpha"}; !equalStrings(displayRowNames(m), want) {
		t.Fatalf("names = %v, want %v (descending)", displayRowNames(m), want)
	}
}

func TestSortDifferentDigitSwitchesColumnAtItsOwnDefault(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			podWithRestartsAndAge("default", "zeta", 9, time.Hour),
			podWithRestartsAndAge("default", "alpha", 1, time.Hour),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "4"}) // Restarts, descending-first
	if m.sortAsc {
		t.Fatalf("Restarts' first press should be descending")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "1"}) // switch to Name, its own default (ascending)
	if m.sortColumn != 1 || !m.sortAsc {
		t.Fatalf("switching to Name should reset to its own default (ascending): sortColumn=%d sortAsc=%v", m.sortColumn, m.sortAsc)
	}
	if want := []string{"alpha", "zeta"}; !equalStrings(displayRowNames(m), want) {
		t.Fatalf("names = %v, want %v (ascending name)", displayRowNames(m), want)
	}
}

func TestSortNoopWhileGrouped(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("ns-a", "zeta"), pod("ns-b", "alpha")},
	}}
	session := newSession()
	session.Location.Namespace = "" // all-namespaces grouped view (6b)
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())
	if !m.grouped() {
		t.Fatalf("expected grouped() true for an all-namespaces Pods view")
	}

	m = step(t, m, tea.KeyPressMsg{Text: "1"})
	if m.sortColumn != 0 {
		t.Fatalf("sortColumn = %d, want 0 (no-op while grouped)", m.sortColumn)
	}
}

func TestSortResetsOnNamespaceSwitch(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "zeta"), pod("other", "alpha")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "4"})
	if m.sortColumn == 0 {
		t.Fatalf("expected a manual sort to be active before switching namespace")
	}

	cmd := m.switchNamespace("other")
	m = step(t, m, cmd())
	if m.sortColumn != 0 || m.sortAsc {
		t.Fatalf("sortColumn=%d sortAsc=%v, want 0/false after a namespace switch", m.sortColumn, m.sortAsc)
	}
}

func TestSortResetsOnKindSwitch(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:        {pod("default", "api-1")},
		kube.KindDeployment: {deploymentObj("default", "api")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "4"})
	if m.sortColumn == 0 {
		t.Fatalf("expected a manual sort to be active before switching kind")
	}

	cmd := m.switchKind(kube.KindDeployment)
	m = step(t, m, cmd())
	if m.sortColumn != 0 || m.sortAsc {
		t.Fatalf("sortColumn=%d sortAsc=%v, want 0/false after a kind switch", m.sortColumn, m.sortAsc)
	}
}

func TestSortCPULiveReorderOnMetricsPoll(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1"), pod("default", "api-2")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "5"}) // column 5 = CPU on Pods
	if m.sortColumn != 5 || m.sortAsc {
		t.Fatalf("sortColumn=%d sortAsc=%v, want 5/false (CPU descending-first)", m.sortColumn, m.sortAsc)
	}

	// api-1 starts busier; a later poll flips the ranking without another
	// keypress — cellLess reads live off m.podMetrics, not a frozen snapshot.
	m = step(t, m, podMetricsLoadedMsg{
		epoch:     m.metricsEpoch,
		namespace: m.countNamespace(),
		metrics: map[string]kube.PodMetrics{
			kube.PodKey("default", "api-1"): {CPU: "500m", CPUMilli: 500},
			kube.PodKey("default", "api-2"): {CPU: "10m", CPUMilli: 10},
		},
	})
	if want := []string{"api-1", "api-2"}; !equalStrings(displayRowNames(m), want) {
		t.Fatalf("names = %v, want %v (api-1 busiest first)", displayRowNames(m), want)
	}

	m = step(t, m, podMetricsLoadedMsg{
		epoch:     m.metricsEpoch,
		namespace: m.countNamespace(),
		metrics: map[string]kube.PodMetrics{
			kube.PodKey("default", "api-1"): {CPU: "5m", CPUMilli: 5},
			kube.PodKey("default", "api-2"): {CPU: "900m", CPUMilli: 900},
		},
	})
	if want := []string{"api-2", "api-1"}; !equalStrings(displayRowNames(m), want) {
		t.Fatalf("names = %v, want %v after api-2 becomes busiest", displayRowNames(m), want)
	}
}

func TestSortArrowRendersOnChosenColumn(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "zeta"), pod("default", "alpha")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "1"})
	view := ansi.Strip(m.Render())
	if !strings.Contains(view, "NAME ↑") {
		t.Fatalf("expected an ascending arrow next to NAME:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "1"})
	view = ansi.Strip(m.Render())
	if !strings.Contains(view, "NAME ↓") {
		t.Fatalf("expected a descending arrow next to NAME:\n%s", view)
	}
}

// TestSortArrowRendersOnNarrowFixedColumns covers the columns whose
// resources.fixedWidths entry is their whole on-screen width: REV on 18a's
// Helm list and AGE everywhere. Both were sized at 4 against a 3-cell title,
// which is one cell short of the " ↓" components.Table appends — so they
// sorted correctly while showing no indicator at all.
func TestSortArrowRendersOnNarrowFixedColumns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   kube.ResourceKind
		objs   []runtime.Object
		key    string
		header string
	}{
		{
			name: "helm rev", kind: kube.KindHelmRelease, key: "5", header: "REV",
			objs: []runtime.Object{
				helmRelease("default", "postgresql", "postgresql", "12.1.9", "15.4.0", "deployed", 3),
				helmRelease("default", "redis", "redis", "18.1.5", "7.2.4", "deployed", 2),
			},
		},
		{
			name: "pod age", kind: kube.KindPod, key: "8", header: "AGE",
			objs: []runtime.Object{pod("default", "zeta"), pod("default", "alpha")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{tc.kind: tc.objs}}
			session := newSession()
			session.Location.Kind = tc.kind
			m := New(Config{Session: session, Lister: lister})
			m.SetSize(120, 36)
			m = step(t, m, m.Init()())

			// Both columns are right-aligned, so they sort descending first
			// (resources.RightAlignTitles drives sort.go's defaultSortAsc).
			m = step(t, m, tea.KeyPressMsg{Text: tc.key})
			view := ansi.Strip(m.Render())
			if !strings.Contains(view, tc.header+" ↓") {
				t.Fatalf("expected a descending arrow next to %s:\n%s", tc.header, view)
			}

			m = step(t, m, tea.KeyPressMsg{Text: tc.key})
			view = ansi.Strip(m.Render())
			if !strings.Contains(view, tc.header+" ↑") {
				t.Fatalf("expected an ascending arrow next to %s:\n%s", tc.header, view)
			}
		})
	}
}

// displayRowNames extracts m.display's data rows' Names in order, skipping
// any 6b group/fold/summary lines (none expected here since these tests all
// stay single-namespace) — bulk.go's own targetNames takes the marked set's
// []tui.BulkTarget, not the display list, so this is its m.display-shaped
// counterpart rather than a duplicate.
func displayRowNames(m Model) []string {
	names := make([]string, 0, len(m.display))
	for _, dr := range m.display {
		if dr.kind == rowKindData {
			names = append(names, dr.row.row.Name)
		}
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
