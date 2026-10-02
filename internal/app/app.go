package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	"github.com/kute-dev/kute/internal/diag"
	"github.com/kute-dev/kute/internal/helmrepo"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/tasks/browse"
	"github.com/kute-dev/kute/internal/tui/tasks/certchain"
	"github.com/kute-dev/kute/internal/tui/tasks/configmapdata"
	"github.com/kute-dev/kute/internal/tui/tasks/cronjobdetail"
	"github.com/kute-dev/kute/internal/tui/tasks/cronjobschedule"
	"github.com/kute-dev/kute/internal/tui/tasks/debugpanel"
	"github.com/kute-dev/kute/internal/tui/tasks/events"
	"github.com/kute-dev/kute/internal/tui/tasks/execpicker"
	"github.com/kute-dev/kute/internal/tui/tasks/fluxdetail"
	"github.com/kute-dev/kute/internal/tui/tasks/fluxtree"
	"github.com/kute-dev/kute/internal/tui/tasks/forwardpicker"
	"github.com/kute-dev/kute/internal/tui/tasks/helmdetail"
	"github.com/kute-dev/kute/internal/tui/tasks/helmhistory"
	"github.com/kute-dev/kute/internal/tui/tasks/jobattempts"
	"github.com/kute-dev/kute/internal/tui/tasks/nodedetail"
	"github.com/kute-dev/kute/internal/tui/tasks/objectdetail"
	"github.com/kute-dev/kute/internal/tui/tasks/overview"
	"github.com/kute-dev/kute/internal/tui/tasks/poddetail"
	"github.com/kute-dev/kute/internal/tui/tasks/podlogs"
	"github.com/kute-dev/kute/internal/tui/tasks/routetable"
	"github.com/kute-dev/kute/internal/tui/tasks/secretdata"
	"github.com/kute-dev/kute/internal/tui/tasks/setup"
	"github.com/kute-dev/kute/internal/tui/tasks/timeline"
	"github.com/kute-dev/kute/internal/tui/tasks/whocan"
	"github.com/kute-dev/kute/internal/tui/tasks/yamlview"
)

// forwardAwareLister dispatches kube.KindForward to the shared
// *kube.ForwardManager and everything else to the underlying cluster
// lister — the seam that lets Forwards flow through browse's normal
// resources.List/goto-palette pipeline as "just another kind" (docs/design
// README.md §13c) without kube.Cluster/fake.Cluster needing to know
// anything about forwarding.
type forwardAwareLister struct {
	resources.RawLister
	forwards *kube.ForwardManager
}

func (l forwardAwareLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if kind == kube.KindForward {
		return l.forwards.ListRaw(), nil
	}
	return l.RawLister.ListRaw(ctx, kind, namespace)
}

// helmAwareLister decodes kube.KindHelmRelease from the already-watched
// Secret cache (docs/design README.md §18a: "browsing needs no helm
// binary") — the same "a kind computed from another kind's own cache, not
// its own informer" shape forwardAwareLister already establishes for
// KindForward, just reading Secrets instead of maintaining its own state.
type helmAwareLister struct {
	resources.RawLister
	// charts answers 18a's LATEST column from the user's own local Helm repo
	// cache. It is read here, at the one point releases are decoded, so the
	// annotation reaches every consumer of the kind without each screen
	// having to know the repo cache exists. nil is a working value (no
	// index, every LATEST unknown) — see helmrepo.Cache.Index.
	charts *helmrepo.Cache
}

// helmSecretLister is implemented by a lister that can hand back just the
// helm.sh/release.v1 Secrets, from a cache holding only those. Optional: a
// lister without it falls back to filtering the shared Secret cache, which
// is correct but means reading every Secret in the namespace to find a
// handful of releases.
type helmSecretLister interface {
	ListHelmReleaseSecrets(ctx context.Context, namespace string) ([]runtime.Object, error)
}

func (l helmAwareLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	if kind != kube.KindHelmRelease {
		return l.RawLister.ListRaw(ctx, kind, namespace)
	}
	secrets, err := l.helmSecrets(ctx, namespace)
	if err != nil {
		return nil, err
	}
	releases := kube.DecodeLatestHelmReleases(secrets)
	index := l.charts.Index()
	// One read of the three rolling caches for the whole list, not one per
	// release — see resources.UnsettledWorkloads.
	unsettled := resources.UnsettledWorkloads(ctx, l.RawLister, namespace)
	out := make([]runtime.Object, len(releases))
	for i, r := range releases {
		// The deployed version is passed in so a chart name served by two
		// unrelated repos resolves to the series this release is actually
		// on, rather than whichever repo happens to number higher.
		if latest, ok := index.LatestFor(r.Chart, r.ChartVersion); ok {
			r = r.WithLatest(latest.Version, latest.Repo, latest.Ambiguous)
		}
		r = r.WithRollout(rolloutPending(r, unsettled))
		out[i] = kube.NewHelmReleaseObject(r)
	}
	return out, nil
}

// rolloutPending reports whether any workload r's own manifest declares is
// still rolling — 18a's "helm says deployed, Kubernetes isn't done" signal.
//
// Annotated here, at the one point releases are decoded, for the same reason
// the chart index is: the answer reaches every consumer of the kind without
// each screen having to know that a release has workloads behind it.
func rolloutPending(r kube.HelmRelease, unsettled map[kube.WorkloadRef]struct{}) bool {
	if len(unsettled) == 0 {
		return false
	}
	for ref := range kube.HelmReleaseWorkloadsSeq(r) {
		if _, ok := unsettled[ref]; ok {
			return true
		}
	}
	return false
}

// ChartIndexStatus reports the state of the local Helm repo cache behind the
// LATEST column, so the 18a strip can caveat a stale or absent one. Another
// optional seam, and like the rest of them it needs the explicit forward
// below plus its compile-time assertion.
func (l helmAwareLister) ChartIndexStatus() helmrepo.Status {
	return l.charts.Index().Status()
}

// Synced forwards to the wrapped lister's own Synced — same reasoning as
// forwardAwareLister.Synced (embedding an interface field only promotes the
// interface's own methods).
func (l helmAwareLister) Synced() bool {
	if sc, ok := l.RawLister.(cacheSyncChecker); ok {
		return sc.Synced()
	}
	return true
}

// Scoped forwards to the wrapped lister's own Scoped, same reasoning as
// Synced — browse's 403 card asks this through the fully-decorated
// sess.Lister, so an unforwarded Scoped would always read false (never
// scoped) and keep suggesting --namespace-scoped to a session already
// running it.
func (l helmAwareLister) Scoped() bool {
	if sc, ok := l.RawLister.(tui.ScopedChecker); ok {
		return sc.Scoped()
	}
	return false
}

// KindSynced answers for the cache a kind is actually served from, which for
// KindHelmRelease is whichever cache the releases actually came from: its
// own filtered Secret informer when the lister has one, otherwise the shared
// Secret cache the fallback read from. Getting this wrong means the Helm
// list asks about a kind nothing backs, hears "nothing to wait for", and
// flashes "no releases" while the real cache is still filling.
func (l helmAwareLister) KindSynced(kind kube.ResourceKind, namespace string) bool {
	kc, ok := l.RawLister.(kindSyncChecker)
	if !ok {
		return true
	}
	if kind == kube.KindHelmRelease && !l.hasHelmReleaseCache() {
		kind = kube.KindSecret
	}
	return kc.KindSynced(kind, namespace)
}

// KindStarted answers for the same cache KindSynced does — for
// KindHelmRelease, whichever one the releases actually come from — so a
// breadth-first reader gated on tui.KindCached asks "is this read free?" of
// the informer it would actually read.
func (l helmAwareLister) KindStarted(kind kube.ResourceKind, namespace string) bool {
	ks, ok := l.RawLister.(tui.KindStartedChecker)
	if !ok {
		return true
	}
	if kind == kube.KindHelmRelease && !l.hasHelmReleaseCache() {
		kind = kube.KindSecret
	}
	return ks.KindStarted(kind, namespace)
}

// KindError answers for the same cache KindSynced does — for
// KindHelmRelease, whichever one the releases actually came from — so a
// screen told "settled" here is told why by the same informer.
func (l helmAwareLister) KindError(kind kube.ResourceKind, namespace string) error {
	kr, ok := l.RawLister.(tui.KindErrorReporter)
	if !ok {
		return nil
	}
	if kind == kube.KindHelmRelease && !l.hasHelmReleaseCache() {
		kind = kube.KindSecret
	}
	return kr.KindError(kind, namespace)
}

// KindForbidden answers for the same cache KindSynced and KindError do. A
// release list served from the shared Secret cache is forbidden exactly when
// Secrets are, so the substitution has to happen here too — otherwise the
// Helm list is told "settled, nothing wrong" about a cache it may not read,
// and renders an empty release list for a namespace it cannot see.
func (l helmAwareLister) KindForbidden(kind kube.ResourceKind, namespace string) error {
	fr, ok := l.RawLister.(tui.KindForbiddenReporter)
	if !ok {
		return nil
	}
	if kind == kube.KindHelmRelease && !l.hasHelmReleaseCache() {
		kind = kube.KindSecret
	}
	return fr.KindForbidden(kind, namespace)
}

// CountLive forwards the server-side count; KindHelmRelease is counted from
// the Secrets that back it. See forwardAwareLister.CountLive for why this
// forward matters rather than being a formality.
func (l helmAwareLister) CountLive(ctx context.Context, kind kube.ResourceKind, namespace string) (int, error) {
	lc, ok := l.RawLister.(liveCounter)
	if !ok {
		return 0, fmt.Errorf("no live counter for %s", kind)
	}
	if kind == kube.KindHelmRelease {
		// No honest cheap answer exists. Counting release Secrets counts
		// revisions, not releases, and aggregating them to releases means
		// decoding every one — which is the read this count is supposed to
		// avoid. Report unknown and let the palette render a dash rather
		// than a confident wrong number.
		return 0, fmt.Errorf("helm releases have no server-side count")
	}
	return lc.CountLive(ctx, kind, namespace)
}

// newSessionLister is the one place the read-side decorator stack is
// composed. Order matters — helmAwareLister has to wrap forwardAwareLister
// so KindForward still short-circuits before the Helm dispatch — and every
// optional seam has to be forwarded through both layers (see the
// compile-time assertions below). Having a single constructor means there's
// one composition for those assertions and the lister tests to speak about,
// rather than five hand-spelled copies that can drift apart.
func newSessionLister(raw resources.RawLister, forwards *kube.ForwardManager, charts *helmrepo.Cache) helmAwareLister {
	return helmAwareLister{
		RawLister: forwardAwareLister{RawLister: raw, forwards: forwards},
		charts:    charts,
	}
}

// ListHelmReleaseSecrets keeps the seam alive one layer further up, so
// anything handed the fully-decorated sess.Lister still reaches the narrow
// cache. See forwardAwareLister's copy for why this is load-bearing.
func (l helmAwareLister) ListHelmReleaseSecrets(ctx context.Context, namespace string) ([]runtime.Object, error) {
	return l.helmSecrets(ctx, namespace)
}

// hasHelmReleaseCache passes the question down; see forwardAwareLister's.
func (l helmAwareLister) hasHelmReleaseCache() bool {
	if p, ok := l.RawLister.(helmCacheProber); ok {
		return p.hasHelmReleaseCache()
	}
	_, ok := l.RawLister.(helmSecretLister)
	return ok
}

// helmSecrets reads the release Secrets from the narrowest source available.
func (l helmAwareLister) helmSecrets(ctx context.Context, namespace string) ([]runtime.Object, error) {
	if hs, ok := l.RawLister.(helmSecretLister); ok {
		return hs.ListHelmReleaseSecrets(ctx, namespace)
	}
	return l.RawLister.ListRaw(ctx, kube.KindSecret, namespace)
}

