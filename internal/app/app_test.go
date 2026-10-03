package app

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/tui/tasks/cronjobdetail"
)

// TestOpenCronJobDetailFuncDemoPushesARealScreen pins 0.8.0 plan Phase 8's
// composition-root wiring: OpenCronJobDetail is no longer nil once app.go
// builds it (browse's own tests already prove ↵ calls whatever opener it's
// handed — see browse_jobs_test.go's TestEnterOnCronJobPushesDetailWith... —
// but only app.go can prove the *real* factory produces a working
// tasks/cronjobdetail screen rather than leaving it wired to nothing, the
// gap TestEnterOnCronJobNoopsWithoutDetailWired documented as "true until
// Phase 7/8 land"). Drives Init's own load command directly (like browse's
// and cronjobdetail's own CronJob tests do) rather than draining every
// returned tea.Cmd, since the screen's own §4.5 UI clock and spinner both
// reschedule themselves once a tick/frame and would hang a naive recursive
// drain on real wall-clock time.
func TestOpenCronJobDetailFuncDemoPushesARealScreen(t *testing.T) {
	task, _ := demoCronJobDetailTask(t, "nightly-backup")

	view := ansi.Strip(task.View().Content)
	if !strings.Contains(view, "nightly-backup") {
		t.Errorf("rendered cronjobdetail view never names the loaded CronJob:\n%s", view)
	}
	if !strings.Contains(view, "associated with this cronjob") {
		t.Errorf("rendered cronjobdetail view never shows its Jobs section (Job cache wiring missing?):\n%s", view)
	}
}

// TestOpenCronJobDetailFuncDemoRunNowStampsTheWiredCreator drives §36b's
// ctrl-r/enter run-now flow through the exact composition openCronJobDetail-
// Func builds, and reads the created Job back off the fake cluster — the
// end-to-end proof that Phase 8's currentUser plumbing (real:
// cluster.Context.UserName, demo: fake.Cluster.CurrentUser()) actually
// reaches kube.AnnotationTriggeredBy, not just that the parameter compiles.
func TestOpenCronJobDetailFuncDemoRunNowStampsTheWiredCreator(t *testing.T) {
	task, demoCluster := demoCronJobDetailTask(t, "nightly-backup")
	ctx := t.Context()

	before, err := demoCluster.ListRaw(ctx, kube.KindJob, "default")
	if err != nil {
		t.Fatalf("ListRaw(Job) before run-now: %v", err)
	}

	updated, _ := task.Update(tea.KeyPressMsg{Text: "R"})
	updated, cmd := updated.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected Enter to return the run-now mutation command")
	}
	result := cmd()
	updated, cmd = updated.Update(result)
	if cmd != nil {
		// The reload triggered by a successful mutation — run it so the
		// created Job is visible in the fake cluster before we read it
		// back (the mutation itself already landed synchronously either
		// way, but this mirrors how the screen would actually settle).
		_, _ = updated.Update(cmd())
	}

	after, err := demoCluster.ListRaw(ctx, kube.KindJob, "default")
	if err != nil {
		t.Fatalf("ListRaw(Job) after run-now: %v", err)
	}
	created := newJobs(before, after)
	if len(created) != 1 {
		t.Fatalf("expected exactly one new Job from run-now, got %d (%v)", len(created), created)
	}
	job := created[0]
	if !strings.Contains(job.Name, "nightly-backup-manual-") {
		t.Fatalf("created Job name = %q, want a nightly-backup-manual-* name", job.Name)
	}
	wantCreator := demoCluster.CurrentUser()
	if wantCreator == "" {
		t.Fatal("demo fixture never set a CurrentUser (fake.NewDemo should call SetUserName)")
	}
	if got := job.Annotations[kube.AnnotationTriggeredBy]; got != wantCreator {
		t.Fatalf("created Job's %s annotation = %q, want the wired creator %q", kube.AnnotationTriggeredBy, got, wantCreator)
	}
}

