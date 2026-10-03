package browse

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

// TestSpaceMarksCursorAndAdvances covers 20a's core grammar: space marks the
// cursor row and advances the cursor, and the marked row renders the ▪ glyph
// plus the health strip/mode pill's "N marked" chrome.
func TestSpaceMarksCursorAndAdvances(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0"), pod("default", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.selected != 0 {
		t.Fatalf("selected = %d, want 0", m.selected)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "space"})
	if m.selected != 1 {
		t.Fatalf("expected space to advance selection, selected = %d", m.selected)
	}
	if !m.isMarked(m.display[0].row.row) {
		t.Fatalf("expected the first row marked after space")
	}
	if m.isMarked(m.display[1].row.row) {
		t.Fatalf("expected the second row unmarked")
	}

	view := plain(m.Render())
	if !strings.Contains(view, "1 marked") {
		t.Fatalf("expected the health strip to show '1 marked':\n%s", view)
	}
	if !strings.Contains(view, "1 MARKED") {
		t.Fatalf("expected the mode pill to show '1 MARKED':\n%s", view)
	}
	if !strings.Contains(view, tui.GlyphMarked) {
		t.Fatalf("expected the mark glyph in the table:\n%s", view)
	}
}

// TestMarkAllFilteredMarksOnlyVisibleRows covers "filter-then-mark": '*'
// marks every row the live filter query currently matches, not the whole
// table.
func TestMarkAllFilteredMarksOnlyVisibleRows(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0"), pod("default", "api-1"), pod("default", "worker-0")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "/"})
	for _, r := range "api" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	if len(m.visible) != 2 {
		t.Fatalf("expected the filter to narrow to 2 rows, got %d", len(m.visible))
	}
	m = step(t, m, tea.KeyPressMsg{Text: "*"})
	if len(m.marks) != 2 {
		t.Fatalf("marks = %d, want 2", len(m.marks))
	}
	if m.isMarked(resources.Row{Namespace: "default", Name: "worker-0"}) {
		t.Fatalf("expected worker-0 (filtered out) to stay unmarked")
	}

	// Clearing the filter (esc) drops the query but keeps the marks — 20a's
	// marked set persists independent of whatever's currently filtered.
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.filterActive {
		t.Fatalf("expected esc to clear the filter (no marks were active while filtering)")
	}
	if len(m.marks) != 2 {
		t.Fatalf("expected marks to survive clearing the filter, got %d", len(m.marks))
	}
}

// TestEscClearsMarksBeforeWalkingBack covers "esc clears marks before it
// walks back a level": with marks active, esc must consume the keypress
// rather than sending tui.BackMsg.
func TestEscClearsMarksBeforeWalkingBack(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "space"})
	if len(m.marks) != 1 {
		t.Fatalf("expected 1 mark, got %d", len(m.marks))
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	next := *updated.(*Model)
	if len(next.marks) != 0 {
		t.Fatalf("expected esc to clear marks, still have %d", len(next.marks))
	}
	if cmd != nil {
		if _, ok := cmd().(tui.BackMsg); ok {
			t.Fatalf("expected esc to consume the keypress (clearing marks) rather than send BackMsg")
		}
	}
}

