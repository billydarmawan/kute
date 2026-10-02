package timeline

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

// Header is 16a's "… › <namespace> › Timeline" + "last 30m" tag, or 16b's
// "… › <object> › Timeline" (docs/design README.md §16a/§16b).
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
	if m.objectScoped() {
		if m.namespace != "" {
			crumbs = append(crumbs,
				tui.Crumb{Text: " › ", Style: ghost},
				tui.Crumb{Text: m.namespace, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
			)
		}
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: string(m.objectKind) + "/" + m.objectName, Style: dim},
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: "Timeline", Style: text},
		)
	} else {
		nsText := m.namespace
		if nsText == "" {
			nsText = tui.GlyphAllNS + " all namespaces"
		}
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: nsText, Style: lipgloss.NewStyle().Foreground(theme.TextPrimary)},
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: "Timeline", Style: text},
			tui.Crumb{Text: " · " + windowLabel(m.window), Style: ghost},
		)
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

func (m Model) Strips(width int) []string {
	if m.state != tui.TaskStateReady && m.state != tui.TaskStateEmpty {
		return nil
	}
	lines := []string{m.summaryLine(m.Theme(), width)}
	if m.filterActive {
		lines = append(lines, m.filterStripLine(m.Theme(), width))
	}
	return lines
}

// counts tallies m.rows (already window/filter-applied by recomputeVisible)
// into the strip's per-kind totals — reading the same rows Body walks, so
// the strip and the feed can never disagree about what's currently shown.
func (m Model) counts() (rollouts, restarts, warnings int) {
	for _, e := range m.rows {
		switch e.Kind {
		case kube.TimelineRollout:
			rollouts++
		case kube.TimelineRestart:
			restarts++
		case kube.TimelineEvent:
			if e.Severity == "Warning" {
				warnings++
			}
		}
	}
	return rollouts, restarts, warnings
}

// summaryLine is 16a's "⇅ 1 rollout · ↺ 41 restarts · ▲ warnings …" strip,
// with a right-aligned yellow correlation callout ("first BackOff 45s after
// rollout of nva-worker") once a rollout has a warning-grade entry after it
// — the same insight changeOffset highlights inline on that row (docs/design
// README.md §16a: "echoed in the health strip").
func (m Model) summaryLine(theme tui.Theme, width int) string {
	rollout := lipgloss.NewStyle().Foreground(theme.Accent)
	restart := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	dim := lipgloss.NewStyle().Foreground(theme.TextFaint)
	num := lipgloss.NewStyle().Foreground(theme.TextPrimary)

	rollouts, restarts, warnings := m.counts()
	parts := make([]string, 0, 3)
	if rollouts > 0 {
		parts = append(parts, rollout.Render(tui.GlyphRollout)+" "+num.Render(strconv.Itoa(rollouts))+" "+dim.Render(pluralize(rollouts, "rollout")))
	}
	parts = append(parts, restart.Render(tui.GlyphRestarts)+" "+num.Render(strconv.Itoa(restarts))+" "+dim.Render("restarts"))
	parts = append(parts, warn.Render(tui.GlyphWarning)+" "+num.Render(strconv.Itoa(warnings))+" "+dim.Render("warnings"))

	left := strings.Join(parts, "   ")
	right := dim.Render(windowLabel(m.window) + " · merged feed")
	if !m.objectScoped() {
		if trouble, rollout, ok := m.firstTroubleAfterRollout(); ok {
			_, depName := splitObject(rollout.Object)
			offset := shortAge(trouble.Time.Sub(rollout.Time))
			right = lipgloss.NewStyle().Foreground(theme.Warn).Render(
				fmt.Sprintf("first %s %s after rollout of %s", trouble.Reason, offset, depName))
		}
	}
	return fillLine(padBetween(left, right, width), width, false, theme)
}

// firstTroubleAfterRollout finds the latest TimelineRollout in m.rows and,
// among rows strictly after it that are a restart or a warning-severity
// event, the one closest in time — the "first sign of trouble" the summary
// strip's callout and changeOffset's yellow highlight both point at (they
// read this same function so they can never disagree).
func (m Model) firstTroubleAfterRollout() (trouble, rollout kube.TimelineEntry, ok bool) {
	var latest *kube.TimelineEntry
	for i := range m.rows {
		r := &m.rows[i]
		if r.Kind != kube.TimelineRollout {
			continue
		}
		if latest == nil || r.Time.After(latest.Time) {
			latest = r
		}
	}
	if latest == nil {
		return kube.TimelineEntry{}, kube.TimelineEntry{}, false
	}
	var best *kube.TimelineEntry
	for i := range m.rows {
		e := &m.rows[i]
		if !e.Time.After(latest.Time) || !isTrouble(*e) {
			continue
		}
		if best == nil || e.Time.Before(best.Time) {
			best = e
		}
	}
	if best == nil {
		return kube.TimelineEntry{}, *latest, false
	}
	return *best, *latest, true
}

// isTrouble reports whether e is a "sign of trouble" — a container restart,
// or a warning-severity event.
func isTrouble(e kube.TimelineEntry) bool {
	return e.Kind == kube.TimelineRestart || (e.Kind == kube.TimelineEvent && e.Severity == "Warning")
}

