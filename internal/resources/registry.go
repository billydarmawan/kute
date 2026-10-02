package resources

import (
	"maps"
	"slices"

	"github.com/kute-dev/kute/internal/kube"
)

// Registry maps resource kinds to their descriptors.
type Registry struct {
	byKind map[kube.ResourceKind]Descriptor
}

// Descriptor returns the descriptor for kind, if registered.
func (r Registry) Descriptor(kind kube.ResourceKind) (Descriptor, bool) {
	d, ok := r.byKind[kind]
	return d, ok
}

// Has reports whether kind is registered.
func (r Registry) Has(kind kube.ResourceKind) bool {
	_, ok := r.byKind[kind]
	return ok
}

// Kinds lists every registered kind, in no particular order. For callers
// that need to do something per kind (counting them all, say) rather than
// look one up.
func (r Registry) Kinds() []kube.ResourceKind {
	return slices.Collect(maps.Keys(r.byKind))
}

// KindFor resolves an API (group, Kind) — an Event's involvedObject, an
// inventory entry — to the registry key it is listed and navigated by.
//
// kube.RegistryKindFor settles every collision it can see from the pair
// alone; the registry settles the one it can't — two discovered groups
// sharing an otherwise-unclaimed Kind, where discovery qualified the loser
// (kube.AssignRegistryKinds).
func (r Registry) KindFor(group, apiKind string) kube.ResourceKind {
	kind := kube.RegistryKindFor(group, apiKind)
	if d, ok := r.Descriptor(kind); ok && d.Custom && d.APIGroup != group {
		return kube.QualifiedKind(apiKind, group)
	}
	return kind
}

// Register adds or replaces d's entry — how discovered kinds (14a) and the
// CustomResourceDefinition list (14b) join a DefaultRegistry() base at
// connect/switch time (see BuildDiscoveredRegistry in crd.go). byKind is a
// map (reference type), so this mutates in place even though Registry
// itself is passed by value everywhere else.
func (r Registry) Register(d Descriptor) {
	if d.Health == nil {
		d.Health = StatusHealth
	}
	if d.HealthLabel == nil {
		d.HealthLabel = DefaultHealthLabel
	}
	if d.HealthGlyph == nil {
		d.HealthGlyph = DefaultHealthGlyph
	}
	if d.HealthTone == nil {
		d.HealthTone = DefaultHealthTone
	}
	r.byKind[d.Kind] = d
}

