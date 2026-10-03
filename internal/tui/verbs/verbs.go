// Package verbs is the shared vocabulary for keybars, the help overlay, and
// the confirm flow (mvp-plan.md §0.4). Every verb — delete, logs, cordon,
// … — is one registry entry: stable ID, key, label, destructiveness tier,
// applicable kinds, mutating flag. Screens hand-compose their keybars
// (curated per-screen grouping and order, only applicable keys shown) but
// only from these entries, never from key/label string literals; the help
// overlay and the confirm policy read the same entries, so they cannot
// drift apart.
package verbs

import (
	"slices"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// Verb is one registry entry.
type Verb struct {
	ID    string // stable id ("delete", "logs") — future key-remapping hook
	Key   string // "ctrl+d"
	Label string // "delete"
	// ReverseID is the Scope.Verb of a two-direction verb's undo direction
	// ("uncordon", "cronjob-resume", …) — empty for a one-way verb. The
	// policy table (Resolve) gives that direction its own tier, so resume
	// can stay reversible-and-immediate while suspend gets friction.
	ReverseID string
	Kinds     []kube.ResourceKind // applicable kinds; nil = all
	// Mutating verbs are hidden from the keybar and refused at
	// actions.Controller.Begin while OFFLINE (docs/design README.md §52,
	// §301) — screens call HiddenWhileOffline when building their own
	// Keybar() rather than hardcoding which verbs need the gate; browse's
	// list view instead swaps its whole keybar to the OFFLINE pill, which
	// covers every verb at once without consulting this field per-verb.
	Mutating bool
	// Bulk declares whether this verb can act on 20a's marked set instead of
	// just the cursor row (docs/design README.md §20a: "bulk-capability is
	// declared per-verb in the command table"). Per-row-only verbs (logs,
	// exec, ↵ open, …) leave this false.
	Bulk bool
	// Global marks a verb as identical on every screen — goto/filter/mark/
	// yaml/edit/events/timeline/meta/namespace/context/all-namespaces
	// (docs/design v.0.3.0.dc.html §29a: "the bar shows what's different
	// here, grammar lives in ?"). A Global verb's Hint never renders in a
	// screen's own Keybar() Groups; it's taught exactly once, in the ?
	// overlay's SCOPE/LIST/RESOURCE columns (helpScopeKeys/helpListKeys/
	// helpResourceKeys).
	Global bool
}

// ScaleUpKey and ScaleDownKey are the two key presses behind Scale's
// display-only "+/−" Key — the minus is the ASCII '-' a terminal sends, not
// the typographic '−' the keybar shows.
const (
	ScaleUpKey   = "+"
	ScaleDownKey = "-"
)

// Hint renders the verb as a keybar key/label pair.
func (v Verb) Hint() tui.KeyHint {
	return tui.KeyHint{Key: v.Key, Label: v.Label}
}

// AppliesTo reports whether the verb is offered for kind. A nil Kinds list
// means "every kind".
func (v Verb) AppliesTo(kind kube.ResourceKind) bool {
	if len(v.Kinds) == 0 {
		return true
	}
	return slices.Contains(v.Kinds, kind)
}

// HiddenWhileOffline reports whether v's keybar hint should be omitted
// given the connection state — true only for a Mutating verb while offline.
// Mutating covers more than kube.Mutator-routed writes: Exec and NodeShell
// hand a tty to kubectl instead, and are gated at their own dispatch sites
// rather than in actions.Controller.Begin, but docs/design README.md §52's
// "mutating actions disabled while OFFLINE" applies to them the same way.
// Forward stays exempt — a port-forward is a local session, not a write.
func (v Verb) HiddenWhileOffline(offline bool) bool {
	return v.Mutating && offline
}

// Navigation/view verbs, plus the two tty-handoff verbs (Exec/NodeShell)
// that are Mutating without being tiered.
var (
	Goto   = Verb{ID: "goto", Key: "g", Label: "goto", Global: true}
	Filter = Verb{ID: "filter", Key: "/", Label: "filter", Global: true}
	Open   = Verb{ID: "open", Key: "↵", Label: "open"}
	// Logs' Kinds includes CronJob as well as Pod (0.8.0 plan §36a: "l opens
	// logs for the newest useful Pod of the selected row's most recent
	// associated Job — active or terminal, succeeded or failed") — a
	// CronJob row is never itself the log source, browse resolves an actual
	// Pod target first and only then reaches this verb, the same
	// one-level-of-indirection Timeline's object-scoped 't' already does for
	// a non-Pod kind.
	Logs = Verb{ID: "logs", Key: "l", Label: "logs", Kinds: []kube.ResourceKind{kube.KindPod, kube.KindCronJob}}
	YAML = Verb{ID: "yaml", Key: "y", Label: "yaml"}
	// Exec is 'x' on a pod — an interactive shell inside a container, handed
	// off to a kubectl subprocess (kube.ExecSpec over tea.ExecProcess) rather
	// than routed through kube.Mutator, hence TierNone: there's no confirm
	// step to run. Mutating even so, because whatever the user types at that
	// prompt is a write, and a shell opened against a dead API connection
	// only fails at the handoff with a raw kubectl error (docs/design
	// README.md §52: OFFLINE disables mutating actions).
	Exec = Verb{ID: "exec", Key: "x", Label: "exec", Mutating: true, Kinds: []kube.ResourceKind{kube.KindPod}}
	// NodeDebug is 'x' on a Node row (§41d) — retired the former standalone
	// 's' NodeShell verb rather than sitting alongside it: a privileged
	// root shell on a node with no will-run line and no PROD confirmation
	// was the one dangerous action in the app with the least friction, it
	// predates the design spec (shipped-but-undesigned), and a second
	// node-only key for "get a shell on this object" contradicted §41's own
	// rule that x alone decides the provider. The debug panel it opens
	// (tasks/debugpanel) absorbs the retired verb's exact command — same
	// image/profile defaults, same chroot/shell-fallback entrypoint,
	// same tea.ExecProcess handoff — behind a will-run line and, in PROD, a
	// confirmation the old key never had. Its policy row (policy.go) gates
	// that launch from tasks/debugpanel itself, since the launch is a
	// tea.ExecProcess handoff, never a Mutator call.
	NodeDebug = Verb{ID: "node-debug", Key: "x", Label: "debug", Mutating: true, Kinds: []kube.ResourceKind{kube.KindNode}}
	// NodeDebugDetail is tasks/nodedetail's own key for the identical §41d
	// panel — 's', not NodeDebug's 'x'. Unlike browse's Nodes list, nodedetail
	// shows a pods table alongside the loaded node, and that table's own row
	// verb is Exec's 'x' on a different object (the pod row, not the node the
	// screen is about) — the two coexist on one screen, so the node action
	// keeps the key it already had rather than colliding. Same target
	// (tasks/debugpanel, node), same policy row as NodeDebug; only
	// the physical key differs, and only in this one screen.
	NodeDebugDetail = Verb{ID: "node-debug-detail", Key: "s", Label: "debug node", Mutating: true, Kinds: []kube.ResourceKind{kube.KindNode}}
	// PodDebug is the §41b/§41c debug panel's '↵ start' on a Pod target — the
	// attach (ephemeral container) or copy-pod kubectl debug launch. There's
	// no key that opens the pod panel itself: Exec's 'x' routes there when no
	// container has a shell or the pod won't stay running. Like NodeDebug, it
	// is a tea.ExecProcess handoff, never a Mutator call, so tasks/debugpanel
	// applies its policy row itself — immediate outside PROD, one y/N in PROD.
	PodDebug = Verb{ID: "pod-debug", Key: "↵", Label: "start", Mutating: true, Kinds: []kube.ResourceKind{kube.KindPod}}
	// DebugCleanup is the debug panel's CLEAN UP 'ctrl+d delete it' (§41c
	// copy pod, §41d node-debugger pod) — an ordinary pod delete through
	// kube.Mutator, so it carries Delete's own friction (policy.go).
	DebugCleanup = Verb{ID: "debug-cleanup", Key: "ctrl+d", Label: "delete it", Mutating: true, Kinds: []kube.ResourceKind{kube.KindPod}}
	// Edit is 'E' on any row, any kind — kubectl edit (kube.EditSpec) over
	// tea.ExecProcess, the same tty-handoff path as Exec/NodeShell, and
	// Mutating for the plainest reason of the three: it applies whatever the
	// user saves. kubectl owns the session once kute suspends, so the only
	// confirm is the PROD-only y/N gate each call site applies from Edit's
	// policy row (Resolve). §4a names it alongside delete and
	// exec as disabled while offline.
	Edit          = Verb{ID: "edit", Key: "E", Label: "edit", Mutating: true, Global: true}
	Events        = Verb{ID: "events", Key: "e", Label: "events", Global: true}
	Namespace     = Verb{ID: "namespace", Key: "n", Label: "namespace", Global: true}
	Context       = Verb{ID: "context", Key: "c", Label: "context", Global: true}
	AllNamespaces = Verb{ID: "all-namespaces", Key: "a", Label: "all namespaces", Global: true}
	// JumpNamespace is 6b's "N" — jump into the selected row's namespace
	// without leaving the all-namespaces triage view for the palette.
	JumpNamespace = Verb{ID: "jump-namespace", Key: "N", Label: "jump into namespace"}
	// ToggleGroup is 6b's "tab" — expand/collapse the namespace group the
	// cursor is currently in (docs/design README.md §6b's "↹"), same key
	// events (9b) and yamlview (8a) already use for their own local fold
	// toggles.
	ToggleGroup = Verb{ID: "toggle-group", Key: "tab", Label: "expand/collapse"}
	PageUp      = Verb{ID: "page-up", Key: "pgup", Label: "page up"}
	PageDown    = Verb{ID: "page-down", Key: "pgdown", Label: "page down"}
	Help        = Verb{ID: "help", Key: "?", Label: "help"}
	// Retry is the reconnect/re-probe key shared by the 4a offline banner,
	// 4b's 403 card, and the 4c/10b tasks/setup screens (mvp-plan.md Phase
	// 4) — one registry entry so every error surface's "r" reads the same.
	Retry = Verb{ID: "retry", Key: "r", Label: "retry"}
	// WhoCan is 'w' — both the 4b 403 card's recovery line ("w who-can")
	// and tasks/whocan's own reachability (docs/design README.md §22a: "w
	// on a 403 card opens who-can pre-filled"), one registry entry so the
	// two surfaces can never drift apart.
	WhoCan = Verb{ID: "who-can", Key: "w", Label: "who-can"}
	// HelmValues is 18a's 'v' — the selected release's decoded values in the
	// read-only YAML viewer.
	HelmValues = Verb{ID: "helm-values", Key: "v", Label: "values", Kinds: []kube.ResourceKind{kube.KindHelmRelease}}
	// EnvMounts is 5a's 'v' — toggles the ENV & MOUNTS section for the
	// selected container: which env vars come from which Secret/ConfigMap
	// key, and what each volumeMount is backed by. Names and keys only,
	// never values. Same key as HelmValues; the two never share a screen.
	EnvMounts = Verb{ID: "env-mounts", Key: "v", Label: "env/mounts", Kinds: []kube.ResourceKind{kube.KindPod}}
	// HelmHistory is 18a's 'h' — the selected release's full revision rail
	// (16b's rail idiom).
	HelmHistory = Verb{ID: "helm-history", Key: "h", Label: "history", Kinds: []kube.ResourceKind{kube.KindHelmRelease}}
	// HelmPods preserves 18a's release-workload shortcut after Enter became
	// the diagnostic detail destination.
	HelmPods = Verb{ID: "helm-pods", Key: "p", Label: "pods", Kinds: []kube.ResourceKind{kube.KindHelmRelease}}
	// Timeline is 't' — the incident timeline (16a namespace-scoped from
	// lists, 16b object-scoped from detail views), docs/design README.md's
	// system-wide interactions list: "t opens the incident timeline
	// (namespace-scoped from lists, object-scoped from detail)".
	Timeline = Verb{ID: "timeline", Key: "t", Label: "timeline", Global: true}
	// Mark is 20a's "space" — marks the cursor row and advances, works in
	// any list.
	Mark = Verb{ID: "mark", Key: "space", Label: "mark", Global: true}
	// MarkAll is 20a's "*" — marks every row the current filter matches
	// ("filter-then-mark is the bulk grammar — no range-mark chord").
	MarkAll = Verb{ID: "mark-all", Key: "*", Label: "mark all"}
)

// Log-view verbs (5b, docs/design README.md). These stay separate from the
// resource-level Logs verb above: Logs opens the screen, while these operate
// on the stream already in view. Pause/Follow intentionally share a key so
// the keybar can describe the action space will perform in the current state
// without inventing a label outside the registry.
var (
	LogPause          = Verb{ID: "log-pause", Key: "space", Label: "pause"}
	LogFollow         = Verb{ID: "log-follow", Key: "space", Label: "follow"}
	LogNextWarning    = Verb{ID: "log-next-warning", Key: "w", Label: "next warning"}
	LogNextError      = Verb{ID: "log-next-error", Key: "e", Label: "next error"}
	LogToggleWrap     = Verb{ID: "log-toggle-wrap", Key: "W", Label: "wrap"}
	LogToggleTime     = Verb{ID: "log-toggle-timestamps", Key: "t", Label: "timestamps"}
	LogCycleSince     = Verb{ID: "log-cycle-since", Key: "s", Label: "since"}
	LogCycleContainer = Verb{ID: "log-cycle-container", Key: "tab", Label: "cycle container"}
	LogCopyView       = Verb{ID: "log-copy-view", Key: "Y", Label: "copy view"}
)

// Mutating verbs — every write path funnels through kube.Mutator, gated by
// actions.Controller under the policy table in policy.go (mvp-plan.md §0.4,
// §8b).
var (
	Delete = Verb{
		ID: "delete", Key: "D", Label: "delete",
		Mutating: true, Bulk: true,
	}
	// ForceDelete is never a list-screen key of its own: it escalates a
	// pending Pod delete confirm (inline or modal) to grace-period-0, and
	// TierModal means that escalation always lands on the type-the-name
	// modal, PROD or not (CLAUDE.md: "drain/force-delete always modal").
	// A ctrl chord rather than a bare letter because the modal is a text
	// buffer — a bare key would be swallowed as (or steal) a typed rune.
	ForceDelete = Verb{
		ID: "force-delete", Key: "ctrl+k", Label: "force delete",
		Kinds: []kube.ResourceKind{kube.KindPod}, Mutating: true,
	}
	RolloutRestart = Verb{
		ID: "rollout-restart", Key: "R", Label: "restart",
		Kinds: []kube.ResourceKind{kube.KindDeployment}, Mutating: true,
	}
	// JobRetry is §37c's "create" rerun choice: clones a Job's spec into a
	// brand-new "<name>-rerun-N" Job — not a delete+recreate of the same
	// object, so nothing about the source Job is destroyed, but it does
	// start a fresh run of the Job's business logic (which can carry real
	// side effects: emails, charges, writes). Same physical key as
	// RolloutRestart/CronJobRunNow above (never both applicable to one row —
	// Kinds is disjoint) and the same non-destructive argument JobRetry's
	// clone semantics make for staying off ctrl- style escalation. Both
	// browse's Jobs list (§37a) and the pushed jobattempts screen (§37b)
	// reach this through the same staged create-vs-replace choice
	// (jobRerunTarget) rather than firing it directly — the staging step is
	// itself the confirmation for this branch, the same "stage first,
	// TierNone commit" shape CronJobRunNow's own doc comment describes.
	JobRetry = Verb{
		ID: "job-retry", Key: "R", Label: "rerun",
		Kinds: []kube.ResourceKind{kube.KindJob}, Mutating: true,
	}
	// JobReplace is §37c's "replace" choice within the same staged 'R'
	// list: delete the Job, then recreate it under the same name from its
	// own spec. Unlike JobRetry's clone, this destroys the original — its
	// Status, events, and §37b's own attempt history are gone — so choosing
	// it in the staged list does not itself confirm anything; committing
	// still routes through actions.Controller's ordinary TierInline/
	// TierModal confirm (verbs.Resolve's typed-name policy for "job-replace" routes
	// PROD to the type-the-name modal, not Drain/Rollback's plain
	// ConfirmCard). No dedicated Key: it's reached only from within the
	// staged choice list (↑↓ to select, enter to commit), never a bare
	// list-screen keybinding of its own.
	JobReplace = Verb{
		ID: "job-replace", Label: "replace",
		Kinds: []kube.ResourceKind{kube.KindJob}, Mutating: true,
	}
	// FluxReconcile is §30a's 'r' on a Flux row: a fresh
	// reconcile.fluxcd.io/requestedAt timestamp — the same
	// annotate-to-trigger mechanism RolloutRestart uses, on a different
	// object. TierNone, no confirm.
	//
	// It keeps bare 'r' against the objection recorded on RolloutRestart
	// above, deliberately and for a reason that does not generalize. That
	// verb was moved off 'r' because a single unmodified letter fired an
	// unconfirmed pod restart across a whole Deployment with zero friction.
	// Reconcile is not that: it asks for the reconciliation Flux would have
	// performed on its own interval anyway, so the worst case of a
	// mistaken press is that a sync happens early. 'r' is also what `flux
	// reconcile` is spelled, and this screen's other meanings for 'r'
	// (retry a failed load, restart a forward) are reachable only in states
	// a Flux row is not in. Do not copy this exemption to a verb that
	// changes cluster state the controller wasn't already going to make.
	FluxReconcile = Verb{
		ID: "flux-reconcile", Key: "r", Label: "reconcile",
		Mutating: true,
	}
	// FluxSuspend is §30a's 's': one verb, two directions, exactly Cordon's
	// shape — the Scope.Verb is "flux-suspend" or "flux-resume" depending on
	// the row, and browse flips the keybar label to match. TierNone for
	// Cordon's reason: reversible and immediate, and resume puts the object
	// back exactly where it was.
	//
	// Kinds stays nil because the Flux kind set is *discovered* at connect,
	// not known at compile time; browse gates it on Descriptor.Flux instead.
	FluxSuspend = Verb{
		ID: "flux-suspend", Key: "s", Label: "suspend", ReverseID: "flux-resume",
		Mutating: true,
	}
	// JobSuspend is a Job's own 's': one verb, two directions — Scope.Verb is
	// "job-suspend" or "job-resume" depending on the row's own Suspended
	// state. v0.9.0 §37a makes the two directions asymmetric, the same way
	// CronJobSuspend's already are (§36c): setting spec.suspend:true on a Job
	// tears down its currently-active pods immediately, a real destructive
	// side effect resume doesn't have (resume only lets the Job resume
	// creating pods toward its existing completion target, restoring exactly
	// the prior state) — its policy row (Resolve) gives each direction its own
	// tier (resume, ReverseID, always TierNone; suspend TierInline outside
	// PROD, the type-the-name modal inside one). Never contends with NodeShell's own 's'
	// (Node-only) or FluxSuspend's (gated on Descriptor.Flux, never true for
	// a Job row).
	JobSuspend = Verb{
		ID: "job-suspend", Key: "s", Label: "suspend", ReverseID: "job-resume",
		Kinds: []kube.ResourceKind{kube.KindJob}, Mutating: true,
	}
	// CronJobRunNow is 'R' on a CronJob row — the exact
	// `kubectl create job --from=cronjob/<name>` recipe JobRetry already
	// gives Jobs, one level up: it creates a brand-new, standalone Job from
	// the CronJob's own jobTemplate, so the CronJob object itself (its
	// schedule, its own spawned-Job history) is never touched. Same physical
	// key as JobRetry/RolloutRestart above (Kinds is disjoint — a row is
	// never both a Job and a CronJob) and the same non-destructive argument
	// JobRetry's own doc comment makes for staying off the type-the-name modal:
	// this is a clone into a new object, not a delete+recreate.
	//
	// Its policy is TierNone (0.8.0 plan Phase 5): browse stages the preflight
	// (selected CronJob, overlap warning, generated name) in screen state —
	// cronjob_actions.go's pendingCronJobRun — *before* ever calling
	// Begin; the staging step is itself the confirmation, so by
	// the time Begin runs there's nothing left for actions.Controller's own
	// TierInline/TierModal confirm to add. Never contends with delete/
	// rollout-restart's own escalation because staging is screen-owned, not
	// Controller-driven — there's no confirming state at the Controller
	// level for a PROD context to escalate.
	CronJobRunNow = Verb{
		ID: "cronjob-run-now", Key: "R", Label: "run now",
		Kinds: []kube.ResourceKind{kube.KindCronJob}, Mutating: true,
	}
	// CronJobSuspend is a CronJob's own 's': one verb, two directions —
	// Scope.Verb is "cronjob-suspend"/"cronjob-resume" depending on the
	// row's own Suspended state — but, unlike FluxSuspend/JobSuspend, the two
	// directions now carry genuinely different tiers (0.8.0 plan §36c):
	// suspend is CronJob's "dangerous half" (it fails silently and forever;
	// nobody gets paged for a scheduled run that never started), so it costs
	// a keystroke — TierInline outside PROD, the type-the-name modal inside
	// one — while resume stays reversible-and-immediate TierNone, restoring
	// exactly the prior state the same way Cordon/FluxSuspend's own resume
	// does. Its policy row (Resolve) carries both directions, resume being
	// ReverseID. Bulk: true — 36c: "s
	// suspends or resumes every marked CronJob behind one confirmation".
	// Never contends with Job's own 's' (disjoint Kinds) or NodeShell's
	// (Node-only).
	CronJobSuspend = Verb{
		ID: "cronjob-suspend", Key: "s", Label: "suspend", ReverseID: "cronjob-resume",
		Kinds: []kube.ResourceKind{kube.KindCronJob}, Mutating: true, Bulk: true,
	}
	// CronJobSetSchedule is 'S' (shift) on a CronJob row — pure navigation to
	// the pushed schedule editor (0.8.0 plan §36d, `internal/tui/tasks/
	// cronjobschedule`), not a mutation itself, so Mutating stays false the
	// same way Open/Forward are false: the editor screen applies the actual
	// patch (through its own registered verb, reused Open relabeled "apply")
	// once the operator commits there. This replaces the inline
	// pendingCronSchedule buffer (cronjobschedule.go) that predates the
	// dedicated task — that file still owns 'S' until Phase 6 replaces it
	// with real navigation, at which point this verb's Kinds/Key/Label carry
	// over unchanged. Uppercase to stay clear of lowercase 's' (this row's
	// own suspend/resume), ctrl-r (run now), and every Global key already
	// live on a CronJob row (g/n/c/a/E/e/y/m/t/…) plus Open's ↵ — checked
	// against the full verbs.go key table, nothing else claims 'S' anywhere
	// in the app.
	CronJobSetSchedule = Verb{
		ID: "cronjob-set-schedule", Key: "S", Label: "edit schedule",
		Kinds: []kube.ResourceKind{kube.KindCronJob},
	}
	// CronJobCopyCommand is the run-now/resume preflights' 'y' — a
	// screen-local override of the global YAML key, the same reuse
	// CopyForwardURL/CopyRouteURL already make (0.8.0 plan §36b: "y copies
	// the will-run command"). Read-only clipboard copy, so no Tier and not
	// Mutating, same shape as CopyForwardURL. Those preflights have no text
	// buffer, so a bare letter is safe there; the schedule editor, which
	// always has one focused, uses CronJobScheduleCopy instead.
	CronJobCopyCommand = Verb{ID: "cronjob-copy-command", Key: "y", Label: "copy"}
	// CronJobScheduleCopy is 36d's copy of the will-run command. A chord
	// rather than CronJobCopyCommand's bare 'y': a schedule or timezone
	// buffer always has focus on that screen, and a bare 'y' turned a typed
	// "@daily" into "@dail" (and copied the will-run line) instead. ctrl+t
	// is bound by neither textfield nor the root shell.
	CronJobScheduleCopy = Verb{ID: "cronjob-schedule-copy", Key: "ctrl+t", Label: "copy"}
	// JobAttemptDiff is 37b's 'd' — toggles the attempt ledger's
	// selected-attempt-vs-attempt-1 comparison. Read-only UI state.
	JobAttemptDiff = Verb{ID: "job-attempt-diff", Key: "d", Label: "diff vs attempt 1"}
	// JobIndexFailedOnly is 37d's 'f' — narrows an Indexed Job's index list
	// to the failed indexes (and back). Read-only UI state.
	JobIndexFailedOnly = Verb{ID: "job-index-failed-only", Key: "f", Label: "failed only"}
	// CronJobFocusTimezone is 36d's schedule editor 'tab' — moves focus onto
	// the timezone field, gated there by the capability read (§3.8) rather
	// than always-editable. Same screen-local-tab-focus shape as
	// FocusTLSStrip; read-only UI focus, not a mutation.
	CronJobFocusTimezone = Verb{ID: "cronjob-focus-timezone", Key: "tab", Label: "timezone"}
	// CronJobScheduleUndo is 36d's one-step undo after a successful apply —
	// itself "an ordinary reversible mutation through the same registry"
	// (§36d), so Mutating: true, TierNone: applying the previous accepted
	// schedule/timezone pair is exactly as reversible as the apply it
	// undoes. A chord for CronJobScheduleCopy's reason: as a bare 'u' it
	// ate the letter out of "@hourly", "sun" and "UTC" (and fired an undo).
	// ctrl+z rather than ctrl+u, which textfield binds to delete-to-start.
	CronJobScheduleUndo = Verb{ID: "cronjob-schedule-undo", Key: "ctrl+z", Label: "undo", Mutating: true}
	// CronJobScheduleFullEdit is 36d's 'ctrl-y' escape hatch to the full
	// kubectl-edit subprocess (17a's existing tty-handoff machinery) for
	// anything the schedule editor doesn't cover — concurrency, deadlines,
	// history limits. Same tty-handoff shape as Edit/Exec/NodeShell: Mutating
	// because it applies whatever the user saves, no Tier because kubectl
	// owns the session once kute suspends. A chord rather than any bare
	// letter (the global Edit verb's 'E', or the 'Y' 72af16b tried): a
	// schedule or timezone buffer always has focus on this screen, and a
	// bare 'Y' swallowed the first letter of "Asia/Yerevan" or
	// "America/Yellowknife" and suspended into kubectl edit instead.
	CronJobScheduleFullEdit = Verb{ID: "cronjob-schedule-full-edit", Key: "ctrl+y", Label: "full yaml edit", Mutating: true}
	// FluxSource is §30a's 'o': jump to the object this one reconciles
	// from. Read-only navigation, so no tier and not mutating.
	FluxSource = Verb{
		ID: "flux-source", Key: "o", Label: "source",
	}
	// ArgoRefresh is §33a's 'r' on an Application row: stamps
	// argocd.argoproj.io/refresh=normal, the same annotate-to-trigger
	// mechanism FluxReconcile uses. TierNone/no confirm for FluxReconcile's
	// own reason — it only asks argocd-application-controller to re-diff
	// early, the same re-diff it already does on its own poll interval.
	// Kinds stays nil: the Application kind is discovered at connect, not
	// known at compile time; browse gates it on Descriptor.Argo instead,
	// same shape as FluxReconcile/Descriptor.Flux.
	ArgoRefresh = Verb{
		ID: "argo-refresh", Key: "r", Label: "refresh",
		Mutating: true,
	}
	// ArgoSync is §33a's 'S': a merge patch on operation.sync.revision,
	// what `argocd app sync` does as a plain API write. Unlike
	// FluxReconcile/ArgoRefresh above, this changes what's actually
	// deployed (a real, visible side effect on the workloads the app
	// manages), so it gets the same TierInline will-run y/N every other
	// state-changing verb gets rather than FluxReconcile's TierNone —
	// RolloutRestart's own doc comment draws this exact line between
	// "asks for what the controller would do anyway" (TierNone) and "a
	// real, unconfirmed run" (TierInline). PROD escalates to the modal like
	// RolloutRestart — but a y/N card, never the delete-grade type-the-name
	// treatment (policy.go). Capital 'S', matching
	// the mockup and staying clear of FluxSuspend/JobSuspend/
	// CronJobSuspend's lowercase 's' — the two never contend on the same
	// row (Kinds is disjoint by construction: an Application row is never
	// a Job/CronJob/Flux kind).
	ArgoSync = Verb{
		ID: "argo-sync", Key: "S", Label: "sync",
		Mutating: true,
	}
	// ArgoURL is §33a's 'u': copies the app's dashboard deep link
	// (read from argocd-cm, not a cluster write) to the clipboard — the
	// same read-only, no-tier, not-Mutating shape CopyForwardURL/
	// CopyRouteURL already use for a local clipboard copy.
	ArgoURL = Verb{
		ID: "argo-url", Key: "u", Label: "dashboard url",
	}
	// CertRenew is §35c's 'r' on a Certificate row: forces cert-manager to
	// reissue with a plain merge patch on the Issuing status condition — the
	// trigger cert-manager's own trigger controller reacts to. Not `cmctl
	// renew`: the will-run line names the literal patch kute sends, never a
	// binary the user may not have installed (docs/design/v.0.7.0.dc.html
	// §35c). TierInline, but — unlike
	// ArgoSync/RolloutRestart — never PROD-escalated (policy.go): cert-manager
	// reissues into a new Secret and keeps serving the old one until the new
	// one is ready, so PROD never escalates it past the plain y/N, the same
	// "clone, not destroy" reasoning JobRetry's own doc comment gives for
	// skipping escalation outright. Shares 'r' with FluxReconcile/ArgoRefresh —
	// Kinds is disjoint, so the three never contend on the same row.
	CertRenew = Verb{
		ID: "cert-renew", Key: "r", Label: "renew",
		Kinds: []kube.ResourceKind{kube.KindCertificate}, Mutating: true,
	}
	Cordon = Verb{
		ID: "cordon", Key: "C", Label: "cordon", ReverseID: "uncordon",
		Kinds: []kube.ResourceKind{kube.KindNode}, Mutating: true,
	}
	Drain = Verb{
		ID: "drain", Key: "ctrl+d", Label: "drain",
		Kinds: []kube.ResourceKind{kube.KindNode}, Mutating: true,
	}
	// Rollback is 18a's 'R' on a Helm release — "inherits 8b friction":
	// inline y/N in non-prod, the type-the-name modal in PROD, same as
	// Delete (policy.go). Shells out to the real helm binary
	// (kube.Mutator.HelmRollback) rather than reading the watch cache.
	Rollback = Verb{
		ID: "rollback", Key: "R", Label: "rollback",
		Kinds: []kube.ResourceKind{kube.KindHelmRelease}, Mutating: true,
	}
	// RolloutUndo is 16b's 'R' on the revision rail — a deliberately
	// separate verb from the Helm-only Rollback above (same key/label,
	// different Kind and a different Scope.Verb string: "rollout-undo"),
	// though both share the same policy (policy.go). §16b:
	// "y/N in the keybar for non-prod, type-the-deployment-name modal in
	// PROD".
	RolloutUndo = Verb{
		ID: "rollout-undo", Key: "R", Label: "rollback",
		Kinds: []kube.ResourceKind{kube.KindDeployment}, Mutating: true,
	}
	// Scale is 17b's '+'/'−' on a Deployment/StatefulSet row — TierNone
	// (docs/design README.md §17b: "reversible → inline keybar prompt, never
	// a modal"). The confirming step every other TierNone verb skips is
	// replaced here by browse's own numeric type-ahead gate
	// (tasks/browse/scale.go's pendingScale) rather than
	// actions.Controller's y/N/type-name flow, since Scale needs to gather a
	// replica count before there's anything to Begin.
	// Key is the display string; ScaleUpKey/ScaleDownKey are the presses
	// that fire it.
	Scale = Verb{
		ID: "scale", Key: "+/−", Label: "scale",
		Kinds: []kube.ResourceKind{kube.KindDeployment, kube.KindStatefulSet}, Mutating: true,
	}
	// SetImage is 24a's 'i' on a Deployment/StatefulSet/DaemonSet/CronJob row —
	// TierNone outside PROD, one inline y/N in PROD (policy.go).
	SetImage = Verb{
		ID: "set-image", Key: "i", Label: "set image",
		Kinds: []kube.ResourceKind{kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet, kube.KindCronJob}, Mutating: true,
	}
	// SetResources is 'r' on a Deployment/StatefulSet/DaemonSet row. Deployment
	// rollout restart owns uppercase R; lowercase r remains distinct by case.
	// Same policy as SetImage above (policy.go).
	SetResources = Verb{
		ID: "set-resources", Key: "r", Label: "resources",
		Kinds: []kube.ResourceKind{kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet}, Mutating: true,
	}
	// Meta is 26a's 'm' on any row, any kind (CRDs included) — the
	// labels/annotations editor. Immediate, PROD or not: 26a's escalation is
	// join-driven (editing a Service-selector-matched label, or any key
	// removal, gets one y/N) rather than PROD-driven — policy.go's Meta row,
	// fed by metapanel's commitMeta/commitMetaRemove.
	Meta = Verb{
		ID: "meta", Key: "m", Label: "labels/annotations",
		Mutating: true, Global: true,
	}
	// AddSecretKey is 27b's 'a' line-insert add on a Secret's Data view —
	// same policy as SetImage/SetResources above (docs/design README.md
	// §27b: "PROD contexts add the inline y/N on apply"), plus a y/N for an
	// add that overwrites an existing key, routed through
	// actions.Controller/kube.Mutator.
	AddSecretKey = Verb{
		ID: "add-secret-key", Key: "a", Label: "add key",
		Kinds: []kube.ResourceKind{kube.KindSecret}, Mutating: true,
	}
	// RemoveSecretKey is 27b's D removal of an existing Data-view key —
	// always TierInline regardless of PROD, the same "removal always
	// confirms inline, never the type-the-name modal" shape 26a's Meta
	// removal uses (docs/design README.md §27b: "removing a key keeps the
	// y/N too" — recoverable only if the old value exists elsewhere, so the
	// prompt says so, not destructive enough for 8b's modal tier).
	RemoveSecretKey = Verb{
		ID: "remove-secret-key", Key: "D", Label: "remove key",
		Kinds: []kube.ResourceKind{kube.KindSecret}, Mutating: true,
	}
	// AddConfigMapKey is 27a's 'a' line-insert add on a ConfigMap's Data
	// view — same policy as AddSecretKey above.
	AddConfigMapKey = Verb{
		ID: "add-configmap-key", Key: "a", Label: "add key",
		Kinds: []kube.ResourceKind{kube.KindConfigMap}, Mutating: true,
	}
	// RemoveConfigMapKey is 27a's D removal of an existing Data-view
	// key — always TierInline regardless of PROD, the same "removal always
	// confirms inline" policy RemoveSecretKey uses.
	RemoveConfigMapKey = Verb{
		ID: "remove-configmap-key", Key: "D", Label: "remove key",
		Kinds: []kube.ResourceKind{kube.KindConfigMap}, Mutating: true,
	}
	// RestartConfigMapConsumers is 27a's ctrl-r — chains a value apply with
	// `kubectl rollout restart` for every workload that consumes the
	// ConfigMap. Same policy as RolloutRestart, not AddConfigMapKey: one key
	// restarts every consumer, so it never gets less friction than
	// restarting one (inline y/N; PROD: type the ConfigMap's name). With no
	// consumers it is just the apply, and configmapdata begins it as
	// AddConfigMapKey. It is only ever
	// pressed while a value buffer has focus, so it must be a chord: as a
	// bare 'R' (72af16b) every capital R typed into a value ("Release",
	// "REDIS_URL") applied and restarted every consumer. A control chord
	// never carries Text, so it can't collide with typed input, and neither
	// textfield nor textarea binds ctrl+r.
	RestartConfigMapConsumers = Verb{
		ID: "restart-configmap-consumers", Key: "ctrl+r", Label: "apply + restart consumers",
		Kinds: []kube.ResourceKind{kube.KindConfigMap}, Mutating: true,
	}
)

// Editor-mode keys for 26a/27a/27b's buffers and grids — registered so the
// keybar hints, the inline row hints and the key handlers all read the same
// Key (hand-written copies drifted: 27b's edit row once advertised "M
// reveal" for what was ctrl-x). None is Mutating: the writes they lead to
// go through AddSecretKey/AddConfigMapKey/Meta's own policy.
var (
	// SecretMaskToggle is 27b's mask toggle on the add/edit value buffer —
	// a chord so a plain 'x' (or 'M') stays typeable. The label is
	// per-state ("re-mask"/"reveal"), so call sites pair Key with their own.
	SecretMaskToggle = Verb{ID: "secret-mask-toggle", Key: "ctrl+x", Label: "re-mask", Kinds: []kube.ResourceKind{kube.KindSecret}}
	// SecretPaste is the paste chord tui.RoutePaste claims (tui.IsPasteKey),
	// advertised on 27b's buffers because a pasted value is never echoed.
	SecretPaste = Verb{ID: "secret-paste", Key: "ctrl+v", Label: "paste (never echoed)", Kinds: []kube.ResourceKind{kube.KindSecret}}
	// ConfigMapBufferApply is 27a's buffer-editor apply — alt+a rather than
	// 17a's ctrl-s, which is the terminal's XOFF flow-control key in some
	// environments; ↵ has to stay a newline there.
	ConfigMapBufferApply = Verb{ID: "configmap-buffer-apply", Key: "alt+a", Label: "apply", Kinds: []kube.ResourceKind{kube.KindConfigMap}}
	// MetaRemoveKey is 26a's removal of the selected label/annotation
	// (navigation mode only — no buffer is open, so a bare letter is safe).
	MetaRemoveKey = Verb{ID: "meta-remove-key", Key: "D", Label: "remove key · y/N"}
	// MetaCopyKeyValue is 26a's copy of the selected row as key=value.
	MetaCopyKeyValue = Verb{ID: "meta-copy-key-value", Key: "y", Label: "copy key=value"}
)

// Port-forward verbs (13a/13c, docs/design README.md). Forward pushes the
// picker from a Pod/Service/Deployment row; Stop/Restart/CopyForwardURL act
// on a Forwards row immediately (Mutating: false — a local port-forward
// session isn't cluster state, so OFFLINE/NoCluster shouldn't grey these
// out); StopAllForwards is the app's only forward verb with a confirm
// (TierInline, a bespoke browse-owned gate rather than actions.Controller,
// since it isn't a kube.Mutator operation — see browse/forwards.go).
var (
	Forward = Verb{
		ID: "forward", Key: "f", Label: "forward",
		Kinds: []kube.ResourceKind{kube.KindPod, kube.KindService, kube.KindDeployment},
	}
	StopForward = Verb{
		ID: "stop-forward", Key: "x", Label: "stop",
		Kinds: []kube.ResourceKind{kube.KindForward},
	}
	RestartForward = Verb{
		ID: "restart-forward", Key: "r", Label: "restart",
		Kinds: []kube.ResourceKind{kube.KindForward},
	}
	StopAllForwards = Verb{
		ID: "stop-all-forwards", Key: "X", Label: "stop all",
		Kinds: []kube.ResourceKind{kube.KindForward},
	}
	CopyForwardURL = Verb{
		ID: "copy-forward-url", Key: "y", Label: "copy url",
		Kinds: []kube.ResourceKind{kube.KindForward},
	}
	CopyServiceClusterIP = Verb{
		ID: "copy-service-cluster-ip", Key: "alt+c", Label: "copy cluster ip",
		Kinds: []kube.ResourceKind{kube.KindService},
	}
	CopyServiceExternalIP = Verb{
		ID: "copy-service-external-ip", Key: "alt+e", Label: "copy external ip",
		Kinds: []kube.ResourceKind{kube.KindService},
	}
)

// Routing-table verbs (23a/23b, docs/design README.md), used only inside
// tasks/routetable's own Keybar — not gated by Kinds since that field only
// matters to browse's per-kind row keybar, which never renders this screen's
// keys itself.
var (
	// CopyRouteURL is 23a's "y copies the full URL" — a screen-local
	// override of the 'y' key, the same reuse CopyForwardURL already makes
	// for Forwards rather than the global YAML verb.
	CopyRouteURL = Verb{ID: "copy-route-url", Key: "y", Label: "copy url"}
	// OpenParentGateway is 23b's "p opens the Gateway" from an HTTPRoute's
	// routing table.
	OpenParentGateway = Verb{ID: "open-parent-gateway", Key: "p", Label: "parent gateway"}
	// CopyRouteYAML is 23a/23b's "Y copies the full yaml" — a screen-local
	// override of the 'Y' key, copying the viewed object's YAML to the
	// clipboard directly rather than opening 8a (same "reuse a local key"
	// precedent as CopyRouteURL/CopyForwardURL).
	CopyRouteYAML = Verb{ID: "copy-route-yaml", Key: "Y", Label: "copy yaml"}
	// FocusTLSStrip is 23a's "tab" toggle onto the below-table TLS-secret
	// strip, so ↵ there can jump to the referenced Secret (docs/design
	// README.md §23a: "a strip above the keybar names each secret — ↵ there
	// jumps to it").
	FocusTLSStrip = Verb{ID: "focus-tls-strip", Key: "tab", Label: "tls secret"}
	// OpenTLSSecret is 23a's "↵ jumps to it" once FocusTLSStrip has moved
	// focus onto the TLS strip.
	OpenTLSSecret = Verb{ID: "open-tls-secret", Key: "↵", Label: "open secret"}
)

// The confirm vocabulary: the answers every confirm takes, whatever verb
// it guards. actions.Controller.HandleConfirmKey matches on these keys and
// ConfirmHints renders them, so a screen's y/N keybar and the keys its
// confirm actually answers to come from one place (review 10 L5: the
// copy-pasted routers and hand-built hints are how ctrl+k's force delete
// came to be advertised under one key and handled under another).
var (
	// ConfirmAccept answers a y/N confirm (inline, or a modal y/N card).
	ConfirmAccept = Verb{ID: "confirm-accept", Key: "y", Label: "confirm"}
	// ConfirmDecline is n, the y/N confirm's unadvertised second way out —
	// ConfirmCancel is the one the keybar shows.
	ConfirmDecline = Verb{ID: "confirm-decline", Key: "n", Label: "cancel"}
	// ConfirmCancel abandons any confirm, the type-the-name modal included.
	ConfirmCancel = Verb{ID: "confirm-cancel", Key: "esc", Label: "cancel"}
	// ConfirmSubmit is the type-the-name modal's ↵, which executes only once
	// the typed name (a marked set: the count) matches. Its Key is the key
	// press's name, not a display glyph: the modal draws its own ↵ key row.
	ConfirmSubmit = Verb{ID: "confirm-submit", Key: "enter", Label: "confirm"}
)

// ConfirmHints is a y/N confirm's keybar group — the bespoke gates outside
// actions.Controller (the kubectl edit/debug handoffs, 13a's stop-all) use
// it directly; a Controller confirm uses Controller.ConfirmHints, which adds
// the force-delete escalation when it applies.
func ConfirmHints() []tui.KeyHint {
	return []tui.KeyHint{ConfirmAccept.Hint(), ConfirmCancel.Hint()}
}

// All is every registered verb, for the help overlay's fixed SCOPE/GLOBAL
// columns and for ByID lookups.
var All = []Verb{
	Goto, Filter, Open, Logs, YAML, Exec, NodeDebug, NodeDebugDetail, PodDebug, DebugCleanup, Edit, Events,
	Namespace, Context, AllNamespaces, JumpNamespace, ToggleGroup, PageUp, PageDown, Help, Retry, WhoCan,
	HelmValues, EnvMounts, HelmHistory, HelmPods, Mark, MarkAll,
	LogPause, LogFollow, LogNextWarning, LogNextError, LogToggleWrap, LogToggleTime,
	LogCycleSince, LogCycleContainer, LogCopyView,
	FluxReconcile, FluxSuspend, FluxSource,
	ArgoRefresh, ArgoSync, ArgoURL,
	CertRenew,
	Delete, ForceDelete, RolloutRestart, JobRetry, JobReplace, JobSuspend, JobAttemptDiff, JobIndexFailedOnly,
	CronJobRunNow, CronJobSuspend, CronJobSetSchedule,
	CronJobCopyCommand, CronJobScheduleCopy, CronJobFocusTimezone, CronJobScheduleUndo, CronJobScheduleFullEdit,
	Cordon, Drain, Rollback, RolloutUndo, Scale, SetImage, SetResources, Meta,
	AddSecretKey, RemoveSecretKey,
	AddConfigMapKey, RemoveConfigMapKey, RestartConfigMapConsumers,
	SecretMaskToggle, SecretPaste, ConfigMapBufferApply, MetaRemoveKey, MetaCopyKeyValue,
	Forward, StopForward, RestartForward, StopAllForwards, CopyForwardURL, CopyServiceClusterIP, CopyServiceExternalIP,
	CopyRouteURL, OpenParentGateway, CopyRouteYAML, FocusTLSStrip, OpenTLSSecret,
	ConfirmAccept, ConfirmDecline, ConfirmCancel, ConfirmSubmit,
}

// ByID looks up a registered verb by its stable ID.
func ByID(id string) (Verb, bool) {
	for _, v := range All {
		if v.ID == id {
			return v, true
		}
	}
	return Verb{}, false
}

// ForceDeleteModalHint is the type-the-name modal's detail-line pointer at
// ForceDelete, rendered from the registry so the advertised chord is the one
// actions.Controller.EscalateForceDelete's callers match on.
func ForceDeleteModalHint() string {
	return ForceDelete.Key + " " + ForceDelete.Label + " (immediate)"
}