func pluralize(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// filterStripLine mirrors tasks/events' own filter strip, including the "N
// hidden by filter — esc to clear" notice once the query hides entries
// (docs/design system-wide interactions: "items never silently disappear").
func (m Model) filterStripLine(theme tui.Theme, width int) string {
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	total, matched := m.filterBaselineRows, len(m.rows)
	right := dim.Render(fmt.Sprintf("%d matched", matched))
	if matched < total {
		right = faint.Render(fmt.Sprintf("%d hidden by filter — esc to clear   ", total-matched)) + right
	}
	inputWidth := max(width-2-lipgloss.Width(right)-1, 1)
	left := accent.Render("/ ") + m.filterInput.ViewWidth(inputWidth)
	return fillLine(padBetween(left, right, width), width, false, theme)
}

func windowLabel(d time.Duration) string {
	switch d {
	case 30 * time.Minute:
		return "last 30m"
	case time.Hour:
		return "last hour"
	case 6 * time.Hour:
		return "last 6h"
	case 24 * time.Hour:
		return "last 24h"
	default:
		return "all time"
	}
}

func (m Model) Body(width, height int) string {
	// 16b's PROD rollback escalates to the type-the-deployment-name modal —
	// a floating card over the still-rendered rail/feed, the same "only the
	// PROD escalation gets this floating card" shape helmhistory.Body uses
	// for its own Helm rollback (TierInline stays inline: the rail/feed
	// render normally below, with the confirm text in the keybar's
	// RightNote instead).
	if m.actionsCtl.Active() && m.actionsCtl.Tier() == actions.TierModal {
		return m.rollbackConfirmModal(width, height)
	}
	switch m.state {
	case tui.TaskStateEmpty:
		return components.CenterLines([]string{m.emptyMessage()}, width, height)
	case tui.TaskStateReady:
		return m.timelineBody(m.Theme(), width, height)
	case tui.TaskStateLoading:
		style := lipgloss.NewStyle().Foreground(m.Theme().Accent)
		return components.LoadingBody(m.spinner, style, m.feedback, width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// rollbackConfirmModal renders 16b's PROD "type the deployment name to
// confirm" card for 'R' rollback — same TypeModalStyles/TypeNameModal shape
// as 8b's delete confirm (poddetail's own deleteConfirmModal), reusing the
// "rollback" actionVerb TypeNameModal now takes so its key row reads "↵
// rollback (when name matches)" rather than the delete-specific default.
func (m Model) rollbackConfirmModal(width, height int) string {
	theme := m.Theme()
	title := "Confirm"
	target := m.railDeployment
	detail := ""
	if pending := m.actionsCtl.Pending(); pending != nil {
		title = "⇅ " + pending.Label
		target = pending.Scope.ResourceName
		detail = "will run: " + kube.RolloutUndoCommandString(pending.Scope.Namespace, pending.Scope.ResourceName, pending.Scope.Revision)
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
	return components.TypeNameModal(title, "", detail, target, m.actionsCtl.TypedInput(), "rollback", m.isProd(), styles, width, height)
}

func (m Model) emptyMessage() string {
	if m.objectScoped() {
		return "nothing changed for " + string(m.objectKind) + "/" + m.objectName + " in " + windowLabel(m.window)
	}
	if m.namespace == "" {
		return "nothing changed cluster-wide in " + windowLabel(m.window)
	}
	return "nothing changed in " + m.namespace + " in " + windowLabel(m.window)
}

// timelineBody puts 16b's revision rail in a fixed-width left sidebar next
// to the merged feed, divided by a vertical rule — the "vertical rail"
// idiom (docs/design README.md §16b: "deployment revisions as a vertical
// rail") sitting alongside the same one-clock feed 16a renders alone, not
// stacked above it.
func (m Model) timelineBody(theme tui.Theme, width, height int) string {
	if len(m.rail) == 0 {
		lines := []string{m.feedHeader(theme, width), headerRule(theme, width)}
		feedHeight := max(height-len(lines), 1)
		lines = append(lines, m.feedLines(theme, width, feedHeight)...)
		return strings.Join(lines, "\n")
	}

	railWidth := m.railWidth(width)
	feedWidth := max(width-railWidth-1, 8)

	rail := m.railColumnLines(theme, railWidth, height)
	// feedLines' own entries aren't always one visual line each (the empty
	// state's components.CenterLines embeds newlines in a single returned
	// element) — split on "\n" to flatten before pairing index-for-index
	// with rail, or an empty feed shifts every card line out of alignment.
	feedText := m.feedHeader(theme, feedWidth) + "\n" + headerRule(theme, feedWidth) + "\n" +
		strings.Join(m.feedLines(theme, feedWidth, max(height-2, 1)), "\n")
	feed := strings.Split(feedText, "\n")
	for len(feed) < height {
		feed = append(feed, fillLine("", feedWidth, false, theme))
	}
	feed = feed[:height]

	divider := lipgloss.NewStyle().Foreground(theme.TextGhost2).Render("│")
	lines := make([]string, height)
	for i := range height {
		lines[i] = rail[i] + divider + feed[i]
	}
	return strings.Join(lines, "\n")
}

// Column widths shared between feedHeader and renderRow/renderRolloutDivider
// so the header always lines up with the data underneath (docs/design
// README.md §16a/§16b's exact grid: "56px 20px 64px minmax(0,1fr) 76px" in
// 16a, no +CHANGE/OBJECT columns in 16b).
const (
	feedWhenW   = 6
	feedGlyphW  = 1
	feedChangeW = 7
	feedObjectW = 20
)

// feedWhatWidth is the WHAT column's flex width — every other column's
// fixed width (plus the 2-space gaps between them) subtracted from the
// available width.
func (m Model) feedWhatWidth(width int) int {
	used := 2 + feedWhenW + 2 + feedGlyphW + 2
	if !m.objectScoped() {
		used += feedChangeW + 2 + feedObjectW + 2
	}
	return max(width-used, 8)
}

// headerRule is a full-width divider under a column header — the same
// ShowHeaderRule/RuleStyle idiom components.Table already draws for
// browse/nodedetail's own tables (theme.TextGhost2, quieter than the row
// grid's own BorderSubtle), reused here since the feed header and the rail's
// "ROLLOUT HISTORY" label aren't rendered through components.Table.
func headerRule(theme tui.Theme, width int) string {
	return lipgloss.NewStyle().Foreground(theme.TextGhost2).Render(strings.Repeat("─", width))
}

// feedHeader is 16a's "WHEN │ │ +CHANGE │ WHAT │ OBJECT" column header row,
// or 16b's narrower "WHEN │ │ WHAT — <kind>/<name> + its pods" (no
// +CHANGE/OBJECT — the mockup drops both once the feed is already scoped to
// one object, docs/design README.md §16b).
func (m Model) feedHeader(theme tui.Theme, width int) string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint)
	whatW := m.feedWhatWidth(width)
	when := padRight("WHEN", feedWhenW)
	glyph := padRight("", feedGlyphW)
	var line string
	if m.objectScoped() {
		what := "WHAT — " + strings.ToLower(string(m.objectKind)) + "/" + m.objectName + " + its pods"
		line = "  " + when + "  " + glyph + "  " + components.Truncate(what, whatW)
	} else {
		change := padRight("+CHANGE", feedChangeW)
		what := padRight("WHAT", whatW)
		object := padLeft("OBJECT", feedObjectW)
		line = "  " + when + "  " + glyph + "  " + change + "  " + what + "  " + object
	}
	return fillLine(label.Render(line), width, false, theme)
}

// railWidth is 16b's revision-rail sidebar width: a fixed fraction of the
// body (the mockup's own 264/960 ratio), clamped so the feed always keeps
// enough room at both golden widths (80 and 120).
func (m Model) railWidth(width int) int {
	return clamp(width*28/100, 22, 36)
}

// railColumnLines renders 16b's revision rail as a left sidebar — a
// "ROLLOUT HISTORY" label (headerRule underneath it, matching the feed
// header's own divider) over one 3-line card per revision (rev + age,
// image, restarts-since/stable-for), newest first, a blank line between
// cards — padded/truncated to exactly w×height so it composes edge-to-edge
// with the feed column in timelineBody. The selected card (m.railSelected)
// carries the highlight regardless of which pane currently has focus: moving
// the rail cursor must be visible in both panels at once, since it live-
// syncs the feed's own cursor to the matching ROLLOUT row (see
// update.go's moveRailSelection/syncFeedToRailSelection).
func (m Model) railColumnLines(theme tui.Theme, w, height int) []string {
	label := lipgloss.NewStyle().Foreground(theme.TextFaint)
	lines := []string{fillLine("  "+label.Render("ROLLOUT HISTORY"), w, false, theme), headerRule(theme, w)}
	for i, e := range m.rail {
		if i > 0 {
			lines = append(lines, fillLine("", w, false, theme))
		}
		lines = append(lines, m.railCardLines(theme, e, i, w)...)
	}
	for len(lines) < height {
		lines = append(lines, fillLine("", w, false, theme))
	}
	return lines[:min(len(lines), height)]
}

// railCardLines renders one revision's 3-line card: "rev N" + right-aligned
// age, the image, then a restarts-since (red) or stable-for (green) status
// line — "was it ever stable?" at a glance (docs/design README.md §16b).
// idx 0 (the current revision) gets bright/accent text instead of the dim
// treatment every other revision gets.
func (m Model) railCardLines(theme tui.Theme, e kube.TimelineEntry, idx, w int) []string {
	current := idx == 0
	selected := idx == m.railSelected

	revStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	imageStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	ageStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
	if current {
		revStyle = lipgloss.NewStyle().Foreground(theme.Text).Bold(true)
		imageStyle = lipgloss.NewStyle().Foreground(theme.AccentHi)
	}
	bar := lipgloss.NewStyle().Foreground(theme.Accent)
	gapStyle := lipgloss.NewStyle()
	if selected {
		revStyle = revStyle.Background(theme.SelBg)
		imageStyle = imageStyle.Background(theme.SelBg)
		ageStyle = ageStyle.Background(theme.SelBg)
		bar = bar.Background(theme.SelBg)
		gapStyle = gapStyle.Background(theme.SelBg)
	}

	// Routed through gapStyle even for the plain "  " case (never a bare
	// literal) so a selected card's background reaches this 2-cell marker
	// slot with no unstyled notch at the left edge — components.Table's own
	// renderRowV2 documents the same "never a bare literal" rule for its
	// identical "▎ "/"  " marker slot.
	gutter := gapStyle.Render("  ")
	if selected {
		gutter = bar.Render(tui.GlyphSelBar) + gapStyle.Render(" ")
	}
	contentW := max(w-3, 4)

	age := "–"
	if !e.Time.IsZero() {
		age = shortAge(m.fetchedAt.Sub(e.Time))
	}
	revText := components.Truncate(fmt.Sprintf("rev %d", e.Revision), max(contentW-lipgloss.Width(age)-1, 1))
	revLine := gutter + padBetweenBG(revStyle.Render(revText), ageStyle.Render(age), contentW, gapStyle)

	imageLine := gutter + imageStyle.Render(components.Truncate(shortImage(e.Image), contentW))

	statusGlyph, statusText, statusStyle := m.revisionStatus(theme, idx)
	if selected {
		statusStyle = statusStyle.Background(theme.SelBg)
	}
	statusLine := gutter + statusStyle.Render(statusGlyph+" "+components.Truncate(statusText, max(contentW-2, 1)))

	return []string{
		fillLine(revLine, w, selected, theme),
		fillLine(imageLine, w, selected, theme),
		fillLine(statusLine, w, selected, theme),
	}
}

// revisionStatus computes rail idx's status line: a red "▲ N restart(s)
// since" if a container restart landed inside this revision's own lifetime
// window (its own rollout up to whenever it was superseded — or "now" for
// the current revision) — scanning m.entries (unwindowed) since a stale
// revision's window can fall well outside the feed's current time window.
// Otherwise, for the current revision only (idx == 0), a live rollout-progress
// override (load.go's attachLiveRolloutStatus) takes over when the owning
// Deployment itself isn't done rolling out yet: yellow "progressing ▸" or red
// "degraded" — so a still-pulling image never reads as "stable" just because
// nothing has restarted yet. Failing that, a green "● stable <duration>".
func (m Model) revisionStatus(theme tui.Theme, idx int) (glyph, text string, style lipgloss.Style) {
	start := m.rail[idx].Time
	end := m.fetchedAt
	if idx > 0 {
		end = m.rail[idx-1].Time
	}
	restarts := 0
	for _, e := range m.entries {
		if e.Kind == kube.TimelineRestart && !e.Time.Before(start) && e.Time.Before(end) {
			restarts++
		}
	}
	if restarts > 0 {
		suffix := ""
		if idx == 0 {
			suffix = " since"
		}
		return tui.GlyphWarning, fmt.Sprintf("%d %s%s", restarts, pluralize(restarts, "restart"), suffix), lipgloss.NewStyle().Foreground(theme.Bad)
	}
	if idx == 0 && m.rail[idx].LiveStatusText != "" {
		if m.rail[idx].LiveStatusBad {
			return tui.GlyphFailed, m.rail[idx].LiveStatusText, lipgloss.NewStyle().Foreground(theme.Bad)
		}
		return tui.GlyphPending, m.rail[idx].LiveStatusText, lipgloss.NewStyle().Foreground(theme.Warn)
	}
	return tui.GlyphRunning, "stable " + shortAge(end.Sub(start)), lipgloss.NewStyle().Foreground(theme.Good)
}

// padBetweenBG mirrors padBetween but fills the gap through gapStyle
// (rather than leaving it unstyled) so a selected rail card's background
// tint reaches all the way across the rev/age line's middle gap.
func padBetweenBG(left, right string, width int, gapStyle lipgloss.Style) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + gapStyle.Render(strings.Repeat(" ", gap)) + right
}

// feedLines renders the currently visible window of m.rows, growing around
// m.selected until height is filled — computed fresh every render from
// Model state alone (no persisted scroll offset), mirroring tasks/events'
// own eventRows. Rows are no longer fixed at one physical line each (long
// WHAT text wraps, and a ROLLOUT divider always carries a trailing rule
// line — see renderRow/rowLineCount), so the window is chosen by each row's
// actual line count (visibleLineWindow), not a flat entry count. 16a's
// folded-normal footer line (foldedNormalLine), when present, takes one
// line off the bottom of the budget.
func (m Model) feedLines(theme tui.Theme, width, height int) []string {
	footer := m.foldedNormalLine(theme, width)
	rowsHeight := height
	if footer != "" {
		rowsHeight = max(height-1, 1)
	}

	if len(m.rows) == 0 {
		body := components.CenterLines([]string{m.emptyMessage()}, width, rowsHeight)
		if footer == "" {
			return []string{body}
		}
		return []string{body, footer}
	}
	heights := make([]int, len(m.rows))
	for i, e := range m.rows {
		heights[i] = m.rowLineCount(e, width)
	}
	start, end := visibleLineWindow(heights, m.selected, rowsHeight)
	lines := make([]string, 0, rowsHeight)
	for i := start; i < end; i++ {
		lines = append(lines, m.renderRow(theme, m.rows[i], i == m.selected, width)...)
	}
	// visibleLineWindow can only overflow rowsHeight when the selected row
	// alone (heights[selected]) is taller than the whole budget — every
	// other row it admits fits inside what's left. timelineBody's no-rail
	// (16a) branch joins feedLines' return straight into the body with no
	// further trim, unlike its with-rail (16b) branch's own pad/truncate-
	// to-height step, so this has to be the one place that guarantees the
	// "at most `height` lines back" contract every caller relies on.
	if len(lines) > rowsHeight {
		lines = lines[:rowsHeight]
	}
	if footer != "" {
		lines = append(lines, footer)
	}
	return lines
}

// rowLineCount is the physical line count m.renderRow(e, ...) will return at
// width — used by feedLines' windowing to budget by actual rendered height
// rather than assuming one line per entry, without rendering every row
// twice just to measure it.
func (m Model) rowLineCount(e kube.TimelineEntry, width int) int {
	whatW := m.feedWhatWidth(width)
	if e.Kind == kube.TimelineRollout {
		return len(components.Wrap(m.rolloutBodyText(e), whatW)) + 1 // +1: the trailing rule line
	}
	return len(components.Wrap(entrySummary(e), whatW))
}

// foldedNormalLine is 9b's "▸ normal · N normal events — reason1 ·
// reason2… ↹ expand" collapsed summary line, adapted for timeline's
// TimelineEntry rows (docs/design README.md §16a: "normals collapsed into
// one group line"). 16b never folds (see recomputeVisible's doc comment).
func (m Model) foldedNormalLine(theme tui.Theme, width int) string {
	if m.objectScoped() || m.normalExpanded || len(m.foldedNormal) == 0 {
		return ""
	}
	glyphStyle := lipgloss.NewStyle().Foreground(theme.TextFaint)
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	reasonStyle := lipgloss.NewStyle().Foreground(theme.TextFaint)
	hintStyle := lipgloss.NewStyle().Foreground(theme.TextDim)

	seen := map[string]bool{}
	names := make([]string, 0, 4)
	for _, e := range m.foldedNormal {
		if e.Reason == "" || seen[e.Reason] {
			continue
		}
		seen[e.Reason] = true
		names = append(names, e.Reason)
		if len(names) == 4 {
			break
		}
	}

	left := glyphStyle.Render(tui.GlyphExpand) + " " + labelStyle.Render("normal") + " " +
		reasonStyle.Render(fmt.Sprintf("%d normal events — %s…", len(m.foldedNormal), strings.Join(names, " · ")))
	right := hintStyle.Render(tui.GlyphTab + " expand")
	return fillLine(padBetween(left, right, width), width, false, theme)
}

// visibleLineWindow mirrors the old entry-count visibleWindow's own "grow
// around selected, forward first, then backward" algorithm, but budgets by
// each row's actual physical line count (heights[i]) rather than assuming
// one line per row, now that wrapped WHAT text and the ROLLOUT divider's
// rule line make rows variable-height. A row only enters the window if it
// fits inside the remaining budget whole — the same "always full rows,
// never a partial one cut off mid-render" contract the old function gave
// for free when every row was exactly one line.
func visibleLineWindow(heights []int, selected, budget int) (start, end int) {
	n := len(heights)
	if n == 0 || budget <= 0 {
		return 0, 0
	}
	selected = clamp(selected, 0, n-1)
	start, end = selected, selected+1
	used := heights[selected]
	for used < budget {
		grew := false
		if end < n && used+heights[end] <= budget {
			used += heights[end]
			end++
			grew = true
		}
		if used < budget && start > 0 && used+heights[start-1] <= budget {
			used += heights[start-1]
			start--
			grew = true
		}
		if !grew {
			break
		}
	}
	return start, end
}

// entryGlyphStyle is 16a's "rollouts (⇅ purple) are the visual anchors"
// plus 9b's warning/normal red-vs-yellow-vs-blue rule reused for the
// Events source, and a quiet secondary tone for restarts (docs/design
// README.md §16a).
func (m Model) entryGlyphStyle(theme tui.Theme, e kube.TimelineEntry) (string, lipgloss.Style) {
	switch e.Kind {
	case kube.TimelineRollout:
		return tui.GlyphRollout, lipgloss.NewStyle().Foreground(theme.Accent).Bold(true)
	case kube.TimelineRevision, kube.TimelineSync:
		// §32a/§34a: "one ◆ marker, two GitOps engines" — the only purple
		// marker in the feed besides the rollout anchor it usually
		// precedes, shared by Flux's revision rows and Argo's sync rows.
		return tui.GlyphRevision, lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true)
	case kube.TimelineRestart:
		return tui.GlyphRestarts, lipgloss.NewStyle().Foreground(theme.Warn)
	default:
		if e.Severity == "Warning" {
			return tui.GlyphWarning, lipgloss.NewStyle().Foreground(theme.Warn)
		}
		return tui.GlyphCompleted, lipgloss.NewStyle().Foreground(theme.Info)
	}
}

// feedSelBarStyle is the timeline feed/rail's own "▎" selection-bar style —
// Foreground(Accent)/Background(SelBg), the exact convention
// components.Table's own SelBarStyle uses everywhere else the app shows a
// list (docs/design README.md: "Selection cue = 1-cell accent bar + SelBg
// row background — the bar is the primary cue"). The feed and ROLLOUT
// divider don't go through components.Table (their layout — +CHANGE/
// OBJECT columns, the rail sidebar — isn't what Table renders), so they
// render this bar by hand rather than inheriting a shared SelBarStyle
// field.
func feedSelBarStyle(theme tui.Theme) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg)
}

