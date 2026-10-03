// Package browse is the inverted-layout main table: the one resting screen
// for every resource kind (mvp-plan.md §Phase 1, docs/design/README.md
// screens 2a/10c). It replaced the pre-redesign root/per-kind-list/
// resource-list screens as the app's root task.
//
// This covers the Chrome v2 Screen contract, loading/ready/empty states,
// the 10c empty-namespace explainer, the 2a health strip, filter, default
// unhealthy-first sort, live CPU/MEM bars for pods, and pushing the
// existing podlogs screen on 'l'. Row-open navigation to a detail/YAML view
// and mutating verbs (delete, …) land in later phases.
package browse

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/components"
	"github.com/kute-dev/kute/internal/tui/metapanel"
)

// pollInterval is the metrics/sync poll cadence (the header's "sync 2s"
// chip, mvp-plan.md's Defaults assumed).
const pollInterval = 2 * time.Second

// reloadDebounce coalesces bursts of watch events into one reload (Phase 1:
// "watch events arrive in bursts").
const reloadDebounce = 250 * time.Millisecond

// MetricsReader is the live pod-usage seam browse needs for the CPU/MEM
// mini-bars — satisfied by *kube.Cluster and *fake.Cluster.
type MetricsReader interface {
	PodMetricsByNamespace(ctx context.Context, namespace string) (map[string]kube.PodMetrics, error)
	// ContainerMetricsByNamespace is 25a's per-container usage seam — like
	// PodMetricsByNamespace but keyed by kube.PodKey(namespace, name) then
	// container name,
	// since 25a's per-field USAGE bar needs the active container's own
	// usage, not the whole pod's summed usage a container tab switch would
	// otherwise show unchanged.
	ContainerMetricsByNamespace(ctx context.Context, namespace string) (map[string]map[string]kube.PodMetrics, error)
}

// OpenLogsFunc pushes the log-stream screen for pod, mirroring the
// pre-redesign pods screen's equivalent shape so app.go's existing streamer
// wiring carries over unchanged. container names which container to start
// streaming — empty falls back to index 0 (browse has no per-container
// selection of its own, so it always passes "").
type OpenLogsFunc func(pod kube.Pod, container string, width, height int) (tea.Model, tea.Cmd)

// ConnRetrier lets browse trigger an immediate reconnect probe on 'r' while
// offline (4a), bypassing the exponential backoff wait — satisfied by
// *kube.Cluster and *fake.Cluster.
type ConnRetrier interface {
	RetryNow()
}

// CacheSyncChecker is optionally implemented by a Lister whose cache
// populates asynchronously (*kube.Cluster's informers, right after launch or
// mid SwitchContext). ListRaw reads the cache regardless of sync state, so
// an empty rowsLoadedMsg during that window looks identical to a genuinely
// empty namespace; applyRowsLoaded type-asserts for this to tell them apart
// instead of flashing the empty state before the first real list lands.
// *fake.Cluster doesn't implement it — its cache is always populated
// synchronously, so the type assertion just falls through to "synced".
type CacheSyncChecker interface {
	Synced() bool
}

// KindSyncChecker is CacheSyncChecker's per-kind refinement, preferred over
// it wherever both are available. Informers fill independently and are
// started independently, so "has this cluster connected" is the wrong
// question when the one being asked is "is this empty list of Secrets
// trustworthy". A lister implementing this answers for the kind actually on
// screen. namespace is the namespace the caller's own read used — see
// tui.KindSyncChecker's doc comment.
type KindSyncChecker interface {
	KindSynced(kind kube.ResourceKind, namespace string) bool
}

// OpenNodeDetailFunc pushes tasks/nodedetail (11b) for the named node.
type OpenNodeDetailFunc func(nodeName string, width, height int) (tea.Model, tea.Cmd)

// PodSiblingRef names one sibling Pod row for OpenPodDetailFunc's `[`/`]`
// movement — namespace-qualified, same reasoning as CronJobSiblingRef:
// all-namespaces mode lists same-named pods side by side.
type PodSiblingRef struct {
	Namespace string
	Name      string
}

// OpenPodDetailFunc pushes tasks/poddetail (5a) for pod. siblings/index are
// the current visible list's ordered namespace-qualified pod refs + the
// selected row's position, so poddetail's j/k can move to the next/prev pod
// without leaving detail (mvp-plan.md §5a).
type OpenPodDetailFunc func(pod kube.Pod, siblings []PodSiblingRef, index int, width, height int) (tea.Model, tea.Cmd)

// OpenYAMLFunc pushes tasks/yamlview (8a) for the named object of kind —
// works for any kind, unlike OpenLogsFunc/OpenPodDetailFunc which are
// Pod-only (docs/design README.md: "y opens the YAML view on any selected
// object, any kind").
type OpenYAMLFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenEventsFunc pushes tasks/events (9b) namespace-scoped for browse's 'e'
// (docs/design README.md §9b: "reached by e from browse, namespace-scoped")
// — namespace == "" mirrors 6b's all-namespaces triage rather than a
// separate mode.
type OpenEventsFunc func(namespace string, width, height int) (tea.Model, tea.Cmd)

// OpenTimelineFunc pushes tasks/timeline (16a) namespace-scoped for
// browse's 't' (docs/design README.md §16a) — namespace == "" mirrors 6b's
// all-namespaces triage, same rule OpenEventsFunc already follows.
type OpenTimelineFunc func(namespace string, width, height int) (tea.Model, tea.Cmd)

// OpenObjectTimelineFunc pushes tasks/timeline (16b) object-scoped for
// browse's 't' on a Deployment/Pod/Node row — the same signature
// poddetail/nodedetail's own OpenTimelineFunc already uses, so app.go's
// openObjectTimelineFunc closure is reused directly rather than duplicated.
// openSelectedTimeline prefers this over the namespace-scoped OpenTimeline
// whenever the cursor sits on one of those three kinds.
type OpenObjectTimelineFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenExecFunc pushes tasks/execpicker (10a) for a pod with more than one
// container — browse execs single-container pods directly via
// kube.ExecSpec without pushing a task (docs/design README.md §10a:
// "skipped entirely for single-container pods").
//
// probed carries the shells the 'x' pre-check already found per container
// (a successful probe only, possibly empty), so the picker probes just the
// rest instead of re-running every kubectl exec; nil when nothing was probed.
type OpenExecFunc func(namespace, name string, containers []kube.ContainerInfo, probed map[string][]string, width, height int) (tea.Model, tea.Cmd)

// OpenForwardFunc pushes tasks/forwardpicker (13a) for a Pod/Service/
// Deployment row (docs/design README.md §13a).
type OpenForwardFunc func(target kube.ForwardTarget, width, height int) (tea.Model, tea.Cmd)

// OpenDebugFunc pushes tasks/debugpanel (§41b/§41c) for a Pod with no shell
// in any container, or one that won't stay running — the shell pre-check
// (exec.go's detectPodShellsCmd) decides when to call this instead of
// OpenExec/execCmd; podPhase/waiting are how the panel picks its own
// default mode (attach vs. copy) and hard-disables attach when it can't
// work. podPhase is the API phase, not the row's display reason.
type OpenDebugFunc func(namespace, name string, containers []kube.ContainerInfo, podPhase string, waiting bool, width, height int) (tea.Model, tea.Cmd)

// OpenNodeDebugFunc pushes tasks/debugpanel (§41d) for a Node row's 'x' —
// replaces the retired standalone NodeShell verb ('s'), see verbs.go's
// NodeDebug doc comment. podCount is the live pod count browse's Nodes
// columns already carry.
type OpenNodeDebugFunc func(name string, podCount, width, height int) (tea.Model, tea.Cmd)

// ObjectSiblingRef names one sibling row for OpenObjectDetailFunc's j/k
// movement — namespace-qualified, same reasoning as PodSiblingRef:
// all-namespaces mode lists same-named objects side by side.
type ObjectSiblingRef struct {
	Namespace string
	Name      string
}

// OpenObjectDetailFunc pushes tasks/objectdetail (14d) for a discovered CRD
// kind's row (resources.Descriptor.Custom) — siblings/index are the current
// visible list's ordered namespace-qualified refs + the selected row's
// position, same shape as OpenPodDetailFunc, so objectdetail's j/k can move
// without leaving detail.
type OpenObjectDetailFunc func(kind kube.ResourceKind, namespace, name string, siblings []ObjectSiblingRef, index, width, height int) (tea.Model, tea.Cmd)

// OpenRouteTableFunc pushes tasks/routetable (23a/23b) for an Ingress row or
// a discovered Gateway API kind's row (HTTPRoute/GRPCRoute/TCPRoute/Gateway)
// — see routes.go's isRouteKind.
type OpenRouteTableFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenWhoCanFunc pushes tasks/whocan (22a), pre-filled with verb/resource/
// namespace — the `g "who"` goto entry (kube.KindWhoCan, handled by
// switchKind's carve-out below rather than an ordinary registry list, since
// there's nothing to list) and the 4b 403 card's 'w' recovery key (denied
// verb/resource/namespace pre-filled, docs/design README.md §22a: "arriving
// with the failed verb+resource pre-filled").
type OpenWhoCanFunc func(verb, resource, namespace string, width, height int) (tea.Model, tea.Cmd)

// OpenFluxDetailFunc pushes tasks/fluxdetail (§31a) for a Flux reconciler
// row. Separate from OpenObjectDetailFunc because §31a is a different
// screen, not a variant of 14d — see the fluxdetail package doc.
type OpenFluxDetailFunc func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenCertChainFunc pushes tasks/certchain (§35a) for a Certificate row.
// Separate from OpenObjectDetailFunc for the same reason OpenFluxDetailFunc
// is: §35a is a different screen, not a variant of 14d. No Kind param,
// unlike OpenFluxDetailFunc — the kind is always Certificate.
type OpenCertChainFunc func(namespace, name string, width, height int) (tea.Model, tea.Cmd)