// DefaultRegistry returns the built-in descriptors for every supported kind.
// Adding a resource type is a single entry here plus its Project function.
func DefaultRegistry() Registry {
	descriptors := []Descriptor{
		{Kind: kube.KindPod, Group: GroupWorkloads, Display: "Pods", Icon: "◈", Columns: []string{"Name", "Rdy", "Status", "Restarts", "CPU", "MEM", "Node", "Age"}, Describe: "running application instances", Project: projectPod, HealthLabel: podHealthLabel},
		{Kind: kube.KindDeployment, Group: GroupWorkloads, Display: "Deployments", Icon: "◈", Columns: []string{"Name", "Rdy", "Rollout", "Image", "Age"}, Describe: "declarative pod rollouts", Project: projectDeployment(nil), HealthLabel: deploymentHealthLabel},
		{Kind: kube.KindDaemonSet, Group: GroupWorkloads, Display: "DaemonSets", Icon: "◈", Columns: []string{"Name", "Rdy", "Available", "Age"}, Describe: "one pod per matching node", Project: projectDaemonSet},
		{Kind: kube.KindStatefulSet, Group: GroupWorkloads, Display: "StatefulSets", Icon: "◈", Columns: []string{"Name", "Rdy", "Age"}, Describe: "stable, ordered pod identities", Project: projectStatefulSet},
		{Kind: kube.KindReplicaSet, Group: GroupWorkloads, Display: "ReplicaSets", Icon: "◈", Columns: []string{"Name", "Rdy", "Replicas", "Age"}, Describe: "pod replica sets behind a deployment", Project: projectReplicaSet},
		// Columns/Project/Health per §37a (v0.9.0): NAME / COMPL / DURATION /
		// FAILED / SOURCE / AGE, Job-specific health semantics (jobHealth/
		// jobHealthLabel, jobs.go). Project is deliberately Job-list-unaware
		// of Pod/CronJob join data for the same reason projectCronJobFallback
		// is — see its own doc comment and jobs.go's package doc comment;
		// browse's Jobs branch bypasses this entirely and calls
		// resources.BuildJobListSummaries + resources.ProjectJobList directly.
		{Kind: kube.KindJob, Group: GroupWorkloads, Display: "Jobs", Icon: "◈", Columns: []string{"Name", "Compl", "Duration", "Failed", "Source", "Age"}, Describe: "run-to-completion pods", Project: projectJobFallback, Health: jobHealth, HealthLabel: jobHealthLabel},
		// Columns/Project/Health per v0.8.0 plan §4.3 (Phase 1): NAME /
		// SCHEDULE / SUSP / ACT / LAST RUN / NEXT, CronJob-specific health
		// semantics (cronJobHealth/cronJobHealthLabel, cronjobs.go).
		//
		// Project is deliberately Job-unaware — see
		// projectCronJobFallback's own doc comment in projections.go and
		// cronjobs.go's package doc comment. browse's CronJob branch
		// (Phase 4) bypasses this entirely and calls
		// resources.BuildCronJobSummaries + resources.ProjectCronJob
		// directly with real Job/Pod data; no future caller should expect
		// this Project field alone to produce a Job-aware row.
		{Kind: kube.KindCronJob, Group: GroupWorkloads, Display: "CronJobs", Icon: "◷", Columns: []string{"Name", "Schedule", "Susp", "Act", "Last Run", "Next"}, Describe: "jobs run on a schedule", Project: projectCronJobFallback, Health: cronJobHealth, HealthLabel: cronJobHealthLabel},
		{Kind: kube.KindService, Group: GroupNetworking, Display: "Services", Icon: "◇", Columns: []string{"Name", "Type", "ClusterIP", "ExternalIP", "Ports", "Age"}, Describe: "stable network endpoints", Project: projectService},
		{Kind: kube.KindIngress, Group: GroupNetworking, Display: "Ingresses", Icon: "◇", Columns: []string{"Name", "Class", "Hosts", "TLS", "Backends", "Age"}, Describe: "external HTTP(S) routing rules", Project: projectIngress(nil)},
		{Kind: kube.KindConfigMap, Group: GroupConfig, Display: "ConfigMaps", Icon: "⚙", Columns: []string{"Name", "Data", "Age"}, Describe: "non-secret configuration data", Project: projectConfigMap},
		{Kind: kube.KindSecret, Group: GroupConfig, Display: "Secrets", Icon: "⚙", Columns: []string{"Name", "Type", "Data", "Age"}, Describe: "sensitive configuration data", Project: projectSecret},
		{Kind: kube.KindPersistentVolumeClaim, Group: GroupStorage, Display: "PersistentVolumeClaims", Icon: "▤", Columns: []string{"Name", "Status", "Capacity", "Age"}, Describe: "requested persistent storage", Project: projectPVC},
		{Kind: kube.KindNode, Group: GroupCluster, Display: "Nodes", Icon: "⬡", Columns: []string{"Name", "Status", "Health", "Pods", "CPU", "MEM", "Version", "Age"}, Describe: "cluster worker machines", ClusterScoped: true, Project: projectNode, HealthLabel: nodeHealthLabel, HealthGlyph: nodeHealthGlyph},
		{Kind: kube.KindNamespace, Group: GroupCluster, Display: "Namespaces", Icon: "⬡", Columns: []string{"Name", "Status", "Age"}, Describe: "cluster resource partitions", ClusterScoped: true, Project: projectNamespace},
		{Kind: kube.KindEvent, Group: GroupObservability, Display: "Events", Icon: "∿", Columns: []string{"Type", "Reason", "Object", "Age"}, Describe: "cluster activity and warnings", Project: projectEvent},
		{Kind: kube.KindForward, Group: GroupCluster, Display: "Forwards", Icon: "⇄", Columns: []string{"Local", "Target", "Namespace", "Uptime", "Traffic"}, FlexColumn: "Target", Describe: "active kubectl port-forward sessions", ClusterScoped: true, Project: projectForward, HealthLabel: forwardHealthLabel},
		{Kind: kube.KindHelmRelease, Group: GroupCluster, Display: "Helm Releases", Icon: "⎈", Columns: []string{"Release", "Chart", "Latest", "App Ver", "Rev", "Status", "Updated"}, FlexColumn: "Release", Describe: "deployed Helm chart releases", Project: projectHelmRelease, Health: helmReleaseHealth, HealthLabel: helmReleaseHealthLabel, HealthGlyph: helmReleaseHealthGlyph},
	}
	// CustomResourceDefinition (14b) is always present, like Forward — a
	// built-in, not a discovered kind — registered here with a nil counter
	// (every row's COUNT reads 0) so DefaultGroups()/DefaultRegistry() stay
	// mutually consistent before any connect has happened.
	// BuildDiscoveredRegistry re-registers it with a live InstanceCounter
	// once a cluster is reachable.
	descriptors = append(descriptors, CRDDescriptor(nil))

	// Through Register, not a second defaulting block of its own: the two
	// used to be copies, so a newly added Health* default reached discovered
	// kinds and silently skipped every built-in one (HealthTone was added
	// that way and nil-panicked on Pods).
	reg := Registry{byKind: make(map[kube.ResourceKind]Descriptor, len(descriptors))}
	for _, d := range descriptors {
		reg.Register(d)
	}
	return reg
}

