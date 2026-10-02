package browse

import (
	"testing"

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
	visible := applyFilter(rows, "web")
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
