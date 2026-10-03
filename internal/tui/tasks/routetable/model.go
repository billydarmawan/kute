// Package routetable is 23a/23b (docs/design/README.md): a live routing
// table replacing "read the raw YAML and join it in your head" — reached
// from tasks/browse's 'enter' on an Ingress (23a), or on a discovered
// HTTPRoute/GRPCRoute/TCPRoute/Gateway (23b, docs/design README.md's Gateway
// API kinds — always CRD-discovered, never a DefaultRegistry entry, per
// CLAUDE.md's "CRD support is data, not code").
//
// One package, three flavors selected by Kind at load time:
//   - flavorIngress: one row per rule host+path -> Service:port, plus a TLS
//     strip naming each referenced Secret's parsed cert expiry.
//   - flavorRoute (HTTPRoute/GRPCRoute/TCPRoute): one row per rule-match x
//     backendRef, weighted splits stacked under their match; a parent strip
//     resolves status.parents against the Gateway that accepted (or
//     rejected) the route.
//   - flavorGateway: one row per listener, each carrying its own
//     attached-route count from status.listeners.
//
// Every backend's health (docs/design README.md's shared "● ready endpoints
// / ✕ not found / ▲ 0 ready" grammar) comes from
// resources.ResolveServiceBackend — a Service lookup plus a selector match
// against cached Pods, deliberately reusing the already-watched Service/Pod
// informers rather than a new EndpointSlice one.
package routetable

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// flavor picks which of the three §23a/§23b layouts a Model renders —
// resolved once at load time from Config.Kind (see load.go's New/load).
type flavor int

const (
	flavorIngress flavor = iota
	flavorRoute          // HTTPRoute, GRPCRoute, TCPRoute
	flavorGateway
)

// routeRow is one resolved host/path (or rule-match) -> backend line, shared
// by the Ingress and HTTPRoute/GRPCRoute/TCPRoute flavors. match is blank for
// a row that continues the previous row's match (a weighted multi-backend
// split) — view.go renders that as "└ same match" rather than repeating text,
// and colors weightPct as the docs/design README.md §23b "canary weight
// yellow" case.
type routeRow struct {
	match         string
	backendNS     string
	backendName   string
	backendText   string // "svc-name:8080"
	glyph         string
	class         resources.StatusClass
	weightPct     string // "" unless this match has more than one backendRef
	endpointsText string // "N ready" or "—" (backend not found/unresolvable)
	tlsText       string // ingress flavor only: compact cert-expiry ("61d"); "" = no TLS block covers this host
	tlsClass      resources.StatusClass
	url           string // full URL, ingress flavor only ("y copies the full URL")
}

// listenerRow is one Gateway listener — flavorGateway's own row shape.
type listenerRow struct {
	name      string
	protoPort string // "HTTPS:443"
	hostname  string // raw spec value; "" (any hostname) renders as "*"
	tlsText   string
	tlsClass  resources.StatusClass
	attached  int
}

// tlsFact is one Ingress TLS block's resolved cert-expiry fact — the strip
// docs/design README.md §23a describes ("a strip above the keybar names each
// secret").
type tlsFact struct {
	secretName string
	hosts      []string
	expiry     string
	class      resources.StatusClass
}

// YAMLReader is the seam for 'Y' (§23a/§23b: "copy yaml") — satisfied by
// *kube.Cluster and *fake.Cluster already (kube/yaml.go, kube/fake/fake.go).
type YAMLReader interface {
	GetYAML(ctx context.Context, kind kube.ResourceKind, namespace, name string) (text, resourceVersion string, err error)
}

// OpenEventsFunc pushes tasks/events (9b) object-scoped for the loaded
// Ingress/HTTPRoute/Gateway on 'e' — same shape as poddetail/nodedetail's own
// OpenEventsFunc.
type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// Config are routetable's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values).
type Config struct {
	Session     *tui.Session
	Lister      resources.RawLister
	YAML        YAMLReader
	OpenEvents  OpenEventsFunc
	Kind        kube.ResourceKind
	Namespace   string
	Name        string
	LoadTimeout time.Duration
}

