package jobactions

import (
	"strings"
	"testing"
	"time"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

type fakeIdentity struct {
	id kube.ReviewedIdentity
	ok bool
}

func (f fakeIdentity) ReviewedIdentity() (kube.ReviewedIdentity, bool) { return f.id, f.ok }

func TestCreatorPrefersReviewedIdentityThenKubeconfigThenKute(t *testing.T) {
	reviewed := fakeIdentity{id: kube.ReviewedIdentity{Username: "arn:aws:iam::123:role/dev"}, ok: true}
	cases := []struct {
		name     string
		identity IdentityReader
		user     string
		want     string
	}{
		{"reviewed wins", reviewed, "eks-admin", "arn:aws:iam::123:role/dev"},
		{"unanswered review falls back", fakeIdentity{}, "eks-admin", "eks-admin"},
		{"empty username falls back", fakeIdentity{ok: true}, "eks-admin", "eks-admin"},
		{"nil identity falls back", nil, "eks-admin", "eks-admin"},
		{"nothing known", nil, "", "kute"},
	}
	for _, c := range cases {
		if got := Creator(c.identity, c.user); got != c.want {
			t.Errorf("%s: Creator = %q, want %q", c.name, got, c.want)
		}
	}
	if got := ActingUser(nil, ""); got != "" {
		t.Errorf("ActingUser with nothing known = %q, want empty", got)
	}
}

func TestStageRerunSkipsEveryTakenName(t *testing.T) {
	taken := map[string]bool{"batch": true, "batch-rerun-1": true}
	r := StageRerun("ns", "batch", func(n string) bool { return taken[n] }, "")
	if r.NewName != "batch-rerun-2" {
		t.Fatalf("NewName = %q, want batch-rerun-2 — batch-rerun-1 already exists", r.NewName)
	}
	if r.Choice != resources.JobRerunCreate {
		t.Fatalf("Choice = %v, want create as the default", r.Choice)
	}
}

func TestRerunKeysToggleCancelAndCommit(t *testing.T) {
	r := StageRerun("ns", "batch", func(string) bool { return false }, "")
	for _, press := range []string{"down", "j"} {
		if out := r.Key(press); out != Stay {
			t.Fatalf("%s: outcome %v, want Stay", press, out)
		}
	}
	if r.Choice != resources.JobRerunCreate {
		t.Fatalf("two toggles should land back on create, got %v", r.Choice)
	}
	r.Key("k")
	if r.Choice != resources.JobRerunReplace {
		t.Fatalf("k should toggle to replace, got %v", r.Choice)
	}
	if out := r.Key("enter"); out != Commit {
		t.Errorf("enter: %v, want Commit", out)
	}
	if out := r.Key("esc"); out != Cancel {
		t.Errorf("esc: %v, want Cancel", out)
	}
	if out := r.Key("x"); out != Ignored {
		t.Errorf("x: %v, want Ignored", out)
	}
}

func TestRerunActionPerChoice(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	r := StageRerun("ns", "batch", func(n string) bool { return n == "batch" }, "")

	req, action := r.Action(true, "alice", now)
	if req.Verb.ID != verbs.JobRetry.ID || !req.Prod {
		t.Fatalf("create request = %+v, want JobRetry with prod", req)
	}
	sc := action.Scope
	if sc.Verb != "job-retry" || sc.NewName != "batch-rerun-1" || sc.TriggerCreator != "alice" || !sc.StagedAt.Equal(now) {
		t.Fatalf("create scope = %+v", sc)
	}
	if action.ID != "job-retry-ns/batch" || action.Label != "Rerun batch" {
		t.Fatalf("create action id/label = %q/%q", action.ID, action.Label)
	}

	r.Choice = resources.JobRerunReplace
	req, action = r.Action(false, "alice", now)
	if req.Verb.ID != verbs.JobReplace.ID || req.Prod {
		t.Fatalf("replace request = %+v, want JobReplace without prod", req)
	}
	if action.Scope.Verb != "job-replace" || action.Scope.NewName != "" || action.Label != "Replace batch?" {
		t.Fatalf("replace action = %+v", action)
	}
	if got, want := ReplaceWillRunLine(action.Scope), "will run: "+kube.JobReplaceCommandString("ns", "batch"); got != want {
		t.Errorf("ReplaceWillRunLine = %q, want %q", got, want)
	}
}

func TestRerunKeybarNamesTheChoiceAndOnlyAClassifiedDiagnostic(t *testing.T) {
	r := StageRerun("ns", "batch", func(n string) bool { return n == "batch" }, "")
	kb := r.Keybar()
	if kb.PillText != "RERUN" || kb.RightWarnNote != "" {
		t.Fatalf("keybar = %+v, want RERUN pill and no warn note", kb)
	}
	if want := "will run: " + kube.JobRetryCommandString("ns", "batch", "batch-rerun-1"); kb.RightNote != want {
		t.Errorf("RightNote = %q, want %q", kb.RightNote, want)
	}
	if got := kb.Groups[0][1]; got.Key != verbs.Open.Key || got.Label != "create" {
		t.Errorf("commit hint = %+v, want ↵ create", got)
	}

	r.Choice = resources.JobRerunReplace
	r.Diagnostic = "deadline exceeded"
	kb = r.Keybar()
	if kb.Groups[0][1].Label != "replace" || kb.RightWarnNote != "deadline exceeded" {
		t.Errorf("replace keybar = %+v", kb)
	}
	if !strings.Contains(kb.RightNote, kube.JobReplaceCommandString("ns", "batch")) {
		t.Errorf("replace RightNote = %q", kb.RightNote)
	}
}
