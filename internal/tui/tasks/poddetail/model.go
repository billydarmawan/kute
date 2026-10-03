// Package poddetail is 5a (docs/design/README.md §5a): a pod's full detail
// view — title/status/restarts, last-termination banner (promoted first
// when present), meta grid, CONTAINERS grid, CPU/MEM bars vs limits,
// EVENTS, and a full-width RELATED │ TOLERATIONS two-column row. Reached from
// tasks/browse's Pods list on 'enter', and from tasks/nodedetail's pod rows
// (mvp-tasks.md Phase 9 exit notes: "swap it for a genuine poddetail push
// once Phase 5 lands").
//
// YAML view ('y', 8a) and the full type-the-name PROD confirm modal (8b)
// aren't built yet — see this package's callers in mvp-tasks.md's Phase 5
// exit notes for the scope calls made here.
package poddetail

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
	"github.com/kute-dev/kute/internal/tui/metapanel"
)

// MetricsReader is the live pod-usage seam poddetail needs for the CPU/MEM
// bars — same shape as browse.MetricsReader/nodedetail.MetricsReader,
// duplicated per the repo's package-local-seam convention.
type MetricsReader interface {
	PodMetricsByNamespace(ctx context.Context, namespace string) (map[string]kube.PodMetrics, error)
}

// EventsReader is the seam for the EVENTS grid — satisfied by *kube.Cluster
// and *fake.Cluster already (kube/events.go, kube/fake/fake.go).
type EventsReader interface {
	ObjectEvents(ctx context.Context, namespace string, kind kube.ResourceKind, name string) ([]kube.Event, error)
}

// OpenLogsFunc pushes the log-stream screen for pod — same shape as
// browse.OpenLogsFunc, so app.go can wire both from the same closure.
// container names which container to start streaming — poddetail passes
// the CONTAINERS grid's selected row so 'l' opens on it rather than always
// index 0.
type OpenLogsFunc func(pod kube.Pod, container string, width, height int) (tea.Model, tea.Cmd)

// OpenYAMLFunc pushes tasks/yamlview (8a) for the named object — same shape
// as browse.OpenYAMLFunc.
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenEventsFunc pushes tasks/events (9b) object-scoped for the loaded pod
// (docs/design README.md §9b: "object-scoped from detail, reusing
// ObjectEvents").
type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenTimelineFunc pushes tasks/timeline (16b) object-scoped for the loaded
// pod (docs/design README.md §16b: "object-scoped from detail").
type OpenTimelineFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenExecFunc pushes tasks/execpicker (10a) for the loaded pod when it has
// more than one container — same shape as browse.OpenExecFunc, duplicated
// per the repo's package-local-seam convention.
//
// probed carries the shells the 'x' pre-check already found per container
// (a successful probe only, possibly empty), so the picker probes just the
// rest instead of re-running every kubectl exec; nil when nothing was probed.
type OpenExecFunc func(namespace, name string, containers []kube.ContainerInfo, probed map[string][]string, width, height int) (tea.Model, tea.Cmd)

