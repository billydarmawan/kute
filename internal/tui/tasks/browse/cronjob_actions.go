// §36b/§36c's staged CronJob action preflights (0.8.0 plan Phase 5):
// ctrl-r run-now and 's' resume both gather everything their preview needs
// into screen state *before* ever calling actions.Begin on a TierNone verb — the
// staging step is itself the confirmation, the same "bespoke gate, TierNone
// commit" shape scale.go's pendingScale uses for 17b's +/− prompt. Suspend
// (the dangerous direction) stays on jobs.go's beginCronJobSuspend, routed
// through actions.Controller's ordinary TierInline/TierModal confirm — this
// file only adds the marked-set (20a bulk) dispatch on top of it. Kept
// separate from jobs.go per that file's own note: this outgrew "navigation
// plus a kind's mutating verbs." The staging state, keys, will-run text
// and requests live in internal/tui/jobactions, shared with cronjobdetail
// (36e: "every mutating verb from the list works here unchanged"); this
// file keeps only what is browse's own — reading rows and the marked set,
// and where the preflight renders.
package browse

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/jobactions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// actingUser is who an action taken now is attributed to: the API server's
// reviewed username (Identity, read at the moment of the action — it lands
// after browse is built and is replaced on every context switch), else the
// kubeconfig user name (CurrentUser), else "". The SOURCE column's "manual ·
// you" compares against the same answer the stamps use.
func (m Model) actingUser() string {
	return jobactions.ActingUser(m.identity, m.currentUser)
}

// cronJobTriggerCreator is the identity §36b's run-now stamps into
// kube.AnnotationTriggeredBy — actingUser, falling back to "kute".
func (m Model) cronJobTriggerCreator() string {
	return jobactions.Creator(m.identity, m.currentUser)
}

// ---------------------------------------------------------------------
// §36b run-now
// ---------------------------------------------------------------------

// beginCronJobRunNow stages §36b's preflight for the selected CronJob row —
// screen state only, no actions.Begin yet (0.8.0 plan §36b: "ctrl-r ...
// stages a manual run in screen state"). Returns false (no-op) when nothing
// applies, mirroring beginScale's ok-bool contract.
func (m *Model) beginCronJobRunNow(row resources.Row) bool {
	if m.kind != kube.KindCronJob || m.mutator == nil || m.state != tui.TaskStateReady {
		return false
	}
	summary, ok := m.cronJobSummaryFor(row.Namespace, row.Name)
	if !ok {
		return false
	}
	t, ok := jobactions.StageRunNow(row.Namespace, row.Name, summary, m.cronJobJobsErr, m.now)
	if !ok {
		return false
	}
	m.pendingCronJobRun = t
	return true
}

// updateCronJobRunKey routes keys while pendingCronJobRun's preflight is
// showing (jobactions.RunNow.Key: esc cancels, y copies the will-run
// command, enter commits).
func (m *Model) updateCronJobRunKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	out, cmd := m.pendingCronJobRun.Key(msg.String())
	switch out {
	case jobactions.Cancel:
		m.pendingCronJobRun = nil
	case jobactions.Commit:
		return m, m.commitCronJobRun()
	}
	return m, cmd
}

// commitCronJobRun executes the staged run-now through actions.Controller at
// TierNone (0.8.0 plan §36b: "the staging step is itself the confirmation").
func (m *Model) commitCronJobRun() tea.Cmd {
	t := m.pendingCronJobRun
	m.pendingCronJobRun = nil
	m.lastCronJobRunName = t.NewName
	return m.actions.Begin(t.Action(m.isProd(), m.cronJobTriggerCreator(), m.now))
}

// cronJobRunOverlapBannerLine renders §36b's amber overlap-warning banner in
// the behavior strip's own reserved slot (view.go's cronJobBehaviorStripLine
// swaps to this while it applies) — the one filled-background line
// WarnBanner* tokens exist for (0.8.0 plan Phase 5 theme requirement 1),
// shown only for a genuine Forbid/Replace conflict (jobactions.RunNow.
// Conflict). ok is false for every other pendingCronJobRun state (no
// overlap, Allow policy, or overlap unknown — all of which stay on the
// keybar's own RightNote/RightWarnNote line instead), which
// cronJobBehaviorStripLine falls back to its ordinary content for.
func (m Model) cronJobRunOverlapBannerLine(theme tui.Theme, width int) (string, bool) {
	t := m.pendingCronJobRun
	if t == nil || !t.Conflict() {
		return "", false
	}
	fill := lipgloss.NewStyle().Background(theme.WarnBannerBg)
	warn := fill.Foreground(theme.Warn)
	text := fill.Foreground(theme.WarnBannerText)
	dim := fill.Foreground(theme.WarnBannerMuted)

	n := len(t.ActiveRuns)
	left := warn.Render(tui.GlyphCronActive) + fill.Render(" ") +
		text.Render(fmt.Sprintf("%d job%s already active", n, jobactions.PluralSuffix(n)))
	right := dim.Render(fmt.Sprintf("concurrencyPolicy: %s · bypasses policy · %s", t.Concurrency, strings.Join(t.ActiveNames(), ", ")))
	return insetStripLineFill(padBetweenFill(left, right, stripInnerWidth(width), fill), width, fill), true
}

// ---------------------------------------------------------------------
// §36c resume (and suspend/resume's marked-set dispatch)
// ---------------------------------------------------------------------

