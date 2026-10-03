package tui

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/components/palette"
)

// TaskState identifies the current user-facing state of the active task screen.
type TaskState string

const (
	TaskStateLoading          TaskState = "loading"
	TaskStateReady            TaskState = "ready"
	TaskStateEmpty            TaskState = "empty"
	TaskStateError            TaskState = "error"
	TaskStatePermissionDenied TaskState = "permission-denied"
	TaskStateConfirming       TaskState = "confirming"
	TaskStateSuccess          TaskState = "success"
	TaskStateCancelled        TaskState = "cancelled"
)

// KubernetesContext describes the active Kubernetes target shown in the TUI.
type KubernetesContext struct {
	ClusterName string
	ContextName string
	Namespace   string
}

// TaskScope describes the Kubernetes target and verb for a task action.
type TaskScope struct {
	ResourceKind string
	ResourceName string
	Namespace    string
	Verb         string
	IsMutating   bool
	// Revision is the target revision for a "rollback" verb (18a) — 0 means
	// Helm's own default (the previous revision). Unset (0) for every other
	// verb.
	Revision int
	// Replicas is the target replica count for a "scale" verb (17b). Unset
	// (0) for every other verb.
	Replicas int32
	// Container is the target container name for a "set-image" verb (24a).
	// Empty for every other verb.
	Container string
	// InitContainer selects spec.initContainers rather than spec.containers
	// for a "set-image" verb. False for every other verb.
	InitContainer bool
	// Image is the target image ref for a "set-image" verb (24a). Empty for
	// every other verb.
	Image string
	// Resources is the changed container resource fields for a
	// "set-resources" verb (25a). Container names which container these
	// apply to, reusing the same field "set-image" already populates. nil
	// for every other verb.
	Resources *kube.ResourceEdits
	// MetaKey/MetaValue/MetaIsAnnotation/MetaRemove are a "set-meta" verb's
	// (26a) target label/annotation edit. MetaRemove true is a ctrl-d key
	// removal (MetaValue is "" in that case). Empty/false for every other
	// verb.
	MetaKey          string
	MetaValue        string
	MetaIsAnnotation bool
	MetaRemove       bool
	// MetaOverwrite is whether the key already existed before this edit —
	// the sole difference in the rendered "will run" command
	// (`--overwrite` vs. a brand-new key). Meaningless when MetaRemove.
	MetaOverwrite bool
	// MetaJoinService/MetaJoinPodCount are set only when a "set-meta" edit
	// (never a removal) targets a label a Service selector currently
	// matches — non-empty MetaJoinService is what escalates that edit's
	// tier to TierInline and lets Keybar() render "changing this detaches N
	// pods from svc/X" straight off this already-resolved Scope, the same
	// way setResourcesWillRunLine/setImageWillRunLine read their own Scope
	// fields rather than needing the (by-then-closed) originating panel.
	MetaJoinService  string
	MetaJoinPodCount int
	// SecretKey/SecretValue/SecretRemove are a "secret-data" verb's (27b)
	// target Secret data-key edit. SecretRemove true is a ctrl-d key
	// removal (SecretValue is "" in that case). Empty/false for every other
	// verb.
	SecretKey    string
	SecretValue  string
	SecretRemove bool
	// ConfigMapKey/ConfigMapValue/ConfigMapRemove are a "configmap-data" verb's
	// (27a) target ConfigMap data-key edit. ConfigMapRemove true is a ctrl-d
	// key removal (ConfigMapValue is "" in that case). Empty/false for every
	// other verb.
	ConfigMapKey    string
	ConfigMapValue  string
	ConfigMapRemove bool
	// DataResourceVersion is a "secret-data"/"configmap-data" verb's
	// precondition: the Secret's/ConfigMap's resourceVersion as the screen
	// observed it when the buffer opened, so a concurrent write returns
	// Conflict instead of being silently overwritten. Empty for every
	// other verb.
	DataResourceVersion string
	// ConfigMapRestartConsumers/ConfigMapConsumers are a "configmap-data"
	// edit/add's ctrl-r commit (27a) — chains the patch with a RolloutRestart
	// of every entry in ConfigMapConsumers. Empty/false for a plain ↵ apply
	// or any removal.
	ConfigMapRestartConsumers bool
	ConfigMapConsumers        []kube.ConfigMapConsumerRef
	// FluxSourceKind/FluxSourceName/FluxSourceNamespace turn a
	// "flux-reconcile" into §30b's *with-source* reconcile: the source is
	// annotated first, then the reconciler. Empty on §30a's plain reconcile
	// and on a reconcile of a source row, which is already the source.
	//
	// Two writes behind one verb, exactly as ConfigMapRestartConsumers above
	// chains a rollout-restart onto a ConfigMap patch — the alternative, a
	// second verb, would put two spellings of "reconcile" in the registry
	// for what a user experiences as one key.
	FluxSourceKind      string
	FluxSourceName      string
	FluxSourceNamespace string
	// NewName is the target name for a create/clone verb ("job-retry"'s
	// cloned Job, "cronjob-run-now"'s triggered Job) — precomputed by the
	// begin method so the will-run line and the executed Create call agree,
	// the same reason FluxSourceKind/Name/Namespace above are resolved once
	// rather than re-derived at execute time. Empty for every other verb.
	NewName string
	// Schedule is the target cron schedule string for a
	// "cronjob-set-schedule" verb — validated client-side via robfig/cron/v3's
	// ParseStandard before Begin is ever called
	// (cronjobschedule.go's commitCronSchedule), the same "the panel does its
	// own gathering, Scope just carries the resolved value" shape
	// Replicas/Image/Resources use for Scale/SetImage/SetResources. Empty for
	// every other verb.
	Schedule string
	// ArgoSyncRevision is an "argo-sync" verb's (§33a) already-resolved
	// target revision — the Application's own spec.source.targetRevision
	// (or "HEAD" when unset), read off the row's rendered REVISION cell at
	// Begin time (browse/argo.go's beginArgoSync) the same way
	// FluxSourceKind/Name/Namespace are resolved once rather than
	// re-derived at execute time. This re-applies what git already says to
	// run, never a move to a different ref. Empty for every other verb.
	ArgoSyncRevision string
	// ArgoSyncInitiator is the "argo-sync" verb's operation.initiatedBy.
	// username (kube.ArgoSyncInitiator), resolved at Begin time alongside
	// ArgoSyncRevision so the will-run line and the executed patch name the
	// same person. Empty for every other verb.
	ArgoSyncInitiator string
	// CronJobResourceVersion is the source CronJob's own resourceVersion at
	// staging time — the precondition "cronjob-suspend"/"cronjob-resume"
	// and "cronjob-set-schedule" pass to kube.Mutator's SetCronJobSuspend/
	// SetCronJobSchedule (0.8.0 plan §4.2/Phase 2 tasks 5-7) so a concurrent
	// external edit returns Conflict rather than being silently overwritten.
	// Empty for every other verb.
	CronJobResourceVersion string
	// CronJobGeneration is the source CronJob's own Generation at staging
	// time — a "cronjob-suspend" verb's SetCronJobSuspend call stamps
	// kute.dev/suspended-generation as this value + 1, the generation the
	// object will have once the suspend patch itself lands (§3.3), the same
	// "preview and execution use the identical value" contract NewName
	// follows for a create/clone verb. Zero for every other verb.
	CronJobGeneration int64
	// CronJobTimeZone is a "cronjob-set-schedule" verb's timezone edit: nil
	// leaves spec.timeZone untouched, a pointer to "" explicitly clears it,
	// a pointer to a non-empty IANA name sets it (§3.8/§3.9's set/clear
	// distinction — an empty or capability-unsupported value must never
	// accidentally serialize as a stale timezone string). nil for every
	// other verb.
	CronJobTimeZone *string
	// StagedAt is the instant a "cronjob-run-now" or "cronjob-suspend"
	// action was staged/confirmed — kept distinct from the moment the API
	// server actually accepts the write (kute.dev/triggered-at /
	// kute.dev/suspended-at, §3.3/§36b) so a delayed confirm still records
	// when the operator asked, not when the request happened to land. Zero
	// for every other verb.
	StagedAt time.Time
	// TriggerCreator is a "cronjob-run-now" verb's kute.dev/triggered-by
	// value — who/what asked for the manual run (§36b). Empty for every
	// other verb.
	TriggerCreator string
	// BulkTargets, when non-empty, turns this action into a marked-set bulk
	// action (Plan Phase 2 task 11): actions.Controller iterates it,
	// substituting each target's Namespace/ResourceName/
	// CronJobResourceVersion/CronJobGeneration onto a copy of this Scope for
	// one execution per target, and reports a BulkResultMsg with one
	// TargetResult per entry rather than a single joined error — so a
	// caller can tell which targets failed and retain only those marks.
	// Every other field on Scope (Verb, TriggerCreator, Schedule, …) stays
	// shared across every target. Empty for a single-target action, which
	// is the ordinary case.
	BulkTargets []BulkTarget
}

// BulkTarget names one object in a marked-set bulk action (Plan Phase 2
// task 11) — namespace-qualified so a bulk action can span namespaces in
// all-namespaces mode. ResourceVersion/Generation mirror TaskScope's own
// per-target precondition fields (CronJobResourceVersion/CronJobGeneration),
// since a bulk suspend/resume validates and stamps each target
// independently rather than sharing one precondition across all of them.
type BulkTarget struct {
	Namespace       string
	ResourceName    string
	ResourceVersion string
	Generation      int64
}

