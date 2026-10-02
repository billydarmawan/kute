package components

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
)

func TestTableRenderHeadersUppercase(t *testing.T) {
	table := Table{
		Width:   40,
		Height:  3,
		Columns: []Column{{Title: "Name", Min: 20}, {Title: "Age", Min: 6, Align: AlignRight}},
		Rows:    []Row{{Cells: []Cell{{Text: "api"}, {Text: "2d"}}}},
	}
	got := table.Render()
	if !strings.Contains(got, "NAME") {
		t.Fatalf("expected uppercase NAME header:\n%s", got)
	}
	if !strings.Contains(got, "AGE") {
		t.Fatalf("expected uppercase AGE header:\n%s", got)
	}
}

func TestTableRenderSortIndicator(t *testing.T) {
	table := Table{
		Width:   40,
		Height:  3,
		SortKey: "age",
		SortAsc: true,
		Columns: []Column{{Title: "Name", Min: 20}, {Title: "Age", Min: 10, Sort: "age"}},
		Rows:    []Row{{Cells: []Cell{{Text: "api"}, {Text: "2d"}}}},
	}
	got := strings.Split(table.Render(), "\n")[0]
	if !strings.Contains(got, "↑") {
		t.Fatalf("expected sort indicator in header:\n%s", got)
	}
}

// TestTableSortIndicatorNeedsTwoSpareCells pins renderHeaderV2's width
// guard: the arrow costs a space plus the glyph, and a column one cell short
// drops it silently rather than overflowing its slot and shifting every
// column to its right. resources.fixedWidths budgets against this contract
// (TestFixedWidthsLeaveRoomForSortArrow), so it may not drift unnoticed.
func TestTableSortIndicatorNeedsTwoSpareCells(t *testing.T) {
	header := func(min int) string {
		table := Table{
			Width:   40,
			Height:  3,
			SortKey: "rev",
			SortAsc: true,
			Columns: []Column{{Title: "Name", Min: 20}, {Title: "Rev", Min: min, Sort: "rev"}},
			Rows:    []Row{{Cells: []Cell{{Text: "api"}, {Text: "2"}}}},
		}
		return strings.Split(table.Render(), "\n")[0]
	}
	if got := header(5); !strings.Contains(got, "REV ↑") {
		t.Fatalf("a 5-cell column fits \"REV ↑\" exactly, want the arrow:\n%s", got)
	}
	if got := header(4); strings.Contains(got, "↑") {
		t.Fatalf("a 4-cell column can't fit the arrow without overflowing:\n%s", got)
	}
}

func TestTableRenderRightAligns(t *testing.T) {
	table := Table{
		Width:   40,
		Height:  3,
		Columns: []Column{{Title: "Name", Min: 20}, {Title: "Age", Min: 8, Align: AlignRight}},
		Rows:    []Row{{Cells: []Cell{{Text: "api"}, {Text: "2d"}}}},
	}
	lines := strings.Split(table.Render(), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines:\n%s", table.Render())
	}
	if !strings.HasSuffix(strings.TrimRight(lines[1], " "), "2d") {
		t.Fatalf("expected right-aligned age at line end: %q", lines[1])
	}
}

func TestTableFlexColumnsFillWidth(t *testing.T) {
	table := Table{
		Width:   50,
		Height:  3,
		Columns: []Column{{Title: "Name", Min: 4, Flex: true}, {Title: "Age", Min: 6}},
		Rows:    []Row{{Cells: []Cell{{Text: "api"}, {Text: "2d"}}}},
	}
	for _, line := range strings.Split(table.Render(), "\n") {
		if runewidth.StringWidth(line) != 50 {
			t.Fatalf("line width = %d, want 50: %q", runewidth.StringWidth(line), line)
		}
	}
}

