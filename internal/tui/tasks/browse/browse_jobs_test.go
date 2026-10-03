package browse

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui/actions"
)

func ptr32(v int32) *int32 { return &v }

func jobObj(ns, name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       batchv1.JobSpec{Completions: ptr32(1)},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
}

func suspendedJobObj(ns, name string, suspend bool) *batchv1.Job {
	j := jobObj(ns, name)
	j.Spec.Suspend = &suspend
	return j
}

func cronJobObj(ns, name string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       batchv1.CronJobSpec{Schedule: "*/5 * * * *"},
	}
}

// suspendedCronJobObj mirrors suspendedJobObj for CronJob.
func suspendedCronJobObj(ns, name string, suspend bool) *batchv1.CronJob {
	cj := cronJobObj(ns, name)
	cj.Spec.Suspend = &suspend
	return cj
}

// TestEnterOnJobPushesAttemptsWithNamespaceQualifiedSiblings pins §37a: ↵ on
// a Job row pushes tasks/jobattempts (stubbed here) instead of the pre-
// v0.9.0 jump-straight-to-Pods shortcut, carrying every visible row's
// namespace-qualified ref plus the selected row's index — mirrors
// TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings' shape.
func TestEnterOnJobPushesAttemptsWithNamespaceQualifiedSiblings(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindJob: {
			jobObj("default", "batch-1"),
			jobObj("default", "batch-2"),
		},
	}}
	var gotNamespace, gotName string
	var gotSiblings []JobSiblingRef
	var gotIndex int
	session := newSession()
	session.Location.Kind = kube.KindJob
	m := New(Config{Session: session, Lister: lister, OpenJobAttempts: func(namespace, name string, siblings []JobSiblingRef, index, _, _ int) (tea.Model, tea.Cmd) {
		gotNamespace, gotName, gotSiblings, gotIndex = namespace, name, siblings, index
		return stubTask{}, nil
	}})
	m.SetSize(120, 36)
	// m.load()() rather than step(m.Init()()): Init's own Job branch also
	// schedules the recurring §37a clock tick (DURATION), which step's
	// synchronous cmd-draining would otherwise follow forever — the same
	// hazard TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings
	// documents for CronJob's own tick.
	m = step(t, m, m.load()())
	m.moveSelection(1)
	row, ok := m.selectedRow()
	if !ok || row.Name != "batch-2" {
		t.Fatalf("expected batch-2 selected before pressing enter, got %+v (ok=%v)", row, ok)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected enter to push the stub attempts task, got %T", updated)
	}
	if gotNamespace != "default" || gotName != "batch-2" {
		t.Fatalf("expected OpenJobAttempts(default, batch-2, ...), got (%q, %q)", gotNamespace, gotName)
	}
	wantSiblings := []JobSiblingRef{{Namespace: "default", Name: "batch-1"}, {Namespace: "default", Name: "batch-2"}}
	if len(gotSiblings) != len(wantSiblings) || gotSiblings[0] != wantSiblings[0] || gotSiblings[1] != wantSiblings[1] {
		t.Fatalf("siblings = %+v, want %+v", gotSiblings, wantSiblings)
	}
	if gotIndex != 1 {
		t.Fatalf("index = %d, want 1 (batch-2)", gotIndex)
	}
}

// TestEnterOnJobNoopsWithoutAttemptsWired mirrors
// TestEnterOnCronJobNoopsWithoutDetailWired: pressing enter stays harmless
// when app.go hasn't wired OpenJobAttempts.
func TestEnterOnJobNoopsWithoutAttemptsWired(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindJob: {jobObj("default", "batch-1")},
	}}
	session := newSession()
	session.Location.Kind = kube.KindJob
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	next, ok := updated.(*Model)
	if !ok {
		t.Fatalf("expected enter to stay on the browse model, got %T", updated)
	}
	if cmd != nil {
		t.Fatalf("expected no command, got one")
	}
	if next.kind != kube.KindJob {
		t.Fatalf("expected kind to stay Job, got %s", next.kind)
	}
}

// TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings pins 0.8.0
// plan Phase 4 task 14: ↵ on a CronJob row pushes tasks/cronjobdetail
// (stubbed here — the real screen doesn't exist until Phase 7) instead of
// the pre-0.8.0 jump-straight-to-Pods shortcut, carrying every visible row's
// namespace-qualified ref plus the selected row's index.
func TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {
			cronJobObj("default", "hourly"),
			cronJobObj("default", "nightly"),
		},
		kube.KindJob: {},
	}}
	var gotNamespace, gotName string
	var gotSiblings []CronJobSiblingRef
	var gotIndex int
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, OpenCronJobDetail: func(namespace, name string, siblings []CronJobSiblingRef, index, _, _ int) (tea.Model, tea.Cmd) {
		gotNamespace, gotName, gotSiblings, gotIndex = namespace, name, siblings, index
		return stubTask{}, nil
	}})
	m.SetSize(120, 36)
	// m.load()() rather than step(m.Init()()): Init's own CronJob branch also
	// schedules the recurring §36a clock tick (Phase 4 task 5), which step's
	// synchronous cmd-draining would otherwise follow forever — the same
	// hazard TestPodMetricsRenderAsBars documents for the metrics poll loop.
	m = step(t, m, m.load()())
	m.moveSelection(1)
	row, ok := m.selectedRow()
	if !ok || row.Name != "nightly" {
		t.Fatalf("expected nightly selected before pressing enter, got %+v (ok=%v)", row, ok)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected enter to push the stub detail task, got %T", updated)
	}
	if gotNamespace != "default" || gotName != "nightly" {
		t.Fatalf("expected OpenCronJobDetail(default, nightly, ...), got (%q, %q)", gotNamespace, gotName)
	}
	wantSiblings := []CronJobSiblingRef{{Namespace: "default", Name: "hourly"}, {Namespace: "default", Name: "nightly"}}
	if len(gotSiblings) != len(wantSiblings) || gotSiblings[0] != wantSiblings[0] || gotSiblings[1] != wantSiblings[1] {
		t.Fatalf("siblings = %+v, want %+v", gotSiblings, wantSiblings)
	}
	if gotIndex != 1 {
		t.Fatalf("index = %d, want 1 (nightly)", gotIndex)
	}
}

// TestEnterOnCronJobNoopsWithoutDetailWired pins that pressing enter stays
// harmless when app.go hasn't wired OpenCronJobDetail yet (true until Phase
// 7/8 land tasks/cronjobdetail) — the list stays exactly where it was rather
// than falling through to some other kind's enter routing.
func TestEnterOnCronJobNoopsWithoutDetailWired(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "nightly")},
		kube.KindJob:     {},
	}}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // see TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	next, ok := updated.(*Model)
	if !ok {
		t.Fatalf("expected enter to stay on the browse model, got %T", updated)
	}
	if cmd != nil {
		t.Fatalf("expected no command, got one")
	}
	if next.kind != kube.KindCronJob {
		t.Fatalf("expected kind to stay CronJob, got %s", next.kind)
	}
}