// renderRow renders one feed entry as one or more physical lines — a
// full-width purple divider (renderRolloutDivider; rollouts are visual
// anchors, not ordinary rows, docs/design README.md §16a) for a
// TimelineRollout entry, or the ordinary WHEN/glyph/[+CHANGE/]WHAT/[OBJECT]
// layout for everything else. WHAT text that doesn't fit the column wraps
// onto continuation lines (components.Wrap) rather than ellipsizing — the WHEN/
// glyph/+CHANGE/OBJECT columns stay blank on those so the wrapped text
// reads as a continuation of the same row, not a new one. A selected row
// gets the app-wide "▎" bar (feedSelBarStyle) in its 2-cell left marker
// slot, and every gap between columns — including that marker slot and the
// trailing slack fillLine adds — is rendered through gap()/fillLine rather
// than a bare literal, so the SelBg background reads as one solid fill with
// no unstyled notches, the same "never a bare literal" rule
// components.Table's renderRowV2 documents for its identical marker slot.
func (m Model) renderRow(theme tui.Theme, e kube.TimelineEntry, selected bool, width int) []string {
	if e.Kind == kube.TimelineRollout {
		return m.renderRolloutDivider(theme, e, selected, width)
	}

	glyph, glyphStyle := m.entryGlyphStyle(theme, e)
	dim := lipgloss.NewStyle().Foreground(theme.TextFaint)
	secondary := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	object := lipgloss.NewStyle().Foreground(theme.TextDim)
	warnTone := lipgloss.NewStyle().Foreground(theme.Warn)
	gapStyle := lipgloss.NewStyle()
	if selected {
		glyphStyle = glyphStyle.Background(theme.SelBg)
		dim = dim.Background(theme.SelBg)
		secondary = secondary.Background(theme.SelBg)
		object = object.Background(theme.SelBg)
		warnTone = warnTone.Background(theme.SelBg)
		gapStyle = gapStyle.Background(theme.SelBg)
	}
	gap := func(n int) string { return gapStyle.Render(strings.Repeat(" ", n)) }

	when := "–"
	if !e.Time.IsZero() {
		when = shortAge(m.fetchedAt.Sub(e.Time))
	}
	marker := gap(2)
	if selected {
		marker = feedSelBarStyle(theme).Render(tui.GlyphSelBar) + gap(1)
	}
	// blankLeft reuses marker (not a bare gap(2)) so a wrapped row's
	// selection bar runs down every physical line, not just the first —
	// the bar is "the primary cue" (docs/design README.md), so a wrapped
	// selected row can't let it stop short.
	left := marker + dim.Render(padRight(when, feedWhenW)) + gap(2) + glyphStyle.Render(glyph)
	blankLeft := marker + dim.Render(padRight("", feedWhenW)) + gap(2) + dim.Render(padRight("", feedGlyphW))

	whatW := m.feedWhatWidth(width)
	whatLines := components.Wrap(entrySummary(e), whatW)

	if m.objectScoped() {
		lines := make([]string, len(whatLines))
		for i, wl := range whatLines {
			prefix := left
			if i > 0 {
				prefix = blankLeft
			}
			line := prefix + gap(2) + secondary.Render(wl)
			lines[i] = fillLine(line, width, selected, theme)
		}
		return lines
	}

	changeText, warn := m.changeOffset(e)
	changeStyle := dim
	if warn {
		changeStyle = warnTone
	}
	objText := padLeft(components.Truncate(e.Object, feedObjectW), feedObjectW)
	lines := make([]string, len(whatLines))
	for i, wl := range whatLines {
		prefix, change, obj := blankLeft, padRight("", feedChangeW), padLeft("", feedObjectW)
		if i == 0 {
			prefix, change, obj = left, padRight(changeText, feedChangeW), objText
		}
		line := prefix + gap(2) + changeStyle.Render(change) + gap(2) +
			secondary.Render(wl) + gap(2) +
			object.Render(obj)
		lines[i] = fillLine(line, width, selected, theme)
	}
	return lines
}

