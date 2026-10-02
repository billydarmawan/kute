// 0.8.0 plan Phase 4 tests: §36a's shared browse list — cache-local
// CronJob+Job aggregation consumed by load(), the one-second UI clock, the
// selection-scoped failure sub-line, the behavior strip, and the l/enter
// routing built on top of it. Phase 1's own aggregation/projection tests
// live in internal/resources/cronjobs_test.go; these stay browse-scoped.
package browse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// cronJobOwnerRef builds the controller owner reference a scheduled Job
// carries back to its CronJob — cronJobAssociation's own recognized shape
// (resources/cronjobs.go).
func cronJobOwnerRef(cj *batchv1.CronJob) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{Kind: "CronJob", Name: cj.Name, UID: cj.UID, Controller: &controller}
}

func succeededOwnedJob(ns, name string, owner metav1.OwnerReference, completedAt time.Time) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec:       batchv1.JobSpec{Completions: ptr32(1)},
		Status: batchv1.JobStatus{
			Succeeded:      1,
			CompletionTime: &metav1.Time{Time: completedAt},
			Conditions:     []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: "True"}},
		},
	}
}

func failedOwnedJob(ns, name string, owner metav1.OwnerReference, completedAt time.Time, reason, message string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec:       batchv1.JobSpec{Completions: ptr32(1)},
		Status: batchv1.JobStatus{
			Failed:     3,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True", Reason: reason, Message: message, LastTransitionTime: metav1.Time{Time: completedAt}}},
		},
	}
}

func activeOwnedJob(ns, name string, owner metav1.OwnerReference, startedAt time.Time) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: []metav1.OwnerReference{owner}},
		Spec:       batchv1.JobSpec{Completions: ptr32(1)},
		Status:     batchv1.JobStatus{Active: 1, StartTime: &metav1.Time{Time: startedAt}},
	}
}

func cronJobBrowseSession() *tui.Session {
	s := newSession()
	s.Location.Kind = kube.KindCronJob
	return s
}

// TestCronJobColumnsAndStableNameOrder pins Phase 4 test 1: the exact §36a
// column set, and namespace/name order as the *default* (unlike every
// workloadKinds member's default unhealthy-first sort) — see
// TestCronJobManualSortOverridesDefaultAndSurvivesClockTick below for the
// manual-sort override on top of this default.
func TestCronJobColumnsAndStableNameOrder(t *testing.T) {
	cjA := cronJobObj("default", "zzz-last")
	cjB := cronJobObj("default", "aaa-first")
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cjA, cjB},
	}}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	if got := m.desc.Columns; len(got) != 6 ||
		got[0] != "Name" || got[1] != "Schedule" || got[2] != "Susp" || got[3] != "Act" || got[4] != "Last Run" || got[5] != "Next" {
		t.Fatalf("columns = %v, want [Name Schedule Susp Act Last Run Next]", got)
	}
	if len(m.rows) != 2 || m.rows[0].Name != "aaa-first" || m.rows[1].Name != "zzz-last" {
		t.Fatalf("expected default name order [aaa-first zzz-last], got %+v", m.rows)
	}
}

// TestCronJobManualSortOverridesDefaultAndSurvivesClockTick pins the current
// §36a contract: CronJobs sort exactly like every other kind — a manual 1-9
// column pick overrides the name-order default — and, unlike every other
// kind, that choice must survive applyCronJobTick's once-a-second rebuild
// (model.go), which regenerates m.rows from cronJobSummaries in their own
// fixed base order on every tick.
func TestCronJobManualSortOverridesDefaultAndSurvivesClockTick(t *testing.T) {
	cjA := cronJobObj("default", "zzz-last")
	cjA.Spec.Schedule = "0 0 * * *"
	cjB := cronJobObj("default", "aaa-first")
	cjB.Spec.Schedule = "*/5 * * * *"
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cjA, cjB},
	}}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	// Sort by Schedule (column 2): "*/5 * * * *" < "0 0 * * *" ascending,
	// putting aaa-first (whose schedule starts with '*') ahead of zzz-last —
	// overriding the name-order default that would otherwise put
	// aaa-first first anyway, so also flip direction to prove the override
	// actually took effect rather than coincidentally matching name order.
	m.handleSortKey(2)
	m.handleSortKey(2) // press again: flips to descending
	if len(m.rows) != 2 || m.rows[0].Name != "zzz-last" || m.rows[1].Name != "aaa-first" {
		t.Fatalf("expected manual descending Schedule sort [zzz-last aaa-first], got %+v", m.rows)
	}

	m.applyCronJobTick(time.Now())
	if len(m.rows) != 2 || m.rows[0].Name != "zzz-last" || m.rows[1].Name != "aaa-first" {
		t.Fatalf("expected manual sort to survive the clock tick, got %+v", m.rows)
	}
}

