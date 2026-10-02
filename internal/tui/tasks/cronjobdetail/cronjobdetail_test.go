package cronjobdetail

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

// step drains a tea.Cmd chain synchronously — mirrors cronjobschedule_test.go's
// own step, duplicated per the repo's package-local-seam convention. Never
// called on Init()'s own batch: tickCmd/spinner.Tick both recur forever
// under synchronous draining.
func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				m = step(t, m, c())
			}
		}
		return m
	}
	updated, cmd := m.Update(msg)
	next := *updated.(*Model)
	if cmd != nil {
		return step(t, next, cmd())
	}
	return next
}

func newSession(namespace string) *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster", Namespace: namespace}}
}

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(v int32) *int32 { return &v }
func int64Ptr(v int64) *int64 { return &v }

func controllerRef(kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{Kind: kind, Name: name, UID: uid, Controller: boolPtr(true)}
}

func newModel(t *testing.T, c *fake.Cluster, namespace, name string) Model {
	t.Helper()
	m := New(Config{Session: newSession(namespace), Lister: c, Mutator: c, Namespace: namespace, Name: name})
	m.SetSize(120, 40)
	return step(t, m, m.load()())
}

// newFakeCronJob seeds a fake cluster with one CronJob carrying the given
// spec fields — a minimal but real object, not a bespoke test double, so
// resources.BuildCronJobSummaries/ProjectCronJob run unmodified.
func newFakeCronJob(namespace, name string) (*fake.Cluster, *batchv1.CronJob) {
	c := fake.New(namespace, "test-cluster")
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			UID: "cj-uid-1", ResourceVersion: "1", Generation: 1,
		},
		Spec: batchv1.CronJobSpec{Schedule: "0 2 * * *"},
	}
	c.Seed(kube.KindCronJob, cj)
	return c, cj
}

func failedJob(namespace, name string, cj *batchv1.CronJob, completedAt time.Time) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			OwnerReferences:   []metav1.OwnerReference{controllerRef("CronJob", cj.Name, cj.UID)},
			CreationTimestamp: metav1.Time{Time: completedAt.Add(-time.Minute)},
		},
		Spec: batchv1.JobSpec{Completions: int32Ptr(1)},
		Status: batchv1.JobStatus{
			Failed: 3,
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
				LastTransitionTime: metav1.Time{Time: completedAt},
			}},
		},
	}
}

func succeededJob(namespace, name string, cj *batchv1.CronJob, completedAt time.Time) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			OwnerReferences:   []metav1.OwnerReference{controllerRef("CronJob", cj.Name, cj.UID)},
			CreationTimestamp: metav1.Time{Time: completedAt.Add(-time.Minute)},
		},
		Spec: batchv1.JobSpec{Completions: int32Ptr(1)},
		Status: batchv1.JobStatus{
			Succeeded:      1,
			CompletionTime: &metav1.Time{Time: completedAt},
			Conditions:     []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: completedAt}}},
		},
	}
}

func activeJob(namespace, name string, cj *batchv1.CronJob, startedAt time.Time) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace,
			OwnerReferences:   []metav1.OwnerReference{controllerRef("CronJob", cj.Name, cj.UID)},
			CreationTimestamp: metav1.Time{Time: startedAt},
		},
		Spec:   batchv1.JobSpec{Completions: int32Ptr(1)},
		Status: batchv1.JobStatus{Active: 1, StartTime: &metav1.Time{Time: startedAt}},
	}
}

