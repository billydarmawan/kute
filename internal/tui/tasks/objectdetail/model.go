// Package objectdetail is 14d (docs/design/README.md §14d): the generic
// custom-resource detail view reached from browse's ↵ on any discovered CRD
// kind's row (resources.Descriptor.Custom). It's built only from what every
// object has — title row, a meta grid from the kind's own declared printer
// columns, a verbatim CONDITIONS grid, and EVENTS — no CRD-specific layout
// code, ever, the payoff of the kind-registry architecture. When an object
// has neither conditions nor events, applyLoaded redirects straight to
// tasks/yamlview instead of rendering an empty screen (see load.go's
// loadedMsg handling in update.go).
package objectdetail

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/metapanel"
)

// EventsReader is the seam for the EVENTS grid — same shape as poddetail's/
// nodedetail's, duplicated per the repo's package-local-seam convention.
type EventsReader interface {
	ObjectEvents(ctx context.Context, namespace string, kind kube.ResourceKind, name string) ([]kube.Event, error)
}

// OpenYAMLFunc pushes tasks/yamlview (8a) for the named object — same shape
// as browse.OpenYAMLFunc/poddetail.OpenYAMLFunc. Also the empty-object
// redirect target (see load.go).
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenEventsFunc pushes tasks/events (9b) object-scoped for the loaded
// object — same shape as poddetail's.
type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// Config are objectdetail's dependencies, per repo convention. Siblings/
// SiblingIndex are the ordered name list + cursor browse hands over so j/k
// can move to the next/prev object without leaving detail, same as
// poddetail's.
type Config struct {
	Session      *tui.Session
	Lister       resources.RawLister
	Events       EventsReader
	Mutator      kube.Mutator
	OpenYAML     OpenYAMLFunc
	OpenEvents   OpenEventsFunc
	Kind         kube.ResourceKind
	Namespace    string
	Name         string
	Siblings     []string
	SiblingIndex int
	LoadTimeout  time.Duration
}

// condition is one status.conditions entry, read generically off any
// object's unstructured form.
type condition struct {
	Type, Status, Message, Reason string
	LastTransition                time.Time
}

type Model struct {
	width, height int

	session *tui.Session
	lister  resources.RawLister
	events  EventsReader
	mutator kube.Mutator
	actions actions.Controller

	openYAML   OpenYAMLFunc
	openEvents OpenEventsFunc
	timeout    time.Duration

	kind         kube.ResourceKind
	namespace    string
	name         string
	siblings     []string
	siblingIndex int

	desc resources.Descriptor

	obj   *unstructured.Unstructured
	found bool
	// gone is set once a load() reports the object no longer exists (watch
	// delete) — mirrors poddetail's own field/behavior.
	gone bool
	// meta is non-nil while 26a's labels/annotations panel is open on this
	// object (the shared internal/tui/metapanel editor) — 14d shows neither
	// labels nor annotations anywhere else, so this is the only place a
	// discovered object's metadata is readable at all. Mirrors poddetail's
	// hosting.
	meta *metapanel.Model
	// redirected is set when applyLoaded hands this screen off to
	// tasks/yamlview because the object has neither conditions nor events.
	// It backs tui.Transient, so the root drops this instance instead of
	// pushing it under the YAML view — otherwise Escape pops back here, the
	// resumed load redirects again, and Escape can never leave.
	redirected bool

	row        resources.Row
	conditions []condition
	eventRows  []kube.Event
	// eventsErr is the best-effort events fetch's own failure — distinct
	// from a genuinely empty result, same reasoning as poddetail's.
	eventsErr error

	// syncRetryGen is the epoch of the pending tui.CacheSyncRetryMsg, so a
	// retry scheduled before a newer one can't fire a duplicate reload.
	syncRetryGen int

	conn kube.ConnState

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

type loadedMsg struct {
	obj        *unstructured.Unstructured
	row        resources.Row
	conditions []condition
	found      bool
	events     []kube.Event
	eventsErr  error
	err        error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}
	state := tui.TaskStateLoading
	feedback := "Loading " + cfg.Name + "..."
	var desc resources.Descriptor
	if cfg.Lister == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
	} else if cfg.Session == nil {
		state = tui.TaskStateError
		feedback = "no session"
	} else if d, ok := cfg.Session.Registry.Descriptor(cfg.Kind); ok {
		desc = d
	} else {
		state = tui.TaskStateError
		feedback = "unknown resource kind " + string(cfg.Kind)
	}
	return Model{
		width:        tui.DefaultWidth,
		height:       tui.DefaultHeight,
		session:      cfg.Session,
		lister:       cfg.Lister,
		events:       cfg.Events,
		mutator:      cfg.Mutator,
		actions:      actions.New(cfg.Mutator),
		openYAML:     cfg.OpenYAML,
		openEvents:   cfg.OpenEvents,
		timeout:      cfg.LoadTimeout,
		kind:         cfg.Kind,
		namespace:    cfg.Namespace,
		name:         cfg.Name,
		siblings:     cfg.Siblings,
		siblingIndex: cfg.SiblingIndex,
		desc:         desc,
		state:        state,
		feedback:     feedback,
		spinner:      components.NewSpinner(),
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil || m.state == tui.TaskStateError {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick)
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

// ScreenObject implements tui.ObjectScreen: a namespace switch from here
// follows this object into the new namespace.
func (m *Model) ScreenObject() (kube.ResourceKind, string, string) {
	return m.kind, m.namespace, m.name
}

var _ tui.ObjectScreen = (*Model)(nil)