// TaskAction describes an operation available from a task screen.
// Confirmation-needed is derived from the Tier passed to
// actions.Controller.Begin (mvp-plan.md §8b), not a field here. Owner is an
// optional presentation hint for the 8b type-the-name modal — "Kind/name"
// of the controller that will recreate this resource once deleted (e.g.
// "ReplicaSet/nva-worker-abc123"), empty when unknown/not applicable.
type TaskAction struct {
	ID    string
	Label string
	Scope TaskScope
	Owner string
	// GracePeriodSeconds is the target pod's actual termination grace
	// period (8b's delete confirm shows this concrete figure per docs/
	// design README.md §8b, e.g. "30s") — nil when not known/applicable
	// (every non-Pod kind, or Pod kinds whose caller doesn't resolve it).
	GracePeriodSeconds *int64
}

// Task is the contract each active Bubble Tea task model must implement.
type Task interface {
	tea.Model
	SetSize(width, height int)
}

// Reloader is implemented by a Task whose rendered rows come from a cache it
// loaded into its own struct fields, rather than read live on every render
// (every list/detail screen in internal/tui/tasks — CLAUDE.md's "render
// functions are pure" means the load happens in Update, not View). Only the
// active task's Update sees kube.ResourceChangedMsg (see the plain
// m.task.Update(msg) forward below); a task sitting in m.stack while another
// is active misses every change event for as long as it's parked, so its
// cached rows silently drift from the cluster. BackMsg restoring it from the
// stack is exactly the moment that drift becomes visible, so it asks the
// resumed task to catch up immediately instead of leaving it to show stale
// data until some unrelated future change happens to reach it while active
// again — which, for a quiet kind, can be a very long wait. Reload should
// do the same thing the task's own kube.ResourceChangedMsg case does, so a
// resumed screen refreshes exactly like an active one would have.
//
// Reload need not re-arm the task's self-rescheduling chains (metrics poll,
// clocks, spinners): their ticks are routed to the task that armed them and
// held while it is parked (taskrouting.go), so popTask delivers them before
// calling Reload and each chain picks up where it stopped.
type Reloader interface {
	Reload() tea.Cmd
}

// EscapeBacker lets a pushed task whose ready-state Escape action is always
// "go back" ask the root to pop it synchronously. Returning BackMsg from a
// tea.Cmd leaves a small input race: the next key can arrive at the task being
// closed before Bubble Tea runs that command. Tasks with local Escape handling
// (an open editor or confirmation) report false until that state is closed.
type EscapeBacker interface {
	BackOnEscape() bool
}

// Transient is implemented by a Task that hands its place in the navigation
// stack to the task it swaps itself out for. tasks/objectdetail's 14d
// redirect is the case that needs it: an object with neither conditions nor
// events replaces itself with tasks/yamlview from inside its own load, so
// pushing it underneath traps Escape — popping back to it runs the very load
// that redirects to YAML again, and "esc walks back exactly one level"
// becomes a no-op loop the user can't leave. A task reporting true is dropped
// instead of pushed, so Escape from the replacement walks back to whatever
// pushed the redirecting task. Report it only for a swap the task performs
// itself, never for one the user asked for. Only pointer-receiver tasks can
// implement it usefully: the root asks the task after its Update has run.
type Transient interface {
	Transient() bool
}

// NamespaceSwitcher is implemented by a Task that retargets itself in place
// on SwitchNamespaceMsg (browse, events, whocan). For any other pushed
// screen the root answers the namespace palette the way it answers a goto,
// with a fresh browse pushed on top so esc walks back to the screen the user
// left — following the object when the screen is an ObjectScreen, else at
// the same kind (see namespaceRoute).
type NamespaceSwitcher interface {
	SwitchesNamespaceInPlace() bool
}

// ObjectScreen is implemented by a pushed Task bound to one namespaced
// object — the one browse's ↵ opens for that row. A namespace switch from it
// follows the object: the root routes a FollowObjectMsg to a fresh browse,
// which reopens the same-named object's screen in the new namespace when it
// exists there and otherwise stays on the list saying it isn't.
type ObjectScreen interface {
	ScreenObject() (kind kube.ResourceKind, namespace, name string)
}

// namespaceRoute decides what a namespace switch means for the active task:
// nil when the task handles it in place (NamespaceSwitcher) or already shows
// an object in that namespace, a FollowObjectMsg for an ObjectScreen, and the
// plain SwitchNamespaceMsg — a fresh browse at the same kind — otherwise.
// Switching to all namespaces ("") never follows: the object is still right
// where it was, and a name alone can't pick one row out of every namespace.
func namespaceRoute(task Task, msg SwitchNamespaceMsg) tea.Msg {
	if ns, ok := task.(NamespaceSwitcher); ok && ns.SwitchesNamespaceInPlace() {
		return nil
	}
	if obj, ok := task.(ObjectScreen); ok {
		kind, namespace, name := obj.ScreenObject()
		switch {
		case namespace == msg.Namespace:
			return nil
		case namespace != "" && msg.Namespace != "" && name != "":
			return FollowObjectMsg{Kind: kind, Namespace: msg.Namespace, Name: name}
		}
	}
	return msg
}

// BackMsg requests returning to the previous task without quitting the program.
type BackMsg struct{}

// Model wraps the active task and applies shared root-level TUI behavior:
// the task stack, the jump/namespace/context palette overlay, the help
// overlay, and connection state (mvp-plan.md §0.9). The overlay/mode
// machinery only engages for tasks implementing the Chrome v2 Screen
// contract — legacy (LegacyScreen) tasks get exactly today's passthrough
// behavior, so their own key handling (e.g. home's internal picker) is
// never shadowed.
type Model struct {
	task  Task
	stack []stackEntry
	// taskID identifies m.task's slot for ownedMsg routing (see
	// taskrouting.go): it survives the task's own value-copying Updates and
	// moves into m.stack with it when something is pushed on top. nextTaskID
	// is the last ID handed out; the zero value is the root task's.
	taskID, nextTaskID uint64

	width, height int
	mode          Mode
	conn          kube.ConnState
	palette       *palette.Model
	helpOpen      bool
	// quitConfirm is 'q's confirmation overlay (ctrl+q/ctrl+c stay each
	// task's own immediate-quit key, unchanged) — a root-shell global
	// exactly like helpOpen, so 'q' works from any screen without touching
	// every task's own update.go.
	quitConfirm bool
	session     *Session

	// probes/probeGen back the 7a context palette's lazily-streamed
	// reachability results (kube.ProbeContexts): probes holds what's
	// arrived so far, keyed by context name. probeGen is the current probe
	// run's generation (bumped by startContextProbe, mirroring browse's
	// reloadEpoch/metricsEpoch guard) — contextProbeMsg/contextProbesDoneMsg
	// carry the generation they belong to, so reopening/re-probing while a
	// previous run is still draining doesn't redirect the stale drain loop
	// onto the new run. probeCancel cancels the current run's context: a
	// re-probe, closing or replacing the palette (stopContextProbe) and
	// quitting all abandon in-flight probes, since each one may be holding
	// an exec credential plugin open for a palette nobody is looking at.
	probes      map[string]kube.ProbeResult
	probeGen    int
	probeCancel context.CancelFunc

	// namespaceItemsCache holds the 6a namespace palette's unfiltered item
	// list (live pod counts, +CPU shares once they land — see
	// namespaceCPUSharesMsg) for the palette session currently open.
	// namespaceItems does one informer Count call per namespace (fast,
	// cache-backed), so it's fetched once when the palette opens rather
	// than on every query edit — refetching per keystroke made typing feel
	// unresponsive whenever the round trip was slow (each fuzzy-filter edit
	// paid the full N-namespace fetch again). refreshNamespacePalette only
	// re-filters this cache.
	namespaceItemsCache []palette.Item
	// namespaceGen guards namespaceCPUSharesMsg against a stale fetch (from
	// a since-closed/reopened namespace palette) landing after a newer one
	// already replaced namespaceItemsCache — mirrors probeGen.
	namespaceGen int
	// gotoCorpus is the open jump palette's resource-name snapshot (see
	// gotoResourceCorpus). nil means "build it on the next fuzzy refresh":
	// it's reset when the palette opens, when the kind registry changes, and
	// on any watch event, so typing filters a snapshot instead of re-reading
	// every started cache per keystroke, yet never searches a stale one.
	gotoCorpus *gotoResourceCorpus
	// gotoGen guards gotoCountsMsg against a stale count fetch (from a
	// since-closed/reopened jump palette) landing after a newer one —
	// mirrors namespaceGen.
	gotoGen int

	// whoCanVerbItemsCache/whoCanResourceItemsCache hold tasks/whocan's (22a)
	// 'v'/'K' palette's unfiltered item lists for the palette session
	// currently open — both lists are static (a fixed verb vocabulary, the
	// registry's kind list), so unlike namespaceItemsCache there's no live
	// fetch to avoid repeating; the cache still exists so refresh*Palette can
	// re-filter by query without losing each row's "current" tag.
	whoCanVerbItemsCache     []palette.Item
	whoCanResourceItemsCache []palette.Item

	// neverConnected/showingSetup/buildSetup/buildBrowse back the 4c
	// "unreachable at launch" swap (mvp-plan.md Phase 4): neverConnected is
	// true from construction whenever a real (non-demo) cluster exists that
	// hasn't yet reported a successful connection; if the *first* signal
	// about it is trouble (Reconnecting/Failed) rather than Connected, the
	// root shell swaps the active task to tasks/setup (built via buildSetup,
	// since tui can't import tasks/setup — or tasks/browse for buildBrowse —
	// without an import cycle, the same constraint Session.HelpScope/
	// HelpList/HelpResource/HelpMisc already documents) and swaps back to a
	// fresh browse task
	// the moment a Connected state arrives. Once any Connected state or real
	// resource event has been observed, neverConnected latches false for good
	// — a later mid-session drop is 4a (handled entirely inside browse), not
	// this. A resource event is equally strong evidence because the eager
	// informer caches cannot deliver one without first reaching the cluster;
	// it also closes the startup window before the first periodic /livez probe.
	neverConnected bool
	showingSetup   bool
	// switching is set while a 7a context switch is in flight. Informer
	// events then come from a target the screen doesn't show yet (or from a
	// rollback): forwarding them reloaded the old kind and namespace against
	// half-filled caches under the old header, which could read as a false
	// empty state. SwitchContextMsg clears it, and both its outcomes reload.
	switching bool
	// switchSpinner animates the header's "switching…" badge while
	// switching is set (switchingBadge); built on the first switch.
	switchSpinner spinner.Model
	// rootResult is a root-level outcome line for the keybar — a failed
	// or refused 7a switch, which no task's actions.Controller owns. It
	// renders exactly like a task's ActionResult (Frame's ResultLine path)
	// and is dismissed by the next key press, as a task's is.
	rootResult  ResultLine
	buildSetup  func(kube.ConnState) Task
	buildBrowse func() Task

	// buildUpdate builds 28b's what's-new panel (WithUpdatePanel) — set
	// independently of buildSetup/buildBrowse above: unlike 4c/10b's
	// factories (WithRootFactories, only armed for a real, not-yet-
	// reachable cluster), 28b's "U from anywhere" must work in real, demo,
	// and no-cluster/setup mode alike, so app.NewModel's three branches all
	// call WithUpdatePanel regardless of which of those they're building.
	buildUpdate func() Task

	// keycastEnabled/keycast back --keycast, a demo-recording aid: a small
	// bottom-right chip echoing recent keypresses. Purely ephemeral shell UI
	// state — nothing outside the root shell ever reads it — so it lives
	// directly on Model next to helpOpen/quitConfirm/palette rather than on
	// Session.
	keycastEnabled bool
	keycast        keycastState
}

