// ctrl-d delete machinery for 8b (docs/design README.md §8b, mvp-plan.md
// §Phase 5): any row, any kind (verbs.Delete.Kinds is nil — "all kinds").
// Kept in its own file, browse's per-concern split convention (like
// nodes.go/sort.go/grouping.go/metrics.go) rather than sprinkled through
// model.go/view.go/update.go.
package browse

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// isDeleteVerb reports whether verb is the delete family 8b's type-the-name
// modal handles — the narrower set typeNameConfirmModal needs to tell apart
// from every other verb Controller.TypeToConfirm also routes here (9a's
// rollout-restart, 36c's cronjob-suspend), since only the delete family
// carries an owner/grace-period detail.
func isDeleteVerb(verb string) bool {
	return verb == "delete" || verb == "force-delete"
}

// typingConfirmName reports whether the type-the-name modal is the active
// confirm surface — i.e. whether there's a text buffer on screen at all.
// updateConfirmKey routes keys by it and pasteTarget routes pastes by it, so
// the two can't disagree about where typed input is going. Reads
// Controller.TypeToConfirm directly rather than a browse-local copy of its
// verb list (0.8.0 plan §3 Phase 3 task 11) — browse's TierModal surface is
// always this one predicate's verbs; rollout-undo is timeline's screen and
// never reaches here, but including it costs nothing since browse never
// stages that verb.
func (m Model) typingConfirmName() bool {
	return m.actions.TypeToConfirm()
}

// isProd reports whether the active session's current context is tagged
// prod in ~/.config/kute/config.yaml — the same source 7a's context
// palette PROD tag reads (internal/tui/context.go).
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// deleteWillRunLine renders 8b's inline (non-PROD) confirm line — the exact
// `kubectl delete` command that will run, replacing the generic
// actions.Controller.Prompt() text (which only duplicated the y/n hints
// already in the keybar's Groups). Kept prefix-free like rollbackPrompt:
// insetChromeLine (tui/chrome.go) drops the whole RightNote rather than
// truncating it when the keybar line overflows width.
func deleteWillRunLine(scope tui.TaskScope) string {
	return kube.DeleteCommandString(kube.ResourceKind(scope.ResourceKind), scope.Namespace, []string{scope.ResourceName})
}

// beginDelete confirms deleting row — inline y/N in non-prod contexts, the
// full type-the-name modal in PROD, and always the modal for a Namespace,
// CustomResourceDefinition (14b) or Node, which delete far more than the one row
// (verbs.Resolve owns that rule). Owner is only known for the Pod kind
// (via m.pods, the fuller kube.Pod projection Row doesn't carry) — every
// other kind's modal simply omits the "will be recreated" line.
func (m *Model) beginDelete(row resources.Row) tea.Cmd {
	var owner string
	var gracePeriod *int64
	if m.kind == kube.KindPod {
		if pod, ok := m.pods[kube.PodKey(row.Namespace, row.Name)]; ok {
			owner = pod.Owner
			gracePeriod = &pod.GracePeriodSeconds
		}
	}
	return m.actions.Begin(actions.Request{Verb: verbs.Delete, Prod: m.isProd()}, tui.TaskAction{
		ID:                 "delete-" + string(m.kind) + "-" + row.Namespace + "/" + row.Name,
		Label:              fmt.Sprintf("Delete %s %s?", singularDisplay(m.desc.Display), row.Name),
		Owner:              owner,
		GracePeriodSeconds: gracePeriod,
		Scope: tui.TaskScope{
			ResourceKind: string(m.kind),
			ResourceName: row.Name,
			Namespace:    row.Namespace,
			Verb:         "delete",
			IsMutating:   true,
		},
	})
}