// cacheSyncChecker and kindSyncChecker mirror browse.CacheSyncChecker and
// browse.KindSyncChecker structurally so this package doesn't need to import
// browse just for the type assertions below.
type cacheSyncChecker interface{ Synced() bool }

type kindSyncChecker interface {
	KindSynced(kind kube.ResourceKind, namespace string) bool
}

// liveCounter mirrors tui.LiveCounter structurally, for the same reason.
type liveCounter interface {
	CountLive(ctx context.Context, kind kube.ResourceKind, namespace string) (int, error)
}

// Synced forwards to the wrapped lister's own Synced, if it has one.
// Embedding resources.RawLister as an interface field only promotes methods
// that interface declares (ListRaw), so *kube.Cluster's Synced silently
// stopped being visible through forwardAwareLister without this — browse's
// listerSynced type-assertion against the wrapper (not the cluster) always
// missed, so a load reply landing before the informer cache's initial sync
// read as a genuinely empty result (10c) instead of "still loading" (15a).
func (l forwardAwareLister) Synced() bool {
	if sc, ok := l.RawLister.(cacheSyncChecker); ok {
		return sc.Synced()
	}
	return true
}

// Scoped forwards to the wrapped lister's own Scoped, for the same reason
// Synced does — see helmAwareLister.Scoped.
func (l forwardAwareLister) Scoped() bool {
	if sc, ok := l.RawLister.(tui.ScopedChecker); ok {
		return sc.Scoped()
	}
	return false
}

// KindSynced forwards per-kind sync state for the same reason Synced
// forwards the aggregate. Forwards are in-process state with no cache to
// wait on, so they are always current.
func (l forwardAwareLister) KindSynced(kind kube.ResourceKind, namespace string) bool {
	if kind == kube.KindForward {
		return true
	}
	if kc, ok := l.RawLister.(kindSyncChecker); ok {
		return kc.KindSynced(kind, namespace)
	}
	return true
}

// KindStarted forwards whether a read would start a new informer. It is
// load-bearing the way CountLive is: unforwarded, tui.KindCached's type
// assertion misses and falls back to KindSynced alone, which vouches for a
// kind nothing has started — and the goto palette's fuzzy corpus then reads
// it, starting that informer on a keystroke. Forwards are in-process state,
// so reading them never starts anything.
func (l forwardAwareLister) KindStarted(kind kube.ResourceKind, namespace string) bool {
	if kind == kube.KindForward {
		return true
	}
	if ks, ok := l.RawLister.(tui.KindStartedChecker); ok {
		return ks.KindStarted(kind, namespace)
	}
	return true
}

// KindError forwards the reason a kind's cache has nothing to show, and is
// load-bearing for the same reason KindSynced is — the two are read as a
// pair. Unforwarded, a screen gets "settled" from the layer below and no
// reason from this one, which is precisely the combination that renders a
// failed read as an empty cluster. Forwards are in-process state that cannot
// fail to load, so they never have one.
func (l forwardAwareLister) KindError(kind kube.ResourceKind, namespace string) error {
	if kind == kube.KindForward {
		return nil
	}
	if kr, ok := l.RawLister.(tui.KindErrorReporter); ok {
		return kr.KindError(kind, namespace)
	}
	return nil
}

// KindForbidden forwards the denial behind an empty cache, and is
// load-bearing for exactly the reason KindError is — it is the third member
// of the set a screen reads before deciding it may say "none". Forwards are
// in-process state with no API server to refuse them, so they never are.
func (l forwardAwareLister) KindForbidden(kind kube.ResourceKind, namespace string) error {
	if kind == kube.KindForward {
		return nil
	}
	if fr, ok := l.RawLister.(tui.KindForbiddenReporter); ok {
		return fr.KindForbidden(kind, namespace)
	}
	return nil
}

// CountLive forwards the server-side count. Forwards are in-process, so
// they're counted from the registry rather than the API.
//
// Forwarding this is load-bearing, not tidy-up: the jump palette asks
// whether its lister can count server-side, and falls back to measuring
// informer caches when it can't. Unforwarded, that fallback fired against a
// real cluster and started an informer per kind on the 'g' keypress —
// exactly the stampede the server-side count exists to avoid.
func (l forwardAwareLister) CountLive(ctx context.Context, kind kube.ResourceKind, namespace string) (int, error) {
	if kind == kube.KindForward {
		return len(l.forwards.ListRaw()), nil
	}
	if lc, ok := l.RawLister.(liveCounter); ok {
		return lc.CountLive(ctx, kind, namespace)
	}
	return 0, fmt.Errorf("no live counter for %s", kind)
}

// ListHelmReleaseSecrets forwards the narrow, server-side-filtered release
// cache, and is load-bearing for the same reason CountLive is. Unforwarded,
// helmAwareLister.helmSecrets type-asserted against *this wrapper*, missed,
// and read every Secret in the namespace through the shared informer
// instead — the exact read internal/kube/helm.go keeps a separate cache to
// avoid — while leaving the release cache cold until the 'h' keypress
// started it, so opening a release's history sat on a spinner through a
// second cluster-wide Secret LIST.
//
// The fallback matters as much as the forward: once a decorator advertises
// this method every caller's type assertion succeeds, so it must never be a
// dead end. Without a narrow cache underneath, the shared Secret cache is
// the documented answer (see helmSecretLister) and this returns it.
func (l forwardAwareLister) ListHelmReleaseSecrets(ctx context.Context, namespace string) ([]runtime.Object, error) {
	if hs, ok := l.RawLister.(helmSecretLister); ok {
		return hs.ListHelmReleaseSecrets(ctx, namespace)
	}
	return l.RawLister.ListRaw(ctx, kube.KindSecret, namespace)
}

// hasHelmReleaseCache reports whether a real filtered release cache is what
// ListHelmReleaseSecrets reads, as opposed to this decorator's shared-Secret
// fallback. KindSynced needs the distinction that the type assertion can no
// longer make for it: the two sources are different informers, and gating a
// loading state on the wrong one is how "no releases" gets claimed about a
// cache that is still filling.
func (l forwardAwareLister) hasHelmReleaseCache() bool {
	_, ok := l.RawLister.(helmSecretLister)
	return ok
}

// helmCacheProber is hasHelmReleaseCache's seam, so the question survives
// one decorator asking the next.
type helmCacheProber interface{ hasHelmReleaseCache() bool }

// Compile-time guarantees that both the informer-backed Cluster and the
// in-memory fake satisfy the seams the screens depend on (--demo
// substitutes the latter behind the same interfaces, mvp-plan.md §0.10).
var (
	_ resources.RawLister          = (*kube.Cluster)(nil)
	_ kube.Mutator                 = (*kube.Cluster)(nil)
	_ browse.MetricsReader         = (*kube.Cluster)(nil)
	_ browse.NodeMetricsReader     = (*kube.Cluster)(nil)
	_ poddetail.EventsReader       = (*kube.Cluster)(nil)
	_ yamlview.YAMLReader          = (*kube.Cluster)(nil)
	_ events.EventsReader          = (*kube.Cluster)(nil)
	_ objectdetail.EventsReader    = (*kube.Cluster)(nil)
	_ timeline.EventsReader        = (*kube.Cluster)(nil)
	_ resources.InstanceCounter    = (*kube.Cluster)(nil)
	_ whocan.WhoCanReader          = (*kube.Cluster)(nil)
	_ debugpanel.AccessReviewer    = (*kube.Cluster)(nil)
	_ overview.NodeMetricsReader   = (*kube.Cluster)(nil)
	_ nodedetail.NodeDiskReader    = (*kube.Cluster)(nil)
	_ browse.KindSyncChecker       = (*kube.Cluster)(nil)
	_ tui.KindErrorReporter        = (*kube.Cluster)(nil)
	_ tui.KindStartedChecker       = (*kube.Cluster)(nil)
	_ tui.KindForbiddenReporter    = (*kube.Cluster)(nil)
	_ tui.ScopedChecker            = (*kube.Cluster)(nil)
	_ tui.LiveCounter              = (*kube.Cluster)(nil)
	_ helmhistory.HelmSecretLister = (*kube.Cluster)(nil)
	_ helmdetail.HelmSecretLister  = (*kube.Cluster)(nil)

	// The two lister decorators must satisfy every optional seam the
	// cluster does. Embedding resources.RawLister as an interface field
	// promotes only ListRaw, so each of these needs an explicit forward and
	// a silently missing one is invisible at runtime: the consumer just
	// type-asserts, misses, and takes its fallback path. That is exactly
	// how CountLive went unforwarded — the jump palette quietly fell back
	// to counting informer caches, which started an informer per kind on
	// the 'g' keypress and hung the app for a minute on a real cluster.
	// ListHelmReleaseSecrets went the same way: it predated this block and
	// was never added to it, so the Helm list read every Secret in the
	// namespace and left the release cache for the 'h' key to start cold.
	// Assert them here so the next omission is a compile error.
	_ browse.KindSyncChecker       = forwardAwareLister{}
	_ browse.CacheSyncChecker      = forwardAwareLister{}
	_ tui.KindErrorReporter        = forwardAwareLister{}
	_ tui.KindStartedChecker       = forwardAwareLister{}
	_ tui.KindForbiddenReporter    = forwardAwareLister{}
	_ tui.ScopedChecker            = forwardAwareLister{}
	_ tui.LiveCounter              = forwardAwareLister{}
	_ helmhistory.HelmSecretLister = forwardAwareLister{}
	_ helmdetail.HelmSecretLister  = forwardAwareLister{}
	_ browse.KindSyncChecker       = helmAwareLister{}
	_ browse.CacheSyncChecker      = helmAwareLister{}
	_ tui.KindErrorReporter        = helmAwareLister{}
	_ tui.KindStartedChecker       = helmAwareLister{}
	_ tui.KindForbiddenReporter    = helmAwareLister{}
	_ tui.ScopedChecker            = helmAwareLister{}
	_ tui.LiveCounter              = helmAwareLister{}
	_ helmhistory.HelmSecretLister = helmAwareLister{}
	_ helmdetail.HelmSecretLister  = helmAwareLister{}
	// Only the outermost decorator can answer this one — nothing below it
	// holds a chart index — but it is the lister screens actually assert
	// against, so this is the assertion that matters.
	_ browse.ChartIndexReporter    = helmAwareLister{}
	_ resources.RawLister          = (*fake.Cluster)(nil)
	_ kube.Mutator                 = (*fake.Cluster)(nil)
	_ browse.MetricsReader         = (*fake.Cluster)(nil)
	_ browse.NodeMetricsReader     = (*fake.Cluster)(nil)
	_ poddetail.EventsReader       = (*fake.Cluster)(nil)
	_ yamlview.YAMLReader          = (*fake.Cluster)(nil)
	_ events.EventsReader          = (*fake.Cluster)(nil)
	_ objectdetail.EventsReader    = (*fake.Cluster)(nil)
	_ timeline.EventsReader        = (*fake.Cluster)(nil)
	_ resources.InstanceCounter    = (*fake.Cluster)(nil)
	_ whocan.WhoCanReader          = (*fake.Cluster)(nil)
	_ debugpanel.AccessReviewer    = (*fake.Cluster)(nil)
	_ overview.NodeMetricsReader   = (*fake.Cluster)(nil)
	_ nodedetail.NodeDiskReader    = (*fake.Cluster)(nil)
	_ browse.KindSyncChecker       = (*fake.Cluster)(nil)
	_ tui.KindErrorReporter        = (*fake.Cluster)(nil)
	_ tui.KindStartedChecker       = (*fake.Cluster)(nil)
	_ tui.KindForbiddenReporter    = (*fake.Cluster)(nil)
	_ helmhistory.HelmSecretLister = (*fake.Cluster)(nil)
	_ helmdetail.HelmSecretLister  = (*fake.Cluster)(nil)
	// This block has no tui.ScopedChecker or tui.LiveCounter assertion for
	// fake.Cluster. The fake has no informers to scope: SwitchNamespace only
	// changes which objects ListRaw filters to (see fake.go's own field
	// comment). The fake also has no server to count against. Scoped() and
	// CountLive() would have nothing honest to report. --demo never accepts
	// --namespace-scoped. Scoped-mode UI behavior — the 403 card's
	// ScopedChecker read — is already tested against purpose-built lister
	// doubles in browse/hints_test.go (scopedRecordingLister), not against
	// this fake.
)

