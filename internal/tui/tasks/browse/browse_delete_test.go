package browse

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// TestCtrlDShowsConcreteGracePeriod pins 8b's fix: the confirm modal must
// show the pod's actual terminationGracePeriodSeconds (docs/design
// README.md §8b: "30s"), not the generic "default grace period applies" —
// this pod's spec sets a non-default 45s, so a hardcoded "30" would also be
// wrong, proving the value is actually threaded through rather than
// hardcoded.
func TestCtrlDShowsConcreteGracePeriod(t *testing.T) {
	grace := int64(45)
	podWithGrace := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers:                    []corev1.Container{{Name: "c"}},
			TerminationGracePeriodSeconds: &grace,
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true}}},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {podWithGrace},
	}}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{Session: sess, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	view := plain(m.Render())
	if !strings.Contains(view, "grace period 45s applies") {
		t.Fatalf("expected the concrete grace period in the modal:\n%s", view)
	}
	if strings.Contains(view, "default grace period applies") {
		t.Fatalf("expected the generic fallback text to be gone once a real value is known:\n%s", view)
	}
}

// TestCtrlDNonProdShowsInlinePromptAndDeletesOnY exercises 8b's non-prod
// path from browse: the table stays visible (no modal body override), the
// keybar becomes the y/N prompt, and 'y' deletes.
func TestCtrlDNonProdShowsInlinePromptAndDeletesOnY(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected ctrl+d in a non-prod context to open the inline prompt, tier=%v", m.actions.Tier())
	}
	// The table itself must still be visible — TierInline never overrides
	// Body().
	if !strings.Contains(plain(m.Render()), "api-0") {
		t.Fatalf("expected the table to stay visible under an inline confirm:\n%s", plain(m.Render()))
	}
	kb := m.Keybar()
	if kb.RightNote == "" || !strings.Contains(kb.RightNote, "api-0") {
		t.Fatalf("expected the keybar prompt to name the target, got %q", kb.RightNote)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.deleted) != 1 || mut.deleted[0] != "api-0" {
		t.Fatalf("deleted = %v, want [api-0]", mut.deleted)
	}
}

// TestCtrlDNoOpsWhileOffline pins the 4a fix: OFFLINE's keybar note ("mutating
// actions disabled") must actually be enforced, not just displayed — ctrl+d
// must neither open the confirm prompt nor reach the mutator while the
// connection is mid-outage.
func TestCtrlDNoOpsWhileOffline(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "boom"})
	if !m.offline() {
		t.Fatal("expected offline() = true after a Reconnecting ConnStateMsg")
	}

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if m.actions.Active() {
		t.Fatal("ctrl+d must not open a confirm prompt while offline")
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("mutator must not run while offline, got %v", mut.deleted)
	}

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected})
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() {
		t.Fatal("expected ctrl+d to work again once back online")
	}
}

// TestCtrlDProdOpensTypeNameModal exercises 8b's PROD escalation: the modal
// covers the body, enter no-ops until the name is typed, esc cancels.
func TestCtrlDProdOpensTypeNameModal(t *testing.T) {
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
	if !m.actions.Active() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected ctrl+d in a prod context to open the type-the-name modal, tier=%v", m.actions.Tier())
	}
	view := plain(m.Render())
	if !strings.Contains(view, "PROD CONTEXT") {
		t.Fatalf("expected the PROD CONTEXT tag in the modal:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 0 {
		t.Fatalf("expected enter to no-op before the name matches: %v", mut.deleted)
	}
	for _, r := range "api-0" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 1 || mut.deleted[0] != "api-0" {
		t.Fatalf("deleted = %v, want [api-0]", mut.deleted)
	}
}

// TestProdModalCaretFollowsArrowKeys is the regression test for the caret
// that never moved: left/right/Home/End always reached the type-ahead
// buffer (updateModalConfirmKey routes every unclaimed key to
// HandleTypeKey), but the modal drew "█" pinned to the end of the string, so
// the motion was invisible. Asserted through the real key path rather than
// on the controller, since the modal's own key routing is what has to leave
// these keys alone instead of treating them as list navigation.
func TestProdModalCaretFollowsArrowKeys(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{sess.Location.Context}}
	m := New(Config{Session: sess, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	for _, r := range "api-0" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	if !strings.Contains(plain(m.Render()), "api-0█") {
		t.Fatalf("expected the caret parked at the end of the buffer:\n%s", plain(m.Render()))
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := m.actions.TypedCursor(); got != 4 {
		t.Fatalf("TypedCursor() after left = %d, want 4 — the modal swallowed the key", got)
	}
	// plain() downsamples to NoTTY, which drops the reverse-video caret
	// entirely; the visible proof here is that the trailing block is gone
	// while the buffer itself is untouched.
	view := plain(m.Render())
	if strings.Contains(view, "api-0█") {
		t.Errorf("caret still pinned to the end of the buffer after left:\n%s", view)
	}
	if !strings.Contains(view, "api-0") {
		t.Errorf("left arrow changed the typed name:\n%s", view)
	}

	// The arrow must not have leaked through to the list underneath.
	if m.actions.TypedName() != "api-0" {
		t.Errorf("TypedName() = %q, want %q", m.actions.TypedName(), "api-0")
	}

	// ...and enter still matches, so moving the caret can't strand a
	// correctly-typed name.
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if !m.actions.NameMatches() {
		t.Error("expected the typed name to still match after caret motion")
	}
}

var forceKey = tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}

// TestProdModalForceKeyMatchesItsHint is the M7 regression: the PROD
// type-the-name modal advertises the force chord from verbs.ForceDelete, and
// pressing exactly that chord must escalate — it used to print "ctrl-k" while
// the handler only matched a literal "C".
func TestProdModalForceKeyMatchesItsHint(t *testing.T) {
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
	if view := plain(m.Render()); !strings.Contains(view, verbs.ForceDeleteModalHint()) {
		t.Fatalf("expected the modal to advertise %q:\n%s", verbs.ForceDeleteModalHint(), view)
	}
	if forceKey.String() != verbs.ForceDelete.Key {
		t.Fatalf("test chord %q drifted from verbs.ForceDelete.Key %q", forceKey.String(), verbs.ForceDelete.Key)
	}
	m = step(t, m, forceKey)
	if got := m.actions.Pending().Scope.Verb; got != "force-delete" {
		t.Fatalf("verb = %q after the advertised chord, want force-delete", got)
	}
	for _, r := range "api-0" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.forceDeleted) != 1 || mut.forceDeleted[0] != "api-0" {
		t.Fatalf("forceDeleted = %v, want [api-0]", mut.forceDeleted)
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("expected the plain delete path untouched, got %v", mut.deleted)
	}
}

