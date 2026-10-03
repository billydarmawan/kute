package certchain

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// load reads the Certificate, walks its ownerRef chain one hop at a time,
// and resolves the refs strip — one pass, narrow reads only. Every ListRaw
// call names a kind the chain actually needs; nothing here walks the
// registry (CLAUDE.md's lazy-informer rule).
func (m Model) load() tea.Cmd {
	lister := m.lister
	namespace, name := m.namespace, m.name
	timeout := m.timeout

	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		certs, err := lister.ListRaw(ctx, kube.KindCertificate, namespace)
		if err != nil {
			return loadedMsg{err: err}
		}
		cert, ok := findByName(certs, name)
		if !ok {
			pending, kerr := cacheState(lister, namespace, kube.KindCertificate)
			if kerr != nil {
				return loadedMsg{err: kerr}
			}
			return loadedMsg{gone: true, pending: pending}
		}

		chain := []chainNode{certNode(cert)}
		attempts := 0
		pending := false
		var chainErr error
		// readChain lists one chain kind and folds its cache state in: an
		// unfilled or stopped cache truncates the chain silently otherwise.
		readChain := func(kind kube.ResourceKind) []runtime.Object {
			objs, err := lister.ListRaw(ctx, kind, namespace)
			p, kerr := cacheState(lister, namespace, kind)
			pending = pending || p
			if err == nil {
				err = kerr
			}
			if chainErr == nil && err != nil {
				chainErr = err
			}
			return objs
		}

		crs := readChain(kube.KindCertificateRequest)
		owned := ownedBy(crs, kube.KindCertificate.APIKind(), name)
		attempts = len(owned)

		if currentCR, ok := newest(owned); ok {
			chain = append(chain, certRequestNode(currentCR, 1))

			orders := readChain(kube.KindOrder)
			ownedOrders := ownedBy(orders, kube.KindCertificateRequest.APIKind(), currentCR.GetName())
			if currentOrder, ok := newest(ownedOrders); ok {
				chain = append(chain, orderNode(currentOrder, 2))

				challenges := readChain(kube.KindChallenge)
				ownedChallenges := ownedBy(challenges, kube.KindOrder.APIKind(), currentOrder.GetName())
				sortByCreation(ownedChallenges)
				for _, ch := range ownedChallenges {
					chain = append(chain, challengeNode(ch, 3))
				}
			}
		}
		chainNote := ""
		switch {
		case chainErr != nil && kube.IsPermissionError(chainErr):
			chainNote = "chain may be incomplete — permission denied: " + chainErr.Error()
		case chainErr != nil:
			chainNote = "chain may be incomplete — " + chainErr.Error()
		case pending:
			chainNote = "chain may be incomplete — still loading"
		}

		fail := buildFailure(chain)
		secretRef, haveSecret := resolveSecretRef(ctx, lister, namespace, cert)
		issuerRef, haveIssuer := resolveIssuerRef(ctx, lister, namespace, cert)
		pending = pending || secretRef.pending || issuerRef.pending

		return loadedMsg{
			pending:    pending,
			chainNote:  chainNote,
			fail:       fail,
			chain:      chain,
			secretRef:  secretRef,
			issuerRef:  issuerRef,
			haveSecret: haveSecret,
			haveIssuer: haveIssuer,
			attempts:   attempts,
		}
	}
}

func findByName(objs []runtime.Object, name string) (*unstructured.Unstructured, bool) {
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetName() == name {
			return u, true
		}
	}
	return nil, false
}

// namer is satisfied by every Kubernetes API object, typed or unstructured.
type namer interface{ GetName() string }

// existsByName reports whether name is present among objs — used for the
// target Secret, which comes back *typed* (Secret has a real informer, only
// discovered CRD kinds are unstructured), unlike findByName's Certificate/
// Issuer/ClusterIssuer callers, which need the object itself, not just its
// presence.
func existsByName(objs []runtime.Object, name string) bool {
	return slices.ContainsFunc(objs, func(o runtime.Object) bool {
		n, ok := o.(namer)
		return ok && n.GetName() == name
	})
}

