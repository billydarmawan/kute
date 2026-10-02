package certchain

import (
	"fmt"
	"image/color"
	"strings"
	"time"

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

// Header is the mockup's own breadcrumb shape: "cluster › namespace ·
// certificate/name › Chain" — the object segment lowercased kind/name, plus
// a final "Chain" segment past the object itself, same idiom as secretdata's
// own "› Data" segment.
func (m Model) Header() tui.HeaderState {
	theme := m.Theme()
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	nsStyle := lipgloss.NewStyle().Foreground(theme.TextPrimary)
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
	)
	if m.namespace != "" {
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: m.namespace, Style: nsStyle})
	}
	crumbs = append(crumbs,
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "certificate/" + m.name, Style: secondary},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "Chain", Style: text})

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

// Strips is §35a's own status line — the Certificate's own Ready read,
// the honest attempt count, and a note that the chain is live — rendered
// only once the Certificate isn't simply Ready=True ("zero chrome until
// earned"). Deliberately missing the mockup's "next retry in 2m": unlike
// Flux's spec.retryInterval/spec.interval, cert-manager publishes no backoff
// field a countdown could honestly be built from (see load.go's package
// doc), so it's omitted rather than guessed.
func (m Model) Strips(width int) []string {
	if len(m.chain) == 0 || m.chain[0].Class == resources.StatusOK {
		return nil
	}
	theme := m.Theme()
	root := m.chain[0]

	left := lipgloss.NewStyle().Foreground(statusColor(theme, root.Class)).Render(root.Glyph) + " " +
		lipgloss.NewStyle().Foreground(theme.TextSecondary).Render(root.StateText)
	if m.attempts > 1 {
		left += "   " + lipgloss.NewStyle().Foreground(theme.TextDim).Render(fmt.Sprintf("attempt %d", m.attempts))
	}
	right := lipgloss.NewStyle().Foreground(theme.TextGhost).Render("chain resolved from the watch · live")

	gap := max(width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return []string{left + strings.Repeat(" ", gap) + right}
}

func (m Model) Body(width, height int) string {
	switch m.state {
	case tui.TaskStateReady:
		return m.readyBody(width, height)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// readyBody stacks §35a's bands: failure banner (only when something in the
// chain has genuinely failed — "zero chrome until earned"), the chain
// table, a rule, then the refs strip.
func (m Model) readyBody(width, height int) string {
	var b strings.Builder
	if banner := m.failureBanner(width); banner != "" {
		b.WriteString(banner)
		b.WriteString("\n\n")
	}
	b.WriteString(m.chainTable(width, height).Render())
	if refs := m.refsStrip(); refs != "" {
		b.WriteString("\n")
		b.WriteString(m.rule(width))
		b.WriteString("\n")
		b.WriteString(refs)
	}
	return b.String()
}

// rule is the horizontal divider the mockup draws above the refs strip
// (its own border-top), matching components.Table's own header-rule idiom.
func (m Model) rule(width int) string {
	return lipgloss.NewStyle().Foreground(m.Theme().BorderSubtle).Render(strings.Repeat("─", width))
}

// failureBanner is §35a's top band — poddetail's own "Last termination"
// banner recipe exactly: a solid tinted block with a left accent bar, never
// a bordered box, since a box-drawing glyph can't be tinted on its inner
// half alone (poddetail/view.go's terminationBanner doc comment). §35a's own
// note calls this "the same move as pod detail putting last-termination
// first."
func (m Model) failureBanner(width int) string {
	if m.fail == nil {
		return ""
	}
	theme := m.Theme()
	title := lipgloss.NewStyle().Foreground(theme.Bad).Background(theme.ErrBannerBg).Bold(true).
		Render(tui.GlyphFailed + " " + failureTitle(m.fail.Kind))

	// Every span below — including bare gaps — must carry
	// Background(theme.ErrBannerBg) itself: each Render() call closes with
	// its own ANSI reset, which wipes the outer banner style's background
	// until the next explicit one, so a plain " " concatenated in between
	// prints in whatever the terminal's own background is instead of the
	// banner's tint (poddetail's terminationBanner draws the same line).
	gapStyle := lipgloss.NewStyle().Background(theme.ErrBannerBg)

	var factParts []string
	if m.fail.Detail != "" {
		factParts = append(factParts, m.fail.Detail)
	}
	facts := ""
	if len(factParts) > 0 {
		facts = lipgloss.NewStyle().Foreground(theme.BadMuted).Background(theme.ErrBannerBg).
			Render("  " + strings.Join(factParts, " · "))
	}

	// "attempt N" lives in Strips' own status line, not here — the mockup's
	// failure card right side names only the age and the parent reference.
	var refParts []string
	if !m.fail.Since.IsZero() {
		refParts = append(refParts, shortAge(m.now.Sub(m.fail.Since))+" ago")
	}
	if m.fail.ParentName != "" {
		refParts = append(refParts, strings.ToLower(string(m.fail.ParentKind))+"/"+m.fail.ParentName)
	}
	right := ""
	if len(refParts) > 0 {
		gap := width - lipgloss.Width(title) - lipgloss.Width(facts) - lipgloss.Width(strings.Join(refParts, " · ")) - 4
		right = gapStyle.Render(strings.Repeat(" ", max(gap, 2)))
		right += lipgloss.NewStyle().Foreground(theme.TextDim).Background(theme.ErrBannerBg).
			Render(strings.Join(refParts, " · "))
	}

	bodyStyle := lipgloss.NewStyle().Foreground(theme.BadText).Background(theme.ErrBannerBg)
	var body strings.Builder
	for _, l := range wrap(m.fail.Message, width-4) {
		if body.Len() > 0 {
			body.WriteString("\n")
		}
		body.WriteString(bodyStyle.Render(l))
	}

	// The mockup's own caption under the message — reliable to state
	// verbatim, unlike a retry countdown (see below): it's a description of
	// what buildFailure just did (walk the chain deepest-first, quote that
	// node's own status), not a claim about cluster data, so it holds
	// regardless of which kind ended up being the deepest failure.
	caption := lipgloss.NewStyle().Foreground(theme.TextGhost).Background(theme.ErrBannerBg).
		Render("message verbatim from " + strings.ToLower(string(m.fail.Kind)) + " status · deepest failure in the chain, found for you")

	content := title + facts + right + "\n" + body.String() + "\n" + caption
	accent := lipgloss.Border{Left: "▌"}
	style := lipgloss.NewStyle().
		Background(theme.ErrBannerBg).
		Border(accent, false, false, false, true).
		BorderForeground(theme.ErrBannerBorder).
		BorderBackground(theme.ErrBannerBg).
		Padding(1, 1).
		Width(width)
	return style.Render(content)
}

// chainTable is §35a's middle band: glyph · tree-indented name · state ·
// age. Built on components.Table rather than hand-padded strings — the tree
// prefix is baked into the name cell exactly as fluxtree's own "└─ " recipe
// does, but the column widths themselves need components.Table's
// ansi.StringWidth-aware padding: a hand-rolled len()-based pad() undercounts
// every row containing a multi-byte glyph like "└" (3 UTF-8 bytes, 1 display
// column), which misaligns STATE/AGE by exactly that difference on every
// non-root row.
func (m Model) chainTable(width, height int) components.Table {
	theme := m.Theme()
	headerStyle := lipgloss.NewStyle().Foreground(theme.TextFaint)
	nameStyle := lipgloss.NewStyle().Foreground(theme.Text)
	dimStyle := lipgloss.NewStyle().Foreground(theme.TextDim)

	rows := make([]components.Row, len(m.chain))
	for i, n := range m.chain {
		indent := ""
		if n.Depth > 0 {
			indent = strings.Repeat("  ", n.Depth-1) + "└ "
		}
		name := indent + strings.ToLower(string(n.Kind)) + "/" + n.Name
		stateStyle := lipgloss.NewStyle().Foreground(statusColor(theme, n.Class))
		rows[i] = components.Row{Cells: []components.Cell{
			{Text: n.Glyph, Style: lipgloss.NewStyle().Foreground(statusColor(theme, n.Class))},
			{Text: name, Style: nameStyle},
			{Text: n.StateText, Style: stateStyle},
			{Text: shortAge(m.now.Sub(n.Created)), Style: dimStyle},
		}}
	}
	h := max(height, len(rows)+2)
	return components.Table{
		Columns: []components.Column{
			{Title: "", Min: 1},
			{Title: "Chain", Min: 20, Flex: true},
			{Title: "State", Min: 16, Flex: true},
			{Title: "Age", Min: 6, Align: components.AlignRight},
		},
		Rows:           rows,
		Selected:       m.selected,
		Width:          width,
		Height:         h,
		HeaderStyle:    headerStyle,
		SelBarStyle:    lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle:    lipgloss.NewStyle().Foreground(theme.Text).Background(theme.SelBg),
		ShowHeaderRule: true,
		RuleStyle:      lipgloss.NewStyle().Foreground(theme.BorderSubtle),
	}
}

// refsStrip is §35a's bottom band: the target Secret's existence and the
// Issuer/ClusterIssuer's own Ready state, both selectable.
func (m Model) refsStrip() string {
	theme := m.Theme()
	label := lipgloss.NewStyle().Foreground(theme.TextFaint)

	var parts []string
	idx := len(m.chain)
	if m.haveSecret {
		parts = append(parts, m.refText(m.secretRef, idx == m.selected))
		idx++
	}
	if m.haveIssuer {
		parts = append(parts, m.refText(m.issuerRef, idx == m.selected))
	}
	if len(parts) == 0 {
		return ""
	}
	return label.Render("refs") + "  " + strings.Join(parts, "  │  ")
}

func (m Model) refText(ref refInfo, selected bool) string {
	theme := m.Theme()
	nameStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	if selected {
		nameStyle = nameStyle.Bold(true).Foreground(theme.Accent)
	}
	statusStyle := lipgloss.NewStyle().Foreground(theme.Good)
	if ref.External {
		statusStyle = lipgloss.NewStyle().Foreground(theme.TextDim)
	} else if !ref.Exists || (ref.HasReady && !ref.Ready) {
		statusStyle = lipgloss.NewStyle().Foreground(theme.Bad)
	}
	return nameStyle.Render(ref.Label) + " · " + statusStyle.Render(ref.StatusText)
}

func statusColor(theme tui.Theme, c resources.StatusClass) color.Color {
	switch c {
	case resources.StatusOK:
		return theme.Good
	case resources.StatusWarn:
		return theme.Warn
	case resources.StatusFail:
		return theme.Bad
	default:
		return theme.TextFaint
	}
}

func wrap(s string, width int) []string {
	if width <= 0 || len(s) <= width {
		return []string{s}
	}
	var out []string
	for len(s) > width {
		cut := strings.LastIndex(s[:width], " ")
		if cut <= 0 {
			cut = width
		}
		out = append(out, strings.TrimSpace(s[:cut]))
		s = strings.TrimSpace(s[cut:])
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func shortAge(d time.Duration) string {
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
