// 25a's 'r' inline resources editor (docs/design README.md §25a): reversible
// outside PROD, so — like scale.go's pendingScale and setimage.go's
// pendingSetImage — this is a bespoke gate (pendingSetResources) rather than
// actions.Controller's y/N/type-name flow, since there's a per-field
// request/limit buffer to gather (plus a dry-run round-trip) before there's
// anything to Begin. Once ↵ commits and the dry-run succeeds, execution
// itself does go through actions.Controller/kube.Mutator
// (verbs.TierForSetResources decides TierNone outside PROD vs. TierInline in
// PROD — the same ordinary inline y/N Controller already renders for
// rollback/delete/set-image). Kept in its own file, browse's per-concern
// split convention (like scale.go/setimage.go).
package browse

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/tui/components/textfield"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Field indices into setResourcesTarget.fields — fixed order, matching the
// mockup's FIELD column (cpu request/limit, then mem request/limit).
const (
	fieldCPURequest = iota
	fieldCPULimit
	fieldMEMRequest
	fieldMEMLimit
)

// cpuNudgeStep/memNudgeStep are 25a's "+/− nudge (64Mi / 50m)" unit steps.
const (
	cpuNudgeStep = "50m"
	memNudgeStep = "64Mi"
)

// resourceField is one FIELD·CURRENT·NEW·P95 USAGE row.
type resourceField struct {
	label   string
	isCPU   bool // cpu (50m steps) vs. mem (64Mi steps)
	isLimit bool
	// hasCurrent is false when this resource key isn't set on the container
	// at all (current == "" in that case).
	hasCurrent bool
	current    string
	// input is the editable NEW value, pre-filled to current. Editing is
	// cursor-anchored throughout — no replace-on-first-keystroke gate — the
	// same continuous text-field model setImageTarget's own buffer uses
	// (scale.go's pendingScale is the one bespoke gate with a different,
	// whole-value-replaces model, since it has no cursor at all). Every
	// wholesale buffer replacement (prefill, nudge, unset) parks the cursor
	// at the end via setBuffer — the same convention setImageTarget.setBuffer
	// uses.
	input textfield.Model
	// unset is true after 'u' — an explicit removal (buffer is "" in this
	// state too, but unset renders "— none" in yellow rather than blocking
	// as an empty/invalid quantity would).
	unset bool
	// invalid is set by validate() when buffer fails to parse as a k8s
	// quantity, or this field is on the losing side of a request>limit
	// violation — renders underlined red, blocks ↵ (docs/design README.md
	// §25a: "same inline-error idiom as 17a").
	invalid bool
}

// changed reports whether f differs from its prefilled state — "only
// changed fields go into the command" (docs/design README.md §25a).
func (f resourceField) changed() bool {
	if f.unset {
		return f.hasCurrent // unsetting an already-unset field is a no-op
	}
	return f.input.Value() != f.current
}

// setBuffer replaces f.input's value wholesale and parks the cursor at its
// end — the shared tail of every place buffer changes as a whole rather than
// by a single keystroke (prefill, nudge, unset), the same convention
// setImageTarget.setBuffer uses.
func (f *resourceField) setBuffer(s string) {
	f.input.SetValue(s)
	f.input.CursorEnd()
}

