package forwardpicker

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// panelWidth is the picker's fixed inner content width, matching 10a's
// execpicker.
const panelWidth = 60

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
	ghost2 := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	text := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)

	crumbs := append(tui.BrandCrumbs(theme),
		tui.Crumb{Text: " │ ", Style: ghost2},
		tui.Crumb{Text: "⇄ forward › ", Style: dim},
		tui.Crumb{Text: m.target.Name, Style: text},
	)

	var chip tui.ConnBadge
	if m.session != nil {
		// docs/design README.md §13d: every screen's Header() carries the
		// ambient forward chip — this one and execpicker's own were the
		// two omitting it.
		chip = tui.BuildForwardChip(theme, m.session.ForwardSummary())
	}
	return tui.HeaderState{
		Crumbs:      crumbs,
		ConfigChip:  tui.BuildConfigChip(theme, m.session),
		UpdateChip:  tui.BuildUpdateChip(theme, m.session),
		ForwardChip: chip,
		Conn:        tui.LiveConnBadge(theme, m.conn, tui.GlyphRunning+" connected"),
	}
}

func (m Model) Strips(width int) []string { return nil }

func (m Model) Body(width, height int) string {
	theme := m.Theme()
	panelStyle := lipgloss.NewStyle().
		Foreground(theme.TextPrimary).
		Background(theme.BgPalette).
		BorderForeground(theme.BorderPalette).
		Padding(1, 2)
	return components.Card(m.panelContent(theme), panelStyle, width, height)
}

func (m Model) panelContent(theme tui.Theme) string {
	if m.state == tui.TaskStateError {
		return lipgloss.NewStyle().Foreground(theme.Bad).Render(ellipsize(m.feedback, panelWidth))
	}
	if m.state == tui.TaskStateLoading {
		return lipgloss.NewStyle().Foreground(theme.TextDim).Render(m.feedback)
	}

	var lines []string
	lines = append(lines, m.panelHeader(theme))
	lines = append(lines, "")
	for i, row := range m.rows {
		lines = append(lines, m.portLine(theme, i, row))
	}
	lines = append(lines, "")
	lines = append(lines, m.willRunLine(theme))
	return strings.Join(lines, "\n")
}

func (m Model) panelHeader(theme tui.Theme) string {
	left := lipgloss.NewStyle().Foreground(theme.Accent).Bold(true).Render("⇄ forward › ") +
		lipgloss.NewStyle().Foreground(theme.TextPrimary).Render(m.target.Name)
	kind := strings.ToLower(string(m.target.Kind))
	right := lipgloss.NewStyle().Foreground(theme.TextFaint).Render(fmt.Sprintf("%s · %s · %d ports", kind, m.target.Namespace, len(m.rows)))
	gap := max(panelWidth-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) portLine(theme tui.Theme, i int, row portRow) string {
	selected := i == m.selected
	// rowFill paints the row's gap cells with the row's own background —
	// SelBg when selected, transparent (BgPalette is empty) otherwise —
	// because an outer Background wrap is cancelled by each inner span's
	// ANSI reset, leaving plain spaces to fall back to the terminal's
	// background (palette's fillSpaces is the same convention).
	var rowFill lipgloss.Style
	marker := "  "
	glyphStyle := lipgloss.NewStyle().Foreground(theme.Good)
	nameStyle := lipgloss.NewStyle().Foreground(theme.Text)
	containerStyle := lipgloss.NewStyle().Foreground(theme.TextFaint)
	localStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	if selected {
		rowFill = lipgloss.NewStyle().Background(theme.SelBg)
		marker = lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg).Render("▸ ")
		nameStyle = nameStyle.Background(theme.SelBg).Bold(true)
		containerStyle = containerStyle.Background(theme.SelBg)
		localStyle = localStyle.Background(theme.SelBg)
		glyphStyle = glyphStyle.Background(theme.SelBg)
	}
	fill := func(n int) string {
		if n <= 0 {
			return ""
		}
		if selected {
			return rowFill.Render(strings.Repeat(" ", n))
		}
		return strings.Repeat(" ", n)
	}

	label := strconv.Itoa(int(row.Port))
	if row.Name != "" {
		label += "/" + row.Name
	}
	name := nameStyle.Render(label)
	if row.Container != "" {
		name += fill(2) + containerStyle.Render("container "+row.Container)
	}

	local := m.localPortText(row)
	localText := localStyle.Render(local)
	if row.editing {
		// CharLimit digits plus one cell for the trailing block cursor: a
		// width of exactly CharLimit makes the field scroll a full-length
		// port one digit off its left edge to make room for the cursor, so
		// a typed 45678 renders as "localhost:5678".
		localText = lipgloss.NewStyle().Foreground(theme.Text).Background(theme.SelBg).Render("localhost:") +
			row.editInput.ViewWidth(row.editInput.CharLimit+1)
	}

	left := marker + glyphStyle.Render("●") + fill(1) + name
	gap := max(panelWidth-lipgloss.Width(left)-lipgloss.Width(localText)-1, 1)
	return left + fill(gap) + localText
}

func (m Model) localPortText(row portRow) string {
	text := fmt.Sprintf("localhost:%d", row.localPort)
	if row.busyFrom != 0 {
		text = fmt.Sprintf("%d busy → %d", row.busyFrom, row.localPort)
	}
	return text
}

// willRunLine shows the exact kubectl invocation the highlighted port's
// enter key is equivalent to (docs/design README.md §13a: "no magic,
// copyable documentation") — the real dial goes through client-go, not this
// subprocess, but the pod/ports it names are exact. A command longer than
// the panel wraps onto continuation lines indented under the label rather
// than ellipsizing, matching debugpanel's willRunLine — the command is
// meant to be copyable, and truncating it silently drops real content.
func (m Model) willRunLine(theme tui.Theme) string {
	const labelText = "will run  "
	label := lipgloss.NewStyle().Foreground(theme.TextGhost).Render(labelText)
	if m.selected < 0 || m.selected >= len(m.rows) {
		return label
	}
	if m.resolvedPod == "" {
		return label + lipgloss.NewStyle().Foreground(theme.Bad).Render("could not resolve a backing pod")
	}
	row := m.rows[m.selected]
	var cmdText string
	if row.podPort > 0 {
		cmdText = kube.PortForwardCommandString(m.target.Namespace, m.resolvedPod, row.localPort, row.podPort)
	} else {
		// A Service targetPort that couldn't be resolved against the pod:
		// name the Service instead, which kubectl translates itself, rather
		// than printing the Service port as if it were the pod's.
		cmdText = kube.ServicePortForwardCommandString(m.target.Namespace, m.target.Name, row.localPort, row.Port)
	}
	cmdStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	indent := strings.Repeat(" ", lipgloss.Width(labelText))
	wrapped := components.Wrap(cmdText, panelWidth-lipgloss.Width(labelText))
	lines := make([]string, len(wrapped))
	for i, l := range wrapped {
		prefix := label
		if i > 0 {
			prefix = indent
		}
		lines[i] = prefix + cmdStyle.Render(strings.TrimRight(l, " "))
	}
	return strings.Join(lines, "\n")
}

func ellipsize(s string, width int) string {
	if width <= 1 || lipgloss.Width(s) <= width {
		return s
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width-1]) + "…"
}
