package browse

import (
	"fmt"
	"image/color"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/verbs"
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
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	ghost := lipgloss.NewStyle().Foreground(theme.TextGhost)
	ghost2 := lipgloss.NewStyle().Foreground(theme.TextGhost2)
	text := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)
	// A step duller than the keybar's own Accent key hints — the breadcrumb's
	// inline g/n/c prefixes are wayfinding, not actions, so they recede into
	// a second layer behind the path text instead of matching the keybar's
	// brightness.
	keyHint := lipgloss.NewStyle().Foreground(theme.AccentMuted)
	// Path segments step up in brightness toward the current kind — dim →
	// mid → bright — so depth reads at a glance as the breadcrumb narrows
	// from cluster to namespace to kind.
	secondary := lipgloss.NewStyle().Foreground(theme.TextSecondary)

	ctxName := "cluster unavailable"
	if m.session != nil && m.session.Location.Context != "" {
		ctxName = m.session.Location.Context
	}

	crumbs := append(tui.BrandCrumbs(theme),
		// Same single-space spacing as the " › " separators below, so the
		// wordmark/path divider reads as one consistent breadcrumb rhythm.
		tui.Crumb{Text: " │ ", Style: ghost2},
		tui.Crumb{Text: verbs.Context.Key + " ", Style: keyHint},
		tui.Crumb{Text: ctxName, Style: secondary},
	)
	if !m.desc.ClusterScoped {
		// One step brighter than the context segment (TextPrimary, not
		// TextSecondary) — and not the muted purple the "n" key itself uses,
		// so the segment text stays distinct from its own key prefix.
		nsText, nsStyle := m.namespace, lipgloss.NewStyle().Foreground(theme.TextPrimary)
		if m.grouped() {
			// 6b: scope is never ambiguous — the blue ALL-NS token
			// (docs/design README.md §6b).
			nsText, nsStyle = tui.GlyphAllNS+" all namespaces", lipgloss.NewStyle().Foreground(theme.Info)
		}
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: verbs.Namespace.Key + " ", Style: keyHint},
			tui.Crumb{Text: nsText, Style: nsStyle},
		)
	}
	if m.originName != "" {
		// esc-back-to-origin (deployments.go's openDeploymentPods, helm.go's
		// openReleaseObjects): names the owning row between the namespace
		// and kind segments, e.g. "… › nva-stage › my-deploy › Pods".
		crumbs = append(crumbs,
			tui.Crumb{Text: " › ", Style: ghost},
			tui.Crumb{Text: m.originName, Style: dim},
		)
	}
	crumbs = append(crumbs,
		tui.Crumb{Text: " › ", Style: ghost},
		tui.Crumb{Text: verbs.Goto.Key + " ", Style: keyHint},
		tui.Crumb{Text: m.desc.Display, Style: text},
	)
	if m.desc.Custom && m.desc.APIGroup != "" && m.desc.APIVersion != "" {
		// docs/design README.md §14a: "Certificates + cert-manager.io/v1" —
		// the CRD's own API group/version, dim, right after the kind name.
		crumbs = append(crumbs, tui.Crumb{Text: " " + m.desc.APIGroup + "/" + m.desc.APIVersion, Style: faint})
	}
	switch {
	case m.kind == kube.KindForward:
		// docs/design README.md §13c: "all namespaces tag (forwards are
		// global, never namespace-filtered)" — Forwards reuses Nodes'
		// ClusterScoped semantics (no per-namespace switching/refetch at
		// all) but is namespaced data viewed globally, not truly
		// cluster-scoped like Nodes, so the breadcrumb tag reads
		// differently — matching 6b's own "all namespaces" blue wording.
		crumbs = append(crumbs, tui.Crumb{Text: "  " + tui.GlyphAllNS + " all namespaces", Style: lipgloss.NewStyle().Foreground(theme.Info)})
	case m.desc.ClusterScoped:
		// 11a: "… cluster › Nodes" + a small cluster-scoped tag — the
		// namespace segment is already dropped above.
		crumbs = append(crumbs, tui.Crumb{Text: "  cluster-scoped", Style: faint})
	}

	if m.state == tui.TaskStateLoading {
		// 15a: the header's right side is a counting timer instead of the
		// usual sync/forward/conn badges — "what" is loading and for how
		// long, with no fake progress bar (docs/design README.md §15a). The
		// leading marker is the shared spinner rather than §15a's static ▲,
		// so the badge reads as live even in the sub-second stretch before
		// the timer's first decimal turns over; View stays pure (the frame
		// index only advances on spinner.TickMsg, in Update).
		elapsed := max(m.now.Sub(m.loadStartedAt), 0)
		return tui.HeaderState{
			Crumbs: crumbs,
			Conn: tui.ConnBadge{
				Text:  fmt.Sprintf("%s loading %s · %.1fs", m.spinner.View(), lowerDisplay(m.desc.Display), elapsed.Seconds()),
				Style: lipgloss.NewStyle().Foreground(theme.Warn),
			},
		}
	}

	syncNote := ""
	if m.pollsMetrics() {
		syncNote = "sync 2s"
	}

	return tui.HeaderState{
		Crumbs:      crumbs,
		SyncNote:    syncNote,
		ConfigChip:  tui.BuildConfigChip(theme, m.session),
		UpdateChip:  tui.BuildUpdateChip(theme, m.session),
		ForwardChip: tui.BuildForwardChip(theme, m.session.ForwardSummary()),
		Conn:        tui.LiveConnBadge(theme, m.conn, tui.GlyphRunning+" connected"),
	}
}

// stripLineCount is how many Strips lines the current state renders — kept
// in sync with Strips itself so selection.go's tableDataRows can budget the
// table viewport correctly. §36a's CronJob behavior strip is NOT counted
// here — per the design mockup it renders at the bottom of the table body,
// not among these top-of-screen strips; see tableDataRows/tableBody's own
// reservation for it instead.
func (m Model) stripLineCount() int {
	switch m.state {
	case tui.TaskStateEmpty, tui.TaskStateLoading:
		return 1
	case tui.TaskStateReady:
		n := 1
		if m.offline() {
			n = 2 // banner + stale strip, replacing the health strip
		}
		if m.filterActive {
			n++
		}
		return n
	default:
		return 0
	}
}

// Strips shows the table's column header line in the empty state (10c: "the
// table column header still renders — the app is fine"), the per-status
// health strip once ready (or, offline, the 4a reconnect banner + stale
// snapshot strip in its place), and — while filtering — an extra line with
// the live query, matched/total, and a "hidden by filter" notice. §36a's
// CronJob behavior strip is deliberately not one of these: the design
// mockup docks it above the keybar, below the table, so tableBody renders
// it as a trailing line instead — see cronJobBehaviorStripLine's own doc
// comment.
func (m Model) Strips(width int) []string {
	theme := m.Theme()
	switch m.state {
	case tui.TaskStateEmpty:
		return []string{m.columnHeaderLine(theme, width)}
	case tui.TaskStateLoading:
		return []string{m.loadingStripLine(theme, width)}
	case tui.TaskStateReady:
		var lines []string
		if m.offline() {
			lines = []string{m.offlineBannerLine(theme, width), m.errorBannerRuleLine(theme, width), m.staleStripLine(theme, width)}
		} else {
			lines = []string{m.healthStripLine(theme, width)}
		}
		if m.auxKindsDeniedNote != "" {
			lines = append(lines, m.auxKindsDeniedNoteLine(theme, width))
		}
		if m.followNote != "" {
			lines = append(lines, insetStripLine(lipgloss.NewStyle().Foreground(theme.Warn).Render("⚠ "+m.followNote), width))
		}
		if m.filterActive {
			lines = append(lines, m.filterStripLine(theme, width))
		}
		return lines
	default:
		return nil
	}
}

