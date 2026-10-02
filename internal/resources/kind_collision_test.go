package resources

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
)

func collidingKind(kind, plural, group string) kube.DiscoveredKind {
	return kube.DiscoveredKind{
		Kind: kind, Plural: plural, Group: group,
		GVR:         schema.GroupVersionResource{Group: group, Version: "v1", Resource: plural},
		Versions:    []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		Established: true,
		CRDName:     plural + "." + group,
	}
}

// TestBuildDiscoveredRegistryKnativeServiceKeepsCoreService: a Knative
// Service CRD must not replace the core Services list — which kept
// TYPE/CLUSTER-IP/PORTS — and must itself stay reachable under its own key.
func TestBuildDiscoveredRegistryKnativeServiceKeepsCoreService(t *testing.T) {
	ksvc := collidingKind("Service", "services", "serving.knative.dev")
	reg, groups := BuildDiscoveredRegistry(kube.AssignRegistryKinds([]kube.DiscoveredKind{ksvc}), nil)

	core, ok := reg.Descriptor(kube.KindService)
	if !ok || core.Custom || core.APIGroup != "" || core.Columns[1] != "Type" {
		t.Fatalf("core Service descriptor replaced: %+v (ok=%v)", core, ok)
	}

	key := kube.QualifiedKind("Service", "serving.knative.dev")
	kn, ok := reg.Descriptor(key)
	if !ok || !kn.Custom || kn.APIGroup != "serving.knative.dev" {
		t.Fatalf("Knative Service unreachable under %q: %+v (ok=%v)", key, kn, ok)
	}
	if kn.Display == core.Display {
		t.Fatalf("goto palette can't tell the two apart: both display %q", kn.Display)
	}
	if !groupHas(groups, GroupCustomResources, key) {
		t.Fatalf("Knative Service missing from the custom resources group: %+v", groups)
	}
}

// TestBuildDiscoveredRegistryTwoGatewaysBothReachable: Istio's and Gateway
// API's Gateway both register, Gateway API's keeps the bare key kute routes
// to the routing table, and neither hides the other.
func TestBuildDiscoveredRegistryTwoGatewaysBothReachable(t *testing.T) {
	gw := collidingKind("Gateway", "gateways", kube.GatewayAPIGroup)
	istio := collidingKind("Gateway", "gateways", "networking.istio.io")
	for _, in := range [][]kube.DiscoveredKind{{gw, istio}, {istio, gw}} {
		reg, groups := BuildDiscoveredRegistry(kube.AssignRegistryKinds(in), nil)

		d, ok := reg.Descriptor(kube.KindGateway)
		if !ok || d.APIGroup != kube.GatewayAPIGroup {
			t.Fatalf("bare Gateway key = %+v (ok=%v), want Gateway API's", d, ok)
		}
		key := kube.QualifiedKind("Gateway", "networking.istio.io")
		d, ok = reg.Descriptor(key)
		if !ok || d.APIGroup != "networking.istio.io" {
			t.Fatalf("Istio Gateway hidden: %+v (ok=%v)", d, ok)
		}
		if !groupHas(groups, GroupCustomResources, kube.KindGateway) || !groupHas(groups, GroupCustomResources, key) {
			t.Fatalf("both gateways must be listed: %+v", groups)
		}
	}
}

// TestCRDRowKeyFollowsTheAssignedKey: 14b's ↵ on a CRD row jumps to the key
// the kind was registered under, not the bare Kind parsed off the object.
func TestCRDRowKeyFollowsTheAssignedKey(t *testing.T) {
	ksvc := collidingKind("Service", "services", "serving.knative.dev")
	reg, _ := BuildDiscoveredRegistry(kube.AssignRegistryKinds([]kube.DiscoveredKind{ksvc}), nil)
	d, _ := reg.Descriptor(kube.KindCustomResourceDefinition)
	row := d.Project(&unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "services.serving.knative.dev"},
		"spec": map[string]any{
			"group": "serving.knative.dev", "scope": "Namespaced",
			"names":    map[string]any{"kind": "Service", "plural": "services"},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}},
		},
	}})
	if row.Key != "Service.serving.knative.dev" {
		t.Fatalf("CRD row key = %q", row.Key)
	}
}

// TestEveryDefaultKindIsReservedFromDiscovery: kube decides which keys a
// discovered kind may not take; it must know every key this registry
// defines, or a CRD of that Kind replaces it again.
func TestEveryDefaultKindIsReservedFromDiscovery(t *testing.T) {
	for _, d := range DefaultRegistry().byKind {
		if !kube.IsBuiltinKind(d.Kind) {
			t.Errorf("%q is a built-in registry key kube.IsBuiltinKind doesn't reserve", d.Kind)
		}
	}
}

func groupHas(groups []Group, id GroupID, kind kube.ResourceKind) bool {
	for _, g := range groups {
		if g.ID != id {
			continue
		}
		for _, k := range g.Kinds {
			if k == kind {
				return true
			}
		}
	}
	return false
}
