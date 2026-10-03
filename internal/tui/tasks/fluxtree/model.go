// Package fluxtree is §30b (docs/design/README.md): one screen grouped by
// source — every GitRepository/HelmRepository/OCIRepository/Bucket with the
// Kustomizations and HelmReleases it drives nested under it.
//
// The join is the value. Flux's failure mode is chain-shaped (source
// fetches → reconciler applies → workload rolls out) and no kubectl view
// shows the chain; you reconstruct it by reading two lists and holding the
// `sourceRef` of each in your head. That is the same both-ways-join argument
// §23b makes for Gateway API, and the reason this screen exists at all.
//
// It is a *computed view over the registry*, not a second implementation of
// Flux. Every row's glyph, status class, revision cell and condition
// sub-line comes from the kind's own registry descriptor — the exact
// resources.Row §30a's list renders — so the two screens can never disagree
// about an object. All this package adds is the arrangement: which
// reconciler hangs off which source, and whether what a reconciler applied
// is what its source now offers.
//
// Reached by `g "flux"` (tui/goto.go's synthetic KindFluxTree item, present
// only on a cluster that actually serves Flux CRDs — zero chrome
// otherwise), never as a browse list: §30a is the registry baseline for
// per-kind listing and this is the join on top of it.
package fluxtree

import (
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
)

// OpenFluxDetailFunc pushes tasks/fluxdetail (§31a) for a reconciler —
// §30b's "↵ inventory". Same shape as browse's own OpenFluxDetailFunc,
// declared here per the repo's package-local-seam convention.
type OpenFluxDetailFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenObjectDetailFunc pushes tasks/objectdetail (§14d) for a *source* row.
// A source publishes an artifact, not a set of applied objects, so §31a's
// inventory-shaped screen has no question to answer about one — 14d's
// generic CRD detail does, and it is what browse's own ↵ would open for the
// same object. browse's OpenObjectDetailFunc minus the sibling list: a
// source opened from the tree has no list of siblings to step through.
type OpenObjectDetailFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// Config are fluxtree's dependencies, per repo convention.
type Config struct {
	Session          *tui.Session
	Lister           resources.RawLister
	Mutator          kube.Mutator
	OpenFluxDetail   OpenFluxDetailFunc
	OpenObjectDetail OpenObjectDetailFunc
	LoadTimeout      time.Duration
}

// treeRow is one object in the chain — a source or a reconciler. Everything
// display-facing on it is copied from the kind descriptor's own projection
// (resources.Row), so §30a and §30b render the same object identically.
type treeRow struct {
	kind      kube.ResourceKind // the *registry* kind (FluxHelmRelease for Flux's own HelmRelease)
	kindLabel string            // the KIND cell: "GitRepo", "Kustomize", …
	namespace string
	name      string

	glyph    string
	class    resources.StatusClass
	revision string // the REVISION cell, already assembled
	// revisionKey is the *unshortened* revision the drift comparison runs
	// on — a source's artifact revision, a reconciler's applied one. Never
	// rendered; the shortened form in revision is lossy by design.
	revisionKey string
	reconciled  string // the RECONCILED cell ("4m ago")
	subLine     string // the Ready condition message, verbatim; "" when there's nothing to say
	// applies summarises where a Kustomization's inventory landed —
	// "applies 58 objects · gitlab 12 · monitoring 20 · +3 namespaces" — so
	// a push that only added manifests to an existing Kustomization is
	// still visible on the tree. "" for anything without an inventory.
	applies   string
	suspended bool

	isSource bool
	// sourceKind/sourceName/sourceNamespace name what a reconciler reconciles
	// *from* — the join key, and the target of both 'o' and the source half
	// of a with-source reconcile. Empty on a source row.
	sourceKind      kube.ResourceKind
	sourceName      string
	sourceNamespace string
	// missing marks a synthetic group head standing in for a sourceRef that
	// resolved to nothing. It is a real finding (a reconciler pointed at a
	// source that isn't there), not a placeholder, but it has no object
	// behind it — so it is never selectable and carries no verbs.
	missing bool
}

// group is one source and everything it drives.
type group struct {
	head     treeRow
	children []treeRow
}

// healthy reports whether the whole chain is fine — the test for whether
// this group folds away behind §30b's summary line. A suspended reconciler
// is deliberately *not* healthy: it is the object silently drifting from
// git, which is the one thing this screen exists to surface.
func (g group) healthy() bool {
	if g.head.missing || g.head.class != resources.StatusOK {
		return false
	}
	for _, c := range g.children {
		if c.class != resources.StatusOK {
			return false
		}
	}
	return true
}

// lineKind distinguishes the three things a rendered line can be, so
// selection can skip the two that aren't objects.
type lineKind int

