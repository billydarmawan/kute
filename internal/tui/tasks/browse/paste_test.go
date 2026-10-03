package browse

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui/actions"
)

// TestPasteIntoFilterNarrowsRows: a pasted query narrows the list and shows
// the hidden-by-filter notice, exactly as typing it does — the paste path has
// to carry the recompute itself, since a bracketed paste never reaches
// updateFilterKey.
func TestPasteIntoFilterNarrowsRows(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1"), pod("default", "worker-2")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m = step(t, m, tea.PasteMsg{Content: "api"})
	if got := m.filterInput.Value(); got != "api" {
		t.Fatalf("filter buffer = %q, want %q", got, "api")
	}
	if len(m.visible) != 1 || m.visible[0].row.Name != "api-1" {
		t.Fatalf("filtered visible = %+v, want just api-1 — paste did not re-apply the filter", m.visible)
	}
	if view := ansi.Strip(m.Render()); !strings.Contains(view, "hidden by filter") {
		t.Fatalf("expected the hidden-by-filter notice:\n%s", view)
	}
	// Session.Location.Filter is the breadcrumb's source and has to track a
	// pasted query too.
	if got := m.session.Location.Filter; got != "api" {
		t.Fatalf("session filter = %q, want %q", got, "api")
	}
}

// TestPasteWithFilterListFocusedIsIgnored: once the query is committed and the
// list has focus, the buffer is done being typed into — g/n/c go back to being
// shell shortcuts, and a paste isn't the filter's either.
func TestPasteWithFilterListFocusedIsIgnored(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1"), pod("default", "worker-2")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m = step(t, m, tea.PasteMsg{Content: "api"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.filterListFocused {
		t.Fatal("expected enter to commit the filter and focus the list")
	}
	m = step(t, m, tea.PasteMsg{Content: "-extra"})
	if got := m.filterInput.Value(); got != "api" {
		t.Fatalf("filter buffer = %q, want it left at %q", got, "api")
	}
}

// TestPasteIntoScaleReplacesPrefill pins §17b's replace-on-first-input rule for
// the paste path: pasting into a freshly-opened scale prompt replaces the
// pre-filled count instead of appending to it, and is digit-gated.
func TestPasteIntoScaleReplacesPrefill(t *testing.T) {
	m := newDeploymentModel(t, &fakeMutator{}, 3)
	m = step(t, m, tea.KeyPressMsg{Text: "+"})
	if m.pendingScale == nil {
		t.Fatal("expected '+' to open the scale prompt")
	}
	if got := m.pendingScale.input.Value(); got != "4" {
		t.Fatalf("prefill = %q, want 4", got)
	}

	m = step(t, m, tea.PasteMsg{Content: "12\n"})
	if got := m.pendingScale.input.Value(); got != "12" {
		t.Fatalf("scale buffer = %q, want the pre-fill replaced by 12", got)
	}
	// A second paste now inserts, since the field has been written to.
	m = step(t, m, tea.PasteMsg{Content: "0"})
	if got := m.pendingScale.input.Value(); got != "120" {
		t.Fatalf("scale buffer = %q, want 120", got)
	}
	// Non-digits are rejected whole, as on the typed path.
	m = step(t, m, tea.PasteMsg{Content: "3 replicas"})
	if got := m.pendingScale.input.Value(); got != "120" {
		t.Fatalf("scale buffer = %q, want the non-digit paste rejected", got)
	}
}

// TestPasteIntoSetImageRematchesHistory: the image field is the one most likely
// to be filled from the clipboard (a digest, a CI tag), and the history rail
// has to re-match after a paste as it does after typing.
func TestPasteIntoSetImageRematchesHistory(t *testing.T) {
	m := newSetImageModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)

	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	if m.pendingSetImage == nil {
		t.Fatal("expected 'i' to open the set-image panel")
	}
	m.pendingSetImage.setBuffer("")
	m = step(t, m, tea.PasteMsg{Content: "sha256:deadbeef"})
	if got := m.pendingSetImage.input.Value(); got != "sha256:deadbeef" {
		t.Fatalf("image buffer = %q, want the pasted digest", got)
	}
}

