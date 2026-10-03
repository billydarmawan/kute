package fluxtree

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// load reads every Flux kind the cluster serves and joins the reconcilers to
// their sources.
//
// The read is cluster-wide (namespace "") because the chain is: a
// Kustomization in one namespace routinely reconciles from a GitRepository
// in flux-system, and a namespace-scoped read would show the reconciler as
// an orphan. It is also *narrow* — only kinds the registry marks Flux, which
// is a handful of discovered CRDs, never a walk of the registry. Starting
// those informers is this screen's whole purpose; starting everything else's
// would be the failure CLAUDE.md's lazy-informer rule exists to prevent.
//
// The reply carries m.loadSeq, which the caller bumps (startLoad) — Init
// runs on a value copy and uses New's seq directly.
func (m Model) load() tea.Cmd {
	seq := m.loadSeq
	lister := m.lister
	registry := m.registry()
	timeout := m.timeout

	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		sourceKinds, reconcilerKinds, chartKind := fluxKinds(registry)
		read := append(append([]kube.ResourceKind{}, sourceKinds...), reconcilerKinds...)

		// One kind that can't be read — a least-privilege tenant Forbidden
		// from Buckets or the image-automation kinds is the common case —
		// degrades the tree rather than failing it: that kind drops out of
		// the join and the strip names it. Only when nothing at all can be
		// read is the screen an error.
		var kindErrs []kindError
		readKind := func(kind kube.ResourceKind) []runtime.Object {
			objs, err := lister.ListRaw(ctx, kind, "")
			if err == nil {
				err = tui.KindsError(lister, "", kind)
			}
			if err != nil {
				kindErrs = append(kindErrs, kindError{kind: kind, err: err})
			}
			return objs
		}

		sources := map[string]*group{}
		var order []string
		for _, kind := range sourceKinds {
			desc, ok := registry.Descriptor(kind)
			if !ok {
				continue
			}
			objs := readKind(kind)
			for _, obj := range objs {
				row := sourceRowOf(desc, kind, obj)
				key := joinKey(kind, row.namespace, row.name)
				sources[key] = &group{head: row}
				order = append(order, key)
			}
		}

		// A reconciler whose sourceRef resolves to nothing still has to
		// appear — that is a real broken chain, and the failure this screen
		// is for. It gets a synthetic head naming the reference itself.
		var rows []treeRow
		for _, kind := range reconcilerKinds {
			desc, ok := registry.Descriptor(kind)
			if !ok {
				continue
			}
			for _, obj := range readKind(kind) {
				rows = append(rows, reconcilerRowOf(desc, kind, obj))
			}
		}
		if len(read) > 0 && len(kindErrs) == len(read) {
			return loadedMsg{seq: seq, err: kindErrs[0].err, kinds: read, kindErrs: kindErrs}
		}

		// A HelmRelease's spec.chartRef may name a HelmChart, which is not a
		// source this screen draws — it is fetched *from* one. Follow it one
		// hop to the repository behind it. HelmChart is read only when some
		// release actually points at one, so the common tree never starts
		// that informer.
		if chartKind != "" && slices.ContainsFunc(rows, func(r treeRow) bool { return r.sourceKind.APIKind() == "HelmChart" }) {
			read = append(read, chartKind)
			objs := readKind(chartKind)
			charts := map[string]*unstructured.Unstructured{}
			for _, obj := range objs {
				if u, ok := obj.(*unstructured.Unstructured); ok {
					charts[u.GetNamespace()+"/"+u.GetName()] = u
				}
			}
			for i := range rows {
				if rows[i].sourceKind.APIKind() != "HelmChart" {
					continue
				}
				if hc, ok := charts[rows[i].sourceNamespace+"/"+rows[i].sourceName]; ok {
					if k, n, ns := sourceRefOf(hc, hc.GetNamespace()); n != "" {
						rows[i].sourceKind, rows[i].sourceName, rows[i].sourceNamespace = k, n, ns
					}
				}
			}
		}

		for _, row := range rows {
			key := joinKey(row.sourceKind, row.sourceNamespace, row.sourceName)
			g, ok := sources[key]
			if !ok {
				g = &group{head: missingSourceRow(row, kindErrOf(kindErrs, row.sourceKind))}
				sources[key] = g
				order = append(order, key)
			}
			// Drift is a boolean, never a count: "N commits ahead" needs
			// git log, which kute never has (docs/flux-plan.md G1). And
			// it is only computed where the two revisions are the same
			// kind of thing — see kube.FluxTracksSourceRevision.
			if kube.FluxTracksSourceRevision(row.kind.APIKind()) &&
				g.head.revisionKey != "" && row.revisionKey != "" && g.head.revisionKey != row.revisionKey {
				row.revision += " · source ahead"
			}
			g.children = append(g.children, row)
		}

		groups := make([]group, 0, len(order))
		for _, key := range order {
			g := sources[key]
			sortRows(g.children)
			groups = append(groups, *g)
		}
		sortGroups(groups)
		return loadedMsg{seq: seq, groups: groups, kinds: read, kindErrs: kindErrs}
	}
}