// seams is what browse, nodedetail, poddetail, yamlview, events, and
// objectdetail need from either a real or fake cluster, satisfied directly
// by both *kube.Cluster and *fake.Cluster.
type seams interface {
	resources.RawLister
	browse.MetricsReader
	browse.NodeMetricsReader
	nodedetail.NodeDiskReader
	kube.Mutator
	poddetail.EventsReader
	yamlview.YAMLReader
	events.EventsReader
	objectdetail.EventsReader
	timeline.EventsReader
	whocan.WhoCanReader
	debugpanel.AccessReviewer
	certchain.EventsReader
	cronjobschedule.CapabilityReader
}

// NewModel builds the root model. It's rooted at tasks/browse when a
// cluster (real or --demo) is available, or tasks/setup's 10b "no
// kubeconfig" state otherwise (mvp-plan.md Phase 4) — a real cluster that
// exists but hasn't yet answered (4c) is detected later, once the program
// is running, by the root shell's neverConnected watch (see
// buildSetupFactory/tui.Model.WithRootFactories). It returns the real
// Cluster (non-nil unless cfg.Demo or the cluster is unreachable) and the
// demo Cluster (non-nil only in demo mode) so Run can drive whichever one
// is active.
func NewModel(cfg Config) (tui.Model, *kube.Cluster, *fake.Cluster) {
	sess, cluster, buildErr := BuildSession(cfg)
	checker := buildChecker(cfg)

	switch {
	case cluster != nil:
		sess.Forwards = kube.NewForwardManager()
		sess.DebugCopies = kube.NewDebugCopyRegistry()
		sess.Lister = newSessionLister(cluster, sess.Forwards, sess.Charts)
		sess.Metrics = cluster
		// Snapshot before buildBrowseTask: browse.New's own KindEvent
		// carve-out (its doc comment) resets Session.Location.Kind to Pod
		// as a side effect of building the underlying browse task below.
		restoreNamespace, restoreToEvents := sess.Location.Namespace, sess.Location.Kind == kube.KindEvent
		model := tui.NewWithSession(buildBrowseTask(cfg, sess, cluster), sess).
			WithRootFactories(buildSetupFactory(cfg, sess, cluster), buildBrowseFactory(cfg, sess, cluster)).
			WithUpdatePanel(buildUpdateFactory(sess, checker)).
			WithKeycast(cfg.Keycast)
		if restoreToEvents {
			// A persisted Location.Kind of "Event" (quitting from 9b) should
			// land back on 9b, not the Pod-fallback browse task built above —
			// pushed on top of it, so esc walks back to browse exactly like
			// live navigation to 9b would have.
			openEvents := openEventsFunc(sess, cluster, openYAMLFunc(sess, cluster))
			eventsModel, _ := openEvents(restoreNamespace, tui.DefaultWidth, tui.DefaultHeight)
			if eventsTask, ok := eventsModel.(tui.Task); ok {
				model = model.WithInitialPush(eventsTask)
			}
		}
		return model, cluster, nil

	case cfg.Demo:
		demoCluster := fake.NewDemo()
		clusterName, namespace := demoCluster.CurrentContext(), demoCluster.CurrentNamespace()
		switch {
		case cfg.ScopeNamespace != "":
			// --namespace-scoped selects the namespace the same way -n does
			// in demo mode (BuildSession's own doc comment on this — the
			// fake has no informers to scope). Checked first, same
			// precedence -n/--namespace gets on a real cluster: without
			// this, BuildSession's correct sess.Location.Namespace gets
			// silently overwritten by the demo's own default namespace
			// below.
			namespace = cfg.ScopeNamespace
		case cfg.Namespace != "":
			// -n/--namespace outranks the fake cluster's own default here for
			// the same reason it does on a real one: an explicit flag wins.
			namespace = cfg.Namespace
		}
		sess.Location.Context = clusterName
		sess.Location.Namespace = namespace
		sess.Registry, sess.Groups = resources.BuildDiscoveredRegistry(demoCluster.DiscoveredKinds(), demoCluster)
		sess.Forwards = kube.NewForwardManager()
		sess.DebugCopies = kube.NewDebugCopyRegistry()
		// The demo's chart versions are fixtures too — reading the running
		// machine's real ~/.cache/helm would make --demo render differently
		// on every laptop, and mostly render "–".
		sess.Charts = fake.DemoChartIndex()
		lister := newSessionLister(demoCluster, sess.Forwards, sess.Charts)
		sess.Lister = lister
		sess.Metrics = demoCluster
		b := buildDemoBrowseTask(sess, demoCluster, clusterName, namespace)
		// No restore-to-9b carve-out here (unlike the real-cluster branch
		// above): BuildSession returns before ever reading PerContext for
		// cfg.Demo (session.go), so Session.Location.Kind is always "" at
		// this point — Location.Kind == KindEvent can't happen in demo mode.
		//
		// WithRootFactories(nil, ...): buildSetup stays nil (--demo never
		// needs 4c's reconnect flow), but routeGoto (internal/tui/model.go)
		// still needs a buildBrowse factory to push a fresh browse view for
		// a jump fired from a pushed screen (poddetail's RELATED, events'/
		// timeline's ↵, ...) — without it those jumps would silently no-op
		// in --demo, the one path WithRootFactories was never wired for
		// before this.
		model := tui.NewWithSession(b, sess).
			WithRootFactories(nil, buildDemoBrowseFactory(sess, demoCluster, clusterName, namespace)).
			WithUpdatePanel(buildUpdateFactory(sess, checker)).
			WithKeycast(cfg.Keycast)
		return model, nil, demoCluster

	default:
		// No reachable cluster and not --demo: root at tasks/setup's 10b
		// "no kubeconfig" state, letting the user fix $KUBECONFIG/point at
		// a different path and retry in place.
		sess.Location.Context = cfg.Cluster
		s := setup.New(setup.Config{
			Session:        sess,
			State:          setup.NoConfig,
			Err:            buildErr,
			KubeconfigPath: kubeconfigPathOrEmpty(),
			Reconnect:      func(path string) tea.Cmd { return attemptReconnect(cfg, sess, path) },
		})
		model := tui.NewWithSession(&s, sess).WithUpdatePanel(buildUpdateFactory(sess, checker)).WithKeycast(cfg.Keycast)
		return model, nil, nil
	}
}

// buildBrowseTask constructs browse against cluster, per repo convention —
// shared by NewModel's initial wiring and attemptReconnect's post-retry
// rebuild so the two never drift.
func buildBrowseTask(cfg Config, sess *tui.Session, cluster *kube.Cluster) *browse.Model {
	streamer := liveClusterLogStreamer{cluster: cluster}
	clusterName, namespace := cluster.Context.ClusterName, cluster.Context.Namespace
	lister := newSessionLister(cluster, sess.Forwards, sess.Charts)
	openLogs := openLogsFunc(sess, cluster, streamer, clusterName, namespace)
	openYAML := openYAMLFunc(sess, cluster)
	openDebug := openDebugFunc(sess, cluster, false)
	openExecDebug := openExecDebugFunc(sess, cluster, false)
	openNodeDebug := openNodeDebugFunc(sess, cluster, false)
	openExec := openExecFunc(sess, kubectlShellDetector{cluster: cluster}, openExecDebug, false)
	openForward := openForwardFunc(sess, lister, cluster)
	openPodDetail := openPodDetailFunc(sess, cluster, openLogs, openYAML, openExec, openForward, kubectlShellDetector{cluster: cluster}, openDebug, false)
	openNodeDetail := openNodeDetailFunc(sess, cluster, openPodDetail, openLogs, openYAML, openExec, openDebug, openForward, openNodeDebug)
	openEvents := openEventsFunc(sess, cluster, openYAML)
	openTimeline := openTimelineFunc(sess, cluster, openEvents)
	openObjectEvents := openObjectEventsFunc(sess, cluster, openYAML)
	openObjectTimeline := openObjectTimelineFunc(sess, cluster, openObjectEvents)
	openCronJobSchedule := openCronJobScheduleFunc(sess, cluster)
	b := browse.New(browse.Config{
		Session:             sess,
		Lister:              lister,
		Metrics:             cluster,
		NodeMetrics:         cluster,
		Mutator:             cluster,
		OpenLogs:            openLogs,
		OpenNodeDetail:      openNodeDetail,
		OpenPodDetail:       openPodDetail,
		OpenYAML:            openYAML,
		OpenEvents:          openEvents,
		OpenTimeline:        openTimeline,
		OpenObjectTimeline:  browse.OpenObjectTimelineFunc(openObjectTimeline),
		OpenExec:            browse.OpenExecFunc(openExec),
		OpenDebug:           browse.OpenDebugFunc(openDebug),
		OpenNodeDebug:       browse.OpenNodeDebugFunc(openNodeDebug),
		Shells:              kubectlShellDetector{cluster: cluster},
		OpenForward:         openForward,
		OpenObjectDetail:    openObjectDetailFunc(sess, cluster, openYAML),
		OpenFluxDetail:      openFluxDetailFunc(sess, cluster, openYAML),
		OpenCertChain:       openCertChainFunc(sess, cluster, openYAML),
		OpenFluxTree:        openFluxTreeFunc(sess, cluster, openFluxDetailFunc(sess, cluster, openYAML), openObjectDetailFunc(sess, cluster, openYAML)),
		OpenRouteTable:      openRouteTableFunc(sess, cluster, openYAML),
		OpenWhoCan:          openWhoCanFunc(sess, cluster),
		OpenHelmHistory:     openHelmHistoryFunc(sess, cluster),
		OpenHelmDetail:      openHelmDetailFunc(sess, lister, cluster, openObjectEvents),
		OpenHelmValues:      openHelmValuesFunc(sess),
		OpenSecretData:      openSecretDataFunc(sess, cluster),
		OpenConfigMapData:   openConfigMapDataFunc(sess, cluster),
		OpenCronJobSchedule: openCronJobSchedule,
		OpenCronJobDetail:   openCronJobDetailFunc(sess, cluster, openLogs, openYAML, openCronJobSchedule, cluster.Context.UserName),
		OpenJobAttempts:     openJobAttemptsFunc(sess, cluster, openPodDetail, openLogs, openYAML, cluster.Context.UserName),
		OpenOverview:        openOverviewFunc(sess, lister, cluster, openNodeDetail, openTimeline, openEvents),
		Forwards:            sess.Forwards,
		Retrier:             cluster,
		CurrentUser:         cluster.Context.UserName,
		Demo:                false,
	})
	return &b
}

// buildSetupFactory returns the closure tui.Model calls to build 4c's
// unreachable-at-launch screen for cluster — installed via
// WithRootFactories/ReplaceRootMsg since tui itself can't import
// tasks/setup (import cycle: setup, like every task, imports tui).
func buildSetupFactory(cfg Config, sess *tui.Session, cluster *kube.Cluster) func(kube.ConnState) tui.Task {
	return func(conn kube.ConnState) tui.Task {
		s := setup.New(setup.Config{
			Session:         sess,
			State:           setup.Unreachable,
			ClusterName:     cluster.Context.ContextName,
			Conn:            conn,
			OtherContexts:   otherContexts(cluster.Context.ContextName),
			KubeconfigPath:  kubeconfigPathOrEmpty(),
			RetryNow:        cluster.RetryNow,
			Reconnect:       func(path string) tea.Cmd { return attemptReconnect(cfg, sess, path) },
			SwitchToContext: func(name string) tea.Cmd { return attemptSwitchContext(cfg, sess, name) },
		})
		return &s
	}
}

