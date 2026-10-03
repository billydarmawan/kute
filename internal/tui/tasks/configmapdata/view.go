package configmapdata

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

func (m Model) View() tea.View { return tea.NewView(m.Render()) }

func (m Model) Render() string { return tui.Frame(m.width, m.height, m) }

func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

// Header is 27a's breadcrumb: "… › <namespace> › cm/<name> › Data" — the
// same trailing-segment shape secretdata's own "secret/<name> › Data" uses.
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
		tui.Crumb{Text: "cm/" + m.name, Style: secondary},
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

// Strips renders two lines: the key count (with a live → N preview while an
// add/remove is in flight, matching 26a/27b's own precedent), and 27a's own
// consumer strip — "deploy/nva-worker ↗ env · deploy/nva-gateway ↗ volume"
// left, "pods don't reload configmaps on their own" right (docs/design
// README.md §27a).
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
	label := fmt.Sprintf("%d %s", len(m.keys), word)
	switch {
	case m.adding != nil || (m.pendingCommit != nil && !m.pendingCommit.remove && !m.pendingCommit.isEdit && m.actions.Active()):
		label = fmt.Sprintf("%d %s → %d", len(m.keys), word, len(m.keys)+1)
	case m.pendingCommit != nil && m.pendingCommit.remove && m.actions.Active():
		label = fmt.Sprintf("%d %s → %d", len(m.keys), word, len(m.keys)-1)
	}
	countLine := insetStripLine(padBetween(count.Render(label), "", stripInnerWidth(width)), width)

	consumersLeft := dim.Render("no consumers found")
	if len(m.consumers)+len(m.fixedConsumers) > 0 {
		parts := make([]string, 0, len(m.consumers)+len(m.fixedConsumers))
		for _, c := range m.consumers {
			parts = append(parts, consumerRefText(theme, c))
		}
		for i, c := range m.fixedConsumers {
			// One marker ahead of the group, so a truncated strip still
			// says ctrl-r leaves these alone.
			ref := fixedConsumerRefText(theme, c)
			if i == 0 {
				ref = dim.Render("not restartable: ") + ref
			}
			parts = append(parts, ref)
		}
		consumersLeft = strings.Join(parts, dim.Render(" · "))
	}
	consumersRight := dim.Render("pods don't reload configmaps on their own")
	consumersLine := insetStripLine(padBetween(consumersLeft, consumersRight, stripInnerWidth(width)), width)

	return []string{countLine, consumersLine}
}

// consumerRefText renders one consumer as "deploy/nva-worker ↗ env" — the
// same short workload-arg forms (deploy/sts/ds) kubectl-command rendering
// uses elsewhere (kube.ConfigMapConsumerRestartCommandString), so the strip
// and the will-run restart lines name workloads identically.
func consumerRefText(theme tui.Theme, c configMapConsumer) string {
	name := lipgloss.NewStyle().Foreground(theme.TextPrimary).Render(shortWorkloadArg(c.Kind) + "/" + c.Name)
	arrow := lipgloss.NewStyle().Foreground(theme.TextFaint).Render(" ↗ " + c.refKind)
	return name + arrow
}

// fixedConsumerRefText renders a Job/CronJob/bare-Pod consumer all faint,
// "job/migrate ↗ volume" — the strip groups these after a "not
// restartable:" marker, since ctrl-r leaves them alone.
func fixedConsumerRefText(theme tui.Theme, c configMapConsumer) string {
	return lipgloss.NewStyle().Foreground(theme.TextFaint).Render(shortWorkloadArg(c.Kind) + "/" + c.Name + " ↗ " + c.refKind)
}

func shortWorkloadArg(kind kube.ResourceKind) string {
	switch kind {
	case kube.KindCronJob:
		return "cj"
	case kube.KindJob:
		return "job"
	case kube.KindPod:
		return "pod"
	case kube.KindStatefulSet:
		return "sts"
	case kube.KindDaemonSet:
		return "ds"
	default:
		return "deploy"
	}
}

