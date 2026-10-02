package routetable

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// load dispatches to the flavor-specific loader for m.kind — the single
// entry point Init/actions.ResultMsg-driven reloads call.
func (m Model) load() tea.Cmd {
	switch m.flavor {
	case flavorGateway:
		return m.loadGateway()
	case flavorIngress:
		return m.loadIngress()
	default:
		return m.loadRoute()
	}
}

// loadIngress resolves 23a: the Ingress itself, one row per rule host+path,
// and a TLS fact per Spec.TLS block.
func (m Model) loadIngress() tea.Cmd {
	lister, ns, name, timeout := m.lister, m.namespace, m.name, m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		objs, err := lister.ListRaw(ctx, kube.KindIngress, ns)
		if err != nil {
			return loadedMsg{err: err}
		}
		var ing *networkingv1.Ingress
		for _, obj := range objs {
			if i, ok := obj.(*networkingv1.Ingress); ok && i.Name == name {
				ing = i
				break
			}
		}
		if ing == nil {
			return loadedMsg{err: fmt.Errorf("ingress %q not found", name)}
		}

		class := "-"
		if ing.Spec.IngressClassName != nil && *ing.Spec.IngressClassName != "" {
			class = *ing.Spec.IngressClassName
		}

		httpsHosts := map[string]bool{}
		for _, t := range ing.Spec.TLS {
			if len(t.Hosts) == 0 {
				httpsHosts[""] = true // no hosts named = covers every rule
			}
			for _, h := range t.Hosts {
				httpsHosts[h] = true
			}
		}

		// facts + hostFactIdx resolve every rule host to the TLS block that
		// covers it — hostFactIdx feeds each row's compact TLS cell,
		// wildcardFactIdx is the "no Hosts named" block that covers every
		// rule (mirrors httpsHosts[""] above).
		var facts []tlsFact
		certsPending := false
		hostFactIdx := map[string]int{}
		wildcardFactIdx := -1
		for i, t := range ing.Spec.TLS {
			expiry, ec, pending := resolveCertExpiry(ctx, lister, ns, t.SecretName)
			certsPending = certsPending || pending
			facts = append(facts, tlsFact{secretName: t.SecretName, hosts: t.Hosts, expiry: expiry, class: ec})
			if len(t.Hosts) == 0 {
				wildcardFactIdx = i
			}
			for _, h := range t.Hosts {
				hostFactIdx[h] = i
			}
		}

		var rows []routeRow
		hostSet := map[string]struct{}{}
		for _, r := range ing.Spec.Rules {
			if r.HTTP == nil {
				continue
			}
			host := r.Host
			hostText := host
			if hostText == "" {
				hostText = "*"
			}
			hostSet[hostText] = struct{}{}
			scheme := "http"
			if httpsHosts[""] || httpsHosts[host] {
				scheme = "https"
			}
			tlsText, tlsClass := "", resources.StatusNeutral
			if idx, ok := hostFactIdx[host]; ok {
				tlsText, tlsClass = compactExpiry(facts[idx].expiry), facts[idx].class
			} else if wildcardFactIdx >= 0 {
				tlsText, tlsClass = compactExpiry(facts[wildcardFactIdx].expiry), facts[wildcardFactIdx].class
			}
			for _, p := range r.HTTP.Paths {
				row, ok := ingressBackendRow(ctx, lister, ns, p.Backend)
				if !ok {
					continue
				}
				row.match = hostText + " " + p.Path
				row.tlsText, row.tlsClass = tlsText, tlsClass
				row.url = scheme + "://" + host + p.Path
				rows = append(rows, row)
			}
		}
		// spec.defaultBackend catches every request no rule matches — a
		// defaultBackend-only Ingress is a common catch-all, not one with
		// no routes.
		if ing.Spec.DefaultBackend != nil {
			if row, ok := ingressBackendRow(ctx, lister, ns, *ing.Spec.DefaultBackend); ok {
				row.match = "* (default)"
				rows = append(rows, row)
			}
		}

		return loadedMsg{
			flavor: flavorIngress, ingressClass: class, ingressHostCount: len(hostSet),
			rows: rows, tlsFacts: facts, certsPending: certsPending,
		}
	}
}

