package kube

import (
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"maps"
	"slices"
)

// typedKind is one built-in kind's entry in the typedKinds table: how to
// reach its informer, how to read its cache, and enough API identity to
// address it without a cache at all.
//
// The informer and list funcs deliberately hang off the same factory rather
// than off a captured lister, because SwitchContext replaces c.factory
// wholesale — every call site passes the factory it just snapshotted under
// c.mu, so a stale factory can never leak into a read.
type typedKind struct {
	// gvr identifies the resource for clients that don't go through the
	// informer at all (the metadata client behind live counts).
	gvr schema.GroupVersionResource
	// clusterScoped kinds ignore the namespace argument entirely.
	clusterScoped bool
	// informer returns (and, as a side effect, registers) this kind's
	// shared informer on f.
	informer func(f informers.SharedInformerFactory) cache.SharedIndexInformer
	// list reads this kind's cache, scoped to namespace when it's
	// namespaced and namespace is non-empty.
	list func(f informers.SharedInformerFactory, namespace string, sel labels.Selector) ([]runtime.Object, error)
}

// typedKinds is the single source of truth for every kind backed by a typed
// informer: which informers exist, how each one's cache is read, and each
// one's API coordinates.
//
// It replaced a hand-maintained pairing of a handler map here and a parallel
// switch in ListRaw, which carried a "keep these in sync" comment. That
// pairing was merely tedious while informers were all started once at
// launch; it became a correctness trap the moment starting them became
// incremental. Calling a factory's Lister() *registers* that informer as a
// side effect, so a read through a path this table doesn't know about would
// silently enlist an informer that the next factory.Start() then runs with
// no watch-error handler attached. One table, one registration path.
var typedKinds = map[ResourceKind]typedKind{
	KindPod: {
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Core().V1().Pods().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Core().V1().Pods().Lister()
			return listNamespaced(l.List, l.Pods, ns, sel)
		},
	},
	KindService: {
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "services"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Core().V1().Services().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Core().V1().Services().Lister()
			return listNamespaced(l.List, l.Services, ns, sel)
		},
	},
	KindIngress: {
		gvr: schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Networking().V1().Ingresses().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Networking().V1().Ingresses().Lister()
			return listNamespaced(l.List, l.Ingresses, ns, sel)
		},
	},
	KindConfigMap: {
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Core().V1().ConfigMaps().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Core().V1().ConfigMaps().Lister()
			return listNamespaced(l.List, l.ConfigMaps, ns, sel)
		},
	},
	KindSecret: {
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Core().V1().Secrets().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Core().V1().Secrets().Lister()
			return listNamespaced(l.List, l.Secrets, ns, sel)
		},
	},
	KindPersistentVolumeClaim: {
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Core().V1().PersistentVolumeClaims().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Core().V1().PersistentVolumeClaims().Lister()
			return listNamespaced(l.List, l.PersistentVolumeClaims, ns, sel)
		},
	},
	KindEvent: {
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "events"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Core().V1().Events().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Core().V1().Events().Lister()
			return listNamespaced(l.List, l.Events, ns, sel)
		},
	},
	KindNode: {
		gvr:           schema.GroupVersionResource{Version: "v1", Resource: "nodes"},
		clusterScoped: true,
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Core().V1().Nodes().Informer()
		},
		list: func(f informers.SharedInformerFactory, _ string, sel labels.Selector) ([]runtime.Object, error) {
			return listAll(f.Core().V1().Nodes().Lister().List, sel)
		},
	},
	KindNamespace: {
		gvr:           schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
		clusterScoped: true,
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Core().V1().Namespaces().Informer()
		},
		list: func(f informers.SharedInformerFactory, _ string, sel labels.Selector) ([]runtime.Object, error) {
			return listAll(f.Core().V1().Namespaces().Lister().List, sel)
		},
	},
	KindDeployment: {
		gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Apps().V1().Deployments().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Apps().V1().Deployments().Lister()
			return listNamespaced(l.List, l.Deployments, ns, sel)
		},
	},
	KindDaemonSet: {
		gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Apps().V1().DaemonSets().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Apps().V1().DaemonSets().Lister()
			return listNamespaced(l.List, l.DaemonSets, ns, sel)
		},
	},
	KindStatefulSet: {
		gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Apps().V1().StatefulSets().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Apps().V1().StatefulSets().Lister()
			return listNamespaced(l.List, l.StatefulSets, ns, sel)
		},
	},
	KindReplicaSet: {
		gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Apps().V1().ReplicaSets().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Apps().V1().ReplicaSets().Lister()
			return listNamespaced(l.List, l.ReplicaSets, ns, sel)
		},
	},
	KindControllerRevision: {
		gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "controllerrevisions"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Apps().V1().ControllerRevisions().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Apps().V1().ControllerRevisions().Lister()
			return listNamespaced(l.List, l.ControllerRevisions, ns, sel)
		},
	},
	KindJob: {
		gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Batch().V1().Jobs().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Batch().V1().Jobs().Lister()
			return listNamespaced(l.List, l.Jobs, ns, sel)
		},
	},
	KindCronJob: {
		gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Batch().V1().CronJobs().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Batch().V1().CronJobs().Lister()
			return listNamespaced(l.List, l.CronJobs, ns, sel)
		},
	},
	KindHorizontalPodAutoscaler: {
		gvr: schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Autoscaling().V2().HorizontalPodAutoscalers().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Autoscaling().V2().HorizontalPodAutoscalers().Lister()
			return listNamespaced(l.List, l.HorizontalPodAutoscalers, ns, sel)
		},
	},
	KindRole: {
		gvr: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Rbac().V1().Roles().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Rbac().V1().Roles().Lister()
			return listNamespaced(l.List, l.Roles, ns, sel)
		},
	},
	KindRoleBinding: {
		gvr: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Rbac().V1().RoleBindings().Informer()
		},
		list: func(f informers.SharedInformerFactory, ns string, sel labels.Selector) ([]runtime.Object, error) {
			l := f.Rbac().V1().RoleBindings().Lister()
			return listNamespaced(l.List, l.RoleBindings, ns, sel)
		},
	},
	KindClusterRole: {
		gvr:           schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
		clusterScoped: true,
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Rbac().V1().ClusterRoles().Informer()
		},
		list: func(f informers.SharedInformerFactory, _ string, sel labels.Selector) ([]runtime.Object, error) {
			return listAll(f.Rbac().V1().ClusterRoles().Lister().List, sel)
		},
	},
	KindClusterRoleBinding: {
		gvr:           schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
		clusterScoped: true,
		informer: func(f informers.SharedInformerFactory) cache.SharedIndexInformer {
			return f.Rbac().V1().ClusterRoleBindings().Informer()
		},
		list: func(f informers.SharedInformerFactory, _ string, sel labels.Selector) ([]runtime.Object, error) {
			return listAll(f.Rbac().V1().ClusterRoleBindings().Lister().List, sel)
		},
	},
}