// New creates a root TUI model for the provided task, with no Session (no
// overlay/mode routing — legacy-screen behavior).
func New(task Task) Model {
	return Model{task: task, mode: ModeBrowse}
}

// NewWithSession creates a root TUI model with a Session, enabling the
// overlay/mode shell routing for tasks that implement Screen.
func NewWithSession(task Task, session *Session) Model {
	return Model{task: task, mode: ModeBrowse, session: session}
}

// WithRootFactories installs the 4c/10b task factories (see the doc comment
// on Model's neverConnected field) and arms neverConnected whenever session
// carries a live, not-yet-confirmed-reachable cluster. Composed onto
// NewWithSession's result by the composition root (internal/app), which is
// the only package that can import both tui and tasks/setup/tasks/browse.
// A no-op call (buildSetup nil) leaves 4c disabled — the --demo and
// no-kubeconfig-at-launch paths have no cluster to watch for that swap.
func (m Model) WithRootFactories(buildSetup func(kube.ConnState) Task, buildBrowse func() Task) Model {
	m.buildSetup = buildSetup
	m.buildBrowse = buildBrowse
	m.neverConnected = buildSetup != nil && m.session != nil && m.session.Cluster != nil
	return m
}

// WithUpdatePanel installs the 28b task factory 'U'/':update' push through
// (build's doc comment on Model.buildUpdate explains why this is a separate
// setter from WithRootFactories).
func (m Model) WithUpdatePanel(build func() Task) Model {
	m.buildUpdate = build
	return m
}

// WithKeycast enables/disables the --keycast bottom-right keypress chip. A
// no-op (false) call leaves it disabled — the default for every build that
// doesn't pass --keycast.
func (m Model) WithKeycast(enabled bool) Model {
	m.keycastEnabled = enabled
	return m
}

// WithInitialPush seeds the navigation stack with the model's current task
// and makes pushed the active one — the composition-root equivalent of a
// live in-app push (a task's Update returning a different task instance;
// see the Task-contract doc comment above), for cases that need to start
// already one level deep. The one caller today is a persisted
// Location.Kind of "Event" restoring straight to 9b (tasks/events) instead
// of browse's own stock Events list (browse.New's own doc comment covers
// why that list is never meant to be a resting screen) — esc from pushed
// pops back to the browse task exactly like pressing 'e' from it live
// would have gotten here in the first place.
func (m Model) WithInitialPush(pushed Task) Model {
	m.push()
	m.task = pushed
	m.taskID = m.allocTaskID()
	return m
}

// Mode is the current shell mode (drives the keybar pill while an overlay
// is open).
func (m Model) Mode() Mode { return m.mode }

// Conn is the last known connection state (from kube.ConnStateMsg).
func (m Model) Conn() kube.ConnState { return m.conn }

// Session is the shell's cross-screen state, or nil if the model wasn't
// built with one.
func (m Model) Session() *Session { return m.session }

// Screen names the active task's concrete type ("browse.Model"). It exists
// for diagnostics — a crash report that says which screen the user was
// looking at — which is why it hands back a string rather than the Task
// itself: nothing outside the shell may drive the active task.
func (m Model) Screen() string {
	if m.task == nil {
		return ""
	}
	return reflect.TypeOf(m.task).String()
}

// Theme returns the session's active theme, or Dark() before a session is
// wired in (openPalette and friends need a Theme to style their embedded
// textfield.Model even in that window).
func (m Model) Theme() Theme {
	if m.session == nil {
		return Dark()
	}
	return m.session.Theme
}

// PaletteOpen reports whether the jump/namespace/context palette overlay is
// showing.
func (m Model) PaletteOpen() bool { return m.palette != nil }

// HelpOpen reports whether the help overlay is showing.
func (m Model) HelpOpen() bool { return m.helpOpen }

// QuitConfirmOpen reports whether 'q's quit confirmation overlay is
// showing.
func (m Model) QuitConfirmOpen() bool { return m.quitConfirm }

func (m Model) Init() tea.Cmd {
	return m.taskInit()
}

// resizeTask pushes the last known terminal size onto a task that was just
// swapped in outside the normal push path (4c's setup↔browse swaps,
// ReplaceRootMsg) — a fresh task is built at tui.Default* dimensions and
// would otherwise render letterboxed until the user resizes the terminal. A
// no-op before the first WindowSizeMsg, so such a task keeps its defaults.
func (m Model) resizeTask() {
	if m.width > 0 && m.height > 0 {
		m.task.SetSize(m.width, m.height)
	}
}

// setAllSizes resizes the active task and every task sitting in m.stack.
// A live push's child normally inherits its parent's already-correct
// current width/height at construction time (the parent built it with
// m.width/m.height), so only the newly active task ever needs resizing —
// but WithInitialPush seeds the stack before the runtime has delivered any
// real terminal size at all (both tasks start at tui.Default* dimensions),
// so the one tea.WindowSizeMsg that follows must reach the whole stack too,
// or the seeded parent renders letterboxed the moment esc pops back to it.
func (m Model) setAllSizes(width, height int) {
	m.task.SetSize(width, height)
	for _, e := range m.stack {
		e.task.SetSize(width, height)
	}
}

// activate makes t the active task: it resizes it to the last known
// terminal size and replays the root's connection state into it. Every
// place the root swaps m.task (push, pop, goto routing, 28b, 4c's swaps,
// ReplaceRootMsg, the context-switch rebuild) goes through here, because the
// root only forwards a kube.ConnStateMsg to whichever task is active when it
// arrives: a freshly built task starts with a zero ConnState, which reads as
// online, and a parked task misses every transition that happened while
// something else was on top. Without the replay, a screen opened or
// returned to during an outage claims "connected", keeps its metrics poll
// running and leaves its mutating verbs enabled — and during
// ConnUnauthenticated nothing ever corrects it, since the health loop
// deliberately stops pinging.
//
// The replay is a synchronous Update call, not a tea.Cmd, so the very first
// frame (and the very next key press) already sees the right state. It is
// skipped until the root has seen a state at all (Phase ""), leaving the
// task's own default. Every ConnStateMsg handler only acts on an edge, so
// replaying a state the task already has is a no-op. Callers that also
// Init the task do so after activate, so a fresh screen opened offline
// never arms a poll in the first place.
//
// id is the slot's routing identity (see taskrouting.go): a task the root
// has just built takes a fresh one (activateFresh), a popped task gets its
// own back so the timer and load messages held for it while it was parked
// reach it again.
func (m *Model) activate(t Task, id uint64) tea.Cmd {
	m.task = t
	m.taskID = id
	m.resizeTask()
	return m.replayConn()
}

// activateFresh activates a task the root has just built, under a new
// routing identity.
func (m *Model) activateFresh(t Task) tea.Cmd {
	return m.activate(t, m.allocTaskID())
}

// replayConn delivers the root's current connection state to the active
// task (see activate).
func (m *Model) replayConn() tea.Cmd {
	if m.conn.Phase == "" || m.task == nil {
		return nil
	}
	updated, cmd := m.task.Update(kube.ConnStateMsg(m.conn))
	if t, ok := updated.(Task); ok {
		m.task = t
	}
	return ownCmd(m.taskID, cmd)
}