// ownedBy filters objs to those whose OwnerReferences name (kind, name) —
// matched on Kind+Name, never UID: the same match style
// internal/kube/pods.go's ownerRef and poddetail's Pod→ReplicaSet→Deployment
// walk already use, and what every demo fixture's OwnerReferences set.
func ownedBy(objs []runtime.Object, kind, name string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, o := range objs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		for _, ref := range u.GetOwnerReferences() {
			if ref.Kind == kind && ref.Name == name {
				out = append(out, u)
				break
			}
		}
	}
	return out
}

// newest picks the most recently created object — cert-manager creates one
// CertificateRequest/Order per issuance attempt and this screen only shows
// the current one, the same "one active chain, not every historical
// attempt" scoping fluxdetail's own inventory takes.
func newest(objs []*unstructured.Unstructured) (*unstructured.Unstructured, bool) {
	if len(objs) == 0 {
		return nil, false
	}
	sortByCreation(objs)
	return objs[0], true
}

func sortByCreation(objs []*unstructured.Unstructured) {
	slices.SortFunc(objs, func(a, b *unstructured.Unstructured) int {
		return b.GetCreationTimestamp().Compare(a.GetCreationTimestamp().Time)
	})
}

type condition struct{ status, reason, message, lastTransition string }

func readCondition(u *unstructured.Unstructured, typ string) (condition, bool) {
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
		reason, _, _ := unstructured.NestedString(cm, "reason")
		message, _, _ := unstructured.NestedString(cm, "message")
		lt, _, _ := unstructured.NestedString(cm, "lastTransitionTime")
		return condition{status, reason, message, lt}, true
	}
	return condition{}, false
}

func conditionTime(c condition, fallback metav1.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, c.lastTransition); err == nil {
		return t
	}
	return fallback.Time
}

// certNode reads Certificate's own Ready condition. False by itself is
// in-flight progress, not failure — cert-manager reports the same
// Ready=False while mid-issuance as it does while genuinely stuck, and only
// a deeper Order/Challenge's own terminal state distinguishes the two (see
// acmeStateClass).
func certNode(u *unstructured.Unstructured) chainNode {
	cond, _ := readCondition(u, "Ready")
	state, class, glyph := readyState(cond)
	return chainNode{
		Kind: kube.KindCertificate, Name: u.GetName(), Depth: 0,
		StateText: state, Class: class, Glyph: glyph,
		Message: cond.message,
		Created: conditionTime(cond, u.GetCreationTimestamp()),
	}
}

// certRequestNode reads Approved/Denied alongside Ready — a request can be
// Denied (terminal failure), Approved-but-not-yet-Ready (normal progress),
// or Ready (done).
func certRequestNode(u *unstructured.Unstructured, depth int) chainNode {
	ready, _ := readCondition(u, "Ready")
	approved, hasApproved := readCondition(u, "Approved")
	denied, hasDenied := readCondition(u, "Denied")

	var state, message string
	class := resources.StatusWarn
	glyph := "▲"
	switch {
	case hasDenied && denied.status == "True":
		state, class, glyph, message = "Denied", resources.StatusFail, "✕", denied.message
	case ready.status == "True":
		state, class, glyph = "Ready", resources.StatusOK, "●"
	case hasApproved && approved.status == "True":
		state = "Approved · not Ready"
	default:
		state = "pending approval"
	}
	return chainNode{
		Kind: kube.KindCertificateRequest, Name: u.GetName(), Depth: depth,
		StateText: state, Class: class, Glyph: glyph, Message: message,
		Created: conditionTime(ready, u.GetCreationTimestamp()),
	}
}

