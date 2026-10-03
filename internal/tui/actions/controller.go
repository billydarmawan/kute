// Package actions is the shared confirm→execute framework for mutating
// operations. A screen embeds a Controller, calls Begin with the action's
// registry verb when the user presses a mutating key — Begin resolves the
// confirm policy itself (verbs.Resolve), so no call site picks a tier —
// routes confirm-state keys to Confirm/Cancel/HandleTypeKey/Escalate
// while the controller is Active, and feeds ResultMsg back through
// HandleResult. Execution runs through kube.Mutator, so no screen calls a
// write verb directly and the confirmation gate is enforced in exactly one
// place.
package actions

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// ResultMsg reports the outcome of an executed action. Screens pass it to
// HandleResult; the ActionID lets a screen ignore results for actions it no
// longer cares about.
type ResultMsg struct {
	ActionID string
	Label    string
	Err      error
	// Seq is the controller-assigned sequence number of the Begin that
	// produced this result. HandleResult compares it against the latest
	// Begin so a slow result from an earlier action (a TierNone cordon, a
	// long drain) can't wipe a newer confirm the user has since opened.
	// Zero (a hand-built message) is treated as belonging to the latest
	// action.
	Seq uint64
	// CronJobSchedule is populated only on a successful "cronjob-set-
	// schedule" action — the API's own accepted schedule/timezone plus the
	// CronJob's new resourceVersion (0.8.0 plan Phase 2 task 10). nil for
	// every other verb, and nil on failure. The schedule editor (§36d) must
	// key its refresh/undo readiness off this authoritative value rather
	// than an immediate, possibly-stale informer cache read.
	CronJobSchedule *kube.CronJobScheduleResult
}

// RestartsFailedError is a "configmap-data" ctrl-r commit's outcome when the
// patch itself landed but one or more of the consumer restarts chained after
// it didn't. It is still an error — the commit didn't fully happen — but a
// caller must not treat it like a failed patch: the value is applied and the
// other consumers did restart, so restoring the editor or retrying the whole
// chain would re-restart workloads that already rolled.
type RestartsFailedError struct {
	Total  int // consumers the commit tried to restart
	Failed []RestartFailure
}

// RestartFailure is one consumer restart that failed.
type RestartFailure struct {
	Ref kube.ConfigMapConsumerRef
	Err error
}

func (e *RestartsFailedError) Error() string {
	parts := make([]string, len(e.Failed))
	for i, f := range e.Failed {
		parts[i] = fmt.Sprintf("%s/%s failed: %v", f.Ref.Kind, f.Ref.Name, f.Err)
	}
	return fmt.Sprintf("restarted %d/%d consumers · %s", e.Total-len(e.Failed), e.Total, strings.Join(parts, "; "))
}

// TargetResult is one bulk target's individual outcome — BulkResultMsg's
// per-object detail (0.8.0 plan Phase 2 task 11): the controller owns
// iteration over a pending action's Scope.BulkTargets, so a caller sees
// which targets failed rather than one joined error that can't tell success
// from failure per object.
type TargetResult struct {
	Namespace    string
	ResourceName string
	Err          error
}

// BulkResultMsg reports a bulk action's per-target outcomes (Scope.
// BulkTargets non-empty). Screens pass it to HandleBulkResult; Results is in
// the same order as the pending action's Scope.BulkTargets.
type BulkResultMsg struct {
	ActionID string
	Label    string
	Results  []TargetResult
	// Seq is ResultMsg.Seq's bulk counterpart.
	Seq uint64
}

// Failed returns the subset of Results with a non-nil Err — the marks a
// caller should retain after a partial failure (0.8.0 plan Phase 2 task 11:
// "let the caller retain only failed marks").
func (m BulkResultMsg) Failed() []TargetResult {
	var out []TargetResult
	for _, r := range m.Results {
		if r.Err != nil {
			out = append(out, r)
		}
	}
	return out
}

