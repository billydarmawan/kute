package podlogs

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
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
	container, _ := m.activeContainer()

	crumbs := append(tui.BrandCrumbs(theme),
		tui.Crumb{Text: " │ ", Style: ghost2},
		tui.Crumb{Text: ctxName, Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: m.pod.Name, Style: text},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "logs", Style: dim},
	)
	if container != "" {
		crumbs = append(crumbs, tui.Crumb{Text: " · " + container, Style: dim})
	}

	conn := tui.ConnBadge{Text: tui.GlyphFollowing + " following", Style: lipgloss.NewStyle().Foreground(theme.Good)}
	switch {
	case m.conn.Offline():
		// Stream state and cluster reachability are independent: a dropped
		// follow request becomes a terminal stream error, while the active
		// context can later recover underneath this still-open task. Show the
		// shared offline vocabulary during the outage, then fall back to the
		// stream's own following/paused/error badge once connectivity returns.
		conn = tui.LiveConnBadge(theme, m.conn, tui.GlyphFollowing+" following")
	case m.stream == StreamError:
		conn = tui.ConnBadge{Text: tui.GlyphFailed + " error", Style: lipgloss.NewStyle().Foreground(theme.Bad)}
	case m.stream == StreamWaitingForContainer:
		conn = tui.ConnBadge{Text: tui.GlyphPending + " starting", Style: lipgloss.NewStyle().Foreground(theme.Warn)}
	case !m.view.AutoScroll:
		conn = tui.ConnBadge{Text: "⏸ paused", Style: dim}
	}

	return tui.HeaderState{Crumbs: crumbs, ConfigChip: tui.BuildConfigChip(theme, m.session), UpdateChip: tui.BuildUpdateChip(theme, m.session), ForwardChip: tui.BuildForwardChip(theme, m.session.ForwardSummary()), Conn: conn}
}

// Strips is the toolbar (docs/design README.md §5b): container + since +
// wrap + timestamps on the left, severity-in-view counts on the right —
// plus, while filtering, a second line mirroring browse's filter strip.
func (m Model) Strips(width int) []string {
	if m.stream == StreamWaitingForContainer {
		return []string{m.waitingStripLine(width)}
	}
	if m.taskState() == tui.TaskStateLoading {
		return []string{m.loadingStripLine(width)}
	}
	if m.taskState() != tui.TaskStateReady {
		// A terminal stream error keeps accepting '/' filter input so the
		// screen remains navigable. Keep that input visible even though the
		// body is now the terminal error state rather than cached log rows.
		if m.filterActive {
			return []string{m.filterStripLine(width)}
		}
		return nil
	}
	lines := []string{m.toolbarLine(width)}
	if m.filterActive {
		lines = append(lines, m.filterStripLine(width))
	}
	return lines
}

func (m Model) loadingStripLine(width int) string {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	container, _ := m.activeContainer()
	left := lipgloss.NewStyle().Foreground(theme.Warn).Render(m.spinner.View()) + " " +
		dim.Render("loading logs for "+container+"…")
	right := faint.Render("history loads before follow starts")
	return insetStripLine(padBetween(left, right, stripInnerWidth(width)), width)
}

// waitingStripLine is 5b's new pre-connect state: the active container
// hasn't started yet, so there's no stream to describe a spinner/progress
// for — just the container's own reason, in the app's standing "pending/
// not-yet-settled" vocabulary (tui.GlyphPending + theme.Warn, the pairing
// browse/nodedetail/overview/timeline/helmhistory all use).
func (m Model) waitingStripLine(width int) string {
	theme := m.Theme()
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	container, _ := m.activeContainer()
	left := warn.Render(tui.GlyphPending + " waiting for container " + container + " to start: " + m.waitingReason)
	right := faint.Render("streams automatically once running")
	return insetStripLine(padBetween(left, right, stripInnerWidth(width)), width)
}

