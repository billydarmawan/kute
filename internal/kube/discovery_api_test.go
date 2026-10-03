package kube

import (
	"testing"
	"testing/synctest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"
)

// crdMeta is one CRD as the metadata client sees it: a name and nothing
// else, which is the whole point — "<plural>.<group>" identifies the
// resource without any of the schema that makes CRDs enormous.
func crdMeta(name string) *metav1.PartialObjectMetadata {
	pom := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name}}
	pom.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition",
	})
	return pom
}

// newDiscoveryTestCluster wires a cluster whose discovery API serves the
// given resource lists and whose metadata client serves the given CRD names.
func newDiscoveryTestCluster(resources []*metav1.APIResourceList, crds ...*metav1.PartialObjectMetadata) *Cluster {
	cs := fake.NewSimpleClientset()
	cs.Discovery().(*discoveryfake.FakeDiscovery).Resources = resources

	scheme := metadatafake.NewTestScheme()
	_ = metav1.AddMetaToScheme(scheme)
	objs := make([]runtime.Object, len(crds))
	for i, c := range crds {
		objs[i] = c
	}
	return &Cluster{
		clientset:  cs,
		metaClient: metadatafake.NewSimpleMetadataClient(scheme, objs...),
	}
}

// TestRefreshDiscoveryUsesTheDiscoveryAPI is the §5.1 fix: the kind list is
// built from two cheap reads instead of listing CRD objects, which on a real
// cluster meant pulling megabytes of OpenAPI schema to learn a few names.
func TestRefreshDiscoveryUsesTheDiscoveryAPI(t *testing.T) {
	t.Parallel()
	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{
			{
				GroupVersion: "cert-manager.io/v1",
				APIResources: []metav1.APIResource{
					{Name: "certificates", Kind: "Certificate", Namespaced: true},
					{Name: "clusterissuers", Kind: "ClusterIssuer", Namespaced: false},
					{Name: "certificates/status", Kind: "Certificate", Namespaced: true},
				},
			},
			// Built-ins are served too and must not become "custom kinds"
			// — no CRD backs them, so no CRD name matches.
			{
				GroupVersion: "apps/v1",
				APIResources: []metav1.APIResource{{Name: "deployments", Kind: "Deployment", Namespaced: true}},
			},
			{
				GroupVersion: "v1",
				APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod", Namespaced: true}},
			},
		},
		crdMeta("certificates.cert-manager.io"),
		crdMeta("clusterissuers.cert-manager.io"),
	)

	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	got := c.DiscoveredKinds()
	if len(got) != 2 {
		t.Fatalf("discovered %d kinds, want 2: %+v", len(got), got)
	}

	byKind := map[string]DiscoveredKind{}
	for _, dk := range got {
		byKind[dk.Kind] = dk
	}
	cert, ok := byKind["Certificate"]
	if !ok {
		t.Fatal("Certificate was not discovered")
	}
	if cert.GVR != (schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}) {
		t.Errorf("Certificate GVR = %v", cert.GVR)
	}
	if cert.ClusterScoped {
		t.Error("Certificate is namespaced")
	}
	if !cert.Established {
		t.Error("a resource the server is serving is established by definition")
	}
	if cert.CRDName != "certificates.cert-manager.io" {
		t.Errorf("CRDName = %q", cert.CRDName)
	}
	if issuer := byKind["ClusterIssuer"]; !issuer.ClusterScoped {
		t.Error("ClusterIssuer is cluster-scoped")
	}
	// The subresource must not have become a browsable kind.
	for _, dk := range got {
		if dk.Plural == "certificates/status" {
			t.Error("a subresource was discovered as a kind")
		}
	}
}

