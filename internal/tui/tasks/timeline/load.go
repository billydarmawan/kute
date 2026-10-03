package timeline

import (
	"cmp"
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

// load fetches every source of 16a/16b's merged feed (Events, container
// restarts, Deployment rollout revisions) for the screen's scope and merges
// them into one newest-first clock (docs/design README.md §16a: "one clock,
// newest first").
func (m Model) load() tea.Cmd {
	src := m.events
	lister := m.lister
	session := m.session
	namespace := m.namespace
	objectKind := m.objectKind
	objectName := m.objectName
	timeout := m.timeout
	epoch := m.loadEpoch

	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		rawEvents, err := eventsForScope(ctx, src, lister, namespace, objectKind, objectName)
		if err != nil {
			return loadedMsg{epoch: epoch, err: err}
		}
		// Deployment is part of applyLoaded's required cache set even when
		// this particular scope has no ReplicaSet-owned Pod and therefore no
		// rollout rows. Start/read it unconditionally: otherwise an ownerless
		// Pod can leave KindSynced(Deployment) false forever while the screen
		// waits on a cache that none of the helpers below ever touches.
		if lister != nil {
			_, _ = lister.ListRaw(ctx, kube.KindDeployment, namespace)
		}

		// §32a: Flux reconcile events become their own revision rows, so
		// they must not also appear as ordinary event rows — the revision
		// row says everything the reconcile event did, better.
		fluxEvents, rest := splitFluxRevisionEvents(rawEvents)
		// §34a: same reasoning for Argo's own sync-completed events — they
		// become their own sync rows rather than an ordinary event row.
		argoEvents, plainEvents := splitArgoSyncEvents(rest)
		// The subject outlives the event that carried it: this load's own
		// "stored artifact for commit" sightings are merged into the
		// session cache and the whole cache is what the rows read, so a
		// revision keeps its subject once seen (docs/flux-plan.md T14).
		subjects := session.RetainFluxCommitSubjects(
			kube.FluxCommitSubjects(rawEvents, sourceRevisionOf(ctx, lister, namespace)))
		revisionEntries := kube.TimelineFromFluxEvents(fluxEvents, subjects)
		argoSyncEntries := kube.TimelineFromArgoEvents(argoEvents, argoSyncStatusOf(ctx, lister, namespace))

		eventEntries := kube.TimelineFromEvents(kube.DedupeEvents(plainEvents))
		restartEntries := restartsForScope(ctx, lister, namespace, objectKind, objectName)
		rolloutEntries, rail, railDeployment := rolloutsForScope(ctx, lister, namespace, objectKind, objectName)
		attachChangeCause(ctx, lister, namespace, rolloutEntries)
		attachChangeCause(ctx, lister, namespace, rail)

		merged := kube.MergeTimeline(eventEntries, restartEntries, rolloutEntries, revisionEntries, argoSyncEntries)
		return loadedMsg{epoch: epoch, entries: merged, rail: rail, railDeployment: railDeployment}
	}
}

// eventsForScope fetches every event relevant to the screen's scope: the
// primary object's own events (ObjectEvents), plus — for scopes that own
// pods (Node, Deployment, StatefulSet, DaemonSet) — events on each of "its
// pods" too, the same promise feedHeader's own "WHAT — <kind>/<name> + its
// pods" text already makes. A container-level event like
// CreateContainerError/BackOff/Unhealthy is always emitted with
// involvedObject == the Pod, never the owning Deployment, so ObjectEvents on
// the Deployment alone silently drops it — exactly the gap this closes.
func eventsForScope(ctx context.Context, src EventsReader, lister resources.RawLister, namespace string, objectKind kube.ResourceKind, objectName string) ([]kube.Event, error) {
	if objectKind == "" {
		return src.NamespaceEvents(ctx, namespace)
	}
	primary, err := src.ObjectEvents(ctx, namespace, objectKind, objectName)
	if err != nil {
		return nil, err
	}
	podNames := podsOwnedByScope(ctx, lister, namespace, objectKind, objectName)
	if len(podNames) == 0 {
		return primary, nil
	}
	all, err := src.NamespaceEvents(ctx, namespace)
	if err != nil {
		// Best-effort: still show the primary object's own events rather
		// than failing the whole load over the owned-pods lookup.
		//nolint:nilerr // deliberate — see above.
		return primary, nil
	}
	out := append([]kube.Event(nil), primary...)
	for _, e := range all {
		kind, name := splitObject(e.Object)
		if kind == kube.KindPod && podNames[kube.PodKey(e.Namespace, name)] {
			out = append(out, e)
		}
	}
	return out, nil
}