func (m Model) toolbarLine(width int) string {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	text := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	bad := lipgloss.NewStyle().Foreground(theme.Bad)

	container, _ := m.activeContainer()
	parts := []string{dim.Render("container ") + text.Render(container)}
	if len(m.pod.Containers) > 1 {
		next := m.pod.Containers[m.nextContainerIndex()]
		parts[0] += dim.Render(" (tab: " + next + ")")
	}
	parts = append(parts, dim.Render("since ")+text.Render(m.sinceLabel()))
	parts = append(parts, dim.Render("wrap ")+text.Render(onOff(m.view.Wrap)))
	parts = append(parts, dim.Render("timestamps ")+text.Render(onOff(m.view.Timestamps)))
	left := strings.Join(parts, dim.Render("  ·  "))

	warnCount, errCount := m.visibleSeverityCounts()
	right := warn.Render(fmt.Sprintf("%d WRN", warnCount)) + dim.Render(" · ") +
		bad.Render(fmt.Sprintf("%d ERR", errCount)) + dim.Render(" in view")

	return insetStripLine(padBetween(left, right, stripInnerWidth(width)), width)
}

// filterStripLine mirrors browse's live "/" query strip (view.go's
// filterStripLine there) — same left query/marker + right matched/total
// shape, applied to log entries instead of table rows, plus the same "N
// hidden by filter — esc to clear" notice once the query hides lines
// (docs/design system-wide interactions: "items never silently disappear").
func (m Model) filterStripLine(width int) string {
	theme := m.Theme()
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	total, matched := len(m.buffer.Entries), m.matchedCount()
	right := dim.Render(fmt.Sprintf("%d/%d", matched, total))
	if matched < total {
		right = faint.Render(fmt.Sprintf("%d hidden by filter — esc to clear   ", total-matched)) + right
	}
	inputWidth := max(stripInnerWidth(width)-2-lipgloss.Width(right)-1, 1)
	left := accent.Render("/ ") + m.filterInput.ViewWidth(inputWidth)
	return insetStripLine(padBetween(left, right, stripInnerWidth(width)), width)
}

func (m Model) Body(width, height int) string {
	if state := m.taskState(); state != tui.TaskStateReady {
		if m.stream == StreamWaitingForContainer {
			return m.waitingBody(width, height)
		}
		if state == tui.TaskStateLoading {
			return m.loadingBody(width, height)
		}
		return components.CenterLines([]string{m.feedback}, width, height)
	}

	theme := m.Theme()
	// docs/design README.md §5b: "a full-width red-tinted row for the most
	// significant one" — every ERR line gets red message/level text, but
	// only the latest (highest-index) ERR entry gets the extra tinted
	// background row.
	lastErr := m.lastErrEntry()
	lines := make([]string, 0, height)
	for _, row := range m.visibleWindow(width) {
		entry := m.buffer.Entries[row.EntryIndex]
		lines = append(lines, m.formatVisualRow(theme, entry, row, width, row.EntryIndex == lastErr))
	}
	if m.buffer.DroppedCount > 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(theme.TextFaint).Render(fmt.Sprintf("… %d older log lines dropped …", m.buffer.DroppedCount)))
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, m.statusLine(theme, width))
	return strings.Join(lines, "\n")
}

func (m Model) loadingBody(width, height int) string {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	faint := lipgloss.NewStyle().Foreground(theme.TextGhost)
	final := faint.Render("since " + m.sinceLabel() + " · waiting for log history…")
	return decorativeBars(width, height, dim, faint) + "\n" + final
}

// waitingBody is 5b's pre-connect state — same decorative-bars shape as
// loadingBody, but the final line names the container's own reason
// (docs/design README.md §5b) instead of loadingBody's generic "waiting for
// log history", in the app's standing pending/not-yet-settled color
// (theme.Warn, matching waitingStripLine/Header's ConnBadge for this state).
func (m Model) waitingBody(width, height int) string {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	faint := lipgloss.NewStyle().Foreground(theme.TextGhost)
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	container, _ := m.activeContainer()
	final := warn.Render(tui.GlyphPending + " waiting for container " + container + " to start: " + m.waitingReason)
	return decorativeBars(width, height, dim, faint) + "\n" + final
}