// routeGoto is what makes a jump "fire the same goto machinery as g": when
// the active task is already the resting browse view (empty stack), it
// reports ok=false and the caller falls through to the existing
// bottom-of-Update forward, so browse mutates itself in place exactly as
// switching kind from its own resting state always has (no stack growth
// for ordinary kind-switching from browse). Otherwise it builds a fresh
// browse instance (m.buildBrowse), retargets it with msg, and pushes the
// task that fired the jump onto the stack beneath it — so Escape walks
// back exactly one level to it, the same contract every other pushed
// screen already gives (CLAUDE.md: "esc always walks back exactly one
// level"). Both GotoKindMsg (12a's kind-result Enter) and GotoResourceMsg
// (12a's resource-result Enter, and every screen's own pre-filled "jump to
// related object" action) route through here identically — there is
// nothing palette-specific about it. ok is false with no buildBrowse
// factory wired (the 10b no-kubeconfig setup screen never fires a jump, so
// this is an untested but harmless dead branch).
func (m *Model) routeGoto(msg tea.Msg) (tea.Cmd, bool) {
	// Any jump is the user choosing where to be, so a kind browse was
	// holding for discovery (Session.HoldPendingKind) no longer applies —
	// and must not seed the fresh instance built below.
	m.session.ClearPendingKind()
	if len(m.stack) == 0 || m.buildBrowse == nil {
		return nil, false
	}
	fresh := m.buildBrowse()
	m.push()
	// Activate (connection replay) first, then Init, then the jump itself:
	// an offline replay must land before Init decides whether to arm polls,
	// and Init must run at all — nothing else ever starts a fresh
	// instance's load, so a jump to the kind/namespace the Session already
	// names would otherwise sit on "Loading …" forever. Retargeting after
	// Init lets a real kind/namespace switch bump browse's epochs, so the
	// superseded Init load and metrics chain are dropped on arrival.
	activated := m.activateFresh(fresh)
	initCmd := m.taskInit()
	updated, cmd := m.task.Update(msg)
	if task, ok := updated.(Task); ok {
		m.task = task
	}
	return tea.Batch(activated, initCmd, ownCmd(m.taskID, cmd)), true
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Unwrap a reconnected cluster's forwarded event/conn message (see
	// clusterwatch.go) to the same kube types the switch below and every
	// task already handle, and re-arm the drain — from here on this frame
	// behaves exactly as if the original forwardEvents goroutine had sent
	// msg directly.
	var rewatch tea.Cmd
	switch wm := msg.(type) {
	case ownedMsg:
		m.settleRebuild(wm.msg)
		return m.routeOwned(wm)
	case clusterEventMsg:
		msg = wm.inner
		rewatch = WatchCluster(wm.events, wm.conn)
	case clusterConnMsg:
		msg = wm.inner
		rewatch = WatchCluster(wm.events, wm.conn)
	}

	switch msg := msg.(type) {
	case ReplaceRootMsg:
		// A different cluster from here on: the last state belonged to the
		// one just replaced, and the new one reports its own over msg.Conn.
		// Replaying the old outage would mark the reconnected browse
		// offline.
		m.conn = kube.ConnState{}
		m.settleRebuild(msg)
		task := msg.Task
		if msg.Commit != nil {
			task = msg.Commit()
		}
		m.activateFresh(task)
		m.stack = nil
		m.showingSetup = false
		m.buildSetup = msg.BuildSetup
		m.buildBrowse = msg.BuildBrowse
		m.neverConnected = msg.BuildSetup != nil
		cmds := []tea.Cmd{m.taskInit()}
		if msg.Events != nil {
			cmds = append(cmds, WatchCluster(msg.Events, msg.Conn))
		}
		return m, tea.Batch(cmds...)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.setAllSizes(msg.Width, msg.Height)
		m.clampPaletteViewport()
	case WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.setAllSizes(msg.Width, msg.Height)
		m.clampPaletteViewport()
	case BackMsg:
		return m, m.popTask()
	case kube.ConnStateMsg:
		// Also forwarded to the task below (unchanged msg), so browse can
		// render its own stale strip (Phase 4).
		m.conn = kube.ConnState(msg)
		// The cluster's own reached-the-apiserver latch is connection proof
		// even if all of its Add events landed before Bubble Tea began
		// consuming the event stream and the first periodic /livez result has
		// not arrived yet. A WATCH can end in that narrow window (the logs
		// lifecycle E2E closes every active request deliberately); that is a
		// mid-session outage with useful cached state, not an unreachable
		// launch that should discard the active task stack for setup.
		//
		// Deliberately Reached and not Synced: Synced waits for the *whole*
		// eager set, but rows reach the screen as soon as the Pod cache fills,
		// so an outage between those two moments used to read as "never
		// reachable" and replace a browse screen listing real pods with 4c's
		// "unreachable · NO CLUSTER". Asking a connect-completion latch a
		// question about cached state is the same mistake CLAUDE.md documents
		// for loading states.
		if m.neverConnected && m.session != nil && m.session.Cluster != nil && m.session.Cluster.Reached() {
			m.neverConnected = false
		}
		switch {
		case m.conn.Phase == kube.ConnConnected:
			m.neverConnected = false
			if m.showingSetup && m.buildBrowse != nil {
				m.showingSetup = false
				replay := m.activateFresh(m.buildBrowse())
				m.stack = nil
				next := tea.Batch(replay, m.taskInit())
				if rewatch != nil {
					next = tea.Batch(next, rewatch)
				}
				return m, next
			}
		case m.neverConnected && !m.showingSetup && m.buildSetup != nil && m.conn.Offline():
			m.showingSetup = true
			replay := m.activateFresh(m.buildSetup(m.conn))
			m.stack = nil
			next := tea.Batch(replay, m.taskInit())
			if rewatch != nil {
				next = tea.Batch(next, rewatch)
			}
			return m, next
		}
	case GotoKindMsg:
		// routeGoto first, Session.Location write after: when it pushes a
		// fresh browse view, that view's own browse.New reads
		// Session.Location.Kind/Namespace to seed its *starting* kind/
		// namespace — writing the destination there before building it
		// would make the fresh instance start already "at" msg.Kind, so
		// its own goToResource sees no change and never issues a load,
		// leaving it stuck on the loading spinner forever (the bug this
		// ordering fixes). browse.goToResource re-syncs Location.Kind/
		// Namespace itself once it actually processes the change (in both
		// the push and in-place-forward paths), so writing them here again
		// afterward is a harmless, idempotent overwrite — kept for the
		// in-place path, where it's still the only place Session.Location
		// stays the single source of truth for the breadcrumb ahead of
		// browse's own Update forwarding it (mvp-plan.md Phase 2).
		cmd, pushed := m.routeGoto(msg)
		if m.session != nil {
			m.session.Location.Kind = msg.Kind
		}
		if pushed {
			return m, cmd
		}
	case GotoResourceMsg:
		cmd, pushed := m.routeGoto(msg)
		if m.session != nil {
			m.session.Location.Kind = msg.Kind
			m.session.Location.Namespace = msg.Namespace
			m.session.Location.Resource = msg.Name
		}
		if pushed {
			return m, cmd
		}
	case SwitchNamespaceMsg:
		// routeGoto before the Location write, for the same reason as
		// GotoKindMsg: the fresh browse seeds its namespace from
		// Session.Location, and seeding it with the destination would make
		// its own switchNamespace a no-op that never loads.
		var cmd tea.Cmd
		pushed := false
		route := namespaceRoute(m.task, msg)
		if route != nil {
			cmd, pushed = m.routeGoto(route)
		}
		if m.session != nil {
			m.session.Location.Namespace = msg.Namespace
			if f, ok := route.(FollowObjectMsg); ok && pushed {
				m.session.Location.Kind = f.Kind
				m.session.Location.Resource = f.Name
			}
		}
		if pushed {
			return m, cmd
		}
	case SwitchContextMsg:
		m.switching = false
		m.settleRebuild(msg)
		m.rootResult = switchFailureResult(msg)
		if m.session != nil && msg.Err != nil && msg.RolledBack {
			// The original context was rebuilt in place: same Location, but
			// fresh caches and a fresh discovery.
			m.session.InvalidateCounts()
			if m.session.Cluster != nil {
				m.session.Registry, m.session.Groups = resources.BuildDiscoveredRegistry(m.session.Cluster.DiscoveredKinds(), m.session.Cluster)
			}
		}
		// Also forwarded to the task below (unchanged msg), so browse can
		// switch its own kind/namespace and reload against the rebuilt
		// cluster (mvp-plan.md Phase 3, 7a). Err set means the switch
		// failed — Session.Location and the task both stay put. The
		// registry/groups rebuild happens here (main Update goroutine),
		// not inside switchContextCmd's closure — that closure already
		// documents "only touches the stable *kube.Cluster pointer" to
		// avoid racing the render goroutine's Session reads, and
		// cluster.SwitchContext (which folds discovery in) has already
		// completed synchronously by the time this message arrives.
		if m.session != nil && msg.Err == nil {
			// The rebuilt cluster starts from a fresh health state and
			// reports its own transitions; the old context's outage must not
			// be replayed into the destination's screens.
			m.conn = kube.ConnState{}
			m.session.Location = Location{Context: msg.Context, Namespace: msg.Namespace, Kind: msg.Kind, Filter: msg.Filter}
			// Every memoized count belongs to the cluster just left.
			m.session.InvalidateCounts()
			if m.session.Cluster != nil {
				m.session.Registry, m.session.Groups = resources.BuildDiscoveredRegistry(m.session.Cluster.DiscoveredKinds(), m.session.Cluster)
			}
			// A context switch has one explicit contract: discard the
			// old-context task stack and return to a fresh browse screen
			// restored for the destination context. Rebuilding an equivalent
			// detail/log/editor would risk carrying old object identity or an
			// old stream into the new frame — and the resting browse is no
			// exception: it captured the old context's user (stamped on every
			// run-now's triggered-by annotation) and cluster name at build.
			// The fresh browse still gets the message, which is what applies
			// the destination's remembered filter.
			if m.buildBrowse != nil {
				m.activateFresh(m.buildBrowse())
				m.stack = nil
				updated, cmd := m.task.Update(msg)
				if t, ok := updated.(Task); ok {
					m.task = t
				}
				return m, tea.Batch(m.taskInit(), ownCmd(m.taskID, cmd))
			}
		}
	case kube.ResourceChangedMsg:
		// Informer traffic proves this session has connected even when it lands
		// before health's first periodic ConnConnected message. Without this
		// latch, a fast post-start outage could be mistaken for an unreachable
		// launch and replace a useful cached browse/log screen with setup.
		m.neverConnected = false
		// Any object may have come or gone: the jump palette's name snapshot
		// is rebuilt on its next refresh rather than here, so an event storm
		// costs nothing while nobody is typing.
		m.gotoCorpus = nil
		// A custom kind's printer columns have just arrived (they're fetched
		// per-kind on first read, not at connect), so the kind registry is
		// stale. Rebuild and let the message carry on to the task, which
		// re-reads its own descriptor. Only on that signal: an ordinary CRD
		// object event changes nothing the registry is built from, and 14b's
		// informer delivers one per CRD on its initial sync.
		if msg.ColumnsArrived && m.session != nil && m.session.Cluster != nil {
			m.session.Registry, m.session.Groups = resources.BuildDiscoveredRegistry(m.session.Cluster.DiscoveredKinds(), m.session.Cluster)
			m.refreshGotoPaletteCorpus()
		}
		if m.switching {
			return m, nil // see switching's doc comment
		}
	case kube.CRDsDiscoveredMsg:
		// The one connect path where discovery finishes outside any
		// tea.Cmd the root can await synchronously (context.go's
		// switchContextCmd and app.attemptReconnect both already rebuild
		// the registry inline, before their own result message is
		// returned) — see kube.CRDsDiscoveredMsg's doc comment. Swallowed
		// (not forwarded to the task): browse.switchKind already re-reads
		// Session.Registry fresh on every kind switch, so there's nothing
		// for the active task to react to here.
		if m.session != nil && m.session.Cluster != nil {
			m.session.Registry, m.session.Groups = resources.BuildDiscoveredRegistry(m.session.Cluster.DiscoveredKinds(), m.session.Cluster)
			m.refreshGotoPaletteCorpus()
		}
		// The launch kind browse had to fall back from, now resolvable:
		// open it, unless the user has since opened something on top.
		if pending := m.session.PendingKind(); pending != "" && len(m.stack) == 0 {
			if _, ok := m.session.Registry.Descriptor(pending); ok {
				return m, func() tea.Msg { return GotoKindMsg{Kind: pending} }
			}
		}
		return m, nil
	case UpdateCheckedMsg:
		// Also forwarded to the task below (unchanged msg): tasks/update
		// (28b) needs it too, to clear its own "checking…" state after 'r'
		// — same reasoning as kube.ConnStateMsg's forward, just below.
		if m.session != nil {
			m.session.UpdateCheckErr = msg.Err
			if msg.Err == nil {
				info := msg.Info
				m.session.Update = &info
				m.session.State.UpdateCheck.LastChecked = msg.CheckedAt
				m.session.State.UpdateCheck.LatestVersion = msg.LatestVersion
			}
		}
	case OpenUpdatePanelMsg:
		return m, m.openUpdatePanel()
	case contextProbeMsg:
		// A stale gen (a since-superseded probe run) still drains its own
		// channel to completion — see contextProbeMsg's doc comment — but
		// its result isn't applied to m.probes/the palette.
		if msg.gen == m.probeGen {
			if m.probes == nil {
				m.probes = map[string]kube.ProbeResult{}
			}
			m.probes[msg.res.Name] = msg.res
			if m.palette != nil && m.palette.Scope == palette.ScopeContext {
				m.refreshPalette()
			}
		}
		return m, waitForProbe(msg.gen, msg.ch)
	case contextProbesDoneMsg:
		return m, nil
	case gotoCountsMsg:
		// A stale gen (a since-closed/reopened jump palette) is dropped —
		// see gotoGen's doc comment. The counts themselves are already in
		// the session cache; this just prompts a re-render with them.
		if msg.gen == m.gotoGen && m.palette != nil && m.palette.Scope == palette.ScopeGoto {
			m.refreshGotoPalette()
		}
		return m, nil
	case namespaceCPUSharesMsg:
		// A stale gen (a since-closed/reopened namespace palette) is
		// dropped — see namespaceGen's doc comment.
		if msg.gen == m.namespaceGen {
			applyNamespaceCPUShares(m.namespaceItemsCache, msg.shares)
			if m.palette != nil && m.palette.Scope == palette.ScopeNamespace {
				m.refreshNamespacePalette()
			}
		}
		return m, nil
	case namespaceSyncRetryMsg:
		// A stale gen (a since-closed/reopened namespace palette) is
		// dropped — see namespaceGen's doc comment.
		if msg.gen != m.namespaceGen || m.palette == nil || m.palette.Scope != palette.ScopeNamespace {
			return m, nil
		}
		return m, m.loadNamespacePalette(msg.gen)
	case KeycastTickMsg:
		if m.keycast.prune(time.Time(msg)) {
			return m, keycastTick()
		}
		return m, nil
	case tea.PasteMsg, tea.ClipboardMsg:
		// An overlay owns text entry while it's up, so a paste has to be
		// claimed here — falling through would insert it into the task
		// rendered *underneath* the palette, e.g. browse's filter box.
		if cmd, ok := m.pasteQuery(msg); ok {
			return m, tea.Batch(cmd, rewatch)
		}
		if m.helpOpen || m.quitConfirm {
			return m, rewatch // no buffer to paste into; swallow
		}
		// Otherwise it belongs to the active task, reached below.
	case ClusterRebuildFailedMsg:
		m.settleRebuild(msg)
	case spinner.TickMsg:
		if cmd, ok := m.tickSwitchSpinner(msg); ok {
			return m, cmd
		}
	case tea.KeyPressMsg:
		// A root-level outcome line lasts until the user's next key, the
		// same contract as a task's DismissResult.
		m.rootResult = ResultLine{}
		var keycastCmd tea.Cmd
		if m.keycastEnabled {
			keycastCmd = m.keycast.record(msg, time.Now())
		}
		// After the keycast record (the chord still shows in a demo) and
		// before handleShellKey, which would otherwise route ctrl+v through
		// the palette's own key handling.
		if cmd, ok := m.pasteQuery(msg); ok {
			return m, tea.Batch(cmd, keycastCmd)
		}
		if _, ok := m.task.(Screen); ok && !taskCapturingInput(m.task) {
			if handled, next, cmd := m.handleShellKey(msg); handled {
				return next, tea.Batch(cmd, keycastCmd)
			}
		}
		if msg.String() == "esc" && m.palette == nil && !m.helpOpen && !m.quitConfirm {
			if backer, ok := m.task.(EscapeBacker); ok && backer.BackOnEscape() && len(m.stack) > 0 {
				return m, tea.Batch(m.popTask(), rewatch, keycastCmd)
			}
		}
		rewatch = tea.Batch(rewatch, keycastCmd)
	}

	cmd := m.forward(msg)
	if rewatch != nil {
		cmd = tea.Batch(cmd, rewatch)
	}

	return m, cmd
}

