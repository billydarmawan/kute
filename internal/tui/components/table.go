package components

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Align controls a column's text alignment (right-align for AGE etc.).
type Align int

const (
	AlignLeft Align = iota
	AlignRight
)

// Column describes one table column. Min is the minimum/fixed width; Flex
// columns share any width left over after every Min (fixed and flex) and
// the inter-column gaps are subtracted. Sort names the sort key this column
// controls — when it matches Table.SortKey, the header renders a "↑"/"↓"
// indicator.
type Column struct {
	Title string
	Min   int
	Flex  bool
	Align Align
	Sort  string
	// TruncateFront ellipsizes this column's data cells from the front
	// instead of the back, so the tail survives — image references, where
	// the tag at the end is the version signal (§10a). Header titles keep
	// back-truncation.
	TruncateFront bool
}

// Cell is one pre-styled table cell.
type Cell struct {
	Text  string
	Style lipgloss.Style
}

// Row is one table row. A non-empty GroupHeader renders as a full-width
// group line instead of Cells (mockup 6b's namespace/health group rows) —
// GroupStyle styles that line, and (like Cell.Style) must already bake in
// the selection background when this row is Table.Selected, since a
// GroupHeader row can be the selected one too (6b's fold/collapsed-summary
// lines are navigable stops when their rows are folded away). GroupHeader
// rows are not backstopped by fillCellBackground (see renderGroupRowV2) —
// browse, their only caller, already bakes the background in itself.
//
// RowStyle backs a non-selected row's own full-width background tint (20a's
// marked-row MarkBg) — the zero value renders no background, unchanged from
// before this field existed. Per-cell Style only covers each cell's own
// text, not the leading marker slot or the gaps between/after cells
// (renderRowV2's pad()), so a row-level tint needs this separate hook the
// same way Table.SelRowStyle already covers those spans for the selected
// row. Selected always overrides RowStyle (SelRowStyle wins), so callers can
// set both without conflict. A plain (single-Style, non-ANSI-text) Cell
// automatically has this background backstopped by fillCellBackground if
// the caller didn't already bake it in; a composite/pre-rendered cell
// (bars, multi-segment glyphs, match highlights) must still bake it in
// itself, since fillCellBackground can't reach inside already-rendered ANSI
// spans without truncating at their internal resets.
type Row struct {
	Cells       []Cell
	GroupHeader string
	GroupStyle  lipgloss.Style
	RowStyle    lipgloss.Style
	// GroupRight holds 6b's right-aligned trouble chips (e.g. "▲2 ✕1"),
	// each its own toned Cell — unlike GroupHeader/GroupStyle's single plain
	// run, these render as independently-styled segments (mirroring how
	// Cells does for data rows) so a crashloop chip can render red while a
	// pending chip renders yellow on the same line. Empty for group rows
	// with nothing to flag (the fold/collapsed-summary lines, and a
	// manually-expanded fully-healthy group).
	GroupRight []Cell
}

