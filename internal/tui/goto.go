package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui/components/palette"
)

// This file wires real data into the 'g' jump palette (mvp-plan.md Phase
// 2): the 3a taxonomy grid, the 2b fuzzy corpus, and Enter's navigation
// dispatch. It's the only place the root shell reaches into Session.Lister/
// Registry/Groups to build palette.Items — the palette package itself stays
// pure UI (see components/palette/palette.go).

// gotoAction identifies what Enter should do with a selected jump-palette
// item. Carried opaquely in palette.Item.Data as a gotoTarget — the palette
// package never interprets it.
type gotoAction int

const (
	gotoSwitchKind gotoAction = iota
	gotoOpenResource
	gotoSwitchNamespace
	// gotoOpenUpdatePanel is 28b's ":update" entry point (docs/design
	// README.md §28b: "also reachable as :update") — ":" already opens this
	// same goto palette (aliased with "g"), so rather than a separate
	// colon-command parser, the fuzzy corpus gains one synthetic "update"
	// item (gotoUpdateItem) whose Enter opens the panel instead of
	// navigating.
	gotoOpenUpdatePanel
)

type gotoTarget struct {
	action    gotoAction
	kind      kube.ResourceKind
	namespace string
	name      string
}

// maxGotoResults caps the resource-name *results* shown for a query
// (mvp-plan.md Phase 2: "cap results at ~50") — applied after filtering and
// ranking, never to the corpus being searched (see gotoResourceCorpus).
const maxGotoResults = 50

func gotoNamespace(sess *Session) string {
	if sess == nil {
		return ""
	}
	return sess.Location.Namespace
}

// gotoHint is the palette's right-hand input-row hint — "jump anywhere ·
// <namespace>" in both the 12a ranked and 12b/2b fuzzy states (the 12a/12b
// mockups keep one hint across them).
func gotoHint(sess *Session) string {
	ns := gotoNamespace(sess)
	if ns == "" {
		ns = "all namespaces"
	}
	return "jump anywhere · " + ns
}

// countUnknown is gotoCount's "no number to show" answer, rendered as a dim
// en dash. Distinct from 0, which is a real count and a real thing to say.
const countUnknown = -1

// gotoCount reports a per-kind count for the palette, scoped like
// browse.countNamespace (cluster-scoped kinds ignore the active namespace).
//
// It reads only what has already been fetched. It cannot ask the cluster,
// for two reasons: it runs inside the Update loop on every keystroke, so a
// round trip here would freeze typing; and it is called for every kind at
// once, so reading counts from informer caches would start a watch per kind
// the instant the palette opened — which is exactly the launch stampede
// lazy informers removed, just relocated to the g key.
//
// fetchGotoCountsCmd fills the cache in the background and the palette
// refreshes when it lands, the same shape the namespace palette's CPU
// column already uses.
func gotoCount(sess *Session, desc resources.Descriptor, namespace string) int {
	if sess == nil {
		return countUnknown
	}
	ns := namespace
	if desc.ClusterScoped {
		ns = ""
	}
	return sess.CachedCount(desc.Kind, ns)
}

// gotoCountText renders a count for the palette's right-hand column.
func gotoCountText(count int) string {
	if count == countUnknown {
		return "–"
	}
	return strconv.Itoa(count)
}

// gotoAliasEntry pairs an alias letter with its kind — the fixed daily-kind
// list from docs/design README.md §12a, in rank order. Every alias IS the
// kind's first letter (the palette captures all input while open, so
// keybar keys like n/c can't collide with it) — the highlighted letter is
// its own documentation, so this list is never surfaced as help text
// anywhere else. Aliases exist only on these built-ins, never CRDs or the
// long tail.
type gotoAliasEntry struct {
	key  string
	kind kube.ResourceKind
}

var gotoAliases = []gotoAliasEntry{
	{"p", kube.KindPod},
	{"d", kube.KindDeployment},
	{"s", kube.KindService},
	{"i", kube.KindIngress},
	{"n", kube.KindNode},
	{"c", kube.KindConfigMap},
	{"e", kube.KindEvent},
}

