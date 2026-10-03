package events

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// Column layout (docs/design README.md §9b's grid: `20px 148px minmax(0,1fr)
// 40px 70px`, translated to a monospace character grid at the mockup's own
// 960px-card-≈120-column ratio): glyph · REASON·OBJECT (fixed) · MESSAGE
// (flex, the widest column — "the message is why you're here") · × count
// (fixed) · LAST (fixed, right-aligned). evColGap is the 12px inter-column
// gap, matching components.Table's own 2-cell convention.
const (
	evGlyphW     = 2
	evReasonObjW = 20
	evCountW     = 4
	evLastW      = 6
	evColGap     = 2
)

// eventColumnWidths derives the flex MESSAGE column width from the terminal
// width and the other columns' fixed widths — one place both the header row
// and every data row read, so they can never drift out of alignment.
func eventColumnWidths(width int) (reasonObjW, msgW, countW, lastW int) {
	fixed := evGlyphW + evReasonObjW + evCountW + evLastW + 4*evColGap
	return evReasonObjW, max(width-fixed, 10), evCountW, evLastW
}

// padLeft right-aligns value within width, the mirror of components.Pad's
// left-align — used for the LAST column, the one right-aligned field in
// 9b's grid.
func padLeft(value string, width int) string {
	value = components.Truncate(value, width)
	slack := width - lipgloss.Width(value)
	if slack <= 0 {
		return value
	}
	return strings.Repeat(" ", slack) + value
}

// columnHeaderLines renders 9b's "" · REASON · OBJECT · MESSAGE · × · LAST
// column-title row plus its rule, both new in this pass (previously this
// screen had no header row at all).
func (m Model) columnHeaderLines(theme tui.Theme, width int) []string {
	reasonObjW, msgW, countW, lastW := eventColumnWidths(width)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	gap := strings.Repeat(" ", evColGap)

	row := strings.Repeat(" ", evGlyphW) + gap +
		faint.Render(components.Pad("REASON · OBJECT", reasonObjW)) + gap +
		faint.Render(components.Pad("MESSAGE", msgW)) + gap +
		faint.Render(components.Pad("×", countW)) + gap +
		faint.Render(padLeft("LAST", lastW))
	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", width))
	return []string{fillLine(row, width, false, theme), rule}
}

func (m Model) View() tea.View { return tea.NewView(m.Render()) }

func (m Model) Render() string { return tui.Frame(m.width, m.height, m) }

func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

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
	if m.objectKind != "" {
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: string(m.objectKind) + "/" + m.objectName, Style: dim},
		)
	} else {
		nsText := m.namespace
		if nsText == "" {
			nsText = tui.GlyphAllNS + " all namespaces"
		}
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: nsText, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
		)
	}
	crumbs = append(crumbs, tui.Crumb{Text: " › ", Style: ghost}, tui.Crumb{Text: "Events", Style: text})

	return tui.HeaderState{
		Crumbs:      crumbs,
		ConfigChip:  tui.BuildConfigChip(theme, m.session),
		UpdateChip:  tui.BuildUpdateChip(theme, m.session),
		ForwardChip: tui.BuildForwardChip(theme, m.session.ForwardSummary()),
		Conn:        tui.LiveConnBadge(theme, m.conn, tui.GlyphRunning+" connected"),
	}
}

func (m Model) Strips(width int) []string {
	if m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty {
		return nil
	}
	lines := []string{m.summaryLine(m.Theme(), width)}
	if m.filterActive {
		lines = append(lines, m.filterStripLine(m.Theme(), width))
	}
	return lines
}

// counts tallies m.rows (already window/filter/fold-applied by
// recomputeVisible) into warning/normal group totals for the summary strip
// — reading the same rows Body walks, so the strip and the list can never
// disagree about what's currently shown.
func (m Model) counts() (warnings, normal int) {
	for _, r := range m.rows {
		switch r.kind {
		case rowFolded:
			normal += len(r.folded)
		case rowGroup:
			if r.group.Type == "Warning" {
				warnings++
			} else {
				normal++
			}
		}
	}
	return warnings, normal
}

// summaryLine is 9b's "▲ 4 warnings · ○ 31 normal    last hour · deduped ·
// warnings first" (docs/design README.md §9b).
func (m Model) summaryLine(theme tui.Theme, width int) string {
	warnStyle := lipgloss.NewStyle().Foreground(theme.Warn)
	dim := lipgloss.NewStyle().Foreground(theme.TextFaint)
	num := lipgloss.NewStyle().Foreground(theme.TextPrimary)

	warnings, normal := m.counts()
	left := warnStyle.Render(tui.GlyphWarning) + " " + num.Render(strconv.Itoa(warnings)) + " " + dim.Render("warnings")
	if !m.warningsOnly {
		left += "   " + dim.Render(tui.GlyphCompleted) + " " + num.Render(strconv.Itoa(normal)) + " " + dim.Render("normal")
	}
	right := dim.Render(windowLabel(m.window) + " · deduped · warnings first")
	return fillLine(padBetween(left, right, width, false, theme), width, false, theme)
}