// typeNameConfirmModal renders the type-the-name modal for a pending verb
// Controller.TypeToConfirm routes here (every other TierModal verb, i.e.
// Drain, keeps rendering through nodes.go's existing confirmBody/
// ConfirmCard, untouched by this file). The delete family gets 8b's owner/
// grace-period detail and its force-delete hint (verbs.ForceDeleteModalHint); rollout-restart (9a)
// and cronjob-suspend (36c) have neither concept, so each gets its own exact
// "will run: kubectl …" line instead, the same one its non-prod inline
// confirm's keybar already shows.
func (m Model) typeNameConfirmModal(width, height int) string {
	theme := m.Theme()
	title := "Confirm"
	target := ""
	detail := ""
	actionVerb := "delete"
	var ownerLine string
	if pending := m.actions.Pending(); pending != nil {
		title = "✕ " + pending.Label
		target = pending.Scope.ResourceName
		switch {
		case pending.Scope.Verb == "rollback":
			// 18a's PROD escalation ("Rollback inherits 8b friction") — the
			// release name, and the exact helm command the inline confirm's
			// keybar shows outside PROD.
			title = "⇅ " + pending.Label
			actionVerb = "rollback"
			detail = rollbackDetail(m.session.CommandTarget(), pending.Scope)
		case pending.Scope.Verb == "rollout-restart":
			actionVerb = "restart"
			detail = rolloutRestartWillRunLine(pending.Scope)
		case pending.Scope.Verb == "cronjob-suspend" && len(pending.Scope.BulkTargets) == 0:
			// PROD escalation only — cronjob-resume never reaches TierModal
			// (verbs.Resolve's CronJobSuspend row), so this branch is single-target
			// suspend only; a marked-set PROD suspend routes to
			// cronjob_bulk.go's own type-the-count modal instead (Body()'s
			// dispatch, pendingBulkCronJobSuspend).
			actionVerb = "suspend"
			detail = cronJobSuspendWillRunLine(pending.Scope)
		case pending.Scope.Verb == "job-suspend":
			// v0.9.0 §37a: PROD escalation only (verbs.Resolve's JobSuspend row never
			// escalates job-resume) — the same asymmetric shape cronjob-suspend
			// above has.
			actionVerb = "suspend"
			detail = jobSuspendWillRunLine(pending.Scope)
		case pending.Scope.Verb == "job-replace":
			// §37c's destructive rerun choice — reaches here every time it's
			// chosen in PROD (never staged-TierNone the way job-retry's own
			// "create" path is; see job_actions.go's own doc comment).
			actionVerb = "replace"
			detail = jobReplaceWillRunLine(pending.Scope)
		case isDeleteVerb(pending.Scope.Verb):
			if pending.Owner != "" {
				ownerLine = pending.Owner + " — will be recreated"
			}
			if pending.Scope.Verb == "force-delete" {
				detail = "grace period 0 — force delete, immediate"
			} else {
				// docs/design README.md §8b: the concrete figure (e.g. "30s"),
				// not a generic "default grace period applies" — falls back to
				// the generic text when the caller didn't resolve one (every
				// non-Pod kind).
				if pending.GracePeriodSeconds != nil {
					detail = fmt.Sprintf("grace period %ds applies", *pending.GracePeriodSeconds)
				} else {
					detail = "default grace period applies"
				}
				if verbs.ForceDelete.AppliesTo(kube.ResourceKind(pending.Scope.ResourceKind)) {
					detail += " · " + verbs.ForceDeleteModalHint()
				}
			}
		}
	}

	styles := components.TypeModalStyles{
		Border:    lipgloss.NewStyle().BorderForeground(theme.ConfirmBorder).Background(theme.ConfirmHeaderBg),
		Title:     lipgloss.NewStyle().Foreground(theme.Bad).Bold(true).Background(theme.ConfirmHeaderBg),
		ProdTag:   lipgloss.NewStyle().Foreground(theme.ProdText).Bold(true).Background(theme.ConfirmHeaderBg),
		Owner:     lipgloss.NewStyle().Foreground(theme.Good).Background(theme.ConfirmHeaderBg),
		Detail:    lipgloss.NewStyle().Foreground(theme.TextSecondary).Background(theme.ConfirmHeaderBg),
		Rule:      lipgloss.NewStyle().Foreground(theme.TextGhost).Background(theme.ConfirmHeaderBg),
		Input:     lipgloss.NewStyle().Foreground(theme.Text).Background(theme.ConfirmHeaderBg),
		Selection: lipgloss.NewStyle().Foreground(theme.Bg).Background(theme.Accent),
		Progress:  lipgloss.NewStyle().Foreground(theme.TextFaint).Background(theme.ConfirmHeaderBg),
		Key:       lipgloss.NewStyle().Foreground(theme.Bad).Background(theme.ConfirmHeaderBg),
		Label:     lipgloss.NewStyle().Foreground(theme.TextDim).Background(theme.ConfirmHeaderBg),
	}
	return components.TypeNameModal(title, ownerLine, detail, target, m.actions.TypedInput(), actionVerb, m.isProd(), styles, width, height)
}