// gotoAliasFor returns kind's alias letter, or "" if it has none.
func gotoAliasFor(kind kube.ResourceKind) string {
	for _, a := range gotoAliases {
		if a.kind == kind {
			return a.key
		}
	}
	return ""
}

// gotoAliasMatch resolves query to its aliased kind (12b) — only for an
// exact single-rune, case-insensitive query; a second character falls back
// to plain fuzzy matching ("Any second character makes it a plain fuzzy
// query, so 'no' still finds Nodes and NetworkPolicies" — docs/design
// README.md §12b).
func gotoAliasMatch(query string) (kube.ResourceKind, bool) {
	if len([]rune(query)) != 1 {
		return "", false
	}
	q := strings.ToLower(query)
	for _, a := range gotoAliases {
		if a.key == q {
			return a.kind, true
		}
	}
	return "", false
}

// gotoRankedItem builds one kind row shared by the 12a ranked list and
// 12b's pinned alias match — alias is "" for kinds without one, which
// render Muted (one step darker than the aliased rows, per the 12a mockup).
// A non-empty alias highlights the label's first letter (Matches = index 0,
// the alias invariant guarantees the letter IS the first rune) instead of a
// chip glyph — "the highlighted letter IS the documentation" (§12a).
// gotoTypeLabel is the palette's "kind · <group>" detail text (12a/12b):
// APIGroup replaces the built-in taxonomy Group for a discovered CRD kind
// (14c: "the type label's group slot carries the API group instead of a
// built-in category"), with a " · cluster" suffix for a cluster-scoped
// discovered kind — scoped to Custom kinds only so built-ins like Nodes
// keep their existing "kind · Cluster" text unchanged.
func gotoTypeLabel(desc resources.Descriptor) string {
	group := string(desc.Group)
	if desc.APIGroup != "" {
		group = desc.APIGroup
	}
	label := "kind · " + group
	if desc.Custom && desc.ClusterScoped {
		label += " · cluster"
	}
	return label
}

func gotoRankedItem(sess *Session, desc resources.Descriptor, ns, alias string) palette.Item {
	count := gotoCount(sess, desc, ns)
	item := palette.Item{
		Label:  desc.Display,
		Detail: gotoTypeLabel(desc),
		Right:  gotoCountText(count),
		Dim:    count <= 0,
		Muted:  alias == "",
		Data:   gotoTarget{action: gotoSwitchKind, kind: desc.Kind},
	}
	if alias != "" {
		item.Matches = []int{0}
	}
	return item
}

// gotoBrowseItems builds the 12a empty-query ranked list: the daily kinds
// first, each with its first letter highlighted as the alias indicator,
// then every unaliased kind in group order. The shared palette viewport
// clips this complete list to the terminal and scrolls with selection.
func gotoBrowseItems(sess *Session) []palette.Item {
	if sess == nil {
		return nil
	}
	ns := gotoNamespace(sess)
	seen := make(map[kube.ResourceKind]bool, len(gotoAliases))
	items := make([]palette.Item, 0, len(gotoAliases))
	for _, a := range gotoAliases {
		desc, ok := sess.Registry.Descriptor(a.kind)
		if !ok {
			continue
		}
		seen[a.kind] = true
		items = append(items, gotoRankedItem(sess, desc, ns, a.key))
	}

	var rest []palette.Item
	for _, group := range sess.Groups {
		for _, kind := range group.Kinds {
			if seen[kind] {
				continue
			}
			desc, ok := sess.Registry.Descriptor(kind)
			if !ok {
				continue
			}
			rest = append(rest, gotoRankedItem(sess, desc, ns, ""))
		}
	}

	return append(items, rest...)
}

