// Package resources is the display catalog for Kubernetes resource kinds. Each
// kind has a Descriptor that knows its columns and how to project a raw API
// object into a display Row; a Registry maps kinds to descriptors and Groups
// bucket kinds for the Home explorer. This is the layer that makes adding a new
// resource type a single descriptor entry rather than a new screen.
//
// It depends on kube (for ResourceKind), the k8s API types it projects, and
// tui/components (Column/Cell — Theme-agnostic rendering primitives, see
// columns.go) — never on tui itself or any screen package, so the catalog
// stays a pure data/display concern with no Theme or Bubble Tea dependency.
package resources

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// StatusClass is a coarse health classification used to color a row.
type StatusClass string

const (
	StatusNeutral StatusClass = "neutral"
	StatusOK      StatusClass = "ok"
	StatusWarn    StatusClass = "warn"
	StatusFail    StatusClass = "fail"
)

// Row is one projected resource ready for display. Cells align positionally with
// the descriptor's Columns.
type Row struct {
	Namespace string
	Name      string
	Cells     []string
	Status    StatusClass

	// Glyph/GlyphClass are the status-column glyph and its color class
	// (rendered via tui/glyphs.go by the screen, which owns Theme — this
	// package stays Theme-agnostic). GlyphClass defaults to Status when a
	// projection leaves it unset. They differ from Status for exactly two
	// cases today: 18a's outdated release, whose glyph goes warn-yellow
	// while Status stays `deployed` for the strip's own tally, and §35b's
	// ready-but-expiring Certificate, whose glyph goes ◷/warn while Status
	// stays StatusOK so it still counts and sorts as "ready".
	Glyph      string
	GlyphClass StatusClass

	// Cordoned marks a Node row as unschedulable (spec.unschedulable) — the
	// 11a nodes-list C verb toggles off this state, so browse needs it
	// outside the display Cells to know which direction to mutate.
	Cordoned bool

	// NodeShellUnavailable is why the 11a node-shell verb can't work on this
	// Node (EKS Fargate, GKE Autopilot), or "" when it can. It rides here
	// for the same reason Cordoned does: the answer lives in the node's
	// labels, which the display Cells don't carry, and browse needs it at
	// keypress time — see kube.NodeShellUnavailable.
	NodeShellUnavailable string

	// Suspended marks a Flux row whose spec.suspend is set (§30a), a Job
	// row whose own spec.suspend is set (projectJob — Job's own 's' verb),
	// or a CronJob row whose own spec.suspend is set (ProjectCronJob —
	// §36a/§36c's 's' verb). It rides outside Cells for the same reason
	// Cordoned does: the 's' verb toggles this state, so browse needs to
	// know which direction to mutate at keypress time, and the health strip
	// counts it.
	Suspended bool

	// ArgoSources is a multi-source Argo Application's spec.sources count
	// (§33a), 0 for a single-source one. Its REVISION cell reads "N
	// sources" — no one ref names them all — and browse's S then sends a
	// sync with no revision, so Argo syncs every source to its own
	// targetRevision, rather than re-expanding that cell into a ref.
	ArgoSources int

	// Active marks a CronJob row with at least one currently-active
	// associated Job (ProjectCronJob, §36a/§4.3) — the ACT column's own
	// state, outside Cells for the same reason Suspended is: a consumer
	// must read this field rather than re-parsing the ACT cell's digit
	// back into a bool, and the health strip's cross-cutting "active now"
	// segment counts it directly. It cross-cuts Status exactly the way
	// Suspended does — an active CronJob's last *terminal* run can still
	// have been a success, so Status stays StatusOK while Active is true.
	// Unset (false) for every other kind.
	Active bool

	// FluxSource is a §30a Flux row's resolved sourceRef — kind, the ref's
	// own namespace (else the row's, Flux's own default) and name. It rides
	// outside Cells because the SOURCE cell is a display abbreviation that
	// can't be read back into a namespace, and §30a's 'o' jumps to it. Zero
	// for a source kind's own row and for every other kind.
	FluxSource FluxSourceRef

	// StatusText is a projection's own one-word summary of the row's health
	// ("ready", "reconciling", "suspended"), for a detail view's title chip.
	// Empty for every kind with nothing to say beyond its primary condition
	// — 14d falls back to reading the condition itself, so no per-kind
	// knowledge leaks into that screen.
	StatusText string

	// SubLine is a second, full-width line rendered under the row: §30a puts
	// the Ready condition's message there verbatim, because it is prose a
	// table cell could only ellipsize. Empty on rows with nothing to add,
	// which is most of them.
	SubLine string

	// Outdated marks an 18a Helm release whose chart has a newer version in
	// the local repo cache. It rides outside Cells for the same reason
	// Cordoned does: the health strip and the 19a overview both need the
	// fact, and it is deliberately *not* folded into Status — a deployed
	// release that happens to be behind is still deployed, and the strip
	// must keep counting it that way.
	Outdated bool

	// NameSuffix is appended after the NAME cell's text (dim, not part of
	// the filter/sort-relevant Name itself) — 11a's inline control-plane
	// role tag ("node-1 (control-plane)"), and §41e/§41c's Pod ⚑ tags: a
	// real ephemeral container attached (projectPod, from the object's own
	// status) or a debug copy kute itself created (browse's own
	// post-projection decoration from Session.DebugCopies, since that fact
	// isn't on the object at all — see applyRowsLoaded).
	NameSuffix string

	// Key is an opaque identifier for verbs that need to reference the
	// underlying object beyond Namespace/Name — Forwards' session ID (Name
	// is a fuzzy-searchable "port→target" label, not a stable key; the
	// 13c stop/restart verbs need the real ID). Unset (empty) for every
	// other kind.
	Key string

	// ExpiresAt/ExpiresClass/RenewalClass are §35b's Certificate-only
	// fields, outside Cells for the same reason Cordoned/Outdated are: the
	// EXPIRES/RENEWAL cell text alone can't be read back for sorting
	// (ExpiresAt — zero when unknown/not yet issued, browse/sort.go's
	// soonest-expiry tiebreak) or recolored per-cell independently of the
	// row's own Status (ExpiresClass/RenewalClass — StatusWarn <30d out,
	// StatusFail expired, StatusOK/StatusNeutral otherwise). Unset (zero
	// value) for every other kind.
	ExpiresAt    time.Time
	ExpiresClass StatusClass
	RenewalClass StatusClass

	// DurationClass recolors §37a's DURATION cell independently of the row's
	// own Status — StatusFail when the Job hit spec.activeDeadlineSeconds,
	// StatusNeutral otherwise. Unset (StatusNeutral's zero value force) for
	// every other kind. Same "a cell can disagree with the row" idiom
	// ExpiresClass/RenewalClass already establish for Certificate.
	DurationClass StatusClass
}