// offlineBannerLine is 4a's reconnect banner: verbatim watch/ping error,
// attempt/backoff countdown, and the r/c exits (docs/design README.md §4a:
// "bg #2a1518, border-bottom #4a2228"). Unlike every other strip line (the
// transparent, terminal-background convention documented on paletteStyles),
// this one carries a real fill — every span below bakes theme.ErrBannerBg
// in explicitly, including the pad/gap fills, since an outer wrap can't do
// it: each inner Render's own ANSI reset would cancel it before the line
// finishes.
func (m Model) offlineBannerLine(theme tui.Theme, width int) string {
	fill := lipgloss.NewStyle().Background(theme.ErrBannerBg)
	warn := fill.Foreground(theme.Bad)
	text := fill.Foreground(theme.BadText)
	dim := fill.Foreground(theme.BadMuted)
	key := fill.Foreground(theme.BadSoft)

	errText := m.conn.Err
	if errText == "" {
		errText = "connection lost"
	}
	left := warn.Render(tui.GlyphWarning) + fill.Render(" ") + text.Render(errText)

	// An expired credential has no scheduled retry to count down to (the
	// health loop stops pinging), so the countdown segment is replaced by
	// what the user actually has to do — 'r' stays, as the way back in once
	// they've done it elsewhere.
	status := fmt.Sprintf("retry %d · next in %ds", m.conn.Attempt, int(max(m.conn.NextRetryAt.Sub(m.now), 0).Round(time.Second).Seconds()))
	retryLabel := "retry now"
	if m.conn.NeedsCredentials() {
		status = "re-authenticate in another shell"
		retryLabel = "retry"
	}
	right := dim.Render(status) +
		fill.Render("   ") + key.Render("r") + fill.Render(" ") + dim.Render(retryLabel) +
		fill.Render("   ") + key.Render("c") + fill.Render(" ") + dim.Render("switch context")

	return insetStripLineFill(padBetweenFill(left, right, stripInnerWidth(width), fill), width, fill)
}

// errorBannerRuleLine draws 4a's "border-bottom #4a2228" under the offline
// banner — the one strip divider that isn't Frame's own inter-band rule,
// since offlineBannerLine is the one strip line with a real background fill
// the others don't carry.
func (m Model) errorBannerRuleLine(theme tui.Theme, width int) string {
	return lipgloss.NewStyle().Foreground(theme.ErrBannerBorder).Render(strings.Repeat("─", width))
}

// staleStripLine is 4a's strip replacing the health strip: the age of the
// kept snapshot, and a note that counts are frozen (docs/design README.md
// §4a: "counts frozen · 36 pods").
func (m Model) staleStripLine(theme tui.Theme, width int) string {
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	dim := lipgloss.NewStyle().Foreground(theme.TextFaint)

	age := max(m.now.Sub(m.fetchedAt), 0)
	left := warn.Render(fmt.Sprintf("%s showing snapshot from %s · %ds old",
		tui.GlyphStale, m.fetchedAt.Format("15:04:05"), int(age.Round(time.Second).Seconds())))
	right := dim.Render(fmt.Sprintf("counts frozen · %d %s", len(m.rows), lowerDisplay(m.desc.Display)))

	return insetStripLine(padBetween(left, right, stripInnerWidth(width)), width)
}

// browseColumns is resources.Columns with the Restarts title swapped for
// the ↺ glyph (docs/design §2a). The mapping lives here rather than in
// resources because glyph choice is tui/glyphs.go's choke point and
// resources can't import tui (cycle via Session).
func browseColumns(desc resources.Descriptor) []components.Column {
	cols := resources.Columns(desc)
	for i := range cols {
		if cols[i].Title == "Restarts" {
			cols[i].Title = tui.GlyphRestarts
		}
	}
	return cols
}

func (m Model) columnHeaderLine(theme tui.Theme, width int) string {
	cols := browseColumns(m.desc)
	if len(cols) == 0 {
		return ""
	}
	t := components.Table{Columns: cols, HeaderStyle: lipgloss.NewStyle().Foreground(theme.TextFaint)}
	return t.HeaderLine(width)
}

// healthStripLine renders the per-status glyph+count segments (docs/design
// §2a: "● 32 running · ▲ 2 pending · ✕ 1 crashloop · ○ 1 completed") in
// OK/Warn/Fail/Neutral order, skipping zero counts, plus a right-aligned
// total.
func (m Model) healthStripLine(theme tui.Theme, width int) string {
	aggs := m.aggregates()
	counts := aggs.health
	if m.desc.Custom && !m.desc.StatusSemantics && len(m.rows) > 0 && counts.OK+counts.Warn+counts.Fail == 0 {
		// docs/design README.md §14a: a CRD kind whose instances carry no
		// Ready/Available condition at all gets no fake health — the strip
		// drops the per-status counts and says so instead. Gated on
		// StatusSemantics because a §30a Flux list whose rows are *all*
		// suspended is all-Neutral too, and telling the user that kind has
		// no status semantics would be a lie about a kind that has them.
		faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
		note := faint.Render("no status semantics · NAME + AGE only")
		right := lipgloss.NewStyle().Foreground(theme.TextDim).Render(fmt.Sprintf("%d %s", len(m.rows), lowerDisplay(m.desc.Display)))
		return insetStripLine(padBetween(note, right, stripInnerWidth(width)), width)
	}
	segments := []struct {
		class resources.StatusClass
		n     int
	}{
		{resources.StatusOK, counts.OK},
		{resources.StatusWarn, counts.Warn},
		{resources.StatusFail, counts.Fail},
		{resources.StatusNeutral, counts.Neutral},
	}
	numStyle := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	labelStyle := lipgloss.NewStyle().Foreground(theme.TextDim)

	var parts []string
	if n := len(m.marks); n > 0 {
		// 20a: "strip's first slot becomes ▪ N marked" — takes over the
		// leading segment rather than fighting the health counts for room.
		markStyle := lipgloss.NewStyle().Foreground(theme.Accent)
		parts = append(parts, markStyle.Render(tui.GlyphMarked)+" "+numStyle.Render(strconv.Itoa(n))+" "+labelStyle.Render("marked"))
	}
	for _, seg := range segments {
		if seg.n == 0 {
			continue
		}
		glyphStyle := lipgloss.NewStyle().Foreground(glyphColor(theme, m.healthTone(seg.class)))
		label := m.desc.HealthLabel(seg.class)
		// Per-kind glyph overrides (11a's cordoned ◈, 18a's pending ◌) are
		// declared on the descriptor, not switched on here — see
		// resources.Descriptor.HealthGlyph.
		glyph := m.desc.HealthGlyph(seg.class)
		parts = append(parts, glyphStyle.Render(glyph)+" "+
			numStyle.Render(strconv.Itoa(seg.n))+" "+labelStyle.Render(label))
	}
	if counts.Outdated > 0 {
		// 18a's outdated segment sits after the status counts because it
		// cross-cuts them — the same release is already counted above as
		// deployed (or failed, or pending-upgrade).
		glyphStyle := lipgloss.NewStyle().Foreground(glyphColor(theme, resources.StatusWarn))
		parts = append(parts, glyphStyle.Render(tui.GlyphWarning)+" "+
			numStyle.Render(strconv.Itoa(counts.Outdated))+" "+labelStyle.Render("outdated"))
	}
	if counts.ExpiringSoon > 0 {
		// §35b's own cross-cutting segment, same reasoning as Outdated
		// above: a ready-but-expiring cert is already counted "ready".
		glyphStyle := lipgloss.NewStyle().Foreground(glyphColor(theme, resources.StatusWarn))
		parts = append(parts, glyphStyle.Render(tui.GlyphExpiring)+" "+
			numStyle.Render(strconv.Itoa(counts.ExpiringSoon))+" "+labelStyle.Render("<30d"))
	}
	if m.kind == kube.KindCronJob {
		// §36a/§4.3's own two cross-cutting segments: a CronJob with an
		// active run (or a currently-suspended one) is already tallied by
		// its last *terminal* outcome above — resources.cronJobHealth's own
		// doc comment.
		if counts.Active > 0 {
			glyphStyle := lipgloss.NewStyle().Foreground(theme.Warn)
			parts = append(parts, glyphStyle.Render(tui.GlyphCronActive)+" "+
				numStyle.Render(strconv.Itoa(counts.Active))+" "+labelStyle.Render("active now"))
		}
		if counts.Suspended > 0 {
			glyphStyle := lipgloss.NewStyle().Foreground(theme.TextDim)
			parts = append(parts, glyphStyle.Render(tui.GlyphCronPaused)+" "+
				numStyle.Render(strconv.Itoa(counts.Suspended))+" "+labelStyle.Render("suspended"))
		}
	}
	left := strings.Join(parts, "   ")
	rightText := fmt.Sprintf("%d %s", len(m.rows), lowerDisplay(m.desc.Display))
	helm := false
	switch {
	case m.kind == kube.KindNode:
		rightText = m.nodeSummaryText()
	case m.kind == kube.KindForward:
		rightText = m.forwardSummaryText()
	case m.kind == kube.KindHelmRelease:
		// 18a's right side is two data sources rather than the usual
		// "<N> <kind>" count, and helmStripRight owns it because they aren't
		// all one color — see its doc comment.
		helm = true
	case m.kind == kube.KindCustomResourceDefinition:
		// 14b: "28 definitions · 9 API groups · sorted by group" — the
		// generic "<N> <kind>" wording never names the group count or the
		// sort order this list is unique in guaranteeing. Dropped once a
		// manual 1-9 sort overrides that guarantee (sort.go's applySort),
		// since the clause would otherwise describe an order the rows are
		// no longer actually in.
		rightText = fmt.Sprintf("%d definitions · %d API groups", len(m.rows), distinctCRDGroups(m.rows))
		if m.sortColumn == 0 {
			rightText += " · sorted by group"
		}
	case m.kind == kube.KindCertificate:
		// §35b: "6 certificates · unhealthy + soonest expiry first" — the
		// generic "<N> <kind>" wording never names the sort guarantee.
		// Dropped once a manual 1-9 sort overrides it, same reasoning as
		// 14b's CRD list just above.
		rightText = fmt.Sprintf("%d certificates", len(m.rows))
		if m.sortColumn == 0 {
			rightText += " · unhealthy + soonest expiry first"
		}
	case m.kind == kube.KindCronJob:
		// §36a: "7 cronjobs · clock 14:02:11 UTC" — the live wall clock the
		// one-second UI tick (Phase 4 task 5) drives, proof the NEXT column
		// is actually counting down rather than frozen at load time.
		rightText = fmt.Sprintf("%d cronjobs · clock %s UTC", len(m.rows), m.now.UTC().Format("15:04:05"))
		if m.cronJobJobsErr != nil {
			// §4.4 point 5: a stalled/denied Job read must stay visible —
			// never silently become the ordinary empty-history reading. A
			// permission denial gets its own wording rather than the raw
			// client-go error text, distinguishing "will never arrive" from
			// "still retrying" (docs/lazy-informers.md §5.6).
			if kube.IsPermissionError(m.cronJobJobsErr) {
				rightText += " · job history unavailable: permission denied"
			} else {
				rightText += " · job history unavailable: " + m.cronJobJobsErr.Error()
			}
		}
	case m.nodeCount > 0:
		rightText += fmt.Sprintf(" · %d nodes", m.nodeCount)
	}
	suffix := ""
	if m.grouped() {
		suffix = fmt.Sprintf(" · %d namespaces", aggs.namespaces)
	}
	inner := stripInnerWidth(width)
	right := labelStyle.Render(rightText + suffix)
	if helm {
		right = helmStripRight(theme, m.chartCacheNote, suffix, inner-lipgloss.Width(left)-3)
	}
	return insetStripLine(padBetween(left, right, inner), width)
}