// OpenSecretDataFunc pushes tasks/secretdata (27b) for a Secret RELATED
// entry — same shape as browse.OpenSecretDataFunc. A Secret link lands on
// its Data view rather than the Secrets list, since that's what a pod's
// reference is about.
type OpenSecretDataFunc func(namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenConfigMapDataFunc pushes tasks/configmapdata (27a) for a ConfigMap
// RELATED entry — same shape as browse.OpenConfigMapDataFunc.
type OpenConfigMapDataFunc func(namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenFluxDetailFunc pushes tasks/fluxdetail (31a) for a Flux reconciler
// RELATED entry — same shape as browse.OpenFluxDetailFunc.
type OpenFluxDetailFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenForwardFunc pushes tasks/forwardpicker (13a) for the loaded pod — same
// shape as browse.OpenForwardFunc. The spec lists 'f' alongside 'x'/'y' as
// available "on any object row" (docs/design README.md §304, §308); browse
// already wires it for Pod rows, this closes the gap on the pod's own
// detail screen.
type OpenForwardFunc func(target kube.ForwardTarget, width, height int) (tea.Model, tea.Cmd)

// OpenDebugFunc pushes tasks/debugpanel (§41b/§41c) for the loaded pod when
// no container has a shell, or the pod won't stay running — same shape as
// browse.OpenDebugFunc, duplicated per the repo's package-local-seam
// convention. podPhase is the API phase, not the display reason.
type OpenDebugFunc func(namespace, name string, containers []kube.ContainerInfo, podPhase string, waiting bool, width, height int) (tea.Model, tea.Cmd)

// ShellDetector answers which shells a container actually has — same shape
// as browse.ShellDetector, duplicated per the repo's consuming-interface
// convention. A nil value falls back to openSelectedExec's old synchronous
// single-vs-multi-container routing.
type ShellDetector interface {
	DetectShells(ctx context.Context, namespace, pod, container string) ([]string, error)
}

// SiblingRef names one sibling pod for [/] movement — namespace-qualified,
// because all-namespaces mode lists same-named pods side by side (mirrors
// browse.PodSiblingRef, duplicated per the repo's package-local-seam
// convention since task packages can't import one another).
type SiblingRef struct {
	Namespace, Name string
}

// Config are poddetail's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values). Siblings/
// SiblingIndex are the ordered pod-ref list + cursor browse hands over so
// [/] can move to the next/prev pod without leaving detail (docs/design
// README.md §5a).
type Config struct {
	Session      *tui.Session
	Lister       resources.RawLister
	Metrics      MetricsReader
	Events       EventsReader
	Mutator      kube.Mutator
	OpenLogs     OpenLogsFunc
	OpenYAML     OpenYAMLFunc
	OpenEvents   OpenEventsFunc
	OpenTimeline OpenTimelineFunc
	OpenExec     OpenExecFunc
	OpenDebug    OpenDebugFunc
	Shells       ShellDetector
	OpenForward  OpenForwardFunc
	// OpenSecretData/OpenConfigMapData open a Secret/ConfigMap RELATED
	// entry on its Data view; nil falls back to the goto jump every other
	// RELATED entry uses.
	OpenSecretData    OpenSecretDataFunc
	OpenConfigMapData OpenConfigMapDataFunc
	// OpenFluxDetail opens a Flux reconciler RELATED entry on its 31a
	// inventory; nil falls back to the goto jump.
	OpenFluxDetail OpenFluxDetailFunc
	Namespace      string
	Name           string
	Siblings       []SiblingRef
	SiblingIndex   int
	LoadTimeout    time.Duration
	// Demo is true for --demo — see browse.Config.Demo's doc comment;
	// guards execCmd's direct paths ('x' on a single-container pod, ↵ on an
	// ephemeral row) so they never run a real kubectl against a fake pod.
	Demo bool
}

type Model struct {
	width, height int

	session      *tui.Session
	lister       resources.RawLister
	metrics      MetricsReader
	events       EventsReader
	mutator      kube.Mutator
	actions      actions.Controller
	openLogs     OpenLogsFunc
	openYAML     OpenYAMLFunc
	openEvents   OpenEventsFunc
	openTimeline OpenTimelineFunc
	openExec     OpenExecFunc
	openDebug    OpenDebugFunc
	shells       ShellDetector
	openForward  OpenForwardFunc
	openSecret   OpenSecretDataFunc
	openConfig   OpenConfigMapDataFunc
	openFlux     OpenFluxDetailFunc
	timeout      time.Duration
	demo         bool
	// execFeedback carries a non-zero directly-run kubectl-exec exit's
	// message (single-container pods exec straight from poddetail without
	// pushing execpicker) — mirrors browse.Model's own execFeedback field.
	// Also carries kubectl edit's exit message (editResultMsg) — same
	// transient channel, same reasoning.
	execFeedback string
	// shellProbing is set while the 'x' async shell probe (debug.go) is in
	// flight, so a repeat 'x' is ignored instead of starting a second probe.
	shellProbing bool
	// pendingEdit is non-nil while 'E' edit's PROD-only y/N line is showing
	// (verbs.Resolve(verbs.Edit, …)) — mirrors browse.Model's own pendingEdit field.
	pendingEdit *editTarget
	// meta is non-nil while 26a's labels/annotations panel is open on this
	// pod (the shared internal/tui/metapanel editor) — the only place labels
	// and annotations show at all now that the resting view has no LABELS
	// section. Mirrors browse's pendingMeta hosting.
	meta *metapanel.Model

	namespace    string
	name         string
	siblings     []SiblingRef
	siblingIndex int
	// loadEpoch is bumped by every sibling move; loadedMsg carries the value
	// its load() saw, so a reply for the pod just left is dropped.
	loadEpoch int
	// metricsEpoch identifies the live usage-poll chain (loadMetrics); a tick
	// or reply from a retired chain is dropped, so exactly one loop runs.
	// usage/usageOK are its last reading, overlaid on every reload's pod.
	metricsEpoch int
	usage        kube.PodMetrics
	usageOK      bool

	pod   kube.Pod
	found bool
	// status is the title line's STATUS, from the raw pod (podStatus).
	status podStatus
	// gone is set once a load() reports the pod no longer exists (watch
	// delete) — Body() renders the "pod gone" banner and every key becomes
	// "go back" rather than the normal keymap.
	gone bool
	// controller is 5a's resolved CONTROLLER display text (loadedMsg's own
	// field doc comment explains the ReplicaSet→Deployment hop) — separate
	// from pod.Owner, which is the pod's direct, unresolved owner.
	controller string
	// related is the RELATED section's numbered jump targets, resolved once
	// in load() (loadedMsg's own field doc comment explains why) — pressing
	// a digit key jumps to related[digit-1] the same way 'o'/'i' used to
	// resolve on demand.
	related []relatedItem
	// sources is each container's ENV & MOUNTS rows, keyed by container name
	// (podContainerSources); showSources is the 'v' toggle that shows the
	// section for the selected container. The toggle survives [/] sibling
	// moves: it's a viewing preference, not per-pod state.
	sources     map[string]containerSources
	showSources bool
	// configErrors drives the "can't start" banner: containers the kubelet
	// refused to create over a missing Secret/ConfigMap or key.
	configErrors []configError

	eventRows []kube.Event
	// eventsErr is the last events fetch's failure — the EVENTS grid shows
	// "events unavailable" instead of a misleading "no events" (a throttled
	// or timed-out call is not an empty result).
	eventsErr error
	// eventsPending is set while the Event cache this pod's EVENTS read
	// hasn't filled yet (lazy first read): an empty grid then is not a claim
	// that the pod has no events, so it says so, and syncRetryGen's retry
	// reloads until the cache settles.
	eventsPending bool
	// syncRetryGen is the epoch of the pending tui.CacheSyncRetryMsg, so a
	// retry scheduled before a newer one can't fire a duplicate reload.
	syncRetryGen int

	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's real connection state (mock 5a: "● connected · 12ms",
	// red "◌ disconnected" mid-outage).
	conn kube.ConnState

	// selectedContainer highlights a row across CONTAINERS, INIT CONTAINERS,
	// and EPHEMERAL. Logs target every group; exec candidates remain limited
	// to the running ContainerInfos projection.
	selectedContainer int
	// bodyOffset pages the complete detail body. Without a viewport, the
	// fixed-height frame silently discarded older Events, including the
	// detailed image-pull error that explains terse ErrImagePull rows.
	bodyOffset int

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result for m.name/m.namespace as of when it
// was issued. epoch is m.loadEpoch at that moment: a load issued before a
// sibling move (a watch reload, an earlier ]) can land after it, and
// applyLoaded drops it rather than painting the previous pod under the new
// pod's name — browse's reloadEpoch pattern.
type loadedMsg struct {
	epoch  int
	status podStatus
	pod    kube.Pod
	found  bool
	events []kube.Event
	// eventsErr is the best-effort events fetch's own failure — it never
	// fails the load (err stays nil), but the EVENTS grid distinguishes it
	// from a genuinely empty result.
	eventsErr error
	err       error
	// controller is 5a's CONTROLLER field display text — pod.Owner, except
	// a ReplicaSet owner resolves one hop further to its own owning
	// Deployment (docs/design README.md §5a: "deploy/nva-worker ↗"), since a
	// Deployment never appears as a pod's direct owner. Resolved here
	// (load()'s tea.Cmd) rather than in metaGrid, which must stay pure.
	controller string
	// related is the RELATED section's numbered jump targets (owning
	// Deployment/StatefulSet, fronting Ingress) — resolved here for the same
	// reason controller is: metaGrid/relatedTolerationsBlock must stay pure, so a digit
	// press can jump without a synchronous lookup (CLAUDE.md: render
	// functions are pure, no I/O).
	related []relatedItem
	// sources and configErrors come from the raw pod spec/status, which
	// only load() has (kube.Pod doesn't project env, mounts or waiting
	// messages).
	sources      map[string]containerSources
	configErrors []configError
}

// metricsTickMsg drives the usage poll — the CPU/MEM bars' only refresh,
// since a pod's usage emits no watch event (CLAUDE.md: "metrics poll on the
// sync interval only").
type metricsTickMsg struct{ epoch int }

// metricsLoadedMsg carries one usage poll for the pod as of its chain.
type metricsLoadedMsg struct {
	epoch int
	usage kube.PodMetrics
	ok    bool
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading " + cfg.Name + "..."
	if cfg.Lister == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:        tui.DefaultWidth,
		height:       tui.DefaultHeight,
		session:      cfg.Session,
		lister:       cfg.Lister,
		metrics:      cfg.Metrics,
		events:       cfg.Events,
		mutator:      cfg.Mutator,
		actions:      actions.New(cfg.Mutator),
		openLogs:     cfg.OpenLogs,
		openYAML:     cfg.OpenYAML,
		openEvents:   cfg.OpenEvents,
		openTimeline: cfg.OpenTimeline,
		openExec:     cfg.OpenExec,
		openDebug:    cfg.OpenDebug,
		shells:       cfg.Shells,
		openForward:  cfg.OpenForward,
		openSecret:   cfg.OpenSecretData,
		openConfig:   cfg.OpenConfigMapData,
		openFlux:     cfg.OpenFluxDetail,
		timeout:      cfg.LoadTimeout,
		demo:         cfg.Demo,
		namespace:    cfg.Namespace,
		name:         cfg.Name,
		siblings:     cfg.Siblings,
		siblingIndex: cfg.SiblingIndex,
		metricsEpoch: 1,
		state:        state,
		feedback:     feedback,
		spinner:      components.NewSpinner(),
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	// Init runs on a value copy, so it reads usage on New's chain (epoch 1)
	// directly rather than through restartMetrics' pointer receiver.
	return tea.Batch(m.load(), m.spinner.Tick, m.loadMetrics())
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

// ScreenObject implements tui.ObjectScreen: a namespace switch from here
// follows this object into the new namespace.
func (m *Model) ScreenObject() (kube.ResourceKind, string, string) {
	return kube.KindPod, m.namespace, m.name
}

var _ tui.ObjectScreen = (*Model)(nil)
