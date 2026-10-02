package poddetail

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
)

// load fetches the pod itself (found=false, not an error, when it's no
// longer in the cache — a watch delete) and best-effort fetches its events.
// An events failure never fails the whole load — the pod's own fields are
// what matters. Live usage for the CPU/MEM bars is deliberately not read
// here: load runs on every watch event, and metrics-server is polled on the
// sync interval only (loadMetrics).
func (m Model) load() tea.Cmd {
	lister := m.lister
	events := m.events
	namespace := m.namespace
	name := m.name
	timeout := m.timeout
	epoch := m.loadEpoch
	registry := m.registry()
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		objs, err := lister.ListRaw(ctx, kube.KindPod, namespace)
		if err != nil {
			return loadedMsg{epoch: epoch, err: err}
		}
		obj := findPod(objs, namespace, name)
		if obj == nil {
			return loadedMsg{epoch: epoch, found: false}
		}
		pod := kube.PodFromObject(obj)

		var eventRows []kube.Event
		var eventsErr error
		if events != nil {
			eventRows, eventsErr = events.ObjectEvents(ctx, namespace, kube.KindPod, name)
		}

		controller := resolveControllerDisplay(ctx, lister, namespace, pod.Owner)
		related := resolveRelatedItems(ctx, lister, namespace, pod.Labels, obj.Spec, controller)
		if flux, ok := resolveFluxItem(ctx, lister, registry, namespace, obj.Labels, controller); ok {
			// Right after the controller: the owner chain reads pod →
			// workload → whoever applied the workload.
			at := min(1, len(related))
			if _, _, hasController := splitOwner(controller); !hasController {
				at = 0
			}
			related = slices.Insert(related, at, flux)
			if len(related) > maxRelatedItems {
				related = related[:maxRelatedItems]
			}
		}

		return loadedMsg{
			epoch: epoch,
			pod:   pod, found: true, events: eventRows, eventsErr: eventsErr, controller: controller, related: related,
			sources: podContainerSources(obj.Spec), configErrors: podConfigErrors(obj.Status),
		}
	}
}

// pollInterval is how often the CPU/MEM bars refresh — the same cadence
// browse and nodedetail poll at, so opening a pod doesn't change how fresh
// the number is.
const pollInterval = 2 * time.Second

// loadMetrics reads the pod's live usage — one poll of the usage chain
// (metricsTickMsg). A failed or empty read reports ok=false, and the bars
// keep "–".
func (m Model) loadMetrics() tea.Cmd {
	metrics := m.metrics
	if metrics == nil {
		return nil
	}
	namespace, name := m.namespace, m.name
	epoch := m.metricsEpoch
	timeout := m.timeout
	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		msg := metricsLoadedMsg{epoch: epoch}
		if podMetrics, err := metrics.PodMetricsByNamespace(ctx, namespace); err == nil {
			msg.usage, msg.ok = podMetrics[kube.PodKey(namespace, name)]
		}
		return msg
	}
}

// scheduleMetricsTick arranges the next tick of an existing poll chain.
func (m Model) scheduleMetricsTick(epoch int) tea.Cmd {
	if !m.pollsMetrics() {
		return nil
	}
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return metricsTickMsg{epoch: epoch} })
}

// restartMetrics retires any running poll chain and reads usage now — on a
// sibling move (the usage on screen belongs to the pod just left), on
// restore from the stack (the chain's ticks went to whichever screen was
// active and never came back), and on reconnect (pollsMetrics let it lapse
// while offline).
func (m *Model) restartMetrics() tea.Cmd {
	m.metricsEpoch++
	if !m.pollsMetrics() {
		return nil
	}
	return m.loadMetrics()
}

// pollsMetrics reports whether a usage poll is worth making: a seam to read,
// and a reachable cluster — the same gate nodedetail.pollsMetrics applies.
func (m Model) pollsMetrics() bool {
	return m.metrics != nil && !m.conn.Offline()
}

