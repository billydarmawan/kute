package browse

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// otherKindHint is one "N kind" fragment for the 10c "g other kinds" line.
type otherKindHint struct {
	label string
	count int
}

// emptyHints is the live data behind 10c's three ways out. Zero value
// renders each way-out line without its trailing "— ..." detail, so a
// hint that failed to load (or genuinely has nothing to suggest) degrades
// to a plain, still-truthful line rather than blocking the empty state.
type emptyHints struct {
	altNamespace string
	altCount     int
	allCount     int
	otherKinds   []otherKindHint
	// namespace is what the Namespace cache says about the namespace found
	// empty — the 10c explainer only claims "the namespace exists" once
	// it's been checked, since a namespace restored from state may since
	// have been deleted.
	namespace namespaceExistence
}

// namespaceExistence is emptyHints' verdict on the current namespace. The
// zero value, unknown, is what renders before the hints land or when the
// Namespace cache can't vouch either way (unsynced, unreadable, empty).
type namespaceExistence int

const (
	namespaceUnknown namespaceExistence = iota
	namespaceExists
	namespaceMissing
)

// loadEmptyHints computes the three 10c ways-out against the informer
// cache: the busiest other namespace for this kind, the all-namespaces
// total, and up to two other kinds with resources in the current
// namespace. Cluster-scoped kinds (Nodes, Namespaces) have no
// namespace-relative "ways out", so this returns nil for them.
func (m Model) loadEmptyHints() tea.Cmd {
	if m.desc.ClusterScoped {
		return nil
	}
	lister := m.lister
	reg := resources.Registry{}
	if m.session != nil {
		reg = m.session.Registry
	}
	kind := m.kind
	namespace := m.namespace
	timeout := m.timeout
	epoch := m.reloadEpoch

	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		hints := emptyHints{
			otherKinds: otherKindsIn(ctx, lister, reg, kind, namespace),
		}
		if namespace != "" {
			hints.namespace = namespaceStatus(ctx, lister, reg, namespace)
			hints.altNamespace, hints.altCount = busiestOtherNamespace(ctx, lister, reg, kind, namespace)
			if n, ok := allNamespacesCount(ctx, lister, kind); ok {
				hints.allCount = n
			}
		}

		return emptyHintsMsg{epoch: epoch, namespace: namespace, kind: kind, hints: hints}
	}
}

// namespaceStatus checks namespace against the Namespace cache, which is
// eager (started at connect), so this is a plain cache read — and gated on
// tui.KindCached all the same, so it can never be the read that starts it.
// It only answers missing from a cache it can believe: filled, readable and
// non-empty — every real cluster has at least default and kube-system, so
// an empty list is a cache that can't see them, not proof of absence.
// Skipped under --namespace-scoped, like busiestOtherNamespace: a session
// scoped to its namespaces is often one that can't list the rest.
func namespaceStatus(ctx context.Context, lister resources.RawLister, reg resources.Registry, namespace string) namespaceExistence {
	if sc, ok := lister.(tui.ScopedChecker); ok && sc.Scoped() {
		return namespaceUnknown
	}
	if !tui.KindCached(lister, kube.KindNamespace, "") || tui.KindsError(lister, "", kube.KindNamespace) != nil {
		return namespaceUnknown
	}
	nsDesc, ok := reg.Descriptor(kube.KindNamespace)
	if !ok {
		return namespaceUnknown
	}
	rows, err := resources.List(ctx, lister, nsDesc, "")
	if err != nil || len(rows) == 0 {
		return namespaceUnknown
	}
	for _, row := range rows {
		if row.Name == namespace {
			return namespaceExists
		}
	}
	return namespaceMissing
}

// allNamespacesCount answers the "N cluster-wide" hint detail. It reads
// the cluster-wide cache only when that read is free (tui.KindCached): under
// plain (cluster-wide) mode that is the same single cache already backing
// the namespace just found empty, so for nearly every kind this is a plain
// cache read. Two cases are not free, and both fall back to
// tui.LiveCounter (CountLive, one server-side list instead of an informer):
//
//   - --namespace-scoped, where kind's cache is keyed per namespace
//     (docs/lazy-informers.md §5.6), so a "" read would start a brand-new
//     cluster-wide informer merely to decorate a hint — the exact implicit
//     global read scoped mode exists to avoid.
//   - KindHelmRelease, which is cached per namespace even in plain mode
//     (§5.5), so a "" read starts the cluster-wide release-Secret informer
//     (8.19 MB on the cluster §5.5 measured) and decodes every release.
//
// CountLive has no honest answer for KindHelmRelease, so that case reports
// no detail at all; the caller already degrades to a plain, still-truthful
// line when this returns false.
func allNamespacesCount(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind) (int, bool) {
	scoped := false
	if sc, ok := lister.(tui.ScopedChecker); ok && sc.Scoped() {
		scoped = true
	}
	if !scoped && tui.KindCached(lister, kind, "") {
		n, err := resources.Count(ctx, lister, kind, "")
		return n, err == nil
	}
	lc, ok := lister.(tui.LiveCounter)
	if !ok {
		return 0, false
	}
	n, err := lc.CountLive(ctx, kind, "")
	return n, err == nil
}

