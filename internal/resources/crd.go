// This file is the payoff of the kind-registry architecture (docs/design
// README.md §14a/§14b): CRD support is data, not code. CustomDescriptor
// turns one kube.DiscoveredKind into a full 14a instance-list Descriptor —
// columns straight off the CRD's declared additionalPrinterColumns, status
// from a Ready/Available-style condition — with no per-CRD layout code.
// CRDDescriptor is the one static built-in for the 14b CRDs list itself.
// BuildDiscoveredRegistry assembles both into a fresh Registry/Groups pair,
// called at every connect/context-switch/reconnect (internal/app, internal/
// tui/context.go) so a previous context's discovered kinds never linger.
package resources

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/jsonpath"

	"github.com/kute-dev/kute/internal/kube"
)

// CustomDescriptor builds the 14a instance-list Descriptor for one
// discovered kind: NAME leads, AGE trails, the CRD's own declared printer
// columns fill the middle exactly as declared — kute never guesses a
// column that isn't there.
func CustomDescriptor(dk kube.DiscoveredKind) Descriptor {
	columns := make([]string, 0, len(dk.PrinterColumns)+2)
	columns = append(columns, "Name")
	for _, col := range dk.PrinterColumns {
		columns = append(columns, col.Name)
	}
	columns = append(columns, "Age")

	return Descriptor{
		Kind:          dk.RegistryKind(),
		Group:         GroupCustomResources,
		Display:       discoveredDisplay(dk),
		Icon:          "◆",
		Columns:       columns,
		Describe:      "custom resource · " + dk.Group,
		ClusterScoped: dk.ClusterScoped,
		Custom:        true,
		APIGroup:      dk.Group,
		APIVersion:    dk.GVR.Version,
		Project:       projectCustomResource(dk),
	}
}

// capitalizePlural title-cases a CRD's own (conventionally all-lowercase)
// plural name for display — docs/design README.md §14a: breadcrumb/pill
// text reads "Certificates", not the raw "certificates" straight off the
// CRD spec.
func capitalizePlural(plural string) string {
	if plural == "" {
		return plural
	}
	r := []rune(plural)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// discoveredDisplay names a generic discovered kind for the breadcrumb, the
// goto palette and the strip: its plural, plus its API group whenever it is
// registered under a qualified key (kube.QualifiedKind) — exactly the case
// where the bare plural would read the same as the kind that owns the name
// ("Services" core vs Knative's "Services (serving.knative.dev)").
func discoveredDisplay(dk kube.DiscoveredKind) string {
	name := capitalizePlural(dk.Plural)
	if strings.Contains(string(dk.RegistryKind()), ".") {
		return name + " (" + dk.Group + ")"
	}
	return name
}

// projectCustomResource is the one generic projector every discovered kind
// shares: printer-column cells via each column's declared JSONPath, status
// from a Ready/Available-style condition (True→OK, False→Fail, Unknown→
// Warn — a literal 2-way/3-state read of that one condition, no scanning of
// other conditions for an "in-progress" nuance, so "kute never guesses"
// holds for the health signal too), neutral "·" when the object has no such
// condition at all (14a's "never fake health" fallback).
func projectCustomResource(dk kube.DiscoveredKind) func(obj runtime.Object) Row {
	return func(obj runtime.Object) Row {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return metaRow(obj)
		}
		ns, name, age := metaOf(obj)
		cells := make([]string, 0, len(dk.PrinterColumns)+2)
		cells = append(cells, name)
		for _, col := range dk.PrinterColumns {
			cells = append(cells, evalPrinterColumn(u, col.JSONPath))
		}
		cells = append(cells, shortAge(age))

		glyph, class := conditionStatus(u, "Ready", "Available")
		return Row{Namespace: ns, Name: name, Cells: cells, Status: class, Glyph: glyph, GlyphClass: class}
	}
}

// conditionStatus scans obj's status.conditions for the first entry whose
// type matches one of wantTypes, mapping True/False/Unknown to the standard
// glyph set. ("·"/StatusNeutral, no condition found) is the 14a fallback for
// a CRD whose instances carry no status semantics at all.
func conditionStatus(u *unstructured.Unstructured, wantTypes ...string) (glyph string, class StatusClass) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(cm, "type")
		if !slices.Contains(wantTypes, typ) {
			continue
		}
		status, _, _ := unstructured.NestedString(cm, "status")
		switch status {
		case "True":
			return "●", StatusOK
		case "False":
			return "✕", StatusFail
		default:
			return "▲", StatusWarn
		}
	}
	return "·", StatusNeutral
}