// TestRStagesJobRerunThenCreatesOnEnter pins §37c's staged contract: 'R'
// stages a create-vs-replace choice in screen state (mirrors
// TestCtrlRStagesRunNowThenTriggersOnEnter's shape for CronJob's own
// run-now), defaulting to "create" — non-destructive, staged-then-TierNone,
// no y/N needed. The generated name is "<name>-rerun-N", not the old
// Unix-timestamp/"-retry-" scheme.
func TestRStagesJobRerunThenCreatesOnEnter(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindJob: {jobObj("default", "batch-1")},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // see TestEnterOnJobPushesAttemptsWithNamespaceQualifiedSiblings

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if m.pendingJobRerun == nil {
		t.Fatalf("expected R to stage the rerun preview")
	}
	if m.actions.Active() {
		t.Fatalf("expected no actions.Controller confirm state while staged (staging is itself the confirmation for 'create')")
	}
	kb := m.Keybar()
	if !strings.Contains(kb.RightNote, "kubectl create job") || !strings.Contains(kb.RightNote, "--from=job/batch-1") {
		t.Fatalf("expected the will-run line in the keybar, got %q", kb.RightNote)
	}
	if !strings.Contains(kb.RightNote, "batch-1-rerun-1") {
		t.Fatalf("expected the generated name to use the -rerun-N scheme, got %q", kb.RightNote)
	}
	if len(mut.retriedJobs) != 0 {
		t.Fatalf("expected no retry before enter, got %v", mut.retriedJobs)
	}
	// No text buffer here, so j/k toggle the choice like ↑↓.
	m = step(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.pendingJobRerun.Choice != resources.JobRerunReplace {
		t.Fatalf("choice = %v after j, want replace", m.pendingJobRerun.Choice)
	}
	m = step(t, m, tea.KeyPressMsg{Code: 'k', Text: "k"})
	if m.pendingJobRerun.Choice != resources.JobRerunCreate {
		t.Fatalf("choice = %v after k, want create", m.pendingJobRerun.Choice)
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.pendingJobRerun != nil {
		t.Fatalf("expected enter to clear the staged preview")
	}
	if len(mut.retriedJobs) != 1 || mut.retriedJobs[0] != "default/batch-1->batch-1-rerun-1" {
		t.Fatalf("retriedJobs = %v, want one default/batch-1->batch-1-rerun-1 entry", mut.retriedJobs)
	}
}

// TestJobRerunReplaceChoiceGoesThroughOrdinaryConfirm pins §37c's other
// firm decision: staging only ever confirms the "create" branch — toggling
// to "replace" and pressing enter hands off to actions.Controller's
// ordinary tiered confirm (still needs 'y') rather than executing directly,
// because replace is genuinely destructive.
func TestJobRerunReplaceChoiceGoesThroughOrdinaryConfirm(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindJob: {jobObj("default", "batch-1")},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.pendingJobRerun != nil {
		t.Fatalf("expected the staged preview cleared once handed to the ordinary confirm")
	}
	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected replace to land on the ordinary TierInline confirm, tier=%v", m.actions.Tier())
	}
	if len(mut.replacedJobs) != 0 {
		t.Fatalf("expected no replace before 'y', got %v", mut.replacedJobs)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.replacedJobs) != 1 || mut.replacedJobs[0] != "default/batch-1" {
		t.Fatalf("replacedJobs = %v, want one default/batch-1 entry", mut.replacedJobs)
	}
}

// TestJobRerunStaysStagedEvenInProd mirrors
// TestCronJobRunNowStaysStagedEvenInProd: the "create" path is a clone into
// a new object, not destructive, so it never escalates to a PROD-only
// confirm — staging is the same in every context.
func TestJobRerunStaysStagedEvenInProd(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindJob: {jobObj("default", "batch-1")},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindJob
	session.Config = config.Config{ProdContexts: []string{session.Location.Context}}
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if m.pendingJobRerun == nil || m.actions.Active() {
		t.Fatalf("expected R to stage the preview with no actions.Controller confirm, even in PROD")
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if len(mut.retriedJobs) != 1 {
		t.Fatalf("expected enter to create immediately, got %v", mut.retriedJobs)
	}
}

// TestJobSuspendEscalatesToTypeNameModalConfirmInProd pins §37a's asymmetric
// tier — mirroring CronJobSuspend's own (36c) rather than the old symmetric
// TierInline both directions: unlike Retry/rerun-create, JobSuspend has a
// real destructive side effect (tears down the Job's active pods
// immediately), so a prod-tagged context escalates 's' from TierInline to
// TierModal — and, since job-suspend is now in
// Controller.TypeToConfirm (mirroring cronjob-suspend), that lands on the
// full type-the-name modal, not the plain ConfirmCard.
func TestJobSuspendEscalatesToTypeNameModalConfirmInProd(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindJob: {suspendedJobObj("default", "batch-1", false)},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindJob
	session.Config = config.Config{ProdContexts: []string{session.Location.Context}}
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected 's' in a prod context to escalate to TierModal, tier=%v", m.actions.Tier())
	}
	view := plain(m.Render())
	if !strings.Contains(view, "PROD CONTEXT") {
		t.Fatalf("expected the PROD CONTEXT tag in the modal:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.jobSuspends) != 0 {
		t.Fatalf("expected enter to no-op before the name matches: %v", mut.jobSuspends)
	}
	for _, r := range "batch-1" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.jobSuspends) != 1 || mut.jobSuspends[0] != "default/batch-1=true" {
		t.Fatalf("expected a suspend call, got %v", mut.jobSuspends)
	}
}

// TestSKeyTogglesJobSuspendLabelResumeExecutesImmediately mirrors
// TestSKeyOnCronJobSuspendResumeStaysTierNone: suspend still needs 'y'
// (TierInline outside PROD), but resume is now TierNone — reversible and
// immediate, the same "restores exactly the prior state" contract Cordon/
// FluxSuspend's own resume direction already has — so pressing 's' on a
// suspended row applies the resume with no confirm state at all.
func TestSKeyTogglesJobSuspendLabelResumeExecutesImmediately(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindJob: {suspendedJobObj("default", "batch-1", false)},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	if !strings.Contains(plain(m.Render()), "suspend") {
		t.Errorf("keybar should read 'suspend' on an active row:\n%s", plain(m.Render()))
	}
	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected 's' to open the inline prompt, tier=%v", m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.jobSuspends) != 1 || mut.jobSuspends[0] != "default/batch-1=true" {
		t.Fatalf("expected a suspend call, got %v", mut.jobSuspends)
	}

	mut2 := &fakeMutator{}
	lister2 := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindJob: {suspendedJobObj("default", "batch-1", true)},
	}}
	session2 := newSession()
	session2.Location.Kind = kube.KindJob
	m2 := New(Config{Session: session2, Lister: lister2, Mutator: mut2})
	m2.SetSize(120, 36)
	m2 = step(t, m2, m2.load()())

	if !strings.Contains(plain(m2.Render()), "resume") {
		t.Errorf("keybar should read 'resume' on a suspended row:\n%s", plain(m2.Render()))
	}
	m2 = step(t, m2, tea.KeyPressMsg{Text: "s"})
	if m2.actions.Active() {
		t.Fatalf("expected resume to execute immediately (TierNone), not enter a confirm state")
	}
	if len(mut2.jobSuspends) != 1 || mut2.jobSuspends[0] != "default/batch-1=false" {
		t.Fatalf("expected a resume call on a suspended row, got %v", mut2.jobSuspends)
	}
}