// TestRefreshDiscoveryIgnoresBuiltInGroups pins the reason the CRD-name list
// exists at all. Filtering by group name would need a hardcoded list of
// built-in groups, and that list gets Gateway API wrong: it is CRD-backed
// despite living under a k8s.io group.
func TestRefreshDiscoveryIgnoresBuiltInGroups(t *testing.T) {
	t.Parallel()
	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{
			{
				GroupVersion: "networking.k8s.io/v1",
				APIResources: []metav1.APIResource{{Name: "ingresses", Kind: "Ingress", Namespaced: true}},
			},
			{
				GroupVersion: "gateway.networking.k8s.io/v1",
				APIResources: []metav1.APIResource{{Name: "httproutes", Kind: "HTTPRoute", Namespaced: true}},
			},
		},
		crdMeta("httproutes.gateway.networking.k8s.io"),
	)

	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	got := c.DiscoveredKinds()
	if len(got) != 1 || got[0].Kind != "HTTPRoute" {
		t.Fatalf("discovered %+v, want only HTTPRoute — Ingress is built in, HTTPRoute is a CRD despite its k8s.io group", got)
	}
}

// TestRefreshDiscoveryDedupesMultiVersionCRDs: a CRD serving several
// versions appears once per version in discovery, but is one browsable kind.
func TestRefreshDiscoveryDedupesMultiVersionCRDs(t *testing.T) {
	t.Parallel()
	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{
			{
				GroupVersion: "example.com/v1",
				APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}},
			},
			{
				GroupVersion: "example.com/v1beta1",
				APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}},
			},
		},
		crdMeta("widgets.example.com"),
	)

	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	got := c.DiscoveredKinds()
	if len(got) != 1 {
		t.Fatalf("discovered %d entries for one multi-version CRD, want 1: %+v", len(got), got)
	}
}

// TestDiscoveredKindsStartWithoutPrinterColumns is the deliberate trade:
// columns live in the part of a CRD that is megabytes, so a kind renders
// with neutral columns until someone actually opens it.
func TestDiscoveredKindsStartWithoutPrinterColumns(t *testing.T) {
	t.Parallel()
	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{
			{
				GroupVersion: "example.com/v1",
				APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}},
			},
		},
		crdMeta("widgets.example.com"),
	)

	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	got := c.DiscoveredKinds()
	if len(got) != 1 {
		t.Fatalf("discovered %+v", got)
	}
	if len(got[0].PrinterColumns) != 0 {
		t.Fatalf("discovery fetched printer columns; they belong to the per-kind lazy read: %+v", got[0].PrinterColumns)
	}
}

// TestCustomKindsFromSkipsUnparseableGroups: one bad entry in the served
// list must not cost the usable ones. Partial discovery is normal on
// clusters with an aggregated API service that isn't answering, and
// ServerGroupsAndResources hands back both an error and everything it did
// reach — which discoverCustomResources keeps.
func TestCustomKindsFromSkipsUnparseableGroups(t *testing.T) {
	t.Parallel()
	lists := []*metav1.APIResourceList{
		{GroupVersion: "not/a/valid/gv", APIResources: []metav1.APIResource{{Name: "x", Kind: "X"}}},
		{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}},
		},
	}
	got := customKindsFrom(lists, map[string]bool{"widgets.example.com": true, "x.": true})

	if len(got) != 1 || got[0].Kind != "Widget" {
		t.Fatalf("a bad group version cost the usable ones: %+v", got)
	}
}

// TestRefreshDiscoveryWithNoCRDsDiscoversNothing: a cluster with no CRDs has
// no custom kinds, and must not fall through to registering built-ins.
func TestRefreshDiscoveryWithNoCRDsDiscoversNothing(t *testing.T) {
	t.Parallel()
	c := newDiscoveryTestCluster([]*metav1.APIResourceList{
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Kind: "Deployment"}}},
	})

	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	if got := c.DiscoveredKinds(); len(got) != 0 {
		t.Fatalf("discovered %+v on a cluster with no CRDs", got)
	}
}

// crdObject builds a full CRD as the API server stores it: names, scope,
// versions, printer columns — and, standing in for the megabytes of OpenAPI
// schema, the part discovery deliberately never fetches.
func crdObject(plural, group, kind string, cols []map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group": group,
			"scope": "Namespaced",
			"names": map[string]any{"kind": kind, "plural": plural},
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"additionalPrinterColumns": toAnySlice(cols),
				"schema":                   map[string]any{"openAPIV3Schema": map[string]any{"type": "object"}},
			}},
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Established", "status": "True"}},
		},
	}}
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, len(in))
	for i, m := range in {
		out[i] = m
	}
	return out
}

