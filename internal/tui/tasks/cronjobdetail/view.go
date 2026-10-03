package cronjobdetail

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	batchv1 "k8s.io/api/batch/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/jobactions"
)

func (m Model) View() tea.View { return tea.NewView(m.Render()) }

func (m Model) Render() string { return tui.Frame(m.width, m.height, m) }

func (m Model) Header() tui.HeaderState {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	ghost := lipgloss.NewStyle().Foreground(theme.TextGhost)
	ghost2 := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	text := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)

	ctxName := "cluster unavailable"
	if m.session != nil && m.session.Location.Context != "" {
		ctxName = m.session.Location.Context
	}

	crumbs := append(tui.BrandCrumbs(theme),
		tui.Crumb{Text: " │ ", Style: ghost2},
		tui.Crumb{Text: ctxName, Style: dim},
	)
	if m.namespace != "" {
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: m.namespace, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
		)
	}
	crumbs = append(crumbs,
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "CronJobs", Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: m.name, Style: text},
	)

	return tui.HeaderState{
		Crumbs:      crumbs,
		ConfigChip:  tui.BuildConfigChip(theme, m.session),
		UpdateChip:  tui.BuildUpdateChip(theme, m.session),
		ForwardChip: tui.BuildForwardChip(theme, m.session.ForwardSummary()),
		Conn:        tui.LiveConnBadge(theme, m.conn, tui.GlyphRunning+" connected"),
	}
}

func (m Model) Strips(width int) []string { return nil }

