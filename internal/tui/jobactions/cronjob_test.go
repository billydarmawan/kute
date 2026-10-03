package jobactions

import (
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

var cronNow = time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)

func cronSummary(policy batchv1.ConcurrencyPolicy, active ...string) resources.CronJobSummary {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "nightly", ResourceVersion: "42", Generation: 7},
		Spec:       batchv1.CronJobSpec{Schedule: "0 * * * *", ConcurrencyPolicy: policy},
	}
	s := resources.CronJobSummary{Object: cj}
	for _, n := range active {
		j := resources.JobSummary{Namespace: "ns", Name: n}
		s.Runs = append(s.Runs, j)
		s.ActiveRuns = append(s.ActiveRuns, j)
	}
	return s
}

func TestStageRunNowNeedsAnObject(t *testing.T) {
	if _, ok := StageRunNow("ns", "nightly", resources.CronJobSummary{}, nil, cronNow); ok {
		t.Fatal("staged a run-now with no CronJob object")
	}
}

func TestStageRunNowAvoidsAnExistingManualName(t *testing.T) {
	first := kube.ManualJobName("nightly", cronNow, func(string) bool { return false })
	r, ok := StageRunNow("ns", "nightly", cronSummary(batchv1.AllowConcurrent, first), nil, cronNow)
	if !ok {
		t.Fatal("not staged")
	}
	if r.NewName == first || !strings.HasPrefix(r.NewName, "nightly-manual-") {
		t.Fatalf("NewName = %q, want a -manual- name other than the taken %q", r.NewName, first)
	}
}

func TestRunNowOverlapTreatments(t *testing.T) {
	cases := []struct {
		name      string
		policy    batchv1.ConcurrencyPolicy
		active    []string
		jobsErr   error
		wantNote  string
		wantWarn  bool
		wantClash bool
	}{
		{"no overlap is zero chrome", batchv1.ForbidConcurrent, nil, nil, "", false, false},
		{"unknown overlap warns", batchv1.AllowConcurrent, nil, errors.New("forbidden"), "overlap unknown", true, false},
		{"forbid warns", batchv1.ForbidConcurrent, []string{"a"}, nil, "1 job already active (a) · manual run bypasses concurrencyPolicy: Forbid", true, true},
		{"replace warns", batchv1.ReplaceConcurrent, []string{"a", "b"}, nil, "2 jobs already active (a, b)", true, true},
		{"allow informs", batchv1.AllowConcurrent, []string{"a"}, nil, "concurrencyPolicy: Allow, no conflict", false, false},
	}
	for _, c := range cases {
		r, _ := StageRunNow("ns", "nightly", cronSummary(c.policy, c.active...), c.jobsErr, cronNow)
		note, warn := r.OverlapNote()
		if c.wantNote == "" && note != "" || !strings.Contains(note, c.wantNote) || warn != c.wantWarn {
			t.Errorf("%s: OverlapNote = %q, %v; want %q, %v", c.name, note, warn, c.wantNote, c.wantWarn)
		}
		if r.Conflict() != c.wantClash {
			t.Errorf("%s: Conflict = %v, want %v", c.name, r.Conflict(), c.wantClash)
		}
		kb := r.Keybar()
		if !strings.HasPrefix(kb.RightNote, "will run: "+r.Command()) {
			t.Errorf("%s: RightNote = %q, want it to lead with the will-run command", c.name, kb.RightNote)
		}
		if c.wantWarn != (kb.RightWarnNote != "") {
			t.Errorf("%s: RightWarnNote = %q, want warn=%v", c.name, kb.RightWarnNote, c.wantWarn)
		}
	}
}

func TestRunNowKeysAndAction(t *testing.T) {
	r, _ := StageRunNow("ns", "nightly", cronSummary(batchv1.AllowConcurrent), nil, cronNow)
	if out, cmd := r.Key(verbs.CronJobCopyCommand.Key); out != Stay || cmd == nil {
		t.Errorf("copy: %v/%v, want Stay with a clipboard command", out, cmd)
	}
	if out, _ := r.Key("enter"); out != Commit {
		t.Errorf("enter: %v, want Commit", out)
	}
	if out, _ := r.Key("esc"); out != Cancel {
		t.Errorf("esc: %v, want Cancel", out)
	}
	if out, cmd := r.Key("j"); out != Ignored || cmd != nil {
		t.Errorf("j: %v, want Ignored", out)
	}
	req, action := r.Action(true, "alice", cronNow)
	if req.Verb.ID != verbs.CronJobRunNow.ID || !req.Prod {
		t.Fatalf("request = %+v", req)
	}
	sc := action.Scope
	if sc.Verb != "cronjob-run-now" || sc.NewName != r.NewName || sc.TriggerCreator != "alice" || !sc.StagedAt.Equal(cronNow) {
		t.Fatalf("scope = %+v", sc)
	}
}

func TestResumeEntryUnknownStartIsNeverAFabricatedCount(t *testing.T) {
	e, ok := ResumeEntryFor("ns", "nightly", cronSummary(batchv1.AllowConcurrent), cronNow)
	if !ok {
		t.Fatal("no entry")
	}
	line := e.SummaryLine()
	if !strings.HasPrefix(line, "suspended · start unknown · suspension start unknown · next ") {
		t.Errorf("SummaryLine = %q", line)
	}
	if e.ResourceVersion != "42" || e.Generation != 7 {
		t.Errorf("precondition = %q/%d, want 42/7", e.ResourceVersion, e.Generation)
	}
}

