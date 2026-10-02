package kube

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TimelineEntryKind distinguishes 16a/16b's three merged-feed sources
// (docs/design README.md §16a: "events + container restarts + rollout
// revisions merged into a single feed").
type TimelineEntryKind int

const (
	TimelineEvent TimelineEntryKind = iota
	TimelineRestart
	TimelineRollout
	// TimelineRevision is §32a's git row: a Flux reconciler applied a new
	// source revision. Its own kind rather than a TimelineEvent because it
	// answers the timeline's actual question ("what changed?") on a GitOps
	// cluster, and gets its own marker to say so.
	TimelineRevision
	// TimelineSync is §34a's Argo CD analogue: an Application finished a
	// sync operation. Shares TimelineRevision's ◆ accent-purple marker —
	// "one ◆ marker, two GitOps engines" — but its own row text ("Sync
	// applied", not "Revision applied") and its own field source: Argo's
	// own sync-completed Event carries neither revision nor initiator,
	// unlike Flux's reconcile events, so those come from the Application
	// object itself (tasks/timeline/load.go's argoSyncStatusOf). Never
	// carries a CommitSubject — Argo's Event has no commit-message text to
	// parse, and there is no other on-cluster source for one.
	TimelineSync
)

// TimelineEntry is one row of 16a/16b's merged clock, newest-first once run
// through MergeTimeline.
type TimelineEntry struct {
	Time      time.Time
	Kind      TimelineEntryKind
	Object    string // "Kind/Name", e.g. "Pod/nva-worker-9k2ss"
	Namespace string
	Severity  string // "Warning" | "Normal" — TimelineEvent only
	Reason    string
	Message   string
	// Revision/Image are set on TimelineRollout entries only — the 16b
	// revision rail reads them directly rather than re-parsing Message.
	Revision int
	Image    string
	// GitRevision is set on TimelineRevision and TimelineSync entries: the
	// shortened "master@efd398b"/"main@e41b90c" form. CommitSubject is set
	// on TimelineRevision entries only — the commit's first line, which
	// source-controller publishes *only* in an event message and which
	// therefore may legitimately be empty — the artifact carries no commit
	// metadata and kute reads no git remote (docs/design README.md §31a).
	// TimelineSync never carries one: Argo's own sync-completed Event has
	// no commit-message text to parse, and there is no other on-cluster
	// source for one (§34a). Never fabricated.
	GitRevision   string
	CommitSubject string
	// By is a TimelineRollout entry's optional attribution — the owning
	// Deployment's own "kubectl.kubernetes.io/change-cause" annotation,
	// when present (16a's "· by ci@github", docs/design README.md §16a) —
	// or a TimelineSync entry's sync initiator: "auto-sync", or the
	// status.operationState.initiatedBy.username Argo recorded (§34a: "was
	// this a human or the machine? — matters at 2am"). Left empty
	// otherwise; never fabricated. Populated by tasks/timeline's own
	// load.go, not here — TimelineFromRollouts only sees ReplicaSets, not
	// their owning Deployment, and TimelineFromArgoEvents' own Event
	// carries no initiator either.
	By string
	// LiveStatusText/LiveStatusBad are the 16b revision rail's
	// current-revision (index 0 only) live rollout-progress override — set
	// when the owning Deployment's own resources.DeploymentRollout status
	// isn't "stable" (still progressing, or degraded/stalled), so the rail
	// doesn't call a rollout "stable" while new pods are still starting.
	// Empty/false means "use the restart-based stable/restarts-since text
	// instead." Populated by tasks/timeline's own load.go, not here (same
	// reasoning as By); never set on any entry but rail[0].
	LiveStatusText string
	LiveStatusBad  bool
}

// TimelineFromEvents projects deduped EventGroups (DedupeEvents) into
// TimelineEntry rows.
func TimelineFromEvents(groups []EventGroup) []TimelineEntry {
	out := make([]TimelineEntry, 0, len(groups))
	for _, g := range groups {
		out = append(out, TimelineEntry{
			Time:      g.LastSeen,
			Kind:      TimelineEvent,
			Object:    g.Object,
			Namespace: g.Namespace,
			Severity:  g.Type,
			Reason:    g.Reason,
			Message:   g.Message,
		})
	}
	return out
}