type Model struct {
	width, height int

	session    *tui.Session
	lister     resources.RawLister
	yaml       YAMLReader
	openEvents OpenEventsFunc
	kind       kube.ResourceKind
	namespace  string
	name       string
	timeout    time.Duration

	flavor flavor

	// Ingress-only facts.
	ingressClass     string
	ingressHostCount int
	tlsFacts         []tlsFact
	// tlsFocused/tlsSelected are 23a's "tab" toggle onto the TLS strip
	// (verbs.FocusTLSStrip) — up/down move tlsSelected instead of the main
	// table's own selection while focused, and ↵ jumps to that fact's
	// Secret (verbs.OpenTLSSecret) instead of the row's backend Service.
	tlsFocused  bool
	tlsSelected int

	// Route rows shared by flavorIngress/flavorRoute.
	rows []routeRow

	// HTTPRoute-only facts: parent-Gateway summary ('p' jump target) plus the
	// hostnames/rule-count strip and the resolved parent-listener detail line
	// (§23b's below-table "parent" strip).
	parentText         string
	parentAttached     bool
	parentGatewayNS    string
	parentGatewayName  string
	parentListenerText string
	routeHostText      string
	routeRuleCount     int

	// Gateway-only facts.
	gatewayClass string
	listeners    []listenerRow

	selected, offset int

	conn kube.ConnState
	now  time.Time

	state    tui.TaskState
	feedback string
	spinner  spinner.Model

	// backendDeniedNote is non-empty when the Service/Pod caches backing
	// the BACKENDS column (flavorIngress/flavorRoute only — Gateway never
	// resolves a backend) are still filling or came back with an error on
	// the most recent Ready load with rows already on screen — rendered as
	// one extra strip line rather than letting every backend read a false
	// ✕ "not found" (resources.ResolveServiceBackend returns the same
	// BackendState for "still filling", "denied", and "genuinely absent").
	backendDeniedNote string

	loadStartedAt time.Time

	// syncRetryGen guards a cache-sync retry against a newer load having
	// landed in the meantime — same shape as events'/overview's own.
	syncRetryGen int
}

// loadedMsg carries one load()'s result — exactly one of the three flavor
// payload groups is populated, per msg.flavor.
type loadedMsg struct {
	flavor flavor
	err    error

	ingressClass     string
	ingressHostCount int
	tlsFacts         []tlsFact
	rows             []routeRow

	parentText         string
	parentAttached     bool
	parentGatewayNS    string
	parentGatewayName  string
	parentListenerText string
	routeHostText      string
	routeRuleCount     int

	gatewayClass string
	listeners    []listenerRow

	// certsPending is set when a TLS cert lookup missed against a Secret
	// cache still filling — applyLoaded retries, since a Secret that
	// genuinely isn't there syncs without a change event.
	certsPending bool
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading " + string(cfg.Kind) + "/" + cfg.Name + "..."
	if cfg.Lister == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:         tui.DefaultWidth,
		height:        tui.DefaultHeight,
		session:       cfg.Session,
		lister:        cfg.Lister,
		yaml:          cfg.YAML,
		openEvents:    cfg.OpenEvents,
		kind:          cfg.Kind,
		namespace:     cfg.Namespace,
		name:          cfg.Name,
		timeout:       cfg.LoadTimeout,
		flavor:        flavorFor(cfg.Kind),
		state:         state,
		feedback:      feedback,
		now:           time.Now(),
		loadStartedAt: time.Now(),
		spinner:       components.NewSpinner(),
	}
}

// flavorFor picks the layout for kind — Ingress is the only built-in
// registry kind this screen handles; every other kind it recognizes is a
// discovered Gateway API CRD (never a DefaultRegistry entry).
func flavorFor(kind kube.ResourceKind) flavor {
	switch kind {
	case kube.KindGateway:
		return flavorGateway
	case kube.KindIngress:
		return flavorIngress
	default:
		return flavorRoute
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

func (m Model) selectedRouteRow() (routeRow, bool) {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return routeRow{}, false
	}
	return m.rows[m.selected], true
}

func (m Model) selectedListener() (listenerRow, bool) {
	if m.selected < 0 || m.selected >= len(m.listeners) {
		return listenerRow{}, false
	}
	return m.listeners[m.selected], true
}

// rowCount is the currently-selectable row count for the active flavor —
// moveSelection/clampOffset stay flavor-agnostic by going through this.
func (m Model) rowCount() int {
	if m.flavor == flavorGateway {
		return len(m.listeners)
	}
	return len(m.rows)
}

// ScreenObject implements tui.ObjectScreen: a namespace switch from here
// follows this object into the new namespace.
func (m *Model) ScreenObject() (kube.ResourceKind, string, string) {
	return m.kind, m.namespace, m.name
}

var _ tui.ObjectScreen = (*Model)(nil)
