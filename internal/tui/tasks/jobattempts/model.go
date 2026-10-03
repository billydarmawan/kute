// Package jobattempts is §37b/§37c/§37d (docs/design/v.0.9.0.dc.html):
// a Job's own attempt ledger, pushed from tasks/browse's Jobs list on
// 'enter' — the same recipe as cronjobdetail (§36e): a status banner and
// failure card on top (the diagnosis, "why is it broken", promoted first —
// the same idiom poddetail's termination banner and cronjobdetail's own
// failure band already establish), then either §37b's flat attempt table or
// §37d's index-completion grid underneath, gated on the Job's own
// spec.completionMode — one screen, two renderings, never two destinations.
//
// Scope note (v0.9.0, decided with the user before implementation): there is
// no retained-pod ledger. Attempts are built from whatever ListRaw(Pod)
// still has in cache — see resources/jobattempts.go's own package doc
// comment for the full reasoning. A Job whose pods have already been
// garbage-collected renders an honest, still-useful screen (the Job's own
// Reason/Message condition, the spec strip, an attempt table with fewer
// rows than FailedAttempts) rather than a fabricated "(collected)" row.
//
// 'R' stages §37c's create-vs-replace rerun choice (rerun.go) — the
// pushed screen's own copy of browse's job_actions.go, task packages can't
// import one another. Unlike browse's own copy, this screen already holds
// the full resources.JobAttemptsSummary from its own load(), so its
// diagnostic strip (resources.ClassifyJobFailure) is the real thing, not
// browse's documented simplification.
package jobattempts

import (
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/jobactions"
)

// OpenLogsFunc pushes the log-stream screen for pod — same shape as
// cronjobdetail.OpenLogsFunc.
type OpenLogsFunc func(pod kube.Pod, container string, width, height int) (tea.Model, tea.Cmd)

// OpenPodDetailFunc pushes tasks/poddetail (5a) for the selected attempt's
// pod — same shape as browse.OpenPodDetailFunc; jobattempts hands it a
// single-pod handoff (siblings=[pod.Name], index=0), mirroring
// nodedetail's own OpenPod wiring (poddetail's j/k across attempts is
// deferred, not required here).
type OpenPodDetailFunc func(pod kube.Pod, siblings []string, index int, width, height int) (tea.Model, tea.Cmd)

// OpenYAMLFunc pushes tasks/yamlview (8a) for the named object — same shape
// as cronjobdetail.OpenYAMLFunc.
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenEventsFunc pushes tasks/events (9b) object-scoped for the loaded
// Job — same shape as cronjobdetail.OpenEventsFunc.
type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// SiblingRef names one sibling Job for [/] movement — namespace-qualified,
// mirrors cronjobdetail.SiblingRef/browse.JobSiblingRef (duplicated per the
// repo's package-local-seam convention).
type SiblingRef struct {
	Namespace, Name string
}

// Config are jobattempts' dependencies, per repo convention.
type Config struct {
	Session       *tui.Session
	Lister        resources.RawLister
	Mutator       kube.Mutator
	OpenLogs      OpenLogsFunc
	OpenPodDetail OpenPodDetailFunc
	OpenYAML      OpenYAMLFunc
	OpenEvents    OpenEventsFunc
	Namespace     string
	Name          string
	Siblings      []SiblingRef
	SiblingIndex  int
	// CurrentUser is the kubeconfig user name §37c's rerun "create" path
	// stamps into kube.AnnotationTriggeredBy when Identity has no reviewed
	// answer — mirrors browse.Config.CurrentUser.
	CurrentUser string
	// Identity is the connect-time SelfSubjectReview answer, read at the
	// moment of the rerun (mirrors browse.Config.Identity). nil uses
	// CurrentUser.
	Identity    IdentityReader
	LoadTimeout time.Duration
}

