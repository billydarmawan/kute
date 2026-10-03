package jobattempts

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
	nsStyle := lipgloss.NewStyle().Foreground(theme.TextPrimary)
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
			tui.Crumb{Text: m.namespace, Style: nsStyle},
		)
	}
	crumbs = append(crumbs,
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "job/" + m.name, Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "Attempts", Style: text},
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
		return m.replaceConfirmModal(width, height)
	}
	switch m.state {
	case tui.TaskStateReady:
		if !m.found {
			return components.CenterLines([]string{
				lipgloss.NewStyle().Foreground(m.Theme().Bad).Bold(true).Render("Job deleted"),
				lipgloss.NewStyle().Foreground(m.Theme().TextDim).Render(m.name + " no longer exists · press any key to go back"),
			}, width, height)
		}
		if m.diffMode {
			return m.diffBody(width, height)
		}
		return m.readyBody(width)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// readyBody stacks §37b/§37d's sections top to bottom: status banner,
// failure card (only when the Job actually failed — the "answer why it's
// broken first" precedent poddetail/cronjobdetail both follow), the attempt
// table or index grid, then the spec strip.
func (m Model) readyBody(width int) string {
	theme := m.Theme()
	var lines []string
	lines = append(lines, m.statusBannerLine(theme, width))
	if band := m.failureCardLines(theme, width); len(band) > 0 {
		lines = append(lines, "", strings.Join(band, "\n"))
	}
	lines = append(lines, "", m.attemptsTable(theme, width).Render())
	lines = append(lines, "", m.specStripLine(theme, width))
	return strings.Join(lines, "\n")
}

// nonTableLineCount mirrors cronjobdetail's own — everything readyBody
// spends above the table, so tableDataRows can budget the rest.
func (m Model) nonTableLineCount(theme tui.Theme, width int) int {
	n := 1 // status banner
	if band := m.failureCardLines(theme, width); len(band) > 0 {
		n += 2
	}
	n += 1 // blank before the table
	n += 2 // blank + spec strip
	return n
}

func (m Model) tableDataRows() int {
	theme := m.Theme()
	body := tui.FrameBodyHeight(m.height, 0)
	used := m.nonTableLineCount(theme, m.width)
	return max(body-used-2, 1)
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

// jobScheduledAt approximates §37b's "scheduled HH:MM" clause for a
// cronjob-sourced Job — the Job's own creationTimestamp, the same stand-in
// resources/cronjobs.go's buildJobSummary uses for JobSourceScheduled
// ("the CronJob controller creates a scheduled Job at (or very near) its
// firing tick").
func (m Model) jobScheduledAt() (time.Time, bool) {
	if m.summary.SourceKind != resources.JobSourceCronJob || m.summary.Object == nil {
		return time.Time{}, false
	}
	if m.summary.Object.CreationTimestamp.IsZero() {
		return time.Time{}, false
	}
	return m.summary.Object.CreationTimestamp.Time, true
}

// statusBannerLine renders §37b's status banner: outcome/reason, attempt
// count vs backoffLimit, source, and "no retries remain" when applicable.
func (m Model) statusBannerLine(theme tui.Theme, width int) string {
	j := m.summary
	glyph, class, label := tui.GlyphCronPaused, resources.StatusNeutral, "Pending"
	switch {
	case j.Active:
		glyph, class, label = tui.GlyphCronActive, resources.StatusWarn, "Running"
	case j.Succeeded:
		glyph, class, label = tui.GlyphRunning, resources.StatusOK, "Succeeded"
	case j.Failed:
		glyph, class, label = "✕", resources.StatusFail, "Failed"
	}
	if j.Failed && j.Reason != "" {
		label += " · " + j.Reason
	}
	glyphStyle := lipgloss.NewStyle().Foreground(statusColor(theme, class)).Bold(true)
	textStyle := lipgloss.NewStyle().Foreground(theme.Text)
	dimStyle := lipgloss.NewStyle().Foreground(theme.TextDim)

	var parts []string
	parts = append(parts, textStyle.Render(label))
	parts = append(parts, dimStyle.Render(fmt.Sprintf("%d of %d attempts used", j.FailedAttempts, j.BackoffLimit)))
	if j.SourceKind == resources.JobSourceCronJob && j.CronJobName != "" {
		from := "from cronjob/" + j.CronJobName
		if scheduled, ok := m.jobScheduledAt(); ok {
			from += " · scheduled " + scheduled.Format("15:04")
		}
		parts = append(parts, dimStyle.Render(from))
	}
	if j.Failed && !j.RetriesRemain {
		parts = append(parts, dimStyle.Render("no retries remain"))
	}
	return glyphStyle.Render(glyph) + " " + strings.Join(parts, dimStyle.Render(" · "))
}

// failureCardLines renders §37b's failure card — the deepest real message,
// same recipe (independently reimplemented, per the codebase's own
// convention) as cronjobdetail's failureBandLines/certchain's buildFailure.
// Only rendered when the Job actually failed.
func (m Model) failureCardLines(theme tui.Theme, width int) []string {
	j := m.summary
	if !j.Failed {
		return nil
	}
	title := j.Reason
	if title == "" {
		title = "Failed"
	}
	titleStyle := lipgloss.NewStyle().Foreground(theme.Bad).Background(theme.ErrBannerBg).Bold(true)
	factStyle := lipgloss.NewStyle().Foreground(theme.BadMuted).Background(theme.ErrBannerBg)
	bodyStyle := lipgloss.NewStyle().Foreground(theme.BadText).Background(theme.ErrBannerBg)
	dimStyle := lipgloss.NewStyle().Foreground(theme.BadMuted).Background(theme.ErrBannerBg)
	fill := lipgloss.NewStyle().Background(theme.ErrBannerBg).Render(" ")

	var fact string
	if !j.CompletedAt.IsZero() {
		fact = shortAge(j.CompletedAt, m.now) + " ago"
	}
	header := titleStyle.Render("✕ "+title) + fill + fill + factStyle.Render(fact)

	var content []string
	content = append(content, header)
	if j.Message != "" {
		content = append(content, bodyStyle.Render(j.Message))
	}
	if same, ok := resources.SameFailureAcrossAttempts(j.Attempts); ok {
		if same {
			content = append(content, dimStyle.Render("same failure all attempts"))
		} else {
			content = append(content, dimStyle.Render("attempts differ — press d to compare"))
		}
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

// attemptGlyph mirrors jobGlyph's own per-attempt reading (cronjobdetail's
// unexported analog), scoped to one attempt instead of a whole summary.
func attemptGlyph(a resources.JobAttempt) (glyph string, class resources.StatusClass) {
	switch a.Result {
	case resources.JobAttemptSucceeded:
		return tui.GlyphRunning, resources.StatusOK
	case resources.JobAttemptFailed:
		return "✕", resources.StatusFail
	default:
		return tui.GlyphCronActive, resources.StatusWarn
	}
}

// attemptsTable builds §37b's flat POD table or §37d's index-grid list
// (rendered as a list rather than a 2D box grid — a documented layout
// simplification; the data — per-index status/duration, colored,
// selectable, ↵ opens that index's own attempts within the same list — is
// unchanged from the mockup).
func (m Model) attemptsTable(theme tui.Theme, width int) components.Table {
	if m.summary.Indexed {
		return m.indexGridTable(theme, width)
	}
	cols := []components.Column{
		{Title: "", Min: 2},
		{Title: "#", Min: 3, Align: components.AlignRight},
		{Title: "Pod", Min: 12, Flex: true},
		{Title: "Result", Min: 9},
		{Title: "Exit", Min: 5, Align: components.AlignRight},
		{Title: "Ran", Min: 7, Align: components.AlignRight},
		{Title: "Node", Min: 9},
		{Title: "When", Min: 7, Align: components.AlignRight},
	}
	rows := make([]components.Row, len(m.summary.Attempts))
	for i, a := range m.summary.Attempts {
		glyph, class := attemptGlyph(a)
		glyphStyle := lipgloss.NewStyle().Foreground(statusColor(theme, class))
		textStyle := lipgloss.NewStyle().Foreground(theme.Text)
		dimStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
		projected := resources.ProjectJobAttemptTable(m.summary, m.now)[i]
		rows[i] = components.Row{Cells: []components.Cell{
			{Text: glyph, Style: glyphStyle},
			{Text: projected.Cells[0], Style: dimStyle},
			{Text: projected.Cells[1], Style: textStyle},
			{Text: projected.Cells[2], Style: lipgloss.NewStyle().Foreground(statusColor(theme, class))},
			{Text: projected.Cells[3], Style: dimStyle},
			{Text: projected.Cells[4], Style: dimStyle},
			{Text: projected.Cells[5], Style: dimStyle},
			{Text: projected.Cells[6], Style: dimStyle},
		}}
	}
	return m.baseTable(theme, width, cols, rows)
}

// indexGridTable renders §37d's per-index list.
func (m Model) indexGridTable(theme tui.Theme, width int) components.Table {
	cols := []components.Column{
		{Title: "", Min: 2},
		{Title: "Idx", Min: 4, Align: components.AlignRight},
		{Title: "Result", Min: 9},
		{Title: "Attempts", Min: 8, Align: components.AlignRight},
		{Title: "Duration", Min: 9, Align: components.AlignRight},
	}
	cells := resources.ProjectJobIndexGrid(m.summary)
	var rows []components.Row
	for _, cell := range cells {
		if m.failedOnly && cell.Result() != resources.JobAttemptFailed {
			continue
		}
		glyph, class := "◌", resources.StatusNeutral
		result := "queued"
		switch cell.Result() {
		case resources.JobAttemptSucceeded:
			glyph, class, result = tui.GlyphRunning, resources.StatusOK, "complete"
		case resources.JobAttemptFailed:
			glyph, class, result = "✕", resources.StatusFail, "failed"
		case resources.JobAttemptRunning:
			glyph, class, result = tui.GlyphCronActive, resources.StatusWarn, "running"
		}
		duration := "–"
		if len(cell.Attempts) > 0 {
			last := cell.Attempts[len(cell.Attempts)-1]
			if !last.StartedAt.IsZero() {
				duration = shortDur(last.Duration(m.now))
			}
		}
		glyphStyle := lipgloss.NewStyle().Foreground(statusColor(theme, class))
		dimStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
		rows = append(rows, components.Row{Cells: []components.Cell{
			{Text: glyph, Style: glyphStyle},
			{Text: strconv.Itoa(int(cell.Index)), Style: dimStyle},
			{Text: result, Style: lipgloss.NewStyle().Foreground(statusColor(theme, class))},
			{Text: strconv.Itoa(len(cell.Attempts)), Style: dimStyle},
			{Text: duration, Style: dimStyle},
		}})
	}
	return m.baseTable(theme, width, cols, rows)
}

func (m Model) baseTable(theme tui.Theme, width int, cols []components.Column, rows []components.Row) components.Table {
	rule := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	rowsBudget := m.tableDataRows()
	return components.Table{
		Columns:        cols,
		Rows:           rows,
		Selected:       m.selectedAttempt,
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

// specStripLine renders §37b/§37d's spec strip: backoffLimit, completions/
// parallelism, deadline, ttlAfterFinished countdown (amber), and (Indexed
// only) the ETA line.
func (m Model) specStripLine(theme tui.Theme, width int) string {
	j := m.summary
	dim := lipgloss.NewStyle().Foreground(theme.TextFaint)
	value := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	sep := dim.Render(" │ ")

	var parts []string
	parts = append(parts, dim.Render("backoffLimit ")+value.Render(strconv.Itoa(int(j.BackoffLimit))))
	completions := "–"
	if j.Completions != nil {
		completions = strconv.Itoa(int(*j.Completions))
	}
	parts = append(parts, dim.Render("completions ")+value.Render(completions)+dim.Render(" · parallelism ")+value.Render(strconv.Itoa(int(j.Parallelism))))
	deadline := "–"
	if j.ActiveDeadlineSeconds != nil {
		deadline = fmt.Sprintf("%ds", *j.ActiveDeadlineSeconds)
	}
	parts = append(parts, dim.Render("deadline ")+value.Render(deadline))
	if j.TTLSecondsAfterFinished != nil && !j.CompletedAt.IsZero() {
		deleteAt := j.CompletedAt.Add(time.Duration(*j.TTLSecondsAfterFinished) * time.Second)
		remaining := deleteAt.Sub(m.now)
		if remaining > 0 {
			parts = append(parts, dim.Render("ttlAfterFinished ")+warn.Render(fmt.Sprintf("%ds", *j.TTLSecondsAfterFinished))+
				dim.Render(" · object deleted in ")+warn.Render(shortDur(remaining)))
		}
	}
	if j.Indexed {
		if eta, ok := resources.JobAttemptETA(j, m.now); ok {
			parts = append(parts, dim.Render("eta ")+value.Render(shortDur(eta)))
		}
	}
	return strings.Join(parts, sep)
}

// diffBody renders §37b's 'd' compare panel — exit-code/result comparison
// between the selected attempt and attempt 1 (a documented simplification:
// no retained log tail exists to text-diff against, see the package doc
// comment).
func (m Model) diffBody(width, height int) string {
	theme := m.Theme()
	if len(m.summary.Attempts) < 2 {
		return components.CenterLines([]string{"not enough attempts to compare"}, width, height)
	}
	a, ok := m.selectedAttemptData()
	if !ok {
		a = m.summary.Attempts[len(m.summary.Attempts)-1]
	}
	base := m.summary.Attempts[0]
	label := lipgloss.NewStyle().Foreground(theme.TextFaint)
	value := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	same := a.ExitCode != nil && base.ExitCode != nil && *a.ExitCode == *base.ExitCode

	exitCell := func(a resources.JobAttempt) string {
		if a.ExitCode == nil {
			return "–"
		}
		return strconv.Itoa(int(*a.ExitCode))
	}
	verdict := "different exit codes"
	if same {
		verdict = "same exit code"
	}
	lines := []string{
		label.Render(fmt.Sprintf("attempt %d", a.Ordinal)) + "  " + value.Render("exit "+exitCell(a)),
		label.Render("attempt 1") + "  " + value.Render("exit "+exitCell(base)),
		"",
		lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Render(verdict),
	}
	return components.CenterLines(lines, width, height)
}

// replaceConfirmModal renders job-replace's PROD type-the-name modal —
// mirrors cronjobdetail's own suspendConfirmModal.
func (m Model) replaceConfirmModal(width, height int) string {
	theme := m.Theme()
	title, target, detail := "Confirm", m.name, ""
	if pending := m.actions.Pending(); pending != nil {
		title = "✕ " + pending.Label
		target = pending.Scope.ResourceName
		detail = jobactions.ReplaceWillRunLine(pending.Scope)
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
	return components.TypeNameModal(title, "", detail, target, m.actions.TypedInput(), "replace", m.isProd(), styles, width, height)
}

// shortAge/shortDur mirror resources.shortAge's compact "12m"/"3h"/"5d"
// shape (unexported there) — same duplicate every task package keeps.
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