func (m Model) Body(width, height int) string {
	if m.actions.Active() && m.actions.Tier() == actions.TierModal {
		return m.suspendConfirmModal(width, height)
	}
	if m.meta != nil {
		return m.metaBody(width, height)
	}
	switch m.state {
	case tui.TaskStateReady:
		if !m.found {
			return components.CenterLines([]string{
				lipgloss.NewStyle().Foreground(m.Theme().Bad).Bold(true).Render("CronJob deleted"),
				lipgloss.NewStyle().Foreground(m.Theme().TextDim).Render(m.name + " no longer exists · press any key to go back"),
			}, width, height)
		}
		return m.readyBody(width)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// metaBody renders 26a's panel in place of the detail body while it's open —
// the CronJob's own title line stays frozen above it, the detail-screen
// analogue of browse's selected-row-above-the-panel framing (docs/design
// README.md §26a); the grid and will-run strip come from the shared
// metapanel renderer.
func (m Model) metaBody(width, height int) string {
	theme := m.Theme()
	lines := []string{m.titleLine(theme, width), ""}
	lines = append(lines, m.meta.PanelLines(width, &m.actions)...)
	lines = append(lines, "", m.meta.WillRunStrip(width, &m.actions))
	return components.Pad(strings.Join(lines, "\n"), width)
}

// readyBody stacks §36e's sections top to bottom: title row, facts grid, an
// optional failure band (promoted just below the facts, same "answer why
// it's broken first" precedent poddetail's own termination banner follows),
// and the Jobs table. height isn't a parameter here (unlike poddetail's own
// readyBody) — there's no side-by-side column to budget against; the Jobs
// table's own viewport comes from tableDataRows, which reads m.height
// directly (kept in sync with the frame's actual height by SetSize).
func (m Model) readyBody(width int) string {
	theme := m.Theme()
	var lines []string
	lines = append(lines, m.titleLine(theme, width))
	lines = append(lines, "")
	lines = append(lines, m.factsGridLines(theme)...)
	if band := m.failureBandLines(theme, width); len(band) > 0 {
		lines = append(lines, "", strings.Join(band, "\n"))
	}
	lines = append(lines, "", m.jobsSectionHeader(theme, width))
	lines = append(lines, m.jobsTable(theme, width).Render())
	return strings.Join(lines, "\n")
}

// nonTableLineCount is how many lines readyBody spends on everything above
// the Jobs table itself — tableDataRows' own budget subtracts this (plus the
// table's header/rule overhead) from the frame body height, mirroring
// nodedetail's tableDataRows/podsPanel split.
func (m Model) nonTableLineCount(theme tui.Theme, width int) int {
	n := 2 // title + blank
	n += len(m.factsGridRows()) * 2
	if band := m.failureBandLines(theme, width); len(band) > 0 {
		n += 2 // blank + the band's own (already-joined) single block line
	}
	n += 2 // blank + jobs section header
	return n
}

// tableDataRows is the Jobs table's own visible row budget — mirrors
// nodedetail's tableDataRows.
func (m Model) tableDataRows() int {
	theme := m.Theme()
	body := tui.FrameBodyHeight(m.height, 0)
	used := m.nonTableLineCount(theme, m.width)
	// -2: the table's own column-header row and ShowHeaderRule divider sit
	// outside its Height budget's data-row count (components.Table.
	// visibleRowCount already subtracts 1 for the header and 1 more for the
	// rule when ShowHeaderRule is set — passing Height = rows+2 keeps that
	// arithmetic in one place rather than duplicating it here too).
	return max(body-used-2, 1)
}

func (m Model) titleLine(theme tui.Theme, width int) string {
	if m.summary.Object == nil {
		return lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Render(m.name)
	}
	row := resources.ProjectCronJob(m.summary, m.now)
	glyphStyle := lipgloss.NewStyle().Foreground(statusColor(theme, row.GlyphClass))
	name := lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Render(m.name)
	schedule := lipgloss.NewStyle().Foreground(theme.TextPrimary).Render(m.summary.Object.Spec.Schedule)

	left := glyphStyle.Render(row.Glyph) + "  " + name + "  " + schedule
	if tz := m.summary.Object.Spec.TimeZone; tz != nil && *tz != "" {
		left += "  " + lipgloss.NewStyle().Foreground(theme.TextDim).Render(*tz)
	}
	next := row.Cells[5] // registry.go's CronJob Columns order: Name, Schedule, Susp, Act, Last Run, Next
	right := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("next ") +
		lipgloss.NewStyle().Foreground(theme.Text).Render(next)
	return padBetween(left, right, width)
}

func statusColor(theme tui.Theme, class resources.StatusClass) color.Color {
	switch class {
	case resources.StatusOK:
		return theme.Good
	case resources.StatusWarn:
		return theme.Warn
	case resources.StatusFail:
		return theme.Bad
	default:
		return theme.TextDim
	}
}

func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 || right == "" {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// factsGridRows is factsGridLines' own field list — kept separate so
// nonTableLineCount can ask "how many rows" without paying for the render.
func (m Model) factsGridRows() [][2]string {
	if m.summary.Object == nil {
		return nil
	}
	spec := m.summary.Object.Spec

	suspended := "no"
	if spec.Suspend != nil && *spec.Suspend {
		suspended = "yes"
	}
	deadline := "–"
	if spec.StartingDeadlineSeconds != nil {
		deadline = fmt.Sprintf("%ds", *spec.StartingDeadlineSeconds)
	}
	backoff := "6 (default)"
	if bl := jobTemplateBackoffLimit(m.summary.Object); bl != nil {
		backoff = strconv.Itoa(int(*bl))
	}
	succ, fail := historyLimits(spec)
	lastSchedule := "never"
	if t := lastScheduleTime(m.summary.Object); !t.IsZero() {
		lastSchedule = shortAge(t, m.now) + " ago · " + t.UTC().Format("15:04:05")
	}
	controller := "–"
	if m.controller != "" {
		controller = m.controller + " ↗"
	}

	return [][2]string{
		{"SUSPENDED", suspended},
		{"CONCURRENCY", resources.ConcurrencyPolicyLabel(spec)},
		{"DEADLINE", deadline},
		{"BACKOFF LIMIT", backoff},
		{"ACTIVE", strconv.Itoa(len(m.summary.ActiveRuns))},
		{"HISTORY KEPT", fmt.Sprintf("%d succeeded · %d failed", succ, fail)},
		{"LAST SCHEDULE", lastSchedule},
		{"CONTROLLER", controller},
	}
}

// factsGridLines renders §36e's facts grid — 4 fields per row (mock 36e's
// grid-template-columns: repeat(4, ...)), label line over value line, same
// idiom poddetail's own metaGrid uses.
func (m Model) factsGridLines(theme tui.Theme) []string {
	fields := m.factsGridRows()
	if len(fields) == 0 {
		return nil
	}
	label := lipgloss.NewStyle().Foreground(theme.TextFaint)
	value := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	linkStyle := lipgloss.NewStyle().Foreground(theme.Accent)

	var lines []string
	const perRow = 4
	for start := 0; start < len(fields); start += perRow {
		end := min(start+perRow, len(fields))
		row := fields[start:end]
		labels := make([]string, len(row))
		values := make([]string, len(row))
		for i, f := range row {
			style := value
			if f[0] == "CONTROLLER" && f[1] != "–" {
				style = linkStyle
			}
			w := max(len(f[0]), lipgloss.Width(f[1]))
			labels[i] = label.Render(components.Pad(f[0], w))
			values[i] = style.Render(f[1]) + strings.Repeat(" ", max(w-lipgloss.Width(f[1]), 0))
		}
		const gap = "    "
		lines = append(lines, strings.Join(labels, gap), strings.Join(values, gap))
	}
	return lines
}

// jobTemplateBackoffLimit reads spec.jobTemplate.spec.backoffLimit — a
// small accessor kept here (view.go, not load.go) since it's read only by
// the facts grid's own render.
func jobTemplateBackoffLimit(cj *batchv1.CronJob) *int32 {
	if cj == nil {
		return nil
	}
	return cj.Spec.JobTemplate.Spec.BackoffLimit
}

// historyLimits reads spec.successfulJobsHistoryLimit/failedJobsHistoryLimit,
// defaulting to Kubernetes' own documented defaults (3/1) when unset.
func historyLimits(spec batchv1.CronJobSpec) (succeeded, failed int32) {
	succeeded, failed = 3, 1
	if spec.SuccessfulJobsHistoryLimit != nil {
		succeeded = *spec.SuccessfulJobsHistoryLimit
	}
	if spec.FailedJobsHistoryLimit != nil {
		failed = *spec.FailedJobsHistoryLimit
	}
	return succeeded, failed
}

// failureBandLines renders §36e's filled failure band — the Job's own
// condition message plus a representative exit-code/log target when a Pod
// was actually collected (§3.5) — only when retained failure data exists.
// Returns nil (no band at all) otherwise, mirroring poddetail's own
// terminationBanner "" contract.
func (m Model) failureBandLines(theme tui.Theme, width int) []string {
	lt := m.summary.LastTerminal
	if lt == nil || !lt.Failed {
		return nil
	}
	title := lt.Reason
	if title == "" {
		title = "Failed"
	}
	titleStyle := lipgloss.NewStyle().Foreground(theme.Bad).Background(theme.ErrBannerBg).Bold(true)
	factStyle := lipgloss.NewStyle().Foreground(theme.BadMuted).Background(theme.ErrBannerBg)
	bodyStyle := lipgloss.NewStyle().Foreground(theme.BadText).Background(theme.ErrBannerBg)
	dimStyle := lipgloss.NewStyle().Foreground(theme.BadMuted).Background(theme.ErrBannerBg)

	fill := lipgloss.NewStyle().Background(theme.ErrBannerBg).Render(" ")
	header := titleStyle.Render("✕ "+title) + fill + fill +
		factStyle.Render(fmt.Sprintf("last run · %s ago · %s", shortAge(runTime(*lt), m.now), lt.Name))

	var content []string
	content = append(content, header)
	if lt.Message != "" {
		content = append(content, bodyStyle.Render(lt.Message))
	}
	var detail []string
	if lt.FailedAttempts > 0 {
		plural := ""
		if lt.FailedAttempts != 1 {
			plural = "s"
		}
		detail = append(detail, fmt.Sprintf("%d pod failure%s", lt.FailedAttempts, plural))
	}
	if lt.ExitCode != nil {
		detail = append(detail, fmt.Sprintf("exit %d", *lt.ExitCode))
	}
	if lt.PodName != "" {
		detail = append(detail, "l logs of the last failed pod")
	} else {
		detail = append(detail, "no pod collected for this run")
	}
	if len(detail) > 0 {
		content = append(content, dimStyle.Render(strings.Join(detail, " · ")))
	}

	accent := lipgloss.Border{Left: "▌"}
	style := lipgloss.NewStyle().
		Background(theme.ErrBannerBg).
		Border(accent, false, false, false, true).
		BorderForeground(theme.ErrBannerBorder).
		BorderBackground(theme.ErrBannerBg).
		Padding(0, 1).
		Width(width)
	return []string{style.Render(strings.Join(content, "\n"))}
}

// runTime is JobSummary's own "when did this run end" — mirrors
// resources.runTimestamp (unexported there), used here for the failure
// band's "last run · N ago" line.
func runTime(s resources.JobSummary) time.Time {
	switch {
	case !s.CompletedAt.IsZero():
		return s.CompletedAt
	case !s.StartedAt.IsZero():
		return s.StartedAt
	default:
		return s.ScheduledAt
	}
}

// jobsSectionHeader renders §36e's "JOBS · associated with this cronjob ·
// newest first · N retained · history limits X/Y" line, plus a right-aligned
// hint — mirrors browse's own section-header idiom.
func (m Model) jobsSectionHeader(theme tui.Theme, width int) string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true).Render("JOBS")
	succ, fail := int32(0), int32(0)
	if m.summary.Object != nil {
		succ, fail = historyLimits(m.summary.Object.Spec)
	}
	retention := fmt.Sprintf("associated with this cronjob · newest first · %d retained · history limits %d succeeded / %d failed",
		len(m.summary.Runs), succ, fail)
	if m.jobsErr != nil {
		// A permission denial gets its own wording rather than the raw
		// client-go error text, distinguishing "will never arrive" from
		// "still retrying" (docs/lazy-informers.md §5.6).
		if kube.IsPermissionError(m.jobsErr) {
			retention = "associated with this cronjob · job history unavailable: permission denied"
		} else {
			retention = "associated with this cronjob · job history unavailable: " + m.jobsErr.Error()
		}
	}
	sub := lipgloss.NewStyle().Foreground(theme.TextDim).Render(retention)
	hint := lipgloss.NewStyle().Foreground(theme.TextDim).Render("↵ opens the job")
	return padBetween(label+"  "+sub, hint, width)
}

// jobGlyph mirrors resources.cronJobStatusGlyph's per-run reading (that
// helper is unexported and scoped to a whole CronJob's own summary, not one
// run), duplicated here for the Jobs table's own leading glyph column.
func jobGlyph(s resources.JobSummary) (glyph string, class resources.StatusClass) {
	switch {
	case s.Active:
		return tui.GlyphCronActive, resources.StatusWarn
	case s.Succeeded:
		return tui.GlyphRunning, resources.StatusOK
	case s.Failed:
		return "✕", resources.StatusFail
	default:
		return "○", resources.StatusNeutral
	}
}

// jobsTable builds §36e's JOBS table — resources.AssociatedJobColumns/
// ProjectAssociatedJob, the same column set browse's own CronJob branch
// would use if it ever needed a per-CronJob Jobs table (0.8.0 plan §Phase1
// task 5's own doc comment names this screen as AssociatedJobColumns' only
// planned caller).
func (m Model) jobsTable(theme tui.Theme, width int) components.Table {
	cols := []components.Column{
		{Title: "", Min: 2},
		{Title: "Name", Min: 12, Flex: true},
		{Title: "Source", Min: 16},
		{Title: "Start", Min: 10, Align: components.AlignRight},
		{Title: "Duration", Min: 9, Align: components.AlignRight},
		{Title: "Outcome", Min: 14, Align: components.AlignRight},
	}
	rows := make([]components.Row, len(m.summary.Runs))
	for i, job := range m.summary.Runs {
		glyph, class := jobGlyph(job)
		glyphStyle := lipgloss.NewStyle().Foreground(statusColor(theme, class))
		nameStyle := lipgloss.NewStyle().Foreground(theme.Text)
		dimStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
		outcomeStyle := lipgloss.NewStyle().Foreground(statusColor(theme, class))

		projected := resources.ProjectAssociatedJob(job, m.now, resources.AssociatedJobColumns)
		rows[i] = components.Row{Cells: []components.Cell{
			{Text: glyph, Style: glyphStyle},
			{Text: projected.Cells[0], Style: nameStyle},
			{Text: projected.Cells[1], Style: dimStyle},
			{Text: projected.Cells[2], Style: dimStyle},
			{Text: projected.Cells[3], Style: dimStyle},
			{Text: projected.Cells[4], Style: outcomeStyle},
		}}
	}
	rule := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	rowsBudget := m.tableDataRows()
	return components.Table{
		Columns:        cols,
		Rows:           rows,
		Selected:       m.selectedJob,
		Offset:         m.offset,
		Width:          width,
		Height:         rowsBudget + 2,
		HeaderStyle:    lipgloss.NewStyle().Foreground(theme.TextFaint),
		FooterStyle:    lipgloss.NewStyle().Foreground(theme.TextGhost),
		SelBarStyle:    lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle:    lipgloss.NewStyle().Background(theme.SelBg),
		ShowHeaderRule: true,
		RuleStyle:      rule,
	}
}

// suspendConfirmModal renders cronjob-suspend's PROD type-the-name modal —
// mirrors browse's own typeNameConfirmModal's cronjob-suspend branch (the
// only TierModal verb this screen ever stages).
func (m Model) suspendConfirmModal(width, height int) string {
	theme := m.Theme()
	title, target, detail := "Confirm", m.name, ""
	if pending := m.actions.Pending(); pending != nil {
		title = "✕ " + pending.Label
		target = pending.Scope.ResourceName
		detail = jobactions.SuspendWillRunLine(pending.Scope)
		if m.summary.Object != nil {
			detail += "   " + jobactions.SuspendDangerNote(m.summary, m.now)
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
	return components.TypeNameModal(title, "", detail, target, m.actions.TypedInput(), "suspend", m.isProd(), styles, width, height)
}

// shortAge renders how long ago t was, relative to now — mirrors
// resources.shortAge's compact "12m"/"3h"/"5d" shape (unexported there), and
// poddetail's own duplicate of the same helper.
func shortAge(t, now time.Time) string {
	return shortDur(now.Sub(t))
}

func shortDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