// restartsForScope reads container-restart entries for the screen's scope:
// every pod in namespace (16a), the one pod (16b on a Pod), every pod
// scheduled on the node (16b on a Node), or every pod owned by the
// Deployment/StatefulSet/DaemonSet (16b on one of those, via
// podsOwnedByScope) — nil (best-effort, matching tasks/events' own
// failingPods degrade-gracefully precedent) when lister isn't wired, and
// skipped entirely for object kinds with no pod-ownership concept at all
// (a Service, say).
func restartsForScope(ctx context.Context, lister resources.RawLister, namespace string, objectKind kube.ResourceKind, objectName string) []kube.TimelineEntry {
	if lister == nil {
		return nil
	}
	objs, err := lister.ListRaw(ctx, kube.KindPod, namespace)
	if err != nil {
		return nil
	}
	var scopedNames map[string]bool
	switch objectKind {
	case kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet:
		scopedNames = podsOwnedByScope(ctx, lister, namespace, objectKind, objectName)
	}
	var pods []kube.Pod
	for _, obj := range objs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		switch objectKind {
		case "":
			// 16a: every pod in scope counts.
		case kube.KindPod:
			if pod.Name != objectName {
				continue
			}
		case kube.KindNode:
			if pod.Spec.NodeName != objectName {
				continue
			}
		case kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet:
			if !scopedNames[kube.PodKey(pod.Namespace, pod.Name)] {
				continue
			}
		default:
			continue
		}
		pods = append(pods, kube.PodFromObject(pod))
	}
	return kube.TimelineFromRestarts(pods)
}

// podsOwnedByScope resolves the pods "owned" by objectKind/objectName, keyed
// by kube.PodKey — a Node's pods span namespaces, so a bare name would let
// a same-named pod elsewhere leak into the node's feed —
// for scopes where that concept applies: scheduled there (Node), or owned
// directly (StatefulSet/DaemonSet) or via an intermediate ReplicaSet
// (Deployment). nil for every other kind, including Pod itself — a Pod's
// own scope needs no separate resolution, it already is the one pod.
func podsOwnedByScope(ctx context.Context, lister resources.RawLister, namespace string, objectKind kube.ResourceKind, objectName string) map[string]bool {
	if lister == nil {
		return nil
	}
	switch objectKind {
	case kube.KindNode, kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet:
	default:
		return nil
	}

	podObjs, err := lister.ListRaw(ctx, kube.KindPod, namespace)
	if err != nil {
		return nil
	}
	var rsToDeployment map[string]string
	if objectKind == kube.KindDeployment {
		rsToDeployment = replicaSetOwners(ctx, lister, namespace)
	}

	names := make(map[string]bool)
	for _, obj := range podObjs {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			continue
		}
		switch objectKind {
		case kube.KindNode:
			if pod.Spec.NodeName == objectName {
				names[kube.PodKey(pod.Namespace, pod.Name)] = true
			}
		case kube.KindStatefulSet, kube.KindDaemonSet:
			if ownerKind, ownerName, ok := splitOwner(ownerRef(pod.OwnerReferences)); ok && ownerKind == objectKind && ownerName == objectName {
				names[kube.PodKey(pod.Namespace, pod.Name)] = true
			}
		case kube.KindDeployment:
			if ownerKind, ownerName, ok := splitOwner(ownerRef(pod.OwnerReferences)); ok && ownerKind == kube.KindReplicaSet && rsToDeployment[ownerName] == objectName {
				names[kube.PodKey(pod.Namespace, pod.Name)] = true
			}
		}
	}
	return names
}

// replicaSetOwners maps every ReplicaSet name in namespace to its owning
// Deployment name (when any) — built once so podsOwnedByScope's Deployment
// case doesn't re-list/re-walk ReplicaSets per pod.
func replicaSetOwners(ctx context.Context, lister resources.RawLister, namespace string) map[string]string {
	objs, err := lister.ListRaw(ctx, kube.KindReplicaSet, namespace)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(objs))
	for _, obj := range objs {
		rs, ok := obj.(*appsv1.ReplicaSet)
		if !ok {
			continue
		}
		if depKind, depName, ok := splitOwner(ownerRef(rs.OwnerReferences)); ok && depKind == kube.KindDeployment {
			out[rs.Name] = depName
		}
	}
	return out
}

