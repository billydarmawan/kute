package secretdata

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// secretDataMaskGlyph is 27b's fixed 16-dot placeholder for every existing
// key's VALUE cell (docs/design/v.0.2.0.dc.html's own §27b mockup literal)
// — length is not proportional to the real value's length, so it can never
// leak size-by-eye beyond the explicit SIZE column figure next to it.
const secretDataMaskGlyph = "••••••••••••••••"

func (m Model) View() tea.View { return tea.NewView(m.Render()) }

func (m Model) Render() string { return tui.Frame(m.width, m.height, m) }

func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

// Header is 27b's breadcrumb: "… › <namespace> › secret/<name> › Data" —
// the same trailing-segment shape 18a's "… › <release> › History" uses for
// a sub-view of an object.
func (m Model) Header() tui.HeaderState {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	ghost := lipgloss.NewStyle().Foreground(theme.TextGhost)
	ghost2 := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	secondary := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	text := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)

	ctxName := "cluster unavailable"
	if m.session != nil && m.session.Location.Context != "" {
		ctxName = m.session.Location.Context
	}

	crumbs := append(tui.BrandCrumbs(theme),
		tui.Crumb{Text: " │ ", Style: ghost2},
		tui.Crumb{Text: ctxName, Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: m.namespace, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "secret/" + m.name, Style: secondary},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "Data", Style: text},
	)

	var forwardChip tui.ConnBadge
	if m.session != nil {
		forwardChip = tui.BuildForwardChip(theme, m.session.ForwardSummary())
	}
	return tui.HeaderState{
		Crumbs:      crumbs,
		ConfigChip:  tui.BuildConfigChip(theme, m.session),
		UpdateChip:  tui.BuildUpdateChip(theme, m.session),
		ForwardChip: forwardChip,
		Conn:        tui.LiveConnBadge(theme, m.conn, tui.GlyphRunning+" connected"),
	}
}

// Strips is the mockup's second header bar: "Opaque · 3 keys[ → 4]" left,
// "values decode in memory only · re-masked on exit" right — the count
// preview (→ N) only shows while an add/remove is actively in flight
// (typing, or awaiting its own y/N), matching 26a's own LABELS·N/
// ANNOTATIONS·N live-count precedent.
func (m Model) Strips(width int) []string {
	if m.state != tui.TaskStateReady {
		return nil
	}
	theme := m.Theme()
	count := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	dim := lipgloss.NewStyle().Foreground(theme.TextFaint)

	word := "key"
	if len(m.keys) != 1 {
		word = "keys"
	}
	label := fmt.Sprintf("%s · %d %s", m.secretType, len(m.keys), word)
	switch {
	case m.adding != nil || (m.pendingCommit != nil && !m.pendingCommit.remove && m.actions.Active()):
		label = fmt.Sprintf("%s · %d %s → %d", m.secretType, len(m.keys), word, len(m.keys)+1)
	case m.pendingCommit != nil && m.pendingCommit.remove && m.actions.Active():
		label = fmt.Sprintf("%s · %d %s → %d", m.secretType, len(m.keys), word, len(m.keys)-1)
	}

	left := count.Render(label)
	right := dim.Render("values decode in memory only · re-masked on exit")
	line := padBetween(left, right, stripInnerWidth(width))
	return []string{insetStripLine(line, width)}
}

