package kube

import "testing"

func TestParseFluxInventoryID(t *testing.T) {
	for _, tc := range []struct {
		id          string
		ns, name    string
		group, kind string
		ok          bool
	}{
		{"gitlab_webservice_apps_Deployment", "gitlab", "webservice", "apps", "Deployment", true},
		{"_gitlab__Namespace", "", "gitlab", "", "Namespace", true},
		{"gitlab__apps_Deployment", "", "", "", "", false},
		{"not-an-id", "", "", "", "", false},
		{"_system__aggregate-to-view_rbac.authorization.k8s.io_ClusterRole", "", "system:aggregate-to-view", "rbac.authorization.k8s.io", "ClusterRole", true},
		{"kube-system_system__controller__bootstrap-signer_rbac.authorization.k8s.io_Role", "kube-system", "system:controller:bootstrap-signer", "rbac.authorization.k8s.io", "Role", true},
		{"_system__aggregate-to-view_apps_Deployment", "", "", "", "", false},
		{"a_b_c_d_e", "", "", "", "", false},
	} {
		ns, name, group, kind, ok := ParseFluxInventoryID(tc.id)
		if ok != tc.ok || (ok && (ns != tc.ns || name != tc.name || group != tc.group || kind != tc.kind)) {
			t.Errorf("%q = (%q, %q, %q, %q, %v), want (%q, %q, %q, %q, %v)", tc.id, ns, name, group, kind, ok, tc.ns, tc.name, tc.group, tc.kind, tc.ok)
		}
	}
}

func TestFluxManagerPrefersTheHelmRelease(t *testing.T) {
	kind, ns, name, ok := FluxManager(map[string]string{
		FluxGroupKustomize + "/name": "apps", FluxGroupKustomize + "/namespace": "flux-system",
		FluxGroupHelm + "/name": "gitlab", FluxGroupHelm + "/namespace": "gitlab",
	})
	if !ok || kind != KindFluxHelmRelease || ns != "gitlab" || name != "gitlab" {
		t.Errorf("FluxManager = (%q, %q, %q, %v), want the HelmRelease", kind, ns, name, ok)
	}
	if _, _, _, ok := FluxManager(map[string]string{"app": "x"}); ok {
		t.Error("an unlabelled object must not resolve to a reconciler")
	}
}

// TestRegistryKindFor: an inventory entry's (group, Kind) resolves to the key
// kute registers that kind under, never to whichever kind owns the bare name.
func TestRegistryKindFor(t *testing.T) {
	for _, tc := range []struct {
		group, kind string
		want        ResourceKind
	}{
		{"apps", "Deployment", KindDeployment},
		{"", "Service", KindService},
		{"", "Secret", KindSecret},
		{FluxGroupHelm, "HelmRelease", KindFluxHelmRelease},
		{FluxGroupKustomize, "Kustomization", "Kustomization"},
		{"serving.knative.dev", "Service", "Service.serving.knative.dev"},
		{"networking.istio.io", "Gateway", "Gateway.networking.istio.io"},
		{GatewayAPIGroup, "Gateway", KindGateway},
		{"example.com", "Widget", "Widget"},
	} {
		if got := RegistryKindFor(tc.group, tc.kind); got != tc.want {
			t.Errorf("RegistryKindFor(%q, %q) = %q, want %q", tc.group, tc.kind, got, tc.want)
		}
	}
}