// changeOffset is 16a's "+CHANGE" column: the row's time offset from the
// nearest TimelineRollout at-or-before it ("+45s"), "before" when the row
// predates every rollout in view, or "–" when there's no rollout in view at
// all to correlate against. warn is true only for the single row
// firstTroubleAfterRollout identifies (docs/design README.md §16a: "'+45s'
// next to the first BackOff is the correlation doing the triage for you").
func (m Model) changeOffset(e kube.TimelineEntry) (text string, warn bool) {
	var nearest *kube.TimelineEntry
	hasRollout := false
	for i := range m.rows {
		r := &m.rows[i]
		if r.Kind != kube.TimelineRollout {
			continue
		}
		hasRollout = true
		if !r.Time.After(e.Time) && (nearest == nil || r.Time.After(nearest.Time)) {
			nearest = r
		}
	}
	if nearest == nil {
		if hasRollout {
			return "before", false
		}
		return "–", false
	}
	if trouble, _, ok := m.firstTroubleAfterRollout(); ok &&
		trouble.Object == e.Object && trouble.Kind == e.Kind && trouble.Time.Equal(e.Time) {
		warn = true
	}
	return "+" + shortAge(e.Time.Sub(nearest.Time)), warn
}

// rolloutBodyText builds a TimelineRollout entry's own message text, shared
// between renderRolloutDivider (which wraps/renders it) and rowLineCount
// (which only needs its wrapped line count) so the two can never disagree.
// 16a's namespace-scoped feed spells out the object since rows from many
// Deployments are interleaved — "ROLLOUT deploy/x · rev A → B · image a → b
// [· by author]" (docs/design README.md §16a). 16b's object-scoped feed
// already names the object in the header/breadcrumb, so it drops the
// "deploy/x"/"image"/"by" context and keeps just the transition itself,
// tag-only on both sides since the repo name is redundant there too —
// "ROLLOUT rev A → B · :a → :b".
func (m Model) rolloutBodyText(e kube.TimelineEntry) string {
	_, name := splitObject(e.Object)
	revText := fmt.Sprintf("rev %d", e.Revision)
	imageText := shortImage(e.Image)
	compactImageText := imageText
	if prev, ok := m.previousRollout(e); ok {
		revText = fmt.Sprintf("rev %d → %d", prev.Revision, e.Revision)
		imageText = imageTransition(shortImage(prev.Image), shortImage(e.Image))
		compactImageText = compactImageTransition(shortImage(prev.Image), shortImage(e.Image))
	}
	if m.objectScoped() {
		return fmt.Sprintf("ROLLOUT %s · %s", revText, compactImageText)
	}
	body := fmt.Sprintf("ROLLOUT deploy/%s · %s · image %s", name, revText, imageText)
	if e.By != "" {
		body += " · by " + e.By
	}
	return body
}