// Controller holds the state of the at-most-one in-flight action for a screen.
// The zero value (no mutator) is usable: Begin reports that mutations are not
// configured rather than panicking.
type Controller struct {
	mutator kube.Mutator
	pending *tui.TaskAction
	tier    Tier
	// typed is the pending confirm's verbs.Policy.TypedName: a TierModal
	// confirm that types the name (a marked set: the count) rather than
	// answering a y/N card.
	typed bool
	// typedInput is the type-ahead buffer for a TierModal confirmation
	// (mvp-plan.md §8b: "type the name to confirm") — unused for
	// TierNone/TierInline. Never rendered via its own View(): components/
	// confirmmodal.go's TypeNameModal/TypeCountModal keep their own bespoke
	// "N/M" progress-counter + "█" cursor look (a deliberate departure from
	// every other text-entry site in the app), reading TypedName() for the
	// text and TypedCursor() for the caret — both, because a modal that read
	// only the value drew its cursor pinned to the end of the buffer while
	// every motion key silently moved the real one. This exists so
	// HandleTypeKey gets Home/End/Ctrl-arrow word-jump for free instead of
	// hand-rolling a 10th implementation. Paste reaches it through
	// PasteTarget, not from here: a bracketed paste is never a keypress.
	typedInput textfield.Model
	state      tui.TaskState
	message    string
	// result is the outcome line every controller-using screen renders in
	// its keybar (tui.Keybar.Result) — set by HandleResult/HandleBulkResult
	// and by Begin's own refusals, cleared by DismissResult on the next key
	// or by the next Begin. Kept apart from state/message so a stale result
	// can still be reported without touching a newer pending confirm.
	result tui.ResultLine
	// seq numbers every Begin; the newest one owns state/pending, and a
	// ResultMsg carrying an older Seq is stale.
	seq uint64
	// offline mirrors the screen's kube.ConnState.Offline() (docs/design
	// README.md §4a: "mutating actions disabled" while OFFLINE) — set via
	// SetOffline from each screen's ConnStateMsg handler, checked in Begin
	// so the gate lives in the one place every mutating verb funnels
	// through, not duplicated per screen/per-key.
	offline bool
}

// New builds a Controller that executes through mutator (which may be nil).
func New(mutator kube.Mutator) Controller {
	return Controller{mutator: mutator}
}

// SetOffline records whether the connection is mid-outage. Call it from the
// screen's kube.ConnStateMsg handler, passing conn.Offline() — Begin refuses
// to start any mutating action while true.
func (c *Controller) SetOffline(offline bool) {
	c.offline = offline
}

// RefuseIfOffline reports whether the connection is mid-outage, and if so
// puts Begin's own offline refusal on the result line — for a mutating
// confirm a screen runs outside Begin (browse's 20a marked-set delete), so
// it is refused the same way and with the same words.
func (c *Controller) RefuseIfOffline() bool {
	if !c.offline {
		return false
	}
	c.fail("Mutating actions are disabled while offline.")
	return true
}

// Active reports whether a confirmation prompt is currently showing.
func (c Controller) Active() bool { return c.state == tui.TaskStateConfirming }

// State is the controller's current state (empty until the first action).
func (c Controller) State() tui.TaskState { return c.state }

// Message is the human-readable status for the current state (prompt, success,
// error, or cancellation text).
func (c Controller) Message() string { return c.message }

// Result is the last action's outcome line for the screen's keybar
// (tui.Keybar.Result): a failure (red, persists until DismissResult) or a
// success (green, same lifetime — "cleared the moment you do anything
// else", docs/design README.md §26a). Empty while a confirmation is
// showing, since the keybar is the confirm prompt then.
func (c Controller) Result() tui.ResultLine {
	if c.Active() {
		return tui.ResultLine{}
	}
	return c.result
}

// DismissResult clears the outcome line. Every controller-using screen
// calls it at the top of its key handling, so a result stays on screen
// until the user acts, and the key still does what it would have. A no-op
// while a confirmation is showing (a y/n into the confirm must not throw
// away a result the user hasn't been able to see yet). Screens that render
// a result on their own spec'd surface (26a's will-run strip, 27a/27b/36d)
// also call it right after HandleResult so the keybar doesn't repeat it.
func (c *Controller) DismissResult() {
	if c.Active() {
		return
	}
	c.result = tui.ResultLine{}
}

// Pending returns the action awaiting confirmation, if any.
func (c Controller) Pending() *tui.TaskAction { return c.pending }

// Tier is the pending/active action's confirmation tier (mvp-plan.md §8b) —
// TierNone never reaches Active(), so this is only meaningful while Active()
// is true.
func (c Controller) Tier() Tier { return c.tier }