func TestResumeEntryCountsMissedRuns(t *testing.T) {
	s := cronSummary(batchv1.AllowConcurrent)
	utc := "UTC"
	s.Object.Spec.TimeZone = &utc
	at := cronNow.Add(-3 * time.Hour)
	s.SuspendedAt = &at
	e, _ := ResumeEntryFor("ns", "nightly", s, cronNow)
	if e.MissedCount != 3 || !e.SuspendedKnown {
		t.Fatalf("entry = %+v, want 3 missed hourly runs and a known start", e)
	}
	if !strings.Contains(e.SummaryLine(), "3 schedules missed while suspended") {
		t.Errorf("SummaryLine = %q", e.SummaryLine())
	}
}

func TestMissedRunsCopy(t *testing.T) {
	cases := []struct {
		count            int
		trunc, eligible  bool
		reason, contains string
	}{
		{0, false, true, "", "no schedules missed while suspended"},
		{1, false, true, "", "1 schedule missed while suspended · Kubernetes may start one"},
		{150, true, true, "", "100+ schedules missed"},
		{4, false, false, "", "none eligible to backfill"},
		{4, false, true, "timezone unknown", "timezone unknown"},
	}
	for _, c := range cases {
		if got := MissedRunsCopy(c.count, c.trunc, c.eligible, c.reason); !strings.Contains(got, c.contains) {
			t.Errorf("MissedRunsCopy(%d,%v,%v,%q) = %q, want it to contain %q", c.count, c.trunc, c.eligible, c.reason, got, c.contains)
		}
	}
}

func TestResumeActionSingleAndBulk(t *testing.T) {
	a := ResumeEntry{Namespace: "ns", Name: "a", ResourceVersion: "1", Generation: 2}
	b := ResumeEntry{Namespace: "ns", Name: "b", ResourceVersion: "3", Generation: 4}

	one := Resume{Entries: []ResumeEntry{a}}
	req, action := one.Action(false, cronNow)
	if req.Verb.ID != verbs.CronJobSuspend.ID || action.Scope.Verb != "cronjob-resume" ||
		action.Scope.ResourceName != "a" || action.Scope.CronJobResourceVersion != "1" || action.Scope.CronJobGeneration != 2 {
		t.Fatalf("single resume = %+v / %+v", req, action)
	}
	if kb := one.Keybar(); !strings.HasPrefix(kb.RightNote, "resume a? ") || kb.PillText != "RESUME" {
		t.Errorf("single keybar = %+v", kb)
	}

	both := Resume{Entries: []ResumeEntry{a, b}}
	_, action = both.Action(true, cronNow)
	if action.ID != "cronjob-bulk-resume" || len(action.Scope.BulkTargets) != 2 || action.Scope.BulkTargets[1].ResourceVersion != "3" {
		t.Fatalf("bulk resume = %+v", action)
	}
	if kb := both.Keybar(); kb.RightNote != "resume 2 cronjobs? a, b" {
		t.Errorf("bulk keybar note = %q", kb.RightNote)
	}
	if out := both.Key("enter"); out != Commit {
		t.Errorf("enter: %v", out)
	}
	if out := both.Key("esc"); out != Cancel {
		t.Errorf("esc: %v", out)
	}
}

func TestSuspendActionAndNotes(t *testing.T) {
	s := cronSummary(batchv1.AllowConcurrent, "nightly-1")
	req, action := SuspendAction("ns", "nightly", s, true, cronNow)
	if req.Verb.ID != verbs.CronJobSuspend.ID || !req.Prod || action.Scope.Verb != "cronjob-suspend" ||
		action.Scope.CronJobResourceVersion != "42" || action.Label != "Suspend nightly?" {
		t.Fatalf("suspend = %+v / %+v", req, action)
	}
	if got, want := SuspendWillRunLine(action.Scope), "will run: "+kube.CronJobSuspendCommandString("ns", "nightly", true); got != want {
		t.Errorf("SuspendWillRunLine = %q, want %q", got, want)
	}
	if note := SuspendDangerNote(s, cronNow); !strings.HasPrefix(note, "running jobs unaffected · nightly-1 · ") {
		t.Errorf("SuspendDangerNote = %q", note)
	}
	if note := SuspendDangerNote(cronSummary(batchv1.AllowConcurrent), cronNow); !strings.HasPrefix(note, "no active jobs right now · ") {
		t.Errorf("SuspendDangerNote without active runs = %q", note)
	}
	if note := SuspendDangerNote(resources.CronJobSummary{}, cronNow); note != "" {
		t.Errorf("SuspendDangerNote without an object = %q, want empty", note)
	}

	_, action = SuspendAction("ns", "gone", resources.CronJobSummary{}, false, cronNow)
	if action.Scope.CronJobResourceVersion != "" || action.Scope.CronJobGeneration != 0 {
		t.Errorf("objectless suspend carries a precondition: %+v", action.Scope)
	}
}

func TestSuspendHintFollowsDirection(t *testing.T) {
	if h := SuspendHint(false); h.Key != verbs.CronJobSuspend.Key || h.Label != verbs.CronJobSuspend.Label {
		t.Errorf("SuspendHint(false) = %+v", h)
	}
	if h := SuspendHint(true); h.Key != verbs.CronJobSuspend.Key || h.Label != "resume" {
		t.Errorf("SuspendHint(true) = %+v", h)
	}
}