func TestLoadBuildsFactsAndOrdersJobsNewestFirst(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	cj.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	cj.Spec.StartingDeadlineSeconds = int64Ptr(600)
	cj.Spec.JobTemplate.Spec.BackoffLimit = int32Ptr(3)
	now := time.Now()
	older := succeededJob("default", "nightly-1", cj, now.Add(-48*time.Hour))
	newer := failedJob("default", "nightly-2", cj, now.Add(-1*time.Hour))
	c.Seed(kube.KindJob, older, newer)

	m := newModel(t, c, "default", "nightly")
	if !m.found {
		t.Fatalf("found = false, want true (feedback=%q)", m.feedback)
	}
	if len(m.summary.Runs) != 2 || m.summary.Runs[0].Name != "nightly-2" {
		t.Fatalf("Runs = %+v, want newest (nightly-2) first", m.summary.Runs)
	}
	if m.summary.LastTerminal == nil || !m.summary.LastTerminal.Failed {
		t.Fatalf("LastTerminal = %+v, want the failed run", m.summary.LastTerminal)
	}

	fields := m.factsGridRows()
	want := map[string]string{
		"CONCURRENCY":   "Forbid",
		"DEADLINE":      "600s",
		"BACKOFF LIMIT": "3",
		"ACTIVE":        "0",
	}
	got := map[string]string{}
	for _, f := range fields {
		got[f[0]] = f[1]
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("facts[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestFactsGridDefaultsForNilPointerFields(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "nightly")
	m := newModel(t, c, "default", "nightly")

	got := map[string]string{}
	for _, f := range m.factsGridRows() {
		got[f[0]] = f[1]
	}
	if got["DEADLINE"] != "–" {
		t.Errorf("DEADLINE = %q, want – for an unset startingDeadlineSeconds", got["DEADLINE"])
	}
	if got["BACKOFF LIMIT"] != "6 (default)" {
		t.Errorf("BACKOFF LIMIT = %q, want the documented Kubernetes default", got["BACKOFF LIMIT"])
	}
	if got["HISTORY KEPT"] != "3 succeeded · 1 failed" {
		t.Errorf("HISTORY KEPT = %q, want the documented Kubernetes defaults (3/1)", got["HISTORY KEPT"])
	}
	if got["LAST SCHEDULE"] != "never" {
		t.Errorf("LAST SCHEDULE = %q, want never for an unset status.lastScheduleTime", got["LAST SCHEDULE"])
	}
	if got["CONTROLLER"] != "–" {
		t.Errorf("CONTROLLER = %q, want – with no owner reference or helm annotation", got["CONTROLLER"])
	}
}

func TestFailureBandRendersOnlyWhenLastTerminalFailed(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	c.Seed(kube.KindJob, succeededJob("default", "nightly-1", cj, time.Now().Add(-time.Hour)))
	m := newModel(t, c, "default", "nightly")
	if band := m.failureBandLines(m.Theme(), 100); len(band) != 0 {
		t.Fatalf("failureBandLines = %v, want empty when the last terminal run succeeded", band)
	}

	c2, cj2 := newFakeCronJob("default", "nightly2")
	c2.Seed(kube.KindJob, failedJob("default", "nightly2-1", cj2, time.Now().Add(-time.Hour)))
	m2 := newModel(t, c2, "default", "nightly2")
	band := m2.failureBandLines(m2.Theme(), 100)
	if len(band) == 0 {
		t.Fatalf("failureBandLines = empty, want a band for a failed last terminal run")
	}
}

func TestFailureBandFallsBackWithoutPods(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	c.Seed(kube.KindJob, failedJob("default", "nightly-1", cj, time.Now().Add(-time.Hour)))
	m := newModel(t, c, "default", "nightly")
	if m.summary.LastTerminal.ExitCode != nil {
		t.Fatalf("ExitCode = %v, want nil with no Pod collected", m.summary.LastTerminal.ExitCode)
	}
	band := m.failureBandLines(m.Theme(), 100)
	if len(band) == 0 {
		t.Fatalf("expected a failure band even without a collected Pod")
	}
}

func TestRetainedWordingNeverImpliesLifetimeCount(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	c.Seed(kube.KindJob, succeededJob("default", "nightly-1", cj, time.Now().Add(-time.Hour)))
	m := newModel(t, c, "default", "nightly")
	header := m.jobsSectionHeader(m.Theme(), 200)
	if !containsAll(header, "1 retained", "history limits") {
		t.Errorf("header = %q, want truthful retained/history-limit wording", header)
	}
	if containsAny(header, "kept of", "lifetime") {
		t.Errorf("header = %q, must never imply a lifetime total (§3.2)", header)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestLoadNotFoundEntersGoneReadyState(t *testing.T) {
	t.Parallel()
	c := fake.New("default", "test-cluster")
	m := newModel(t, c, "default", "ghost")
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready (a deleted-object banner, not an error)", m.state)
	}
	if m.found {
		t.Fatalf("found = true, want false when the CronJob no longer exists")
	}
}

type erroringLister struct{ err error }

func (l erroringLister) ListRaw(context.Context, kube.ResourceKind, string) ([]runtime.Object, error) {
	return nil, l.err
}

func TestLoadPermissionErrorReportsPermissionDenied(t *testing.T) {
	t.Parallel()
	lister := erroringLister{err: fmt.Errorf("cronjobs is forbidden: user cannot list")}
	m := New(Config{Session: newSession("default"), Lister: lister, Namespace: "default", Name: "nightly"})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())
	if m.state != tui.TaskStatePermissionDenied {
		t.Fatalf("state = %v, want PermissionDenied", m.state)
	}
}

// unsyncedThenSyncedLister mirrors cronjobschedule_test.go's own lister of
// the same shape: reports unsynced once, then synced, exercising §4.4's
// retry path for both required kinds (CronJob and Job).
type unsyncedThenSyncedLister struct {
	cronJobs, jobs []runtime.Object
	polls          int
	synced         bool
}

func (l *unsyncedThenSyncedLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	l.polls++
	if kind == kube.KindJob {
		return l.jobs, nil
	}
	return l.cronJobs, nil
}
func (l *unsyncedThenSyncedLister) KindSynced(kube.ResourceKind, string) bool { return l.synced }

func TestUnsyncedCronJobOrJobCacheStaysLoadingThenRetries(t *testing.T) {
	t.Parallel()
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", ResourceVersion: "1"}, Spec: batchv1.CronJobSpec{Schedule: "0 2 * * *"}}
	lister := &unsyncedThenSyncedLister{cronJobs: []runtime.Object{cj}}
	m := New(Config{Session: newSession("default"), Lister: lister, Namespace: "default", Name: "nightly"})
	m.SetSize(120, 40)
	// Deliberately not step(): applyLoaded's own retry cmd is a real
	// tea.Tick, and draining it would really sleep — same hazard every
	// other synchronous-cmd-draining test in this repo avoids.
	updated, _ := m.Update(m.load()())
	m = *updated.(*Model)
	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %v, want Loading while the cache reports unsynced", m.state)
	}
	firstPolls := lister.polls
	lister.synced = true
	m = step(t, m, tui.CacheSyncRetryMsg{Gen: m.reloadEpoch})
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready once the cache reports synced", m.state)
	}
	if lister.polls <= firstPolls {
		t.Fatalf("expected the retry to issue another ListRaw call")
	}
}

