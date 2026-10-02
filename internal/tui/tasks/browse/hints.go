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
}

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
		hints.altNamespace, hints.altCount = busiestOtherNamespace(ctx, lister, reg, kind, namespace)
		if n, ok := allNamespacesCount(ctx, lister, kind); ok {
			hints.allCount = n
		}

		return emptyHintsMsg{epoch: epoch, namespace: namespace, kind: kind, hints: hints}
	}
}

// allNamespacesCount answers the "N cluster-wide" hint detail. Under plain
// (cluster-wide) mode this is a free cache read: resources.Count(kind, "")
// goes through the same single cache already backing the namespace just
// found empty. Under --namespace-scoped, kind's cache is keyed per
// namespace (docs/lazy-informers.md §5.6), so that same
// call would start a brand-new cluster-wide informer for kind merely to
// decorate a hint — the exact implicit global read scoped mode exists to
// avoid. tui.LiveCounter (CountLive) answers the same question with one
// server-side list instead of an informer, so scoped mode uses that and
// falls back to no detail at all when it isn't available (KindHelmRelease
// has no cheap live count; the caller already degrades to a plain,
// still-truthful line when this returns false).
func allNamespacesCount(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind) (int, bool) {
	if sc, ok := lister.(tui.ScopedChecker); ok && sc.Scoped() {
		lc, ok := lister.(tui.LiveCounter)
		if !ok {
			return 0, false
		}
		n, err := lc.CountLive(ctx, kind, "")
		return n, err == nil
	}
	n, err := resources.Count(ctx, lister, kind, "")
	return n, err == nil
}

// busiestOtherNamespace finds the namespace (other than the current one)
// with the most instances of kind, for the "n switch namespace — X has N
// pods" hint.
//
// Skipped entirely under --namespace-scoped: resources.Count(kind, ns) goes
// through ListRaw, so calling it once per namespace would start one
// per-namespace informer for kind just to decorate an empty state — the
// exact breadth-first read the mode exists to avoid (docs/lazy-informers.md
// §5.6, same reasoning as the namespace palette's own per-namespace count
// loop). The caller already degrades to a plain, still-truthful line when
// this returns nothing.
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
