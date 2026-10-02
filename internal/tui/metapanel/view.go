// 26a's panel rendering (docs/design README.md §26a, sourced from
// docs/design/v.0.2.0.dc.html's 26a mockup): a LABELS·N grid, then an
// ANNOTATIONS·N grid, each key=/value/right-note column shape, inside a
// bordered box, and a "will run" strip below it — the same shape browse's
// setresources_view.go/setimage_view.go established. The host's Body()
// composes these lines under its own frozen title/selected-row line.
package metapanel

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
)

// PanelLines builds the bordered panel (LABELS·N grid, ANNOTATIONS·N grid,
// the add-row hint) as already-inset, already-bordered lines — mirrors
// browse setResourcesPanelLines' shape exactly, same box style/inset. ctrl
// is read (Active) so a TierInline confirm renders the selected row's
// about-to-apply state; rendering stays pure f(state, theme, width).
func (p *Model) PanelLines(width int, ctrl *actions.Controller) []string {
	theme := p.theme()
	t := p.t
	outerWidth := max(width-2*tui.FrameInset, 4)
	innerWidth := max(outerWidth-2, 2)
	contentWidth := max(innerWidth-2, 1)

	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", innerWidth))

	// The focused grid's header renders in accent/bold — tab/shift+tab and
	// a/insert both act on whichever grid this highlights, so it's the one
	// piece of chrome that has to say so.
	labelsHeader, annotationsHeader := faint, faint
	if t.section == metaSectionLabels {
		labelsHeader = lipgloss.NewStyle().Foreground(theme.Accent).Bold(true)
	} else {
		annotationsHeader = lipgloss.NewStyle().Foreground(theme.Accent).Bold(true)
	}

	// While a TierInline confirm is showing (a joined-label edit, or any
	// removal), the panel stays open underneath it (the package doc comment)
	// — the selected row renders its about-to-apply state (the new value for
	// an edit, a "remove · y/N" note for a removal) instead of the live
	// typing cursor, since input has already moved to the confirm.
	confirming := ctrl.Active()

	var content []string
	content = append(content, panelInset(labelsHeader.Render(fmt.Sprintf("LABELS · %d", len(t.labels))), contentWidth))
	for i := range t.labels {
		content = append(content, panelInset(metaRowLine(t, false, i, theme, contentWidth, confirming), contentWidth))
	}
	if t.adding == metaAddLabel {
		content = append(content, panelInset(metaAddRowLine(t, theme, contentWidth), contentWidth))
	}

	content = append(content, rule)
	content = append(content, panelInset(annotationsHeader.Render(fmt.Sprintf("ANNOTATIONS · %d", len(t.annotations))), contentWidth))
	for i := range t.annotations {
		content = append(content, panelInset(metaRowLine(t, true, i, theme, contentWidth, confirming), contentWidth))
	}
	if t.adding == metaAddAnnotation {
		content = append(content, panelInset(metaAddRowLine(t, theme, contentWidth), contentWidth))
	}
	if t.adding == metaAddNone {
		hint := lipgloss.NewStyle().Foreground(theme.TextGhost).Render("+ a add to focused grid · tab switch grid")
		content = append(content, panelInset(hint, contentWidth))
	}

	// +2: lipgloss v2's Width counts the border itself (v1 added it on top),
	// and innerWidth is the pre-border content width the lines above are
	// already padded to — so the box needs innerWidth+2 to render at the
	// same outerWidth total as before.
	box := panelBorderStyle(theme).Border(lipgloss.RoundedBorder()).Width(innerWidth + 2).Render(strings.Join(content, "\n"))
	out := make([]string, 0)
	for _, l := range strings.Split(box, "\n") {
		out = append(out, strings.Repeat(" ", tui.FrameInset)+l)
	}
	return out
}

