package fluxtree

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
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

func (m Model) Header() tui.HeaderState {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	ghost := lipgloss.NewStyle().Foreground(theme.TextGhost)
	ghost2 := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	text := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)

	// No namespace segment: the chain crosses namespaces by design (a
	// Kustomization in one routinely reconciles from a GitRepository in
	// flux-system), so scoping the breadcrumb to one would misdescribe what
	// is on screen.
	crumbs := append(tui.BrandCrumbs(theme),
		tui.Crumb{Text: " │ ", Style: ghost2},
		tui.Crumb{Text: m.contextName(), Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "Flux", Style: text},
	)

	if m.state == tui.TaskStateLoading {
		elapsed := max(m.now.Sub(m.loadStartedAt), 0)
		return tui.HeaderState{
			Crumbs: crumbs,
			Conn: tui.ConnBadge{
				Text:  fmt.Sprintf("%s loading Flux · %.1fs", m.spinner.View(), elapsed.Seconds()),
				Style: lipgloss.NewStyle().Foreground(theme.Warn),
			},
		}
	}

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

// stripLineCount mirrors Strips so bodyRowCount can budget the table.
func (m Model) stripLineCount() int {
	n := 0
	if m.state == tui.TaskStateLoading || m.state == tui.TaskStateReady || m.state == tui.TaskStateEmpty {
		n++
	}
	if m.unreadableNote() != "" {
		n++
	}
	if m.execFeedback != "" {
		n++
	}
	return n
}

func (m Model) Strips(width int) []string {
	theme := m.Theme()
	var out []string
	if m.state == tui.TaskStateLoading {
		return []string{insetStripLine(m.loadingStripLine(theme, width), width)}
	}
	if m.state == tui.TaskStateReady || m.state == tui.TaskStateEmpty {
		out = append(out, insetStripLine(m.healthLine(theme, width), width))
	}
	if note := m.unreadableNote(); note != "" {
		out = append(out, insetStripLine(
			statusStyle(theme, resources.StatusWarn).Render(tui.GlyphWarning+" "+note), width))
	}
	if m.execFeedback != "" {
		out = append(out, insetStripLine(
			lipgloss.NewStyle().Foreground(theme.TextDim).Render(m.execFeedback), width))
	}
	return out
}

// unreadableNote names the Flux kinds the tree couldn't read, so a partial
// tree never passes for the whole one. Empty when every kind was read, and
// outside Ready/Empty, where the body already says what went wrong.
func (m Model) unreadableNote() string {
	if len(m.kindErrs) == 0 || (m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty) {
		return ""
	}
	parts := make([]string, 0, len(m.kindErrs))
	for _, e := range m.kindErrs {
		parts = append(parts, e.kind.APIKind()+": "+kindErrReason(e.err))
	}
	return "not shown · " + strings.Join(parts, " · ")
}

// kindErrReason is the short form of why a kind couldn't be read: a denial
// is the common case and reads as one word; anything else verbatim.
func kindErrReason(err error) string {
	if kube.IsPermissionError(err) {
		return "forbidden"
	}
	return err.Error()
}

// healthLine is the strip: the reconciler tally on the left (the number an
// operator scans first), the chain count on the right.
func (m Model) healthLine(theme tui.Theme, width int) string {
	numStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextDim)

	counts := m.health()
	segments := []struct {
		class resources.StatusClass
		n     int
		label string
		glyph string
	}{
		{resources.StatusOK, counts.OK, "ready", tui.GlyphRunning},
		{resources.StatusWarn, counts.Warn, "reconciling", "◌"},
		{resources.StatusFail, counts.Fail, "failed", tui.GlyphFailed},
		{resources.StatusNeutral, counts.Neutral, "suspended", tui.GlyphSuspended},
	}
	var parts []string
	for _, seg := range segments {
		if seg.n == 0 {
			continue
		}
		// Suspended keeps §30a's amber tone rather than Neutral's blue —
		// resources.Descriptor.HealthTone's reasoning, applied to the one
		// strip this screen draws for itself.
		class := seg.class
		if class == resources.StatusNeutral {
			class = resources.StatusWarn
		}
		parts = append(parts, statusStyle(theme, class).Render(seg.glyph)+" "+
			numStyle.Render(strconv.Itoa(seg.n))+" "+labelStyle.Render(seg.label))
	}
	left := strings.Join(parts, "   ")

	sources, reconcilers := m.countRows()
	right := labelStyle.Render(fmt.Sprintf("%s · %s",
		plural(sources, "source"), plural(reconcilers, "reconciler")))

	gap := width - 2*tui.FrameInset - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// insetStripLine aligns a strip's text to the chrome inset, exactly as
// tasks/routetable's own helper of the same name does.
func insetStripLine(line string, width int) string {
	return components.Pad(strings.Repeat(" ", tui.FrameInset)+line, width)
}

func statusStyle(theme tui.Theme, class resources.StatusClass) lipgloss.Style {
	switch class {
	case resources.StatusOK:
		return lipgloss.NewStyle().Foreground(theme.Good)
	case resources.StatusWarn:
		return lipgloss.NewStyle().Foreground(theme.Warn)
	case resources.StatusFail:
		return lipgloss.NewStyle().Foreground(theme.Bad)
	default:
		return lipgloss.NewStyle().Foreground(theme.Info)
	}
}

