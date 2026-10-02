package poddetail

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
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
	if m.namespace != "" {
		// mock 5a: "… › nva-stage › Pods › …" — the namespace segment matches
		// browse's own convention (TextPrimary, not Accent).
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: m.namespace, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
		)
	}
	crumbs = append(crumbs,
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "Pods", Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: m.name, Style: text},
	)

	return tui.HeaderState{
		Crumbs:      crumbs,
		ConfigChip:  tui.BuildConfigChip(theme, m.session),
		UpdateChip:  tui.BuildUpdateChip(theme, m.session),
		ForwardChip: tui.BuildForwardChip(theme, m.session.ForwardSummary()),
		Conn:        tui.LiveConnBadge(theme, m.conn, tui.GlyphRunning+" connected"),
	}
}

func (m Model) Strips(width int) []string { return nil }

func (m Model) Body(width, height int) string {
	if m.actions.Active() && m.actions.Tier() == actions.TierModal {
		return m.deleteConfirmModal(width, height)
	}
	if m.meta != nil {
		return m.metaBody(width, height)
	}
	switch m.state {
	case tui.TaskStateReady:
		if m.gone {
			return components.CenterLines([]string{
				lipgloss.NewStyle().Foreground(m.Theme().Bad).Bold(true).Render("Pod deleted"),
				lipgloss.NewStyle().Foreground(m.Theme().TextDim).Render(m.name + " no longer exists · press any key to go back"),
			}, width, height)
		}
		return m.readyBody(width, height)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// metaBody renders 26a's panel in place of the detail body while it's open —
// the pod's own title line stays frozen above it, the detail-screen analogue
// of browse's selected-row-above-the-panel framing (docs/design README.md
// §26a); the grid and will-run strip come from the shared metapanel renderer.
func (m Model) metaBody(width, height int) string {
	theme := m.Theme()
	lines := []string{m.titleLine(theme, width), ""}
	lines = append(lines, m.meta.PanelLines(width, &m.actions)...)
	lines = append(lines, "", m.meta.WillRunStrip(width, &m.actions))
	return components.Pad(strings.Join(lines, "\n"), width)
}

// readyBody stacks 5a's sections top to bottom: title row, an optional
// last-termination banner (promoted first, per docs/design README.md §5a —
// "answers why is it broken first, never bury it"), the meta grid, the
// CONTAINERS grid + CPU/MEM bars, the RELATED │ TOLERATIONS two-column row,
// then EVENTS — all full width (the old right sidebar is gone; labels and
// annotations live in 26a's 'm' metapanel now).
func (m Model) readyBody(width, height int) string {
	content := m.readyContent(width)
	lines := strings.Split(content, "\n")
	maxOffset := max(len(lines)-height, 0)
	offset := min(max(m.bodyOffset, 0), maxOffset)
	end := min(offset+height, len(lines))
	visible := append([]string(nil), lines[offset:end]...)
	for len(visible) < height {
		visible = append(visible, "")
	}
	return strings.Join(visible, "\n")
}

func (m Model) readyContent(width int) string {
	theme := m.Theme()

	var main []string
	main = append(main, m.titleLine(theme, width))
	if len(m.configErrors) > 0 {
		main = append(main, "", m.configErrorBanner(theme, width))
	}
	if m.pod.LastTermination != nil {
		main = append(main, "", m.terminationBanner(theme, width))
	}
	main = append(main, "", m.metaGrid(theme))
	main = append(main, "", m.containersBlock(theme, width))
	if len(m.pod.InitContainerInfos) > 0 {
		main = append(main, "", m.initContainersBlock(theme, width))
	}
	if len(m.pod.EphemeralContainerInfos) > 0 {
		main = append(main, "", m.ephemeralBlock(theme, width))
	}
	if m.showSources {
		main = append(main, "", m.sourcesBlock(theme, width))
	}
	main = append(main, "", m.relatedTolerationsBlock(theme, width))
	main = append(main, "", m.eventsBlock(theme, width))

	return strings.Join(main, "\n")
}

func (m Model) titleLine(theme tui.Theme, width int) string {
	name := lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Render(m.name)
	statusStyle := lipgloss.NewStyle().Foreground(statusColor(theme, m.status.class))
	status := statusStyle.Render(m.status.glyph + " " + m.status.text)
	restarts := ""
	if m.pod.Restarts > 0 {
		restarts = lipgloss.NewStyle().Foreground(theme.Warn).Render(fmt.Sprintf("%s %d restarts", tui.GlyphRestarts, m.pod.Restarts))
	}
	parts := []string{name, status}
	if restarts != "" {
		parts = append(parts, restarts)
	}
	left := strings.Join(parts, "  ")
	// mock 5a: "watching · live" sits at the right of the title row (the
	// header badge carries the real connection state).
	right := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("watching · live")
	return padBetween(left, right, width)
}

// padBetween right-aligns right against width, dropping it when the line is
// too tight — same shape as browse's own padBetween, duplicated per the
// repo's package-local convention.
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 || right == "" {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func statusColor(theme tui.Theme, class string) color.Color {
	switch class {
	case "ok":
		return theme.Good
	case "warn":
		return theme.Warn
	case "fail":
		return theme.Bad
	// "neutral" (statusClass's Completed pod) is the same "parked, benign,
	// nothing to see" hue the browse list's StatusNeutral already renders
	// (docs/design README.md; resources.go's own doc comment) — Info blue,
	// not the TextDim the unhandled default used to fall through to.
	case "neutral":
		return theme.Info
	default:
		return theme.TextDim
	}
}

func (m Model) terminationBanner(theme tui.Theme, width int) string {
	lt := m.pod.LastTermination
	if lt == nil {
		return ""
	}
	title := lipgloss.NewStyle().Foreground(theme.Bad).Background(theme.ErrBannerBg).Bold(true).Render("Last termination")
	// An undated termination (ContainerStatusUnknown carries no finishedAt)
	// drops the age segment rather than inventing one.
	factText := fmt.Sprintf("exit %d · %s", lt.ExitCode, lt.Reason)
	if lt.AgeKnown {
		factText += fmt.Sprintf(" · %s ago", shortDur(lt.Age))
	}
	facts := lipgloss.NewStyle().Foreground(theme.BadMuted).Background(theme.ErrBannerBg).Render(factText)

	// mock 5a's body line names the container and says what happened —
	// the OOMKilled wording only when the limit that was exceeded is known —
	// then the next backoff estimate (docs/design README.md §5a: "the
	// memory limit + next backoff").
	// Every inner style here must repeat Background(theme.ErrBannerBg): each
	// styled substring closes with its own ANSI reset, which wipes the outer
	// banner style's background for the gap until the next explicit one
	// (same reasoning as deleteConfirmModal's per-field Background pairing).
	bodyStyle := lipgloss.NewStyle().Foreground(theme.BadText).Background(theme.ErrBannerBg)
	container := lipgloss.NewStyle().Foreground(theme.Warn).Background(theme.ErrBannerBg).Render(lt.Container)
	what := fmt.Sprintf(" exited with code %d.", lt.ExitCode)
	if lt.Reason == "OOMKilled" && m.pod.MEMLimitBytes > 0 {
		what = " exceeded memory limit " + formatBytes(m.pod.MEMLimitBytes) + "."
	}
	if lt.BackingOff {
		what += fmt.Sprintf(" Next backoff ~%s.", shortDur(lt.NextBackoff()))
	}
	body := bodyStyle.Render("Container ") + container + bodyStyle.Render(what)

	gap := lipgloss.NewStyle().Background(theme.ErrBannerBg).Render("  ")
	return errBanner(theme, width, title+gap+facts+"\n"+body)
}

// configErrorBanner is the "why won't it start" banner for containers stuck
// in CreateContainerConfigError: the kubelet's own message (which names the
// missing Secret/ConfigMap and key) per container. It shares the termination
// banner's shape and sits above it, since it's the current reason and the
// termination is history.
func (m Model) configErrorBanner(theme tui.Theme, width int) string {
	title := lipgloss.NewStyle().Foreground(theme.Bad).Background(theme.ErrBannerBg).Bold(true).Render("Can't start container")
	facts := lipgloss.NewStyle().Foreground(theme.BadMuted).Background(theme.ErrBannerBg).Render("CreateContainerConfigError")
	gap := lipgloss.NewStyle().Background(theme.ErrBannerBg).Render("  ")
	bodyStyle := lipgloss.NewStyle().Foreground(theme.BadText).Background(theme.ErrBannerBg)
	nameStyle := lipgloss.NewStyle().Foreground(theme.Warn).Background(theme.ErrBannerBg)
	lines := []string{title + gap + facts}
	// Padding(1,1) plus the one-cell accent bar leave width-3 for text; a
	// long kubelet message wraps rather than being clipped.
	textWidth := max(width-3, 10)
	for _, e := range m.configErrors {
		msg := e.Message
		if msg == "" {
			msg = "a referenced Secret or ConfigMap is missing"
		}
		prefix := "Container " + e.Container + ": "
		wrapped := strings.Split(lipgloss.NewStyle().Width(max(textWidth-lipgloss.Width(prefix), 10)).Render(msg), "\n")
		lines = append(lines, bodyStyle.Render("Container ")+nameStyle.Render(e.Container)+bodyStyle.Render(": "+wrapped[0]))
		for _, cont := range wrapped[1:] {
			lines = append(lines, bodyStyle.Render(strings.Repeat(" ", lipgloss.Width(prefix))+cont))
		}
	}
	return errBanner(theme, width, strings.Join(lines, "\n"))
}

// errBanner draws 5a's solid tinted banner block around pre-styled content.
func errBanner(theme tui.Theme, width int, content string) string {
	// The banner is a solid tinted block, not a bordered box. A box-drawing
	// glyph inks the middle of a full cell and the two halves aren't separately
	// addressable, so a boxed banner can only pick which artifact to show:
	// BorderBackground bleeds the tint one cell *outside* the stroke, omitting
	// it leaves a one-cell gap *inside* it. Letting the fill be the shape
	// sidesteps the choice. ErrBannerBorder survives as a left accent bar —
	// ▌ inks the left half of its cell and takes the fill on the right half, so
	// the bar sits flush against the block with no seam of its own.
	accent := lipgloss.Border{Left: "▌"}
	style := lipgloss.NewStyle().
		Background(theme.ErrBannerBg).
		Border(accent, false, false, false, true).
		BorderForeground(theme.ErrBannerBorder).
		BorderBackground(theme.ErrBannerBg).
		Padding(1, 1).
		Width(width) // lipgloss v2's Width already counts the border, so this is exactly width
	return style.Render(content)
}

func (m Model) metaGrid(theme tui.Theme) string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint)
	value := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	link := lipgloss.NewStyle().Foreground(theme.Accent)

	node := m.pod.Node
	if node == "" {
		node = "–"
	}
	ip := m.pod.IP
	if ip == "" {
		ip = "–"
	}
	qos := m.pod.QoSClass
	if qos == "" {
		qos = "–"
	}
	controller := "–"
	controllerStyle := value
	if m.controller != "" {
		controller = m.controller + " ↗"
		controllerStyle = link
	}

	fields := []struct {
		label, value string
		style        lipgloss.Style
	}{
		{"NODE", node, value},
		{"IP", ip, value},
		{"QOS", qos, value},
		{"CONTROLLER", controller, controllerStyle},
		// mock 5a shows "3h" — never kube.Pod.Age's raw Go duration string.
		{"AGE", shortDur(m.pod.AgeDuration), value},
	}
	// mock 5a: label over value, columns aligned across both rows.
	labels := make([]string, len(fields))
	values := make([]string, len(fields))
	for i, f := range fields {
		w := max(len(f.label), lipgloss.Width(f.value))
		labels[i] = label.Render(components.Pad(f.label, w))
		values[i] = f.style.Render(f.value) + strings.Repeat(" ", w-lipgloss.Width(f.value))
	}
	const gap = "    "
	return strings.Join(labels, gap) + "\n" + strings.Join(values, gap)
}

func (m Model) containersBlock(theme tui.Theme, width int) string {
	return m.containerTableBlock(theme, width, "CONTAINERS", m.pod.ContainerInfos, 0, true)
}

func (m Model) initContainersBlock(theme tui.Theme, width int) string {
	return m.containerTableBlock(theme, width, "INIT CONTAINERS", m.pod.InitContainerInfos, len(m.pod.ContainerInfos), false)
}

func (m Model) containerTableBlock(theme tui.Theme, width int, titleText string, infos []kube.ContainerInfo, selectionBase int, showBars bool) string {
	title := lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true).Render(titleText)
	if len(infos) == 0 {
		return title + "\n" + lipgloss.NewStyle().Foreground(theme.TextDim).Render("none")
	}

	cols := []components.Column{
		{Title: "", Min: 1},
		{Title: "Name", Min: 10, Flex: true},
		{Title: "Image", Min: 12, Flex: true, TruncateFront: true},
		{Title: "State", Min: 20},
		{Title: "Restarts", Min: 8, Align: components.AlignRight},
	}
	rows := make([]components.Row, 0, len(infos))
	for _, c := range infos {
		var glyph string
		var glyphStyle lipgloss.Style
		stateStyle := lipgloss.NewStyle().Foreground(theme.Good)
		stateText := c.State
		switch c.State {
		case "Waiting":
			glyph, glyphStyle = "▲", lipgloss.NewStyle().Foreground(theme.Bad)
			stateStyle = lipgloss.NewStyle().Foreground(theme.Bad)
			if c.Reason != "" {
				stateText = "Waiting · " + c.Reason
			}
		case "Terminated":
			// A clean exit (Reason "Completed", the kubelet's own label for
			// ExitCode 0) isn't a warning — e.g. a Job container that ran to
			// completion — so it renders the app's neutral blue (theme.Info),
			// the same "parked, benign, nothing to see" hue statusColor's
			// "neutral" class and the browse list's StatusNeutral already use
			// for a Completed pod, not yellow.
			termColor := theme.Warn
			if c.Reason == "Completed" {
				termColor = theme.Info
			}
			glyph, glyphStyle = "○", lipgloss.NewStyle().Foreground(termColor)
			stateStyle = lipgloss.NewStyle().Foreground(termColor)
			if c.Reason != "" {
				stateText = "Terminated · " + c.Reason
			}
		case "Running":
			glyph, glyphStyle = "●", lipgloss.NewStyle().Foreground(theme.Good)
		default:
			glyph, glyphStyle = "◌", lipgloss.NewStyle().Foreground(theme.TextDim)
			stateStyle = lipgloss.NewStyle().Foreground(theme.TextDim)
			stateText = "–"
		}
		nameStyle := lipgloss.NewStyle().Foreground(theme.TextPrimary)
		imgStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
		// mock 5a: restarts render "6 ↺" — yellow once non-zero.
		restartStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
		if c.Restarts > 0 {
			restartStyle = lipgloss.NewStyle().Foreground(theme.Warn)
		}
		rows = append(rows, components.Row{Cells: []components.Cell{
			{Text: glyph, Style: glyphStyle},
			{Text: c.Name, Style: nameStyle},
			{Text: components.ShortImageRef(c.Image), Style: imgStyle},
			{Text: stateText, Style: stateStyle},
			{Text: fmt.Sprintf("%d %s", c.Restarts, tui.GlyphRestarts), Style: restartStyle},
		}})
	}
	// The selection bar only means something when ↑↓ has somewhere to go —
	// a single-container pod renders plain (no stray highlight). Selection
	// goes through Table's own Selected/SelRowStyle/SelBarStyle (not a
	// per-cell Background) so the highlight covers the inter-column gaps
	// too, not just the cell text (components.Table.renderRowV2's own doc
	// comment on the class of bug this avoids).
	selected := -1
	if m.totalContainerRows() > 1 && m.selectedContainer >= selectionBase && m.selectedContainer < selectionBase+len(infos) {
		selected = m.selectedContainer - selectionBase
	}
	rule := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	t := components.Table{
		Columns:        cols,
		Rows:           rows,
		Selected:       selected,
		Width:          width,
		Height:         len(rows) + 2,
		HeaderStyle:    lipgloss.NewStyle().Foreground(theme.TextFaint),
		FooterStyle:    lipgloss.NewStyle().Foreground(theme.TextGhost),
		SelBarStyle:    lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle:    lipgloss.NewStyle().Background(theme.SelBg),
		ShowHeaderRule: true,
		RuleStyle:      rule,
	}
	hr := rule.Render(strings.Repeat("─", width))
	block := title + "\n" + hr + "\n" + t.Render() + "\n" + hr
	if showBars {
		block += "\n" + m.barsLine(theme) + "\n" + hr
	}
	return block
}