// kindError is one Flux kind this load couldn't read, and why.
type kindError struct {
	kind kube.ResourceKind
	err  error
}

func kindErrOf(errs []kindError, kind kube.ResourceKind) error {
	for _, e := range errs {
		if e.kind == kind {
			return e.err
		}
	}
	return nil
}

// registry is the kind catalog to read Flux descriptors from.
func (m Model) registry() resources.Registry {
	if m.session == nil {
		return resources.DefaultRegistry()
	}
	return m.session.Registry
}

// fluxKinds splits the registry's Flux descriptors into the two halves of
// the chain, by API group — never by kind name, per §30a's recognition rule.
// Notification-controller kinds (Alert, Provider, Receiver) are in neither:
// they are not part of the fetch→apply chain this screen draws.
//
// chartKind is HelmChart's registry kind, when the cluster serves it: not a
// source of its own, but the hop between a chartRef'd HelmRelease and the
// repository it really comes from.
func fluxKinds(registry resources.Registry) (sources, reconcilers []kube.ResourceKind, chartKind kube.ResourceKind) {
	for _, kind := range registry.Kinds() {
		desc, ok := registry.Descriptor(kind)
		if !ok || !desc.Flux {
			continue
		}
		switch desc.APIGroup {
		case kube.FluxGroupSource:
			// HelmChart is an artifact of a HelmRelease, not a source an
			// operator points anything at — it would double every Helm
			// chain. The HelmRepository behind it is the real source.
			if desc.Kind.APIKind() == "HelmChart" {
				chartKind = kind
				continue
			}
			sources = append(sources, kind)
		case kube.FluxGroupKustomize, kube.FluxGroupHelm:
			reconcilers = append(reconcilers, kind)
		}
	}
	slices.Sort(sources)
	slices.Sort(reconcilers)
	return sources, reconcilers, chartKind
}

// sourceRowOf projects one source object. Everything but the revision cell
// is the descriptor's own §30a projection, unchanged.
func sourceRowOf(desc resources.Descriptor, kind kube.ResourceKind, obj runtime.Object) treeRow {
	row := baseRow(desc, kind, obj)
	row.isSource = true
	u, _ := obj.(*unstructured.Unstructured)
	row.revisionKey = artifactRevision(u)
	row.revision = sourceRevisionCell(desc.Kind.APIKind(), row.revisionKey, artifactUpdated(u))
	return row
}

// reconcilerRowOf projects one Kustomization/HelmRelease and resolves the
// sourceRef that decides which group it belongs to.
func reconcilerRowOf(desc resources.Descriptor, kind kube.ResourceKind, obj runtime.Object) treeRow {
	row := baseRow(desc, kind, obj)
	u, _ := obj.(*unstructured.Unstructured)
	row.revisionKey = appliedRevision(u)
	if chart := chartName(u); chart != "" && row.revision != "–" {
		// A HelmRelease's REVISION cell is a chart version on its own
		// ("19.6.1"), which says nothing about *what* was installed.
		row.revision = chart + " " + row.revision
	}
	row.sourceKind, row.sourceName, row.sourceNamespace = sourceRefOf(u, row.namespace)
	if u != nil {
		row.applies = appliesLine(kube.FluxInventoryIDs(u.Object))
	}
	return row
}

// appliesTopNamespaces is how many namespaces the applies line names before
// folding the rest into "+N namespaces".
const appliesTopNamespaces = 3

