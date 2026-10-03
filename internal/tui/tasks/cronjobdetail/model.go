// Package cronjobdetail is 36e (docs/design README.md §36e, 0.8.0 plan
// Phase 7): a CronJob's own detail screen, pushed from tasks/browse's
// CronJobs list on 'enter' — the same recipe as poddetail (5a) and
// nodedetail (11b): facts grid on top, the diagnosis (a failure band, shown
// only when retained failure data exists) in the middle, a related-objects
// table (the CronJob's own associated Jobs) at the bottom, keybar
// underneath. CronJobs get no bespoke navigation.
//
// The JOBS table is scoped to this CronJob's own associated Jobs — same
// resources.AssociatedJobColumns/ProjectAssociatedJob the plan's Phase 1
// built, so browse's list and this screen can never disagree about what a
// Job's SOURCE/OUTCOME cell says. Its heading reads "associated with this
// cronjob," never "owned by this cronjob": manual Jobs are deliberately
// ownerless (§36b) and linked back purely by Kute's own annotations, so
// "owned" would misdescribe half of what's listed. Retention wording is
// honest about what Kubernetes actually kept — "N retained · history limits
// X succeeded / Y failed" — never a fabricated lifetime total (§3.2).
//
// j/k and arrows move the Jobs table's own row selection; [/] move between
// sibling CronJobs, matching poddetail's own movement contract (§3.6) — this
// deliberately replaces the mockup's "j/k next cronjob", which would
// otherwise collide with selecting a Job for enter and with the
// repository-wide j/k == ↑↓ invariant. Every mutating verb from the list
// works here unchanged (ctrl-r run now, s suspend/resume, S schedule) — the
// staging/preflight logic is duplicated from browse's jobs.go/
// cronjob_actions.go per the repo's "task packages can't import one
// another" rule, built on the same pure resources.ConcurrencyPolicyLabel/
// MissedRuns/SuspendedDuration/NextRunLabel helpers (resources/
// cronjob_actions.go's own doc comment names this screen as their other
// caller).
package cronjobdetail