// helmStripRight renders 18a's strip right side: the data source, plus the
// note caveating the repo cache the LATEST column is answered from.
//
// Both are caveats, but of different kinds. "from helm.sh/release.v1 secrets"
// says what this list *is* — decoded release Secrets in this namespace, so
// releases on Helm's ConfigMap or SQL storage backends aren't here, and no
// helm binary was run to build it. True, and identical on every render. The
// cache note is the one that varies and the one a user acts on, so it
// outranks the source at every width: padBetween drops a right side that
// doesn't fit, and below ~110 columns what it used to drop was the warning,
// leaving an all-`–` LATEST column with nothing saying why.
//
// note is resolved at load time (chartCacheNoteFor) because it reads the
// clock; this stays pure.
func helmStripRight(theme tui.Theme, note chartCacheNote, suffix string, avail int) string {
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	noteStyle := dim
	if note.warn {
		noteStyle = lipgloss.NewStyle().Foreground(theme.Warn)
	}
	source := "from " + string(kube.HelmReleaseSecretType) + " secrets"
	if note.empty() {
		return dim.Render(source + suffix)
	}
	render := func(source, text, remedy string) string {
		out := ""
		if source != "" {
			out = dim.Render(source + " · ")
		}
		if remedy != "" {
			text += " — " + remedy
		}
		return out + noteStyle.Render(text) + dim.Render(suffix)
	}
	// Widest first, shedding the least important part each rung: the source,
	// then the remedy, then the note's own wording. The last rung is what an
	// 80-column strip has room for beside the health counts.
	forms := []string{render(source, note.text, note.remedy)}
	if note.remedy != "" {
		forms = append(forms, render("", note.text, note.remedy))
	}
	forms = append(forms, render("", note.text, ""), render("", note.short, ""))
	for _, form := range forms {
		if lipgloss.Width(form) <= avail {
			return form
		}
	}
	return forms[len(forms)-1]
}

// distinctNamespaces counts the unique namespaces represented in rows, for
// 6b's health-strip "125 pods · 6 namespaces" right side.
func distinctNamespaces(rows []resources.Row) int {
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		seen[r.Namespace] = struct{}{}
	}
	return len(seen)
}

// distinctCRDGroups counts the unique API groups represented in rows, for
// 14b's health-strip "9 API groups" right side.
func distinctCRDGroups(rows []resources.Row) int {
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		seen[crdGroupCell(r)] = struct{}{}
	}
	return len(seen)
}

// auxKindsDeniedNoteLine renders §5's inline denial flag: a secondary cache
// the current kind's own columns/prompts read (auxKinds) came back Forbidden,
// while the primary rows on screen stayed correct and stay showing — this is
// the one extra line that says so, rather than a blank/wrong cell saying
// nothing (docs/lazy-informers.md §5.6).
func (m Model) auxKindsDeniedNoteLine(theme tui.Theme, width int) string {
	warn := lipgloss.NewStyle().Foreground(theme.Warn)
	return insetStripLine(warn.Render("⚠ "+m.auxKindsDeniedNote), width)
}

// filterStripLine renders the live "/" query, matched/total, and — when
// rows are hidden — the "esc to clear" notice (docs/design system-wide
// interactions: "items never silently disappear").
func (m Model) filterStripLine(theme tui.Theme, width int) string {
	accent := lipgloss.NewStyle().Foreground(theme.Accent)
	faint := lipgloss.NewStyle().Foreground(theme.TextFaint)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)

	// m.filterInput is blurred once filterListFocused commits the query
	// (updateFilterKey's "enter" case), which is what actually hides its
	// cursor overlay here — matching the old code's explicit
	// !m.filterListFocused gate on appending the cursor glyph.
	total, matched := len(m.rows), len(m.visible)
	right := dim.Render(fmt.Sprintf("%d/%d", matched, total))
	if matched < total {
		right = faint.Render(fmt.Sprintf("%d hidden by filter — esc to clear   ", total-matched)) + right
	}
	inputWidth := max(stripInnerWidth(width)-2-lipgloss.Width(right)-1, 1)
	left := accent.Render("/ ") + m.filterInput.ViewWidth(inputWidth)
	return insetStripLine(padBetween(left, right, stripInnerWidth(width)), width)
}