// orderNode/challengeNode read the ACME status.state enum directly — Order
// and Challenge carry no Ready-style condition array, unlike Certificate/
// CertificateRequest.
func orderNode(u *unstructured.Unstructured, depth int) chainNode {
	state, _, _ := unstructured.NestedString(u.Object, "status", "state")
	reason, _, _ := unstructured.NestedString(u.Object, "status", "reason")
	class, glyph := acmeStateClass(state, reason)
	text := state
	if text == "" {
		text = "pending"
	}
	return chainNode{
		Kind: kube.KindOrder, Name: u.GetName(), Depth: depth,
		StateText: text, Class: class, Glyph: glyph, Message: reason,
		Created: u.GetCreationTimestamp().Time,
	}
}

func challengeNode(u *unstructured.Unstructured, depth int) chainNode {
	typ, _, _ := unstructured.NestedString(u.Object, "spec", "type")
	dnsName, _, _ := unstructured.NestedString(u.Object, "spec", "dnsName")
	state, _, _ := unstructured.NestedString(u.Object, "status", "state")
	reason, _, _ := unstructured.NestedString(u.Object, "status", "reason")
	class, glyph := acmeStateClass(state, reason)
	text := state
	if text == "" {
		text = "pending"
	}
	detail := typ
	if typ != "" {
		text = typ + " · " + text
		if dnsName != "" {
			detail = typ + " · " + dnsName
		}
	}
	return chainNode{
		Kind: kube.KindChallenge, Name: u.GetName(), Depth: depth,
		StateText: text, Class: class, Glyph: glyph,
		Message: reason, Detail: detail,
		Created: u.GetCreationTimestamp().Time,
	}
}

// acmeStateClass maps an Order/Challenge's status.state to a status class.
// valid/ready is done; invalid/expired/errored is a terminal failure. A bare
// "pending"/"processing" is ordinary progress — but cert-manager only
// populates status.reason to explain something currently going wrong, never
// as routine narration, so a non-empty reason on an otherwise-pending object
// is itself the honest, on-cluster signal that this hop is the one stuck —
// the exact case this screen exists to surface.
func acmeStateClass(state, reason string) (resources.StatusClass, string) {
	switch state {
	case "valid", "ready":
		return resources.StatusOK, "●"
	case "invalid", "expired", "errored":
		return resources.StatusFail, "✕"
	default:
		if reason != "" {
			return resources.StatusFail, "✕"
		}
		return resources.StatusWarn, "▲"
	}
}

// readyState renders a Ready condition into the chain row's STATE cell and
// status class. Unknown/False is Warn, never Fail — see certNode's own
// comment for why.
func readyState(c condition) (text string, class resources.StatusClass, glyph string) {
	switch c.status {
	case "True":
		text, class, glyph = "True", resources.StatusOK, "●"
	case "False":
		text, class, glyph = "Ready=False", resources.StatusWarn, "▲"
		if c.reason != "" {
			text += " · " + c.reason
		}
	default:
		text, class, glyph = "–", resources.StatusNeutral, "·"
	}
	return
}