// rolloutsForScope reads Deployment rollout-revision entries: every
// Deployment's revisions in namespace (16a, no rail — mixed objects don't
// get one), or the scope's resolved owning Deployment's revisions plus the
// 16b revision rail (newest-first, index 0 the current revision) when the
// object resolves to one. ("", nil, "") when it doesn't (a Node, or a Pod
// with no Deployment-owned ReplicaSet).
func rolloutsForScope(ctx context.Context, lister resources.RawLister, namespace string, objectKind kube.ResourceKind, objectName string) (entries, rail []kube.TimelineEntry, railDeployment string) {
	if lister == nil {
		return nil, nil, ""
	}
	objs, err := lister.ListRaw(ctx, kube.KindReplicaSet, namespace)
	if err != nil {
		return nil, nil, ""
	}
	all := kube.TimelineFromRollouts(objs)
	// A rollback reuses the old ReplicaSet, so without this the rollback
	// lands at that ReplicaSet's original creation date, far outside the
	// incident window it belongs in.
	if deps, depErr := lister.ListRaw(ctx, kube.KindDeployment, namespace); depErr == nil {
		kube.RedateRolledBackRevisions(all, deps)
	}
	if objectKind == "" {
		return all, nil, ""
	}

	depName, ok := resolveOwningDeployment(ctx, lister, namespace, objectKind, objectName)
	if !ok {
		return nil, nil, ""
	}
	scoped := make([]kube.TimelineEntry, 0, len(all))
	for _, e := range all {
		if e.Object == "Deployment/"+depName {
			scoped = append(scoped, e)
		}
	}
	if len(scoped) == 0 {
		return nil, nil, ""
	}
	revisionRail := append([]kube.TimelineEntry(nil), scoped...)
	slices.SortFunc(revisionRail, func(a, b kube.TimelineEntry) int { return cmp.Compare(b.Revision, a.Revision) })
	attachLiveRolloutStatus(ctx, lister, namespace, depName, revisionRail)
	return scoped, revisionRail, depName
}

// attachLiveRolloutStatus sets rail[0]'s LiveStatusText/LiveStatusBad from
// depName's own live Deployment object — the same resources.DeploymentRollout
// classification the browse table's ROLLOUT column uses — so the revision
// rail's current-revision card doesn't call a rollout "stable" while the
// Deployment itself is still progressing (new pods starting/being pulled) or
// degraded (stalled past its progress deadline). Best-effort, matching every
// other load.go helper: a nil lister, a list error, or no matching Deployment
// just leaves the fields at their zero value, falling back to the rail's
// restart-based stable/restarts-since text.
func attachLiveRolloutStatus(ctx context.Context, lister resources.RawLister, namespace, depName string, rail []kube.TimelineEntry) {
	if lister == nil || len(rail) == 0 {
		return
	}
	objs, err := lister.ListRaw(ctx, kube.KindDeployment, namespace)
	if err != nil {
		return
	}
	for _, obj := range objs {
		dep, ok := obj.(*appsv1.Deployment)
		if !ok || dep.Name != depName {
			continue
		}
		text, status := resources.DeploymentRollout(dep)
		if status == resources.StatusOK {
			return
		}
		rail[0].LiveStatusText = text
		rail[0].LiveStatusBad = status == resources.StatusFail
		return
	}
}

// changeCauseAnnotation is the standard `kubectl rollout history` /
// `--record` annotation key ("kubectl.kubernetes.io/change-cause") — 16a's
// optional "· by ci@github" attribution (docs/design README.md §16a) reads
// it straight off the Deployment; never fabricated when absent.
const changeCauseAnnotation = "kubectl.kubernetes.io/change-cause"