// HealthCounts tallies a kind's rows by StatusClass, for the browse
// health-strip ("● 32 ▲ 2 ✕ 1"). It's coarse (OK/Warn/Fail/Neutral) rather
// than kind-specific so every Descriptor can share one Health
// implementation (StatusHealth) unless a kind needs bespoke tallying.
type HealthCounts struct {
	OK, Warn, Fail, Neutral int

	// Outdated is 18a's extra strip segment. It cross-cuts the four status
	// buckets rather than joining them (an outdated release is also
	// deployed), so it is excluded from Total and only ever non-zero for the
	// kinds whose Health implementation tallies it.
	Outdated int

	// ExpiringSoon is §35b's extra strip segment ("◷ 1 <30d") — cross-cuts
	// the same way Outdated does: a ready-but-expiring Certificate is still
	// tallied as ready above, and this counts it a second time. Excluded
	// from Total, non-zero only for Certificate's own Health implementation.
	ExpiringSoon int

	// Active is v0.8.0 §36a/§4.3's extra strip segment ("◐ 1 active now")
	// for CronJobs — cross-cuts the same way Outdated/ExpiringSoon do: a
	// CronJob with an active run is already tallied by its last *terminal*
	// outcome above (OK/Fail/Neutral), and this counts it a second time.
	// Excluded from Total, non-zero only for CronJob's own Health
	// implementation.
	Active int

	// Suspended is v0.8.0 §36a/§4.3's extra strip segment ("⏸ 2 suspended")
	// for CronJobs — same cross-cutting reasoning as Active above: a
	// suspended CronJob keeps whatever OK/Fail/Neutral bucket its retained
	// history already earned it. Excluded from Total, non-zero only for
	// CronJob's own Health implementation.
	Suspended int
}

// Total is the row count the counts were tallied from.
func (h HealthCounts) Total() int { return h.OK + h.Warn + h.Fail + h.Neutral }

// StatusHealth tallies rows by Status. It's the default Health
// implementation for every built-in Descriptor.
func StatusHealth(rows []Row) HealthCounts {
	var h HealthCounts
	for _, r := range rows {
		switch r.Status {
		case StatusOK:
			h.OK++
		case StatusWarn:
			h.Warn++
		case StatusFail:
			h.Fail++
		default:
			h.Neutral++
		}
	}
	return h
}

// RawLister fetches raw API objects for a kind from a data source (the informer
// cache in production, a fake in tests). It is the catalog's only dependency on
// live cluster data.
type RawLister interface {
	ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error)
}