// TimelineFromRestarts turns each pod's most recent container termination
// into one restart entry. The k8s API only retains the latest terminated
// state per container (ContainerStatus.LastTerminationState), so a pod's
// full restart history isn't reconstructable from the live cluster alone —
// one entry per pod's LastTermination is the same approximation 5a's own
// last-termination banner already makes.
func TimelineFromRestarts(pods []Pod) []TimelineEntry {
	out := make([]TimelineEntry, 0, len(pods))
	for _, p := range pods {
		lt := p.LastTermination
		if lt == nil || lt.FinishedAt.IsZero() {
			continue
		}
		out = append(out, TimelineEntry{
			Time:      lt.FinishedAt,
			Kind:      TimelineRestart,
			Object:    "Pod/" + p.Name,
			Namespace: p.Namespace,
			Reason:    "Restarted",
			Message:   fmt.Sprintf("%s · %s · exit %d", lt.Container, lt.Reason, lt.ExitCode),
		})
	}
	return out
}

// TimelineFromRollouts derives one entry per Deployment revision from its
// owned ReplicaSets' "deployment.kubernetes.io/revision" annotation — the
// same signal `kubectl rollout history` reads. objs is a ReplicaSet list
// (KindReplicaSet); callers may pre-filter it to one Deployment's owned set
// for 16b's object-scoped feed.
func TimelineFromRollouts(objs []runtime.Object) []TimelineEntry {
	out := make([]TimelineEntry, 0, len(objs))
	for _, obj := range objs {
		rs, ok := obj.(*appsv1.ReplicaSet)
		if !ok || len(rs.OwnerReferences) == 0 || rs.OwnerReferences[0].Kind != "Deployment" {
			continue
		}
		revText, ok := rs.Annotations["deployment.kubernetes.io/revision"]
		if !ok {
			continue
		}
		rev, _ := strconv.Atoi(revText)
		out = append(out, TimelineEntry{
			Time:      rs.CreationTimestamp.Time,
			Kind:      TimelineRollout,
			Object:    "Deployment/" + rs.OwnerReferences[0].Name,
			Namespace: rs.Namespace,
			Reason:    "Rollout",
			Message:   fmt.Sprintf("revision %d · %s", rev, rolloutReplicaSetImage(rs)),
			Revision:  rev,
			Image:     rolloutReplicaSetImage(rs),
		})
	}
	return out
}

func rolloutReplicaSetImage(rs *appsv1.ReplicaSet) string {
	containers := rs.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return "–"
	}
	img := containers[0].Image
	if len(containers) > 1 {
		img += fmt.Sprintf(" +%d", len(containers)-1)
	}
	return img
}

// MergeTimeline concatenates every source's entries and sorts them
// newest-first — 16a/16b's "one clock" (docs/design README.md §16a).
func MergeTimeline(sources ...[]TimelineEntry) []TimelineEntry {
	var out []TimelineEntry
	for _, s := range sources {
		out = append(out, s...)
	}
	slices.SortFunc(out, func(a, b TimelineEntry) int { return b.Time.Compare(a.Time) })
	return out
}

// fluxRevisionAnnotations are the per-controller annotations Flux stamps on
// its own reconcile Events, carrying the revision it just applied. Reading
// the annotation rather than parsing the message is the whole point: the
// message is prose that has changed between Flux versions, the annotation
// is data.
var fluxRevisionAnnotations = []string{
	"kustomize.toolkit.fluxcd.io/revision",
	"helm.toolkit.fluxcd.io/revision",
	"source.toolkit.fluxcd.io/revision",
}

// storedArtifactCommit matches source-controller's own "stored artifact"
// event message, the one place on the cluster a commit subject appears.
// Anchored on the message shape rather than the event Reason, which has
// varied between NewArtifact and GitOperationSucceeded across versions.
var storedArtifactCommit = regexp.MustCompile(`stored artifact for commit '(.+)'`)