// TestCtrlRStagesRunNowThenTriggersOnEnter pins 0.8.0 §36b's staged
// contract: ctrl-r stages a preview in screen state (not an
// actions.Controller confirm — no inline y/N, no modal), ↵ executes through
// the reversible TierNone tier, and the generated Job name is readable
// (§36b: "-manual-HHMM" in UTC) rather than the old Unix-timestamp name.
func TestCtrlRStagesRunNowThenTriggersOnEnter(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "nightly")},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // see TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if m.pendingCronJobRun == nil {
		t.Fatalf("expected ctrl+r to stage the run-now preview")
	}
	if m.actions.Active() {
		t.Fatalf("expected no actions.Controller confirm state while staged (staging is itself the confirmation)")
	}
	kb := m.Keybar()
	if !strings.Contains(kb.RightNote, "kubectl create job") || !strings.Contains(kb.RightNote, "--from=cronjob/nightly") {
		t.Fatalf("expected the will-run line in the keybar, got %q", kb.RightNote)
	}
	if len(mut.triggeredCronJobs) != 0 {
		t.Fatalf("expected no trigger before enter, got %v", mut.triggeredCronJobs)
	}

	// 'y' copies rather than triggering — only enter executes.
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.triggeredCronJobs) != 0 {
		t.Fatalf("expected 'y' to copy, not trigger: %v", mut.triggeredCronJobs)
	}
	if m.pendingCronJobRun == nil {
		t.Fatalf("expected 'y' to leave the preview open")
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m.pendingCronJobRun != nil {
		t.Fatalf("expected enter to clear the staged preview")
	}
	if len(mut.triggeredCronJobs) != 1 {
		t.Fatalf("triggeredCronJobs = %v, want one entry", mut.triggeredCronJobs)
	}
	if !strings.HasPrefix(mut.triggeredCronJobs[0], "default/nightly->nightly-manual-") {
		t.Fatalf("triggeredCronJobs[0] = %q, want a default/nightly->nightly-manual-<HHMM> entry", mut.triggeredCronJobs[0])
	}
}

// TestCronJobRunNowEscCancelsWithoutTriggering pins the staged preview's
// esc-cancels contract (§36b: "esc cancels").
func TestCronJobRunNowEscCancelsWithoutTriggering(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "nightly")},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if m.pendingCronJobRun == nil {
		t.Fatalf("expected ctrl+r to stage the run-now preview")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.pendingCronJobRun != nil {
		t.Fatalf("expected esc to clear the staged preview")
	}
	if len(mut.triggeredCronJobs) != 0 {
		t.Fatalf("expected no trigger after esc, got %v", mut.triggeredCronJobs)
	}
}

