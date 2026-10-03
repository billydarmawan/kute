package objectdetail

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
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
	if !m.desc.ClusterScoped && m.namespace != "" {
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: m.namespace, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
		)
	}
	crumbs = append(crumbs,
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: m.desc.Display, Style: dim},
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
				lipgloss.NewStyle().Foreground(m.Theme().Bad).Bold(true).Render(m.desc.Display + " deleted"),
				lipgloss.NewStyle().Foreground(m.Theme().TextDim).Render(m.name + " no longer exists · press any key to go back"),
			}, width, height)
		}
		return m.readyBody(width)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// metaBody renders 26a's panel in place of the detail body while it's open —
// the object's own title line stays frozen above it, the detail-screen
// analogue of browse's selected-row-above-the-panel framing (docs/design
// README.md §26a); the grid and will-run strip come from the shared
// metapanel renderer.
func (m Model) metaBody(width, height int) string {
	theme := m.Theme()
	lines := []string{m.titleLine(theme, width), ""}
	lines = append(lines, m.meta.PanelLines(width, &m.actions)...)
	lines = append(lines, "", m.meta.WillRunStrip(width, &m.actions))
	return components.Pad(strings.Join(lines, "\n"), width)
}

// readyBody stacks 14d's sections top to bottom: title row, meta grid (the
// kind's own declared printer columns, plus an OWNER line when the object
// has an ownerReference), CONDITIONS verbatim, EVENTS. No sidebar (unlike
// 5a) — 14d's decided scope is title/meta/conditions/events only.
func (m Model) readyBody(width int) string {
	theme := m.Theme()
	var lines []string
	lines = append(lines, m.titleLine(theme, width))
	if grid := m.metaGrid(theme); grid != "" {
		lines = append(lines, "", grid)
	}
	lines = append(lines, "", m.conditionsBlock(theme))
	lines = append(lines, "", m.eventsBlock(theme, width))
	return strings.Join(lines, "\n")
}

func (m Model) titleLine(theme tui.Theme, width int) string {
	name := lipgloss.NewStyle().Foreground(theme.Text).Bold(true).Render(m.name)
	parts := []string{name}
	if c, ok := primaryCondition(m.conditions); ok {
		style := lipgloss.NewStyle().Foreground(statusColor(theme, m.row.Status))
		glyph := m.row.Glyph
		if glyph == "" {
			glyph = "·"
		}
		parts = append(parts, style.Render(glyph+" "+statusWord(c)))
	}
	if m.obj != nil {
		if av := m.obj.GetAPIVersion(); av != "" {
			parts = append(parts, lipgloss.NewStyle().Foreground(theme.TextGhost).Render(av))
		}
	}
	left := strings.Join(parts, "  ")
	right := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("watching · live")
	return padBetween(left, right, width)
}

// padBetween right-aligns right against width, dropping it when the line is
// too tight — same shape as poddetail's own, duplicated per the repo's
// package-local convention.
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 || right == "" {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func statusColor(theme tui.Theme, class resources.StatusClass) color.Color {
	switch class {
	case resources.StatusOK:
		return theme.Good
	case resources.StatusWarn:
		return theme.Warn
	case resources.StatusFail:
		return theme.Bad
	default:
		return theme.TextDim
	}
}

// metaGrid pairs the kind's own declared columns (Descriptor.Columns,
// excluding the always-present leading Name and trailing Age) with the
// loaded object's projected cells — purely data-driven, no per-CRD code —
// plus one appended OWNER line when the object carries an ownerReference,
// rendered the same purple-accent-plus-↗ way poddetail's RELATED entries
// are — and, like them, '1' (the keybar's "1 owner") jumps to it through the
// goto machinery (docs/design README.md §14d; update.go's openOwner).
func (m Model) metaGrid(theme tui.Theme) string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint)
	value := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	link := lipgloss.NewStyle().Foreground(theme.Accent)

	type field struct {
		label, value string
		style        lipgloss.Style
	}
	var fields []field
	cols, cells := m.desc.Columns, m.row.Cells
	if len(cols) == len(cells) && len(cols) > 2 {
		for i := 1; i < len(cols)-1; i++ {
			v := cells[i]
			if v == "" {
				v = "–"
			}
			fields = append(fields, field{label: strings.ToUpper(cols[i]), value: v, style: value})
		}
	}
	if owner := firstOwnerRef(m.obj); owner != "" {
		fields = append(fields, field{label: "OWNER", value: owner + " ↗", style: link})
	}
	if len(fields) == 0 {
		return ""
	}

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

