package verbs

import (
	"strings"
	"testing"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

func TestHint(t *testing.T) {
	t.Parallel()
	got := Delete.Hint()
	want := tui.KeyHint{Key: "D", Label: "delete"}
	if got != want {
		t.Fatalf("Hint() = %+v, want %+v", got, want)
	}
}

func TestEditHint(t *testing.T) {
	t.Parallel()
	got := Edit.Hint()
	want := tui.KeyHint{Key: "E", Label: "edit"}
	if got != want {
		t.Fatalf("Hint() = %+v, want %+v", got, want)
	}
}

// TestHiddenWhileOffline pins the cross-cutting fix for the dead Mutating
// field: HiddenWhileOffline is the one real (non-test) consumer, called by
// poddetail/nodedetail/helmhistory/objectdetail's Keybar() instead of each
// hardcoding which verbs need the offline gate.
func TestHiddenWhileOffline(t *testing.T) {
	t.Parallel()
	if !Delete.HiddenWhileOffline(true) {
		t.Fatal("expected a Mutating verb hidden while offline")
	}
	if Delete.HiddenWhileOffline(false) {
		t.Fatal("expected a Mutating verb shown while online")
	}
	if Goto.HiddenWhileOffline(true) {
		t.Fatal("expected a non-Mutating verb never hidden, even offline")
	}
}

func TestAppliesTo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		verb Verb
		kind kube.ResourceKind
		want bool
	}{
		{"nil kinds applies to any kind", Delete, kube.KindConfigMap, true},
		{"restricted kinds match", Logs, kube.KindPod, true},
		{"restricted kinds reject others", Logs, kube.KindNode, false},
		{"drain restricted to node", Drain, kube.KindNode, true},
		{"drain rejects pod", Drain, kube.KindPod, false},
		{"set image applies to cronjob", SetImage, kube.KindCronJob, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.verb.AppliesTo(tt.kind); got != tt.want {
				t.Fatalf("AppliesTo(%s) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

func TestByID(t *testing.T) {
	t.Parallel()

	if v, ok := ByID("delete"); !ok || v.ID != "delete" {
		t.Fatalf("ByID(delete) = %+v, %v", v, ok)
	}
	if _, ok := ByID("does-not-exist"); ok {
		t.Fatalf("ByID(does-not-exist) unexpectedly found")
	}
}

func TestTiers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		verb Verb
		want actions.Tier
	}{
		{Delete, actions.TierInline},
		{ForceDelete, actions.TierModal},
		{Drain, actions.TierModal},
		{RolloutRestart, actions.TierInline},
		{Cordon, actions.TierNone},
	}
	for _, tt := range tests {
		if tt.verb.Tier != tt.want {
			t.Errorf("%s tier = %v, want %v", tt.verb.ID, tt.verb.Tier, tt.want)
		}
	}
}

func TestMutatingVerbsCoverAllRegisteredWriteOps(t *testing.T) {
	t.Parallel()

	// Mutating verbs are expected to be tiered (an inline y/N or a
	// type-the-name modal). These are the deliberate exceptions: reversible-
	// and-immediate verbs whose own editor panel is the confirmation step,
	// plus the three tty-handoff verbs, which have no Tier because kubectl
	// owns the session once kute suspends (edit's PROD y/N comes from
	// TierForEdit instead).
	untiered := map[string]bool{
		"cordon": true, "scale": true, "set-image": true, "set-resources": true,
		"meta": true, "add-secret-key": true, "add-configmap-key": true,
		"restart-configmap-consumers": true,
		"exec":                        true, "node-debug": true, "node-debug-detail": true, "edit": true,
		// §30a. Suspend is Cordon's exact shape: reversible and immediate,
		// and resume restores the prior state exactly. Reconcile requests
		// the sync Flux would have run on its own interval anyway, so it
		// makes no change the controller wasn't already going to make —
		// the one property that lets it keep a bare letter (see
		// FluxReconcile's own comment for why that doesn't generalize).
		"flux-suspend": true, "flux-reconcile": true,
		// §36c. CronJobSuspend's own .Tier is nominal now (SetImage's own
		// nominal-Tier shape), matching resume's TierNone default — the real
		// per-direction/PROD tier comes from TierForCronJobSuspend, which
		// gives suspend real friction (TierInline/TierModal) while resume
		// stays this reversible-and-immediate TierNone, restoring the prior
		// state exactly (see CronJobSuspend's own doc comment). CronJobSetSchedule
		// isn't listed here any more — it's pure navigation now (Mutating:
		// false), so it never reaches this loop's condition at all.
		"cronjob-suspend": true,
		// §36b. CronJobRunNow is TierNone for the same reason: the run-now
		// preflight browse stages before ever calling Begin (cronjob_actions.go's
		// pendingCronJobRun — overlap warning, generated name) is itself the
		// confirmation, so there's nothing left for actions.Controller's own
		// TierInline/TierModal to add by the time Begin executes.
		"cronjob-run-now": true,
		// §36d. CronJobScheduleUndo is itself "an ordinary reversible
		// mutation through the same registry" (docs/design README.md §36d) —
		// applying the previous accepted schedule/timezone pair is exactly as
		// reversible as the apply it undoes, TierNone the same way. Its own
		// pushed screen (tasks/cronjobschedule) is the confirmation step,
		// same as CronJobSetSchedule's editor is for the apply itself.
		// CronJobScheduleFullEdit is a tty-handoff verb, same shape as
		// exec/node-shell/edit above: no Tier because kubectl owns the
		// session once kute suspends.
		"cronjob-schedule-undo": true, "cronjob-schedule-full-edit": true,
		// §33a. ArgoRefresh is FluxReconcile's exact shape: it asks
		// argocd-application-controller to re-diff early, the same re-diff
		// it already does on its own poll interval. ArgoSync is not
		// listed here — it's TierInline, a real change to what's deployed
		// (see ArgoSync's own doc comment).
		"argo-refresh": true,
		// v0.9.0 §37c. JobRetry ("rerun"'s create path) is CronJobRunNow's
		// exact shape one level down: the staged choice browse/jobattempts
		// both gather (job_actions.go/rerun.go's pendingJobRerun/
		// pendingRerun — generated name, amber diagnostic) is itself the
		// confirmation, so there's nothing left for actions.Controller's own
		// TierInline/TierModal to add by the time Begin executes. JobReplace
		// (the same staged choice's destructive alternative) is deliberately
		// NOT listed here — it stays TierInline/TierModal, reached every
		// time it's chosen (see JobReplace's own doc comment).
		"job-retry": true,
	}
	for _, v := range All {
		if v.Mutating && v.Tier == actions.TierNone && !untiered[v.ID] {
			t.Errorf("%s is mutating with TierNone but isn't an allow-listed reversible verb", v.ID)
		}
	}
}

func TestTierForEscalatesInlineToModalInProd(t *testing.T) {
	t.Parallel()

	if got := TierFor(Delete, false); got != actions.TierInline {
		t.Errorf("TierFor(Delete, non-prod) = %v, want TierInline", got)
	}
	if got := TierFor(Delete, true); got != actions.TierModal {
		t.Errorf("TierFor(Delete, prod) = %v, want TierModal (escalated)", got)
	}
	if got := TierFor(RolloutRestart, false); got != actions.TierInline {
		t.Errorf("TierFor(RolloutRestart, non-prod) = %v, want TierInline", got)
	}
	if got := TierFor(RolloutRestart, true); got != actions.TierModal {
		t.Errorf("TierFor(RolloutRestart, prod) = %v, want TierModal (escalated)", got)
	}
}

func TestTierForEdit(t *testing.T) {
	t.Parallel()

	if got := TierForEdit(false); got != actions.TierNone {
		t.Errorf("TierForEdit(non-prod) = %v, want TierNone", got)
	}
	if got := TierForEdit(true); got != actions.TierInline {
		t.Errorf("TierForEdit(prod) = %v, want TierInline", got)
	}
}

func TestTierForSetImage(t *testing.T) {
	t.Parallel()

	if got := TierForSetImage(false); got != actions.TierNone {
		t.Errorf("TierForSetImage(non-prod) = %v, want TierNone", got)
	}
	if got := TierForSetImage(true); got != actions.TierInline {
		t.Errorf("TierForSetImage(prod) = %v, want TierInline", got)
	}
}

func TestTierForSetResources(t *testing.T) {
	t.Parallel()

	if got := TierForSetResources(false); got != actions.TierNone {
		t.Errorf("TierForSetResources(non-prod) = %v, want TierNone", got)
	}
	if got := TierForSetResources(true); got != actions.TierInline {
		t.Errorf("TierForSetResources(prod) = %v, want TierInline", got)
	}
}

func TestTierForLeavesNonInlineVerbsAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		verb Verb
		want actions.Tier
	}{
		{Drain, actions.TierModal},
		{ForceDelete, actions.TierModal},
		{Cordon, actions.TierNone},
	}
	for _, tt := range tests {
		for _, isProd := range []bool{false, true} {
			if got := TierFor(tt.verb, isProd); got != tt.want {
				t.Errorf("TierFor(%s, isProd=%v) = %v, want %v (unaffected by prod)", tt.verb.ID, isProd, got, tt.want)
			}
		}
	}
}