// podHealthLabel is the docs/design/README.md 2a health-strip wording:
// "32 running", "2 pending", "1 crashloop", "1 completed".
func podHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "running"
	case StatusWarn:
		return "pending"
	case StatusFail:
		return "crashloop"
	default:
		return "completed"
	}
}

// deploymentHealthLabel is 9a's ROLLOUT-column wording reused for the
// health strip, so both agree on what "stable"/"progressing"/"degraded"
// mean for a Deployment (docs/design README.md §9a).
func deploymentHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "stable"
	case StatusWarn:
		return "progressing"
	case StatusFail:
		return "degraded"
	default:
		return "other"
	}
}

// forwardHealthLabel is 13c's health-strip wording: "3 active · 1
// reconnecting".
func forwardHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "active"
	case StatusWarn:
		return "reconnecting"
	default:
		return "other"
	}
}

// helmReleaseHealth is 18a's bespoke tally: the usual per-status counts,
// plus outdated releases counted *across* them. A release that is both
// deployed and behind its repo shows up in both numbers, which is why
// Outdated sits outside HealthCounts.Total().
func helmReleaseHealth(rows []Row) HealthCounts {
	h := StatusHealth(rows)
	for _, r := range rows {
		if r.Outdated {
			h.Outdated++
		}
	}
	return h
}

// helmReleaseHealthGlyph is 18a's own strip example: pending-upgrade
// renders ◌, not the generic ▲ every other kind's Warn class uses.
func helmReleaseHealthGlyph(class StatusClass) string {
	if class == StatusWarn {
		return "\u25cc" // ◌
	}
	return DefaultHealthGlyph(class)
}

// nodeHealthGlyph is 11a's: a cordoned node renders ◈, not the generic ○
// every other kind's Neutral class falls back to.
func nodeHealthGlyph(class StatusClass) string {
	if class == StatusNeutral {
		return "\u25c8" // ◈
	}
	return DefaultHealthGlyph(class)
}

// helmReleaseHealthLabel is 18a's health-strip wording: "3 deployed · 1
// pending-upgrade · 1 failed".
func helmReleaseHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "deployed"
	case StatusWarn:
		return "pending-upgrade"
	case StatusFail:
		return "failed"
	default:
		return "other"
	}
}

// nodeHealthLabel is the docs/design/README.md §11a health-strip wording:
// "3 ready · 1 pressure · 1 cordoned".
func nodeHealthLabel(class StatusClass) string {
	switch class {
	case StatusOK:
		return "ready"
	case StatusWarn:
		return "pressure"
	case StatusFail:
		return "not ready"
	default:
		return "cordoned"
	}
}