// CronJobSiblingRef names one sibling CronJob row for OpenCronJobDetailFunc's
// `[`/`]` movement (0.8.0 plan §3.6, tasks/cronjobdetail Phase 7) —
// namespace-qualified, like OpenPodDetailFunc's and
// OpenObjectDetailFunc's, because all-namespaces mode can list two different CronJobs
// sharing a name (Phase 4 test 11: "including all-namespaces mode and
// duplicate names across namespaces").
type CronJobSiblingRef struct {
	Namespace string
	Name      string
}

// OpenCronJobDetailFunc pushes tasks/cronjobdetail (§36e) for the named
// CronJob — siblings/index are the current visible list's ordered
// namespace-qualified refs + the selected row's position, the same
// "siblings + index" shape as OpenPodDetailFunc, so `[`/`]` can move between
// sibling CronJobs without leaving detail. Replaces the pre-0.8.0
// openCronJobPods jump-to-Pods shortcut (0.8.0 plan §2's "Enter" row).
type OpenCronJobDetailFunc func(namespace, name string, siblings []CronJobSiblingRef, index, width, height int) (tea.Model, tea.Cmd)

// JobSiblingRef names one sibling Job row for OpenJobAttemptsFunc's `[`/`]`
// movement — namespace-qualified, same reasoning as CronJobSiblingRef.
type JobSiblingRef struct {
	Namespace string
	Name      string
}

// OpenJobAttemptsFunc pushes §37b/§37d (tasks/jobattempts) for the named
// Job — siblings/index are the current visible list's ordered namespace-
// qualified refs + the selected row's position, same "siblings + index"
// shape as OpenCronJobDetailFunc. Replaces the pre-v0.9.0 openJobPods
// jump-to-Pods shortcut (§37a: "↵ opens the attempt ledger, not a describe
// page").
type OpenJobAttemptsFunc func(namespace, name string, siblings []JobSiblingRef, index, width, height int) (tea.Model, tea.Cmd)

