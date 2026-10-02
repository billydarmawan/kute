package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// defaultResync is the informer resync period; watch events drive updates, so
// this only backstops missed notifications.
const defaultResync = 5 * time.Minute

// ErrClusterStopped is returned by Start for a Cluster that Stop has already
// torn down. Stop's sentinel for that is a nil stopCh, which the ensure* paths
// already refuse on; Start refuses on it too rather than starting informers
// that could never be stopped.
var ErrClusterStopped = errors.New("cluster is stopped")

// ErrCacheSyncFailed is returned by Start when the eager informer caches never
// settle — including the legitimate case of a Stop landing mid-connect, which
// closes the wait early rather than hanging.
var ErrCacheSyncFailed = errors.New("informer caches failed to sync")

// newTypedFactory builds the shared informer factory every cluster-wide typed
// cache comes from. The one construction site, so the cache transform can't
// be wired into production and quietly missed by a test's hand-built
// factory. Namespace-scoped mode's own per-namespace factories
// (scopedFactories) are built the same way, inline where they're needed —
// see factoryForScopeLocked.
func newTypedFactory(client kubernetes.Interface) informers.SharedInformerFactory {
	return informers.NewSharedInformerFactoryWithOptions(client, defaultResync,
		informers.WithTransform(stripManagedFields))
}

// scopeKey identifies one cache: a kind plus the namespace its informer is
// scoped to. "" means a cluster-wide cache — the only value ever used in
// cluster-wide mode (Decisions: every cache scope key normalizes to ""),
// and namespace-scoped mode's own explicit global cache (all-namespaces,
// overview, who-can's ClusterRole half, …).
type scopeKey struct {
	kind      ResourceKind
	namespace string
}

// Cluster is the live data layer: a clientset plus a shared informer factory
// (so reads hit in-memory caches, not the API server) and a metrics client. It
// implements the resources.RawLister contract via ListRaw and emits change
// events for watched kinds. It is the informer/watch replacement for the
// one-shot Client* readers.
type Cluster struct {
	clientset kubernetes.Interface
	metrics   metricsclient.Interface
	factory   informers.SharedInformerFactory
	restCfg   *rest.Config
	// credentialCfg is the inner authenticated config used only when an
	// upgraded SPDY transport must be constructed. Ordinary and derived REST
	// clients use restCfg's gated outer transport.
	credentialCfg *rest.Config
	Context       Context
	authGate      *authenticationGate
	watcher       *watchObserver

	// replaceJobGoneTimeout overrides ReplaceJob's bounded wait for the
	// deleted Job to disappear (zero = replaceJobGoneTimeout); tests only.
	replaceJobGoneTimeout time.Duration

	// scoped is set once via SetNamespaceScope, before Start, and never
	// cleared — a session that launched cluster-wide stays cluster-wide, and
	// vice versa (docs/lazy-informers.md §5.6: "not
	// persisted... a later launch without the flag is cluster-wide again").
	// cacheScopeLocked is the single place it's consulted.
	scoped bool
	// scopedFactories holds one namespace-filtered typed informer factory per
	// namespace actually read in scoped mode — built on demand, the same
	// laziness every other cache gets. Keyed by namespace (never "", which
	// always means the cluster-wide c.factory).
	scopedFactories map[string]informers.SharedInformerFactory
	// dynScopedFactories is scopedFactories' dynamic-informer counterpart for
	// discovered CRD kinds.
	dynScopedFactories map[string]dynamicinformer.DynamicSharedInformerFactory

	// generation increments on every Start, at the very start of
	// SwitchContext's own critical section (see its doc comment for why that
	// has to happen before, not after, its map clears), and on Stop — and is
	// captured by every watch-error/notify closure and health loop. A
	// callback whose captured generation no longer matches c.generation
	// belongs to a context that's already been replaced and must not write
	// into the new context's kindFailed/kindStalled maps — see
	// generationCurrent, and noteWatchError/markKindFailed/notify for why the
	// actual writes re-check gen themselves rather than trusting a caller's
	// earlier generationCurrent call.
	generation int
	// eagerKeys is the exact eager set as registered by the most recent
	// Start — Namespace@"", Node@"", and Pod@ whatever cacheScopeLocked
	// resolved for the launch namespace (cluster-wide "" in cluster-wide
	// mode, the scoped namespace in scoped mode). eagerHasSynced reads this
	// rather than a fixed list, since the Pod entry's scope depends on mode.
	eagerKeys []scopeKey

	// dynClient/dynFactory back CRD discovery and every discovered kind's
	// list/watch (discovery.go/dynamic.go): a second, generic informer
	// mechanism alongside factory's typed one, since Pod/Deployment/…
	// have compile-time listers but a CRD's shape is only known at
	// runtime. dynKinds maps a (ResourceKind, scope) pair (built-in
	// KindCustomResourceDefinition always at "", or a discovered kind's own
	// Kind name at whichever scope it's been read at) to its GVR/lister;
	// discovered is the last refreshDiscovery pass's parsed CRD cache
	// (docs/design README.md's "discovery" state entry).
	dynClient  dynamic.Interface
	dynFactory dynamicinformer.DynamicSharedInformerFactory
	dynKinds   map[scopeKey]dynamicKindInfo
	discovered []DiscoveredKind
	// crdColumnsFetched marks kinds whose printer columns have been pulled
	// from their own CRD. Discovery deliberately skips them (they live in
	// the part of a CRD that is megabytes), so they arrive per-kind on
	// first use — see ensurePrinterColumns.
	crdColumnsFetched map[ResourceKind]bool
	// crdColumnsInFlight dedupes concurrent fetches: browse re-reads on
	// every change event, and each read would otherwise launch its own.
	crdColumnsInFlight map[ResourceKind]bool

	// metaClient is CountLive's PartialObjectMetadata client, built on first
	// use and dropped on SwitchContext along with everything else bound to
	// the old cluster.
	metaClient metadata.Interface

	// helmFactories/helmInformers back KindHelmRelease: Secret informers of
	// their own, filtered server-side to type=helm.sh/release.v1 so listing
	// releases doesn't pull every Secret in the cluster (see helm.go's
	// ensureHelmSecrets). Started on first read, like every other lazy kind,
	// and keyed by namespace ("" = all) because a cluster-wide release list
	// is a much bigger read than the one namespace a screen is showing —
	// independent of scoped/cluster-wide mode, since this scoping predates
	// and is orthogonal to it (docs/lazy-informers.md §5.6
	// §1: "Delete Helm's mutable helmScope field: its per-namespace
	// informers are already keyed correctly and now answer through the same
	// scope-aware seam").
	helmFactories map[string]informers.SharedInformerFactory
	helmInformers map[string]cache.SharedIndexInformer

	// kindInformers is every typed informer registered so far, keyed by
	// (kind, scope) — the handle KindSynced needs (a lister can't report its
	// own sync state). kindFailed holds the error from a watch that hit a
	// permanent failure — today only "you may not list this" — so those
	// caches are never waited on again. It keeps the error rather than a
	// bare bool because the screen has to say *why* it has nothing: settled
	// with no reason attached is indistinguishable from a kind the cluster
	// genuinely has none of, which is a claim this app has no basis for
	// making. Read through KindForbidden. kindStalled holds the last error
	// from a cache that is still failing its initial LIST — recoverable, so
	// it is cleared rather than latched, but until it does recover it is the
	// difference between a screen showing why it is empty and one spinning.
	// A denial for kind@"" must never poison kind@"team-a", and vice versa —
	// the whole reason these are keyed by scope rather than by kind alone.
	kindInformers map[scopeKey]cache.SharedIndexInformer
	kindFailed    map[scopeKey]error
	kindStalled   map[scopeKey]error
	// watchUnhealthy records post-sync watch failures until that informer
	// delivers another object event. A successful /livez only proves the API
	// endpoint answers; it must not declare the session recovered while a
	// resource stream is still broken.
	watchUnhealthy map[scopeKey]bool

	// allKindsSynced latches allStartedKindsSyncedLocked's answer once every
	// informer started so far has finished its initial LIST, so the steady
	// state costs a bool read instead of a sweep over every started
	// informer. Sound because HasSynced is monotonic per informer — a
	// DeltaFIFO only counts an initial population once, so a watch drop and
	// re-LIST on an already-synced cache never un-syncs it — and because the
	// only other way the sweep's answer changes is a *new* informer, which
	// clears the latch through noteInformerStartedLocked. It matters on the
	// watch-error path: every reflector's error funnels through
	// recordWatchError, which needs this answer while holding the same c.mu
	// ListRaw/ensureKind take on the render loop's behalf. With thirty lazy
	// informers started and a connection flapping, an O(all-informers) sweep
	// per error — each HasSynced taking that informer's own lock — is UI jank
	// by construction.
	allKindsSynced bool

	events chan ResourceChangedMsg
	health *health
	stopCh chan struct{}
	// cancelHealthProbe cancels the /livez request owned by the current
	// generation. It is created alongside the client/gate/generation snapshot
	// in Start and cancelled before SwitchContext replaces any of them.
	cancelHealthProbe context.CancelFunc
	started           bool
	synced            bool
	// reached latches the first proof this generation ever got an answer out
	// of the apiserver: any cache event, or the eager set finishing its sync.
	// Distinct from synced, which only latches once *every* eager cache has
	// settled — a session whose Pod cache filled and rendered real rows, but
	// whose connection died before Namespace/Node settled, has reached the
	// cluster without ever being synced. Cleared by SwitchContext alongside
	// synced, so a switch into an unreachable context starts from no proof.
	reached bool
	mu      sync.Mutex

	// tzCapability is capability.go's tri-state "does this server honor
	// CronJob spec.timeZone" answer, refreshed by probeTimeZoneCapability
	// on Start/SwitchContext — never read directly, always through
	// TimeZoneCapability() (mu-guarded like every other field above).
	tzCapability TimeZoneCapability
}