// applyUsage overlays the last usage poll on the loaded pod, which load()
// replaces wholesale on every watch event.
func (m *Model) applyUsage() {
	if !m.usageOK {
		m.pod.CPU, m.pod.MEM, m.pod.CPUMilli, m.pod.MEMBytes = "", "", 0, 0
		return
	}
	m.pod.CPU, m.pod.MEM = m.usage.CPU, m.usage.MEM
	m.pod.CPUMilli, m.pod.MEMBytes = m.usage.CPUMilli, m.usage.MemBytes
}

// relatedItem is one RELATED entry (docs/design README.md §5a) — a
// numbered jump target resolved once here in load(), never in the render
// path (CLAUDE.md: render functions are pure, no I/O), so pressing its
// digit key (update.go's openRelated) can jump without a synchronous
// lookup. Label is the pre-formatted "Kind/name" text
// relatedTolerationsBlock renders.
type relatedItem struct {
	Kind kube.ResourceKind
	// Namespace is set only when the target lives outside the pod's own
	// namespace (a Flux reconciler in flux-system); "" means the pod's.
	Namespace string
	Name      string
	Label     string
}

// maxRelatedItems caps resolveRelatedItems' output. update.go's openRelated
// and the digit keys in updateKey ("1"..."9") are single ASCII characters,
// so this is a hard ceiling on how many numbered entries can ever be jumped
// to, not a style choice — extra candidates beyond it are simply dropped.
const maxRelatedItems = 9

// resolveRelatedItems resolves RELATED's numbered entries, in priority
// order: the resolved parent controller (whatever kube.PodFromObject's
// owner resolves to — Deployment/StatefulSet/DaemonSet/Job/an unresolved
// ReplicaSet — reusing controller's already-resolved ReplicaSet→Deployment
// hop; matches the CONTROLLER field in the meta grid, so if that field
// shows something, RELATED links to it too), the PersistentVolumeClaims
// this pod mounts that still exist, the Services whose selector matches
// this pod, every Ingress that references one of those Services, and then
// the Secrets and ConfigMaps the spec references (podConfigRefs — named from
// the spec, never checked against a cache; they come last so a pod with many
// of them can't push its workload/network links past the cap). Capped at
// maxRelatedItems.
func resolveRelatedItems(ctx context.Context, lister resources.RawLister, namespace string, podLabels map[string]string, spec corev1.PodSpec, controller string) []relatedItem {
	var items []relatedItem
	if kind, name, ok := splitOwner(controller); ok {
		items = append(items, relatedItem{Kind: kind, Name: name, Label: controller})
	}
	if lister != nil {
		items = append(items, resolvePVCItems(ctx, lister, namespace, spec.Volumes)...)
		services, ingresses := resolveServiceAndIngressItems(ctx, lister, namespace, podLabels)
		items = append(items, services...)
		items = append(items, ingresses...)
	}
	for _, ref := range podConfigRefs(spec) {
		items = append(items, relatedItem{Kind: ref.Kind, Name: ref.Name, Label: string(ref.Kind) + "/" + ref.Name})
	}
	if len(items) > maxRelatedItems {
		items = items[:maxRelatedItems]
	}
	return items
}