// cronJobBehaviorStripLine renders §36a's selected-row behavior strip
// (Phase 4 task 9): concurrency policy, starting deadline, configured
// history limits, timezone, and job-template backoff limit — the one
// genuinely list-relevant fact the pre-0.8.0 always-docked detail pane
// carried, now one strip line instead of a 336px side panel (docs/design
// README.md §36a's own note). Defaults mirror batchv1's own documented
// zero-value behavior (ConcurrencyPolicy "" == Allow, history limits 3/1,
// Job's own BackoffLimit 6) rather than rendering a misleading blank.
// Renders an inset-only line when nothing's selected (filtered to zero
// rows, or the cursor rests on a group/fold line) so tableBody's reserved
// height stays correct either way. Composed as a trailing line of
// tableBody, not Strips: the mockup docks this strip directly above the
// keybar, below the table — the same "extra line inside Body's own
// budgeted height" idiom secretdata/configmapdata's willRunStrip and
// browse's own meta.go panel use, rather than a Chrome-level strip above
// the table.
func (m Model) cronJobBehaviorStripLine(theme tui.Theme, width int) string {
	if line, ok := m.cronJobRunOverlapBannerLine(theme, width); ok {
		// §36b's amber overlap-warning banner (cronjob_actions.go) swaps into
		// this strip's own reserved slot while a genuine Forbid/Replace
		// conflict is staged — reusing stripLineCount's existing +1 for
		// KindCronJob rather than growing the reserved height further.
		return line
	}
	label := lipgloss.NewStyle().Foreground(theme.TextFaint)
	value := lipgloss.NewStyle().Foreground(theme.TextSecondary)
	sep := lipgloss.NewStyle().Foreground(theme.TextGhost2).Render(" │ ")
	field := func(name, text string) string {
		return label.Render(name+" ") + value.Render(text)
	}

	summary, ok := m.selectedCronJobSummary()
	if !ok || summary.Object == nil {
		return insetStripLine("", width)
	}
	spec := summary.Object.Spec

	concurrency := string(spec.ConcurrencyPolicy)
	if concurrency == "" {
		concurrency = "Allow"
	}
	deadline := "none"
	if spec.StartingDeadlineSeconds != nil {
		deadline = fmt.Sprintf("%ds", *spec.StartingDeadlineSeconds)
	}
	succLimit, failLimit := int32(3), int32(1)
	if spec.SuccessfulJobsHistoryLimit != nil {
		succLimit = *spec.SuccessfulJobsHistoryLimit
	}
	if spec.FailedJobsHistoryLimit != nil {
		failLimit = *spec.FailedJobsHistoryLimit
	}
	timezone := "controller local"
	if spec.TimeZone != nil && *spec.TimeZone != "" {
		timezone = *spec.TimeZone
	}
	backoff := "6"
	if spec.JobTemplate.Spec.BackoffLimit != nil {
		backoff = strconv.Itoa(int(*spec.JobTemplate.Spec.BackoffLimit))
	}

	line := value.Render(summary.Object.Name) + sep +
		field("concurrency", concurrency) + sep +
		field("deadline", deadline) + sep +
		field("history", fmt.Sprintf("%d ok · %d failed", succLimit, failLimit)) + sep +
		field("tz", timezone) + sep +
		field("backoff", backoff)
	return insetStripLine(line, width)
}

// stripInnerWidth/insetStripLine give the health/filter strips the same
// tui.FrameInset horizontal inset as the Frame's chrome bands (mock 2a's
// 14px padding). The empty-state column-header strip skips them — the
// table's own 2-cell marker slot already provides that inset, keeping it
// aligned with the ready-state table.
func stripInnerWidth(width int) int {
	return max(width-2*tui.FrameInset, 0)
}

func insetStripLine(line string, width int) string {
	return components.Pad(strings.Repeat(" ", tui.FrameInset)+line, width)
}

// insetStripLineFill is insetStripLine's background-filled counterpart —
// 4a's offline banner is the one strip line with a real bg (theme.
// ErrBannerBg), so its inset/trailing pad must carry fill's background
// explicitly rather than components.Pad's plain unstyled spaces.
func insetStripLineFill(line string, width int, fill lipgloss.Style) string {
	content := fill.Render(strings.Repeat(" ", tui.FrameInset)) + line
	slack := width - lipgloss.Width(content)
	if slack <= 0 {
		return content
	}
	return content + fill.Render(strings.Repeat(" ", slack))
}

// defaultGlyphFor is the status-column glyph a row falls back to when its
// own projection leaves Row.Glyph unset (every kind but Pods today).
func defaultGlyphFor(class resources.StatusClass) string {
	switch class {
	case resources.StatusOK:
		return tui.GlyphRunning
	case resources.StatusWarn:
		return tui.GlyphPending
	case resources.StatusFail:
		return tui.GlyphFailed
	default:
		return tui.GlyphCompleted
	}
}

// healthTone applies the kind descriptor's own class→colour remap
// (resources.Descriptor.HealthTone) — identity for every kind but Flux,
// whose suspended rows stay Neutral for counting and label purposes while
// rendering amber. Nil-guarded because a Model can hold a zero Descriptor
// before its kind resolves.
func (m Model) healthTone(class resources.StatusClass) resources.StatusClass {
	if m.desc.HealthTone == nil {
		return class
	}
	return m.desc.HealthTone(class)
}

func glyphColor(theme tui.Theme, class resources.StatusClass) color.Color {
	switch class {
	case resources.StatusOK:
		return theme.Good
	case resources.StatusWarn:
		return theme.Warn
	case resources.StatusFail:
		return theme.Bad
	default:
		return theme.Info
	}
}

// padBetween places left-aligned left and right-aligned right within width,
// measuring already-styled (ANSI-containing) strings via lipgloss.Width.
// When there isn't room for both, right is dropped.
func padBetween(left, right string, width int) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// padBetweenFill is padBetween's background-filled counterpart, for the one
// strip line (4a's offline banner) with a real bg fill — see
// insetStripLineFill's doc comment.
func padBetweenFill(left, right string, width int, fill lipgloss.Style) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + fill.Render(strings.Repeat(" ", gap)) + right
}

func (m Model) Body(width, height int) string {
	if m.pendingSetImage != nil {
		return m.setImageBody(width, height)
	}
	if m.pendingSetResources != nil {
		return m.setResourcesBody(width, height)
	}
	if m.pendingMeta != nil {
		return m.setMetaBody(width, height)
	}
	if m.pendingBulkDelete != nil && m.pendingBulkDelete.tier == actions.TierModal {
		return m.bulkDeleteConfirmModal(width, height)
	}
	if m.actions.Active() && m.actions.Tier() == actions.TierModal {
		if m.pendingBulkCronJobSuspend() {
			// A marked-set suspend has no single ResourceName for
			// typeNameConfirmModal's grammar — cronjob_bulk.go's own
			// type-the-count modal instead (0.8.0 plan §3 Phase 5 task 8).
			return m.bulkCronJobSuspendConfirmModal(width, height)
		}
		if pending := m.actions.Pending(); pending != nil && actions.RequiresTypedName(pending.Scope.Verb) {
			return m.typeNameConfirmModal(width, height)
		}
		return m.confirmBody(width, height)
	}
	switch m.state {
	case tui.TaskStateEmpty:
		return m.emptyBody(width, height)
	case tui.TaskStateReady:
		return m.tableBody(width, height)
	case tui.TaskStatePermissionDenied:
		return m.permissionDeniedBody(width, height)
	case tui.TaskStateError:
		return m.loadErrorBody(width, height)
	case tui.TaskStateLoading:
		if m.cachedView && len(m.rows) > 0 {
			// 15a: "revisiting a kind seen this session: cached rows dimmed
			// instead of skeletons" — render the real table (muted) over the
			// rowCache snapshot rather than the skeleton loader.
			return m.tableBody(width, height)
		}
		return m.loadingBody(width, height)
	default:
		return components.CenterLines([]string{m.feedback}, width, height)
	}
}

// loadErrorBody is the retryable counterpart to permissionDeniedBody. The
// informer continues retrying the initial LIST underneath this screen, so the
// card explains the failure without presenting it as a permanent outage.
func (m Model) loadErrorBody(width, height int) string {
	theme := m.Theme()
	onCard := func(fg color.Color) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(fg).Background(theme.ErrCardBg)
	}
	title := onCard(theme.Bad).Bold(true)
	meta := onCard(theme.TextFaint)
	body := onCard(theme.TextSecondary)
	key := onCard(theme.Accent)
	label := onCard(theme.TextSecondary)
	note := onCard(theme.TextFaint)
	footerNote := lipgloss.NewStyle().Foreground(theme.TextFaint)
	errBody := body.Width(max(20, min(66, width-14)))

	kind := lowerDisplay(m.desc.Display)
	errText := m.feedback
	if errText == "" {
		errText = "the initial list request failed"
	} else {
		errText = strings.TrimPrefix(errText, "couldn't load "+kind+": ")
		errText = strings.TrimSuffix(errText, " — retrying")
	}
	lines := []string{
		title.Render("Couldn't load "+kind) + meta.Render("  list"),
		errBody.Render(errText),
		"",
		note.Render("The connection will keep retrying automatically."),
		"",
		key.Render("r") + label.Render(" retry now"),
		key.Render("g") + label.Render(" jump to another kind"),
		key.Render("c") + label.Render(" switch context"),
		key.Render("y") + label.Render(" copy error"),
	}

	cardStyle := lipgloss.NewStyle().
		Foreground(theme.TextSecondary).
		Background(theme.ErrCardBg).
		BorderForeground(theme.ErrCardBorder).
		Padding(1, 2).
		Width(min(72, max(width-8, 20)))
	card := components.Card(strings.Join(lines, "\n"), cardStyle, width, height-2)
	footer := footerNote.Render("last successful list: never · retrying in the background")
	return card + "\n" + components.Pad(strings.Repeat(" ", max((width-lipgloss.Width(footer))/2, 0))+footer, width)
}

