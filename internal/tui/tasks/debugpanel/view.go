package debugpanel

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
)

// panelWidth is the debug panel's fixed inner content width — the wider of
// 10a's execpicker (56) and 13a's forwardpicker (60), since this panel's
// rows carry more per-field hint text than either.
const panelWidth = 68

// labelWidth is the left column every field row's label is padded to.
const labelWidth = 10

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
	kind, name := "Pods", m.podName
	if m.tgt == targetNode {
		kind, name = "Nodes", m.nodeName
	}

	crumbs := append(tui.BrandCrumbs(theme),
		tui.Crumb{Text: " │ ", Style: ghost2},
		tui.Crumb{Text: ctxName, Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: kind, Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: name, Style: text},
	)

	var chip tui.ConnBadge
	if m.session != nil {
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
	if m.actions.Active() && m.actions.Tier() == actions.TierModal {
		return m.typeNameConfirmModal(width, height)
	}
	theme := m.Theme()
	panelStyle := lipgloss.NewStyle().
		Foreground(theme.TextPrimary).
		Background(theme.BgPalette).
		BorderForeground(theme.BorderPalette).
		Padding(1, 2)
	return components.Card(m.panelContent(theme), panelStyle, width, height)
}

// typeNameConfirmModal renders §41c's cleanup delete in PROD — the ordinary
// type-the-name modal every Pod delete gets (Controller.TypeToConfirm's
// "delete" entry), unchanged from browse's own shape.
func (m Model) typeNameConfirmModal(width, height int) string {
	theme := m.Theme()
	title := "Confirm"
	target := ""
	if pending := m.actions.Pending(); pending != nil {
		title = "✕ " + pending.Label
		target = pending.Scope.ResourceName
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
	return components.TypeNameModal(title, "", "", target, m.actions.TypedInput(), "delete", m.isProd(), styles, width, height)
}

// panelContent builds the panel body: header, mode selector (pod target
// only), fields, a warning line, the "will run" documentation line, and a
// reserved feedback line.
func (m Model) panelContent(theme tui.Theme) string {
	var lines []string
	lines = append(lines, m.panelHeader(theme), "")
	if m.tgt == targetPod {
		lines = append(lines, m.modeSelectorLines(theme)...)
		lines = append(lines, "")
	}
	lines = append(lines, m.fieldLines(theme)...)
	lines = append(lines, "", m.warningLine(theme), "")
	lines = append(lines, m.willRunLine(theme))
	if m.feedback != "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(theme.Bad).Render(ellipsize(m.feedback, panelWidth)))
	}
	return strings.Join(lines, "\n")
}

func (m Model) panelHeader(theme tui.Theme) string {
	name := m.podName
	if m.tgt == targetNode {
		name = "node/" + m.nodeName
	}
	left := lipgloss.NewStyle().Foreground(theme.Accent).Bold(true).Render("⚑ debug › ") +
		lipgloss.NewStyle().Foreground(theme.TextPrimary).Render(name)
	right := lipgloss.NewStyle().Foreground(theme.TextFaint).Render(m.headerRight())
	gap := max(panelWidth-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) headerRight() string {
	switch {
	case m.tgt == targetNode:
		return "host namespaces · privileged"
	case m.mode == modeCopy:
		return "copy mode"
	default:
		return "pod running · ephemeral container"
	}
}

// modeSelectorLines is §41b/§41c's two-row mode picker — the "attach
// ephemeral" row is hard-disabled (never selectable, per the confirmed
// decision) whenever the pod won't stay running, rather than merely dim.
func (m Model) modeSelectorLines(theme tui.Theme) []string {
	disabledReason := ""
	if m.podWontStayRunning() {
		disabledReason = "needs a running container"
		if m.podPhase != "" && m.podPhase != "Running" {
			disabledReason += " — pod phase is " + m.podPhase
		} else if m.waiting {
			disabledReason += " — a container is waiting"
		}
	}
	return []string{
		m.modeRow(theme, "attach ephemeral", "shares this pod's namespaces · nothing restarts", m.mode == modeAttach, disabledReason),
		m.modeRow(theme, "copy pod", "a new pod from this spec · for pods that won't stay up", m.mode == modeCopy, ""),
	}
}

func (m Model) modeRow(theme tui.Theme, label, desc string, selected bool, disabledReason string) string {
	marker := "  "
	labelStyle := lipgloss.NewStyle().Foreground(theme.Text)
	descStyle := lipgloss.NewStyle().Foreground(theme.TextFaint)
	switch {
	case disabledReason != "":
		labelStyle = labelStyle.Foreground(theme.TextDim)
		descStyle = lipgloss.NewStyle().Foreground(theme.Warn)
		desc = disabledReason
	case selected:
		marker = lipgloss.NewStyle().Foreground(theme.Accent).Render("▸ ")
		labelStyle = labelStyle.Bold(true)
	}
	left := marker + labelStyle.Render(label)
	gap := max(panelWidth-lipgloss.Width(left)-lipgloss.Width(desc)-2, 1)
	return left + strings.Repeat(" ", gap) + descStyle.Render(desc)
}

// fieldLines dispatches to the field set for the current target/mode — the
// one place §41b/§41c/§41d's genuinely different field sets are named.
func (m Model) fieldLines(theme tui.Theme) []string {
	switch {
	case m.tgt == targetNode:
		return []string{
			m.fieldRow(theme, "image", fieldImage, m.nodeImage, "i edit · tab recents"),
			m.fieldRow(theme, "profile", fieldNone, string(m.nodeProfile), "p cycle · "+profileHint(m.nodeProfile, true)),
			m.fieldRow(theme, "rootfs", fieldNone, "/host", "chroot /host once you land"),
			m.fieldRow(theme, "creates", fieldNone, "a real pod on this node", "kute finds it and offers to delete it after"),
		}
	case m.mode == modeAttach:
		return []string{
			m.fieldRow(theme, "image", fieldImage, m.attachImage, "i edit · tab recents"),
			m.fieldRow(theme, "target", fieldNone, m.attachTargetContainer().Name, "t cycle · shares the process namespace, so its pid is visible"),
			m.fieldRow(theme, "profile", fieldNone, string(m.podProfile), "p cycle · "+profileHint(m.podProfile, false)),
		}
	default:
		return []string{
			m.fieldRow(theme, "copy name", fieldCopyName, m.copyName, "i edit"),
			m.fieldRow(theme, "container", fieldNone, m.copyContainer().Name, "t cycle · image kept, so you debug the real thing"),
			m.fieldRow(theme, "profile", fieldNone, string(m.podProfile), "p cycle · "+profileHint(m.podProfile, false)),
			m.fieldRow(theme, "entrypoint", fieldEntrypoint, m.copyEntrypoint, "e edit · replaces the command — that is what stops the crash loop"),
			m.fieldRow(theme, "processes", fieldNone, processesText(m.copyShareProcesses), "s toggle"),
		}
	}
}

func (m Model) fieldRow(theme tui.Theme, label string, field fieldID, staticValue, hint string) string {
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextFaint)
	valueStyle := lipgloss.NewStyle().Foreground(theme.Text)
	hintStyle := lipgloss.NewStyle().Foreground(theme.TextGhost)

	left := "  " + labelStyle.Render(padRight(label, labelWidth))
	var value string
	if field != fieldNone && m.editingField == field {
		inputWidth := max(m.width-lipgloss.Width(left)-lipgloss.Width(hint)-4, 1)
		value = valueStyle.Render(m.editInput.ViewWidth(inputWidth))
	} else {
		value = valueStyle.Render(staticValue)
	}
	line := left + value
	if hint != "" {
		line += "  " + hintStyle.Render(hint)
	}
	return line
}

