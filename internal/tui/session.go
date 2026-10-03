package tui

import (
	"context"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/helmrepo"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/state"
)

// MetricsReader is the live pod-usage seam the namespace palette needs for
// its CPU-share column (docs/design README.md §6a) — satisfied by
// *kube.Cluster and *kube/fake.Cluster, the same concrete types
// browse.MetricsReader accepts. Declared again here rather than reused from
// browse: tui can't import tasks/browse (browse imports tui — the same
// import-cycle constraint Session.HelpScope/HelpList/HelpResource/HelpMisc
// already documents), so the composition root wires the same value into
// both fields.
type MetricsReader interface {
	PodMetricsByNamespace(ctx context.Context, namespace string) (map[string]kube.PodMetrics, error)
}

// Location is the active browse position: kube-context, namespace ("" means
// all namespaces), resource kind, and (optionally) a selected resource
// name. Filter mirrors browse's live filterQuery (browse writes it on every
// change via its setFilter helper) so context.go's switchContextCmd — which
// runs in this package, not browse — can read the outgoing context's filter
// to persist into state.PerContext and restore it on switching back
// (docs/design README.md §7a: "each context remembers its own namespace +
// kind + filter").
type Location struct {
	Context   string
	Namespace string
	Kind      kube.ResourceKind
	Resource  string
	Filter    string
}

// Session is the cross-screen state the root shell owns and hands to every
// task (mvp-plan.md §0.9): the live cluster seam, the resource catalog,
// persisted recents/per-context state, user config (prod contexts), the
// current browse location, and the selected Theme/Styles (picked once at
// startup — see decision #3 — and threaded through Session rather than a
// package-level var, so a --theme override only ever touches this one
// value).
type Session struct {
	Cluster  *kube.Cluster // nil when no kubeconfig / cluster unreachable
	Registry resources.Registry
	Groups   []resources.Group
	State    state.State
	Config   config.Config
	Location Location
	Theme    Theme
	Styles   Styles
	// Kubeconfig is --kubeconfig's value ("" when unset), set once at the
	// composition root — the half of CommandTarget a kubectl/helm child
	// process can't discover on its own.
	Kubeconfig string
	// Lister is the same RawLister browse reads through (the real
	// *kube.Cluster or, in --demo, *fake.Cluster) — the root shell needs it
	// too, to build the jump palette's live kind counts and resource-name
	// corpus (mvp-plan.md Phase 2) without depending on a concrete cluster
	// type. Nil when no cluster is reachable.
	Lister resources.RawLister
	// ctx carries the process-lifetime context the composition root installs
	// (SetContext) plus the per-kube-context child every cluster read hangs
	// off — see sessionctx.go for why there are two and which one a caller
	// wants. Zero value works: both accessors fall back to
	// context.Background() until a root is installed.
	ctx sessionCtx
	// counts memoizes the jump palette's per-kind counts for countTTL, so
	// rebuilding the item list on every keystroke doesn't re-ask the
	// cluster. Filled by fetchGotoCountsCmd off the Update loop; see
	// gotoCount for why the palette must never count synchronously.
	counts countCache
	// rebuild lets one context switch or cluster rebuild run at a time
	// (rebuildgate.go).
	rebuild rebuildGate
	// fluxSubjects retains §32a's revision→commit-subject pairs for the life
	// of the session, so a "◆ revision applied" row keeps naming its commit
	// after the source-controller event that carried the subject has aged
	// out. Filled by tasks/timeline's load off the Update loop; see
	// fluxsubjects.go for why this one is in memory and never persisted.
	fluxSubjects fluxSubjectCache
	// Metrics is the same seam as browse.Config.Metrics, wired to the same
	// concrete cluster value — used by the namespace palette (namespace.go)
	// for its CPU-share column. Nil when no cluster is reachable, or when a
	// reachable cluster has no metrics-server (PodMetricsByNamespace then
	// errors per-call) — both degrade to the column simply being omitted.
	Metrics MetricsReader
	// Forwards is the app-wide port-forward registry (13a/13c/13d) — built
	// once at the composition root and never rebuilt on context switch, so
	// forwards survive one (docs/design README.md §13d: "global across
	// context switches"). Nil when no cluster is reachable.
	Forwards *kube.ForwardManager
	// DebugCopies tracks §41c's "copy pod" debug-mode pods kute itself
	// created this session — same placement/lifetime as Forwards (built
	// once at the composition root, survives context switches, since a
	// copy pod's identity is stable regardless of which context/namespace
	// the user is currently browsing). Nil when no cluster is reachable.
	DebugCopies *kube.DebugCopyRegistry
	// Charts is the local Helm repo cache behind 18a's LATEST column, built
	// once at the composition root and — like Forwards — never rebuilt on a
	// context switch: it describes the user's own disk, not the cluster.
	// Nil is a working value (every chart version reads unknown).
	Charts *helmrepo.Cache
	// Version is kute's own running build version ("0.2.0", no leading
	// "v") — set once at the composition root from the ldflags-injected
	// build version (main.go), the "you run X" side of every 28a/28b
	// comparison.
	Version string
	// Update is 28a/28b's ambient check result for this process — nil until
	// the startup check resolves (or forever, if update.check is disabled).
	// See UpdateInfo's doc comment for why this is richer than, and
	// separate from, the State.UpdateCheck trio that actually persists.
	Update *UpdateInfo
	// UpdateCheckErr is the most recently resolved check's error (ambient or
	// 'r'-forced) — nil on success, and also nil until any check has
	// resolved at all. Update alone can't distinguish "still loading" from
	// "checked, but the release feed was unreachable": both leave Update
	// nil forever, which used to strand 28b on "checking for updates…" with
	// no escape hatch (28a's own ambient chip stays silent on error by
	// design, but the manually-opened panel still needs a way out). Cleared
	// back to nil by a later successful check.
	UpdateCheckErr error
	// HelpScope, HelpList, HelpResource, and HelpMisc are the 7b help
	// overlay's four fixed groups, pre-built at the composition root
	// (internal/app.BuildSession) from the verbs registry: tui itself can't
	// import verbs (verbs depends on tui, directly and via actions), so the
	// registry lookup happens where the import direction allows it and the
	// result is threaded through Session like everything else cross-screen.
	// HelpScope is the navigation-scope verbs (goto/namespace/all-namespaces/
	// context); HelpList is generic list-row actions (filter/sort/move/mark/
	// open); HelpResource is per-object actions (yaml/edit/events/timeline/
	// meta); HelpMisc is the horizontal footer row (what's-new/help/back/
	// quit). Keys with no registry entry (pure movement/exit — ↑↓, 1-9, esc)
	// are literals baked in alongside the verb-sourced ones at the same call
	// site.
	HelpScope, HelpList, HelpResource, HelpMisc []KeyHint
	// pendingKind is a restored kind browse could not open yet, and
	// pendingKindContext the context it was restored for — see
	// HoldPendingKind.
	pendingKind        kube.ResourceKind
	pendingKindContext string
}

