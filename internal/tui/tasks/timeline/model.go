// Package timeline is 16a/16b (docs/design/README.md §16a/§16b): one merged
// clock — Events, container restarts, and Deployment rollout revisions —
// newest first, answering "what changed" during an incident. Reached
// namespace-scoped from tasks/browse's 't' (16a) and object-scoped from
// tasks/poddetail and tasks/nodedetail's 't' (16b), the same dual-mode shape
// tasks/events already establishes (ObjectKind/ObjectName empty = namespace
// scope).
//
// 16b additionally grows a revision rail — "deployment revisions as a
// vertical rail with the current one highlighted" — when the scoped object
// resolves to an owning Deployment (a Pod's ReplicaSet owner, or the
// Deployment itself): a fixed-width left sidebar of per-revision cards
// alongside the feed, not a table stacked above it. tasks/helmhistory
// (18a's 'h') reuses the rail concept for Helm release history, in its own
// single-line-per-revision table shape.
package timeline

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
)

// EventsReader is the seam timeline needs for its Events source — the same
// interface tasks/events declares (ObjectEvents/NamespaceEvents), satisfied
// by *kube.Cluster and *fake.Cluster already.
type EventsReader interface {
	NamespaceEvents(ctx context.Context, namespace string) ([]kube.Event, error)
	ObjectEvents(ctx context.Context, namespace string, kind kube.ResourceKind, name string) ([]kube.Event, error)
}

// windowSteps are the 't' time-window cycle's stops. 16a's default is "last
// 30m" (docs/design README.md §16a's breadcrumb tag).
var windowSteps = []time.Duration{30 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 0}

// OpenEventsFunc pushes tasks/events (9b) for the 'e' global verb — object-
// scoped (kind/name non-empty) when timeline itself is 16b object-scoped,
// namespace-scoped (kind/name empty) when it's 16a — same signature as
// poddetail/nodedetail's own OpenEventsFunc so app.go's openObjectEventsFunc
// closure can be reused directly for the 16b wiring.
type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// Config are timeline's dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values).
// ObjectKind/ObjectName non-empty switches the screen into 16b's
// object-scoped mode; empty means 16a's namespace-scoped mode (Namespace ==
// "" mirrors 6b's all-namespaces triage, same rule tasks/events already
// follows).
type Config struct {
	Session     *tui.Session
	Events      EventsReader
	Lister      resources.RawLister
	Mutator     kube.Mutator
	Namespace   string
	ObjectKind  kube.ResourceKind
	ObjectName  string
	OpenEvents  OpenEventsFunc
	LoadTimeout time.Duration
}

type Model struct {
	width, height int

	session *tui.Session
	events  EventsReader
	lister  resources.RawLister
	// syncRetryGen guards a cache-sync retry against a newer load having
	// landed in the meantime.
	syncRetryGen int
	mutator      kube.Mutator
	actionsCtl   actions.Controller
	openEvents   OpenEventsFunc
	timeout      time.Duration

	namespace  string
	objectKind kube.ResourceKind
	objectName string

	entries []kube.TimelineEntry // every merged entry, unwindowed
	rows    []kube.TimelineEntry // entries after the window/filter/warnings/fold — what's walked/rendered
	// filterBaselineRows is recomputeVisible's own window-only tally (no
	// filterQuery applied) — lets the strip say how many the query itself
	// hid, same reasoning as tasks/events' filterBaselineGroups.
	filterBaselineRows int
	selected           int

	// warningsOnly is 16a's 'w' toggle (docs/design README.md §16a),
	// mirroring tasks/events' own field of the same name: hard-excludes
	// Normal-severity Events from m.rows entirely (restarts/rollouts always
	// survive it — they're never "normal" chatter). 16b has no 'w' key.
	warningsOnly bool
	// normalExpanded is 16a's fold/expand toggle for Normal-severity Events
	// (tasks/events' own §9b idiom) — folded entries are tracked in
	// foldedNormal for the footer line rather than appearing in m.rows.
	normalExpanded bool
	foldedNormal   []kube.TimelineEntry
	// normalPresent is set by recomputeVisible regardless of normalExpanded
	// (unlike foldedNormal, which empties out once expanded) — the
	// keybar's own hasNormal-equivalent gate for advertising 'tab'.
	normalPresent bool

	// rail is 16b's revision rail: the resolved owning Deployment's
	// TimelineRollout entries, newest-first, index 0 == current. Empty in
	// 16a (namespace-scoped) and in 16b whenever the object doesn't resolve
	// to a Deployment (e.g. a Node).
	rail           []kube.TimelineEntry
	railDeployment string
	// railFocused/railSelected are 16b's rail navigation: 'tab' toggles
	// focus between the rail (↑↓ moves railSelected, enabling 'R' rollback —
	// the default focus once a rail resolves, applyLoaded's own doc comment)
	// and the feed (↑↓ moves m.selected). Moving railSelected live-syncs
	// m.selected to the most recent entry from that revision's own lifetime
	// (update.go's syncFeedToRailSelection/railSelectionTarget) so the feed
	// always reflects whichever revision the rail cursor is on without
	// waiting for '↵'. The rail's own selection highlight isn't gated on
	// railFocused — it stays visible in both panels at once, since the two
	// are one linked view.
	railFocused  bool
	railSelected int

	window       time.Duration
	filterActive bool
	filterInput  textfield.Model

	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's real connection state (never a hardcoded "connected").
	conn kube.ConnState

	fetchedAt time.Time
	state     tui.TaskState
	feedback  string
	spinner   spinner.Model
}