// TypedName is the current type-ahead buffer for a TierModal confirmation.
func (c Controller) TypedName() string { return c.typedInput.Value() }

// TypedCursor is the caret's rune index within TypedName — what the modal
// draws its block cursor at, so Home/End/Ctrl-arrow motion and a paste
// landing mid-buffer are visible rather than silent.
func (c Controller) TypedCursor() int { return c.typedInput.Position() }

// TypedInput returns a value copy for the type-confirmation modal. The shared
// field is value-backed, so the component can apply modal-specific styles and
// a render width without mutating controller state.
func (c Controller) TypedInput() textfield.Model { return c.typedInput }

// NameMatches reports whether TypedName equals the pending action's target
// resource name — the gate Confirm() checks for TierModal.
func (c Controller) NameMatches() bool {
	return c.pending != nil && c.typedInput.Value() == c.pending.Scope.ResourceName
}

// Request names the verb an action runs and the facts verbs.Resolve needs
// that the action's Scope can't carry. Kind, direction (Verb.ReverseID) and
// key removal come from the Scope itself, so they can't disagree with what
// executes.
type Request struct {
	Verb verbs.Verb
	// Prod is the active context's PROD tag (config.yaml's prodContexts).
	Prod bool
	// Overwrite: an add that replaces an existing key (27a/27b).
	Overwrite bool
	// Joined: a label edit whose Service-selector join is real or unknown (26a).
	Joined bool
}

// Begin starts action under r.Verb's confirm policy, resolved here through
// verbs.Resolve — the one place the destructive-action policy is decided, so
// a call site can't pass the wrong tier. TierNone executes immediately;
// TierInline/TierModal both move to the confirming state, remembering which
// (and whether the modal types the name) so screens render the right
// surface.
func (c *Controller) Begin(r Request, action tui.TaskAction) tea.Cmd {
	return c.begin(verbs.Resolve(r.Verb, target(r, action.Scope), r.Prod), action)
}

// target derives Resolve's Target from the request and the action's Scope.
func target(r Request, scope tui.TaskScope) verbs.Target {
	return verbs.Target{
		Kind:      kube.ResourceKind(scope.ResourceKind),
		Reverse:   r.Verb.ReverseID != "" && scope.Verb == r.Verb.ReverseID,
		Remove:    scope.MetaRemove || scope.SecretRemove || scope.ConfigMapRemove,
		Overwrite: r.Overwrite,
		Joined:    r.Joined,
	}
}

func (c *Controller) begin(p verbs.Policy, action tui.TaskAction) tea.Cmd {
	if c.RefuseIfOffline() {
		return nil
	}
	if c.mutator == nil {
		c.fail("Mutations are not configured.")
		return nil
	}
	// A bulk action (Scope.BulkTargets non-empty) carries no single
	// ResourceName on Scope itself — every target's own name lives in
	// BulkTargets, substituted in per-target by executeBulk — so the empty-
	// ResourceName guard is skipped for it; ResourceKind/Verb are still
	// required either way.
	if action.Scope.ResourceKind == "" || action.Scope.Verb == "" ||
		(action.Scope.ResourceName == "" && len(action.Scope.BulkTargets) == 0) {
		c.fail("Cannot run " + action.Label + ": missing target metadata.")
		return nil
	}
	c.seq++
	c.pending = &action
	c.tier = p.Tier
	c.typed = p.TypedName
	c.typedInput = textfield.New()
	c.typedInput.Focus()
	c.message = ""
	c.result = tui.ResultLine{}
	if p.Tier == TierNone {
		return c.execute()
	}
	c.state = tui.TaskStateConfirming
	return nil
}

// Confirm executes the pending action. It is a no-op unless a confirmation
// is showing, and — for a type-the-name modal (TypeToConfirm) — unless the
// typed name matches. Other TierModal verbs (Drain, PROD Argo sync) stay a
// plain y/N card: verbs.Resolve decides which.
//
// The typed-name gate is skipped outright when BulkTargets is non-empty: a
// marked-set "cronjob-suspend" PROD confirm has no single ResourceName to
// type (0.8.0 plan §3 Phase 5 task 8: "In PROD, use the existing
// type-the-count bulk grammar") — the caller (browse's own bulk-count
// modal/key routing, not this predicate) gates Confirm on the typed digit
// count matching len(BulkTargets) before ever calling this method.
func (c *Controller) Confirm() tea.Cmd {
	if c.state != tui.TaskStateConfirming || c.pending == nil {
		return nil
	}
	if c.TypeToConfirm() && len(c.pending.Scope.BulkTargets) == 0 && !c.NameMatches() {
		return nil
	}
	return c.execute()
}