// TestEnsurePrinterColumnsFetchesOneCRD is the other half of §5.1: columns
// are the only thing the cheap discovery pass can't supply, and they arrive
// for one kind at a time, when that kind is actually read.
func TestEnsurePrinterColumnsFetchesOneCRD(t *testing.T) {
	crd := crdObject("widgets", "example.com", "Widget", []map[string]any{
		{"name": "Phase", "type": "string", "jsonPath": ".status.phase"},
		{"name": "Size", "type": "string", "jsonPath": ".spec.size"},
	})
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crd)

	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}},
		}},
		crdMeta("widgets.example.com"),
	)
	c.dynClient = dyn
	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	if cols := c.DiscoveredKinds()[0].PrinterColumns; len(cols) != 0 {
		t.Fatalf("precondition: discovery should not have fetched columns, got %+v", cols)
	}

	// Synchronous half, so the assertion doesn't race the goroutine
	// ensurePrinterColumns spawns.
	if changed := c.fetchPrinterColumns(t.Context(), c.generation, "Widget"); !changed {
		t.Fatal("fetchPrinterColumns reported no change despite the CRD declaring two columns")
	}
	got := c.DiscoveredKinds()[0].PrinterColumns
	if len(got) != 2 || got[0].Name != "Phase" || got[1].JSONPath != ".spec.size" {
		t.Fatalf("printer columns = %+v, want Phase and Size", got)
	}
	// The version discovery reported is what the informer watches; a CRD
	// Get must not quietly change it.
	if v := c.DiscoveredKinds()[0].GVR.Version; v != "v1" {
		t.Fatalf("GVR version = %q, want the served version discovery reported", v)
	}
}

// TestEnsurePrinterColumnsFetchesOncePerKind: browse re-reads on every
// change event and every retry tick, so a per-read CRD Get would undo the
// saving this exists for.
func TestEnsurePrinterColumnsFetchesOncePerKind(t *testing.T) {
	crd := crdObject("widgets", "example.com", "Widget", []map[string]any{
		{"name": "Phase", "type": "string", "jsonPath": ".status.phase"},
	})
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crd)

	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}},
		}},
		crdMeta("widgets.example.com"),
	)
	c.dynClient = dyn
	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	for i := 0; i < 10; i++ {
		c.fetchPrinterColumns(t.Context(), c.generation, "Widget")
	}

	gets := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "get" && a.GetResource() == crdGVR {
			gets++
		}
	}
	if gets != 1 {
		t.Fatalf("fetched the CRD %d times across 10 reads, want exactly 1", gets)
	}
}

// TestEnsurePrinterColumnsUnknownKindIsANoOp: a kind discovery never saw has
// no CRD to fetch, and must not error or spin.
func TestEnsurePrinterColumnsUnknownKindIsANoOp(t *testing.T) {
	t.Parallel()
	c := newDiscoveryTestCluster(nil)
	if c.fetchPrinterColumns(t.Context(), c.generation, "Nonexistent") {
		t.Fatal("reported a change for a kind discovery never saw")
	}
}