// HoldPendingKind records kind as the kind the user was last on in the
// current context, which browse fell back from because Registry can't
// resolve it yet. At launch the registry is the built-ins only until the
// detached Start's discovery lands, so a persisted CRD kind (a Flux
// Kustomization, a Certificate, an HTTPRoute) opens on Pods: holding it here
// is what lets the root shell jump to it on kube.CRDsDiscoveredMsg, and
// SyncLocationToPerContext persist it rather than the Pod fallback, so
// quitting before discovery finishes doesn't lose it for good.
func (s *Session) HoldPendingKind(kind kube.ResourceKind) {
	s.pendingKind = kind
	s.pendingKindContext = s.Location.Context
}

// PendingKind is the kind HoldPendingKind recorded, or "" if there is none
// or it belongs to a context other than the current one.
func (s *Session) PendingKind() kube.ResourceKind {
	if s == nil || s.pendingKindContext != s.Location.Context {
		return ""
	}
	return s.pendingKind
}

// ClearPendingKind drops a held kind — the user navigated, or the kind
// resolved and browse opened it.
func (s *Session) ClearPendingKind() {
	if s == nil {
		return
	}
	s.pendingKind, s.pendingKindContext = "", ""
}

// CommandTarget is the cluster every kubectl/helm subprocess a screen
// launches (exec, debug, edit, and their "will run" lines) must target: the
// active context kute itself is browsing, never the kubeconfig's own
// current-context (see kube.CommandTarget). Zero with no real cluster — in
// --demo nothing shells out, and the will-run lines stay free of a fake
// context name no kubectl could resolve.
func (s *Session) CommandTarget() kube.CommandTarget {
	if s == nil || s.Cluster == nil {
		return kube.CommandTarget{}
	}
	return kube.CommandTarget{Kubeconfig: s.Kubeconfig, Context: s.Location.Context}
}

// SyncLocationToPerContext snapshots s.Location's namespace/kind/filter into
// s.State.PerContext[s.Location.Context], overwriting whatever was there.
// switchContextCmd calls this for the outgoing context right before
// rebuilding the cluster (so 7a's per-context restore survives switching
// away); RunWithConfig calls it once more at exit for whatever context is
// still active — otherwise a session that only ever visits one context (e.g.
// a single-cluster dev setup) would never persist its browsed
// namespace/kind, since the switch-time snapshot never runs. A held
// PendingKind is persisted in place of the Pod fallback browse is showing. A no-op when no
// context is set (no reachable cluster, or --demo, whose location Session
// never restores from PerContext anyway).
func (s *Session) SyncLocationToPerContext() {
	if s == nil || s.Location.Context == "" {
		return
	}
	pc := s.State.PerContext[s.Location.Context]
	pc.Namespace = s.Location.Namespace
	pc.Kind = string(s.Location.Kind)
	if pending := s.PendingKind(); pending != "" {
		pc.Kind = string(pending)
	}
	pc.Filter = s.Location.Filter
	s.State.PerContext[s.Location.Context] = pc
}

// ForwardSummary tallies s.Forwards for 13d's header chip — zero value
// (BuildForwardChip renders nothing) when Forwards is nil.
func (s *Session) ForwardSummary() ForwardSummary {
	if s == nil || s.Forwards == nil {
		return ForwardSummary{}
	}
	var summary ForwardSummary
	for _, session := range s.Forwards.List() {
		if session.State == kube.ForwardReconnecting {
			summary.Reconnecting++
		} else {
			summary.Active++
		}
	}
	return summary
}