// evalPrinterColumn evaluates a CRD-declared additionalPrinterColumns
// jsonPath (a bare path like `.status.conditions[?(@.type=="Ready")].status`
// — the apiserver's own relaxed form, not a full `{...}` template) against
// obj, tolerating a missing field (a brand-new object without status yet)
// as an empty cell rather than an error.
func evalPrinterColumn(u *unstructured.Unstructured, path string) string {
	if path == "" {
		return ""
	}
	jp := jsonpath.New("printercolumn").AllowMissingKeys(true)
	if err := jp.Parse(fmt.Sprintf("{%s}", path)); err != nil {
		return ""
	}
	var buf bytes.Buffer
	if err := jp.Execute(&buf, u.Object); err != nil {
		return ""
	}
	return buf.String()
}

// httpRouteDescriptor is the one bespoke discovered-kind Descriptor (besides
// CRDDescriptor itself): a discovered "HTTPRoute" gains an ATTACHED column
// right after NAME (docs/design README.md §23b: "a valid-but-unattached
// route is the #1 Gateway API footgun") ahead of its own declared printer
// columns, everything else identical to CustomDescriptor.
func httpRouteDescriptor(dk kube.DiscoveredKind) Descriptor {
	columns := make([]string, 0, len(dk.PrinterColumns)+3)
	columns = append(columns, "Name", "Attached")
	for _, col := range dk.PrinterColumns {
		columns = append(columns, col.Name)
	}
	columns = append(columns, "Age")

	return Descriptor{
		Kind:          dk.RegistryKind(),
		Group:         GroupCustomResources,
		Display:       capitalizePlural(dk.Plural),
		Icon:          "◆",
		Columns:       columns,
		Describe:      "custom resource · " + dk.Group,
		ClusterScoped: dk.ClusterScoped,
		Custom:        true,
		APIGroup:      dk.Group,
		APIVersion:    dk.GVR.Version,
		Project:       projectHTTPRoute(dk),
	}
}

// projectHTTPRoute mirrors projectCustomResource but swaps the generic
// Ready-condition health scan for httpRouteAttachedCell's status.parents
// read — an HTTPRoute's health question isn't "is it Ready", it's "did the
// Gateway(s) it asked to attach to accept it".
func projectHTTPRoute(dk kube.DiscoveredKind) func(obj runtime.Object) Row {
	return func(obj runtime.Object) Row {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return metaRow(obj)
		}
		ns, name, age := metaOf(obj)
		attached, status := httpRouteAttachedCell(u)

		cells := make([]string, 0, len(dk.PrinterColumns)+3)
		cells = append(cells, name, attached)
		for _, col := range dk.PrinterColumns {
			cells = append(cells, evalPrinterColumn(u, col.JSONPath))
		}
		cells = append(cells, shortAge(age))

		glyph := "●"
		if status == StatusFail {
			glyph = "✕"
		}
		return Row{Namespace: ns, Name: name, Cells: cells, Status: status, Glyph: glyph, GlyphClass: status}
	}
}

// httpRouteAttachedCell scans status.parents for the first Accepted
// condition, rendering the design's exact copy: green "✓ gw/public" when
// accepted, red "✕ not accepted" (plus the condition's own message, the
// design's "verbatim" requirement) otherwise — including when the route has
// no status.parents at all yet (a brand-new route before the controller has
// reconciled it).
func httpRouteAttachedCell(u *unstructured.Unstructured) (text string, class StatusClass) {
	parents, _, _ := unstructured.NestedSlice(u.Object, "status", "parents")
	for _, p := range parents {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		parentName, _, _ := unstructured.NestedString(pm, "parentRef", "name")
		conds, _, _ := unstructured.NestedSlice(pm, "conditions")
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if typ, _, _ := unstructured.NestedString(cm, "type"); typ != "Accepted" {
				continue
			}
			if st, _, _ := unstructured.NestedString(cm, "status"); st == "True" {
				return "✓ gw/" + parentName, StatusOK
			}
			msg, _, _ := unstructured.NestedString(cm, "message")
			if msg == "" {
				return "✕ not accepted", StatusFail
			}
			return "✕ not accepted: " + msg, StatusFail
		}
	}
	return "✕ not accepted", StatusFail
}

// crdColumns is the 14b CRDs list's fixed column set — every CRD, whatever
// custom kind it declares, reports the same shape about itself.
var crdColumns = []string{"Name", "Group", "Versions", "Scope", "Count", "Age"}

