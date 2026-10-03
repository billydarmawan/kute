package cronjobschedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

func (m Model) View() tea.View { return tea.NewView(m.Render()) }

func (m Model) Render() string { return tui.Frame(m.width, m.height, m) }

// Header is 36d's breadcrumb: "… › <namespace> › cronjob/<name> › Schedule"
// — the same trailing-segment shape secretdata's own Header() uses for
// 27b's "› Data".
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
		tui.Crumb{Text: "cronjob/" + m.name, Style: secondary},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "Schedule", Style: text},
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

// Strips shows the live UTC clock while ready — the same "clock HH:MM:SS
// UTC" wall-clock 36a's own strip carries, so a screen reached from that
// list keeps the same ambient time reference.
func (m Model) Strips(width int) []string {
	if m.state != tui.TaskStateReady {
		return nil
	}
	theme := m.Theme()
	label := lipgloss.NewStyle().Foreground(theme.TextPrimary).Render(m.namespace + "/" + m.name)
	clock := lipgloss.NewStyle().Foreground(theme.TextDim).Render("clock " + m.now.UTC().Format("15:04:05") + " UTC")
	line := padBetween(label, clock, stripInnerWidth(width))
	return []string{insetStripLine(line, width)}
}

func (m Model) Body(width, height int) string {
	switch m.state {
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	case tui.TaskStateReady:
		return m.scheduleBody(m.Theme(), width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

func (m Model) scheduleBody(theme tui.Theme, width, height int) string {
	var lines []string
	lines = append(lines, m.fieldLine(theme, "SCHEDULE", m.scheduleInput.View(), "")...)
	lines = append(lines, m.breakdownLines(theme)...)
	lines = append(lines, "")
	lines = append(lines, m.timezoneLines(theme)...)
	lines = append(lines, "")
	lines = append(lines, m.nextRunsLines(theme)...)
	lines = append(lines, "")
	lines = append(lines, m.whatChangesLines(theme)...)
	if strip := m.willRunStrip(theme, width); strip != "" {
		lines = append(lines, "", strip)
	}
	return components.Pad(strings.Join(lines, "\n"), width)
}

// fieldLine renders one labeled input row: a dim uppercase label, the
// buffer's own View() (which draws its own cursor while Focused), and an
// optional trailing note.
func (m Model) fieldLine(theme tui.Theme, label, value, note string) []string {
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextFaint)
	line := labelStyle.Render(fmt.Sprintf("%-10s", label)) + value
	if note != "" {
		line += lipgloss.NewStyle().Foreground(theme.TextDim).Render("  " + note)
	}
	return []string{line}
}

// breakdownLines renders the MIN/HOUR/DOM/MONTH/DOW token row plus the
// conservative English description (task 5) — or, for a macro schedule, the
// macro literal alone.
func (m Model) breakdownLines(theme tui.Theme) []string {
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextFaint)
	tokenStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	descStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	if m.parseErr != nil {
		return []string{"          " + lipgloss.NewStyle().Foreground(theme.Bad).Render("invalid schedule: "+m.parseErr.Error())}
	}
	if m.fields.Macro != "" {
		return []string{"          " + tokenStyle.Render(m.fields.Macro), "          " + descStyle.Render(describeSchedule(m.fields))}
	}
	breakdown := fmt.Sprintf("MIN %s · HOUR %s · DOM %s · MONTH %s · DOW %s",
		m.fields.Minute, m.fields.Hour, m.fields.DOM, m.fields.Month, m.fields.DOW)
	out := []string{"          " + labelStyle.Render(breakdown)}
	if desc := describeSchedule(m.fields); desc != "" {
		out = append(out, "          "+descStyle.Render(desc))
	}
	return out
}

// timezoneLines renders the TIMEZONE field: the live buffer when editable,
// a plain read-only value/placeholder otherwise, plus a note explaining
// why it can't be edited when that's the case (§3.8: never a fabricated
// cluster version).
func (m Model) timezoneLines(theme tui.Theme) []string {
	note := ""
	switch {
	case m.tzFocused && m.tzErr != nil:
		note = lipgloss.NewStyle().Foreground(theme.Bad).Render("invalid: " + m.tzErr.Error())
	case !m.tzEditable():
		switch m.timeZoneCapability() {
		case kube.TimeZoneCapabilityUnsupported:
			note = lipgloss.NewStyle().Foreground(theme.TextDim).Render("read-only · this cluster is below Kubernetes 1.25 · ctrl+y full edit")
		default:
			note = lipgloss.NewStyle().Foreground(theme.TextDim).Render("read-only · cluster support unknown · ctrl+y full edit")
		}
	default:
		note = lipgloss.NewStyle().Foreground(theme.TextDim).Render("tab to edit")
	}
	value := m.tzInput.View()
	if !m.tzEditable() {
		text := m.accepted.timezone
		if text == "" {
			text = "unset — controller local time"
		}
		value = lipgloss.NewStyle().Foreground(theme.TextSecondary).Render(text)
	}
	return m.fieldLine(theme, "TIMEZONE", value, note)
}

// nextRunsLines renders the next-five-runs proof (§36d: "the timestamps are
// the proof, the sentence is a courtesy") — absolute time in the selected
// zone plus a relative ETA computed from m.now, so the clock tick alone
// keeps the ETA column live without recomputing the timestamps themselves.
func (m Model) nextRunsLines(theme tui.Theme) []string {
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	out := []string{labelStyle.Render("NEXT 5 RUNS")}
	switch {
	case m.parseErr != nil:
		out = append(out, "  "+dim.Render("—"))
	case nextRunsUnset(m.nextErr):
		out = append(out, "  "+dim.Render("controller local · kute cannot discover the kube-controller-manager's own time zone"))
	case m.nextErr != nil:
		out = append(out, "  "+lipgloss.NewStyle().Foreground(theme.Bad).Render(m.nextErr.Error()))
	case len(m.nextRuns) == 0:
		out = append(out, "  "+dim.Render("no upcoming runs"))
	default:
		for _, at := range m.nextRuns {
			eta := dim.Render("in " + shortDuration(at.Sub(m.now)))
			out = append(out, "  "+lipgloss.NewStyle().Foreground(theme.TextSecondary).Render(at.Format("2006-01-02 15:04 MST"))+"  "+eta)
		}
	}
	return out
}

// whatChangesLines renders WHAT CHANGES (task 7): added/removed/annual
// delta and whether the next occurrence itself moves, "computing…" while
// the async comparison for the current edit is in flight, an explicit
// Unavailable reason (an unset timezone on either side), or nothing at all
// when the buffers match what's accepted — there is genuinely nothing to
// compare yet.
func (m Model) whatChangesLines(theme tui.Theme) []string {
	if !m.hasPendingChange() || m.parseErr != nil || m.tzErr != nil {
		return nil
	}
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextFaint).Bold(true)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	out := []string{labelStyle.Render("WHAT CHANGES")}
	switch {
	case m.comparing || m.comparison == nil:
		out = append(out, "  "+dim.Render("computing…"))
	case m.comparison.Unavailable:
		out = append(out, "  "+dim.Render(m.comparison.UnavailableWhy))
	default:
		c := m.comparison
		qualifier := ""
		if c.Truncated {
			qualifier = " (at least, truncated at the comparison cap)"
		}
		delta := strconv.Itoa(c.AnnualDelta)
		if c.AnnualDelta >= 0 {
			delta = "+" + delta
		}
		line := fmt.Sprintf("+%d added · −%d removed · annual delta %s%s", c.Added, c.Removed, delta, qualifier)
		out = append(out, "  "+lipgloss.NewStyle().Foreground(theme.TextSecondary).Render(line))
		nextLine := "next occurrence unchanged"
		if c.NextChanges {
			nextLine = "next occurrence changes"
		}
		out = append(out, "  "+dim.Render(nextLine))
	}
	return out
}

