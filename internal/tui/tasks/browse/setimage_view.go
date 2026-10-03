// 24a's panel rendering (docs/design README.md §24a, sourced from
// docs/design/v.0.2.0.dc.html lines 160-230): the column header + the
// selected row frozen above a bordered panel (container tabs, the tag-first
// image field, the TAG · SEEN · FROM history table) and a "will run" strip
// below it. Kept in its own file, browse's per-concern split convention
// (view.go stays the live-table renderer; this is the one Body() branch
// that replaces it while pendingSetImage is showing).
package browse

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// setImageBody renders 24a's panel in place of the live table: column
// header + the selected row alone (not the whole scrollable table — a
// deliberate simplification from the mockup's illustrative 2-row framing,
// keeping the panel's height fixed/predictable, the same way
// bulkDeleteConfirmModal/confirmBody already drop the live table for a
// TierModal confirm rather than compositing table+modal).
func (m Model) setImageBody(width, height int) string {
	theme := m.Theme()
	var lines []string
	if row, ok := m.selectedRow(); ok {
		lines = append(lines, m.setImageSelectedRowLine(row, theme, width))
	} else {
		lines = append(lines, m.columnHeaderLine(theme, width))
	}
	lines = append(lines, "")
	lines = append(lines, m.setImagePanelLines(theme, width)...)
	lines = append(lines, "", m.setImageWillRunStrip(theme, width))
	return components.Pad(strings.Join(lines, "\n"), width)
}

// setImageSelectedRowLine renders row through components.Table (Selected:0,
// a single-row table) so it carries the exact same per-column styling
// tableBody's own selected row does — components.Table.Render already
// composes the selection bar/background, so this is a thin wrapper rather
// than hand-building a line.
func (m Model) setImageSelectedRowLine(row resources.Row, theme tui.Theme, width int) string {
	cols := browseColumns(m.desc)
	st := newRowCellStyles(theme, true, false, false)
	cells := m.rowCells(row, nil, cols, width, st, theme, 0, 0, "", false, false)
	t := components.Table{
		Columns:        cols,
		Rows:           []components.Row{{Cells: cells, RowStyle: st.dim}},
		Selected:       0,
		Width:          width,
		HeaderStyle:    lipgloss.NewStyle().Foreground(theme.TextFaint),
		SelBarStyle:    lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle:    lipgloss.NewStyle().Background(theme.SelBg),
		ShowHeaderRule: true,
		RuleStyle:      lipgloss.NewStyle().Foreground(theme.TextGhost2),
	}
	return t.Render()
}

// setImagePanelBorderStyle is the panel box's border — theme.BorderPalette
// (dark #3b3b58 / light #b9bcd0) is already the mockup's exact panel-border
// color, the same token the goto/namespace/context palette shell uses, so
// no new theme token is needed. theme.BgPalette (empty in both themes) is
// the existing "dialogs render on the terminal's own background" convention
// (theme.go's doc comment on BgPalette/ConfirmHeaderBg) — the mockup's
// #101018 panel fill is that same convention's illustrative screenshot
// background, not a real fill to reproduce.
func setImagePanelBorderStyle(theme tui.Theme) lipgloss.Style {
	return lipgloss.NewStyle().BorderForeground(theme.BorderPalette).Background(theme.BgPalette)
}

// setImagePanelLines builds the bordered panel (container tabs, image
// field, history table) as already-inset, already-bordered lines — a
// []string so setImageBody can splice a blank spacer/will-run strip around
// it without re-parsing a joined block.
func (m Model) setImagePanelLines(theme tui.Theme, width int) []string {
	t := m.pendingSetImage
	outerWidth := max(width-2*tui.FrameInset, 4)
	innerWidth := max(outerWidth-2, 2) // minus the box's own left/right border chars
	contentWidth := max(innerWidth-2, 1)

	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", innerWidth))

	content := []string{
		setImageInset(m.setImageContainerTabLine(t, theme, contentWidth), contentWidth),
		rule,
		setImageInset(m.setImageFieldLine(t, theme, contentWidth), contentWidth),
		rule,
		setImageInset(setImageHistoryHeaderLine(theme, contentWidth), contentWidth),
	}
	for _, l := range setImageHistoryRowLines(t, theme, contentWidth) {
		content = append(content, setImageInset(l, contentWidth))
	}

	// +2: lipgloss v2's Width counts the border itself (v1 added it on top),
	// and innerWidth is the pre-border content width the lines above are
	// already padded to — so the box needs innerWidth+2 to render at the
	// same outerWidth total as before.
	box := setImagePanelBorderStyle(theme).Border(lipgloss.RoundedBorder()).Width(innerWidth + 2).Render(strings.Join(content, "\n"))
	out := make([]string, 0)
	for _, l := range strings.Split(box, "\n") {
		out = append(out, strings.Repeat(" ", tui.FrameInset)+l)
	}
	return out
}

