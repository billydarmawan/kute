// 25a's panel rendering (docs/design README.md §25a, sourced from
// docs/design/v.0.2.0.dc.html's 25a mockup): the column header + the
// selected row frozen above a bordered panel (container tabs + failure
// callout strip, the FIELD·CURRENT·NEW·P95 USAGE grid) and a "will run"
// strip below it — the same shape setimage_view.go's setImageBody already
// established for 24a's panel (view.go stays the live-table renderer; this
// is the one Body() branch that replaces it while pendingSetResources is
// showing).
package browse

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// setResourcesBody renders 25a's panel in place of the live table — same
// simplification setImageBody already makes (the selected row alone, not
// the whole scrollable table, keeping the panel's height fixed).
func (m Model) setResourcesBody(width, height int) string {
	theme := m.Theme()
	var lines []string
	if row, ok := m.selectedRow(); ok {
		lines = append(lines, m.setImageSelectedRowLine(row, theme, width))
	} else {
		lines = append(lines, m.columnHeaderLine(theme, width))
	}
	lines = append(lines, "")
	lines = append(lines, m.setResourcesPanelLines(theme, width)...)
	lines = append(lines, "", m.setResourcesWillRunStrip(theme, width))
	return components.Pad(strings.Join(lines, "\n"), width)
}

// setResourcesPanelLines builds the bordered panel (container-tab/failure
// strip, FIELD grid) as already-inset, already-bordered lines — mirrors
// setImagePanelLines' shape exactly, same box style/inset.
func (m Model) setResourcesPanelLines(theme tui.Theme, width int) []string {
	t := m.pendingSetResources
	outerWidth := max(width-2*tui.FrameInset, 4)
	innerWidth := max(outerWidth-2, 2)
	contentWidth := max(innerWidth-2, 1)

	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", innerWidth))

	content := []string{
		setImageInset(m.setResourcesStripLine(t, theme, contentWidth), contentWidth),
		rule,
		setImageInset(setResourcesFieldHeaderLine(theme, contentWidth), contentWidth),
	}
	for i := range t.fields {
		content = append(content, setImageInset(m.setResourcesFieldLine(t, i, theme, contentWidth), contentWidth))
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

// setResourcesStripLine is the panel's top row: the "container" label, the
// active container as a highlighted pill (reusing setImageContainerTabLine's
// exact styling), a dim provenance note, and — only when the active
// container has one — a right-aligned OOMKill callout (docs/design
// README.md §25a: "right-aligned failure callout").
func (m Model) setResourcesStripLine(t *setResourcesTarget, theme tui.Theme, width int) string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("container")
	pillStyle := lipgloss.NewStyle().Foreground(theme.AccentHi).Background(theme.SelBg).Bold(true)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)

	parts := []string{label}
	for i, c := range t.containers {
		name := c.Name
		if c.IsSidecar {
			name += " sidecar"
		}
		if i == t.containerIdx {
			parts = append(parts, pillStyle.Render(" "+name+" "))
		} else {
			parts = append(parts, dim.Render(name))
		}
	}
	left := strings.Join(parts, "  ") + "  " + faint.Render("usage: from the metrics poll")

	right := ""
	if t.oomOK {
		right = lipgloss.NewStyle().Foreground(theme.Bad).Render(
			fmt.Sprintf("%s OOMKilled %s ago at the current limit", tui.GlyphFailed, shortAge(t.oomAge)),
		)
	}
	return padBetween(left, right, width)
}

// setResourcesFieldHeaderLine is the FIELD·CURRENT·NEW·P95 USAGE column
// header.
func setResourcesFieldHeaderLine(theme tui.Theme, width int) string {
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	return faint.Render(resourcesRowColumns("", "FIELD", "CURRENT", "NEW", "P95 USAGE", width, lipgloss.NewStyle()))
}