// gotoBrowseSelection picks the initial Sel for a freshly opened 12a list:
// the most recently visited *other* kind — mostRecentOther, since
// recentKinds[0] is always current (only a completed jump ever pushes to
// it) — so a bare "g ↵" alt-tabs back to whatever kind you were on before
// (docs/design README.md §2b's "RECENT" semantics, applied to the
// empty-query state). Falls back to the first (always selectable) item when
// there's no other kind to toggle to.
func gotoBrowseSelection(items []palette.Item, recentKinds []string, current kube.ResourceKind) int {
	if target, ok := mostRecentOther(recentKinds, string(current)); ok {
		if i := slices.IndexFunc(items, func(it palette.Item) bool {
			t, ok := it.Data.(gotoTarget)
			return ok && string(t.kind) == target
		}); i >= 0 {
			return i
		}
	}
	return 0
}

// gotoAliasFooter is 12a's static instructive footer, shown whenever the
// empty-query ranked list is on screen — the "alias" word renders Accent,
// the explanation dim (per the mockup).
func gotoAliasFooter() []palette.FooterSpan {
	return []palette.FooterSpan{
		{Text: "alias", Tone: palette.FooterKey},
		{Text: " — colored first letter · typing it pins that kind to rank 1 · ↵ jumps"},
	}
}

// gotoAliasMatchFooter is 12b's destination confirmation, shown only while
// query is a single alias character with a resolvable kind — names the
// exact jump target before Enter commits to it (↵ and the namespace in
// Accent, the kind bright, per the mockup).
func gotoAliasMatchFooter(sess *Session, query string) []palette.FooterSpan {
	kind, ok := gotoAliasMatch(query)
	if !ok || sess == nil {
		return nil
	}
	desc, ok := sess.Registry.Descriptor(kind)
	if !ok {
		return nil
	}
	ns := gotoNamespace(sess)
	if ns == "" {
		ns = "all namespaces"
	}
	return []palette.FooterSpan{
		{Text: "↵", Tone: palette.FooterKey},
		{Text: " jumps to "},
		{Text: desc.Display, Tone: palette.FooterEm},
		{Text: " in "},
		{Text: ns, Tone: palette.FooterKey},
		{Text: " — namespace and filter carry over"},
	}
}

// gotoFuzzyItems builds the 2b fuzzy corpus — kinds, the current kind's
// resource names first then other kinds' cached rows, and namespaces — and
// filters it against query. When query is exactly one alias character
// (12b), the aliased kind is pinned to rank 1 with its first letter
// highlighted and excluded from the corpus below it, so it isn't listed
// twice; every other
// fuzzy match for that character (DaemonSets, pod names, …) still ranks
// normally beneath it. The resource names come from resourceCorpus, the
// per-palette-session snapshot, so a keystroke reads no object cache; the
// whole corpus is filtered and only then are resource results capped.
func gotoFuzzyItems(sess *Session, query string, resourceCorpus *gotoResourceCorpus) []palette.Item {
	if sess == nil {
		return nil
	}
	var corpus []palette.Item
	corpus = append(corpus, gotoKindItems(sess)...)
	if resourceCorpus != nil {
		corpus = append(corpus, resourceCorpus.items...)
	}
	corpus = append(corpus, gotoNamespaceItems(sess)...)
	corpus = append(corpus, gotoWhoCanItem())
	corpus = append(corpus, gotoOverviewItem())
	if item, ok := gotoFluxTreeItem(sess); ok {
		corpus = append(corpus, item)
	}
	corpus = append(corpus, gotoUpdateItem())

	var pinned []palette.Item
	if kind, ok := gotoAliasMatch(query); ok {
		if desc, ok := sess.Registry.Descriptor(kind); ok {
			match := gotoRankedItem(sess, desc, gotoNamespace(sess), gotoAliasFor(kind))
			match.AliasMatch = true
			match.Muted = false
			// The pinned row shows "alias match" where the type label would
			// be; gotoRankedItem already set Matches=[0] to highlight the
			// alias letter in the kind name (12b).
			match.Detail = ""
			pinned = append(pinned, match)
			corpus = gotoExcludeKind(corpus, kind)
		}
	}

	items := append(pinned, palette.Filter(corpus, query)...)
	return gotoCapResources(items, resourceCorpus)
}