// dynamicKindInfo is one dynamically registered kind's watch/list handle.
type dynamicKindInfo struct {
	gvr        schema.GroupVersionResource
	namespaced bool
	lister     cache.GenericLister
	// informer is retained alongside the lister purely for HasSynced — a
	// lister can't report whether its cache has finished its initial fill.
	informer cache.SharedIndexInformer
}

// NewCluster builds a Cluster from the active kubeconfig (same resolution as
// NewClient). The metrics client is best-effort: a nil metrics client just
// yields empty usage rather than an error.
func NewCluster() (*Cluster, error) {
	return NewClusterForContext("")
}

// NewClusterForContext builds a Cluster pinned to the named kubeconfig
// context (same resolution as NewClientForContext), or the kubeconfig's
// current-context when contextName is "" — the seam BuildSession uses to
// restore the most-recently-used context at startup instead of always
// deferring to the kubeconfig file's own current-context.
func NewClusterForContext(contextName string) (*Cluster, error) {
	client, err := NewClientForContext(contextName)
	if err != nil {
		return nil, err
	}
	metrics, _ := metricsclient.NewForConfig(client.RESTConfig)
	dynClient, _ := dynamic.NewForConfig(client.RESTConfig)

	c := &Cluster{
		clientset:     client.Interface,
		metrics:       metrics,
		factory:       newTypedFactory(client.Interface),
		restCfg:       client.RESTConfig,
		credentialCfg: client.credentialConfig,
		Context:       client.Context,
		dynClient:     dynClient,
		dynFactory:    dynamicinformer.NewDynamicSharedInformerFactory(dynClient, defaultResync),
		events:        make(chan ResourceChangedMsg, 64),
		health:        newHealth(),
		stopCh:        make(chan struct{}),
		authGate:      client.authGate,
		watcher:       client.watcher,
	}
	client.authGate.onBlocked = func(err error) { c.recordAuthenticationFailure(client.authGate, err) }
	c.health.setStaticCredentials(client.authGate.staticCredentials)
	c.health.setCredentialStderr(client.pluginStderr)
	return c, nil
}

// Clientset exposes the underlying clientset for callers that still need direct
// access (e.g. the existing one-shot pod lister and log streamer).
func (c *Cluster) Clientset() kubernetes.Interface { return c.clientset }

// RESTConfig exposes the REST config for building auxiliary clients (e.g. the
// metrics reader used by the rich pods screen).
func (c *Cluster) RESTConfig() *rest.Config { return c.restCfg }

// Events is the stream of change notifications from watched informers.
func (c *Cluster) Events() <-chan ResourceChangedMsg { return c.events }

// SetNamespaceScope turns on namespace-scoped mode for this Cluster and pins
// it to namespace: every namespaced kind's informer scopes to just this
// namespace instead of the whole cluster
// (docs/lazy-informers.md §5.6: "restricts kute to this
// namespace, for identities without cluster-wide list access"). Must be
// called before Start — it sets Context.Namespace too, so Session.Location,
// Cluster.Context, and the eager Pod cache all agree from the first frame.
// Once set it is never cleared: the mode survives namespace and context
// switches for the life of this process (Decisions: "not persisted").
func (c *Cluster) SetNamespaceScope(namespace string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scoped = true
	c.Context.Namespace = namespace
}

// Scoped reports whether SetNamespaceScope has pinned this Cluster to one
// namespace — the namespace palette's denied-state notice and the 403 card's
// scoped-mode hint both need to tell scoped mode apart from an ordinary
// cluster-wide denial.
func (c *Cluster) Scoped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scoped
}

// cacheScope normalizes namespace for kind under the current mode — the
// single place mode is consulted (Decisions). Locked callers use
// cacheScopeLocked directly.
func (c *Cluster) cacheScope(kind ResourceKind, namespace string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cacheScopeLocked(kind, namespace)
}

// cacheScopeLocked is cacheScope's body. In cluster-wide mode, or for an
// explicit all-namespaces/cluster-scoped read, it always answers ""
// (Decisions: every cache scope key normalizes to "", so the data layer
// behaves — and tests byte-for-byte assert — exactly as today). In scoped
// mode, a namespaced kind's own namespace is the scope; a cluster-scoped
// kind (Node, Namespace, ClusterRole(Binding), the built-in CRD list, a
// discovered cluster-scoped CRD) still normalizes to "" — there is exactly
// one of those caches regardless of mode. c.mu must be held.
func (c *Cluster) cacheScopeLocked(kind ResourceKind, namespace string) string {
	if !c.scoped || namespace == "" {
		return ""
	}
	if ClusterScopedKind(kind, c.discovered) {
		return ""
	}
	// Undiscovered (not yet known), or a kind whose own scoping is
	// independent of this normalizer (KindHelmRelease, KindForward): default
	// to namespaced, the safe assumption once scoped mode is on.
	return namespace
}

