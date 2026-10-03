package browse

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

// TestFilterKeepsSortedOrderUnderGrouping: the filter is a predicate over
// already-sorted rows, never a re-rank by fuzzy score. Scored, "web" (aaa)
// outranks "web-1" (bbb) outranks "web-crashing-worker" (aaa), which split
// aaa into two runs: a collapsed "all running" summary and then a second
// aaa header holding its crashlooping row.
func TestFilterKeepsSortedOrderUnderGrouping(t *testing.T) {
	rows := []resources.Row{
		{Namespace: "aaa", Name: "web-crashing-worker", Status: resources.StatusFail},
		{Namespace: "aaa", Name: "web", Status: resources.StatusOK},
		{Namespace: "bbb", Name: "web-1", Status: resources.StatusOK},
	}
	visible, _ := applyFilter(rows, "web")
	if len(visible) != 3 {
		t.Fatalf("visible = %d rows, want 3", len(visible))
	}
	for i, fm := range visible {
		if fm.row.Name != rows[i].Name {
			t.Fatalf("visible[%d] = %s, want %s: the filter re-ordered the sorted rows", i, fm.row.Name, rows[i].Name)
		}
		if len(fm.matches) == 0 {
			t.Fatalf("visible[%d] lost its match highlight indexes", i)
		}
	}

	headers := map[string]int{}
	for _, dr := range buildDisplayRows(visible, true, nil, false) {
		switch dr.kind {
		case rowKindHeader, rowKindCollapsedSummary:
			headers[dr.namespace]++
			if dr.kind == rowKindCollapsedSummary && dr.namespace == "aaa" {
				t.Fatal("aaa was collapsed as healthy while it holds a crashlooping row")
			}
		}
	}
	for ns, n := range headers {
		if n != 1 {
			t.Fatalf("namespace %s rendered %d group headers, want 1", ns, n)
		}
	}
}

// TestFilterSelectsBestMatchNotFirstSorted: the filter keeps sort order,
// but typing a query moves the cursor to the row it matches best. A failed
// attempts-fail-… pod sorts first and also fuzzy-matches "api" (a…p…i), so
// with only order to go on ↵ would open it instead of api-1.
func TestFilterSelectsBestMatchNotFirstSorted(t *testing.T) {
	failed := pod("default", "attempts-fail-2hbhr")
	failed.Status.Phase = corev1.PodFailed
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {failed, pod("default", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if len(m.visible) != 2 || m.visible[0].row.Name != "attempts-fail-2hbhr" {
		t.Fatalf("fixture: want the failed pod sorted first, got %+v", m.visible)
	}

	m = step(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Code: 'a', Text: "api"})
	if len(m.visible) != 2 || m.visible[0].row.Name != "attempts-fail-2hbhr" {
		t.Fatalf("the filter re-ordered rows: %+v", m.visible)
	}
	if got := m.selectedName(); got != "api-1" {
		t.Fatalf("selected %q after typing api, want api-1", got)
	}

	// A reload with the same query keeps whatever the user moved to.
	m.moveSelection(-1)
	if got := m.selectedName(); got != "attempts-fail-2hbhr" {
		t.Fatalf("moveSelection(-1) selected %q", got)
	}
	m.recomputeVisible()
	if got := m.selectedName(); got != "attempts-fail-2hbhr" {
		t.Fatalf("a reload with an unchanged query moved the cursor to %q", got)
	}
}