// OpenCronJobScheduleFunc pushes tasks/cronjobschedule (36d) for the
// selected CronJob's own schedule editor — replaces Phase 0-5's placeholder
// beginCronJobSetSchedule inline panel (cronjobschedule.go, deleted by
// Phase 6) now that 'S' navigates to a real pushed screen instead of
// gathering a typed buffer directly in browse's own model.
type OpenCronJobScheduleFunc func(namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenHelmHistoryFunc pushes tasks/helmhistory (18a's `h`) for a release's
// full revision rail.
type OpenHelmHistoryFunc func(namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenHelmDetailFunc pushes the release transaction/live-evidence detail.
type OpenHelmDetailFunc func(release kube.HelmRelease, width, height int) (tea.Model, tea.Cmd)

// OpenHelmValuesFunc pushes tasks/yamlview (18a's `v`) read-only on
// release's decoded values.
type OpenHelmValuesFunc func(release kube.HelmRelease, width, height int) (tea.Model, tea.Cmd)

// OpenSecretDataFunc pushes tasks/secretdata (27b) for a Secret row's Data
// view.
type OpenSecretDataFunc func(namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenConfigMapDataFunc pushes tasks/configmapdata (27a) for a ConfigMap
// row's Data view.
type OpenConfigMapDataFunc func(namespace, name string, width, height int) (tea.Model, tea.Cmd)

// OpenOverviewFunc pushes tasks/overview (19a) — the `g "ov"` goto entry
// (kube.KindOverview, handled by switchKind's carve-out below, the same
// shape as OpenWhoCanFunc's KindWhoCan carve-out since there's nothing to
// list).
type OpenOverviewFunc func(width, height int) (tea.Model, tea.Cmd)

// OpenFluxTreeFunc pushes tasks/fluxtree (§30b) — the `g "flux"` goto entry
// point, same no-argument shape as OpenOverviewFunc since the tree is
// cluster-wide and carries no row scope of its own.
type OpenFluxTreeFunc func(width, height int) (tea.Model, tea.Cmd)

// Config are the dependencies browse needs, per repo convention
// (package-local Config struct, interface-typed fields, New fills zero
// values).
type Config struct {
	Session     *tui.Session
	Lister      resources.RawLister
	Metrics     MetricsReader
	NodeMetrics NodeMetricsReader
	Mutator     kube.Mutator
	// Shells answers which shells a container actually has (debug.go's
	// detectPodShellsCmd) — a nil value falls back to openSelectedExec's
	// old synchronous single-vs-multi-container routing, so 'x' still
	// works with this seam unwired, just without §41a's distroless fork.
	Shells              ShellDetector
	OpenLogs            OpenLogsFunc
	OpenNodeDetail      OpenNodeDetailFunc
	OpenPodDetail       OpenPodDetailFunc
	OpenYAML            OpenYAMLFunc
	OpenEvents          OpenEventsFunc
	OpenTimeline        OpenTimelineFunc
	OpenObjectTimeline  OpenObjectTimelineFunc
	OpenExec            OpenExecFunc
	OpenDebug           OpenDebugFunc
	OpenNodeDebug       OpenNodeDebugFunc
	OpenForward         OpenForwardFunc
	OpenObjectDetail    OpenObjectDetailFunc
	OpenRouteTable      OpenRouteTableFunc
	OpenWhoCan          OpenWhoCanFunc
	OpenFluxDetail      OpenFluxDetailFunc
	OpenCertChain       OpenCertChainFunc
	OpenCronJobDetail   OpenCronJobDetailFunc
	OpenCronJobSchedule OpenCronJobScheduleFunc
	OpenJobAttempts     OpenJobAttemptsFunc
	OpenHelmHistory     OpenHelmHistoryFunc
	OpenHelmDetail      OpenHelmDetailFunc
	OpenHelmValues      OpenHelmValuesFunc
	OpenSecretData      OpenSecretDataFunc
	OpenConfigMapData   OpenConfigMapDataFunc
	OpenOverview        OpenOverviewFunc
	OpenFluxTree        OpenFluxTreeFunc
	// Forwards is the app-wide port-forward registry (13c's stop/restart/
	// stop-all verbs act on it directly, unlike OpenForward's picker push) —
	// nil disables those verbs the same way a nil Mutator disables delete.
	Forwards    *kube.ForwardManager
	Retrier     ConnRetrier
	LoadTimeout time.Duration
	// CurrentUser is the kubeconfig user name (kube.Context.UserName) — the
	// fallback for who §36b's run-now and §37c's rerun stamp into
	// kube.AnnotationTriggeredBy when Identity has no reviewed answer. Empty
	// falls back to a generic "kute" attribution rather than leaving the
	// annotation blank.
	CurrentUser string
	// Identity is the connect-time SelfSubjectReview answer §33a's sync
	// credits as operation.initiatedBy.username, and §36b/§37c's stamps
	// prefer over CurrentUser. Read at Begin time, not
	// captured here, because the review lands after browse is built and is
	// replaced on every context switch. nil credits "kute".
	Identity IdentityReader
	// InitErr is the reason Lister is nil (cluster unreachable at launch) —
	// surfaced verbatim instead of a generic message when set. Phase 4's
	// tasks/setup screen replaces this with the real 4c/10b recovery flow.
	InitErr error
	// Demo is true for --demo (app.go's buildDemoBrowseTask) — guards
	// execCmd's single-container fast path (openSelectedExec, debug.go's
	// routePodShellsProbed) so 'x' never shells out to a real kubectl
	// against a fake pod: Shells is faked (kube/fake/shells.go) so the
	// shell pre-check still renders realistic states, but the actual exec
	// is a live tty handoff with no real cluster on the other end.
	Demo bool
}

type Model struct {
	width, height int

	session             *tui.Session
	lister              resources.RawLister
	metrics             MetricsReader
	nodeMetricsSrc      NodeMetricsReader
	mutator             kube.Mutator
	identity            IdentityReader
	shells              ShellDetector
	actions             actions.Controller
	openLogs            OpenLogsFunc
	openNodeDetail      OpenNodeDetailFunc
	openPodDetail       OpenPodDetailFunc
	openYAML            OpenYAMLFunc
	openEvents          OpenEventsFunc
	openTimeline        OpenTimelineFunc
	openObjectTimeline  OpenObjectTimelineFunc
	openExec            OpenExecFunc
	openDebug           OpenDebugFunc
	openNodeDebug       OpenNodeDebugFunc
	openForward         OpenForwardFunc
	openObjectDetail    OpenObjectDetailFunc
	openRouteTable      OpenRouteTableFunc
	openWhoCan          OpenWhoCanFunc
	openFluxDetail      OpenFluxDetailFunc
	openCertChain       OpenCertChainFunc
	openCronJobDetail   OpenCronJobDetailFunc
	openCronJobSchedule OpenCronJobScheduleFunc
	openJobAttempts     OpenJobAttemptsFunc
	openHelmHistory     OpenHelmHistoryFunc
	openHelmDetail      OpenHelmDetailFunc
	openHelmValues      OpenHelmValuesFunc
	openSecretData      OpenSecretDataFunc
	openConfigMapData   OpenConfigMapDataFunc
	openOverview        OpenOverviewFunc
	openFluxTree        OpenFluxTreeFunc
	forwards            *kube.ForwardManager
	retrier             ConnRetrier
	timeout             time.Duration
	currentUser         string
	demo                bool
	// execFeedback carries a non-zero kubectl-exec exit's message (docs/design
	// README.md §10a: "exit returns to the same pod with a feedback line on
	// non-zero exit") — single-container pods exec directly from browse
	// without pushing execpicker, so browse needs its own copy of this
	// transient state rather than relying on the picker's. Also carries
	// kubectl edit's exit message (editResultMsg, edit.go) and node shell's
	// (nodeShellResultMsg) — same transient channel, same reasoning.
	execFeedback string
	// shellProbing is set while the 'x' async shell probe (debug.go) is in
	// flight, so a repeat 'x' is ignored instead of starting a second probe.
	shellProbing bool
	// pendingEdit is non-nil while 'E' edit's PROD-only y/N line is showing
	// (edit.go's bespoke gate — verbs.Resolve(verbs.Edit, …)) — nil the rest of the
	// time, including right after a non-prod 'E' press, which skips this
	// state entirely and launches kubectl edit directly.
	pendingEdit *editTarget
	// pendingStopAllForwards is true while 13c's "stop all" (X) inline y/N
	// is showing — the same bespoke (non-actions.Controller) gate shape as
	// pendingEdit, since stopping local forward sessions isn't a
	// kube.Mutator operation (browse/forwards.go).
	pendingStopAllForwards bool
	// pendingScale is non-nil while 17b's +/− numeric prompt is showing
	// (scale.go) — a bespoke gate like pendingEdit, since Scale needs a
	// typed-ahead replica count gathered before there's an action to Begin,
	// rather than actions.Controller's own y/N/type-name confirmation shapes.
	pendingScale *scaleTarget
	// pendingSetImage is non-nil while 24a's inline set-image/set-tag panel
	// is showing (setimage.go) — a bespoke gate like pendingScale, since
	// there's a container/tag/history buffer to gather before there's an
	// action to Begin.
	pendingSetImage *setImageTarget
	// pendingSetResources is non-nil while 25a's inline resources panel is
	// showing (setresources.go) — a bespoke gate like pendingSetImage, since
	// there's a per-field request/limit buffer to gather (plus a dry-run
	// round-trip) before there's an action to Begin.
	pendingSetResources *setResourcesTarget
	// pendingMeta is non-nil while 26a's inline labels/annotations panel is
	// showing (the shared internal/tui/metapanel editor, hosted per meta.go)
	// — a bespoke gate like pendingSetImage/pendingSetResources, since
	// there's a per-row value buffer (or, while adding, a key+value pair) to
	// gather before there's an action to Begin.
	pendingMeta *metapanel.Model
	// pendingCronJobRun is non-nil while §36b's ctrl-r run-now preflight is
	// showing (cronjob_actions.go) — a bespoke gate like pendingScale, since
	// run-now stages its own preview (overlap warning, generated name)
	// before ever calling actions.Begin on a TierNone verb: the staging step is
	// itself the confirmation (0.8.0 plan §36b/Phase 5), so there's no
	// TierInline/TierModal for actions.Controller to render.
	pendingCronJobRun *cronJobRunTarget
	// pendingCronJobResume is non-nil while §36c's resume preflight is
	// showing (cronjob_actions.go) — same "stage first, TierNone commit"
	// shape as pendingCronJobRun, covering both a single selected row and a
	// marked set (targets has one entry or many).
	pendingCronJobResume *cronJobResumeTarget
	// pendingJobRerun is non-nil while §37c's 'R' create-vs-replace
	// choice is showing (job_actions.go) — a bespoke gate like
	// pendingCronJobRun, since the choice list stages its own preview
	// (generated rerun name, amber diagnostic) before either committing
	// directly (create, TierNone) or handing off to the ordinary
	// actions.Controller confirm (replace, tiered).
	pendingJobRerun *jobRerunTarget
	// lastCronJobRunName is the Job name commitCronJobRun just staged for
	// creation — actions.ResultMsg carries no NewName of its own (Scope
	// isn't part of the message), so update.go's "cronjob-run-now-" branch
	// reads this to show "✓ job created · <name>" once the ResultMsg for
	// that exact action lands (0.8.0 plan §36b: "On success, remain on the
	// list, show the created name").
	lastCronJobRunName string
	// marks is 20a's marked set (bulk.go), keyed by markKey(namespace, name)
	// so 6b's cross-namespace grouped view can't collide two same-named rows
	// in different namespaces. nil/empty means no marks — every marks-aware
	// render path (health strip, mode pill, table's mark column) must render
	// zero chrome in that case (13d's rule).
	marks     map[string]bool
	kind      kube.ResourceKind
	namespace string
	desc      resources.Descriptor

	rows      []resources.Row
	fetchedAt time.Time
	// cronJobSummaries backs §36a (CronJob kind only) — the same cache-local
	// aggregation m.rows was projected from (resources.BuildCronJobSummaries
	// + resources.ProjectCronJob), held onto with absolute timestamps so the
	// one-second UI clock tick (cronJobTick) can re-derive m.rows' relative
	// cells from a fresh `now` without a lister call. Cleared by
	// resetAndLoad; nil for every other kind.
	cronJobSummaries []resources.CronJobSummary
	// cronJobJobsErr is non-nil when §4.4's Job read failed on the most
	// recent CronJob load — Job.Status.Active/history stays unavailable
	// rather than rendering as a fake "no retained runs" (markCronJobHistoryUnavailable),
	// and it drives the health strip's own inline note. Always nil for
	// every other kind.
	cronJobJobsErr error
	// auxKindsDeniedNote is non-empty when a secondary cache one of the
	// current kind's own columns/prompts reads (auxKinds) came back
	// Forbidden on the most recent Ready load — rendered as one extra strip
	// line (view.go) rather than blanking or wrongly-populating the cells
	// that depend on it. Reset on every kind/namespace switch by
	// resetAndLoad, same lifecycle as cronJobJobsErr.
	auxKindsDeniedNote string
	// jobListSummaries backs §37a (Job kind only) — same cache-local
	// aggregation shape as cronJobSummaries: absolute timestamps, held onto
	// so the one-second UI clock tick can re-derive m.rows' relative
	// DURATION/AGE cells without a lister call. Cleared by resetAndLoad; nil
	// for every other kind.
	jobListSummaries []resources.JobListSummary
	// rowColumns is how many Descriptor columns m.rows were projected with —
	// resources.Row's contract is that Cells align positionally with the
	// descriptor's Columns, and a custom kind's descriptor can change shape
	// under a loaded list (printer columns arrive per-kind on first read, and
	// a context switch resets them again). Rows whose shape doesn't match the
	// live m.desc are misaligned by definition, so every path that adopts
	// rows records this and every path that would show a mismatched set drops
	// it instead (dropStaleShapedRows).
	rowColumns int
	// rowCache holds the last successfully-loaded rows per
	// context+kind+namespace seen this session (browseCacheKey), so revisiting one mid-session can
	// show cached rows dimmed instead of the skeleton loader (docs/design
	// README.md §15a: "revisiting a kind seen this session: cached rows
	// dimmed instead of skeletons" — the same muted treatment 4a's stale
	// grammar already gives an offline table). Never cleared by
	// resetAndLoad — it outlives any single kind/namespace view on purpose,
	// including across a context switch: entries are keyed by context so a
	// switch never shows the previous cluster's rows, and each carries the
	// column shape its rows were projected with.
	rowCache map[browseCacheKey]cachedRows
	// cachedView is true while m.state is TaskStateLoading but m.rows holds
	// a rowCache hit rather than fresh data — Body()/tableBody read it to
	// render the muted cached snapshot instead of the skeleton, and it's
	// cleared the moment a real rowsLoadedMsg lands (applyRowsLoaded).
	cachedView bool
	// nodeCount backs the health strip's "36 pods · 3 nodes" right side
	// (Pods kind only; 0 = unknown, suffix omitted).
	nodeCount int
	// conn is the last kube.ConnStateMsg forwarded by the root shell — the
	// header badge's "· 12ms" latency, and (once Phase is Reconnecting/
	// Failed) the 4a offline banner/stale-strip/muted-table treatment. Zero
	// until the first ping completes.
	conn kube.ConnState
	// now is the wall-clock time as of the last kube.ConnStateMsg — 4a's
	// "next in Ns"/"NNs old" figures are computed from this rather than a
	// clock read inside Render (render must stay pure: f(model, theme,
	// size)). It ticks roughly every pingInterval while offline, since the
	// health ping loop re-emits ConnStateMsg on every attempt.
	now time.Time
	// podMetrics is nil until the first successful poll (browse renders "–"
	// bars until then); keyed by kube.PodKey(namespace, name), never bare
	// name — all-namespaces mode lists same-named pods side by side.
	podMetrics map[string]kube.PodMetrics
	// aggs caches the whole-list figures a frame shows (health tallies,
	// metrics bar denominators, majority kubelet version, namespace count),
	// kept in step by Update so a frame costs its visible window rather than
	// the list (aggregates.go).
	aggs rowAggregates
	// pods is the fuller kube.PodFromObject projection (Pod and CronJob
	// kinds only — the CronJob branch reuses its own best-effort Pod read,
	// see loadCronJobRows) — Row only carries display Cells, but the 'l'
	// logs verb needs each pod's container names. Keyed by
	// kube.PodKey(namespace, name) for the same reason as podMetrics.
	pods map[string]kube.Pod
	// helmReleases is the fuller decoded kube.HelmRelease (HelmRelease kind
	// only), keyed by namespace/name — Row only carries display Cells, but
	// 'v'/'h'/'R' need the chart/values/revision fields it doesn't.
	helmReleases map[string]kube.HelmRelease // keyed by namespace/name
	// chartCacheNote caveats the LATEST column's data source on the 18a
	// strip ("repo cache 6d old" / "no helm repo cache"), resolved in load()
	// because it depends on the clock and Render must not read one.
	chartCacheNote chartCacheNote

	// nodeMetrics/nodeCapacity/podCountByNode/clusterPodTotal/nodePodHealth
	// back 11a's Nodes columns and health-strip right side (Node kind only)
	// — see nodes.go's loadNodeExtras and
	// nodeMetricCell/nodePodsCell/nodeHealthCell.
	nodeMetrics     map[string]kube.NodeMetric
	nodeCapacity    map[string]nodeCapacity
	podCountByNode  map[string]int
	clusterPodTotal int
	nodePodHealth   map[string]resources.HealthCounts
	// drainImpactByNode is what a drain would do to each node's pods
	// (kube.DrainImpact) — the drain confirm's consequence line.
	drainImpactByNode map[string]kube.DrainImpact

	visible []filterMatch // rows after the filter, before 6b's fold/collapse
	// expandedGroups tracks which 6b namespace groups the user has manually
	// expanded past their triage-default collapsed state (grouping.go's
	// buildDisplayRows) — absent/false means collapsed. Reset by
	// resetAndLoad alongside the rest of the per-view UI state below, since
	// it's meaningless once the kind/namespace/context changes.
	expandedGroups map[string]bool
	// display is visible expanded into 6b's render/selection list — one
	// entry per rendered components.Row, including synthetic group header/
	// fold/collapsed-summary lines (grouping.go's buildDisplayRows).
	// selected indexes THIS list, not visible — every entry maps 1:1 to one
	// Table.Rows entry, so Table.Selected can just be set to selected
	// directly, with no separate Rows-space translation (grouping.go used
	// to carry groupBoundaries/rowsPosition for exactly that, before this
	// field replaced them).
	display  []displayRow
	selected int
	offset   int
	// pendingSelect names a row to select once the next rowsLoadedMsg lands
	// — set by a jump-palette resource Enter (tui.GotoResourceMsg) that
	// switched kind, consumed by recomputeVisible. An empty namespace
	// matches the first row with that name.
	pendingSelect rowRef
	// pendingFollow names the object a tui.FollowObjectMsg asked to reopen
	// once the new namespace's rows land (follow.go); followNote says it
	// wasn't there, and followedAway marks this instance as having handed
	// its stack slot to the reopened screen (tui.Transient).
	pendingFollow string
	followNote    string
	followedAway  bool

	// sortColumn is the 1-based index into m.desc.Columns the user picked
	// with a 1-9 key (sort.go's handleSortKey) — 0 means no manual override,
	// so applySort falls back to sortForDisplay's built-in default. Reset by
	// resetAndLoad alongside the rest of the per-view state, since a column
	// index only means something against the kind/namespace it was chosen
	// on. sortAsc is only meaningful while sortColumn is non-zero.
	sortColumn int
	sortAsc    bool

	filterActive bool
	filterInput  textfield.Model
	// appliedFilter is the query recomputeVisible last ran against rows
	// that had loaded, so it can tell a changed query from a reload.
	appliedFilter string
	// filterListFocused is true once a live filter has been committed
	// (enter, unconditionally — see updateFilterKey) without clearing it:
	// the query/chrome stay exactly as filterActive left them, but keys
	// stop being captured as typing and route through updateKey instead, so
	// j/k, ctrl-d, a second enter to open the row's destination, etc. all
	// act on the narrowed rows directly. '/' resets it to false to resume
	// editing the same query.
	filterListFocused bool

	// originKind/originName name the row whose filtered child view (a
	// Deployment/StatefulSet/DaemonSet's pods, openDeploymentPods/
	// openStatefulSetPods/openDaemonSetPods; a Helm release's objects,
	// openReleaseObjects) is currently showing — when originName is
	// non-empty, esc jumps back to that row on originKind instead of popping
	// the task stack, and Header()'s breadcrumb names it. Cleared by every
	// navigation path that isn't "back to the origin row" (see clearOrigin)
	// so esc can't misfire once the user has moved on to something else.
	originKind kube.ResourceKind
	originName string
	// originNamespace is set (originWidened) when opening the child view
	// widened the list to all namespaces — openReleaseObjects does, for a
	// release whose manifest names another namespace — so esc can put the
	// user's own namespace back rather than leaving the session in
	// all-namespaces mode (and the Helm list on the cluster-wide release
	// informer, docs/lazy-informers.md §5.5). originRowNamespace pins the
	// origin row's own namespace, so the reselect can't land on a same-named
	// row in another namespace.
	originNamespace    string
	originWidened      bool
	originRowNamespace string
	// releasePodSources narrows the current Pods view to controller-owned Pods
	// declared by the Helm release Enter was pressed on. It is deliberately
	// separate from the user's fuzzy filter.
	releasePodSources []kube.HelmReleasePodSource
	releasePodView    bool

	// reloadEpoch/metricsEpoch guard debounced/ticked reloads against a
	// kind switch mid-flight (Phase 2): a scheduled tick only acts if the
	// epoch it captured is still current.
	reloadEpoch  int
	metricsEpoch int
	// clockEpoch guards §36a/§37a's one-second cronJobTickMsg chain. It is
	// bumped only by resetAndLoad — never by a reload — so exactly one chain
	// stays alive while a CronJob/Job list is shown: a watch event, the
	// KindsSynced gate, the empty-rows retry or Reload() on pop must neither
	// kill the running chain nor start a second one.
	clockEpoch int

	state    tui.TaskState
	feedback string
	spinner  spinner.Model
	// loadStartedAt is when the current TaskStateLoading spell began — 15a's
	// header timer ("▲ loading pods · 0.4s") measures elapsed against this
	// rather than reading the clock in Render (render must stay pure:
	// f(model, theme, size)); SpinnerTickMsg's 100ms cadence keeps m.now
	// fresh while it's ticking.
	loadStartedAt time.Time

	hints emptyHints
}

// rowsLoadedMsg carries a kind's freshly-listed rows. kind guards against a
// reply for a since-superseded kind (relevant once the jump palette can
// switch kinds mid-flight, Phase 2); columns — how many descriptor columns
// the rows were projected against — guards the same way against a descriptor
// that changed shape mid-flight (a custom kind's printer columns landing, or
// a context switch resetting them). nodeCount rides along for the Pods
// health strip (0 when unknown or not the Pods kind); nodeCapacity/
// podCountByNode/clusterPodTotal ride along for the Nodes columns/health
// strip (Node kind only, see nodes.go's loadNodeExtras).
// epoch is m.reloadEpoch at the moment load() was dispatched. Kind and
// columns alone don't catch a namespace switch that lands back on the same
// kind (or a context switch that restores it) — the request that's now
// stale still names the right kind and the right column count, just the
// wrong namespace/cluster. resetAndLoad bumps reloadEpoch on every path
// that changes what should be showing (namespace, kind, context switch, a
// CRD's columns landing), so this one check subsumes all of them.
type rowsLoadedMsg struct {
	epoch     int
	namespace string
	kind      kube.ResourceKind
	columns   int
	// cacheUnreadyAtRead records that this load read the cache before its
	// initial population completed. HasSynced can flip before this message is
	// applied, but that does not make the already-captured empty rows current;
	// one post-sync read is still required before rendering an empty state.
	cacheUnreadyAtRead bool
	rows               []resources.Row
	pods               map[string]kube.Pod
	helmReleases       map[string]kube.HelmRelease
	nodeCount          int
	nodeCapacity       map[string]nodeCapacity
	podCountByNode     map[string]int
	clusterPodTotal    int
	nodePodHealth      map[string]resources.HealthCounts
	drainImpactByNode  map[string]kube.DrainImpact
	chartCacheNote     chartCacheNote
	// cronJobSummaries/cronJobJobsErr ride along for KindCronJob only —
	// loadCronJobRows' own aggregation output, applyRowsLoaded stores them
	// as m.cronJobSummaries/m.cronJobJobsErr.
	cronJobSummaries []resources.CronJobSummary
	cronJobJobsErr   error
	// jobListSummaries rides along for KindJob only — loadJobRows' own
	// aggregation output, applyRowsLoaded stores it as m.jobListSummaries.
	jobListSummaries []resources.JobListSummary
	err              error
}

// emptyHintsMsg carries the 10c ways-out data computed once rows come back
// empty. epoch/namespace guard the same way rowsLoadedMsg's epoch does: kind
// alone doesn't catch a namespace switch that lands back on the same kind
// (switching between two empty namespaces of the same kind, say), so a slow
// reply from the namespace just left can still land after the switch's own
// faster reply already repopulated m.hints for the new namespace — epoch is
// bumped by every switch that matters (resetAndLoad), namespace is the
// belt-and-suspenders check podMetricsLoadedMsg's own guard already uses.
type emptyHintsMsg struct {
	epoch     int
	namespace string
	kind      kube.ResourceKind
	hints     emptyHints
}

// reloadDueMsg fires reloadDebounce after a ResourceChangedMsg; epoch guards
// against acting on a stale (superseded) debounce timer.
type reloadDueMsg struct{ epoch int }

// metricsTickMsg drives the 2s metrics poll loop; epoch guards it the same
// way reloadDueMsg guards reload debouncing.
type metricsTickMsg struct{ epoch int }

// cronJobTickMsg drives §36a's one-second UI clock (Phase 4 task 5, §4.5).
// epoch is m.clockEpoch at scheduling time. It deliberately isn't
// reloadEpoch: every watch event for the kind or an aux kind (Pod, for both
// CronJob and Job) bumps reloadEpoch, and nothing on that path re-arms the
// clock, so sharing it froze NEXT/DURATION and m.now within seconds on any
// live cluster. Only resetAndLoad (kind/namespace/context switch) bumps
// clockEpoch, and it starts the next chain itself when the new kind still
// ticks.
type cronJobTickMsg struct{ epoch int }

// scheduleCronJobTick arranges the next §36a clock tick one second out.
func (m Model) scheduleCronJobTick(epoch int) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return cronJobTickMsg{epoch: epoch} })
}

// applyCronJobTick rebuilds m.rows from the stored cronJobSummaries at the
// current instant (Phase 4 task 5) — no lister call, ever. ProjectCronJob
// sets each row's own SubLine directly (docs/design README.md §36a: every
// failed row shows its reason, not just the selected one), so no selection
// bookkeeping is needed here. applySort re-applies a manual 1-9 sort
// (sort.go) since the rebuild above always regenerates rows in the
// cronJobSummaries' own fixed name order; recomputeVisible then re-derives
// m.visible/m.display from the (possibly resorted) rows and preserves
// filtering/selected identity (task 15). A stale summaries/rows length
// mismatch (only possible mid-reload, between a kind switch and its first
// rowsLoadedMsg) is a no-op; the reload in flight settles it.
func (m *Model) applyCronJobTick(now time.Time) {
	if len(m.cronJobSummaries) != len(m.rows) {
		return
	}
	for i, s := range m.cronJobSummaries {
		row := resources.ProjectCronJob(s, now)
		if m.cronJobJobsErr != nil {
			markCronJobHistoryUnavailable(&row)
		}
		m.rows[i] = row
	}
	m.applySort()
	m.recomputeVisible()
}

// applyJobTick is applyCronJobTick's own sibling for §37a: DURATION runs
// live for an active Job (mockup: "DURATION runs live for active jobs"),
// which needs the same per-second rebuild-from-stored-summary — no lister
// call, ever. currentUser threads through the same way loadJobRows'
// ProjectJobList call does.
func (m *Model) applyJobTick(now time.Time, currentUser string) {
	if len(m.jobListSummaries) != len(m.rows) {
		return
	}
	for i, s := range m.jobListSummaries {
		m.rows[i] = resources.ProjectJobList(s, now, currentUser)
	}
	m.applySort()
	m.recomputeVisible()
}

// podMetricsLoadedMsg carries one poll's result. epoch/namespace guard
// against a reply for a since-superseded namespace/kind.
type podMetricsLoadedMsg struct {
	epoch     int
	namespace string
	metrics   map[string]kube.PodMetrics
	err       error
}

func New(cfg Config) Model {
	if cfg.LoadTimeout == 0 {
		cfg.LoadTimeout = 10 * time.Second
	}

	kind := kube.KindPod
	namespace := "default"
	var reg resources.Registry
	if cfg.Session != nil {
		reg = cfg.Session.Registry
		namespace = cfg.Session.Location.Namespace
		if cfg.Session.Location.Kind != "" {
			kind = cfg.Session.Location.Kind
		}
		// A kind an earlier instance had to fall back from (below) is still
		// what the user was on — try it again now the registry may know it.
		if pending := cfg.Session.PendingKind(); pending != "" {
			kind = pending
		}
	}
	// A persisted Location.Kind can name a CRD kind that isn't in reg yet
	// (real-cluster startup builds browse before discovery has run, so
	// Session.Registry is still resources.DefaultRegistry's built-ins only)
	// — falling back to Pod here, like switchKind already does for the same
	// "unknown kind" case, avoids handing load() a zero-value Descriptor
	// (empty Kind) that ListRaw has no informer for.
	//
	// KindEvent gets the same fallback even though it does resolve a real
	// Descriptor: 9b (tasks/events) is the curated experience for this kind
	// (docs/design README.md §9b) — every live path to it (the 'e' key, the
	// goto palette's Events entry) redirects through openSelectedEvents
	// rather than ever calling switchKind(KindEvent), so browse's own stock
	// Events table is otherwise unreachable. But Location.Kind can still
	// end up "Event" (the root shell's GotoKindMsg handler sets it before
	// forwarding to that redirect, and 9b itself never corrects it back
	// while it's the active task) — if the user quits from 9b, PerContext
	// persists that verbatim, and next launch's buildBrowseTask would
	// otherwise render the never-meant-to-be-seen stock list instead of
	// bouncing back to 9b. Resetting kind (and Location.Kind, so quitting
	// again doesn't just re-persist "Event") to Pod here is simpler than
	// teaching NewModel/buildBrowseTask to redirect to tasks/events before
	// the root model even exists.
	//
	// The unresolved CRD kind is held on the Session rather than dropped:
	// the root shell jumps to it once discovery lands, and quitting first
	// persists it instead of Pod (Session.HoldPendingKind).
	desc, ok := reg.Descriptor(kind)
	if !ok || kind == kube.KindEvent {
		if !ok && cfg.Session != nil {
			cfg.Session.HoldPendingKind(kind)
		}
		kind = kube.KindPod
		desc, _ = reg.Descriptor(kind)
	} else if cfg.Session != nil {
		cfg.Session.ClearPendingKind()
	}
	// Mirror the resolved kind back unconditionally, not just in the
	// fallback branch above: Session.Location is documented as the
	// authoritative answer to "where is the user", and switchKind already
	// keeps it that way for every later change. A first launch that never
	// navigates used to leave it empty while this screen showed Pods, which
	// is a Location that disagrees with the screen — harmless until
	// something read it and reported "(none)", which is what the crash
	// report did.
	if cfg.Session != nil {
		cfg.Session.Location.Kind = kind
	}

	theme := tui.Dark()
	if cfg.Session != nil {
		theme = cfg.Session.Theme
	}
	filterInput := textfield.New()
	filterInput.SetStyles(tui.TextInputStyles(theme))
	filterInput.Prompt = ""

	state := tui.TaskStateLoading
	feedback := "Loading " + desc.Display + "..."
	if cfg.Lister == nil {
		state = tui.TaskStateError
		feedback = "no cluster connection"
		if cfg.InitErr != nil {
			feedback = cfg.InitErr.Error()
		}
	}

	return Model{
		width:               tui.DefaultWidth,
		height:              tui.DefaultHeight,
		session:             cfg.Session,
		lister:              cfg.Lister,
		metrics:             cfg.Metrics,
		nodeMetricsSrc:      cfg.NodeMetrics,
		mutator:             cfg.Mutator,
		shells:              cfg.Shells,
		actions:             actions.New(cfg.Mutator),
		openLogs:            cfg.OpenLogs,
		openNodeDetail:      cfg.OpenNodeDetail,
		openPodDetail:       cfg.OpenPodDetail,
		openYAML:            cfg.OpenYAML,
		openEvents:          cfg.OpenEvents,
		openTimeline:        cfg.OpenTimeline,
		openObjectTimeline:  cfg.OpenObjectTimeline,
		openExec:            cfg.OpenExec,
		openDebug:           cfg.OpenDebug,
		openNodeDebug:       cfg.OpenNodeDebug,
		openForward:         cfg.OpenForward,
		openObjectDetail:    cfg.OpenObjectDetail,
		openRouteTable:      cfg.OpenRouteTable,
		openWhoCan:          cfg.OpenWhoCan,
		openFluxDetail:      cfg.OpenFluxDetail,
		openCertChain:       cfg.OpenCertChain,
		openCronJobDetail:   cfg.OpenCronJobDetail,
		openCronJobSchedule: cfg.OpenCronJobSchedule,
		openJobAttempts:     cfg.OpenJobAttempts,
		openHelmHistory:     cfg.OpenHelmHistory,
		openHelmDetail:      cfg.OpenHelmDetail,
		openHelmValues:      cfg.OpenHelmValues,
		openSecretData:      cfg.OpenSecretData,
		openConfigMapData:   cfg.OpenConfigMapData,
		openOverview:        cfg.OpenOverview,
		openFluxTree:        cfg.OpenFluxTree,
		forwards:            cfg.Forwards,
		retrier:             cfg.Retrier,
		timeout:             cfg.LoadTimeout,
		currentUser:         cfg.CurrentUser,
		identity:            cfg.Identity,
		demo:                cfg.Demo,
		kind:                kind,
		namespace:           namespace,
		desc:                desc,
		state:               state,
		feedback:            feedback,
		now:                 time.Now(),
		loadStartedAt:       time.Now(),
		filterInput:         filterInput,
		spinner:             components.NewSpinner(),
	}
}

// grouped reports whether the table renders 6b's namespace-grouped triage
// view: all-namespaces mode (namespace == "") on a namespaced kind.
// Cluster-scoped kinds (Nodes, Namespaces) always pass namespace="" to
// ListRaw regardless of m.namespace (see countNamespace), so they're
// excluded — there's no namespace to group by.
func (m Model) grouped() bool {
	return m.namespace == "" && !m.desc.ClusterScoped
}

// listerSynced reports whether the cache backing the kind currently on
// screen is done with its initial fill. Prefers the per-kind answer and
// falls back to the cluster-wide one, then to "synced" for any lister that
// opts into neither (fakes, test doubles), so this only changes behavior for
// *kube.Cluster.
func (m Model) listerSynced() bool {
	return tui.KindsSynced(m.lister, m.countNamespace(), m.kind)
}

// namespaceScopedHint names --namespace-scoped=<ns> as a recovery option on
// the 403 card, when it's plausibly one: a namespace is known (m.namespace,
// the one the user would scope to) and this session isn't already running
// scoped (docs/lazy-informers.md §5.6 — a session already
// scoped has nothing left to suggest, and a session with no namespace
// selected yet has nothing to name). Empty otherwise, so the card renders
// exactly as before it existed.
func (m Model) namespaceScopedHint() string {
	if m.namespace == "" {
		return ""
	}
	if sc, ok := m.lister.(tui.ScopedChecker); ok && sc.Scoped() {
		return ""
	}
	return "kute --namespace-scoped=" + m.namespace + " may see more, if that namespace grants it"
}

// pollsMetrics reports whether kind's CPU/MEM columns currently need the 2s
// metrics poll — Pods (pod usage) and Nodes (11a's live cluster/node bars)
// today. Offline screens keep their last values frozen: a metrics request is
// authenticated cluster traffic and must not keep invoking an expired exec
// credential plugin after the health loop has stopped.
func (m Model) pollsMetrics() bool {
	if m.offline() {
		return false
	}
	switch m.kind {
	case kube.KindPod:
		return m.metrics != nil
	case kube.KindNode:
		return m.nodeMetricsSrc != nil
	default:
		return false
	}
}

// ticksCronJobClock reports whether the current kind needs the one-second
// UI clock cronJobTickMsg drives (Phase 4 task 5, §4.5): NEXT/LAST RUN for
// CronJobs, DURATION for Jobs (§37a) — both recomputed from a stored
// summary on every tick, never a lister call, metrics poll, or discovery
// read. Name kept from its CronJob-only origin; the tick itself now serves
// both kinds via applyJobTick.epoch dispatch (update.go), and Forwards
// (§13c): UPTIME, "idle 12m" and a Reconnecting row's "next in Ns" are
// derived at projection time, and the manager only notifies on a state
// change, so without the tick the countdown would sit frozen until the
// retry fired. A Forwards tick reloads from the in-memory ForwardManager —
// still no cluster read.
func (m Model) ticksCronJobClock() bool {
	return m.kind == kube.KindCronJob || m.kind == kube.KindJob || m.kind == kube.KindForward
}

// offline reports whether the 4a treatment applies: the last known
// connection state is mid-outage (watch/ping failing, with backoff
// retries under way) rather than a one-shot failure. The table/rows
// themselves are untouched — browsing the stale snapshot still works.
func (m Model) offline() bool {
	return m.conn.Offline()
}

func (m Model) Init() tea.Cmd {
	if m.lister == nil {
		return nil
	}
	cmds := []tea.Cmd{m.load(), m.spinner.Tick}
	if m.pollsMetrics() {
		cmds = append(cmds, m.loadMetricsCmd(m.metricsEpoch), m.scheduleMetricsTick(m.metricsEpoch))
	}
	if m.ticksCronJobClock() {
		cmds = append(cmds, m.scheduleCronJobTick(m.clockEpoch))
	}
	if prefetch := m.prefetchAuxKinds(); prefetch != nil {
		cmds = append(cmds, prefetch)
	}
	return tea.Batch(cmds...)
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width = size.Width
	m.height = size.Height
}

// countNamespace is the namespace argument for List/Count calls against the
// current kind: cluster-scoped kinds (Nodes, Namespaces) ignore the active
// namespace entirely.
func (m Model) countNamespace() string {
	if m.desc.ClusterScoped {
		return ""
	}
	return m.namespace
}

func (m Model) load() tea.Cmd {
	lister := m.lister
	desc := m.desc
	ns := m.countNamespace()
	kind := m.kind
	timeout := m.timeout
	epoch := m.reloadEpoch
	podDesc, _ := m.session.Registry.Descriptor(kube.KindPod)
	currentUser := m.actingUser()
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		cacheUnreadyAtRead := !tui.KindsSynced(lister, ns, kind)
		if kind == kube.KindCronJob {
			// §36a bypasses resources.List/Descriptor.Project entirely
			// (registry.go's own doc comment on the CronJob entry) — Job-aware
			// rows need the cache-local CronJob+Job+Pod join Phase 1 built
			// (resources.BuildCronJobSummaries), never a per-row ListRaw(Job).
			msg := loadCronJobRows(ctx, lister, ns, len(desc.Columns))
			msg.epoch, msg.namespace = epoch, ns
			return msg
		}
		if kind == kube.KindJob {
			// §37a bypasses resources.List/Descriptor.Project entirely
			// (registry.go's own doc comment on the Job entry), same reasoning
			// as CronJob above — SOURCE/FAILED/COMPL need the real aggregation,
			// never the single-object fallback.
			msg := loadJobRows(ctx, lister, ns, len(desc.Columns), currentUser)
			msg.epoch, msg.namespace = epoch, ns
			return msg
		}
		var rows []resources.Row
		var podObjs, helmObjs []runtime.Object
		var err error
		if kind == kube.KindHelmRelease {
			// One read serves both the rows and the helmReleases map: every
			// ListRaw(HelmRelease) decodes release Secrets and re-reads the
			// three workload caches for the rollout glyph, so a second read
			// for the map doubled the most expensive load in the app.
			helmObjs, err = lister.ListRaw(ctx, kube.KindHelmRelease, ns)
			if err == nil {
				rows = resources.Project(desc, helmObjs)
			}
		} else if kind == kube.KindPod && m.releasePodView {
			podObjs, err = lister.ListRaw(ctx, kube.KindPod, ns)
			if err == nil {
				replicaSets, jobs, err := releasePodSupportObjects(ctx, lister, ns, m.releasePodSources)
				if err == nil {
					keys := kube.HelmReleasePodKeys(m.releasePodSources, podObjs, replicaSets, jobs)
					matched := make([]runtime.Object, 0, len(podObjs))
					for _, obj := range podObjs {
						if p, ok := obj.(*corev1.Pod); ok {
							if _, ok := keys[kube.PodKey(p.Namespace, p.Name)]; ok {
								matched = append(matched, obj)
							}
						}
					}
					podObjs = matched
					rows = resources.Project(desc, podObjs)
				}
			}
		} else {
			rows, err = resources.List(ctx, lister, desc, ns)
		}
		if err != nil {
			return rowsLoadedMsg{epoch: epoch, namespace: ns, kind: kind, columns: len(desc.Columns), err: err}
		}
		var pods map[string]kube.Pod
		var helmReleases map[string]kube.HelmRelease
		var cacheNote chartCacheNote
		nodeCount := 0
		var nodeCap map[string]nodeCapacity
		var podCountByNode map[string]int
		var nodePodHealth map[string]resources.HealthCounts
		var drainImpactByNode map[string]kube.DrainImpact
		clusterPodTotal := 0
		switch kind {
		case kube.KindPod:
			if podObjs != nil {
				pods = podsFromObjs(podObjs)
			} else {
				pods = podsByName(ctx, lister, ns)
			}
			// Best-effort node count for the health strip's "· 3 nodes"
			// suffix — 0 on error just omits it.
			if n, err := resources.Count(ctx, lister, kube.KindNode, ""); err == nil {
				nodeCount = n
			}
		case kube.KindNode:
			nodeCap, podCountByNode, clusterPodTotal, nodePodHealth, drainImpactByNode = loadNodeExtras(ctx, lister, podDesc.Project)
		case kube.KindHelmRelease:
			helmReleases = helmReleasesFromObjs(helmObjs)
			cacheNote = chartCacheNoteFor(lister, time.Now())
		}
		return rowsLoadedMsg{
			epoch: epoch, namespace: ns,
			kind: kind, columns: len(desc.Columns), cacheUnreadyAtRead: cacheUnreadyAtRead,
			rows: rows, pods: pods, helmReleases: helmReleases, nodeCount: nodeCount,
			nodeCapacity: nodeCap, podCountByNode: podCountByNode, clusterPodTotal: clusterPodTotal,
			nodePodHealth: nodePodHealth, drainImpactByNode: drainImpactByNode, chartCacheNote: cacheNote,
		}
	}
}