// attachChangeCause sets entries' By field from each entry's own Deployment
// object's change-cause annotation, in place — best-effort (nil lister or a
// list error just leaves every By empty, the same degrade-gracefully
// precedent restartsForScope/rolloutsForScope already follow).
func attachChangeCause(ctx context.Context, lister resources.RawLister, namespace string, entries []kube.TimelineEntry) {
	if lister == nil || len(entries) == 0 {
		return
	}
	objs, err := lister.ListRaw(ctx, kube.KindDeployment, namespace)
	if err != nil {
		return
	}
	causes := make(map[string]string, len(objs))
	for _, obj := range objs {
		dep, ok := obj.(*appsv1.Deployment)
		if !ok {
			continue
		}
		if cause := dep.Annotations[changeCauseAnnotation]; cause != "" {
			causes[dep.Namespace+"/"+dep.Name] = cause
		}
	}
	for i, e := range entries {
		kind, name := splitObject(e.Object)
		if kind != kube.KindDeployment {
			continue
		}
		entries[i].By = causes[e.Namespace+"/"+name]
	}
}

// resolveOwningDeployment resolves objectKind/objectName to an owning
// Deployment name — objectName itself when it already is one, or (for a
// Pod) a walk through its ReplicaSet owner to that ReplicaSet's own
// Deployment owner, the identical two-hop lookup poddetail's own
// resolveOwnerWorkload makes for its "o" related-Deployment jump
// (duplicated per the repo's package-local-seam convention — task packages
// don't import each other). Every other object kind (Node, a StatefulSet-
// owned pod, …) has no rollout-revision concept, so ok is false.
func resolveOwningDeployment(ctx context.Context, lister resources.RawLister, namespace string, objectKind kube.ResourceKind, objectName string) (string, bool) {
	switch objectKind {
	case kube.KindDeployment:
		return objectName, true
	case kube.KindPod:
		objs, err := lister.ListRaw(ctx, kube.KindPod, namespace)
		if err != nil {
			return "", false
		}
		for _, obj := range objs {
			pod, ok := obj.(*corev1.Pod)
			if !ok || pod.Name != objectName {
				continue
			}
			ownerKind, ownerName, ok := splitOwner(ownerRef(pod.OwnerReferences))
			if !ok || ownerKind != kube.KindReplicaSet {
				return "", false
			}
			rsObjs, err := lister.ListRaw(ctx, kube.KindReplicaSet, namespace)
			if err != nil {
				return "", false
			}
			for _, rsObj := range rsObjs {
				rs, ok := rsObj.(*appsv1.ReplicaSet)
				if !ok || rs.Name != ownerName {
					continue
				}
				depKind, depName, ok := splitOwner(ownerRef(rs.OwnerReferences))
				if ok && depKind == kube.KindDeployment {
					return depName, true
				}
			}
			return "", false
		}
		return "", false
	default:
		return "", false
	}
}

// splitObject splits a kube.TimelineEntry.Object string ("Pod/nva-worker-
// 9k2ss") into its Kind and Name — mirrors tasks/events' own helper of the
// same name.
func splitObject(object string) (kube.ResourceKind, string) {
	kind, name, ok := strings.Cut(object, "/")
	if !ok {
		return "", ""
	}
	return kube.ResourceKind(kind), name
}

// splitOwner parses an "Owner/name" string (kube.PodFromObject's ownerRef
// shape) into its kind/name — mirrors poddetail's own helper of the same
// name.
func splitOwner(owner string) (kube.ResourceKind, string, bool) {
	kind, name, found := strings.Cut(owner, "/")
	if !found || kind == "" || name == "" {
		return "", "", false
	}
	return kube.ResourceKind(kind), name, true
}

// ownerRef mirrors kube.PodFromObject's own unexported helper of the same
// name — duplicated per the repo's small-pure-helper convention.
func ownerRef(refs []metav1.OwnerReference) string {
	if len(refs) == 0 {
		return ""
	}
	return refs[0].Kind + "/" + refs[0].Name
}

// splitFluxRevisionEvents partitions events into those carrying a Flux
// revision annotation (which become §32a revision rows) and the rest.
func splitFluxRevisionEvents(events []kube.Event) (flux, plain []kube.Event) {
	for _, e := range events {
		if kube.FluxEventRevision(e) != "" {
			flux = append(flux, e)
			continue
		}
		plain = append(plain, e)
	}
	return flux, plain
}