// metaRowColumns lays marker/key/value/note out (marker fixed 2ch, key fixed
// 26ch, note fixed 30ch right-aligned, value takes the remainder) —
// already-styled spans, measured via lipgloss.Width so ANSI never throws off
// alignment, the same fill-aware-padding idiom browse's resourcesRowColumns
// uses for 25a's own grid (widths sized for a terminal character grid, not a
// literal copy of the mockup's 220px CSS columns — docs/design README.md's
// Fidelity section: "exact pixel sizes are approximations of a character
// grid").
func metaRowColumns(marker, key, value, note string, width int, fill lipgloss.Style) string {
	const markerWidth, colGap = 2, 2
	// keyWidth/noteWidth cap at a comfortable size for the longest keys/notes
	// this screen actually renders (a "deployment.kubernetes.io/revision="
	// annotation key, a "helm-owned · edits survive until next upgrade"
	// note), but scale down as a share of the available width rather than
	// hard constants — otherwise a narrow terminal's floor-clamped value
	// column can push the row wider than width, which the caller's own
	// Pad(line, contentWidth) would then silently truncate from the right,
	// clipping the note instead of the (already-truncated) value.
	keyWidth, valueWidth, noteWidth := metaColumnWidths(width)

	pad := func(s string, w int) string {
		s = components.Truncate(s, w)
		gap := w - lipgloss.Width(s)
		if gap < 0 {
			gap = 0
		}
		return s + fill.Render(strings.Repeat(" ", gap))
	}
	value = components.Truncate(value, valueWidth)
	if gap := valueWidth - lipgloss.Width(value); gap > 0 {
		value += fill.Render(strings.Repeat(" ", gap))
	}
	note = components.Truncate(note, noteWidth)
	notePad := noteWidth - lipgloss.Width(note)
	if notePad < 0 {
		notePad = 0
	}
	return pad(marker, markerWidth) + pad(key, keyWidth) + value +
		fill.Render(strings.Repeat(" ", colGap)) + fill.Render(strings.Repeat(" ", notePad)) + note
}

func metaColumnWidths(width int) (keyWidth, valueWidth, noteWidth int) {
	const markerWidth, colGap = 2, 2
	avail := max(width-markerWidth-colGap, 6)
	keyWidth = min(36, avail*35/100)
	noteWidth = min(30, avail*3/10)
	valueWidth = max(avail-keyWidth-noteWidth, 1)
	return keyWidth, valueWidth, noteWidth
}

// metaRowLine renders one LABELS/ANNOTATIONS row (idx into t.labels or
// t.annotations, per isAnnotation). selected (the row the cursor is on,
// highlighted regardless of mode) and editing (selected AND the value is a
// live free-typing buffer, entered via ↵) are deliberately distinct — a
// merely-selected row shows its plain current value, exactly like the
// mockup's non-editing rows. confirming (ctrl.Active(), read by the
// caller) marks the selected row as the one a TierInline y/N is now deciding
// — mutually exclusive with editing in practice (updateEditKey/
// Update's D both end editing/never start it before Begin), but
// checked as its own case rather than folded into editing since a removal
// reaches pendingConfirm with no live buffer at all.
func metaRowLine(t *metaTarget, isAnnotation bool, idx int, theme tui.Theme, width int, confirming bool) string {
	rows := t.labels
	section, sectionIdx := metaSectionLabels, t.labelIdx
	if isAnnotation {
		rows, section, sectionIdx = t.annotations, metaSectionAnnotations, t.annotationIdx
	}
	r := rows[idx]
	selected := t.section == section && sectionIdx == idx
	editing := selected && t.editing
	pendingConfirm := selected && confirming

	withBg := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(theme.SelBg)
		}
		return s
	}
	fill := withBg(lipgloss.NewStyle())
	keyStyle := withBg(lipgloss.NewStyle().Foreground(theme.TextDim))

	marker := fill.Render("  ")
	if selected {
		marker = withBg(lipgloss.NewStyle().Foreground(theme.Accent)).Render("›") + fill.Render(" ")
	}

	key := keyStyle.Render(r.key + "=")

	var value string
	switch {
	case editing:
		_, valueWidth, _ := metaColumnWidths(width)
		value = metaValueCell(r, theme, true, valueWidth)
	case pendingConfirm && r.changed():
		value = metaPendingValueCell(r, theme, selected)
	case r.readOnly:
		value = withBg(lipgloss.NewStyle().Foreground(theme.TextDim)).Render(displayOrDash(r.current))
	default:
		style := withBg(lipgloss.NewStyle().Foreground(theme.Text))
		value = style.Render(displayOrDash(r.current))
	}

	note := metaNoteText(r, editing, pendingConfirm, theme, withBg)
	return metaRowColumns(marker, key, value, note, width, fill)
}

