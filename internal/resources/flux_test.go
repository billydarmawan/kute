package resources

import (
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/kube"
)

func fluxKustomizationDiscoveredKind() kube.DiscoveredKind {
	return kube.DiscoveredKind{
		Kind: "Kustomization", Plural: "kustomizations", Group: kube.FluxGroupKustomize,
		GVR:         schema.GroupVersionResource{Group: kube.FluxGroupKustomize, Version: "v1", Resource: "kustomizations"},
		Versions:    []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		Established: true,
		CRDName:     "kustomizations." + kube.FluxGroupKustomize,
	}
}

func fluxHelmReleaseDiscoveredKind() kube.DiscoveredKind {
	return kube.DiscoveredKind{
		Kind: "HelmRelease", Plural: "helmreleases", Group: kube.FluxGroupHelm,
		GVR:         schema.GroupVersionResource{Group: kube.FluxGroupHelm, Version: "v2", Resource: "helmreleases"},
		Versions:    []kube.CRDVersion{{Name: "v2", Served: true, Storage: true}},
		Established: true,
		CRDName:     "helmreleases." + kube.FluxGroupHelm,
	}
}

// TestFluxHelmReleaseDoesNotReplaceHelmReleasesList is the regression this
// whole design exists for. Registry.Register is last-write-wins on the bare
// Kind, so before the substitution a Flux cluster's discovered HelmRelease
// silently overwrote 18a's Helm-3 release descriptor: the Helm Releases
// list rendered NAME+AGE only (its projection type-asserts a decoded
// release and missed), and Flux's own HelmReleases never appeared at all.
func TestFluxHelmReleaseDoesNotReplaceHelmReleasesList(t *testing.T) {
	t.Parallel()
	reg, _ := BuildDiscoveredRegistry([]kube.DiscoveredKind{fluxHelmReleaseDiscoveredKind()}, nil)

	helm, ok := reg.Descriptor(kube.KindHelmRelease)
	if !ok {
		t.Fatal("18a's Helm Releases descriptor is gone entirely")
	}
	if helm.Display != "Helm Releases" {
		t.Errorf("18a's descriptor was overwritten: Display = %q, want %q", helm.Display, "Helm Releases")
	}
	if helm.Custom {
		t.Error("18a's descriptor was overwritten by a discovered one (Custom = true)")
	}
	if helm.Flux {
		t.Error("18a's Helm-3 release kind must not be marked Flux")
	}

	fluxHR, ok := reg.Descriptor(kube.KindFluxHelmRelease)
	if !ok {
		t.Fatal("Flux's own HelmRelease was not registered under its substituted key")
	}
	if !fluxHR.Flux || !fluxHR.Custom {
		t.Errorf("Flux HelmRelease descriptor flags: Flux=%v Custom=%v, want both true", fluxHR.Flux, fluxHR.Custom)
	}
	if fluxHR.APIGroup != kube.FluxGroupHelm {
		t.Errorf("APIGroup = %q, want %q", fluxHR.APIGroup, kube.FluxGroupHelm)
	}
}

// TestFluxKindsAppearInExactlyOneGroup pins the mutual exclusion: a Flux
// kind belongs to GroupFlux and must not also be listed under Custom
// Resources, or the goto corpus offers it twice.
func TestFluxKindsAppearInExactlyOneGroup(t *testing.T) {
	t.Parallel()
	discovered := []kube.DiscoveredKind{
		fluxKustomizationDiscoveredKind(),
		fluxHelmReleaseDiscoveredKind(),
		certificateDiscoveredKind(),
	}
	_, groups := BuildDiscoveredRegistry(discovered, nil)

	var flux, custom []kube.ResourceKind
	for _, g := range groups {
		switch g.ID {
		case GroupFlux:
			flux = g.Kinds
		case GroupCustomResources:
			custom = g.Kinds
		}
	}
	if !slices.Contains(flux, kube.ResourceKind("Kustomization")) {
		t.Errorf("Kustomization missing from the Flux group, got %v", flux)
	}
	if !slices.Contains(flux, kube.KindFluxHelmRelease) {
		t.Errorf("Flux HelmRelease missing from the Flux group, got %v", flux)
	}
	for _, k := range flux {
		if slices.Contains(custom, k) {
			t.Errorf("%s is in both the Flux and Custom Resources groups", k)
		}
	}
	if !slices.Contains(custom, kube.ResourceKind("Certificate")) {
		t.Errorf("a non-Flux CRD should stay in Custom Resources, got %v", custom)
	}
}

