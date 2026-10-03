// §36b/§36c's staged CronJob action preflights (0.8.0 plan Phase 5):
// ctrl-r run-now and 's' resume both gather everything their preview needs
// into screen state *before* ever calling actions.Begin on a TierNone verb — the
// staging step is itself the confirmation, the same "bespoke gate, TierNone
// commit" shape scale.go's pendingScale uses for 17b's +/− prompt. Suspend
// (the dangerous direction) stays on jobs.go's beginCronJobSuspend, routed
// through actions.Controller's ordinary TierInline/TierModal confirm — this
// file only adds the marked-set (20a bulk) dispatch on top of it. Kept
// separate from jobs.go per that file's own note: this outgrew "navigation
// plus a kind's mutating verbs."
package browse

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// cronJobTriggerCreator is the identity §36b's run-now stamps into
// kube.AnnotationTriggeredBy — m.currentUser (wired from the active
// context's kube.Context.UserName by app.go's composition root, Phase 8)
// falling back to a generic, still-truthful "kute" rather than an empty
// annotation value when that wiring hasn't happened yet (demo mode, or a
// test model with no CurrentUser set).
func (m Model) cronJobTriggerCreator() string {
	if m.currentUser != "" {
		return m.currentUser
	}
	return "kute"
}

// ---------------------------------------------------------------------
// §36b run-now
// ---------------------------------------------------------------------

// cronJobRunTarget is the state pendingCronJobRun gates on while §36b's
// ctrl-r preflight is showing.
type cronJobRunTarget struct {
	namespace, name string
	newName         string
	concurrency     string
	// activeRuns is the CronJob's own ActiveRuns snapshot at staging time —
	// re-staging (esc, ctrl-r again) always re-reads the live summary, so
	// this never goes stale while the preflight sits open.
	activeRuns []resources.JobSummary
	// unknownOverlap is true when the Job cache couldn't be read (§4.4 task
	// 6): "never treat a failed Job read as evidence that no run is
	// active" — the overlap card renders "overlap unknown" rather than
	// silently taking the no-conflict path.
	unknownOverlap bool
}

// beginCronJobRunNow stages §36b's preflight for the selected CronJob row —
// screen state only, no actions.Begin yet (0.8.0 plan §36b: "ctrl-r ...
// stages a manual run in screen state"). Returns false (no-op) when nothing
// applies, mirroring beginScale's ok-bool contract.
func (m *Model) beginCronJobRunNow(row resources.Row) bool {
	if m.kind != kube.KindCronJob || m.mutator == nil || m.state != tui.TaskStateReady {
		return false
	}
	summary, ok := m.cronJobSummaryFor(row.Namespace, row.Name)
	if !ok || summary.Object == nil {
		return false
	}
	taken := make(map[string]bool, len(summary.Runs))
	for _, r := range summary.Runs {
		taken[r.Name] = true
	}
	m.pendingCronJobRun = &cronJobRunTarget{
		namespace:      row.Namespace,
		name:           row.Name,
		newName:        kube.ManualJobName(row.Name, m.now, func(n string) bool { return taken[n] }),
		concurrency:    resources.ConcurrencyPolicyLabel(summary.Object.Spec),
		activeRuns:     summary.ActiveRuns,
		unknownOverlap: m.cronJobJobsErr != nil,
	}
	return true
}

// updateCronJobRunKey routes keys while pendingCronJobRun's preflight is
// showing: esc cancels, ctrl-r/y copies the will-run command
// (verbs.CronJobCopyCommand), enter commits.
func (m *Model) updateCronJobRunKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pendingCronJobRun = nil
	case "enter":
		return m, m.commitCronJobRun()
	case verbs.CronJobCopyCommand.Key:
		return m, tea.SetClipboard(cronJobRunCommand(*m.pendingCronJobRun))
	}
	return m, nil
}