// ClusterScopedKind reports whether kind is backed by exactly one
// cluster-wide cache regardless of any namespace argument — Node, Namespace,
// ClusterRole(Binding), the built-in CRD list, and any discovered CRD kind
// whose own scope (from CRD discovery) is Cluster rather than Namespaced.
//
// Exported so kube/fake's own per-scope health maps can normalize a
// (kind, namespace) key exactly like cacheScopeLocked does, without either
// duplicating the typed-kind table (unexported, package-private) or drifting
// from it — a second copy of this rule is exactly the kind of thing that
// goes stale the next time a kind's clusterScoped flag changes.
func ClusterScopedKind(kind ResourceKind, discovered []DiscoveredKind) bool {
	if tk, ok := typedKinds[kind]; ok {
		return tk.clusterScoped
	}
	if kind == KindCustomResourceDefinition {
		return true
	}
	for _, dk := range discovered {
		if dk.RegistryKind() == kind {
			return dk.ClusterScoped
		}
	}
	return false
}

// factoryForScopeLocked returns the typed informer factory backing scope,
// building and caching a namespace-filtered one on first use for a non-empty
// scope — the same informers.WithNamespace(ns) shape helm.go's
// ensureHelmSecrets already established. c.mu must be held.
func (c *Cluster) factoryForScopeLocked(scope string) informers.SharedInformerFactory {
	if scope == "" {
		return c.factory
	}
	if c.scopedFactories == nil {
		c.scopedFactories = map[string]informers.SharedInformerFactory{}
	}
	f, ok := c.scopedFactories[scope]
	if !ok {
		f = informers.NewSharedInformerFactoryWithOptions(c.clientset, defaultResync,
			informers.WithNamespace(scope), informers.WithTransform(stripManagedFields))
		c.scopedFactories[scope] = f
	}
	return f
}

// dynFactoryForScopeLocked is factoryForScopeLocked's dynamic-informer
// counterpart, for discovered CRD kinds. c.mu must be held.
func (c *Cluster) dynFactoryForScopeLocked(scope string) dynamicinformer.DynamicSharedInformerFactory {
	if scope == "" {
		return c.dynFactory
	}
	if c.dynScopedFactories == nil {
		c.dynScopedFactories = map[string]dynamicinformer.DynamicSharedInformerFactory{}
	}
	f, ok := c.dynScopedFactories[scope]
	if !ok {
		f = dynamicinformer.NewFilteredDynamicSharedInformerFactory(c.dynClient, defaultResync, scope, nil)
		c.dynScopedFactories[scope] = f
	}
	return f
}

// generationCurrent reports whether gen is still this Cluster's active
// generation — the guard every watch-error/notify closure checks before
// writing anything, so a late callback from a context SwitchContext has
// already replaced can't corrupt the new context's state (see the
// generation field's doc comment).
func (c *Cluster) generationCurrent(gen int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation == gen
}

// Start registers the eager watch handlers, starts those informers, and
// blocks until their caches have settled — synced, or (Decisions: startup
// tolerance applies in both modes) permanently denied — or ctx is done.
// Kinds outside the eager set are started later, on first read.
//
// The eager set is scope-aware: Namespace@"" and Node@"" always (there is
// only ever one cluster-wide cache for either), and Pod at whatever
// cacheScopeLocked resolves for the launch namespace — "" in cluster-wide
// mode (Pod watches every namespace, same as before this feature), or the
// pinned namespace in scoped mode.
func (c *Cluster) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	// A nil stopCh is Stop's "this cluster is dead" sentinel, the same one
	// ensureKind/ensureDynamicKind/ensureHelmSecrets refuse on. Starting
	// informers against it would hand every one of them a stop channel that
	// never closes, and hand the health loop a nil channel to select on.
	if c.stopCh == nil {
		c.mu.Unlock()
		return ErrClusterStopped
	}
	c.started = true
	c.generation++
	probeCtx, cancelProbe := context.WithCancel(context.WithoutCancel(ctx))
	c.cancelHealthProbe = cancelProbe
	probe := healthProbeSnapshot{
		clientset:  c.clientset,
		gate:       c.authGate,
		generation: c.generation,
		ctx:        probeCtx,
	}

	c.registerWatchesLocked(KindNamespace, KindNode)
	podScope := c.cacheScopeLocked(KindPod, c.Context.Namespace)
	podFactory := c.factoryForScopeLocked(podScope)
	c.registerTypedWatchLocked(KindPod, podScope, podFactory)
	c.eagerKeys = []scopeKey{{KindNamespace, ""}, {KindNode, ""}, {KindPod, podScope}}

	// Snapshot stopCh while the lock is still held. Every writer of the
	// field (Stop, SwitchContext) holds c.mu, and the three uses below
	// outlive this critical section — reading the field again after the
	// Unlock is a data race, and one Stop wins by installing nil, which
	// panics the health loop and hangs the cache wait forever.
	stopCh := c.stopCh

	c.factory.Start(stopCh)
	if podScope != "" {
		podFactory.Start(stopCh)
	}
	c.mu.Unlock()
	go c.startHealthLoop(stopCh, probe)

	// Wait on the eager informers by name rather than calling
	// factory.WaitForCacheSync, which waits on whatever is started at the
	// moment it's called: a lazily-started heavy kind (browse's restored
	// kind, say) can race into that snapshot and drag connect-time back to
	// what this change exists to remove.
	//
	// No dynamic informer is waited on, because none is started here any
	// more — discovery reads the API directly (refreshDiscovery) and the
	// CRD informer starts only if the 14b CRDs list is opened.
	synced, err := waitForCacheSync(ctx, stopCh, c.eagerHasSynced()...)
	if err != nil {
		return err
	}
	if !synced {
		return ErrCacheSyncFailed
	}
	// Everything from here on writes into the cluster this Start connected,
	// so each write is gated on its generation: a Start that a
	// SwitchContext has superseded (the detached initial one still inside
	// slow discovery, say) must not mark the new context synced, or replace
	// its CRDs or server-version answer with the old cluster's.
	c.mu.Lock()
	if c.generation == probe.generation {
		c.synced = true
		c.reached = true
	}
	c.mu.Unlock()

	c.refreshDiscovery(ctx, probe.generation, probe.clientset)
	c.probeTimeZoneCapability(ctx, probe.generation, probe.clientset)
	return nil
}

// eagerHasSynced collects the eager set's HasSynced funcs, each tolerant of
// a permanent permission denial: settled = the initial LIST completed OR a
// watch error recorded a permanent denial against that exact key. A
// transient LIST failure (an outage, a slow link) keeps the func returning
// false, so cache.WaitForCacheSync keeps waiting for recovery or ctx
// cancellation — only a denial short-circuits it. Without this, a denied
// Namespace or Node cache (the common shape under a namespace-bound
// identity) stops startup from ever completing, because it is a missing
// capability, not a failed connection.
// waitForCacheSync waits for synced to settle, returning early — and without
// stranding a goroutine — if ctx is done or stopCh closes first.
//
// cache.WaitForCacheSync knows nothing about context: it returns when its
// caches settle or when the stop channel it was given closes, and nothing
// else. Handing it a cluster's own stopCh therefore left its goroutine blocked
// past every cancelled connect, which is the *normal* path whenever
// SwitchContext's or attemptReconnect's timeout expires against a slow
// cluster — blocked until the whole cluster was torn down, still writing a
// result nobody would read.
//
// So it gets a private stop channel instead, closed on whichever of ctx,
// stopCh, or this function's own return comes first. Both goroutines below are
// bounded by the call.
func waitForCacheSync(ctx context.Context, stopCh <-chan struct{}, synced ...cache.InformerSynced) (bool, error) {
	waitCh := make(chan struct{})
	stopWaiting := sync.OnceFunc(func() { close(waitCh) })
	defer stopWaiting()
	go func() {
		defer stopWaiting()
		select {
		case <-ctx.Done():
		case <-stopCh:
		case <-waitCh:
		}
	}()

	// Buffered so the waiter can always deposit its result and exit, even
	// once this function has already returned on the ctx branch.
	done := make(chan bool, 1)
	go func() {
		done <- cache.WaitForCacheSync(waitCh, synced...)
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case ok := <-done:
		return ok, nil
	}
}

