// Package events is 9b (docs/design/README.md §9b): a deduped, severity-
// colored feed of cluster Events. Reached namespace-scoped from
// tasks/browse's 'e' (or all-namespaces, mirroring 6b's namespace == ""
// triage) and object-scoped from tasks/poddetail and tasks/nodedetail's 'e'
// (kube.Cluster.ObjectEvents — the same seam poddetail's own EVENTS grid
// already reads).
//
// Not built on components.Table: the mockup's REASON/OBJECT cell is two
// lines tall ("reason colored by severity, object pod/name under it"),
// which the shared Table only ever renders as a single line per row — so
// this screen hand-rolls its row layout instead (view.go), the same call
// nodedetail's facts panel already made for its own two-column layout.
package events

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/components"
)

// EventsReader is the seam events needs — satisfied by *kube.Cluster and
// *fake.Cluster already (kube/events.go, kube/fake/fake.go). ObjectEvents
// is the same method poddetail's own EventsReader already declares;
// NamespaceEvents is new (this phase).
type EventsReader interface {
	NamespaceEvents(ctx context.Context, namespace string) ([]kube.Event, error)
	ObjectEvents(ctx context.Context, namespace string, kind kube.ResourceKind, name string) ([]kube.Event, error)
}

// OpenYAMLFunc pushes tasks/yamlview (8a) for a selected row's involved
// object — same shape as browse.OpenYAMLFunc/poddetail.OpenYAMLFunc/etc.
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// Config are events' dependencies, per repo convention (package-local
// Config struct, interface-typed fields, New fills zero values).
// ObjectKind/ObjectName non-empty switches the screen into object-scoped
// mode (poddetail/nodedetail's 'e'); empty means namespace-scoped
// (browse's 'e'), where Namespace == "" mirrors 6b's all-namespaces mode.
type Config struct {
	Session     *tui.Session
	Events      EventsReader
	Lister      resources.RawLister // optional: best-effort "actively-failing object" red cross-check
	OpenYAML    OpenYAMLFunc
	Namespace   string
	ObjectKind  kube.ResourceKind
	ObjectName  string
	LoadTimeout time.Duration
}

// rowKind distinguishes a real deduped event group from the folded "N
// normal events" summary row (docs/design README.md §9b: "normal events
// fold into one group line").
type rowKind int

const (
	rowGroup rowKind = iota
	rowFolded
)

// displayRow is one row events walks/renders — either a single EventGroup
// or (rowFolded) the summary standing in for every folded normal group.
type displayRow struct {
	kind   rowKind
	group  kube.EventGroup
	folded []kube.EventGroup
}

// eventWindows are the 't' time-window cycle's steps; 0 means "all time".
var eventWindows = []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 0}

type Model struct {
	width, height int

	session *tui.Session
	events  EventsReader
	lister  resources.RawLister
	// syncRetryGen guards a cache-sync retry against a newer load having
	// landed in the meantime.
	syncRetryGen int
	// loadEpoch guards every load() dispatch — bumped on every namespace
	// switch (and every other reload) so a slow reply from a namespace the
	// screen has since left can't land and overwrite the feed now showing.
	// See loadedMsg's own doc comment.
	loadEpoch int
	openYAML  OpenYAMLFunc
	timeout   time.Duration

	namespace  string
	objectKind kube.ResourceKind
	objectName string

	groups   []kube.EventGroup
	failing  map[string]bool // pod name -> currently StatusFail, for the red/yellow warning cross-check
	rows     []displayRow    // recomputeVisible's output: what's walked/rendered right now
	selected int

	// filterMatchedGroups/filterBaselineGroups are recomputeVisible's own
	// group tallies (before displayRow folding, which isn't 1:1 with group
	// count) — baseline is window+warningsOnly narrowing only, matched adds
	// filterQuery on top, so the strip can say how many the query itself
	// hid (docs/design system-wide interactions: "items never silently
	// disappear") without conflating that with the window/warningsOnly
	// toggles' own, separately-intentional narrowing.
	filterMatchedGroups, filterBaselineGroups int

	warningsOnly   bool
	normalExpanded bool
	window         time.Duration
	filterActive   bool
	filterInput    textfield.Model

	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's real connection state (never a hardcoded "connected").
	conn kube.ConnState

	fetchedAt time.Time
	state     tui.TaskState
	feedback  string
	spinner   spinner.Model
}