// ephemeralBlock renders §41e's EPHEMERAL group — a debug container
// kubectl debug attached to this pod, real and permanent state (13d: this
// group exists only while at least one does — callers only call this when
// EphemeralContainerInfos is non-empty). Selection shares the CONTAINERS
// grid's combined index space (totalContainerRows); this table's own
// Selected is that index re-based to its own row range.
func (m Model) ephemeralBlock(theme tui.Theme, width int) string {
	title := lipgloss.NewStyle().Foreground(theme.Accent).Bold(true).Render("EPHEMERAL")
	caption := lipgloss.NewStyle().Foreground(theme.TextGhost).Render("attached by debug · not part of the spec · cannot be removed — it leaves when the pod does")

	cols := []components.Column{
		{Title: "", Min: 1},
		{Title: "Name", Min: 10, Flex: true},
		{Title: "Target", Min: 10, Flex: true},
		{Title: "State", Min: 16},
		{Title: "Age", Min: 6, Align: components.AlignRight},
	}
	rows := make([]components.Row, 0, len(m.pod.EphemeralContainerInfos))
	for _, e := range m.pod.EphemeralContainerInfos {
		glyph := lipgloss.NewStyle().Foreground(theme.Accent).Render("⚑")
		var stateStyle lipgloss.Style
		stateText := e.State
		switch e.State {
		case "Waiting":
			stateStyle = lipgloss.NewStyle().Foreground(theme.Warn)
			if e.Reason != "" {
				stateText = "Waiting · " + e.Reason
			}
		case "Terminated":
			stateStyle = lipgloss.NewStyle().Foreground(theme.Warn)
			if e.Reason != "" {
				stateText = "Terminated · " + e.Reason
			}
		case "Running":
			stateStyle = lipgloss.NewStyle().Foreground(theme.Good)
		default:
			stateStyle = lipgloss.NewStyle().Foreground(theme.TextDim)
			stateText = "–"
		}
		target := e.TargetContainer
		if target == "" {
			target = "–"
		}
		rows = append(rows, components.Row{Cells: []components.Cell{
			{Text: glyph},
			{Text: e.Name + " " + components.ShortImageRef(e.Image), Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
			{Text: target, Style: lipgloss.NewStyle().Foreground(theme.TextDim)},
			{Text: stateText, Style: stateStyle},
			{Text: shortDur(e.Age), Style: lipgloss.NewStyle().Foreground(theme.TextDim)},
		}})
	}
	selected := -1
	base := len(m.pod.ContainerInfos) + len(m.pod.InitContainerInfos)
	if m.selectedContainer >= base {
		selected = m.selectedContainer - base
	}
	rule := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	t := components.Table{
		Columns:        cols,
		Rows:           rows,
		Selected:       selected,
		Width:          width,
		Height:         len(rows) + 2,
		HeaderStyle:    lipgloss.NewStyle().Foreground(theme.TextFaint),
		FooterStyle:    lipgloss.NewStyle().Foreground(theme.TextGhost),
		SelBarStyle:    lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle:    lipgloss.NewStyle().Background(theme.SelBg),
		ShowHeaderRule: true,
		RuleStyle:      rule,
	}
	hr := rule.Render(strings.Repeat("─", width))
	return title + "\n" + hr + "\n" + t.Render() + "\n" + caption
}

// barsLine renders the CPU/MEM bars vs each container's summed limits, side
// by side on one line (docs/design README.md §5a: "CPU/MEM bars with
// used/limit text; MEM at 96% renders the bar and text red"), fenced by
// containersBlock's own rule lines above/below.
func (m Model) barsLine(theme tui.Theme) string {
	barStyles := components.BarStyles{
		Track: lipgloss.NewStyle().Foreground(theme.BarTrack),
		Fill:  lipgloss.NewStyle().Foreground(theme.Accent),
		Warn:  lipgloss.NewStyle().Foreground(theme.Warn),
		Bad:   lipgloss.NewStyle().Foreground(theme.Bad),
	}
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	const barWidth = 16

	cpuBar := components.MiniBar(m.pod.CPUMilli, m.pod.CPULimitMilli, barWidth, barStyles)
	cpuText := dim.Render(fmt.Sprintf("%s / %s", usageText(m.pod.CPU), limitText(m.pod.CPULimitMilli, formatMilli)))
	memRatio := ratio(m.pod.MEMBytes, m.pod.MEMLimitBytes)
	memStyle := dim
	if memRatio >= 0.96 {
		memStyle = lipgloss.NewStyle().Foreground(theme.BadText)
	}
	memBar := components.MiniBarBadAt(m.pod.MEMBytes, m.pod.MEMLimitBytes, barWidth, barStyles, 0.96)
	memText := memStyle.Render(fmt.Sprintf("%s / %s", usageText(m.pod.MEM), limitText(m.pod.MEMLimitBytes, formatBytes)))

	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	cpu := faint.Render("CPU ") + cpuBar + " " + cpuText
	mem := faint.Render("MEM ") + memBar + " " + memText
	return cpu + "   " + mem
}

func ratio(used, limit int64) float64 {
	if limit <= 0 {
		return 0
	}
	return float64(used) / float64(limit)
}

func limitText(limit int64, format func(int64) string) string {
	if limit <= 0 {
		return "–"
	}
	return format(limit)
}

func formatMilli(v int64) string { return fmt.Sprintf("%dm", v) }

func formatBytes(v int64) string {
	const gi = 1024 * 1024 * 1024
	const mi = 1024 * 1024
	switch {
	case v >= gi:
		return fmt.Sprintf("%.1fGi", float64(v)/gi)
	case v >= mi:
		return fmt.Sprintf("%.0fMi", float64(v)/mi)
	default:
		return fmt.Sprintf("%dB", v)
	}
}

func (m Model) eventsBlock(theme tui.Theme, width int) string {
	title := lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true).Render("EVENTS") +
		lipgloss.NewStyle().Foreground(theme.TextGhost2).Render(" · newest first")
	if len(m.eventRows) == 0 {
		// a failed fetch is not an empty result — never render a throttled
		// or timed-out events call as a reassuring "no events".
		// An unfilled cache isn't an empty one either, and a cache that
		// stopped filling (Forbidden) says why.
		note := "no events"
		switch {
		case m.eventsErr != nil:
			note = components.Truncate("events unavailable · "+m.eventsErr.Error(), width)
		case m.eventsPending:
			note = "loading events…"
		}
		return title + "\n" + lipgloss.NewStyle().Foreground(theme.TextDim).Render(note)
	}
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	bad := lipgloss.NewStyle().Foreground(theme.Bad)
	info := lipgloss.NewStyle().Foreground(theme.Info)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	msgStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)

	// docs/design README.md §5a: "type (Warning yellow/red, Normal blue)" —
	// 9b's sibling events screen already escalates Warning to red for a
	// currently-failing object (severityStyle); this pod IS the object, so
	// its own current status class stands in for 9b's cross-object lookup.
	failing := m.status.class == "fail"

	lines := []string{title}
	for _, e := range m.eventRows {
		typeStyle := info
		if e.Type == "Warning" {
			typeStyle = warn
			if failing {
				typeStyle = bad
			}
		}
		age := shortAge(e.LastSeen)
		prefix := fmt.Sprintf("%s  %s  %s  ",
			typeStyle.Render(padTo(e.Type, 8)),
			dim.Render(padTo(e.Reason, 16)),
			dim.Render(padTo(age, 6)))
		messageOffset := lipgloss.Width(prefix)
		msgWidth := max(width-messageOffset-4, 10)
		msgLines := strings.Split(lipgloss.NewStyle().Width(msgWidth).Render(e.Message), "\n")
		lines = append(lines, prefix+msgStyle.Render(msgLines[0]))
		for _, continuation := range msgLines[1:] {
			lines = append(lines, strings.Repeat(" ", messageOffset)+msgStyle.Render(continuation))
		}
	}
	return strings.Join(lines, "\n")
}