// buildBrowseFactory returns the closure tui.Model calls to swap back to
// browse once a never-yet-connected cluster reports its first Connected
// state — same import-cycle reasoning as buildSetupFactory.
func buildBrowseFactory(cfg Config, sess *tui.Session, cluster *kube.Cluster) func() tui.Task {
	return func() tui.Task { return buildBrowseTask(cfg, sess, cluster) }
}

// buildDemoBrowseTask constructs browse against demoCluster, per repo
// convention — shared by NewModel's initial --demo wiring and
// buildDemoBrowseFactory's rebuild so the two never drift, the same
// relationship buildBrowseTask/buildBrowseFactory have for a real cluster.
// clusterName/namespace are threaded through explicitly (rather than
// re-derived from demoCluster) since namespace may already carry -n's
// override by the time NewModel's demo branch calls this.
func buildDemoBrowseTask(sess *tui.Session, demoCluster *fake.Cluster, clusterName, namespace string) *browse.Model {
	lister := newSessionLister(demoCluster, sess.Forwards, sess.Charts)
	openLogs := openLogsFunc(sess, demoCluster, demoCluster, clusterName, namespace)
	openYAML := openYAMLFunc(sess, demoCluster)
	openDebug := openDebugFunc(sess, demoCluster, true)
	openExecDebug := openExecDebugFunc(sess, demoCluster, true)
	openNodeDebug := openNodeDebugFunc(sess, demoCluster, true)
	openExec := openExecFunc(sess, demoCluster, openExecDebug, true)
	openForward := openForwardFuncDemo(sess, lister, sess.Forwards, demoCluster)
	openPodDetail := openPodDetailFunc(sess, demoCluster, openLogs, openYAML, openExec, openForward, demoCluster, openDebug, true)
	openNodeDetail := openNodeDetailFunc(sess, demoCluster, openPodDetail, openLogs, openYAML, openExec, openDebug, openForward, openNodeDebug)
	openEvents := openEventsFunc(sess, demoCluster, openYAML)
	openTimeline := openTimelineFunc(sess, demoCluster, openEvents)
	openObjectEvents := openObjectEventsFunc(sess, demoCluster, openYAML)
	openObjectTimeline := openObjectTimelineFunc(sess, demoCluster, openObjectEvents)
	openCronJobSchedule := openCronJobScheduleFunc(sess, demoCluster)
	b := browse.New(browse.Config{
		Session:             sess,
		Lister:              lister,
		Metrics:             demoCluster,
		NodeMetrics:         demoCluster,
		Mutator:             demoCluster,
		OpenLogs:            openLogs,
		OpenNodeDetail:      openNodeDetail,
		OpenPodDetail:       openPodDetail,
		OpenYAML:            openYAML,
		OpenEvents:          openEvents,
		OpenTimeline:        openTimeline,
		OpenObjectTimeline:  browse.OpenObjectTimelineFunc(openObjectTimeline),
		OpenExec:            browse.OpenExecFunc(openExec),
		OpenDebug:           browse.OpenDebugFunc(openDebug),
		OpenNodeDebug:       browse.OpenNodeDebugFunc(openNodeDebug),
		Shells:              demoCluster,
		OpenForward:         openForward,
		OpenObjectDetail:    openObjectDetailFunc(sess, demoCluster, openYAML),
		OpenFluxDetail:      openFluxDetailFunc(sess, demoCluster, openYAML),
		OpenCertChain:       openCertChainFunc(sess, demoCluster, openYAML),
		OpenFluxTree:        openFluxTreeFunc(sess, demoCluster, openFluxDetailFunc(sess, demoCluster, openYAML), openObjectDetailFunc(sess, demoCluster, openYAML)),
		OpenRouteTable:      openRouteTableFunc(sess, demoCluster, openYAML),
		OpenWhoCan:          openWhoCanFunc(sess, demoCluster),
		OpenHelmHistory:     openHelmHistoryFunc(sess, demoCluster),
		OpenHelmDetail:      openHelmDetailFunc(sess, lister, demoCluster, openObjectEvents),
		OpenHelmValues:      openHelmValuesFunc(sess),
		OpenSecretData:      openSecretDataFunc(sess, demoCluster),
		OpenConfigMapData:   openConfigMapDataFunc(sess, demoCluster),
		OpenCronJobSchedule: openCronJobSchedule,
		OpenCronJobDetail:   openCronJobDetailFunc(sess, demoCluster, openLogs, openYAML, openCronJobSchedule, demoCluster.CurrentUser()),
		OpenJobAttempts:     openJobAttemptsFunc(sess, demoCluster, openPodDetail, openLogs, openYAML, demoCluster.CurrentUser()),
		OpenOverview:        openOverviewFunc(sess, lister, demoCluster, openNodeDetail, openTimeline, openEvents),
		Forwards:            sess.Forwards,
		Retrier:             demoCluster,
		CurrentUser:         demoCluster.CurrentUser(),
		Demo:                true,
	})
	return &b
}

// buildDemoBrowseFactory mirrors buildBrowseFactory for --demo.
func buildDemoBrowseFactory(sess *tui.Session, demoCluster *fake.Cluster, clusterName, namespace string) func() tui.Task {
	return func() tui.Task { return buildDemoBrowseTask(sess, demoCluster, clusterName, namespace) }
}

// attemptReconnect is tasks/setup's Reconnect hook (10b's 'r'/'k', 4c's
// 'e'): optionally repoints the kubeconfig and rebuilds the cluster through
// rebuildCluster, resolving the context the way launch does (--context, else
// the most recent one the file still has) rather than the file's own
// current-context, and reopening on the same namespace/kind restore and -n.
func attemptReconnect(cfg Config, sess *tui.Session, path string) tea.Cmd {
	// Snapshotted here, on the Update loop: the Cmd must not read Session.
	recents := state.State{RecentContexts: slices.Clone(sess.State.RecentContexts)}
	return rebuildCluster(cfg, sess, path, true, func() (*kube.Cluster, error) {
		if path != "" {
			// The override every kubeconfig read resolves through. Setting
			// $KUBECONFIG instead did nothing under --kubeconfig, which the
			// override outranks.
			kube.SetKubeconfigPath(path)
		}
		return kube.NewClusterForContext(startupContext(cfg, recents))
	})
}

// attemptSwitchContext is 4c's SWITCH CONTEXT list's '↵' (docs/design
// README.md §4c: "↵ connect to selected") — the same shape as
// attemptReconnect, but pinned to a named kubeconfig context via
// kube.NewClusterForContext instead of re-resolving $KUBECONFIG's own
// current-context, so selecting a reachable sibling context actually
// switches to it rather than retrying the same failing one.
func attemptSwitchContext(cfg Config, sess *tui.Session, contextName string) tea.Cmd {
	return rebuildCluster(cfg, sess, "", false, func() (*kube.Cluster, error) {
		return kube.NewClusterForContext(contextName)
	})
}

// rebuildCluster builds a fresh cluster with build, and only then stops the
// existing one (so its informer goroutines don't leak). On success it starts
// that cluster's informers (bounded, like 7a's SwitchContext) and hands back
// a tui.ReplaceRootMsg carrying the new cluster's event channels — the root
// shell forwards those via tui.WatchCluster without needing a *tea.Program
// reference, since this Cmd runs long after RunWithConfig's own
// forwardEvents goroutine (bound to whatever cluster existed at process
// start) was already launched.
//
// The Cmd touches no Session field but the mutex-guarded context: the root
// keeps reading sess.Cluster, Location and the rest while it runs (every
// ConnStateMsg, the c palette 4c leaves open), so every Session write — and
// the browse build that reads them — happens in ReplaceRootMsg.Commit, on
// the Update loop.
//
// kubeconfig, when set, is the path build pointed kube at, recorded on the
// Session for the kubectl commands kute runs. relaunch applies -n on top of
// the restored location, as launch does; a switch to a named context
// doesn't, as 7a's doesn't.
func rebuildCluster(cfg Config, sess *tui.Session, kubeconfig string, relaunch bool, build func() (*kube.Cluster, error)) tea.Cmd {
	old := sess.Cluster // captured on the Update loop, before the Cmd runs
	return func() tea.Msg {
		// Built before the old cluster is touched: a build that fails (a
		// broken context entry, a missing cert file) leaves 4c up with r
		// bound to the old cluster's RetryNow, which only works while that
		// cluster's health loop is still running.
		cluster, err := build()
		if err != nil {
			return setup.RetryFailedMsg{Err: err}
		}
		// A scoped session that reconnects must stay scoped — otherwise a
		// namespace-bound identity's fresh cluster launches cluster-wide and
		// every informer goes back to the denied cluster-wide LIST the flag
		// exists to avoid (docs/lazy-informers.md §5.6). Scoped mode is
		// session-wide and survives a context switch too (§5.6's Decisions).
		// Same slot BuildSession gives it, before a single informer starts.
		if cfg.ScopeNamespace != "" {
			cluster.SetNamespaceScope(cfg.ScopeNamespace)
		}
		// Same reasoning as tui.switchContextCmd's reset, and for the same
		// window: cluster.Start below blocks while the new caches sync, and
		// any read still running against the cluster just stopped would
		// otherwise land in a screen that has moved on.
		sess.ResetClusterContext()
		if old != nil {
			old.Stop()
		}

		ctx, cancel := context.WithTimeout(sess.Context(), reconnectStartTimeout)
		defer cancel()
		_ = cluster.Start(ctx) // best-effort: an unreachable cluster still hands back ReplaceRootMsg; 4c's neverConnected watch takes over from there.

		return tui.ReplaceRootMsg{
			Commit: func() tui.Task {
				if old != nil && old != cluster {
					// Stop is idempotent; a second one undoes a 7a switch
					// that revived the old cluster while this rebuild ran.
					old.Stop()
				}
				if sess.Forwards == nil {
					sess.Forwards = kube.NewForwardManager()
				}
				if sess.DebugCopies == nil {
					sess.DebugCopies = kube.NewDebugCopyRegistry()
				}
				if kubeconfig != "" {
					sess.Kubeconfig = kubeconfig
				}
				sess.Cluster = cluster
				sess.Lister = newSessionLister(cluster, sess.Forwards, sess.Charts)
				sess.Metrics = cluster
				sess.ClearPendingKind()
				sess.Location = restoredLocation(sess.State, cluster.Context)
				if relaunch && cfg.Namespace != "" {
					sess.Location.Namespace = cfg.Namespace
				}
				if cfg.ScopeNamespace != "" {
					sess.Location.Namespace = cfg.ScopeNamespace
				}
				// cluster.Start above already ran discovery synchronously
				// (folded in, best-effort), so DiscoveredKinds is ready by
				// now — no need for the async CRDsDiscoveredMsg hop this
				// path's initial-connect counterpart (RunWithConfig) requires.
				sess.Registry, sess.Groups = resources.BuildDiscoveredRegistry(cluster.DiscoveredKinds(), cluster)
				return buildBrowseTask(cfg, sess, cluster)
			},
			Events:      cluster.Events(),
			Conn:        cluster.ConnEvents(),
			BuildSetup:  buildSetupFactory(cfg, sess, cluster),
			BuildBrowse: buildBrowseFactory(cfg, sess, cluster),
		}
	}
}

// reconnectStartTimeout bounds attemptReconnect's initial cluster.Start
// (client build + informer cache sync) so an unreachable target doesn't
// hang the retry indefinitely — mirrors tui/context.go's
// switchContextTimeout for the same reason.
const reconnectStartTimeout = 15 * time.Second

// otherContexts lists kubeconfig context names other than current, for 4c's
// SWITCH CONTEXT preview.
func otherContexts(current string) []string {
	names, _, err := kube.AvailableContexts()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != current {
			out = append(out, n)
		}
	}
	return out
}