func (m Model) Body(width, height int) string {
	switch m.state {
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	case tui.TaskStateReady:
		if m.actions.TypeToConfirm() {
			return m.restartConfirmModal(width, height)
		}
		if m.multiline != nil {
			return m.multilineBody(m.Theme(), width, height)
		}
		return m.gridBody(m.Theme(), width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// gridBody renders the KEY/VALUE/SIZE grid, the add row (or its hint), and
// the will-run strip — mirrors secretdata's own gridBody.
func (m Model) gridBody(theme tui.Theme, width, height int) string {
	var lines []string
	lines = append(lines, m.columnHeaderLine(theme, width))
	for i := range m.keys {
		lines = append(lines, m.configMapRowLine(theme, i, width))
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
	return configMapRowColumns("", faint.Render("KEY"), faint.Render("VALUE"), faint.Render("SIZE"), width, lipgloss.NewStyle())
}

// configMapRowLine renders one existing key's row — the real value (no
// masking) normally, a folded "▸ N lines · e opens the buffer editor"
// summary for a multi-line value, or the in-place edit buffer/pending-
// confirm note for whichever row is currently in flight.
func (m Model) configMapRowLine(theme tui.Theme, idx int, width int) string {
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
	valueStyle := fill.Foreground(theme.TextSecondary)
	dimStyle := fill.Foreground(theme.TextFaint)
	sizeStyle := fill.Foreground(theme.TextDim)

	var value string
	switch {
	case isEditingNow:
		_, valueWidth, _ := configMapColumnWidths(width)
		value = m.editValueCell(theme, fill, valueWidth)
	case isPendingEdit:
		value = valueStyle.Render(components.Truncate(oneLine(r.value), 24)) + fill.Render("  ") + fill.Foreground(theme.Warn).Render(m.confirmNote("update"))
	case isPendingRemove:
		value = valueStyle.Render(components.Truncate(oneLine(r.value), 24)) + fill.Render("  ") + fill.Foreground(theme.Bad).Render("remove · y/N")
	case r.binary:
		value = dimStyle.Render("binary · read-only")
	case r.multiline():
		lineCount := strings.Count(r.value, "\n") + 1
		value = dimStyle.Render(fmt.Sprintf("▸ %d lines · e opens the buffer editor", lineCount))
	default:
		value = valueStyle.Render(r.value)
	}
	size := sizeStyle.Render(formatByteSize(r.size))
	return configMapRowColumns(marker, keyStyle.Render(r.key), value, size, width, fill)
}

// editValueCell renders '↵'s single-line in-place edit buffer — "was
// <original> · <live buffer>" per docs/design README.md §27a ("prior value
// stays visible as `was info ·` while typing").
func (m Model) editValueCell(theme tui.Theme, fill lipgloss.Style, width int) string {
	e := m.editing
	dim := fill.Foreground(theme.TextDim)
	was := dim.Render("was " + components.Truncate(oneLine(e.original), 20) + " · ")
	return was + e.valueInput.ViewWidth(max(width-lipgloss.Width(was), 1))
}

// pendingEditConfirm reports whether an existing key's PROD y/N is currently
// showing, and which key it targets — mirrors pendingRemove.
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

// pendingRemove reports whether a removal's inline y/N is currently showing,
// and which key it targets.
func (m Model) pendingRemove() (bool, string) {
	if !m.actions.Active() {
		return false, ""
	}
	p := m.actions.Pending()
	if p == nil || !p.Scope.ConfigMapRemove {
		return false, ""
	}
	return true, p.Scope.ConfigMapKey
}

// addRowLine renders 'a'/insert's line-insert row: a highlighted "+" marker,
// the key buffer, then the value buffer — always visible while typing, no
// mask toggle (a ConfigMap value is never sensitive).
func (m Model) addRowLine(theme tui.Theme, width int) string {
	a := m.adding
	fill := lipgloss.NewStyle().Background(theme.SelBg)
	dim := fill.Foreground(theme.TextDim)
	good := fill.Foreground(theme.Good)

	marker := good.Render("+") + fill.Render(" ")
	keyWidth, valueWidth, _ := configMapColumnWidths(width)
	key := addBufferCell(a.keyInput, dim, keyWidth)
	value := addBufferCell(a.valueInput, dim, valueWidth)
	size := dim.Render("new")
	return configMapRowColumns(marker, key, value, size, width, fill)
}

// addBufferCell renders one of the add/edit row's buffers via its own View
// (which embeds the cursor only while Focused) — mirrors secretdata's own
// secretBufferCell. The one case View() doesn't already cover is an
// unfocused, empty buffer, which old code showed as a dim "…" rather than
// nothing.
func addBufferCell(input textfield.Model, dim lipgloss.Style, width int) string {
	if !input.Focused() && input.Value() == "" {
		return dim.Render("…")
	}
	return input.ViewWidth(width)
}

// pendingAddRowLine renders the in-flight add row while its own PROD y/N
// confirm is showing (m.adding is already nil by then).
func (m Model) pendingAddRowLine(theme tui.Theme, width int, pc *configMapPendingCommit) string {
	fill := lipgloss.NewStyle().Background(theme.SelBg)
	good := fill.Foreground(theme.Good)
	bold := fill.Foreground(theme.Text).Bold(true)
	warn := fill.Foreground(theme.Warn)
	dim := fill.Foreground(theme.TextDim)

	marker := good.Render("+") + fill.Render(" ")
	value := bold.Render(components.Truncate(oneLine(pc.value), 24)) + fill.Render("  ") + warn.Render(m.confirmNote("add"))
	return configMapRowColumns(marker, bold.Render(pc.key), value, dim.Render("new"), width, fill)
}

// confirmNote is the in-flight row's inline y/N note — naming the restarts
// too when the pending commit is ctrl-r's apply + restart.
func (m Model) confirmNote(verb string) string {
	if pc := m.pendingCommit; pc != nil && pc.restartConsumers && len(pc.consumers) > 0 {
		return "confirm to " + verb + " + restart " + restartScope(len(pc.consumers), len(pc.skipped)) + " · y/N"
	}
	return "confirm to " + verb + " · y/N"
}

// restartConfirmModal is ctrl-r's PROD confirm: the same type-the-name modal
// (and the same red border, reserved for destructive confirms) a single
// rollout restart gets, against the ConfigMap's own name. The consumers it
// restarts are listed, and the will-run lines are every command it runs —
// the patch, then one rollout restart per consumer.
func (m Model) restartConfirmModal(width, height int) string {
	theme := m.Theme()
	title, target, ownerLine := "Confirm", m.name, ""
	var detail []string
	if pending := m.actions.Pending(); pending != nil {
		title = "✕ " + pending.Label
		target = pending.Scope.ResourceName
	}
	if pc := m.pendingCommit; pc != nil {
		names := make([]string, len(pc.consumers))
		for i, c := range pc.consumers {
			names[i] = shortWorkloadArg(c.Kind) + "/" + c.Name
		}
		if len(names) > 0 {
			ownerLine = "restarts " + strings.Join(names, " · ")
		}
		if len(pc.skipped) > 0 {
			skipped := make([]string, len(pc.skipped))
			for i, c := range pc.skipped {
				skipped[i] = shortWorkloadArg(c.Kind) + "/" + c.Name
			}
			detail = append(detail, "not restartable, left alone: "+strings.Join(skipped, " · "))
		}
		detail = append(detail, "will run: "+m.commandForKey(pc.key))
		for _, c := range pc.consumers {
			detail = append(detail, "          "+kube.ConfigMapConsumerRestartCommandString(m.namespace, c.ConfigMapConsumerRef))
		}
	}
	styles := components.TypeModalStyles{
		Border:    lipgloss.NewStyle().BorderForeground(theme.ConfirmBorder).Background(theme.ConfirmHeaderBg),
		Title:     lipgloss.NewStyle().Foreground(theme.Bad).Bold(true).Background(theme.ConfirmHeaderBg),
		ProdTag:   lipgloss.NewStyle().Foreground(theme.ProdText).Bold(true).Background(theme.ConfirmHeaderBg),
		Owner:     lipgloss.NewStyle().Foreground(theme.Good).Background(theme.ConfirmHeaderBg),
		Detail:    lipgloss.NewStyle().Foreground(theme.TextSecondary).Background(theme.ConfirmHeaderBg),
		Rule:      lipgloss.NewStyle().Foreground(theme.TextGhost).Background(theme.ConfirmHeaderBg),
		Input:     lipgloss.NewStyle().Foreground(theme.Text).Background(theme.ConfirmHeaderBg),
		Selection: lipgloss.NewStyle().Foreground(theme.Bg).Background(theme.Accent),
		Progress:  lipgloss.NewStyle().Foreground(theme.TextFaint).Background(theme.ConfirmHeaderBg),
		Key:       lipgloss.NewStyle().Foreground(theme.Bad).Background(theme.ConfirmHeaderBg),
		Label:     lipgloss.NewStyle().Foreground(theme.TextDim).Background(theme.ConfirmHeaderBg),
	}
	return components.TypeNameModal(title, ownerLine, strings.Join(detail, "\n"), target, m.actions.TypedInput(), "apply + restart", m.isProd(), styles, width, height)
}

// multilineBody renders the buffer editor full-screen — the "simpler
// solution" this package substitutes for 17a's own shared buffer editor: a
// key header, a scrollable window of numbered lines with the cursor glyph on
// the active row/column, and the will-run strip at the bottom.
func (m Model) multilineBody(theme tui.Theme, width, height int) string {
	e := m.multiline
	headerStyle := lipgloss.NewStyle().Foreground(theme.Accent).Bold(true)
	hintStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	header := headerStyle.Render(e.key) + hintStyle.Render("  "+verbs.ConfigMapBufferApply.Key+" apply · "+verbs.RestartConfigMapConsumers.Key+" apply + restart consumers · esc discard")

	willRun := m.willRunStrip(theme, width)
	budget := height - 2 // header + blank line
	if willRun != "" {
		// blank line + however many lines willRunStrip actually returned
		// (the rule + the primary line, plus one more per restart
		// consumer) — e.textarea.SetHeight below always renders exactly
		// this many rows (padding short content with blank lines itself,
		// unlike the hand-rolled loop it replaced, which only emitted as
		// many rows as the buffer actually had), so under-reserving here
		// pushes willRun past the bottom of the frame instead of just
		// leaving trailing blank space.
		budget -= 1 + strings.Count(willRun, "\n") + 1
	}
	budget = max(budget, 1)

	// e.textarea owns its own soft-wrap/scrolling/line-number gutter now —
	// no more hand-rolled scrollWindow or "… N of M lines shown" hint (the
	// component doesn't expose an equivalent "currently scrolled" signal to
	// reproduce it, and its own scrollbar-less viewport is the accepted
	// trade for adopting a real, tested textarea). Sized to the buffer's
	// actual line count (capped at budget so a long buffer still scrolls,
	// not the full budget outright) — SetHeight(budget) would instead pad
	// every short buffer out to budget with blank rows *inside* the
	// textarea's own View(), pushing the will-run strip below them instead
	// of directly under the real content.
	e.textarea.SetWidth(width)
	e.textarea.SetHeight(max(min(e.textarea.LineCount(), budget), 1))

	lines := []string{header, "", e.textarea.View()}
	if willRun != "" {
		lines = append(lines, "", willRun)
	}
	return components.Pad(strings.Join(lines, "\n"), width)
}

// overwriteWarning is the add row's "overwrites existing KEY · " prefix when
// key is already in the ConfigMap — "" otherwise.
func (m Model) overwriteWarning(theme tui.Theme, key string) string {
	if _, exists := indexOfConfigMapKey(m.keys, key); !exists {
		return ""
	}
	return lipgloss.NewStyle().Background(theme.BgStrip).Foreground(theme.Warn).Render("overwrites existing " + key + " · ")
}

// willRunStrip is the screen's own "will run" line(s), styled like
// secretdata's own metaWillRunStrip-derived band: a BorderSubtle top rule,
// then a BgStrip-filled band showing the exact command(s) that would run.
// For a ctrl-r commit this is multiple lines — the patch, then one `kubectl
// rollout restart` per consumer (docs/design README.md §27a: "prints every
// command it runs"). Returns "" when there's nothing to preview.
func (m Model) willRunStrip(theme tui.Theme, width int) string {
	fill := lipgloss.NewStyle().Background(theme.BgStrip)
	label := fill.Foreground(theme.TextDim)
	cmd := fill.Foreground(theme.TextSecondary)

	var primary string
	restart := false
	hasContent := true
	removing, removeKey := m.pendingRemove()
	editConfirming, editConfirmKey := m.pendingEditConfirm()
	switch {
	case m.lastError != "":
		primary = label.Render("error") + fill.Render(" ") + fill.Foreground(theme.Bad).Render(m.lastError)
	case m.message != "":
		primary = fill.Foreground(theme.Good).Render(m.message)
	case m.notice != "":
		primary = fill.Foreground(theme.Warn).Render(m.notice)
	case removing:
		primary = label.Render("will run") + fill.Render(" ") + cmd.Render(kube.ConfigMapDataCommandString(m.namespace, m.name, removeKey, "", true))
	case editConfirming:
		primary = label.Render("will run") + fill.Render(" ") + cmd.Render(m.commandForKey(editConfirmKey))
		restart = m.pendingCommit != nil && m.pendingCommit.restartConsumers
	case m.adding != nil:
		key := strings.TrimSpace(m.adding.keyInput.Value())
		switch {
		case key == "":
			primary = label.Render("will run") + fill.Render(" ") + cmd.Render("type a key to add")
		case m.binaryKey(key):
			primary = fill.Foreground(theme.Warn).Render(key + " is already a binaryData key · pick another name")
		default:
			primary = label.Render("will run") + fill.Render(" ") + m.overwriteWarning(theme, key) + cmd.Render(kube.ConfigMapDataCommandString(m.namespace, m.name, key, m.adding.valueInput.Value(), false))
		}
	case m.pendingCommit != nil && !m.pendingCommit.remove && !m.pendingCommit.isEdit:
		primary = label.Render("will run") + fill.Render(" ") + m.overwriteWarning(theme, m.pendingCommit.key) + cmd.Render(kube.ConfigMapDataCommandString(m.namespace, m.name, m.pendingCommit.key, m.pendingCommit.value, false))
		restart = m.pendingCommit.restartConsumers
	case m.editing != nil:
		if !m.editing.changed() {
			primary = label.Render("will run") + fill.Render(" ") + cmd.Render("no changes — ↵ has nothing to apply")
		} else {
			primary = label.Render("will run") + fill.Render(" ") + cmd.Render(kube.ConfigMapDataCommandString(m.namespace, m.name, m.editing.key, m.editing.valueInput.Value(), false))
		}
	case m.multiline != nil:
		if !m.multiline.changed() {
			primary = label.Render("will run") + fill.Render(" ") + cmd.Render("no changes — "+verbs.ConfigMapBufferApply.Key+" has nothing to apply")
		} else {
			primary = label.Render("will run") + fill.Render(" ") + cmd.Render(m.commandForKey(m.multiline.key))
		}
	default:
		hasContent = false
	}
	if !hasContent {
		return ""
	}

	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", width))
	linesOut := []string{rule, insetStripLineFill(padBetweenFill(primary, "", stripInnerWidth(width), fill), width, fill)}
	if restart {
		for _, c := range m.consumers {
			restartLine := label.Render("       ") + cmd.Render(kube.ConfigMapConsumerRestartCommandString(m.namespace, c.ConfigMapConsumerRef))
			linesOut = append(linesOut, insetStripLineFill(padBetweenFill(restartLine, "", stripInnerWidth(width), fill), width, fill))
		}
	}
	return strings.Join(linesOut, "\n")
}

// commandForKey renders the will-run command for key's currently-buffered
// edit — a single-line value gets the real ConfigMapDataCommandString, a
// multi-line one elides the value to a line count rather than embedding the
// whole escaped buffer in the preview.
func (m Model) commandForKey(key string) string {
	if m.multiline != nil && m.multiline.key == key {
		lines := strings.Count(m.multiline.value(), "\n") + 1
		return fmt.Sprintf("kubectl patch cm/%s --type merge -p '{\"data\":{%q:\"<%d lines>\"}}' -n %s", m.name, key, lines, m.namespace)
	}
	if m.editing != nil && m.editing.key == key {
		return kube.ConfigMapDataCommandString(m.namespace, m.name, key, m.editing.valueInput.Value(), false)
	}
	if m.pendingCommit != nil && m.pendingCommit.key == key {
		if strings.Contains(m.pendingCommit.value, "\n") {
			lines := strings.Count(m.pendingCommit.value, "\n") + 1
			return fmt.Sprintf("kubectl patch cm/%s --type merge -p '{\"data\":{%q:\"<%d lines>\"}}' -n %s", m.name, key, lines, m.namespace)
		}
		return kube.ConfigMapDataCommandString(m.namespace, m.name, key, m.pendingCommit.value, false)
	}
	return kube.ConfigMapDataCommandString(m.namespace, m.name, key, "", false)
}

// oneLine collapses a value to its first line, for cells that must stay a
// single terminal row (an edit-in-flight/pending-confirm cell showing a
// multi-line value's original).
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "…"
	}
	return s
}

// configMapRowColumns lays marker/key/value/size out — mirrors secretdata's
// own secretRowColumns (widths sized for a terminal character grid, package-
// local per the repo's duplication-over-cross-package-coupling convention).
func configMapRowColumns(marker, key, value, size string, width int, fill lipgloss.Style) string {
	const markerWidth, colGap = 2, 2
	keyWidth, valueWidth, sizeWidth := configMapColumnWidths(width)

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

func configMapColumnWidths(width int) (keyWidth, valueWidth, sizeWidth int) {
	const markerWidth, colGap = 2, 2
	avail := max(width-markerWidth-colGap, 6)
	keyWidth = min(28, avail*30/100)
	sizeWidth = min(9, max(avail*8/100, 6))
	valueWidth = max(avail-keyWidth-sizeWidth-colGap, 1)
	return keyWidth, valueWidth, sizeWidth
}

// formatByteSize renders a value's byte length the same "N B"/"N.N KiB"
// style secretdata's own formatByteSize uses.
func formatByteSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}

// padBetween/stripInnerWidth/insetStripLine/insetStripLineFill/
// padBetweenFill mirror browse's/secretdata's own strip-line helpers
// (package-local since Go doesn't share unexported functions across
// packages).
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