// decorativeBars renders loadingBody/waitingBody's shared shrinking-bar
// filler, everything but the final status line each caller supplies itself.
func decorativeBars(width, height int, first, rest lipgloss.Style) string {
	rows := max(height-2, 1)
	lines := make([]string, 0, rows)
	for i := range rows {
		bar := strings.Repeat("━", max(width/3-(i%3)*2, 8))
		style := rest
		if i == 0 {
			style = first
		}
		lines = append(lines, style.Render(bar))
	}
	return strings.Join(lines, "\n")
}

// visualLogRow is one physical terminal row produced from a logical log
// entry. EntryIndex points into filteredEntries; First distinguishes the row
// that carries timestamp/severity from a continuation row indented beneath
// the message column.
type visualLogRow struct {
	EntryIndex int
	First      bool
	Message    string
}

// appendEntryRows lays out one entry, tagging each physical row with the
// caller's own index for it. Every layout path funnels through here so the
// windowed and whole-buffer passes can't drift apart.
func (m Model) appendEntryRows(dst []visualLogRow, entry LogEntry, index, width int) []visualLogRow {
	if entry.Boundary {
		return append(dst, visualLogRow{EntryIndex: index, First: true})
	}

	message := strings.ToValidUTF8(entry.Message, "\uFFFD")
	messages := []string{message}
	if m.view.Wrap {
		messageWidth := max(width-m.entryPrefixWidth(entry), 1)
		messages = strings.Split(lipgloss.Wrap(message, messageWidth, ""), "\n")
		if len(messages) == 0 {
			messages = []string{""}
		}
	} else if m.view.HorizontalOffset > 0 {
		messages[0] = ansi.Cut(message, m.view.HorizontalOffset, ansi.StringWidth(message))
	}

	for line, message := range messages {
		dst = append(dst, visualLogRow{EntryIndex: index, First: line == 0, Message: message})
	}
	return dst
}

// entryRowCount is appendEntryRows' row count without building the rows —
// what the layout index stores per entry. Counting newlines instead of
// splitting keeps the per-line append path allocation-free.
func (m Model) entryRowCount(entry LogEntry) int {
	if entry.Boundary || !m.view.Wrap {
		return 1
	}
	messageWidth := max(m.view.Width-m.entryPrefixWidth(entry), 1)
	message := strings.ToValidUTF8(entry.Message, "\uFFFD")
	return strings.Count(lipgloss.Wrap(message, messageWidth, ""), "\n") + 1
}

// visualRows performs the width-dependent, pure layout pass over entries,
// indexed relative to the slice it is given. The viewport itself no longer
// calls this — see visibleWindow — but the per-entry helpers and tests do.
func (m Model) visualRows(entries []LogEntry, width int) []visualLogRow {
	rows := make([]visualLogRow, 0, len(entries))
	for i, entry := range entries {
		rows = m.appendEntryRows(rows, entry, i, width)
	}
	return rows
}

// allVisualRows lays out every entry the filter keeps, indexed into
// buffer.Entries. This is the whole-buffer pass the row index exists to
// avoid; it remains as the fallback for a buffer the index hasn't seen
// (layoutValid), so a hand-populated model still renders correctly.
func (m Model) allVisualRows(width int) []visualLogRow {
	rows := make([]visualLogRow, 0, len(m.buffer.Entries))
	for i, entry := range m.buffer.Entries {
		if !m.entryMatchesFilter(entry) {
			continue
		}
		rows = m.appendEntryRows(rows, entry, i, width)
	}
	return rows
}