// switchKind mutates kind in place (mvp-plan.md Phase 2: "no stack push
// between kinds — jumping kinds mutates the browse model"), keeping the
// current namespace, and re-issues load()/metrics setup. A no-op (nil cmd)
// for an unknown kind or the kind already showing.
// switchKind mutates kind in place. Like switchNamespace below, it also
// mirrors Session.Location.Kind — reached both via the root-forwarded
// tui.GotoKindMsg (idempotent: the root already set Location.Kind before
// forwarding) and via direct internal calls that bypass the root entirely
// (openDeploymentPods/openReleaseObjects's "↵ = this row's filtered pods",
// and their own esc-back) — the same staleness switchNamespace's doc
// comment describes, just for the Kind dimension instead of Namespace.
func (m *Model) switchKind(kind kube.ResourceKind) tea.Cmd {
	if m.session == nil {
		return nil
	}
	// m.rows != nil: same rule as goToResource — a never-loaded instance
	// (routeGoto's fresh browse, seeded from Session.Location) must still
	// load when the jump names the kind it already holds.
	if kind == m.kind && m.rows != nil {
		return nil
	}
	desc, ok := m.session.Registry.Descriptor(kind)
	if !ok {
		return nil
	}
	m.kind = kind
	m.desc = desc
	m.session.Location.Kind = kind
	m.session.ClearPendingKind()
	return m.resetAndLoad()
}