// displayOrDash renders s, or a dim placeholder dash when empty — labels can
// legitimately carry an empty string value.
func displayOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// metaValueCell renders the selected, editable row's value via r.input's own
// cursor-anchored View (SelBg background and "—" empty-placeholder already
// baked into its Styles by metaInputStyles), bolded and prefixed with "was
// <current> · " once touched — docs/design README.md §26a's own mockup
// example ("was stage · staging▎"). selected is always true in practice
// (metaRowLine only calls this for the selected+editing row) — kept as a
// parameter so the withBg fallback below stays explicit rather than assumed.
func metaValueCell(r metaRow, theme tui.Theme, selected bool, width int) string {
	withBg := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(theme.SelBg)
		}
		return s
	}
	was := withBg(lipgloss.NewStyle().Foreground(theme.TextFaint))

	input := r.input
	if r.changed() {
		styles := input.Styles()
		styles.Focused.Text = styles.Focused.Text.Bold(true)
		styles.Blurred.Text = styles.Blurred.Text.Bold(true)
		input.SetStyles(styles)
	}
	prefixWidth := 0
	if r.changed() {
		prefixWidth = lipgloss.Width(was.Render("was " + displayOrDash(r.current) + " · "))
	}
	rendered := input.ViewWidth(max(width-prefixWidth, 1))
	if r.changed() {
		rendered = was.Render("was "+displayOrDash(r.current)+" · ") + rendered
	}
	return rendered
}

// metaPendingValueCell renders the selected row's about-to-apply value while
// a TierInline confirm is deciding it — the same "was <current> · <new>"
// framing metaValueCell uses mid-edit, minus the cursor glyph, since typing
// has already handed off to the confirm's own y/N.
func metaPendingValueCell(r metaRow, theme tui.Theme, selected bool) string {
	withBg := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(theme.SelBg)
		}
		return s
	}
	style := withBg(lipgloss.NewStyle().Foreground(theme.Text).Bold(true))
	was := withBg(lipgloss.NewStyle().Foreground(theme.TextFaint))
	return was.Render("was "+displayOrDash(r.current)+" · ") + style.Render(displayOrDash(r.input.Value()))
}

// metaNoteText renders the right-note column: the read-only explanation
// (controller-managed annotation / immutable selector label) takes priority,
// then "editing value" while this row's buffer is actually live (editing —
// not merely selected), then the pending-confirm state (a removal's "remove
// · y/N", or an edit's "confirm to apply · y/N"), then the Service-selector
// join warning, then the Helm-owned note — each row shows at most one,
// matching the mockup's own one-note-per-row shape.
func metaNoteText(r metaRow, editing, pendingConfirm bool, theme tui.Theme, withBg func(lipgloss.Style) lipgloss.Style) string {
	switch {
	case r.readOnly:
		return withBg(lipgloss.NewStyle().Foreground(theme.TextFaint)).Render(r.readOnlyNote)
	case editing:
		return withBg(lipgloss.NewStyle().Foreground(theme.TextDim)).Render("editing value")
	case pendingConfirm && !r.changed():
		return withBg(lipgloss.NewStyle().Foreground(theme.Bad)).Render("remove · y/N")
	case pendingConfirm:
		return withBg(lipgloss.NewStyle().Foreground(theme.Warn)).Render("confirm to apply · y/N")
	case r.joinService != "":
		return withBg(lipgloss.NewStyle().Foreground(theme.Warn)).Render(fmt.Sprintf("%s selector · svc/%s", tui.GlyphSelectorJoin, r.joinService))
	case r.joinUnknown != "":
		return withBg(lipgloss.NewStyle().Foreground(theme.Warn)).Render("service joins unknown · " + r.joinUnknown)
	case r.helmOwnedNote:
		return withBg(lipgloss.NewStyle().Foreground(theme.TextFaint)).Render("helm-owned · edits survive until next upgrade")
	default:
		return ""
	}
}

