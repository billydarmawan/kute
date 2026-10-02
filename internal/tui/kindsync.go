package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
)

// KindSyncChecker is implemented by a lister whose per-kind caches populate
// asynchronously (*kube.Cluster's informers, which start on first read).
// Mirrors browse.KindSyncChecker; declared here too so task packages that
// don't import browse can gate on it. namespace is the exact namespace the
// caller's own read used — cluster-wide mode always normalizes to the same
// answer regardless of what's passed, but namespace-scoped mode keys each
// cache by namespace, so asking about the wrong one answers for a cache that
// was never read (docs/lazy-informers.md §5.6: "ask about
// the same namespace the read used").
type KindSyncChecker interface {
	KindSynced(kind kube.ResourceKind, namespace string) bool
}

// KindStartedChecker is implemented by a lister whose informers start on
// first read (*kube.Cluster). KindStarted reports whether reading kind at
// namespace would start nothing new — its cache is already running, or
// nothing could ever back it. namespace follows KindSyncChecker's rule.
//
// Distinct from KindSyncChecker on purpose: KindSynced reports "settled" for
// a kind whose informer doesn't exist yet (so a spinner can never hang),
// which is exactly the wrong answer for a consumer deciding whether a read
// is free. See KindCached.
type KindStartedChecker interface {
	KindStarted(kind kube.ResourceKind, namespace string) bool
}

// CacheSyncChecker is the pre-per-kind, cluster-wide form.
type CacheSyncChecker interface {
	Synced() bool
}

// KindErrorReporter is implemented by a lister that can say why a kind's
// cache is empty when the reason isn't "there are none of them"
// (*kube.Cluster: an initial LIST that keeps failing). namespace follows the
// same rule as KindSyncChecker's.
type KindErrorReporter interface {
	KindError(kind kube.ResourceKind, namespace string) error
}

// KindForbiddenReporter is implemented by a lister that can say a kind's
// cache is empty because this identity may not list it (*kube.Cluster: a
// watch that came back Forbidden). namespace follows the same rule as
// KindSyncChecker's.
//
// Separate from KindErrorReporter because the two answers are not the same
// answer. A stalled initial LIST is being retried and may yet succeed, so a
// screen says "couldn't load — retrying"; a denial will not change while the
// process runs, so a screen says so and offers the 4b card instead.
type KindForbiddenReporter interface {
	KindForbidden(kind kube.ResourceKind, namespace string) error
}

// ScopedChecker is implemented by a lister that knows whether it's running
// under namespace-scoped mode (*kube.Cluster: SetNamespaceScope was called
// at launch). Browse's 403 card reads this to decide whether to name
// --namespace-scoped as a recovery option — there is nothing to suggest to a
// session already running scoped.
type ScopedChecker interface {
	Scoped() bool
}

// KindsError returns the first reason any of kinds has no data to show, or
// nil when the caches are simply empty.
//
// It is the companion to KindsSynced, and the two are only correct together.
// A cache that keeps failing its initial LIST reports *synced* — it is not
// going to fill, so holding a spinner on it is a spinner that never ends —
// which on its own would turn a failed read into the confident claim that the
// cluster has none of this kind. Screens that gate an empty state on
// KindsSynced ask this next, and show the failure instead.
//
// A denial outranks a stall, and is checked across every kind first rather
// than kind-by-kind: with one kind forbidden and another merely stalling, the
// permanent reason is the one worth showing, whichever order the caller
// happened to list them in.
//
// namespace is the namespace the caller's own read used — the same value it
// passes to KindsSynced for the same kinds.
func KindsError(lister any, namespace string, kinds ...kube.ResourceKind) error {
	if fr, ok := lister.(KindForbiddenReporter); ok {
		for _, kind := range kinds {
			if err := fr.KindForbidden(kind, namespace); err != nil {
				return err
			}
		}
	}
	kr, ok := lister.(KindErrorReporter)
	if !ok {
		return nil
	}
	for _, kind := range kinds {
		if err := kr.KindError(kind, namespace); err != nil {
			return err
		}
	}
	return nil
}

// KindsSynced reports whether every one of kinds has a cache worth believing.
//
// This is what stands between a screen and the worst failure mode of lazy
// informers: reading a kind for the first time returns an empty cache, and a
// screen that takes that at face value announces "nothing here" about data
// that is seconds away. The empty *state* is a claim about the cluster, so it
// may only be entered once the caches backing it have actually filled.
//
// True for any lister implementing neither checker (fakes, test doubles), so
// this only changes behavior against a live cluster. Also true for kinds
// nothing will ever deliver — a stopped cluster, a kind with no informer, one
// whose watch came back Forbidden — so a caller gating a spinner on it cannot
// hang waiting for a cache that is never coming.
//
// namespace is the namespace the caller's own read used for kinds — pass ""
// for a cluster-wide read (Nodes, an explicit all-namespaces screen); pass
// the read's actual namespace otherwise. Getting this wrong under
// namespace-scoped mode asks a cache that was never read whether it's
// trustworthy, which reads as "settled" for the wrong reason (nothing has
// ever started that informer) rather than the right one.
func KindsSynced(lister any, namespace string, kinds ...kube.ResourceKind) bool {
	if lister == nil {
		return true
	}
	if kc, ok := lister.(KindSyncChecker); ok {
		for _, kind := range kinds {
			if !kc.KindSynced(kind, namespace) {
				return false
			}
		}
		return true
	}
	if sc, ok := lister.(CacheSyncChecker); ok {
		return sc.Synced()
	}
	return true
}

// KindCached reports whether kind's cache at namespace is already running
// and filled — the gate for every breadth-first reader of caches it isn't
// displaying (the goto palette's fuzzy resource corpus, browse's empty-state
// hints). Those must only read what this session has already paid for:
// reading anything else starts an informer per kind on a keystroke, which is
// the launch stampede relocated (CLAUDE.md, "never read a cache
// breadth-first").
//
// KindsSynced alone is not that gate. It answers "settled" for a kind with no
// informer yet, so before 14b had been opened it vouched for
// KindCustomResourceDefinition and goto started the full-CRD informer on
// the first fuzzy keystroke. True for a lister implementing neither checker
// (fakes, test doubles), whose reads cost nothing.
func KindCached(lister any, kind kube.ResourceKind, namespace string) bool {
	if lister == nil {
		return true
	}
	if ks, ok := lister.(KindStartedChecker); ok && !ks.KindStarted(kind, namespace) {
		return false
	}
	return KindsSynced(lister, namespace, kind)
}

// CacheSyncRetryInterval is how often a screen re-checks a cache that was
// still filling when it last looked. Matches browse's own reload debounce.
//
// A retry is needed rather than relying on the informer's change events
// alone, because a kind with genuinely zero objects syncs without ever
// emitting one — so the screen would otherwise sit on a spinner forever for
// the one case where "empty" is the right answer.
const CacheSyncRetryInterval = 250 * time.Millisecond

// CacheSyncRetryMsg asks a screen to reload because a cache it read was not
// yet populated. Screens carry their own reload epoch in Gen and drop a msg
// that doesn't match, so a retry scheduled before a since-superseded load
// can't resurrect it.
type CacheSyncRetryMsg struct{ Gen int }

// ScheduleCacheSyncRetry is the tick behind CacheSyncRetryMsg.
func ScheduleCacheSyncRetry(gen int) tea.Cmd {
	return tea.Tick(CacheSyncRetryInterval, func(time.Time) tea.Msg {
		return CacheSyncRetryMsg{Gen: gen}
	})
}