// CRDDescriptor is the 14b CRDs list's built-in Descriptor — cluster-scoped
// like Nodes/Namespaces/Forwards, not itself Custom (it's always present,
// not a discovered instance kind). counter supplies the live COUNT column;
// nil renders every row's count as unknown, "–" (no live cluster to read).
func CRDDescriptor(counter InstanceCounter) Descriptor {
	return crdDescriptor(counter, nil)
}

// crdDescriptor is CRDDescriptor with the discovery snapshot's registry keys
// by CRD name, so a row's ↵ and COUNT address the key that kind was actually
// registered under — which, for a collision only the whole snapshot can
// resolve (kube.AssignRegistryKinds), a CRD object parsed on its own can't
// derive.
func crdDescriptor(counter InstanceCounter, keys map[string]kube.ResourceKind) Descriptor {
	return Descriptor{
		Kind:          kube.KindCustomResourceDefinition,
		Group:         GroupCluster,
		Display:       "CustomResourceDefinitions",
		Icon:          "◆",
		Columns:       crdColumns,
		Describe:      "cluster extension types",
		ClusterScoped: true,
		Project:       projectCRD(counter, keys),
		HealthLabel:   crdHealthLabel,
	}
}

// crdHealthLabel is 14b's own strip wording ("N established · M
// installing") — the generic "N ok · M warn" DefaultHealthLabel falls back
// to doesn't match a CRD's own Established-condition semantics.
func crdHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "established"
	default:
		return "installing"
	}
}

// projectCRD renders one CustomResourceDefinition row: glyph from its own
// Established condition (▲ while the API hasn't started serving it yet per
// 14b), VERSIONS joins every declared version (deprecated ones marked),
// COUNT from counter — the one place a Descriptor's Project reads live
// cluster state beyond its own object, since a CRD's instance count isn't a
// field on the CRD itself (metaOf's own Age computation already reads the
// wall clock the same way, so Project was never perfectly pure to begin
// with).
func projectCRD(counter InstanceCounter, keys map[string]kube.ResourceKind) func(obj runtime.Object) Row {
	return func(obj runtime.Object) Row {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return metaRow(obj)
		}
		dk, ok := kube.ParseDiscoveredKind(u)
		if !ok {
			return metaRow(obj)
		}
		_, name, age := metaOf(obj)

		glyph, class := "▲", StatusWarn
		if dk.Established {
			glyph, class = "●", StatusOK
		}

		key := dk.RegistryKind()
		if k, ok := keys[dk.CRDName]; ok {
			key = k
		}
		// "–", not 0, until the count is actually known — a cache still
		// filling or a Forbidden kind has not reported zero instances
		// (docs/lazy-informers.md §4: counts are never fabricated).
		count := "–"
		if counter != nil {
			if n, ok := counter.CountInstances(key); ok {
				count = strconv.Itoa(n)
			}
		}

		return Row{
			Name: name,
			Cells: []string{
				name, dk.Group, versionsCell(dk.Versions), scopeCell(dk.ClusterScoped),
				count, shortAge(age),
			},
			Status: class, Glyph: glyph, GlyphClass: class,
			// Key carries the discovered instance kind's ResourceKind
			// string — browse's ↵ on a CRD row reads it back to build the
			// GotoKindMsg that jumps into 14a's instance list, the same
			// opaque-id convention Forward's session ID already uses.
			Key: string(key),
		}
	}
}