// setResourcesTarget is the state pendingSetResources gates on while 25a's
// panel is showing.
type setResourcesTarget struct {
	kind      kube.ResourceKind
	namespace string
	name      string

	// obj is the raw workload object — re-read per container switch to
	// pull that container's own resources.requests/limits (workloadObject
	// is a single cache read at beginSetResources time; the object doesn't
	// change again while the panel is open).
	obj runtime.Object
	// pods are the workload's own live pods (workloadPods), the source for
	// both the per-container usage sample and the OOMKill callout.
	pods []*corev1.Pod
	// containerMetrics is one ContainerMetricsByNamespace snapshot taken at
	// beginSetResources time — namespace-scoped, re-sliced per container on
	// each tab switch rather than re-fetched.
	containerMetrics map[string]map[string]kube.PodMetrics

	containers   []kube.ContainerInfo
	containerIdx int
	desiredCount int32 // "applying rolls out N pods"

	fields   [4]resourceField
	fieldIdx int

	// cpuMilli/memBytes/usageOK are the active container's live usage
	// sample (summed across every live pod) — shared by both the request
	// and limit row of that resource type, since it's one real measurement
	// compared against two different denominators. usageOK is false when
	// browse has no metrics reader or no pods reported usage yet
	// ("metrics unavailable").
	cpuMilli, memBytes int64
	usageOK            bool

	// oomAge/oomOK back the strip's "✕ OOMKilled <age> ago" callout for the
	// active container — the most recent OOMKilled termination across the
	// workload's own pods.
	oomAge time.Duration
	oomOK  bool

	// dryRunErr is the most recent pre-flight dry-run's rejection message
	// (a LimitRange/ResourceQuota violation, verbatim from the API server) —
	// rendered in place of the will-run strip's "applying rolls out N pods"
	// note until the next edit, never a modal (docs/design README.md §25a:
	// "same idiom as 17a").
	dryRunErr string

	// pendingCommit identifies the real apply which follows a successful
	// dry-run. message/lastError are the panel-local result line. Keeping
	// all three on the target lets the editor follow the same
	// confirm -> execute -> refresh -> remain contract as set image and
	// metadata instead of closing as soon as the dry-run succeeds.
	pendingCommit *setResourcesPendingCommit
	message       string
	lastError     string
}

type setResourcesPendingCommit struct {
	container string
}

// activeContainer is the container the panel is currently editing.
func (t setResourcesTarget) activeContainer() kube.ContainerInfo {
	return t.containers[t.containerIdx]
}

// edits collects every changed field into a kube.ResourceEdits — "only
// changed fields go into the command" (docs/design README.md §25a).
func (t setResourcesTarget) edits() kube.ResourceEdits {
	var e kube.ResourceEdits
	assign := func(dst **string, f resourceField) {
		if !f.changed() {
			return
		}
		v := f.input.Value()
		if f.unset {
			v = ""
		}
		*dst = &v
	}
	assign(&e.CPURequest, t.fields[fieldCPURequest])
	assign(&e.CPULimit, t.fields[fieldCPULimit])
	assign(&e.MEMRequest, t.fields[fieldMEMRequest])
	assign(&e.MEMLimit, t.fields[fieldMEMLimit])
	return e
}

// resourceEditable reports whether kind takes 25a's resources prompt —
// Deployment, StatefulSet, and DaemonSet, the same three pod-template kinds
// 24a's set-image editor targets.
func resourceEditable(kind kube.ResourceKind) bool {
	return kind == kube.KindDeployment || kind == kube.KindStatefulSet || kind == kube.KindDaemonSet
}

// beginSetResources opens 25a's panel for the selected row. ok is false
// when nothing applies — mirrors beginSetImage's ok-bool contract.
func (m *Model) beginSetResources() bool {
	if !resourceEditable(m.kind) || m.mutator == nil || m.state != tui.TaskStateReady {
		return false
	}
	row, ok := m.selectedRow()
	if !ok {
		return false
	}
	t, ok := m.buildSetResourcesTarget(m.kind, row.Namespace, row.Name, currentReplicas(row))
	if !ok {
		return false
	}
	m.pendingSetResources = t
	m.selectSetResourcesContainer(0)
	return true
}

func (m *Model) buildSetResourcesTarget(kind kube.ResourceKind, namespace, name string, desiredCount int32) (*setResourcesTarget, bool) {
	obj, ok := workloadObject(m.session.ClusterContext(), m.lister, kind, namespace, name)
	if !ok {
		return nil, false
	}
	containers := workloadContainerInfos(obj)
	if len(containers) == 0 {
		return nil, false
	}
	return &setResourcesTarget{
		kind: kind, namespace: namespace, name: name,
		obj: obj, containers: containers, desiredCount: desiredCount,
		pods:             workloadPods(m.session.ClusterContext(), m.lister, kind, namespace, name),
		containerMetrics: m.containerMetricsSnapshot(namespace),
	}, true
}

// containerMetricsSnapshot reads one ContainerMetricsByNamespace poll — nil
// when browse has no metrics reader wired or the read fails, in which case
// every field renders "metrics unavailable" rather than blocking the editor
// (docs/design README.md §25a: "No metrics → USAGE column reads 'metrics
// unavailable' dim, editor still works").
func (m *Model) containerMetricsSnapshot(namespace string) map[string]map[string]kube.PodMetrics {
	if m.metrics == nil {
		return nil
	}
	metrics, err := m.metrics.ContainerMetricsByNamespace(m.session.ClusterContext(), namespace)
	if err != nil {
		return nil
	}
	return metrics
}