// jobForbiddenLister wraps a *fake.Cluster, answering every ListRaw call
// normally except KindJob, which returns a Forbidden-shaped error — pins §5's
// wording fix (view.go's jobsSectionHeader): a genuine Job denial must read
// "permission denied", not the raw apierrors text a stalled/retrying read
// would also produce.
type jobForbiddenLister struct {
	*fake.Cluster
	err error
}

func (l jobForbiddenLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if kind == kube.KindJob {
		return nil, l.err
	}
	return l.Cluster.ListRaw(ctx, kind, namespace)
}

// TestJobsErrForbiddenShowsPermissionDeniedWording pins §5 of
// docs/lazy-informers.md §5.6: CronJob's own read stays
// healthy, but a Forbidden Job cache used to render the JOBS section header
// with the raw client-go error text — indistinguishable from a transient
// stall that's still retrying. It must instead say "permission denied".
func TestJobsErrForbiddenShowsPermissionDeniedWording(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "nightly")
	lister := jobForbiddenLister{Cluster: c, err: fmt.Errorf("jobs is forbidden: user cannot list")}
	m := New(Config{Session: newSession("default"), Lister: lister, Mutator: c, Namespace: "default", Name: "nightly"})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready — a denied Job cache must not take over the whole screen when CronJob's own read is healthy", m.state)
	}
	if m.jobsErr == nil {
		t.Fatal("jobsErr = nil, want the Job Forbidden error carried through")
	}
	header := m.jobsSectionHeader(m.Theme(), 100)
	if !strings.Contains(header, "job history unavailable: permission denied") {
		t.Fatalf("expected permission-denied wording in the JOBS header, got %q", header)
	}
	if strings.Contains(header, "forbidden: user cannot list") {
		t.Fatalf("raw apierrors text leaked into the JOBS header: %q", header)
	}
}

