package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/components/palette"
)

// namespaceColumnHeadersFor builds 6a's count/HEALTH/CPU column headers
// (docs/design README.md §6a) for the kind namespaceCountDescriptor
// resolved: the first column's label follows the active kind ("PODS",
// "DEPLOYMENTS", "INGRESSES", …) so the palette visibly names what it's
// counting. Width clamps to [5, 14] — 5 keeps the common Pods case
// pixel-identical to the old fixed header, 14 accommodates the longer
// built-in kind names without crowding the NAMESPACE flex column; anything
// longer (a verbose discovered CRD name) truncates with components.
// Truncate's existing "…" handling when rendered. HEALTH's four glyph
// segments ("●999 ▲99 ✕99 ○9") fit inside 13 with margin to spare for
// realistic cluster sizes; CPU is unaffected by the active kind.
func namespaceColumnHeadersFor(desc resources.Descriptor) []palette.ColumnHeader {
	label := strings.ToUpper(desc.Display)
	width := min(max(len(label), 5), 14)
	return []palette.ColumnHeader{
		{Label: label, Width: width, Align: components.AlignLeft},
		{Label: "HEALTH", Width: 13, Align: components.AlignLeft},
		{Label: "CPU", Width: 5, Align: components.AlignRight},
	}
}

// namespaceCountDescriptor resolves which kind's per-namespace counts/health
// the palette shows: the kind browse currently has open
// (sess.Location.Kind), or the Pod descriptor — today's behavior — when
// that kind is unset (before the first navigation), unregistered, or
// ClusterScoped (Nodes, Namespaces, Forwards, some CRDs have no meaningful
// per-namespace count).
func namespaceCountDescriptor(sess *Session) resources.Descriptor {
	if sess == nil {
		return resources.Descriptor{}
	}
	podDesc, _ := sess.Registry.Descriptor(kube.KindPod)
	desc, ok := sess.Registry.Descriptor(sess.Location.Kind)
	if !ok || desc.ClusterScoped {
		return podDesc
	}
	return desc
}

// namespaceGutterGlyph is 6a's selected-row "▸" gutter — distinct from
// 12a/7a, which explicitly have no gutter column.
const namespaceGutterGlyph = "▸"

// This file wires real data into the 'n' namespace palette (mvp-plan.md
// Phase 3, docs/design README.md §6a): namespaces are listed with a live pod
// count, the current one tagged, and "all namespaces" pinned as a first-class
// last row. It follows goto.go's pattern — the palette package itself stays
// pure UI.

// namespaceTarget is the opaque payload carried in palette.Item.Data for a
// namespace-scope item; namespace == "" means the "all namespaces" row.
type namespaceTarget struct {
	namespace string
}

// namespaceHint is the palette's right-hand input-row hint (docs/design
// README.md §6a: "microk8s-cluster · 6 namespaces").
func namespaceHint(sess *Session) string {
	if sess == nil {
		return ""
	}
	ctx := sess.Location.Context
	if ctx == "" {
		ctx = "cluster"
	}
	return fmt.Sprintf("%s · %d namespaces", ctx, namespaceCount(sess))
}

func namespaceCount(sess *Session) int {
	if sess == nil || sess.Lister == nil {
		return 0
	}
	desc, ok := sess.Registry.Descriptor(kube.KindNamespace)
	if !ok {
		return 0
	}
	n, err := resources.Count(sess.ClusterContext(), sess.Lister, desc.Kind, "")
	if err != nil {
		return 0
	}
	return n
}

// namespaceItems lists every namespace (PODS/HEALTH columns, dim when
// empty, current tagged) plus the pinned "all namespaces" last row,
// separated by a top rule (docs/design README.md §6a), and the namespace
// rows fetched alongside them for a later CPU-share fetch. HEALTH's tally
// is a byproduct of listing each namespace's pods (resources.StatusHealth
// over the same rows resources.Count used to just measure the length of),
// so it's free once counts need a per-namespace List anyway — no extra
// cluster round trip versus the plain-count version this replaced.
//
// CPU share is deliberately not fetched here: it needs one
// PodMetricsByNamespace (a live metrics-server call) per namespace, slow
// enough on a bad connection that doing it synchronously made every palette
// open feel frozen despite PODS/HEALTH themselves being a fast,
// informer-cache-backed read. openPalette shows this fast list immediately
// and kicks off fetchNamespaceCPUSharesCmd separately so the CPU column
// fills in once it lands instead of blocking the open.
// namespaceSyncRetryInterval is how often the namespace palette re-checks
// the informer cache while it's still filling (mirrors browse's
// reloadDebounce).
const namespaceSyncRetryInterval = 250 * time.Millisecond