// gotoExcludeKind drops kind's own kind-switch row from items (used to
// dedupe the 12b pinned alias match from the fuzzy results below it) —
// resource names and every other kind are untouched.
func gotoExcludeKind(items []palette.Item, kind kube.ResourceKind) []palette.Item {
	out := make([]palette.Item, 0, len(items))
	for _, it := range items {
		if t, ok := it.Data.(gotoTarget); ok && t.action == gotoSwitchKind && t.kind == kind {
			continue
		}
		out = append(out, it)
	}
	return out
}

// gotoWhoCanItem is 22a's synthetic "who-can" goto result (`g "who"`,
// docs/design README.md §22a: "also reachable as a registry kind via g").
// KindWhoCan has no resources.Descriptor (there's nothing to list — 22a is
// "a query, not a browser"), so it can't come from gotoKindItems' usual
// Registry walk; it's appended to the fuzzy corpus directly, always
// present, uncounted, and never part of 12a's ranked daily-kinds list (it's
// long-tail like Helm/CRDs, only reachable by typing).
func gotoWhoCanItem() palette.Item {
	return palette.Item{
		Label:  "who-can",
		Detail: "query · RBAC",
		Data:   gotoTarget{action: gotoSwitchKind, kind: kube.KindWhoCan},
	}
}

// gotoOverviewItem is 19a's synthetic "cluster overview" goto result
// (`g "ov"`, docs/design README.md §19a). Like gotoWhoCanItem, KindOverview
// has no resources.Descriptor to list — 19a is "a routing layer, not a
// dashboard" — so it's appended to the fuzzy corpus directly rather than
// coming from gotoKindItems' Registry walk: always present, uncounted, and
// never part of 12a's ranked daily-kinds list.
func gotoOverviewItem() palette.Item {
	return palette.Item{
		Label:  "overview",
		Detail: "cluster · routing",
		Data:   gotoTarget{action: gotoSwitchKind, kind: kube.KindOverview},
	}
}

// gotoFluxTreeItem is §30b's synthetic "flux" goto result — the source →
// reconciler tree. Like gotoWhoCanItem/gotoOverviewItem it has nothing of
// its own to list (§30b is a computed join over kinds already in the
// registry), so it joins the fuzzy corpus directly rather than through
// gotoKindItems' Registry walk.
//
// Unlike those two it is *conditional*: a cluster with no Flux CRDs gets no
// entry at all, per §30a's "zero chrome on a non-Flux cluster". The test is
// the descriptors' own Flux flag — the same API-group recognition every
// other Flux surface uses, never a kind-name match.
func gotoFluxTreeItem(sess *Session) (palette.Item, bool) {
	if !hasFluxKinds(sess) {
		return palette.Item{}, false
	}
	return palette.Item{
		Label:  "flux",
		Detail: "tree · sources → reconcilers",
		Data:   gotoTarget{action: gotoSwitchKind, kind: kube.KindFluxTree},
	}, true
}

// hasFluxKinds reports whether this cluster serves any Flux kind at all.
func hasFluxKinds(sess *Session) bool {
	if sess == nil {
		return false
	}
	for _, kind := range sess.Registry.Kinds() {
		if desc, ok := sess.Registry.Descriptor(kind); ok && desc.Flux {
			return true
		}
	}
	return false
}

// gotoUpdateItem is 28b's synthetic ":update" goto result — like
// gotoWhoCanItem/gotoOverviewItem, it has nothing to list (28b is a static
// panel, not a resource kind), so it's appended to the fuzzy corpus
// directly rather than coming from gotoKindItems' Registry walk: always
// present, uncounted, never part of 12a's ranked daily-kinds list.
func gotoUpdateItem() palette.Item {
	return palette.Item{
		Label:  "update",
		Detail: "what's new · upgrade",
		Data:   gotoTarget{action: gotoOpenUpdatePanel},
	}
}

func gotoKindItems(sess *Session) []palette.Item {
	ns := gotoNamespace(sess)
	items := make([]palette.Item, 0, 16)
	for _, group := range sess.Groups {
		for _, kind := range group.Kinds {
			desc, ok := sess.Registry.Descriptor(kind)
			if !ok {
				continue
			}
			count := gotoCount(sess, desc, ns)
			// No zero-count dimming here, unlike the 12a ranked list — the
			// 12b mockup renders fuzzy-matched kinds at full strength even
			// at count 0 (relevance, not liveness, ranks the fuzzy list).
			items = append(items, palette.Item{
				Label:  desc.Display,
				Detail: gotoTypeLabel(desc),
				Right:  gotoCountText(count),
				Data:   gotoTarget{action: gotoSwitchKind, kind: kind},
			})
		}
	}
	return items
}