// bodyRowCount is how many table lines fit — mirrors Body's own budget so
// clampOffset scrolls against the real viewport.
func (m Model) bodyRowCount() int {
	// Frame's own bands (header, rule, keybar) plus this screen's strips and
	// the table's header row + rule.
	const chrome = 8
	return max(m.height-chrome-m.stripLineCount(), 1)
}

func (m Model) Body(width, height int) string {
	theme := m.Theme()
	switch m.state {
	case tui.TaskStateLoading:
		return m.loadingBody(width, height)
	case tui.TaskStateError:
		return components.CenterLines([]string{
			statusStyle(theme, resources.StatusFail).Render(tui.GlyphFailed + " " + m.feedback),
		}, width, height)
	case tui.TaskStateEmpty:
		return components.CenterLines([]string{
			lipgloss.NewStyle().Foreground(theme.TextDim).Render(m.emptyText()),
		}, width, height)
	}
	return m.table(theme, width, height).Render()
}

// table builds the §30b grid. The tree is drawn in the first column with a
// "└─ " lead on a child, which is why the whole screen is one table rather
// than a table per source: alignment across the chain is the point (you
// compare a source's revision with its reconcilers' at a glance).
func (m Model) table(theme tui.Theme, width, height int) components.Table {
	st := newStyles(theme)
	rows := make([]components.Row, 0, len(m.lines))
	for _, l := range m.lines {
		switch l.kind {
		case lineRow:
			rows = append(rows, m.dataRow(st, m.rows[l.row], l.child))
		case lineSub:
			rows = append(rows, components.Row{
				GroupHeader: "    " + l.text,
				GroupStyle:  st.subLine,
			})
		case lineFold:
			rows = append(rows, components.Row{
				GroupHeader: "  " + l.text,
				GroupStyle:  st.fold,
			})
		}
	}
	return components.Table{
		Columns: []components.Column{
			{Title: "", Min: 1},
			// Two flex columns, not one: NAME and REVISION are the two that
			// carry variable-length text (a long Kustomization name, a
			// revision plus its drift note), and at 80 columns a fixed
			// REVISION wide enough for "main@2b91f04 · source ahead" pushed
			// RECONCILED off the screen entirely. Sharing the slack lets
			// both degrade instead of one disappearing.
			{Title: "Source / Reconciler", Min: 20, Flex: true},
			{Title: "Kind", Min: 11},
			{Title: "Revision", Min: 20, Flex: true},
			{Title: "Reconciled", Min: 10},
		},
		Rows:           rows,
		Selected:       m.selected,
		Offset:         m.offset,
		Width:          width,
		Height:         height,
		HeaderStyle:    st.header,
		SelBarStyle:    st.selBar,
		SelRowStyle:    st.selRow,
		FooterStyle:    st.footer,
		ShowHeaderRule: true,
		RuleStyle:      st.rule,
	}
}

func (m Model) dataRow(st styles, row treeRow, child bool) components.Row {
	name := row.name
	if child {
		name = "└─ " + name
	}
	class := row.class
	if class == resources.StatusNeutral {
		// §30a's suspended amber, again — a paused reconciler is the object
		// drifting from git, not a parked one.
		class = resources.StatusWarn
	}
	nameStyle := st.name
	if child {
		nameStyle = st.child
	}
	return components.Row{Cells: []components.Cell{
		{Text: row.glyph, Style: st.status[class]},
		{Text: name, Style: nameStyle},
		{Text: row.kindLabel, Style: st.dim},
		{Text: row.revision, Style: st.dim},
		{Text: row.reconciled, Style: st.dim},
	}}
}

type styles struct {
	header  lipgloss.Style
	name    lipgloss.Style
	child   lipgloss.Style
	dim     lipgloss.Style
	subLine lipgloss.Style
	fold    lipgloss.Style
	selBar  lipgloss.Style
	selRow  lipgloss.Style
	footer  lipgloss.Style
	rule    lipgloss.Style
	status  map[resources.StatusClass]lipgloss.Style
}

func newStyles(theme tui.Theme) styles {
	return styles{
		header:  lipgloss.NewStyle().Foreground(theme.TextFaint),
		name:    lipgloss.NewStyle().Foreground(theme.Text),
		child:   lipgloss.NewStyle().Foreground(theme.TextSecondary),
		dim:     lipgloss.NewStyle().Foreground(theme.TextDim),
		subLine: lipgloss.NewStyle().Foreground(theme.TextFaint),
		fold:    lipgloss.NewStyle().Foreground(theme.TextFaint),
		selBar:  lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		selRow:  lipgloss.NewStyle().Foreground(theme.Text).Background(theme.SelBg),
		footer:  lipgloss.NewStyle().Foreground(theme.TextGhost),
		rule:    lipgloss.NewStyle().Foreground(theme.BorderSubtle),
		status: map[resources.StatusClass]lipgloss.Style{
			resources.StatusOK:      lipgloss.NewStyle().Foreground(theme.Good),
			resources.StatusWarn:    lipgloss.NewStyle().Foreground(theme.Warn),
			resources.StatusFail:    lipgloss.NewStyle().Foreground(theme.Bad),
			resources.StatusNeutral: lipgloss.NewStyle().Foreground(theme.Info),
		},
	}
}

// emptyText is the empty state's claim, narrowed when some kinds couldn't
// be read: then "none in this cluster" is more than kute knows.
func (m Model) emptyText() string {
	if len(m.kindErrs) > 0 {
		return "No Flux sources or reconcilers this identity can read."
	}
	return "No Flux sources or reconcilers in this cluster."
}