// FluxCommitSubject extracts the commit subject from a source-controller
// event message, or "" when the message isn't that one.
//
// source-controller does not always have a subject to put there. Observed
// on a real cluster, the same message renders all of these:
//
//	stored artifact for commit 'openwebui bumped to 16.0.0'
//	stored artifact for commit 'master@sha1:efd398bed98a38348c7702355ecd…'
//
// The second is the revision standing in for a subject it didn't have.
// Echoing that back would print the SHA twice on one row, once as REVISION
// and once as a quoted "subject" — a fabricated fact in the slot reserved
// for the one thing the cluster genuinely knows. A revision-shaped capture
// is therefore no subject at all.
func FluxCommitSubject(message string) string {
	m := storedArtifactCommit.FindStringSubmatch(message)
	if len(m) != 2 {
		return ""
	}
	subject := strings.TrimSpace(m[1])
	if looksLikeFluxRevision(subject) {
		return ""
	}
	return subject
}

// looksLikeFluxRevision reports whether s is a Flux revision string
// ("<ref>@<algo>:<hex>" or a bare "<algo>:<hex>") rather than prose.
func looksLikeFluxRevision(s string) bool {
	digest := s
	if at := strings.LastIndex(s, "@"); at >= 0 {
		digest = s[at+1:]
	}
	algo, hex, found := strings.Cut(digest, ":")
	if !found || len(hex) < 7 {
		return false
	}
	switch algo {
	case "sha1", "sha256", "sha512":
	default:
		return false
	}
	for _, r := range hex {
		isHexDigit := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHexDigit {
			return false
		}
	}
	return true
}

// TimelineFromFluxEvents projects Flux reconcile events carrying a revision
// annotation into §32a's revision rows, newest-first once merged.
//
// subjects maps a revision to its commit subject, gathered from
// source-controller's own events; a revision missing from it renders as the
// bare SHA, which is the honest degradation — Events expire, and there is
// no other on-cluster source for the subject.
func TimelineFromFluxEvents(events []Event, subjects map[string]string) []TimelineEntry {
	var out []TimelineEntry
	for _, e := range events {
		rev := FluxEventRevision(e)
		if rev == "" {
			continue
		}
		out = append(out, TimelineEntry{
			Time:          e.LastSeen,
			Kind:          TimelineRevision,
			Object:        e.Object,
			Namespace:     e.Namespace,
			Reason:        e.Reason,
			GitRevision:   ShortFluxRevision(rev),
			CommitSubject: subjects[rev],
		})
	}
	return out
}

// FluxCommitSubjects builds the revision→subject index for one namespace's
// events, pairing every "stored artifact for commit" message with the
// revision it was stored for.
//
// The event's own revision annotation is that revision, and wins whenever
// it is present. Without one the only stand-in is the revision the source
// object reports *now* (revisionOf), which is true of the newest
// stored-artifact event per source alone: a source that fetched two commits
// within the Event TTL would otherwise pair both subjects with the head
// revision, and whichever was iterated last — possibly the older commit —
// would be printed against it.
func FluxCommitSubjects(events []Event, revisionOf func(object string) string) map[string]string {
	out := map[string]string{}
	annotated := map[string]bool{}
	newest := map[string]Event{} // per source object, unannotated only
	for _, e := range events {
		subject := FluxCommitSubject(e.Message)
		if subject == "" {
			continue
		}
		if rev := FluxEventRevision(e); rev != "" {
			out[rev], annotated[rev] = subject, true
			continue
		}
		if prev, ok := newest[e.Object]; !ok || e.LastSeen.After(prev.LastSeen) {
			newest[e.Object] = e
		}
	}
	for object, e := range newest {
		if rev := revisionOf(object); rev != "" && !annotated[rev] {
			out[rev] = FluxCommitSubject(e.Message)
		}
	}
	return out
}

// FluxEventRevision reads whichever Flux revision annotation the event
// carries.
func FluxEventRevision(e Event) string {
	for _, key := range fluxRevisionAnnotations {
		if v := e.Annotations[key]; v != "" {
			return v
		}
	}
	return ""
}