// TestFluxOnlyClusterGetsNoEmptyCustomGroup guards the gating change: the
// Custom Resources group keys off its own kinds now, not off len(discovered).
func TestFluxOnlyClusterGetsNoEmptyCustomGroup(t *testing.T) {
	t.Parallel()
	_, groups := BuildDiscoveredRegistry([]kube.DiscoveredKind{fluxKustomizationDiscoveredKind()}, nil)
	for _, g := range groups {
		if g.ID == GroupCustomResources {
			t.Fatalf("expected no Custom Resources group on a Flux-only cluster, got %v", g.Kinds)
		}
	}
}

// TestNoFluxKindsOmitsTheFluxGroup is the other half: zero chrome on a
// cluster that doesn't run Flux.
func TestNoFluxKindsOmitsTheFluxGroup(t *testing.T) {
	t.Parallel()
	_, groups := BuildDiscoveredRegistry([]kube.DiscoveredKind{certificateDiscoveredKind()}, nil)
	for _, g := range groups {
		if g.ID == GroupFlux {
			t.Fatal("a non-Flux cluster must not get a Flux group")
		}
	}
}

// fluxObj builds an unstructured Flux object. conds are {type,status,message}
// triples flattened; suspend is applied only when true, matching the real
// shape where spec.suspend is absent rather than false.
func fluxObj(kind, name string, suspend bool, spec map[string]any, status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": kube.FluxGroupKustomize + "/v1",
		"kind":       kind,
		"metadata": map[string]any{
			"name": name, "namespace": "flux-system",
			"creationTimestamp": time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
	if spec == nil {
		spec = map[string]any{}
	}
	if suspend {
		spec["suspend"] = true
	}
	obj["spec"] = spec
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func cond(typ, status, message string) map[string]any {
	return map[string]any{
		"type": typ, "status": status, "message": message,
		"lastTransitionTime": time.Now().Add(-4 * time.Minute).UTC().Format(time.RFC3339),
	}
}

func conds(cs ...map[string]any) map[string]any {
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = c
	}
	return map[string]any{"conditions": out}
}

// TestProjectFluxResourceStatus is §30a's precedence table. Two rows here
// are what the generic conditionStatus read got wrong: a reconciling object
// (Ready=False + Reconciling=True) rendered a red ✕ as if it had failed,
// and a suspended object rendered a green ● off its frozen Ready condition.
func TestProjectFluxResourceStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		obj       *unstructured.Unstructured
		wantGlyph string
		wantClass StatusClass
		wantReady string
		wantSusp  bool
	}{
		{
			name:      "suspended outranks a stale Ready=True",
			obj:       fluxObj("Kustomization", "infra", true, nil, conds(cond("Ready", "True", "Applied revision: main@sha1:abc"))),
			wantGlyph: "‖", wantClass: StatusNeutral, wantReady: "suspended", wantSusp: true,
		},
		{
			name:      "stalled is terminal failure",
			obj:       fluxObj("Kustomization", "bad", false, nil, conds(cond("Ready", "False", "path not found"), cond("Stalled", "True", "path not found"))),
			wantGlyph: "✕", wantClass: StatusFail, wantReady: "False",
		},
		{
			name:      "reconciling with Ready=False is progress, not failure",
			obj:       fluxObj("Kustomization", "rolling", false, nil, conds(cond("Ready", "False", "building manifests"), cond("Reconciling", "True", "building manifests"))),
			wantGlyph: "◌", wantClass: StatusWarn, wantReady: "False",
		},
		{
			name:      "ready",
			obj:       fluxObj("Kustomization", "ok", false, nil, conds(cond("Ready", "True", "Applied revision: main@sha1:abc"))),
			wantGlyph: "●", wantClass: StatusOK, wantReady: "True",
		},
		{
			name:      "not ready with no Reconciling is failure",
			obj:       fluxObj("Kustomization", "broken", false, nil, conds(cond("Ready", "False", "health check failed after 2m0s"))),
			wantGlyph: "✕", wantClass: StatusFail, wantReady: "False",
		},
		{
			name:      "unknown Ready",
			obj:       fluxObj("Kustomization", "huh", false, nil, conds(cond("Ready", "Unknown", "reconciliation in progress"))),
			wantGlyph: "▲", wantClass: StatusWarn, wantReady: "Unknown",
		},
		{
			name:      "reconciling with Ready=Unknown is progress",
			obj:       fluxObj("Kustomization", "progressing", false, nil, conds(cond("Ready", "Unknown", "reconciliation in progress"), cond("Reconciling", "True", "reconciliation in progress"))),
			wantGlyph: "◌", wantClass: StatusWarn, wantReady: "Unknown",
		},
		{
			name:      "no conditions yet is pending, not neutral",
			obj:       fluxObj("Kustomization", "fresh", false, nil, nil),
			wantGlyph: "▲", wantClass: StatusWarn, wantReady: "–",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := projectFluxResource(tc.obj)
			if row.Glyph != tc.wantGlyph {
				t.Errorf("glyph = %q, want %q", row.Glyph, tc.wantGlyph)
			}
			if row.Status != tc.wantClass {
				t.Errorf("class = %v, want %v", row.Status, tc.wantClass)
			}
			if row.Cells[1] != tc.wantReady {
				t.Errorf("READY cell = %q, want %q", row.Cells[1], tc.wantReady)
			}
			if row.Suspended != tc.wantSusp {
				t.Errorf("Suspended = %v, want %v", row.Suspended, tc.wantSusp)
			}
		})
	}
}