// ingressBackendRow resolves one Ingress backend into a row's backend
// fields: a Service through the shared ●/✕/▲ resolution, a resource backend
// (spec.rules[].http.paths[].backend.resource) as a neutral "<kind>/<name>"
// kute has nothing to resolve against and never jumps to.
func ingressBackendRow(ctx context.Context, lister resources.RawLister, ns string, b networkingv1.IngressBackend) (routeRow, bool) {
	switch {
	case b.Service != nil:
		svcName := b.Service.Name
		state := resources.ResolveServiceBackend(ctx, lister, ns, svcName)
		glyph, class := state.Glyph()
		return routeRow{
			backendNS: ns, backendName: svcName,
			backendText:   svcName + ":" + backendPortText(ctx, lister, ns, svcName, b.Service.Port),
			glyph:         glyph,
			class:         class,
			endpointsText: formatEndpoints(state),
		}, true
	case b.Resource != nil:
		return routeRow{
			backendText: strings.ToLower(b.Resource.Kind) + "/" + b.Resource.Name,
			glyph:       "·", class: resources.StatusNeutral, endpointsText: "—",
		}, true
	}
	return routeRow{}, false
}

// formatEndpoints renders a resolved backend's ready-pod count for the
// ENDPOINTS column (docs/design README.md §23a/§23b) — "N ready" when
// resolvable, "—" when the Service doesn't exist or its readiness can't be
// computed (no selector).
func formatEndpoints(state resources.BackendState) string {
	if !state.Exists || state.Unresolvable {
		return "—"
	}
	return fmt.Sprintf("%d ready", state.Ready)
}

// compactExpiry drops resolveCertExpiry's "expires in " prefix for the
// row-level TLS cell ("61d" rather than "expires in 61d") — the bottom "tls"
// strip keeps the full phrase.
func compactExpiry(full string) string {
	return strings.TrimPrefix(full, "expires in ")
}

// backendPortText resolves an Ingress path's backend port to its numeric
// display form: Number is used directly; a named port is looked up against
// the Service's own declared ports (falling back to the bare name if the
// Service can't be read, rather than erroring the whole row).
func backendPortText(ctx context.Context, lister resources.RawLister, ns, svcName string, port networkingv1.ServiceBackendPort) string {
	if port.Number != 0 {
		return strconv.Itoa(int(port.Number))
	}
	if port.Name == "" {
		return "-"
	}
	objs, err := lister.ListRaw(ctx, kube.KindService, ns)
	if err == nil {
		for _, obj := range objs {
			svc, ok := obj.(*corev1.Service)
			if !ok || svc.Name != svcName {
				continue
			}
			for _, sp := range svc.Spec.Ports {
				if sp.Name == port.Name {
					return strconv.Itoa(int(sp.Port))
				}
			}
		}
	}
	return port.Name
}