// Table renders the inverted-layout resource table.
type Table struct {
	Columns []Column
	// Rows is every row of the list, or — when RowsTotal is set — only the
	// window [RowsStart, RowsStart+len(Rows)) of a RowsTotal-row list.
	Rows []Row
	// RowsStart/RowsTotal let a caller with a long list build (and style)
	// only the rows the viewport shows: set RowsTotal to the full row count,
	// ask VisibleRange for the window, and hand over just those rows with
	// RowsStart = the window's Start. Selected/Offset stay absolute indexes
	// into the full list, and the footer counts RowsTotal, so the output is
	// identical to passing every row — the per-frame cost is not (docs/review
	// 03 H5: 44.5 ms a frame at 5,000 pods when every row was built). Zero
	// RowsTotal means Rows is the whole list, as before these fields existed.
	RowsStart int
	RowsTotal int
	Selected  int
	Offset    int
	Width     int
	Height    int
	SortKey   string
	SortAsc   bool

	// HeaderStyle styles the uppercase header row.
	HeaderStyle lipgloss.Style
	// SortStyle styles the sorted column's "↑"/"↓" indicator (accent in the
	// mockups, brighter than the HeaderStyle titles around it).
	SortStyle lipgloss.Style
	// SelBarStyle styles the "▎" selection-bar glyph. Must declare the same
	// background as SelRowStyle (both are independently self-contained ANSI
	// spans) so the row's background reads as one continuous fill.
	SelBarStyle lipgloss.Style
	// SelRowStyle styles the rest of the selected row (background +
	// foreground), replacing per-cell Style for that row.
	SelRowStyle lipgloss.Style
	// FooterStyle styles the FooterLine range text and scrollbar.
	FooterStyle lipgloss.Style

	// ShowHeaderRule draws a full-width horizontal rule between the column
	// header row and the first data row, styled through RuleStyle — the
	// zero value renders no rule, unchanged from before this field existed.
	ShowHeaderRule bool
	// RuleStyle styles the ShowHeaderRule divider.
	RuleStyle lipgloss.Style
}

const tableSelBar = "▎"

// tableRightMargin mirrors the 2-cell "▎ "/"  " marker slot reserved on the
// left of every row, so grid rows read as evenly inset on both sides instead
// of the marker column being the only margin.
const tableRightMargin = 2

func (t Table) Render() string {
	width := t.Width
	if width <= 0 {
		width = 80
	}
	if len(t.Columns) == 0 {
		return Pad("No columns", width)
	}

	widths := t.columnWidths(width)
	lines := make([]string, 0, len(t.Rows)+2)
	lines = append(lines, t.renderHeaderV2(widths, width))
	if t.ShowHeaderRule {
		lines = append(lines, t.renderHeaderRuleV2(width))
	}

	for _, idx := range t.visibleRowsV2() {
		local := idx - t.RowsStart
		if local < 0 || local >= len(t.Rows) {
			// A windowed caller handed over a window that doesn't cover
			// the viewport; draw nothing rather than index out of range —
			// a panic in a render path takes the whole program down.
			continue
		}
		row := t.Rows[local]
		if row.GroupHeader != "" {
			lines = append(lines, t.renderGroupRowV2(row, idx == t.Selected, width))
			continue
		}
		lines = append(lines, t.renderRowV2(row, idx == t.Selected, widths, width))
	}
	if t.totalRows() == 0 {
		lines = append(lines, Pad("  No resources found", width-tableRightMargin)+strings.Repeat(" ", tableRightMargin))
	}
	return strings.Join(lines, "\n")
}

// ColumnWidths reports the on-screen width each column will be rendered at,
// for callers that need to know whether a cell's text will fit — a screen
// that shows an overflowing value somewhere else has to ask rather than
// re-derive the flex arithmetic, or the two drift and it starts offering to
// expand text that was never truncated.
func (t Table) ColumnWidths(width int) []int {
	if width <= 0 {
		width = 80
	}
	return t.columnWidths(width)
}

// columnWidths distributes width across columns: fixed/Min columns get
// exactly Min, flex columns split whatever remains (extra cells going to
// the earliest flex columns).
func (t Table) columnWidths(width int) []int {
	n := len(t.Columns)
	widths := make([]int, n)

	avail := width - 2 - tableRightMargin // leading marker column + trailing margin
	if n > 1 {
		avail -= 2 * (n - 1) // inter-column gaps
	}
	if avail < 0 {
		avail = 0
	}

	totalMin, flexCount := 0, 0
	for _, c := range t.Columns {
		totalMin += max(c.Min, 0)
		if c.Flex {
			flexCount++
		}
	}
	// remaining is space left over after every column (fixed and flex) has
	// claimed its Min; flex columns then split it on top of their Min.
	remaining := avail - totalMin
	if remaining < 0 {
		remaining = 0
	}

	flexEach, extra := 0, 0
	if flexCount > 0 {
		flexEach, extra = remaining/flexCount, remaining%flexCount
	}
	flexSeen := 0
	for i, c := range t.Columns {
		if !c.Flex {
			widths[i] = c.Min
			continue
		}
		w := c.Min + flexEach
		if flexSeen < extra {
			w++
		}
		flexSeen++
		widths[i] = w
	}
	return widths
}