// renderRolloutDivider is the rollout anchor row: WHEN/glyph/message, no
// +CHANGE/OBJECT columns, on the exact same (unselected: plain, selected:
// theme.SelBg) background every other feed row uses — rollouts are called
// out by their bold purple ⇅/text and a full-width rule on the line right
// below them, not by a permanent background tint of their own, so every row
// in the feed reads on one consistent surface. Selection uses the same
// "▎" bar (feedSelBarStyle) + gap()-routed backgrounds renderRow uses, so
// the bar color and the solidness of the selected background match every
// other row in the feed (and every other list in the app). Long bodies
// wrap onto continuation lines (components.Wrap) the same way renderRow's WHAT
// column does, rather than ellipsizing.
func (m Model) renderRolloutDivider(theme tui.Theme, e kube.TimelineEntry, selected bool, width int) []string {
	bg := lipgloss.NewStyle()
	if selected {
		bg = bg.Background(theme.SelBg)
	}
	gap := func(n int) string { return bg.Render(strings.Repeat(" ", n)) }
	when := bg.Foreground(theme.TextFaint)
	glyphStyle := bg.Foreground(theme.Accent).Bold(true)
	text := bg.Foreground(theme.TextPrimary)

	whenText := "–"
	if !e.Time.IsZero() {
		whenText = shortAge(m.fetchedAt.Sub(e.Time))
	}
	marker := gap(2)
	if selected {
		marker = feedSelBarStyle(theme).Render(tui.GlyphSelBar) + gap(1)
	}

	bodyLines := components.Wrap(m.rolloutBodyText(e), m.feedWhatWidth(width))
	lines := make([]string, 0, len(bodyLines)+1)
	for i, bl := range bodyLines {
		// marker (not a bare gap(2)) on every line, including
		// continuation ones, so a wrapped selected divider's bar runs the
		// full height of the row rather than stopping after line one.
		prefix := marker + when.Render(padRight("", feedWhenW)) + gap(2) + bg.Render(padRight("", feedGlyphW))
		if i == 0 {
			prefix = marker + when.Render(padRight(whenText, feedWhenW)) + gap(2) + glyphStyle.Render(tui.GlyphRollout)
		}
		line := prefix + gap(2) + text.Render(bl)
		lines = append(lines, fillLine(line, width, selected, theme))
	}
	rule := lipgloss.NewStyle().Foreground(theme.TextGhost2).Render(strings.Repeat("─", width))
	return append(lines, rule)
}