// namespaceSyncRetryMsg drives the namespace palette's retry loop while the
// cache isn't synced yet; gen is checked against Model.namespaceGen so a
// stale retry (from a since-closed/reopened palette) is dropped.
type namespaceSyncRetryMsg struct{ gen int }

func scheduleNamespaceSyncRetry(gen int) tea.Cmd {
	return tea.Tick(namespaceSyncRetryInterval, func(time.Time) tea.Msg {
		return namespaceSyncRetryMsg{gen: gen}
	})
}

// namespaceForbidden reports the denial behind the Namespace cache, or nil
// if it's readable (or the session/lister can't say) — the namespace
// palette's own KindForbidden question, asked at namespace "" since
// Namespace is always a cluster-scoped kind regardless of mode.
func namespaceForbidden(sess *Session) error {
	if sess == nil {
		return nil
	}
	fr, ok := sess.Lister.(KindForbiddenReporter)
	if !ok {
		return nil
	}
	return fr.KindForbidden(kube.KindNamespace, "")
}

// loadNamespacePalette populates m.palette from namespaceItems for gen, or
// — while the Namespace informer's cache is still empty just after launch
// or mid SwitchContext — puts the palette in 6a's loading state and retries
// shortly rather than flashing "no matches" for a cluster that actually has
// namespaces (mirrors browse's listerSynced retry in applyRowsLoaded).
//
// A permanent denial (docs/lazy-informers.md §5.6 — the
// shape a namespace-bound identity actually hits) short-circuits both: it
// will never sync, so waiting on it would spin forever, and there is
// nothing to list, so the palette switches to the denied notice + a
// free-typed "switch to" input instead of the browsable list. Checked before
// the sync question below, since a denied cache also never reports synced
// and would otherwise loop the retry indefinitely.
//
// The sync question asked is specifically about the Namespace cache. It used
// to be the cluster-wide one, which only flipped true once every registered
// informer (Pods, RBAC, HPA, …) had synced — so on a cluster whose RBAC
// forbids listing one of them, it never flipped, and this palette hung on
// "loading namespaces…" forever even though its own data had arrived long
// before. The rows check remains as well, so a populated list renders
// immediately whatever the cache reports.
func (m *Model) loadNamespacePalette(gen int) tea.Cmd {
	if err := namespaceForbidden(m.session); err != nil {
		m.palette.Loading = false
		m.palette.Denied = true
		m.palette.DeniedNote = "cannot list namespaces"
		m.palette.Items = nil
		m.palette.Input.Placeholder = "switch to"
		m.palette.Recent = namespaceRecentLabels(m.session)
		m.palette.Footer = namespaceDeniedFooter(m.session)
		return nil
	}
	m.palette.Denied = false
	items, rows := namespaceItems(m.session)
	if len(rows) == 0 && m.session != nil && !KindsSynced(m.session.Lister, "", kube.KindNamespace) {
		m.palette.Loading = true
		m.palette.Items = nil
		return scheduleNamespaceSyncRetry(gen)
	}
	m.palette.Loading = false
	m.namespaceItemsCache = items
	m.refreshNamespacePalette()
	return fetchNamespaceCPUSharesCmd(m.session, rows, gen)
}

// namespaceDeniedFooter names the current namespace on the denied palette's
// footer line ("currently nva-prod") — the one piece of orientation a
// browsable list would otherwise have supplied via the "current" row tag.
func namespaceDeniedFooter(sess *Session) []palette.FooterSpan {
	if sess == nil || sess.Location.Namespace == "" {
		return nil
	}
	return []palette.FooterSpan{
		{Text: "currently ", Tone: palette.FooterDim},
		{Text: sess.Location.Namespace, Tone: palette.FooterEm},
	}
}