// visibleWindow is the viewport's own physical rows, indexed into
// buffer.Entries. It lays out only the entries the window actually shows:
// the row index answers where the window starts, so the cost is the visible
// rows rather than the 5,000-entry buffer behind them
// (docs/performance.md). Every caller that used to lay out the whole buffer
// and slice it — Body, the toolbar's severity counts, ctrl-y's copy — goes
// through here.
func (m Model) visibleWindow(width int) []visualLogRow {
	height := m.entryViewportHeight()
	if !m.layoutValid() || width != m.view.Width {
		return clampWindow(m.allVisualRows(width), m.view.VerticalOffset, height)
	}
	index, rowsBefore := m.entryAtRow(m.view.VerticalOffset)
	skip := m.view.VerticalOffset - rowsBefore
	rows := make([]visualLogRow, 0, height+skip)
	for ; index < len(m.buffer.Entries) && len(rows) < height+skip; index++ {
		if m.rowCounts[index] == 0 {
			continue
		}
		rows = m.appendEntryRows(rows, m.buffer.Entries[index], index, width)
	}
	return clampWindow(rows, skip, height)
}

func clampWindow(rows []visualLogRow, start, height int) []visualLogRow {
	start = min(start, len(rows))
	return rows[start:min(start+height, len(rows))]
}

// formatEntry preserves the old single-row helper used by focused tests. The
// viewport itself calls formatVisualRow for every row returned by visualRows.
func (m Model) formatEntry(theme tui.Theme, entry LogEntry, width int, mostSignificantErr bool) string {
	rows := m.visualRows([]LogEntry{entry}, width)
	if len(rows) == 0 {
		return ""
	}
	return m.formatVisualRow(theme, entry, rows[0], width, mostSignificantErr)
}

func (m Model) entryPrefixWidth(entry LogEntry) int {
	parts := make([]string, 0, 2)
	if m.view.Timestamps && entry.Timestamp != "" {
		parts = append(parts, entry.Timestamp)
	}
	if entry.Severity != "" {
		parts = append(parts, entry.Severity)
	}
	if len(parts) == 0 {
		return 0
	}
	return ansi.StringWidth(strings.Join(parts, " ")) + 1
}

// formatVisualRow renders one physical row. mostSignificantErr is true for
// every continuation row belonging to the latest ERR entry, so its semantic
// background remains a full-width band instead of disappearing after the
// first wrapped row.
func (m Model) formatVisualRow(theme tui.Theme, entry LogEntry, row visualLogRow, width int, mostSignificantErr bool) string {
	if entry.Boundary {
		return centeredRule(entry.Message+" · "+entry.Timestamp, width, lipgloss.NewStyle().Foreground(theme.TextFaint))
	}

	dimTS := lipgloss.NewStyle().Foreground(theme.TextGhost)
	msgStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	levelStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	isErr := entry.Severity == SeverityErr
	tinted := isErr && mostSignificantErr
	switch {
	case isErr:
		levelStyle = lipgloss.NewStyle().Foreground(theme.Bad)
		msgStyle = lipgloss.NewStyle().Foreground(theme.BadText)
		if tinted {
			dimTS = dimTS.Background(theme.ErrBannerBg)
			levelStyle = levelStyle.Background(theme.ErrBannerBg)
			msgStyle = msgStyle.Background(theme.ErrBannerBg)
		}
	case entry.Severity == SeverityWarn:
		levelStyle = lipgloss.NewStyle().Foreground(theme.Warn)
	}

	var prefix string
	if row.First {
		parts := []string{}
		if m.view.Timestamps && entry.Timestamp != "" {
			parts = append(parts, dimTS.Render(entry.Timestamp))
		}
		if entry.Severity != "" {
			parts = append(parts, levelStyle.Render(entry.Severity))
		}
		if len(parts) > 0 {
			prefix = strings.Join(parts, " ") + " "
		}
	} else if indent := m.entryPrefixWidth(entry); indent > 0 {
		indentStyle := lipgloss.NewStyle()
		if tinted {
			indentStyle = indentStyle.Background(theme.ErrBannerBg)
		}
		prefix = indentStyle.Render(strings.Repeat(" ", indent))
	}
	line := prefix + msgStyle.Render(row.Message)
	if !tinted {
		return line
	}

	// A full-width tinted row (docs/design README.md §5b's "red-tinted
	// row" for ERR lines) needs the trailing pad itself styled with the
	// background — plain components.Pad spaces would leave an unstyled
	// gap once each span's own ANSI reset lands (the same lesson 3a's
	// palette exit notes document for BgPalette/BgInput fills).
	line = components.Truncate(line, width)
	if fill := width - lipgloss.Width(line); fill > 0 {
		line += lipgloss.NewStyle().Background(theme.ErrBannerBg).Render(strings.Repeat(" ", fill))
	}
	return line
}