// sourcesBlock renders the 'v' ENV & MOUNTS section for the selected
// container: env vars drawn from a Secret/ConfigMap key or imported whole via
// envFrom, then every volumeMount with what backs it. Names and keys only —
// no value is ever read or shown, so this needs nothing beyond the pod spec.
// It follows the container selection, so ↑↓ re-targets it.
func (m Model) sourcesBlock(theme tui.Theme, width int) string {
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	ghost := lipgloss.NewStyle().Foreground(theme.TextGhost)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	primary := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	secondary := lipgloss.NewStyle().Foreground(theme.TextSecondary)

	container := m.selectedContainerName()
	title := faint.Bold(true).Render("ENV & MOUNTS")
	if container != "" {
		title += lipgloss.NewStyle().Foreground(theme.TextGhost2).Render(" · " + container)
	}
	src := m.sources[container]
	if len(src.Env) == 0 && len(src.Mounts) == 0 {
		return title + "\n" + dim.Render("no env from secrets or configmaps · no mounts")
	}

	// Left column (var name / mount path) is sized to its widest entry but
	// capped at half the width, so a long source never loses its name.
	const indent = "  "
	leftWidth := 0
	for _, e := range src.Env {
		leftWidth = max(leftWidth, lipgloss.Width(envVarText(e)))
	}
	for _, mt := range src.Mounts {
		leftWidth = max(leftWidth, lipgloss.Width(mountPathText(mt)))
	}
	leftWidth = min(leftWidth, max(width/2-len(indent), 8))
	rightWidth := max(width-len(indent)-leftWidth-len("  ← "), 8)

	row := func(left string, leftStyle lipgloss.Style, right string, rightStyle lipgloss.Style, suffix string) string {
		right = components.Truncate(right, rightWidth)
		suffix = components.Truncate(suffix, max(rightWidth-lipgloss.Width(right), 0))
		return indent + leftStyle.Render(components.Pad(left, leftWidth)) + ghost.Render("  ← ") +
			rightStyle.Render(right) + dim.Render(suffix)
	}

	lines := []string{title}
	if len(src.Env) > 0 {
		lines = append(lines, faint.Render("ENV"))
		for _, e := range src.Env {
			suffix := " · " + e.Key
			if e.All {
				suffix = " · all keys"
			}
			lines = append(lines, row(envVarText(e), primary, string(e.Kind)+"/"+e.Name, secondary, suffix))
		}
	}
	if len(src.Mounts) > 0 {
		lines = append(lines, faint.Render("MOUNTS"))
		for _, mt := range src.Mounts {
			suffix := ""
			if mt.SubPath != "" {
				suffix = " · subPath " + mt.SubPath
			}
			lines = append(lines, row(mountPathText(mt), primary, mt.Source, secondary, suffix))
		}
	}
	return strings.Join(lines, "\n")
}