// jobCacheForbiddenLister simulates *kube.Cluster's real behavior for a
// Forbidden Job cache: ListRaw(KindJob, …) returns an empty, error-free
// slice (CLAUDE.md: "a Forbidden reflector just leaves it empty"), and the
// denial only surfaces through KindSynced/KindForbidden — unlike
// jobForbiddenLister above, which returns the error synchronously from
// ListRaw, a shape a real informer-backed cache never produces.
type jobCacheForbiddenLister struct {
	*fake.Cluster
	err error
}

func (l jobCacheForbiddenLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if kind == kube.KindJob {
		return nil, nil
	}
	return l.Cluster.ListRaw(ctx, kind, namespace)
}

func (l jobCacheForbiddenLister) KindSynced(kube.ResourceKind, string) bool { return true }

func (l jobCacheForbiddenLister) KindForbidden(kind kube.ResourceKind, _ string) error {
	if kind == kube.KindJob {
		return l.err
	}
	return nil
}

// TestJobCacheForbiddenViaKindErrorMarksHistoryUnavailable pins the real-
// cluster shape TestJobsErrForbiddenShowsPermissionDeniedWording's fake
// doesn't cover: against a real *kube.Cluster, a Forbidden Job cache never
// returns an error from ListRaw itself — only KindSynced/KindForbidden say
// so. Without load()'s own tui.KindsError fallback, jobsErr would stay nil
// and the Jobs table would render a false "no retained runs" instead of
// "unavailable: permission denied".
func TestJobCacheForbiddenViaKindErrorMarksHistoryUnavailable(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "nightly")
	lister := jobCacheForbiddenLister{Cluster: c, err: fmt.Errorf("jobs is forbidden: user cannot list")}
	m := New(Config{Session: newSession("default"), Lister: lister, Mutator: c, Namespace: "default", Name: "nightly"})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready — a denied Job cache must not take over the whole screen when CronJob's own read is healthy", m.state)
	}
	if m.jobsErr == nil {
		t.Fatal("jobsErr = nil, want the Job cache's real Forbidden state carried through via tui.KindsError")
	}
	header := m.jobsSectionHeader(m.Theme(), 100)
	if !strings.Contains(header, "job history unavailable: permission denied") {
		t.Fatalf("expected permission-denied wording in the JOBS header, got %q", header)
	}
}

func TestMoveJobSelectionClampsAndPreservesOnReload(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	now := time.Now()
	c.Seed(kube.KindJob,
		succeededJob("default", "nightly-1", cj, now.Add(-3*time.Hour)),
		succeededJob("default", "nightly-2", cj, now.Add(-2*time.Hour)),
		succeededJob("default", "nightly-3", cj, now.Add(-1*time.Hour)),
	)
	m := newModel(t, c, "default", "nightly")
	if len(m.summary.Runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(m.summary.Runs))
	}
	// newest-first: index 0 = nightly-3
	m = step(t, m, tea.KeyPressMsg{Code: 'j'})
	if m.selectedJob != 1 {
		t.Fatalf("selectedJob = %d, want 1 after one down-move", m.selectedJob)
	}
	selected, _ := m.selectedJobSummary()
	if selected.Name != "nightly-2" {
		t.Fatalf("selected job = %q, want nightly-2", selected.Name)
	}
	// Reload must keep the same Job selected by name, not by index.
	m = step(t, m, reloadDueMsg{epoch: m.reloadEpoch})
	selected, _ = m.selectedJobSummary()
	if selected.Name != "nightly-2" {
		t.Fatalf("selected job after reload = %q, want nightly-2 preserved", selected.Name)
	}
	// Clamp at the top.
	m.selectedJob = 0
	m = step(t, m, tea.KeyPressMsg{Code: 'k'})
	if m.selectedJob != 0 {
		t.Fatalf("selectedJob = %d, want clamped at 0", m.selectedJob)
	}
}

func TestEnterEmitsGotoForSelectedJob(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	c.Seed(kube.KindJob, succeededJob("default", "nightly-1", cj, time.Now()))
	m := newModel(t, c, "default", "nightly")

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("expected a goto cmd for the selected Job")
	}
	msg := cmd()
	goTo, ok := msg.(tui.GotoResourceMsg)
	if !ok {
		t.Fatalf("msg = %T, want tui.GotoResourceMsg", msg)
	}
	if goTo.Kind != kube.KindJob || goTo.Namespace != "default" || goTo.Name != "nightly-1" {
		t.Fatalf("goTo = %+v, want KindJob default/nightly-1", goTo)
	}
}