func (c *Cluster) eagerHasSynced() []cache.InformerSynced {
	c.mu.Lock()
	keys := append([]scopeKey(nil), c.eagerKeys...)
	c.mu.Unlock()
	out := make([]cache.InformerSynced, 0, len(keys))
	for _, k := range keys {
		key := k
		out = append(out, func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.kindFailed[key] != nil {
				return true
			}
			if inf, ok := c.kindInformers[key]; ok {
				return inf.HasSynced()
			}
			return true
		})
	}
	return out
}

// ensureKind idempotently registers and starts kind's typed informer at the
// scope namespace normalizes to, the typed counterpart of
// ensureDynamicKind. ListRaw calls it before every read, which is what makes
// laziness invisible to callers: no screen has to declare what it needs, and
// a kind nobody opens is never watched. Reports the scope it registered (or
// found already registered) at, so ListRaw's own factory lookup and a
// caller's ensureDynamicKindFor-style scope resolution agree, and the
// factory that scope resolved to under the same lock — nil for a stopped
// cluster, where there is nothing safe to read. ListRaw must list from that
// factory rather than look it up again: a SwitchContext between two critical
// sections would hand it the new cluster's factory, whose Lister() registers
// an informer there with no handlers, which the next Start then runs — and
// SetWatchErrorHandler can never be attached to it afterwards.
//
// The whole body holds c.mu, for the same two reasons it always has. It
// keeps the factory/stopCh reads from racing SwitchContext's replacement of
// both. And it makes registration-then-Start atomic: were the lock dropped
// in between, a concurrent caller's Start could run this informer before its
// watch-error handler was attached, and SetWatchErrorHandler then fails
// outright on an already-running informer.
func (c *Cluster) ensureKind(kind ResourceKind, namespace string) (string, informers.SharedInformerFactory) {
	if _, ok := typedKinds[kind]; !ok {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	scope := c.cacheScopeLocked(kind, namespace)
	// A stopped cluster has a nil stopCh, and an informer started with one
	// would never stop, so leave it unregistered.
	if c.stopCh == nil {
		return scope, nil
	}
	f := c.factoryForScopeLocked(scope)
	if _, ok := c.kindInformers[scopeKey{kind, scope}]; ok {
		return scope, f
	}
	c.registerTypedWatchLocked(kind, scope, f)
	f.Start(c.stopCh)
	c.health.noteListBurst()
	return scope, f
}

// Synced reports whether the startup informer set has completed its initial
// sync. It is a latch: set once at the end of Start and never cleared, so it
// answers "has this cluster finished connecting", not "is every cache
// currently up to date". Callers deciding whether a specific empty list is
// trustworthy want KindSynced instead.
func (c *Cluster) Synced() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.synced
}

// Reached reports whether this cluster generation has ever got an answer out
// of the apiserver — one cache event is enough, and so is the eager set
// finishing its sync. Also a latch, cleared only by SwitchContext.
//
// It exists because Synced is the wrong question for "does this session hold
// cached state worth keeping". Synced waits for *every* eager cache, so
// between the Pod cache filling (which is what puts real rows on screen) and
// the last of Namespace/Node settling, Synced is still false — and an outage
// landing in that window looked indistinguishable from a cluster that was
// never reachable at all. The 4c swap read it that way and replaced a browse
// screen listing the cluster's pods with "unreachable · NO CLUSTER".
//
// Unlike ResourceChangedMsg — the root shell's other proof-of-connection,
// which rides a lossy channel (notifyScope drops on a full buffer) and can be
// sent before the Bubble Tea event loop is consuming — a latch on the cluster
// itself cannot be missed by whoever asks later.
func (c *Cluster) Reached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reached
}

// linkProvenLocked reports whether this generation has any evidence that its
// apiserver is reachable — the connectGrace precondition. Reached counts (a
// /livez success, a cache event, a filled cache), and so does the transport
// having received any HTTP response at all: a slow initial LIST over a
// saturated link has its response headers long before its body, which is
// exactly the case grace exists for, and it can arrive before the first ping
// or cache has had the chance to prove anything. Both are per-generation:
// SwitchContext clears reached and installs a fresh gate. c.mu must be held.
func (c *Cluster) linkProvenLocked() bool {
	return c.reached || (c.authGate != nil && c.authGate.hasResponded())
}

// noteReached latches Reached. Called from the informer event callbacks, where
// the generation check has already passed.
func (c *Cluster) noteReached() {
	c.mu.Lock()
	c.reached = true
	c.mu.Unlock()
}

// scopeKeyForLocked resolves kind/namespace to the exact cache key
// KindSynced/KindError/KindForbidden answer for. KindHelmRelease bypasses
// cacheScopeLocked's mode normalization entirely — its own per-namespace
// scoping (helm.go) predates and is independent of scoped/cluster-wide mode,
// so the namespace a read actually used is the key regardless of mode. c.mu
// must be held.
func (c *Cluster) scopeKeyForLocked(kind ResourceKind, namespace string) scopeKey {
	if kind == KindHelmRelease {
		return scopeKey{kind, namespace}
	}
	return scopeKey{kind, c.cacheScopeLocked(kind, namespace)}
}

// KindSynced reports whether kind's own informer cache — the one namespace
// backs, once normalized by the current mode — has completed its initial
// fill. ListRaw reads caches directly regardless of sync state, so an empty
// result is ambiguous — genuinely no objects, or the cache hasn't been
// populated yet — and this is what disambiguates it.
//
// Per-(kind, scope) rather than cluster-wide because each informer fills
// independently: the Namespace cache routinely has real data long before
// some unrelated, rarely-watched kind finishes, and on a cluster whose RBAC
// forbids listing (say) HorizontalPodAutoscalers, an aggregate flag never
// flips at all. In scoped mode a denial for one namespace's cache must not
// vouch for (or poison) another's — callers must ask about the same
// namespace the read used (docs/lazy-informers.md §5.6).
//
// Reports true — "this empty answer is trustworthy, render it" — for a
// stopped cluster, for synthetic kinds with no informer to wait on, and for
// a kind whose watch failed with a permission error, so a caller gating a
// loading state on this can never spin forever waiting for a cache that is
// never going to arrive.
// Also reports true for a cache whose initial LIST keeps failing, which is
// settled in the only sense a spinner cares about: it is not about to fill.
// KindError says why, so the screen can show the failure instead of
// asserting the cluster is empty. Should a later retry succeed after all, the
// stall clears itself here and the kind reads as genuinely synced.
func (c *Cluster) KindSynced(kind ResourceKind, namespace string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.scopeKeyForLocked(kind, namespace)
	if c.kindSyncedLockedKey(key) {
		delete(c.kindStalled, key)
		return true
	}
	return c.kindStalled[key] != nil
}