// resourcesRowColumns lays marker/field/current/new/usage out at fixed
// widths (2/12/10/14, USAGE takes the remainder) — already-styled spans,
// measured via lipgloss.Width so ANSI never throws off alignment. fill
// renders every gap/padding space, so a selected row's Background(SelBg)
// covers the whole width rather than just the pre-styled spans (mirrors
// setimage_view.go's historyRowColumns idiom, plus the fill-aware padding
// view.go's own padBetweenFill/insetStripLineFill already use for the one
// other real-background strip in this package).
func resourcesRowColumns(marker, field, current, newVal, usage string, width int, fill lipgloss.Style) string {
	const markerWidth, fieldWidth, currentWidth, newWidth = 2, 12, 10, 14
	pad := func(s string, w int) string {
		gap := w - lipgloss.Width(s)
		if gap < 0 {
			gap = 1 // always leave a separator when content overflows its column
		}
		return s + fill.Render(strings.Repeat(" ", gap))
	}
	usageWidth := max(width-markerWidth-fieldWidth-currentWidth-newWidth, 1)
	usage = components.Truncate(usage, usageWidth)
	if gap := usageWidth - lipgloss.Width(usage); gap > 0 {
		usage += fill.Render(strings.Repeat(" ", gap))
	}
	return pad(marker, markerWidth) + pad(field, fieldWidth) + pad(current, currentWidth) + pad(newVal, newWidth) + usage
}

// setResourcesFieldLine renders one FIELD row (idx into t.fields). Every
// span in a selected row — not just the row's own text — carries
// Background(SelBg) explicitly (rather than wrapping the whole pre-rendered
// line in one outer-background Style.Render, which the codebase avoids
// here for the same reason newRowCellStyles' doc comment gives for the main
// table: each inner Render call emits its own trailing reset, which would
// cut the outer background off at the first styled span instead of
// covering the row end to end).
func (m Model) setResourcesFieldLine(t *setResourcesTarget, idx int, theme tui.Theme, width int) string {
	f := t.fields[idx]
	selected := idx == t.fieldIdx
	withBg := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(theme.SelBg)
		}
		return s
	}
	fill := withBg(lipgloss.NewStyle())

	labelFg := theme.TextSecondary
	if selected {
		labelFg = theme.Text
	}
	labelStyle := withBg(lipgloss.NewStyle().Foreground(labelFg))
	dimStyle := withBg(lipgloss.NewStyle().Foreground(theme.TextDim))

	marker := fill.Render("  ")
	if selected {
		marker = withBg(lipgloss.NewStyle().Foreground(theme.Accent)).Render("›") + fill.Render(" ")
	}

	current := f.current
	if !f.hasCurrent {
		current = "—"
	}

	return resourcesRowColumns(
		marker, labelStyle.Render(f.label), dimStyle.Render(current),
		m.setResourcesNewCell(f, selected, theme), t.usageCell(f, selected, theme),
		width, fill,
	)
}

// setResourcesNewCell renders the NEW column: "— none" in Warn when unset,
// underlined Bad when invalid (docs/design README.md §25a: "an invalid
// quantity underlines red inline and blocks ↵"), bold otherwise once
// touched. The cursor glyph renders whenever this is the selected field,
// regardless of whether it's been edited yet — the cursor belongs in the
// focused input the moment ↑↓ lands on it, the same as any text field.
func (m Model) setResourcesNewCell(f resourceField, selected bool, theme tui.Theme) string {
	withBg := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(theme.SelBg)
		}
		return s
	}
	warn := withBg(lipgloss.NewStyle().Foreground(theme.Warn))
	bad := withBg(lipgloss.NewStyle().Foreground(theme.Bad).Underline(true))
	bold := withBg(lipgloss.NewStyle().Foreground(theme.Text).Bold(true))
	dim := withBg(lipgloss.NewStyle().Foreground(theme.TextDim))
	accent := withBg(lipgloss.NewStyle().Foreground(theme.Accent))

	if f.unset {
		rendered := warn.Render("— none")
		if selected {
			rendered += accent.Render(tui.GlyphSelBar)
		}
		return rendered
	}

	style := dim
	switch {
	case f.invalid:
		style = bad
	case f.changed():
		style = bold
	}

	if !selected {
		text := f.input.Value()
		if text == "" {
			text = "—"
		}
		return style.Render(text)
	}

	// Selected: f.input carries the real cursor position (←/→ move it,
	// backspace/typing act at it) — style/Placeholder are set fresh on a
	// local copy each render since they depend on invalid/changed, computed
	// above, not on construction-time state.
	input := f.input
	styles := input.Styles()
	styles.Focused.Text = style
	styles.Focused.Placeholder = style
	input.SetStyles(styles)
	input.Placeholder = "—" // an empty, untouched buffer still needs a placeholder before the cursor
	return input.ViewWidth(14)
}