// TypeToConfirm reports whether the pending confirm is a type-the-name modal
// (a marked set: type-the-count) rather than a y/N — verbs.Resolve's
// Policy.TypedName for the pending verb. Every confirm surface, key router
// and paste router asks this, never a verb list of its own.
func (c Controller) TypeToConfirm() bool {
	return c.pending != nil && c.tier == TierModal && c.typed
}

// Cancel abandons the pending confirmation.
func (c *Controller) Cancel() {
	if c.state != tui.TaskStateConfirming {
		return
	}
	label := "action"
	if c.pending != nil {
		label = c.pending.Label
	}
	c.pending = nil
	c.tier = TierNone
	c.typed = false
	c.typedInput.Blur()
	c.state = tui.TaskStateCancelled
	c.message = "Cancelled " + label + "."
}

// HandleTypeKey routes a keypress that isn't one of the type-the-name
// modal's own chords (esc/enter/verbs.ForceDelete.Key, intercepted by the caller first)
// into the type-ahead buffer — this is where Home/End and Ctrl-arrow
// word-jump arrive for free. A no-op unless a TierModal confirmation is
// active. Paste does *not* arrive here (it isn't a keypress at all) — see
// PasteTarget.
func (c *Controller) HandleTypeKey(msg tea.KeyPressMsg) tea.Cmd {
	if c.state != tui.TaskStateConfirming || c.tier != TierModal {
		return nil
	}
	var cmd tea.Cmd
	c.typedInput, cmd = c.typedInput.Update(msg)
	return cmd
}

// PasteTarget exposes the type-ahead buffer to the screen's paste router
// (tui.RoutePaste), or nil when no TierModal confirmation is active. Pasting
// the name is deliberately allowed: the gate is "you produced the exact
// name", not "you typed it by hand" — the same bar GitHub's repo-delete and
// Terraform Cloud's destroy prompts set.
func (c *Controller) PasteTarget() tui.PasteTarget {
	if c.state != tui.TaskStateConfirming || c.tier != TierModal {
		return nil
	}
	return tui.PasteInto(&c.typedInput)
}

// EscalateForceDelete is the handler for verbs.ForceDelete.Key inside any
// delete confirm: when the pending action is a Delete of a kind ForceDelete
// applies to, it becomes a force-delete under verbs.Resolve(ForceDelete, …)
// — the type-the-name modal, whether it started as an inline y/N or as
// PROD's modal. Reports whether it escalated; anything else is left
// untouched, so a modal's caller can hand an unclaimed chord on to its text
// buffer.
func (c *Controller) EscalateForceDelete(isProd bool) bool {
	p := c.pending
	if !c.Active() || p == nil || p.Scope.Verb != verbs.Delete.ID {
		return false
	}
	kind := kube.ResourceKind(p.Scope.ResourceKind)
	if !verbs.ForceDelete.AppliesTo(kind) {
		return false
	}
	c.escalate(verbs.ForceDelete.ID, verbs.Resolve(verbs.ForceDelete, verbs.Target{Kind: kind}, isProd))
	return true
}

// escalate switches the pending action to verb under policy p. The tier
// only ever rises: escalating an inline y/N to TierModal opens a fresh,
// focused type-ahead buffer, while escalating inside an already-open modal
// keeps the typed progress — it's still the same resource name either way.
// A no-op unless a confirmation is showing.
func (c *Controller) escalate(verb string, p verbs.Policy) {
	if c.state != tui.TaskStateConfirming || c.pending == nil {
		return
	}
	c.pending.Scope.Verb = verb
	c.pending.Label = capitalize(strings.ReplaceAll(verb, "-", " ")) + " " + c.pending.Scope.ResourceName + "?"
	c.typed = c.typed || p.TypedName
	if p.Tier > c.tier {
		c.tier = p.Tier
		c.typedInput = textfield.New()
		c.typedInput.Focus()
	}
}