// appliesLine summarises a reconciler's inventory by namespace, largest
// first. It reads only the ids already on the object — no per-kind list,
// so it costs nothing however many kinds the inventory spans (a health read
// here would start an informer per kind; 31a's inventory is where that
// belongs). A Namespace object counts toward the namespace it creates,
// which is how "the gitlab namespace appeared" shows up; any other
// cluster-scoped object counts only toward the total.
func appliesLine(ids []string) string {
	perNS := map[string]int{}
	total := 0
	for _, id := range ids {
		ns, name, group, kind, ok := kube.ParseFluxInventoryID(id)
		if !ok {
			continue
		}
		total++
		switch {
		case ns != "":
			perNS[ns]++
		case group == "" && kind == string(kube.KindNamespace):
			perNS[name]++
		}
	}
	if total == 0 {
		return ""
	}
	namespaces := make([]string, 0, len(perNS))
	for ns := range perNS {
		namespaces = append(namespaces, ns)
	}
	slices.SortFunc(namespaces, func(a, b string) int {
		return cmp.Or(cmp.Compare(perNS[b], perNS[a]), cmp.Compare(a, b))
	})

	noun := "objects"
	if total == 1 {
		noun = "object"
	}
	parts := []string{"applies " + strconv.Itoa(total) + " " + noun}
	for i, ns := range namespaces {
		if i == appliesTopNamespaces {
			rest := len(namespaces) - i
			more := "+" + strconv.Itoa(rest) + " namespaces"
			if rest == 1 {
				more = "+1 namespace"
			}
			parts = append(parts, more)
			break
		}
		parts = append(parts, ns+" "+strconv.Itoa(perNS[ns]))
	}
	return strings.Join(parts, " · ")
}

// baseRow copies the descriptor's own projected row — the same
// resources.Row §30a's table renders — into this screen's shape. Nothing
// about status, glyph or the condition message is re-derived here; that is
// what keeps the two screens from ever disagreeing about an object.
func baseRow(desc resources.Descriptor, kind kube.ResourceKind, obj runtime.Object) treeRow {
	r := desc.Project(obj)
	row := treeRow{
		kind:       kind,
		kindLabel:  kindLabel(desc.Kind.APIKind()),
		namespace:  r.Namespace,
		name:       r.Name,
		glyph:      r.Glyph,
		class:      r.Status,
		subLine:    r.SubLine,
		suspended:  r.Suspended,
		revision:   "–",
		reconciled: "–",
	}
	if r.GlyphClass != "" {
		row.class = r.GlyphClass
	}
	// §30a's curated column order: name, ready, revision, source, reconciled, age.
	if len(r.Cells) >= 5 {
		row.revision, row.reconciled = r.Cells[2], r.Cells[4]
	}
	return row
}

// missingSourceRow is the synthetic head for a sourceRef that resolved to
// nothing. It names the reference rather than inventing an object, and says
// plainly what was looked for and not found.
//
// readErr is why the source's kind couldn't be read, when it couldn't: then
// "not found" would be a claim kute has no grounds for, so the sub-line says
// the source is unreadable instead.
func missingSourceRow(child treeRow, readErr error) treeRow {
	name := child.sourceName
	if name == "" {
		name = "no source"
	}
	return treeRow{
		kindLabel:  kindLabel(string(child.sourceKind)),
		namespace:  child.sourceNamespace,
		name:       name,
		glyph:      "▲",
		class:      resources.StatusWarn,
		revision:   "–",
		reconciled: "–",
		subLine:    missingSubLine(child, readErr),
		isSource:   true,
		missing:    true,
	}
}

func missingSubLine(child treeRow, readErr error) string {
	if child.sourceName == "" {
		return "no sourceRef — nothing tells this reconciler what to apply"
	}
	if readErr != nil {
		return "source unreadable: " + strings.ToLower(string(child.sourceKind)) + "/" + child.sourceName +
			" in " + child.sourceNamespace + " — " + kindErrReason(readErr)
	}
	return "source not found: " + strings.ToLower(string(child.sourceKind)) + "/" + child.sourceName +
		" in " + child.sourceNamespace
}

