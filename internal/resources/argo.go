// Argo CD's Application kind (docs/design README.md §33a). Application
// carries two independent status axes — sync (is the live state applied
// from git?) and health (is what's applied actually working?) — neither of
// which is a Ready-style condition, so it gets a curated descriptor for the
// same reason §30a's Flux kinds and §23b's HTTPRoute do: the generic
// Ready-condition read (crd.go's projectCustomResource) has nothing to
// scan, and would silently fall back to 14a's "no status semantics" neutral
// dot for a kind whose health signal is very much present, just not shaped
// as a condition.
//
// Recognition happens in BuildDiscoveredRegistry by API group *and* Kind —
// argoproj.io also serves AppProject, which has no sync/health status at
// all and stays on the generic CustomDescriptor path untouched (the same
// Kind-as-filter precedent §23b's httpRouteDescriptor already sets: Gateway
// API's own GRPCRoute/TCPRoute/Gateway kinds share HTTPRoute's group but
// not its ATTACHED treatment).
package resources

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// argoColumns is §33a's curated column set: Sync and Health render as their
// own plain-text columns (Argo's own vocabulary, "Synced"/"OutOfSync"/…,
// "Healthy"/"Degraded"/…) rather than collapsed into one glyph the way a
// single condition would be — the design's own reasoning is that the two
// answer different questions ("did git apply?" vs "is it working?") and
// Synced+Degraded (git is right, workload is sick) needs to read
// differently on screen from OutOfSync+Healthy (drift, nothing broken).
var argoColumns = []string{"Name", "Sync", "Health", "Revision", "Synced", "Age"}

// argoDescriptor builds the §33a list Descriptor for the discovered
// Application kind. Custom stays true — 14d detail routing, 14c's API-group
// type label and 14a's breadcrumb version tag all key off it and all remain
// correct; there is no bespoke Argo detail screen, unlike Flux's 31a.
func argoDescriptor(dk kube.DiscoveredKind) Descriptor {
	return Descriptor{
		Kind:            dk.RegistryKind(),
		Group:           GroupArgo,
		Display:         capitalizePlural(dk.Plural),
		Icon:            "◎",
		Columns:         argoColumns,
		FlexColumn:      "Name",
		Describe:        "argo cd application · " + dk.Group,
		ClusterScoped:   dk.ClusterScoped,
		Custom:          true,
		Argo:            true,
		StatusSemantics: true,
		APIGroup:        dk.Group,
		APIResource:     dk.Plural,
		APIVersion:      dk.GVR.Version,
		Project:         projectArgoResource,
		HealthLabel:     argoHealthLabel,
		HealthGlyph:     argoHealthGlyph,
	}
}

// argoHealthLabel is §33a's health-strip wording: "N healthy · N out of
// sync · N progressing · N degraded" — Argo's own four words, not the
// generic ok/warn/fail/other DefaultHealthLabel would produce.
func argoHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "healthy"
	case StatusWarn:
		return "out of sync"
	case StatusFail:
		return "degraded"
	default:
		return "progressing"
	}
}

// argoHealthGlyph renders §33a's one departure from the generic set: a
// syncing/progressing app's ◌, the same "work is happening" glyph §30a
// gives Flux's own Reconciling — a different meaning on a different kind,
// not a shared constant, so no collision.
func argoHealthGlyph(class StatusClass) string {
	if class == StatusNeutral {
		return "◌"
	}
	return DefaultHealthGlyph(class)
}

// projectArgoResource is §33a's status derivation: Sync and Health read
// directly off status.sync.status/status.health.status — plain strings, no
// conditions array to scan, unlike Flux.
//
// Precedence (docs/design README.md §33a): Degraded/Missing health outranks
// everything (the workload itself is sick, worse than a sync mismatch);
// OutOfSync is next regardless of health (drift is real even when nothing's
// visibly broken yet); Progressing is normal activity, not trouble; Synced+
// Healthy is the quiet state. Only Degraded/Missing and OutOfSync stay
// pinned above the fold (StatusFail/StatusWarn) — Progressing (StatusNeutral)
// and Healthy (StatusOK) both fold into the healthy tail, ordered by
// argoRank (browse/sort.go) rather than by this StatusClass, since Neutral
// sorting after OK in the generic health-rank order would put Progressing
// below Healthy — backwards from §33a's stated "Progressing → Healthy".
func projectArgoResource(obj runtime.Object) Row {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return metaRow(obj)
	}
	ns, name, age := metaOf(obj)

	sync, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	glyph, class := argoStatus(sync, health)
	targetRevision, _, _ := unstructured.NestedString(u.Object, "spec", "source", "targetRevision")
	revision, _, _ := unstructured.NestedString(u.Object, "status", "sync", "revision")

	return Row{
		Namespace: ns,
		Name:      name,
		Cells: []string{
			name,
			argoCell(sync),
			argoCell(health),
			argoRevisionCell(targetRevision, revision),
			argoSyncedCell(u),
			shortAge(age),
		},
		Status:     class,
		Glyph:      glyph,
		GlyphClass: class,
		StatusText: argoStatusText(sync, health),
		SubLine:    argoSubLine(u, health),
	}
}