// selectSetResourcesContainer switches the panel's active container tab
// (beginSetResources' initial 0, or 'tab' cycling), recomputing the four
// fields/usage/OOMKill fact for the newly active container.
func (m *Model) selectSetResourcesContainer(idx int) {
	t := m.pendingSetResources
	t.containerIdx = idx
	c := t.activeContainer()
	t.fields = newResourceFields(workloadContainerResources(t.obj, c.Name), m.Theme())
	t.fieldIdx = 0
	t.fields[0].input.Focus()
	t.cpuMilli, t.memBytes, t.usageOK = containerUsage(t.containerMetrics, t.pods, c.Name)
	t.oomAge, t.oomOK = containerOOMFact(t.pods, c.Name)
	t.dryRunErr = ""
}

// newResourceFields builds the four FIELD rows from container's own raw
// resources.requests/limits.
func newResourceFields(res corev1.ResourceRequirements, theme tui.Theme) [4]resourceField {
	styles := tui.TextInputStyles(theme)
	field := func(label string, isCPU, isLimit bool, list corev1.ResourceList, name corev1.ResourceName) resourceField {
		q, has := list[name]
		current := ""
		if has {
			current = q.String()
		}
		f := resourceField{label: label, isCPU: isCPU, isLimit: isLimit, hasCurrent: has, current: current}
		f.input = textfield.New()
		f.input.Prompt = ""
		f.input.SetStyles(styles)
		f.setBuffer(current)
		return f
	}
	var out [4]resourceField
	out[fieldCPURequest] = field("cpu request", true, false, res.Requests, corev1.ResourceCPU)
	out[fieldCPULimit] = field("cpu limit", true, true, res.Limits, corev1.ResourceCPU)
	out[fieldMEMRequest] = field("mem request", false, false, res.Requests, corev1.ResourceMemory)
	out[fieldMEMLimit] = field("mem limit", false, true, res.Limits, corev1.ResourceMemory)
	return out
}

// workloadContainerResources returns container's own corev1.
// ResourceRequirements straight off obj's raw pod template — separate from
// setimage.go's workloadContainerInfos (name/image/sidecar only, since 24a
// never needed the resources field).
func workloadContainerResources(obj runtime.Object, container string) corev1.ResourceRequirements {
	var spec corev1.PodSpec
	switch o := obj.(type) {
	case *appsv1.Deployment:
		spec = o.Spec.Template.Spec
	case *appsv1.StatefulSet:
		spec = o.Spec.Template.Spec
	case *appsv1.DaemonSet:
		spec = o.Spec.Template.Spec
	default:
		return corev1.ResourceRequirements{}
	}
	for _, c := range spec.Containers {
		if c.Name == container {
			return c.Resources
		}
	}
	for _, c := range spec.InitContainers {
		if c.Name == container {
			return c.Resources
		}
	}
	return corev1.ResourceRequirements{}
}

// workloadPods finds kind/name's own live pods from the watch cache — the
// OOMKill callout and per-container usage need the workload's actual pods,
// not its pod template. Deployment → owned ReplicaSets → pods owned by
// those ReplicaSets (the same owner-matching setimage.go's
// deploymentRevisions already does for its own revision history);
// StatefulSet/DaemonSet → pods owned directly.
func workloadPods(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name string) []*corev1.Pod {
	if lister == nil {
		return nil
	}
	podObjs, err := lister.ListRaw(ctx, kube.KindPod, namespace)
	if err != nil {
		return nil
	}
	ownerKind, ownerNames := ownerSelector(ctx, lister, kind, namespace, name)
	if ownerKind == "" {
		return nil
	}
	var out []*corev1.Pod
	for _, obj := range podObjs {
		pod, ok := obj.(*corev1.Pod)
		if !ok || len(pod.OwnerReferences) == 0 {
			continue
		}
		ref := pod.OwnerReferences[0]
		if ref.Kind == ownerKind && ownerNames[ref.Name] {
			out = append(out, pod)
		}
	}
	return out
}

