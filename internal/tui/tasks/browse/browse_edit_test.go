package browse

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
)

// TestEditNonProdLaunchesImmediately confirms 'E' in a non-prod context
// launches kubectl edit directly (verbs.Resolve(verbs.Edit, …) == TierNone) — no
// pendingEdit confirm gate, and the returned Cmd is the tea.ExecProcess
// wrapping kube.EditSpec.
func TestEditNonProdLaunchesImmediately(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "E"})
	next, ok := updated.(*Model)
	if !ok {
		t.Fatalf("expected browse to stay the active task, got %T", updated)
	}
	if next.pendingEdit != nil {
		t.Fatal("non-prod 'E' must not stage a pendingEdit confirm")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil edit Cmd")
	}
}

// TestEditProdShowsInlinePromptAndLaunchesOnY confirms 'E' in a PROD context
// stages pendingEdit and shows the inline y/N line (verbs.Resolve(verbs.Edit, …) ==
// TierInline), and only launches kubectl edit once 'y' confirms.
func TestEditProdShowsInlinePromptAndLaunchesOnY(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "E"})
	if m.pendingEdit == nil {
		t.Fatal("expected a PROD 'E' press to stage pendingEdit")
	}
	kb := m.Keybar()
	if kb.RightNote == "" || !strings.Contains(kb.RightNote, "api-0") {
		t.Fatalf("expected the keybar prompt to name the target, got %q", kb.RightNote)
	}
	// The table itself must still be visible — the inline gate never
	// overrides Body(), same as TierInline delete.
	if !strings.Contains(plain(m.Render()), "api-0") {
		t.Fatalf("expected the table to stay visible under the inline edit confirm:\n%s", plain(m.Render()))
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "y"})
	next := updated.(*Model)
	if next.pendingEdit != nil {
		t.Fatal("expected pendingEdit cleared after confirming")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil edit Cmd after confirming")
	}
}

// TestEditProdCancelOnN confirms 'n' (or esc) drops pendingEdit without
// launching kubectl edit.
func TestEditProdCancelOnN(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{Session: sess, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "E"})
	updated, cmd := m.Update(tea.KeyPressMsg{Text: "n"})
	next := updated.(*Model)
	if next.pendingEdit != nil {
		t.Fatal("expected pendingEdit cleared after cancelling")
	}
	if cmd != nil {
		t.Fatal("expected no Cmd after cancelling")
	}
}

// TestEditResultFeedbackSurfacesInKeybar confirms a non-zero kubectl edit
// exit sets browse's own execFeedback, surfaced via Keybar's RightNote —
// mirrors exec's own feedback contract.
func TestEditResultFeedbackSurfacesInKeybar(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, editResultMsg{err: exitStatusError{}})
	kb := m.Keybar()
	if kb.RightNote == "" {
		t.Fatal("expected the edit-exit feedback in Keybar RightNote")
	}
}