// Descriptor describes how to list and render one resource kind.
type Descriptor struct {
	Kind    kube.ResourceKind
	Group   GroupID
	Display string // plural display name, e.g. "Deployments"
	Icon    string
	Columns []string
	// Describe is a short noun phrase for the 3a browse-grid footer, e.g.
	// "running application instances".
	Describe string
	// ClusterScoped kinds have no namespace segment (Nodes, Namespaces) —
	// the browse breadcrumb drops it and tags "cluster-scoped" instead.
	ClusterScoped bool
	// FlexColumn names the Columns entry that should flex to fill leftover
	// table width. Empty defaults to "Name" (every built-in kind but
	// Forwards, whose widest/variable cell is Target instead).
	FlexColumn string
	// Short is the keybar pill's short kind name (§14a: "Keybar pill = short
	// kind name (CERTS)"), set for a discovered kind that declares short
	// names. Empty means the pill is the uppercase Display.
	Short string
	// Project converts a raw API object into a display Row. It must tolerate an
	// unexpected type by returning a best-effort Row (see metaRow).
	Project func(obj runtime.Object) Row
	// Batch, when set, is what List projects a whole read with: called once
	// per List with that read's ctx, it returns a projector whose cross-kind
	// lookups (Ingress backends, a Deployment's previous ReplicaSet) are
	// shared across the batch and parented on ctx, instead of repeated per
	// object against context.Background(). Project stays the per-object form
	// single-object callers use.
	Batch func(ctx context.Context) func(obj runtime.Object) Row
	// Health tallies a kind's rows for the browse health strip. Defaults to
	// StatusHealth for every built-in descriptor; a kind can override it if
	// its health signal isn't well captured by StatusClass alone.
	Health func(rows []Row) HealthCounts
	// HealthLabel names a StatusClass for the health-strip segment ("32
	// running", "2 pending"). Defaults to DefaultHealthLabel's generic
	// wording; Pods overrides it with the docs/design/README.md 2a copy.
	HealthLabel func(StatusClass) string
	// HealthGlyph picks the health-strip glyph for a StatusClass. Defaults
	// to DefaultHealthGlyph; a kind overrides it where its own design
	// section names a different glyph for a class — Nodes render Neutral as
	// ◈ (11a's cordoned), Helm renders Warn as ◌ (18a's pending-upgrade).
	// Lives on the descriptor rather than as a kind-name check in browse so
	// a new kind's strip is declared with the rest of its data.
	HealthGlyph func(StatusClass) string
	// HealthTone remaps a StatusClass to the class whose *colour* renders it,
	// in the strip and on the row glyph alike. Defaults to identity, which is
	// what every kind but one wants: the class carries both the meaning and
	// the hue.
	//
	// Flux is the exception, and it needs the two split. A suspended
	// reconciler must stay StatusNeutral so the strip can count and name it
	// separately from the reconciling ones (its label mapping has four
	// distinct words for four classes), but Neutral's blue is the hue of a
	// Completed pod and a cordoned node — parked, benign, nothing to see. A
	// Kustomization someone paused is not parked: it is silently drifting
	// from git for as long as it stays that way, which is a risk state and
	// reads amber (docs/design README.md §30a).
	HealthTone func(StatusClass) StatusClass
	// Custom marks a kind discovered from a CRD at connect time (14a) — the
	// one generic flag browse/goto key off to route a row to the 14d
	// generic detail screen and the 14c API-group type label, instead of
	// any kind-name check. False for every built-in kind, including the
	// always-present CustomResourceDefinition list itself (14b) — that one
	// is a routing kind, not a discovered instance kind.
	Custom bool
	// APIGroup is the discovered kind's CRD API group (e.g.
	// "cert-manager.io") — non-empty only when Custom is true. The goto
	// palette's type label (14c) uses it instead of the built-in Group
	// taxonomy.
	APIGroup string
	// APIResource is the plural API resource RBAC rules name this kind by
	// ("certificates", or "secrets" for the synthetic Helm Releases kind,
	// which is stored as release Secrets) — set for every discovered kind
	// and for a synthetic kind backed by another resource. Empty for a
	// typed kind, whose resource comes from kube's typed table. Never
	// derived from Display, which carries decorations ("Flux HelmReleases",
	// "Services (example.io)").
	APIResource string
	// APIVersion is the discovered kind's display version (e.g. "v1") —
	// non-empty only when Custom is true. 14a's breadcrumb combines it with
	// APIGroup for the dim "cert-manager.io/v1" tag (docs/design README.md
	// §14a).
	APIVersion string
	// StatusSemantics reports that this descriptor's Project derives a real
	// status. 14a's two never-fake-health fallbacks — the strip's "no status
	// semantics · NAME + AGE only" note and the faint neutral row glyph —
	// apply only to a Custom descriptor *without* it. A Flux list whose rows
	// are all suspended is all-Neutral but emphatically does have status
	// semantics, and saying otherwise there would be a lie.
	StatusSemantics bool
	// Flux marks a discovered kind in one of Flux's API groups (§30a): it
	// carries spec.suspend and honours reconcile.fluxcd.io/requestedAt, so
	// §30a's suspend/resume and reconcile verbs apply. The one flag browse
	// keys off — never a kind-name check, since the Flux kind set is
	// discovered at connect rather than compiled in.
	Flux bool
	// Argo marks the discovered Argo CD Application kind (§33a): it carries
	// status.sync/status.health and honours argocd.argoproj.io/refresh, so
	// §33a's refresh/sync/dashboard-url verbs apply. The one flag browse
	// keys off, same reasoning as Flux — recognition happens once in
	// BuildDiscoveredRegistry, never a kind-name check downstream.
	Argo bool
}

