package tui

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/sync/errgroup"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

// LiveCounter counts a kind server-side, without reading or starting an
// informer — satisfied by *kube.Cluster.CountLive. Optional: a session whose
// lister doesn't provide it falls back to counting whatever caches are
// already populated, which is what --demo and the tests do.
type LiveCounter interface {
	CountLive(ctx context.Context, kind kube.ResourceKind, namespace string) (int, error)
}

// countTTL is how long a fetched count stays fresh. The jump palette rebuilds
// its whole item list on every keystroke, so without a memo each keypress
// would re-ask the cluster for every kind; with one, a palette session costs
// a single round trip per kind. Short enough that reopening the palette after
// doing something reflects the change.
const countTTL = 10 * time.Second

// countCacheKey identifies one counted scope.
type countCacheKey struct {
	kind      kube.ResourceKind
	namespace string
}

type countCacheEntry struct {
	count     int
	fetchedAt time.Time
}

// countCache memoizes per-kind counts across a palette session. Guarded by
// its own mutex because fetchGotoCountsCmd writes from a tea.Cmd goroutine
// while the Update loop reads.
type countCache struct {
	mu      sync.Mutex
	entries map[countCacheKey]countCacheEntry
}

func (c *countCache) get(kind kube.ResourceKind, namespace string) (int, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[countCacheKey{kind, namespace}]
	if !ok || time.Since(entry.fetchedAt) > countTTL {
		return 0, false
	}
	return entry.count, true
}

func (c *countCache) put(kind kube.ResourceKind, namespace string, count int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[countCacheKey]countCacheEntry{}
	}
	c.entries[countCacheKey{kind, namespace}] = countCacheEntry{count: count, fetchedAt: time.Now()}
}

func (c *countCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = nil
	c.mu.Unlock()
}

// CachedCount returns a previously fetched count for kind in namespace, or
// countUnknown when none is fresh. Never blocks and never touches the
// cluster — see gotoCount's doc comment for why that matters.
func (s *Session) CachedCount(kind kube.ResourceKind, namespace string) int {
	if s == nil {
		return countUnknown
	}
	if n, ok := s.counts.get(kind, namespace); ok {
		return n
	}
	// Without a live counter (lister doubles in tests), fall back to counting
	// whatever cache is already populated. That reads as 0 for a kind whose
	// informer hasn't started, which is why it is only a fallback.
	if s.Lister != nil && s.liveCounter() == nil {
		if n, err := resources.Count(s.ClusterContext(), s.Lister, kind, namespace); err == nil {
			return n
		}
	}
	return countUnknown
}

// InvalidateCounts drops every memoized count — used on a context switch,
// where every number belongs to the cluster just left.
func (s *Session) InvalidateCounts() {
	if s != nil {
		s.counts.clear()
	}
}

func (s *Session) liveCounter() LiveCounter {
	if s == nil || s.Lister == nil {
		return nil
	}
	lc, ok := s.Lister.(LiveCounter)
	if !ok {
		return nil
	}
	return lc
}

// gotoCountsMsg carries fetchGotoCountsCmd's background result back to the
// root Update. gen is checked against Model.gotoGen before applying.
type gotoCountsMsg struct {
	gen int
}

// countFetchConcurrency bounds how many count requests are in flight at
// once. Each is a couple of kilobytes, but a cluster with many CRDs would
// otherwise open dozens of connections the instant the palette opened.
const countFetchConcurrency = 8

// fetchGotoCountsCmd counts every registered kind off the Update loop —
// tea.Cmds run in their own goroutine — so the palette opens instantly and
// the numbers fill in behind it. Mirrors fetchNamespaceCPUSharesCmd.
func fetchGotoCountsCmd(sess *Session, gen int) tea.Cmd {
	counter := sess.liveCounter()
	if counter == nil {
		return nil
	}
	kinds := sess.Registry.Kinds()
	ns := gotoNamespace(sess)
	parent := sess.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, countFetchTimeout)
		defer cancel()

		// SetLimit blocks at g.Go, so this bounds goroutines and not just
		// requests in flight. The hand-rolled semaphore it replaces was
		// acquired *inside* the goroutine, which meant a cluster with a few
		// hundred discovered CRDs spawned a few hundred goroutines the moment
		// the palette opened — and queued ones ignored ctx, outliving
		// countFetchTimeout instead of bailing.
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(countFetchConcurrency)
		for _, kind := range kinds {
			desc, ok := sess.Registry.Descriptor(kind)
			if !ok {
				continue
			}
			scope := ns
			if desc.ClusterScoped {
				scope = ""
			}
			if _, fresh := sess.counts.get(kind, scope); fresh {
				continue
			}
			g.Go(func() error {
				if n, err := counter.CountLive(gctx, kind, scope); err == nil {
					sess.counts.put(kind, scope, n)
				}
				// A count that failed is simply unknown, rendered as a dash.
				// It must not cancel its siblings, so no error ever leaves
				// this task and g.Wait can only ever report nil.
				return nil
			})
		}
		_ = g.Wait()
		return gotoCountsMsg{gen: gen}
	}
}

// countFetchTimeout bounds the whole fan-out. A palette hint is never worth
// holding a goroutine open on an unresponsive cluster.
const countFetchTimeout = 5 * time.Second
