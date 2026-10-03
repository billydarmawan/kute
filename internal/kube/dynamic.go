package kube

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// getDynKind guards dynKinds with the same mutex as every other Cluster
// field — refreshDiscovery/ensureDynamicKind run inside Start (background
// goroutine in app.RunWithConfig) while ListRaw is called from the Bubble
// Tea Update loop, so this map is genuinely cross-goroutine. There is no
// paired setter: ensureDynamicKind is the only writer and holds the lock
// across registration and Start, which a separate setter would break.
func (c *Cluster) getDynKind(kind ResourceKind, scope string) (dynamicKindInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, ok := c.dynKinds[scopeKey{kind, scope}]
	return info, ok
}

// ensureDynamicKind idempotently starts watching gvr under kind at scope
// ("" for the cluster-wide instance, the built-in
// KindCustomResourceDefinition's only shape): the built-in
// KindCustomResourceDefinition (registered once, on first 14b read) or a
// discovered custom kind (registered by ensureDynamicKindFor once its CRD is
// seen to be Established+Served). Safe to call repeatedly — the target
// factory's Start only starts informers that haven't already been started.
//
// The whole body runs under c.mu, which does two things: it keeps the
// dynFactory/stopCh reads from racing SwitchContext's wholesale replacement
// of both, and it makes registration-then-Start atomic, so a concurrent
// caller's Start can't run this informer before its handlers are attached.
func (c *Cluster) ensureDynamicKind(kind ResourceKind, scope string, gvr schema.GroupVersionResource, namespaced bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := scopeKey{kind, scope}
	if _, ok := c.dynKinds[key]; ok || c.stopCh == nil {
		return
	}
	f := c.dynFactoryForScopeLocked(scope)
	informer := f.ForResource(gvr)
	// The dynamic factory takes no transform option, so set it per informer
	// — which must happen before it starts. Same reasoning as the typed
	// factory's: managedFields is bookkeeping nothing here reads.
	// Best-effort: a failure just means this cache keeps managedFields.
	_ = informer.Informer().SetTransform(stripManagedFields)
	k := kind
	gen := c.generation
	if c.watcher != nil {
		c.watcher.register(gvr, scope, "", func() {
			c.recordWatchEstablished(gen, k, scope)
		}, func(err error) {
			c.recordWatchEnded(gen, k, scope, err)
		})
	}
	// Same reason the typed informers have one
	// (registerTypedWatchLocked): a discovered kind can be forbidden, or
	// its initial LIST can keep failing, and without this the only symptom
	// is a screen that never leaves its spinner.
	// Best-effort: a failed registration just means no health signal from
	// this informer.
	// recordWatchError re-checks gen atomically with both the state write
	// and the health.onWatchError call — see its doc comment.
	_ = informer.Informer().SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		c.recordWatchError(gen, k, scope, err)
	})
	// Handler registration errors are non-fatal for a read-only UI.
	_, _ = informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.notifyScope(gen, k, scope) },
		UpdateFunc: func(any, any) { c.notifyScope(gen, k, scope) },
		DeleteFunc: func(any) { c.notifyScope(gen, k, scope) },
	})
	if c.dynKinds == nil {
		c.dynKinds = map[scopeKey]dynamicKindInfo{}
	}
	c.dynKinds[key] = dynamicKindInfo{
		gvr:        gvr,
		namespaced: namespaced,
		lister:     informer.Lister(),
		informer:   informer.Informer(),
	}
	c.noteInformerStartedLocked()
	f.Start(c.stopCh)
	c.health.noteListBurst()
}