type Model struct {
	width, height int

	session       *tui.Session
	lister        resources.RawLister
	mutator       kube.Mutator
	actions       actions.Controller
	openLogs      OpenLogsFunc
	openPodDetail OpenPodDetailFunc
	openYAML      OpenYAMLFunc
	openEvents    OpenEventsFunc
	timeout       time.Duration
	currentUser   string
	identity      IdentityReader

	namespace string
	name      string

	siblings     []SiblingRef
	siblingIndex int

	// found is false once a load() reports the Job no longer exists (a
	// watch delete) — Body() renders a "job deleted" banner, mirroring
	// cronjobdetail's own found field.
	found bool
	// summary is the cache-local Job+Pod aggregation this screen's status
	// banner, failure card, spec strip, and attempt table/index grid all
	// project from.
	summary resources.JobAttemptsSummary
	// pods is a best-effort namespace-scoped Pod lookup (by name) for the
	// attempt table's own 'l' logs target — mirrors cronjobdetail's own pods.
	pods map[string]kube.Pod
	// jobNames is every Job name in the namespace from load()'s own Job
	// read — the taken set §37c's rerun name is chosen against, so a second
	// rerun proposes -rerun-2 instead of colliding with -rerun-1.
	jobNames map[string]bool

	// selectedAttempt indexes m.summary.Attempts (§37b flat mode) or the
	// index-grid cell list (§37d, when m.summary.Indexed) — one selection
	// concept either way, since the two never render at once.
	selectedAttempt int
	offset          int
	// failedOnly is §37d's 'f' filter (Indexed jobs only) — restricts the
	// index grid's own rendering to failed cells, cleared on every load.
	failedOnly bool
	// diffMode is §37b's 'd' — a compare panel over the currently selected
	// attempt vs attempt 1, replacing the table body while shown (esc/d
	// toggles back). Simplified scope: without a retained log tail (see the
	// package doc comment), the comparison is exit-code/result only, not a
	// text diff of two log tails.
	diffMode bool

	// pendingRerun is non-nil while §37c's 'R' create-vs-replace choice
	// is showing (rerun.go) — the same jobactions.Rerun browse stages.
	pendingRerun *jobactions.Rerun

	now time.Time // UI clock (tea.Tick); relative age text only, never a reload trigger

	conn        kube.ConnState
	reloadEpoch int

	// execFeedback carries a non-zero 'l' logs-unavailable explanation or a
	// rerun/suspend result note — mirrors cronjobdetail's own execFeedback.
	execFeedback string

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result — epoch guards a stale reply the
// same way cronjobdetail's own loadedMsg does.
type loadedMsg struct {
	epoch    int
	found    bool
	summary  resources.JobAttemptsSummary
	pods     map[string]kube.Pod
	jobNames map[string]bool
	err      error
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
		width:         tui.DefaultWidth,
		height:        tui.DefaultHeight,
		session:       cfg.Session,
		lister:        cfg.Lister,
		mutator:       cfg.Mutator,
		actions:       actions.New(cfg.Mutator),
		openLogs:      cfg.OpenLogs,
		openPodDetail: cfg.OpenPodDetail,
		openYAML:      cfg.OpenYAML,
		openEvents:    cfg.OpenEvents,
		timeout:       cfg.LoadTimeout,
		currentUser:   cfg.CurrentUser,
		identity:      cfg.Identity,
		namespace:     cfg.Namespace,
		name:          cfg.Name,
		siblings:      cfg.Siblings,
		siblingIndex:  cfg.SiblingIndex,
		now:           time.Now(),
		state:         state,
		feedback:      feedback,
		spinner:       components.NewSpinner(),
	}
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	return tea.Batch(m.load(), m.spinner.Tick, tickCmd())
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

// CapturingInput mirrors cronjobdetail's own — only while a confirmation is
// active; pendingRerun/diffMode's own keys (enter/↑↓/d/esc) never collide
// with a global shortcut.
func (m Model) CapturingInput() bool {
	return m.actions.Active()
}

// Theme mirrors every other task package's own Config-independent Theme().
func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

// isProd mirrors cronjobdetail/browse's own isProd.
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// selectedAttemptData resolves the current selection against whichever
// source is live: flat Attempts (§37b) or the index grid's own flattened
// attempt list (§37d) — see indexGridAttempts.
func (m Model) selectedAttemptData() (resources.JobAttempt, bool) {
	list := m.attemptList()
	if m.selectedAttempt < 0 || m.selectedAttempt >= len(list) {
		return resources.JobAttempt{}, false
	}
	return list[m.selectedAttempt], true
}

// attemptList is the flat, selectable attempt sequence for whichever mode
// is active — §37b's Attempts directly, or §37d's index-grid cells reduced
// to their newest attempt each (skipping queued/empty cells, which carry
// nothing to select).
func (m Model) attemptList() []resources.JobAttempt {
	if !m.summary.Indexed {
		return m.summary.Attempts
	}
	var out []resources.JobAttempt
	for _, cell := range resources.ProjectJobIndexGrid(m.summary) {
		if m.failedOnly && cell.Result() != resources.JobAttemptFailed {
			continue
		}
		if len(cell.Attempts) == 0 {
			continue
		}
		out = append(out, cell.Attempts[len(cell.Attempts)-1])
	}
	return out
}

// ScreenObject implements tui.ObjectScreen: a namespace switch from here
// follows this object into the new namespace.
func (m *Model) ScreenObject() (kube.ResourceKind, string, string) {
	return kube.KindJob, m.namespace, m.name
}

var _ tui.ObjectScreen = (*Model)(nil)