// busiestOtherNamespace finds the namespace (other than the current one)
// with the most instances of kind, for the "n switch namespace — X has N
// pods" hint.
//
// Only namespaces whose cache for kind is already running and filled
// (tui.KindCached) are counted. For most kinds in plain mode that is every
// namespace — one cluster-wide cache backs them all — so the hint is as
// complete as ever. KindHelmRelease is the exception: it is cached per
// namespace even in plain mode (docs/lazy-informers.md §5.5), so counting
// every namespace would start one release-Secret informer per namespace and
// decode every release in the cluster just to decorate an empty state. The
// hint then names the busiest namespace this session has already read, or
// nothing.
//
// Skipped entirely under --namespace-scoped: every namespaced kind is keyed
// per namespace there (§5.6), so the KindCached gate would leave only the
// namespaces already visited — and the namespace palette's own per-namespace
// count loop makes the same call. The caller already degrades to a plain,
// still-truthful line when this returns nothing.
func busiestOtherNamespace(ctx context.Context, lister resources.RawLister, reg resources.Registry, kind kube.ResourceKind, current string) (string, int) {
	if sc, ok := lister.(tui.ScopedChecker); ok && sc.Scoped() {
		return "", 0
	}
	nsDesc, ok := reg.Descriptor(kube.KindNamespace)
	if !ok {
		return "", 0
	}
	nsRows, err := resources.List(ctx, lister, nsDesc, "")
	if err != nil {
		return "", 0
	}
	best, bestCount := "", 0
	for _, row := range nsRows {
		if row.Name == "" || row.Name == current {
			continue
		}
		if !tui.KindCached(lister, kind, row.Name) {
			continue
		}
		n, err := resources.Count(ctx, lister, kind, row.Name)
		if err != nil || n <= bestCount {
			continue
		}
		best, bestCount = row.Name, n
	}
	return best, bestCount
}

// otherKindsIn returns up to two non-zero, non-cluster-scoped kinds (other
// than exclude) present in namespace, ordered by count descending, for the
// "g other kinds — this namespace has 2 configmaps, 1 secret" hint.
//
// Only kinds whose caches are already running and populated (tui.KindCached)
// are considered. Counting the
// rest would mean starting an informer per kind just to decorate an empty
// state — the launch stampede, relocated to whenever a namespace happens to
// be empty. A thinner hint is the right trade: the caller already degrades
// to a plain, still-truthful line when there's nothing to name.
func otherKindsIn(ctx context.Context, lister resources.RawLister, reg resources.Registry, exclude kube.ResourceKind, namespace string) []otherKindHint {
	var found []otherKindHint
	for _, group := range resources.DefaultGroups() {
		for _, k := range group.Kinds {
			if k == exclude {
				continue
			}
			desc, ok := reg.Descriptor(k)
			if !ok || desc.ClusterScoped {
				continue
			}
			if !tui.KindCached(lister, k, namespace) {
				continue
			}
			n, err := resources.Count(ctx, lister, k, namespace)
			if err != nil || n == 0 {
				continue
			}
			found = append(found, otherKindHint{label: kindNoun(desc, n), count: n})
		}
	}
	slices.SortStableFunc(found, func(a, b otherKindHint) int { return cmp.Compare(b.count, a.count) })
	if len(found) > 2 {
		found = found[:2]
	}
	return found
}

// kindNoun renders "2 configmaps" / "1 secret": every built-in Descriptor.Display
// is already the plural noun (registry.go), so the singular is just that
// noun minus its trailing "s".
func kindNoun(desc resources.Descriptor, n int) string {
	singular := strings.ToLower(strings.TrimSuffix(desc.Display, "s"))
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %ss", n, singular)
}