// quotedEntity matches a "..."-quoted span in an RBAC message ("dev-readonly",
// "secrets", "nva-stage", …), for 4b's yellow entity highlighting.
var quotedEntity = regexp.MustCompile(`"[^"]*"`)

// highlightQuoted renders text with base, except any "quoted" spans (RBAC
// messages name the denied user/verb/resource/namespace this way) which
// render through highlight instead — docs/design README.md §4b.
func highlightQuoted(text string, base, highlight lipgloss.Style) string {
	var b strings.Builder
	last := 0
	for _, span := range quotedEntity.FindAllStringIndex(text, -1) {
		b.WriteString(base.Render(text[last:span[0]]))
		b.WriteString(highlight.Render(text[span[0]:span[1]]))
		last = span[1]
	}
	b.WriteString(base.Render(text[last:]))
	return b.String()
}

// permissionDeniedBody is 4b's centered 403 card: title + kind/verb, the
// verbatim RBAC message with quoted entities highlighted, and the recovery
// lines (docs/design README.md §4b) — header stays connected/green (this
// screen only renders when the *load*, not the connection, failed) and
// there's deliberately no auto-retry, only the manual 'r'.
func (m Model) permissionDeniedBody(width, height int) string {
	theme := m.Theme()
	// Every span inside the card carries the card's own background: a
	// foreground-only style ends its run with a full SGR reset, which drops
	// the background for the rest of the line and leaves the card patchy.
	// The card's fill is only invisible-when-wrong while it matches the page.
	onCard := func(fg color.Color) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(fg).Background(theme.ErrCardBg)
	}
	title := onCard(theme.Bad).Bold(true)
	meta := onCard(theme.TextFaint)
	body := onCard(theme.TextSecondary)
	entity := onCard(theme.Warn)
	key := onCard(theme.Accent)
	label := onCard(theme.TextSecondary)
	note := onCard(theme.TextFaint)
	footerNote := lipgloss.NewStyle().Foreground(theme.TextFaint)

	recover := func(k, l, n string) string {
		line := key.Render(k) + label.Render(" "+l)
		if n != "" {
			line += note.Render(" — " + n)
		}
		return line
	}

	lines := []string{
		title.Render("403 Forbidden") + meta.Render("  "+lowerDisplay(m.desc.Display)+" · list"),
		highlightQuoted(m.feedback, body, entity),
		"",
		recover("g", "jump to another kind", "everything else still works"),
		recover("c", "switch context", "another context may grant access"),
		recover("w", "who-can", "see who does have access"),
		recover("y", "copy error", "paste to your cluster admin"),
		recover("r", "retry", ""),
	}
	if hint := m.namespaceScopedHint(); hint != "" {
		lines = append(lines, "", note.Render(hint))
	}

	cardStyle := lipgloss.NewStyle().
		Foreground(theme.TextSecondary).
		Background(theme.ErrCardBg).
		BorderForeground(theme.ErrCardBorder).
		Padding(1, 2).
		Width(min(60, max(width-8, 20)))
	card := components.Card(strings.Join(lines, "\n"), cardStyle, width, height-2)
	footer := footerNote.Render("last successful list: never · RBAC errors are not retried automatically")
	return card + "\n" + components.Pad(strings.Repeat(" ", max((width-lipgloss.Width(footer))/2, 0))+footer, width)
}

// emptyBody is the 10c centered explainer: "no <kind> in <ns>" plus the
// three live ways out. Each line is built from pre-styled spans (never a
// raw hex literal) per the Theme invariant.
func (m Model) emptyBody(width, height int) string {
	theme := m.Theme()
	text := lipgloss.NewStyle().Foreground(theme.TextPrimary)
	ns := lipgloss.NewStyle().Foreground(theme.AccentHi)
	dim := lipgloss.NewStyle().Foreground(theme.TextDim)
	key := lipgloss.NewStyle().Foreground(theme.Accent)
	detail := lipgloss.NewStyle().Foreground(theme.TextFaint)

	kind := lowerDisplay(m.desc.Display)
	var lines []string
	if m.desc.ClusterScoped {
		// Cluster-scoped kinds (Nodes, Namespaces, Forwards) have no
		// namespace to name — "no X in <ns>" would misdescribe scope the
		// same way the header breadcrumb already drops the namespace
		// segment for them (docs/design README.md §11a/§13c).
		lines = []string{
			text.Render("no " + kind + " yet"),
			dim.Render("the cluster is reachable — there's just nothing here yet"),
			"",
		}
	} else {
		lines = []string{
			text.Render("no "+kind+" in ") + ns.Render(m.namespace),
			dim.Render("the namespace exists and you can read it — there's just nothing here"),
			"",
		}
	}

	if !m.desc.ClusterScoped {
		nsLine := key.Render("n") + " " + dim.Render("switch namespace")
		if m.hints.altNamespace != "" {
			nsLine += " " + detail.Render(fmt.Sprintf("— %s has %d %s", m.hints.altNamespace, m.hints.altCount, kind))
		}
		lines = append(lines, nsLine)

		allLine := key.Render("a") + " " + dim.Render("all namespaces")
		if m.hints.allCount > 0 {
			allLine += " " + detail.Render(fmt.Sprintf("— %d %s cluster-wide", m.hints.allCount, kind))
		}
		lines = append(lines, allLine)

		gotoLine := key.Render("g") + " " + dim.Render("other kinds")
		if len(m.hints.otherKinds) > 0 {
			parts := make([]string, len(m.hints.otherKinds))
			for i, h := range m.hints.otherKinds {
				parts[i] = h.label
			}
			gotoLine += " " + detail.Render("— this namespace has "+strings.Join(parts, ", "))
		}
		lines = append(lines, gotoLine)
	}

	return components.CenterLines(lines, width, height)
}

// rowCellStyles is the docs/design §2a per-column palette, resolved from
// theme once per render: only the status glyph and STATUS text carry the
// status color; the name is TextPrimary (BadText when crashlooping), RDY
// TextSecondary, restarts Warn when non-zero, and NODE/AGE/metric values
// TextDim. selBg carries the selection background: for the selected row
// every style (and every ANSI span embedded in a cell — match highlights,
// metric bars) is derived with Background(SelBg), because Table renders the
// selected row per-cell rather than flattening it (mockup 2a: only the name
// brightens).
type rowCellStyles struct {
	name, nameBad, ready, restartsZero, restartsHot, dim, match lipgloss.Style
	bars                                                        components.BarStyles
	status                                                      map[resources.StatusClass]lipgloss.Style
	// customNeutral is 14a's "no status semantics" fallback glyph color
	// (TextFaint) — distinct from status[StatusNeutral]'s Info blue, which
	// every other kind's legitimately-neutral rows (Completed pods,
	// cordoned nodes) keep using.
	customNeutral lipgloss.Style
	// faint is §36a's second dim tier: a suspended CronJob row's SCHEDULE/
	// ACT/LAST RUN/NEXT cells (dim's own name/glyph tier reads brighter,
	// mockup 36a's own two-step fade). Same TextFaint foreground as
	// customNeutral, kept as its own field since the two exist for
	// unrelated reasons and reusing one for the other would read as a
	// coincidence rather than a shared token.
	faint lipgloss.Style
}

