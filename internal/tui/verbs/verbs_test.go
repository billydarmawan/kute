package verbs

import (
	"strings"
	"testing"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
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

// TestTtyHandoffVerbsAreGatedOffline pins docs/design README.md §4a's
// "delete/exec/edit verbs are disabled while offline" for the three verbs
// that reach the cluster through a kubectl subprocess rather than
// kube.Mutator: exec hands the user a prompt they can write from, node
// debug creates a privileged debugger pod before they type anything, and edit
// applies whatever they save. Forward stays exempt — a port-forward is a
// local session, not a cluster write.
func TestTtyHandoffVerbsAreGatedOffline(t *testing.T) {
	t.Parallel()

	for _, v := range []Verb{Exec, NodeDebug, NodeDebugDetail, PodDebug, Edit} {
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
		Goto, Filter, Open, Logs, YAML, Exec, NodeDebug, NodeDebugDetail, PodDebug, Edit, Events,
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

// TestCronJobSetScheduleIsPureNavigation pins decision §3 task 5/10:
// CronJobSetSchedule stopped being a mutating verb (the SetImage/Scale
// shape) and became pure navigation to the pushed editor — Mutating: false.
// Its policy row (TierNone) is what the editor's apply begins under.
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
