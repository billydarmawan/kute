// Package cronjobschedule is 36d (docs/design README.md §36d, 0.8.0 plan
// Phase 6): a CronJob's own schedule editor, pushed from tasks/browse's
// CronJobs list on 'S' — a dedicated task, not the bespoke inline browse
// buffer (browse/cronjobschedule.go, Phase 0-5's placeholder) that predates
// it. Its own breadcrumb segment ("cronjob/report-nightly › Schedule"), one
// 'esc' back, and — like meta.go/secretdata/configmapdata — a commit never
// closes the screen: apply is a reversible TierNone patch through
// actions.Controller, always followed by "confirm → execute → refresh →
// show result → remain on screen" (CLAUDE.md), never the close-on-commit
// shape 25a's setresources.go still has.
//
// One field, three kinds of live feedback recomputed on every keystroke: a
// per-field MIN/HOUR/DOM/MONTH/DOW breakdown plus a conservative English
// description (cron.go's decodeScheduleFields/describeSchedule), and the
// next five real timestamps in the selected timezone
// (resources.NextCronRuns). A fourth — WHAT CHANGES, the old/new firing-set
// diff over the coming year — runs asynchronously, keyed to an edit
// generation so a stale keystroke's result is never shown
// (cron.go's compareFiringSets).
package cronjobschedule

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

// CapabilityReader is 36d's own package-local capability seam (Architecture's
// "define the interface you need in the task package" rule, §3.8) —
// satisfied by both *kube.Cluster and kube/fake.Cluster's TimeZoneCapability
// method without either being imported here as a concrete type. A nil
// Config.Capabilities reads as kube.TimeZoneCapabilityUnknown throughout
// this package (timeZoneCapability's own nil check) — the same safe default
// a real cluster carries before its first probe completes.
type CapabilityReader interface {
	TimeZoneCapability() kube.TimeZoneCapability
}

// Config are cronjobschedule's dependencies, per repo convention
// (package-local Config struct, interface-typed fields, New fills zero
// values).
type Config struct {
	Session      *tui.Session
	Lister       resources.RawLister
	Mutator      kube.Mutator
	Capabilities CapabilityReader
	Namespace    string
	Name         string
	LoadTimeout  time.Duration
}

// acceptedPair is the schedule/timezone pair the server has actually
// accepted — the baseline commitApply compares the typed buffers against
// (hasPendingChange), the source for a fresh reload's buffer values, and
// what a one-step undo restores. hasTimezone distinguishes an explicit ""
// (never valid) from "spec.timeZone is genuinely unset", the same nil-vs-
// pointer-to-"" convention kube.CronJobScheduleEdit.TimeZone uses.
type acceptedPair struct {
	schedule        string
	timezone        string
	hasTimezone     bool
	resourceVersion string
	generation      int64
}

// scheduleCommit remembers what an in-flight apply/undo is trying to write
// — cleared once handleResult applies the outcome, the same bookkeeping
// meta.go's metaPendingCommit and secretdata's secretPendingCommit provide
// for their own screens. before is the accepted pair as of just prior to
// this commit — handleResult promotes it to the new one-step undo target on
// success, unless this commit was itself an undo (isUndo), which clears the
// undo target instead: one step, not a stack.
type scheduleCommit struct {
	schedule        string
	timezone        *string
	resourceVersion string
	isUndo          bool
	before          acceptedPair
}

type Model struct {
	width, height int

	session      *tui.Session
	lister       resources.RawLister
	mutator      kube.Mutator
	capabilities CapabilityReader
	actions      actions.Controller
	timeout      time.Duration

	namespace string
	name      string

	initialized bool // true once the first successful load has seeded the buffers
	accepted    acceptedPair

	scheduleInput textfield.Model
	tzInput       textfield.Model
	tzFocused     bool

	// Derived state, recomputed synchronously on every edit
	// (recomputeDerived) — never read the clock, never touch the cluster.
	parseErr error
	tzErr    error
	fields   scheduleFields
	nextRuns []time.Time
	nextErr  error

	// editGen/comparison/comparing/comparisonDirty back WHAT CHANGES' async,
	// generation-keyed comparison (cron.go's compareFiringSets) — task 7.
	// comparisonDirty is set by recomputeAll whenever the typed buffers
	// validly differ from m.accepted and cleared once a comparison for the
	// current editGen is in flight or has landed; the next tickMsg is what
	// actually starts the background command (see update.go's tickMsg
	// case) — recomputeAll runs synchronously on every keystroke (including
	// from inside a paste closure, which can't return a tea.Cmd) and so
	// can never dispatch the heavy comparison itself.
	editGen         int
	comparison      *scheduleComparison
	comparing       bool
	comparisonDirty bool

	pendingCommit *scheduleCommit
	previous      *acceptedPair // one-step undo target; nil once nothing to undo
	message       string
	lastError     string
	conflict      bool // last apply failed with Conflict — 'r' offers a reload

	// supersededRV is the resourceVersion a successful apply/undo replaced:
	// a cache snapshot still carrying it predates that write, so
	// applyLoaded keeps the mutation response's m.accepted over it.
	supersededRV string

	now time.Time // UI clock (tea.Tick); relative ETA text only, never a reload trigger

	conn        kube.ConnState
	reloadEpoch int

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
}

