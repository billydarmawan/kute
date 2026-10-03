// Flux CD kinds (docs/design README.md §30a). Flux's five API groups are a
// fixed, versioned, documented status vocabulary rather than the unknown
// long tail CustomDescriptor serves, so they get a curated descriptor —
// the same narrow exception §23b already makes for HTTPRoute, and for the
// same reason: their health question is not "does Ready say True".
//
// Recognition happens in BuildDiscoveredRegistry by API group, never by
// bare Kind name (see kube.IsFluxGroup).
package resources

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// fluxColumns is §30a's curated column set, the same for every Flux kind.
//
// Every Flux CRD declares exactly Ready, Status and Age (plus URL on the
// source kinds) — measured across all 11 CRDs on a real cluster. READY and
// AGE are kept; the declared Status column is *not* a cell, because its
// value is prose ("health check failed after 2m0s: Deployment/nebula-stage/
// nebula-worker status: 'InProgress'") that a table cell can only ellipsize
// into uselessness — it renders verbatim as the row's sub-line instead.
// REVISION and SOURCE are derived (Flux declares neither) and answer the
// two questions the declared set cannot: what is deployed, and from where.
var fluxColumns = []string{"Name", "Ready", "Revision", "Source", "Reconciled", "Age"}

// fluxDescriptor builds the §30a list Descriptor for one discovered Flux
// kind. Custom stays true — 14d detail routing, 14c's API-group type label
// and 14a's breadcrumb version tag all key off it and all remain correct.
func fluxDescriptor(dk kube.DiscoveredKind) Descriptor {
	return Descriptor{
		Kind:            dk.RegistryKind(),
		Group:           GroupFlux,
		Display:         fluxDisplay(dk),
		Icon:            "⇅",
		Columns:         fluxColumns,
		FlexColumn:      "Name",
		Describe:        "flux resource · " + dk.Group,
		ClusterScoped:   dk.ClusterScoped,
		Custom:          true,
		Flux:            true,
		StatusSemantics: true,
		APIGroup:        dk.Group,
		APIVersion:      dk.GVR.Version,
		Project:         projectFluxResource,
		HealthLabel:     fluxHealthLabel,
		HealthGlyph:     fluxHealthGlyph,
		HealthTone:      fluxHealthTone,
	}
}

// fluxDisplay names the kind for the breadcrumb, the goto palette and the
// strip. A substituted kind gets a "Flux " prefix, and only a substituted
// one does.
//
// The prefix exists exactly where the collision made the bare name
// ambiguous: "Helmreleases" and §18a's "Helm Releases" are one keystroke
// apart in the fuzzy palette, so `g "helm"` became a coin flip between two
// genuinely different kinds. Kustomizations and the source kinds collide
// with nothing and keep their own plural.
func fluxDisplay(dk kube.DiscoveredKind) string {
	name := capitalizePlural(dk.Plural)
	if dk.RegistryKind() != kube.ResourceKind(dk.Kind) {
		return "Flux " + name
	}
	return name
}

// fluxHealthLabel is §30a's health-strip wording: "12 ready · 2 reconciling
// · 1 stalled · 3 suspended".
func fluxHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "ready"
	case StatusWarn:
		return "reconciling"
	case StatusFail:
		return "failed"
	default:
		return "suspended"
	}
}

// fluxHealthGlyph renders §30a's two departures from the generic set: a
// suspended object's ‖ (Neutral) and a reconciling one's ◌ (Warn).
func fluxHealthGlyph(class StatusClass) string {
	switch class {
	case StatusNeutral:
		return "‖" // ‖
	case StatusWarn:
		return "◌" // ◌
	default:
		return DefaultHealthGlyph(class)
	}
}