// TestTtyHandoffVerbsAreGatedOffline pins docs/design README.md §4a's
// "delete/exec/edit verbs are disabled while offline" for the three verbs
// that reach the cluster through a kubectl subprocess rather than
// kube.Mutator: exec hands the user a prompt they can write from, node
// debug creates a privileged debugger pod before they type anything, and edit
// applies whatever they save. Forward stays exempt — a port-forward is a
// local session, not a cluster write.
func TestTtyHandoffVerbsAreGatedOffline(t *testing.T) {
	t.Parallel()

	for _, v := range []Verb{Exec, NodeDebug, NodeDebugDetail, Edit} {
		if !v.Mutating {
			t.Errorf("%s should be Mutating so the OFFLINE gate reaches it", v.ID)
		}
		if !v.HiddenWhileOffline(true) {
			t.Errorf("%s should be hidden/refused while offline", v.ID)
		}
		if v.HiddenWhileOffline(false) {
			t.Errorf("%s should be available while connected", v.ID)
		}
	}
	if Forward.HiddenWhileOffline(true) {
		t.Error("Forward should stay available while offline — a local session, not a write")
	}
}

// TestAllRegistersEveryDefinedVerb pins 0.8.0 plan §3 Phase 3 task 7: a verb
// defined as a package-level var but missing from All silently breaks
// help/lookup consistency (ByID, the ? overlay's fixed columns) while still
// working at every call site that references the var directly — exactly how
// JobRetry/JobSuspend went unnoticed. Checked by ID rather than by counting,
// so it stays correct as more verbs are added.
func TestAllRegistersEveryDefinedVerb(t *testing.T) {
	t.Parallel()

	defined := []Verb{
		Goto, Filter, Open, Logs, YAML, Exec, NodeDebug, NodeDebugDetail, Edit, Events,
		Namespace, Context, AllNamespaces, JumpNamespace, ToggleGroup, PageUp, PageDown, Help, Retry, WhoCan,
		HelmValues, HelmHistory, HelmPods, Mark, MarkAll,
		FluxReconcile, FluxSuspend, FluxSource,
		ArgoRefresh, ArgoSync, ArgoURL,
		CertRenew,
		Delete, ForceDelete, RolloutRestart, JobRetry, JobSuspend,
		CronJobRunNow, CronJobSuspend, CronJobSetSchedule,
		CronJobCopyCommand, CronJobScheduleCopy, CronJobFocusTimezone, CronJobScheduleUndo, CronJobScheduleFullEdit,
		Cordon, Drain, Rollback, RolloutUndo, Scale, SetImage, SetResources, Meta,
		AddSecretKey, RemoveSecretKey,
		AddConfigMapKey, RemoveConfigMapKey, RestartConfigMapConsumers,
		Forward, StopForward, RestartForward, StopAllForwards, CopyForwardURL, CopyServiceClusterIP, CopyServiceExternalIP,
		CopyRouteURL, OpenParentGateway, CopyRouteYAML, FocusTLSStrip, OpenTLSSecret,
	}
	for _, v := range defined {
		if _, ok := ByID(v.ID); !ok {
			t.Errorf("%s is a defined verb but missing from All/ByID", v.ID)
		}
	}
}