// HeaderLine renders just the column-header row (no rows), for screens that
// keep the header visible with an empty body (10c) instead of routing it
// through Render.
func (t Table) HeaderLine(width int) string {
	if width <= 0 {
		width = 80
	}
	if len(t.Columns) == 0 {
		return Pad("No columns", width)
	}
	return t.renderHeaderV2(t.columnWidths(width), width)
}

// renderHeaderV2 draws the column-title row. The sorted column's arrow is
// its own SortStyle span (accent in the mockups), so titles and indicator
// are styled independently and padding is measured on the styled result.
func (t Table) renderHeaderV2(widths []int, width int) string {
	parts := make([]string, 0, len(t.Columns))
	for i, c := range t.Columns {
		w := widths[i]
		title := Truncate(strings.ToUpper(c.Title), w)
		styled := t.HeaderStyle.Render(title)
		if c.Sort != "" && c.Sort == t.SortKey {
			arrow := " ↑"
			if !t.SortAsc {
				arrow = " ↓"
			}
			if ansi.StringWidth(title)+2 <= w {
				styled += t.SortStyle.Render(arrow)
			}
		}
		gap := max(w-ansi.StringWidth(styled), 0)
		if c.Align == AlignRight {
			parts = append(parts, strings.Repeat(" ", gap)+styled)
		} else {
			parts = append(parts, styled+strings.Repeat(" ", gap))
		}
	}
	line := "  " + strings.Join(parts, "  ")
	return Pad(line, width)
}

// renderRowV2 draws one data row. The selected row keeps per-cell styling
// (mockup 2a: only the name brightens — RDY/STATUS/… keep their colors on
// the selection background): the table renders each cell's padding and the
// inter-column gaps through SelRowStyle/RowStyle so the background reads as
// one continuous fill, and backstops each plain (single-Style) cell's own
// background the same way via fillCellBackground — a composite/pre-rendered
// cell (bars, multi-segment glyphs, match highlights) still needs the
// caller to bake the background into its own embedded spans, since
// fillCellBackground can't reach inside already-rendered ANSI text without
// truncating at its internal resets.
func (t Table) renderRowV2(row Row, selected bool, widths []int, width int) string {
	gapStyle := row.RowStyle
	if selected {
		gapStyle = t.SelRowStyle
	}
	rowBg := rowBackgroundColor(row, t.SelRowStyle, selected)
	pad := func(n int) string {
		if n <= 0 {
			return ""
		}
		return gapStyle.Render(strings.Repeat(" ", n))
	}

	cells := make([]string, len(t.Columns))
	for i, col := range t.Columns {
		text := ""
		var style lipgloss.Style
		if i < len(row.Cells) {
			text, style = row.Cells[i].Text, row.Cells[i].Style
		}
		style = fillCellBackground(style, text, rowBg)
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		truncate := Truncate
		if col.TruncateFront {
			truncate = TruncateFront
		}
		content := style.Render(truncate(text, w))
		slack := max(w-ansi.StringWidth(content), 0)
		if col.Align == AlignRight {
			cells[i] = pad(slack) + content
		} else {
			cells[i] = content + pad(slack)
		}
	}
	body := strings.Join(cells, pad(2))

	// Two leading cells — the same "▎ "/"  " marker slot the header budget
	// (columnWidths subtracts 2) accounts for, so all rows align. Rendered
	// through gapStyle/SelBarStyle (never a bare literal) and the trailing
	// slack through gapStyle too, so a marked row's RowStyle tint (or the
	// selected row's SelRowStyle) reaches the full row width — unstyled
	// padding would otherwise punch a no-background gap at the row's left
	// and right edges.
	prefix := gapStyle.Render("  ")
	if selected {
		prefix = t.SelBarStyle.Render(tableSelBar) + pad(1)
	}
	line := prefix + body
	return line + pad(width-ansi.StringWidth(line))
}

