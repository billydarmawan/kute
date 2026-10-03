// Package overview is 19a (docs/design/README.md §19a): the cluster
// overview — cluster-scoped (namespace drops from the breadcrumb, like
// Nodes/CRDs/WhoCan) and reached only via the goto corpus (`g "ov"`,
// tui/goto.go's gotoOverviewItem). "Not the start screen" — tasks/browse's
// pods table remains the resting state; this is a routing layer, not a
// dashboard: CAPACITY (cluster cpu/mem/pods bars, no selection of its own)
// and NODES (pressure/cordoned first, "+N ready" collapse) sit in the left
// column; TROUBLE (cluster-wide unhealthy-first pod aggregation) and RECENT
// CHANGES (the timeline's rollout feed, cluster-wide, 30m) sit in the
// right. `↹` cycles focus between the three selectable panels (NODES/
// TROUBLE/CHANGES — CAPACITY is facts only); every focused row's ↵ lands on
// an existing screen (11b node detail, or a jump to the object via
// tui.GotoResource, the same navigation tasks/timeline's own
// openSelectedObject and the root shell's 'g' palette use — one esc back to
// this overview screen, per model.go's routeGoto). `t`/`e` escape to the
// full cluster-wide timeline/
// events screens (namespace "" — the same all-namespaces convention
// tasks/timeline and tasks/events already use).
package overview

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

// changesWindow is 19a's fixed "last 30m" RECENT CHANGES window (docs/design
// README.md §19a). Unlike tasks/timeline's own `t` window cycle (16a's
// windowSteps), this screen shows one fixed window — `t` escapes to the full
// timeline screen for anything wider.
const changesWindow = 30 * time.Minute

// NodeMetricsReader is the live node-usage seam CAPACITY needs for its cpu/
// mem bars — the same interface tasks/browse declares for 11a's own bars,
// satisfied by *kube.Cluster and *kube/fake.Cluster already.
type NodeMetricsReader interface {
	NodeMetrics(ctx context.Context) (map[string]kube.NodeMetric, error)
}

// OpenNodeDetailFunc pushes tasks/nodedetail (11b) for a NODES row — same
// shape as browse's own OpenNodeDetailFunc.
type OpenNodeDetailFunc func(nodeName string, width, height int) (tea.Model, tea.Cmd)

// OpenTimelineFunc pushes tasks/timeline (16a), cluster-wide (namespace "").
type OpenTimelineFunc func(namespace string, width, height int) (tea.Model, tea.Cmd)

// OpenEventsFunc pushes tasks/events (9b), cluster-wide (namespace "").
type OpenEventsFunc func(namespace string, width, height int) (tea.Model, tea.Cmd)

// Config are overview's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values).
type Config struct {
	Session        *tui.Session
	Lister         resources.RawLister
	NodeMetrics    NodeMetricsReader
	OpenNodeDetail OpenNodeDetailFunc
	OpenTimeline   OpenTimelineFunc
	OpenEvents     OpenEventsFunc
	LoadTimeout    time.Duration
}

// panel identifies one of 19a's three selectable panels — CAPACITY carries
// no selection of its own, so it isn't one of these.
type panel int

const (
	panelNodes panel = iota
	panelTrouble
	panelChanges
)

