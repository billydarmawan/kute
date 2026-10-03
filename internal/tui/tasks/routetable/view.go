package routetable

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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

	ctxName := "cluster unavailable"
	if m.session != nil && m.session.Location.Context != "" {
		ctxName = m.session.Location.Context
	}

	crumbs := append(tui.BrandCrumbs(theme),
		tui.Crumb{Text: " │ ", Style: ghost2},
		tui.Crumb{Text: ctxName, Style: dim},
	)
	if m.namespace != "" {
		crumbs = append(crumbs, tui.Crumb{Text: " › ", Style: ghost}, tui.Crumb{Text: m.namespace, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)})
	}
	crumbs = append(crumbs,
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: string(m.kind) + "/" + m.name, Style: text},
	)

	if m.state == tui.TaskStateLoading {
		elapsed := max(m.now.Sub(m.loadStartedAt), 0)
		return tui.HeaderState{
			Crumbs: crumbs,
			Conn: tui.ConnBadge{
				Text:  fmt.Sprintf("%s loading %s · %.1fs", m.spinner.View(), m.name, elapsed.Seconds()),
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

// stripLineCount mirrors Strips itself, so tableDataRows budgets the table
// viewport correctly (nodedetail's stripLineCount/Strips split). Every ready
// flavor renders exactly one top summary line — the richer per-flavor detail
// (TLS facts / the parent-Gateway listener) lives below the table instead,
// inside Body (see footerHeight/footerLine).
func (m Model) stripLineCount() int {
	if m.state == tui.TaskStateLoading {
		return 1
	}
	if m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty {
		return 0
	}
	if m.backendDeniedNote != "" {
		return 2
	}
	return 1
}

func (m Model) Strips(width int) []string {
	theme := m.Theme()
	switch {
	case m.state == tui.TaskStateLoading:
		return []string{insetStripLine(lipgloss.NewStyle().Foreground(theme.TextDim).Render("facts & rows enable when data lands"), width)}
	case m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty:
		return nil
	}
	switch m.flavor {
	case flavorIngress:
		lines := []string{insetStripLine(m.ingressSummaryLine(theme, width), width)}
		if m.backendDeniedNote != "" {
			lines = append(lines, m.backendDeniedNoteLine(theme, width))
		}
		return lines
	case flavorRoute:
		lines := []string{insetStripLine(m.routeSummaryLine(theme, width), width)}
		if m.backendDeniedNote != "" {
			lines = append(lines, m.backendDeniedNoteLine(theme, width))
		}
		return lines
	case flavorGateway:
		dim := lipgloss.NewStyle().Foreground(theme.TextDim)
		text := lipgloss.NewStyle().Foreground(theme.Text)
		class := m.gatewayClass
		if class == "" {
			class = "-"
		}
		return []string{insetStripLine(dim.Render("CLASS ")+text.Render(class), width)}
	}
	return nil
}

// backendDeniedNoteLine renders §5's inline flag when the Service/Pod
// caches backing the BACKENDS column aren't trustworthy — mirrors browse's
// own auxKindsDeniedNoteLine.
func (m Model) backendDeniedNoteLine(theme tui.Theme, width int) string {
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	return insetStripLine(warn.Render("⚠ "+m.backendDeniedNote), width)
}

// ingressSummaryLine is 23a's top strip: class + host/route counts, then the
// unhealthy-first ●/✕ tally, then a right-aligned "live" note (docs/design
// README.md §23a: "nginx · 2 hosts · 4 routes   ● 3 healthy   ✕ 1 broken").
func (m Model) ingressSummaryLine(theme tui.Theme, width int) string {
	secondary := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	class := m.ingressClass
	if class == "" {
		class = "-"
	}
	left := secondary.Render(fmt.Sprintf("%s · %d hosts · %d routes", class, m.ingressHostCount, len(m.rows)))

	healthy, broken := 0, 0
	for _, r := range m.rows {
		if r.class == resources.StatusFail {
			broken++
		} else {
			healthy++
		}
	}
	if healthy > 0 {
		left += "   " + lipgloss.NewStyle().Foreground(theme.Good).Render("●") + " " + dim.Render(fmt.Sprintf("%d healthy", healthy))
	}
	if broken > 0 {
		left += "   " + lipgloss.NewStyle().Foreground(theme.Bad).Render("✕") + " " + dim.Render(fmt.Sprintf("%d broken", broken))
	}

	right := dim.Render("backends resolved from the watch · live")
	return padBetween(left, right, stripInnerWidth(width))
}

// routeSummaryLine is 23b's top strip: hostnames + rule count, then the
// parent-Gateway accepted/rejected chip, then the right-aligned "live" note.
func (m Model) routeSummaryLine(theme tui.Theme, width int) string {
	secondary := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	left := secondary.Render(fmt.Sprintf("%s · %d rules", m.routeHostText, m.routeRuleCount))
	if m.parentHasStatus {
		glyph, glyphStyle := "✓", lipgloss.NewStyle().Foreground(theme.Good)
		if !m.parentAttached {
			glyph, glyphStyle = "✕", lipgloss.NewStyle().Foreground(theme.Bad)
		}
		left += "   " + glyphStyle.Render(glyph) + " " + dim.Render(m.parentText)
	} else {
		left += "   " + dim.Render(m.parentText)
	}

	right := dim.Render("backends resolved from the watch · live")
	return padBetween(left, right, stripInnerWidth(width))
}

// padBetween/stripInnerWidth mirror browse's own strip-line helpers (package-
// local since Go doesn't share unexported functions across packages).
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

func (m Model) Body(width, height int) string {
	switch m.state {
	case tui.TaskStateEmpty:
		return components.CenterLines([]string{m.emptyMessage()}, width, height)
	case tui.TaskStateReady:
		if m.flavor == flavorGateway {
			return m.listenersTable(width, height)
		}
		return m.routesTable(width, height)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

func (m Model) emptyMessage() string {
	if m.flavor == flavorGateway {
		return "no listeners on " + m.name
	}
	return "no routing rules on " + string(m.kind) + "/" + m.name
}

// hasFooter reports whether routesTable renders a below-table detail line
// (docs/design README.md §23a's "tls" strip / §23b's "parent" strip) — the
// Ingress flavor only carries one when the Ingress actually declares a TLS
// block; the Route flavor always has a parent line (falls back to "no parent
// status yet").
func (m Model) hasFooter() bool {
	switch m.flavor {
	case flavorIngress:
		return len(m.tlsFacts) > 0
	case flavorRoute:
		return true
	default:
		return false
	}
}

// footerHeight is the below-table block's line count (a blank separator plus
// the one detail line) — tableDataRows and routesTable share this so the
// scroll-offset math and the actual render agree on the table's budgeted
// height.
func (m Model) footerHeight() int {
	if m.hasFooter() {
		return 2
	}
	return 0
}

func (m Model) footerLine(theme tui.Theme) string {
	switch m.flavor {
	case flavorIngress:
		return m.ingressFooterLine(theme)
	case flavorRoute:
		return m.routeFooterLine(theme)
	default:
		return ""
	}
}

// ingressFooterLine is 23a's below-table "tls" strip: every TLS block's
// secret + expiry, "│"-separated on one line (docs/design README.md §23a:
// "a strip above the keybar names each secret").
func (m Model) ingressFooterLine(theme tui.Theme) string {
	if len(m.tlsFacts) == 0 {
		return ""
	}
	label := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("tls")
	dim := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	sep := lipgloss.NewStyle().Foreground(theme.TextGhost).Render(" │ ")

	parts := make([]string, 0, len(m.tlsFacts))
	for i, f := range m.tlsFacts {
		expiryStyle := dim
		switch f.class {
		case resources.StatusWarn:
			expiryStyle = lipgloss.NewStyle().Foreground(theme.Warn)
		case resources.StatusFail:
			expiryStyle = lipgloss.NewStyle().Foreground(theme.Bad)
		}
		secretStyle := dim
		if m.tlsFocused && i == m.tlsSelected {
			// verbs.FocusTLSStrip: the focused fact gets the standard
			// selection treatment, so ↵'s target is never ambiguous.
			secretStyle = secretStyle.Background(theme.SelBg)
			expiryStyle = expiryStyle.Background(theme.SelBg)
		}
		parts = append(parts, secretStyle.Render(f.secretName+" · ")+expiryStyle.Render(f.expiry))
	}
	return label + "  " + strings.Join(parts, sep)
}

// routeFooterLine is 23b's below-table "parent" strip: the accepted parent
// Gateway's listener + TLS cert detail, resolved at load time by
// resolveParentListenerDetail.
func (m Model) routeFooterLine(theme tui.Theme) string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint).Render("parent")
	dim := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	detail := m.parentListenerText
	if detail == "" {
		detail = m.parentText
	}
	return label + "  " + dim.Render(detail)
}

// tableDataRows is how many data rows the table can show at once — mirrors
// routesTable/listenersTable's own math so moveSelection's clampOffset
// scrolls against the viewport the render actually produces.
func (m Model) tableDataRows() int {
	body := tui.FrameBodyHeight(m.height, m.stripLineCount())
	return max(body-1-m.footerHeight(), 1)
}

// routeColumns is 23a's HOST+PATH/TLS/BACKEND/ENDPOINTS grid for the Ingress
// flavor, or 23b's MATCH/WEIGHT/BACKEND/ENDPOINTS grid for the Route flavor
// — the two share a position (column index 2) for their one flavor-specific
// column, so routeRowCells can fill it in without the caller branching too.
func (m Model) routeColumns() []components.Column {
	if m.flavor == flavorIngress {
		return []components.Column{
			{Title: "", Min: 1},
			{Title: "Host + Path", Min: 20, Flex: true},
			{Title: "TLS", Min: 9},
			{Title: "Backend", Min: 14, Flex: true},
			{Title: "Endpoints", Min: 11, Align: components.AlignRight},
		}
	}
	return []components.Column{
		{Title: "", Min: 1},
		{Title: "Match", Min: 20, Flex: true},
		{Title: "Weight", Min: 7, Align: components.AlignRight},
		{Title: "Backend", Min: 14, Flex: true},
		{Title: "Endpoints", Min: 11, Align: components.AlignRight},
	}
}

// routeRowCells renders one routeRow: the health glyph colors the whole row's
// tone (docs/design README.md §23a/§23b's "unhealthy-first" language) — a
// fail-class row's match/backend text swap to BadText/Bad, its ENDPOINTS cell
// to Bad; a warn-class row (0 ready) keeps normal match/backend text but
// warns its ENDPOINTS cell; an ok-class row's ENDPOINTS stays understated
// (TextDim), the ●/✓ health signal already carried the good news.
func (m Model) routeRowCells(theme tui.Theme, i int, r routeRow) components.Row {
	selected := i == m.selected
	bg := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(theme.SelBg)
		}
		return s
	}

	glyphStyle := bg(statusStyle(theme, r.class))

	matchColor, backendColor := theme.TextPrimary, theme.TextPrimary
	if selected {
		matchColor = theme.Text
	}
	if r.class == resources.StatusFail {
		matchColor, backendColor = theme.BadText, theme.Bad
	}
	matchStyle := bg(lipgloss.NewStyle().Foreground(matchColor))
	backendStyle := bg(lipgloss.NewStyle().Foreground(backendColor))

	matchText := r.match
	if matchText == "" {
		matchText = "└ same match"
		matchStyle = bg(lipgloss.NewStyle().Foreground(theme.TextDim))
	}

	endpointsStyle := bg(lipgloss.NewStyle().Foreground(theme.TextDim))
	switch r.class {
	case resources.StatusWarn:
		endpointsStyle = bg(lipgloss.NewStyle().Foreground(theme.Warn))
	case resources.StatusFail:
		endpointsStyle = bg(lipgloss.NewStyle().Foreground(theme.Bad))
	}

	var flavorCell components.Cell
	if m.flavor == flavorIngress {
		text, style := "–", bg(lipgloss.NewStyle().Foreground(theme.TextDim))
		if r.tlsText != "" {
			text = "● " + r.tlsText
			style = bg(statusStyle(theme, r.tlsClass))
		}
		flavorCell = components.Cell{Text: text, Style: style}
	} else {
		text, style := "—", bg(lipgloss.NewStyle().Foreground(theme.TextDim))
		if r.weightPct != "" {
			text = r.weightPct
			style = bg(lipgloss.NewStyle().Foreground(theme.TextPrimary))
			if r.match == "" {
				// A continuation row is always the split's minority/canary
				// backend (docs/design README.md §23b: "canary weight
				// yellow").
				style = bg(lipgloss.NewStyle().Foreground(theme.Warn))
			}
		}
		flavorCell = components.Cell{Text: text, Style: style}
	}

	return components.Row{Cells: []components.Cell{
		{Text: r.glyph, Style: glyphStyle},
		{Text: matchText, Style: matchStyle},
		flavorCell,
		{Text: r.backendText, Style: backendStyle},
		{Text: r.endpointsText, Style: endpointsStyle},
	}}
}

func (m Model) routesTable(width, height int) string {
	theme := m.Theme()

	footerLine := ""
	if m.hasFooter() {
		footerLine = m.footerLine(theme)
	}
	footerH := 0
	if footerLine != "" {
		footerH = 2
	}
	tableHeight := max(height-footerH, 1)

	rows := make([]components.Row, 0, len(m.rows))
	for i, r := range m.rows {
		rows = append(rows, m.routeRowCells(theme, i, r))
	}

	t := components.Table{
		Columns:     m.routeColumns(),
		Rows:        rows,
		Selected:    m.selected,
		Offset:      m.offset,
		Width:       width,
		Height:      tableHeight,
		HeaderStyle: lipgloss.NewStyle().Foreground(theme.TextFaint),
		SelBarStyle: lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle: lipgloss.NewStyle().Background(theme.SelBg),
		FooterStyle: lipgloss.NewStyle().Foreground(theme.TextGhost),
	}
	tableStr := t.Render()
	if footerLine == "" {
		return tableStr
	}
	return tableStr + "\n" + insetStripLine(footerLine, width)
}

func (m Model) listenersTable(width, height int) string {
	theme := m.Theme()
	cols := []components.Column{
		{Title: "Name", Min: 10, Flex: true},
		{Title: "Proto:Port", Min: 12},
		{Title: "Hostname", Min: 14},
		{Title: "TLS", Min: 16},
		{Title: "Attached", Min: 8, Align: components.AlignRight},
	}
	rows := make([]components.Row, 0, len(m.listeners))
	for i, l := range m.listeners {
		selected := i == m.selected
		text := lipgloss.NewStyle().Foreground(theme.TextPrimary)
		dim := lipgloss.NewStyle().Foreground(theme.TextDim)
		tls := statusStyle(theme, l.tlsClass)
		if selected {
			text = text.Background(theme.SelBg)
			dim = dim.Background(theme.SelBg)
			tls = tls.Background(theme.SelBg)
		}
		hostname := l.hostname
		if hostname == "" {
			hostname = "*"
		}
		rows = append(rows, components.Row{Cells: []components.Cell{
			{Text: l.name, Style: text},
			{Text: l.protoPort, Style: dim},
			{Text: hostname, Style: dim},
			{Text: l.tlsText, Style: tls},
			{Text: strconv.Itoa(l.attached), Style: dim},
		}})
	}

	t := components.Table{
		Columns:     cols,
		Rows:        rows,
		Selected:    m.selected,
		Offset:      m.offset,
		Width:       width,
		Height:      max(height-1, 1),
		HeaderStyle: lipgloss.NewStyle().Foreground(theme.TextFaint),
		SelBarStyle: lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle: lipgloss.NewStyle().Background(theme.SelBg),
		FooterStyle: lipgloss.NewStyle().Foreground(theme.TextGhost),
	}
	return t.Render()
}