func namespaceItems(sess *Session) ([]palette.Item, []resources.Row) {
	if sess == nil || sess.Lister == nil {
		return nil, nil
	}
	nsDesc, ok := sess.Registry.Descriptor(kube.KindNamespace)
	if !ok {
		return nil, nil
	}
	rows, err := resources.List(sess.ClusterContext(), sess.Lister, nsDesc, "")
	if err != nil {
		return nil, nil
	}

	// docs/lazy-informers.md §5.6: counting every
	// namespace's rows one at a time is exactly the breadth-first read
	// scoped mode forbids — under it, each of these resources.List calls
	// would start its own per-namespace informer just to decorate a palette
	// row. Count/HEALTH/CPU drop to the ghost dash instead; cluster-wide
	// mode keeps today's counts unchanged.
	scoped := sessionScoped(sess)

	countDesc := namespaceCountDescriptor(sess)
	ctx := sess.ClusterContext()
	counts := make([]int, len(rows))
	healths := make([]resources.HealthCounts, len(rows))
	// known[i] reports whether row i's count is a real reading. A
	// namespace is only counted when its cache for countDesc's kind is
	// already running and filled (KindCached): this is a breadth-first
	// read, so it may only touch what the session has already paid for.
	// For most kinds in cluster-wide mode one cache backs every namespace
	// and every row is counted as before; KindHelmRelease is cached per
	// namespace even there (docs/lazy-informers.md §5.5), and reading an
	// un-started namespace would start — and block the Update loop on —
	// its own release-Secret informer. An un-started or still-filling
	// cache gets the ghost dash, never a claimed 0.
	known := make([]bool, len(rows))
	allKnown := !scoped
	totalCount := 0
	var totalHealth resources.HealthCounts
	if !scoped {
		for i, row := range rows {
			if !KindCached(sess.Lister, countDesc.Kind, row.Name) {
				allKnown = false
				continue
			}
			countRows, err := resources.List(ctx, sess.Lister, countDesc, row.Name)
			if err != nil {
				allKnown = false
				continue
			}
			known[i] = true
			counts[i] = len(countRows)
			totalCount += len(countRows)
			healths[i] = countDesc.Health(countRows)
			totalHealth.OK += healths[i].OK
			totalHealth.Warn += healths[i].Warn
			totalHealth.Fail += healths[i].Fail
			totalHealth.Neutral += healths[i].Neutral
		}
	}

	recents := contextRecentNamespaces(sess)
	recentNums := recentNumbers(recents, sess.Location.Namespace)
	prevTarget, hasPrev := mostRecentOther(recents, sess.Location.Namespace)
	items := make([]palette.Item, 0, len(rows)+1)
	for i, row := range rows {
		item := palette.Item{
			Label: row.Name,
			Data:  namespaceTarget{namespace: row.Name},
		}
		if !known[i] {
			item.Cols = namespaceColsUnknown()
		} else {
			item.Dim = counts[i] == 0
			item.Cols = namespaceCols(counts[i], healths[i])
		}
		switch {
		case row.Name == sess.Location.Namespace:
			item.Tag = "current"
		case hasPrev && row.Name == prevTarget:
			item.Tag = "previous"
		}
		item.RecentNum = recentNums[row.Name]
		items = append(items, item)
	}
	promoteRecentItems(items)

	allItem := palette.Item{
		Label:   GlyphAllNS + " all namespaces",
		AllNS:   true,
		TopRule: true,
		Data:    namespaceTarget{namespace: ""},
		// CPU is intentionally left blank — the mockup's pinned
		// "all namespaces" row has no cluster-wide CPU-share value.
		Cols: []palette.Cell{
			{Text: strconv.Itoa(totalCount), Tone: palette.ToneSecondary},
			healthCell(totalHealth),
			{},
		},
	}
	if !allKnown {
		// totalCount/totalHealth miss at least one namespace (scoped mode
		// counts none; otherwise an un-started cache was skipped) — the
		// same unknown ghost dash the per-namespace rows get, not a
		// partial sum passed off as the cluster total.
		allItem.Cols = namespaceColsUnknown()
	}
	if sess.Location.Namespace == "" {
		allItem.Tag = "current"
	}
	items = append(items, allItem)
	return items, rows
}