type Model struct {
	width, height int

	session        *tui.Session
	lister         resources.RawLister
	nodeMetrics    NodeMetricsReader
	openNodeDetail OpenNodeDetailFunc
	openTimeline   OpenTimelineFunc
	openEvents     OpenEventsFunc
	timeout        time.Duration

	focus panel

	version   string
	nodeCount int
	podCount  int
	nsCount   int
	// nsDenied/helmDenied/changesDenied are true when the corresponding
	// best-effort read (Namespace/HelmRelease/ReplicaSet — loadOverview's
	// own "nothing here fails the screen" reads) came back Forbidden on the
	// most recent load. Unlike Node/Pod (the two required caches, gated in
	// applyLoaded), a denial on these must not take over the whole screen —
	// each flags only the fact/panel it backs
	// (docs/lazy-informers.md §5.6).
	//
	// nsPending/helmPending/changesPending are true when the corresponding
	// cache hasn't finished its initial fill yet (or has stalled on a
	// non-permission error) — loadOverview's own reads swallow every error
	// for these three, so without this flag a merely-still-filling cache
	// reads no differently from "the cluster truly has none of these",
	// which is the same false zero/empty claim the Denied flags exist to
	// prevent, just for the transient case instead of the permanent one.
	nsDenied       bool
	nsPending      bool
	helmDenied     bool
	helmPending    bool
	changesDenied  bool
	changesPending bool

	metricsAvailable bool
	// metricsErr is why the last poll failed, nil when it answered — it
	// picks CAPACITY's note when there's no reading to draw.
	metricsErr error
	// nodeUsage is the last poll's reading for every node that reported
	// one, nodeCapacity the last load's per-node capacity; clusterUsage
	// joins them, so CAPACITY's cpu/mem only ever divide usage by the
	// capacity of the nodes that usage came from.
	nodeUsage                 map[string]kube.NodeMetric
	nodeCapacity              map[string]kube.NodeResources
	capPodsUsed, capPodsTotal int64

	// nodeTrouble/podTrouble are the unhealthy (Warn/Fail, or cordoned for
	// nodes) rows only — the healthy remainder is just a count, per the
	// "+N ready"/"nothing unhealthy" collapse idiom.
	nodeTrouble []resources.Row
	nodeHealthy int
	nodesSel    int

	podTrouble []resources.Row
	podHealthy int
	troubleSel int
	// helmOutdated are 18a's outdated releases, shown at the *end* of the
	// TROUBLE panel: a chart behind its repo is the least urgent thing in
	// there, and unlike everything above it, it isn't a health problem at
	// all — which is why it rides in its own slice rather than being mixed
	// into podTrouble.
	helmOutdated []resources.Row

	changes    []kube.TimelineEntry
	changesSel int

	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's real connection state, same as every other screen.
	conn kube.ConnState

	// reloadEpoch gates applyLoaded's own still-syncing retry
	// (tui.CacheSyncRetryMsg). loadSeq numbers every load issued and
	// appliedSeq is the newest one applied, so replies landing out of order
	// can never put an older snapshot back on screen. reloadPending is set
	// while a debounced watch-event reload is armed.
	reloadEpoch   int
	loadSeq       int
	appliedSeq    int
	reloadPending bool

	// metricsEpoch identifies the live node-usage poll chain (loadMetrics);
	// a tick or reply from a retired chain is dropped, so exactly one loop
	// runs. metricsPolled is set once a poll has answered, so CAPACITY
	// doesn't claim "no metrics-server" while the first is in flight.
	metricsEpoch  int
	metricsPolled bool

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result.
type loadedMsg struct {
	seq  int
	data loadedData
	err  error
}

// metricsTickMsg drives the node-usage poll — CAPACITY's cpu/mem only
// refresh, since usage emits no watch event (CLAUDE.md: "metrics poll on the
// sync interval only").
type metricsTickMsg struct{ epoch int }

// metricsLoadedMsg carries one poll's per-node usage — only the nodes
// that reported a real reading.
type metricsLoadedMsg struct {
	epoch int
	usage map[string]kube.NodeMetric
	err   error
}

// loadedData is every field load() computes in one pass — applied onto
// Model atomically by applyLoaded so a render never sees a half-updated
// screen.
type loadedData struct {
	version   string
	nodeCount int
	podCount  int
	nsCount   int

	nodeCapacity              map[string]kube.NodeResources
	capPodsUsed, capPodsTotal int64

	nodeTrouble []resources.Row
	nodeHealthy int

	podTrouble []resources.Row
	podHealthy int

	helmOutdated []resources.Row

	changes []kube.TimelineEntry
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading cluster overview..."
	if cfg.Lister == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:          tui.DefaultWidth,
		height:         tui.DefaultHeight,
		session:        cfg.Session,
		lister:         cfg.Lister,
		nodeMetrics:    cfg.NodeMetrics,
		openNodeDetail: cfg.OpenNodeDetail,
		openTimeline:   cfg.OpenTimeline,
		openEvents:     cfg.OpenEvents,
		timeout:        cfg.LoadTimeout,
		state:          state,
		feedback:       feedback,
		spinner:        components.NewSpinner(),
		loadSeq:        1,
		metricsEpoch:   1,
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	// Init runs on a value copy, so it loads and polls on New's seq and
	// chain (both 1) directly rather than through the pointer helpers.
	return tea.Batch(m.load(), m.spinner.Tick, m.loadMetrics())
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

func (m Model) selectedNode() (resources.Row, bool) {
	if m.nodesSel < 0 || m.nodesSel >= len(m.nodeTrouble) {
		return resources.Row{}, false
	}
	return m.nodeTrouble[m.nodesSel], true
}

// troubleEntry is one TROUBLE row plus the kind it belongs to. The panel
// aggregates two kinds now, and `↵` has to land on the right screen for each
// — a hardcoded KindPod would send an outdated release to the pods table.
type troubleEntry struct {
	row  resources.Row
	kind kube.ResourceKind
}

// troubleEntries is the panel's full row list in display order: unhealthy
// pods first (already sorted Fail-then-Warn by load), outdated releases
// after them.
func (m Model) troubleEntries() []troubleEntry {
	out := make([]troubleEntry, 0, len(m.podTrouble)+len(m.helmOutdated))
	for _, r := range m.podTrouble {
		out = append(out, troubleEntry{row: r, kind: kube.KindPod})
	}
	for _, r := range m.helmOutdated {
		out = append(out, troubleEntry{row: r, kind: kube.KindHelmRelease})
	}
	return out
}

func (m Model) selectedTrouble() (troubleEntry, bool) {
	entries := m.troubleEntries()
	if m.troubleSel < 0 || m.troubleSel >= len(entries) {
		return troubleEntry{}, false
	}
	return entries[m.troubleSel], true
}

func (m Model) selectedChange() (kube.TimelineEntry, bool) {
	if m.changesSel < 0 || m.changesSel >= len(m.changes) {
		return kube.TimelineEntry{}, false
	}
	return m.changes[m.changesSel], true
}