// loadedMsg carries one load()'s result.
type loadedMsg struct {
	epoch int
	found bool
	cj    *cronJobSnapshot
	err   error
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
	theme := tui.Dark()
	if cfg.Session != nil {
		theme = cfg.Session.Theme
	}
	scheduleInput := newScheduleInput(theme)
	tzInput := newScheduleInput(theme)
	return Model{
		width:         tui.DefaultWidth,
		height:        tui.DefaultHeight,
		session:       cfg.Session,
		lister:        cfg.Lister,
		mutator:       cfg.Mutator,
		capabilities:  cfg.Capabilities,
		actions:       actions.New(cfg.Mutator),
		timeout:       cfg.LoadTimeout,
		namespace:     cfg.Namespace,
		name:          cfg.Name,
		scheduleInput: scheduleInput,
		tzInput:       tzInput,
		now:           time.Now(),
		state:         state,
		feedback:      feedback,
		spinner:       components.NewSpinner(),
	}
}

// newScheduleInput builds one of this screen's two buffers — styled from
// Theme (never bubbles' own DefaultStyles, per TextInputStyles' own doc
// comment) and non-blinking, the same static-cursor idiom every other
// hand-rolled cursor in the app uses. Skipping SetStyles here isn't just a
// missing color: bubbles' own default Cursor.Blink defaults true, which
// under a running program schedules a real tea.Tick-backed blink command on
// every keystroke — harmless in the app itself (Bubble Tea's own event loop
// just keeps ticking), but it turns a test's synchronous cmd-draining
// (cronjobschedule_test.go's step) into an infinite loop the same way
// skipping this would for §36a's own recurring clock tick.
func newScheduleInput(theme tui.Theme) textfield.Model {
	ti := textfield.New()
	ti.Prompt = ""
	ti.SetStyles(tui.TextInputStyles(theme))
	return ti
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
	// The previous Bubbles input rendered one cursor-reserved cell beyond
	// SetWidth. Keep this screen's established field/note alignment while
	// the shared text field itself uses strict viewport widths.
	fieldWidth := max(size.Width-40, 10) + 1
	m.scheduleInput.SetWidth(fieldWidth)
	m.tzInput.SetWidth(fieldWidth)
}

// cronJobSnapshot is load()'s own minimal read of the fields this screen
// needs — namespace/name are already known from Config, so there's no
// reason to carry a whole *batchv1.CronJob (and its own deep-copy cost)
// through a tea.Msg the way BuildCronJobSummaries' aggregation does for
// browse/cronjobdetail.
type cronJobSnapshot struct {
	schedule        string
	timezone        string
	hasTimezone     bool
	resourceVersion string
	generation      int64
}

func (m Model) load() tea.Cmd {
	epoch := m.reloadEpoch
	lister := m.lister
	namespace := m.namespace
	name := m.name
	timeout := m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		objs, err := lister.ListRaw(ctx, kube.KindCronJob, namespace)
		if err != nil {
			return loadedMsg{epoch: epoch, err: err}
		}
		snap, ok := findCronJob(objs, name)
		if !ok {
			return loadedMsg{epoch: epoch, found: false}
		}
		return loadedMsg{epoch: epoch, found: true, cj: snap}
	}
}

// CapturingInput reports whether either buffer wants every keystroke — the
// root shell's own InputCapturer check, so 'g'/'n'/'c'/'?' don't get
// hijacked as shell shortcuts while a schedule/timezone value is being
// typed. Both buffers are always "open" once the screen is Ready (there is
// no separate navigation-vs-editing mode the way secretdata's add/edit rows
// have — a single field is always live), so this is simply "ready and not
// mid-confirm."
func (m Model) CapturingInput() bool {
	return m.state == tui.TaskStateReady && !m.actions.Active()
}

// Theme mirrors every other task package's own Config-independent Theme()
// (secretdata's own doc comment on this exact method applies verbatim).
func (m Model) Theme() tui.Theme {
	if m.session != nil {
		return m.session.Theme
	}
	return tui.Dark()
}

// isProd reports whether the active context is tagged PROD — the input
// actions.Controller.Begin resolves this screen's verbs' confirm policy with.
func (m Model) isProd() bool {
	if m.session == nil {
		return false
	}
	return m.session.Config.IsProd(m.session.Location.Context)
}