// popTask restores one task-stack level and refreshes the resumed task when
// necessary. Both BackMsg and EscapeBacker's synchronous path use this so
// their reload behavior stays identical.
func (m *Model) popTask() tea.Cmd {
	if len(m.stack) == 0 {
		return nil
	}
	entry := m.stack[len(m.stack)-1]
	m.stack = m.stack[:len(m.stack)-1]
	cmds := []tea.Cmd{m.activate(entry.task, entry.id)}
	// Catch up on what arrived for it while it was parked — its timer
	// ticks above all, which are what keep its self-rescheduling chains
	// (metrics poll, spinners, clocks, cache-sync retries) alive — after the
	// connection replay, so a chain resumed during an outage sees the
	// outage first and doesn't re-arm a poll.
	cmds = append(cmds, m.deliverHeld(entry))
	if m.taskID != entry.id {
		// A held message made the resumed task push or replace itself;
		// the refresh below belongs to it, and it now sits parked.
		return tea.Batch(cmds...)
	}
	if r, ok := m.task.(Reloader); ok {
		cmds = append(cmds, ownCmd(m.taskID, r.Reload()))
	}
	return tea.Batch(cmds...)
}

// handleShellKey routes keys the root shell owns: overlay navigation while
// a palette/help/quit-confirm panel is open, otherwise g/n/c/q/? to open
// one. Reports false when the key isn't the shell's to handle, so the
// caller falls through to the task.
func (m Model) handleShellKey(msg tea.KeyPressMsg) (bool, Model, tea.Cmd) {
	if m.quitConfirm {
		switch msg.String() {
		case "y", "enter":
			return true, m, tea.Quit
		case "n", "esc":
			m.quitConfirm = false
			m.mode = ModeBrowse
		}
		return true, m, nil
	}
	if m.palette != nil {
		return m.handlePaletteKey(msg)
	}
	if m.helpOpen {
		switch msg.String() {
		case "esc", "?":
			m.helpOpen = false
			m.mode = ModeBrowse
		}
		return true, m, nil
	}
	if m.session == nil {
		return false, m, nil
	}
	if wc, ok := m.task.(WhoCanScoped); ok {
		switch msg.String() {
		case "v":
			verb, _ := wc.WhoCanQuery()
			return true, m, m.openVerbPalette(verb)
		case "K":
			// Capital K, not lowercase k — whocan's own updateKey already
			// binds "up"/"k" to move-selection-up, so a lowercase intercept
			// here would permanently steal j/k movement from that one screen
			// (mvp-tasks.md/CLAUDE.md: "j/k ≡ ↑↓ everywhere").
			_, resource := wc.WhoCanQuery()
			return true, m, m.openResourcePalette(resource)
		}
	}
	switch msg.String() {
	case "g", ":":
		return true, m, m.openPalette(palette.ScopeGoto, "›", "jump anywhere")
	case "n":
		return true, m, m.openPalette(palette.ScopeNamespace, "ns ›", "")
	case "c":
		return true, m, m.openPalette(palette.ScopeContext, "ctx ›", "")
	case "U":
		return true, m, m.openUpdatePanel()
	case "q":
		m.quitConfirm = true
		m.mode = ModeConfirm
	case "?":
		m.helpOpen = true
		m.mode = ModeHelp
	default:
		return false, m, nil
	}
	return true, m, nil
}