// ownerSelector resolves the pod-owner kind/name-set a workload's own pods
// carry in their OwnerReferences: a Deployment's pods are owned by its
// ReplicaSets (plural — old revisions can still have terminating pods), a
// StatefulSet/DaemonSet's pods are owned by it directly.
func ownerSelector(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name string) (ownerKind string, names map[string]bool) {
	switch kind {
	case kube.KindDeployment:
		return "ReplicaSet", ownedReplicaSetNames(ctx, lister, namespace, name)
	case kube.KindStatefulSet, kube.KindDaemonSet:
		return string(kind), map[string]bool{name: true}
	default:
		return "", nil
	}
}

// ownedReplicaSetNames finds every ReplicaSet owned by deployment — the
// same owner-reference match setimage.go's deploymentRevisions uses,
// reduced to just names since workloadPods only needs the set to filter
// pods by.
func ownedReplicaSetNames(ctx context.Context, lister resources.RawLister, namespace, deployment string) map[string]bool {
	objs, err := lister.ListRaw(ctx, kube.KindReplicaSet, namespace)
	if err != nil {
		return nil
	}
	names := map[string]bool{}
	for _, obj := range objs {
		rs, ok := obj.(*appsv1.ReplicaSet)
		if !ok || len(rs.OwnerReferences) == 0 || rs.OwnerReferences[0].Kind != "Deployment" || rs.OwnerReferences[0].Name != deployment {
			continue
		}
		names[rs.Name] = true
	}
	return names
}

// containerOOMFact scans pods for the most recent OOMKilled termination of
// container. This filters by container explicitly (kube.PodFromObject's own
// LastTermination picks a pod's single most-recent termination across every
// container, which isn't precise enough once a panel is scoped to one
// container tab), so it reads corev1.ContainerStatus directly rather than
// going through that projection.
func containerOOMFact(pods []*corev1.Pod, container string) (age time.Duration, ok bool) {
	var best time.Time
	consider := func(t *corev1.ContainerStateTerminated) {
		if t == nil || t.Reason != "OOMKilled" {
			return
		}
		if !ok || t.FinishedAt.After(best) {
			best, ok = t.FinishedAt.Time, true
		}
	}
	for _, pod := range pods {
		for _, s := range pod.Status.ContainerStatuses {
			if s.Name != container {
				continue
			}
			consider(s.State.Terminated)
			consider(s.LastTerminationState.Terminated)
		}
	}
	if !ok {
		return 0, false
	}
	return time.Since(best), true
}

// containerUsage sums container's live usage across every one of pods, from
// browse's own ContainerMetricsByNamespace poll (metrics, keyed by
// kube.PodKey(namespace, name) then container name — never bare pod name,
// which both providers' keys never match and which, cluster-wide, would
// collide same-named pods in different namespaces) — the one real measurement both the request and limit
// USAGE cell of that resource type compare against. ok is false when no pod
// reported a sample yet (metrics not configured, or the informer hasn't
// caught up).
func containerUsage(metrics map[string]map[string]kube.PodMetrics, pods []*corev1.Pod, container string) (cpuMilli, memBytes int64, ok bool) {
	for _, pod := range pods {
		byContainer, exists := metrics[kube.PodKey(pod.Namespace, pod.Name)]
		if !exists {
			continue
		}
		pm, exists := byContainer[container]
		if !exists {
			continue
		}
		cpuMilli += pm.CPUMilli
		memBytes += pm.MemBytes
		ok = true
	}
	return cpuMilli, memBytes, ok
}

// setResourcesPasteTarget is the selected field's quantity buffer (one of the
// four request/limit fields), clearing that field's unset/invalid flags after
// insertion the same way the typed path does.
func (m *Model) setResourcesPasteTarget() tui.PasteTarget {
	f := &m.pendingSetResources.fields[m.pendingSetResources.fieldIdx]
	insert := tui.PasteInto(&f.input)
	return func(s string) {
		before := f.input.Value()
		insert(s)
		if f.input.Value() != before {
			f.unset, f.invalid = false, false
		}
	}
}