// TestJobRetryAndJobSuspendRegistered is the narrow regression pin for the
// specific bug task 7 found: JobRetry/JobSuspend (used directly by
// browse/jobs.go) were defined but never added to All.
func TestJobRetryAndJobSuspendRegistered(t *testing.T) {
	t.Parallel()

	if v, ok := ByID("job-retry"); !ok || v.Key != JobRetry.Key {
		t.Fatalf("ByID(job-retry) = %+v, %v, want %+v", v, ok, JobRetry)
	}
	if v, ok := ByID("job-suspend"); !ok || v.Key != JobSuspend.Key {
		t.Fatalf("ByID(job-suspend) = %+v, %v, want %+v", v, ok, JobSuspend)
	}
}

// TestTierForCronJobSuspend pins §36c's asymmetric-friction shape: resume is
// always TierNone regardless of PROD, suspend is TierInline outside PROD and
// escalates to TierModal inside it — the same PROD axis TierFor gives
// Inline verbs, but resolved per-direction since CronJobSuspend's own
// Verb.Tier is only the nominal (resume) default.
func TestTierForCronJobSuspend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		suspending bool
		isProd     bool
		want       actions.Tier
	}{
		{"resume non-prod", false, false, actions.TierNone},
		{"resume prod", false, true, actions.TierNone},
		{"suspend non-prod", true, false, actions.TierInline},
		{"suspend prod", true, true, actions.TierModal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := TierForCronJobSuspend(tt.suspending, tt.isProd); got != tt.want {
				t.Errorf("TierForCronJobSuspend(%v, %v) = %v, want %v", tt.suspending, tt.isProd, got, tt.want)
			}
		})
	}
}