import (
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

// OpenLogsFunc pushes the log-stream screen for pod — same shape as
// poddetail/nodedetail's own OpenLogsFunc, so app.go can wire all three from
// the same closure. cronjobdetail has no per-container selection of its
// own (a Job's pod usually has one container), so it always passes "" for
// container (falls back to index 0).
type OpenLogsFunc func(pod kube.Pod, container string, width, height int) (tea.Model, tea.Cmd)

// OpenYAMLFunc pushes tasks/yamlview (8a) for the named object — same shape
// as poddetail.OpenYAMLFunc.
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenEventsFunc pushes tasks/events (9b) object-scoped for the loaded
// CronJob — same shape as poddetail.OpenEventsFunc.
type OpenEventsFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenScheduleFunc pushes tasks/cronjobschedule (36d) for the loaded
// CronJob's own schedule editor — same shape as browse.OpenCronJobScheduleFunc,
// duplicated per the repo's package-local-seam convention.
type OpenScheduleFunc func(namespace, name string, width, height int) (tea.Model, tea.Cmd)

// SiblingRef names one sibling CronJob for [/] movement — namespace-
// qualified, because all-namespaces mode can list two different CronJobs
// sharing a name (mirrors browse.CronJobSiblingRef, duplicated per the
// repo's package-local-seam convention since task packages can't import one
// another).
type SiblingRef struct {
	Namespace, Name string
}

// Config are cronjobdetail's dependencies, per repo convention
// (package-local Config struct, interface-typed fields, New fills zero
// values). Siblings/SiblingIndex are the CronJobs list's current visible
// order + this row's index, the same shape poddetail.Config's own
// Siblings/SiblingIndex use for their own [/] movement.
type Config struct {
	Session      *tui.Session
	Lister       resources.RawLister
	Mutator      kube.Mutator
	OpenLogs     OpenLogsFunc
	OpenYAML     OpenYAMLFunc
	OpenEvents   OpenEventsFunc
	OpenSchedule OpenScheduleFunc
	Namespace    string
	Name         string
	Siblings     []SiblingRef
	SiblingIndex int
	// CurrentUser is the kubeconfig user name §36b's run-now stamps into
	// kube.AnnotationTriggeredBy when Identity has no reviewed answer —
	// mirrors browse.Config.CurrentUser, falling back to the same generic
	// "kute" (cronJobTriggerCreator) when both are unset.
	CurrentUser string
	// Identity is the connect-time SelfSubjectReview answer, read at the
	// moment of the run-now (mirrors browse.Config.Identity). nil uses
	// CurrentUser.
	Identity    IdentityReader
	LoadTimeout time.Duration
}

type Model struct {
	width, height int

	session      *tui.Session
	lister       resources.RawLister
	mutator      kube.Mutator
	actions      actions.Controller
	openLogs     OpenLogsFunc
	openYAML     OpenYAMLFunc
	openEvents   OpenEventsFunc
	openSchedule OpenScheduleFunc
	timeout      time.Duration
	currentUser  string
	identity     IdentityReader

	namespace string
	name      string

	siblings     []SiblingRef
	siblingIndex int

	// found is false once a load() reports the CronJob no longer exists (a
	// watch delete) — Body() renders a "cronjob deleted" banner, mirroring
	// poddetail's own m.gone.
	found bool
	// summary is the cache-local CronJob+Job+Pod aggregation this screen's
	// facts grid, failure band, and Jobs table all project from — the exact
	// same resources.CronJobSummary shape browse's own CronJob branch holds,
	// so the two screens can never disagree about a Job's outcome.
	summary resources.CronJobSummary
	// jobsErr is non-nil when the Job cache read failed on the most recent
	// load (§4.4) — the Jobs table renders "unavailable" rather than a false
	// "no retained runs", and run-now's overlap card renders "overlap
	// unknown" instead of silently taking the no-conflict path.
	jobsErr error
	// pods is a best-effort namespace-scoped Pod lookup (by name) for the
	// Jobs table's own 'l' logs target — mirrors browse's own m.pods.
	pods map[string]kube.Pod
	// controller is this CronJob's own best-effort CONTROLLER field —
	// resolved once in load() (never the render path), mirroring
	// poddetail's own controller field.
	controller string

	selectedJob int
	offset      int

	// pendingRun/pendingResume are non-nil while §36b's ctrl-r run-now or
	// §36c's resume preflight is showing — screen-owned staged state before
	// ever calling actions.Begin on a TierNone verb, same contract browse's own
	// pendingCronJobRun/pendingCronJobResume follow (jobs.go/
	// cronjob_actions.go's own doc comments). Single-target only: this
	// screen has no marked-set bulk concept.
	pendingRun    *cronJobRunTarget
	pendingResume *cronJobResumeTarget

	// meta is non-nil while 26a's labels/annotations panel is open on this
	// CronJob (the shared internal/tui/metapanel editor) — mirrors
	// poddetail's own meta hosting.
	meta *metapanel.Model

	now time.Time // UI clock (tea.Tick); relative ETA/age text only, never a reload trigger

	conn        kube.ConnState
	reloadEpoch int

	// execFeedback carries a non-zero 'l' logs-unavailable explanation or a
	// run-now/suspend/resume result note — mirrors poddetail/nodedetail's
	// own execFeedback field.
	execFeedback string

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result — epoch guards a stale reply the
// same way browse/nodedetail's own reloadDueMsg does.
type loadedMsg struct {
	epoch   int
	found   bool
	summary resources.CronJobSummary
	jobsErr error
	pods    map[string]kube.Pod

	// controller is resolved in load()'s own tea.Cmd (render must stay
	// pure) — see resolveCronJobController's own doc comment.
	controller string

	err error
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
		mutator:      cfg.Mutator,
		actions:      actions.New(cfg.Mutator),
		openLogs:     cfg.OpenLogs,
		openYAML:     cfg.OpenYAML,
		openEvents:   cfg.OpenEvents,
		openSchedule: cfg.OpenSchedule,
		timeout:      cfg.LoadTimeout,
		currentUser:  cfg.CurrentUser,
		identity:     cfg.Identity,
		namespace:    cfg.Namespace,
		name:         cfg.Name,
		siblings:     cfg.Siblings,
		siblingIndex: cfg.SiblingIndex,
		now:          time.Now(),
		state:        state,
		feedback:     feedback,
		spinner:      components.NewSpinner(),
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

// CapturingInput reports whether the root shell should let this screen see
// every keystroke instead of treating g/n/c/? as global shortcuts — while a
// confirmation is active or 26a's panel is open, mirroring poddetail/
// nodedetail. The run-now/resume preflights (pendingRun/pendingResume) are
// deliberately excluded, matching browse's own CapturingInput: their own
// keys (enter/y/esc) never collide with a global shortcut.
func (m Model) CapturingInput() bool {
	return m.actions.Active() || m.meta != nil
}

// Theme mirrors every other task package's own Config-independent Theme().
func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

// isProd reports whether the active session's current context is tagged
// prod — same source poddetail/browse's own isProd reads.
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}

// IdentityReader is the connect-time SelfSubjectReview seam — satisfied by
// *kube.Cluster and *fake.Cluster's ReviewedIdentity.
type IdentityReader interface {
	ReviewedIdentity() (kube.ReviewedIdentity, bool)
}

// cronJobTriggerCreator is the identity §36b's run-now stamps into
// kube.AnnotationTriggeredBy — mirrors browse's own cronJobTriggerCreator:
// the reviewed username, read now, else the kubeconfig name, else "kute".
func (m Model) cronJobTriggerCreator() string {
	if m.identity != nil {
		if id, ok := m.identity.ReviewedIdentity(); ok && id.Username != "" {
			return id.Username
		}
	}
	if m.currentUser != "" {
		return m.currentUser
	}
	return "kute"
}

// selectedJobSummary returns the Jobs table's currently selected
// resources.JobSummary, ok false when there are no retained/active runs.
func (m Model) selectedJobSummary() (resources.JobSummary, bool) {
	if m.selectedJob < 0 || m.selectedJob >= len(m.summary.Runs) {
		return resources.JobSummary{}, false
	}
	return m.summary.Runs[m.selectedJob], true
}

// ScreenObject implements tui.ObjectScreen: a namespace switch from here
// follows this object into the new namespace.
func (m *Model) ScreenObject() (kube.ResourceKind, string, string) {
	return kube.KindCronJob, m.namespace, m.name
}

var _ tui.ObjectScreen = (*Model)(nil)