// switchNamespace mutates namespace in place, keeping kind and filter, and
// re-issues load()/metrics setup. A no-op for the namespace already active,
// unless this instance has never loaded (see switchKind).
// resetAndLoad normally clears the filter along with the rest of the
// per-view state (right for a kind switch, whose query no longer means
// anything against a different kind's names), so a namespace switch snapshots
// filterQuery/filterActive first and restores them after — docs/design
// README.md §6a: "switching keeps kind + filter".
//
// Mirrors Session.Location.Namespace the same way setFilter mirrors
// Location.Filter: this is the only place browse mutates m.namespace, but
// it's reached two ways — forwarded from the root shell's own
// tui.SwitchNamespaceMsg handling (which already set Location.Namespace
// before forwarding, so this is idempotent there) and two hotkeys that
// bypass the root entirely ('a' all-namespaces, 'N' jump-into-namespace,
// update.go) which previously left Location.Namespace stale. That staleness
// was invisible in browse's own view (which reads m.namespace directly) but
// broke the goto palette's live per-kind counts (tui/goto.go's
// gotoNamespace reads Location.Namespace) after either hotkey — a `g`
// right after 'a' kept scoping counts to the namespace you'd left, even
// though the header already said "∗ all namespaces".
func (m *Model) switchNamespace(namespace string) tea.Cmd {
	if namespace == m.namespace && m.rows != nil {
		return nil
	}
	m.namespace = namespace
	if m.session != nil {
		m.session.Location.Namespace = namespace
	}
	query, active, focused := m.filterInput.Value(), m.filterActive, m.filterListFocused
	cmd := m.resetAndLoad()
	m.filterActive = active
	m.filterListFocused = focused
	m.setFilter(query)
	return cmd
}