// stale reports whether a result belongs to an action older than the
// latest Begin — whose confirm/execution now owns pending/state.
func (c *Controller) stale(seq uint64) bool {
	return seq != 0 && seq != c.seq
}

// HandleResult applies an execution outcome, transitioning to success or
// error and setting Result. A stale result (msg.Seq older than the latest
// Begin) only reports its outcome line: it never clears the newer pending
// confirm or execution.
func (c *Controller) HandleResult(msg ResultMsg) {
	label := summary(msg.Label)
	var message string
	if msg.Err != nil {
		message = fmt.Sprintf("%s failed: %v", label, msg.Err)
		c.result = tui.ResultLine{Text: message, Failed: true}
	} else {
		message = "Done: " + label + "."
		c.result = tui.ResultLine{Text: label}
	}
	if c.stale(msg.Seq) {
		return
	}
	c.pending = nil
	c.tier = TierNone
	c.typed = false
	c.typedInput.Blur()
	c.message = message
	if msg.Err != nil {
		c.state = tui.TaskStateError
		return
	}
	c.state = tui.TaskStateSuccess
}

// summary turns a confirm label phrased as a question ("Delete Pod x?")
// into the statement a result line reports.
func summary(label string) string {
	return strings.TrimSuffix(strings.TrimSpace(label), "?")
}

// HandleBulkResult is HandleResult's counterpart for a bulk action (0.8.0
// plan Phase 2 task 11): success when every target succeeded, error with a
// count when any failed. Callers that need per-target detail (e.g. to keep
// only the failed rows marked) read msg.Results/msg.Failed() themselves —
// this only drives the controller's own State()/Message().
func (c *Controller) HandleBulkResult(msg BulkResultMsg) {
	message := c.ReportBulkResult(msg)
	failed := msg.Failed()
	if c.stale(msg.Seq) {
		return
	}
	c.pending = nil
	c.tier = TierNone
	c.typed = false
	c.typedInput.Blur()
	c.message = message
	if len(failed) == 0 {
		c.state = tui.TaskStateSuccess
		return
	}
	c.state = tui.TaskStateError
}

// ReportBulkResult sets only the keybar outcome line (Result) for a bulk
// action's per-target outcomes — green "<label> (N)" when every target
// succeeded, red "N of M targets failed: <first error>" otherwise — and
// returns the matching Message() text. It never touches pending/state, so a
// bulk verb that runs its own confirm outside Begin (browse's 20a marked-set
// delete) reports through the same line without disturbing the controller.
// HandleBulkResult calls it for the controller's own bulk actions.
func (c *Controller) ReportBulkResult(msg BulkResultMsg) string {
	label := summary(msg.Label)
	failed := msg.Failed()
	var message string
	switch {
	case len(failed) == 0:
		message = fmt.Sprintf("Done: %s (%d).", label, len(msg.Results))
		c.result = tui.ResultLine{Text: fmt.Sprintf("%s (%d)", label, len(msg.Results))}
	case len(failed) == len(msg.Results):
		message = fmt.Sprintf("%s failed: all %d targets failed: %v", label, len(failed), failed[0].Err)
		c.result = tui.ResultLine{Text: message, Failed: true}
	default:
		message = fmt.Sprintf("%s: %d of %d targets failed: %v", label, len(failed), len(msg.Results), failed[0].Err)
		c.result = tui.ResultLine{Text: message, Failed: true}
	}
	return message
}

// Prompt is the confirmation question shown while Active.
func (c Controller) Prompt() string {
	if c.pending == nil {
		return ""
	}
	s := c.pending.Scope
	target := s.ResourceKind
	if s.Namespace != "" {
		target += " " + s.Namespace + "/" + s.ResourceName
	} else {
		target += " " + s.ResourceName
	}
	return fmt.Sprintf("%s %s? (y) confirm  (esc) cancel", capitalize(s.Verb), target)
}

