package objectdetail

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
)

// goldenCertSession mirrors testSession() (objectdetail_test.go) but with a
// third printer column (Issuer) so the golden fixture exercises §14a's own
// worked example column set (READY · SECRET · ISSUER), not just the
// two-column shape the unit tests need. Built via resources.CustomDescriptor
// directly rather than resources.BuildDiscoveredRegistry — see
// testSession's own doc comment for why, as of §35b.
func goldenCertSession() *tui.Session {
	dk := kube.DiscoveredKind{
		Kind: "Certificate", Plural: "certificates", Group: "cert-manager.io",
		Versions:      []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		ClusterScoped: false,
		PrinterColumns: []kube.PrinterColumn{
			{Name: "Ready", Type: "string", JSONPath: `.status.conditions[?(@.type=="Ready")].status`},
			{Name: "Secret", Type: "string", JSONPath: ".spec.secretName"},
			{Name: "Issuer", Type: "string", JSONPath: ".spec.issuerRef.name"},
		},
		Established: true,
	}
	reg := resources.DefaultRegistry()
	reg.Register(resources.CustomDescriptor(dk))
	return &tui.Session{Theme: tui.Dark(), Registry: reg, Location: tui.Location{Context: "nva-staging", Namespace: "nva-stage"}}
}

// goldenCertObj mirrors §14a/§14d's own worked example: a cert-manager
// Certificate mid-issuance — Ready=False/DoesNotExist (the exact diagnosis
// message the design doc quotes verbatim: "Issuing certificate as Secret
// does not exist") alongside Issuing=True (green), an issuerRef surfaced
// as an ownerReference so the meta grid's purple "↗" OWNER link renders,
// and lastTransitionTime set relative to now so AGE stays deterministic.
func goldenCertObj() *unstructured.Unstructured {
	transition := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	obj := map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      "api-tls",
			"namespace": "nva-stage",
			"ownerReferences": []any{
				map[string]any{"kind": "ClusterIssuer", "name": "letsencrypt-prod"},
			},
		},
		"spec": map[string]any{
			"secretName": "api-tls-secret",
			"issuerRef":  map[string]any{"kind": "ClusterIssuer", "name": "letsencrypt-prod"},
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type": "Ready", "status": "False", "reason": "DoesNotExist",
					"message":            "Issuing certificate as Secret does not exist",
					"lastTransitionTime": transition,
				},
				map[string]any{
					"type": "Issuing", "status": "True", "reason": "DoesNotExist",
					"message":            "Issuing certificate as Secret does not exist",
					"lastTransitionTime": transition,
				},
			},
		},
	}
	return &unstructured.Unstructured{Object: obj}
}

// goldenCertEvents mirrors realistic cert-manager controller events for a
// Certificate mid-issuance — relative LastSeen timestamps keep AGE stable
// day to day, same reasoning as poddetail's own goldenEvents.
func goldenCertEvents() []kube.Event {
	return []kube.Event{
		{Type: "Normal", Reason: "Issuing", Message: "Issuing certificate as Secret does not exist", LastSeen: time.Now().Add(-2 * time.Minute)},
		{Type: "Normal", Reason: "Generated", Message: `Stored new private key in temporary Secret resource "api-tls-xk9j2"`, LastSeen: time.Now().Add(-2 * time.Minute)},
		{Type: "Normal", Reason: "Requested", Message: `Created new CertificateRequest resource "api-tls-1"`, LastSeen: time.Now().Add(-3 * time.Minute)},
	}
}

// goldenObjectDetailModel wires 14d's generic detail screen against the
// golden Certificate above — the open seams + mutator + siblings are wired
// (no-op stubs) so the keybar renders the full hint set, same reasoning as
// poddetail's own goldenPodDetailModel.
func goldenObjectDetailModel(t *testing.T, width, height int) *Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		certificateKind(): {goldenCertObj()},
	}}
	openObjStub := func(kube.ResourceKind, string, string, int, int) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
	m := New(Config{
		Session: goldenCertSession(), Lister: lister,
		Events:     fakeEvents{rows: goldenCertEvents()},
		Mutator:    &fakeMutator{},
		OpenYAML:   openObjStub,
		OpenEvents: openObjStub,
		Kind:       certificateKind(), Namespace: "nva-stage", Name: "api-tls",
		Siblings: []SiblingRef{{Namespace: "default", Name: "api-tls"}, {Namespace: "default", Name: "web-tls"}}, SiblingIndex: 0,
	})
	m.SetSize(width, height)
	updated, _ := step(t, &m, m.Init()())
	got := updated.(*Model)
	// The header badge's "· 12ms" comes from the conn-state ping loop — a
	// fixed latency keeps the golden deterministic (mirrors poddetail's own
	// goldenPodDetailModel).
	updated2, _ := step(t, got, kube.ConnStateMsg{Phase: kube.ConnConnected, Latency: 12 * time.Millisecond})
	return updated2.(*Model)
}

func goldenObjectDetailFixtures(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"120x36.golden": goldentest.Plain(goldenObjectDetailModel(t, 120, 36).Render()),
		"80x24.golden":  goldentest.Plain(goldenObjectDetailModel(t, 80, 24).Render()),
	}
}

func goldenDir() string {
	return filepath.Join("..", "..", "..", "..", "test", "golden", "objectdetail")
}

func TestGenerateGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate objectdetail golden fixtures")
	}
	if err := os.MkdirAll(goldenDir(), 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	for name, got := range goldenObjectDetailFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestGoldenFixtures(t *testing.T) {
	for name, got := range goldenObjectDetailFixtures(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(goldenDir(), name)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", name, string(want), got)
			}
		})
	}
}

// truecolorGoldenFixtures renders 14d with a forced truecolor profile in
// both themes, pinning the per-cell color mapping (condition glyphs, OWNER
// purple link, conn badge) that the profile-less goldens above can't see.
// The profile swap is global, so these tests must not run parallel with
// other renders in this package (none of them do).
func truecolorGoldenFixtures(t *testing.T) map[string]string {
	t.Helper()
	dark := goldenObjectDetailModel(t, 120, 36)
	light := goldenObjectDetailModel(t, 120, 36)
	light.session.Theme = tui.Light()
	return map[string]string{
		"120x36-dark.golden":  goldentest.Truecolor(dark.Render()),
		"120x36-light.golden": goldentest.Truecolor(light.Render()),
	}
}

func TestGenerateTruecolorGoldenFixtures(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to regenerate objectdetail golden fixtures")
	}
	if err := os.MkdirAll(goldenDir(), 0o755); err != nil {
		t.Fatalf("mkdir golden dir: %v", err)
	}
	for name, got := range truecolorGoldenFixtures(t) {
		if err := os.WriteFile(filepath.Join(goldenDir(), name), []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestTruecolorGoldenFixtures(t *testing.T) {
	for name, got := range truecolorGoldenFixtures(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(goldenDir(), name)
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v", path, err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", name, string(want), got)
			}
		})
	}
}