// setFilter updates the live filter query, keeping Session.Location.Filter
// (the cross-screen mirror context.go's switchContextCmd reads to persist
// into state.PerContext and writes back on restore — mvp-tasks.md's
// "PerContext.Filter exists but nothing writes it" gap) in sync. Every
// filterQuery mutation goes through this instead of assigning the field
// directly.
func (m *Model) setFilter(query string) {
	m.filterInput.SetValue(query)
	m.filterInput.CursorEnd()
	if m.session != nil {
		m.session.Location.Filter = query
	}
}

// goToResource handles a jump-to-resource navigation: switch kind and/or
// namespace first if the target lives in either a different one (selecting
// it once rows land, via pendingSelect), otherwise select it immediately
// since rows are already loaded.
//
// The namespace switch matters for cluster-wide jumps — tasks/overview's
// TROUBLE/RECENT CHANGES panels aggregate pods/rollouts across every
// namespace (19a), so ↵ there can land on an object outside whatever
// namespace browse currently has active. Without switching, the target
// object simply never appears in the freshly namespace-scoped load and
// pendingSelect silently finds nothing to select.
func (m *Model) goToResource(msg tui.GotoResourceMsg) tea.Cmd {
	m.clearOrigin()
	m.pendingSelect = rowRef{namespace: msg.Namespace, name: msg.Name}

	if m.session == nil {
		m.recomputeVisible()
		return nil
	}

	desc := m.desc
	kindChanged := msg.Kind != m.kind
	if kindChanged {
		d, ok := m.session.Registry.Descriptor(msg.Kind)
		if !ok {
			return nil
		}
		desc = d
	}
	if desc.ClusterScoped {
		m.pendingSelect.namespace = ""
	}
	namespaceChanged := !desc.ClusterScoped && msg.Namespace != "" && msg.Namespace != m.namespace

	// m.rows == nil is the tell that this instance has never actually
	// loaded anything — routeGoto (tui/model.go) builds a brand-new browse
	// instance and routes msg straight into this method instead of calling
	// Init(), trusting this method to be the one thing that starts the
	// load. New() seeds a fresh instance's kind/namespace from
	// Session.Location, which a jump fired from a screen pushed on top of
	// browse (without ever changing kind away from what it already was —
	// §35a's certchain root row is the first jump to actually hit this: it
	// names the very Certificate the Session never stopped being "on")
	// makes identical to msg's own target. Skipping the load in that case
	// left the fresh instance on its constructor's permanent "Loading
	// Certificates..." skeleton forever, since nothing else was ever going
	// to ask it to fetch anything.
	if !kindChanged && !namespaceChanged && m.rows != nil {
		m.recomputeVisible()
		return nil
	}
	if kindChanged {
		m.kind = msg.Kind
		m.desc = desc
		m.session.Location.Kind = msg.Kind
	}
	if namespaceChanged {
		m.namespace = msg.Namespace
		m.session.Location.Namespace = msg.Namespace
	}
	return m.resetAndLoad()
}