func TestTableRenderSelectedRowShowsBar(t *testing.T) {
	table := Table{
		Width:    30,
		Height:   4,
		Selected: 1,
		Columns:  []Column{{Title: "Name", Min: 20, Flex: true}, {Title: "Age", Min: 6}},
		Rows: []Row{
			{Cells: []Cell{{Text: "api"}, {Text: "2d"}}},
			{Cells: []Cell{{Text: "worker"}, {Text: "1d"}}},
		},
	}
	lines := strings.Split(table.Render(), "\n")
	if !strings.Contains(lines[1], "api") || strings.Contains(lines[1], tableSelBar) {
		t.Fatalf("row 0 should be unmarked: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], tableSelBar) {
		t.Fatalf("row 1 should start with the selection bar: %q", lines[2])
	}
}

func TestTableRenderSelectedRowMergesBackgroundOntoPlainCellStyle(t *testing.T) {
	fg := lipgloss.Color("#d0d0d0")
	selBg := lipgloss.Color("#3a3a5c")
	// A plain, Foreground-only cell style — the bug shape: the caller never
	// baked the selection background into the cell, only Table's own
	// SelRowStyle/SelBarStyle carry it.
	cellStyle := lipgloss.NewStyle().Foreground(fg)
	table := Table{
		Width:    30,
		Height:   3,
		Selected: 0,
		Columns:  []Column{{Title: "Name", Min: 20, Flex: true}, {Title: "Age", Min: 6}},
		Rows: []Row{
			{Cells: []Cell{{Text: "api", Style: cellStyle}, {Text: "2d", Style: cellStyle}}},
		},
		SelBarStyle: lipgloss.NewStyle().Background(selBg),
		SelRowStyle: lipgloss.NewStyle().Background(selBg),
	}
	got := table.Render()

	want := lipgloss.NewStyle().Foreground(fg).Background(selBg).Render("api")
	if !strings.Contains(got, want) {
		t.Fatalf("expected selected row's plain cell to carry both foreground and background:\nwant substring: %q\ngot: %q", want, got)
	}
}

func TestTableRenderMarkedRowMergesBackgroundOntoPlainCellStyle(t *testing.T) {
	fg := lipgloss.Color("#d0d0d0")
	markBg := lipgloss.Color("#403020")
	cellStyle := lipgloss.NewStyle().Foreground(fg)
	table := Table{
		Width:    30,
		Height:   3,
		Selected: -1, // nothing selected — isolates the RowStyle/mark path
		Columns:  []Column{{Title: "Name", Min: 20, Flex: true}, {Title: "Age", Min: 6}},
		Rows: []Row{
			{
				Cells:    []Cell{{Text: "api", Style: cellStyle}, {Text: "2d", Style: cellStyle}},
				RowStyle: lipgloss.NewStyle().Background(markBg),
			},
		},
	}
	got := table.Render()

	want := lipgloss.NewStyle().Foreground(fg).Background(markBg).Render("api")
	if !strings.Contains(got, want) {
		t.Fatalf("expected marked row's plain cell to carry both foreground and background:\nwant substring: %q\ngot: %q", want, got)
	}
}

func TestTableRenderSelectedRowLeavesPreRenderedCellUntouched(t *testing.T) {
	fg := lipgloss.Color("#00ff00")
	selBg := lipgloss.Color("#3a3a5c")
	// A composite/pre-rendered cell (mirrors browse's MiniBar/health-glyph
	// cells): Cell.Style is the zero value, the color is already embedded in
	// Text with its own internal SGR reset.
	preRendered := lipgloss.NewStyle().Foreground(fg).Render("██")
	table := Table{
		Width:    30,
		Height:   3,
		Selected: 0,
		Columns:  []Column{{Title: "Name", Min: 20, Flex: true}, {Title: "Age", Min: 6}},
		Rows: []Row{
			{Cells: []Cell{{Text: preRendered}, {Text: "2d"}}},
		},
		SelBarStyle: lipgloss.NewStyle().Background(selBg),
		SelRowStyle: lipgloss.NewStyle().Background(selBg),
	}
	got := table.Render()

	if !strings.Contains(got, preRendered) {
		t.Fatalf("expected composite/pre-rendered cell text to pass through untouched:\ngot: %q", got)
	}
}

func TestTableRenderGroupHeader(t *testing.T) {
	table := Table{
		Width:   30,
		Height:  4,
		Columns: []Column{{Title: "Name", Min: 20, Flex: true}, {Title: "Age", Min: 6}},
		Rows: []Row{
			{GroupHeader: "▸ default · 3 pods"},
			{Cells: []Cell{{Text: "api"}, {Text: "2d"}}},
		},
	}
	lines := strings.Split(table.Render(), "\n")
	if !strings.Contains(lines[1], "▸ default · 3 pods") {
		t.Fatalf("expected group header line, got: %q", lines[1])
	}
}

func TestTableVisibleRange(t *testing.T) {
	rows := make([]Row, 12)
	for i := range rows {
		rows[i] = Row{Cells: []Cell{{Text: "x"}}}
	}
	table := Table{Width: 20, Height: 5, Offset: 3, Columns: []Column{{Title: "Name", Min: 10}}, Rows: rows}
	r := table.VisibleRange()
	if r.Start != 3 || r.End != 7 || r.Total != 12 {
		t.Fatalf("VisibleRange() = %+v, want Start=3 End=7 Total=12", r)
	}
}

func TestTableFooterLine(t *testing.T) {
	rows := make([]Row, 36)
	for i := range rows {
		rows[i] = Row{Cells: []Cell{{Text: "x"}}}
	}
	table := Table{Width: 40, Height: 10, Columns: []Column{{Title: "Name", Min: 10}}, Rows: rows}
	got := table.FooterLine(40)
	if !strings.Contains(got, "1–9 of 36") {
		t.Fatalf("expected range text, got: %q", got)
	}
	if runewidth.StringWidth(got) != 40 {
		t.Fatalf("footer width = %d, want 40: %q", runewidth.StringWidth(got), got)
	}
}

// TestColumnWidthsMatchesWhatRenderUses pins the exported accessor against
// the header Render actually produces — callers use it to decide whether a
// cell will truncate, so a divergence would have them offering to expand
// text that fits, or staying quiet about text that doesn't.
func TestColumnWidthsMatchesWhatRenderUses(t *testing.T) {
	table := Table{
		Width:   60,
		Height:  3,
		Columns: []Column{{Title: "Rev", Min: 5}, {Title: "Status", Min: 16, Flex: true}},
		Rows:    []Row{{Cells: []Cell{{Text: "2"}, {Text: "deployed"}}}},
	}
	widths := table.ColumnWidths(60)
	if len(widths) != 2 {
		t.Fatalf("ColumnWidths returned %d entries, want 2", len(widths))
	}
	if widths[0] != 5 {
		t.Errorf("fixed column width = %d, want its Min of 5", widths[0])
	}
	// The flex column takes everything the fixed column and the table's own
	// margins/gaps leave behind: 60 - 2 (marker) - 2 (right margin) - 2 (gap) - 5.
	if want := 60 - 2 - tableRightMargin - 2 - 5; widths[1] != want {
		t.Errorf("flex column width = %d, want %d", widths[1], want)
	}
	// A cell exactly at the reported width must survive Render untruncated.
	fits := strings.Repeat("x", widths[1])
	table.Rows = []Row{{Cells: []Cell{{Text: "2"}, {Text: fits}}}}
	if !strings.Contains(table.Render(), fits) {
		t.Errorf("a cell of exactly ColumnWidths' width was truncated:\n%s", table.Render())
	}
}

// TestTableWindowedRowsRenderIdentically pins RowsStart/RowsTotal's contract:
// handing Table only the window VisibleRange reports must draw exactly what
// handing it every row does — table, footer and all — at every scroll
// position, including a selection inside the window and short lists.
func TestTableWindowedRowsRenderIdentically(t *testing.T) {
	for _, total := range []int{0, 1, 5, 40} {
		all := make([]Row, total)
		for i := range all {
			if i%7 == 3 {
				all[i] = Row{GroupHeader: "group " + strings.Repeat("x", i%5), GroupStyle: lipgloss.NewStyle().Bold(true)}
				continue
			}
			all[i] = Row{Cells: []Cell{{Text: "row-" + strings.Repeat("r", i%9)}, {Text: "1d"}}}
		}
		for _, height := range []int{1, 2, 8} {
			for offset := -1; offset <= total+1; offset++ {
				base := Table{
					Width:          40,
					Height:         height,
					Columns:        []Column{{Title: "Name", Min: 10, Flex: true}, {Title: "Age", Min: 4, Align: AlignRight}},
					Selected:       offset + 1,
					Offset:         offset,
					ShowHeaderRule: true,
				}
				full := base
				full.Rows = all
				windowed := base
				windowed.RowsTotal = total
				win := windowed.VisibleRange()
				windowed.Rows = all[win.Start:win.End]
				windowed.RowsStart = win.Start
				if got, want := windowed.Render()+"\n"+windowed.FooterLine(40), full.Render()+"\n"+full.FooterLine(40); got != want {
					t.Fatalf("total=%d height=%d offset=%d: windowed render differs\n got:\n%s\nwant:\n%s", total, height, offset, got, want)
				}
			}
		}
	}
}