// envVarText is an env row's left cell: the variable name, or for envFrom
// the prefix (if any) followed by "*", since it imports every key.
func envVarText(e envSource) string {
	if e.All {
		return e.Var + "*"
	}
	return e.Var
}

// mountPathText is a mount row's left cell, "(ro)" marking a read-only mount.
func mountPathText(mt mountSource) string {
	if mt.ReadOnly {
		return mt.Path + " (ro)"
	}
	return mt.Path
}

// relatedTolerationsBlock lays RELATED and TOLERATIONS out side by side —
// the old right sidebar's surviving sections, now a full-width two-column
// row under the container grids (nodedetail.factsPanel's shape: pre-styled
// single-line strings joined per line through components.Pad, since
// lipgloss.JoinHorizontal silently corrupts the taller column). LABELS is
// gone from the resting view — 26a's 'm' metapanel is the labels/annotations
// surface.
func (m Model) relatedTolerationsBlock(theme tui.Theme, width int) string {
	title := lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	secondary := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	accent := lipgloss.NewStyle().Foreground(theme.Accent)

	left := []string{title.Render("RELATED")}
	if len(m.related) == 0 {
		left = append(left, dim.Render("none"))
	} else {
		// Numbered so a digit key (update.go's openRelated) can jump
		// straight to it — replaces the old 'o'/'i' shortcuts. The number
		// shares RELATED's own color (theme.TextFaint) rather than the link's
		// accent, reading as a hint prefix rather than part of the link text.
		num := lipgloss.NewStyle().Foreground(theme.TextFaint)
		for i, item := range m.related {
			left = append(left, num.Render(fmt.Sprintf("%d ", i+1))+accent.Render(item.Label+" ↗"))
		}
	}

	right := []string{title.Render("TOLERATIONS")}
	if len(m.pod.Tolerations) == 0 {
		right = append(right, dim.Render("none"))
	} else {
		for _, t := range m.pod.Tolerations {
			right = append(right, secondary.Render(t))
		}
	}

	leftWidth := width / 2
	rightWidth := width - leftWidth - 2
	n := max(len(left), len(right))
	lines := make([]string, n)
	for i := range n {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines[i] = components.Pad(l, leftWidth) + "  " + components.Pad(r, rightWidth)
	}
	return strings.Join(lines, "\n")
}