// TestCronJobRunNowStaysStagedEvenInProd mirrors
// TestJobRetryStaysInlineEvenInProd: CronJobRunNow is a clone into a new
// object, not destructive, so it never escalates to a PROD-only confirm —
// staging is the same in every context.
func TestCronJobRunNowStaysStagedEvenInProd(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "nightly")},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	session.Config = config.Config{ProdContexts: []string{session.Location.Context}}
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	if m.pendingCronJobRun == nil || m.actions.Active() {
		t.Fatalf("expected R to stage the preview with no actions.Controller confirm, even in PROD")
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if len(mut.triggeredCronJobs) != 1 {
		t.Fatalf("expected enter to trigger immediately, got %v", mut.triggeredCronJobs)
	}
}

// TestCronJobRunNowOverlapWarningNamesActiveJobs pins §36b's overlap card:
// an active associated Job under a Forbid concurrencyPolicy renders a
// warning naming it, and the run still executes (a manual run bypasses the
// policy rather than being blocked by it).
func TestCronJobRunNowOverlapWarningNamesActiveJobs(t *testing.T) {
	cj := cronJobObj("default", "sync")
	cj.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	active := jobObj("default", "sync-29310612")
	active.OwnerReferences = []metav1.OwnerReference{controllerRefFor(cj)}
	active.Status = batchv1.JobStatus{Active: 1, StartTime: &metav1.Time{Time: time.Now().Add(-2 * time.Minute)}}
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

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	kb := m.Keybar()
	if !strings.Contains(kb.RightWarnNote, "sync-29310612") || !strings.Contains(kb.RightWarnNote, "Forbid") {
		t.Fatalf("expected the overlap warning to name the active job and policy, got %q", kb.RightWarnNote)
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if len(mut.triggeredCronJobs) != 1 {
		t.Fatalf("expected the manual run to execute despite the overlap, got %v", mut.triggeredCronJobs)
	}
}

// TestCronJobRunNowNoOverlapChromeWhenNoActiveRun pins "a complete Job view
// with no overlap renders zero warning chrome — just the will-run line."
func TestCronJobRunNowNoOverlapChromeWhenNoActiveRun(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "nightly")},
		kube.KindJob:     {},
	}}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "R"})
	kb := m.Keybar()
	if kb.RightWarnNote != "" {
		t.Fatalf("expected no warning chrome with no active run, got %q", kb.RightWarnNote)
	}
}

// controllerRefFor builds a controller=true owner reference for cj — the
// "scheduled" association rule cronJobAssociation (resources/cronjobs.go)
// requires for an active Job to be counted as one of the CronJob's own
// ActiveRuns.
func controllerRefFor(cj *batchv1.CronJob) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion: "batch/v1", Kind: "CronJob", Name: cj.Name, UID: cj.UID, Controller: &t,
	}
}

// TestSKeyTogglesCronJobSuspendImmediatelyNoConfirm pins CronJobSuspend's
// TierNone contract (unlike Job's own 's', TierInline): pressing 's' applies
// the suspend/resume immediately, with no confirm state to pass through
// first — the same shape beginFluxSuspend/beginCordon use.
// TestSKeyOnCronJobSuspendRequiresConfirmNonProd pins 0.8.0 §36c's
// asymmetric friction: unlike FluxSuspend/beginCordon, suspend is CronJob's
// "dangerous half" and costs a keystroke — TierInline outside PROD — even
// though the underlying verb (Scope.Verb "cronjob-suspend") is the same one
// TestSKeyOnCronJobSuspendResumeStaysTierNone's resume half leaves at
// TierNone.
func TestSKeyOnCronJobSuspendRequiresConfirmNonProd(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {suspendedCronJobObj("default", "nightly", false)},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // see TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected 's' on an active CronJob to open the inline prompt, tier=%v", m.actions.Tier())
	}
	if len(mut.cronJobSuspends) != 0 {
		t.Fatalf("expected no call before 'y', got %v", mut.cronJobSuspends)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.cronJobSuspends) != 1 || mut.cronJobSuspends[0] != "default/nightly=true" {
		t.Fatalf("expected a suspend call, got %v", mut.cronJobSuspends)
	}
}