// commitCronJobRun executes the staged run-now through actions.Controller at
// TierNone (0.8.0 plan §36b: "the staging step is itself the confirmation")
// — clones a new standalone Job from the CronJob's jobTemplate, never
// touching the CronJob itself.
func (m *Model) commitCronJobRun() tea.Cmd {
	t := m.pendingCronJobRun
	m.pendingCronJobRun = nil
	m.lastCronJobRunName = t.newName
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobRunNow, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-run-now-" + t.namespace + "/" + t.name,
		Label: fmt.Sprintf("Run %s now", t.name),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: t.name,
			Namespace: t.namespace, Verb: "cronjob-run-now", IsMutating: true,
			NewName:        t.newName,
			TriggerCreator: m.cronJobTriggerCreator(),
			StagedAt:       m.now,
		},
	})
}

// cronJobRunCommand is the exact `kubectl create job ... --from=cronjob/...`
// invocation TriggerCronJob is equivalent to — the same command both the
// keybar's will-run line and 'y' copy show, so they can never disagree.
func cronJobRunCommand(t cronJobRunTarget) string {
	return kube.CronJobTriggerCommandString(t.namespace, t.name, t.newName)
}

// cronJobOverlapNote is §36b's overlap card text: "" (no chrome at all) when
// there's no active run, an "overlap unknown" warning when the Job cache
// couldn't be read, a warning naming the active Job(s) under Forbid/Replace,
// or a quieter informational note under Allow (docs/design README.md §36b:
// "Forbid/Replace concurrency get warning treatment, Allow gets quieter
// informational treatment"). warn selects which of the two treatments a
// caller should render the note with.
func cronJobOverlapNote(t cronJobRunTarget) (note string, warn bool) {
	if t.unknownOverlap {
		return "overlap unknown — job history could not be listed", true
	}
	if len(t.activeRuns) == 0 {
		return "", false
	}
	names := make([]string, len(t.activeRuns))
	for i, r := range t.activeRuns {
		names[i] = r.Name
	}
	plural := ""
	if len(t.activeRuns) != 1 {
		plural = "s"
	}
	joined := strings.Join(names, ", ")
	switch t.concurrency {
	case "Forbid", "Replace":
		return fmt.Sprintf("%d job%s already active (%s) · manual run bypasses concurrencyPolicy: %s · next scheduled run unaffected",
			len(t.activeRuns), plural, joined, t.concurrency), true
	default: // Allow
		return fmt.Sprintf("%d job%s already active (%s) · concurrencyPolicy: Allow, no conflict",
			len(t.activeRuns), plural, joined), false
	}
}

// cronJobRunKeybar is Keybar()'s rendering while pendingCronJobRun is
// showing (docs/design README.md §36b: "↵ run · y copy · esc cancel", "a
// complete Job view with no overlap renders zero warning chrome — just the
// will-run line").
func (m Model) cronJobRunKeybar() tui.Keybar {
	t := *m.pendingCronJobRun
	kb := tui.Keybar{
		Pill: tui.ModeBrowse, PillText: "RUN NOW",
		Groups: [][]tui.KeyHint{{
			{Key: "↵", Label: "run"},
			{Key: verbs.CronJobCopyCommand.Key, Label: verbs.CronJobCopyCommand.Label},
			{Key: "esc", Label: "cancel"},
		}},
		RightNote: "will run: " + cronJobRunCommand(t),
	}
	if note, warn := cronJobOverlapNote(t); note != "" {
		if warn {
			kb.RightWarnNote = note
		} else {
			kb.RightNote += "   " + note
		}
	}
	return kb
}

