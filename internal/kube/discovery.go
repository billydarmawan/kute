package kube

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// crdGVR is the always-watched, cluster-scoped resource behind the 14b CRDs
// list — the seed every other discovered kind (14a) is parsed from.
var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// PrinterColumn is one CRD-declared additionalPrinterColumn (docs/design
// README.md §14a: "kute never guesses" — columns come straight from the
// CRD, never inferred from the schema).
type PrinterColumn struct {
	Name     string
	Type     string
	JSONPath string
}

// CRDVersion is one served/storage version entry off a CRD's spec.versions
// — the 14b VERSIONS column dims Deprecated ones.
type CRDVersion struct {
	Name       string
	Served     bool
	Storage    bool
	Deprecated bool
}

// DiscoveredKind is one CRD's shape as of the last discovery pass — the
// per-context cache (docs/design README.md's "discovery" state entry) that
// feeds dynamically registered kind-registry entries (14a/14d), the CRDs
// list (14b), and the goto corpus (14c).
type DiscoveredKind struct {
	GVR    schema.GroupVersionResource // the display version's group/version + plural resource
	Kind   string                      // singular Kind, e.g. "Certificate"
	Plural string
	// ShortNames are the resource's declared short names from API discovery
	// (cert-manager's Certificate: cert, certs) — 14a's keybar pill.
	ShortNames []string
	Group      string
	// Versions is every version declared on the CRD (spec.versions),
	// served or not — the 14b VERSIONS column renders all of them,
	// dimming Deprecated ones.
	Versions []CRDVersion
	// ClusterScoped mirrors resources.Descriptor.ClusterScoped for the
	// discovered kind (spec.scope == "Cluster").
	ClusterScoped bool
	// PrinterColumns come from the display version (its storage version,
	// or the first served version if none is marked storage) —
	// Name/Age entries are filtered out even if a CRD author redundantly
	// declared them, since every kind gets those two implicitly.
	PrinterColumns []PrinterColumn
	// Established mirrors the CRD's own Established condition — 14b's "▲
	// until the API serves them" glyph, and 14a/refreshDiscovery's gate on
	// whether to start watching instances at all.
	Established bool
	// CRDName is the CustomResourceDefinition object's own metadata.name
	// (<plural>.<group>) — 14b's delete target.
	CRDName string
	// AssignedKind, when non-empty, overrides RegistryKind's own derivation.
	// AssignRegistryKinds sets it on the losers of a collision only it can
	// see — two discovered groups serving the same otherwise-unclaimed Kind
	// (CAPI and CNPG "Cluster") — since that depends on the whole snapshot,
	// not on the one kind.
	AssignedKind ResourceKind
}

// RegistryKind is the ResourceKind the kind registry addresses dk by:
// ResourceKind(dk.Kind) unless dk's (Group, Kind) pair is in the
// substitution table, in which case the substituted key.
//
// Every lookup that resolves a ResourceKind against the discovery snapshot
// goes through this, so a substituted key still finds the real GVR — only
// the map key changes, never the coordinates kute watches.
//
// A discovered kind never takes a key something else owns: a Kind that is a
// built-in registry key (IsBuiltinKind — Knative's Service), or a bare Kind
// kute routes on that belongs to another group (ownedKinds — Istio's
// Gateway), is registered under QualifiedKind instead. Both sides stay
// reachable; neither replaces the other.
func (dk DiscoveredKind) RegistryKind() ResourceKind {
	if dk.AssignedKind != "" {
		return dk.AssignedKind
	}
	for k, s := range substitutedKinds {
		if s.group == dk.Group && s.apiKind == dk.Kind {
			return k
		}
	}
	if IsBuiltinKind(ResourceKind(dk.Kind)) {
		return QualifiedKind(dk.Kind, dk.Group)
	}
	if owner, ok := ownedKinds[dk.Kind]; ok && owner != dk.Group {
		return QualifiedKind(dk.Kind, dk.Group)
	}
	return ResourceKind(dk.Kind)
}