// fluxHealthTone renders §30a's suspended state amber rather than the
// Neutral blue its class would otherwise give it.
//
// The class has to stay Neutral: it is what lets the strip count and label
// suspended objects separately from reconciling ones, and what keeps
// fluxStatus's precedence expressible at all. But the hue is a separate
// claim, and Neutral's blue is the hue of a Completed pod and a cordoned
// node — states that are finished or deliberately parked, with nothing to
// act on. A suspended Kustomization is neither: it is drifting from git for
// as long as it stays paused, which is the one thing an operator scanning
// the list needs to notice. §31a's own SUSPENDED card has always been
// amber; this is what stops the list and the detail screen disagreeing
// about the same object.
func fluxHealthTone(class StatusClass) StatusClass {
	if class == StatusNeutral {
		return StatusWarn
	}
	return class
}

// projectFluxResource is §30a's status derivation, in strict precedence.
//
// Suspended outranks everything because a suspended object's conditions are
// frozen at whatever they said when reconciliation stopped: reporting Ready
// from a paused object is reporting a fact that has stopped being true.
// Stalled is terminal failure. Reconciling with Ready=False is *normal
// progress* — treating it as failure is the single most common way a GitOps
// dashboard cries wolf, and is exactly what the generic conditionStatus
// read got wrong.
//
// Nothing here requires Reconciling or Stalled to be present: on a settled
// cluster the only condition is Ready, so the table degrades to the plain
// Ready read rather than to "unknown".
func projectFluxResource(obj runtime.Object) Row {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return metaRow(obj)
	}
	ns, name, age := metaOf(obj)

	suspended := fluxSuspended(u)
	glyph, class, ready := fluxStatus(u, suspended)
	src := fluxSource(u, ns)

	row := Row{
		Namespace: ns,
		Name:      name,
		Cells: []string{
			name,
			ready,
			fluxRevision(u),
			fluxSourceCell(src, ns),
			fluxReconciled(u),
			shortAge(age),
		},
		Status:     class,
		Glyph:      glyph,
		GlyphClass: class,
		Suspended:  suspended,
		StatusText: fluxStatusText(class, suspended),
		SubLine:    fluxSubLine(u, suspended),
		FluxSource: src,
	}
	return row
}

// fluxSuspended reads spec.suspend, which is absent rather than false on an
// unsuspended object.
func fluxSuspended(u *unstructured.Unstructured) bool {
	v, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend")
	return v
}

// fluxStatus applies §30a's precedence, returning the glyph, the class and
// the READY cell's text.
func fluxStatus(u *unstructured.Unstructured, suspended bool) (glyph string, class StatusClass, ready string) {
	if suspended {
		return "‖", StatusNeutral, "suspended" // ‖
	}
	if cond, ok := fluxCondition(u, "Stalled"); ok && cond.Status == "True" {
		return "✕", StatusFail, "False" // ✕
	}
	readyCond, hasReady := fluxCondition(u, "Ready")
	switch {
	case hasReady && readyCond.Status == "True":
		return "●", StatusOK, "True" // ●
	case hasReady && readyCond.Status == "False":
		// Reconciling alongside a False Ready is progress, not failure.
		if rec, ok := fluxCondition(u, "Reconciling"); ok && rec.Status == "True" {
			return "◌", StatusWarn, "False" // ◌
		}
		return "✕", StatusFail, "False" // ✕
	case hasReady:
		// Ready=Unknown + Reconciling=True is Flux's ordinary in-progress
		// state, the one every reconcile passes through.
		if rec, ok := fluxCondition(u, "Reconciling"); ok && rec.Status == "True" {
			return "◌", StatusWarn, readyCond.Status // ◌
		}
		return "▲", StatusWarn, readyCond.Status // ▲ (Unknown)
	}
	// No Ready condition at all. Unlike 14a's neutral "no status semantics"
	// fallback this is a real pending state — every Flux kind has a status
	// subresource its controller always writes — and keeping Neutral to mean
	// exactly "suspended" is what keeps the strip's suspended count honest.
	if rec, ok := fluxCondition(u, "Reconciling"); ok && rec.Status == "True" {
		return "◌", StatusWarn, "–" // ◌
	}
	return "▲", StatusWarn, "–" // ▲
}