// cronJobRunOverlapBannerLine renders §36b's amber overlap-warning banner in
// the behavior strip's own reserved slot (view.go's cronJobBehaviorStripLine
// swaps to this while it applies) — the one filled-background line
// WarnBanner* tokens exist for (0.8.0 plan Phase 5 theme requirement 1),
// shown only for a genuine Forbid/Replace conflict. ok is false for every
// other pendingCronJobRun state (no overlap, Allow policy, or overlap
// unknown — all of which stay on the keybar's own RightNote/RightWarnNote
// line instead), which cronJobBehaviorStripLine falls back to its ordinary
// content for.
func (m Model) cronJobRunOverlapBannerLine(theme tui.Theme, width int) (string, bool) {
	t := m.pendingCronJobRun
	if t == nil || t.unknownOverlap || len(t.activeRuns) == 0 {
		return "", false
	}
	if t.concurrency != "Forbid" && t.concurrency != "Replace" {
		return "", false
	}
	fill := lipgloss.NewStyle().Background(theme.WarnBannerBg)
	warn := fill.Foreground(theme.Warn)
	text := fill.Foreground(theme.WarnBannerText)
	dim := fill.Foreground(theme.WarnBannerMuted)

	names := make([]string, len(t.activeRuns))
	for i, r := range t.activeRuns {
		names[i] = r.Name
	}
	left := warn.Render(tui.GlyphCronActive) + fill.Render(" ") +
		text.Render(fmt.Sprintf("%d job%s already active", len(t.activeRuns), pluralSuffix(len(t.activeRuns))))
	right := dim.Render(fmt.Sprintf("concurrencyPolicy: %s · bypasses policy · %s", t.concurrency, strings.Join(names, ", ")))
	return insetStripLineFill(padBetweenFill(left, right, stripInnerWidth(width), fill), width, fill), true
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---------------------------------------------------------------------
// §36c resume (and suspend/resume's marked-set dispatch)
// ---------------------------------------------------------------------

// cronJobResumeEntry is one CronJob's own resume preview — computed once at
// staging time from its cache-local summary, so the panel never re-reads
// the clock or the lister while it's open (only the commit re-executes).
type cronJobResumeEntry struct {
	namespace, name, resourceVersion string
	generation                       int64
	// suspendedLabel/suspendedKnown are resources.SuspendedDuration's own
	// pair — "" / false renders "start unknown" rather than a fabricated
	// duration (§3.3: externally suspended, or a stale generation marker).
	suspendedLabel string
	suspendedKnown bool
	missedCount    int
	missedTrunc    bool
	missedEligible bool
	missedReason   string
	nextRun        string
}

// cronJobResumeTarget is the state pendingCronJobResume gates on while
// §36c's resume preflight is showing — one entry for the selected row, N for
// a marked-set bulk resume.
type cronJobResumeTarget struct {
	entries []cronJobResumeEntry
}

// cronJobResumeEntryFor computes row's own resume preview from its
// cache-local summary. ok is false when the summary isn't available (row
// not actually a known CronJob — defensive, since callers only ever pass
// rows already confirmed Suspended).
func (m Model) cronJobResumeEntryFor(row resources.Row) (cronJobResumeEntry, bool) {
	summary, ok := m.cronJobSummaryFor(row.Namespace, row.Name)
	if !ok || summary.Object == nil {
		return cronJobResumeEntry{}, false
	}
	spec := summary.Object.Spec
	suspendedLabel, suspendedKnown := resources.SuspendedDuration(summary.SuspendedAt, m.now)

	var missedCount int
	var missedTrunc, missedEligible bool
	var missedReason string
	if summary.SuspendedAt != nil {
		missedCount, missedTrunc, missedEligible, missedReason = resources.MissedRuns(spec, *summary.SuspendedAt, m.now)
	} else {
		missedReason = "suspension start unknown"
	}

	return cronJobResumeEntry{
		namespace: row.Namespace, name: row.Name,
		resourceVersion: summary.Object.ResourceVersion, generation: summary.Object.Generation,
		suspendedLabel: suspendedLabel, suspendedKnown: suspendedKnown,
		missedCount: missedCount, missedTrunc: missedTrunc, missedEligible: missedEligible, missedReason: missedReason,
		nextRun: resources.NextRunLabel(spec, m.now),
	}, true
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
	m.pendingCronJobResume = &cronJobResumeTarget{entries: []cronJobResumeEntry{entry}}
}

// updateCronJobResumeKey routes keys while pendingCronJobResume's preflight
// is showing: esc cancels, enter commits (§36c: "Resume executes on ↵ with
// no destructive escalation").
func (m *Model) updateCronJobResumeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pendingCronJobResume = nil
	case "enter":
		return m, m.commitCronJobResume()
	}
	return m, nil
}

