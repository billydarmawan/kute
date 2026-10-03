package tui

import (
	"cmp"
	"fmt"
	"image/color"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/tui/components"
)

// This file renders the 7b help overlay (docs/design README.md §7b): a
// floating ~79%-width panel over the dimmed table, four columns — the
// active screen's own keybar groups (so it always matches what's on
// screen, mvp-plan.md §0.4), plus the fixed SCOPE, LIST, and RESOURCE
// columns — followed by a horizontal MISC footer row for the handful of
// shell-level odds and ends (what's-new/help/back/quit) that don't belong
// in a vertical column of their own.

// helpWidth is the panel's outer width: ~79% of the screen, floored/capped
// like palette.Width.
func helpWidth(screenWidth int) int {
	return min(max(int(float64(screenWidth)*0.79), 40), screenWidth)
}

// flattenHints joins a Keybar's groups into one column, in order.
func flattenHints(groups [][]KeyHint) []KeyHint {
	var out []KeyHint
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// renderHelp draws the 7b panel for the currently active Screen. scope/list/
// resource are Session.HelpScope/HelpList/HelpResource (built at the
// composition root from the verbs registry — see session.go's doc comment
// on those fields); misc is Session.HelpMisc, rendered as a horizontal
// footer row rather than a fifth column. maxHeight caps the panel's outer
// height (the palette's own cap): on a short terminal the columns lose their
// bottom rows to a "+N more" marker so the MISC row and "esc close" stay on
// screen, instead of Compose clipping the panel's bottom edge.
func renderHelp(theme Theme, view Screen, scope, list, resource, misc []KeyHint, screenWidth, maxHeight int) string {
	width := helpWidth(screenWidth)
	frameWidth := max(width-2, 20)

	fg := func(c color.Color) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(c).Background(theme.BgPalette)
	}
	accent := fg(theme.Accent).Bold(true)
	headStyle := fg(theme.AccentHi).Bold(true)
	keyStyle := fg(theme.Accent)
	labelStyle := fg(theme.TextDim)
	faint := fg(theme.TextFaint)
	fill := fg(theme.BgPalette) // background-only, for pads/gaps

	kb := view.Keybar()
	viewTitle := strings.ToUpper(kb.PillText)
	if viewTitle == "" {
		viewTitle = "VIEW"
	}
	viewHints := flattenHints(kb.Groups)

	title := accent.Render("? help") + "  " + faint.Render(fmt.Sprintf("keys for %s view · globals below", viewTitle))
	// TextGhost, not BorderSubtle — matches chrome.go's own band rules and
	// palette's Rule; BorderSubtle reads as too dark without a dialog
	// background fill behind it (BgPalette's the empty lipgloss.Color).
	rule := fg(theme.TextGhost).Render(strings.Repeat("─", frameWidth))

	// colGap shrinks from 7b's old 3-column value (3) to 2: one more column
	// now shares the same panel width, and the extra cell is worth more as
	// column budget than as visual air between four columns instead of
	// three. innerWidth is the row's real budget: helpInset reserves a
	// 1-cell margin on each side, so columns sized against frameWidth itself
	// came out two cells too wide and got truncated with a stray "…" (7b:
	// help.go docs/design README.md §7b).
	const colGap = 2
	const colFloor = 8
	innerWidth := frameWidth - 2

	natural := [4]int{
		helpColumnNaturalWidth(viewTitle+" VIEW", viewHints),
		helpColumnNaturalWidth("SCOPE", scope),
		helpColumnNaturalWidth("LIST", list),
		helpColumnNaturalWidth("RESOURCE", resource),
	}
	widths := helpColumnWidths(natural, innerWidth-3*colGap, colFloor)

	cols := [][]string{
		helpColumn(viewTitle+" VIEW", viewHints, headStyle, keyStyle, labelStyle, fill, widths[0]),
		helpColumn("SCOPE", scope, headStyle, keyStyle, labelStyle, fill, widths[1]),
		helpColumn("LIST", list, headStyle, keyStyle, labelStyle, fill, widths[2]),
		helpColumn("RESOURCE", resource, headStyle, keyStyle, labelStyle, fill, widths[3]),
	}
	rows := 0
	for _, c := range cols {
		rows = max(rows, len(c))
	}
	for i, c := range cols {
		for len(c) < rows {
			c = append(c, helpFill(fill, widths[i]))
		}
		cols[i] = c
	}

	miscLines := helpMiscLines(misc, headStyle, keyStyle, labelStyle, fill, innerWidth)
	// title + rule, rule, MISC, rule, close, and the two border rows.
	if budget := max(maxHeight-7-len(miscLines), 1); rows > budget {
		for i, c := range cols {
			// Every non-blank row from the last kept one down, which the
			// marker replaces — a column whose hints all fit keeps them.
			hidden := 0
			for _, l := range c[budget-1:] {
				if strings.TrimSpace(ansi.Strip(l)) != "" {
					hidden++
				}
			}
			if budget > 1 && hidden > 1 {
				c[budget-1] = helpPad(faint.Render(fmt.Sprintf("+%d more", hidden)), widths[i], fill)
			}
			cols[i] = c[:budget]
		}
		rows = budget
	}

	gap := helpFill(fill, colGap)
	lines := []string{helpInset(title, frameWidth, fill), rule}
	for r := range rows {
		lines = append(lines, helpInset(cols[0][r]+gap+cols[1][r]+gap+cols[2][r]+gap+cols[3][r], frameWidth, fill))
	}
	lines = append(lines, rule)
	for _, l := range miscLines {
		lines = append(lines, helpInset(l, frameWidth, fill))
	}
	lines = append(lines, rule)
	closeHint := keyStyle.Render("esc") + labelStyle.Render(" close")
	closeGap := max(frameWidth-2-lipgloss.Width(closeHint), 0)
	lines = append(lines, helpInset(helpFill(fill, closeGap)+closeHint, frameWidth, fill))

	// +2: lipgloss v2's Width counts the border itself (v1 added it on
	// top), and frameWidth is the pre-border content width the lines
	// above are already wrapped to.
	frame := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.BorderPalette).
		BorderBackground(theme.BgPalette).
		Background(theme.BgPalette).
		Width(frameWidth + 2)
	return frame.Render(strings.Join(lines, "\n"))
}