// previousRollout finds the next-older TimelineRollout entry for the same
// object as e, scanning the full unwindowed/unfiltered m.entries so the
// divider's "rev A → B" context survives even once the window trims the
// earlier rollout out of view.
func (m Model) previousRollout(e kube.TimelineEntry) (kube.TimelineEntry, bool) {
	var best *kube.TimelineEntry
	for i := range m.entries {
		r := &m.entries[i]
		if r.Kind != kube.TimelineRollout || r.Object != e.Object || !r.Time.Before(e.Time) {
			continue
		}
		if best == nil || r.Time.After(best.Time) {
			best = r
		}
	}
	if best == nil {
		return kube.TimelineEntry{}, false
	}
	return *best, true
}

// shortImage drops the registry/path prefix from a container image
// reference, keeping just the trailing "name:tag" component —
// "registry.example.com/nva/nva.bp.app:5.31.0.58108" renders as
// "nva.bp.app:5.31.0.58108". Nobody needs the internal registry host in a
// narrow rail card or a truncated feed row.
func shortImage(image string) string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// imageTransition elides a shared "repo:" prefix between two image
// references ("nva-worker:3.4.0" → "nva-worker:3.4.1" renders as
// "nva-worker:3.4.0 → :3.4.1", the mockup's own abbreviation) — a plain
// "a → b" when the repos differ or either side lacks a tag.
func imageTransition(prev, next string) string {
	if prev == "" || prev == next {
		return next
	}
	prevRepo, _, prevOk := strings.Cut(prev, ":")
	nextRepo, nextTag, nextOk := strings.Cut(next, ":")
	if prevOk && nextOk && prevRepo == nextRepo {
		return prev + " → :" + nextTag
	}
	return prev + " → " + next
}