// newRowCellStyles resolves the table's per-cell colors from theme, or —
// when muted — from Theme.Muted plus one step down the text ramp (4a's
// desaturated snapshot: docs/design README.md "filter: saturate(0.35);
// opacity: 0.66"). Each theme declares its own muted ramp rather than this
// computing one, so the two render paths only ever differ in which palette
// they read from. marked bakes in 20a's quieter MarkBg tint instead of SelBg
// for a marked-but-not-selected row — mutually exclusive with selected, since
// "cursor keeps the purple bar" wins when both apply (callers never pass
// both true).
func newRowCellStyles(theme tui.Theme, selected, muted, marked bool) rowCellStyles {
	style := func(fg color.Color) lipgloss.Style {
		s := lipgloss.NewStyle().Foreground(fg)
		switch {
		case selected:
			s = s.Background(theme.SelBg)
		case marked:
			s = s.Background(theme.MarkBg)
		}
		return s
	}
	good, warnColor, bad, info, primary, secondary, dimColor := theme.Good, theme.Warn, theme.Bad, theme.Info, theme.TextPrimary, theme.TextSecondary, theme.TextDim
	if muted {
		good, warnColor, bad, info = theme.Muted.Good, theme.Muted.Warn, theme.Muted.Bad, theme.Muted.Info
		primary, secondary, dimColor = theme.TextSecondary, theme.TextDim, theme.TextFaint
	}

	name := style(primary)
	if selected {
		name = style(theme.Text) // the one cell that brightens on selection
	}
	nameBad := style(theme.BadText)
	if muted {
		nameBad = style(bad)
	}
	if selected {
		nameBad = name // selection brightening wins over the crashloop tint (mock 2a)
	}
	warn := style(warnColor)
	return rowCellStyles{
		name:         name,
		nameBad:      nameBad,
		ready:        style(secondary),
		restartsZero: style(dimColor),
		restartsHot:  warn,
		dim:          style(dimColor),
		match:        style(theme.AccentHi).Bold(true),
		bars: components.BarStyles{
			Track: style(theme.BarTrack),
			Fill:  style(theme.Accent),
			Warn:  warn,
			// These bars are relative to the busiest visible pod, so the
			// busiest one always sits at ratio 1.0 — that's not "at limit",
			// and 2a has no red bar tier (fill purple, yellow ≥70%). Bad
			// stays reserved for real request/limit bars (5a).
			Bad: warn,
		},
		status: map[resources.StatusClass]lipgloss.Style{
			resources.StatusOK:      style(good),
			resources.StatusWarn:    style(warnColor),
			resources.StatusFail:    style(bad),
			resources.StatusNeutral: style(info),
		},
		customNeutral: style(theme.TextFaint),
		faint:         style(theme.TextFaint),
	}
}

// tableBody renders the 2a resource table: per-status glyph + STATUS
// coloring, filter match highlighting on Name, live CPU/MEM mini-bars for
// Pods, and a footer range/scrollbar line under the table itself. In 6b's
// grouped mode, m.display (grouping.go's buildDisplayRows) interleaves data
// rows with group header/fold/collapsed-summary lines one-to-one with what
// gets rendered here, so m.selected — which indexes m.display — can be
// handed to Table.Selected directly.
func (m Model) tableBody(width, height int) string {
	theme := m.Theme()
	cols := browseColumns(m.desc)
	grouped := m.grouped()
	// The sorted column's header arrow (components.Table's own SortKey/
	// SortAsc → renderHeaderV2) — a manual 1-9 choice (sort.go) wins over
	// the built-in default, mutating cols[m.sortColumn] in place (rather
	// than pre-populating every column's Sort field) since only one column
	// is ever active at a time. This must happen before tableCols is
	// derived below, since a marked view's tableCols copies cols into a new
	// backing array — mutating cols afterward wouldn't reach it.
	sortKey, sortAsc := "", false
	switch {
	case m.sortColumn > 0 && !grouped:
		if m.sortColumn < len(cols) {
			cols[m.sortColumn].Sort = cols[m.sortColumn].Title
			sortKey, sortAsc = cols[m.sortColumn].Title, m.sortAsc
		}
	case workloadKinds[m.kind] && !grouped:
		// The sort indicator implies a straight global status sort; 6b
		// groups by namespace first, so showing "↑" on STATUS would
		// misdescribe the order.
		sortKey, sortAsc = "status", true
	}
	aggs := m.aggregates()
	cpuMax, memMax := aggs.cpuMax, aggs.memMax
	muted := m.offline() || m.cachedView
	styles := [2]rowCellStyles{newRowCellStyles(theme, false, muted, false), newRowCellStyles(theme, true, muted, false)}
	// marksOn/markedStyle back 20a's mark column and marked-row tint — the
	// mark column only exists while something's marked (13d's zero-chrome
	// rule), so tableCols stays exactly cols when nothing is.
	marksOn := len(m.marks) > 0
	markedStyle := styles[0]
	if marksOn {
		markedStyle = newRowCellStyles(theme, false, muted, true)
	}
	tableCols := cols
	if marksOn {
		tableCols = append([]components.Column{{Title: "", Min: 1}}, cols...)
	}
	majorityVersion := aggs.nodeMajority

	// One line reserved for FooterLine always; CronJobs reserve a second for
	// §36a's trailing behavior strip, docked below the table rather than
	// among Strips' top-of-screen lines (keep in sync with selection.go's
	// tableDataRows).
	reserved := 1
	cronJobStrip := m.kind == kube.KindCronJob && !m.offline()
	if cronJobStrip {
		reserved = 2
	}
	t := components.Table{
		Columns:        tableCols,
		RowsTotal:      len(m.display),
		Selected:       m.selected,
		Offset:         m.offset,
		Width:          width,
		Height:         max(height-reserved, 1),
		SortKey:        sortKey,
		SortAsc:        sortAsc,
		HeaderStyle:    lipgloss.NewStyle().Foreground(theme.TextFaint),
		SortStyle:      lipgloss.NewStyle().Foreground(theme.Accent),
		SelBarStyle:    lipgloss.NewStyle().Foreground(theme.Accent).Background(theme.SelBg),
		SelRowStyle:    lipgloss.NewStyle().Background(theme.SelBg),
		FooterStyle:    lipgloss.NewStyle().Foreground(theme.TextGhost),
		ShowHeaderRule: true,
		RuleStyle:      lipgloss.NewStyle().Foreground(theme.TextGhost2),
	}
	// Window first, style second (docs/review/03 H5, podlogs' visibleWindow
	// precedent): Table only ever draws [Start, End), so building and
	// styling cells for the rest of m.display was the whole of a 5,000-pod
	// frame's cost. The Table works out the window itself, so the slice
	// handed back is exactly the one it will draw.
	win := t.VisibleRange()
	rows := make([]components.Row, 0, win.End-win.Start)
	for idx := win.Start; idx < win.End; idx++ {
		dr := m.display[idx]
		selected := idx == m.selected
		switch dr.kind {
		case rowKindHeader:
			rows = append(rows, components.Row{
				GroupHeader: m.groupHeaderLine(dr.namespace, dr.counts),
				GroupStyle:  groupLineStyle(theme, rowKindHeader, selected),
				GroupRight:  groupHeaderChips(theme, dr.counts, selected),
			})
			continue
		case rowKindCollapsedSummary:
			rows = append(rows, components.Row{
				GroupHeader: m.collapsedSummaryLine(dr.namespace, dr.counts),
				GroupStyle:  groupLineStyle(theme, rowKindCollapsedSummary, selected),
			})
			continue
		case rowKindFold:
			rows = append(rows, components.Row{
				GroupHeader: m.foldLine(dr.folded),
				GroupStyle:  groupLineStyle(theme, rowKindFold, selected),
			})
			continue
		case rowKindSubLine:
			// Indented past the glyph column so it reads as a continuation
			// of the row above rather than a row of its own.
			rows = append(rows, components.Row{
				GroupHeader: "  " + dr.text,
				GroupStyle:  subLineStyle(theme, dr.subClass),
			})
			continue
		}

		fm := dr.row
		r := fm.row
		isMarkedRow := marksOn && m.isMarked(r)
		st := styles[0]
		switch {
		case selected:
			st = styles[1]
		case isMarkedRow:
			st = markedStyle
		}
		cells := m.rowCells(r, fm.matches, cols, width, st, theme, cpuMax, memMax, majorityVersion, marksOn, isMarkedRow)
		// RowStyle carries st.dim's background (MarkBg for a marked row, none
		// otherwise — Table's own SelRowStyle wins when selected regardless)
		// into the leading marker slot and inter-column gaps, which per-cell
		// Style alone doesn't reach (components.Table's renderRowV2).
		rows = append(rows, components.Row{Cells: cells, RowStyle: st.dim})
	}
	t.Rows, t.RowsStart = rows, win.Start
	out := t.Render() + "\n" + t.FooterLine(width)
	if cronJobStrip {
		out += "\n" + m.cronJobBehaviorStripLine(theme, width)
	}
	return out
}

