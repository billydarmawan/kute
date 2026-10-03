// Package nodedetail is 11b (docs/design/README.md §11b): a node's
// CONDITIONS/ALLOCATED-ALLOCATABLE/TAINTS facts panel over the node's own
// pods table (sorted unhealthy-first then name, the same order 2a's own
// Pods list uses). Reached from tasks/browse's Nodes list on 'enter'.
//
// Pod-row 'enter' opens the same log-stream screen browse's 'l' verb does
// (tasks/poddetail doesn't exist yet — Phase 5) rather than a stub; see the
// package doc in mvp-tasks.md's Phase 9 exit notes for the scope call.
package nodedetail

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	corev1 "k8s.io/api/core/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/metapanel"
)

// MetricsReader is the live pod-usage seam nodedetail needs for the bottom
// pane's MEM/CPU columns — same shape as browse.MetricsReader, duplicated
// per the repo's package-local-seam convention (CLAUDE.md: "define the
// interface you need in the task package").
type MetricsReader interface {
	PodMetricsByNamespace(ctx context.Context, namespace string) (map[string]kube.PodMetrics, error)
}

// NodeMetricsReader is the live node-usage seam 11b's USED / CAPACITY block
// needs — the same shape as browse.NodeMetricsReader, duplicated per the
// repo's package-local-seam convention. It is deliberately separate from
// MetricsReader above: that one answers for the pods in the bottom table,
// this one for the node itself, and the two are different numbers (a node's
// usage includes kubelet and the container runtime, which belong to no pod).
type NodeMetricsReader interface {
	NodeMetrics(ctx context.Context) (map[string]kube.NodeMetric, error)
}

// NodeDiskReader is the seam behind USED / CAPACITY's disk row: the node's
// root filesystem as the kubelet reports it. metrics-server has no disk
// figure, so this is a separate read with its own slower cadence
// (diskPollEvery) — kept off NodeMetricsReader so a screen that only wants
// cpu/mem never pays for it.
type NodeDiskReader interface {
	NodeDiskUsage(ctx context.Context, nodeName string) (kube.NodeDisk, error)
}

// diskState is where the disk row stands. It is its own tri-state rather
// than a usedOK-style flag because "not read yet", "you may not read it" and
// "the kubelet didn't answer" each say something different on screen.
type diskState int

const (
	diskPending diskState = iota
	diskOK
	diskForbidden
	diskUnavailable
)

// OpenPodFunc pushes a view for the named pod — nodedetail hands it the same
// kube.Pod/width/height shape browse.OpenLogsFunc does, so app.go can wire
// both from the same closure.
type OpenPodFunc func(pod kube.Pod, width, height int) (tea.Model, tea.Cmd)

// OpenLogsFunc pushes the log-stream screen for pod — same shape as
// browse.OpenLogsFunc, so app.go can wire both from the same closure.
// nodedetail has no per-container selection of its own, so it always
// passes "" for container (falls back to index 0).
type OpenLogsFunc func(pod kube.Pod, container string, width, height int) (tea.Model, tea.Cmd)

// OpenExecFunc pushes tasks/execpicker (10a) for a pod row with more than one
// container — same shape as browse.OpenExecFunc, duplicated per the repo's
// package-local-seam convention. A single container execs immediately via
// kube.ExecSpec without pushing a task.
type OpenExecFunc func(namespace, name string, containers []kube.ContainerInfo, width, height int) (tea.Model, tea.Cmd)

// OpenDebugFunc pushes tasks/debugpanel (§41c) for a selected pod row that
// is not running. podPhase is the API phase, not the display reason.
type OpenDebugFunc func(namespace, name string, containers []kube.ContainerInfo, podPhase string, waiting bool, width, height int) (tea.Model, tea.Cmd)

// OpenYAMLFunc pushes tasks/yamlview (8a) for the named object — same shape
// as browse.OpenYAMLFunc/poddetail.OpenYAMLFunc.
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenEventsFunc pushes tasks/events (9b) object-scoped for the loaded node
// — same shape as poddetail.OpenEventsFunc.
type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenTimelineFunc pushes tasks/timeline (16b) object-scoped for the loaded
// node (docs/design README.md §16b: "object-scoped from detail") — same
// shape as poddetail.OpenTimelineFunc.
type OpenTimelineFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenForwardFunc pushes tasks/forwardpicker (13a) for a selected pod row —
// same shape as browse.OpenForwardFunc. The spec lists 'f' alongside 'x'/'y'
// as available "on any object row" (docs/design README.md §304, §308);
// browse already wires it for Pod rows, this closes the gap on a node's own
// pod rows.
type OpenForwardFunc func(target kube.ForwardTarget, width, height int) (tea.Model, tea.Cmd)