// conditionsBlock renders CONDITIONS verbatim (docs/design README.md §14d:
// "the message text IS the diagnosis — never paraphrase it").
func (m Model) conditionsBlock(theme tui.Theme) string {
	title := lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true).Render("CONDITIONS")
	if len(m.conditions) == 0 {
		return title + "\n" + lipgloss.NewStyle().Foreground(theme.TextDim).Render("none")
	}
	good := lipgloss.NewStyle().Foreground(theme.Good)
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	bad := lipgloss.NewStyle().Foreground(theme.Bad)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	msgStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)

	lines := []string{title}
	for _, c := range m.conditions {
		glyph, style := "▲", warn
		switch c.Status {
		case "True":
			glyph, style = "●", good
		case "False":
			glyph, style = "✕", bad
		}
		age := ""
		if !c.LastTransition.IsZero() {
			age = shortAge(m.now, c.LastTransition)
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s  %s",
			style.Render(glyph),
			dim.Render(padTo(c.Type, 16)),
			style.Render(padTo(c.Status, 8)),
			msgStyle.Render(c.Message),
			dim.Render(age)))
	}
	return strings.Join(lines, "\n")
}

// eventsBlock mirrors poddetail's own eventsBlock rendering (glyph-less
// type/reason/age/message grid), duplicated per the repo's
// package-local-seam convention.
func (m Model) eventsBlock(theme tui.Theme, width int) string {
	title := lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true).Render("EVENTS") +
		lipgloss.NewStyle().Foreground(theme.TextGhost).Render(" · newest first")
	if len(m.eventRows) == 0 {
		note := "no events"
		if m.eventsErr != nil {
			note = "events unavailable"
		}
		return title + "\n" + lipgloss.NewStyle().Foreground(theme.TextDim).Render(note)
	}
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	info := lipgloss.NewStyle().Foreground(theme.Info)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	msgStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)

	lines := []string{title}
	for _, e := range m.eventRows {
		typeStyle := info
		if e.Type == "Warning" {
			typeStyle = warn
		}
		age := shortAge(m.now, e.LastSeen)
		msg := ellipsize(e.Message, max(width-40, 10))
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s",
			typeStyle.Render(padTo(e.Type, 8)),
			dim.Render(padTo(e.Reason, 16)),
			dim.Render(padTo(age, 6)),
			msgStyle.Render(msg)))
	}
	return strings.Join(lines, "\n")
}

// deleteConfirmModal renders 8b's type-the-name modal for the object's
// delete confirmation — same shape as poddetail's own (objectdetail has
// only the one mutating verb too, so this branch is unconditional).
func (m Model) deleteConfirmModal(width, height int) string {
	theme := m.Theme()
	title := "Confirm"
	target := m.name
	detail := "default grace period applies"
	var ownerLine string
	if pending := m.actions.Pending(); pending != nil {
		title = "✕ " + pending.Label
		target = pending.Scope.ResourceName
		if pending.Owner != "" {
			ownerLine = pending.Owner + " — will be recreated"
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

// shortAge/shortDur/ellipsize mirror poddetail's own copies (that package's
// versions are unexported too), duplicated per the repo's
// package-local-seam convention.
func shortAge(now, t time.Time) string {
	return shortDur(now.Sub(t))
}

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

func ellipsize(s string, width int) string {
	if width <= 1 || len(s) <= width {
		return s
	}
	return s[:width-1] + "…"
}
