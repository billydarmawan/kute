package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var gadgetGVR = schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "gadgets"}

func newGadgetCluster(objs ...runtime.Object) (*Cluster, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		crdGVR:    "CustomResourceDefinitionList",
		gadgetGVR: "GadgetList",
	}, objs...)
	return &Cluster{
		dynClient:  dyn,
		dynFactory: dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0),
		stopCh:     make(chan struct{}),
		events:     make(chan ResourceChangedMsg, 256),
		health:     newHealth(),
		discovered: []DiscoveredKind{{
			GVR: gadgetGVR, Kind: "Gadget", Plural: "gadgets", Group: "example.com",
			Established: true, CRDName: "gadgets.example.com",
		}},
	}, dyn
}

// TestCountInstancesNeverFabricatesZero pins 14b's COUNT column to
// docs/lazy-informers.md §4's "counts are never fabricated": a cache still on
// its initial LIST, or one that is Forbidden, has no count to report — only a
// filled cache does.
func TestCountInstancesNeverFabricatesZero(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		c, dyn := newGadgetCluster()
		defer close(c.stopCh)
		dyn.PrependReactor("list", "gadgets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(gadgetGVR.GroupResource(), "", nil)
		})

		if n, ok := c.CountInstances("Gadget"); ok {
			t.Fatalf("CountInstances = %d before the cache filled, want unknown", n)
		}
		waitFor(t, "the Forbidden list to be recorded", func() bool { return c.KindForbidden("Gadget", "") != nil })
		if n, ok := c.CountInstances("Gadget"); ok {
			t.Fatalf("CountInstances = %d for a Forbidden kind, want unknown", n)
		}
	})

	t.Run("filled", func(t *testing.T) {
		gadget := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1", "kind": "Gadget",
			"metadata": map[string]any{"name": "g1", "namespace": "default"},
		}}
		c, _ := newGadgetCluster(gadget)
		defer close(c.stopCh)

		c.CountInstances("Gadget")
		waitFor(t, "the Gadget cache to fill", func() bool { return c.KindSynced("Gadget", "") })
		if n, ok := c.CountInstances("Gadget"); !ok || n != 1 {
			t.Fatalf("CountInstances = (%d, %v) for a filled cache, want (1, true)", n, ok)
		}
	})
}