// loadedMsg carries one load()'s result.
type loadedMsg struct {
	entries        []kube.TimelineEntry
	rail           []kube.TimelineEntry
	railDeployment string
	err            error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading timeline..."
	if cfg.Events == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	}
	return Model{
		width:      tui.DefaultWidth,
		height:     tui.DefaultHeight,
		session:    cfg.Session,
		events:     cfg.Events,
		lister:     cfg.Lister,
		mutator:    cfg.Mutator,
		actionsCtl: actions.New(cfg.Mutator),
		openEvents: cfg.OpenEvents,
		timeout:    cfg.LoadTimeout,
		namespace:  cfg.Namespace,
		objectKind: cfg.ObjectKind,
		objectName: cfg.ObjectName,
		window:     windowSteps[0],
		state:      state,
		feedback:   feedback,
		spinner:    components.NewSpinner(),
	}
}

func (m Model) Init() tea.Cmd {
	if m.events == nil {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

// objectScoped reports whether this is 16b (an object-scoped feed) rather
// than 16a (namespace-scoped).
func (m Model) objectScoped() bool {
	return m.objectKind != "" && m.objectName != ""
}

func (m Model) selectedRow() (kube.TimelineEntry, bool) {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return kube.TimelineEntry{}, false
	}
	return m.rows[m.selected], true
}

// revisionSelected reports whether the feed cursor is on a §32a git
// revision row or a §34a Argo sync row — either way, something 'v' could
// copy.
func (m Model) revisionSelected() bool {
	row, ok := m.selectedRow()
	return ok && (row.Kind == kube.TimelineRevision || row.Kind == kube.TimelineSync) && row.GitRevision != ""
}

// selectedRevision is 16b's rail cursor target for 'R' rollback.
func (m Model) selectedRevision() (kube.TimelineEntry, bool) {
	if m.railSelected < 0 || m.railSelected >= len(m.rail) {
		return kube.TimelineEntry{}, false
	}
	return m.rail[m.railSelected], true
}

// isProd mirrors helmhistory's/browse's own isProd (Session.Config.IsProd),
// duplicated per the repo's package-local-seam convention — every screen's
// write-confirm policy reads its own Session directly.
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// openSelectedObject is 16a/16b's "↵ goes to the object": dispatch a jump
// to the entry's involved object via tui.GotoResource, the same navigation
// tasks/events' own openSelectedObject and the root shell's palette Enter
// use — model.go's routeGoto pushes a fresh browse view retargeted at the
// destination and keeps this timeline screen one esc-back away.
func (m Model) openSelectedObject() (tea.Cmd, bool) {
	row, ok := m.selectedRow()
	if !ok {
		return nil, false
	}
	kind, name := splitObject(row.Object)
	if kind == "" || name == "" {
		return nil, false
	}
	kind = tui.EventObjectKind(m.session, row.ObjectAPIVersion, kind)
	return tui.GotoResource(m.session, kind, row.Namespace, name), true
}

// openSelectedEvents is the 'e' global verb: pushes 9b scoped exactly like
// this timeline itself is scoped — object-scoped in 16b, namespace-scoped
// in 16a (docs/design README.md: "e opens events (namespace-scoped from a
// list view; object-scoped from a detail view)").
func (m Model) openSelectedEvents() (tea.Model, tea.Cmd, bool) {
	if m.openEvents == nil {
		return nil, nil, false
	}
	task, cmd := m.openEvents(m.objectKind, m.namespace, m.objectName, m.width, m.height)
	return task, cmd, task != nil
}
