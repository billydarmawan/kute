package kube

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metadatafake "k8s.io/client-go/metadata/fake"
)

func newCountTestCluster(objs ...*metav1.PartialObjectMetadata) *Cluster {
	scheme := metadatafake.NewTestScheme()
	_ = metav1.AddMetaToScheme(scheme)
	runtimeObjs := make([]runtime.Object, len(objs))
	for i, o := range objs {
		runtimeObjs[i] = o
	}
	return &Cluster{metaClient: metadatafake.NewSimpleMetadataClient(scheme, runtimeObjs...)}
}

func partial(gvk schema.GroupVersionKind, namespace, name string) *metav1.PartialObjectMetadata {
	pom := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	pom.SetGroupVersionKind(gvk)
	return pom
}

// TestCountLiveCountsWithoutAnInformer is the point of CountLive: the jump
// palette needs a number per kind, and reading those from informer caches
// would start a watch per kind the moment the palette opened.
func TestCountLiveCountsWithoutAnInformer(t *testing.T) {
	secretGVK := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	c := newCountTestCluster(
		partial(secretGVK, "default", "a"),
		partial(secretGVK, "default", "b"),
		partial(secretGVK, "other", "c"),
	)

	n, err := c.CountLive(t.Context(), KindSecret, "default")
	if err != nil {
		t.Fatalf("CountLive: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountLive(Secret, default) = %d, want 2", n)
	}

	// Counting must not have registered or started anything.
	c.mu.Lock()
	informers := len(c.kindInformers)
	c.mu.Unlock()
	if informers != 0 {
		t.Fatalf("CountLive started %d informers; it must not touch them at all", informers)
	}
}

func TestCountLiveIgnoresNamespaceForClusterScopedKinds(t *testing.T) {
	nodeGVK := schema.GroupVersionKind{Version: "v1", Kind: "Node"}
	c := newCountTestCluster(partial(nodeGVK, "", "node-1"), partial(nodeGVK, "", "node-2"))

	n, err := c.CountLive(t.Context(), KindNode, "default")
	if err != nil {
		t.Fatalf("CountLive: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountLive(Node, \"default\") = %d, want 2 — Nodes have no namespace to filter by", n)
	}
}

// TestCountLiveCountsCRDsBeforeTheirInformer: the CRD kind isn't in
// typedKinds and has no dynamic entry until 14b is opened, so without its own
// case the palette showed – for CRDs forever.
func TestCountLiveCountsCRDsBeforeTheirInformer(t *testing.T) {
	crdGVK := schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	c := newCountTestCluster(partial(crdGVK, "", "a.example.com"), partial(crdGVK, "", "b.example.com"))

	n, err := c.CountLive(t.Context(), KindCustomResourceDefinition, "default")
	if err != nil {
		t.Fatalf("CountLive(CustomResourceDefinition): %v", err)
	}
	if n != 2 {
		t.Fatalf("CountLive(CustomResourceDefinition) = %d, want 2", n)
	}
}

func TestCountLiveUnknownKindErrors(t *testing.T) {
	c := newCountTestCluster()
	if _, err := c.CountLive(t.Context(), ResourceKind("Nonexistent"), ""); err == nil {
		t.Fatal("expected an error for a kind with no known API resource")
	}
}

// TestResourceForResolvesEveryTypedKind: CountLive addresses kinds by GVR
// rather than by lister, so every table entry has to resolve.
func TestResourceForResolvesEveryTypedKind(t *testing.T) {
	t.Parallel()
	c := &Cluster{}
	for kind := range typedKinds {
		gvr, _, ok := c.resourceFor(kind)
		if !ok || gvr.Resource == "" {
			t.Errorf("resourceFor(%s) = %v, %v; want a resolved GVR", kind, gvr, ok)
		}
	}
}