// argoCell renders an empty Sync/Health field (a brand-new Application
// before its first sync) as "–", 14a's own not-yet-known idiom, rather than
// a blank cell.
func argoCell(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// argoStatus applies §33a's precedence, returning the glyph and the class
// that governs fold-visibility (isUnhealthy) and the health-strip tally —
// never the sort order, which argoRank derives from the rendered cells
// instead (see projectArgoResource's doc comment).
func argoStatus(sync, health string) (glyph string, class StatusClass) {
	switch {
	case health == "Degraded" || health == "Missing":
		return "✕", StatusFail
	case sync == "OutOfSync":
		return "▲", StatusWarn
	case health == "Progressing":
		return "◌", StatusNeutral
	case health == "Healthy" && sync == "Synced":
		return "●", StatusOK
	default:
		// Unknown health/sync, or a combination §33a doesn't name (health
		// Unknown with sync Synced, a brand-new Application with neither
		// field written yet, …) — surfaced rather than silently folded,
		// the same "when in doubt, stay visible" call fluxStatus's own
		// default branch makes.
		return "▲", StatusWarn
	}
}

// argoStatusText is the one-word summary a detail view's title chip shows
// (14d falls back to it before reading raw fields itself) — health when
// there's one to report, else sync, else empty.
func argoStatusText(sync, health string) string {
	if health != "" {
		return strings.ToLower(health)
	}
	if sync != "" {
		return strings.ToLower(sync)
	}
	return ""
}

// argoSubLine is the row's second line for a Degraded/Missing app: the
// sickest managed resource's own health message, verbatim — status.
// resources[] is Argo's own per-object health rollup, the same "prose
// renders as a continuation line, never paraphrased" rule §30a's
// fluxSubLine applies to a Ready condition's message. Falls back to the
// Application's own status.conditions (e.g. ComparisonError) when no
// managed resource carries one. Healthy/OutOfSync/Progressing rows get no
// sub-line: a healthy or merely-drifting app has nothing a table cell
// hasn't already said.
func argoSubLine(u *unstructured.Unstructured, health string) string {
	if health != "Degraded" && health != "Missing" {
		return ""
	}
	items, _, _ := unstructured.NestedSlice(u.Object, "status", "resources")
	for _, item := range items {
		rm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		rHealth, _, _ := unstructured.NestedString(rm, "health", "status")
		if rHealth == "" || rHealth == "Healthy" {
			continue
		}
		msg, _, _ := unstructured.NestedString(rm, "health", "message")
		if msg == "" {
			continue
		}
		kind, _, _ := unstructured.NestedString(rm, "kind")
		name, _, _ := unstructured.NestedString(rm, "name")
		if kind == "" || name == "" {
			return msg
		}
		return kind + "/" + name + ": " + msg
	}
	// No per-resource message found (a Missing resource carries no health
	// block to read a message off) — fall back to the Application's own
	// conditions, same verbatim rule.
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if msg, _, _ := unstructured.NestedString(cm, "message"); msg != "" {
			return msg
		}
	}
	return ""
}

// argoRevisionCell is what this Application actually has deployed:
// spec.source.targetRevision (the git ref — "main", a tag, or empty for
// Argo's own "HEAD" default) alongside the short form of the SHA it
// resolved to. Argo's own status.sync.revision is a bare 40-character SHA
// with no algorithm prefix, unlike Flux's "sha1:"/"sha256:"-prefixed
// digests — shortRevision's colon-splitting logic doesn't apply here, so
// this is its own short-form, not a reuse.
func argoRevisionCell(targetRevision, revision string) string {
	ref := targetRevision
	if ref == "" {
		ref = "HEAD"
	}
	sha := revision
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if sha == "" {
		return ref
	}
	return ref + "@" + sha
}

// argoSyncedCell is how long ago the last sync operation finished —
// status.operationState.finishedAt, the closest honest answer to "when did
// this last do something", the same role fluxReconciled plays for Flux.
func argoSyncedCell(u *unstructured.Unstructured) string {
	ts, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "finishedAt")
	if ts == "" {
		return "–"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "–"
	}
	return shortAge(time.Since(t)) + " ago"
}