// statusLine is 5b's bottom status ("▶ live · 2 new lines/s" / paused /
// reconnecting), computed from state set in Update — never a clock read
// here, keeping Render pure.
func (m Model) statusLine(theme tui.Theme, width int) string {
	good := lipgloss.NewStyle().Foreground(theme.Good)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	var line string
	switch {
	case m.stream == StreamReconnecting:
		line = dim.Render("↺ reconnecting…")
	case !m.view.AutoScroll:
		line = dim.Render("⏸ paused · space to resume following")
	default:
		line = good.Render(fmt.Sprintf("%s live · %d new lines/s", tui.GlyphFollowing, m.lastRate))
	}
	if m.crashLoopReason != "" {
		// First, so it survives truncation at narrow widths: the lines above
		// are the container's last run, not a live process.
		container, _ := m.activeContainer()
		warn := lipgloss.NewStyle().Foreground(theme.Warn)
		line = warn.Render(tui.GlyphPending+" "+container+" is crash-looping ("+m.crashLoopReason+") · showing its last run") +
			dim.Render("  ·  ") + line
	}
	return components.Pad(line, width)
}

// visibleViewText renders the currently-visible lines as plain text for
// ctrl-y's clipboard copy (docs/design README.md §5b).
func (m Model) visibleViewText() string {
	rows := m.visibleWindow(m.view.Width)
	lines := make([]string, 0, len(rows))
	seen := make(map[int]struct{}, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.EntryIndex]; ok {
			continue
		}
		seen[row.EntryIndex] = struct{}{}
		e := m.buffer.Entries[row.EntryIndex]
		if e.Boundary {
			lines = append(lines, "--- "+e.Message+" · "+e.Timestamp+" ---")
			continue
		}
		var parts []string
		if m.view.Timestamps && e.Timestamp != "" {
			parts = append(parts, e.Timestamp)
		}
		if e.Severity != "" {
			parts = append(parts, e.Severity)
		}
		parts = append(parts, e.Message)
		lines = append(lines, strings.Join(parts, " "))
	}
	return strings.Join(lines, "\n")
}

func (m Model) visibleSeverityCounts() (warn, err int) {
	rows := m.visibleWindow(m.view.Width)
	seen := make(map[int]struct{}, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.EntryIndex]; ok {
			continue
		}
		seen[row.EntryIndex] = struct{}{}
		e := m.buffer.Entries[row.EntryIndex]
		switch e.Severity {
		case SeverityWarn:
			warn++
		case SeverityErr:
			err++
		}
	}
	return warn, err
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func centeredRule(text string, width int, style lipgloss.Style) string {
	label := " " + text + " "
	dashes := max(width-lipgloss.Width(label), 0)
	left := dashes / 2
	right := dashes - left
	return style.Render(strings.Repeat("─", left) + label + strings.Repeat("─", right))
}

func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func stripInnerWidth(width int) int {
	return max(width-2*tui.FrameInset, 0)
}

func insetStripLine(line string, width int) string {
	return components.Pad(strings.Repeat(" ", tui.FrameInset)+line, width)
}