// OpenNodeDebugFunc pushes tasks/debugpanel (§41d) for the loaded node —
// same shape as browse.OpenNodeDebugFunc, duplicated per the repo's
// package-local-seam convention. Bound to 's' here rather than NodeDebug's
// 'x' (verbs.NodeDebugDetail's own doc comment explains why: this screen's
// pods table already claims 'x' for Exec on a pod row, a different object).
type OpenNodeDebugFunc func(name string, podCount, width, height int) (tea.Model, tea.Cmd)

// Config are nodedetail's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values).
type Config struct {
	Session       *tui.Session
	Lister        resources.RawLister
	Metrics       MetricsReader
	NodeMetrics   NodeMetricsReader
	NodeDisk      NodeDiskReader
	Mutator       kube.Mutator
	OpenPod       OpenPodFunc
	OpenLogs      OpenLogsFunc
	OpenExec      OpenExecFunc
	OpenDebug     OpenDebugFunc
	OpenYAML      OpenYAMLFunc
	OpenEvents    OpenEventsFunc
	OpenTimeline  OpenTimelineFunc
	OpenForward   OpenForwardFunc
	OpenNodeDebug OpenNodeDebugFunc
	NodeName      string
	LoadTimeout   time.Duration
}

// allocation is one CPU/MEM/Pods triple — used for the node/allocatable
// capacity and the node's pods' summed requests.
type allocation struct {
	cpuMilli, memBytes, pods int64
}

// nodePodRow is one bottom-pane row: pod carries what the row's actions
// (openPod/openLogs/openExec/openForward) key off; row is the same
// resources.Row the Pod descriptor's Project func produces for browse's own
// Pods list (Name/Namespace/Ready/Status/Restarts/CPU/MEM-placeholder/Node/
// Age cells, Status/Glyph/GlyphClass) — reusing it is what gives this
// screen's pods table the same kubectl-style status fidelity (Init:,
// CrashLoopBackOff, OOMKilled, Terminating…) browse's table already has.
type nodePodRow struct {
	pod kube.Pod
	row resources.Row
}

type Model struct {
	width, height int

	session       *tui.Session
	lister        resources.RawLister
	metrics       MetricsReader
	nodeMetrics   NodeMetricsReader
	nodeDisk      NodeDiskReader
	mutator       kube.Mutator
	actions       actions.Controller
	openPod       OpenPodFunc
	openLogs      OpenLogsFunc
	openExec      OpenExecFunc
	openDebug     OpenDebugFunc
	openYAML      OpenYAMLFunc
	openEvents    OpenEventsFunc
	openTimeline  OpenTimelineFunc
	openForward   OpenForwardFunc
	openNodeDebug OpenNodeDebugFunc
	timeout       time.Duration

	nodeName string

	node        *corev1.Node
	allocated   allocation
	allocatable allocation
	// used/capacity back the USED / CAPACITY block: live metrics-server
	// usage for this node over its full machine size. Distinct from
	// allocated/allocatable above, which are the pods' summed requests over
	// what the scheduler may hand out — the two answer different questions,
	// and conflating them is why 11a and 11b used to disagree about a node's
	// memory. usedOK is false when no reading has landed, in which case the
	// block says so rather than drawing a flatlined zero bar.
	used     allocation
	capacity allocation
	usedOK   bool
	// disk/diskState back USED / CAPACITY's disk row (NodeDiskReader).
	// diskTicks counts usage-poll ticks since the last disk read, so the
	// disk read rides the existing poll chain at a fraction of its rate.
	disk      kube.NodeDisk
	diskState diskState
	diskTicks int
	allPods   []nodePodRow // every pod load() found on this node, before the filter
	// drainImpact is what a drain would do to allPods (kube.DrainImpact)
	// — the drain confirm's consequence line.
	drainImpact kube.DrainImpact
	pods        []nodePodRow // allPods after filterQuery — what's selectable/rendered
	selected    int
	offset      int

	filterActive bool
	filterInput  textfield.Model

	// reloadEpoch guards a debounced reload-on-still-syncing retry (see
	// CacheSyncChecker/scheduleReload in load.go) against a stale reply
	// arriving after a newer one's already in flight — mirrors browse's own
	// reloadEpoch.
	reloadEpoch int

	// metricsEpoch identifies the live usage-poll chain. Exactly one tick is
	// ever in flight: Init and Reload arm a chain (bumping this), and each
	// poll's reply arms the next tick of its own chain. It is deliberately
	// not reloadEpoch — a reload must not silently kill the poll loop, and
	// re-entering the screen from the stack must not leave two loops
	// running when the previous chain's tick is still pending in the
	// runtime.
	metricsEpoch int
	// podMetrics is the last poll's pod usage, overlaid on the rows every
	// reload rebuilds — load() itself never reads metrics-server.
	podMetrics map[string]kube.PodMetrics
	// usagePolled is set once any usage poll has answered, so USED doesn't
	// claim "no metrics-server" while the first one is still in flight.
	usagePolled bool

	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's real connection state (never a hardcoded "connected").
	conn kube.ConnState
	// now is the wall-clock time as of the last SpinnerTickMsg/ConnStateMsg —
	// the loading header's "· 0.4s" counting timer is computed from this
	// rather than reading the clock in Render (render must stay pure:
	// f(model, theme, size)), mirroring browse's own now field.
	now time.Time

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
	// loadStartedAt is when this screen's one-shot load() began — see now's
	// doc comment (docs/design README.md §15a's loading-state pattern,
	// applied here the same way browse.Model.loadStartedAt is).
	loadStartedAt time.Time

	// execFeedback carries a non-zero kubectl-exec exit's message (docs/design
	// README.md §10a: "feedback line on non-zero exit") — per-call-site
	// transient state, same as browse/poddetail's. Also carries kubectl
	// edit's exit message (editResultMsg) — same channel, same reasoning.
	execFeedback string
	// pendingEdit is non-nil while 'E' edit's PROD-only y/N line is showing
	// (verbs.TierForEdit) — mirrors browse.Model's own pendingEdit field.
	pendingEdit *editTarget
	// meta is non-nil while 26a's labels/annotations panel is open on this
	// node (the shared internal/tui/metapanel editor) — the only place a
	// node's labels and annotations are visible and editable from 11b, since
	// the facts panel shows conditions/allocation/taints but never metadata.
	// Mirrors poddetail's hosting of the same panel.
	meta *metapanel.Model
}