// sessionScoped reports whether sess.Lister is running under
// --namespace-scoped mode — the single check namespaceItems/
// namespaceCPUShares consult before doing a per-namespace fan-out read.
func sessionScoped(sess *Session) bool {
	if sess == nil {
		return false
	}
	sc, ok := sess.Lister.(ScopedChecker)
	return ok && sc.Scoped()
}

// namespaceCols builds one row's count/HEALTH/CPU cells for the active kind
// (namespaceCountDescriptor). CPU starts as the ghost dash placeholder;
// applyNamespaceCPUShares fills it in once the background fetch lands. A
// zero-count namespace ghosts every cell, matching the mockup's "default 0 –
// –" row, and never gets a CPU value even once shares land
// (applyNamespaceCPUShares checks the same zero-count condition via Cols[0]'s
// tone).
func namespaceCols(count int, health resources.HealthCounts) []palette.Cell {
	if count == 0 {
		return []palette.Cell{
			{Text: "0", Tone: palette.ToneGhost},
			{Text: "–", Tone: palette.ToneGhost},
			{Text: "–", Tone: palette.ToneGhost},
		}
	}
	return []palette.Cell{
		{Text: strconv.Itoa(count), Tone: palette.ToneSecondary},
		healthCell(health),
		{Text: "–", Tone: palette.ToneGhost},
	}
}

// namespaceColsUnknown is namespaceCols' scoped-mode counterpart: every cell
// ghosted, for a namespace whose count genuinely wasn't read rather than one
// known to be zero (namespaceCols' own count==0 branch above) — the two
// must render the same way (docs/lazy-informers.md §5.6:
// "Count/HEALTH/CPU render as the existing ghost dash") without namespaceCols
// itself having to know why.
func namespaceColsUnknown() []palette.Cell {
	return []palette.Cell{
		{Text: "–", Tone: palette.ToneGhost},
		{Text: "–", Tone: palette.ToneGhost},
		{Text: "–", Tone: palette.ToneGhost},
	}
}

// healthCell renders 6a's HEALTH column: one independently toned glyph
// segment per nonzero status class, same glyphs/colors/order as 2a's
// health strip (OK green, Warn yellow, Fail red, Neutral — Info blue, the
// same token AllNS uses). A ghost dash when every class is zero (doesn't
// happen for a nonzero-pod namespace, but keeps the cell well-defined).
func healthCell(h resources.HealthCounts) palette.Cell {
	var segs []palette.Segment
	if h.OK > 0 {
		segs = append(segs, palette.Segment{Text: fmt.Sprintf("%s%d", GlyphRunning, h.OK), Tone: palette.ToneOK})
	}
	if h.Warn > 0 {
		segs = append(segs, palette.Segment{Text: fmt.Sprintf("%s%d", GlyphPending, h.Warn), Tone: palette.ToneWarn})
	}
	if h.Fail > 0 {
		segs = append(segs, palette.Segment{Text: fmt.Sprintf("%s%d", GlyphFailed, h.Fail), Tone: palette.ToneBad})
	}
	if h.Neutral > 0 {
		segs = append(segs, palette.Segment{Text: fmt.Sprintf("%s%d", GlyphCompleted, h.Neutral), Tone: palette.ToneInfo})
	}
	if len(segs) == 0 {
		return palette.Cell{Text: "–", Tone: palette.ToneGhost}
	}
	return palette.Cell{Segments: segs}
}

// namespaceCPUSharesMsg carries fetchNamespaceCPUSharesCmd's background
// result back to the root Update — see namespaceItems' doc comment for why
// this is split out from the initial fast fetch. gen is checked against
// Model.namespaceGen before applying.
type namespaceCPUSharesMsg struct {
	gen    int
	shares map[string]int
}