// TestCronJobSetScheduleIsPureNavigation pins decision §3 task 5/10:
// CronJobSetSchedule stopped being a nominal-Tier mutating verb (the
// SetImage/Scale shape) and became pure navigation to the pushed editor —
// Mutating: false, so it never needs a TierFor* resolver of its own and
// never appears in TestMutatingVerbsCoverAllRegisteredWriteOps's allow-list.
func TestCronJobSetScheduleIsPureNavigation(t *testing.T) {
	t.Parallel()

	if CronJobSetSchedule.Mutating {
		t.Error("CronJobSetSchedule should be pure navigation (Mutating: false) — the pushed editor applies the actual patch")
	}
}

// TestCronJobScheduleModeVerbsRegistered pins task 6: the schedule editor's
// mode-specific keys (copy/timezone/undo/full-edit) are registered ahead of
// the screen that uses them (Phase 6), with the exact keys/labels docs/
// design README.md §36d names.
func TestCronJobScheduleModeVerbsRegistered(t *testing.T) {
	t.Parallel()

	tests := []struct {
		verb  Verb
		key   string
		label string
	}{
		{CronJobCopyCommand, "y", "copy"},
		{CronJobScheduleCopy, "ctrl+t", "copy"},
		{CronJobFocusTimezone, "tab", "timezone"},
		{CronJobScheduleUndo, "ctrl+z", "undo"},
		{CronJobScheduleFullEdit, "ctrl+y", "full yaml edit"},
	}
	for _, tt := range tests {
		if tt.verb.Key != tt.key || tt.verb.Label != tt.label {
			t.Errorf("%s = key %q label %q, want key %q label %q", tt.verb.ID, tt.verb.Key, tt.verb.Label, tt.key, tt.label)
		}
	}
	if !CronJobScheduleUndo.Mutating {
		t.Error("CronJobScheduleUndo should be Mutating — it applies the previous schedule/timezone through the mutator")
	}
	if !CronJobScheduleFullEdit.Mutating {
		t.Error("CronJobScheduleFullEdit should be Mutating — same tty-handoff shape as Edit")
	}
	if CronJobCopyCommand.Mutating || CronJobScheduleCopy.Mutating || CronJobFocusTimezone.Mutating {
		t.Error("copy/focus-timezone are read-only UI actions, not mutations")
	}
}

// TestLogsAppliesToCronJob pins task 9: Logs' Kinds now includes CronJob
// (§36a: "l opens logs for the newest useful Pod of the selected row's most
// recent associated Job"), alongside its original Pod applicability.
func TestLogsAppliesToCronJob(t *testing.T) {
	t.Parallel()

	if !Logs.AppliesTo(kube.KindCronJob) {
		t.Error("Logs should apply to CronJob rows")
	}
	if !Logs.AppliesTo(kube.KindPod) {
		t.Error("Logs should still apply to Pod rows")
	}
	if Logs.AppliesTo(kube.KindNode) {
		t.Error("Logs should not apply to Node rows")
	}
}