func (c *Controller) execute() tea.Cmd {
	action := *c.pending
	mutator := c.mutator
	seq := c.seq
	c.state = tui.TaskStateLoading
	c.message = capitalize(action.Scope.Verb) + " " + action.Scope.ResourceName + "…"

	if len(action.Scope.BulkTargets) > 0 {
		return executeBulk(mutator, action, seq)
	}

	// cronjob-set-schedule is handled outside executeScope because its
	// mutator method returns a value (kube.CronJobScheduleResult) that has
	// to reach ResultMsg — every other verb's execution collapses to a
	// plain error, which executeScope's shared switch returns.
	if action.Scope.Verb == "cronjob-set-schedule" {
		return func() tea.Msg {
			result, err := mutator.SetCronJobSchedule(context.Background(),
				action.Scope.Namespace, action.Scope.ResourceName, scheduleEdit(action.Scope))
			msg := ResultMsg{ActionID: action.ID, Label: action.Label, Err: err, Seq: seq}
			if err == nil {
				msg.CronJobSchedule = &result
			}
			return msg
		}
	}

	return func() tea.Msg {
		return ResultMsg{ActionID: action.ID, Label: action.Label, Err: executeScope(mutator, action.Scope), Seq: seq}
	}
}

// executeBulk runs action.Scope.Verb once per action.Scope.BulkTargets entry
// (0.8.0 plan Phase 2 task 11), substituting each target's own Namespace/
// ResourceName/CronJobResourceVersion/CronJobGeneration onto a copy of the
// shared Scope. Every other Scope field (Verb, TriggerCreator, StagedAt, …)
// stays common across every target — only the fields a per-target
// precondition can differ on are overridden.
func executeBulk(mutator kube.Mutator, action tui.TaskAction, seq uint64) tea.Cmd {
	targets := append([]tui.BulkTarget(nil), action.Scope.BulkTargets...)
	baseScope := action.Scope
	baseScope.BulkTargets = nil
	return func() tea.Msg {
		results := make([]TargetResult, 0, len(targets))
		for _, target := range targets {
			scope := baseScope
			scope.Namespace = target.Namespace
			scope.ResourceName = target.ResourceName
			scope.CronJobResourceVersion = target.ResourceVersion
			scope.CronJobGeneration = target.Generation
			results = append(results, TargetResult{
				Namespace:    target.Namespace,
				ResourceName: target.ResourceName,
				Err:          executeScope(mutator, scope),
			})
		}
		return BulkResultMsg{ActionID: action.ID, Label: action.Label, Results: results, Seq: seq}
	}
}

// scheduleEdit builds a kube.CronJobScheduleEdit from a "cronjob-set-
// schedule" Scope — the single place that translation happens, so the
// single-target and (a future bulk-capable, per Plan §4/Phase 3) schedule
// path can never drift.
func scheduleEdit(scope tui.TaskScope) kube.CronJobScheduleEdit {
	var timezone *string
	if scope.CronJobTimeZone != nil {
		tz := *scope.CronJobTimeZone
		timezone = &tz
	}
	return kube.CronJobScheduleEdit{
		Schedule:        scope.Schedule,
		TimeZone:        timezone,
		ResourceVersion: scope.CronJobResourceVersion,
	}
}

