package jobactions

import (
	"fmt"
	"time"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Rerun is §37c's staged 'R' choice: create (clone into a new
// <name>-rerun-N, the default, and staging it is itself the confirmation —
// TierNone) or replace (delete and recreate under the same name, which
// stages nothing and hands off to the controller's ordinary tiered
// confirm, since it destroys the original's status and attempt history).
type Rerun struct {
	Namespace, Name string
	// NewName is the create branch's generated name — the exact name the
	// clone gets.
	NewName string
	Choice  resources.JobRerunChoice
	// Diagnostic is §37c's amber strip (resources.ClassifyJobFailure), ""
	// when the failure isn't classifiable — or when the caller doesn't hold
	// the raw Pod data classifying it needs (browse's Jobs list keeps only
	// the curated pod map, so it always passes "").
	Diagnostic string
}

// StageRerun stages the create-default choice for namespace/name. taken
// must report every Job name already in the namespace — not just this one,
// or every rerun after the first proposes the -rerun-1 that already exists
// and the create fails AlreadyExists (review 06 M6).
func StageRerun(namespace, name string, taken func(string) bool, diagnostic string) *Rerun {
	return &Rerun{
		Namespace:  namespace,
		Name:       name,
		NewName:    kube.NextRerunName(name, taken),
		Choice:     resources.JobRerunCreate,
		Diagnostic: diagnostic,
	}
}

// Key routes a press while the choice is showing: esc cancels, ↑↓ (or
// j/k — there's no text buffer) toggles create/replace, ↵ commits.
func (r *Rerun) Key(press string) Outcome {
	switch {
	case verbs.Back.Matches(press):
		return Cancel
	case verbs.Move.Matches(press):
		if r.Choice == resources.JobRerunCreate {
			r.Choice = resources.JobRerunReplace
		} else {
			r.Choice = resources.JobRerunCreate
		}
		return Stay
	case verbs.Open.Matches(press):
		return Commit
	}
	return Ignored
}

// Command is the staged choice's own will-run command.
func (r Rerun) Command() string {
	if r.Choice == resources.JobRerunCreate {
		return kube.JobRetryCommandString(r.Namespace, r.Name, r.NewName)
	}
	return kube.JobReplaceCommandString(r.Namespace, r.Name)
}

// Action is the request a commit hands to Controller.Begin: create executes
// at TierNone, replace reaches the ordinary tiered confirm. creator and now
// are stamped on the clone (kube.AnnotationTriggeredBy, triggered-at).
func (r Rerun) Action(prod bool, creator string, now time.Time) (actions.Request, tui.TaskAction) {
	if r.Choice == resources.JobRerunCreate {
		return actions.Request{Verb: verbs.JobRetry, Prod: prod}, tui.TaskAction{
			ID:    "job-retry-" + r.Namespace + "/" + r.Name,
			Label: "Rerun " + r.Name,
			Scope: tui.TaskScope{
				ResourceKind: string(kube.KindJob), ResourceName: r.Name,
				Namespace: r.Namespace, Verb: "job-retry", IsMutating: true,
				NewName: r.NewName, TriggerCreator: creator, StagedAt: now,
			},
		}
	}
	return actions.Request{Verb: verbs.JobReplace, Prod: prod}, tui.TaskAction{
		ID:    "job-replace-" + r.Namespace + "/" + r.Name,
		Label: fmt.Sprintf("Replace %s?", r.Name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindJob), ResourceName: r.Name,
			Namespace: r.Namespace, Verb: "job-replace", IsMutating: true,
		},
	}
}

// Keybar is the keybar while the choice is showing (§37c: "↑↓ create/
// replace · ↵ create|replace · esc cancel", pill RERUN), plus the amber
// diagnostic when there is one.
func (r Rerun) Keybar() tui.Keybar {
	action := "create"
	if r.Choice == resources.JobRerunReplace {
		action = "replace"
	}
	kb := tui.Keybar{
		Pill:     tui.ModeBrowse,
		PillText: "RERUN",
		Groups: [][]tui.KeyHint{{
			verbs.Move.HintAs("create/replace"),
			verbs.Open.HintAs(action),
			verbs.Back.HintAs("cancel"),
		}},
		RightNote: "will run: " + r.Command(),
	}
	if r.Diagnostic != "" {
		kb.RightWarnNote = r.Diagnostic
	}
	return kb
}

// RetryWillRunLine is job-retry's confirm "will run: ..." line. A job-retry
// passes through the controller at TierNone, so this only renders on the
// defensive path every verb's confirm note covers.
func RetryWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.JobRetryCommandString(scope.Namespace, scope.ResourceName, scope.NewName)
}

// ReplaceWillRunLine is job-replace's confirm "will run: ..." line — the
// inline y/N's note and the PROD type-the-name modal's detail.
func ReplaceWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.JobReplaceCommandString(scope.Namespace, scope.ResourceName)
}