// gotoResourceCorpus is the fuzzy corpus's resource-name half: one item per
// cached object of every already-started kind, names only. It is built once
// per palette session — lazily, on the first fuzzy keystroke, since merely
// opening the palette must read no cache at all — and again only after a
// watch event or a registry change marks it stale, so a keystroke pays for a
// fuzzy match over the names and nothing else.
//
// It is deliberately uncapped. The cap (maxGotoResults) is a *display* cap,
// applied after filtering in gotoCapResources; capping the corpus instead
// made only the first 50 objects in cache order searchable, and the palette
// answered "no matches" for everything else (review 13 H4).
type gotoResourceCorpus struct {
	items []palette.Item
	// objs carries each item's object and descriptor, so status is projected
	// only for the few results that survive the filter and the cap.
	objs map[gotoTarget]gotoCorpusObject
}

type gotoCorpusObject struct {
	desc resources.Descriptor
	obj  runtime.Object
}

// buildGotoResourceCorpus snapshots resource names for the fuzzy corpus: the
// current kind's objects first, then every other started kind's, each kind
// in the same (namespace, name) order the list screens use — the order ties
// in fuzzy score fall back to.
func buildGotoResourceCorpus(sess *Session) *gotoResourceCorpus {
	corpus := &gotoResourceCorpus{objs: map[gotoTarget]gotoCorpusObject{}}
	if sess == nil || sess.Lister == nil {
		return corpus
	}
	for _, kind := range gotoResourceKindOrder(sess) {
		desc, ok := sess.Registry.Descriptor(kind)
		if !ok {
			continue
		}
		ns := gotoNamespace(sess)
		if desc.ClusterScoped {
			ns = ""
		}
		// Only kinds whose caches are already running and populated.
		// Listing the rest would start an informer per kind on a keystroke
		// — the launch stampede, relocated into the search box. The corpus
		// is therefore "things you've looked at this session", and the kind
		// rows above still get you to anything else in one more keypress.
		// KindCached, not kindSynced: the latter reports "settled" for a
		// kind nothing has started yet, which let the first keystroke start
		// the full-CRD informer.
		if !KindCached(sess.Lister, kind, ns) {
			continue
		}
		objs, err := sess.Lister.ListRaw(sess.ClusterContext(), kind, ns)
		if err != nil {
			continue
		}
		entries := make([]gotoCorpusEntry, 0, len(objs))
		for _, obj := range objs {
			namespace, name := gotoObjectName(desc, obj)
			entries = append(entries, gotoCorpusEntry{namespace: namespace, name: name, sortName: strings.ToLower(name), obj: obj})
		}
		slices.SortStableFunc(entries, func(a, b gotoCorpusEntry) int {
			if c := strings.Compare(a.namespace, b.namespace); c != 0 {
				return c
			}
			return strings.Compare(a.sortName, b.sortName)
		})
		kindLabel := strings.ToLower(string(kind))
		for _, e := range entries {
			scope := e.namespace
			if desc.ClusterScoped {
				scope = "cluster"
			}
			target := gotoTarget{action: gotoOpenResource, kind: kind, namespace: e.namespace, name: e.name}
			corpus.items = append(corpus.items, palette.Item{
				Label:  e.name,
				Detail: kindLabel + " · " + scope,
				Data:   target,
			})
			corpus.objs[target] = gotoCorpusObject{desc: desc, obj: e.obj}
		}
	}
	return corpus
}

type gotoCorpusEntry struct {
	namespace, name, sortName string
	obj                       runtime.Object
}

