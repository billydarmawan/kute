// Package certchain is §35a (docs/design/v.0.7.0.dc.html): the cert-manager
// Certificate issuance-chain view reached from browse's ↵ on a Certificate
// row.
//
// It exists as its own screen rather than as a branch inside 14d for the
// same reason tasks/fluxdetail (§31a) does: 14d's contract is "built only
// from what every object has — no CRD-specific layout code, ever", and this
// screen is entirely cert-manager-specific — it walks the
// Certificate → CertificateRequest → Order → Challenge ownerRef chain and
// promotes whichever object in it is the deepest real failure. Putting that
// walk in 14d would break the invariant that makes 14d work for every
// discovered kind on earth. CertificateRequest, Order and Challenge still
// have no destination screens of their own: a chain row's ↵ lands on 14d,
// same as browsing to any other discovered kind directly.
//
// Band order is 5a's promotion rule, same as fluxdetail's: why it is broken
// first, what it is wired to second, what it depends on third.
//
// §35b (the Certificates list's own EXPIRES/RENEWAL columns) is out of
// scope here. §35c's `r` force-renew verb acts on the Certificate itself —
// the object this whole screen is about — regardless of which chain/refs
// row is selected, the same "always the object, never the selection" shape
// 'y'/'e' above already take.
package certchain

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
)

// EventsReader is the seam for 'e' — same shape as objectdetail's/
// fluxdetail's sibling screens, duplicated per the repo's package-local-seam
// convention.
type EventsReader interface {
	ObjectEvents(ctx context.Context, namespace string, kind kube.ResourceKind, name string) ([]kube.Event, error)
}

// OpenYAMLFunc pushes tasks/yamlview (8a) for the named object — same shape
// as every other screen's.
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenEventsFunc pushes tasks/events (9b) object-scoped for the named
// object — same shape as objectdetail's/poddetail's.
type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// Config are certchain's dependencies, per repo convention. Mutator backs
// §35c's 'r' force-renew alone — delete stays reachable only from the
// Certificates list (§35b), same as every other detail screen leaving
// delete to browse.
type Config struct {
	Session     *tui.Session
	Lister      resources.RawLister
	Mutator     kube.Mutator
	Events      EventsReader
	OpenYAML    OpenYAMLFunc
	OpenEvents  OpenEventsFunc
	Namespace   string
	Name        string
	LoadTimeout time.Duration
}

// chainNode is one resolved object in the
// Certificate → CertificateRequest → Order → Challenge walk. Depth 0 is the
// Certificate itself; it deepens by one per hop, and drives the tree indent
// the view bakes into the name cell (fluxtree's "└─" recipe — no separate
// tree component).
type chainNode struct {
	Kind      kube.ResourceKind
	Name      string
	Depth     int
	StateText string
	Class     resources.StatusClass
	Glyph     string
	Created   time.Time
	// Message is the node's own verbatim reason/message text (a condition's
	// message, an Order/Challenge's status.reason) — carried here rather
	// than re-read later so buildFailure can quote it without a second pass
	// over the object.
	Message string
	// Detail is a secondary identity line, populated only for a Challenge
	// ("dns-01 · app.nva.dev") — empty for every other kind.
	Detail string
}

// failure is §35a's top band, populated only when some node in the chain is
// genuinely failing — never for a node that is merely still pending, which
// is progress, not failure (the same distinction §30a's fluxStatus draws for
// Reconciling).
type failure struct {
	Kind kube.ResourceKind
	Name string
	// Detail is the failing node's own secondary identity line ("dns-01 ·
	// app.nva.dev" for a Challenge), empty for every other kind.
	Detail  string
	Message string
	Since   time.Time
	// ParentKind/ParentName name the chain's next-shallower node, for the
	// "8m ago · order/web-tls-1-2847563921" reference line — zero when the
	// failing node is the Certificate itself (nothing shallower to name).
	ParentKind kube.ResourceKind
	ParentName string
}