// kubeconfigPathOrEmpty is kube.KubeconfigPath without the bool — setup
// just wants a display string, empty when it can't be determined.
func kubeconfigPathOrEmpty() string {
	if p, ok := kube.KubeconfigPath(); ok {
		return p
	}
	return ""
}

// openNodeDetailFunc pushes tasks/nodedetail (11b) for a node, wiring it
// against the same seams as browse. nodedetail's pod rows now open a real
// poddetail (5a) via openPodDetail rather than the log-stream stand-in
// nodedetail's own exit notes flagged — as a single-pod handoff (siblings
// = [pod]), so poddetail's j/k is inert from this entry point; wiring
// real prev/next through nodedetail's own pod table is a natural follow-up,
// not required here.
func openNodeDetailFunc(sess *tui.Session, active seams, openPodDetail browse.OpenPodDetailFunc, openLogs browse.OpenLogsFunc, openYAML browse.OpenYAMLFunc, openExec func(namespace, name string, containers []kube.ContainerInfo, width, height int) (tea.Model, tea.Cmd), openDebug func(namespace, name string, containers []kube.ContainerInfo, podPhase string, waiting bool, width, height int) (tea.Model, tea.Cmd), openForward browse.OpenForwardFunc, openNodeDebug func(name string, podCount, width, height int) (tea.Model, tea.Cmd)) browse.OpenNodeDetailFunc {
	openPod := func(pod kube.Pod, width, height int) (tea.Model, tea.Cmd) {
		return openPodDetail(pod, []browse.PodSiblingRef{{Namespace: pod.Namespace, Name: pod.Name}}, 0, width, height)
	}
	openObjectEvents := openObjectEventsFunc(sess, active, openYAML)
	openObjectTimeline := openObjectTimelineFunc(sess, active, openObjectEvents)
	return func(nodeName string, width, height int) (tea.Model, tea.Cmd) {
		nd := nodedetail.New(nodedetail.Config{
			Session:       sess,
			Lister:        active,
			Metrics:       active,
			NodeMetrics:   active,
			NodeDisk:      active,
			Mutator:       active,
			OpenPod:       nodedetail.OpenPodFunc(openPod),
			OpenLogs:      nodedetail.OpenLogsFunc(openLogs),
			OpenExec:      nodedetail.OpenExecFunc(openExec),
			OpenDebug:     nodedetail.OpenDebugFunc(openDebug),
			OpenYAML:      nodedetail.OpenYAMLFunc(openYAML),
			OpenEvents:    nodedetail.OpenEventsFunc(openObjectEvents),
			OpenTimeline:  nodedetail.OpenTimelineFunc(openObjectTimeline),
			OpenForward:   nodedetail.OpenForwardFunc(openForward),
			OpenNodeDebug: nodedetail.OpenNodeDebugFunc(openNodeDebug),
			NodeName:      nodeName,
		})
		nd.SetSize(width, height)
		return &nd, nd.Init()
	}
}

// openPodDetailFunc pushes tasks/poddetail (5a) for a pod, wiring it against
// the same seams active already satisfies for browse/nodedetail. shells/
// openDebug wire §41a/§41b/§41c's shell pre-check (debug.go). The pushed
// debug panel owns the authoritative access review and who-can handoff.
func openPodDetailFunc(sess *tui.Session, active seams, openLogs browse.OpenLogsFunc, openYAML browse.OpenYAMLFunc, openExec func(namespace, name string, containers []kube.ContainerInfo, width, height int) (tea.Model, tea.Cmd), openForward browse.OpenForwardFunc, shells poddetail.ShellDetector, openDebug func(namespace, name string, containers []kube.ContainerInfo, podPhase string, waiting bool, width, height int) (tea.Model, tea.Cmd), demo bool) browse.OpenPodDetailFunc {
	openObjectEvents := openObjectEventsFunc(sess, active, openYAML)
	openObjectTimeline := openObjectTimelineFunc(sess, active, openObjectEvents)
	return func(pod kube.Pod, siblings []browse.PodSiblingRef, index int, width, height int) (tea.Model, tea.Cmd) {
		refs := make([]poddetail.SiblingRef, len(siblings))
		for i, s := range siblings {
			refs[i] = poddetail.SiblingRef{Namespace: s.Namespace, Name: s.Name}
		}
		pd := poddetail.New(poddetail.Config{
			Session:      sess,
			Lister:       active,
			Metrics:      active,
			Events:       active,
			Shells:       shells,
			OpenDebug:    poddetail.OpenDebugFunc(openDebug),
			Mutator:      active,
			OpenLogs:     poddetail.OpenLogsFunc(openLogs),
			OpenYAML:     poddetail.OpenYAMLFunc(openYAML),
			OpenEvents:   poddetail.OpenEventsFunc(openObjectEvents),
			OpenTimeline: poddetail.OpenTimelineFunc(openObjectTimeline),
			OpenExec:     poddetail.OpenExecFunc(openExec),
			OpenForward:  poddetail.OpenForwardFunc(openForward),
			// RELATED's Secret/ConfigMap entries land on their Data views.
			OpenSecretData:    poddetail.OpenSecretDataFunc(openSecretDataFunc(sess, active)),
			OpenConfigMapData: poddetail.OpenConfigMapDataFunc(openConfigMapDataFunc(sess, active)),
			// RELATED's Flux reconciler entry lands on its 31a inventory.
			OpenFluxDetail: poddetail.OpenFluxDetailFunc(openFluxDetailFunc(sess, active, openYAML)),
			Namespace:      pod.Namespace,
			Name:           pod.Name,
			Siblings:       refs,
			SiblingIndex:   index,
			Demo:           demo,
		})
		pd.SetSize(width, height)
		return &pd, pd.Init()
	}
}

// openObjectDetailFunc pushes tasks/objectdetail (14d) for a discovered CRD
// kind's row, wiring it against the same seams active already satisfies for
// browse/nodedetail/poddetail.
func openObjectDetailFunc(sess *tui.Session, active seams, openYAML browse.OpenYAMLFunc) browse.OpenObjectDetailFunc {
	openObjectEvents := openObjectEventsFunc(sess, active, openYAML)
	return func(kind kube.ResourceKind, namespace, name string, siblings []browse.ObjectSiblingRef, index, width, height int) (tea.Model, tea.Cmd) {
		refs := make([]objectdetail.SiblingRef, len(siblings))
		for i, s := range siblings {
			refs[i] = objectdetail.SiblingRef{Namespace: s.Namespace, Name: s.Name}
		}
		od := objectdetail.New(objectdetail.Config{
			Session:      sess,
			Lister:       active,
			Events:       active,
			Mutator:      active,
			OpenYAML:     objectdetail.OpenYAMLFunc(openYAML),
			OpenEvents:   objectdetail.OpenEventsFunc(openObjectEvents),
			Kind:         kind,
			Namespace:    namespace,
			Name:         name,
			Siblings:     refs,
			SiblingIndex: index,
		})
		od.SetSize(width, height)
		return &od, od.Init()
	}
}

// openFluxDetailFunc pushes tasks/fluxdetail (§31a) for a Flux reconciler
// row — active alone satisfies every seam it declares.
func openFluxDetailFunc(sess *tui.Session, active seams, openYAML browse.OpenYAMLFunc) browse.OpenFluxDetailFunc {
	return func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		fd := fluxdetail.New(fluxdetail.Config{
			Session:   sess,
			Lister:    active,
			Mutator:   active,
			OpenYAML:  fluxdetail.OpenYAMLFunc(openYAML),
			Kind:      kind,
			Namespace: namespace,
			Name:      name,
		})
		fd.SetSize(width, height)
		return &fd, fd.Init()
	}
}

// openCertChainFunc pushes tasks/certchain (§35a) for a Certificate row —
// active alone satisfies every seam it declares, same shape as
// openFluxDetailFunc.
func openCertChainFunc(sess *tui.Session, active seams, openYAML browse.OpenYAMLFunc) browse.OpenCertChainFunc {
	openObjectEvents := openObjectEventsFunc(sess, active, openYAML)
	return func(namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		cc := certchain.New(certchain.Config{
			Session:    sess,
			Lister:     active,
			Mutator:    active,
			Events:     active,
			OpenYAML:   certchain.OpenYAMLFunc(openYAML),
			OpenEvents: certchain.OpenEventsFunc(openObjectEvents),
			Namespace:  namespace,
			Name:       name,
		})
		cc.SetSize(width, height)
		return &cc, cc.Init()
	}
}

// openFluxTreeFunc pushes tasks/fluxtree (§30b) — the `g "flux"` source →
// reconciler tree. Cluster-wide and row-less like 19a's own opener, and
// reusing openFluxDetail so ↵ on a reconciler lands on exactly the §31a
// screen browse's own ↵ opens.
func openFluxTreeFunc(sess *tui.Session, active seams, openFluxDetail browse.OpenFluxDetailFunc, openObjectDetail browse.OpenObjectDetailFunc) browse.OpenFluxTreeFunc {
	// A source opened from the tree has no sibling list to step through.
	openSource := func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		return openObjectDetail(kind, namespace, name, nil, 0, width, height)
	}
	return func(width, height int) (tea.Model, tea.Cmd) {
		ft := fluxtree.New(fluxtree.Config{
			Session:          sess,
			Lister:           active,
			Mutator:          active,
			OpenFluxDetail:   fluxtree.OpenFluxDetailFunc(openFluxDetail),
			OpenObjectDetail: openSource,
		})
		ft.SetSize(width, height)
		return &ft, ft.Init()
	}
}

// openRouteTableFunc pushes tasks/routetable (23a/23b) for an Ingress or
// discovered Gateway API kind's row — active alone satisfies every seam it
// needs (RawLister for rows, YAMLReader for 'Y', plus its own object-scoped
// events push for 'e', the same openObjectEventsFunc objectdetail already
// uses).
func openRouteTableFunc(sess *tui.Session, active seams, openYAML browse.OpenYAMLFunc) browse.OpenRouteTableFunc {
	openEvents := openObjectEventsFunc(sess, active, openYAML)
	return func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		rt := routetable.New(routetable.Config{
			Session:    sess,
			Lister:     active,
			YAML:       active,
			OpenEvents: routetable.OpenEventsFunc(openEvents),
			Kind:       kind,
			Namespace:  namespace,
			Name:       name,
		})
		rt.SetSize(width, height)
		return &rt, rt.Init()
	}
}

// openWhoCanFunc pushes tasks/whocan (22a), pre-filled with verb/resource/
// namespace — active alone satisfies the WhoCanReader seam whocan needs.
func openWhoCanFunc(sess *tui.Session, active seams) browse.OpenWhoCanFunc {
	return func(verb, resource, namespace string, width, height int) (tea.Model, tea.Cmd) {
		wc := whocan.New(whocan.Config{
			Session:   sess,
			RBAC:      active,
			OpenYAML:  whocan.OpenYAMLFunc(openYAMLFunc(sess, active)),
			Verb:      verb,
			Resource:  resource,
			Namespace: namespace,
		})
		wc.SetSize(width, height)
		return &wc, wc.Init()
	}
}

// openYAMLFunc pushes tasks/yamlview (8a) for any object, any kind, wiring
// it against the same seams active already satisfies for browse/nodedetail/
// poddetail.
func openYAMLFunc(sess *tui.Session, active seams) browse.OpenYAMLFunc {
	return func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		yv := yamlview.New(yamlview.Config{
			Session: sess,
			Lister:  active,
			YAML:    active,
			// Caches strip managedFields, so 8a fetches them for the one
			// object it is showing. A seam that doesn't provide it (the
			// --demo fake) falls back to the cached object.
			ManagedFields: managedFieldsReaderOf(active),
			Kind:          kind,
			Namespace:     namespace,
			Name:          name,
		})
		yv.SetSize(width, height)
		return &yv, yv.Init()
	}
}