// KindStarted reports whether reading kind at namespace would start nothing
// new — an informer already backs that exact cache, or no informer could
// ever back it (a stopped cluster, a kind nothing serves). It is the
// question a breadth-first consumer (the goto palette's fuzzy resource
// corpus, browse's empty-state hints) must ask before it reads a cache it
// isn't displaying, and it is deliberately not KindSynced: KindSynced
// answers "settled" for anything that will never arrive so a spinner can't
// hang, which for a kind whose informer simply hasn't been started yet is
// the opposite of the answer a breadth-first reader needs. Asking KindSynced
// alone is how opening goto started the full-CRD informer (the 2.76 MB read
// docs/lazy-informers.md §5.1 removed from connect): no cache key matched
// KindCustomResourceDefinition before 14b had been opened, so it fell
// through to "unknown kind, settled".
//
// Mirrors the decisions ListRaw's own ensureKind/ensureDynamicKindFor make,
// so the two cannot disagree about which reads start an informer. namespace
// follows the same rule as KindSynced's.
func (c *Cluster) KindStarted(kind ResourceKind, namespace string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopCh == nil {
		// ensureKind/ensureDynamicKind register nothing on a stopped
		// cluster, so a read here is free.
		return true
	}
	key := c.scopeKeyForLocked(kind, namespace)
	if kind == KindHelmRelease {
		return c.helmInformers[key.namespace] != nil
	}
	if _, ok := c.kindInformers[key]; ok {
		return true
	}
	if _, ok := c.dynKinds[key]; ok {
		return true
	}
	if _, typed := typedKinds[kind]; typed {
		return false
	}
	if kind == KindCustomResourceDefinition {
		return false
	}
	for _, dk := range c.discovered {
		if dk.RegistryKind() == kind {
			return false
		}
	}
	return true
}

// kindSyncedLockedKey is KindSynced's cache-state half for an already-
// resolved key, without the stall fallback — the question "has this cache
// actually filled", which is also what decides whether a watch error is an
// initial-LIST failure or ordinary post-sync churn. c.mu must be held.
func (c *Cluster) kindSyncedLockedKey(key scopeKey) bool {
	if c.stopCh == nil {
		return true
	}
	if c.kindFailed[key] != nil {
		return true
	}
	if key.kind == KindHelmRelease {
		// Releases come from their own filtered Secret cache, not the
		// shared one, so this must not answer for KindSecret — and there is
		// one such cache per namespace read, so it answers for exactly the
		// namespace this key names.
		inf := c.helmInformers[key.namespace]
		return inf != nil && inf.HasSynced()
	}
	if inf, ok := c.kindInformers[key]; ok {
		return inf.HasSynced()
	}
	if info, ok := c.dynKinds[key]; ok {
		return info.informer != nil && info.informer.HasSynced()
	}
	if _, typed := typedKinds[key.kind]; typed {
		// A real kind whose informer hasn't been registered yet at this
		// scope.
		return false
	}
	for _, dk := range c.discovered {
		if dk.RegistryKind() == key.kind {
			// A known discovered CRD kind whose instance informer hasn't
			// been started at this scope yet — not started is not synced.
			// Reporting true here (the old fallback) let a discovered kind
			// read as "settled" before its first read, which is exactly
			// what Goto's fuzzy resource corpus (goto.go's kindSynced guard)
			// and the empty-state hints use to decide a kind is safe to
			// list — so both would list it, starting its informer just to
			// render a jump entry or an empty-state hint. Only a genuinely
			// unknown/undiscovered kind (never in this loop) still falls
			// through to true below.
			return false
		}
	}
	return true
}

// KindError reports why kind's cache is empty, when it is empty for a reason
// other than "the cluster has none of them": the last error from an initial
// LIST that has yet to succeed. Nil once the cache fills — including for a
// kind that failed a few times and then recovered.
//
// This is the other half of the no-hanging-spinner contract. KindSynced
// saying "settled" is what gets a screen off its spinner; without a reason to
// show alongside it, the screen would report an empty cluster instead of a
// failed read, which is a worse lie than the spinner.
//
// Permission failures are deliberately not reported here. They travel their
// own path (kindFailed, and the Forbidden error a screen's own read returns)
// with a dedicated permission-denied state at the other end.
func (c *Cluster) KindError(kind ResourceKind, namespace string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.scopeKeyForLocked(kind, namespace)
	if c.kindSyncedLockedKey(key) {
		delete(c.kindStalled, key)
		return nil
	}
	return c.kindStalled[key]
}

// noteWatchError records err against (kind, namespace) — already the
// resolved scope, not a raw request namespace, since every caller passes
// through cacheScopeLocked (or, for Helm, its own namespace) before calling
// this — from the reflector's own goroutine. A watch dropping after the
// cache has filled is ordinary churn — the informer re-establishes it and
// the screen keeps rendering real data, so it is not this kind's problem. A
// failure *before* the initial LIST has ever succeeded is: nothing is on
// screen, nothing is coming, and the reflector will retry the same failing
// request indefinitely.
//
// One failure is enough to say so. The read is retried regardless, and the
// stall clears the moment a retry lands, so the cost of speaking up early is
// a message that corrects itself while the alternative is silence for as long
// as the user is willing to wait.
//
// gen is the caller's own registration-time generation, re-checked against
// c.generation inside the same critical section as the write: SwitchContext
// bumps c.generation and clears kindStalled/kindFailed under this same c.mu
// (see its own doc comment), so either this write happens entirely before
// that critical section, or it sees the bumped generation and no-ops — there
// is no window in between for it to land in the new context's freshly-
// cleared maps under an old context's error. Kept as a thin wrapper around
// noteWatchErrorLocked so recordWatchError can fold this write and the
// connection-health update that follows it into one lock acquisition.
func (c *Cluster) noteWatchError(gen int, kind ResourceKind, namespace string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		return
	}
	c.noteWatchErrorLocked(kind, namespace, err)
}

// noteWatchErrorLocked is noteWatchError's write, for a caller that already
// holds c.mu and has already checked the generation. c.mu must be held.
func (c *Cluster) noteWatchErrorLocked(kind ResourceKind, namespace string, err error) {
	if IsPermissionError(err) {
		c.markKindFailedLocked(kind, namespace, err)
		return
	}
	key := scopeKey{kind, namespace}
	if c.kindSyncedLockedKey(key) {
		return
	}
	if c.kindStalled == nil {
		c.kindStalled = map[scopeKey]error{}
	}
	c.kindStalled[key] = err
}

