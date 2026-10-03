package helmdetail

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

func (m Model) View() tea.View { return tea.NewView(m.Render()) }
func (m Model) Render() string { return tui.Frame(m.width, m.height, m) }

func (m Model) Header() tui.HeaderState {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	ghost := lipgloss.NewStyle().Foreground(theme.TextGhost)
	ghost2 := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	text := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)
	ctx := "cluster unavailable"
	if m.session != nil && m.session.Location.Context != "" {
		ctx = m.session.Location.Context
	}
	crumbs := append(tui.BrandCrumbs(theme),
		tui.Crumb{Text: " │ ", Style: ghost2}, tui.Crumb{Text: ctx, Style: dim},
		tui.Crumb{Text: " › ", Style: ghost}, tui.Crumb{Text: m.release.Namespace, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
		tui.Crumb{Text: " › ", Style: ghost}, tui.Crumb{Text: "Helm Releases", Style: dim},
		tui.Crumb{Text: " › ", Style: ghost}, tui.Crumb{Text: m.release.Name, Style: text},
	)
	var forward tui.ConnBadge
	if m.session != nil {
		forward = tui.BuildForwardChip(theme, m.session.ForwardSummary())
	}
	return tui.HeaderState{Crumbs: crumbs, ConfigChip: tui.BuildConfigChip(theme, m.session), UpdateChip: tui.BuildUpdateChip(theme, m.session), ForwardChip: forward, Conn: tui.LiveConnBadge(theme, m.conn, tui.GlyphRunning+" connected")}
}

func (m Model) Strips(width int) []string {
	if m.state != tui.TaskStateReady {
		return nil
	}
	theme := m.Theme()
	tone := lipgloss.NewStyle().Foreground(statusColor(theme, m.release.Status)).Bold(true)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	status := m.release.Status + fmt.Sprintf(" · revision %d · updated %s", m.release.Revision, age(m.now, m.release.Updated))
	if m.release.StatusReason != "" {
		status += " · " + m.release.StatusReason
	}
	statusLines := wrapDiagnosticText(status, max(width-4, 1))
	lines := make([]string, 0, len(statusLines)+1)
	for i, line := range statusLines {
		line = strings.TrimRight(line, " ")
		if i == 0 && strings.HasPrefix(line, m.release.Status) {
			line = tone.Render(m.release.Status) + dim.Render(strings.TrimPrefix(line, m.release.Status))
		} else {
			line = dim.Render(line)
		}
		lines = append(lines, components.Pad("  "+line, width))
	}
	if m.diagnosis != "" {
		warning := lipgloss.NewStyle().Foreground(theme.Warn)
		for _, line := range wrapDiagnosticText(tui.GlyphWarning+" "+m.diagnosis, max(width-4, 1)) {
			lines = append(lines, components.Pad("  "+warning.Render(strings.TrimRight(line, " ")), width))
		}
	}
	return lines
}