// managedFieldsReaderOf returns v as a yamlview.ManagedFieldsReader if it
// provides one, and nil otherwise — nil selects yamlview's cached-object
// fallback rather than being an error.
func managedFieldsReaderOf(v any) yamlview.ManagedFieldsReader {
	if r, ok := v.(yamlview.ManagedFieldsReader); ok {
		return r
	}
	return nil
}

// openHelmHistoryFunc pushes tasks/helmhistory (18a's 'h') for a release —
// active alone satisfies the RawLister/Mutator seams it needs (it reads
// Secrets directly, the same undecorated seam every real kind's own
// informer already backs, and rolls back through kube.Mutator.HelmRollback
// like every other write verb).
func openHelmHistoryFunc(sess *tui.Session, active seams) browse.OpenHelmHistoryFunc {
	return func(namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		hh := helmhistory.New(helmhistory.Config{
			Session:   sess,
			Lister:    active,
			Mutator:   active,
			Namespace: namespace,
			Name:      name,
		})
		hh.SetSize(width, height)
		return &hh, hh.Init()
	}
}

// openHelmDetailFunc pushes the Helm transaction/live-evidence screen. The
// decorated lister is required so KindHelmRelease resolves from the narrow
// release cache; events and object reads stay on the active cluster seams.
func openHelmDetailFunc(sess *tui.Session, lister resources.RawLister, eventSource helmdetail.EventsReader, openEvents func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd)) browse.OpenHelmDetailFunc {
	return func(release kube.HelmRelease, width, height int) (tea.Model, tea.Cmd) {
		hd := helmdetail.New(helmdetail.Config{
			Session: sess, Lister: lister, Events: eventSource, Release: release,
			OpenEvents:    helmdetail.OpenEventsFunc(openEvents),
			OpenSavedYAML: openSavedHelmYAMLFunc(sess),
		})
		hd.SetSize(width, height)
		return &hd, hd.Init()
	}
}

// openSecretDataFunc pushes tasks/secretdata (27b) for a Secret row — active
// alone satisfies the RawLister/Mutator seams it needs (ListRaw for the
// Secret itself, kube.Mutator.PatchSecretData for add/remove).
func openSecretDataFunc(sess *tui.Session, active seams) browse.OpenSecretDataFunc {
	return func(namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		sd := secretdata.New(secretdata.Config{
			Session:   sess,
			Lister:    active,
			Mutator:   active,
			Namespace: namespace,
			Name:      name,
		})
		sd.SetSize(width, height)
		return &sd, sd.Init()
	}
}

// openConfigMapDataFunc pushes tasks/configmapdata (27a) for a ConfigMap
// row — active alone satisfies the RawLister/Mutator seams it needs (ListRaw
// for the ConfigMap itself and its Deployment/StatefulSet/DaemonSet
// consumers, kube.Mutator.PatchConfigMapData/RolloutRestart for apply/ctrl-r).
func openConfigMapDataFunc(sess *tui.Session, active seams) browse.OpenConfigMapDataFunc {
	return func(namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		cd := configmapdata.New(configmapdata.Config{
			Session:   sess,
			Lister:    active,
			Mutator:   active,
			Namespace: namespace,
			Name:      name,
		})
		cd.SetSize(width, height)
		return &cd, cd.Init()
	}
}

// openCronJobScheduleFunc pushes tasks/cronjobschedule (36d) for a CronJob
// row's own schedule editor — active alone satisfies every seam it needs
// (ListRaw for the CronJob itself, kube.Mutator.SetCronJobSchedule for
// apply/undo, TimeZoneCapability for §3.8's tri-state gate).
func openCronJobScheduleFunc(sess *tui.Session, active seams) browse.OpenCronJobScheduleFunc {
	return func(namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		cs := cronjobschedule.New(cronjobschedule.Config{
			Session:      sess,
			Lister:       active,
			Mutator:      active,
			Capabilities: active,
			Namespace:    namespace,
			Name:         name,
		})
		cs.SetSize(width, height)
		return &cs, cs.Init()
	}
}

// openCronJobDetailFunc pushes tasks/cronjobdetail (§36e) for a CronJob
// row — active alone satisfies every seam it needs (ListRaw for the
// CronJob/Job/Pod caches its own aggregation reads, kube.Mutator for its
// own run-now/suspend/resume verbs, same as browse's CronJob branch).
// openLogs/openYAML/openSchedule are the already-built closures buildBrowse-
// Task/buildDemoBrowseTask hand to browse itself, so cronjobdetail's `l`/`y`/
// `S` push exactly the same screens browse's own keys do rather than a
// second, possibly-drifting construction. currentUser threads through
// browse.Config.CurrentUser's own identity (real: cluster.Context.UserName,
// demo: fake.Cluster.CurrentUser()) so cronjobdetail's run-now preflight
// stamps the same kube.AnnotationTriggeredBy creator browse's list does.
func openCronJobDetailFunc(sess *tui.Session, active seams, openLogs browse.OpenLogsFunc, openYAML browse.OpenYAMLFunc, openSchedule browse.OpenCronJobScheduleFunc, currentUser string) browse.OpenCronJobDetailFunc {
	openObjectEvents := openObjectEventsFunc(sess, active, openYAML)
	return func(namespace, name string, siblings []browse.CronJobSiblingRef, index, width, height int) (tea.Model, tea.Cmd) {
		refs := make([]cronjobdetail.SiblingRef, len(siblings))
		for i, s := range siblings {
			refs[i] = cronjobdetail.SiblingRef{Namespace: s.Namespace, Name: s.Name}
		}
		cd := cronjobdetail.New(cronjobdetail.Config{
			Session:      sess,
			Lister:       active,
			Mutator:      active,
			OpenLogs:     cronjobdetail.OpenLogsFunc(openLogs),
			OpenYAML:     cronjobdetail.OpenYAMLFunc(openYAML),
			OpenEvents:   cronjobdetail.OpenEventsFunc(openObjectEvents),
			OpenSchedule: cronjobdetail.OpenScheduleFunc(openSchedule),
			Namespace:    namespace,
			Name:         name,
			Siblings:     refs,
			SiblingIndex: index,
			CurrentUser:  currentUser,
		})
		cd.SetSize(width, height)
		return &cd, cd.Init()
	}
}

// openJobAttemptsFunc pushes tasks/jobattempts (§37b/§37c/§37d) for a Job
// row — mirrors openCronJobDetailFunc's own reasoning exactly: active alone
// satisfies every seam it needs, openPodDetail/openLogs/openYAML are the
// same closures browse itself uses so this screen's `↵`/`l`/`y` push
// exactly what browse's own keys do (enter -> poddetail single-pod handoff,
// mirroring nodedetail's own OpenPod wiring), and currentUser threads
// through the same identity §37c's rerun "create" path stamps into
// kube.AnnotationTriggeredBy.
func openJobAttemptsFunc(sess *tui.Session, active seams, openPodDetail browse.OpenPodDetailFunc, openLogs browse.OpenLogsFunc, openYAML browse.OpenYAMLFunc, currentUser string) browse.OpenJobAttemptsFunc {
	openObjectEvents := openObjectEventsFunc(sess, active, openYAML)
	// A Job's attempt pods all live in the Job's own namespace, so
	// jobattempts' plain name siblings qualify with the opened pod's.
	openAttemptPod := func(pod kube.Pod, siblings []string, index, width, height int) (tea.Model, tea.Cmd) {
		refs := make([]browse.PodSiblingRef, len(siblings))
		for i, s := range siblings {
			refs[i] = browse.PodSiblingRef{Namespace: pod.Namespace, Name: s}
		}
		return openPodDetail(pod, refs, index, width, height)
	}
	return func(namespace, name string, siblings []browse.JobSiblingRef, index, width, height int) (tea.Model, tea.Cmd) {
		refs := make([]jobattempts.SiblingRef, len(siblings))
		for i, s := range siblings {
			refs[i] = jobattempts.SiblingRef{Namespace: s.Namespace, Name: s.Name}
		}
		ja := jobattempts.New(jobattempts.Config{
			Session:       sess,
			Lister:        active,
			Mutator:       active,
			OpenLogs:      jobattempts.OpenLogsFunc(openLogs),
			OpenPodDetail: jobattempts.OpenPodDetailFunc(openAttemptPod),
			OpenYAML:      jobattempts.OpenYAMLFunc(openYAML),
			OpenEvents:    jobattempts.OpenEventsFunc(openObjectEvents),
			Namespace:     namespace,
			Name:          name,
			Siblings:      refs,
			SiblingIndex:  index,
			CurrentUser:   currentUser,
		})
		ja.SetSize(width, height)
		return &ja, ja.Init()
	}
}

// helmValuesReader adapts an already-decoded kube.HelmRelease to yamlview's
// YAMLReader/RawLister seams — 18a's 'v' pushes 8a on data that was already
// fetched by browse's own load() (browse/helm.go's openSelectedHelmValues),
// not a fresh cluster round-trip, so this always answers the same release
// regardless of the (kind, namespace, name) yamlview's load() passes back.
// ListRaw returns a bare stand-in object just so yamlview's own
// findByName/ManagedFieldsYAML step (which every other kind needs to marshal
// managedFields) has something to find — a release has no managedFields of
// its own to show.
type helmValuesReader struct {
	release kube.HelmRelease
}

type savedHelmYAMLReader struct{ ref kube.HelmObjectRef }

func (r savedHelmYAMLReader) ListRaw(context.Context, kube.ResourceKind, string) ([]runtime.Object, error) {
	return []runtime.Object{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: r.ref.Name, Namespace: r.ref.Namespace}}}, nil
}

func (r savedHelmYAMLReader) GetYAML(context.Context, kube.ResourceKind, string, string) (string, string, error) {
	return r.ref.YAML, "saved release manifest", nil
}

func openSavedHelmYAMLFunc(sess *tui.Session) helmdetail.OpenSavedYAMLFunc {
	return func(ref kube.HelmObjectRef, width, height int) (tea.Model, tea.Cmd) {
		reader := savedHelmYAMLReader{ref: ref}
		yv := yamlview.New(yamlview.Config{
			Session: sess, Lister: reader, YAML: reader,
			Kind: kube.ResourceKind(ref.Kind), Namespace: ref.Namespace, Name: ref.Name,
		})
		yv.SetSize(width, height)
		return &yv, yv.Init()
	}
}

func (r helmValuesReader) ListRaw(_ context.Context, _ kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return []runtime.Object{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: r.release.Name, Namespace: r.release.Namespace}}}, nil
}

func (r helmValuesReader) GetYAML(_ context.Context, _ kube.ResourceKind, _, _ string) (string, string, error) {
	text := r.release.Values
	if text == "" {
		text = "# this release was installed with no values\n"
	}
	return text, fmt.Sprintf("revision %d", r.release.Revision), nil
}

// openHelmValuesFunc pushes tasks/yamlview (8a), read-only, on release's
// already-decoded values (docs/design README.md §18a: "v values in the YAML
// viewer, read-only").
func openHelmValuesFunc(sess *tui.Session) browse.OpenHelmValuesFunc {
	return func(release kube.HelmRelease, width, height int) (tea.Model, tea.Cmd) {
		reader := helmValuesReader{release: release}
		yv := yamlview.New(yamlview.Config{
			Session:   sess,
			Lister:    reader,
			YAML:      reader,
			Kind:      kube.KindHelmRelease,
			Namespace: release.Namespace,
			Name:      release.Name,
		})
		yv.SetSize(width, height)
		return &yv, yv.Init()
	}
}

