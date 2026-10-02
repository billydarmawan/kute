package resources

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kute-dev/kute/internal/kube"
)

// certObj builds a minimal cert-manager.io/v1 Certificate for
// projectCertificate's table tests. readyStatus == "" omits the Ready
// condition entirely (the "no condition at all" branch); every time.Time
// arg being zero omits the corresponding status field, matching
// demoCertificate's own "not set yet" convention.
func certObj(name string, readyStatus string, notAfter, renewalTime, lastFailure time.Time) *unstructured.Unstructured {
	status := map[string]any{}
	if readyStatus != "" {
		status["conditions"] = []any{map[string]any{"type": "Ready", "status": readyStatus}}
	}
	if !notAfter.IsZero() {
		status["notAfter"] = notAfter.UTC().Format(time.RFC3339)
	}
	if !renewalTime.IsZero() {
		status["renewalTime"] = renewalTime.UTC().Format(time.RFC3339)
	}
	if !lastFailure.IsZero() {
		status["lastFailureTime"] = lastFailure.UTC().Format(time.RFC3339)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec":       map[string]any{"issuerRef": map[string]any{"name": "letsencrypt-prod"}},
		"status":     status,
	}}
}

// TestProjectCertificateReadyPrecedence pins §35b's status precedence: a
// repeated failure (lastFailureTime set) outranks a first-attempt issuance,
// Unknown/absent Ready both read as issuing rather than a fake neutral.
func TestProjectCertificateReadyPrecedence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		readyStatus string
		lastFailure time.Time
		wantStatus  StatusClass
		wantGlyph   string
		wantReady   string
	}{
		{"ready", "True", time.Time{}, StatusOK, "●", "True"},
		{"repeated failure", "False", time.Now().Add(-time.Minute), StatusFail, "✕", "False"},
		{"first attempt", "False", time.Time{}, StatusWarn, "▲", "False"},
		{"unknown", "Unknown", time.Time{}, StatusWarn, "▲", "Unknown"},
		{"no condition", "", time.Time{}, StatusWarn, "▲", "–"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := projectCertificate(certObj(c.name, c.readyStatus, time.Time{}, time.Time{}, c.lastFailure))
			if row.Status != c.wantStatus || row.Glyph != c.wantGlyph {
				t.Fatalf("got Status=%s Glyph=%s, want Status=%s Glyph=%s", row.Status, row.Glyph, c.wantStatus, c.wantGlyph)
			}
			if row.Cells[1] != c.wantReady {
				t.Fatalf("READY cell = %q, want %q", row.Cells[1], c.wantReady)
			}
		})
	}
}

// TestProjectCertificateExpiryBuckets pins §35b's EXPIRES thresholds — the
// same day-bucketing routetable's resolveCertExpiry uses for Ingress TLS
// (docs/design README.md §23a): expired, <30d warn, <365d ok, else years.
func TestProjectCertificateExpiryBuckets(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name       string
		notAfter   time.Time
		wantText   string
		wantClass  StatusClass
		wantStatus StatusClass // the row's overall Status, to confirm the ready-but-expiring override
		wantGlyph  string
	}{
		{"unknown", time.Time{}, "–", StatusNeutral, StatusOK, "●"},
		{"expired", now.Add(-24 * time.Hour), "expired", StatusFail, StatusFail, "✕"},
		{"expired hours ago", now.Add(-12 * time.Hour), "expired", StatusFail, StatusFail, "✕"},
		{"expiring soon", now.Add(22*24*time.Hour + time.Hour), "22d", StatusWarn, StatusOK, "◷"},
		{"comfortable", now.Add(61*24*time.Hour + time.Hour), "61d", StatusOK, StatusOK, "●"},
		{"long-dated", now.Add(8*365*24*time.Hour + time.Hour), "8y", StatusOK, StatusOK, "●"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := projectCertificate(certObj(c.name, "True", c.notAfter, time.Time{}, time.Time{}))
			if row.Cells[2] != c.wantText {
				t.Fatalf("EXPIRES cell = %q, want %q", row.Cells[2], c.wantText)
			}
			if row.ExpiresClass != c.wantClass {
				t.Fatalf("ExpiresClass = %s, want %s", row.ExpiresClass, c.wantClass)
			}
			// A Ready=True Certificate that is merely expiring stays OK —
			// §35b: "among ready certs, soonest expiry floats up" means the
			// expiring one still counts and sorts as ready. Only the glyph
			// flags the <30d case. One already past notAfter is failing,
			// whatever its stale Ready condition says.
			if row.Status != c.wantStatus {
				t.Fatalf("Status = %s, want %s", row.Status, c.wantStatus)
			}
			if row.Glyph != c.wantGlyph {
				t.Fatalf("Glyph = %s, want %s", row.Glyph, c.wantGlyph)
			}
		})
	}
}