// FluxTracksSourceRevision reports whether a reconciler of this API kind
// records what it applied in its *source's* revision vocabulary — the
// precondition for comparing the two and calling the difference drift
// (§30b's "source ahead", §31a's chain grid).
//
// A Kustomization does: its lastAppliedRevision is the git revision its
// GitRepository published, so an inequality means the source moved on. A
// HelmRelease does not, whichever source kind backs it: it records a *chart
// version* ("19.6.1") while its HelmRepository's artifact revision is the
// digest of the repo index. Those two are never equal and never will be, so
// comparing them reports permanent drift on every healthy Helm chain — a
// confident, always-wrong claim, which is worse than saying nothing.
func FluxTracksSourceRevision(apiKind string) bool {
	return apiKind != "HelmRelease"
}

// ArgoSyncStatus is what TimelineFromArgoEvents needs from an Application's
// own status.operationState — unlike Flux's reconcile events, Argo's
// OperationCompleted Event carries neither revision nor initiator, so
// callers resolve them from the object itself (tasks/timeline/load.go's
// argoSyncStatusOf).
type ArgoSyncStatus struct {
	Ref      string // status.operationState.operation.sync.revision, e.g. "main"
	Revision string // status.operationState.syncResult.revision, the bare SHA it resolved to
	By       string // "auto-sync", or status.operationState.initiatedBy.username
}

// ArgoOperationCompletedReason is the Event Reason
// argocd-application-controller stamps once a sync operation finishes —
// TimelineFromArgoEvents' own filter.
const ArgoOperationCompletedReason = "OperationCompleted"

// ShortArgoRevision composes Argo's own "<ref>@<7-char-sha>" short form —
// mirrors resources.argoRevisionCell's identical composition for §33a's own
// REVISION column, duplicated here rather than imported since kube can't
// depend on resources (resources already depends on kube). Argo's SHA
// carries no "sha1:"-style algorithm prefix, unlike Flux's own digests, so
// ShortFluxRevision's colon-splitting logic doesn't apply.
func ShortArgoRevision(ref, sha string) string {
	if len(sha) > 7 {
		sha = sha[:7]
	}
	if ref == "" {
		ref = "HEAD"
	}
	if sha == "" {
		return ref
	}
	return ref + "@" + sha
}

// TimelineFromArgoEvents projects Argo CD sync-completed events into §34a
// sync rows — 34a's own analogue of TimelineFromFluxEvents, sharing its
// glyph/styling (docs/design README.md §34a: "one ◆ marker, two GitOps
// engines") but its own row text ("Sync applied", not "Revision applied")
// since an Application's sync is a different fact from a Kustomization's
// reconcile.
//
// statusOf resolves an event's own object ("Application/kute-billing") to
// the sync it just completed; an event whose object statusOf can't resolve
// (already gone, or the lister isn't wired) is skipped rather than rendered
// with blank fields.
func TimelineFromArgoEvents(events []Event, statusOf func(object string) (ArgoSyncStatus, bool)) []TimelineEntry {
	var out []TimelineEntry
	for _, e := range events {
		if e.Reason != ArgoOperationCompletedReason {
			continue
		}
		status, ok := statusOf(e.Object)
		if !ok {
			continue
		}
		out = append(out, TimelineEntry{
			Time:        e.LastSeen,
			Kind:        TimelineSync,
			Object:      e.Object,
			Namespace:   e.Namespace,
			Reason:      e.Reason,
			GitRevision: ShortArgoRevision(status.Ref, status.Revision),
			By:          status.By,
		})
	}
	return out
}

// ShortFluxRevision collapses "master@sha1:efd398be…" to "master@efd398b",
// and a bare "sha256:d83a…" digest to its first 7 hex characters. A plain
// version string is returned unchanged.
func ShortFluxRevision(rev string) string {
	ref := ""
	digest := rev
	if at := strings.LastIndex(rev, "@"); at >= 0 {
		ref, digest = rev[:at], rev[at+1:]
	}
	colon := strings.Index(digest, ":")
	if colon < 0 {
		return rev
	}
	digest = digest[colon+1:]
	if len(digest) > 7 {
		digest = digest[:7]
	}
	switch {
	case digest == "":
		return ref
	case ref == "":
		return digest
	}
	return ref + "@" + digest
}