// rowCells builds one data row's per-column cells with 2a's per-column
// styling — factored out of tableBody's loop above so setImageBody's frozen
// context row (setimage_view.go) renders through the exact same styling as
// the live table, with no pixel drift between the two. matches backs Name's
// filter-highlight spans (nil outside an active filter — setImageBody's
// caller never has one, since pendingSetImage can't open while filterActive).
// marksOn/isMarkedRow false skips the leading mark column entirely
// (setImageBody's one frozen row never renders it, 20a marks being
// orthogonal to a single row's set-image edit).
func (m Model) rowCells(r resources.Row, matches []int, cols []components.Column, width int, st rowCellStyles, theme tui.Theme, cpuMax, memMax int64, majorityVersion string, marksOn, isMarkedRow bool) []components.Cell {
	cells := resources.Cells(r, width, nil)
	// resources.Row's Cells align positionally with the descriptor's Columns,
	// and the model drops any rows that stop doing so (dropStaleShapedRows).
	// This clamp is the render path's own guarantee that it can never index
	// past the columns it was handed whatever the model believes: a panic
	// inside View takes the whole program down with it.
	if len(cells) > len(cols) {
		cells = cells[:len(cols)]
	}
	for i := range cells {
		switch {
		case i == 0: // status glyph column
			// GlyphClass is the row's own glyph coloring where a projection
			// set one, falling back to Status (resources.Row's documented
			// contract). They differ for exactly one case today: 18a's
			// outdated release, which stays `deployed` in the strip while
			// its glyph goes warn-yellow.
			glyphClass := r.Status
			if r.GlyphClass != "" {
				glyphClass = r.GlyphClass
			}
			if cells[i].Text == "" {
				cells[i].Text = defaultGlyphFor(glyphClass)
			}
			cells[i].Style = st.status[m.healthTone(glyphClass)]
			if m.desc.Custom && !m.desc.StatusSemantics && r.Status == resources.StatusNeutral {
				// docs/design README.md §14a: a CRD instance with no
				// Ready/Available condition at all — "never fake
				// health" — renders TextFaint, not the generic Neutral/
				// Info blue every other kind's Neutral class (Completed
				// pods, cordoned nodes) uses.
				cells[i].Style = st.customNeutral
			}
			if (m.kind == kube.KindCronJob || m.kind == kube.KindJob) && r.Suspended {
				// §36a/§37a: a suspended row's glyph dims to plain TextDim
				// rather than StatusNeutral's Info blue — "suspended" isn't
				// a status class here, ProjectCronJob/ProjectJobList's own
				// priority order puts it ahead of last-terminal-outcome
				// entirely. §37a: "same treatment as suspended CronJobs".
				cells[i].Style = st.dim
			}
		case m.kind == kube.KindPod && cols[i].Title == "CPU":
			cells[i] = m.metricCell(r.Namespace, r.Name, true, cpuMax, st)
		case m.kind == kube.KindPod && cols[i].Title == "MEM":
			cells[i] = m.metricCell(r.Namespace, r.Name, false, memMax, st)
		case m.kind == kube.KindNode && cols[i].Title == "CPU":
			cells[i] = m.nodeMetricCell(r.Name, true, st)
		case m.kind == kube.KindNode && cols[i].Title == "MEM":
			cells[i] = m.nodeMetricCell(r.Name, false, st)
		case m.kind == kube.KindNode && cols[i].Title == "Health":
			cells[i] = m.nodeHealthCell(r.Name, st)
		case m.kind == kube.KindNode && cols[i].Title == "Pods":
			cells[i] = components.Cell{Text: m.nodePodsCell(r.Name), Style: st.dim}
		case m.kind == kube.KindNode && cols[i].Title == "Version":
			text := cells[i].Text
			if majorityVersion != "" && text != majorityVersion {
				text += " " + tui.GlyphWarning
			}
			cells[i] = components.Cell{Text: text, Style: st.dim}
		case cols[i].Title == "Name":
			base := st.name
			if r.Status == resources.StatusFail {
				base = st.nameBad
			}
			if (m.kind == kube.KindCronJob || m.kind == kube.KindJob) && r.Suspended {
				// §36a/§37a: "suspended rows dimmed with a struck-through
				// NAME" — row-style, not ANSI embedded into the name string
				// itself.
				base = st.dim.Strikethrough(true)
			}
			text := highlightName(r.Name, matches, st.match, base)
			if r.NameSuffix != "" {
				text += st.dim.Render(r.NameSuffix)
			}
			cells[i] = components.Cell{Text: text}
		case cols[i].Title == "Rdy":
			cells[i].Style = st.ready
		case m.kind == kube.KindNode && cols[i].Title == "Status":
			// docs/design README.md §11a: "healthy state renders dim,
			// not green" — the same carve-out 9a's ROLLOUT column
			// already gets (rowKindHeader case above), extended to
			// Nodes' own STATUS column: Ready dims, NotReady/other keep
			// the usual status color.
			cells[i].Style = st.dim
			if r.Status != resources.StatusOK {
				cells[i].Style = st.status[r.Status]
			}
		case cols[i].Title == "Status":
			cells[i].Style = st.status[r.Status]
		case cols[i].Title == "Rollout":
			// docs/design README.md §9a: "stable dim" — unlike STATUS,
			// the healthy rollout state renders dim rather than green;
			// progressing/degraded keep their usual warn/bad coloring.
			cells[i].Style = st.dim
			if r.Status != resources.StatusOK {
				cells[i].Style = st.status[r.Status]
			}
		case m.desc.Argo && cols[i].Title == "Sync":
			// §33a: sync and health are independent axes. Quiet sync values
			// stay secondary while drift gets its own warning color, even
			// when the application's aggregate status is Degraded.
			cells[i].Style = st.ready
			if cells[i].Text == "OutOfSync" {
				cells[i].Style = st.status[resources.StatusWarn]
			}
		case m.desc.Argo && cols[i].Title == "Health":
			// Healthy/Progressing are intentionally neutral in the design;
			// only terminal health values carry the failure color.
			cells[i].Style = st.ready
			if cells[i].Text == "Degraded" || cells[i].Text == "Missing" {
				cells[i].Style = st.status[resources.StatusFail]
			}
		case m.kind == kube.KindCertificate && cols[i].Title == "Ready":
			// §35b: same "healthy dim, not green" idiom as Rollout/Node
			// Status above. Kind-gated — Flux already owns an uncolored
			// "Ready" column of its own and must not be repainted.
			cells[i].Style = st.dim
			if r.Status != resources.StatusOK {
				cells[i].Style = st.status[r.Status]
			}
		case cols[i].Title == "Expires":
			// §35b: yellow <30d, red expired/not-ready, dim otherwise —
			// resources.projectCertificate's own ExpiresClass, not Status,
			// since a ready-but-expiring cert's Status stays StatusOK.
			cells[i].Style = st.dim
			if r.ExpiresClass == resources.StatusWarn || r.ExpiresClass == resources.StatusFail {
				cells[i].Style = st.status[r.ExpiresClass]
			}
		case cols[i].Title == "Renewal":
			cells[i].Style = st.dim
			if r.RenewalClass == resources.StatusWarn || r.RenewalClass == resources.StatusFail {
				cells[i].Style = st.status[r.RenewalClass]
			}
		case m.kind == kube.KindJob && cols[i].Title == "Duration":
			// §37a: "turns red when it hits activeDeadlineSeconds" —
			// resources.ProjectJobList's own DurationClass, independent of
			// the row's own Status (a Job can still be Active/Warn while its
			// DURATION cell alone has already crossed the deadline).
			cells[i].Style = st.dim
			if r.DurationClass == resources.StatusFail {
				cells[i].Style = st.status[resources.StatusFail]
			}
		case m.kind == kube.KindCronJob && cols[i].Title == "Schedule":
			cells[i].Style = st.dim
			if r.Suspended {
				cells[i].Style = st.faint
			}
		case m.kind == kube.KindCronJob && cols[i].Title == "Susp":
			cells[i].Style = st.dim
			if r.Suspended {
				cells[i].Style = st.status[resources.StatusWarn]
			}
		case m.kind == kube.KindCronJob && cols[i].Title == "Act":
			switch {
			case r.Suspended:
				cells[i].Style = st.faint
			case r.Active:
				cells[i].Style = st.status[resources.StatusWarn]
			default:
				cells[i].Style = st.dim
			}
		case m.kind == kube.KindCronJob && cols[i].Title == "Last Run":
			switch {
			case r.Suspended:
				cells[i].Style = st.faint
			case r.Status == resources.StatusFail:
				cells[i].Style = st.status[resources.StatusFail]
			default:
				cells[i].Style = st.dim
			}
		case m.kind == kube.KindCronJob && cols[i].Title == "Next":
			cells[i].Style = st.dim
			if r.Suspended {
				cells[i].Style = st.faint
			}
		case cols[i].Title == tui.GlyphRestarts:
			cells[i].Style = st.restartsHot
			if cells[i].Text == "0" {
				cells[i].Style = st.restartsZero
			}
		default:
			cells[i].Style = st.dim
		}
	}
	if marksOn {
		// 20a's leading mark cell: ▪ (purple) when marked, blank
		// otherwise — st.dim already carries the right background
		// (SelBg/MarkBg/none) for this row, so overriding just the
		// foreground keeps the row's background one continuous fill.
		markCell := components.Cell{Style: st.dim}
		if isMarkedRow {
			markCell = components.Cell{Text: tui.GlyphMarked, Style: st.dim.Foreground(theme.Accent)}
		}
		cells = append([]components.Cell{markCell}, cells...)
	}
	return cells
}