// refreshDiscovery builds the per-context list of custom kinds (docs/design
// README.md §14c: "cached per context"). Called once at the end of
// Start/SwitchContext, not continuously re-run mid-session.
//
// It deliberately does not read CRD *objects*. A CustomResourceDefinition
// carries the full OpenAPI v3 validation schema for every version it serves,
// which on a cluster running a few operators is megabytes — 2.76 MB across 48
// CRDs on the cluster this was measured against, of which 1.8% was anything
// kute reads. Listing them to learn a handful of names was the single largest
// thing a connect pulled.
//
// Instead, two cheap reads that together say the same thing:
//
//   - a PartialObjectMetadata list of CRDs, which is just names — and a CRD's
//     name is exactly "<plural>.<group>", so it identifies precisely which
//     group/resource pairs are custom without any hardcoded list of built-in
//     API groups (a list that would get Gateway API wrong, since
//     gateway.networking.k8s.io is CRD-backed despite the k8s.io suffix);
//   - the discovery API, which supplies each one's Kind, served version and
//     scope.
//
// Printer columns are the one thing neither provides, and they only live in
// the part of a CRD that makes it huge. Those are fetched per-kind on first
// use — see ensurePrinterColumns.
//
// gen and cs are the generation and clientset Start snapshotted under c.mu:
// reading c.clientset here would race SwitchContext's write of it, and the
// result is dropped unless gen is still current, so a superseded Start can't
// hand the new context the old cluster's CRDs.
func (c *Cluster) refreshDiscovery(ctx context.Context, gen int, cs kubernetes.Interface) {
	crdNames, err := c.listCRDNames(ctx)
	if err != nil || len(crdNames) == 0 {
		return
	}
	custom, err := discoverCustomResources(cs, crdNames)
	if err != nil && len(custom) == 0 {
		return
	}
	c.mu.Lock()
	if c.generation == gen {
		c.discovered = custom
	}
	c.mu.Unlock()
}

// listCRDNames returns every CRD's metadata.name ("<plural>.<group>"),
// fetched as PartialObjectMetadata so the schemas never leave the server.
func (c *Cluster) listCRDNames(ctx context.Context) (map[string]bool, error) {
	client, err := c.metadataClient()
	if err != nil {
		return nil, err
	}
	list, err := client.Resource(crdGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		names[list.Items[i].Name] = true
	}
	return names, nil
}

// discoverCustomResources turns the discovery API's served-resource list into
// DiscoveredKinds, keeping only the resources whose "<name>.<group>" appears
// in crdNames. Established needs no separate check: a resource the API server
// is serving is, by definition, established.
//
// A partial-discovery error is tolerated and its usable half kept — one
// unreachable aggregated API service (a metrics adapter, typically) must not
// cost the user every other custom kind on the cluster.
func discoverCustomResources(cs kubernetes.Interface, crdNames map[string]bool) ([]DiscoveredKind, error) {
	_, lists, err := cs.Discovery().ServerGroupsAndResources()
	if lists == nil {
		return nil, err
	}
	return customKindsFrom(lists, crdNames), err
}

// customKindsFrom is discoverCustomResources' pure half: match served
// resources against the CRD names and shape the survivors. Split out so the
// matching rules are testable without a discovery client.
func customKindsFrom(lists []*metav1.APIResourceList, crdNames map[string]bool) []DiscoveredKind {
	var out []DiscoveredKind
	for _, list := range lists {
		gv, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil || gv.Group == "" {
			continue
		}
		for _, r := range list.APIResources {
			// Subresources ("widgets/status") are not browsable kinds.
			if strings.Contains(r.Name, "/") || r.Kind == "" {
				continue
			}
			if !crdNames[r.Name+"."+gv.Group] {
				continue
			}
			out = append(out, DiscoveredKind{
				GVR:           schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: r.Name},
				Kind:          r.Kind,
				Plural:        r.Name,
				Group:         gv.Group,
				ClusterScoped: !r.Namespaced,
				Established:   true,
				CRDName:       r.Name + "." + gv.Group,
			})
		}
	}
	return dedupeDiscovered(out)
}

// dedupeDiscovered keeps one entry per (Group, Kind). A CRD serving several
// versions appears once per version in discovery; the preferred version
// sorts first within a group, so the first sighting wins.
//
// Deduping by registry key instead silently dropped a whole kind whenever
// two groups served the same Kind (Istio's and Gateway API's Gateway):
// whichever discovery listed second was simply never seen. Same-Kind kinds
// from different groups are kept, and AssignRegistryKinds gives them
// distinct keys.
func dedupeDiscovered(in []DiscoveredKind) []DiscoveredKind {
	type groupKind struct{ group, kind string }
	seen := make(map[groupKind]bool, len(in))
	out := make([]DiscoveredKind, 0, len(in))
	for _, dk := range in {
		gk := groupKind{dk.Group, dk.Kind}
		if seen[gk] {
			continue
		}
		seen[gk] = true
		out = append(out, dk)
	}
	slices.SortFunc(out, func(a, b DiscoveredKind) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Group, b.Group))
	})
	return AssignRegistryKinds(out)
}