// resolveCertExpiry parses a TLS Secret's tls.crt to report expiry, per
// docs/design README.md §23a: "yellow <30d, red expired". A miss is only
// "secret not found" once the Secret cache for ns has settled without an
// error: a still-filling cache reports pending (the caller schedules a
// retry, since a namespace with no such Secret syncs without an event), and
// a Forbidden one — common, many users can't list Secrets — says so,
// neutral, instead of a permanent red claim about the cluster.
func resolveCertExpiry(ctx context.Context, lister resources.RawLister, ns, secretName string) (text string, class resources.StatusClass, pending bool) {
	if lister == nil || secretName == "" {
		return "–", resources.StatusNeutral, false
	}
	objs, err := lister.ListRaw(ctx, kube.KindSecret, ns)
	if err != nil {
		return "–", resources.StatusNeutral, false
	}
	var secret *corev1.Secret
	for _, obj := range objs {
		if s, ok := obj.(*corev1.Secret); ok && s.Name == secretName {
			secret = s
			break
		}
	}
	if secret == nil {
		if !tui.KindsSynced(lister, ns, kube.KindSecret) {
			return "loading…", resources.StatusNeutral, true
		}
		if kerr := tui.KindsError(lister, ns, kube.KindSecret); kerr != nil {
			if kube.IsPermissionError(kerr) {
				return "secrets: permission denied", resources.StatusNeutral, false
			}
			return "secrets: unavailable", resources.StatusNeutral, false
		}
		return "secret not found", resources.StatusFail, false
	}
	raw := secret.Data["tls.crt"]
	if len(raw) == 0 {
		return "no cert data", resources.StatusWarn, false
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "invalid cert", resources.StatusWarn, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "invalid cert", resources.StatusWarn, false
	}
	days, expired, class := resources.CertExpiry(cert.NotAfter, time.Now())
	switch {
	case expired && days == 0:
		return "expired <1d ago", class, false
	case expired:
		return fmt.Sprintf("expired %dd ago", days), class, false
	default:
		return fmt.Sprintf("expires in %dd", days), class, false
	}
}

// loadRoute resolves 23b's HTTPRoute/GRPCRoute/TCPRoute flavor: one row per
// rule-match x backendRef (weighted splits stacked under their match) plus
// the parent-Gateway summary from status.parents. Every field is read
// tolerantly via unstructured.Nested* so GRPCRoute/TCPRoute (which lack
// hostnames/path matches) degrade to backendRef-only rows instead of
// erroring.
func (m Model) loadRoute() tea.Cmd {
	lister, kind, ns, name, timeout := m.lister, m.kind, m.namespace, m.name, m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		u, err := findUnstructured(ctx, lister, kind, ns, name)
		if err != nil {
			return loadedMsg{err: err}
		}

		rows := routeRowsFromRoute(ctx, lister, ns, u)
		parentText, attached, parentNS, parentName, sectionName := parentSummary(ns, u)
		listenerText, certsPending := "", false
		if parentName != "" {
			listenerText, certsPending = resolveParentListenerDetail(ctx, lister, parentNS, parentName, sectionName)
		}

		hostnames, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "hostnames")
		hostText := "*"
		if len(hostnames) > 0 {
			hostText = strings.Join(hostnames, ",")
		}
		rulesRaw, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")

		return loadedMsg{
			flavor: flavorRoute, rows: rows,
			parentText: parentText, parentAttached: attached,
			parentGatewayNS: parentNS, parentGatewayName: parentName,
			parentListenerText: listenerText,
			routeHostText:      hostText, routeRuleCount: len(rulesRaw),
			certsPending: certsPending,
		}
	}
}

// resolveParentListenerDetail resolves the accepted parent Gateway's own
// listener into the below-table "parent" line's detail (docs/design
// README.md §23b: "gw/public · listener https:443 · secret/nva-tls-prod
// expires in 61d") — best-effort: a Gateway that can't be read, or a listener
// name that no longer matches, still returns the bare "gw/<name>" identity
// rather than an empty line.
func resolveParentListenerDetail(ctx context.Context, lister resources.RawLister, gatewayNS, gatewayName, sectionName string) (text string, certsPending bool) {
	base := "gw/" + gatewayName
	u, err := findUnstructured(ctx, lister, kube.KindGateway, gatewayNS, gatewayName)
	if err != nil {
		return base, false
	}
	listenersSpec, _, _ := unstructured.NestedSlice(u.Object, "spec", "listeners")
	for _, l := range listenersSpec {
		lm, ok := l.(map[string]any)
		if !ok {
			continue
		}
		lname, _, _ := unstructured.NestedString(lm, "name")
		if sectionName != "" && lname != sectionName {
			continue
		}
		proto, _, _ := unstructured.NestedString(lm, "protocol")
		port, _, _ := unstructured.NestedInt64(lm, "port")
		detail := fmt.Sprintf("%s · listener %s:%d", base, proto, port)
		certRefs, found, _ := unstructured.NestedSlice(lm, "tls", "certificateRefs")
		if !found || len(certRefs) == 0 {
			return detail, false
		}
		cr, ok := certRefs[0].(map[string]any)
		if !ok {
			return detail, false
		}
		secretName, _, _ := unstructured.NestedString(cr, "name")
		expiry, _, pending := resolveCertExpiry(ctx, lister, certRefNamespace(cr, gatewayNS), secretName)
		return fmt.Sprintf("%s · secret/%s %s", detail, secretName, expiry), pending
	}
	return base, false
}