// updateSetResourcesKey routes keys while pendingSetResources's panel is
// showing.
func (m *Model) updateSetResourcesKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	t := m.pendingSetResources
	t.dryRunErr, t.message, t.lastError = "", "", "" // any further interaction clears the last result
	switch msg.String() {
	case "esc":
		m.pendingSetResources = nil
	case "enter":
		if !t.validate() {
			return m, nil
		}
		return m, m.commitSetResources(*t)
	case "up":
		if t.fieldIdx > 0 {
			t.fields[t.fieldIdx].input.Blur()
			t.fieldIdx--
			t.fields[t.fieldIdx].input.Focus()
		}
	case "down":
		if t.fieldIdx < len(t.fields)-1 {
			t.fields[t.fieldIdx].input.Blur()
			t.fieldIdx++
			t.fields[t.fieldIdx].input.Focus()
		}
	case "tab":
		if len(t.containers) > 1 {
			m.selectSetResourcesContainer((t.containerIdx + 1) % len(t.containers))
		}
	case "+":
		t.nudge(1)
	case "-":
		t.nudge(-1)
	case "u":
		f := &t.fields[t.fieldIdx]
		f.setBuffer("")
		f.unset, f.invalid = true, false
	default:
		f := &t.fields[t.fieldIdx]
		before := f.input.Value()
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(msg)
		if f.input.Value() != before {
			f.unset, f.invalid = false, false
		}
		return m, cmd
	}
	return m, nil
}

// nudge adjusts the selected field's buffer by one unit step (cpuNudgeStep/
// memNudgeStep), clamped at zero — §25a: "+/− nudges by unit steps (64Mi /
// 50m)". An unparseable buffer nudges from zero rather than blocking, so a
// bad quantity is always recoverable via +/− as well as backspace.
func (t *setResourcesTarget) nudge(sign int) {
	f := &t.fields[t.fieldIdx]
	stepStr := cpuNudgeStep
	if !f.isCPU {
		stepStr = memNudgeStep
	}
	step := resource.MustParse(stepStr)
	cur, err := resource.ParseQuantity(f.input.Value())
	if err != nil {
		cur = resource.MustParse("0")
	}
	if sign > 0 {
		cur.Add(step)
	} else {
		cur.Sub(step)
		if cur.Sign() < 0 {
			cur = resource.MustParse("0")
		}
	}
	f.setBuffer(cur.String())
	f.unset, f.invalid = false, false
}

// validate checks every touched field parses as a k8s quantity and that
// neither resource type's request exceeds its limit, marking the offending
// fields' invalid flag (underlined red, docs/design README.md §25a: "same
// inline-error idiom as 17a") rather than a popup. Returns false when ↵
// should be blocked.
func (t *setResourcesTarget) validate() bool {
	ok := true
	for i := range t.fields {
		f := &t.fields[i]
		f.invalid = false
		if f.unset || !f.changed() {
			continue
		}
		if _, err := resource.ParseQuantity(f.input.Value()); err != nil {
			f.invalid = true
			ok = false
		}
	}
	if !ok {
		return false
	}
	if !t.checkRequestLimit(true) {
		ok = false
	}
	if !t.checkRequestLimit(false) {
		ok = false
	}
	return ok
}

// checkRequestLimit blocks apply when isCPU's request exceeds its limit,
// comparing each field's effective value (its edited buffer, since an
// untouched field's buffer is already prefilled to current) — skipped
// entirely when either side is unset or has no value at all, since there's
// no bound to violate in that case.
func (t *setResourcesTarget) checkRequestLimit(isCPU bool) bool {
	reqIdx, limIdx := fieldCPURequest, fieldCPULimit
	if !isCPU {
		reqIdx, limIdx = fieldMEMRequest, fieldMEMLimit
	}
	req, limit := &t.fields[reqIdx], &t.fields[limIdx]
	if req.unset || limit.unset || req.input.Value() == "" || limit.input.Value() == "" {
		return true
	}
	reqQ, err1 := resource.ParseQuantity(req.input.Value())
	limQ, err2 := resource.ParseQuantity(limit.input.Value())
	if err1 != nil || err2 != nil {
		return true // already flagged invalid by validate()'s parse pass
	}
	if reqQ.Cmp(limQ) > 0 {
		req.invalid, limit.invalid = true, true
		return false
	}
	return true
}

// dryRunSetResourcesMsg is commitSetResources' pre-flight result: a
// LimitRange/ResourceQuota rejection surfaces inline (target.dryRunErr) and
// keeps the panel open rather than closing it; success proceeds through
// actions.Controller for the real apply.
type dryRunSetResourcesMsg struct {
	target setResourcesTarget
	err    error
}