func TestSiblingMovementLoadsNextCronJob(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "alpha")
	c.Seed(kube.KindCronJob, &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "beta", Namespace: "default", ResourceVersion: "1"},
		Spec:       batchv1.CronJobSpec{Schedule: "0 3 * * *"},
	})
	m := New(Config{
		Session: newSession("default"), Lister: c, Mutator: c,
		Namespace: "default", Name: "alpha",
		Siblings:     []SiblingRef{{Namespace: "default", Name: "alpha"}, {Namespace: "default", Name: "beta"}},
		SiblingIndex: 0,
	})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())
	if m.name != "alpha" {
		t.Fatalf("name = %q, want alpha before moving", m.name)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "]"})
	if m.name != "beta" || m.siblingIndex != 1 {
		t.Fatalf("after ']': name=%q siblingIndex=%d, want beta/1", m.name, m.siblingIndex)
	}
	if m.state != tui.TaskStateReady || !m.found {
		t.Fatalf("state=%v found=%v, want Ready/found after loading the sibling", m.state, m.found)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "["})
	if m.name != "alpha" || m.siblingIndex != 0 {
		t.Fatalf("after '[': name=%q siblingIndex=%d, want alpha/0", m.name, m.siblingIndex)
	}
}

func TestOpenLogsUsesCollectedPodOrReportsUnavailable(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	job := failedJob("default", "nightly-1", cj, time.Now().Add(-time.Hour))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nightly-1-abcde", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{controllerRef("Job", job.Name, job.UID)},
		},
	}
	c.Seed(kube.KindJob, job)
	c.Seed(kube.KindPod, pod)

	var openedPod kube.Pod
	opened := false
	m := New(Config{
		Session: newSession("default"), Lister: c, Mutator: c,
		Namespace: "default", Name: "nightly",
		OpenLogs: func(p kube.Pod, container string, w, h int) (tea.Model, tea.Cmd) {
			openedPod, opened = p, true
			return nil, nil
		},
	})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())
	if len(m.summary.Runs) != 1 || m.summary.Runs[0].PodName == "" {
		t.Fatalf("expected the failed run to have a collected Pod, got %+v", m.summary.Runs)
	}
	_, _ = m.Update(tea.KeyPressMsg{Text: "l"})
	if !opened || openedPod.Name != "nightly-1-abcde" {
		t.Fatalf("expected openLogs called with the collected pod, got opened=%v pod=%+v", opened, openedPod)
	}
}

func TestOpenLogsReportsUnavailableWithoutAPod(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	c.Seed(kube.KindJob, failedJob("default", "nightly-1", cj, time.Now().Add(-time.Hour)))
	opened := false
	m := New(Config{
		Session: newSession("default"), Lister: c, Mutator: c,
		Namespace: "default", Name: "nightly",
		OpenLogs: func(p kube.Pod, container string, w, h int) (tea.Model, tea.Cmd) {
			opened = true
			return nil, nil
		},
	})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())
	m2, _ := m.Update(tea.KeyPressMsg{Text: "l"})
	m = *m2.(*Model)
	if opened {
		t.Fatalf("expected openLogs not called with no Pod collected for the run")
	}
	if m.execFeedback == "" {
		t.Fatalf("expected execFeedback to explain why logs are unavailable")
	}
}

func TestRunNowOverlapWarningNamesActiveJobsUnderForbid(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	cj.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	c.Seed(kube.KindJob, activeJob("default", "nightly-active", cj, time.Now()))
	m := newModel(t, c, "default", "nightly")

	if !m.beginRunNow() {
		t.Fatalf("beginRunNow returned false, want true")
	}
	note, warn := cronJobOverlapNote(*m.pendingRun)
	if !warn {
		t.Fatalf("warn = false, want true under Forbid with an active run")
	}
	if !containsAll(note, "nightly-active", "Forbid") {
		t.Errorf("note = %q, want it to name the active job and the policy", note)
	}
}

func TestRunNowNoWarningWithoutOverlap(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "nightly")
	m := newModel(t, c, "default", "nightly")
	if !m.beginRunNow() {
		t.Fatalf("beginRunNow returned false, want true")
	}
	note, warn := cronJobOverlapNote(*m.pendingRun)
	if note != "" || warn {
		t.Fatalf("note=%q warn=%v, want no chrome at all with no active run", note, warn)
	}
}