// InstanceCounter reads a live instance count for a discovered kind — the
// 14b CRDs list's COUNT column. ok is false when the count isn't known (cache
// not yet filled, Forbidden, failing), which renders as "–", never 0.
// Satisfied directly by *kube.Cluster and *kube/fake.Cluster.
type InstanceCounter interface {
	CountInstances(kind kube.ResourceKind) (count int, ok bool)
}

// DefaultHealthGlyph is the generic per-class glyph used by every kind that
// doesn't set Descriptor.HealthGlyph. Rune literals rather than the
// tui.Glyph* constants because resources must not import tui — the same
// reason projections.go and crd.go spell their glyphs out.
func DefaultHealthGlyph(class StatusClass) string {
	switch class {
	case StatusOK:
		return "\u25cf" // ●
	case StatusWarn:
		return "\u25b2" // ▲
	case StatusFail:
		return "\u2715" // ✕
	default:
		return "\u25cb" // ○
	}
}

// DefaultHealthTone is the identity remap used by every kind that doesn't
// set Descriptor.HealthTone: a class is rendered in its own colour.
func DefaultHealthTone(class StatusClass) StatusClass { return class }

// DefaultHealthLabel is the generic per-class wording used by every kind
// that doesn't set Descriptor.HealthLabel.
func DefaultHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	default:
		return "other"
	}
}

// sortableRow pairs a projected row with its precomputed sort key, so List's
// comparator never lowercases the same name twice.
type sortableRow struct {
	row  Row
	name string
}

// List fetches and projects every object of the descriptor's kind in namespace.
func List(ctx context.Context, src RawLister, d Descriptor, namespace string) ([]Row, error) {
	objs, err := src.ListRaw(ctx, d.Kind, namespace)
	if err != nil {
		return nil, err
	}
	if d.Batch != nil {
		return projectRows(d.Batch(ctx), objs), nil
	}
	return Project(d, objs), nil
}

// Project converts an already-read object snapshot into the stable display
// ordering List returns. Joins that need the raw objects can use it to avoid
// reading the same informer cache twice.
func Project(d Descriptor, objs []runtime.Object) []Row {
	return projectRows(d.Project, objs)
}

func projectRows(project func(obj runtime.Object) Row, objs []runtime.Object) []Row {
	// The informer cache returns objects in unstable map-iteration order, which
	// makes lists visibly jump on every watch event. Sort into a stable ascending
	// order (namespace, then case-insensitive name) so refreshes don't reshuffle.
	//
	// The lowercased name is computed once per row here rather than inside the
	// comparator. Lowercasing both operands per comparison made this allocate
	// n·log n strings instead of n, on a function that runs on every
	// watch-driven reload of the browse list — 170k allocations for a
	// 5000-object namespace, against 10k now (see BenchmarkList).
	sorted := make([]sortableRow, 0, len(objs))
	for _, obj := range objs {
		row := project(obj)
		sorted = append(sorted, sortableRow{row: row, name: strings.ToLower(row.Name)})
	}
	slices.SortStableFunc(sorted, func(a, b sortableRow) int {
		return cmp.Or(
			cmp.Compare(a.row.Namespace, b.row.Namespace),
			cmp.Compare(a.name, b.name),
		)
	})

	rows := make([]Row, len(sorted))
	for i, s := range sorted {
		rows[i] = s.row
	}
	return rows
}

// Count returns how many objects of kind exist in namespace, for the Home tiles
// and per-kind counts.
func Count(ctx context.Context, src RawLister, kind kube.ResourceKind, namespace string) (int, error) {
	objs, err := src.ListRaw(ctx, kind, namespace)
	if err != nil {
		return 0, err
	}
	return len(objs), nil
}