// rowBackgroundColor returns the background that should backstop every
// plain (single-Style) cell in row: SelRowStyle's when selected (selection
// always wins over a marked-row tint), else row.RowStyle's own tint, or nil
// when neither carries a real background. GetBackground() reports the
// zero-value lipgloss.NoColor{} (not nil) when nothing was ever set, so a
// type-check is enough to detect "no background in play" — the fast path
// every ordinary unselected/untinted row takes.
func rowBackgroundColor(row Row, selRowStyle lipgloss.Style, selected bool) color.Color {
	style := row.RowStyle
	if selected {
		style = selRowStyle
	}
	bg := style.GetBackground()
	if _, ok := bg.(lipgloss.NoColor); ok {
		return nil
	}
	return bg
}

// fillCellBackground backstops a data cell's background from rowBg when the
// cell didn't already bake one in itself (the class of bug where a plain
// Foreground-only cell style on a selected/marked row rendered a hollow
// highlight — background only in the gaps, not under the text). It is a
// byte-identical no-op when: rowBg is nil (no selection/tint in play); the
// cell already set its own Background (an already-correct caller, or a cell
// with a genuinely independent background); or text already carries
// embedded ANSI (a composite pre-rendered cell — MiniBar bars, multi-segment
// health glyphs, fuzzy-match spans — which must stay fully caller-owned,
// since each embedded sub-span's own SGR reset would truncate an outer
// background partway through the text).
func fillCellBackground(style lipgloss.Style, text string, rowBg color.Color) lipgloss.Style {
	if rowBg == nil {
		return style
	}
	if _, ok := style.GetBackground().(lipgloss.NoColor); !ok {
		return style
	}
	if ansi.Strip(text) != text {
		return style
	}
	return style.Background(rowBg)
}

// renderGroupRowV2 draws one Row.GroupHeader line. Unselected, it's just
// row.GroupStyle wrapped around the padded text (the "  " two-space marker
// baked into the text itself, same convention the data-row marker column
// reserves). Selected mirrors renderRowV2's split: the bar glyph always
// takes SelBarStyle (independent of the row's own color — green/dim/purple),
// the rest renders through row.GroupStyle, which the caller must have
// already baked the selection background into (mirroring how Cell.Style
// does for selected data rows) so the background reads as one continuous
// fill.
func (t Table) renderGroupRowV2(row Row, selected bool, width int) string {
	innerWidth := width - tableRightMargin
	rightWidth, right := renderGroupRight(row.GroupRight, row.GroupStyle)
	leftBudget := max(innerWidth-rightWidth, 0)
	gap := ""
	if rightWidth > 0 {
		leftBudget = max(leftBudget-1, 0) // 1-cell gap before the chips
		gap = row.GroupStyle.Render(" ")
	}
	left := Pad("  "+row.GroupHeader, leftBudget)
	trailing := row.GroupStyle.Render(strings.Repeat(" ", tableRightMargin))

	if !selected {
		return row.GroupStyle.Render(left) + gap + right + trailing
	}
	return t.SelBarStyle.Render(tableSelBar) + row.GroupStyle.Render(left[2:]) + gap + right + trailing
}

// renderGroupRight draws Row.GroupRight's chips (each independently toned),
// separated by 1-cell gaps filled through the group line's own background —
// returns the rendered string and its total display width so the caller can
// budget the left text's Pad width around it.
func renderGroupRight(cells []Cell, fill lipgloss.Style) (int, string) {
	if len(cells) == 0 {
		return 0, ""
	}
	parts := make([]string, len(cells))
	width := 0
	for i, c := range cells {
		parts[i] = c.Style.Render(c.Text)
		width += ansi.StringWidth(parts[i])
	}
	width += len(cells) - 1
	return width, strings.Join(parts, fill.Render(" "))
}