// TestPasteIntoMetaBuffers verifies browse's pasteTarget resolver forwards
// to the open panel's own PasteTarget (the buffer-level walk lives in
// internal/tui/metapanel's TestPasteLandsInFocusedBuffer) — asserted through
// the rendered body, since the panel's buffers are its own now.
func TestPasteIntoMetaBuffers(t *testing.T) {
	dep := metaDeployment("default", "nva-worker",
		map[string]string{"team": "platform"}, nil, nil)
	m := newMetaModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})
	if !m.beginMeta() {
		t.Fatal("beginMeta returned false")
	}

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	m = step(t, m, tea.PasteMsg{Content: "envkey"})
	if body := m.Body(120, 36); !strings.Contains(body, "envkey") {
		t.Fatalf("pasted add-row key missing from the panel body:\n%s", body)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	m = step(t, m, tea.PasteMsg{Content: "stagingvalue"})
	if body := m.Body(120, 36); !strings.Contains(body, "stagingvalue") {
		t.Fatalf("pasted add-row value missing from the panel body:\n%s", body)
	}
}

// TestPasteIntoProdDeleteConfirm: the type-the-name gate accepts a paste — the
// bar is "you produced the exact name", not "you typed it by hand".
func TestPasteIntoProdDeleteConfirm(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	mut := &fakeMutator{}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{Session: sess, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected the type-the-name modal, tier=%v", m.actions.Tier())
	}
	m = step(t, m, tea.PasteMsg{Content: "api-0"})
	if got := m.actions.TypedName(); got != "api-0" {
		t.Fatalf("typed name = %q, want %q", got, "api-0")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 1 || mut.deleted[0] != "api-0" {
		t.Fatalf("deleted = %v, want [api-0] once the pasted name matched", mut.deleted)
	}
}

// TestPasteIntoInlineConfirmIsIgnored: a non-prod delete is a y/N prompt with
// no text field, so a paste there must go nowhere rather than fall through to
// the filter box or the row list.
func TestPasteIntoInlineConfirmIsIgnored(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // commit, list focused
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected the inline y/N confirm, tier=%v", m.actions.Tier())
	}
	m = step(t, m, tea.PasteMsg{Content: "api-0"})
	if got := m.actions.TypedName(); got != "" {
		t.Fatalf("inline confirm collected %q; it has no text field", got)
	}
	if got := m.filterInput.Value(); got != "" {
		t.Fatalf("paste leaked into the filter box: %q", got)
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("nothing should have been deleted: %v", mut.deleted)
	}
}

// TestPasteChordRequestsClipboard: ctrl+v with a buffer open answers with the
// clipboard read; with no buffer open it stays an ordinary (unbound) key.
func TestPasteChordRequestsClipboard(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-1")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if _, cmd := m.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'v'}); cmd != nil {
		t.Fatal("ctrl+v with no buffer open should not read the clipboard")
	}
	m = step(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	if _, cmd := m.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'v'}); cmd == nil {
		t.Fatal("ctrl+v with the filter open returned no cmd, want the clipboard read")
	}
}

// TestPasteIntoSetResourcesField: the selected quantity field takes the paste
// and clears its unset/invalid flags, as the typed path does.
func TestPasteIntoSetResourcesField(t *testing.T) {
	dep := resourcesDeployment("default", "nva-worker")
	rs, pod := resourcesOwnerChain("default", "nva-worker")
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: resourcesLister(dep, rs, pod), Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	if !m.beginSetResources() {
		t.Fatal("beginSetResources returned false")
	}

	m = step(t, m, tea.KeyPressMsg{Text: "u"}) // unset the selected field first
	if !m.pendingSetResources.fields[fieldCPURequest].unset {
		t.Fatal("expected 'u' to unset the cpu request field")
	}
	m = step(t, m, tea.PasteMsg{Content: "750m"})
	f := m.pendingSetResources.fields[fieldCPURequest]
	if f.input.Value() != "750m" {
		t.Fatalf("cpu request buffer = %q, want 750m", f.input.Value())
	}
	if f.unset {
		t.Fatal("a paste into the field must clear its unset flag, as typing does")
	}
}

// TestPasteIntoBulkDeleteCountIsDigitGated: the type-the-count confirm accepts
// a pasted count, digit-gated like the typed path.
func TestPasteIntoBulkDeleteCountIsDigitGated(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0"), pod("default", "api-1")},
	}}
	mut := &fakeMutator{}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{Session: sess, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"})
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.pendingBulkDelete() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected the PROD type-the-count modal, got %+v", m.actions.Tier())
	}

	m = step(t, m, tea.PasteMsg{Content: "two"})
	if got := m.actions.TypedName(); got != "" {
		t.Fatalf("count buffer = %q, want a non-digit paste rejected", got)
	}
	m = step(t, m, tea.PasteMsg{Content: "2\n"})
	if got := m.actions.TypedName(); got != "2" {
		t.Fatalf("count buffer = %q, want 2", got)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 2 {
		t.Fatalf("deleted = %v, want both once the pasted count matched", mut.deleted)
	}
}