// handlePaletteKey drives the open palette: linear navigation, typing/
// backspace re-filtering, Enter's navigation dispatch (per-scope:
// gotoDispatch/namespaceDispatch/contextDispatch), ctrl+r re-probing and
// ctrl+p mark/unmark-prod on the context palette. Every scope shares one
// alt-tab grammar (docs/design
// README.md §2b/§6a/§7a): opening the palette pre-selects the most recent
// *other* entry (mostRecentOther), so a bare open+Enter with no typing toggles
// straight back to it — same two keystrokes the old "n n"/"c c" double-tap
// used, now visible through the palette instead of bypassing it.
func (m Model) handlePaletteKey(msg tea.KeyPressMsg) (bool, Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.palette = nil
		m.stopContextProbe()
		m.mode = ModeBrowse
	case "enter":
		if m.palette.Scope == palette.ScopeNamespace && m.palette.Denied {
			// The denied namespace palette has no Items to select — Enter
			// commits whatever the user typed as the switch target instead
			// (Model.Denied's doc comment).
			target := strings.TrimSpace(m.palette.Query())
			m.palette = nil
			m.stopContextProbe()
			m.mode = ModeBrowse
			if target == "" {
				return true, m, nil
			}
			pushRecentNamespace(m.session, target)
			return true, m, func() tea.Msg { return SwitchNamespaceMsg{Namespace: target} }
		}
		item, ok := m.palette.Selected()
		scope := m.palette.Scope
		m.palette = nil
		m.stopContextProbe()
		m.mode = ModeBrowse
		if !ok {
			return true, m, nil
		}
		var cmd tea.Cmd
		switch scope {
		case palette.ScopeGoto:
			// The goto palette can be opened from any Screen, not just the
			// root browse task (e.g. poddetail). No stack surgery needed
			// here any more: the cmd below delivers a GotoKindMsg/
			// GotoResourceMsg/OpenUpdatePanelMsg on a later Update cycle,
			// and routeGoto (triggered from those message cases) is what
			// pushes a fresh browse view and keeps the screen the palette
			// was opened from one esc-back away — the same machinery every
			// pre-filled "jump to related object" action now shares.
			cmd = gotoDispatch(m.session, item)
		case palette.ScopeNamespace:
			cmd = namespaceDispatch(m.session, item)
		case palette.ScopeContext:
			cmd = contextDispatch(m.session, item)
			if cmd != nil {
				cmd = tea.Batch(cmd, m.beginSwitchFeedback())
			}
		case palette.ScopeVerb:
			cmd = whoCanVerbDispatch(item)
		case palette.ScopeResource:
			cmd = whoCanResourceDispatch(item)
		}
		if cmd != nil {
			return true, m, cmd
		}
	// No plain j/k synonyms here, unlike list screens: the palette is a text
	// input, and 12a's "type to narrow" must let those letters reach the
	// query (its keybar advertises ↑↓ accordingly). ctrl+j/ctrl+k are safe
	// additions though — a control-modified key never carries Text
	// (charm.land/bubbletea/v2's Key.Text doc), so they can't leak into the
	// query and fall through to typeChar's default case.
	case "up", "ctrl+k":
		m.movePalette(-1)
	case "down", "ctrl+j":
		m.movePalette(1)
	case "ctrl+d":
		m.movePaletteHalfPage(1)
	case "ctrl+u":
		m.movePaletteHalfPage(-1)
	case "tab":
		// docs/design README.md §2b: "tab complete" fills the query in to the
		// highlighted result's own label, so a fuzzy match can be completed
		// then narrowed further rather than jumped to immediately.
		if item, ok := m.palette.Selected(); ok {
			m.palette.Input.SetValue(item.Label)
			m.palette.Input.CursorEnd()
			m.palette.Browse = false
			m.refreshPalette()
		}
	case palette.KeyContextReprobe:
		// Ctrl-chorded rather than a bare 'r', which "prod", "staging" and
		// every "arn:…" context contain — typing "prod" filtered on "pod"
		// and re-probed every context instead.
		if m.palette.Scope == palette.ScopeContext {
			return true, m, m.startContextProbe(true)
		}
	case palette.KeyContextMarkProd:
		// Ctrl-chorded rather than a bare letter: "P"/"p" can lead a
		// context name ("Prod-EKS") and must keep reaching the fuzzy query
		// instead of rewriting config.yaml's prodContexts mid-word.
		if m.palette.Scope == palette.ScopeContext {
			m.toggleSelectedContextProd()
		}
	default:
		return true, m, m.typeKey(msg)
	}
	return true, m, nil
}

// otherRecents filters current out of recents (most-recent-first, see
// state.State's doc comment), preserving order. Every numbered/alt-tab
// mechanic — mostRecentOther's bare-Enter target, digitRecentTarget's
// query-digit lookup, recentNumbers' gutter-digit assignment — indexes
// against this same "recents minus current" list, so they always agree on
// what "1" means.
func otherRecents(recents []string, current string) []string {
	out := make([]string, 0, len(recents))
	for _, r := range recents {
		if r != current {
			out = append(out, r)
		}
	}
	return out
}

// mostRecentOther returns the first entry in recents that isn't current, or
// ("", false) if there is none — the shared alt-tab target every scope's
// *BrowseSelection helper (gotoBrowseSelection, namespaceBrowseSelection,
// contextBrowseSelection) resolves against, and the same entry recentNumbers
// assigns digit "1" to.
func mostRecentOther(recents []string, current string) (string, bool) {
	others := otherRecents(recents, current)
	if len(others) == 0 {
		return "", false
	}
	return others[0], true
}

// numberedRecents is otherRecents with its own first entry (index 0, the
// "previous" alt-tab target) also dropped: current and previous both
// already have their own on-row tag ("current"/"previous"), so repeating
// them in the numbered pick or the RECENT summary row would be redundant —
// digit "1" is whatever's next after that. mostRecentOther/the bare-Enter
// alt-tab toggle are unaffected — they keep resolving against otherRecents,
// not this. Shared by recentNumbers (row gutter), the RECENT row's label
// list (namespaceRecentLabels/contextRecentLabels), and
// refreshNamespacePalette/refreshContextPalette's digitRecentTarget lookup,
// so all three always agree on what "1" means.
func numberedRecents(recents []string, current string) []string {
	others := otherRecents(recents, current)
	if len(others) == 0 {
		return nil
	}
	return others[1:]
}

// recentNumbers assigns each of numberedRecents' entries a 1-based digit,
// most-recent-first, capped at 9 (only '1'-'9' are addressable from the
// keyboard). Used both to render each row's gutter digit
// (palette.Item.RecentNum) and, via numberedRecents, to resolve
// digitRecentTarget's query-digit lookup — so the number on screen always
// matches what typing it does.
func recentNumbers(recents []string, current string) map[string]int {
	others := numberedRecents(recents, current)
	if len(others) > 9 {
		others = others[:9]
	}
	nums := make(map[string]int, len(others))
	for i, r := range others {
		nums[r] = i + 1
	}
	return nums
}

// promoteRecentItems reorders items in place so current, then the row
// tagged "previous", then the numbered 1-9 recents (in digit order) lead —
// current/previous/recentNumbers must already be applied to each item's
// Tag/RecentNum before calling this. Everything else keeps its existing
// relative order (stable sort). Callers pass only the ordinary namespace/
// context rows; namespace's pinned "all namespaces" row is appended after
// promotion and therefore stays at the bottom of the scrollable list.
//
// This only visibly affects the empty-query browse state: once a query is
// typed, palette.Filter's fuzzy.Find re-sorts by match score regardless of
// input order, so a promoted item's position here has no effect on filtered
// results — exactly the "recency ordering, not while searching" behavior
// wanted (docs/design README.md §6a/§7a).
func promoteRecentItems(items []palette.Item) {
	priority := func(it palette.Item) int {
		switch {
		case it.Tag == "current":
			return 0
		case it.Tag == "previous":
			return 1
		case it.RecentNum > 0:
			return 1 + it.RecentNum // 2..10, in digit order
		default:
			return 1 << 30
		}
	}
	slices.SortStableFunc(items, func(a, b palette.Item) int {
		return cmp.Compare(priority(a), priority(b))
	})
}

// digitRecentTarget resolves a typed query to a 1-based index into others
// (a numberedRecents result — current and previous both already excluded,
// matching recentNumbers' gutter digits) — so numbers give direct keyboard
// access to what's already on screen. Only a single-digit '1'-'9' query
// matches; anything else (empty, two-plus characters, a non-digit, or a
// digit past the end of others) returns false and the caller falls back to
// normal fuzzy filtering. This mirrors gotoAliasMatch's single-rune-only
// rule: typing a second character makes the digit "just the query's first
// char" instead of a shortcut, so e.g. "2048" still filters to a
// namespace/context named that.
func digitRecentTarget(query string, others []string) (string, bool) {
	r := []rune(query)
	if len(r) != 1 {
		return "", false
	}
	d := r[0]
	if d < '1' || d > '9' {
		return "", false
	}
	n := int(d - '0')
	if n > len(others) {
		return "", false
	}
	return others[n-1], true
}