// renderHeaderRuleV2 draws the ShowHeaderRule divider: a full-bleed rule
// (unlike the inset FooterLine) matching the rules Chrome v2 draws between
// its own bands.
func (t Table) renderHeaderRuleV2(width int) string {
	return t.RuleStyle.Render(strings.Repeat("─", width))
}

// visibleRowCount is how many data rows the viewport shows, after the
// header row and (when ShowHeaderRule) its divider claim their own lines.
func (t Table) visibleRowCount() int {
	rowCount := t.Height - 1
	if t.ShowHeaderRule {
		rowCount--
	}
	if rowCount < 1 {
		rowCount = t.totalRows()
	}
	return rowCount
}

// totalRows is the full list's row count: RowsTotal for a windowed caller,
// else every row it handed over.
func (t Table) totalRows() int {
	if t.RowsTotal > 0 {
		return t.RowsTotal
	}
	return len(t.Rows)
}

func (t Table) visibleRowsV2() []int {
	rowCount := t.visibleRowCount()
	total := t.totalRows()
	start := t.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + rowCount
	if end > total {
		end = total
	}
	indexes := make([]int, 0, end-start)
	for i := start; i < end; i++ {
		indexes = append(indexes, i)
	}
	return indexes
}

// VisibleRange reports the [Start,End) row window currently rendered plus
// the total row count, so a screen can render the "1–9 of 36" footer.
func (t Table) VisibleRange() VisibleRange {
	return ClampRange(t.Offset, t.visibleRowCount(), t.totalRows())
}

// FooterLine renders the range text ("1–9 of 36") left-aligned and a
// scrollbar right-aligned, both inset two cells from the screen edges like
// the chrome bands (mockup 2a's 14px padding).
func (t Table) FooterLine(width int) string {
	const inset = 2
	r := t.VisibleRange()
	rangeText := "0 of 0"
	if r.Total > 0 {
		rangeText = fmt.Sprintf("%d–%d of %d", r.Start+1, r.End, r.Total)
	}
	const trackWidth = 6
	bar := scrollbar(r, trackWidth)

	inner := max(width-2*inset, 0)
	avail := inner - ansi.StringWidth(rangeText) - ansi.StringWidth(bar)
	line := rangeText
	if avail >= 1 {
		line = rangeText + strings.Repeat(" ", avail) + bar
	}
	return t.FooterStyle.Render(Pad(strings.Repeat(" ", inset)+line, width))
}

// scrollbar draws the thumb at half-cell resolution (mockup 2a's
// "▐▌░░░░"): fully covered cells render █, a leading half-covered cell ▐
// (right half block), a trailing one ▌ (left half block), the rest ░.
func scrollbar(r VisibleRange, trackWidth int) string {
	if r.Total <= 0 || r.End <= r.Start {
		return strings.Repeat("░", trackWidth)
	}
	halves := trackWidth * 2
	start := r.Start * halves / r.Total
	end := start + max(2, (r.End-r.Start)*halves/r.Total)
	if end > halves {
		start, end = max(0, start-(end-halves)), halves
	}
	var b strings.Builder
	for i := range trackWidth {
		lo, hi := i*2, i*2+2
		switch {
		case start <= lo && end >= hi:
			b.WriteRune('█')
		case start == lo+1 && end >= hi:
			b.WriteRune('▐')
		case start <= lo && end == lo+1:
			b.WriteRune('▌')
		default:
			b.WriteRune('░')
		}
	}
	return b.String()
}

func padLeft(value string, width int) string {
	value = Truncate(value, width)
	cellWidth := ansi.StringWidth(value)
	if cellWidth >= width {
		return value
	}
	return strings.Repeat(" ", width-cellWidth) + value
}