// sourceRevisionOf resolves a source object ("GitRepository/flux-system")
// to the revision its artifact currently reports, so a "stored artifact for
// commit" event — whose message names the subject but not the SHA — can be
// paired with one.
//
// ListRaw starts the named source kind's informer if nothing has read it
// yet. That is deliberate and narrow: only source kinds named by a matching
// "stored artifact" event are read, each at most once per load, from this
// load's tea.Cmd rather than the update loop — never a sweep of every Flux
// kind. A cache still filling simply yields no pairing this load.
func sourceRevisionOf(ctx context.Context, lister resources.RawLister, namespace string) func(string) string {
	cache := map[string]string{}
	return func(object string) string {
		kind, name := splitObject(object)
		if kind == "" || name == "" {
			return ""
		}
		if rev, ok := cache[object]; ok {
			return rev
		}
		objs, err := lister.ListRaw(ctx, kind, namespace)
		if err != nil {
			cache[object] = ""
			return ""
		}
		for _, o := range objs {
			u, ok := o.(*unstructured.Unstructured)
			if !ok || u.GetName() != name {
				continue
			}
			rev, _, _ := unstructured.NestedString(u.Object, "status", "artifact", "revision")
			cache[object] = rev
			return rev
		}
		cache[object] = ""
		return ""
	}
}

// argoApplicationKind is the bare Kind Argo's Application objects are
// discovered and read under (internal/resources/argo.go's own descriptor
// registers it the same way) — not a typed kube.ResourceKind constant since
// Application is read entirely through the generic dynamic-informer path,
// same as any other discovered CRD.
const argoApplicationKind = kube.ResourceKind("Application")

// splitArgoSyncEvents partitions events into those reporting a completed
// Argo sync (which become §34a sync rows) and the rest. Only an Argo
// Application's event is diverted: OperationCompleted is a generic reason
// another controller may use too, and TimelineFromArgoEvents drops anything
// that isn't an Application, so diverting it would lose it from the feed.
func splitArgoSyncEvents(events []kube.Event) (argo, plain []kube.Event) {
	for _, e := range events {
		kind, _ := splitObject(e.Object)
		group, _, _ := strings.Cut(e.ObjectAPIVersion, "/")
		if e.Reason == kube.ArgoOperationCompletedReason && kind == argoApplicationKind &&
			(e.ObjectAPIVersion == "" || group == kube.ArgoGroup) {
			argo = append(argo, e)
			continue
		}
		plain = append(plain, e)
	}
	return argo, plain
}

// argoSyncStatusOf resolves an Application object ("Application/kute-
// billing") to the sync its own status.operationState last completed — §34a's
// analogue of sourceRevisionOf, needed because (unlike Flux's reconcile
// events) Argo's OperationCompleted Event carries neither the revision nor
// who/what triggered it.
//
// Reads only kinds already in the cache; an un-started Application informer
// yields no rows rather than starting one from the update loop, the same
// contract sourceRevisionOf follows for Flux's source kinds.
func argoSyncStatusOf(ctx context.Context, lister resources.RawLister, namespace string) func(string) (kube.ArgoSyncStatus, bool) {
	type cacheEntry struct {
		status kube.ArgoSyncStatus
		found  bool
	}
	cache := map[string]cacheEntry{}
	return func(object string) (kube.ArgoSyncStatus, bool) {
		kind, name := splitObject(object)
		if kind != argoApplicationKind || name == "" {
			return kube.ArgoSyncStatus{}, false
		}
		if c, ok := cache[object]; ok {
			return c.status, c.found
		}
		objs, err := lister.ListRaw(ctx, kind, namespace)
		if err != nil {
			cache[object] = cacheEntry{}
			return kube.ArgoSyncStatus{}, false
		}
		for _, o := range objs {
			u, ok := o.(*unstructured.Unstructured)
			if !ok || u.GetName() != name {
				continue
			}
			ref, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "operation", "sync", "revision")
			rev, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "syncResult", "revision")
			username, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "initiatedBy", "username")
			automated, _, _ := unstructured.NestedBool(u.Object, "status", "operationState", "initiatedBy", "automated")
			// §34a: "was this a human or the machine? — matters at 2am."
			// Only Argo's own automated flag says machine; a sync with no
			// recorded username (a bare operation patch from another
			// tool) names nobody rather than crediting auto-sync. kute's
			// own S records operation.initiatedBy (kube.ArgoSyncPatch), so
			// its syncs read as the reviewed user, or "kute".
			by := username
			if automated {
				by = "auto-sync"
			}
			status := kube.ArgoSyncStatus{Ref: ref, Revision: rev, By: by}
			cache[object] = cacheEntry{status: status, found: true}
			return status, true
		}
		cache[object] = cacheEntry{}
		return kube.ArgoSyncStatus{}, false
	}
}