func padRight(s string, n int) string {
	if lipgloss.Width(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-lipgloss.Width(s))
}

func processesText(shared bool) string {
	if shared {
		return "shared"
	}
	return "isolated"
}

func profileHint(p kube.DebugProfile, node bool) string {
	switch p {
	case kube.ProfileSysadmin:
		if node {
			return "host pid + privileged"
		}
		return "privileged container"
	case kube.ProfileNetadmin:
		return "network admin capabilities"
	case kube.ProfileRestricted:
		return "least privilege"
	default:
		return "curated default toolset"
	}
}

// warningLine is the one line every sub-screen commits to naming its own
// real cost — an ephemeral container that outlives removal, a real pod
// counting against quota, or the node's own live pod count as a number from
// the data rather than a scare string (docs/design v.0.11.0.dc.html §41d).
func (m Model) warningLine(theme tui.Theme) string {
	style := lipgloss.NewStyle().Foreground(theme.Warn)
	switch {
	case m.tgt == targetNode:
		return style.Render(fmt.Sprintf("⚑ root on %s — %d pods are running here", m.nodeName, m.nodePodCount))
	case m.mode == modeAttach:
		return style.Render("⚑ becomes part of this pod — an ephemeral container cannot be removed, only outlived")
	default:
		return style.Render("⚑ creates a real pod — it counts against quota and it is yours to delete")
	}
}

// willRunLine shows the exact kubectl invocation 'enter' will run (docs/
// design README.md §10a's "no magic, copyable documentation" idiom, reused
// here) — recomputed live off the in-place field buffers on every
// keystroke, the same configmapdata's willRunStrip contract. A command
// longer than the panel wraps onto continuation lines indented under the
// label rather than ellipsizing — every debug command is meant to be
// copyable, and a truncated one silently drops its tail (e.g. profile
// flags on a long image name). execpicker and forwardpicker's own
// willRunLine follow the same shape.
func (m Model) willRunLine(theme tui.Theme) string {
	const labelText = "will run  "
	label := lipgloss.NewStyle().Foreground(theme.TextGhost).Render(labelText)
	var cmdText string
	switch {
	case m.tgt == targetNode:
		cmdText = kube.NodeDebugCommandString(m.session.CommandTarget(), m.nodeName, m.nodeImage, m.nodeProfile)
	case m.mode == modeAttach:
		cmdText = kube.PodDebugAttachCommandString(m.session.CommandTarget(), m.namespace, m.podName, m.attachImage, m.attachTargetContainer().Name, m.podProfile)
	default:
		cmdText = kube.PodDebugCopyCommandString(m.session.CommandTarget(), m.namespace, m.podName, m.copyName, m.copyContainer().Name, m.copyEntrypoint, m.copyShareProcesses, m.podProfile)
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