// recordWatchError folds one watch failure into the per-kind cache state
// (noteWatchErrorLocked) and connection health (health.onWatchError) as a
// single atomic operation — the fix for a race the two-call version left
// open. SwitchContext's generation bump and its call to health.reset() both
// run inside its own c.mu critical section, so holding c.mu across the gen
// check, the kindFailed/kindStalled write, *and* the health.onWatchError
// call is what actually closes the window: a callback belonging to a context
// SwitchContext has already replaced can no longer call health.onWatchError
// after the replacement context's health has been reset, because either this
// whole function runs to completion before SwitchContext's critical section
// starts, or it sees the bumped generation at the top and no-ops before
// touching health at all. The previous shape re-checked the generation
// atomically with the *state write* (noteWatchError/markKindFailed already
// did that) but called health.onWatchError afterward, unlocked relative to
// c.mu — exactly the gap a SwitchContext could land in between the write and
// the health call, flipping a freshly-reset context's health back to
// Reconnecting/Unauthenticated using the old context's error.
func (c *Cluster) recordWatchError(gen int, kind ResourceKind, namespace string, err error) {
	err = c.health.resolveCredentialError(err)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		return
	}
	key := scopeKey{kind, namespace}
	wasSynced := c.kindSyncedLockedKey(key)
	c.noteWatchErrorLocked(kind, namespace, err)
	// Initial LIST failures are already represented by kindStalled and clear
	// when HasSynced flips, including for a legitimately empty cache where no
	// Add event can ever arrive. watchUnhealthy is only the post-sync case.
	if wasSynced && !IsPermissionError(err) {
		if c.watchUnhealthy == nil {
			c.watchUnhealthy = map[scopeKey]bool{}
		}
		c.watchUnhealthy[key] = true
	}
	c.health.onWatchError(err, c.allStartedKindsReadyLocked(), c.linkProvenLocked(), time.Now())
}

// recordWatchEnded is the transport observer's report that an established
// WATCH body ended. A clean end — io.EOF, or the reader closing it — is how
// every watch rotates when the apiserver's timeoutSeconds expires, so it only
// marks that informer's watch unhealthy until its replacement WATCH is
// established (which keeps a /livez success from turning the header green
// across an HTTP 410's streaming relist, the case body-end detection exists
// for). It never reaches health.onWatchError: reporting it there flipped the
// header to Reconnecting on every routine rotation of every informer. A real
// failure still does — any other read error goes through recordWatchError,
// and a replacement WATCH that can't be opened reaches the reflector's own
// WatchErrorHandler.
func (c *Cluster) recordWatchEnded(gen int, kind ResourceKind, namespace string, err error) {
	if !errors.Is(err, io.EOF) && !errors.Is(err, errWatchStreamClosed) {
		c.recordWatchError(gen, kind, namespace, err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		return
	}
	key := scopeKey{kind, namespace}
	if !c.kindSyncedLockedKey(key) {
		return
	}
	if c.watchUnhealthy == nil {
		c.watchUnhealthy = map[scopeKey]bool{}
	}
	c.watchUnhealthy[key] = true
}

// recordWatchEstablished clears a post-sync watch failure when the matching
// HTTP WATCH response succeeds. Unlike an object handler this also fires for
// a legitimately empty collection.
func (c *Cluster) recordWatchEstablished(gen int, kind ResourceKind, namespace string) {
	c.mu.Lock()
	if c.generation != gen {
		c.mu.Unlock()
		return
	}
	key := scopeKey{kind, namespace}
	_, wasUnhealthy := c.watchUnhealthy[key]
	delete(c.watchUnhealthy, key)
	recovered := wasUnhealthy && len(c.watchUnhealthy) == 0
	c.mu.Unlock()
	if recovered {
		c.health.retryNow()
	}
}

func (c *Cluster) recordAuthenticationFailure(gate *authenticationGate, err error) {
	err = c.health.resolveCredentialError(err)
	c.mu.Lock()
	if c.authGate != gate {
		c.mu.Unlock()
		return
	}
	prev := c.health.get()
	c.health.setUnauthenticated(err, 0, prev)
	c.mu.Unlock()
}

// allStartedKindsSynced reports whether every informer started so far has
// finished its initial LIST and no observed watch failure is still awaiting
// a recovered event. This is what the health loop keys off, rather than
// Synced: with informers starting on demand,
// the burst of LIST traffic that starves the /livez ping no longer happens
// only at connect time — opening the Secrets list on a constrained link
// produces exactly the same contention mid-session. Asking "is anything
// currently listing" covers both, where "have we connected yet" covers only
// the first.
func (c *Cluster) allStartedKindsSynced() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allStartedKindsReadyLocked()
}

// allStartedKindsReadyLocked requires both an initial cache population and
// recovery from every watch error observed since it. c.mu must be held.
func (c *Cluster) allStartedKindsReadyLocked() bool {
	return c.allStartedKindsSyncedLocked() && len(c.watchUnhealthy) == 0
}

// allStartedKindsSyncedLocked is allStartedKindsSynced's body for a caller
// that already holds c.mu — recordWatchError, so its health.onWatchError
// call reads sync state from inside the same critical section as its
// generation check rather than re-acquiring the lock. c.mu must be held.
//
// Sweeps only while the answer can still be no: once every started informer
// has synced it latches (see allKindsSynced), and a later start clears the
// latch. Callers on a hot path keep the lock hold O(1) in the steady state.
func (c *Cluster) allStartedKindsSyncedLocked() bool {
	if c.allKindsSynced {
		return true
	}
	if c.allStartedKindsSweepLocked() {
		c.allKindsSynced = true
		return true
	}
	return false
}

// noteInformerStartedLocked invalidates the allKindsSynced latch. Every site
// that adds an entry to kindInformers/dynKinds/helmInformers must call it in
// the same critical section as the write: a freshly started informer has not
// synced, so a stale latch would claim otherwise and hand the health loop's
// connect-grace window the wrong answer for exactly the LIST burst it exists
// to forgive. c.mu must be held.
func (c *Cluster) noteInformerStartedLocked() { c.allKindsSynced = false }

// allStartedKindsSweepLocked is the unmemoized sweep. c.mu must be held.
func (c *Cluster) allStartedKindsSweepLocked() bool {
	for key, inf := range c.kindInformers {
		if c.kindFailed[key] == nil && !inf.HasSynced() {
			return false
		}
	}
	for key, info := range c.dynKinds {
		if c.kindFailed[key] == nil && info.informer != nil && !info.informer.HasSynced() {
			return false
		}
	}
	for ns, inf := range c.helmInformers {
		if c.kindFailed[scopeKey{KindHelmRelease, ns}] == nil && !inf.HasSynced() {
			return false
		}
	}
	return true
}

// noteReachedIfAnyCacheSyncedLocked latches Reached when at least one started
// informer has completed its initial LIST. c.mu must be held.
//
// This is the synchronous counterpart to notifyScope's latch, and it is what
// makes Reached reliable rather than merely likely: a shared informer delivers
// to its listeners on its own goroutine, so the AddFunc that latches can still
// be queued when the cache is already filled and readable — which is the exact
// instant the UI reads rows out of it. ping calls this immediately before
// folding a probe result into ConnState, so the latch is already set for the
// very probe that reports an outage.
//
// "Any", not "all": one filled cache is one screenful of real data worth
// keeping, which is the whole question Reached is asked.
func (c *Cluster) noteReachedIfAnyCacheSyncedLocked() {
	if c.reached {
		return
	}
	for _, inf := range c.kindInformers {
		if inf.HasSynced() {
			c.reached = true
			return
		}
	}
	for _, info := range c.dynKinds {
		if info.informer != nil && info.informer.HasSynced() {
			c.reached = true
			return
		}
	}
	for _, inf := range c.helmInformers {
		if inf.HasSynced() {
			c.reached = true
			return
		}
	}
}

// markKindFailed records that (kind, namespace)'s watch reported an error the
// cache will never recover from on its own — today only "you may not list
// this", which is a permanent answer for the session, not a transient
// outage. namespace is already the resolved scope, same as noteWatchError.
// gen is checked against c.generation under the same lock as the write —
// see noteWatchError's doc comment for why the caller's own up-front
// generationCurrent check isn't enough on its own.
func (c *Cluster) markKindFailed(gen int, kind ResourceKind, namespace string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		return
	}
	c.markKindFailedLocked(kind, namespace, err)
}