// metaAddRowLine renders a/A's insert row: a highlighted "+" marker, the
// key buffer (cursor-anchored while unfocused-from-value), then "=" and the
// value buffer (cursor-anchored once tab moves focus there).
func metaAddRowLine(t *metaTarget, theme tui.Theme, width int) string {
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	keyWidth, valueWidth, _ := metaColumnWidths(width)
	keyCell := metaAddBufferCell(t.addKeyInput, dim, max(keyWidth-1, 1))
	valueCell := metaAddBufferCell(t.addValueInput, dim, valueWidth)

	marker := accent.Render("+ ")
	kind := "label"
	if t.adding == metaAddAnnotation {
		kind = "annotation"
	}
	note := dim.Render("new " + kind)
	return metaRowColumns(marker, keyCell+dim.Render("="), valueCell, note, width, lipgloss.NewStyle())
}

// metaAddBufferCell renders one of the add-row's two buffers via its own
// View (which embeds the cursor only while Focused) — the one case View()
// doesn't already cover is an unfocused, empty buffer, which old code showed
// as a dim "…" rather than nothing.
func metaAddBufferCell(input textfield.Model, dim lipgloss.Style, width int) string {
	if !input.Focused() && input.Value() == "" {
		return dim.Render("…")
	}
	return input.ViewWidth(width)
}