// setImageInset adds the box's own 1-space left/right breathing room around
// an already-built contentWidth-wide line (mockup's "padding: 6px 12px").
func setImageInset(line string, contentWidth int) string {
	return " " + components.Pad(line, contentWidth) + " "
}

// setImageContainerTabLine is the panel's top row: the "container" label,
// the active container as a highlighted pill, the remaining container names
// dim, and (only with more than one container) the "↹ switch container"
// note.
func (m Model) setImageContainerTabLine(t *setImageTarget, theme tui.Theme, width int) string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("container")
	pillStyle := lipgloss.NewStyle().Foreground(theme.AccentHi).Background(theme.SelBg).Bold(true)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	parts := []string{label}
	for i, c := range t.containers {
		name := c.Name
		if c.IsSidecar {
			name += " sidecar"
		} else if c.initContainer {
			name += " init"
		}
		if i == t.containerIdx {
			parts = append(parts, pillStyle.Render(" "+name+" "))
		} else {
			parts = append(parts, dim.Render(name))
		}
	}
	left := strings.Join(parts, "  ")

	right := ""
	if len(t.containers) > 1 {
		right = lipgloss.NewStyle().Foreground(theme.TextFaint).Render(tui.GlyphTab + " switch container")
	}
	return padBetween(left, right, width)
}

// setImageFieldLine is the panel's tag-first "image ›" prompt: the dim repo
// prefix (outside fullRef mode), the bold editable buffer split around
// t.cursor, and a cursor glyph at that position — §24a: "the image › field
// pre-fills the current ref with the cursor on the tag, repo prefix dim".
func (m Model) setImageFieldLine(t *setImageTarget, theme tui.Theme, width int) string {
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)

	prompt := accent.Render("image ›") + " "
	note := "editing tag · ctrl+e edit full ref"
	if t.fullRef {
		note = "editing full ref"
	}
	renderedNote := faint.Render(note)
	available := max(width-lipgloss.Width(prompt), 1)

	// Give the value first claim on the row. At narrow widths an editing
	// hint is less useful than seeing the current value and caret; once the
	// whole field fits, show the hint only when it also fits in full.
	naturalInput := t.input.ViewWidth(lipgloss.Width(t.input.Value()) + 1)
	naturalInputWidth := lipgloss.Width(naturalInput)
	prefix := ""
	if !t.fullRef {
		prefixBudget := min(lipgloss.Width(t.repo+":"), max(available-min(naturalInputWidth, available), 1))
		// Front-elide like every truncated image ref (§10a): the repo tail
		// next to the tag being edited beats the registry host.
		prefix = dim.Render(components.TruncateFront(t.repo+":", prefixBudget))
	}
	inputWidth := max(available-lipgloss.Width(prefix), 1)
	left := prompt + prefix + t.input.ViewWidth(inputWidth)
	if width-lipgloss.Width(left) <= lipgloss.Width(renderedNote) {
		renderedNote = ""
	}

	// §24a's "same image" message belongs to the will-run strip below (the
	// surface that normally names the exact kubectl command) — this field
	// row's own right note always names the editing mode.
	return padBetween(left, renderedNote, width)
}

// setImageHistoryHeaderLine is the TAG · SEEN · FROM column header — a
// bespoke fixed-width mini-grid (not components.Table, same "hand-rolled
// rows" idiom tasks/events already uses for its own two-line rows) since
// this table lives inside 24a's own bordered panel, not the app's shared
// list skeleton.
func setImageHistoryHeaderLine(theme tui.Theme, width int) string {
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	return faint.Render(historyRowColumns("TAG", "SEEN", "FROM", width))
}

