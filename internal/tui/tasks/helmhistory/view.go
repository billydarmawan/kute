package helmhistory

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
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

// Header is 18a's history breadcrumb: "… › <namespace> › <release> ›
// History" — the same trailing-segment shape 5b's "… › pod › logs" uses for
// a sub-view of an object.
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
		tui.Crumb{Text: m.namespace, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: m.name, Style: dim},
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: "History", Style: text},
	)

	if m.state == tui.TaskStateLoading {
		// 15a's loading-header treatment applied to a detail screen: a
		// counting timer instead of the usual conn/forward badges (see
		// nodedetail.Model.Header's equivalent branch).
		elapsed := max(m.now.Sub(m.loadStartedAt), 0)
		return tui.HeaderState{
			Crumbs: crumbs,
			Conn: tui.ConnBadge{
				Text:  fmt.Sprintf("%s loading %s history · %.1fs", m.spinner.View(), m.name, elapsed.Seconds()),
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

// stripLineCount is how many Strips lines the current state renders — kept
// in sync with Strips itself so tableDataRows budgets the rail viewport
// correctly (mirrors nodedetail's own stripLineCount/Strips split).
//
// Loading and ready deliberately render the same number of lines: the strip
// and its divider rule come out of tui.FrameBodyHeight, so a strip that
// appeared only once the data landed would push the rail down two rows at
// exactly the moment it filled in.
func (m Model) stripLineCount() int {
	switch m.state {
	case tui.TaskStateReady, tui.TaskStateLoading:
		return 1
	default:
		return 0
	}
}

func (m Model) Strips(width int) []string {
	if m.state == tui.TaskStateLoading {
		return []string{m.loadingStripLine(m.Theme(), width)}
	}
	if m.state != tui.TaskStateReady {
		return nil
	}
	if line, ok := m.overflowStripLine(m.Theme(), width); ok {
		return []string{line}
	}
	dim := lipgloss.NewStyle().Foreground(theme(m).TextFaint)
	count := lipgloss.NewStyle().Foreground(theme(m).TextPrimary)
	word := "revisions"
	if len(m.revisions) == 1 {
		word = "revision"
	}
	line := count.Render(strconv.Itoa(len(m.revisions))) + " " + dim.Render(word+" · newest first · current highlighted")
	if note := m.newerChartNote(theme(m), stripInnerWidth(width)-lipgloss.Width(line)-1); note != "" {
		// Right-aligned on the count line rather than a line of its own: the
		// strip is one line by contract (stripLineCount), and this is the
		// only slot on the screen that isn't per-revision — which is right,
		// because "a newer chart exists" is a fact about the release, not
		// about whichever revision the cursor happens to be on.
		return []string{insetStripLine(padBetween(line, note, stripInnerWidth(width)), width)}
	}
	return []string{insetStripLine(line, width)}
}

// newerChartNote is 18a's outdated signal repeated on the history rail: the
// version the local repo cache offers, when it's ahead of what's deployed.
// Empty — no placeholder line — when the release is current, when no repo
// cache knows the chart, or before the revisions have loaded.
//
// It shortens to fit avail rather than being dropped: padBetween discards a
// right side that doesn't fit outright, so the full phrasing would have made
// the whole finding vanish at 80 columns. The version is the part that
// matters, so it's the part that survives.
func (m Model) newerChartNote(th tui.Theme, avail int) string {
	current, ok := m.currentRevision()
	if !ok || !current.Outdated() {
		return ""
	}
	dim := lipgloss.NewStyle().Foreground(th.TextDim)
	warn := lipgloss.NewStyle().Foreground(th.Warn)
	version := current.LatestCell()

	forms := []struct{ prefix, suffix string }{
		{"newer chart available: ", " (" + current.LatestRepo + ")"},
		{"newer chart available: ", ""},
		{"→ ", " available"},
		{"→ ", ""},
	}
	if current.LatestRepo == "" || current.LatestAmbiguous {
		// No single repo to credit (or two repos disagree — LatestCell
		// already carries the "?" that says so).
		forms = forms[1:]
	}
	for _, f := range forms {
		if len(f.prefix)+lipgloss.Width(version)+len(f.suffix) <= avail {
			return dim.Render(f.prefix) + warn.Render(version) + dim.Render(f.suffix)
		}
	}
	return ""
}

// currentRevision is the release's live state — the highest revision, which
// is the first row since the rail is newest-first. The chart-version
// comparison belongs to it and not to the selected row: an older revision
// being behind the repo is simply what "older revision" means.
func (m Model) currentRevision() (kube.HelmRelease, bool) {
	if len(m.revisions) == 0 {
		return kube.HelmRelease{}, false
	}
	return m.revisions[0], true
}

// statusOverflow reports the selected revision's STATUS text when the rail
// can't render it whole, so something else can. A failure reason is the
// diagnosis (§18a: "failed carries the reason verbatim"), and even with
// STATUS flexing, real Helm descriptions —
// `Upgrade "x" failed: post-upgrade hooks failed: job failed:
// BackoffLimitExceeded` — outrun any column at any terminal width.
//
// The width comes from the same components.Table the rail renders through
// rather than being re-derived here: the flex arithmetic lives in one place,
// so this can't start claiming an overflow that isn't there.
func (m Model) statusOverflow(width int) (kube.HelmRelease, string, bool) {
	rev, ok := m.selectedRevision()
	if !ok {
		return kube.HelmRelease{}, "", false
	}
	cell := rev.StatusCell()
	widths := (components.Table{Columns: railColumns}).ColumnWidths(width)
	for i, col := range railColumns {
		if col.Title != "STATUS" {
			continue
		}
		if lipgloss.Width(cell) > widths[i] {
			return rev, cell, true
		}
		break
	}
	return kube.HelmRelease{}, "", false
}

// overflowStripLine is where that reason goes: the strip, in place of the
// count line, for as long as a revision whose status doesn't fit is
// selected. It replaces rather than adds a line — the strip's height is
// budgeted by stripLineCount, and a strip that grew on selection would shove
// the rail down a row every time the cursor passed a failed revision.
//
// It appears only while there is genuinely something unreadable on the row;
// otherwise the count line stands, since restating a status the user can
// already read in full is noise.
func (m Model) overflowStripLine(theme tui.Theme, width int) (string, bool) {
	rev, cell, ok := m.statusOverflow(width)
	if !ok {
		return "", false
	}
	glyphStyle, glyph := statusGlyph(theme, rev.Status)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)

	left := glyphStyle.Render(glyph) + " " +
		faint.Render(fmt.Sprintf("revision %d", rev.Revision)) + " " +
		dim.Render("· "+cell)
	// Truncate rather than wrap: the strip is one line by contract, and a
	// reason long enough to overflow even here is already past the point of
	// being read at a glance — 'y' on the row has the whole object.
	return insetStripLine(components.Truncate(left, stripInnerWidth(width)), width), true
}

func theme(m Model) tui.Theme { return m.Theme() }

func insetStripLine(line string, width int) string {
	return components.Pad(strings.Repeat(" ", tui.FrameInset)+line, width)
}

// stripInnerWidth/padBetween give the loading strip the same inset and
// left/right split every other screen's strip uses, duplicated per the
// repo's package-local-seam convention (nodedetail's own copies).
func stripInnerWidth(width int) int {
	return max(width-2*tui.FrameInset, 0)
}

func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) Body(width, height int) string {
	if m.actions.Active() && m.actions.Tier() == actions.TierModal {
		// TierInline (non-prod: "Rollback inherits 8b friction" — inline
		// y/N, never a modal) keeps rendering the rail underneath, with the
		// confirm text in the keybar's RightNote (keys.go) — only the PROD
		// escalation gets this type-the-name modal.
		return m.confirmBody(width, height)
	}
	switch m.state {
	case tui.TaskStateEmpty:
		return components.CenterLines([]string{"no revisions found — the release secrets may have been deleted"}, width, height)
	case tui.TaskStateReady:
		return m.railBody(m.Theme(), width, height)
	case tui.TaskStateLoading:
		return m.loadingBody(m.Theme(), width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// confirmBody renders 18a's PROD "type the release name to confirm" modal —
// the same TypeModalStyles/TypeNameModal shape as 8b's delete confirm and
// 16b's rollout-undo (timeline's rollbackConfirmModal), with the exact helm
// command as its will-run line.
func (m Model) confirmBody(width, height int) string {
	theme := m.Theme()
	title, detail, target := "Confirm", "", m.name
	if pending := m.actions.Pending(); pending != nil {
		title = "⇅ " + pending.Label
		target = pending.Scope.ResourceName
		detail = "will run: " + kube.HelmRollbackCommandString(m.session.CommandTarget(), pending.Scope.Namespace, pending.Scope.ResourceName, pending.Scope.Revision)
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
	return components.TypeNameModal(title, "", detail, target, m.actions.TypedInput(), "rollback", m.isProd(), styles, width, height)
}

// tableDataRows is how many data rows railBody's own Table renders — the
// value update.go's clampOffset scrolls m.offset against, mirroring
// nodedetail/routetable's own tableDataRows (Table.visibleRowCount(),
// table.go: Height-1, no ShowHeaderRule here).
func (m Model) tableDataRows() int {
	height := tui.FrameBodyHeight(m.height, m.stripLineCount())
	rows := max(height-2, 1) - 1
	return max(rows, 1)
}

// railColumns puts STATUS after CHART and lets it flex, which is the
// opposite of the list's shared skeleton and deliberate: §18a says a failed
// revision "carries the reason verbatim", and the reason is the whole point
// of opening history on a broken release. A fixed STATUS clipped it to
// "failed · Upgrad…" while CHART soaked up the leftover width to render a
// chart name the user already knows. Chart names are the predictable half
// of the row, so they take the truncation now.
//
// The order follows: the flex column sits next to the right-aligned UPDATED
// so the elastic gap is between them, and CHART/REV stay left as a stable
// pair of fixed columns.
var railColumns = []components.Column{
	{Title: "", Min: 2},
	{Title: "REV", Min: 13},
	{Title: "CHART", Min: 22},
	{Title: "STATUS", Min: 16, Flex: true},
	{Title: "UPDATED", Min: 10, Align: components.AlignRight},
}

// railBody renders 16b's revision rail: newest-first, one line per
// revision, the current (index 0) revision's REV cell bright/bold instead
// of the dim treatment every other revision gets — "the current one
// highlighted" (docs/design README.md §16b).
func (m Model) railBody(theme tui.Theme, width, height int) string {
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	current := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)

	rows := make([]components.Row, 0, len(m.revisions))
	for i, rev := range m.revisions {
		glyphStyle, glyph := statusGlyph(theme, rev.Status)
		revStyle := dim
		revText := strconv.Itoa(rev.Revision)
		if i == 0 {
			revStyle = current
			revText += " (current)"
		}
		updated := "–"
		if !rev.Updated.IsZero() {
			updated = shortAge(time.Since(rev.Updated)) + " ago"
		}
		rows = append(rows, components.Row{Cells: []components.Cell{
			{Text: glyph, Style: glyphStyle},
			{Text: revText, Style: revStyle},
			{Text: rev.Chart + " " + rev.ChartVersion, Style: dim},
			{Text: rev.StatusCell(), Style: glyphStyle},
			{Text: updated, Style: dim},
		}})
	}

	t := components.Table{
		Columns:     railColumns,
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

// statusGlyph maps a release status to its 2a glyph and severity color —
// the one place that mapping lives for this screen, so the strip's overflow
// line can't disagree with the row it's quoting.
func statusGlyph(theme tui.Theme, status string) (lipgloss.Style, string) {
	switch helmStatusClass(status) {
	case "warn":
		return lipgloss.NewStyle().Foreground(theme.Warn), tui.GlyphPending
	case "fail":
		return lipgloss.NewStyle().Foreground(theme.Bad), tui.GlyphFailed
	case "neutral":
		return lipgloss.NewStyle().Foreground(theme.Info), tui.GlyphCompleted
	default:
		return lipgloss.NewStyle().Foreground(theme.Good), tui.GlyphRunning
	}
}

// helmStatusClass mirrors resources.helmReleaseStatusClass without an
// import (resources is the caller of this package's sibling, not the other
// way — see model.go's package doc on the seam boundary), returning the
// same three-letter tokens railBody's glyph switch reads.
func helmStatusClass(status string) string {
	switch {
	case status == "deployed":
		return "ok"
	case strings.HasPrefix(status, "pending-"):
		return "warn"
	case status == "failed":
		return "fail"
	default:
		return "neutral"
	}
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
