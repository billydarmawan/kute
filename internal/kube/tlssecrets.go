package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// tlsSecretFieldSelector narrows a Secret list/watch to kubernetes.io/tls
// Secrets — the only ones a TLS cell or a cert chain ever reads. Secret.type
// is a server-supported field selector, so the narrowing happens at the API
// server, the same as helmReleaseFieldSelector.
var tlsSecretFieldSelector = fields.OneTermEqualSelector("type", string(corev1.SecretTypeTLS)).String()

// ensureTLSSecrets idempotently starts the informer behind ListTLSSecrets for
// one namespace ("" for all) and returns it, or nil on a stopped cluster.
//
// It exists for the same reason ensureHelmSecrets does
// (docs/lazy-informers.md §5.7): a TLS Ingress's or Gateway's expiry cell, and
// 31a's cert chain, need one Secret's tls.crt, and reading it from the shared
// Secret cache started that cache cluster-wide with full payloads — every
// Secret in the cluster, Helm release manifests included — to answer one
// cell. This cache holds only kubernetes.io/tls Secrets, and only for the
// namespace actually read, independent of --namespace-scoped (its key is the
// read's own namespace in both modes, like KindHelmRelease's).
//
// Registered under (KindTLSSecret, namespace) in kindInformers, so the
// per-kind sync/stall/forbidden bookkeeping, the health loop's sweep and the
// Reached latch all cover it without a parallel map. c.mu must not be held.
func (c *Cluster) ensureTLSSecrets(namespace string) cache.SharedIndexInformer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopCh == nil {
		return nil
	}
	key := scopeKey{KindTLSSecret, namespace}
	if inf := c.kindInformers[key]; inf != nil {
		return inf
	}
	factory := informers.NewSharedInformerFactoryWithOptions(
		c.clientset, defaultResync,
		informers.WithNamespace(namespace),
		informers.WithTransform(stripManagedFields),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = tlsSecretFieldSelector
		}),
	)
	informer := factory.Core().V1().Secrets().Informer()
	gen := c.generation
	if c.watcher != nil {
		c.watcher.register(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, namespace, tlsSecretFieldSelector, func() {
			c.recordWatchEstablished(gen, KindTLSSecret, namespace)
		}, func(err error) {
			c.recordWatchEnded(gen, KindTLSSecret, namespace, err)
		})
	}
	// Same handler wiring as ensureHelmSecrets: best-effort, and every
	// callback re-checks gen so a replaced context's informer can't write
	// into the new one's state.
	_ = informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		c.recordWatchError(gen, KindTLSSecret, namespace, err)
	})
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.notifyScope(gen, KindTLSSecret, namespace) },
		UpdateFunc: func(any, any) { c.notifyScope(gen, KindTLSSecret, namespace) },
		DeleteFunc: func(any) { c.notifyScope(gen, KindTLSSecret, namespace) },
	})
	if c.kindInformers == nil {
		c.kindInformers = map[scopeKey]cache.SharedIndexInformer{}
	}
	c.kindInformers[key] = informer
	c.noteInformerStartedLocked()
	factory.Start(c.stopCh)
	c.health.noteListBurst()
	return informer
}

// ListTLSSecrets returns the kubernetes.io/tls Secrets in namespace ("" for
// all) from a cache that holds only those, starting it on first read. Gate a
// miss on KindSynced/KindError/KindForbidden(KindTLSSecret, namespace) — the
// same namespace passed here — never on KindSecret, which answers for the
// shared cache this deliberately doesn't touch.
func (c *Cluster) ListTLSSecrets(_ context.Context, namespace string) ([]runtime.Object, error) {
	inf := c.ensureTLSSecrets(namespace)
	if inf == nil {
		// A stopped cluster: empty, with KindSynced reporting settled.
		return nil, nil
	}
	lister := corev1listers.NewSecretLister(inf.GetIndexer())
	return listNamespaced(lister.List, lister.Secrets, namespace, labels.Everything())
}