// TestCertExpiryBoundaries pins the bucketing both the EXPIRES cell and
// routetable's TLS cell share: a cert is expired from the instant notAfter
// passes, not 24h later, and day counts floor in both directions.
func TestCertExpiryBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		notAfter    time.Time
		wantDays    int
		wantExpired bool
		wantClass   StatusClass
	}{
		{"an hour left", now.Add(time.Hour), 0, false, StatusWarn},
		{"exactly now", now, 0, true, StatusFail},
		{"an hour ago", now.Add(-time.Hour), 0, true, StatusFail},
		{"25h ago", now.Add(-25 * time.Hour), 1, true, StatusFail},
		{"30d left", now.Add(30*24*time.Hour + time.Hour), 30, false, StatusOK},
	}
	for _, c := range cases {
		days, expired, class := CertExpiry(c.notAfter, now)
		if days != c.wantDays || expired != c.wantExpired || class != c.wantClass {
			t.Errorf("%s: CertExpiry = (%d, %v, %s), want (%d, %v, %s)", c.name, days, expired, class, c.wantDays, c.wantExpired, c.wantClass)
		}
	}
}

// TestProjectCertificateExpiresAtDrivesTiebreak confirms ExpiresAt is
// carried through for browse/sort.go's certExpiryTiebreak — zero when
// unknown, the parsed notAfter otherwise.
func TestProjectCertificateExpiresAtDrivesTiebreak(t *testing.T) {
	t.Parallel()
	notAfter := time.Now().Add(61 * 24 * time.Hour)
	row := projectCertificate(certObj("api-tls", "True", notAfter, time.Time{}, time.Time{}))
	if row.ExpiresAt.IsZero() || row.ExpiresAt.Unix() != notAfter.Unix() {
		t.Fatalf("ExpiresAt = %v, want %v", row.ExpiresAt, notAfter)
	}
	unset := projectCertificate(certObj("staging-tls", "True", time.Time{}, time.Time{}, time.Time{}))
	if !unset.ExpiresAt.IsZero() {
		t.Fatalf("expected zero ExpiresAt when notAfter is unset, got %v", unset.ExpiresAt)
	}
}

// TestProjectCertificateRenewalCell pins §35b's RENEWAL cell: a not-ready
// certificate names its state rather than fabricating a schedule; a ready
// one with a known renewalTime gets shortEta's forward-looking duration
// (which reads "due" rather than a negative day count once renewalTime has
// already passed) plus " · auto", colored the same class EXPIRES computed.
func TestProjectCertificateRenewalCell(t *testing.T) {
	t.Parallel()
	now := time.Now()

	failing := projectCertificate(certObj("web-tls", "False", time.Time{}, time.Time{}, now.Add(-time.Minute)))
	if failing.Cells[3] != "retrying" || failing.RenewalClass != StatusFail {
		t.Fatalf("got Renewal=%q/%s, want retrying/fail", failing.Cells[3], failing.RenewalClass)
	}

	issuing := projectCertificate(certObj("new-svc-tls", "False", time.Time{}, time.Time{}, time.Time{}))
	if issuing.Cells[3] != "issuing" || issuing.RenewalClass != StatusWarn {
		t.Fatalf("got Renewal=%q/%s, want issuing/warn", issuing.Cells[3], issuing.RenewalClass)
	}

	noSchedule := projectCertificate(certObj("api-tls", "True", now.Add(61*24*time.Hour), time.Time{}, time.Time{}))
	if noSchedule.Cells[3] != "–" || noSchedule.RenewalClass != StatusNeutral {
		t.Fatalf("got Renewal=%q/%s, want –/neutral", noSchedule.Cells[3], noSchedule.RenewalClass)
	}

	scheduled := projectCertificate(certObj("api-tls", "True", now.Add(61*24*time.Hour+time.Hour), now.Add(31*24*time.Hour+time.Hour), time.Time{}))
	if scheduled.Cells[3] != "in 31d · auto" || scheduled.RenewalClass != StatusOK {
		t.Fatalf("got Renewal=%q/%s, want %q/ok", scheduled.Cells[3], scheduled.RenewalClass, "in 31d · auto")
	}

	overdue := projectCertificate(certObj("admin-tls", "True", now.Add(22*24*time.Hour), now.Add(-8*24*time.Hour), time.Time{}))
	if overdue.Cells[3] != "due · auto" || overdue.RenewalClass != StatusWarn {
		t.Fatalf("got Renewal=%q/%s, want due · auto/warn", overdue.Cells[3], overdue.RenewalClass)
	}
}