// fetchNamespaceCPUSharesCmd runs namespaceCPUShares off the Update loop —
// tea.Cmds execute in their own goroutine — so a slow metrics-server round
// trip never blocks typing or navigation while the namespace palette is
// open.
func fetchNamespaceCPUSharesCmd(sess *Session, rows []resources.Row, gen int) tea.Cmd {
	return func() tea.Msg {
		return namespaceCPUSharesMsg{gen: gen, shares: namespaceCPUShares(sess, rows)}
	}
}

// applyNamespaceCPUShares merges a namespaceCPUSharesMsg's result into an
// already-built item list in place, replacing each matching namespace
// row's CPU cell (Cols[2]) with its percentage. Left as the ghost dash
// placeholder for the "all namespaces" row (no per-namespace target), a
// zero-pod row (Cols[0]'s Ghost tone — never gets a CPU value even if
// shares somehow has an entry), or any row missing from shares (nil map,
// or that namespace's metrics-server query errored).
func applyNamespaceCPUShares(items []palette.Item, shares map[string]int) {
	for i := range items {
		target, ok := items[i].Data.(namespaceTarget)
		if !ok || target.namespace == "" || len(items[i].Cols) < 3 {
			continue
		}
		if items[i].Cols[0].Tone == palette.ToneGhost {
			continue
		}
		pct, ok := shares[target.namespace]
		if !ok {
			continue
		}
		items[i].Cols[2] = palette.Cell{Text: fmt.Sprintf("%d%%", pct), Tone: palette.ToneFaint}
	}
}

// namespaceCPUShares fetches live pod CPU usage per namespace (one
// PodMetricsByNamespace call per row, same N-calls shape namespaceItems
// already pays for per-namespace PODS/HEALTH) and returns each namespace's
// share of the summed cluster-wide usage as a whole-number percentage.
// Returns nil — every row's CPU cell then stays the ghost dash placeholder
// — when Session has no metrics seam (nil Metrics: no cluster, or --demo
// before a fake cluster is wired) or every namespace's usage query errored
// (no metrics-server installed); a namespace with zero usage while others
// have some still gets an explicit "0%" entry; every namespace reporting
// zero (encoded as a nil map, not a per-entry error) also degrades to no
// column at all, since "0%" everywhere is a metrics-server outage, not a
// real reading.
func namespaceCPUShares(sess *Session, rows []resources.Row) map[string]int {
	if sess == nil || sess.Metrics == nil {
		return nil
	}
	if sessionScoped(sess) {
		// Same reasoning as namespaceItems' count/HEALTH loop: one
		// PodMetricsByNamespace round trip per namespace is the
		// per-namespace fan-out scoped mode exists to avoid. Every CPU
		// cell stays the ghost dash namespaceColsUnknown already set.
		return nil
	}
	ctx := sess.ClusterContext()
	milli := make(map[string]int64, len(rows))
	var total int64
	anyOK := false
	for _, row := range rows {
		metrics, err := sess.Metrics.PodMetricsByNamespace(ctx, row.Name)
		if err != nil {
			continue
		}
		anyOK = true
		var nsMilli int64
		for _, pm := range metrics {
			nsMilli += pm.CPUMilli
		}
		milli[row.Name] = nsMilli
		total += nsMilli
	}
	if !anyOK || total == 0 {
		return nil
	}
	shares := make(map[string]int, len(milli))
	for name, m := range milli {
		shares[name] = int(m * 100 / total)
	}
	return shares
}

// contextRecentNamespaces returns the active context's remembered
// namespace-recents list (state.PerContext) — namespaces only exist inside
// their own cluster, so unlike RecentKinds/RecentContexts this is never a
// single global list (see state.State's doc comment).
func contextRecentNamespaces(sess *Session) []string {
	if sess == nil {
		return nil
	}
	return sess.State.PerContext[sess.Location.Context].RecentNamespaces
}