// openExecFunc pushes tasks/execpicker (10a) for a pod's containers. Unlike
// this file's other Open*Func builders it takes no `active seams` argument —
// the picker never reads from the cluster; it spawns a kubectl subprocess to
// exec, and probes each container's shells through the shells seam (real:
// kubectlShellDetector, demo: *fake.Cluster). openDebug wires §41a's fork —
// enter on a row already known to have no shell.
func openExecFunc(sess *tui.Session, shells execpicker.ShellDetector, openDebug execpicker.OpenDebugFunc, demo bool) func(namespace, name string, containers []kube.ContainerInfo, width, height int) (tea.Model, tea.Cmd) {
	return func(namespace, name string, containers []kube.ContainerInfo, width, height int) (tea.Model, tea.Cmd) {
		ep := execpicker.New(execpicker.Config{
			Session:    sess,
			Namespace:  namespace,
			PodName:    name,
			Containers: containers,
			Shells:     shells,
			OpenDebug:  openDebug,
			Demo:       demo,
		})
		ep.SetSize(width, height)
		return &ep, ep.Init()
	}
}

// openDebugFunc pushes tasks/debugpanel (§41b/§41c) for a pod — the
// browse/poddetail-shaped seam (podPhase/waiting decide the panel's default
// mode). mutator backs §41c's cleanup delete only; the debug launch itself
// is a tea.ExecProcess handoff, never a Mutator call.
func openDebugFunc(sess *tui.Session, active seams, demo bool) func(namespace, name string, containers []kube.ContainerInfo, podPhase string, waiting bool, width, height int) (tea.Model, tea.Cmd) {
	return func(namespace, name string, containers []kube.ContainerInfo, podPhase string, waiting bool, width, height int) (tea.Model, tea.Cmd) {
		dp := debugpanel.New(debugpanel.Config{
			Session:    sess,
			Mutator:    active,
			Access:     active,
			OpenWhoCan: debugpanel.OpenWhoCanFunc(openWhoCanFunc(sess, active)),
			Namespace:  namespace,
			Name:       name,
			Containers: containers,
			PodPhase:   podPhase,
			Waiting:    waiting,
			Demo:       demo,
		})
		dp.SetSize(width, height)
		return &dp, dp.Init()
	}
}

// openExecDebugFunc pushes tasks/debugpanel in attach mode from
// tasks/execpicker's own §41a fork — target pre-selects the specific
// container the picker already knows has no shell. PodPhase/Waiting stay
// unset: the picker is only ever reached when at least one other container
// in the pod does have a shell, so attach is always the right default here
// (debugpanel.New's own mode gate needs neither field to pick it).
func openExecDebugFunc(sess *tui.Session, active seams, demo bool) execpicker.OpenDebugFunc {
	return func(namespace, podName string, containers []kube.ContainerInfo, target string, width, height int) (tea.Model, tea.Cmd) {
		dp := debugpanel.New(debugpanel.Config{
			Session:                sess,
			Mutator:                active,
			Access:                 active,
			OpenWhoCan:             debugpanel.OpenWhoCanFunc(openWhoCanFunc(sess, active)),
			Namespace:              namespace,
			Name:                   podName,
			Containers:             containers,
			InitialTargetContainer: target,
			Demo:                   demo,
		})
		dp.SetSize(width, height)
		return &dp, dp.Init()
	}
}

// openNodeDebugFunc pushes tasks/debugpanel (§41d) for a node — replaces
// the retired standalone NodeShell verb (verbs.go's NodeDebug/
// NodeDebugDetail doc comments). Shared by browse's 'x' on a Node row and
// nodedetail's own 's'. lister backs the panel's own post-exit "find the
// pod kubectl just created" cleanup lookup (debugpanel/node.go) — always
// the same *kube.Cluster/*fake.Cluster already passed in as mutator, which
// also implements resources.RawLister.
func openNodeDebugFunc(sess *tui.Session, active seams, demo bool) func(name string, podCount, width, height int) (tea.Model, tea.Cmd) {
	return func(name string, podCount, width, height int) (tea.Model, tea.Cmd) {
		dp := debugpanel.New(debugpanel.Config{
			Session:        sess,
			Mutator:        active,
			Lister:         active,
			IsNode:         true,
			Name:           name,
			NodePodCount:   podCount,
			NodeShellImage: sess.Config.NodeShellImage,
			Demo:           demo,
		})
		dp.SetSize(width, height)
		return &dp, dp.Init()
	}
}

// kubectlShellDetector adapts kube.DetectShells to execpicker's own
// ShellDetector seam. It carries the cluster only to pin the probe's kubectl
// to that cluster's context (kube.CommandTarget) — read when the probe runs,
// under the cluster's own lock, since probes run off the Update loop.
type kubectlShellDetector struct{ cluster *kube.Cluster }

func (d kubectlShellDetector) DetectShells(ctx context.Context, namespace, pod, container string) ([]string, error) {
	return kube.DetectShells(ctx, d.cluster.CommandTarget(), namespace, pod, container)
}

// openForwardFunc pushes tasks/forwardpicker (13a) against a real cluster.
// Dialer/Resolver are built fresh from cluster.Clientset()/RESTConfig()
// inside the returned closure — not captured once here — so a forward
// started after a context switch always dials the currently active
// context, while an already-running forward (whose own dialer/resolver was
// snapshotted at its own Start call) keeps reconnecting against whichever
// context it was started under (docs/design README.md §13d: forwards
// "survive context switches").
func openForwardFunc(sess *tui.Session, lister resources.RawLister, cluster *kube.Cluster) browse.OpenForwardFunc {
	return func(target kube.ForwardTarget, width, height int) (tea.Model, tea.Cmd) {
		fp := forwardpicker.New(forwardpicker.Config{
			Session:  sess,
			Lister:   lister,
			Resolver: kube.NewClientsetPodResolver(cluster.Clientset()),
			Dialer:   cluster.NewForwardDialer(),
			Manager:  sess.Forwards,
			Target:   target,
		})
		fp.SetSize(width, height)
		return &fp, fp.Init()
	}
}

// openForwardFuncDemo is openForwardFunc's --demo counterpart, wired
// against kube/fake's stand-in dialer/resolver.
func openForwardFuncDemo(sess *tui.Session, lister resources.RawLister, mgr *kube.ForwardManager, demoCluster *fake.Cluster) browse.OpenForwardFunc {
	return func(target kube.ForwardTarget, width, height int) (tea.Model, tea.Cmd) {
		fp := forwardpicker.New(forwardpicker.Config{
			Session:  sess,
			Lister:   lister,
			Resolver: fake.NewPodResolver(demoCluster),
			Dialer:   fake.NewForwardDialer(),
			Manager:  mgr,
			Target:   target,
		})
		fp.SetSize(width, height)
		return &fp, fp.Init()
	}
}

// openEventsFunc pushes tasks/events (9b) namespace-scoped for browse's 'e'
// (and the goto palette's Events kind carve-out) — openYAML wires 9b's own
// 'y' the same way every other list/detail screen already gets it.
func openEventsFunc(sess *tui.Session, active seams, openYAML browse.OpenYAMLFunc) browse.OpenEventsFunc {
	return func(namespace string, width, height int) (tea.Model, tea.Cmd) {
		ev := events.New(events.Config{
			Session:   sess,
			Events:    active,
			Lister:    active,
			OpenYAML:  events.OpenYAMLFunc(openYAML),
			Namespace: namespace,
		})
		ev.SetSize(width, height)
		return &ev, ev.Init()
	}
}

// openObjectEventsFunc pushes tasks/events (9b) object-scoped for
// poddetail/nodedetail/objectdetail/routetable's 'e' — the underlying
// closure is reused by all four via an explicit named-type conversion at
// each call site (the same pattern OpenYAMLFunc already uses across
// browse/poddetail/nodedetail).
func openObjectEventsFunc(sess *tui.Session, active seams, openYAML browse.OpenYAMLFunc) func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
	return func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		ev := events.New(events.Config{
			Session:    sess,
			Events:     active,
			Lister:     active,
			OpenYAML:   events.OpenYAMLFunc(openYAML),
			Namespace:  namespace,
			ObjectKind: kind,
			ObjectName: name,
		})
		ev.SetSize(width, height)
		return &ev, ev.Init()
	}
}

// openTimelineFunc pushes tasks/timeline (16a) namespace-scoped for
// browse's 't' — openEvents backs 16a's own 'e' (namespace-scoped, so
// timeline.OpenEventsFunc's kind/name args are dropped on the way through).
func openTimelineFunc(sess *tui.Session, active seams, openEvents browse.OpenEventsFunc) browse.OpenTimelineFunc {
	return func(namespace string, width, height int) (tea.Model, tea.Cmd) {
		tl := timeline.New(timeline.Config{
			Session:   sess,
			Events:    active,
			Lister:    active,
			Mutator:   active,
			Namespace: namespace,
			OpenEvents: func(_ kube.ResourceKind, namespace string, _ string, width, height int) (tea.Model, tea.Cmd) {
				return openEvents(namespace, width, height)
			},
		})
		tl.SetSize(width, height)
		return &tl, tl.Init()
	}
}

// openOverviewFunc pushes tasks/overview (19a) — reusing the same node-
// detail/timeline/events openers browse's own 'C'/'t'/'e' verbs already
// wire (openNodeDetail/openTimeline/openEvents), so 11b/16a/9b never drift
// between browse's row-level entry points and 19a's own NODES/`t`/`e` keys.
func openOverviewFunc(sess *tui.Session, lister resources.RawLister, nodeMetrics overview.NodeMetricsReader, openNodeDetail browse.OpenNodeDetailFunc, openTimeline browse.OpenTimelineFunc, openEvents browse.OpenEventsFunc) browse.OpenOverviewFunc {
	return func(width, height int) (tea.Model, tea.Cmd) {
		ov := overview.New(overview.Config{
			Session:        sess,
			Lister:         lister,
			NodeMetrics:    nodeMetrics,
			OpenNodeDetail: overview.OpenNodeDetailFunc(openNodeDetail),
			OpenTimeline:   overview.OpenTimelineFunc(openTimeline),
			OpenEvents:     overview.OpenEventsFunc(openEvents),
		})
		ov.SetSize(width, height)
		return &ov, ov.Init()
	}
}

// openObjectTimelineFunc pushes tasks/timeline (16b) object-scoped for
// poddetail/nodedetail's 't' — same named-type-conversion-at-call-site
// pattern openObjectEventsFunc already establishes. openEvents backs 16b's
// own 'e', reusing the same object-scoped closure poddetail/nodedetail's
// own OpenEvents is built from.
func openObjectTimelineFunc(sess *tui.Session, active seams, openEvents func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)) func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
	return func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
		tl := timeline.New(timeline.Config{
			Session:    sess,
			Events:     active,
			Lister:     active,
			Mutator:    active,
			Namespace:  namespace,
			ObjectKind: kind,
			ObjectName: name,
			OpenEvents: timeline.OpenEventsFunc(openEvents),
		})
		tl.SetSize(width, height)
		return &tl, tl.Init()
	}
}

// liveClusterLogStreamer re-resolves cluster's clientset on every stream
// request instead of capturing it once at wiring time. Necessary now that
// contexts can switch at runtime (7a, mvp-plan.md Phase 3):
// Cluster.SwitchContext rebuilds the clientset in place on the same
// *kube.Cluster pointer, so a kube.ClientPodLogStreamer built from a single
// cluster.Clientset() snapshot at startup would keep streaming against the
// pre-switch cluster forever.
type liveClusterLogStreamer struct {
	cluster *kube.Cluster
}

func (s liveClusterLogStreamer) StreamPodLogs(ctx context.Context, req kube.LogStreamRequest) (io.ReadCloser, error) {
	return kube.ClientPodLogStreamer{Client: s.cluster.Clientset()}.StreamPodLogs(ctx, req)
}

