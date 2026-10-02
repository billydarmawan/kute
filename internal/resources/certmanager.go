// cert-manager's Certificate kind (docs/design README.md §35b) is a curated
// discovered kind — the same narrow exception §30a (Flux) and §23b
// (HTTPRoute) already make: EXPIRES/RENEWAL only mean anything read off
// Certificate's own status.notAfter/status.renewalTime, which no generic
// CRD-declared printer column can express, and "is it Ready" alone can't
// tell an operator what the whole screen is for — how long until this cert
// needs attention.
//
// Recognition is by bare Kind name, the same shape §23b's httpRouteDescriptor
// already takes: unlike Flux's HelmRelease, nothing else discovered on a
// real cluster is plausibly also named "Certificate".
//
// §35a (the Certificate → CertificateRequest → Order → Challenge chain,
// tasks/certchain) and its own ↵ routing in browse/certchain.go are
// unaffected by any of this — that gate fires on the bare kind name
// regardless of what Descriptor the kind carries. §35c's force-renew verb
// and Issuer/ClusterIssuer's own reverse-ref CERTS column stay out of scope
// here (see docs/design/v.0.7.0.dc.html's own #35b notes).
package resources

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// certificateColumns is §35b's curated column set: NAME leads and AGE
// trails like every kind, READY/EXPIRES/RENEWAL/ISSUER fill the middle in
// the mockup's own order.
var certificateColumns = []string{"Name", "Ready", "Expires", "Renewal", "Issuer", "Age"}

// certificateDescriptor builds the §35b list Descriptor for the discovered
// Certificate kind. Custom stays true — 14d detail routing (moot here,
// since browse/certchain.go's ↵ gate fires first), 14c's API-group type
// label and 14a's breadcrumb version tag all key off it and all remain
// correct. StatusSemantics is true: unlike the long tail of CRDs 14a's
// "never fake health" fallback exists for, Certificate always carries real
// Ready-condition status.
func certificateDescriptor(dk kube.DiscoveredKind) Descriptor {
	return Descriptor{
		Kind:            dk.RegistryKind(),
		Group:           GroupCustomResources,
		Display:         capitalizePlural(dk.Plural),
		Icon:            "◆",
		Columns:         certificateColumns,
		Describe:        "custom resource · " + dk.Group,
		ClusterScoped:   dk.ClusterScoped,
		Custom:          true,
		StatusSemantics: true,
		APIGroup:        dk.Group,
		APIVersion:      dk.GVR.Version,
		Project:         projectCertificate,
		Health:          certificateHealth,
		HealthLabel:     certificateHealthLabel,
		HealthGlyph:     certificateHealthGlyph,
	}
}

// certificateHealthLabel is §35b's health-strip wording: "4 ready · 1
// issuing · 1 not ready".
func certificateHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "ready"
	case StatusWarn:
		return "issuing"
	case StatusFail:
		return "not ready"
	default:
		return "unknown"
	}
}

// certificateHealthGlyph renders §35b's one departure from the generic set:
// an issuing (Warn) certificate's ▲, matching §35a's own chain-row glyph
// for "Ready=False · Issuing" rather than the generic ▲.
func certificateHealthGlyph(class StatusClass) string {
	if class == StatusWarn {
		return "▲" // ▲
	}
	return DefaultHealthGlyph(class)
}

// certificateHealth is §35b's own tally: the usual per-status counts, plus
// ExpiringSoon counted *across* them — a ready-but-expiring cert is still
// tallied "ready" above, exactly the cross-cutting shape helmReleaseHealth's
// Outdated already uses.
func certificateHealth(rows []Row) HealthCounts {
	h := StatusHealth(rows)
	for _, r := range rows {
		if r.ExpiresClass == StatusWarn {
			h.ExpiringSoon++
		}
	}
	return h
}

// projectCertificate is §35b's status derivation, in strict precedence.
//
// A repeated failure (status.lastFailureTime set — a real cert-manager v1
// Certificate.Status field, not a fabricated attempt counter) outranks a
// first-attempt issuance: cert-manager writes lastFailureTime only once
// issuance has actually failed, so its presence is the honest signal that
// distinguishes "still working on it" from "stuck". Ready=Unknown or no
// Ready condition at all reads as issuing too — Certificate always has real
// status semantics, so unlike 14a's generic fallback this is progress, not
// a fake neutral (the same reasoning §30a's fluxStatus applies to an absent
// condition).
func projectCertificate(obj runtime.Object) Row {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return metaRow(obj)
	}
	ns, name, age := metaOf(obj)
	now := time.Now()

	readyCond, hasReady := certCondition(u, "Ready")
	notAfter := certTimeField(u, "notAfter")
	renewalTime := certTimeField(u, "renewalTime")
	lastFailure := certTimeField(u, "lastFailureTime")
	issuerName, _, _ := unstructured.NestedString(u.Object, "spec", "issuerRef", "name")

	var class StatusClass
	var glyph, readyText string
	switch {
	case hasReady && readyCond.status == "True":
		class, glyph, readyText = StatusOK, "●", "True" // ●
	case hasReady && readyCond.status == "False" && !lastFailure.IsZero():
		class, glyph, readyText = StatusFail, "✕", "False" // ✕
	case hasReady && readyCond.status == "False":
		class, glyph, readyText = StatusWarn, "▲", "False" // ▲
	case hasReady:
		class, glyph, readyText = StatusWarn, "▲", readyCond.status // ▲ (Unknown)
	default:
		class, glyph, readyText = StatusWarn, "▲", "–" // ▲, "–"
	}

	expiresText, expiresClass := certExpiryCell(notAfter, now)
	renewalText, renewalClass := certRenewalCell(class, notAfter, renewalTime, now)

	glyphClass := class
	if class == StatusOK && expiresClass == StatusFail {
		// Ready=True past notAfter is what a cert looks like once cert-
		// manager stops reconciling it: the condition is stale, the cert is
		// not. It sorts and counts as failing, not ready.
		class, glyph, glyphClass = StatusFail, "✕", StatusFail // ✕
	}
	if class == StatusOK && expiresClass == StatusWarn {
		// §35b: "among ready certs, soonest expiry floats up" — the glyph
		// flags it without demoting Status, so it still counts and sorts as
		// ready (Row.GlyphClass's own doc comment names this as its second
		// case, alongside 18a's outdated release).
		glyph, glyphClass = "◷", StatusWarn // ◷
	}

	return Row{
		Namespace: ns, Name: name,
		Cells: []string{
			name, readyText, expiresText, renewalText,
			dashIfEmpty(issuerName), shortAge(age),
		},
		Status:       class,
		Glyph:        glyph,
		GlyphClass:   glyphClass,
		ExpiresAt:    notAfter,
		ExpiresClass: expiresClass,
		RenewalClass: renewalClass,
	}
}

