// Backend resolution shared by projectIngress's list-view BACKENDS column and
// tasks/routetable's detail rows (docs/design README.md §23a/§23b): "backends
// resolve live from the watch — green service exists + ready endpoints; red
// service not found; yellow 0 ready." Deliberately reuses the already-watched
// Service/Pod informers via RawLister rather than a new EndpointSlice
// informer — a Service's own spec.selector matched against cached Pods gives
// the same ready/not-ready signal an EndpointSlice would, with zero new
// watched kinds.
package resources

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
)

// BackendState is one Service backend's resolved health.
type BackendState struct {
	// Exists is false when no Service by that name exists in the namespace.
	Exists bool
	// Ready/Total are matching-pod counts. A Service with an empty selector
	// (headless/ExternalName/manual Endpoints) can't be matched against pods
	// at all — Ready==Total==0 with Exists true is that case, kept distinct
	// from "0 replicas currently ready" only by callers that care (routetable
	// shows different copy for the two; the list's coarser OK/broken split
	// doesn't need to).
	Ready, Total int
	// Unresolvable is true for a Service with no selector — the ready count
	// can't be computed, so callers should treat this as "exists" rather than
	// "0 ready" (a headless Service backing an external name isn't broken).
	Unresolvable bool
}

// Glyph renders b as the shared ●/✕/▲ backend grammar (docs/design
// README.md §23a: "green ● service exists + ready endpoints; red ✕ service
// not found; yellow ▲ 0 ready").
func (b BackendState) Glyph() (glyph string, class StatusClass) {
	switch {
	case !b.Exists:
		return "✕", StatusFail
	case b.Unresolvable, b.Ready > 0:
		return "●", StatusOK
	default:
		return "▲", StatusWarn
	}
}

// ResolveServiceBackend looks up the named Service in namespace via lister,
// then counts how many of its selector-matching Pods are Ready. A nil lister
// or empty name (no cluster connection yet, or an Ingress rule with no
// Service backend) resolves to the zero BackendState (not Exists).
func ResolveServiceBackend(ctx context.Context, lister RawLister, namespace, name string) BackendState {
	return newBackendResolver(ctx, lister).resolve(namespace, name)
}

// backendResolver is ResolveServiceBackend with its reads shared: each
// namespace's Services and Pods are listed once, Services are indexed by
// name, and each Service's answer is memoized — so a batch projection (an
// Ingress list, many rules × paths, often naming the same Service) pays one
// Service list and one Pod scan per namespace and per distinct backend,
// instead of both per path.
type backendResolver struct {
	ctx      context.Context
	lister   RawLister
	services map[string]map[string]*corev1.Service // namespace → name; nil value = list failed
	pods     map[string][]runtime.Object           // namespace → pods; nil value = list failed
	memo     map[[2]string]BackendState
}

func newBackendResolver(ctx context.Context, lister RawLister) *backendResolver {
	return &backendResolver{
		ctx: ctx, lister: lister,
		services: map[string]map[string]*corev1.Service{},
		pods:     map[string][]runtime.Object{},
		memo:     map[[2]string]BackendState{},
	}
}

func (r *backendResolver) resolve(namespace, name string) BackendState {
	if r.lister == nil || name == "" {
		return BackendState{}
	}
	key := [2]string{namespace, name}
	if state, ok := r.memo[key]; ok {
		return state
	}
	state := r.resolveUncached(namespace, name)
	r.memo[key] = state
	return state
}

func (r *backendResolver) resolveUncached(namespace, name string) BackendState {
	byName, listed := r.services[namespace]
	if !listed {
		if objs, err := r.lister.ListRaw(r.ctx, kube.KindService, namespace); err == nil {
			byName = make(map[string]*corev1.Service, len(objs))
			for _, obj := range objs {
				if s, ok := obj.(*corev1.Service); ok {
					byName[s.Name] = s
				}
			}
		}
		r.services[namespace] = byName
	}
	if byName == nil {
		return BackendState{}
	}
	svc := byName[name]
	if svc == nil {
		return BackendState{Exists: false}
	}
	if len(svc.Spec.Selector) == 0 {
		return BackendState{Exists: true, Unresolvable: true}
	}

	podObjs, listed := r.pods[namespace]
	if !listed {
		if objs, err := r.lister.ListRaw(r.ctx, kube.KindPod, namespace); err == nil {
			podObjs = objs
			if podObjs == nil {
				podObjs = []runtime.Object{}
			}
		}
		r.pods[namespace] = podObjs
	}
	if podObjs == nil {
		return BackendState{Exists: true}
	}
	sel := labels.SelectorFromSet(svc.Spec.Selector)
	var ready, total int
	for _, obj := range podObjs {
		pod, ok := obj.(*corev1.Pod)
		if !ok || !sel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		total++
		if podReady(pod) {
			ready++
		}
	}
	return BackendState{Exists: true, Ready: ready, Total: total}
}

// podReady reports whether every one of pod's containers is Ready — the
// same ContainerStatuses-based readiness projectPod's own RDY column
// computes (projections.go), rather than Status.Conditions' PodReady entry:
// this repo's demo fixtures (and, in practice, kubelet's own container
// statuses) are the reliable signal either way.
func podReady(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}