// ensureDynamicKindFor starts the instance informer for a discovered kind by
// name, scoped to whatever namespace normalizes to under the current mode,
// the dynamic counterpart of ensureKind. Reports whether kind is something
// discovery knows about at all — a false answer is what lets ListRaw tell
// "no such kind" apart from "not watched yet".
func (c *Cluster) ensureDynamicKindFor(kind ResourceKind, namespace string) bool {
	c.mu.Lock()
	scope := c.cacheScopeLocked(kind, namespace)
	if _, ok := c.dynKinds[scopeKey{kind, scope}]; ok {
		c.mu.Unlock()
		return true
	}
	if kind == KindCustomResourceDefinition {
		// 14b's own list, the one screen that wants whole CRD objects —
		// schemas and all, and always cluster-wide. Nothing else does, so
		// this informer starts only when that screen is opened, not at
		// connect.
		c.mu.Unlock()
		c.ensureDynamicKind(KindCustomResourceDefinition, "", crdGVR, false)
		return true
	}
	var match DiscoveredKind
	var found bool
	for _, dk := range c.discovered {
		if dk.RegistryKind() == kind && dk.Established && dk.GVR.Version != "" {
			match, found = dk, true
			break
		}
	}
	c.mu.Unlock()
	if !found {
		return false
	}
	c.ensureDynamicKind(kind, scope, match.GVR, !match.ClusterScoped)
	return true
}

// listDynamic lists a dynamically registered kind's cache, scoped by
// namespace when the kind is namespaced and namespace is non-empty —
// mirrors listNamespaced's typed-lister dispatch for the generic
// cache.GenericLister shape.
func listDynamic(info dynamicKindInfo, namespace string) ([]runtime.Object, error) {
	if !info.namespaced || namespace == "" {
		return info.lister.List(labels.Everything())
	}
	return info.lister.ByNamespace(namespace).List(labels.Everything())
}

// ensurePrinterColumns fetches one CRD's declared additionalPrinterColumns
// and folds them into the discovery cache, once per kind per context.
//
// Columns are the only thing refreshDiscovery can't get cheaply: they live in
// spec.versions[], the same part of a CRD that carries the megabytes of
// OpenAPI schema. So rather than pay that for every CRD at connect, one CRD
// is fetched the first time its kind is actually read — typically none per
// session, occasionally one or two.
//
// Until it lands the kind renders with neutral NAME/AGE columns, which
// §14a's spec already provides for ("printer columns → columns, Ready-style
// condition → status, else neutral"). Reports whether anything changed, so
// the caller only announces a registry rebuild when there's something new.
// ensurePrinterColumns kicks off the fetch and returns immediately. It must
// never block: ListRaw calls it, and ListRaw is called from the Bubble Tea
// update loop — a synchronous CRD Get there froze the whole app for as long
// as the round trip took, once per custom kind touched.
func (c *Cluster) ensurePrinterColumns(kind ResourceKind) {
	c.mu.Lock()
	if c.crdColumnsFetched[kind] || c.crdColumnsInFlight[kind] || c.hasCuratedColumnsLocked(kind) {
		c.mu.Unlock()
		return
	}
	if c.crdColumnsInFlight == nil {
		c.crdColumnsInFlight = map[ResourceKind]bool{}
	}
	c.crdColumnsInFlight[kind] = true
	gen := c.generation
	c.mu.Unlock()

	go func() {
		defer utilruntime.HandleCrash()
		ctx, cancel := context.WithTimeout(context.Background(), crdFetchTimeout)
		defer cancel()
		changed := c.fetchPrinterColumns(ctx, gen, kind)
		c.clearPrinterColumnsInFlight(gen, kind)
		if changed {
			// Announcing this as a CRD change is what prompts the registry
			// rebuild that puts the new columns on screen — but only for the
			// context this fetch was actually kicked off against; a slow Get
			// landing after SwitchContext must not rebuild the new context's
			// registry from the old one's CRD. notify itself already drops a
			// stale gen, but fetchPrinterColumns' own write is what has to
			// stay honest — see its doc comment.
			c.notifyColumnsArrived(gen)
		}
	}()
}