// TestCronJobActiveRowKeepsPriorLastRunWhileActShowsRunning pins Phase 4
// test 3 (§4.3): an active run must never blank out the previous terminal
// outcome — LAST RUN keeps reading the last success/failure while ACT and
// the row's own Active flag show the run in flight.
func TestCronJobActiveRowKeepsPriorLastRunWhileActShowsRunning(t *testing.T) {
	cj := cronJobObj("default", "sync-inventory")
	cj.UID = "cj-1"
	owner := cronJobOwnerRef(cj)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cj},
		kube.KindJob: {
			succeededOwnedJob("default", "sync-inventory-1", owner, time.Now().Add(-2*time.Minute)),
			activeOwnedJob("default", "sync-inventory-2", owner, time.Now().Add(-30*time.Second)),
		},
	}}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	row, ok := m.selectedRow()
	if !ok || row.Name != "sync-inventory" {
		t.Fatalf("expected sync-inventory selected, got %+v (ok=%v)", row, ok)
	}
	if !row.Active {
		t.Fatal("expected Row.Active true with one active run")
	}
	if row.Cells[3] != "1" {
		t.Fatalf("ACT cell = %q, want %q", row.Cells[3], "1")
	}
	if !strings.Contains(row.Cells[4], "ago") || !strings.Contains(row.Cells[4], "✓") {
		t.Fatalf("LAST RUN cell = %q, want the prior success, not blanked by the active run", row.Cells[4])
	}
}

// TestCronJobFailureSubLineAlwaysVisible pins the current §36a contract: the
// inline failure reason renders under *every* failed CronJob row, all the
// time — the same always-on treatment §30a/§33a's Flux/Argo SubLine already
// gets — never gated by which row is selected, and unaffected by moving the
// cursor.
func TestCronJobFailureSubLineAlwaysVisible(t *testing.T) {
	cjA := cronJobObj("default", "report-nightly")
	cjA.UID = "cj-a"
	cjB := cronJobObj("default", "webhook-retry")
	cjB.UID = "cj-b"
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cjA, cjB},
		kube.KindJob: {
			failedOwnedJob("default", "report-nightly-1", cronJobOwnerRef(cjA), time.Now().Add(-time.Hour),
				"BackoffLimitExceeded", "Job has reached the specified backoff limit"),
			failedOwnedJob("default", "webhook-retry-1", cronJobOwnerRef(cjB), time.Now().Add(-time.Minute),
				"DeadlineExceeded", "Job was active longer than specified deadline"),
		},
	}}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	// Both failed rows show their own reason line regardless of selection —
	// report-nightly is selected (alphabetically first) but webhook-retry's
	// line renders too.
	view := plain(m.Render())
	if !strings.Contains(view, "BackoffLimitExceeded") {
		t.Fatalf("expected report-nightly's failure reason visible:\n%s", view)
	}
	if !strings.Contains(view, "DeadlineExceeded") {
		t.Fatalf("expected webhook-retry's failure reason visible even though it's not selected:\n%s", view)
	}

	m.moveSelection(1)
	view = plain(m.Render())
	if !strings.Contains(view, "BackoffLimitExceeded") {
		t.Fatalf("expected report-nightly's failure reason to stay visible after moving off it:\n%s", view)
	}
	if !strings.Contains(view, "DeadlineExceeded") {
		t.Fatalf("expected webhook-retry's failure reason still visible:\n%s", view)
	}
}

// TestCronJobBehaviorStripChangesWithSelection pins Phase 4 task 9/test 5.
func TestCronJobBehaviorStripChangesWithSelection(t *testing.T) {
	cjA := cronJobObj("default", "aaa")
	cjA.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	cjB := cronJobObj("default", "bbb")
	cjB.Spec.ConcurrencyPolicy = batchv1.AllowConcurrent
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cjA, cjB},
	}}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	first := m.cronJobBehaviorStripLine(m.Theme(), m.width)
	if !strings.Contains(plain(first), "aaa") || !strings.Contains(plain(first), "Forbid") {
		t.Fatalf("expected the strip to name aaa/Forbid, got %q", plain(first))
	}
	m.moveSelection(1)
	second := m.cronJobBehaviorStripLine(m.Theme(), m.width)
	if !strings.Contains(plain(second), "bbb") || !strings.Contains(plain(second), "Allow") {
		t.Fatalf("expected the strip to change to bbb/Allow after moving selection, got %q", plain(second))
	}
}

// countingLister wraps fakeLister and counts every ListRaw call, so
// TestCronJobClockTickNeverCallsTheLister can prove a tick performs zero
// cluster reads.
type countingLister struct {
	fakeLister
	calls int
}