func versionsCell(versions []kube.CRDVersion) string {
	if len(versions) == 0 {
		return "–"
	}
	names := make([]string, 0, len(versions))
	for _, v := range versions {
		name := v.Name
		if v.Deprecated {
			name += " (deprecated)"
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

func scopeCell(clusterScoped bool) string {
	if clusterScoped {
		return "Cluster"
	}
	return "Namespaced"
}

// ClusterReader is what BuildDiscoveredRegistry needs from a live cluster:
// InstanceCounter for the 14b CRDs list's live COUNT column, RawLister for
// cross-object resolution a Project closure needs beyond its own object —
// currently just Ingress's live backend health (docs/design README.md
// §23a). Satisfied directly by *kube.Cluster and *kube/fake.Cluster (both
// already implement RawLister for browse and InstanceCounter for 14b).
type ClusterReader interface {
	InstanceCounter
	RawLister
}

// BuildDiscoveredRegistry rebuilds a fresh Registry/Groups pair from
// scratch: DefaultRegistry()/DefaultGroups() plus CRDDescriptor, one
// CustomDescriptor per discovered kind (httpRouteDescriptor instead for a
// discovered "HTTPRoute" — §23b's ATTACHED column), and a live-backend
// Ingress override. Called wholesale (never incrementally) at every
// connect/context-switch/reconnect/demo-startup so a previous context's
// discovered kinds can never linger — pure, no Session/Cluster dependency,
// so it's unit-testable on its own.
func BuildDiscoveredRegistry(discovered []kube.DiscoveredKind, reader ClusterReader) (Registry, []Group) {
	registry := DefaultRegistry()
	keys := make(map[string]kube.ResourceKind, len(discovered))
	for _, dk := range discovered {
		keys[dk.CRDName] = dk.RegistryKind()
	}
	registry.Register(crdDescriptor(reader, keys))
	// Ingress stays in DefaultRegistry with a nil-safe Project (pre-connect
	// fallback renders every backend "–"); once a live reader exists, swap in
	// the closure that actually resolves Service/Pod backend health.
	if ingressDesc, ok := registry.Descriptor(kube.KindIngress); ok {
		ingressDesc.Project = projectIngress(reader)
		registry.Register(ingressDesc)
	}
	// Deployment stays in DefaultRegistry with a nil-safe Project (pre-connect
	// fallback never shows the "new ← old" transition); once a live reader
	// exists, swap in the closure that resolves the previous ReplicaSet's
	// image — same pattern as Ingress just above.
	if deployDesc, ok := registry.Descriptor(kube.KindDeployment); ok {
		deployDesc.Project = projectDeployment(reader)
		registry.Register(deployDesc)
	}
	var fluxKinds, argoKinds, customKinds []kube.ResourceKind
	for _, dk := range discovered {
		// Never let a discovered kind replace an entry already here — a
		// built-in, or an earlier discovered kind. kube.DiscoveredKind's
		// RegistryKind already qualifies every key it knows is taken; this
		// is the backstop for one it doesn't, since Register is
		// last-write-wins and a replaced built-in fails silently (the
		// Services list rendering NAME/AGE for Knative's schema).
		if _, taken := registry.Descriptor(dk.RegistryKind()); taken {
			continue
		}
		switch {
		case kube.IsFluxGroup(dk.Group):
			// By API group, never by bare Kind name — Flux's HelmRelease
			// shares its Kind with 18a's, and a name check is the bug §30a
			// exists to prevent (see kube.IsFluxGroup).
			registry.Register(fluxDescriptor(dk))
			fluxKinds = append(fluxKinds, dk.RegistryKind())
		case kube.IsArgoGroup(dk.Group) && dk.Kind == "Application":
			// Group *and* Kind — argoproj.io also serves AppProject, which
			// has no sync/health status and stays on the generic
			// CustomDescriptor path below (see resources/argo.go's package
			// doc comment).
			registry.Register(argoDescriptor(dk))
			argoKinds = append(argoKinds, dk.RegistryKind())
		case dk.Group == kube.GatewayAPIGroup && dk.Kind == string(kube.KindHTTPRoute):
			registry.Register(httpRouteDescriptor(dk))
			customKinds = append(customKinds, dk.RegistryKind())
		case dk.Group == kube.CertManagerGroup && dk.Kind == string(kube.KindCertificate):
			// §35b: EXPIRES/RENEWAL only mean anything read off
			// Certificate's own status, so it gets the same curated
			// treatment HTTPRoute's ATTACHED column does. Group *and*
			// Kind, like Flux and Argo: ACK's ACM serves a Certificate
			// too, with none of these fields.
			registry.Register(certificateDescriptor(dk))
			customKinds = append(customKinds, dk.RegistryKind())
		default:
			registry.Register(CustomDescriptor(dk))
			customKinds = append(customKinds, dk.RegistryKind())
		}
	}

	groups := DefaultGroups()
	if len(fluxKinds) > 0 {
		groups = append(groups, Group{ID: GroupFlux, Icon: "⇅", Kinds: fluxKinds})
	}
	if len(argoKinds) > 0 {
		groups = append(groups, Group{ID: GroupArgo, Icon: "◎", Kinds: argoKinds})
	}
	// Gated on customKinds rather than on discovered, so a cluster running
	// Flux/Argo and nothing else doesn't get an empty Custom Resources group.
	if len(customKinds) > 0 {
		groups = append(groups, Group{ID: GroupCustomResources, Icon: "◆", Kinds: customKinds})
	}
	return registry, groups
}