// certRefNamespace is a listener certificateRef's own namespace, defaulting
// to the Gateway's — a cross-namespace certificateRef is legal wherever a
// ReferenceGrant permits it.
func certRefNamespace(certRef map[string]any, gatewayNS string) string {
	if ns, _, _ := unstructured.NestedString(certRef, "namespace"); ns != "" {
		return ns
	}
	return gatewayNS
}

func findUnstructured(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, ns, name string) (*unstructured.Unstructured, error) {
	objs, err := lister.ListRaw(ctx, kind, ns)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetName() == name {
			return u, nil
		}
	}
	return nil, fmt.Errorf("%s %q not found", kind, name)
}

func routeRowsFromRoute(ctx context.Context, lister resources.RawLister, ns string, u *unstructured.Unstructured) []routeRow {
	refNotPermitted := routeRefsNotPermitted(u)
	rulesRaw, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
	var rows []routeRow
	for _, rr := range rulesRaw {
		rule, ok := rr.(map[string]any)
		if !ok {
			continue
		}
		matchText := routeMatchText(rule)

		type backendRef struct {
			namespace string
			name      string
			kind      string // "" for a core Service, else the ref's own kind
			port      int64
			weight    int64
		}
		var refs []backendRef
		var totalWeight int64
		backendRefsRaw, _, _ := unstructured.NestedSlice(rule, "backendRefs")
		for _, b := range backendRefsRaw {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(bm, "name")
			port, _, _ := unstructured.NestedInt64(bm, "port")
			weight, found, _ := unstructured.NestedInt64(bm, "weight")
			if !found {
				weight = 1
			}
			// backendRefs[].namespace defaults to the route's own; a cross-
			// namespace ref is legal wherever a ReferenceGrant permits it.
			refNS, _, _ := unstructured.NestedString(bm, "namespace")
			if refNS == "" {
				refNS = ns
			}
			kind, _, _ := unstructured.NestedString(bm, "kind")
			group, _, _ := unstructured.NestedString(bm, "group")
			if (group == "" || group == "core") && (kind == "" || kind == "Service") {
				kind = ""
			} else if kind == "" {
				kind = "Service"
			}
			refs = append(refs, backendRef{namespace: refNS, name: name, kind: kind, port: port, weight: weight})
			totalWeight += weight
		}

		for i, ref := range refs {
			match := ""
			if i == 0 {
				match = matchText
			}
			weightPct := ""
			if len(refs) > 1 && totalWeight > 0 {
				weightPct = fmt.Sprintf("%d%%", ref.weight*100/totalWeight)
			}
			portText := "-"
			if ref.port != 0 {
				portText = strconv.FormatInt(ref.port, 10)
			}
			label := ref.name
			if ref.namespace != ns {
				label = ref.namespace + "/" + ref.name
			}
			row := routeRow{match: match, weightPct: weightPct}
			switch {
			case ref.kind != "":
				// Not a core Service (a ServiceImport, an extension's own
				// backend kind): kute has nothing to resolve it against, so
				// it is shown as-is — never looked up as a same-named
				// Service, never a jump target.
				row.backendText = strings.ToLower(ref.kind) + "/" + label + ":" + portText
				row.glyph, row.class, row.endpointsText = "·", resources.StatusNeutral, "—"
			case ref.namespace != ns && refNotPermitted:
				// The controller's own verdict (ResolvedRefs=False,
				// RefNotPermitted only ever names a cross-namespace ref): no
				// ReferenceGrant lets this route reach it, whatever exists
				// there.
				row.backendNS, row.backendName = ref.namespace, ref.name
				row.backendText = label + ":" + portText
				row.glyph, row.class, row.endpointsText = "✕", resources.StatusFail, "not permitted"
			default:
				state := resources.ResolveServiceBackend(ctx, lister, ref.namespace, ref.name)
				row.backendNS, row.backendName = ref.namespace, ref.name
				row.backendText = label + ":" + portText
				row.glyph, row.class = state.Glyph()
				row.endpointsText = formatEndpoints(state)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// routeRefsNotPermitted reports whether any parent's ResolvedRefs condition
// is False with reason RefNotPermitted — the controller's verdict that a
// cross-namespace ref has no ReferenceGrant, which kute reports rather than
// re-deriving by reading ReferenceGrants itself.
func routeRefsNotPermitted(u *unstructured.Unstructured) bool {
	parents, _, _ := unstructured.NestedSlice(u.Object, "status", "parents")
	for _, p := range parents {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		conds, _, _ := unstructured.NestedSlice(pm, "conditions")
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			typ, _, _ := unstructured.NestedString(cm, "type")
			status, _, _ := unstructured.NestedString(cm, "status")
			reason, _, _ := unstructured.NestedString(cm, "reason")
			if typ == "ResolvedRefs" && status == "False" && reason == "RefNotPermitted" {
				return true
			}
		}
	}
	return false
}

// routeMatchText renders one rule's match summary: the path match plus its
// type ("prefix"/"exact"/"regex") and any header matches (docs/design
// README.md §23b: "/internal prefix · header x-env=stage") — hostnames live
// in the top strip, not here. GRPCRoute/TCPRoute rules carry no "matches"
// field at all, so this degrades to "*" (match-all).
func routeMatchText(rule map[string]any) string {
	matches, _, _ := unstructured.NestedSlice(rule, "matches")
	if len(matches) == 0 {
		return "*"
	}
	pm, ok := matches[0].(map[string]any)
	if !ok {
		return "*"
	}
	path, _, _ := unstructured.NestedString(pm, "path", "value")
	if path == "" {
		return "*"
	}
	var extras []string
	if label := routeMatchTypeLabel(pm); label != "" {
		extras = append(extras, label)
	}
	if headers := routeHeaderMatchSummary(pm); headers != "" {
		extras = append(extras, headers)
	}
	if len(extras) == 0 {
		return path
	}
	return path + " " + strings.Join(extras, " · ")
}

// routeMatchTypeLabel renders spec.rules[].matches[].path.type in the
// mockups' lowercase form ("prefix", "exact", "regex"); PathPrefix is
// Gateway API's default when type is unset.
func routeMatchTypeLabel(pathMatch map[string]any) string {
	switch t, _, _ := unstructured.NestedString(pathMatch, "path", "type"); t {
	case "Exact":
		return "exact"
	case "RegularExpression":
		return "regex"
	default:
		return "prefix"
	}
}

// routeHeaderMatchSummary renders matches[0].headers as "header k=v"
// clauses, joined for the rare multi-header rule.
func routeHeaderMatchSummary(pathMatch map[string]any) string {
	headers, _, _ := unstructured.NestedSlice(pathMatch, "headers")
	if len(headers) == 0 {
		return ""
	}
	parts := make([]string, 0, len(headers))
	for _, h := range headers {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(hm, "name")
		if name == "" {
			continue
		}
		value, _, _ := unstructured.NestedString(hm, "value")
		parts = append(parts, "header "+name+"="+value)
	}
	return strings.Join(parts, ", ")
}

// parentSummary resolves status.parents' first Accepted condition into the
// docs/design README.md §23b parent strip text ("gw/public › https:443 ·
// accepted" — the leading ✓/✕ glyph is the view's own span, styled off
// attached) plus the Gateway identity 'p'/the below-table listener lookup
// need.
func parentSummary(routeNamespace string, u *unstructured.Unstructured) (text string, attached bool, gatewayNS, gatewayName, sectionName string) {
	parents, _, _ := unstructured.NestedSlice(u.Object, "status", "parents")
	for _, p := range parents {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		parentName, _, _ := unstructured.NestedString(pm, "parentRef", "name")
		parentNS, hasNS, _ := unstructured.NestedString(pm, "parentRef", "namespace")
		if !hasNS || parentNS == "" {
			parentNS = routeNamespace
		}
		section, _, _ := unstructured.NestedString(pm, "parentRef", "sectionName")

		conds, _, _ := unstructured.NestedSlice(pm, "conditions")
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if typ, _, _ := unstructured.NestedString(cm, "type"); typ != "Accepted" {
				continue
			}
			label := "gw/" + parentName
			if section != "" {
				label += " › " + section
			}
			if st, _, _ := unstructured.NestedString(cm, "status"); st == "True" {
				return label + " · accepted", true, parentNS, parentName, section
			}
			msg, _, _ := unstructured.NestedString(cm, "message")
			text := label + " · not accepted"
			if msg != "" {
				text += ": " + msg
			}
			return text, false, parentNS, parentName, section
		}
	}
	return "no parent status yet", false, "", "", ""
}

// loadGateway resolves 23b's Gateway flavor: one row per spec.listeners
// entry, its status.listeners counterpart supplying the attached-route
// count.
func (m Model) loadGateway() tea.Cmd {
	lister, ns, name, timeout := m.lister, m.namespace, m.name, m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		u, err := findUnstructured(ctx, lister, kube.KindGateway, ns, name)
		if err != nil {
			return loadedMsg{err: err}
		}

		gwClass, _, _ := unstructured.NestedString(u.Object, "spec", "gatewayClassName")

		attachedByName := map[string]int64{}
		statusListeners, _, _ := unstructured.NestedSlice(u.Object, "status", "listeners")
		for _, sl := range statusListeners {
			slm, ok := sl.(map[string]any)
			if !ok {
				continue
			}
			lname, _, _ := unstructured.NestedString(slm, "name")
			n, _, _ := unstructured.NestedInt64(slm, "attachedRoutes")
			attachedByName[lname] = n
		}

		var rows []listenerRow
		certsPending := false
		listenersSpec, _, _ := unstructured.NestedSlice(u.Object, "spec", "listeners")
		for _, l := range listenersSpec {
			lm, ok := l.(map[string]any)
			if !ok {
				continue
			}
			lname, _, _ := unstructured.NestedString(lm, "name")
			proto, _, _ := unstructured.NestedString(lm, "protocol")
			port, _, _ := unstructured.NestedInt64(lm, "port")
			hostname, _, _ := unstructured.NestedString(lm, "hostname")
			if hostname == "" {
				hostname = "*"
			}

			tlsText, tlsClass := "–", resources.StatusNeutral
			if certRefs, found, _ := unstructured.NestedSlice(lm, "tls", "certificateRefs"); found && len(certRefs) > 0 {
				if cr, ok := certRefs[0].(map[string]any); ok {
					secretName, _, _ := unstructured.NestedString(cr, "name")
					var pending bool
					tlsText, tlsClass, pending = resolveCertExpiry(ctx, lister, certRefNamespace(cr, ns), secretName)
					certsPending = certsPending || pending
				}
			}

			rows = append(rows, listenerRow{
				name: lname, protoPort: fmt.Sprintf("%s:%d", proto, port), hostname: hostname,
				tlsText: tlsText, tlsClass: tlsClass, attached: int(attachedByName[lname]),
			})
		}

		return loadedMsg{flavor: flavorGateway, gatewayClass: gwClass, listeners: rows, certsPending: certsPending}
	}
}