// TestCronJobSuspendEscalatesToModalConfirmInProd mirrors
// TestJobSuspendEscalatesToModalConfirmInProd, but — unlike Job's own
// suspend, which is a plain y/N even in PROD (browse's ConfirmCard) — a
// CronJob suspend confirmation in PROD is routed to the full type-the-name
// modal (verbs.Resolve's typed-name policy for "cronjob-suspend", 0.8.0 plan §3
// tasks 2/3), the same red-bordered surface delete/rollout-restart use.
func TestCronJobSuspendEscalatesToModalConfirmInProd(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {suspendedCronJobObj("default", "nightly", false)},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	session.Config = config.Config{ProdContexts: []string{session.Location.Context}}
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // see TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected 's' in a prod context to escalate to TierModal, tier=%v", m.actions.Tier())
	}
	view := plain(m.Render())
	if !strings.Contains(view, "PROD CONTEXT") {
		t.Fatalf("expected the PROD CONTEXT tag in the modal:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.cronJobSuspends) != 0 {
		t.Fatalf("expected enter to no-op before the name matches: %v", mut.cronJobSuspends)
	}
	for _, r := range "nightly" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.cronJobSuspends) != 1 || mut.cronJobSuspends[0] != "default/nightly=true" {
		t.Fatalf("expected a suspend call, got %v", mut.cronJobSuspends)
	}
}

// TestSKeyOnCronJobSuspendResumeStaysTierNone mirrors
// TestSKeyTogglesJobSuspendAndResumeLabel's keybar-label assertions, but —
// unlike Job's own resume, which still needs 'y' — resume stays reversible-
// and-immediate TierNone: it stages its own preview before Begin the same
// way run-now does, so by the time 's' lands there's nothing left for
// Controller's own confirm to add (0.8.0 plan §36c: "Resume is reversible
// and executes on ↵ with no destructive escalation"). This holds in PROD
// too — verbs.Resolve's CronJobSuspend row never escalates the resume direction.
func TestSKeyOnCronJobSuspendResumeStaysTierNone(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {suspendedCronJobObj("default", "nightly", false)},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // see TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings

	if !strings.Contains(plain(m.Render()), "suspend") {
		t.Errorf("keybar should read 'suspend' on an active row:\n%s", plain(m.Render()))
	}

	mut2 := &fakeMutator{}
	lister2 := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {suspendedCronJobObj("default", "nightly", true)},
	}}
	session2 := newSession()
	session2.Location.Kind = kube.KindCronJob
	session2.Config = config.Config{ProdContexts: []string{session2.Location.Context}}
	m2 := New(Config{Session: session2, Lister: lister2, Mutator: mut2})
	m2.SetSize(120, 36)
	m2 = step(t, m2, m2.load()())

	if !strings.Contains(plain(m2.Render()), "resume") {
		t.Errorf("keybar should read 'resume' on a suspended row:\n%s", plain(m2.Render()))
	}
	m2 = step(t, m2, tea.KeyPressMsg{Text: "s"})
	if m2.pendingCronJobResume == nil {
		t.Fatalf("expected 's' to stage the resume preview")
	}
	if m2.actions.Active() {
		t.Fatalf("expected no actions.Controller confirm state while staged, even in PROD")
	}
	if len(mut2.cronJobSuspends) != 0 {
		t.Fatalf("expected no resume call before enter, got %v", mut2.cronJobSuspends)
	}

	m2 = step(t, m2, tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if m2.pendingCronJobResume != nil {
		t.Fatalf("expected enter to clear the staged preview")
	}
	if len(mut2.cronJobSuspends) != 1 || mut2.cronJobSuspends[0] != "default/nightly=false" {
		t.Fatalf("expected a resume call on a suspended row, got %v", mut2.cronJobSuspends)
	}
}

