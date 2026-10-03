package browse

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui/actions"
)

// TestBulkSuspendNonProdNamesEveryTarget pins 0.8.0 §3 Phase 5 task 8: a
// marked-set 's' suspends every marked CronJob behind one confirmation,
// direction chosen from the cursor row, non-PROD staying the ordinary
// TierInline y/N.
func TestBulkSuspendNonProdNamesEveryTarget(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "a"), cronJobObj("default", "b")},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(160, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"}) // mark all
	if len(m.marks) != 2 {
		t.Fatalf("expected 2 marks, got %d", len(m.marks))
	}

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected a bulk suspend to open the inline prompt, tier=%v", m.actions.Tier())
	}
	pending := m.actions.Pending()
	if pending == nil || len(pending.Scope.BulkTargets) != 2 {
		t.Fatalf("expected 2 bulk targets staged, got %+v", pending)
	}
	kb := m.Keybar()
	if !strings.Contains(kb.RightNote, "default/a") || !strings.Contains(kb.RightNote, "default/b") {
		t.Fatalf("expected both targets named in the confirm, got %q", kb.RightNote)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.cronJobSuspends) != 2 {
		t.Fatalf("cronJobSuspends = %v, want 2 entries", mut.cronJobSuspends)
	}
	if m.marks != nil {
		t.Fatalf("expected marks cleared after a fully successful bulk suspend, got %v", m.marks)
	}
}

// TestBulkSuspendProdUsesTypeCountGrammar pins task 8's PROD escalation: the
// type-the-count modal, not the single-target type-the-name one.
func TestBulkSuspendProdUsesTypeCountGrammar(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "a"), cronJobObj("default", "b")},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	session.Config = config.Config{ProdContexts: []string{session.Location.Context}}
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(160, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"})
	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected a PROD bulk suspend to escalate to TierModal, tier=%v", m.actions.Tier())
	}
	if !m.pendingBulkCronJobSuspend() {
		t.Fatalf("expected pendingBulkCronJobSuspend to recognize the marked-set confirm")
	}
	view := plain(m.Render())
	if strings.Contains(view, "type the") && strings.Contains(view, "name") {
		// Weak guard: the count modal never asks for a resource name.
		t.Fatalf("expected the type-the-count modal, not type-the-name:\n%s", view)
	}

	// Enter is a no-op until the typed digits equal the marked count (2).
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if len(mut.cronJobSuspends) != 0 {
		t.Fatalf("expected enter to no-op before the count matches: %v", mut.cronJobSuspends)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "2"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if len(mut.cronJobSuspends) != 2 {
		t.Fatalf("cronJobSuspends = %v, want 2 entries once the count matches", mut.cronJobSuspends)
	}
}

// TestBulkResumeStagesPreviewThenExecutesOnEnter pins task 9: bulk resume
// uses one Enter preflight (the reversible direction), never a destructive
// escalation even in PROD.
func TestBulkResumeStagesPreviewThenExecutesOnEnter(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {
			suspendedCronJobObj("default", "a", true),
			suspendedCronJobObj("default", "b", true),
		},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	session.Config = config.Config{ProdContexts: []string{session.Location.Context}}
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(160, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"})
	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if m.pendingCronJobResume == nil || len(m.pendingCronJobResume.Entries) != 2 {
		t.Fatalf("expected a 2-entry bulk resume preview staged, got %+v", m.pendingCronJobResume)
	}
	if m.actions.Active() {
		t.Fatalf("expected no destructive escalation for resume, even in PROD")
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if len(mut.cronJobSuspends) != 2 {
		t.Fatalf("cronJobSuspends = %v, want 2 entries", mut.cronJobSuspends)
	}
	for _, entry := range mut.cronJobSuspends {
		if !strings.HasSuffix(entry, "=false") {
			t.Fatalf("expected every bulk target resumed (=false), got %v", mut.cronJobSuspends)
		}
	}
}

// TestBulkSuspendSkipsRowsAlreadyInTargetState pins task 7: a marked set
// with a mix of suspended/unsuspended rows only acts on the ones that
// aren't already in the direction's target state.
func TestBulkSuspendSkipsRowsAlreadyInTargetState(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {
			cronJobObj("default", "a"),                // not suspended
			suspendedCronJobObj("default", "b", true), // already suspended
		},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(160, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "*"}) // marks both; cursor still on "a" (not suspended)
	m = step(t, m, tea.KeyPressMsg{Text: "s"}) // direction: suspend (cursor row)
	pending := m.actions.Pending()
	if pending == nil || len(pending.Scope.BulkTargets) != 1 || pending.Scope.BulkTargets[0].ResourceName != "a" {
		t.Fatalf("expected only the not-yet-suspended row targeted, got %+v", pending)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.cronJobSuspends) != 1 || mut.cronJobSuspends[0] != "default/a=true" {
		t.Fatalf("expected only default/a suspended, got %v", mut.cronJobSuspends)
	}
}

// TestCronJobSuspendDangerNoteNamesActiveJobs pins §36c: the suspend confirm
// names any currently-active associated Jobs and states they're unaffected.
func TestCronJobSuspendDangerNoteNamesActiveJobs(t *testing.T) {
	cj := cronJobObj("default", "backup")
	active := jobObj("default", "backup-29310612")
	active.OwnerReferences = []metav1.OwnerReference{controllerRefFor(cj)}
	active.Status.Active = 1
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cj},
		kube.KindJob:     {active},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(160, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	kb := m.Keybar()
	if !strings.Contains(kb.RightNote, "backup-29310612") {
		t.Fatalf("expected the active job named in the confirm, got %q", kb.RightNote)
	}
	if !strings.Contains(kb.RightNote, "unaffected") {
		t.Fatalf("expected the running-jobs-unaffected note, got %q", kb.RightNote)
	}
}