// clearOrigin drops the "esc back to origin row" state (originKind/
// originName) — called from every navigation path that isn't the esc-back
// one itself, so a stale target can't resurface after the user has moved on
// to something else.
func (m *Model) clearOrigin() {
	m.originKind = ""
	m.originName = ""
	m.originNamespace, m.originWidened, m.originRowNamespace = "", false, ""
}

// browseCacheKey identifies one kind+namespace view for rowCache — the same
// two dimensions switchKind/switchNamespace mutate — within one kube
// context, so a 7a switch never seeds the new cluster's loading view with
// the previous cluster's rows.
type browseCacheKey struct {
	context   string
	kind      kube.ResourceKind
	namespace string
}

// cacheKey is rowCache's key for kind/namespace under the active context.
func (m Model) cacheKey(kind kube.ResourceKind, namespace string) browseCacheKey {
	ctx := ""
	if m.session != nil {
		ctx = m.session.Location.Context
	}
	return browseCacheKey{context: ctx, kind: kind, namespace: namespace}
}

// cachedRows is one rowCache entry: the snapshot plus the number of
// descriptor columns its Cells were projected against, so a snapshot taken
// under a different shape of the same kind — a custom kind loaded with its
// printer columns, then revisited after a context switch reset discovery back
// to the interim NAME/AGE pair — is recognisably stale rather than a set of
// rows whose cells land under the wrong headers.
type cachedRows struct {
	rows      []resources.Row
	columns   int
	fetchedAt time.Time // when rows were listed — restored with them
}

// cachedRowsFor returns rowCache's snapshot for kind/namespace, if any —
// used by resetAndLoad to seed the 15a "cached rows dimmed" loading view.
// A snapshot projected against a different column count than the descriptor
// now in force is no hit at all: showing it would mean rendering cells under
// headers they don't belong to.
func (m Model) cachedRowsFor(kind kube.ResourceKind, namespace string) ([]resources.Row, bool) {
	entry, ok := m.cachedEntryFor(kind, namespace)
	return entry.rows, ok
}

func (m Model) cachedEntryFor(kind kube.ResourceKind, namespace string) (cachedRows, bool) {
	entry, ok := m.rowCache[m.cacheKey(kind, namespace)]
	if !ok || entry.columns != len(m.desc.Columns) {
		return cachedRows{}, false
	}
	return entry, true
}

// cacheCurrentRows snapshots m.rows into rowCache under the current
// kind/namespace, called once a load succeeds (applyRowsLoaded) — a copy,
// since m.rows is mutated in place by later sorts/marks.
func (m *Model) cacheCurrentRows() {
	if m.releasePodView {
		return
	}
	if m.rowCache == nil {
		m.rowCache = make(map[browseCacheKey]cachedRows)
	}
	cp := slices.Clone(m.rows)
	m.rowCache[m.cacheKey(m.kind, m.namespace)] = cachedRows{rows: cp, columns: m.rowColumns, fetchedAt: m.fetchedAt}
}

// dropStaleShapedRows clears any loaded rows that were projected against a
// different descriptor shape than m.desc now has, putting the screen back
// into its loading state. Called wherever the descriptor can change under an
// already-loaded list; the reload that follows refills it in shape.
func (m *Model) dropStaleShapedRows() {
	if m.rows == nil || m.rowColumns == len(m.desc.Columns) {
		return
	}
	m.rows = nil
	m.visible = nil
	m.expandedGroups = nil
	m.display = nil
	m.selected, m.offset = 0, 0
	m.cachedView = false
	m.state = tui.TaskStateLoading
	m.feedback = "Loading " + m.desc.Display + "..."
	m.loadStartedAt = time.Now()
}

// resetAndLoad puts the model back into the loading state for whatever
// kind/namespace switchKind/switchNamespace just set, bumping both epochs
// so any in-flight reload/metrics reply for the previous kind/namespace is
// ignored, and re-issues load() (+ metrics polling for Pods).
func (m *Model) resetAndLoad() tea.Cmd {
	m.clearOrigin()
	m.state = tui.TaskStateLoading
	m.feedback = "Loading " + m.desc.Display + "..."
	m.loadStartedAt = time.Now()
	m.rows = nil
	m.pods = nil
	m.helmReleases = nil
	m.releasePodSources = nil
	m.releasePodView = false
	m.chartCacheNote = chartCacheNote{}
	m.visible = nil
	m.expandedGroups = nil
	m.display = nil
	m.selected, m.offset = 0, 0
	m.filterActive = false
	m.filterListFocused = false
	m.setFilter("")
	m.filterInput.Blur()
	m.nodeCount = 0
	m.podMetrics = nil
	m.nodeMetrics = nil
	m.nodeCapacity = nil
	m.podCountByNode = nil
	m.clusterPodTotal = 0
	m.nodePodHealth = nil
	m.drainImpactByNode = nil
	m.cronJobSummaries = nil
	m.cronJobJobsErr = nil
	m.auxKindsDeniedNote = ""
	m.pendingFollow = ""
	m.followNote = ""
	m.pendingCronJobRun = nil
	m.pendingCronJobResume = nil
	m.jobListSummaries = nil
	m.pendingJobRerun = nil
	// 20a: "marks are per-view and drop on kind/namespace switch."
	m.marks = nil
	// A marked-set delete's confirm names the marks just dropped, so it
	// goes with them.
	if m.pendingBulkDelete() {
		m.actions.Cancel()
	}
	// A manual column choice only means something against the kind/
	// namespace it was picked on, so it resets here too — same lifecycle as
	// the filter box above.
	m.sortColumn = 0
	m.sortAsc = false
	m.reloadEpoch++
	m.metricsEpoch++
	m.clockEpoch++

	// 15a: "revisiting a kind seen this session: cached rows dimmed instead
	// of skeletons" — seed the loading view from rowCache if this exact
	// kind+namespace was ever successfully loaded before; the fresh load
	// kicked off below still runs and replaces it once it lands
	// (applyRowsLoaded clears cachedView).
	m.cachedView = false
	m.rowColumns = 0
	// fetchedAt belongs to the rows on screen: a fresh kind/namespace has
	// never been listed (4a/4b's "last successful list: never"), a cached
	// one was listed when its snapshot was.
	m.fetchedAt = time.Time{}
	if cached, ok := m.cachedEntryFor(m.kind, m.namespace); ok && len(cached.rows) > 0 {
		m.rows = cached.rows
		m.fetchedAt = cached.fetchedAt
		m.rowColumns = len(m.desc.Columns) // cachedRowsFor only hits on a shape match
		m.cachedView = true
		m.recomputeVisible()
	}

	if m.lister == nil {
		m.state = tui.TaskStateError
		m.feedback = "no cluster connection"
		return nil
	}
	cmds := []tea.Cmd{m.load(), m.spinner.Tick}
	if m.pollsMetrics() {
		cmds = append(cmds, m.loadMetricsCmd(m.metricsEpoch), m.scheduleMetricsTick(m.metricsEpoch))
	}
	if m.ticksCronJobClock() {
		cmds = append(cmds, m.scheduleCronJobTick(m.clockEpoch))
	}
	if prefetch := m.prefetchAuxKinds(); prefetch != nil {
		cmds = append(cmds, prefetch)
	}
	return tea.Batch(cmds...)
}

// switchContext applies a completed 7a cluster rebuild (tui.SwitchContextMsg
// with Err == nil, the only case update.go forwards it for): unlike
// switchKind/switchNamespace, kind/namespace are set unconditionally —
// even an unchanged kind/namespace string needs a fresh load, since the
// underlying cluster identity changed under it. msg.Filter is the target
// context's own remembered filter (state.PerContext, restored by
// switchContextCmd) — docs/design README.md §7a: "each context remembers
// its own namespace + kind + filter; switching restores them" — not the
// outgoing context's filter, so it replaces rather than preserves.
func (m *Model) switchContext(msg tui.SwitchContextMsg) tea.Cmd {
	if m.session == nil {
		return nil
	}
	desc, ok := m.session.Registry.Descriptor(msg.Kind)
	if !ok {
		return nil
	}
	m.kind = msg.Kind
	m.desc = desc
	m.namespace = msg.Namespace
	cmd := m.resetAndLoad()
	m.filterActive = msg.Filter != ""
	// A restored filter is already committed navigation state, not a text
	// edit the user must confirm again. Keep focus on the list so the first
	// Enter after a context switch opens the selected destination object.
	m.filterListFocused = msg.Filter != ""
	m.setFilter(msg.Filter)
	return cmd
}