func openLogsFunc(sess *tui.Session, lister resources.RawLister, streamer kube.PodLogStreamer, clusterName, namespace string) browse.OpenLogsFunc {
	return func(pod kube.Pod, container string, width, height int) (tea.Model, tea.Cmd) {
		if pod.Context == "" {
			pod.Context = clusterName
		}
		if pod.Namespace == "" {
			pod.Namespace = namespace
		}
		logs := podlogs.FromPod(sess, lister, pod, container, streamer)
		logs.SetSize(width, height)
		return &logs, logs.Start()
	}
}

func Run() error {
	return RunWithConfig(DefaultConfig())
}

// RunWithConfig runs the program with cfg (--demo/--theme wired in from
// flags by the caller). It pipes both resource-change and connection-state
// events into the program (real cluster or --demo fake, whichever is
// active) and saves session state on exit.
func RunWithConfig(cfg Config) error {
	return run(cfg)
}

// run is RunWithConfig's body with the tea.Program's options left to the
// caller. The end-to-end harness (test/e2e) drives the program headlessly —
// WithInput/WithOutput/WithWindowSize over pipes rather than a terminal —
// and has to run *this* startup sequence, goroutines and all, rather than a
// copy of it that drifts out of step with the real one.
func run(cfg Config, opts ...tea.ProgramOption) error {
	sink, live, err := openDiagnostics(cfg)
	if err != nil {
		return err
	}
	defer sink.Close() // a close error at exit helps nobody
	configureKlog(sink)
	var term terminalOwner
	defer installPanicHandler(sink, term.release)()

	model, cluster, demoCluster := NewModel(cfg)
	logConfigLoad(sink, model.Session())
	// ForwardManager sessions deliberately survive context switches, but they
	// must not survive the process that owns them. Their reconnect loops are
	// rooted independently from the active Cluster so a context swap cannot
	// cancel them; make the composition root the matching lifetime boundary.
	if sess := model.Session(); sess != nil && sess.Forwards != nil {
		defer sess.Forwards.StopAll()
	}
	program := tea.NewProgram(newCrashCatcher(model, sink, live), opts...)
	term.set(program)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The same ctx that stops the event-forwarding goroutines and the
	// informer factories is what every screen's reads hang off (Session.
	// SetContext, internal/tui/sessionctx.go) — so quitting cancels a
	// half-finished list against a slow cluster instead of leaving it to run
	// out its own timeout against a program that is already gone.
	if sess := model.Session(); sess != nil {
		sess.SetContext(ctx)
	}

	switch {
	case cluster != nil:
		defer cluster.Stop()
		go forwardEvents(ctx, cluster, program, model.Session().Forwards)
		go func() {
			defer utilruntime.HandleCrash()
			_ = cluster.Start(ctx)
			// The one connect path where CRD discovery (folded into
			// Start) finishes outside any tea.Cmd the root can await
			// synchronously — see kube.CRDsDiscoveredMsg's doc comment.
			// Harmless to send even if Start failed: DiscoveredKinds()
			// is just empty then.
			program.Send(kube.CRDsDiscoveredMsg{})
		}()
	case demoCluster != nil:
		go forwardEvents(ctx, demoCluster, program, model.Session().Forwards)
	}
	if sess := model.Session(); sess != nil && sess.Forwards != nil {
		go watchForwardManager(ctx, sess.Forwards, program)
	}
	if sess := model.Session(); sess != nil {
		if cmd := updateCheckCmd(sess, buildChecker(cfg), false); cmd != nil {
			// Same "result arrives outside any tea.Cmd the root can await
			// synchronously" shape as the CRDsDiscoveredMsg goroutine just
			// above — updateCheckCmd's own tea.Cmd already does the
			// network round trip off-thread, so calling it directly here
			// (rather than threading it through Model.Init) doesn't block
			// program startup.
			go func() {
				defer utilruntime.HandleCrash()
				program.Send(cmd())
			}()
		}
	}

	_, err = program.Run()

	if sess := model.Session(); sess != nil {
		sess.SyncLocationToPerContext()
		_ = sess.State.Save()
	}
	return reportProgramCrash(sink, err)
}

// logConfigLoad records a config.yaml that didn't load cleanly — the header
// chip (tui.BuildConfigChip) only has room to say *that* it's broken; the
// log carries the parser's line-numbered reason.
func logConfigLoad(sink *diag.Sink, sess *tui.Session) {
	if sess == nil || sess.Config.LoadErr == nil {
		return
	}
	if sess.Config.ProdUnknown() {
		sink.Logf("config: %v (prodContexts unreadable: treating every context as PROD)", sess.Config.LoadErr)
		return
	}
	sink.Logf("config: %v (keys that failed to parse are ignored)", sess.Config.LoadErr)
}

// openDiagnostics builds the process's one diagnostics sink and the live
// crash Snapshot that feeds it. --log-file failing to open is fatal on
// purpose: a user who asked for a log file and silently got none is worse
// off than one who got an error at startup.
func openDiagnostics(cfg Config) (*diag.Sink, *liveState, error) {
	live := newLiveState(nil, cfg.Demo)
	sink, err := diag.Open(diag.Options{
		Build:    diag.Build{Version: cfg.Version, Commit: cfg.Commit, Date: cfg.Date},
		LogFile:  cfg.LogFile,
		CrashDir: filepath.Dir(state.Path()),
		Snapshot: live.Snapshot,
	})
	if err != nil {
		return nil, nil, err
	}
	live.sink = sink
	sink.Logf("starting %s (demo=%t, context=%q, namespace=%q, kubeconfig=%q)",
		diag.Build{Version: cfg.Version, Commit: cfg.Commit, Date: cfg.Date},
		cfg.Demo, cfg.Context, cfg.Namespace, cfg.Kubeconfig)
	return sink, live, nil
}

// configureKlog routes client-go's logger (klog) away from stderr, where its
// output — client-side throttling warnings, reflector errors — would splatter
// over the Bubble Tea screen, and into the diagnostics sink instead. This
// also covers everything routed through utilruntime.ErrorHandlers, which
// logs via klog. Without --log-file the sink writes no file, so the visible
// behaviour is what it always was (nothing on screen) — the difference is
// that the tail of the stream is now in memory when a crash report needs it.
func configureKlog(sink *diag.Sink) {
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	_ = fs.Set("logtostderr", "false")
	_ = fs.Set("alsologtostderr", "false")
	_ = fs.Set("stderrthreshold", "FATAL")
	klog.SetOutput(sink)
}

// watchForwardManager bridges mgr's change notifications into the Bubble
// Tea program as a kube.ResourceChangedMsg{Kind: KindForward} — reusing the
// exact message type/path every other kind's watch events already flow
// through, so browse's Forwards row reloads without any bespoke message or
// Update case (docs/design README.md §13c).
func watchForwardManager(ctx context.Context, mgr *kube.ForwardManager, program *tea.Program) {
	defer utilruntime.HandleCrash()
	events := mgr.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			program.Send(kube.ResourceChangedMsg{Kind: kube.KindForward})
		}
	}
}

// eventSource is what forwardEvents needs, satisfied by both *kube.Cluster
// and *fake.Cluster.
type eventSource interface {
	Events() <-chan kube.ResourceChangedMsg
	ConnEvents() <-chan kube.ConnStateMsg
}

// resourceEventDebounce is the event bridge's trailing-edge coalescing
// window. Informers commonly emit a burst for one logical change (and a
// controller can emit hundreds during a rollout); forwarding each one into
// Bubble Tea starts one cache-local screen reload per message and can fill
// Cluster.events before the final state is delivered. One pending deadline
// per kind keeps the bridge draining watches while guaranteeing a quiet kind
// sends its latest state promptly. A single timer services every kind, so a
// storm cannot accumulate timers either.
const resourceEventDebounce = 250 * time.Millisecond

// resourceEventMaxWait caps how long a kind's first unflushed event may wait,
// however continuously that kind keeps changing. A trailing-edge debounce on
// its own never fires for a kind whose events arrive less than
// resourceEventDebounce apart — routine for Pods on a cluster of a few
// thousand (kubelet status, probes, controller churn) — so the screen froze
// on a stale snapshot under a green "connected" badge. One second keeps a
// storm coalesced (at most one turn per kind per second, i.e. at least four
// debounce windows' worth of events folded into each) while bounding how
// stale a churning list can get to about what a person reads as live. It is
// also comfortably longer than the screens' own 250 ms reload debounce, so a
// delivered turn's reload lands before the next one arrives.
const resourceEventMaxWait = time.Second

// eventCoalescer is forwardEvents' per-kind pending state, kept free of
// timers and the clock so its schedule is testable with synthetic times. It
// is owned by the single bridge goroutine and needs no locking.
type eventCoalescer struct {
	pending map[kube.ResourceKind]pendingEvent
}

type pendingEvent struct {
	first time.Time // when the oldest unflushed event arrived
	last  time.Time // when the newest one arrived
}

func newEventCoalescer() *eventCoalescer {
	return &eventCoalescer{pending: map[kube.ResourceKind]pendingEvent{}}
}

// observe records one event for kind at now.
func (c *eventCoalescer) observe(kind kube.ResourceKind, now time.Time) {
	p, ok := c.pending[kind]
	if !ok {
		p.first = now
	}
	p.last = now
	c.pending[kind] = p
}

// due is when kind flushes: the trailing edge of its quiet window, or its
// max-wait bound, whichever comes first.
func (p pendingEvent) due() time.Time {
	quiet := p.last.Add(resourceEventDebounce)
	if bound := p.first.Add(resourceEventMaxWait); bound.Before(quiet) {
		return bound
	}
	return quiet
}

// next is the earliest due time across every pending kind, or zero when
// nothing is pending.
func (c *eventCoalescer) next() time.Time {
	var next time.Time
	for _, p := range c.pending {
		if due := p.due(); next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next
}

// flush removes and returns every kind due at or before now, in a stable
// order. A kind that keeps changing afterwards starts a fresh window.
func (c *eventCoalescer) flush(now time.Time) []kube.ResourceKind {
	var kinds []kube.ResourceKind
	for kind, p := range c.pending {
		if p.due().After(now) {
			continue
		}
		delete(c.pending, kind)
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

func forwardEvents(ctx context.Context, src eventSource, program *tea.Program, forwards *kube.ForwardManager) {
	defer utilruntime.HandleCrash()
	events := src.Events()
	conn := src.ConnEvents()
	pending := newEventCoalescer()
	var eventTimer *time.Timer
	var eventTimerC <-chan time.Time

	resetEventTimer := func(now time.Time) {
		next := pending.next()
		if next.IsZero() {
			if eventTimer != nil && !eventTimer.Stop() {
				select {
				case <-eventTimer.C:
				default:
				}
			}
			eventTimerC = nil
			return
		}
		delay := max(next.Sub(now), 0)
		if eventTimer == nil {
			eventTimer = time.NewTimer(delay)
		} else {
			if !eventTimer.Stop() {
				select {
				case <-eventTimer.C:
				default:
				}
			}
			eventTimer.Reset(delay)
		}
		eventTimerC = eventTimer.C
	}

	flushDue := func(now time.Time) {
		for _, kind := range pending.flush(now) {
			program.Send(kube.ResourceChangedMsg{Kind: kind})
			if kind == kube.KindPod && forwards != nil {
				forwards.ReconcilePodTargets()
			}
			if kind == kube.KindSecret {
				// A release Secret changing already emits KindHelmRelease from
				// the real filtered release informer. This covers fake/demo
				// listers whose releases derive from their shared Secrets.
				program.Send(kube.ResourceChangedMsg{Kind: kube.KindHelmRelease})
			}
		}
		resetEventTimer(now)
	}

	for {
		select {
		case <-ctx.Done():
			if eventTimer != nil {
				eventTimer.Stop()
			}
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			now := time.Now()
			pending.observe(ev.Kind, now)
			resetEventTimer(now)
		case now := <-eventTimerC:
			flushDue(now)
		case cs, ok := <-conn:
			if !ok {
				conn = nil
				continue
			}
			program.Send(cs)
		}
	}
}