func TestRunNowCommitCreatesJobAndSurvivesReload(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "nightly")
	m := newModel(t, c, "default", "nightly")
	m.beginRunNow()
	newName := m.pendingRun.newName

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pendingRun != nil {
		t.Fatalf("pendingRun still set after commit")
	}
	found := false
	for _, r := range m.summary.Runs {
		if r.Name == newName && r.Source == 1 { // JobSourceManual
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the manual run %q in Runs after commit+reload, got %+v", newName, m.summary.Runs)
	}
}

func TestSuspendThenResumeFlowNonProd(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "nightly")
	m := newModel(t, c, "default", "nightly")

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected an inline suspend confirm outside PROD, got active=%v tier=%v", m.actions.Active(), m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if m.summary.Object == nil || m.summary.Object.Spec.Suspend == nil || !*m.summary.Object.Spec.Suspend {
		t.Fatalf("expected spec.suspend=true after commit+reload, got %+v", m.summary.Object)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "s"})
	if m.pendingResume == nil {
		t.Fatalf("expected 's' on a suspended cronjob to stage a resume preview")
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.summary.Object == nil || (m.summary.Object.Spec.Suspend != nil && *m.summary.Object.Spec.Suspend) {
		t.Fatalf("expected spec.suspend=false after resume commit+reload, got %+v", m.summary.Object)
	}
}

// countingLister wraps *fake.Cluster to count ListRaw calls — the tick test
// needs to assert zero additional calls, which the fake cluster itself
// doesn't expose a counter for.
type countingLister struct {
	*fake.Cluster
	calls int
}

func (l *countingLister) ListRaw(ctx context.Context, kind kube.ResourceKind, ns string) ([]runtime.Object, error) {
	l.calls++
	return l.Cluster.ListRaw(ctx, kind, ns)
}

func TestClockTickNeverCallsLister(t *testing.T) {
	t.Parallel()
	c, cj := newFakeCronJob("default", "nightly")
	c.Seed(kube.KindJob, succeededJob("default", "nightly-1", cj, time.Now().Add(-time.Hour)))
	lister := &countingLister{Cluster: c}
	m := New(Config{Session: newSession("default"), Lister: lister, Mutator: c, Namespace: "default", Name: "nightly"})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())

	before := lister.calls
	next := m.now.Add(time.Second)
	m2, cmd := m.Update(tickMsg(next))
	m = *m2.(*Model)
	if m.now != next {
		t.Fatalf("now = %v, want %v", m.now, next)
	}
	if lister.calls != before {
		t.Fatalf("ListRaw call count changed from %d to %d on a clock tick", before, lister.calls)
	}
	if cmd == nil {
		t.Fatalf("expected the tick to reschedule itself")
	}
}

// TestKeysWhileLoadingNeverPopTheScreen pins review 06 M7: found is false
// until the first load lands, so the deleted banner's "any key goes back"
// used to fire on a key pressed during the initial load or while a ']'
// sibling move was loading — holding ']' walked straight out of the screen.
func TestKeysWhileLoadingNeverPopTheScreen(t *testing.T) {
	t.Parallel()
	c, _ := newFakeCronJob("default", "alpha")
	m := New(Config{
		Session: newSession("default"), Lister: c, Mutator: c,
		Namespace: "default", Name: "alpha",
		Siblings: []SiblingRef{
			{Namespace: "default", Name: "alpha"}, {Namespace: "default", Name: "beta"}, {Namespace: "default", Name: "gamma"},
		},
	})
	m.SetSize(120, 40)
	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %v, want Loading before the first load lands", m.state)
	}
	for _, key := range []string{"j", "]", "]"} {
		updated, cmd := m.Update(tea.KeyPressMsg{Text: key})
		m = *updated.(*Model)
		if cmd != nil {
			if msg := cmd(); msg != nil {
				if _, back := msg.(tui.BackMsg); back {
					t.Fatalf("%q while loading sent BackMsg, want the screen to stay", key)
				}
			}
		}
	}
	if m.name != "gamma" {
		t.Fatalf("name = %q, want gamma — ']' while a sibling loads still moves", m.name)
	}
}