// filterStripLine mirrors browse's own filter strip (the live "/" query and
// matched/total, plus the "N hidden by filter — esc to clear" notice once
// the query itself hides groups — docs/design system-wide interactions:
// "items never silently disappear"), substring rather than fuzzy — events
// are free-text messages, not short row names, the same reasoning podlogs'
// live filter uses (mvp-tasks.md's Phase 6 exit notes). The denominator is
// filterBaselineGroups (window/warningsOnly narrowing), not the raw group
// count, so those separate toggles never get misreported as the query's
// own doing.
func (m Model) filterStripLine(theme tui.Theme, width int) string {
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	total, matched := m.filterBaselineGroups, m.filterMatchedGroups
	right := dim.Render(fmt.Sprintf("%d matched", matched))
	if matched < total {
		right = faint.Render(fmt.Sprintf("%d hidden by filter — esc to clear   ", total-matched)) + right
	}
	inputWidth := max(width-2-lipgloss.Width(right)-1, 1)
	left := accent.Render("/ ") + m.filterInput.ViewWidth(inputWidth)
	return fillLine(padBetween(left, right, width, false, theme), width, false, theme)
}

func windowLabel(d time.Duration) string {
	switch d {
	case 15 * time.Minute:
		return "last 15m"
	case time.Hour:
		return "last hour"
	case 6 * time.Hour:
		return "last 6h"
	case 24 * time.Hour:
		return "last 24h"
	default:
		return "all time"
	}
}