// TestFetchPrinterColumnsDropsStaleGeneration is fetchPrinterColumns'
// counterpart to TestNoteWatchErrorDropsStaleGeneration and
// TestMarkKindFailedDropsStaleGeneration. fetchPrinterColumns must pass the
// captured generation through each printer-column fetch, and check that
// generation in the same critical section that writes crdColumnsFetched and
// discovered. A CRD Get started against one context must not write its
// result after SwitchContext has moved c.generation forward. This holds
// even when, as in this test, the replacement context has rediscovered a
// kind with the exact same name and the in-flight Get still resolves
// successfully.
func TestFetchPrinterColumnsDropsStaleGeneration(t *testing.T) {
	t.Parallel()
	crd := crdObject("widgets", "example.com", "Widget", []map[string]any{
		{"name": "Phase", "type": "string", "jsonPath": ".status.phase"},
	})
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crd)

	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}},
		}},
		crdMeta("widgets.example.com"),
	)
	c.dynClient = dyn
	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	staleGen := c.generation
	c.generation++ // simulates a SwitchContext that ran while this Get was in flight

	if changed := c.fetchPrinterColumns(t.Context(), staleGen, "Widget"); changed {
		t.Fatal("fetchPrinterColumns reported a change for a stale generation")
	}
	if got := c.DiscoveredKinds()[0].PrinterColumns; len(got) != 0 {
		t.Fatalf("printer columns = %+v; a stale-generation fetch must not write into the replacement context's discovered kind", got)
	}
	c.mu.Lock()
	fetched := c.crdColumnsFetched[ResourceKind("Widget")]
	c.mu.Unlock()
	if fetched {
		t.Fatal("crdColumnsFetched[Widget] = true from a stale-generation fetch; the replacement context would never retry it")
	}
}

// TestSupersededStartLeavesDiscoveryAndVersionAlone pins Start's tail to its
// own generation. A context switch landing while discovery or the version
// probe is still on the wire (the detached initial Start inside a slow
// aggregated API, say) must not let the old cluster's CRDs or server version
// overwrite the new context's.
func TestSupersededStartLeavesDiscoveryAndVersionAlone(t *testing.T) {
	t.Parallel()
	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{{
			GroupVersion: "cert-manager.io/v1",
			APIResources: []metav1.APIResource{{Name: "certificates", Kind: "Certificate", Namespaced: true}},
		}},
		crdMeta("certificates.cert-manager.io"),
	)
	cs := c.clientset.(*fake.Clientset)
	cs.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "30"}
	// Each discovery read is where a SwitchContext lands: it bumps the
	// generation while the old Start's request is in flight.
	switchMidRead := func(k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		c.generation++
		c.mu.Unlock()
		return false, nil, nil
	}
	cs.PrependReactor("get", "resource", switchMidRead)
	cs.PrependReactor("get", "version", switchMidRead)

	gen := c.generation
	c.refreshDiscovery(t.Context(), gen, cs)
	c.probeTimeZoneCapability(t.Context(), gen, cs)

	if got := c.DiscoveredKinds(); len(got) != 0 {
		t.Fatalf("superseded discovery wrote %d kinds into the new context: %+v", len(got), got)
	}
	if got := c.TimeZoneCapability(); got != TimeZoneCapabilityUnknown {
		t.Fatalf("superseded version probe wrote %v into the new context, want Unknown", got)
	}
}

// TestFetchPrinterColumnsLandsOnAQualifiedKind: a CRD whose Kind collides
// with a built-in is addressed by its qualified registry key, and the fetched
// columns must be written back under that same key — not dropped while the
// kind is marked fetched for good.
func TestFetchPrinterColumnsLandsOnAQualifiedKind(t *testing.T) {
	t.Parallel()
	crd := crdObject("services", "serving.knative.dev", "Service", []map[string]any{
		{"name": "URL", "type": "string", "jsonPath": ".status.url"},
	})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crd)
	c := newDiscoveryTestCluster(
		[]*metav1.APIResourceList{{
			GroupVersion: "serving.knative.dev/v1",
			APIResources: []metav1.APIResource{{Name: "services", Kind: "Service", Namespaced: true}},
		}},
		crdMeta("services.serving.knative.dev"),
	)
	c.dynClient = dyn
	c.refreshDiscovery(t.Context(), c.generation, c.clientset)

	kind := QualifiedKind("Service", "serving.knative.dev")
	if changed := c.fetchPrinterColumns(t.Context(), c.generation, kind); !changed {
		t.Fatal("fetchPrinterColumns reported no change for a qualified kind whose CRD declares a column")
	}
	if got := c.DiscoveredKinds()[0].PrinterColumns; len(got) != 1 || got[0].Name != "URL" {
		t.Fatalf("printer columns = %+v, want URL", got)
	}
}