// recentPickHint is 6a/7a's shared RECENT-row trailing hint — "1-9 recent ·
// ↵ toggles last" — advertising both digitRecentTarget's numbered shortcut
// (whose digits also appear directly on the rows themselves, recentNumbers)
// and the alt-tab bare-Enter grammar in one line.
func recentPickHint() []palette.FooterSpan {
	return []palette.FooterSpan{
		{Text: "1-9", Tone: palette.FooterKey},
		{Text: " recent · ", Tone: palette.FooterDim},
		{Text: "↵", Tone: palette.FooterKey},
		{Text: " toggles last", Tone: palette.FooterDim},
	}
}

// typeKey routes a keypress that isn't one of the palette's own chords into
// the query box's textfield.Model — this is where backspace, left/right,
// Home/End and Ctrl-arrow word-jump arrive for free (paste does not — see
// pasteQuery, since a bracketed paste is never a keypress) — then
// re-filters, common to every scope's default typing/editing path.
func (m *Model) typeKey(msg tea.KeyPressMsg) tea.Cmd {
	var cmd tea.Cmd
	m.palette.Input, cmd = m.palette.Input.Update(msg)
	return m.afterQueryEdit(cmd)
}

// afterQueryEdit re-filters the palette after any edit to the query box —
// shared by typeKey's typing and pasteQuery's paste, since a pasted query has
// to re-rank the list exactly as a typed one does.
func (m *Model) afterQueryEdit(cmd tea.Cmd) tea.Cmd {
	m.palette.Browse = m.palette.Scope == palette.ScopeGoto && m.palette.Query() == ""
	m.refreshPalette()
	return cmd
}

// pasteQuery routes a paste into the open palette's query box. Reports false
// when there's no palette open or msg isn't a paste, leaving the caller to
// carry on — including the case that matters most: with no overlay open a
// paste belongs to the active task's own buffer, not to the shell.
func (m *Model) pasteQuery(msg tea.Msg) (tea.Cmd, bool) {
	if m.palette == nil {
		return nil, false
	}
	cmd, ok := RoutePaste(msg, PasteInto(&m.palette.Input))
	if !ok {
		return nil, false
	}
	return m.afterQueryEdit(cmd), true
}

// startContextProbe (re)opens the 7a context palette's data: reset probe
// results, populate items immediately (every probed row starts
// "probing…"), and kick off a fresh kube.ProbeContexts run — used both by
// openPalette and by the palette's own ctrl+r re-probe key.
//
// all is false on open: a context whose user mints credentials through an
// exec or auth-provider plugin is left "not probed" (unless it's the active
// one, already connected), since probing it runs that plugin — a browser
// login per OIDC context, an SSO prompt per AWS profile — for a context the
// user only scrolled past. ctrl+r is the explicit ask, and probes all of
// them (docs/design README.md §7a).
func (m *Model) startContextProbe(all bool) tea.Cmd {
	m.palette.Hint = contextHint()
	m.probes = map[string]kube.ProbeResult{}
	names := contextNames()
	if !all {
		names = m.skipCredentialPluginProbes(names)
	}
	m.probeGen++
	m.refreshContextPalette()
	m.stopContextProbe()
	ctx, cancel := context.WithCancel(m.session.Context())
	m.probeCancel = cancel
	return probeContextsCmd(ctx, m.probeGen, names)
}

// skipCredentialPluginProbes marks every credential-plugin context in names
// but the active one as not probed, and returns the rest.
func (m *Model) skipCredentialPluginProbes(names []string) []string {
	plugin := kube.CredentialPluginContexts()
	probe := make([]string, 0, len(names))
	for _, name := range names {
		if plugin[name] && name != m.session.Location.Context {
			m.probes[name] = kube.ProbeResult{Name: name, Skipped: true}
			continue
		}
		probe = append(probe, name)
	}
	return probe
}

// stopContextProbe cancels the in-flight context probe run, if any — called
// wherever the 7a palette goes away (closed, or replaced by another scope)
// and before a re-probe starts a new run. Its drain loop still runs to the
// channel's close, which a cancelled run reaches promptly; its results are
// already out-gen'd or about to be (startContextProbe bumps probeGen).
func (m *Model) stopContextProbe() {
	if m.probeCancel != nil {
		m.probeCancel()
		m.probeCancel = nil
	}
}

// openUpdatePanel pushes the current task and swaps in 28b (buildUpdate) —
// shared by the 'U' shortcut (handleShellKey) and the goto palette's
// synthetic ":update" item (OpenUpdatePanelMsg). Marks the chip's version
// seen right away: opening the panel at all counts as "seen" for 28a's
// ambient chip (docs/design README.md §28b), whether or not the user goes
// on to press 'x' inside it. A no-op (nil Cmd) when no factory was
// installed (WithUpdatePanel never called — shouldn't happen once
// app.NewModel wires it, but every branch guards the same way buildSetup/
// buildBrowse already do).
func (m *Model) openUpdatePanel() tea.Cmd {
	if m.buildUpdate == nil {
		return nil
	}
	if m.session != nil {
		if v, ok := m.session.UpdateChip(); ok {
			m.session.State.MarkUpdateSeen(v)
		}
	}
	m.push()
	replay := m.activateFresh(m.buildUpdate())
	return tea.Batch(replay, m.taskInit())
}

// movePalette routes an up/down press to the palette's linear list.
func (m *Model) movePalette(delta int) {
	m.palette.Move(delta, palettePanelMaxHeight(m.height))
}

func (m *Model) movePaletteHalfPage(direction int) {
	m.palette.MoveHalfPage(direction, palettePanelMaxHeight(m.height))
}

// refreshPalette rebuilds the open palette's Items/Hint/Recent/Sel for its
// current scope/Browse/Query state — called after every query edit. A no-op
// with no session.
func (m *Model) refreshPalette() {
	if m.palette == nil || m.session == nil {
		return
	}
	switch m.palette.Scope {
	case palette.ScopeGoto:
		m.refreshGotoPalette()
	case palette.ScopeNamespace:
		m.refreshNamespacePalette()
	case palette.ScopeContext:
		m.refreshContextPalette()
	case palette.ScopeVerb:
		m.refreshWhoCanVerbPalette()
	case palette.ScopeResource:
		m.refreshWhoCanResourcePalette()
	}
}

// refreshGotoPaletteCorpus re-ranks an *open* jump palette after the kind
// registry itself changed underneath it — a discovered kind landing, or a
// custom kind's printer columns arriving.
//
// The corpus is snapshotted when the palette opens, so without this a kind
// discovery finds while the palette is up stays invisible until it's closed
// and reopened: `g` pressed early in a connect offers the built-in kinds and
// nothing else, and keeps offering exactly that however long it's held open,
// with the CRD *object* rows the only thing a query like "kustomiz" can
// match. Same shape as gotoCountsMsg's refresh — late data, re-rank in
// place — and a no-op unless the jump palette is the one on screen.
func (m *Model) refreshGotoPaletteCorpus() {
	m.gotoCorpus = nil
	if m.palette == nil || m.palette.Scope != palette.ScopeGoto {
		return
	}
	m.refreshGotoPalette()
}

// refreshGotoPalette rebuilds the goto palette's Items/Hint/Recent/Sel/
// Footer for its current Browse/Query state — called after every query edit
// so typing a character switches 12a's ranked-chips list to 2b/12b's fuzzy
// results (and clearing back to an empty query restores the ranked list).
func (m *Model) refreshGotoPalette() {
	m.palette.Hint = gotoHint(m.session)
	if m.palette.Browse {
		m.palette.Items = gotoBrowseItems(m.session)
		m.palette.Recent = nil
		m.palette.Sel = gotoBrowseSelection(m.palette.Items, m.session.State.RecentKinds, m.session.Location.Kind)
		m.palette.Footer = gotoAliasFooter()
		return
	}
	if m.gotoCorpus == nil {
		m.gotoCorpus = buildGotoResourceCorpus(m.session)
	}
	m.palette.Items = gotoFuzzyItems(m.session, m.palette.Query(), m.gotoCorpus)
	m.palette.Recent = gotoRecentKindLabels(m.session)
	m.palette.Sel = 0
	m.palette.Footer = gotoAliasMatchFooter(m.session, m.palette.Query())
}

// refreshNamespacePalette re-filters the 6a namespace list against the
// current query (the same "one palette shell, same fuzzy input" as every
// other scope — docs/design README.md's system-wide interactions). It
// filters m.namespaceItemsCache rather than re-fetching from the cluster —
// see that field's doc comment — so it never blocks on cluster/metrics
// calls. A query that's a bare digit '1'-'9' short-circuits to
// digitRecentTarget's RECENT-row pick instead of fuzzy-filtering on the
// digit text itself (see that func's doc comment).
func (m *Model) refreshNamespacePalette() {
	if m.palette.Denied {
		// Nothing to filter — the palette is a free-typed "switch to" input
		// while the Namespace cache is denied (loadNamespacePalette already
		// set Items/Recent/Footer for this state), so typing must not
		// re-fuzzy-filter an empty corpus out from under it.
		return
	}
	// namespaceRecentLabels already excludes current and previous (see its
	// doc comment) — it IS the numberedRecents list, so it doubles as
	// digitRecentTarget's lookup with no further filtering.
	recents := namespaceRecentLabels(m.session)
	items := m.namespaceItemsCache
	if target, ok := digitRecentTarget(m.palette.Query(), recents); ok {
		if i, ok := namespaceItemIndex(items, target); ok {
			m.palette.Items = items
			m.palette.Recent = recents
			m.palette.Sel = i
			m.palette.Footer = namespaceRecentFooter(target)
			return
		}
	}
	items = m.namespaceItemsCache
	if m.palette.Query() != "" {
		items = palette.Filter(items, m.palette.Query())
	}
	m.palette.Items = items
	m.palette.Recent = recents
	m.palette.Sel = namespacePaletteSelection(m.session, m.palette.Items, m.palette.Query())
	m.palette.Footer = nil
}

