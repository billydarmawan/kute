// Package fluxdetail is §31a (docs/design README.md): the Flux reconciler
// detail view reached from browse's ↵ on a Kustomization or HelmRelease row.
//
// It exists as its own screen rather than as branches inside 14d because
// 14d's contract is "built only from what every object has — no CRD-specific
// layout code, ever", and §31a is entirely CRD-specific: a failure card that
// has followed the failing health check to the workload behind it, the
// source→revision chain, and the inventory of what this reconciler manages.
// Putting any of that in 14d would break the invariant that makes 14d work
// for every discovered kind on earth.
//
// Band order is 5a's promotion rule: why it is broken first, what it is
// wired to second, what it manages third.
package fluxdetail

import (
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

// OpenYAMLFunc pushes tasks/yamlview (8a) for the named object — same shape
// as objectdetail's, duplicated per the repo's package-local-seam convention.
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// Config are fluxdetail's dependencies, per repo convention.
type Config struct {
	Session     *tui.Session
	Lister      resources.RawLister
	Mutator     kube.Mutator
	OpenYAML    OpenYAMLFunc
	Kind        kube.ResourceKind
	Namespace   string
	Name        string
	LoadTimeout time.Duration
}

// inventoryItem is one object this reconciler manages, resolved from
// status.inventory against the caches.
type inventoryItem struct {
	Kind      kube.ResourceKind
	Namespace string
	Name      string
	Ready     string
	Status    resources.StatusClass
	Glyph     string
	// obj is the cached object behind the row, when the caches had it —
	// what the failure card's drill-through follows to the pods.
	obj runtime.Object
}

// chain is §31a's middle band: what this object is wired to.
type chain struct {
	SourceKind      kube.ResourceKind
	SourceName      string
	SourceNamespace string // the ref's own namespace, else the object's
	SourceDisplay   string // "git/nebula-config", "helm/gitlab in flux-system"
	Path            string
	Interval        string
	SourceRevision  string // the source's own artifact revision, shortened
	AppliedRevision string // what this reconciler last applied, shortened
	// DriftComparable reports whether the two revisions above describe the
	// same thing, and so whether their difference means anything. False for
	// a HelmRelease, whose applied revision is a chart version while its
	// source publishes an index digest — see kube.FluxTracksSourceRevision.
	DriftComparable  bool
	DependsOn        []dependency
	InventoryMissing string // why there is no inventory, when there isn't
}

// dependency is one spec.dependsOn entry plus whether it is suspended —
// §31a: "a suspended dependency is the second most common 'why won't it
// reconcile'".
type dependency struct {
	Name      string
	Namespace string // the entry's own namespace, else the object's
	// CrossNamespace marks an entry naming a namespace other than the
	// object's, so the chain says where it lives.
	CrossNamespace bool
	Suspended      bool
}

// failure is §31a's top band, populated only when the object is not ready.
type failure struct {
	Reason  string
	Message string // the condition message, verbatim
	Since   time.Time
	RetryAt time.Time // zero when no retry interval could be resolved
	// RetryEvery is the interval Flux keeps retrying on after RetryAt.
	// Ready's lastTransitionTime doesn't move while it stays False, so past
	// the first attempt the next one can only be projected from it.
	RetryEvery time.Duration
	// Culprit names the inventory object the failure resolved to, and
	// Detail what was found on it: the workload's replica count, then the
	// real reason its worst pod gives ("3/4 · CrashLoopBackOff — exit 137,
	// OOMKilled", drillToPod). Both empty when the join found nothing, in
	// which case the card shows the condition message alone rather than
	// inventing a lead.
	Culprit kube.ResourceKind
	CulpritName,
	CulpritNamespace,
	Detail string
}

// Model is §31a's screen.
type Model struct {
	session *tui.Session
	lister  resources.RawLister
	mutator kube.Mutator
	desc    resources.Descriptor

	kind      kube.ResourceKind
	namespace string
	name      string

	state    tui.TaskState
	feedback string
	conn     kube.ConnState
	spinner  spinner.Model
	actions  actions.Controller
	openYAML OpenYAMLFunc
	timeout  time.Duration

	// suspended/statusText come from the same projection the list uses, so
	// the two screens can never disagree about what an object's state is.
	suspended  bool
	statusText string

	fail      *failure
	chn       chain
	inventory []inventoryItem
	// invExpanded controls §31a's healthy-tail fold, same idiom as 30a's.
	invExpanded bool
	selected    int
	offset      int

	// loadEpoch guards a cache-sync retry against a newer load.
	loadEpoch int

	width, height int
	// now is the clock read the retry countdown renders from, refreshed on
	// a tick rather than read inside View — render functions are pure.
	now time.Time
}

type loadedMsg struct {
	fail       *failure
	chn        chain
	inventory  []inventoryItem
	suspended  bool
	statusText string
	gone       bool
	// pending: the object wasn't found, but its kind's cache hasn't filled,
	// so its absence proves nothing yet.
	pending bool
	// invPending: a cache behind the inventory or the source revision is
	// still filling — the screen renders, and reloads once it settles.
	invPending bool
	err        error
}

// tickMsg refreshes the retry countdown.
type tickMsg time.Time

// New builds the screen, filling defaults for zero values per repo
// convention.
func New(cfg Config) Model {
	timeout := cfg.LoadTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	m := Model{
		session:   cfg.Session,
		lister:    cfg.Lister,
		mutator:   cfg.Mutator,
		openYAML:  cfg.OpenYAML,
		kind:      cfg.Kind,
		namespace: cfg.Namespace,
		name:      cfg.Name,
		timeout:   timeout,
		state:     tui.TaskStateLoading,
		feedback:  "loading " + cfg.Name + "…",
		spinner:   sp,
		now:       time.Now(),
	}
	if cfg.Session != nil {
		if d, ok := cfg.Session.Registry.Descriptor(cfg.Kind); ok {
			m.desc = d
		}
	}
	m.actions = actions.New(cfg.Mutator)
	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.load(), m.spinner.Tick, tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) SetSize(w, h int) { m.width, m.height = w, h }

// No isProd here, deliberately: §30a's two verbs are both TierNone and do
// not escalate in a PROD context (suspend is reversible, reconcile changes
// nothing the controller wasn't about to change anyway), so there is no
// tier for a prod tag to raise. A future destructive Flux verb would need
// TierFor and this read back.

// offline mirrors the shared connection gate.
func (m Model) offline() bool { return m.conn.Offline() }

// selectedItem is the inventory cursor's target for ↵.
func (m Model) selectedItem() (inventoryItem, bool) {
	visible := m.visibleInventory()
	if m.selected < 0 || m.selected >= len(visible) {
		return inventoryItem{}, false
	}
	return visible[m.selected], true
}

// visibleInventory applies §31a's healthy-tail fold: unhealthy entries
// always show, the rest collapse behind a "+ N ready" line until expanded.
func (m Model) visibleInventory() []inventoryItem {
	if m.invExpanded {
		return m.inventory
	}
	unhealthy := 0
	for unhealthy < len(m.inventory) && isUnhealthy(m.inventory[unhealthy].Status) {
		unhealthy++
	}
	if unhealthy == 0 || unhealthy == len(m.inventory) {
		return m.inventory
	}
	return m.inventory[:unhealthy]
}

// foldedCount is how many healthy inventory entries the fold hides.
func (m Model) foldedCount() int {
	return len(m.inventory) - len(m.visibleInventory())
}

// clampOffset keeps the selected inventory item inside the rendered viewport.
func (m *Model) clampOffset(rows int) {
	items := len(m.visibleInventory())
	if items == 0 {
		m.selected, m.offset = 0, 0
		return
	}
	if rows < 1 {
		rows = 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= items {
		m.selected = items - 1
	}
	if m.selected < m.offset {
		m.offset = m.selected
	}
	if m.selected >= m.offset+rows {
		m.offset = m.selected - rows + 1
	}
	maxOffset := items - rows
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func isUnhealthy(c resources.StatusClass) bool {
	return c == resources.StatusFail || c == resources.StatusWarn
}