// executeScope runs one action's mutation and returns its error — the
// shared body both execute()'s single-target path and executeBulk's
// per-target loop call, so a bulk-capable verb (§36c's marked-set suspend/
// resume) executes through exactly the same dispatch a single Enter would.
// Takes mutator explicitly rather than a *Controller receiver so a bulk
// action's per-target loop (running on a tea.Cmd goroutine) never touches
// shared controller state.
func executeScope(mutator kube.Mutator, scope tui.TaskScope) error {
	var err error
	switch scope.Verb {
	case "delete":
		err = mutator.DeleteResource(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName)
	case "force-delete":
		err = mutator.DeleteResourceForced(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName)
	case "flux-suspend", "flux-resume":
		err = mutator.SetFluxSuspend(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace,
			scope.ResourceName, scope.Verb == "flux-suspend")
	case "job-retry":
		err = mutator.RetryJob(context.Background(),
			scope.Namespace, scope.ResourceName, scope.NewName, scope.TriggerCreator, scope.StagedAt)
	case "job-replace":
		err = mutator.ReplaceJob(context.Background(), scope.Namespace, scope.ResourceName)
	case "job-suspend", "job-resume":
		err = mutator.SetJobSuspend(context.Background(),
			scope.Namespace, scope.ResourceName, scope.Verb == "job-suspend")
	case "cronjob-run-now":
		err = mutator.TriggerCronJob(context.Background(),
			scope.Namespace, scope.ResourceName, scope.NewName, scope.TriggerCreator, scope.StagedAt)
	case "cronjob-suspend", "cronjob-resume":
		err = mutator.SetCronJobSuspend(context.Background(),
			scope.Namespace, scope.ResourceName, scope.Verb == "cronjob-suspend",
			scope.CronJobResourceVersion, scope.CronJobGeneration, scope.StagedAt)
	case "cronjob-set-schedule":
		// Only reached from executeBulk (execute()'s single-target path
		// intercepts this verb before ever calling executeScope, since it
		// needs the mutator's returned CronJobScheduleResult). Schedule
		// isn't bulk-capable today, but keeping this branch means a future
		// caller that does mark a bulk schedule edit still executes
		// correctly, just without a per-target CronJobScheduleResult.
		_, err = mutator.SetCronJobSchedule(context.Background(), scope.Namespace, scope.ResourceName, scheduleEdit(scope))
	case "flux-reconcile":
		// §30b's with-source reconcile stamps the source first: asking a
		// reconciler to sync against an artifact that is still stale
		// just re-applies what it already has.
		if scope.FluxSourceName != "" {
			err = mutator.RequestFluxReconcile(context.Background(),
				kube.ResourceKind(scope.FluxSourceKind), scope.FluxSourceNamespace, scope.FluxSourceName)
		}
		if err == nil {
			err = mutator.RequestFluxReconcile(context.Background(),
				kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName)
		}
	case "argo-refresh":
		err = mutator.RequestArgoRefresh(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName)
	case "argo-sync":
		err = mutator.RequestArgoSync(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace,
			scope.ResourceName, scope.ArgoSyncRevision)
	case "rollout-restart":
		err = mutator.RolloutRestart(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName)
	case "cert-renew":
		err = mutator.RenewCertificate(context.Background(), scope.Namespace, scope.ResourceName)
	case "cordon":
		err = mutator.Cordon(context.Background(), scope.ResourceName, true)
	case "uncordon":
		err = mutator.Cordon(context.Background(), scope.ResourceName, false)
	case "drain":
		_, err = mutator.Drain(context.Background(), scope.ResourceName)
	case "rollback":
		err = mutator.HelmRollback(context.Background(), scope.Namespace, scope.ResourceName, scope.Revision)
	case "rollout-undo":
		err = mutator.RolloutUndo(context.Background(), scope.Namespace, scope.ResourceName, scope.Revision)
	case "scale":
		err = mutator.Scale(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName, scope.Replicas)
	case "set-image":
		err = mutator.SetImage(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName,
			scope.Container, scope.Image, scope.InitContainer)
	case "set-resources":
		err = mutator.SetResources(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName, scope.Container, *scope.Resources, false)
	case "set-meta":
		err = mutator.PatchMeta(context.Background(),
			kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName,
			scope.MetaIsAnnotation, scope.MetaKey, scope.MetaValue, scope.MetaRemove)
	case "secret-data":
		err = mutator.PatchSecretData(context.Background(),
			scope.Namespace, scope.ResourceName, scope.DataResourceVersion, scope.SecretKey, scope.SecretValue, scope.SecretRemove)
	case "configmap-data":
		err = mutator.PatchConfigMapData(context.Background(),
			scope.Namespace, scope.ResourceName, scope.DataResourceVersion, scope.ConfigMapKey, scope.ConfigMapValue, scope.ConfigMapRemove)
		if err == nil && scope.ConfigMapRestartConsumers {
			partial := &RestartsFailedError{Total: len(scope.ConfigMapConsumers)}
			for _, ref := range scope.ConfigMapConsumers {
				if rerr := mutator.RolloutRestart(context.Background(), ref.Kind, scope.Namespace, ref.Name); rerr != nil {
					partial.Failed = append(partial.Failed, RestartFailure{Ref: ref, Err: rerr})
				}
			}
			if len(partial.Failed) > 0 {
				err = partial
			}
		}
	default:
		err = fmt.Errorf("unsupported verb %q", scope.Verb)
	}
	return err
}

func (c *Controller) fail(message string) {
	if c.Active() {
		// A refused Begin over an open confirm reports the refusal without
		// wiping the confirm (same rule as a stale result).
		c.result = tui.ResultLine{Text: message, Failed: true}
		return
	}
	c.pending = nil
	c.state = tui.TaskStateError
	c.message = message
	c.result = tui.ResultLine{Text: message, Failed: true}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