// TestCuratedKindsFetchNoCRD: Flux kinds, Argo CD's Application and
// cert-manager's Certificate render fixed curated columns, so reading one
// must not pull its full CRD (schema and all) for printer columns nobody
// reads.
func TestCuratedKindsFetchNoCRD(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		crds := []*unstructured.Unstructured{
			crdObject("kustomizations", FluxGroupKustomize, "Kustomization", []map[string]any{{"name": "Ready", "type": "string", "jsonPath": ".status.x"}}),
			crdObject("helmreleases", FluxGroupHelm, "HelmRelease", []map[string]any{{"name": "Ready", "type": "string", "jsonPath": ".status.x"}}),
			crdObject("applications", ArgoGroup, "Application", []map[string]any{{"name": "Sync", "type": "string", "jsonPath": ".status.x"}}),
			crdObject("certificates", CertManagerGroup, "Certificate", []map[string]any{{"name": "Ready", "type": "string", "jsonPath": ".status.x"}}),
		}
		objs := make([]runtime.Object, len(crds))
		for i, crd := range crds {
			objs[i] = crd
		}
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, objs...)
		c := newDiscoveryTestCluster(
			[]*metav1.APIResourceList{
				{GroupVersion: FluxGroupKustomize + "/v1", APIResources: []metav1.APIResource{{Name: "kustomizations", Kind: "Kustomization", Namespaced: true}}},
				{GroupVersion: FluxGroupHelm + "/v2", APIResources: []metav1.APIResource{{Name: "helmreleases", Kind: "HelmRelease", Namespaced: true}}},
				{GroupVersion: ArgoGroup + "/v1alpha1", APIResources: []metav1.APIResource{{Name: "applications", Kind: "Application", Namespaced: true}}},
				{GroupVersion: CertManagerGroup + "/v1", APIResources: []metav1.APIResource{{Name: "certificates", Kind: "Certificate", Namespaced: true}}},
			},
			crdMeta("kustomizations."+FluxGroupKustomize),
			crdMeta("helmreleases."+FluxGroupHelm),
			crdMeta("applications."+ArgoGroup),
			crdMeta("certificates."+CertManagerGroup),
		)
		c.dynClient = dyn
		c.refreshDiscovery(t.Context(), c.generation, c.clientset)
		if got := len(c.DiscoveredKinds()); got != 4 {
			t.Fatalf("precondition: discovered %d kinds, want 4", got)
		}

		for _, dk := range c.DiscoveredKinds() {
			c.ensurePrinterColumns(dk.RegistryKind())
		}
		synctest.Wait()

		for _, a := range dyn.Actions() {
			if a.GetVerb() == "get" && a.GetResource() == crdGVR {
				t.Fatalf("a curated kind fetched its CRD for printer columns it never reads: %v", a)
			}
		}
	})
}

// TestPrinterColumnsArrivalIsFlagged: the one CRD event that changes the kind
// registry is printer columns landing, and it must say so — the root shell
// rebuilds the registry only on ColumnsArrived, not on every CRD object event.
func TestPrinterColumnsArrivalIsFlagged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		crd := crdObject("widgets", "example.com", "Widget", []map[string]any{{"name": "Phase", "type": "string", "jsonPath": ".status.phase"}})
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crd)
		c := newDiscoveryTestCluster(
			[]*metav1.APIResourceList{
				{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true}}},
			},
			crdMeta("widgets.example.com"),
		)
		c.dynClient = dyn
		c.events = make(chan ResourceChangedMsg, 4)
		c.refreshDiscovery(t.Context(), c.generation, c.clientset)
		if got := len(c.DiscoveredKinds()); got != 1 {
			t.Fatalf("precondition: discovered %d kinds, want 1", got)
		}

		c.ensurePrinterColumns(c.DiscoveredKinds()[0].RegistryKind())
		synctest.Wait()

		select {
		case got := <-c.events:
			if got != (ResourceChangedMsg{Kind: KindCustomResourceDefinition, ColumnsArrived: true}) {
				t.Fatalf("columns arrival sent %+v, want a ColumnsArrived CRD event", got)
			}
		default:
			t.Fatal("printer columns arrived without announcing it")
		}
	})
}