// deleteConfirmModal renders 8b's type-the-name modal for the pod's delete
// confirmation (poddetail has only the one mutating verb, so — unlike
// browse — this branch is unconditional, no per-verb split needed).
func (m Model) deleteConfirmModal(width, height int) string {
	theme := m.Theme()
	title := "Confirm"
	target := m.name
	detail := ""
	var ownerLine string
	if pending := m.actions.Pending(); pending != nil {
		title = "✕ " + pending.Label
		target = pending.Scope.ResourceName
		if pending.Owner != "" {
			ownerLine = pending.Owner + " — will be recreated"
		}
		if pending.Scope.Verb == "force-delete" {
			detail = "grace period 0 — force delete, immediate"
		} else {
			detail = "default grace period applies · " + verbs.ForceDeleteModalHint()
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
	return components.TypeNameModal(title, ownerLine, detail, target, m.actions.TypedInput(), "delete", m.isProd(), styles, width, height)
}

func padTo(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-len(s))
}

// shortAge renders how long ago t was — mirrors resources.shortAge's
// compact "12m"/"3h"/"5d" shape (that helper is unexported in the resources
// package, so poddetail's EVENTS grid needs its own copy).
func shortAge(t time.Time) string {
	return shortDur(time.Since(t))
}

// shortDur is shortAge for an already-computed duration — the termination
// banner's "4m ago", never a raw Go duration like "456h29m47s".
func shortDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
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