// TestCronJobResumeEscCancelsWithoutResuming pins the staged resume
// preview's esc-cancels contract, mirroring run-now's own.
func TestCronJobResumeEscCancelsWithoutResuming(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {suspendedCronJobObj("default", "nightly", true)},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if m.pendingCronJobResume == nil {
		t.Fatalf("expected 's' to stage the resume preview")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.pendingCronJobResume != nil {
		t.Fatalf("expected esc to clear the staged preview")
	}
	if len(mut.cronJobSuspends) != 0 {
		t.Fatalf("expected no resume call after esc, got %v", mut.cronJobSuspends)
	}
}

// TestCronJobResumePreviewShowsMissedRunsAndNext pins §3.3's truthful
// missed-schedule copy and §36c's next-run preview, computed from a Kute
// suspend annotation whose generation still matches the CronJob's current
// one.
func TestCronJobResumePreviewShowsMissedRunsAndNext(t *testing.T) {
	cj := suspendedCronJobObj("default", "warm-cache", true)
	cj.Spec.Schedule = "*/5 * * * *"
	tz := "UTC"
	cj.Spec.TimeZone = &tz
	cj.Generation = 3
	suspendedAt := time.Now().Add(-20 * time.Minute)
	cj.Annotations = map[string]string{
		kube.AnnotationSuspendedAt:         suspendedAt.UTC().Format(time.RFC3339),
		kube.AnnotationSuspendedGeneration: "3",
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cj},
	}}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(160, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	kb := m.Keybar()
	if !strings.Contains(kb.RightNote, "schedule") || !strings.Contains(kb.RightNote, "missed") {
		t.Fatalf("expected a missed-schedule count in the preview, got %q", kb.RightNote)
	}
	if !strings.Contains(kb.RightNote, "next") {
		t.Fatalf("expected a next-run preview, got %q", kb.RightNote)
	}
	if strings.Contains(kb.RightNote, "start unknown") {
		t.Fatalf("expected a known suspended duration (valid generation marker), got %q", kb.RightNote)
	}
}

// TestCronJobResumePreviewUnknownStartOmitsSkippedCount pins §3.3: an
// externally-suspended CronJob (no Kute annotation) renders "start unknown"
// and omits a fabricated skipped count.
func TestCronJobResumePreviewUnknownStartOmitsSkippedCount(t *testing.T) {
	cj := suspendedCronJobObj("default", "warm-cache", true) // no suspended-at annotation
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cj},
	}}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(160, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	kb := m.Keybar()
	if !strings.Contains(kb.RightNote, "start unknown") {
		t.Fatalf("expected 'start unknown' for an externally suspended cronjob, got %q", kb.RightNote)
	}
	if !strings.Contains(kb.RightNote, "suspension start unknown") {
		t.Fatalf("expected the missed-run reason to explain the unknown start, got %q", kb.RightNote)
	}
}

// TestShiftSPushesCronJobScheduleEditor pins Phase 6's replacement for the
// old inline beginCronJobSetSchedule buffer: 'S' on a CronJob row now pushes
// tasks/cronjobschedule (stubbed here) via OpenCronJobSchedule, carrying the
// selected row's own namespace/name — mirrors
// TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings' shape for
// OpenCronJobDetail.
func TestShiftSPushesCronJobScheduleEditor(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "nightly")},
		kube.KindJob:     {},
	}}
	var gotNamespace, gotName string
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, OpenCronJobSchedule: func(namespace, name string, _, _ int) (tea.Model, tea.Cmd) {
		gotNamespace, gotName = namespace, name
		return stubTask{}, nil
	}})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // see TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings

	updated, _ := m.Update(tea.KeyPressMsg{Text: "S"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected 'S' to push the stub schedule task, got %T", updated)
	}
	if gotNamespace != "default" || gotName != "nightly" {
		t.Fatalf("expected OpenCronJobSchedule(default, nightly, ...), got (%q, %q)", gotNamespace, gotName)
	}
}

// TestShiftSNoopsWithoutScheduleWired mirrors
// TestEnterOnCronJobNoopsWithoutDetailWired: pressing 'S' stays harmless
// when app.go hasn't wired OpenCronJobSchedule.
func TestShiftSNoopsWithoutScheduleWired(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "nightly")},
		kube.KindJob:     {},
	}}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // see TestEnterOnCronJobPushesDetailWithNamespaceQualifiedSiblings

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "S"})
	next, ok := updated.(*Model)
	if !ok {
		t.Fatalf("expected 'S' to stay on the browse model, got %T", updated)
	}
	if cmd != nil {
		t.Fatalf("expected no command, got one")
	}
	if next.kind != kube.KindCronJob {
		t.Fatalf("expected kind to stay CronJob, got %s", next.kind)
	}
}