// willRunStrip mirrors secretdata/configmapdata's own will-run band: a
// BorderSubtle rule, then the exact command that would run, or the last
// inline result/error — "" (no rule at all) when there's genuinely nothing
// to say, the same "no placeholder line" rule those screens follow.
func (m Model) willRunStrip(theme tui.Theme, width int) string {
	fill := lipgloss.NewStyle().Background(theme.BgStrip)
	label := fill.Foreground(theme.TextDim)
	cmd := fill.Foreground(theme.TextSecondary)

	left := ""
	hasContent := true
	switch {
	case m.lastError != "":
		left = label.Render("error") + fill.Render(" ") + fill.Foreground(theme.Bad).Render(m.lastError)
	case m.message != "":
		left = fill.Foreground(theme.Good).Render(m.message)
	case m.previous != nil:
		left = label.Render("applied") + fill.Render(" ") + fill.Foreground(theme.TextDim).Render("press u to undo")
	case !m.hasPendingChange():
		hasContent = false
	case m.parseErr != nil || m.tzErr != nil:
		left = label.Render("will run") + fill.Render(" ") + cmd.Render("fix the error above before applying")
	default:
		left = label.Render("will run") + fill.Render(" ") + cmd.Render(m.willRunCommand())
	}
	if !hasContent {
		return ""
	}
	rule := lipgloss.NewStyle().Foreground(theme.BorderSubtle).Render(strings.Repeat("─", width))
	return rule + "\n" + insetStripLineFill(fill.Render(strings.Repeat(" ", tui.FrameInset))+left, width, fill)
}

// nextRunsUnset reports whether err is resources.ErrTimeZoneUnset — §3.9's
// "no explicit spec.timeZone" case, which NextRunsLines renders as
// "controller local" rather than a plain error.
func nextRunsUnset(err error) bool {
	return errors.Is(err, resources.ErrTimeZoneUnset)
}

// shortDuration renders a duration the same coarse "4m"/"2h"/"3d" grain
// resources.shortEta uses for 36a's own NEXT cell — duplicated here (a
// private helper in an internal package, not exported) rather than
// stretching resources' own unexported helper across a package boundary.
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func stripInnerWidth(width int) int {
	return max(width-2*tui.FrameInset, 0)
}

func insetStripLine(line string, width int) string {
	return components.Pad(strings.Repeat(" ", tui.FrameInset)+line, width)
}

func insetStripLineFill(line string, width int, fill lipgloss.Style) string {
	content := line
	slack := width - lipgloss.Width(content)
	if slack <= 0 {
		return content
	}
	return content + fill.Render(strings.Repeat(" ", slack))
}