// WillRunStrip is the panel's own "will run" line, styled like browse's
// setResourcesWillRunStrip/setImageWillRunStrip: a BorderSubtle top rule,
// then BgStrip-filled left "will run: kubectl label/annotate ..." (or the
// add-row's live preview, or a neutral "no changes" note) and a static right
// note ("metadata only — no rollout") whenever there's a command to show —
// docs/design README.md §26a's own mockup right note, verbatim. ctrl is read
// (Active/Pending) so a pending removal renders its own command rather than
// "no changes".
func (p *Model) WillRunStrip(width int, ctrl *actions.Controller) string {
	theme := p.theme()
	t := p.t
	fill := lipgloss.NewStyle().Background(theme.BgStrip)
	label := fill.Foreground(theme.TextDim)
	cmd := fill.Foreground(theme.TextSecondary)
	rightNote := fill.Foreground(theme.TextDim)

	warnLabel := fill.Foreground(theme.Warn)
	// joinPrefix renders the "detaches N pods from svc/X" warning ahead of
	// the kubectl command — the panel's own will-run strip has the full
	// width to say so plainly, unlike the keybar's own RightNote for this
	// same confirm (the host's "set-meta" case), which keeps to the short
	// form alone since the combined line regularly overruns keybar width.
	joinPrefix := func(r *metaRow) string {
		if r.joinService == "" {
			if r.joinUnknown != "" {
				return warnLabel.Render("service joins unknown (" + r.joinUnknown + ") · ")
			}
			return ""
		}
		return warnLabel.Render(fmt.Sprintf("detaches %d pods from svc/%s · ", r.joinPodCount, r.joinService))
	}

	left := label.Render("will run") + fill.Render(" ")
	right := ""
	switch {
	case t.lastError != "":
		// docs/design README.md §26a: "remain in edit mode with the
		// attempted value intact and show the server error" — takes over
		// the whole strip rather than sharing it with the command preview,
		// since the error is the more decision-relevant fact right now.
		left = label.Render("error") + fill.Render(" ") + fill.Foreground(theme.Bad).Render(t.lastError)
	case t.message != "":
		// The just-applied result ("updated env=staging" / "removed
		// kute.dev/owner") — cleared the moment the user moves on in
		// navigation mode (Update), so it only ever answers "what
		// just happened."
		left = fill.Foreground(theme.Good).Render(t.message)
	case t.adding != metaAddNone:
		key := strings.TrimSpace(t.addKeyInput.Value())
		if key == "" {
			left += cmd.Render("type a key to add a " + addKindLabel(t.adding))
		} else {
			isAnnotation := t.adding == metaAddAnnotation
			overwrite := metaKeyExists(t, isAnnotation, key)
			left += cmd.Render(kube.MetaCommandString(t.kind, t.namespace, t.name, isAnnotation, key, t.addValueInput.Value(), false, overwrite))
			right = rightNote.Render("metadata only — no rollout")
		}
	default:
		r := t.selectedRow()
		// A pending removal's row never diverges from current (there's no
		// buffer edit to show), so it needs its own branch ahead of the
		// generic "!r.changed()" — otherwise a removal awaiting the y/N
		// would misleadingly read "no changes" while a delete is in flight.
		removing := false
		if pending := ctrl.Pending(); ctrl.Active() && pending != nil {
			removing = pending.Scope.MetaRemove
		}
		switch {
		case r == nil:
			left += cmd.Render("no changes — ↵ has nothing to apply")
		case r.readOnly:
			left += cmd.Render(r.readOnlyNote)
		case removing:
			left += joinPrefix(r) + cmd.Render(kube.MetaCommandString(t.kind, t.namespace, t.name, r.isAnnotation, r.key, "", true, false))
			right = rightNote.Render("metadata only — no rollout")
		case !textfield.RoundTrips(r.current):
			left += cmd.Render("multi-line value — ↵ can't edit it in place · E edits the whole object")
		case !r.changed():
			left += cmd.Render("no changes — ↵ has nothing to apply")
		default:
			left += joinPrefix(r) + cmd.Render(kube.MetaCommandString(t.kind, t.namespace, t.name, r.isAnnotation, r.key, r.input.Value(), false, true))
			right = rightNote.Render("metadata only — no rollout")
		}
	}

	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", width))
	return rule + "\n" + insetStripLineFill(padBetweenFill(left, right, stripInnerWidth(width), fill), width, fill)
}

func addKindLabel(k metaAddKind) string {
	if k == metaAddAnnotation {
		return "annotation"
	}
	return "label"
}

// The four layout helpers below are private copies of browse's own
// (setImageInset, setImagePanelBorderStyle, insetStripLineFill,
// padBetweenFill, stripInnerWidth) — task packages can't import one another,
// and these few pure lines are the accepted duplication cost (the jobattempts
// precedent) that keeps browse's other panels untouched.

func panelInset(line string, contentWidth int) string {
	return " " + components.Pad(line, contentWidth) + " "
}

func panelBorderStyle(theme tui.Theme) lipgloss.Style {
	return lipgloss.NewStyle().BorderForeground(theme.BorderPalette).Background(theme.BgPalette)
}

func stripInnerWidth(width int) int {
	return max(width-2*tui.FrameInset, 0)
}

func insetStripLineFill(line string, width int, fill lipgloss.Style) string {
	content := fill.Render(strings.Repeat(" ", tui.FrameInset)) + line
	slack := width - lipgloss.Width(content)
	if slack <= 0 {
		return content
	}
	return content + fill.Render(strings.Repeat(" ", slack))
}

func padBetweenFill(left, right string, width int, fill lipgloss.Style) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + fill.Render(strings.Repeat(" ", gap)) + right
}