// TestProjectCertificateIssuerCell pins the ISSUER cell: spec.issuerRef.name
// verbatim, "–" when unset.
func TestProjectCertificateIssuerCell(t *testing.T) {
	t.Parallel()
	row := projectCertificate(certObj("api-tls", "True", time.Time{}, time.Time{}, time.Time{}))
	if row.Cells[4] != "letsencrypt-prod" {
		t.Fatalf("ISSUER cell = %q, want letsencrypt-prod", row.Cells[4])
	}

	noIssuer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": "bare", "namespace": "default"},
		"status":     map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}}
	if got := projectCertificate(noIssuer).Cells[4]; got != "–" {
		t.Fatalf("ISSUER cell = %q, want – for an unset issuerRef", got)
	}
}

// TestCertificateHealthTalliesExpiringSoon confirms the cross-cutting <30d
// segment counts a ready-but-expiring row on top of (not instead of) its
// StatusHealth "ready" tally — the same shape helmReleaseHealth's Outdated
// already uses.
func TestCertificateHealthTalliesExpiringSoon(t *testing.T) {
	t.Parallel()
	now := time.Now()
	rows := []Row{
		projectCertificate(certObj("api-tls", "True", now.Add(61*24*time.Hour), time.Time{}, time.Time{})),
		projectCertificate(certObj("admin-tls", "True", now.Add(22*24*time.Hour), time.Time{}, time.Time{})),
		projectCertificate(certObj("web-tls", "False", time.Time{}, time.Time{}, now.Add(-time.Minute))),
	}
	counts := certificateHealth(rows)
	if counts.OK != 2 {
		t.Fatalf("OK = %d, want 2 (both Ready=True rows still count as ready)", counts.OK)
	}
	if counts.Fail != 1 {
		t.Fatalf("Fail = %d, want 1", counts.Fail)
	}
	if counts.ExpiringSoon != 1 {
		t.Fatalf("ExpiringSoon = %d, want 1 (admin-tls only)", counts.ExpiringSoon)
	}
}

// TestCertificateHealthGlyphAndLabel pins §35b's own departures from the
// generic set: issuing (Warn) renders ▲, not the generic ▲.
func TestCertificateHealthGlyphAndLabel(t *testing.T) {
	t.Parallel()
	if g := certificateHealthGlyph(StatusWarn); g != "▲" {
		t.Fatalf("Warn glyph = %q, want ▲", g)
	}
	if g := certificateHealthGlyph(StatusOK); g != DefaultHealthGlyph(StatusOK) {
		t.Fatalf("OK glyph = %q, want the generic default", g)
	}
	cases := map[StatusClass]string{
		StatusOK: "ready", StatusWarn: "issuing", StatusFail: "not ready", StatusNeutral: "unknown",
	}
	for class, want := range cases {
		if got := certificateHealthLabel(class); got != want {
			t.Fatalf("label(%s) = %q, want %q", class, got, want)
		}
	}
}

// TestCertificateDescriptorViaRegistry pins the registration path (crd.go's
// BuildDiscoveredRegistry dispatch): Certificate gets §35b's curated
// columns and status semantics, not the generic 14a set.
func TestCertificateDescriptorViaRegistry(t *testing.T) {
	t.Parallel()
	reg, _ := BuildDiscoveredRegistry([]kube.DiscoveredKind{certificateDiscoveredKind()}, nil)
	d, ok := reg.Descriptor(kube.ResourceKind("Certificate"))
	if !ok {
		t.Fatal("expected a Certificate descriptor")
	}
	want := []string{"Name", "Ready", "Expires", "Renewal", "Issuer", "Age"}
	if len(d.Columns) != len(want) {
		t.Fatalf("Columns = %v, want %v", d.Columns, want)
	}
	for i, w := range want {
		if d.Columns[i] != w {
			t.Fatalf("Columns[%d] = %q, want %q", i, d.Columns[i], w)
		}
	}
	if !d.Custom || !d.StatusSemantics {
		t.Fatalf("expected Custom=true StatusSemantics=true, got %+v", d)
	}
}