// gotoObjectName reads an object's namespace/name straight from its
// metadata — no projection, which for 5k pods is the difference between a
// corpus build that costs a name read per object and one that derives every
// column of every row. The one kind without ObjectMeta (Forwards, whose row
// name is synthesized from the session) falls back to its projection.
func gotoObjectName(desc resources.Descriptor, obj runtime.Object) (namespace, name string) {
	if acc, ok := obj.(metav1.Object); ok {
		return acc.GetNamespace(), acc.GetName()
	}
	row := desc.Project(obj)
	return row.Namespace, row.Name
}

// gotoCapResources keeps at most maxGotoResults resource-name results from
// an already filtered and ranked item list — kinds, namespaces and the
// synthetic entries are never dropped — and projects the status glyph for
// just the survivors.
func gotoCapResources(items []palette.Item, corpus *gotoResourceCorpus) []palette.Item {
	out := items[:0]
	resourcesKept := 0
	for _, it := range items {
		target, ok := it.Data.(gotoTarget)
		if !ok || target.action != gotoOpenResource {
			out = append(out, it)
			continue
		}
		if resourcesKept >= maxGotoResults {
			continue
		}
		resourcesKept++
		if corpus != nil {
			if src, ok := corpus.objs[target]; ok && src.desc.Project != nil {
				it.Right, it.RightTone = gotoRowStatus(src.desc.Project(src.obj))
			}
		}
		out = append(out, it)
	}
	return out
}

// gotoRowStatus maps a projected row's health to the right-aligned status
// glyph 12b's mockup puts on resource results (the pod row's green ●).
// Neutral/status-less kinds (configmaps, …) get no glyph — never fake
// health, same rule as 14a's fallback.
func gotoRowStatus(row resources.Row) (string, palette.Tone) {
	class := row.GlyphClass
	if class == "" {
		class = row.Status
	}
	glyph := row.Glyph
	switch class {
	case resources.StatusOK:
		if glyph == "" {
			glyph = GlyphRunning
		}
		return glyph, palette.ToneOK
	case resources.StatusWarn:
		if glyph == "" {
			glyph = GlyphPending
		}
		return glyph, palette.ToneWarn
	case resources.StatusFail:
		if glyph == "" {
			glyph = GlyphFailed
		}
		return glyph, palette.ToneBad
	}
	return "", palette.ToneDefault
}