// groupHeaderLine draws one 6b namespace group's ▾ heading (docs/design
// README.md §6b): name, row count, and a trouble summary — "all running"
// when every row in the group is healthy (only reachable once the group is
// manually expanded — a collapsed, fully-healthy group renders through
// collapsedSummaryLine instead), else the non-zero classes named via the
// kind's own HealthLabel wording (worst first). Table.GroupHeader renders
// through a single per-row style (no per-segment coloring like the 2a
// health strip's per-glyph colors), so this stays plain text; Table owns
// the leading "  "/"▎ " marker column, so this — like collapsedSummaryLine/
// foldLine — doesn't add its own.
func (m Model) groupHeaderLine(namespace string, counts resources.HealthCounts) string {
	name := namespace
	if name == "" {
		name = "(none)"
	}
	line := fmt.Sprintf("%s %s · %d %s", tui.GlyphCollapse, name, counts.Total(), lowerDisplay(m.desc.Display))
	if counts.Fail+counts.Warn == 0 {
		// Nothing to flag on the right — an expanded, fully-healthy group
		// (collapsedSummaryLine handles the default collapsed case) still
		// says so inline, same as before this had right-aligned chips.
		line += " · all running"
	}
	return line
}

// groupHeaderChips builds 6b's right-aligned trouble chips (docs/design
// README.md §6b: "right-aligned trouble chips (▲2 ✕1)") — one per non-zero
// class, each its own status color, replacing the inline "N crashloop · M
// pending" text groupHeaderLine used to append. Empty when the group has
// nothing to flag.
func groupHeaderChips(theme tui.Theme, counts resources.HealthCounts, selected bool) []components.Cell {
	bg := theme.BgChrome
	if selected {
		bg = theme.SelBg
	}
	var cells []components.Cell
	add := func(class resources.StatusClass, n int) {
		if n == 0 {
			return
		}
		style := lipgloss.NewStyle().Foreground(glyphColor(theme, class)).Background(bg)
		cells = append(cells, components.Cell{Text: fmt.Sprintf("%s%d", defaultGlyphFor(class), n), Style: style})
	}
	add(resources.StatusWarn, counts.Warn)
	add(resources.StatusFail, counts.Fail)
	return cells
}

// collapsedSummaryLine draws 6b's fully-healthy group's single line,
// replacing the ▾ header entirely: the ▸ glyph, namespace, total count, and
// "all running" — green (groupLineStyle), the app's only other collapse/
// expand affordance besides yamlview's managedFields fold.
func (m Model) collapsedSummaryLine(namespace string, counts resources.HealthCounts) string {
	name := namespace
	if name == "" {
		name = "(none)"
	}
	return fmt.Sprintf("%s %s · %d %s · all running", tui.GlyphExpand, name, counts.Total(), lowerDisplay(m.desc.Display))
}

// foldLine draws 6b's "+N running · ↹ expand" tail standing in for a
// partially-shown group's folded healthy rows.
func (m Model) foldLine(folded int) string {
	return fmt.Sprintf("+ %d %s · %s expand", folded, m.desc.HealthLabel(resources.StatusOK), tui.GlyphTab)
}

// groupLineStyle resolves 6b's three group-line colors from theme — purple
// for the normal ▾ header (AccentHi, unchanged from pre-collapse), gray for
// both the fully-healthy ▸ summary and the "+N running" fold tail
// (TextFaint) — a fully-healthy namespace has nothing to triage, so it's
// deliberately de-emphasized rather than drawing the eye with green — baking
// in theme.SelBg when selected, the same selected-variant convention
// newRowCellStyles already uses for cells, so a selected group line's
// background reads as one continuous fill across Table's bar + content
// split (renderGroupRowV2).
// subLineStyle colors a §30a continuation line: muted rather than the full
// status hue, so the verbatim condition message reads as supporting detail
// and doesn't compete with the glyph column it explains. Never selected —
// selectableStop keeps the cursor off these lines.
func subLineStyle(theme tui.Theme, class resources.StatusClass) lipgloss.Style {
	fg := theme.TextFaint
	switch class {
	case resources.StatusFail:
		fg = theme.BadMuted
	case resources.StatusWarn, resources.StatusNeutral:
		fg = theme.Muted.Warn
	}
	return lipgloss.NewStyle().Foreground(fg)
}

func groupLineStyle(theme tui.Theme, kind displayRowKind, selected bool) lipgloss.Style {
	fg := theme.AccentHi
	switch kind {
	case rowKindCollapsedSummary, rowKindFold:
		fg = theme.TextFaint
	}
	s := lipgloss.NewStyle().Foreground(fg)
	switch {
	case selected:
		s = s.Background(theme.SelBg)
	case kind == rowKindHeader:
		// docs/design README.md §6b: "group header line (bg #0e0e15)" — a
		// distinguishing fill the collapsed-summary/fold lines don't get.
		s = s.Background(theme.BgChrome)
	}
	return s
}

// metricCell renders one Pod row's CPU/MEM cell: a MiniBar scaled to the
// busiest currently-visible pod, then the metric's compact value in dim
// (mockup 2a's bar-then-number order; no request/limit data reaches
// browse's Row, so this is a relative-usage bar rather than a request/limit
// zone bar) — "–" for both while metrics haven't loaded yet.
func (m Model) metricCell(namespace, name string, cpu bool, maxVal int64, st rowCellStyles) components.Cell {
	const barWidth = 6
	valWidth := resources.MetricColumnWidth - barWidth - 1

	pm, ok := m.podMetrics[kube.PodKey(namespace, name)]
	value, used := "–", int64(0)
	if ok {
		if cpu {
			value, used = pm.CPU, pm.CPUMilli
		} else {
			value, used = pm.MEM, pm.MemBytes
		}
		if value == "" || value == "n/a" {
			value = "–"
		}
	}
	// The separating space renders through st.dim too, so a selected row's
	// background stays continuous across the cell.
	valText := st.dim.Render(" " + components.Truncate(value, valWidth))
	return components.Cell{Text: components.MiniBar(used, maxVal, barWidth, st.bars) + valText}
}

// metricsMax finds the busiest CPU/MEM usage across the currently loaded
// rows, the bar denominator described in metricCell.
func (m Model) metricsMax() (cpuMax, memMax int64) {
	for _, r := range m.rows {
		if pm, ok := m.podMetrics[kube.PodKey(r.Namespace, r.Name)]; ok {
			cpuMax = max(cpuMax, pm.CPUMilli)
			memMax = max(memMax, pm.MemBytes)
		}
	}
	return cpuMax, memMax
}