// buildFailure walks the chain deepest-first for the first genuinely
// failing node (mirrors fluxdetail's buildFailure walking its inventory for
// the first StatusFail entry) and assembles §35a's top band from it,
// verbatim. nil when nothing in the chain has actually failed — a
// still-in-flight chain gets no red card, only the healthy-tail's absence of
// one ("zero chrome until earned").
func buildFailure(chain []chainNode) *failure {
	for i := len(chain) - 1; i >= 0; i-- {
		n := chain[i]
		if n.Class != resources.StatusFail {
			continue
		}
		f := &failure{
			Kind: n.Kind, Name: n.Name, Detail: n.Detail,
			Message: firstNonEmpty(n.Message, n.StateText),
			Since:   n.Created,
		}
		if i > 0 {
			f.ParentKind, f.ParentName = chain[i-1].Kind, chain[i-1].Name
		}
		return f
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// failureTitle is the failure card's headline — "challenge failed", "order
// failed", etc. Mechanical and uniform across kinds rather than a per-kind
// phrase table: honest about what's known (which object, which kind) without
// inventing kind-specific prose.
func failureTitle(kind kube.ResourceKind) string {
	return strings.ToLower(string(kind)) + " failed"
}

// TLSSecretLister is implemented by a lister that can hand back just the
// kubernetes.io/tls Secrets in a namespace, from a cache holding only those
// (*kube.Cluster.ListTLSSecrets, docs/lazy-informers.md §5.7). cert-manager
// always writes a Certificate's Secret as that type. Optional: a lister
// without it falls back to the shared Secret cache, which on a real cluster
// starts the cluster-wide, full-payload Secret informer for one name.
type TLSSecretLister interface {
	ListTLSSecrets(ctx context.Context, namespace string) ([]runtime.Object, error)
}

// tlsSecrets reads namespace's Secrets from the narrowest source lister
// offers, and names the cache a miss must be gated on — KindTLSSecret when
// the filtered cache is what was read, never KindSecret, which answers for a
// cache this path deliberately leaves unstarted.
func tlsSecrets(ctx context.Context, lister resources.RawLister, namespace string) ([]runtime.Object, kube.ResourceKind, error) {
	if tl, ok := lister.(TLSSecretLister); ok {
		objs, err := tl.ListTLSSecrets(ctx, namespace)
		return objs, kube.KindTLSSecret, err
	}
	objs, err := lister.ListRaw(ctx, kube.KindSecret, namespace)
	return objs, kube.KindSecret, err
}

// resolveSecretRef reports whether the Certificate's spec.secretName Secret
// exists — existence only, no Ready concept for a Secret.
func resolveSecretRef(ctx context.Context, lister resources.RawLister, namespace string, cert *unstructured.Unstructured) (refInfo, bool) {
	secretName, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
	if secretName == "" {
		return refInfo{}, false
	}
	ref := refInfo{Label: "secret/" + secretName, Kind: kube.KindSecret, Name: secretName}
	secrets, cacheKind, err := tlsSecrets(ctx, lister, namespace)
	if err == nil && existsByName(secrets, secretName) {
		ref.Exists = true
	}
	if !ref.Exists && cacheKind == kube.KindTLSSecret && sharedSecretsLoaded(lister, namespace) {
		// The filtered cache holds only kubernetes.io/tls Secrets; one of
		// another type carrying tls.crt can still back the Certificate. Look
		// in the shared cache, but only when something already paid for it
		// — never start it here (docs/lazy-informers.md §5.7). A miss there
		// is a miss, since it holds every type.
		if shared, serr := lister.ListRaw(ctx, kube.KindSecret, namespace); serr == nil {
			switch found, hasCert := sharedSecretCert(shared, secretName); {
			case hasCert:
				ref.Exists, ref.StatusText = true, "exists"
			case found:
				ref.StatusText = "not a tls secret"
			default:
				ref.StatusText = "missing"
			}
			return ref, true
		}
	}
	if ref.Exists {
		ref.StatusText = "exists"
	} else if !markUnknownIn(&ref, cacheKind, lister, namespace, err) {
		ref.StatusText = "missing"
		if cacheKind == kube.KindTLSSecret {
			// Only kubernetes.io/tls Secrets were checked.
			ref.StatusText = "missing as kubernetes.io/tls"
		}
	}
	return ref, true
}

// sharedSecretsLoaded reports whether the shared Secret cache at namespace
// is already running, filled and readable — the only condition under which
// the Secret ref may consult it after a miss in the TLS cache. A lister that
// can't say whether the cache is started is treated as not started: on a
// real cluster reading it starts the cluster-wide, full-payload Secret
// informer the TLS cache exists to avoid.
func sharedSecretsLoaded(lister resources.RawLister, namespace string) bool {
	ks, ok := lister.(tui.KindStartedChecker)
	if !ok || !ks.KindStarted(kube.KindSecret, namespace) {
		return false
	}
	return tui.KindsSynced(lister, namespace, kube.KindSecret) && tui.KindsError(lister, namespace, kube.KindSecret) == nil
}

// sharedSecretCert reports whether objs holds a Secret named name, and
// whether it carries a tls.crt key.
func sharedSecretCert(objs []runtime.Object, name string) (found, hasCert bool) {
	for _, o := range objs {
		switch s := o.(type) {
		case *corev1.Secret:
			if s.Name == name {
				_, has := s.Data["tls.crt"]
				return true, has
			}
		case *unstructured.Unstructured:
			if s.GetName() == name {
				_, has, _ := unstructured.NestedFieldNoCopy(s.Object, "data", "tls.crt")
				return true, has
			}
		}
	}
	return false, false
}

// cacheState is one kind's cache answer for namespace: pending while it is
// still filling, or the reason it stopped (KindsError — Forbidden, a LIST
// that keeps failing). Settled is not the same as empty.
func cacheState(lister resources.RawLister, namespace string, kind kube.ResourceKind) (pending bool, err error) {
	if !tui.KindsSynced(lister, namespace, kind) {
		return true, nil
	}
	return false, tui.KindsError(lister, namespace, kind)
}

// markUnknown turns a ref lookup that missed into an honest unknown when
// the miss proves nothing — the cache for ref.Kind at namespace hasn't
// filled, or can't be read — and reports whether it did. Otherwise the
// caller's "missing" stands.
func markUnknown(ref *refInfo, lister resources.RawLister, namespace string, listErr error) bool {
	return markUnknownIn(ref, ref.Kind, lister, namespace, listErr)
}

// markUnknownIn is markUnknown for a ref read from a cache other than its
// own kind's — the Secret ref, read from the filtered TLS cache.
func markUnknownIn(ref *refInfo, cacheKind kube.ResourceKind, lister resources.RawLister, namespace string, listErr error) bool {
	pending, kerr := cacheState(lister, namespace, cacheKind)
	if kerr == nil {
		kerr = listErr
	}
	switch {
	case pending:
		ref.Unknown, ref.pending, ref.StatusText = true, true, "loading…"
	case kerr != nil && kube.IsPermissionError(kerr):
		ref.Unknown, ref.StatusText = true, "permission denied"
	case kerr != nil:
		ref.Unknown, ref.StatusText = true, "unavailable"
	default:
		return false
	}
	return true
}

// resolveIssuerRef reports the Certificate's issuerRef existence and, when
// found, its own Ready state. cert-manager defaults an omitted kind to an
// Issuer in the Certificate's own namespace; a ref into any group other than
// cert-manager.io names an external issuer (AWS PCA, Google CAS, …) kute has
// no kind for, so it is shown as-is, neutral, with no read and no jump.
func resolveIssuerRef(ctx context.Context, lister resources.RawLister, namespace string, cert *unstructured.Unstructured) (refInfo, bool) {
	issuerName, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	issuerKindStr, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "kind")
	issuerGroup, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "group")
	if issuerName == "" {
		return refInfo{}, false
	}
	if issuerGroup != "" && issuerGroup != "cert-manager.io" {
		label := issuerKindStr
		if label == "" {
			label = string(kube.KindIssuer)
		}
		return refInfo{
			Label: strings.ToLower(label) + "/" + issuerName, Name: issuerName,
			External: true, StatusText: issuerGroup,
		}, true
	}
	kind := kube.KindIssuer
	issuerNamespace := namespace
	if issuerKindStr == string(kube.KindClusterIssuer) {
		kind = kube.KindClusterIssuer
		issuerNamespace = ""
	}
	ref := refInfo{
		Label: strings.ToLower(string(kind)) + "/" + issuerName,
		Kind:  kind, Name: issuerName, HasReady: true,
	}
	issuers, err := lister.ListRaw(ctx, kind, issuerNamespace)
	if err == nil {
		if u, ok := findByName(issuers, issuerName); ok {
			ref.Exists = true
			cond, _ := readCondition(u, "Ready")
			ref.Ready = cond.status == "True"
		}
	}
	switch {
	case !ref.Exists && markUnknown(&ref, lister, issuerNamespace, err):
	case !ref.Exists:
		ref.StatusText = "missing"
	case ref.Ready:
		ref.StatusText = "Ready"
	default:
		ref.StatusText = "not Ready"
	}
	return ref, true
}