// resolvePVCItems resolves the PersistentVolumeClaim volumes mounted into
// the pod's spec (order follows spec.volumes), skipping any claim name that
// no longer exists in the cluster — "don't guess, don't dead-link" mirrors
// resolveServiceAndIngressItems' own discipline.
func resolvePVCItems(ctx context.Context, lister resources.RawLister, namespace string, volumes []corev1.Volume) []relatedItem {
	var claims []string
	seen := map[string]bool{}
	for _, v := range volumes {
		if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName == "" {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		if seen[name] {
			continue
		}
		seen[name] = true
		claims = append(claims, name)
	}
	if len(claims) == 0 {
		return nil
	}
	objs, err := lister.ListRaw(ctx, kube.KindPersistentVolumeClaim, namespace)
	if err != nil {
		return nil
	}
	existing := map[string]bool{}
	for _, obj := range objs {
		if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok {
			existing[pvc.Name] = true
		}
	}
	var items []relatedItem
	for _, name := range claims {
		if !existing[name] {
			continue
		}
		items = append(items, relatedItem{Kind: kube.KindPersistentVolumeClaim, Name: name, Label: string(kube.KindPersistentVolumeClaim) + "/" + name})
	}
	return items
}

// resolveServiceAndIngressItems resolves every Service in namespace whose
// label selector matches podLabels, and every Ingress that references one
// of those Services by name (default backend or a rule path) — RELATED's
// own numbered entries for both, not just an internal step toward Ingress
// discovery. Unlike the resolver this replaced, more than one matching
// Ingress is no longer treated as unresolvable: a numbered list has room
// for each one. Both results are sorted by name for deterministic output —
// ListRaw iterates an informer store, not a sorted list.
func resolveServiceAndIngressItems(ctx context.Context, lister resources.RawLister, namespace string, podLabels map[string]string) (services, ingresses []relatedItem) {
	svcObjs, err := lister.ListRaw(ctx, kube.KindService, namespace)
	if err != nil {
		return nil, nil
	}
	selector := labels.Set(podLabels)
	matched := map[string]bool{}
	for _, obj := range svcObjs {
		svc, ok := obj.(*corev1.Service)
		if !ok || len(svc.Spec.Selector) == 0 {
			continue
		}
		if labels.SelectorFromSet(svc.Spec.Selector).Matches(selector) {
			matched[svc.Name] = true
			services = append(services, relatedItem{Kind: kube.KindService, Name: svc.Name, Label: string(kube.KindService) + "/" + svc.Name})
		}
	}
	slices.SortFunc(services, func(a, b relatedItem) int { return cmp.Compare(a.Name, b.Name) })
	if len(matched) == 0 {
		return services, nil
	}

	ingObjs, err := lister.ListRaw(ctx, kube.KindIngress, namespace)
	if err != nil {
		return services, nil
	}
	for _, obj := range ingObjs {
		ing, ok := obj.(*networkingv1.Ingress)
		if !ok || !ingressReferencesServices(ing, matched) {
			continue
		}
		ingresses = append(ingresses, relatedItem{Kind: kube.KindIngress, Name: ing.Name, Label: string(kube.KindIngress) + "/" + ing.Name})
	}
	slices.SortFunc(ingresses, func(a, b relatedItem) int { return cmp.Compare(a.Name, b.Name) })
	return services, ingresses
}

// ingressReferencesServices reports whether ing's default backend or any
// rule's paths name one of services.
func ingressReferencesServices(ing *networkingv1.Ingress, services map[string]bool) bool {
	if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil && services[b.Service.Name] {
		return true
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service != nil && services[path.Backend.Service.Name] {
				return true
			}
		}
	}
	return false
}

// splitOwner parses an "Owner/name" string (kube.PodFromObject's ownerRef
// shape, e.g. "ReplicaSet/nva-worker-abc123") into its kind/name.
func splitOwner(owner string) (kube.ResourceKind, string, bool) {
	kind, name, found := strings.Cut(owner, "/")
	if !found || kind == "" || name == "" {
		return "", "", false
	}
	return kube.ResourceKind(kind), name, true
}

// ownerRef mirrors kube.PodFromObject's own unexported helper of the same
// name (pods.go), duplicated here since a ReplicaSet's OwnerReferences need
// the identical "Kind/Name" projection and that helper isn't exported.
func ownerRef(refs []metav1.OwnerReference) string {
	if len(refs) == 0 {
		return ""
	}
	return refs[0].Kind + "/" + refs[0].Name
}

