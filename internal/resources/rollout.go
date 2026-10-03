// Rollout settlement shared by the kinds that roll (Deployment, StatefulSet,
// DaemonSet). projectDeployment already derives 9a's ROLLOUT cell from it;
// this file adds the cross-kind, whole-namespace answer 18a's Helm list needs
// to say whether a release's own workloads are still moving.
package resources

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// rollingKinds are the kinds UnsettledWorkloads reads. Three named kinds, not
// a sweep of the registry: the lazy-informer rule forbids reading caches
// breadth-first, and these are the only kinds with a rollout to observe.
var rollingKinds = []kube.ResourceKind{kube.KindDeployment, kube.KindStatefulSet, kube.KindDaemonSet}

// UnsettledWorkloads returns every workload in namespace ("" for all) that
// has not finished rolling out — one list per rolling kind, and a set of only
// the ones that are moving, which on a healthy cluster is empty.
//
// Shaped as "give me the unsettled ones" rather than "is this one settled?"
// on purpose: the 18a caller has N releases each naming several workloads,
// and asking per workload would re-list the same three caches N times.
//
// Reading these three caches is what starts their informers, so this is only
// called from a screen that has already decided to pay for it (the Helm
// list, via app's lister decorator). A read that fails contributes nothing
// and is not an error: an unavailable cache means "nothing known to be
// pending", never a fabricated rollout.
func UnsettledWorkloads(ctx context.Context, lister RawLister, namespace string) map[kube.WorkloadRef]struct{} {
	out := map[kube.WorkloadRef]struct{}{}
	if lister == nil {
		return out
	}
	for _, kind := range rollingKinds {
		objs, err := lister.ListRaw(ctx, kind, namespace)
		if err != nil {
			continue
		}
		for _, obj := range objs {
			ref, settled, ok := workloadSettled(obj)
			if !ok || settled {
				continue
			}
			out[ref] = struct{}{}
		}
	}
	return out
}

// workloadSettled reports whether one workload object has finished rolling
// out. ok is false for anything that isn't one of the three rolling kinds.
//
// "Settled" is deliberately the same question for all three — every replica
// the spec asks for exists, is updated to the current revision, and is
// ready — rather than three subtly different ones. Deployments defer to
// DeploymentRollout so the Helm list and 9a's ROLLOUT cell can never disagree
// about the same object.
func workloadSettled(obj runtime.Object) (ref kube.WorkloadRef, settled, ok bool) {
	switch w := obj.(type) {
	case *appsv1.Deployment:
		ref = kube.WorkloadRef{Kind: kube.KindDeployment, Namespace: w.Namespace, Name: w.Name}
		_, class := DeploymentRollout(w)
		return ref, class == StatusOK, true
	case *appsv1.StatefulSet:
		ref = kube.WorkloadRef{Kind: kube.KindStatefulSet, Namespace: w.Namespace, Name: w.Name}
		if w.Generation > w.Status.ObservedGeneration {
			return ref, false, true
		}
		want := int32ptr(w.Spec.Replicas)
		return ref, w.Status.UpdatedReplicas >= statefulSetUpdateTarget(w, want) && w.Status.ReadyReplicas >= want, true
	case *appsv1.DaemonSet:
		ref = kube.WorkloadRef{Kind: kube.KindDaemonSet, Namespace: w.Namespace, Name: w.Name}
		if w.Generation > w.Status.ObservedGeneration {
			return ref, false, true
		}
		want := w.Status.DesiredNumberScheduled
		updated := w.Status.UpdatedNumberScheduled >= want
		if w.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
			// The controller never replaces a pod on its own: nothing is
			// rolling once it has observed the spec.
			updated = true
		}
		return ref, updated && w.Status.NumberReady >= want, true
	}
	return kube.WorkloadRef{}, false, false
}

// statefulSetUpdateTarget is how many of want replicas the controller will
// itself move to the current revision — what `kubectl rollout status`
// waits for. OnDelete moves none (pods update only when someone deletes
// them), and a RollingUpdate partition holds every ordinal below it back.
func statefulSetUpdateTarget(w *appsv1.StatefulSet, want int32) int32 {
	strategy := w.Spec.UpdateStrategy
	if strategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return 0
	}
	if strategy.RollingUpdate != nil && strategy.RollingUpdate.Partition != nil {
		return max(want-*strategy.RollingUpdate.Partition, 0)
	}
	return want
}