// pushRecentNamespace records ns as the active context's most recent
// namespace (state.PerContext[ctx].RecentNamespaces), leaving every other
// PerContext field untouched. Shared by namespaceDispatch and goto.go's
// gotoDispatch (its gotoSwitchNamespace case).
//
// Also seeds the namespace being left (sess.Location.Namespace, still the
// outgoing value here since this always runs before the SwitchNamespaceMsg
// that actually updates it) — otherwise a namespace only ever entered
// RecentNamespaces by being switched *to*, so alt-tabbing away from a
// namespace you only ever arrived at via launch or a context restore had no
// "previous" to resolve to and silently no-op'd on the very next bare
// "n <enter>".
func pushRecentNamespace(sess *Session, ns string) {
	if sess == nil || ns == "" {
		return
	}
	ctx := sess.Location.Context
	pc := sess.State.PerContext[ctx]
	pc.RecentNamespaces = state.PushRecent(pc.RecentNamespaces, sess.Location.Namespace)
	pc.RecentNamespaces = state.PushRecent(pc.RecentNamespaces, ns)
	sess.State.PerContext[ctx] = pc
}

// namespaceRecentLabels is the 6a RECENT row's entries — namespace names are
// already their own display label, unlike goto's kind recents. Current and
// the immediately-previous namespace are excluded (numberedRecents): they
// already have their own on-row tag ("current"/"previous"), so repeating
// them here would be redundant — the RECENT row only needs to surface what
// isn't already visible elsewhere on screen.
func namespaceRecentLabels(sess *Session) []string {
	if sess == nil {
		return nil
	}
	return numberedRecents(contextRecentNamespaces(sess), sess.Location.Namespace)
}

// namespaceItemIndex finds target's row in items by its namespaceTarget
// payload — shared by namespaceBrowseSelection (alt-tab) and
// refreshNamespacePalette's digit-recent lookup.
func namespaceItemIndex(items []palette.Item, target string) (int, bool) {
	for i, it := range items {
		if t, ok := it.Data.(namespaceTarget); ok && t.namespace == target {
			return i, true
		}
	}
	return 0, false
}

// namespaceRecentFooter is 6a's digit-select confirmation, shown once a
// typed digit resolves to a RECENT-row namespace — mirrors 12b's
// gotoAliasMatchFooter, naming the exact switch target before Enter commits
// to it.
func namespaceRecentFooter(target string) []palette.FooterSpan {
	return []palette.FooterSpan{
		{Text: "↵", Tone: palette.FooterKey},
		{Text: " switches to ", Tone: palette.FooterDim},
		{Text: target, Tone: palette.FooterEm},
	}
}

// namespaceDispatch turns a selected namespace item into the SwitchNamespaceMsg
// cmd, recording the target in recents. Mirrors gotoDispatch's
// gotoSwitchNamespace case.
func namespaceDispatch(sess *Session, item palette.Item) tea.Cmd {
	target, ok := item.Data.(namespaceTarget)
	if !ok || sess == nil {
		return nil
	}
	pushRecentNamespace(sess, target.namespace)
	ns := target.namespace
	return func() tea.Msg { return SwitchNamespaceMsg{Namespace: ns} }
}

// namespaceBrowseSelection picks the initial Sel for a freshly opened 6a
// list: the most recently visited *other* namespace — mostRecentOther,
// since recentNamespaces[0] is always the current one — so a bare "n ↵"
// alt-tabs back to whatever namespace you were on before (docs/design
// README.md §6a). Falls back to the first (always selectable) item when
// there's no other namespace to toggle to.
func namespaceBrowseSelection(items []palette.Item, recentNamespaces []string, current string) int {
	if target, ok := mostRecentOther(recentNamespaces, current); ok {
		if i, ok := namespaceItemIndex(items, target); ok {
			return i
		}
	}
	return 0
}

// namespacePaletteSelection resolves the 6a palette's Sel on open and on
// every re-filter: the alt-tab target (namespaceBrowseSelection) while the
// query is empty, otherwise the top fuzzy match — mirrors goto's
// Browse-vs-fuzzy split without needing a separate Browse flag, since
// namespace never has a distinct ranked-chips state.
func namespacePaletteSelection(sess *Session, items []palette.Item, query string) int {
	if query != "" {
		return 0
	}
	return namespaceBrowseSelection(items, contextRecentNamespaces(sess), sess.Location.Namespace)
}