// demoCronJobDetailTask builds tasks/cronjobdetail exactly the way app.go's
// openCronJobDetailFunc does for --demo, and runs Init's load command once
// to bring it to a ready state — the shared setup both tests above need.
func demoCronJobDetailTask(t *testing.T, name string) (*cronjobdetail.Model, *fake.Cluster) {
	t.Helper()
	sess, _, err := BuildSession(Config{AppName: DefaultAppName, Demo: true})
	if err != nil {
		t.Fatalf("BuildSession: %v", err)
	}
	demoCluster := fake.NewDemo()
	sess.Forwards = kube.NewForwardManager()

	openLogs := openLogsFunc(sess, demoCluster, demoCluster)
	openYAML := openYAMLFunc(sess, demoCluster)
	openSchedule := openCronJobScheduleFunc(sess, demoCluster)
	openDetail := openCronJobDetailFunc(sess, demoCluster, openLogs, openYAML, openSchedule, demoCluster.CurrentUser())

	task, cmd := openDetail("default", name, nil, 0, 120, 36)
	cd, ok := task.(*cronjobdetail.Model)
	if !ok {
		t.Fatalf("expected *cronjobdetail.Model, got %T", task)
	}
	if cmd == nil {
		t.Fatal("expected Init to return a load command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("expected Init's batched load/spinner/clock commands, got %#v", cmd())
	}
	// batch[0] is m.load() (see cronjobdetail.Model.Init) — the only one of
	// the three safe to run synchronously; the other two are the spinner
	// and §4.5 clock ticks, which block for real time and reschedule
	// themselves forever.
	updated, _ := cd.Update(batch[0]())
	ready, ok := updated.(*cronjobdetail.Model)
	if !ok {
		t.Fatalf("expected Update to keep returning *cronjobdetail.Model, got %T", updated)
	}
	return ready, demoCluster
}

// TestNewModelDemoScopeNamespaceSelectsNamespace pins the fix for
// docs/lazy-informers.md §5.6 in demo mode: --namespace-
// scoped must select the namespace the same way -n does there (the fake has
// no informers to scope). BuildSession already sets sess.Location.Namespace
// correctly from cfg.ScopeNamespace; without this fix NewModel's own demo
// branch silently overwrote it with the fake cluster's default namespace
// right after.
func TestNewModelDemoScopeNamespaceSelectsNamespace(t *testing.T) {
	model, _, _ := NewModel(Config{AppName: DefaultAppName, Demo: true, ScopeNamespace: "team-a"})
	if got := model.Session().Location.Namespace; got != "team-a" {
		t.Fatalf("Session().Location.Namespace = %q, want team-a", got)
	}
}

// TestNewModelDemoScopeNamespaceOutranksNamespaceFlag mirrors the real-
// cluster precedence cmd/kute's conflictingScopeFlags already enforces (the
// two flags are mutually exclusive) — belt and suspenders here, since a
// caller that somehow set both should still get ScopeNamespace's answer,
// the same slot cfg.Namespace holds on a real cluster.
func TestNewModelDemoScopeNamespaceOutranksNamespaceFlag(t *testing.T) {
	model, _, _ := NewModel(Config{AppName: DefaultAppName, Demo: true, ScopeNamespace: "team-a", Namespace: "team-b"})
	if got := model.Session().Location.Namespace; got != "team-a" {
		t.Fatalf("Session().Location.Namespace = %q, want team-a (ScopeNamespace should outrank Namespace)", got)
	}
}

// newJobs returns the Jobs present in after but not in before, keyed by
// name — run-now's own "what got created" diff.
func newJobs(before, after []runtime.Object) []*batchv1.Job {
	seen := map[string]bool{}
	for _, obj := range before {
		if j, ok := obj.(*batchv1.Job); ok {
			seen[j.Name] = true
		}
	}
	var out []*batchv1.Job
	for _, obj := range after {
		j, ok := obj.(*batchv1.Job)
		if ok && !seen[j.Name] {
			out = append(out, j)
		}
	}
	return out
}