// AssignRegistryKinds resolves the collisions RegistryKind can't see from one
// kind alone: two discovered groups whose Kinds would land on the same key.
// The owning group (ownedKinds) keeps the bare key if it's one of them;
// otherwise the lexically first group does, so the outcome never depends on
// discovery order. Every other one gets QualifiedKind. in must already hold
// one entry per (Group, Kind); the returned slice is a copy.
func AssignRegistryKinds(in []DiscoveredKind) []DiscoveredKind {
	out := slices.Clone(in)
	byKey := make(map[ResourceKind][]int, len(out))
	for i := range out {
		out[i].AssignedKind = ""
		k := out[i].RegistryKind()
		byKey[k] = append(byKey[k], i)
	}
	for _, idx := range byKey {
		if len(idx) < 2 {
			continue
		}
		winner := idx[0]
		for _, i := range idx[1:] {
			if out[i].Group < out[winner].Group {
				winner = i
			}
		}
		for _, i := range idx {
			if ownedKinds[out[i].Kind] == out[i].Group {
				winner = i
			}
		}
		for _, i := range idx {
			if i != winner {
				out[i].AssignedKind = QualifiedKind(out[i].Kind, out[i].Group)
			}
		}
	}
	return out
}

// ParseDiscoveredKind extracts a DiscoveredKind from one CRD object's
// unstructured form. It is pure (no cluster access) so it's unit-testable
// against hand-built fixtures, and exported so resources.projectCRD (the
// 14b CRDs list's own row projection) can read the same CRD shape without a
// second, independently maintained parser. ok is false when the object is
// missing the fields every CRD must have (group/kind/plural) — never
// expected in practice, but a defensive skip beats a panic on a malformed
// object.
func ParseDiscoveredKind(u *unstructured.Unstructured) (DiscoveredKind, bool) {
	group, _, _ := unstructured.NestedString(u.Object, "spec", "group")
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "names", "kind")
	plural, _, _ := unstructured.NestedString(u.Object, "spec", "names", "plural")
	scope, _, _ := unstructured.NestedString(u.Object, "spec", "scope")
	if group == "" || kind == "" || plural == "" {
		return DiscoveredKind{}, false
	}

	versionsRaw, _, _ := unstructured.NestedSlice(u.Object, "spec", "versions")
	versions := make([]CRDVersion, 0, len(versionsRaw))
	var displayVersion map[string]any
	for _, v := range versionsRaw {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(vm, "name")
		served, _, _ := unstructured.NestedBool(vm, "served")
		storage, _, _ := unstructured.NestedBool(vm, "storage")
		deprecated, _, _ := unstructured.NestedBool(vm, "deprecated")
		versions = append(versions, CRDVersion{Name: name, Served: served, Storage: storage, Deprecated: deprecated})
		if storage {
			displayVersion = vm
		}
	}
	if displayVersion == nil {
		for _, v := range versionsRaw {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if served, _, _ := unstructured.NestedBool(vm, "served"); served {
				displayVersion = vm
				break
			}
		}
	}

	var printerCols []PrinterColumn
	displayVersionName := ""
	if displayVersion != nil {
		displayVersionName, _, _ = unstructured.NestedString(displayVersion, "name")
		colsRaw, _, _ := unstructured.NestedSlice(displayVersion, "additionalPrinterColumns")
		for _, c := range colsRaw {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(cm, "name")
			if strings.EqualFold(name, "Name") || strings.EqualFold(name, "Age") {
				continue
			}
			typ, _, _ := unstructured.NestedString(cm, "type")
			jsonPath, _, _ := unstructured.NestedString(cm, "jsonPath")
			printerCols = append(printerCols, PrinterColumn{Name: name, Type: typ, JSONPath: jsonPath})
		}
	}

	established := false
	condsRaw, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range condsRaw {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(cm, "type")
		status, _, _ := unstructured.NestedString(cm, "status")
		if typ == "Established" && status == "True" {
			established = true
		}
	}

	return DiscoveredKind{
		GVR:            schema.GroupVersionResource{Group: group, Version: displayVersionName, Resource: plural},
		Kind:           kind,
		Plural:         plural,
		Group:          group,
		Versions:       versions,
		ClusterScoped:  scope == "Cluster",
		PrinterColumns: printerCols,
		Established:    established,
		CRDName:        u.GetName(),
	}, true
}
