package whocan

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/tui"
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

// Header is 22a's breadcrumb — cluster-scoped like Nodes/CRDs (11a/14b):
// the query's namespace slot is already spelled out in the summary strip's
// question line, so it isn't repeated in the breadcrumb too.
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
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "Who Can", Style: text},
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

// Strips is 22a's editable question line — "who can <verb> <resource> in
// <namespace>" — plus the "same as" kubectl auth can-i equivalent, the same
// copyable-documentation idiom 10a/13a/17b already use for a real command
// the app itself never executes.
func (m Model) Strips(width int) []string {
	if m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty {
		return nil
	}
	return []string{m.questionLine(m.Theme(), width), m.sameAsLine(m.Theme(), width)}
}

func (m Model) questionLine(theme tui.Theme, width int) string {
	dim := lipgloss.NewStyle().Foreground(theme.TextFaint)
	accent := lipgloss.NewStyle().Foreground(theme.Accent).Bold(true)
	ns := lipgloss.NewStyle().Foreground(theme.Accent)
	count := lipgloss.NewStyle().Foreground(theme.TextPrimary)

	left := dim.Render("who can ") + accent.Render(m.verb) + " " + accent.Render(m.resource)
	if m.namespace != "" {
		left += dim.Render(" in ") + ns.Render(m.namespace)
	} else {
		left += dim.Render(" ") + lipgloss.NewStyle().Foreground(theme.Info).Render(tui.GlyphAllNS+" all namespaces")
	}
	subjectWord := "subjects"
	if len(m.result.Subjects) == 1 {
		subjectWord = "subject"
	}
	right := count.Render(strconv.Itoa(len(m.result.Subjects))) + " " + dim.Render(subjectWord)
	return insetStripLine(padBetween(left, right, stripInnerWidth(width)), width)
}

func (m Model) sameAsLine(theme tui.Theme, width int) string {
	dim := lipgloss.NewStyle().Foreground(theme.TextFaint)
	cmd := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	line := "kubectl auth can-i " + m.verb + " " + m.resource
	if m.namespace != "" {
		line += " -n " + m.namespace
	}
	return insetStripLine(dim.Render("same as: ")+cmd.Render(line), width)
}

// stripInnerWidth/insetStripLine give whocan's strips the same tui.
// FrameInset horizontal inset as the Frame's chrome bands and every other
// screen's strips (browse/routetable/nodedetail) — duplicated locally per
// the repo's existing pattern (tui/chrome.go's own doc comment on
// FrameInset: each screen applies it to its own Strips content).
func stripInnerWidth(width int) int {
	return max(width-2*tui.FrameInset, 0)
}

func insetStripLine(line string, width int) string {
	return components.Pad(strings.Repeat(" ", tui.FrameInset)+line, width)
}

func (m Model) Body(width, height int) string {
	switch m.state {
	case tui.TaskStateEmpty:
		return components.CenterLines([]string{m.emptyMessage()}, width, height)
	case tui.TaskStateReady:
		return m.rowsBody(m.Theme(), width, height)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

func (m Model) emptyMessage() string {
	msg := "no subject can " + m.verb + " " + m.resource
	if m.namespace != "" {
		msg += " in " + m.namespace
	}
	return msg
}

var whoCanColumns = []components.Column{
	{Title: "", Min: 2},
	{Title: "SUBJECT", Min: 12, Flex: true},
	{Title: "KIND", Min: 15},
	{Title: "VIA", Min: 20, Flex: true},
	{Title: "SCOPE", Min: 9, Align: components.AlignRight},
}

// tableDataRows is how many data rows rowsBody's own Table renders — the
// value update.go's clampOffset scrolls m.offset against, mirroring
// nodedetail/routetable's own tableDataRows (Table.visibleRowCount(),
// table.go: Height-1, no ShowHeaderRule here).
func (m Model) tableDataRows() int {
	height := tui.FrameBodyHeight(m.height, len(m.Strips(m.width)))
	rows := max(height-2, 1) - 1
	return max(rows, 1)
}

func (m Model) rowsBody(theme tui.Theme, width, height int) string {
	rows := make([]components.Row, 0, len(m.rows))
	for _, r := range m.rows {
		rows = append(rows, m.renderRow(theme, r))
	}
	t := components.Table{
		Columns:     whoCanColumns,
		Rows:        rows,
		Selected:    m.selected,
		Offset:      m.offset,
		Width:       width,
		Height:      max(height-2, 1),
		HeaderStyle: lipgloss.NewStyle().Foreground(theme.TextFaint),
		SortStyle:   lipgloss.NewStyle().Foreground(theme.Accent),
		SelBarStyle: lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle: lipgloss.NewStyle().Background(theme.SelBg),
		FooterStyle: lipgloss.NewStyle().Foreground(theme.TextGhost),
	}
	return "\n" + t.Render() + "\n" + t.FooterLine(width)
}

func (m Model) renderRow(theme tui.Theme, r whoCanRow) components.Row {
	good := lipgloss.NewStyle().Foreground(theme.Good)
	bad := lipgloss.NewStyle().Foreground(theme.Bad)
	name := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	nameDim := lipgloss.NewStyle().Foreground(theme.TextDim)
	secondary := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	scopeCluster := lipgloss.NewStyle().Foreground(theme.Info)
	scopeNS := lipgloss.NewStyle().Foreground(theme.TextFaint)

	glyph, glyphStyle := tui.GlyphRunning, good
	subjectText := r.subject.Name
	if r.subject.Namespace != "" {
		subjectText = r.subject.Namespace + "/" + r.subject.Name
	}
	subjectStyle := name
	if r.pinned {
		if r.granted {
			subjectText += " (you)"
		} else {
			glyph, glyphStyle = tui.GlyphFailed, bad
			subjectText += " (you)"
			subjectStyle = bad
		}
	}

	scopeText, scopeStyle := "namespace", scopeNS
	if r.subject.ClusterScope {
		scopeText, scopeStyle = "cluster", scopeCluster
	}
	if r.pinned && !r.granted {
		scopeText, scopeStyle = "–", nameDim
	}

	return components.Row{Cells: []components.Cell{
		{Text: glyph, Style: glyphStyle},
		{Text: subjectText, Style: subjectStyle},
		{Text: r.subject.Kind, Style: secondary},
		{Text: r.subject.Via, Style: nameDim},
		{Text: scopeText, Style: scopeStyle},
	}}
}

func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}