func (l *countingLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	l.calls++
	return l.fakeLister.ListRaw(ctx, kind, namespace)
}

// TestCronJobClockTickNeverCallsTheLister pins Phase 4 test 6/§4.5: the
// one-second UI clock rebuilds NEXT/LAST RUN and the strip's own UTC clock
// from the stored summaries alone — never a lister call. Fires the tick
// message directly through Update (not step, which would otherwise follow
// the tick's own self-rescheduling command forever in this synchronous
// harness — see TestPodMetricsRenderAsBars's comment for the same hazard).
func TestCronJobClockTickNeverCallsTheLister(t *testing.T) {
	cj := cronJobObj("default", "webhook-retry")
	cj.Spec.Schedule = "*/2 * * * *"
	tz := "Etc/UTC"
	cj.Spec.TimeZone = &tz
	lister := &countingLister{fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cj},
	}}}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	before := lister.calls
	beforeNext := m.rows[0].Cells[5]

	updated, cmd := m.Update(cronJobTickMsg{epoch: m.clockEpoch})
	m = *updated.(*Model)
	if cmd == nil {
		t.Fatal("expected the tick to reschedule itself")
	}
	if lister.calls != before {
		t.Fatalf("tick issued %d ListRaw calls, want 0 (before=%d)", lister.calls-before, before)
	}
	if !strings.Contains(plain(m.Render()), "clock") {
		t.Fatalf("expected the health strip's live clock text:\n%s", plain(m.Render()))
	}
	_ = beforeNext // NEXT's exact text is timing-sensitive; the no-lister-call assertion above is the point of this test.
}

// perKindSyncedCronJobLister is a minimal KindSynced-capable lister scoped
// to this file, mirroring browse_sync_test.go's perKindSyncedLister without
// depending on its unexported type across files unnecessarily — kept local
// so this file's intent (§4.4 point 1) reads standalone.
type perKindSyncedCronJobLister struct {
	fakeLister
	unsynced map[kube.ResourceKind]bool
}

func (l *perKindSyncedCronJobLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return !l.unsynced[kind]
}

// TestCronJobsPresentButJobCacheUnsyncedStaysLoading pins Phase 4 test 9/
// §4.4 point 1: even with CronJob rows already readable, an unsynced Job
// cache must hold the loading state rather than rendering LAST RUN/ACT off
// an incomplete join.
func TestCronJobsPresentButJobCacheUnsyncedStaysLoading(t *testing.T) {
	lister := &perKindSyncedCronJobLister{
		unsynced: map[kube.ResourceKind]bool{kube.KindJob: true},
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindCronJob: {cronJobObj("default", "nightly")},
		}},
	}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)

	msg := m.load()()
	updated, cmd := m.Update(msg)
	m = *updated.(*Model)

	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %s, want loading while the Job cache is still filling", m.state)
	}
	if cmd == nil {
		t.Fatal("expected a retry to be scheduled")
	}
}

// TestCronJobJobCacheErrorStaysVisibleNeverFakeEmpty pins Phase 4 test 10/
// §4.4 point 5: a Job list failure must not render as the ordinary
// history-less "no retained runs"/"0" — those cells go "unavailable"/"–"
// instead, and the health strip names the failure inline.
func TestCronJobJobCacheErrorStaysVisibleNeverFakeEmpty(t *testing.T) {
	lister := &jobErrLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindCronJob: {cronJobObj("default", "nightly")},
		}},
		err: errors.New("stream error when reading response body"),
	}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — the CronJob cache itself is fine", m.state)
	}
	if m.cronJobJobsErr == nil {
		t.Fatal("expected cronJobJobsErr set")
	}
	row := m.rows[0]
	if row.Cells[4] != "unavailable" {
		t.Fatalf("LAST RUN cell = %q, want %q — never the ordinary no-history reading", row.Cells[4], "unavailable")
	}
	if strings.Contains(row.Cells[4], "no retained runs") {
		t.Fatal("LAST RUN must never render the ordinary empty-history text on a Job read failure")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "job history unavailable") {
		t.Fatalf("expected an inline note naming the Job read failure:\n%s", view)
	}
}

// jobErrLister returns CronJobs normally but fails every Job ListRaw call.
type jobErrLister struct {
	fakeLister
	err error
}

func (l *jobErrLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if kind == kube.KindJob {
		return nil, l.err
	}
	return l.fakeLister.ListRaw(ctx, kind, namespace)
}