// markKindFailedLocked is markKindFailed's write, for a caller that already
// holds c.mu and has already checked the generation. c.mu must be held.
func (c *Cluster) markKindFailedLocked(kind ResourceKind, namespace string, err error) {
	if c.kindFailed == nil {
		c.kindFailed = map[scopeKey]error{}
	}
	c.kindFailed[scopeKey{kind, namespace}] = err
}

// KindForbidden reports the denial behind a kind whose cache will stay empty
// because this identity may not list it, or nil for any other reason a cache
// is empty.
//
// It is the missing half of the pair KindSynced/KindError form. A forbidden
// cache reports *settled* — it is never going to fill, so a spinner gated on
// it would outlive the user — and KindError deliberately stays nil for it,
// because that map is for an initial LIST that is still failing and may yet
// succeed, which a 403 will not. Between them that left a screen with
// "settled, no reason, no rows", and the only sentence available for those
// three facts is "the cluster has none of this kind" — which is false, and
// which the empty state stated outright, under a green connected header.
//
// Latched for the session on purpose: RBAC does not change under a running
// process, and a screen that reverted to claiming emptiness on the next
// reload would be the same lie with a delay. A denial for one namespace's
// scope (scoped mode) says nothing about another's.
func (c *Cluster) KindForbidden(kind ResourceKind, namespace string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.scopeKeyForLocked(kind, namespace)
	return c.kindFailed[key]
}

// CurrentNamespace and CurrentContext expose the active scope for switchers.
func (c *Cluster) CurrentNamespace() string { return c.Context.Namespace }
func (c *Cluster) CurrentContext() string   { return c.Context.ContextName }

// Contexts lists the kubeconfig context names available to switch to.
func (c *Cluster) Contexts() []string {
	names, _, err := AvailableContexts()
	if err != nil {
		return nil
	}
	return names
}

// SwitchNamespace changes the active namespace. In cluster-wide mode
// informers watch all namespaces, so this is a cheap filter change with no
// cache rebuild; ListRaw and the screens pick up the new scope on their next
// load. In scoped mode it's the same cheap change — the caches themselves
// don't move, ListRaw just starts a differently-scoped one on first read of
// the new namespace, same as any other namespace switch always has.
func (c *Cluster) SwitchNamespace(namespace string) {
	if namespace == "" {
		return
	}
	c.mu.Lock()
	c.Context.Namespace = namespace
	c.mu.Unlock()
}

// SwitchContext rebuilds the clientset, metrics client, and informer
// factories against a different kubeconfig context, then restarts the
// informers. namespace, when non-empty and this Cluster is in scoped mode,
// overrides the new context's own default namespace with the caller's
// already-resolved restore target (the target context's own persisted
// per-context namespace) — the same thing SetNamespaceScope does for the
// very first Start, so the eager Pod cache scopes to the namespace the UI is
// actually about to show rather than the kubeconfig context's default.
// Ignored in cluster-wide mode, and ignored entirely when empty, so this is
// a strict no-op for every caller that predates scoped mode.
//
// The events channel is preserved, so a caller already ranging over Events
// keeps receiving notifications from the new cluster. It blocks until the
// new caches sync (or ctx is done).
func (c *Cluster) SwitchContext(ctx context.Context, contextName, namespace string) error {
	client, err := NewClientForContext(contextName)
	if err != nil {
		return err
	}
	metrics, _ := metricsclient.NewForConfig(client.RESTConfig)
	dynClient, _ := dynamic.NewForConfig(client.RESTConfig)

	c.mu.Lock()
	// Bump generation before touching anything else: a late watch-error/
	// notify callback from the context being replaced re-checks its captured
	// gen against c.generation under this same c.mu (noteWatchError/
	// markKindFailed/notify's own doc comments), so this has to be current
	// before kindFailed/kindStalled are cleared below, not after — Start
	// bumps it again once informers are re-registered, which is redundant
	// but harmless (nothing depends on the exact delta, only equality
	// against the current value), and closes the window where a callback
	// could otherwise pass its generation check by arriving after the clear
	// below but before Start's own bump.
	c.generation++
	if c.cancelHealthProbe != nil {
		c.cancelHealthProbe()
		c.cancelHealthProbe = nil
	}
	// Tear down the current informers before swapping in the new factory.
	if c.stopCh != nil {
		close(c.stopCh)
	}
	c.stopCh = make(chan struct{})
	c.clientset = client.Interface
	c.authGate = client.authGate
	c.watcher = client.watcher
	client.authGate.onBlocked = func(err error) { c.recordAuthenticationFailure(client.authGate, err) }
	c.health.setStaticCredentials(client.authGate.staticCredentials)
	c.health.setCredentialStderr(client.pluginStderr)
	c.metrics = metrics
	c.restCfg = client.RESTConfig
	c.credentialCfg = client.credentialConfig
	c.factory = newTypedFactory(client.Interface)
	c.scopedFactories = nil
	c.dynClient = dynClient
	c.dynFactory = dynamicinformer.NewDynamicSharedInformerFactory(dynClient, defaultResync)
	c.dynScopedFactories = nil
	c.dynKinds = nil
	c.discovered = nil
	c.crdColumnsFetched = nil
	c.crdColumnsInFlight = nil
	// The new factory's informers are all unregistered and unsynced, and
	// whatever the old cluster forbade says nothing about this one.
	c.kindInformers = nil
	c.kindFailed = nil
	c.kindStalled = nil
	c.watchUnhealthy = nil
	// Belt and braces: Start re-registers the eager informers through
	// noteInformerStartedLocked, which clears this anyway, but a latch left
	// standing across a wholesale swap of the maps it summarizes is the kind
	// of thing that only stays correct by accident.
	c.allKindsSynced = false
	c.eagerKeys = nil
	c.metaClient = nil
	c.helmFactories = nil
	c.helmInformers = nil
	c.Context = client.Context
	if c.scoped && namespace != "" {
		c.Context.Namespace = namespace
	}
	c.health.reset()
	c.started = false
	c.synced = false
	// The old context's proof says nothing about this one: switching into an
	// unreachable context has to be able to reach 4c's setup screen again.
	c.reached = false
	c.mu.Unlock()

	return c.Start(ctx)
}

// Stop tears down the informers.
func (c *Cluster) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Invalidate in-flight health probes before cancelling them. A transport
	// is allowed to return a result after its request context is cancelled;
	// the generation guard in ping keeps that stale result from landing.
	c.generation++
	if c.cancelHealthProbe != nil {
		c.cancelHealthProbe()
		c.cancelHealthProbe = nil
	}
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
}

