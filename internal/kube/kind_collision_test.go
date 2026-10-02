package kube

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// collisionLists is a cluster serving three CRDs whose Kinds collide: a
// Knative Service (with core v1 Service), and Istio's and Gateway API's
// Gateway (with each other). reversed flips discovery order, which must not
// change who gets which key.
func collisionLists(reversed bool) []*metav1.APIResourceList {
	lists := []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "services", Kind: "Service", Namespaced: true}}},
		{GroupVersion: "serving.knative.dev/v1", APIResources: []metav1.APIResource{{Name: "services", Kind: "Service", Namespaced: true}}},
		{GroupVersion: "networking.istio.io/v1", APIResources: []metav1.APIResource{{Name: "gateways", Kind: "Gateway", Namespaced: true}}},
		{GroupVersion: "gateway.networking.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "gateways", Kind: "Gateway", Namespaced: true}}},
	}
	if reversed {
		slices.Reverse(lists)
	}
	return lists
}

// TestDiscoveredKindNeverTakesABuiltInOrOwnedKey: a CRD sharing its Kind
// with a built-in (Knative Service) or with the group kute routes that bare
// name to (Istio Gateway) is registered under a qualified key, and every one
// of them stays discovered and resolvable — in either discovery order.
func TestDiscoveredKindNeverTakesABuiltInOrOwnedKey(t *testing.T) {
	t.Parallel()
	for _, reversed := range []bool{false, true} {
		c := newDiscoveryTestCluster(collisionLists(reversed),
			crdMeta("services.serving.knative.dev"),
			crdMeta("gateways.networking.istio.io"),
			crdMeta("gateways.gateway.networking.k8s.io"),
		)
		c.refreshDiscovery(t.Context(), c.generation, c.clientset)

		keys := map[ResourceKind]string{}
		for _, dk := range c.DiscoveredKinds() {
			keys[dk.RegistryKind()] = dk.Group
		}
		want := map[ResourceKind]string{
			"Service.serving.knative.dev": "serving.knative.dev",
			"Gateway":                     GatewayAPIGroup,
			"Gateway.networking.istio.io": "networking.istio.io",
		}
		if len(keys) != len(want) {
			t.Fatalf("reversed=%v: discovered keys %v, want %v", reversed, keys, want)
		}
		for k, g := range want {
			if keys[k] != g {
				t.Fatalf("reversed=%v: key %q → group %q, want %q (all: %v)", reversed, k, keys[k], g, keys)
			}
			gvr, _, ok := c.resourceFor(k)
			if !ok || gvr.Group != g {
				t.Fatalf("reversed=%v: resourceFor(%q) = %v ok=%v, want group %q", reversed, k, gvr, ok, g)
			}
		}
		if gvr, _, _ := c.resourceFor(KindService); gvr.Group != "" {
			t.Fatalf("reversed=%v: KindService resolves to %v, want the core group", reversed, gvr)
		}
	}
}

// TestQualifiedKindRoundTrips: a qualified key still sends the real Kind to
// anything comparing against API-populated fields, and a group-qualified
// argument to kubectl.
func TestQualifiedKindRoundTrips(t *testing.T) {
	t.Parallel()
	k := QualifiedKind("Service", "serving.knative.dev")
	if got := k.APIKind(); got != "Service" {
		t.Fatalf("APIKind = %q, want Service", got)
	}
	if got := k.ResourceArg(); got != "service.serving.knative.dev" {
		t.Fatalf("ResourceArg = %q, want service.serving.knative.dev", got)
	}
	if got := KindService.ResourceArg(); got != "service" {
		t.Fatalf("core Service ResourceArg = %q", got)
	}
}

// TestAssignRegistryKindsIsOrderIndependent: two groups sharing an
// otherwise-unclaimed Kind (CAPI and CNPG Cluster) both stay reachable, and
// which one keeps the bare key doesn't depend on discovery order.
func TestAssignRegistryKindsIsOrderIndependent(t *testing.T) {
	t.Parallel()
	capi := DiscoveredKind{Kind: "Cluster", Group: "cluster.x-k8s.io"}
	cnpg := DiscoveredKind{Kind: "Cluster", Group: "postgresql.cnpg.io"}
	for _, in := range [][]DiscoveredKind{{capi, cnpg}, {cnpg, capi}} {
		got := map[string]ResourceKind{}
		for _, dk := range AssignRegistryKinds(in) {
			got[dk.Group] = dk.RegistryKind()
		}
		if got["cluster.x-k8s.io"] != "Cluster" || got["postgresql.cnpg.io"] != "Cluster.postgresql.cnpg.io" {
			t.Fatalf("keys %v", got)
		}
	}
}

// TestFluxHelmReleaseKeepsItsSubstitution: the hand-kept substitution still
// wins over the built-in qualification its Kind would otherwise get.
func TestFluxHelmReleaseKeepsItsSubstitution(t *testing.T) {
	t.Parallel()
	dk := DiscoveredKind{Kind: "HelmRelease", Group: FluxGroupHelm}
	if got := dk.RegistryKind(); got != KindFluxHelmRelease {
		t.Fatalf("Flux HelmRelease key = %q", got)
	}
	other := DiscoveredKind{Kind: "HelmRelease", Group: "example.com"}
	if got := other.RegistryKind(); got != "HelmRelease.example.com" {
		t.Fatalf("non-Flux HelmRelease key = %q", got)
	}
}