// loadedMsg carries one load()'s result. epoch is m.loadEpoch at the moment
// load() was dispatched — the guard applyLoaded needs against a slow load
// from a namespace the user has since switched away from landing late and
// overwriting the feed now showing under a different namespace (there is no
// kind/columns pair here the way browse has, so nothing else catches it).
type loadedMsg struct {
	epoch   int
	groups  []kube.EventGroup
	failing map[string]bool
	err     error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading events..."
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
		openYAML:   cfg.OpenYAML,
		timeout:    cfg.LoadTimeout,
		namespace:  cfg.Namespace,
		objectKind: cfg.ObjectKind,
		objectName: cfg.ObjectName,
		window:     time.Hour, // docs/design README.md §9b: "last hour" default
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

func (m Model) selectedRow() (displayRow, bool) {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return displayRow{}, false
	}
	return m.rows[m.selected], true
}

// openSelectedObject is 9b's "↵ go to object": dispatch a jump to the
// event's involved object via tui.GotoResource, the same navigation the
// root shell's palette Enter fires — model.go's routeGoto pushes a fresh
// browse view retargeted at the destination and keeps this events screen
// one esc-back away, rather than discarding it. A no-op for the folded
// summary row or an involvedObject kind the registry doesn't carry (e.g.
// "Endpoints") — ok reports whether a navigation was dispatched.
func (m Model) openSelectedObject() (tea.Cmd, bool) {
	row, ok := m.selectedRow()
	if !ok || row.kind != rowGroup {
		return nil, false
	}
	kind, name := splitObject(row.group.Object)
	if kind == "" || name == "" {
		return nil, false
	}
	kind = tui.EventObjectKind(m.session, row.group.ObjectAPIVersion, kind)
	return tui.GotoResource(m.session, kind, row.group.Namespace, name), true
}

// openSelectedYAML pushes 8a for the selected row's involved object (the
// system-wide "y opens the YAML view on any selected object, any kind"
// interaction browse/poddetail/nodedetail/whocan/objectdetail already
// implement) — same kind/namespace/name resolution as openSelectedObject's
// ↵, a no-op for the folded normal-events summary row or an unresolvable
// object.
func (m Model) openSelectedYAML() (tea.Model, tea.Cmd, bool) {
	if m.openYAML == nil {
		return nil, nil, false
	}
	row, ok := m.selectedRow()
	if !ok || row.kind != rowGroup {
		return nil, nil, false
	}
	kind, name := splitObject(row.group.Object)
	if kind == "" || name == "" {
		return nil, nil, false
	}
	kind = tui.EventObjectKind(m.session, row.group.ObjectAPIVersion, kind)
	task, cmd := m.openYAML(kind, row.group.Namespace, name, m.width, m.height)
	return task, cmd, task != nil
}

// splitObject splits a kube.Event.Object string ("Pod/nva-worker-9k2ss")
// into its Kind and Name.
func splitObject(object string) (kube.ResourceKind, string) {
	kind, name, ok := strings.Cut(object, "/")
	if !ok {
		return "", ""
	}
	return kube.ResourceKind(kind), name
}

// SwitchesNamespaceInPlace reports that this screen handles
// tui.SwitchNamespaceMsg itself (see tui.NamespaceSwitcher).
func (m Model) SwitchesNamespaceInPlace() bool { return true }

var _ tui.NamespaceSwitcher = (*Model)(nil)