// compactImageTransition is 16b's own abbreviation of imageTransition: the
// object-scoped feed already names the object (and so its repo) in the
// header/breadcrumb, so — unlike 16a's divider — neither side needs the
// repo name at all, just the tags ("nva-worker:3.4.0" → "nva-worker:3.4.1"
// renders as ":3.4.0 → :3.4.1"). Falls back to imageTransition's own
// plain "a → b" when the repos differ or either side lacks a tag, same as
// imageTransition.
func compactImageTransition(prev, next string) string {
	if prev == "" || prev == next {
		return next
	}
	prevRepo, prevTag, prevOk := strings.Cut(prev, ":")
	nextRepo, nextTag, nextOk := strings.Cut(next, ":")
	if prevOk && nextOk && prevRepo == nextRepo {
		return ":" + prevTag + " → :" + nextTag
	}
	return prev + " → " + next
}

// entrySummary is the row's "what changed" text — the Reason leads (9b's
// own REASON·OBJECT cell reused as a prefix) with the fuller Message
// trailing, since Message alone drops the reason for restarts/rollouts
// (their own Reason is "Restarted"/"Rollout", not embedded in Message the
// way an Event's already reads).
func entrySummary(e kube.TimelineEntry) string {
	if e.Kind == kube.TimelineRevision {
		// "Revision applied · kustomization/nva-workers · master@efd398b"
		// plus the commit subject when the cluster actually had one — the
		// subject lives only in a source-controller event that expires, so
		// its absence is normal and never faked (§31a/§32a).
		parts := []string{"Revision applied", strings.ToLower(e.Object), e.GitRevision}
		if e.CommitSubject != "" {
			parts = append(parts, `"`+e.CommitSubject+`"`)
		}
		return strings.Join(parts, " · ")
	}
	if e.Kind == kube.TimelineSync {
		// "Sync applied · application/nva-billing · main@e41b90c ·
		// ci-bot" — no subject slot: Argo's own sync-completed Event
		// carries no commit-message text to parse, unlike Flux's, and
		// there is no other on-cluster source for one (§34a).
		parts := []string{"Sync applied", strings.ToLower(e.Object)}
		if e.GitRevision != "" {
			parts = append(parts, e.GitRevision)
		}
		if e.By != "" {
			parts = append(parts, e.By)
		}
		return strings.Join(parts, " · ")
	}
	if e.Reason == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Reason
	}
	return e.Reason + " · " + e.Message
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

// padBetween places left-aligned left and right-aligned right within width
// (measuring already-styled/ANSI content) — mirrors tasks/events' own strip
// layout helper.
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// fillLine pads content out to width with trailing spaces styled through
// the selection background when selected — mirrors tasks/events' own
// helper of the same name.
func fillLine(content string, width int, selected bool, theme tui.Theme) string {
	pad := lipgloss.NewStyle()
	if selected {
		pad = lipgloss.NewStyle().Background(theme.SelBg)
	}
	slack := max(width-lipgloss.Width(content), 0)
	return content + pad.Render(strings.Repeat(" ", slack))
}

// padRight/padLeft fix plain (unstyled) text to width — the feed's fixed
// columns (WHEN, +CHANGE, OBJECT) are padded before styling so header and
// data rows always line up.
func padRight(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

func padLeft(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return strings.Repeat(" ", width-w) + s
	}
	return s
}