// helpColumnGutter is the display width of the widest key in hints — the
// left-aligned column every row's label starts after, within this column
// only. Computed independently per column, so a wide key in one column
// (e.g. LIST's "↑↓ jk") never shifts labels in another (e.g. SCOPE's "g").
func helpColumnGutter(hints []KeyHint) int {
	g := 0
	for _, h := range hints {
		g = max(g, lipgloss.Width(h.Key))
	}
	return g
}

// helpColumnNaturalWidth is the column's ideal, untruncated width: the
// uppercase heading vs. every "gutter + 1 space + label" row, whichever is
// widest.
func helpColumnNaturalWidth(heading string, hints []KeyHint) int {
	w := lipgloss.Width(strings.ToUpper(heading))
	gutter := helpColumnGutter(hints)
	for _, h := range hints {
		w = max(w, gutter+1+lipgloss.Width(h.Label))
	}
	return w
}

// helpColumnWidths distributes available cells across the four main columns.
// When every column's natural width already fits, it's returned unchanged —
// helpInset's own trailing fill absorbs any width left over. Otherwise it
// water-fills: every column is raised toward one common cap, so a column
// narrower than the cap keeps its full natural width and the wider ones
// share what's left evenly, each truncating a little. (Topping up the
// narrow columns in full first left the widest — LIST at 80 columns — at
// floor, ellipsizing every one of its labels to a bare "…".)
func helpColumnWidths(natural [4]int, available, floor int) [4]int {
	total := natural[0] + natural[1] + natural[2] + natural[3]
	if total <= available {
		return natural
	}
	widths := [4]int{floor, floor, floor, floor}
	if available <= floor*4 {
		return widths // pathologically narrow terminal; Truncate saves every cell
	}
	order := []int{0, 1, 2, 3}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(natural[a], natural[b]) })
	budget := available
	for k, i := range order {
		share := budget / (len(order) - k)
		widths[i] = max(min(natural[i], share), floor)
		budget -= widths[i]
	}
	return widths
}

// helpColumn renders one 7b column: an uppercase heading followed by up to
// one row per hint, each padded to width so every column in a row lines up.
// Keys are left-padded to the column's own gutter so labels start at a
// consistent offset regardless of key width (g vs ↑↓ jk vs ctrl+q).
func helpColumn(heading string, hints []KeyHint, headStyle, keyStyle, labelStyle, fill lipgloss.Style, width int) []string {
	gutter := helpColumnGutter(hints)
	lines := []string{helpPad(headStyle.Render(strings.ToUpper(heading)), width, fill)}
	for _, h := range hints {
		style := keyStyle
		if h.Disabled {
			style = labelStyle
		}
		key := h.Key + strings.Repeat(" ", gutter-lipgloss.Width(h.Key))
		line := style.Render(key) + labelStyle.Render(" "+h.Label)
		lines = append(lines, helpPad(line, width, fill))
	}
	return lines
}

// helpMiscLines renders the MISC footer row: "MISC" in the same heading
// style as the four main columns, followed by every hint's key+label pair
// on one line — joined the same way the app's own on-screen keybar joins
// hint pairs (chrome.go's renderHints). When that doesn't fit width, it
// wraps onto a second line (heading alone, then the joined items) rather
// than shrinking the four columns above.
func helpMiscLines(hints []KeyHint, headStyle, keyStyle, labelStyle, fill lipgloss.Style, width int) []string {
	head := headStyle.Render("MISC")
	items := make([]string, 0, len(hints))
	for _, h := range hints {
		items = append(items, keyStyle.Render(h.Key)+labelStyle.Render(" "+h.Label))
	}
	joined := strings.Join(items, "  ")
	oneLine := head + strings.Repeat(" ", 3) + joined
	if lipgloss.Width(oneLine) <= width {
		return []string{helpPad(oneLine, width, fill)}
	}
	return []string{helpPad(head, width, fill), helpPad(joined, width, fill)}
}

func helpFill(fill lipgloss.Style, n int) string {
	if n <= 0 {
		return ""
	}
	return fill.Render(strings.Repeat(" ", n))
}

// helpPad truncates/pads styled content to exactly width cells, padding
// through fill so it carries the panel background (an outer wrap can't fix
// a bare space's missing background — each span's ANSI reset cancels it).
func helpPad(content string, width int, fill lipgloss.Style) string {
	content = components.Truncate(content, width)
	return content + helpFill(fill, width-lipgloss.Width(content))
}

// helpInset adds the panel's 1-cell side margins to a row already padded to
// width-2.
func helpInset(content string, width int, fill lipgloss.Style) string {
	return helpFill(fill, 1) + helpPad(content, width-2, fill) + helpFill(fill, 1)
}
