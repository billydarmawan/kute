package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// withSubstitution installs a temporary substitution-table entry for the
// duration of one test. The table is a package var read by APIKind,
// ResourceArg and RegistryKind; these tests exercise the *mechanism*
// independently of whichever kinds happen to need it, so they register
// their own rather than depending on the real table's contents.
//
// It writes a package global, so **a caller must not be t.Parallel** — the
// same process-global rule the repo already applies to t.Setenv and the
// lipgloss colour profile. A parallel caller races every other parallel test
// that resolves a kind, which is most of them (-race caught exactly that).
func withSubstitution(t *testing.T, key ResourceKind, s substituted) {
	t.Helper()
	prev, had := substitutedKinds[key]
	substitutedKinds[key] = s
	t.Cleanup(func() {
		if had {
			substitutedKinds[key] = prev
			return
		}
		delete(substitutedKinds, key)
	})
}

const testSubKind ResourceKind = "TestSubstituted"

const testSubGroup = "example.test.io"

func testSubstitution() substituted {
	return substituted{apiKind: "Widget", group: testSubGroup, resource: "widgets." + testSubGroup}
}

func testSubDiscoveredKind() DiscoveredKind {
	return DiscoveredKind{
		GVR:         schema.GroupVersionResource{Group: testSubGroup, Version: "v2", Resource: "widgets"},
		Kind:        "Widget",
		Plural:      "widgets",
		Group:       testSubGroup,
		Versions:    []CRDVersion{{Name: "v2", Served: true, Storage: true}},
		Established: true,
		CRDName:     "widgets." + testSubGroup,
	}
}

func TestAPIKindAndResourceArg(t *testing.T) {
	withSubstitution(t, testSubKind, testSubstitution())

	// An unsubstituted kind is unchanged on both accessors — the invariant
	// the substitution table must not disturb for the other ~25 kinds.
	if got := KindPod.APIKind(); got != "Pod" {
		t.Errorf("KindPod.APIKind() = %q, want %q", got, "Pod")
	}
	if got := KindPod.ResourceArg(); got != "pod" {
		t.Errorf("KindPod.ResourceArg() = %q, want %q", got, "pod")
	}
	if got := KindHelmRelease.APIKind(); got != "HelmRelease" {
		t.Errorf("KindHelmRelease.APIKind() = %q, want %q", got, "HelmRelease")
	}

	// A substituted kind reports its API Kind, not its registry key.
	if got := testSubKind.APIKind(); got != "Widget" {
		t.Errorf("APIKind() = %q, want %q — an Event's involvedObject.kind carries the API Kind", got, "Widget")
	}
	if got, want := testSubKind.ResourceArg(), "widgets."+testSubGroup; got != want {
		t.Errorf("ResourceArg() = %q, want %q — the bare key wouldn't resolve for kubectl", got, want)
	}
}

func TestRegistryKindResolvesByGroupAndKind(t *testing.T) {
	withSubstitution(t, testSubKind, testSubstitution())

	if got := testSubDiscoveredKind().RegistryKind(); got != testSubKind {
		t.Errorf("RegistryKind() = %q, want %q", got, testSubKind)
	}

	// Same bare Kind, different API group: must NOT be substituted. This is
	// the whole point of keying the table on (group, kind) — another
	// vendor's identically-named CRD is a different kind.
	other := testSubDiscoveredKind()
	other.Group = "someone.else.io"
	other.GVR.Group = "someone.else.io"
	if got := other.RegistryKind(); got != ResourceKind("Widget") {
		t.Errorf("RegistryKind() for a foreign group = %q, want %q", got, "Widget")
	}
}

// TestResourceForResolvesSubstitutedKind is the load-bearing one: a
// substituted registry key still has to resolve to the real GVR, or every
// read, patch and delete of that kind goes nowhere.
func TestResourceForResolvesSubstitutedKind(t *testing.T) {
	withSubstitution(t, testSubKind, testSubstitution())

	c := &Cluster{discovered: []DiscoveredKind{testSubDiscoveredKind()}}
	gvr, clusterScoped, ok := c.resourceFor(testSubKind)
	if !ok {
		t.Fatal("resourceFor did not resolve the substituted kind — the discovery snapshot is keyed on the API Kind")
	}
	want := schema.GroupVersionResource{Group: testSubGroup, Version: "v2", Resource: "widgets"}
	if gvr != want {
		t.Errorf("resourceFor GVR = %+v, want %+v", gvr, want)
	}
	if clusterScoped {
		t.Error("resourceFor reported cluster-scoped for a namespaced kind")
	}

	// The API Kind itself must no longer resolve: it is not a registry key.
	if _, _, ok := c.resourceFor(ResourceKind("Widget")); ok {
		t.Error("the raw API Kind resolved as a registry key — the substitution would be ambiguous")
	}
}

// TestEnsureDynamicKindForRegistersSubstitutedKindAgainstRealGVR checks the
// informer registration itself: the map key is the substituted kind, the
// watched coordinates are the real ones.
func TestEnsureDynamicKindForRegistersSubstitutedKindAgainstRealGVR(t *testing.T) {
	withSubstitution(t, testSubKind, testSubstitution())

	// Built here rather than via newLazyTestCluster because registering the
	// kind starts its informer, and the dynamic fake panics on a LIST for a
	// GVR whose list kind it wasn't told about.
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	widgetGVR := schema.GroupVersionResource{Group: testSubGroup, Version: "v2", Resource: "widgets"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		crdGVR:    "CustomResourceDefinitionList",
		widgetGVR: "WidgetList",
	})
	c := &Cluster{
		dynClient:  dyn,
		dynFactory: dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0),
		stopCh:     make(chan struct{}),
		events:     make(chan ResourceChangedMsg, 256),
		health:     newHealth(),
		discovered: []DiscoveredKind{testSubDiscoveredKind()},
	}
	defer close(c.stopCh)

	if _, ok := c.ensureDynamicKindFor(testSubKind, ""); !ok {
		t.Fatal("ensureDynamicKindFor did not register the substituted kind")
	}
	info, ok := c.getDynKind(testSubKind, "")
	if !ok {
		t.Fatal("the informer was not registered under the substituted registry key")
	}
	want := schema.GroupVersionResource{Group: testSubGroup, Version: "v2", Resource: "widgets"}
	if info.gvr != want {
		t.Errorf("registered GVR = %+v, want %+v — the substitution must not change what is watched", info.gvr, want)
	}
	if !info.namespaced {
		t.Error("registered as cluster-scoped, want namespaced")
	}
}
