package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/metadata"
)

// CountLive asks the API server how many objects of kind exist in namespace
// ("" for cluster-wide), without touching — or starting — an informer.
//
// This exists because counting from a cache means having the cache, and the
// jump palette wants a number for every kind at once. Reading those from
// informers would start a watch per kind the moment the palette opened,
// undoing the laziness everywhere else. Instead: ask for a single object's
// metadata and read how many were left behind.
//
//	GET /api/v1/secrets?limit=1  →  metadata.remainingItemCount: 4127
//
// The request is a PartialObjectMetadata list, so a kind with thousands of
// large objects still answers in about two kilobytes.
//
// The server documents remainingItemCount as an estimate and omits it
// entirely for lists it served from its own watch cache — which is why the
// request must not carry resourceVersion=0. Good enough for a palette hint;
// don't build anything on it that has to be exact.
func (c *Cluster) CountLive(ctx context.Context, kind ResourceKind, namespace string) (int, error) {
	gvr, clusterScoped, ok := c.resourceFor(kind)
	if !ok {
		return 0, fmt.Errorf("no API resource known for kind %s", kind)
	}
	client, err := c.metadataClient()
	if err != nil {
		return 0, err
	}

	ns := namespace
	if clusterScoped {
		ns = ""
	}
	var lister interface {
		List(context.Context, metav1.ListOptions) (*metav1.PartialObjectMetadataList, error)
	} = client.Resource(gvr)
	if ns != "" {
		lister = client.Resource(gvr).Namespace(ns)
	}

	list, err := lister.List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return 0, err
	}
	n := len(list.Items)
	if list.GetRemainingItemCount() != nil {
		n += int(*list.GetRemainingItemCount())
	}
	return n, nil
}

// resourceFor resolves a kind's API coordinates from whichever of the three
// places knows about it: the built-in table, an already-registered dynamic
// informer, or the discovery snapshot for a CRD nothing has opened yet.
func (c *Cluster) resourceFor(kind ResourceKind) (gvr schema.GroupVersionResource, clusterScoped, ok bool) {
	if tk, found := typedKinds[kind]; found {
		return tk.gvr, tk.clusterScoped, true
	}
	// The CRD list's own kind is known before 14b ever starts its informer,
	// and a palette count must not wait for that.
	if kind == KindCustomResourceDefinition {
		return crdGVR, true, true
	}
	if info, found := c.getDynKind(kind, ""); found {
		return info.gvr, !info.namespaced, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, dk := range c.discovered {
		if dk.RegistryKind() == kind && dk.GVR.Version != "" {
			return dk.GVR, dk.ClusterScoped, true
		}
	}
	return schema.GroupVersionResource{}, false, false
}

// metadataClient lazily builds and caches the PartialObjectMetadata client
// CountLive uses. Cached because the palette asks for every kind at once and
// building a client per count would mean a fresh TLS handshake each time.
func (c *Cluster) metadataClient() (metadata.Interface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.metaClient != nil {
		return c.metaClient, nil
	}
	if c.restCfg == nil {
		return nil, fmt.Errorf("no REST config")
	}
	client, err := metadata.NewForConfig(c.restCfg)
	if err != nil {
		return nil, err
	}
	c.metaClient = client
	return client, nil
}