// registerWatches is registerWatchesLocked for callers that don't already
// hold c.mu. Anything that needs registration and factory.Start to be atomic
// (Start, ensureKind) must take the lock itself and call the locked form.
func (c *Cluster) registerWatches(kinds ...ResourceKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registerWatchesLocked(kinds...)
}

// registerWatchesLocked registers each of kinds against the cluster-wide
// factory at scope "" — the shape every caller outside Start's own
// scope-aware Pod registration wants (Namespace/Node are always cluster-wide
// regardless of mode; every test-only caller predates scoped mode entirely).
// Passing no kinds registers every kind in typedKinds. c.mu must be held.
func (c *Cluster) registerWatchesLocked(kinds ...ResourceKind) {
	if len(kinds) == 0 {
		kinds = slices.Collect(maps.Keys(typedKinds))
	}
	for _, kind := range kinds {
		if _, ok := typedKinds[kind]; !ok {
			continue
		}
		c.registerTypedWatchLocked(kind, "", c.factory)
	}
}

// registerTypedWatchLocked attaches change/watch-error handlers to kind's
// informer on f and registers it under (kind, scope) — the single place a
// typed informer's handlers are wired, whether it's part of Start's eager
// set, ensureKind's lazy path, or a test's direct registerWatches call. Must
// run before f.Start (SetWatchErrorHandler errors once an informer is
// already running). c.mu must be held.
func (c *Cluster) registerTypedWatchLocked(kind ResourceKind, scope string, f informers.SharedInformerFactory) {
	tk, ok := typedKinds[kind]
	if !ok {
		return
	}
	informer := tk.informer(f)
	gen := c.generation
	if c.watcher != nil {
		c.watcher.register(tk.gvr, scope, "", func() {
			c.recordWatchEstablished(gen, kind, scope)
		}, func(err error) {
			c.recordWatchEnded(gen, kind, scope, err)
		})
	}
	// Best-effort: a failed registration just means no health signal from
	// this informer.
	_ = informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		// recordWatchError re-checks gen itself, atomically with both the
		// kindFailed/kindStalled write and the health.onWatchError call —
		// see its doc comment for why a callback belonging to a context
		// SwitchContext has already replaced can't corrupt the new
		// context's state, health included.
		c.recordWatchError(gen, kind, scope, err)
	})
	// Handler registration errors are non-fatal for a read-only UI.
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.notifyScope(gen, kind, scope) },
		UpdateFunc: func(any, any) { c.notifyScope(gen, kind, scope) },
		DeleteFunc: func(any) { c.notifyScope(gen, kind, scope) },
	})
	if c.kindInformers == nil {
		c.kindInformers = map[scopeKey]cache.SharedIndexInformer{}
	}
	c.kindInformers[scopeKey{kind, scope}] = informer
	c.noteInformerStartedLocked()
}

// notify delivers a change event without blocking the informer goroutine; if
// the buffer is full the event is dropped and the next resync/refresh
// reconciles. gen is the informer's own registration-time generation — a
// notify from an informer a since-completed SwitchContext has already torn
// down is dropped rather than prompting a reload against the new context's
// cache for a kind change that happened in the old one. Lower-stakes than
// noteWatchError's atomicity (worst case here is one wasted reload reading
// the new context's own, correct data), so a plain locked read is enough.
func (c *Cluster) notify(gen int, kind ResourceKind) {
	c.notifyScope(gen, kind, "")
}

// notifyScope is notify with the informer cache key retained, so recovery of
// one namespace cannot clear another scope's outstanding watch failure.
func (c *Cluster) notifyScope(gen int, kind ResourceKind, scope string) {
	// Latch before the send, not after, and regardless of whether the send
	// lands: an object came out of this apiserver, which is the fact Reached
	// reports. The message below may be dropped here or arrive before the
	// event loop is consuming; the latch survives both. The generation check
	// and the latch share one critical section, so a late event from a
	// context SwitchContext just replaced can't latch Reached for the new one.
	if !c.noteReachedIfGen(gen) {
		return
	}
	select {
	case c.events <- ResourceChangedMsg{Kind: kind}:
	default:
	}
}