// hasCuratedColumnsLocked reports whether kind is one of the discovered kinds
// resources.BuildDiscoveredRegistry gives a curated descriptor with fixed
// columns — Flux (by group), Argo CD's Application and cert-manager's
// Certificate (by group and Kind) — so its CRD's printer columns would never
// be read, and fetching them is a full CRD Get (schema and all, and Flux's
// are among the largest) plus a registry rebuild for nothing. Keep the
// predicates in step with BuildDiscoveredRegistry's switch. c.mu must be
// held.
func (c *Cluster) hasCuratedColumnsLocked(kind ResourceKind) bool {
	for _, dk := range c.discovered {
		if dk.RegistryKind() != kind {
			continue
		}
		return IsFluxGroup(dk.Group) ||
			(IsArgoGroup(dk.Group) && dk.Kind == "Application") ||
			(dk.Group == CertManagerGroup && dk.Kind == string(KindCertificate))
	}
	return false
}

// clearPrinterColumnsInFlight drops kind's in-flight marker, gated on gen so
// a fetch belonging to a context SwitchContext has already replaced can't
// clear the *new* context's own in-flight marker for a same-named kind
// (crdColumnsInFlight is reset to a fresh map on SwitchContext, same as
// every other per-context cache, but the map identity changes while the key
// — a bare ResourceKind — does not).
func (c *Cluster) clearPrinterColumnsInFlight(gen int, kind ResourceKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		return
	}
	delete(c.crdColumnsInFlight, kind)
}

// crdFetchTimeout bounds one CRD Get. Columns are a nicety; a kind renders
// perfectly well with neutral ones.
const crdFetchTimeout = 15 * time.Second

// fetchPrinterColumns does the actual CRD Get and folds the result into
// c.discovered/crdColumnsFetched. gen is ensurePrinterColumns' registration-
// time generation, re-checked against c.generation in the same critical
// section as both reads of c.discovered — the one before the Get (so a
// context already replaced by SwitchContext doesn't even issue a request for
// a CRD name that belongs to the old one) and, more importantly, the one
// after it that performs the write. Without that second check, a slow Get
// landing after SwitchContext had already cleared and rebuilt
// discovered/crdColumnsFetched for a new context would still pass the first,
// now-stale crdName lookup and the unconditional `c.crdColumnsFetched[kind]
// = true` / `c.discovered[i].PrinterColumns = ...` write below — silently
// stamping the new context's freshly discovered kind (which happens to share
// the same Kind string) with the old context's CRD's columns, or marking a
// kind the new context has never fetched as already fetched so it never
// tries again.
func (c *Cluster) fetchPrinterColumns(ctx context.Context, gen int, kind ResourceKind) bool {
	c.mu.Lock()
	if c.generation != gen || c.crdColumnsFetched[kind] {
		c.mu.Unlock()
		return false
	}
	var crdName string
	for _, dk := range c.discovered {
		if dk.RegistryKind() == kind {
			crdName = dk.CRDName
			break
		}
	}
	dyn := c.dynClient
	c.mu.Unlock()
	if crdName == "" || dyn == nil {
		return false
	}

	obj, err := dyn.Resource(crdGVR).Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		// Leave it unfetched: a transient failure should retry on the next
		// read, and a permanent one (no permission to read CRDs) just means
		// this kind keeps its neutral columns.
		return false
	}
	full, ok := ParseDiscoveredKind(obj)
	if !ok {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != gen {
		// The context that requested this fetch has already been replaced.
		// c.discovered and c.crdColumnsFetched are the new context's own,
		// unrelated maps/slice now — writing this stale result into them
		// would corrupt state that has nothing to do with what was fetched.
		return false
	}
	if c.crdColumnsFetched == nil {
		c.crdColumnsFetched = map[ResourceKind]bool{}
	}
	c.crdColumnsFetched[kind] = true
	for i := range c.discovered {
		// The registry key, as the CRD-name lookup above used: a substituted
		// or qualified kind's bare Kind is not its key, and matching on it
		// dropped the columns (or landed them on a same-Kind neighbour) while
		// crdColumnsFetched already said they were in.
		if c.discovered[i].RegistryKind() != kind {
			continue
		}
		// Only the fields the cheap discovery pass couldn't supply. GVR
		// stays as discovery reported it — that's the version the server
		// actually serves, which is what the instance informer must watch.
		c.discovered[i].PrinterColumns = full.PrinterColumns
		c.discovered[i].Versions = full.Versions
		return len(full.PrinterColumns) > 0
	}
	return false
}