// cronJobResumeEntryFor computes row's own resume preview from its
// cache-local summary. ok is false when the summary isn't available (row
// not actually a known CronJob — defensive, since callers only ever pass
// rows already confirmed Suspended).
func (m Model) cronJobResumeEntryFor(row resources.Row) (jobactions.ResumeEntry, bool) {
	summary, ok := m.cronJobSummaryFor(row.Namespace, row.Name)
	if !ok {
		return jobactions.ResumeEntry{}, false
	}
	return jobactions.ResumeEntryFor(row.Namespace, row.Name, summary, m.now)
}

// beginCronJobResume stages §36c's resume preflight for one suspended
// CronJob row — screen-owned like beginCronJobRunNow, since resume "still
// stages first, the same as run-now, so the preview can show suspended
// duration, a bounded missed-schedule count, and the next run before
// commit" (docs/design README.md §36c).
func (m *Model) beginCronJobResume(row resources.Row) {
	if m.mutator == nil || m.state != tui.TaskStateReady {
		return
	}
	entry, ok := m.cronJobResumeEntryFor(row)
	if !ok {
		return
	}
	m.pendingCronJobResume = &jobactions.Resume{Entries: []jobactions.ResumeEntry{entry}}
}

// updateCronJobResumeKey routes keys while pendingCronJobResume's preflight
// is showing: esc cancels, enter commits (§36c: "Resume executes on ↵ with
// no destructive escalation").
func (m *Model) updateCronJobResumeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.pendingCronJobResume.Key(msg.String()) {
	case jobactions.Cancel:
		m.pendingCronJobResume = nil
	case jobactions.Commit:
		return m, m.commitCronJobResume()
	}
	return m, nil
}

// commitCronJobResume executes the staged resume(s) through
// actions.Controller at TierNone — single-target or a BulkTargets action
// for a marked set (jobactions.Resume.Action).
func (m *Model) commitCronJobResume() tea.Cmd {
	t := m.pendingCronJobResume
	m.pendingCronJobResume = nil
	return m.actions.Begin(t.Action(m.isProd(), m.now))
}

// ---------------------------------------------------------------------
// 's' direction/marked-set dispatch
// ---------------------------------------------------------------------

// beginCronJobSuspendOrResume is 's' on a CronJob row (update.go's
// dispatch): chooses direction from row when nothing's marked, or from the
// cursor row when a marked set exists (0.8.0 plan §3 Phase 5 task 7: "the
// selected row determines target direction for the set"). Suspend routes to
// jobs.go's beginCronJobSuspend (single) or beginCronJobSuspendBulk (marked)
// — both go through actions.Controller's ordinary TierInline/TierModal
// confirm. Resume stages its own preview instead (beginCronJobResume/
// beginCronJobResumeBulk) and returns nil, since staging — not a Controller
// tier — is resume's own confirmation.
func (m *Model) beginCronJobSuspendOrResume(row resources.Row) tea.Cmd {
	marked := m.markedRows()
	if len(marked) == 0 {
		if row.Suspended {
			m.beginCronJobResume(row)
			return nil
		}
		return m.beginCronJobSuspend(row)
	}
	if row.Suspended {
		m.beginCronJobResumeBulk(marked)
		return nil
	}
	return m.beginCronJobSuspendBulk(marked)
}

// beginCronJobResumeBulk stages §36c's resume preflight for every marked row
// already suspended — rows already resumed (an inconsistent mark, e.g. from
// a stale filter) are skipped rather than resumed a second time (0.8.0 plan
// §3 Phase 5 task 7: "identify rows already in the requested state").
func (m *Model) beginCronJobResumeBulk(marked []resources.Row) {
	if m.mutator == nil || m.state != tui.TaskStateReady {
		return
	}
	var entries []jobactions.ResumeEntry
	for _, row := range marked {
		if !row.Suspended {
			continue
		}
		if entry, ok := m.cronJobResumeEntryFor(row); ok {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return
	}
	m.pendingCronJobResume = &jobactions.Resume{Entries: entries}
}

// beginCronJobSuspendBulk stages a marked-set suspend through
// actions.Controller's structured BulkTargets payload (0.8.0 plan §3 Phase 5
// task 10) — non-PROD TierInline (a plain y/N naming every target), PROD
// TierModal (the type-the-count grammar 20a's bulk delete already
// established, rendered by cronjob_bulk.go's bulkCronJobSuspendConfirmModal
// rather than the single-target type-the-name modal). Rows already
// suspended are skipped, the same "don't re-apply to an in-state row" rule
// beginCronJobResumeBulk applies to its own direction.
func (m *Model) beginCronJobSuspendBulk(marked []resources.Row) tea.Cmd {
	var targets []tui.BulkTarget
	for _, row := range marked {
		if row.Suspended {
			continue
		}
		summary, ok := m.cronJobSummaryFor(row.Namespace, row.Name)
		if !ok || summary.Object == nil {
			continue
		}
		targets = append(targets, tui.BulkTarget{
			Namespace: row.Namespace, ResourceName: row.Name,
			ResourceVersion: summary.Object.ResourceVersion, Generation: summary.Object.Generation,
		})
	}
	if len(targets) == 0 {
		return nil
	}
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobSuspend, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-bulk-suspend",
		Label: fmt.Sprintf("Suspend %d cronjobs", len(targets)),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), Verb: "cronjob-suspend", IsMutating: true,
			StagedAt:    m.now,
			BulkTargets: targets,
		},
	})
}