// commitCronJobResume executes the staged resume(s) through
// actions.Controller at TierNone — a single-target action.Begin for one
// entry, or a BulkTargets action for a marked-set resume (0.8.0 plan §3
// Phase 5 task 9: "Bulk resume uses one Enter preflight because it is the
// reversible direction").
func (m *Model) commitCronJobResume() tea.Cmd {
	t := m.pendingCronJobResume
	m.pendingCronJobResume = nil
	if len(t.entries) == 1 {
		e := t.entries[0]
		return m.actions.Begin(actions.Request{Verb: verbs.CronJobSuspend, Prod: m.isProd()}, tui.TaskAction{
			ID:    "cronjob-resume-" + e.namespace + "/" + e.name,
			Label: "Resume " + e.name,
			Scope: tui.TaskScope{
				ResourceKind: string(kube.KindCronJob), ResourceName: e.name,
				Namespace: e.namespace, Verb: "cronjob-resume", IsMutating: true,
				CronJobResourceVersion: e.resourceVersion,
				CronJobGeneration:      e.generation,
				StagedAt:               m.now,
			},
		})
	}
	targets := make([]tui.BulkTarget, len(t.entries))
	for i, e := range t.entries {
		targets[i] = tui.BulkTarget{Namespace: e.namespace, ResourceName: e.name, ResourceVersion: e.resourceVersion, Generation: e.generation}
	}
	return m.actions.Begin(actions.Request{Verb: verbs.CronJobSuspend, Prod: m.isProd()}, tui.TaskAction{
		ID:    "cronjob-bulk-resume",
		Label: fmt.Sprintf("Resume %d cronjobs", len(targets)),
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), Verb: "cronjob-resume", IsMutating: true,
			StagedAt:    m.now,
			BulkTargets: targets,
		},
	})
}

// missedRunsCopy renders §3.3's truthful missed-schedule sentence:
//
//	"N schedules were missed · Kubernetes may start one immediately on resume"
//
// — explicit about starting-deadline ineligibility, an unset/unknown
// timezone, or an unknown suspension start (reason non-empty), and silent
// (a plain "no schedules missed" statement) when count is genuinely 0.
func missedRunsCopy(count int, truncated, eligible bool, reason string) string {
	if reason != "" {
		return reason
	}
	if count == 0 {
		return "no schedules missed while suspended"
	}
	label := strconv.Itoa(count)
	if truncated {
		label = "100+"
	}
	plural := pluralSuffix(count)
	if eligible {
		return fmt.Sprintf("%s schedule%s missed while suspended · Kubernetes may start one immediately on resume", label, plural)
	}
	return fmt.Sprintf("%s schedule%s missed while suspended · none eligible to backfill (starting deadline elapsed)", label, plural)
}

// cronJobResumeSummaryLine renders one entry's full preview text — the
// keybar RightNote for a single-target resume.
func cronJobResumeSummaryLine(e cronJobResumeEntry) string {
	suspended := "suspended · start unknown"
	if e.suspendedKnown {
		suspended = "suspended " + e.suspendedLabel
	}
	return fmt.Sprintf("%s · %s · next %s", suspended,
		missedRunsCopy(e.missedCount, e.missedTrunc, e.missedEligible, e.missedReason), e.nextRun)
}

// cronJobResumeKeybar is Keybar()'s rendering while pendingCronJobResume is
// showing — single-target gets the full suspended-duration/missed-run/next
// preview (docs/design README.md §36c); a marked-set bulk resume gets a
// quieter aggregate line naming the targets, since a per-target breakdown
// doesn't fit one keybar row.
func (m Model) cronJobResumeKeybar() tui.Keybar {
	t := *m.pendingCronJobResume
	kb := tui.Keybar{
		Pill: tui.ModeBrowse, PillText: "RESUME",
		Groups: [][]tui.KeyHint{{
			{Key: "↵", Label: "resume"},
			{Key: "esc", Label: "cancel"},
		}},
	}
	if len(t.entries) == 1 {
		e := t.entries[0]
		kb.RightNote = fmt.Sprintf("resume %s? %s", e.name, cronJobResumeSummaryLine(e))
		return kb
	}
	names := make([]string, len(t.entries))
	for i, e := range t.entries {
		names[i] = e.name
	}
	kb.RightNote = fmt.Sprintf("resume %d cronjobs? %s", len(t.entries), strings.Join(names, ", "))
	return kb
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
	var entries []cronJobResumeEntry
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
	m.pendingCronJobResume = &cronJobResumeTarget{entries: entries}
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