// sourceRevisionCell is §30b's REVISION cell for a source: what it fetched,
// and when.
//
// A HelmRepository's artifact revision is the digest of the repo index — 71
// characters of hex that identify nothing a human compares — so it reads
// "index", the mockup's own word for it. Every other source carries a git
// revision worth naming.
func sourceRevisionCell(apiKind, revision string, updated time.Time) string {
	text := revision
	switch {
	case apiKind == "HelmRepository":
		text = "index"
	case text == "":
		text = "–"
	default:
		text = kube.ShortFluxRevision(text)
	}
	if !updated.IsZero() {
		text += " · " + shortAge(time.Since(updated)) + " ago"
	}
	return text
}

// sourceRefOf reads the reference that makes this object part of a chain
// (kube.FluxSourceOf). The namespace defaults to defaultNS, Flux's own rule,
// and is defaultNS too when there is no ref.
func sourceRefOf(u *unstructured.Unstructured, defaultNS string) (kube.ResourceKind, string, string) {
	if u == nil {
		return "", "", defaultNS
	}
	kind, ns, name, ok := kube.FluxSourceOf(u.Object, defaultNS)
	if !ok {
		return "", "", defaultNS
	}
	return kind, name, ns
}

func artifactRevision(u *unstructured.Unstructured) string {
	if u == nil {
		return ""
	}
	rev, _, _ := unstructured.NestedString(u.Object, "status", "artifact", "revision")
	return rev
}

func artifactUpdated(u *unstructured.Unstructured) time.Time {
	if u == nil {
		return time.Time{}
	}
	ts, _, _ := unstructured.NestedString(u.Object, "status", "artifact", "lastUpdateTime")
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}
	}
	return t
}

// appliedRevision is what a reconciler has actually applied — the left half
// of the drift comparison. HelmRelease reports it as lastAttemptedRevision;
// its lastAppliedRevision is null.
func appliedRevision(u *unstructured.Unstructured) string {
	if u == nil {
		return ""
	}
	for _, path := range [][]string{
		{"status", "lastAppliedRevision"},
		{"status", "lastAttemptedRevision"},
	} {
		if v, _, _ := unstructured.NestedString(u.Object, path...); v != "" {
			return v
		}
	}
	return ""
}

func chartName(u *unstructured.Unstructured) string {
	if u == nil {
		return ""
	}
	name, _, _ := unstructured.NestedString(u.Object, "spec", "chart", "spec", "chart")
	return name
}

// kindLabel is the KIND cell — the short name the mockup uses, not the API
// kind, which would push the column twice as wide for no added meaning.
func kindLabel(apiKind string) string {
	switch apiKind {
	case "GitRepository":
		return "GitRepo"
	case "HelmRepository":
		return "HelmRepo"
	case "OCIRepository":
		return "OCIRepo"
	case "Kustomization":
		return "Kustomize"
	case "":
		return "–"
	default:
		return apiKind
	}
}

func joinKey(kind kube.ResourceKind, namespace, name string) string {
	return string(kind) + "/" + namespace + "/" + name
}

// sortRows orders a group's reconcilers unhealthy-first, then by name — the
// same triage order §30a's list uses.
func sortRows(rows []treeRow) {
	slices.SortStableFunc(rows, func(a, b treeRow) int {
		return cmp.Or(
			cmp.Compare(statusRank(a.class), statusRank(b.class)),
			cmp.Compare(a.namespace, b.namespace),
			cmp.Compare(a.name, b.name),
		)
	})
}

// sortGroups puts the chains that need attention first: a group with
// anything unhealthy in it outranks a clean one, and among equals the worst
// row decides.
func sortGroups(groups []group) {
	slices.SortStableFunc(groups, func(a, b group) int {
		return cmp.Or(
			cmp.Compare(groupRank(a), groupRank(b)),
			cmp.Compare(a.head.namespace, b.head.namespace),
			cmp.Compare(a.head.name, b.head.name),
		)
	})
}

func groupRank(g group) int {
	worst := statusRank(g.head.class)
	for _, c := range g.children {
		worst = min(worst, statusRank(c.class))
	}
	return worst
}

// statusRank orders the classes worst-first. Suspended (Neutral) sits
// between failing and warning rather than last: a paused reconciler is
// drifting, not finished, so it must not sink below the healthy rows.
func statusRank(c resources.StatusClass) int {
	switch c {
	case resources.StatusFail:
		return 0
	case resources.StatusNeutral:
		return 1
	case resources.StatusWarn:
		return 2
	default:
		return 3
	}
}

// shortAge renders a duration the way every other compact age cell does.
func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "0m"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}