func (m Model) Body(width, height int) string {
	switch m.state {
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	case tui.TaskStateReady:
		return m.gridBody(m.Theme(), width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// gridBody renders the KEY/VALUE/SIZE grid, the add row (or its hint), and
// the will-run strip — the same "table lines + a trailing will-run band,
// all inside Body's own budgeted height" idiom 26a's meta.go setMetaBody
// uses, minus meta.go's outer bordered panel: this screen has no live table
// underneath to float over (it's a full pushed screen, not a browse-
// embedded overlay), so the grid renders directly. The will-run band is
// omitted entirely (no rule, no line) when there's genuinely nothing to
// preview — a plain idle navigation state — rather than a static "no
// changes" placeholder line.
func (m Model) gridBody(theme tui.Theme, width, height int) string {
	var lines []string
	lines = append(lines, m.columnHeaderLine(theme, width))
	for i := range m.keys {
		lines = append(lines, m.secretRowLine(theme, i, width))
	}
	switch {
	case m.adding != nil:
		lines = append(lines, m.addRowLine(theme, width))
	case m.pendingCommit != nil && !m.pendingCommit.remove && !m.pendingCommit.isEdit && m.actions.Active():
		lines = append(lines, m.pendingAddRowLine(theme, width, m.pendingCommit))
	default:
		hint := lipgloss.NewStyle().Foreground(theme.TextGhost).Render("+ a add key")
		lines = append(lines, hint)
	}
	if len(m.keys) == 0 && m.adding == nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(theme.TextGhost).Render("no keys"))
	}
	if strip := m.willRunStrip(theme, width); strip != "" {
		lines = append(lines, "", strip)
	}
	return components.Pad(strings.Join(lines, "\n"), width)
}

func (m Model) columnHeaderLine(theme tui.Theme, width int) string {
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	return secretRowColumns("", faint.Render("KEY"), faint.Render("VALUE"), faint.Render("SIZE"), width, lipgloss.NewStyle())
}

// secretRowLine renders one existing key's row: the mask glyph normally
// stands in for VALUE, except the row currently open for editing (real
// decoded value, live buffer) or the one a pending removal/edit confirm is
// deciding (an inline "remove · y/N"/"confirm to update · y/N" note).
func (m Model) secretRowLine(theme tui.Theme, idx int, width int) string {
	r := m.keys[idx]
	removing, removeKey := m.pendingRemove()
	editConfirming, editConfirmKey := m.pendingEditConfirm()
	isPendingRemove := removing && removeKey == r.key
	isPendingEdit := editConfirming && editConfirmKey == r.key
	isEditingNow := m.editing != nil && m.editing.key == r.key
	selected := m.adding == nil && m.editing == nil && m.selected == idx

	fill := lipgloss.NewStyle()
	if selected || isEditingNow {
		fill = fill.Background(theme.SelBg)
	}
	marker := fill.Render("  ")
	if selected || isEditingNow {
		marker = lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg).Render("›") + fill.Render(" ")
	}
	keyStyle := fill.Foreground(theme.Text)
	maskStyle := fill.Foreground(theme.TextFaint)
	sizeStyle := fill.Foreground(theme.TextDim)

	var value string
	switch {
	case isEditingNow:
		_, valueWidth, _ := secretColumnWidths(width)
		value = m.editValueCell(theme, valueWidth)
	case isPendingEdit:
		value = maskStyle.Render(secretDataMaskGlyph) + fill.Render("  ") + fill.Foreground(theme.Warn).Render("confirm to update · y/N")
	case isPendingRemove:
		value = maskStyle.Render(secretDataMaskGlyph) + fill.Render("  ") + fill.Foreground(theme.Bad).Render("remove · y/N")
	default:
		value = maskStyle.Render(secretDataMaskGlyph)
	}
	size := sizeStyle.Render(formatByteSize(r.size))
	return secretRowColumns(marker, keyStyle.Render(r.key), value, size, width, fill)
}

// editValueCell renders '↵'s decode-then-edit buffer in place of an
// existing row's mask — visible while typing by default (masked once
// ctrl-x toggles it), the same shape addBufferCell already gives the add
// row's own value entry, since editing a key needs to actually show what's
// being changed (the user's own choice: "decode-then-edit", not a blind
// rewrite).
func (m Model) editValueCell(theme tui.Theme, width int) string {
	e := m.editing
	accent := lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg)
	bold := lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Background(theme.SelBg)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim).Background(theme.SelBg)
	if e.masked {
		return bold.Render(secretDataMaskGlyph) + accent.Render(tui.GlyphSelBar) + dim.Render(" · masked · M reveal")
	}
	return e.valueInput.ViewWidth(width) + dim.Render(" · visible while editing · M re-mask")
}

// pendingEditConfirm reports whether an existing key's PROD y/N is
// currently showing, and which key it targets — mirrors pendingRemove.
func (m Model) pendingEditConfirm() (bool, string) {
	if !m.actions.Active() {
		return false, ""
	}
	pc := m.pendingCommit
	if pc == nil || pc.remove || !pc.isEdit {
		return false, ""
	}
	return true, pc.key
}

// addRowLine renders 'a'/insert's line-insert row: a highlighted "+"
// marker, the key buffer, then the value buffer — visible while typing by
// default, masked once ctrl-x toggles it (docs/design README.md §27b:
// "ctrl-x re-mask input").
func (m Model) addRowLine(theme tui.Theme, width int) string {
	a := m.adding
	fill := lipgloss.NewStyle().Background(theme.SelBg)
	accent := lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg)
	bold := lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Background(theme.SelBg)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim).Background(theme.SelBg)
	good := lipgloss.NewStyle().Foreground(theme.Good).Background(theme.SelBg)

	marker := good.Render("+") + fill.Render(" ")
	keyWidth, valueWidth, _ := secretColumnWidths(width)
	key := secretBufferCell(a.keyInput, dim, keyWidth)

	var valueCell string
	var note string
	switch {
	case a.masked:
		valueCell = bold.Render(secretDataMaskGlyph)
		if a.onValue {
			valueCell += accent.Render(tui.GlyphSelBar)
		}
		note = dim.Render(" · masked · ctrl-x reveal")
	default:
		valueCell = secretBufferCell(a.valueInput, dim, valueWidth)
		note = dim.Render(" · visible while typing · ctrl-x re-mask")
	}
	size := dim.Render("new")
	return secretRowColumns(marker, key, valueCell+note, size, width, fill)
}