// CertExpiry buckets a certificate's notAfter against now — the one copy of
// the day thresholds both this list's EXPIRES cell and routetable's Ingress/
// Gateway TLS cell (docs/design README.md §23a: "yellow <30d, red expired")
// use. expired is true from the instant notAfter passes, and days is the
// floored whole days remaining (or, once expired, elapsed): truncating a
// negative duration toward zero used to read a cert that expired hours ago
// as "0d" left, in yellow.
func CertExpiry(notAfter, now time.Time) (days int, expired bool, class StatusClass) {
	if !now.Before(notAfter) {
		return int(now.Sub(notAfter).Hours() / 24), true, StatusFail
	}
	days = int(notAfter.Sub(now).Hours() / 24)
	if days < 30 {
		return days, false, StatusWarn
	}
	return days, false, StatusOK
}

// certExpiryCell renders the EXPIRES cell: "–"/Neutral when notAfter is
// unknown (not yet issued), "expired"/Fail once past, "Nd"/Warn inside 30
// days, "Nd"/OK inside a year, "Ny"/OK beyond, bucketed by CertExpiry.
func certExpiryCell(notAfter, now time.Time) (text string, class StatusClass) {
	if notAfter.IsZero() {
		return "–", StatusNeutral // "–"
	}
	days, expired, class := CertExpiry(notAfter, now)
	switch {
	case expired:
		return "expired", class
	case days < 365:
		return fmt.Sprintf("%dd", days), class
	default:
		return fmt.Sprintf("%dy", days/365), class
	}
}

// certRenewalCell renders the RENEWAL cell. A not-Ready certificate has no
// renewal schedule to show yet — "retrying"/"issuing" name the state
// instead of fabricating one, honoring the same "never a fabricated number"
// rule the fixtures for §35a's own web-tls attempts already document. A
// Ready certificate with a known renewalTime gets certEta's forward-looking
// duration (which renders "due" rather than a negative day count for a
// renewalTime already past, same simplification CronJob's own NEXT RUN
// column makes with shortEta) plus " · auto", colored the same class the
// EXPIRES cell computed — the same signal, one cell over.
func certRenewalCell(certClass StatusClass, notAfter, renewalTime, now time.Time) (text string, class StatusClass) {
	if certClass == StatusFail {
		return "retrying", StatusFail
	}
	if certClass != StatusOK {
		return "issuing", StatusWarn
	}
	if renewalTime.IsZero() {
		return "–", StatusNeutral // "–"
	}
	_, expClass := certExpiryCell(notAfter, now)
	return certEta(renewalTime.Sub(now)) + " · auto", expClass
}

// certEta is projections.go's shortEta with certExpiryCell's own year
// bucket added: shortEta alone renders a long-lived CA's multi-year
// renewalTime as an ugly "in 2555d" (fine for CronJob's own NEXT RUN, whose
// durations never run past a schedule's own period) — kept local rather
// than changing shortEta itself, which would also reshape CronJob's column.
func certEta(d time.Duration) string {
	if d <= 0 {
		return "due"
	}
	days := int(d.Hours() / 24)
	if days < 365 {
		return fmt.Sprintf("in %dd", days)
	}
	return fmt.Sprintf("in %dy", days/365)
}

type certCond struct{ status string }

// certCondition finds one status.conditions entry by type — Certificate's
// own local twin of flux.go's fluxCondition (kept separate rather than
// shared: cert-manager and Flux read different condition sets and nothing
// forces the shapes to stay identical). Only status is read: §35b has
// nothing to say with a condition's reason/message the way §30a's SubLine
// does.
func certCondition(u *unstructured.Unstructured, typ string) (certCond, bool) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cm, "type"); t != typ {
			continue
		}
		status, _, _ := unstructured.NestedString(cm, "status")
		return certCond{status: status}, true
	}
	return certCond{}, false
}

// certTimeField reads an RFC3339 status field, returning the zero Time on
// anything that isn't a valid parse — missing (not yet issued) and
// malformed are the same "unknown" to every caller here.
func certTimeField(u *unstructured.Unstructured, field string) time.Time {
	s, _, _ := unstructured.NestedString(u.Object, "status", field)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// dashIfEmpty renders s, or "–" when it's empty — Certificate's ISSUER cell
// when spec.issuerRef.name is somehow unset.
func dashIfEmpty(s string) string {
	if s == "" {
		return "–"
	}
	return s
}