// TestFluxSubLineCarriesTheConditionMessageVerbatim pins §30a's "the message
// text IS the diagnosis" rule: never paraphrased, never truncated here, and
// absent on a healthy row where it would only repeat the REVISION cell.
func TestFluxSubLineCarriesTheConditionMessageVerbatim(t *testing.T) {
	t.Parallel()
	msg := "health check failed after 2m0s: Deployment/nebula-stage/nebula-worker status: 'InProgress'"
	failing := projectFluxResource(fluxObj("Kustomization", "nebula-workers", false, nil, conds(cond("Ready", "False", msg))))
	if failing.SubLine != msg {
		t.Errorf("SubLine = %q, want the message verbatim %q", failing.SubLine, msg)
	}
	healthy := projectFluxResource(fluxObj("Kustomization", "ok", false, nil, conds(cond("Ready", "True", "Applied revision: main@sha1:abcdef1"))))
	if healthy.SubLine != "" {
		t.Errorf("a healthy row should carry no sub-line, got %q", healthy.SubLine)
	}
}

// TestFluxRevisionAndSource covers the three shapes Flux uses for "what is
// deployed" and "from where", measured against a real cluster.
func TestFluxRevisionAndSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		obj            *unstructured.Unstructured
		wantRev, wantS string
	}{
		{
			name: "kustomization: lastAppliedRevision + sourceRef",
			obj: fluxObj("Kustomization", "flux-system", false,
				map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}},
				map[string]any{"lastAppliedRevision": "master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef"}),
			wantRev: "master@efd398b", wantS: "git/flux-system",
		},
		{
			name: "gitrepository: artifact revision, and it IS the source",
			obj: fluxObj("GitRepository", "flux-system", false, nil,
				map[string]any{"artifact": map[string]any{"revision": "master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef"}}),
			wantRev: "master@efd398b", wantS: "–",
		},
		{
			name: "helmrelease: history head chartVersion, chart sourceRef",
			obj: fluxObj("HelmRelease", "cert-manager", false,
				map[string]any{"chart": map[string]any{"spec": map[string]any{
					"chart": "cert-manager", "version": "v1.21.0",
					"sourceRef": map[string]any{"kind": "HelmRepository", "name": "jetstack"}}}},
				map[string]any{"lastAttemptedRevision": "v1.21.0"}),
			wantRev: "v1.21.0", wantS: "helm/jetstack",
		},
		{
			name:    "nothing deployed yet",
			obj:     fluxObj("Kustomization", "fresh", false, nil, nil),
			wantRev: "–", wantS: "–",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := projectFluxResource(tc.obj)
			if row.Cells[2] != tc.wantRev {
				t.Errorf("REVISION = %q, want %q", row.Cells[2], tc.wantRev)
			}
			if row.Cells[3] != tc.wantS {
				t.Errorf("SOURCE = %q, want %q", row.Cells[3], tc.wantS)
			}
		})
	}
}