func (m Model) Body(width, height int) string {
	switch m.state {
	case tui.TaskStateLoading:
		return components.LoadingBody(m.spinner, lipgloss.NewStyle().Foreground(m.Theme().Accent), m.feedback, width, height)
	case tui.TaskStateError, tui.TaskStatePermissionDenied:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
	if len(m.rows) == 0 {
		return components.CenterLines([]string{"This release contains no saved hooks or manifest objects."}, width, height)
	}
	theme := m.Theme()
	columns := []components.Column{{Title: "Source", Min: 15}, {Title: "Resource", Min: 18, Flex: true}, {Title: "Helm state", Min: 12}, {Title: "Live evidence", Min: 22, Flex: true}}
	table := components.Table{
		Columns: columns, Selected: m.selected, Width: width, Height: max(height, 3),
		HeaderStyle:    lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true),
		SelBarStyle:    lipgloss.NewStyle().Foreground(theme.AccentHi).Background(theme.SelBg),
		SelRowStyle:    lipgloss.NewStyle().Foreground(theme.Text).Background(theme.SelBg),
		FooterStyle:    lipgloss.NewStyle().Foreground(theme.TextGhost),
		ShowHeaderRule: true, RuleStyle: lipgloss.NewStyle().Foreground(theme.BorderSubtle),
	}
	widths := table.ColumnWidths(width)

	// Scroll in logical rows, but budget in physical lines so a wrapped row
	// remains one selection and the selected row is kept fully visible when
	// the viewport permits it. Only rows that can reach the screen are
	// wrapped and styled: walk back from the selection to the first row that
	// still fits, then render forward until the budget is spent — so a chart
	// with hundreds of objects pays for one screenful per frame, not all of
	// them.
	budget := max(height-2, 1) // sticky header + rule
	selected := min(m.selected, len(m.rows)-1)
	start := selected
	used := diagnosticRowHeight(m.rows[selected], widths, m.now)
	for start > 0 {
		h := diagnosticRowHeight(m.rows[start-1], widths, m.now)
		if used+h > budget {
			break
		}
		start--
		used += h
	}
	lines := []string{table.HeaderLine(width), lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", width))}
	remaining := budget
	for i := start; i < len(m.rows) && remaining > 0; i++ {
		rowLines := renderDiagnosticRow(m.rows[i], i == m.selected, widths, width, theme, m.now)
		if len(rowLines) > remaining {
			if i == start || i == m.selected {
				lines = append(lines, rowLines[:remaining]...)
			}
			break
		}
		lines = append(lines, rowLines...)
		remaining -= len(rowLines)
	}
	return strings.Join(lines, "\n")
}

func renderDiagnosticRow(row diagnosticRow, selected bool, widths []int, width int, theme tui.Theme, now time.Time) []string {
	typeStyle := lipgloss.NewStyle().Foreground(theme.Accent)
	if row.kindType == rowHook {
		typeStyle = lipgloss.NewStyle().Foreground(theme.Warn)
	}
	liveStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	switch {
	case row.live:
		liveStyle = lipgloss.NewStyle().Foreground(theme.Good)
	case row.unchecked:
		// Neutral: an unread cache is neither present nor missing.
		liveStyle = lipgloss.NewStyle().Foreground(theme.TextFaint)
	case strings.Contains(row.liveState, "Completed"):
		liveStyle = lipgloss.NewStyle().Foreground(theme.Warn)
	case strings.HasPrefix(row.liveState, "not created"):
		liveStyle = lipgloss.NewStyle().Foreground(theme.BadSoft)
	case strings.HasPrefix(row.liveState, "unknown"):
		liveStyle = lipgloss.NewStyle().Foreground(theme.Warn)
	}
	styles := []lipgloss.Style{
		typeStyle,
		lipgloss.NewStyle().Foreground(theme.TextPrimary),
		lipgloss.NewStyle().Foreground(theme.TextDim),
		liveStyle,
	}
	values := diagnosticRowCells(row, widths, now)
	height := 1
	for _, lines := range values {
		height = max(height, len(lines))
	}

	gapStyle := lipgloss.NewStyle()
	if selected {
		gapStyle = gapStyle.Background(theme.SelBg)
		for i := range styles {
			styles[i] = styles[i].Background(theme.SelBg)
		}
	}
	pad := func(n int) string { return gapStyle.Render(strings.Repeat(" ", max(n, 0))) }
	out := make([]string, height)
	for line := range height {
		cells := make([]string, len(values))
		for col := range values {
			text := ""
			if line < len(values[col]) {
				text = strings.TrimRight(values[col][line], " ")
			}
			cells[col] = styles[col].Render(components.Pad(text, widths[col]))
		}
		prefix := pad(2)
		if selected && line == 0 {
			prefix = lipgloss.NewStyle().Foreground(theme.AccentHi).Background(theme.SelBg).Render("▎") + pad(1)
		}
		content := prefix + strings.Join(cells, pad(2))
		out[line] = content + pad(max(width-lipgloss.Width(content), 0))
	}
	return out
}

// diagnosticRowCells is a row's four cells, wrapped to widths but unstyled.
func diagnosticRowCells(row diagnosticRow, widths []int, now time.Time) [][]string {
	return [][]string{
		wrapDiagnosticText(row.typeLabel, widths[0]),
		{components.Truncate(row.ref.Kind+"/"+row.ref.Name, widths[1])},
		wrapDiagnosticText(row.helmStateText(now), widths[2]),
		wrapDiagnosticText(row.liveState, widths[3]),
	}
}

// diagnosticRowHeight is how many physical lines renderDiagnosticRow gives
// row, without paying for its styling.
func diagnosticRowHeight(row diagnosticRow, widths []int, now time.Time) int {
	height := 1
	for _, lines := range diagnosticRowCells(row, widths, now) {
		height = max(height, len(lines))
	}
	return height
}

// wrapDiagnosticText keeps words and comma-separated hook events intact when
// they fit, while still hard-wrapping an individual token that is wider than
// its column. Inputs are plain text; styling is applied after wrapping.
func wrapDiagnosticText(value string, width int) []string {
	width = max(width, 1)
	words := strings.Fields(value)
	if len(words) == 0 {
		return []string{""}
	}
	lines := make([]string, 0, 2)
	current := ""
	flush := func() {
		if current != "" {
			lines = append(lines, current)
			current = ""
		}
	}
	for _, word := range words {
		if current != "" && lipgloss.Width(current+" "+word) <= width {
			current += " " + word
			continue
		}
		flush()
		for lipgloss.Width(word) > width {
			prefix := ""
			for _, r := range word {
				if lipgloss.Width(prefix+string(r)) > width {
					if prefix == "" {
						prefix = string(r)
					}
					break
				}
				prefix += string(r)
			}
			lines = append(lines, prefix)
			word = strings.TrimPrefix(word, prefix)
		}
		current = word
	}
	flush()
	return lines
}

func (m Model) Keybar() tui.Keybar {
	groups := [][]tui.KeyHint{{verbs.Move.Hint()}}
	if m.state == tui.TaskStateReady && len(m.rows) > 0 {
		row, _ := m.selectedRow()
		rowGroup := []tui.KeyHint{}
		if row.live {
			rowGroup = append(rowGroup, verbs.Open.Hint())
		}
		if m.openEvents != nil && row.kind != "" {
			rowGroup = append(rowGroup, verbs.Events.Hint())
		}
		if m.openYAML != nil {
			hint := verbs.YAML.Hint()
			hint.Label = "saved yaml"
			rowGroup = append(rowGroup, hint)
		}
		if len(rowGroup) > 0 {
			groups = append(groups, rowGroup)
		}
	}
	pill, pillText := tui.ModeBrowse, "HELM"
	if m.conn.Offline() {
		pill, pillText = tui.ModeOffline, "OFFLINE"
	}
	rightNote := ""
	if row, ok := m.selectedRow(); ok {
		rightNote = row.note
	}
	return tui.Keybar{Pill: pill, PillText: pillText, Groups: groups, RightNote: rightNote, RightHints: append(tui.UpdateRightHints(m.session), verbs.Help.Hint())}
}

func statusColor(theme tui.Theme, status string) color.Color {
	switch {
	case status == "deployed":
		return theme.Good
	case status == "failed":
		return theme.Bad
	case strings.HasPrefix(status, "pending-"):
		return theme.Warn
	default:
		return theme.TextDim
	}
}

func age(now, then time.Time) string {
	if then.IsZero() {
		return "unknown"
	}
	d := max(now.Sub(then), 0)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
