package kube

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ParseFluxInventoryID splits one status.inventory entry id of a Flux
// Kustomization: "<namespace>_<name>_<group>_<Kind>", with an empty first
// segment for a cluster-scoped object. Kubernetes names can't contain an
// underscore, so the split is unambiguous — except for RBAC objects, whose
// names may contain ':' and which cli-utils (the encoder Flux uses) writes
// as "__" ("_system__aggregate-to-view_rbac.authorization.k8s.io_ClusterRole").
// Like cli-utils' own parser, the namespace is the first field, Kind and
// group the last two, and the name everything between, with "__" decoded
// back to ':' for an RBAC group only.
//
// kind is the bare API Kind and group its API group ("" for core). The pair
// is the object's identity — the Kind alone names a different resource
// whenever two groups serve it (Flux's HelmRelease vs §18a's Helm list,
// Knative's Service vs core's) — so resolve it with RegistryKindFor before
// reading or navigating, never ResourceKind(kind).
func ParseFluxInventoryID(id string) (namespace, name, group, kind string, ok bool) {
	namespace, rest, found := strings.Cut(id, "_")
	if !found {
		return "", "", "", "", false
	}
	i := strings.LastIndex(rest, "_")
	if i < 0 {
		return "", "", "", "", false
	}
	rest, kind = rest[:i], rest[i+1:]
	i = strings.LastIndex(rest, "_")
	if i < 0 {
		return "", "", "", "", false
	}
	name, group = rest[:i], rest[i+1:]
	if strings.Contains(name, "_") {
		if group != "rbac.authorization.k8s.io" {
			return "", "", "", "", false
		}
		name = strings.ReplaceAll(name, "__", ":")
		if strings.Contains(name, "_") {
			return "", "", "", "", false
		}
	}
	return namespace, name, group, kind, name != "" && kind != ""
}

// FluxInventoryIDs reads a reconciler's status.inventory entry ids — the
// list a Kustomization publishes of every object it applied. A HelmRelease
// publishes none (Helm owns its object list), so it yields nil.
func FluxInventoryIDs(obj map[string]any) []string {
	status, _ := obj["status"].(map[string]any)
	inv, _ := status["inventory"].(map[string]any)
	entries, _ := inv["entries"].([]any)
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		em, _ := e.(map[string]any)
		if id, _ := em["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// The labels Flux's controllers stamp on every object they apply, naming the
// reconciler that owns it. They are the only in-object record of "who put
// this here" — a Deployment applied by a Kustomization has no ownerReference
// back to it.
const (
	fluxKustomizeNameLabel      = FluxGroupKustomize + "/name"
	fluxKustomizeNamespaceLabel = FluxGroupKustomize + "/namespace"
	fluxHelmNameLabel           = FluxGroupHelm + "/name"
	fluxHelmNamespaceLabel      = FluxGroupHelm + "/namespace"
)

// FluxManager reports which Flux reconciler applied an object, from its
// labels. kind is the *registry* kind (KindFluxHelmRelease for a
// HelmRelease, never the bare API Kind that names §18a's Helm list).
//
// A HelmRelease's labels win over a Kustomization's: a Kustomization that
// applies a HelmRelease doesn't label the release's rendered objects, so an
// object carrying both was relabelled by helm-controller, which is the
// closer owner.
func FluxManager(labels map[string]string) (kind ResourceKind, namespace, name string, ok bool) {
	if n := labels[fluxHelmNameLabel]; n != "" {
		return KindFluxHelmRelease, labels[fluxHelmNamespaceLabel], n, true
	}
	if n := labels[fluxKustomizeNameLabel]; n != "" {
		return ResourceKind("Kustomization"), labels[fluxKustomizeNamespaceLabel], n, true
	}
	return "", "", "", false
}

// fluxSourceRefPaths are the three places a Flux reconciler names what it
// reconciles from: a Kustomization's spec.sourceRef, a HelmRelease's
// spec.chart.spec.sourceRef (its chart names the repository it comes from),
// and a HelmRelease's spec.chartRef (an OCIRepository or HelmChart it
// installs from directly). Every screen reads them through FluxSourceOf —
// a copy that drifted once lost the ref's namespace.
var fluxSourceRefPaths = [][]string{
	{"spec", "sourceRef"},
	{"spec", "chart", "spec", "sourceRef"},
	{"spec", "chartRef"},
}

// FluxSourceOf resolves what a Flux reconciler reconciles from. A ref with
// no namespace of its own defaults to defaultNS (the object's own) — Flux's
// own rule; a HelmRelease in an app namespace pointing at a HelmRepository
// in flux-system is the common layout, not the exception. ok is false for
// an object with no sourceRef, such as the source kinds themselves.
func FluxSourceOf(obj map[string]any, defaultNS string) (kind ResourceKind, namespace, name string, ok bool) {
	for _, path := range fluxSourceRefPaths {
		ref, found, _ := unstructured.NestedMap(obj, path...)
		if !found {
			continue
		}
		k, _, _ := unstructured.NestedString(ref, "kind")
		n, _, _ := unstructured.NestedString(ref, "name")
		if n == "" {
			continue
		}
		ns, _, _ := unstructured.NestedString(ref, "namespace")
		if ns == "" {
			ns = defaultNS
		}
		return ResourceKind(k), ns, n, true
	}
	return "", "", "", false
}

// FluxCondition is one status condition of a Flux object.
type FluxCondition struct {
	Status, Reason, Message, LastTransition string
}

// FluxConditionOf finds one status condition by type.
func FluxConditionOf(obj map[string]any, typ string) (FluxCondition, bool) {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cm, "type"); t != typ {
			continue
		}
		status, _, _ := unstructured.NestedString(cm, "status")
		reason, _, _ := unstructured.NestedString(cm, "reason")
		message, _, _ := unstructured.NestedString(cm, "message")
		lt, _, _ := unstructured.NestedString(cm, "lastTransitionTime")
		return FluxCondition{Status: status, Reason: reason, Message: message, LastTransition: lt}, true
	}
	return FluxCondition{}, false
}

// ShortFluxSourceKind abbreviates a sourceRef kind to the design's compact
// form ("git/nebula-config", not "GitRepository/nebula-config").
func ShortFluxSourceKind(kind string) string {
	switch kind {
	case "GitRepository":
		return "git"
	case "OCIRepository":
		return "oci"
	case "HelmRepository":
		return "helm"
	case "HelmChart":
		return "chart"
	case "Bucket":
		return "bucket"
	default:
		return strings.ToLower(kind)
	}
}