// TestShortRevisionCoversEveryShapeFluxUses. All three were observed on one
// real cluster; the bare-digest case is the one that matters most, since a
// HelmRepository's artifact revision is 71 characters left whole — wider
// than the entire table.
func TestShortRevisionCoversEveryShapeFluxUses(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"master@sha1:efd398bed98a38348c7702355ecd98fc11ac2bef", "master@efd398b"},
		{"sha256:d83a8a3354d98907a55beba524407a0b94d319623d0370a65fcd390e68db9852", "d83a8a3"},
		{"v1.21.0", "v1.21.0"},
		{"1.11.0", "1.11.0"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := shortRevision(tc.in); got != tc.want {
			t.Errorf("shortRevision(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := shortRevision(tc.in); len(got) > 24 {
			t.Errorf("shortRevision(%q) = %q, too wide for a column at %d chars", tc.in, got, len(got))
		}
	}
}

// TestSuspendedIsAmberWithoutLosingItsOwnCount pins the split between what a
// suspended row *is* and what colour it renders. The class stays Neutral so
// the strip keeps counting and naming suspended objects apart from
// reconciling ones; the tone is Warn so the row doesn't read as parked and
// benign, which is what Neutral's blue says everywhere else in the app
// (Completed pods, cordoned nodes). §31a's SUSPENDED card was already amber
// — this is what stops the two screens disagreeing about one object.
func TestSuspendedIsAmberWithoutLosingItsOwnCount(t *testing.T) {
	t.Parallel()
	reg := DefaultRegistry()
	reg.Register(fluxDescriptor(fluxKustomizationDiscoveredKind()))
	d, ok := reg.Descriptor(kube.ResourceKind("Kustomization"))
	if !ok {
		t.Fatal("no Kustomization descriptor")
	}
	if d.HealthTone == nil {
		t.Fatal("HealthTone was not defaulted by the registry")
	}
	if got := d.HealthTone(StatusNeutral); got != StatusWarn {
		t.Errorf("HealthTone(Neutral) = %v, want Warn — a suspended reconciler is a risk state", got)
	}
	// The count and the word it carries must survive the recolouring.
	if got := d.HealthLabel(StatusNeutral); got != "suspended" {
		t.Errorf("HealthLabel(Neutral) = %q, want %q", got, "suspended")
	}
	if got := d.HealthLabel(StatusWarn); got != "reconciling" {
		t.Errorf("HealthLabel(Warn) = %q, want %q — the two classes must stay distinct", got, "reconciling")
	}
	if got := d.HealthGlyph(StatusNeutral); got != "‖" {
		t.Errorf("HealthGlyph(Neutral) = %q, want %q", got, "‖")
	}
	// Every other class is its own colour, here as everywhere.
	for _, class := range []StatusClass{StatusOK, StatusWarn, StatusFail} {
		if got := d.HealthTone(class); got != class {
			t.Errorf("HealthTone(%v) = %v, want identity", class, got)
		}
	}
	// And no other kind is touched.
	pod, _ := reg.Descriptor(kube.KindPod)
	if got := pod.HealthTone(StatusNeutral); got != StatusNeutral {
		t.Errorf("Pods HealthTone(Neutral) = %v, want identity", got)
	}
}