// gotoResourceKindOrder puts the current kind first (its rows are what the
// user is most likely jumping between), then every other registered kind in
// group order.
func gotoResourceKindOrder(sess *Session) []kube.ResourceKind {
	kinds := make([]kube.ResourceKind, 0, 16)
	if sess.Location.Kind != "" {
		kinds = append(kinds, sess.Location.Kind)
	}
	for _, group := range sess.Groups {
		for _, kind := range group.Kinds {
			if kind == sess.Location.Kind {
				continue
			}
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// gotoNamespaceItems lists every namespace (kube.KindNamespace is a single
// cluster-scoped cache read) for the fuzzy corpus.
func gotoNamespaceItems(sess *Session) []palette.Item {
	if sess.Lister == nil {
		return nil
	}
	desc, ok := sess.Registry.Descriptor(kube.KindNamespace)
	if !ok {
		return nil
	}
	rows, err := resources.List(sess.ClusterContext(), sess.Lister, desc, "")
	if err != nil {
		return nil
	}
	items := make([]palette.Item, 0, len(rows))
	for _, row := range rows {
		items = append(items, palette.Item{
			Label:  row.Name,
			Detail: "namespace · " + sess.Location.Context,
			Right:  "↗", // a namespace result is a jump, not a count (12b)
			Data:   gotoTarget{action: gotoSwitchNamespace, namespace: row.Name},
		})
	}
	return items
}

// gotoRecentKindLabels maps recent kind strings (Session.State.RecentKinds)
// to their display names for the 2b RECENT row.
func gotoRecentKindLabels(sess *Session) []string {
	if sess == nil || len(sess.State.RecentKinds) == 0 {
		return nil
	}
	labels := make([]string, 0, len(sess.State.RecentKinds))
	for _, k := range sess.State.RecentKinds {
		if desc, ok := sess.Registry.Descriptor(kube.ResourceKind(k)); ok {
			labels = append(labels, desc.Display)
		}
	}
	return labels
}

// pushRecentKind records kind as the most recently visited kind
// (Session.State.RecentKinds), also seeding the kind being left
// (sess.Location.Kind, still the outgoing value here since this always
// runs before the GotoKindMsg/GotoResourceMsg that actually updates it) —
// see pushRecentNamespace's identical note in namespace.go. Without it, a
// kind only ever entered RecentKinds by being switched *to*, so the first
// alt-tab away from whatever kind browse opened on had no "previous" to
// resolve to.
func pushRecentKind(sess *Session, kind kube.ResourceKind) {
	sess.State.RecentKinds = state.PushRecent(sess.State.RecentKinds, string(sess.Location.Kind))
	sess.State.RecentKinds = state.PushRecent(sess.State.RecentKinds, string(kind))
}

// gotoDispatch turns a selected item's gotoTarget into the tea.Cmd that
// performs the actual navigation: the produced message flows through the
// same path as BackMsg/ConnStateMsg (root Update updates Session.Location,
// then forwards to the active task's Update — mvp-plan.md §0.9), and
// records the pick in Session.State's recents. Returns nil for an item
// with no (or unrecognized) Data payload.
func gotoDispatch(sess *Session, item palette.Item) tea.Cmd {
	target, ok := item.Data.(gotoTarget)
	if !ok || sess == nil {
		return nil
	}
	switch target.action {
	case gotoSwitchKind:
		return GotoKind(sess, target.kind, "")
	case gotoOpenResource:
		return GotoResource(sess, target.kind, target.namespace, target.name)
	case gotoSwitchNamespace:
		pushRecentNamespace(sess, target.namespace)
		ns := target.namespace
		return func() tea.Msg { return SwitchNamespaceMsg{Namespace: ns} }
	case gotoOpenUpdatePanel:
		return func() tea.Msg { return OpenUpdatePanelMsg{} }
	}
	return nil
}

// EventObjectKind resolves an Event's involved object — its apiVersion and
// bare Kind — to the registry kind that lists it. The bare Kind alone is
// ambiguous wherever two groups serve it: a Flux HelmRelease's events name
// "HelmRelease", which is §18a's synthetic Helm kind, where the Flux object
// isn't listed. An empty apiVersion (an Event that didn't record one) keeps
// the bare Kind, which is all there is to go on. sess may be nil.
func EventObjectKind(sess *Session, apiVersion string, apiKind kube.ResourceKind) kube.ResourceKind {
	if apiVersion == "" || apiKind == "" {
		return apiKind
	}
	var reg resources.Registry
	if sess != nil {
		reg = sess.Registry
	}
	return reg.KindFor(kube.APIGroupOf(apiVersion), string(apiKind))
}

// GotoResource fires the same navigation gotoDispatch's gotoOpenResource
// case does — pre-filled, without ever opening the palette — so 'g' and
// every screen's own "jump to related object" action (poddetail's RELATED
// sidebar, events'/timeline's ↵, routetable's jumps, overview's
// trouble/changes list) share one goto machinery: the same recents
// bookkeeping here, and (via routeGoto in model.go's Update, once the
// returned cmd's message actually arrives) the same push-a-fresh-browse,
// one-esc-back semantics. sess may be nil (a screen with no session wired,
// e.g. in tests) — the message still fires, just without the recents push.
func GotoResource(sess *Session, kind kube.ResourceKind, namespace, name string) tea.Cmd {
	if sess != nil {
		pushRecentKind(sess, kind)
	}
	return func() tea.Msg { return GotoResourceMsg{Kind: kind, Namespace: namespace, Name: name} }
}

// GotoKind mirrors GotoResource for a bare kind switch (routetable's
// Gateway → HTTPRoute listener-filter jump, gotoDispatch's gotoSwitchKind).
func GotoKind(sess *Session, kind kube.ResourceKind, filter string) tea.Cmd {
	if sess != nil {
		pushRecentKind(sess, kind)
	}
	return func() tea.Msg { return GotoKindMsg{Kind: kind, Filter: filter} }
}