// TestCronJobJobCacheForbiddenShowsPermissionDeniedWording pins §5 of
// docs/lazy-informers.md §5.6: a permission denial on the Job
// side of the CronJob/Job join used to render identically to a transient
// stall — both as the raw client-go error text. A denial is permanent for
// the session and gets its own wording distinguishing it from "still
// retrying".
func TestCronJobJobCacheForbiddenShowsPermissionDeniedWording(t *testing.T) {
	lister := &jobErrLister{
		fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindCronJob: {cronJobObj("default", "nightly")},
		}},
		err: apierrors.NewForbidden(schema.GroupResource{Resource: "jobs"}, "", errors.New("nope")),
	}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — the CronJob cache itself is fine", m.state)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "job history unavailable: permission denied") {
		t.Fatalf("expected the strip to say 'permission denied', got:\n%s", view)
	}
	if strings.Contains(view, "forbidden") {
		t.Fatalf("raw apierrors text leaked into the strip instead of the 'permission denied' wording:\n%s", view)
	}
}

// TestCronJobLogsOpensActiveRunsPod pins Phase 4 task 13/test 12: 'l' opens
// logs for the newest active run's Pod when one exists.
func TestCronJobLogsOpensActiveRunsPod(t *testing.T) {
	cj := cronJobObj("default", "sync-inventory")
	cj.UID = "cj-1"
	owner := cronJobOwnerRef(cj)
	job := activeOwnedJob("default", "sync-inventory-2", owner, time.Now())
	podOwnerController := true
	podOwner := metav1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID, Controller: &podOwnerController}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cj},
		kube.KindJob:     {job},
		kube.KindPod:     {podWithOwner("default", "sync-inventory-2-x7f2k", podOwner)},
	}}
	var gotPod kube.Pod
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister, OpenLogs: func(pod kube.Pod, _ string, _, _ int) (tea.Model, tea.Cmd) {
		gotPod = pod
		return stubTask{}, nil
	}})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "l"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected 'l' to push the stub log task, got %T", updated)
	}
	if gotPod.Name != "sync-inventory-2-x7f2k" {
		t.Fatalf("expected logs opened for sync-inventory-2-x7f2k, got %q", gotPod.Name)
	}
	if len(gotPod.Containers) == 0 {
		t.Fatalf("expected gotPod to carry its container names (podlogs can't stream without them), got none")
	}
}

// TestCronJobLogsOpensSucceededRunsPod pins the request that 'l' open logs
// for the latest job run regardless of outcome: no active run, and the last
// terminal run succeeded rather than failed — still a valid log target,
// since summary.Runs (newest-first, every state) is what cronJobLogsTarget
// reads now, not the old active-or-failed-only filter.
func TestCronJobLogsOpensSucceededRunsPod(t *testing.T) {
	cj := cronJobObj("default", "sync-inventory")
	cj.UID = "cj-1"
	owner := cronJobOwnerRef(cj)
	job := succeededOwnedJob("default", "sync-inventory-2", owner, time.Now())
	podOwnerController := true
	podOwner := metav1.OwnerReference{Kind: "Job", Name: job.Name, UID: job.UID, Controller: &podOwnerController}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cj},
		kube.KindJob:     {job},
		kube.KindPod:     {podWithOwner("default", "sync-inventory-2-x7f2k", podOwner)},
	}}
	var gotPod kube.Pod
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister, OpenLogs: func(pod kube.Pod, _ string, _, _ int) (tea.Model, tea.Cmd) {
		gotPod = pod
		return stubTask{}, nil
	}})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "l"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected 'l' to push the stub log task for a succeeded run, got %T", updated)
	}
	if gotPod.Name != "sync-inventory-2-x7f2k" {
		t.Fatalf("expected logs opened for sync-inventory-2-x7f2k, got %q", gotPod.Name)
	}
	if len(gotPod.Containers) == 0 {
		t.Fatalf("expected gotPod to carry its container names (podlogs can't stream without them), got none")
	}
}

// TestCronJobLogsReportsWhenNoRunToShow pins the "reports why it cannot"
// half of Phase 4 test 12: a CronJob with no associated Jobs at all leaves
// nothing for 'l' to open, and browse says so inline instead of silently
// doing nothing.
func TestCronJobLogsReportsWhenNoRunToShow(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindCronJob: {cronJobObj("default", "nightly")},
	}}
	m := New(Config{Session: cronJobBrowseSession(), Lister: lister, OpenLogs: func(kube.Pod, string, int, int) (tea.Model, tea.Cmd) {
		t.Fatal("openLogs must not be called with nothing to show")
		return nil, nil
	}})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	m = step(t, m, tea.KeyPressMsg{Text: "l"})
	if !strings.Contains(m.execFeedback, "no runs") {
		t.Fatalf("execFeedback = %q, want an explanation that nothing is available", m.execFeedback)
	}
}

func podWithOwner(ns, name string, owner metav1.OwnerReference) *corev1.Pod {
	p := pod(ns, name)
	p.OwnerReferences = []metav1.OwnerReference{owner}
	return p
}
