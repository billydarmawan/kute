package nodedetail

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// manyPods builds n schedulable, healthy pods on node — all tie at the same
// health rank, so load()'s sort falls back to its name tiebreak, giving a
// stable, predictable order: pod-00 sorts first, pod-(n-1) sorts last. The
// distinct memory requests are unused by the sort (that only ever reads
// live usage, never Requests) — kept only for schedPod's signature.
func manyPods(node string, n int) []runtime.Object {
	objs := make([]runtime.Object, n)
	for i := range n {
		objs[i] = schedPod("default", fmt.Sprintf("pod-%02d", i), node, fmt.Sprintf("%dMi", (i+1)*10))
	}
	return objs
}

// TestPodsListScrolls confirms moveSelection scrolls the bottom pane's
// viewport to keep the selected pod visible — regression test for the pod
// list being unscrollable when it overflows a short terminal.
func TestPodsListScrolls(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod:  manyPods("node-a", 30),
	}}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a"})
	m.SetSize(120, 20) // short terminal: far fewer than 30 rows fit
	m = step(t, m, m.Init()())

	if len(m.pods) != 30 {
		t.Fatalf("pods = %d, want 30", len(m.pods))
	}
	rows := m.tableDataRows()
	if rows >= 30 {
		t.Fatalf("tableDataRows = %d, want < 30 for this to be a meaningful test", rows)
	}
	if m.offset != 0 {
		t.Fatalf("offset = %d before any movement, want 0", m.offset)
	}

	// All 30 pods are healthy (same Status), so load()'s sort falls back to
	// its name tiebreak (ascending) — the last row here starts off the
	// initial viewport regardless of which name that is.
	for range m.pods {
		m.moveSelection(1)
	}
	last := m.pods[len(m.pods)-1]
	if m.offset == 0 {
		t.Fatal("expected offset to advance once selection scrolled past the initial viewport")
	}

	view := plain(m.Render())
	if !strings.Contains(view, last.pod.Name) {
		t.Fatalf("expected the scrolled-to selection %q to be visible:\n%s", last.pod.Name, view)
	}

	// Scrolling back up to the top should bring the offset back to 0.
	for range m.pods {
		m.moveSelection(-1)
	}
	if m.offset != 0 {
		t.Fatalf("offset = %d after scrolling back to the top, want 0", m.offset)
	}
}

// TestPodsListFilters confirms '/' opens a live filter over the pods list
// (narrowing m.pods, keeping m.allPods intact) and esc clears it —
// regression test for the pod list having no filter at all.
func TestPodsListFilters(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod: {
			schedPod("default", "api-server", "node-a", "1Gi"),
			schedPod("default", "worker-1", "node-a", "512Mi"),
			schedPod("default", "worker-2", "node-a", "256Mi"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if len(m.allPods) != 3 {
		t.Fatalf("allPods = %d, want 3", len(m.allPods))
	}

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	if !m.filterActive {
		t.Fatal("expected '/' to activate the filter")
	}
	if !m.CapturingInput() {
		t.Fatal("expected CapturingInput to be true while filtering")
	}

	for _, r := range "worker" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	if len(m.pods) != 2 {
		t.Fatalf("filtered pods = %d, want 2 (api-server excluded)", len(m.pods))
	}
	// ctrl+j/ctrl+k move the selection while filtering, as in browse;
	// plain j/k stay typeable.
	m.selected = 0
	m = step(t, m, tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	if m.selected != 1 {
		t.Fatalf("selected = %d after ctrl+j, want 1", m.selected)
	}
	m = step(t, m, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	if m.selected != 0 || m.filterInput.Value() != "worker" {
		t.Fatalf("after ctrl+k: selected = %d, query = %q; want 0 and the query untouched", m.selected, m.filterInput.Value())
	}
	if len(m.allPods) != 3 {
		t.Fatal("expected allPods to stay intact while filtering")
	}
	view := plain(m.Render())
	if strings.Contains(view, "api-server") {
		t.Fatalf("expected api-server hidden by the filter:\n%s", view)
	}
	if !strings.Contains(view, "FILTER") {
		t.Fatalf("expected FILTER pill while filtering:\n%s", view)
	}
	// docs/design system-wide interactions: "items never silently
	// disappear" — the strip must say a row was hidden by the filter, not
	// just show a bare matched count.
	if !strings.Contains(view, "hidden by filter") {
		t.Fatalf("expected the 'hidden by filter' notice:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.filterActive {
		t.Fatal("expected esc to close the filter")
	}
	if len(m.pods) != 3 {
		t.Fatalf("pods after esc = %d, want 3 (filter cleared)", len(m.pods))
	}
}