// refreshContextPalette rebuilds the 7a context list against the latest
// probe results, fuzzy-filtered by the current query — or, for a bare digit
// '1'-'9' query, jumps Sel straight to that RECENT-row entry instead
// (digitRecentTarget), mirroring refreshNamespacePalette.
func (m *Model) refreshContextPalette() {
	// contextRecentLabels already excludes current and previous (see its
	// doc comment) — it IS the numberedRecents list, so it doubles as
	// digitRecentTarget's lookup with no further filtering.
	recents := contextRecentLabels(m.session)
	items := contextItems(m.session, m.probes)
	if target, ok := digitRecentTarget(m.palette.Query(), recents); ok {
		if i, ok := contextItemIndex(items, target); ok {
			m.palette.Items = items
			m.palette.Recent = recents
			m.palette.Sel = i
			m.palette.Footer = contextRecentFooter(target)
			return
		}
	}
	if m.palette.Query() != "" {
		items = palette.Filter(items, m.palette.Query())
	}
	m.palette.Items = items
	m.palette.Recent = recents
	m.palette.Sel = contextPaletteSelection(m.session, m.palette.Items, m.palette.Query())
	m.palette.Footer = nil
}

// openPalette opens the shell's one palette instance scoped to scope,
// populating real Items/Hint/Sel from Session, and returns the tea.Cmd the
// scope needs to kick off (nil for goto/namespace; the context palette's
// probe drain for context). Browse (12a's ranked-chips list) is goto-only —
// namespace/context are always plain lists.
func (m *Model) openPalette(scope palette.Scope, prompt, hint string) tea.Cmd {
	m.stopContextProbe() // a palette replaced by another scope stops probing too
	m.palette = &palette.Model{Scope: scope, Prompt: prompt, Hint: hint, Browse: scope == palette.ScopeGoto}
	m.palette.Input = palette.NewInput(scope)
	m.palette.Input.SetStyles(TextInputStyles(m.Theme()))
	m.mode = ModeGoto
	if m.session == nil {
		return nil
	}
	switch scope {
	case palette.ScopeGoto:
		m.gotoCorpus = nil
		m.refreshGotoPalette()
		// The list renders immediately from whatever counts are still
		// fresh; the rest fill in behind it (see fetchGotoCountsCmd).
		m.gotoGen++
		return fetchGotoCountsCmd(m.session, m.gotoGen)
	case palette.ScopeNamespace:
		m.palette.Hint = namespaceHint(m.session)
		m.palette.ColumnHeaders = namespaceColumnHeadersFor(namespaceCountDescriptor(m.session))
		m.palette.NameColumnLabel = "NAMESPACE"
		m.palette.GutterGlyph = namespaceGutterGlyph
		m.palette.RecentHint = recentPickHint()
		m.palette.Recent = namespaceRecentLabels(m.session)
		m.namespaceGen++
		return m.loadNamespacePalette(m.namespaceGen)
	case palette.ScopeContext:
		m.palette.ColumnHeaders = contextColumnHeaders()
		m.palette.NameColumnLabel = "CONTEXT"
		m.palette.RecentHint = recentPickHint()
		return m.startContextProbe(false)
	}
	return nil
}

// taskCapturingInput reports whether the active task has an open free-text
// input that wants every keystroke (see InputCapturer) — false for tasks
// that don't implement the interface at all — or has an inline confirm
// pending. A ModeConfirm keybar is the one signal every confirm-capable
// screen already sets, so its y/n/q reach the confirm instead of the shell
// opening the namespace palette or the quit confirm over a still-pending
// cert renew, Helm rollback or key removal.
func taskCapturingInput(task Task) bool {
	if c, ok := task.(InputCapturer); ok && c.CapturingInput() {
		return true
	}
	s, ok := task.(Screen)
	return ok && s.Keybar().Pill == ModeConfirm
}

// transient reports whether the task being swapped out asked to be dropped
// from the stack rather than pushed onto it (see Transient).
func transient(t Task) bool {
	tr, ok := t.(Transient)
	return ok && tr.Transient()
}

func sameTask(a, b Task) bool {
	if a == nil || b == nil {
		return a == b
	}
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)
	if av.Kind() == reflect.Pointer && bv.Kind() == reflect.Pointer {
		return av.Pointer() == bv.Pointer()
	}
	return reflect.TypeOf(a) == reflect.TypeOf(b)
}

func (m Model) View() tea.View {
	view := m.task.View()
	view.AltScreen = true

	width, height := m.width, m.height
	if width <= 0 {
		width = DefaultWidth
	}
	if height <= 0 {
		height = DefaultHeight
	}
	view.Content = m.spliceSwitchFeedback(view.Content, width)

	if m.session != nil && (m.palette != nil || m.helpOpen || m.quitConfirm) {
		theme := m.session.Theme
		dim := lipgloss.NewStyle().Foreground(theme.TextGhost)

		var panel string
		switch {
		case m.quitConfirm:
			panel = renderQuitConfirm(theme)
		case m.palette != nil:
			panel = m.palette.Render(paletteStyles(theme), width, palettePanelMaxHeight(height))
		case m.helpOpen:
			screen, ok := m.task.(Screen)
			if !ok {
				return view
			}
			panel = renderHelp(theme, screen, m.session.HelpScope, m.session.HelpList, m.session.HelpResource, m.session.HelpMisc, width)
		}
		view.Content = components.Compose(view.Content, panel, width, height, paletteTop, dim)

		// 2b: "Main keybar while open: GOTO mode pill + one-line explanation" —
		// Compose dims the whole base uniformly (including the underlying
		// screen's own keybar band, which still reads its own PillText), so the
		// goto palette needs its own undimmed keybar line spliced in on top,
		// the same way the palette panel itself stays undimmed. Scoped to goto
		// only: 6a/7a's namespace/context palettes have no equivalent spec'd
		// bullet, so inventing pill copy for them isn't a spec-driven fix.
		if m.palette != nil && m.palette.Scope == palette.ScopeGoto {
			panelHeight := len(strings.Split(panel, "\n"))
			actualTop := max(min(paletteTop, height-panelHeight), 0)
			if actualTop+panelHeight <= height-1 {
				view.Content = replaceLastLine(view.Content, gotoKeybarLine(theme, width))
			}
		}
		// 7b: "Keybar pill HELP" — same reasoning as goto's splice above: Compose
		// dims the whole base uniformly, including the underlying screen's own
		// keybar band, so the help overlay needs its own undimmed HELP-pilled
		// line spliced in on top rather than inventing a new compose path.
		if m.helpOpen {
			view.Content = replaceLastLine(view.Content, helpKeybarLine(theme, width))
		}
	}

	// --keycast's chip composites last, over any other overlay above (or
	// none), so it stays visible while jumping through the palette/help —
	// the whole point of a demo-recording key legend.
	if m.keycastEnabled {
		theme := Dark()
		if m.session != nil {
			theme = m.session.Theme
		}
		if chip := renderKeycastChip(m.keycast, theme); chip != "" {
			view.Content = components.ComposeCorner(view.Content, chip, width, height)
		}
	}
	return view
}

// renderQuitConfirm draws 'q's quit confirmation: the same
// components.ConfirmBox shape every other y/N confirm card uses, but
// bordered in Theme.BorderPalette/BgPalette — the neutral floating-dialog
// tokens palette/help already use — rather than Theme.ConfirmBorder, since
// quitting isn't a destructive resource action (invariant: "red borders
// reserved exclusively for destructive confirms").
func renderQuitConfirm(theme Theme) string {
	bg := lipgloss.NewStyle().Background(theme.BgPalette)
	styles := components.ConfirmStyles{
		Border: bg.BorderForeground(theme.BorderPalette),
		Title:  bg.Foreground(theme.Text).Bold(true),
		Detail: bg.Foreground(theme.TextSecondary),
		Rule:   bg.Foreground(theme.TextGhost),
		Key:    bg.Foreground(theme.Accent),
		Label:  bg.Foreground(theme.TextDim),
	}
	return components.ConfirmBox("Quit kute?", "", styles)
}

// gotoKeybarLine renders 2b's main-keybar-while-open treatment: a GOTO mode
// pill plus a one-line explanation, reusing renderKeybarV2 for the exact
// same chrome (border/inset/pill shape) every other screen's keybar gets.
func gotoKeybarLine(theme Theme, width int) string {
	kb := Keybar{
		Pill:      ModeGoto,
		PillText:  "GOTO",
		RightNote: "jump to any kind, resource, namespace, or context",
	}
	return renderKeybarV2(kb, theme, width)
}

// helpKeybarLine renders 7b's "Keybar pill HELP" treatment, the same
// undimmed-splice shape gotoKeybarLine already establishes for 2b.
func helpKeybarLine(theme Theme, width int) string {
	kb := Keybar{
		Pill:     ModeHelp,
		PillText: "HELP",
	}
	return renderKeybarV2(kb, theme, width)
}

// replaceLastLine swaps content's final "\n"-joined line for line — the
// keybar band is always exactly Frame's last line (LegendHeight == 1).
func replaceLastLine(content, line string) string {
	idx := strings.LastIndex(content, "\n")
	if idx < 0 {
		return line
	}
	return content[:idx+1] + line
}

// paletteTop is the palette overlay's fixed anchor row — a couple of rows
// below the header, like the mockups' 44/56px offsets. Anchoring (rather
// than vertical centering) keeps the panel still while its height changes
// with every keystroke's result count.
const paletteTop = 2

func palettePanelMaxHeight(screenHeight int) int {
	if screenHeight <= 0 {
		screenHeight = DefaultHeight
	}
	return max(screenHeight-2*paletteTop, 1)
}

func (m *Model) clampPaletteViewport() {
	if m.palette != nil {
		m.palette.ClampViewport(palettePanelMaxHeight(m.height))
	}
}