const (
	lineRow  lineKind = iota // a source or reconciler row — selectable
	lineSub                  // a condition message under its row
	lineFold                 // the trailing "+ N sources · M reconcilers ready"
)

type line struct {
	kind  lineKind
	row   int // index into Model.rows; -1 for sub/fold lines
	text  string
	child bool // a reconciler nested under its source
}

type Model struct {
	width, height int

	session          *tui.Session
	lister           resources.RawLister
	mutator          kube.Mutator
	actionsCtl       actions.Controller
	openFluxDetail   OpenFluxDetailFunc
	openObjectDetail OpenObjectDetailFunc
	timeout          time.Duration

	groups []group
	// rows is every selectable object, in display order; lines is what
	// renders, including the sub-lines and the fold tail that rows has no
	// entry for.
	rows  []treeRow
	lines []line

	// expanded reveals the healthy groups the fold hides. Same 'tab' idiom
	// as §30a's healthy-tail fold.
	expanded bool
	// foldedSources/foldedReconcilers back the fold line's own text.
	foldedSources, foldedReconcilers int

	selected, offset int

	// reloadEpoch guards a cache-sync retry against a newer load. loadSeq
	// numbers every load issued (startLoad) and appliedSeq is the newest one
	// applied, so a slow watch-triggered join landing late can never put an
	// older tree back on screen.
	reloadEpoch int
	loadSeq     int
	appliedSeq  int
	// kindErrs are the Flux kinds the last load couldn't read, named on the
	// strip so a partial tree never passes for the whole one.
	kindErrs []kindError

	conn kube.ConnState
	now  time.Time

	state    tui.TaskState
	feedback string
	// execFeedback is the will-run line for a TierNone verb — there is no
	// confirm to hang it on, so it is set the moment the key lands (browse's
	// own execFeedback, same reasoning).
	execFeedback string
	spinner      spinner.Model

	loadStartedAt time.Time
}

// loadedMsg carries one load()'s result.
type loadedMsg struct {
	seq    int
	groups []group
	// kinds are the Flux kinds this load actually read, so the empty state
	// can gate on their caches rather than claiming a cluster has no Flux
	// objects while the informers are still filling.
	kinds []kube.ResourceKind
	// kindErrs are the kinds that couldn't be read — dropped from the join
	// rather than failing it. err is set only when every kind failed.
	kindErrs []kindError
	err      error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading Flux tree..."
	if cfg.Lister == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:            tui.DefaultWidth,
		height:           tui.DefaultHeight,
		session:          cfg.Session,
		lister:           cfg.Lister,
		mutator:          cfg.Mutator,
		actionsCtl:       actions.New(cfg.Mutator),
		openFluxDetail:   cfg.OpenFluxDetail,
		openObjectDetail: cfg.OpenObjectDetail,
		timeout:          cfg.LoadTimeout,
		state:            state,
		feedback:         feedback,
		now:              time.Now(),
		loadStartedAt:    time.Now(),
		spinner:          components.NewSpinner(),
		loadSeq:          1,
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
	m.clampOffset()
}

// selectedRow is the object under the cursor, if the cursor is on one.
func (m Model) selectedRow() (treeRow, bool) {
	if m.selected < 0 || m.selected >= len(m.lines) {
		return treeRow{}, false
	}
	l := m.lines[m.selected]
	if l.kind != lineRow || l.row < 0 || l.row >= len(m.rows) {
		return treeRow{}, false
	}
	return m.rows[l.row], true
}

// contextName is the breadcrumb's cluster segment.
func (m Model) contextName() string {
	if m.session != nil && m.session.Location.Context != "" {
		return m.session.Location.Context
	}
	return "cluster unavailable"
}

// countRows tallies what the strip reports.
func (m Model) countRows() (sources, reconcilers int) {
	for _, g := range m.groups {
		if !g.head.missing {
			sources++
		}
		reconcilers += len(g.children)
	}
	return sources, reconcilers
}

// health tallies every reconciler for the strip, which is the number an
// operator scans first. Sources are counted separately: a broken source is
// reported by the group it heads.
func (m Model) health() resources.HealthCounts {
	var counts resources.HealthCounts
	for _, g := range m.groups {
		for _, c := range g.children {
			switch c.class {
			case resources.StatusOK:
				counts.OK++
			case resources.StatusWarn:
				counts.Warn++
			case resources.StatusFail:
				counts.Fail++
			default:
				counts.Neutral++
			}
		}
	}
	return counts
}

// isProd reports whether the active context is tagged PROD — the input
// actions.Controller.Begin resolves this screen's verbs' confirm policy with.
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}