// secretBufferCell renders one of the add row's two buffers via its own
// View (which embeds the cursor only while Focused) — mirrors meta.go's
// metaAddBufferCell. The one case View() doesn't already cover is an
// unfocused, empty buffer, which old code showed as a dim "…" rather than
// nothing.
func secretBufferCell(input textfield.Model, dim lipgloss.Style, width int) string {
	if !input.Focused() && input.Value() == "" {
		return dim.Render("…")
	}
	return input.ViewWidth(width)
}

// pendingAddRowLine renders the in-flight add row while its own PROD y/N
// confirm is showing (m.adding is already nil by then — commitAdd clears
// it before Begin, matching meta.go's own commit-time clearing) — the key
// is fixed (already committed to the pending action), the value stays
// masked, and the note reads "confirm to add · y/N" in place of the typing
// hints.
func (m Model) pendingAddRowLine(theme tui.Theme, width int, pc *secretPendingCommit) string {
	fill := lipgloss.NewStyle().Background(theme.SelBg)
	good := lipgloss.NewStyle().Foreground(theme.Good).Background(theme.SelBg)
	bold := lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Background(theme.SelBg)
	warn := lipgloss.NewStyle().Foreground(theme.Warn).Background(theme.SelBg)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim).Background(theme.SelBg)

	marker := good.Render("+") + fill.Render(" ")
	value := bold.Render(secretDataMaskGlyph) + fill.Render("  ") + warn.Render("confirm to add · y/N")
	return secretRowColumns(marker, bold.Render(pc.key), value, dim.Render("new"), width, fill)
}

// pendingRemove reports whether a removal's inline y/N is currently
// showing, and which key it targets.
func (m Model) pendingRemove() (bool, string) {
	if !m.actions.Active() {
		return false, ""
	}
	p := m.actions.Pending()
	if p == nil || !p.Scope.SecretRemove {
		return false, ""
	}
	return true, p.Scope.SecretKey
}

// overwriteWarning is the add row's "overwrites existing KEY · " prefix when
// key is already in the Secret — "" otherwise.
func (m Model) overwriteWarning(theme tui.Theme, key string) string {
	if _, exists := indexOfSecretKey(m.keys, key); !exists {
		return ""
	}
	return lipgloss.NewStyle().Background(theme.BgStrip).Foreground(theme.Warn).Render("overwrites existing " + key + " · ")
}

// willRunStrip is the screen's own "will run" line, styled like 26a's
// meta.go metaWillRunStrip: a BorderSubtle top rule, then a BgStrip-filled
// band showing the exact command that would run (add, edit, or remove), or
// the last inline result/error. Returns "" — no rule, no line at all —
// when there's genuinely nothing to preview (plain idle navigation, or an
// edit sitting unchanged from its original value): the band only earns its
// place on screen when there's something to say, not a static "no changes"
// placeholder.
func (m Model) willRunStrip(theme tui.Theme, width int) string {
	fill := lipgloss.NewStyle().Background(theme.BgStrip)
	label := fill.Foreground(theme.TextDim)
	cmd := fill.Foreground(theme.TextSecondary)
	rightNote := fill.Foreground(theme.TextDim)

	left := label.Render("will run") + fill.Render(" ")
	right := ""
	hasContent := true
	removing, removeKey := m.pendingRemove()
	editConfirming, editConfirmKey := m.pendingEditConfirm()
	switch {
	case m.lastError != "":
		// docs/design README.md §27b: "leaves the input row's typed value
		// exactly as entered ... with the server's error shown" — the error
		// takes over the whole strip, same priority meta.go's own gives it.
		left = label.Render("error") + fill.Render(" ") + fill.Foreground(theme.Bad).Render(m.lastError)
	case m.message != "":
		left = fill.Foreground(theme.Good).Render(m.message)
	case m.notice != "":
		left = fill.Foreground(theme.Warn).Render(m.notice)
	case removing:
		left += cmd.Render(kube.SecretDataCommandString(m.namespace, m.name, removeKey, true))
	case editConfirming:
		left += cmd.Render(kube.SecretDataCommandString(m.namespace, m.name, editConfirmKey, false))
		right = rightNote.Render("value masked here · sent verbatim")
	case m.adding != nil:
		key := strings.TrimSpace(m.adding.keyInput.Value())
		if key == "" {
			left += cmd.Render("type a key to add")
		} else {
			left += m.overwriteWarning(theme, key) + cmd.Render(kube.SecretDataCommandString(m.namespace, m.name, key, false))
			right = rightNote.Render("value masked here · sent verbatim")
		}
	case m.pendingCommit != nil && !m.pendingCommit.remove && !m.pendingCommit.isEdit:
		// The add row's own confirm in flight (PROD, or an overwrite) —
		// m.adding is already nil by then (commitAdd clears it before Begin).
		left += m.overwriteWarning(theme, m.pendingCommit.key) + cmd.Render(kube.SecretDataCommandString(m.namespace, m.name, m.pendingCommit.key, false))
		right = rightNote.Render("value masked here · sent verbatim")
	case m.editing != nil:
		if !m.editing.changed() {
			left += cmd.Render("no changes — ↵ has nothing to apply")
		} else {
			left += cmd.Render(kube.SecretDataCommandString(m.namespace, m.name, m.editing.key, false))
			right = rightNote.Render("value masked here · sent verbatim")
		}
	default:
		hasContent = false
	}
	if !hasContent {
		return ""
	}

	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", width))
	return rule + "\n" + insetStripLineFill(padBetweenFill(left, right, stripInnerWidth(width), fill), width, fill)
}