// TestTierForDelete pins the delete family's whole confirm policy in one
// table: force delete is always modal, and a Namespace, CRD or Node delete —
// which removes far more than the row — is always modal too, PROD or not.
func TestTierForDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		verb   Verb
		kind   kube.ResourceKind
		isProd bool
		want   actions.Tier
	}{
		{Delete, kube.KindPod, false, actions.TierInline},
		{Delete, kube.KindPod, true, actions.TierModal},
		{Delete, kube.KindDeployment, false, actions.TierInline},
		{Delete, kube.KindNamespace, false, actions.TierModal},
		{Delete, kube.KindNamespace, true, actions.TierModal},
		{Delete, kube.KindCustomResourceDefinition, false, actions.TierModal},
		{Delete, kube.KindCustomResourceDefinition, true, actions.TierModal},
		{Delete, kube.KindNode, false, actions.TierModal},
		{Delete, kube.KindNode, true, actions.TierModal},
		{ForceDelete, kube.KindPod, false, actions.TierModal},
		{ForceDelete, kube.KindPod, true, actions.TierModal},
	}
	for _, tt := range tests {
		if got := TierForDelete(tt.verb, tt.kind, tt.isProd); got != tt.want {
			t.Errorf("TierForDelete(%s, %s, prod=%v) = %v, want %v", tt.verb.ID, tt.kind, tt.isProd, got, tt.want)
		}
	}
}

func podDeleteAction(kind kube.ResourceKind) tui.TaskAction {
	return tui.TaskAction{
		ID: "delete", Label: "Delete api?",
		Scope: tui.TaskScope{ResourceKind: string(kind), ResourceName: "api", Namespace: "dev", Verb: Delete.ID, IsMutating: true},
	}
}

type nopMutator struct{ kube.Mutator }

// TestEscalateForceDeleteIsModalOutsideProd: escalating a non-prod inline
// Pod delete must reach the type-the-name modal, never stay an inline y/N.
func TestEscalateForceDeleteIsModalOutsideProd(t *testing.T) {
	t.Parallel()

	c := actions.New(nopMutator{})
	c.Begin(TierForDelete(Delete, kube.KindPod, false), podDeleteAction(kube.KindPod))
	if !EscalateForceDelete(&c, false) {
		t.Fatal("expected EscalateForceDelete to claim a pending Pod delete")
	}
	if c.Tier() != actions.TierModal || c.Pending().Scope.Verb != ForceDelete.ID {
		t.Fatalf("tier=%v verb=%q, want TierModal force-delete", c.Tier(), c.Pending().Scope.Verb)
	}
	if !actions.RequiresTypedName(c.Pending().Scope.Verb) {
		t.Fatal("force-delete's modal must be type-the-name")
	}
}

func TestEscalateForceDeleteIgnoresOtherKindsAndVerbs(t *testing.T) {
	t.Parallel()

	c := actions.New(nopMutator{})
	c.Begin(actions.TierInline, podDeleteAction(kube.KindDeployment))
	if EscalateForceDelete(&c, false) || c.Pending().Scope.Verb != Delete.ID || c.Tier() != actions.TierInline {
		t.Fatalf("expected a Deployment delete to stay an inline delete, got tier=%v verb=%q", c.Tier(), c.Pending().Scope.Verb)
	}

	drain := podDeleteAction(kube.KindNode)
	drain.Scope.Verb = Drain.ID
	c.Begin(actions.TierModal, drain)
	if EscalateForceDelete(&c, false) || c.Pending().Scope.Verb != Drain.ID {
		t.Fatalf("expected a drain to be left alone, got %q", c.Pending().Scope.Verb)
	}
}

// TestForceDeleteKeyIsAChord: the force chord is matched inside the
// type-the-name modal's text buffer, so it must not be a bare printable key.
func TestForceDeleteKeyIsAChord(t *testing.T) {
	t.Parallel()

	if !strings.HasPrefix(ForceDelete.Key, "ctrl+") {
		t.Fatalf("ForceDelete.Key = %q, want a ctrl chord", ForceDelete.Key)
	}
	if !strings.Contains(ForceDeleteModalHint(), ForceDelete.Key) {
		t.Fatalf("ForceDeleteModalHint() = %q, does not name %q", ForceDeleteModalHint(), ForceDelete.Key)
	}
}