// refInfo is one row of §35a's bottom refs strip: the target Secret (exists
// only, no Ready concept) or the Issuer/ClusterIssuer (exists + its own
// Ready read).
type refInfo struct {
	Label      string // "secret/web-tls-cert", "clusterissuer/letsencrypt-prod"
	Kind       kube.ResourceKind
	Name       string
	Exists     bool
	HasReady   bool // false for a Secret — existence is the whole story
	Ready      bool
	StatusText string
	// External marks an issuerRef into a non-cert-manager group: no Kind,
	// never read, never a jump target, rendered neutral.
	External bool
	// Unknown marks a ref whose cache is still filling or unreadable: its
	// StatusText says why, rendered neutral rather than a red "missing".
	Unknown bool
	pending bool
}

// Model is §35a's screen.
type Model struct {
	session *tui.Session
	lister  resources.RawLister
	mutator kube.Mutator
	actions actions.Controller
	events  EventsReader

	openYAML   OpenYAMLFunc
	openEvents OpenEventsFunc
	timeout    time.Duration

	namespace string
	name      string

	state    tui.TaskState
	feedback string
	conn     kube.ConnState
	spinner  spinner.Model

	fail       *failure
	chain      []chainNode
	secretRef  refInfo
	issuerRef  refInfo
	haveSecret bool
	haveIssuer bool
	attempts   int
	// chainNote flags a chain read whose cache hadn't filled or had stopped
	// filling (Forbidden, failing LIST) — the chain shown may be truncated,
	// and saying so beats a healthy-looking one-node chain.
	chainNote    string
	syncRetryGen int

	// now is the clock the age columns and the failure card's "N ago" render
	// from, refreshed on a tick — never a clock read inside View/Render
	// (CLAUDE.md: render functions are pure).
	now time.Time

	// selected indexes the combined selectable list: chain rows first, then
	// the refs strip's rows (whichever of secretRef/issuerRef resolved) —
	// one cursor, the same shape fluxdetail's single `selected` over
	// inventory uses.
	selected int

	width, height int
}

type loadedMsg struct {
	fail       *failure
	chain      []chainNode
	secretRef  refInfo
	issuerRef  refInfo
	haveSecret bool
	haveIssuer bool
	attempts   int
	gone       bool
	err        error
	// pending is set when a read missed against a cache still filling;
	// applyLoaded schedules a retry, since an object that genuinely isn't
	// there syncs without a change event.
	pending   bool
	chainNote string
}

// New builds the screen, filling defaults for zero values per repo
// convention.
func New(cfg Config) Model {
	timeout := cfg.LoadTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	m := Model{
		session:    cfg.Session,
		lister:     cfg.Lister,
		mutator:    cfg.Mutator,
		actions:    actions.New(cfg.Mutator),
		events:     cfg.Events,
		openYAML:   cfg.OpenYAML,
		openEvents: cfg.OpenEvents,
		namespace:  cfg.Namespace,
		name:       cfg.Name,
		timeout:    timeout,
		state:      tui.TaskStateLoading,
		feedback:   "loading " + cfg.Name + "…",
		spinner:    components.NewSpinner(),
		now:        time.Now(),
	}
	return m
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick, tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// tickMsg refreshes the age columns and the failure card's "N ago" line.
type tickMsg time.Time

func (m *Model) SetSize(w, h int) {
	size := tui.NormalizeSize(w, h)
	m.width, m.height = size.Width, size.Height
}

// selectableCount is how many rows the cursor can land on: the chain plus
// whichever ref rows actually resolved.
func (m Model) selectableCount() int {
	n := len(m.chain)
	if m.haveSecret {
		n++
	}
	if m.haveIssuer {
		n++
	}
	return n
}

// selectedTarget resolves the cursor to a (kind, name) pair for ↵ —
// whichever chain node or ref row is currently selected.
func (m Model) selectedTarget() (kube.ResourceKind, string, bool) {
	if m.selected < 0 {
		return "", "", false
	}
	if m.selected < len(m.chain) {
		n := m.chain[m.selected]
		return n.Kind, n.Name, true
	}
	i := m.selected - len(m.chain)
	if m.haveSecret {
		if i == 0 {
			return m.secretRef.Kind, m.secretRef.Name, true
		}
		i--
	}
	if m.haveIssuer && i == 0 && !m.issuerRef.External {
		return m.issuerRef.Kind, m.issuerRef.Name, true
	}
	return "", "", false
}