// secretRowColumns lays marker/key/value/size out (marker fixed 2ch, key a
// share of the available width, size a small fixed right-aligned share,
// value taking the remainder) — the same fill-aware-padding idiom 26a's
// meta.go metaRowColumns uses, duplicated per the repo's package-local-seam
// convention (widths sized for a terminal character grid, not a literal
// copy of the mockup's 20/200/1fr/90px CSS columns — docs/design README.md's
// Fidelity section: "exact pixel sizes are approximations of a character
// grid").
func secretRowColumns(marker, key, value, size string, width int, fill lipgloss.Style) string {
	const markerWidth, colGap = 2, 2
	keyWidth, valueWidth, sizeWidth := secretColumnWidths(width)

	padLeft := func(s string, w int) string {
		s = components.Truncate(s, w)
		gap := w - lipgloss.Width(s)
		if gap < 0 {
			gap = 0
		}
		return s + fill.Render(strings.Repeat(" ", gap))
	}
	padRight := func(s string, w int) string {
		s = components.Truncate(s, w)
		gap := w - lipgloss.Width(s)
		if gap < 0 {
			gap = 0
		}
		return fill.Render(strings.Repeat(" ", gap)) + s
	}

	return padLeft(marker, markerWidth) + padLeft(key, keyWidth) + padLeft(value, valueWidth) +
		fill.Render(strings.Repeat(" ", colGap)) + padRight(size, sizeWidth)
}

func secretColumnWidths(width int) (keyWidth, valueWidth, sizeWidth int) {
	const markerWidth, colGap = 2, 2
	avail := max(width-markerWidth-colGap, 6)
	keyWidth = min(28, avail*30/100)
	sizeWidth = min(9, max(avail*8/100, 6))
	valueWidth = max(avail-keyWidth-sizeWidth-colGap, 1)
	return keyWidth, valueWidth, sizeWidth
}

// formatByteSize renders a decoded value's byte length the same "N B"/"N.N
// KiB" style docs/design README.md §27a's own mockup uses (yamlview's 21a
// reveal placeholder does the equivalent "· base64 · N B" for its own
// decoded byte count).
func formatByteSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}

// padBetween/stripInnerWidth/insetStripLine/insetStripLineFill/
// padBetweenFill mirror browse's own strip-line helpers (package-local
// since Go doesn't share unexported functions across packages — the same
// duplication routetable/helmhistory's own view.go files already make).
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func padBetweenFill(left, right string, width int, fill lipgloss.Style) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + fill.Render(strings.Repeat(" ", gap)) + right
}

func stripInnerWidth(width int) int {
	return max(width-2*tui.FrameInset, 0)
}

func insetStripLine(line string, width int) string {
	return components.Pad(strings.Repeat(" ", tui.FrameInset)+line, width)
}

func insetStripLineFill(line string, width int, fill lipgloss.Style) string {
	content := fill.Render(strings.Repeat(" ", tui.FrameInset)) + line
	slack := width - lipgloss.Width(content)
	if slack <= 0 {
		return content
	}
	return content + fill.Render(strings.Repeat(" ", slack))
}
