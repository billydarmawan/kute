package browse

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// auxKinds maps a listed kind to the other kinds its rows and prompts read.
// A list is rarely built from one cache: the Deployments IMAGE column
// compares against ReplicaSets, the Ingresses BACKENDS column resolves
// through Services and Pods, the Pods health strip counts Nodes, and the
// scale prompt looks for a managing HorizontalPodAutoscaler.
//
// Two things use this. Reloading: a change to any of these kinds changes
// what the current list should show, and browse subscribed only to the kind
// it was displaying — so a Deployment's image cell went stale on a rollout
// until something else prompted a reload. Prefetching: informers start on
// first read, and several of these are read synchronously from a keypress
// handler with nowhere to put a loading state, so the first press would
// otherwise get an empty cache and quietly render a wrong-but-plausible
// answer ("no rollout history", "no HPA manages this").
var auxKinds = map[kube.ResourceKind][]kube.ResourceKind{
	kube.KindPod:  {kube.KindNode},
	kube.KindNode: {kube.KindPod},
	kube.KindDeployment: {
		kube.KindReplicaSet,              // IMAGE column + rollout history
		kube.KindHorizontalPodAutoscaler, // scale prompt
		kube.KindPod,                     // set-resources usage
	},
	kube.KindStatefulSet: {
		kube.KindControllerRevision, // rollout history
		kube.KindHorizontalPodAutoscaler,
		kube.KindPod,
	},
	kube.KindDaemonSet: {
		kube.KindControllerRevision,
		kube.KindPod,
	},
	kube.KindIngress: {kube.KindService, kube.KindPod}, // BACKENDS column
	kube.KindService: {kube.KindPod},                   // label-join in the meta editor
	kube.KindCronJob: {
		// §36a: last outcome, active runs, and history all come from
		// associated Jobs (0.8.0 plan §4.4) — a Job or Pod watch event
		// changes what the list should show even though CronJob's own
		// object didn't change. Job is also what opening CronJobs must
		// start lazily alongside CronJob itself (§4.4 point 2); Pod is
		// already eager at connect, listed here only for the reload trigger.
		kube.KindJob,
		kube.KindPod,
	},
	kube.KindJob: {
		// §37a: FAILED/COMPL come straight from the Job object itself
		// (loadJobRows' own doc comment), but the 'l' key's newest-attempt
		// target and the pushed attempts screen (§37b) both read live Pod
		// data — a Pod watch event changes what 'l' would open even though
		// the Job row's own cells didn't change. Pod is already eager at
		// connect, listed here only for the reload trigger.
		kube.KindPod,
	},
	kube.KindHelmRelease: {
		// 18a's rollout glyph: a release's workloads settling is a change to
		// three other kinds and none to the release Secret, so without these
		// the ▸ would be painted once and never cleared — the release would
		// read "rolling out" until something unrelated prompted a reload.
		kube.KindDeployment,
		kube.KindStatefulSet,
		kube.KindDaemonSet,
	},
	kube.ResourceKind("Application"): {kube.KindConfigMap, kube.KindEvent},
}

// Deliberately absent: KindSecret under KindHelmRelease. Releases used to be
// decoded from the shared Secret cache, so the Helm list did need it — but
// they have had their own server-side-filtered release cache since
// docs/lazy-informers.md §5.2, and that cache emits KindHelmRelease change
// events itself. All the entry did was prefetch the shared, unfiltered
// cluster-wide Secret cache (12.3 MB on the cluster this was measured
// against) on the way into a screen that never reads it — reinstating, as a
// prefetch, the exact read §5.2 removed. On a slow link the two multi-MB
// LISTs then compete for the same bandwidth and the Helm list is the one
// left waiting.

// auxScope is the namespace argument a sync/error check on aux kind kind
// must use for the kind currently on screen. Every aux relationship reads
// at the primary kind's own namespace except one: Node's Pod aux-kind read
// (nodes.go's loadNodeExtras) is unconditionally cluster-wide, since a
// node's pods can come from any namespace — unlike Pod's own Node aux-kind
// read, which needs no override at all, because Node is cluster-scoped and
// *kube.Cluster's own cacheScope already normalizes any namespace passed
// for it to "" (docs/lazy-informers.md §5.6).
func (m Model) auxScope(kind kube.ResourceKind) string {
	if m.kind == kube.KindNode && kind == kube.KindPod {
		return ""
	}
	return m.namespace
}

// currentAuxKinds includes the owner caches needed by a Helm-release-scoped
// Pods view. They are dynamic because only manifest sources actually shown
// may start an informer.
func (m Model) currentAuxKinds() []kube.ResourceKind {
	kinds := append([]kube.ResourceKind(nil), auxKinds[m.kind]...)
	if m.kind != kube.KindPod || !m.releasePodView {
		return kinds
	}
	for _, needed := range m.releasePodSupportKinds() {
		if needed != "" && !slices.Contains(kinds, needed) {
			kinds = append(kinds, needed)
		}
	}
	return kinds
}

func (m Model) releasePodSupportKinds() []kube.ResourceKind {
	var kinds []kube.ResourceKind
	for _, source := range m.releasePodSources {
		var needed kube.ResourceKind
		switch source.Kind {
		case kube.KindDeployment:
			needed = kube.KindReplicaSet
		case kube.KindCronJob:
			needed = kube.KindJob
		}
		if needed != "" && !slices.Contains(kinds, needed) {
			kinds = append(kinds, needed)
		}
	}
	return kinds
}

// auxKindsSynced reports whether every one of kinds' own caches — each
// asked about at its own correct scope (auxScope) — is worth believing.
func (m Model) auxKindsSynced(kinds []kube.ResourceKind) bool {
	for _, k := range kinds {
		if !tui.KindsSynced(m.lister, m.auxScope(k), k) {
			return false
		}
	}
	return true
}

// auxKindsError returns the first reason any of kinds has nothing to show,
// each asked about at its own correct scope (auxScope).
func (m Model) auxKindsError(kinds []kube.ResourceKind) error {
	for _, k := range kinds {
		if err := tui.KindsError(m.lister, m.auxScope(k), k); err != nil {
			return err
		}
	}
	return nil
}

// auxKindOf reports whether changed is one of listed's secondary kinds.
func auxKindOf(listed, changed kube.ResourceKind) bool {
	return slices.Contains(auxKinds[listed], changed)
}

// prefetchAuxKinds warms the caches the current kind's rows and prompts read,
// off the Update loop. Each read is what starts the kind's informer, so by
// the time the user has moved to a row and pressed a key, the data those
// synchronous handlers need has had seconds to arrive.
//
// Results are discarded: this is a cache-warming side effect, not a fetch.
// That keeps it free of any new interface — it works through the same
// RawLister every screen already holds, decorators included.
func (m Model) prefetchAuxKinds() tea.Cmd {
	kinds := m.currentAuxKinds()
	if len(kinds) == 0 || m.lister == nil {
		return nil
	}
	// Warm the same cache each aux read will use (auxScope: a Nodes list
	// reads Pods cluster-wide), not the active namespace's — under
	// --namespace-scoped that would start a cache nothing reads.
	scopes := make([]string, len(kinds))
	for i, kind := range kinds {
		scopes[i] = m.auxScope(kind)
	}
	lister := m.lister
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		for i, kind := range kinds {
			// Warming a cache; a failure just means the read happens later.
			_, _ = lister.ListRaw(parent, kind, scopes[i])
		}
		return nil
	}
}