// resolveControllerDisplay resolves owner (pod.Owner, "Kind/name") for 5a's
// CONTROLLER field: a ReplicaSet-owned pod resolves one hop further to its
// own owning Deployment (docs/design README.md §5a: "deploy/nva-worker ↗"),
// since a Deployment never appears as a pod's direct owner — every other
// owner kind (StatefulSet, DaemonSet, Job) already IS the display-worthy
// controller and passes through unchanged. Runs inside load()'s tea.Cmd, not
// the render path, so the extra ReplicaSet lookup is fine here (CLAUDE.md:
// render functions are pure, no I/O).
func resolveControllerDisplay(ctx context.Context, lister resources.RawLister, namespace, owner string) string {
	kind, name, ok := splitOwner(owner)
	if !ok || kind != kube.KindReplicaSet || lister == nil {
		return owner
	}
	objs, err := lister.ListRaw(ctx, kube.KindReplicaSet, namespace)
	if err != nil {
		return owner
	}
	for _, obj := range objs {
		rs, ok := obj.(*appsv1.ReplicaSet)
		if !ok || rs.Name != name {
			continue
		}
		if depOwner := ownerRef(rs.OwnerReferences); depOwner != "" {
			if depKind, _, ok := splitOwner(depOwner); ok && depKind == kube.KindDeployment {
				return depOwner
			}
		}
	}
	return owner
}

// resolveFluxItem finds the Flux reconciler that applied this pod — from the
// pod's own labels, else from its resolved controller's. Flux labels the
// objects it applies, not the pods a workload's controller creates later,
// so the controller is where the label usually is. Only the controller's
// own kind is read, and only on a cluster that serves the reconciler's kind:
// kute never links to a kind it can't open.
func resolveFluxItem(ctx context.Context, lister resources.RawLister, registry resources.Registry, namespace string, podLabels map[string]string, controller string) (relatedItem, bool) {
	kind, ns, name, ok := kube.FluxManager(podLabels)
	if !ok && lister != nil {
		if ck, cn, isOwner := splitOwner(controller); isOwner {
			if objs, err := lister.ListRaw(ctx, ck, namespace); err == nil {
				for _, o := range objs {
					if acc, err := meta.Accessor(o); err == nil && acc.GetName() == cn {
						kind, ns, name, ok = kube.FluxManager(acc.GetLabels())
						break
					}
				}
			}
		}
	}
	if !ok {
		return relatedItem{}, false
	}
	if desc, served := registry.Descriptor(kind); !served || !desc.Flux {
		return relatedItem{}, false
	}
	if ns == "" {
		ns = namespace
	}
	label := kind.APIKind() + "/" + name
	if ns != namespace {
		label += " in " + ns
	}
	return relatedItem{Kind: kind, Namespace: ns, Name: name, Label: label}, true
}

func findPod(objs []runtime.Object, namespace, name string) *corev1.Pod {
	for _, obj := range objs {
		if p, ok := obj.(*corev1.Pod); ok && p.Name == name && (namespace == "" || p.Namespace == namespace) {
			return p
		}
	}
	return nil
}

// statusClass mirrors resources.projectPod's classification (docs/design
// README.md §2a/§5a), but works from the already-projected kube.Pod (whose
// Reason field, from kube.PodFromObject, already carries the waiting/
// terminated reason) instead of a raw runtime.Object — the same reasoning
// nodedetail.podGlyph documents for working off kube.Pod.
func statusClass(p kube.Pod) (glyph, class, text string) {
	switch {
	case p.Deleting:
		return "◌", "warn", "Terminating"
	case p.Status == string(corev1.PodSucceeded):
		return "○", "neutral", "Completed"
	case p.Status == string(corev1.PodFailed):
		return "✕", "fail", p.Reason
	case strings.Contains(p.Reason, "CrashLoop"):
		return "✕", "fail", p.Reason
	case p.Status == string(corev1.PodPending):
		return "▲", "warn", p.Reason
	default:
		return "●", "ok", p.Reason
	}
}

func usageText(v string) string {
	if v == "" || v == "n/a" {
		return "–"
	}
	return v
}

// registry is the kind catalog RELATED's Flux entry checks the reconciler
// kind against.
func (m Model) registry() resources.Registry {
	if m.session == nil {
		return resources.DefaultRegistry()
	}
	return m.session.Registry
}