func (m Model) Body(width, height int) string {
	switch m.state {
	case tui.TaskStateEmpty:
		return components.CenterLines([]string{m.emptyMessage()}, width, height)
	case tui.TaskStateReady:
		return m.eventRows(m.Theme(), width, height)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

func (m Model) emptyMessage() string {
	if m.objectKind != "" {
		return "no events for " + string(m.objectKind) + "/" + m.objectName
	}
	if m.namespace == "" {
		return "no events cluster-wide"
	}
	return "no events in " + m.namespace
}

// eventRows renders 9b's column header + rule followed by the currently
// visible window of m.rows, growing around m.selected until the remaining
// height is filled — computed fresh every render from Model state alone (no
// persisted scroll offset), which keeps Body a pure function of (m, width,
// height).
func (m Model) eventRows(theme tui.Theme, width, height int) string {
	header := m.columnHeaderLines(theme, width)
	if len(m.rows) == 0 {
		return strings.Join(header, "\n") + "\n" + components.CenterLines([]string{m.emptyMessage()}, width, max(height-len(header), 0))
	}
	rowsHeight := max(height-len(header), 0)
	start, end := visibleWindow(m.rows, m.selected, rowsHeight, width)
	lines := make([]string, 0, len(header)+end-start)
	lines = append(lines, header...)
	for i := start; i < end; i++ {
		lines = append(lines, m.renderRow(theme, m.rows[i], i == m.selected, width))
	}
	return strings.Join(lines, "\n")
}

// rowLines is how many terminal lines r occupies at width: a group row is 2
// lines at minimum (the REASON line and the OBJECT line under it, docs/design
// README.md §9b's "two-line REASON·OBJECT cell") plus one more per MESSAGE
// line beyond the first 2 — the first 2 wrapped message lines share the
// REASON and OBJECT lines respectively, so only wraps past that grow the
// row (docs/design README.md §9b's "MESSAGE (widest, verbatim)" — wrapped
// rather than truncated, so a long message just grows the row instead of
// losing text); 1 for the folded normal-events summary.
func rowLines(r displayRow, width int) int {
	if r.kind == rowFolded {
		return 1
	}
	_, msgW, _, _ := eventColumnWidths(width)
	n := len(wrapMessage(r.group.Message, msgW))
	return 2 + max(n-2, 0)
}

// visibleWindow finds a [start,end) row range around selected that fits
// within height lines (rowLines-aware, since rows have mixed heights),
// growing downward first then upward while budget remains.
func visibleWindow(rows []displayRow, selected, height, width int) (start, end int) {
	n := len(rows)
	if n == 0 || height <= 0 {
		return 0, 0
	}
	selected = clamp(selected, 0, n-1)
	start, end = selected, selected+1
	used := rowLines(rows[selected], width)
	for {
		grew := false
		if end < n && used+rowLines(rows[end], width) <= height {
			used += rowLines(rows[end], width)
			end++
			grew = true
		}
		if start > 0 && used+rowLines(rows[start-1], width) <= height {
			used += rowLines(rows[start-1], width)
			start--
			grew = true
		}
		if !grew {
			break
		}
	}
	return start, end
}

func (m Model) renderRow(theme tui.Theme, r displayRow, selected bool, width int) string {
	if r.kind == rowFolded {
		return m.renderFoldedRow(theme, r.folded, selected, width)
	}
	return m.renderGroupRow(theme, r.group, selected, width)
}

// severityStyle is 9b's red-vs-yellow-vs-blue rule (docs/design README.md
// §9b): Warning groups render red when the involved object is a currently
// StatusFail pod — a Pod, not any same-named object (m.failing, load.go's best-effort cross-check keyed by
// "namespace/name" so all-namespaces mode can't confuse two same-named pods
// in different namespaces), yellow otherwise; Normal groups are always the
// neutral Info color.
func (m Model) severityStyle(theme tui.Theme, g kube.EventGroup) lipgloss.Style {
	if g.Type != "Warning" {
		return lipgloss.NewStyle().Foreground(theme.Info)
	}
	// m.failing is keyed by pod name alone, so a Warning on Service/redis
	// must not turn red because Pod/redis is crashlooping.
	if strings.HasPrefix(g.Object, "Pod/") && m.failing[g.Namespace+"/"+objectName(g.Object)] {
		return lipgloss.NewStyle().Foreground(theme.Bad)
	}
	return lipgloss.NewStyle().Foreground(theme.Warn)
}

// objectDisplay is the OBJECT line's text: g.Object ("Kind/Name") as-is when
// the screen is scoped to one namespace, prefixed with the group's own
// namespace when this is 9b's all-namespaces mode (m.namespace == "",
// mirroring 6b) — otherwise two different namespaces' identically-named
// objects (e.g. "Pod/cache-0" in both shop-checkout and shop-payments)
// would render as indistinguishable rows.
func (m Model) objectDisplay(g kube.EventGroup) string {
	if m.namespace != "" || m.objectKind != "" {
		return g.Object
	}
	return g.Namespace + "/" + g.Object
}

func objectName(object string) string {
	_, name, ok := strings.Cut(object, "/")
	if !ok {
		return object
	}
	return name
}

// renderGroupRow draws one 9b row on the same 5-column grid as
// columnHeaderLines (docs/design README.md §9b): glyph · REASON (line 1) /
// OBJECT (line 2, dim, under REASON) · MESSAGE (its own flex column,
// word-wrapped in place rather than truncated so long messages just grow
// the row) · × count · LAST — count/last only ever render on line 1, the
// mockup's own layout (README.md's `Kute Spec.dc.html#9b`).
func (m Model) renderGroupRow(theme tui.Theme, g kube.EventGroup, selected bool, width int) string {
	reasonObjW, msgW, countW, lastW := eventColumnWidths(width)

	sev := m.severityStyle(theme, g)
	objStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	msgStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	if selected {
		msgStyle = lipgloss.NewStyle().Foreground(theme.TextPrimary)
	}
	countStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	lastStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	if selected {
		sev = sev.Background(theme.SelBg)
		objStyle = objStyle.Background(theme.SelBg)
		msgStyle = msgStyle.Background(theme.SelBg)
		countStyle = countStyle.Background(theme.SelBg)
		lastStyle = lastStyle.Background(theme.SelBg)
	}

	glyph := tui.GlyphWarning
	if g.Type != "Warning" {
		glyph = tui.GlyphCompleted
	}
	countText := ""
	if g.Count > 1 {
		countText = strconv.Itoa(int(g.Count))
	}

	msgLines := wrapMessage(g.Message, msgW)
	gap := selSpace(evColGap, selected, theme)
	blankGlyph := selSpace(evGlyphW, selected, theme)
	blankReasonObj := selSpace(reasonObjW, selected, theme)
	blankCount := selSpace(countW, selected, theme)
	blankLast := selSpace(lastW, selected, theme)

	msgLine := func(i int) string {
		if i < len(msgLines) {
			return msgStyle.Render(components.Pad(msgLines[i], msgW))
		}
		return selSpace(msgW, selected, theme)
	}

	glyphCell := sev.Render(components.Pad(glyph, evGlyphW))
	line1 := glyphCell + gap +
		sev.Render(components.Pad(g.Reason, reasonObjW)) + gap +
		msgLine(0) + gap +
		countStyle.Render(components.Pad(countText, countW)) + gap +
		lastStyle.Render(padLeft(shortEventAge(g.LastSeen, m.fetchedAt), lastW))
	line2 := blankGlyph + gap +
		objStyle.Render(components.Pad(components.Truncate(m.objectDisplay(g), reasonObjW), reasonObjW)) + gap +
		msgLine(1) + gap + blankCount + gap + blankLast

	lines := make([]string, 0, 2+max(len(msgLines)-2, 0))
	lines = append(lines, fillLine(line1, width, selected, theme), fillLine(line2, width, selected, theme))
	for i := 2; i < len(msgLines); i++ {
		l := blankGlyph + gap + blankReasonObj + gap + msgLine(i) + gap + blankCount + gap + blankLast
		lines = append(lines, fillLine(l, width, selected, theme))
	}
	return strings.Join(lines, "\n")
}

// wrapMessage word-wraps a message to fit msgW — the MESSAGE column's own
// width (eventColumnWidths), rather than truncating it away (docs/design
// README.md §9b's "MESSAGE (widest, verbatim)"). rowLines calls this too, so
// the row-height math visibleWindow relies on never disagrees with what
// renderGroupRow actually draws.
func wrapMessage(message string, msgW int) []string {
	wrapped := lipgloss.NewStyle().Width(max(msgW, 1)).Render(message)
	return strings.Split(wrapped, "\n")
}

// renderFoldedRow is 9b's "▸ normal · 31 events — Pulled · Created… ↹
// expand" collapsed summary line for every folded normal group (docs/design
// README.md §9b), naming up to 4 distinct reasons. Not on the 5-column grid
// (the mockup's own fold row is a plain full-width flex line, chrome-toned
// unless selected).
func (m Model) renderFoldedRow(theme tui.Theme, groups []kube.EventGroup, selected bool, width int) string {
	bg := theme.BgChrome
	if selected {
		bg = theme.SelBg
	}
	space := lipgloss.NewStyle().Background(bg)
	glyphStyle := lipgloss.NewStyle().Foreground(theme.TextFaint).Background(bg)
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary).Background(bg)
	reasonStyle := lipgloss.NewStyle().Foreground(theme.TextFaint).Background(bg)
	hintStyle := lipgloss.NewStyle().Foreground(theme.TextDim).Background(bg)

	seen := map[string]bool{}
	names := make([]string, 0, 4)
	for _, g := range groups {
		if seen[g.Reason] {
			continue
		}
		seen[g.Reason] = true
		names = append(names, g.Reason)
		if len(names) == 4 {
			break
		}
	}

	left := space.Render("  ") + glyphStyle.Render(tui.GlyphExpand) + space.Render(" ") +
		labelStyle.Render("normal") + space.Render(" ") +
		reasonStyle.Render(fmt.Sprintf("%d events — %s", len(groups), strings.Join(names, " · ")))
	right := hintStyle.Render(tui.GlyphTab + " expand")

	line := left
	if gap := width - lipgloss.Width(left) - lipgloss.Width(right); gap >= 1 {
		line = left + space.Render(strings.Repeat(" ", gap)) + right
	}
	line = components.Truncate(line, width)
	slack := max(width-lipgloss.Width(line), 0)
	return line + space.Render(strings.Repeat(" ", slack))
}

func shortEventAge(t, now time.Time) string {
	if t.IsZero() || now.IsZero() {
		return "–"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
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

// padBetween places left-aligned left and right-aligned right within width
// (measuring already-styled/ANSI content), matching browse's own strip
// layout helper. Drops right when there isn't room for both. The gap itself
// is rendered through selSpace so a selected row's highlight doesn't break
// into disjoint patches around the plain-space fill (the bug docs/design
// README.md §9b's golden fixtures caught: an unstyled gap of literal spaces
// between the reason and the age column left a hole in the highlight).
func padBetween(left, right string, width int, selected bool, theme tui.Theme) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + selSpace(gap, selected, theme) + right
}

// selSpace renders n literal spaces, styled with the selection background
// when selected — used everywhere a row concatenates plain-string indent/gap
// spaces next to styled text, so a selected row's highlight is one solid bar
// rather than patches around each Render() call (unstyled string
// concatenation leaves the terminal's default background showing through).
func selSpace(n int, selected bool, theme tui.Theme) string {
	if n <= 0 {
		return ""
	}
	if !selected {
		return strings.Repeat(" ", n)
	}
	return lipgloss.NewStyle().Background(theme.SelBg).Render(strings.Repeat(" ", n))
}

// fillLine pads content out to width with trailing spaces styled through
// the selection background when selected, so a selected row's highlight
// reaches the row's right edge instead of stopping at the last glyph.
func fillLine(content string, width int, selected bool, theme tui.Theme) string {
	slack := max(width-lipgloss.Width(content), 0)
	return content + selSpace(slack, selected, theme)
}