// loadedMsg carries one load()'s result: the node itself plus its pods
// (already sorted unhealthy-first then name) and their summed requests.
type loadedMsg struct {
	node        *corev1.Node
	allocated   allocation
	allocatable allocation
	capacity    allocation
	pods        []nodePodRow
	drainImpact kube.DrainImpact
	err         error
}

// metricsTickMsg drives the 2s usage refresh — 11b polls metrics the same
// way browse does (CLAUDE.md sanctions "metrics poll on the sync interval
// only"), because everything else here is watch-driven and a node's usage
// emits no watch event. Without it the USED block would freeze at whatever
// the last Node/Pod change left behind and drift away from the list the
// user just came from, which is the disagreement this block exists to end.
type metricsTickMsg struct{ epoch int }

// metricsLoadedMsg carries one poll's node + pod usage. epoch guards a
// stale reply the same way reloadEpoch guards a stale reload.
// Capacity is deliberately absent: it comes off the Node object and only
// changes when the node itself does, which is a watch event and so a full
// reload.
type metricsLoadedMsg struct {
	epoch      int
	used       allocation
	usedOK     bool
	podMetrics map[string]kube.PodMetrics
}

// diskLoadedMsg carries one NodeDiskReader read.
type diskLoadedMsg struct {
	disk kube.NodeDisk
	err  error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading " + cfg.NodeName + "..."
	if cfg.Lister == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:         tui.DefaultWidth,
		height:        tui.DefaultHeight,
		session:       cfg.Session,
		lister:        cfg.Lister,
		metrics:       cfg.Metrics,
		nodeMetrics:   cfg.NodeMetrics,
		nodeDisk:      cfg.NodeDisk,
		mutator:       cfg.Mutator,
		actions:       actions.New(cfg.Mutator),
		openPod:       cfg.OpenPod,
		openLogs:      cfg.OpenLogs,
		openExec:      cfg.OpenExec,
		openDebug:     cfg.OpenDebug,
		openYAML:      cfg.OpenYAML,
		openEvents:    cfg.OpenEvents,
		openTimeline:  cfg.OpenTimeline,
		openForward:   cfg.OpenForward,
		openNodeDebug: cfg.OpenNodeDebug,
		timeout:       cfg.LoadTimeout,
		nodeName:      cfg.NodeName,
		state:         state,
		feedback:      feedback,
		now:           time.Now(),
		loadStartedAt: time.Now(),
		spinner:       components.NewSpinner(),
		metricsEpoch:  1,
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	// The usage poll starts once the first load lands (applyLoaded): it
	// reads pod usage only for the namespaces this node's pods live in,
	// which aren't known until then.
	return tea.Batch(m.load(), m.spinner.Tick, m.loadDisk())
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

func (m Model) selectedPod() (nodePodRow, bool) {
	if m.selected < 0 || m.selected >= len(m.pods) {
		return nodePodRow{}, false
	}
	return m.pods[m.selected], true
}