// setImageHistoryRowLines renders a 3-row window of t.history centered on
// historyIdx (or every row, unpadded, when there are 3 or fewer) — the
// selected entry gets the ▎ bar + SelBg background, the same selected-row
// idiom tableBody's own rows use.
func setImageHistoryRowLines(t *setImageTarget, theme tui.Theme, width int) []string {
	const visible = 3
	if len(t.history) == 0 {
		return []string{lipgloss.NewStyle().Foreground(theme.TextFaint).Render("no history yet — nothing seen on this workload or its revisions")}
	}
	start := 0
	if len(t.history) > visible {
		start = t.historyIdx - 1
		if start < 0 {
			start = 0
		}
		if start > len(t.history)-visible {
			start = len(t.history) - visible
		}
	}
	end := min(start+visible, len(t.history))

	bar := lipgloss.NewStyle().Foreground(theme.Accent)
	tagSel := lipgloss.NewStyle().Foreground(theme.Text)
	tagOther := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		e := t.history[i]
		selected := i == t.historyIdx
		cursor := "  "
		tagStyle := tagOther
		if selected {
			cursor = bar.Render("›") + " "
			tagStyle = tagSel
		}
		row := historyRowColumns(cursor+tagStyle.Render(e.tag), dim.Render(e.seenLabel), dim.Render(e.from), width)
		if selected {
			row = lipgloss.NewStyle().Background(theme.SelBg).Render(components.Pad(row, width))
		}
		lines = append(lines, row)
	}
	return lines
}

// historyRowColumns lays tag/seen/from out at fixed widths (10/14, FROM
// takes the remainder) — already-styled spans, measured via lipgloss.Width
// so ANSI never throws off alignment.
func historyRowColumns(tag, seen, from string, width int) string {
	const tagWidth, seenWidth = 12, 15
	pad := func(s string, w int) string {
		// Always leave at least one gap space, even when s overflows w — an
		// unusually long SEEN value (a multi-digit revision, say) must never
		// run straight into FROM with no separator.
		if gap := w - lipgloss.Width(s); gap > 1 {
			return s + strings.Repeat(" ", gap)
		}
		return s + " "
	}
	fromWidth := max(width-tagWidth-seenWidth, 1)
	return pad(tag, tagWidth) + pad(seen, seenWidth) + components.Truncate(from, fromWidth)
}

// setImageWillRunStrip is the panel's own "will run" line, styled like the
// mockup's #0c0c12 strip: a BorderSubtle top rule, then BgStrip-filled left
// "will run: kubectl set image ..." (or the no-op message) and a
// right-aligned "applying rolls out N pods" note — or, once there's a result
// to report, t.lastError/t.message take over the strip entirely (same
// error-then-message precedence as metaWillRunStrip), and while a PROD
// confirm is deciding the pending apply, the right note becomes "confirm to
// apply · y/N" instead (mirrors metaNoteText's own pendingConfirm case).
func (m Model) setImageWillRunStrip(theme tui.Theme, width int) string {
	t := m.pendingSetImage
	fill := lipgloss.NewStyle().Background(theme.BgStrip)
	label := fill.Foreground(theme.TextDim)
	cmd := fill.Foreground(theme.TextSecondary)
	warn := fill.Foreground(theme.Warn)

	var left, right string
	switch {
	case t.lastError != "":
		left = label.Render("error") + fill.Render(" ") + fill.Foreground(theme.Bad).Render(t.lastError)
	case t.message != "":
		left = fill.Foreground(theme.Good).Render(t.message)
	case t.unchanged():
		message := "same image — apply is a no-op; use rollout restart"
		if t.kind == kube.KindCronJob {
			message = "same image — apply is a no-op"
		}
		left = label.Render("will run") + fill.Render(" ") + cmd.Render(message)
	default:
		left = label.Render("will run") + fill.Render(" ") + cmd.Render(kube.SetImageCommandString(t.kind, t.namespace, t.name, t.activeContainer().Name, t.composedImage()))
		if m.actions.Active() {
			right = warn.Render("confirm to apply · y/N")
		} else if t.kind == kube.KindCronJob {
			right = warn.Render("future jobs only · running jobs unaffected")
		} else {
			right = warn.Render(fmt.Sprintf("applying rolls out %d pods", t.desiredCount))
		}
	}

	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", width))
	return rule + "\n" + insetStripLineFill(padBetweenFill(left, right, stripInnerWidth(width), fill), width, fill)
}