// usageCell renders the P95 USAGE column: a MiniBar + compact usage value
// against f's own current quantity as the denominator (the request/limit
// row's own configured value — the NEW typed value is a decision being
// made, not what's being measured against), or "metrics unavailable" in dim
// when no usage sample exists for the active container at all ("loading…"
// while the panel's one metrics read is still in flight).
func (t *setResourcesTarget) usageCell(f resourceField, selected bool, theme tui.Theme) string {
	withBg := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(theme.SelBg)
		}
		return s
	}
	fill := withBg(lipgloss.NewStyle())
	dim := withBg(lipgloss.NewStyle().Foreground(theme.TextDim))
	if !t.usageOK {
		if t.metricsLoading {
			return dim.Render("loading…")
		}
		return dim.Render("metrics unavailable")
	}

	var used, denom int64
	var valueText string
	if f.isCPU {
		used = t.cpuMilli
		valueText = kube.FormatCPU(*resource.NewMilliQuantity(used, resource.DecimalSI))
	} else {
		used = t.memBytes
		valueText = kube.FormatMemory(*resource.NewQuantity(used, resource.BinarySI))
	}
	if f.hasCurrent {
		if q, err := resource.ParseQuantity(f.current); err == nil {
			if f.isCPU {
				denom = q.MilliValue()
			} else {
				denom = q.Value()
			}
		}
	}

	const barWidth = 10
	bars := components.BarStyles{
		Track: withBg(lipgloss.NewStyle().Foreground(theme.BarTrack)),
		Fill:  withBg(lipgloss.NewStyle().Foreground(theme.Accent)),
		Warn:  withBg(lipgloss.NewStyle().Foreground(theme.Warn)),
		Bad:   withBg(lipgloss.NewStyle().Foreground(theme.Bad)),
	}
	bar := components.MiniBar(used, denom, barWidth, bars)

	textStyle := dim
	if denom > 0 {
		ratio := float64(used) / float64(denom)
		switch {
		case ratio >= 1:
			textStyle = withBg(lipgloss.NewStyle().Foreground(theme.BadText))
		case ratio >= 0.7:
			textStyle = withBg(lipgloss.NewStyle().Foreground(theme.Warn))
		}
	}
	return bar + fill.Render(" ") + textStyle.Render(valueText)
}

// setResourcesWillRunStrip is the panel's own "will run" line, styled like
// setImageWillRunStrip: a BorderSubtle top rule, then BgStrip-filled left
// "will run: kubectl set resources ..." (or the dry-run rejection message)
// and a right-aligned "applying rolls out N pods" note.
func (m Model) setResourcesWillRunStrip(theme tui.Theme, width int) string {
	t := m.pendingSetResources
	fill := lipgloss.NewStyle().Background(theme.BgStrip)
	label := fill.Foreground(theme.TextDim)
	cmd := fill.Foreground(theme.TextSecondary)
	warn := fill.Foreground(theme.Warn)
	bad := fill.Foreground(theme.BadText)
	good := fill.Foreground(theme.Good)

	edits := t.edits()
	changed := edits.CPURequest != nil || edits.CPULimit != nil || edits.MEMRequest != nil || edits.MEMLimit != nil

	var left, right string
	switch {
	case t.lastError != "":
		left = label.Render("error") + fill.Render(" ") + bad.Render(t.lastError)
	case t.message != "":
		left = good.Render(t.message)
	case t.dryRunErr != "":
		left = label.Render("error") + fill.Render(" ") + bad.Render(t.dryRunErr)
	case !changed:
		left = label.Render("will run") + fill.Render(" ") + cmd.Render("no changed fields — apply is a no-op")
	default:
		left = label.Render("will run") + fill.Render(" ") + cmd.Render(kube.SetResourcesCommandString(t.kind, t.namespace, t.name, t.activeContainer().Name, edits))
		if m.actions.Active() {
			right = warn.Render("confirm to apply · y/N")
		} else {
			right = warn.Render(fmt.Sprintf("applying rolls out %d pods", t.desiredCount))
		}
	}

	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", width))
	return rule + "\n" + insetStripLineFill(padBetweenFill(left, right, stripInnerWidth(width), fill), width, fill)
}