// commitSetResources runs a dry-run SetResources first — 25a's own
// validation (quantity parsing, request>limit) is entirely client-side, but
// a namespace LimitRange/ResourceQuota violation only surfaces from the API
// server itself (docs/design README.md §25a: "surface as the server's
// verbatim dry-run message, same idiom as 17a").
func (m *Model) commitSetResources(t setResourcesTarget) tea.Cmd {
	mutator := m.mutator
	kind, namespace, name := t.kind, t.namespace, t.name
	container := t.activeContainer().Name
	edits := t.edits()
	return func() tea.Msg {
		err := mutator.SetResources(context.Background(), kind, namespace, name, container, edits, true)
		return dryRunSetResourcesMsg{target: t, err: err}
	}
}

// handleDryRunSetResources routes a commitSetResources dry-run result: a
// rejection stays in the panel (dryRunErr rendered by the will-run strip);
// success keeps the panel open and hands off to actions.Controller/kube.Mutator
// for the real apply, TierNone outside PROD (executes immediately, mirroring
// commitSetImage) or TierInline in PROD (the ordinary inline y/N Controller
// already renders for rollback/delete/set-image).
func (m *Model) handleDryRunSetResources(msg dryRunSetResourcesMsg) (tea.Model, tea.Cmd) {
	if m.pendingSetResources == nil {
		return m, nil // panel was cancelled before the dry-run returned
	}
	if msg.err != nil {
		m.pendingSetResources.dryRunErr = msg.err.Error()
		return m, nil
	}
	edits := msg.target.edits()
	m.pendingSetResources.pendingCommit = &setResourcesPendingCommit{container: msg.target.activeContainer().Name}
	return m, m.actions.Begin(verbs.TierForSetResources(m.isProd()), tui.TaskAction{
		ID:    "set-resources-" + msg.target.namespace + "/" + msg.target.name,
		Label: fmt.Sprintf("Set resources for %s?", msg.target.name),
		Scope: tui.TaskScope{
			ResourceKind: string(msg.target.kind),
			ResourceName: msg.target.name,
			Namespace:    msg.target.namespace,
			Verb:         "set-resources",
			IsMutating:   true,
			Container:    msg.target.activeContainer().Name,
			Resources:    &edits,
		},
	})
}

// handleSetResourcesResult refreshes the still-open editor from the cache
// after a successful apply, or preserves the attempted fields and surfaces
// the server error after a failure. Only esc closes the panel.
func (m *Model) handleSetResourcesResult(msg actions.ResultMsg) tea.Cmd {
	t := m.pendingSetResources
	pc := t.pendingCommit
	t.pendingCommit = nil
	if msg.Err != nil {
		t.lastError = msg.Err.Error()
		t.message = ""
		return nil
	}

	container := ""
	if pc != nil {
		container = pc.container
	}
	fresh, ok := m.buildSetResourcesTarget(t.kind, t.namespace, t.name, t.desiredCount)
	if !ok {
		m.pendingSetResources = nil
		return nil
	}
	m.pendingSetResources = fresh
	idx := 0
	for i, c := range fresh.containers {
		if c.Name == container {
			idx = i
			break
		}
	}
	m.selectSetResourcesContainer(idx)
	m.pendingSetResources.message = "set resources: " + container
	return nil
}

func (m *Model) refreshSetResourcesTarget() {
	t := m.pendingSetResources
	container, message, lastError := t.activeContainer().Name, t.message, t.lastError
	fresh, ok := m.buildSetResourcesTarget(t.kind, t.namespace, t.name, t.desiredCount)
	if !ok {
		return
	}
	m.pendingSetResources = fresh
	idx := 0
	for i, c := range fresh.containers {
		if c.Name == container {
			idx = i
			break
		}
	}
	m.selectSetResourcesContainer(idx)
	m.pendingSetResources.message = message
	m.pendingSetResources.lastError = lastError
}

// setResourcesWillRunLine renders the exact "will run: kubectl set
// resources ..." line for a pending TierInline (PROD) confirmation's keybar
// RightNote — same idiom as setImageWillRunLine, reading straight off the
// already-resolved actions.Controller Scope.
func setResourcesWillRunLine(scope tui.TaskScope) string {
	return "will run: " + kube.SetResourcesCommandString(kube.ResourceKind(scope.ResourceKind), scope.Namespace, scope.ResourceName, scope.Container, *scope.Resources)
}