// TestNonProdForceDeleteReachesTypeNameModal is the H2 regression: outside
// PROD, the force chord on an inline Pod delete must open the type-the-name
// modal (force delete is always modal), never stage a second inline "y".
func TestNonProdForceDeleteReachesTypeNameModal(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected the plain non-prod delete to stay inline, got %v", m.actions.Tier())
	}
	found := false
	for _, h := range m.Keybar().Groups[0] {
		if h == verbs.ForceDelete.Hint() {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the force-delete hint in the inline confirm's Groups, got %+v", m.Keybar().Groups)
	}

	m = step(t, m, forceKey)
	if m.actions.Tier() != actions.TierModal || !m.typingConfirmName() {
		t.Fatalf("expected the force chord to open the type-the-name modal, tier=%v", m.actions.Tier())
	}
	view := plain(m.Render())
	if !strings.Contains(view, "Force delete api-0?") || strings.Contains(view, "PROD CONTEXT") {
		t.Fatalf("expected a non-prod force-delete modal:\n%s", view)
	}

	// The old inline path: a bare "y" must now do nothing.
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.forceDeleted) != 0 || len(mut.deleted) != 0 {
		t.Fatalf("expected nothing to run before the name is typed, deleted=%v forceDeleted=%v", mut.deleted, mut.forceDeleted)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	for _, r := range "api-0" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.forceDeleted) != 1 || mut.forceDeleted[0] != "api-0" || len(mut.deleted) != 0 {
		t.Fatalf("deleted=%v forceDeleted=%v, want one force delete of api-0", mut.deleted, mut.forceDeleted)
	}
}

// TestForceDeleteModalEscCancels: esc after escalating ends the whole confirm.
func TestForceDeleteModalEscCancels(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	m = step(t, m, forceKey)
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.actions.Active() {
		t.Fatal("expected esc to cancel the force-delete modal outright")
	}
	if len(mut.deleted) != 0 || len(mut.forceDeleted) != 0 {
		t.Fatalf("expected nothing to execute after esc, deleted=%v forceDeleted=%v", mut.deleted, mut.forceDeleted)
	}
}

// TestNamespaceDeleteNonProdOpensTypeNameModal is the H3 regression for a
// single row: a Namespace takes everything inside it, so its delete gets
// the type-the-name modal even outside PROD, like a CRD's.
func TestNamespaceDeleteNonProdOpensTypeNameModal(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNamespace: {namespace("team-a"), namespace("team-b")},
	}}
	mut := &fakeMutator{}
	sess := newSession()
	sess.Location.Kind = kube.KindNamespace
	m := New(Config{Session: sess, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierModal || !m.typingConfirmName() {
		t.Fatalf("expected the type-the-name modal for a non-prod Namespace delete, tier=%v", m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 0 {
		t.Fatalf("expected y/enter to run nothing before the name is typed, got %v", mut.deleted)
	}
}

// TestNodeDeleteNonProdOpensTypeNameModal is review 05 M4: deleting a Node
// makes PodGC remove every pod bound to it — more than a drain, which is
// always modal — so its delete gets the type-the-name modal outside PROD
// too, never the inline y/N.
func TestNodeDeleteNonProdOpensTypeNameModal(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("worker-1", true, false), nodeObj("worker-2", true, false)},
	}}
	mut := &fakeMutator{}
	sess := newSession()
	sess.Location.Kind = kube.KindNode
	m := New(Config{Session: sess, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierModal || !m.typingConfirmName() {
		t.Fatalf("expected the type-the-name modal for a non-prod Node delete, tier=%v", m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 0 {
		t.Fatalf("expected y/enter to run nothing before the name is typed, got %v", mut.deleted)
	}
}