// ListRaw returns the cached objects of kind in namespace ("" for all
// namespaces; ignored for cluster-scoped kinds). It satisfies
// resources.RawLister. Named reads never implicitly consult or start a
// broader cache than the one namespace normalizes to — an explicit ""
// starts (or reads) the global cache on demand, even in scoped mode.
func (c *Cluster) ListRaw(_ context.Context, kind ResourceKind, namespace string) ([]runtime.Object, error) {
	if tk, ok := typedKinds[kind]; ok {
		// Start this kind's informer at the scope namespace normalizes to,
		// if nothing has yet. The first read therefore returns an empty
		// cache, which is exactly what KindSynced is for — the caller sees
		// "not synced", holds its loading state, and the informer's own
		// change events bring it back once objects land.
		//
		// The factory comes back from the same critical section that
		// registered the informer: SwitchContext replaces it wholesale, and
		// one looked up afterwards could belong to the next cluster, where
		// reading its Lister() registers an informer with no handlers.
		_, f := c.ensureKind(kind, namespace)
		if f == nil {
			return nil, nil
		}
		return tk.list(f, namespace, labels.Everything())
	}
	c.ensureDynamicKindFor(kind, namespace)
	scope := c.cacheScope(kind, namespace)
	if info, ok := c.getDynKind(kind, scope); ok {
		// Reading a custom kind is the moment its columns become worth
		// fetching. Fire-and-forget: this is on the update loop's path.
		c.ensurePrinterColumns(kind)
		return listDynamic(info, namespace)
	}
	return nil, fmt.Errorf("no informer registered for kind %s", kind)
}

// DiscoveredKinds returns the last discovery pass's parsed CRD cache
// (docs/design README.md's "discovery" state entry) — feeds
// resources.BuildDiscoveredRegistry.
func (c *Cluster) DiscoveredKinds() []DiscoveredKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DiscoveredKind(nil), c.discovered...)
}

// CountInstances reads a dynamically registered kind's cluster-wide informer
// cache length — the 14b CRDs list's live COUNT column. ok is false whenever
// that length isn't the answer: no registered informer (not yet discovered,
// or discovery hasn't run), a cache still on its initial LIST, or one that
// is Forbidden (always the case under --namespace-scoped, since this read is
// cluster-wide) or failing — "counts are never fabricated", so the column
// shows unknown rather than a 0 the cluster never reported.
//
// This starts the kind's instance informer if discovery knows it, so
// opening the CRDs list opens a watch per discovered kind. That's the one
// place laziness doesn't help: it's strictly better than the old behavior
// (which opened them all at connect, list or no list), but the column
// really wants a server-side count rather than a cache length. Always
// cluster-wide (namespace ""), regardless of scoped/cluster-wide mode — the
// CRDs list itself is a cluster-wide screen.
func (c *Cluster) CountInstances(kind ResourceKind) (int, bool) {
	c.ensureDynamicKindFor(kind, "")
	info, ok := c.getDynKind(kind, "")
	if !ok {
		return 0, false
	}
	if !c.KindSynced(kind, "") || c.KindError(kind, "") != nil || c.KindForbidden(kind, "") != nil {
		return 0, false
	}
	objs, err := listDynamic(info, "")
	if err != nil {
		return 0, false
	}
	return len(objs), true
}

// PodMetricsByNamespace fetches all pod metrics in namespace in a single List,
// replacing the previous per-pod Get (the N+1 loop). Keyed by PodKey, not by
// bare name: callers pass "" for cluster-wide reads (11b's node detail, and
// browse in all-namespaces mode), where two pods with the same name in
// different namespaces — a DaemonSet, or one chart installed twice — would
// otherwise overwrite each other and show one pod the other's usage.
func (c *Cluster) PodMetricsByNamespace(ctx context.Context, namespace string) (map[string]PodMetrics, error) {
	if c.metrics == nil {
		return nil, fmt.Errorf("pod metrics client is not configured")
	}
	list, err := c.metrics.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]PodMetrics, len(list.Items))
	for i := range list.Items {
		pm := list.Items[i]
		if len(pm.Containers) == 0 {
			out[PodKey(pm.Namespace, pm.Name)] = PodMetrics{CPU: "n/a", MEM: "n/a"}
			continue
		}
		cpu := pm.Containers[0].Usage.Cpu().DeepCopy()
		mem := pm.Containers[0].Usage.Memory().DeepCopy()
		for j := 1; j < len(pm.Containers); j++ {
			cpu.Add(*pm.Containers[j].Usage.Cpu())
			mem.Add(*pm.Containers[j].Usage.Memory())
		}
		out[PodKey(pm.Namespace, pm.Name)] = PodMetrics{CPU: FormatCPU(cpu), MEM: FormatMemory(mem), CPUMilli: cpu.MilliValue(), MemBytes: mem.Value()}
	}
	return out, nil
}

// ContainerMetricsByNamespace fetches all pod metrics in namespace in a
// single List, like PodMetricsByNamespace, but keeps each container's own
// usage separate instead of summing them — 25a's per-field USAGE bar needs
// the active container's own number, not the whole pod's. Keyed by PodKey
// for consistency with PodMetricsByNamespace; this one has no cluster-wide
// caller today, so the namespace in the key is uniformity, not a fix.
func (c *Cluster) ContainerMetricsByNamespace(ctx context.Context, namespace string) (map[string]map[string]PodMetrics, error) {
	if c.metrics == nil {
		return nil, fmt.Errorf("pod metrics client is not configured")
	}
	list, err := c.metrics.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]PodMetrics, len(list.Items))
	for i := range list.Items {
		pm := list.Items[i]
		containers := make(map[string]PodMetrics, len(pm.Containers))
		for _, ctr := range pm.Containers {
			cpu := ctr.Usage.Cpu()
			mem := ctr.Usage.Memory()
			containers[ctr.Name] = PodMetrics{
				CPU: FormatCPU(*cpu), MEM: FormatMemory(*mem),
				CPUMilli: cpu.MilliValue(), MemBytes: mem.Value(),
			}
		}
		out[PodKey(pm.Namespace, pm.Name)] = containers
	}
	return out, nil
}

// NodeMetrics fetches live CPU/MEM usage for every node in one List, keyed
// by node name — the 11a nodes-list bars' numerator. A nil metrics client
// (no metrics-server) reports the same "not configured" error
// PodMetricsByNamespace does, so callers degrade identically either way.
func (c *Cluster) NodeMetrics(ctx context.Context) (map[string]NodeMetric, error) {
	if c.metrics == nil {
		return nil, fmt.Errorf("node metrics client is not configured")
	}
	list, err := c.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]NodeMetric, len(list.Items))
	for i := range list.Items {
		nm := list.Items[i]
		cpu := nm.Usage.Cpu().DeepCopy()
		mem := nm.Usage.Memory().DeepCopy()
		out[nm.Name] = NodeMetric{CPU: FormatCPU(cpu), MEM: FormatMemory(mem), CPUMilli: cpu.MilliValue(), MemBytes: mem.Value()}
	}
	return out, nil
}

// listNamespaced dispatches to the cluster-wide or per-namespace lister. The
// generic signatures let one helper serve every namespaced kind.
func listNamespaced[T runtime.Object, N interface {
	List(labels.Selector) ([]T, error)
}](
	all func(labels.Selector) ([]T, error),
	scoped func(string) N,
	namespace string,
	sel labels.Selector,
) ([]runtime.Object, error) {
	if namespace == "" {
		items, err := all(sel)
		return toObjects(items), err
	}
	items, err := scoped(namespace).List(sel)
	return toObjects(items), err
}

// listAll is listNamespaced's cluster-scoped counterpart, for the kinds that
// have no per-namespace lister at all (Node, Namespace, ClusterRole,
// ClusterRoleBinding).
func listAll[T runtime.Object](all func(labels.Selector) ([]T, error), sel labels.Selector) ([]runtime.Object, error) {
	items, err := all(sel)
	return toObjects(items), err
}

func toObjects[T runtime.Object](items []T) []runtime.Object {
	out := make([]runtime.Object, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	return out
}