// podsByName re-fetches the raw Pod objects for the 'l' logs verb: Row only
// carries the display Cells, not the container names StreamPodLogs needs
// per container (kube.PodFromObject's fuller projection). Best-effort — a
// failure here still leaves the table itself populated from resources.List.
func podsByName(ctx context.Context, lister resources.RawLister, namespace string) map[string]kube.Pod {
	objs, err := lister.ListRaw(ctx, kube.KindPod, namespace)
	if err != nil {
		return nil
	}
	return podsFromObjs(objs)
}

// podsFromObjs indexes an already-fetched Pod slice by
// kube.PodKey(namespace, name) — never bare name, since all-namespaces mode
// lists same-named pods (StatefulSet ordinals, a chart installed twice) side
// by side and they would silently overwrite each other. The same
// projection podsByName builds from its own ListRaw, split out so
// loadCronJobRows can reuse the Pod read it already does for
// resources.BuildCronJobSummaries instead of listing Pods twice (mirrors
// cronjobdetail/load.go's own podsByName(podObjs []runtime.Object)).
func podsFromObjs(objs []runtime.Object) map[string]kube.Pod {
	out := make(map[string]kube.Pod, len(objs))
	for _, obj := range objs {
		if p, ok := obj.(*corev1.Pod); ok {
			out[kube.PodKey(p.Namespace, p.Name)] = kube.PodFromObject(p)
		}
	}
	return out
}

// releasePodSupportObjects reads only the controller caches needed to resolve
// this release's manifest sources. It deliberately does not sweep workload
// kinds: opening one release pays only for the owner links it displays.
func releasePodSupportObjects(ctx context.Context, lister resources.RawLister, namespace string, sources []kube.HelmReleasePodSource) (replicaSets, jobs []runtime.Object, err error) {
	needReplicaSets, needJobs := false, false
	for _, source := range sources {
		switch source.Kind {
		case kube.KindDeployment:
			needReplicaSets = true
		case kube.KindCronJob:
			needJobs = true
		}
	}
	if needReplicaSets {
		if replicaSets, err = lister.ListRaw(ctx, kube.KindReplicaSet, namespace); err != nil {
			return nil, nil, err
		}
	}
	if needJobs {
		if jobs, err = lister.ListRaw(ctx, kube.KindJob, namespace); err != nil {
			return nil, nil, err
		}
	}
	return replicaSets, jobs, nil
}

func helmReleaseKey(namespace, name string) string { return namespace + "\x00" + name }

// helmReleasesFromObjs keeps the full decoded kube.HelmRelease per row, from
// the same ListRaw result the rows were projected from: Row only carries the
// display Cells, not the chart/values/revision fields 'v'/'h'/'R' need
// (helm.go's openReleaseValues/openReleaseHistory/beginRollback).
func helmReleasesFromObjs(objs []runtime.Object) map[string]kube.HelmRelease {
	out := make(map[string]kube.HelmRelease, len(objs))
	for _, obj := range objs {
		if ho, ok := obj.(*kube.HelmReleaseObject); ok {
			out[helmReleaseKey(ho.Release.Namespace, ho.Release.Name)] = ho.Release
		}
	}
	return out
}

// loadCronJobRows is §36a's own load() branch (0.8.0 plan Phase 4 task 2):
// lists CronJobs, Jobs, and best-effort Pods once each, then aggregates
// client-side via resources.BuildCronJobSummaries + resources.ProjectCronJob
// — never a ListRaw(Job) call per CronJob row. A CronJob list failure fails
// the whole load, same as every other kind; a Job list failure does not —
// §4.4 point 5 requires the already-readable CronJob rows to stay visible,
// with their history cells marked unavailable (markCronJobHistoryUnavailable)
// rather than rendering the ordinary, and here false, "no retained runs". The
// same Pod read also backs cronJobLogsTarget's 'l' lookup (jobs.go) — it's
// indexed into rowsLoadedMsg.pods via podsFromObjs below, exactly like
// cronjobdetail's own load() does with its identical Pod read, so 'l' finds
// a real kube.Pod (with Containers) instead of falling back to a bare
// Namespace/Name stub podlogs can't stream anything from.
func loadCronJobRows(ctx context.Context, lister resources.RawLister, namespace string, columns int) rowsLoadedMsg {
	cronJobObjs, err := lister.ListRaw(ctx, kube.KindCronJob, namespace)
	if err != nil {
		return rowsLoadedMsg{kind: kube.KindCronJob, columns: columns, err: err}
	}
	jobObjs, jobsErr := lister.ListRaw(ctx, kube.KindJob, namespace)
	if jobsErr == nil {
		// ListRaw returns an errorless empty cache for a Forbidden or stalled
		// Job informer, so ask the cache's own health too — the same fallback
		// cronjobdetail's load() uses — or ACT/LAST RUN and run-now's overlap
		// check would read a denial as "no retained runs". A cache that's
		// merely still filling reports nil here; the KindsSynced gate in
		// applyRowsLoaded resolves that case before these rows are shown.
		jobsErr = tui.KindsError(lister, namespace, kube.KindJob)
	}
	// Pods are best-effort (§4.4: "may be read best-effort for exit/log
	// targeting") — a failed Pod read must never erase a valid Job-level
	// failure (buildJobSummary already tolerates a nil/partial pods slice),
	// so its own error is simply discarded here.
	podObjs, _ := lister.ListRaw(ctx, kube.KindPod, namespace)

	summaries := resources.BuildCronJobSummaries(cronJobObjs, jobObjs, podObjs)
	// Namespace/name order is the base default (§36a: sorted by name unless
	// the user picks a column) — the same stable sort resources.List itself
	// applies, since CronJob is deliberately absent from sort.go's
	// workloadKinds/unhealthyFirst sets (resources/cronjobs.go's own doc
	// comment). applyRowsLoaded's own applySort call (update.go) layers a
	// manual 1-9 sort on top of this when one is active.
	slices.SortStableFunc(summaries, func(x, y resources.CronJobSummary) int {
		a, b := x.Object, y.Object
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
		)
	})

	now := time.Now()
	rows := make([]resources.Row, len(summaries))
	for i, s := range summaries {
		row := resources.ProjectCronJob(s, now)
		if jobsErr != nil {
			markCronJobHistoryUnavailable(&row)
		}
		rows[i] = row
	}
	return rowsLoadedMsg{
		kind: kube.KindCronJob, columns: columns, rows: rows, pods: podsFromObjs(podObjs),
		cronJobSummaries: summaries, cronJobJobsErr: jobsErr,
	}
}

// loadJobRows is §37a's own load() branch: lists Jobs (required — a failure
// fails the whole load, same as any other kind) and best-effort Pods, then
// aggregates client-side via resources.BuildJobListSummaries +
// resources.ProjectJobList — never a per-row Pod read. Unlike
// loadCronJobRows, SOURCE resolution needs nothing beyond the Job object
// itself (resources/jobs.go's own jobSource) — Pods are read purely for the
// 'l' key's newest-attempt target (JobListSummary.NewestPodName), so a
// failed Pod read degrades that one verb, never the list itself.
func loadJobRows(ctx context.Context, lister resources.RawLister, namespace string, columns int, currentUser string) rowsLoadedMsg {
	jobObjs, err := lister.ListRaw(ctx, kube.KindJob, namespace)
	if err != nil {
		return rowsLoadedMsg{kind: kube.KindJob, columns: columns, err: err}
	}
	podObjs, _ := lister.ListRaw(ctx, kube.KindPod, namespace)

	summaries := resources.BuildJobListSummaries(jobObjs, podObjs, nil)
	// Base stable order (namespace, then name) — applyRowsLoaded's own
	// applySort call layers §37a's real default (unhealthy-first, then
	// newest — sort.go's jobRanked/jobAgeTiebreak) on top of this, the same
	// two-step loadCronJobRows above already uses.
	slices.SortStableFunc(summaries, func(x, y resources.JobListSummary) int {
		a, b := x.Object, y.Object
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
		)
	})

	now := time.Now()
	rows := make([]resources.Row, len(summaries))
	for i, s := range summaries {
		rows[i] = resources.ProjectJobList(s, now, currentUser)
	}
	return rowsLoadedMsg{
		kind: kube.KindJob, columns: columns, rows: rows, pods: podsFromObjs(podObjs),
		jobListSummaries: summaries,
	}
}

// markCronJobHistoryUnavailable overwrites row's ACT/LAST RUN cells
// (registry.go's CronJob Columns order: Name, Schedule, Susp, Act, Last
// Run, Next — indexes 3/4) when the Job cache couldn't be read for this
// load (§4.4 point 5): a stalled/denied Job list must never render as the
// ordinary "no retained runs"/"0" a genuinely history-less CronJob gets,
// since that is a claim about the cluster this load never actually
// confirmed. SCHEDULE/SUSP/NEXT are CronJob-spec-only (no Job data
// involved) and stay exactly as ProjectCronJob computed them.
func markCronJobHistoryUnavailable(row *resources.Row) {
	if len(row.Cells) > 4 {
		row.Cells[3] = "–"
		row.Cells[4] = "unavailable"
	}
	row.Active = false
}

// SwitchesNamespaceInPlace reports that this screen handles
// tui.SwitchNamespaceMsg itself (see tui.NamespaceSwitcher).
func (m Model) SwitchesNamespaceInPlace() bool { return true }

var _ tui.NamespaceSwitcher = (*Model)(nil)