// fluxStatusText is the one-word summary a detail view's title chip shows,
// so 14d needs no Flux-specific condition knowledge of its own.
func fluxStatusText(class StatusClass, suspended bool) string {
	if suspended {
		return "suspended"
	}
	switch class {
	case StatusOK:
		return "ready"
	case StatusWarn:
		return "reconciling"
	case StatusFail:
		return "failed"
	default:
		return ""
	}
}

// fluxSubLine is the row's second line: the Ready condition's message
// verbatim — the CRD's own declared Status column, moved out of a cell
// because it is prose.
//
// Three rows deliberately get none. A healthy row's message is just
// "Applied revision: <sha>", which the REVISION cell already says. A
// suspended row's would only repeat the READY cell's own "suspended" —
// §30a draws a drift note there ("source is N commits ahead"), but that
// needs the source object's artifact revision, which is a cross-kind read
// a projection can't make, and the commit *count* is unbuildable at all
// (see docs/flux-plan.md G1). Saying nothing beats saying the same word
// twice.
func fluxSubLine(u *unstructured.Unstructured, suspended bool) string {
	if suspended {
		return ""
	}
	cond, ok := fluxCondition(u, "Ready")
	if !ok || cond.Status == "True" || cond.Message == "" {
		return ""
	}
	return cond.Message
}

// fluxCondition finds one condition by type.
func fluxCondition(u *unstructured.Unstructured, typ string) (kube.FluxCondition, bool) {
	return kube.FluxConditionOf(u.Object, typ)
}

// fluxRevision is what this object actually has deployed, across the three
// shapes Flux uses for it.
func fluxRevision(u *unstructured.Unstructured) string {
	for _, path := range [][]string{
		{"status", "lastAppliedRevision"},   // Kustomization
		{"status", "artifact", "revision"},  // sources
		{"status", "lastAttemptedRevision"}, // HelmRelease (applied is null)
	} {
		if v, _, _ := unstructured.NestedString(u.Object, path...); v != "" {
			return kube.ShortFluxRevision(v)
		}
	}
	// HelmRelease's real deployed version lives in its history head.
	hist, _, _ := unstructured.NestedSlice(u.Object, "status", "history")
	if len(hist) > 0 {
		if hm, ok := hist[0].(map[string]any); ok {
			if v, _, _ := unstructured.NestedString(hm, "chartVersion"); v != "" {
				return v
			}
		}
	}
	return "–"
}

// FluxSourceRef is the object a Flux reconciler reconciles from.
type FluxSourceRef struct {
	Kind      kube.ResourceKind
	Namespace string
	Name      string
}

// fluxSource resolves what this object reconciles from (kube.FluxSourceOf).
// Zero for the source kinds themselves, which have no sourceRef.
func fluxSource(u *unstructured.Unstructured, namespace string) FluxSourceRef {
	kind, ns, name, ok := kube.FluxSourceOf(u.Object, namespace)
	if !ok {
		return FluxSourceRef{}
	}
	return FluxSourceRef{Kind: kind, Namespace: ns, Name: name}
}

// fluxSourceCell renders the SOURCE cell. The source kinds themselves read
// "–": they are the source, and their declared URL column carries the
// identity. A source in another namespace says so, as §31a's chain does.
func fluxSourceCell(src FluxSourceRef, namespace string) string {
	if src.Name == "" {
		return "–"
	}
	cell := src.Name
	if src.Kind != "" {
		cell = kube.ShortFluxSourceKind(string(src.Kind)) + "/" + src.Name
	}
	if src.Namespace != namespace {
		cell += " in " + src.Namespace
	}
	return cell
}

// fluxReconciled is how long ago the Ready condition last changed — the
// closest honest answer to "when did this last do something". Flux records
// no separate reconcile timestamp.
func fluxReconciled(u *unstructured.Unstructured) string {
	cond, ok := fluxCondition(u, "Ready")
	if !ok || cond.LastTransition == "" {
		return "–"
	}
	ts, err := time.Parse(time.RFC3339, cond.LastTransition)
	if err != nil {
		return "–"
	}
	return shortAge(time.Since(ts)) + " ago"
}