// TestBulkDeleteNonProdShowsInlinePromptAndDeletesMarkedSet covers 20a's
// non-prod path: ctrl-d with marks active deletes the whole marked set (not
// just the cursor row) after an inline y/N, and clears the marks afterward.
func TestBulkDeleteNonProdShowsInlinePromptAndDeletesMarkedSet(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0"), pod("default", "api-1"), pod("default", "worker-0")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "space"}) // mark api-0, advance
	m = step(t, m, tea.KeyPressMsg{Text: "space"}) // mark api-1, advance
	if len(m.marks) != 2 {
		t.Fatalf("marks = %d, want 2", len(m.marks))
	}

	// Before opening the confirm, the keybar's ctrl-d hint already names the
	// marked count (20a: "ctrl-d delete 3 · y/N").
	deleteHintFound := false
	for _, g := range m.Keybar().Groups {
		for _, h := range g {
			if h.Key == "D" && h.Label == "delete 2" {
				deleteHintFound = true
			}
		}
	}
	if !deleteHintFound {
		t.Fatalf("expected the ctrl-d hint to read 'delete 2', groups=%v", m.Keybar().Groups)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.pendingBulkDelete() {
		t.Fatal("expected ctrl-d with marks active to open the bulk delete confirm")
	}
	kb := m.Keybar()
	if !strings.Contains(kb.RightNote, "kubectl delete") {
		t.Fatalf("expected the inline confirm's will-run line, got %q", kb.RightNote)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.deleted) != 2 {
		t.Fatalf("deleted = %v, want 2 entries", mut.deleted)
	}
	if len(m.marks) != 0 {
		t.Fatalf("expected marks cleared after a successful bulk delete, got %d", len(m.marks))
	}
	if m.pendingBulkDelete() {
		t.Fatalf("expected pendingBulkDelete cleared after execution")
	}
}

// TestBulkDeleteProdOpensTypeCountModal covers 20a's PROD escalation: enter
// no-ops until the marked count is typed, and the modal lists every marked
// object.
func TestBulkDeleteProdOpensTypeCountModal(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0"), pod("default", "api-1")},
	}}
	mut := &fakeMutator{}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{Session: sess, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"}) // mark all (no filter = every visible row)
	if len(m.marks) != 2 {
		t.Fatalf("marks = %d, want 2", len(m.marks))
	}

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.pendingBulkDelete() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected the PROD type-the-count modal, got %+v", m.actions.Tier())
	}
	view := plain(m.Render())
	if !strings.Contains(view, "PROD CONTEXT") {
		t.Fatalf("expected the PROD CONTEXT tag in the modal:\n%s", view)
	}
	if !strings.Contains(view, "api-0") || !strings.Contains(view, "api-1") {
		t.Fatalf("expected the modal to list every marked object:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 0 {
		t.Fatalf("expected enter to no-op before the count matches: %v", mut.deleted)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "2"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 2 {
		t.Fatalf("deleted = %v, want 2 entries", mut.deleted)
	}
}

// TestBulkNamespaceDeleteNonProdOpensTypeCountModal is the H3 regression for
// the marked set: `*` `D` `y` on the Namespaces list used to delete every
// listed namespace outside PROD. A cascade-scope kind's bulk delete is the
// type-the-count modal regardless of context.
func TestBulkNamespaceDeleteNonProdOpensTypeCountModal(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNamespace: {namespace("team-a"), namespace("team-b")},
	}}
	mut := &fakeMutator{}
	sess := newSession()
	sess.Location.Kind = kube.KindNamespace
	m := New(Config{Session: sess, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"})
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.pendingBulkDelete() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected the type-the-count modal, got %+v", m.actions.Tier())
	}
	view := plain(m.Render())
	if strings.Contains(view, "PROD CONTEXT") || !strings.Contains(view, "team-a") {
		t.Fatalf("expected a non-prod modal listing the namespaces:\n%s", view)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 0 {
		t.Fatalf("expected y/enter to run nothing before the count is typed, got %v", mut.deleted)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "2"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 2 {
		t.Fatalf("deleted = %v, want both namespaces after typing the count", mut.deleted)
	}
}

// TestMarksClearOnNamespaceSwitch covers "marks are per-view and drop on
// kind/namespace switch."
func TestMarksClearOnNamespaceSwitch(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0"), pod("nva-stage", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "space"})
	if len(m.marks) != 1 {
		t.Fatalf("marks = %d, want 1", len(m.marks))
	}

	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "nva-stage"})
	if len(m.marks) != 0 {
		t.Fatalf("expected marks cleared after a namespace switch, got %d", len(m.marks))
	}
}

// TestBulkNodeDeleteNonProdOpensTypeCountModal is review 05 M4 for the
// marked set: `*` `D` on the Nodes list is the type-the-count modal outside
// PROD, never an inline y/N.
func TestBulkNodeDeleteNonProdOpensTypeCountModal(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("worker-1", true, false), nodeObj("worker-2", true, false)},
	}}
	mut := &fakeMutator{}
	sess := newSession()
	sess.Location.Kind = kube.KindNode
	m := New(Config{Session: sess, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"})
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.pendingBulkDelete() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected the type-the-count modal, pending=%v tier=%v", m.pendingBulkDelete(), m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 0 {
		t.Fatalf("expected y/enter to run nothing before the count is typed, got %v", mut.deleted)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "2"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 2 {
		t.Fatalf("deleted = %v, want both nodes after typing the count", mut.deleted)
	}
}

// TestBulkDeleteIgnoresForceDeleteChord: a marked set has no force-delete
// form, so ctrl-k on its inline confirm leaves the y/N exactly as it was.
func TestBulkDeleteIgnoresForceDeleteChord(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0"), pod("default", "api-1")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"})
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	m = step(t, m, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	if !m.pendingBulkDelete() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected the inline bulk confirm untouched by ctrl-k, pending=%v tier=%v", m.pendingBulkDelete(), m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.deleted) != 2 || len(mut.forceDeleted) != 0 {
		t.Fatalf("deleted = %v, forceDeleted = %v, want a plain delete of both", mut.deleted, mut.forceDeleted)
	}
}

// TestBulkDeleteConfirmDropsOnNamespaceSwitch: the marks drop on a namespace
// switch (20a), and the confirm naming them goes with them.
func TestBulkDeleteConfirmDropsOnNamespaceSwitch(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0"), pod("nva-stage", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "space"})
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.pendingBulkDelete() {
		t.Fatal("expected the bulk delete confirm open")
	}
	m = step(t, m, tui.SwitchNamespaceMsg{Namespace: "nva-stage"})
	if m.pendingBulkDelete() || m.actions.Active() {
		t.Fatal("expected the bulk delete confirm dropped with the marks")
	}
}
