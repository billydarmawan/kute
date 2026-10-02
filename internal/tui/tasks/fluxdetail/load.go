package fluxdetail

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
)

// load reads the object, its chain and its inventory in one pass.
//
// Every read is cache-scoped and *narrow*: only the kinds the inventory
// actually names are listed, never a walk of the registry. A breadth-first
// read here would start an informer per registered kind from the update
// loop, which is the failure CLAUDE.md's lazy-informer rule exists to
// prevent.
func (m Model) load() tea.Cmd {
	lister := m.lister
	kind, namespace, name := m.kind, m.namespace, m.name
	desc := m.desc
	timeout := m.timeout
	var reg resources.Registry
	if m.session != nil {
		reg = m.session.Registry
	}

	parent := m.session.ClusterContext()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()

		objs, err := lister.ListRaw(ctx, kind, namespace)
		if err != nil {
			return loadedMsg{err: err}
		}
		obj, ok := findObject(objs, name)
		if !ok {
			// "No longer exists" is a claim about the cluster (CLAUDE.md's
			// empty-state rule). This screen is often the first reader of
			// its kind — the pod-detail reconciler link, a scoped session's
			// fresh per-namespace cache — so a cold cache is the common
			// reason the object isn't there yet, and a cache that can't fill
			// says why rather than that the object is gone.
			if !tui.KindsSynced(lister, namespace, kind) {
				return loadedMsg{pending: true}
			}
			if err := tui.KindsError(lister, namespace, kind); err != nil {
				return loadedMsg{err: err}
			}
			return loadedMsg{gone: true}
		}

		row := resources.Row{}
		if desc.Project != nil {
			row = desc.Project(obj)
		}

		chn := buildChain(ctx, lister, kind, obj, namespace)
		inv, invPending := buildInventory(ctx, lister, reg, obj, &chn)
		fail := buildFailure(obj, inv, time.Now())

		return loadedMsg{
			fail:       fail,
			chn:        chn,
			inventory:  inv,
			suspended:  row.Suspended,
			statusText: row.StatusText,
			// The source's revision comes from a cache too; an unfilled one
			// reads as "no revision", which the drift verdict would trust.
			invPending: invPending || (chn.SourceKind != "" && !tui.KindsSynced(lister, chn.SourceNamespace, chn.SourceKind)),
		}
	}
}

func findObject(objs []runtime.Object, name string) (*unstructured.Unstructured, bool) {
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetName() == name {
			return u, true
		}
	}
	return nil, false
}

// buildChain resolves §31a's middle band. The source's own artifact
// revision is read from the source object so "applied vs available" is a
// real comparison rather than a restatement of one field.
func buildChain(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, u *unstructured.Unstructured, namespace string) chain {
	c := chain{
		Path:            nested(u, "spec", "path"),
		Interval:        nested(u, "spec", "interval"),
		AppliedRevision: appliedRevision(u),
		DriftComparable: kube.FluxTracksSourceRevision(u.GetKind()),
	}

	// A ref with no namespace of its own defaults to the object's — Flux's
	// own rule. A HelmRelease in an app namespace pointing at a
	// HelmRepository in flux-system is the common layout, not the exception.
	for _, base := range [][]string{
		{"spec", "sourceRef"},
		{"spec", "chart", "spec", "sourceRef"},
		{"spec", "chartRef"}, // HelmRelease installing an OCIRepository/HelmChart directly
	} {
		k := nested(u, append(append([]string{}, base...), "kind")...)
		n := nested(u, append(append([]string{}, base...), "name")...)
		if n == "" {
			continue
		}
		ns := nested(u, append(append([]string{}, base...), "namespace")...)
		if ns == "" {
			ns = namespace
		}
		c.SourceKind, c.SourceName, c.SourceNamespace = kube.ResourceKind(k), n, ns
		c.SourceDisplay = shortSourceKind(k) + "/" + n
		if ns != namespace {
			c.SourceDisplay += " in " + ns
		}
		break
	}

	// The source object's artifact revision — the "available" half of the
	// drift comparison. Read only when there is a source to read.
	if c.SourceKind != "" && c.SourceName != "" {
		if srcObjs, err := lister.ListRaw(ctx, c.SourceKind, c.SourceNamespace); err == nil {
			if src, ok := findObject(srcObjs, c.SourceName); ok {
				c.SourceRevision = kube.ShortFluxRevision(nested(src, "status", "artifact", "revision"))
			}
		}
	}

	deps, _, _ := unstructured.NestedSlice(u.Object, "spec", "dependsOn")
	for _, d := range deps {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		dn, _, _ := unstructured.NestedString(dm, "name")
		if dn == "" {
			continue
		}
		// Flux allows a cross-namespace dependency; an entry without a
		// namespace of its own defaults to the object's, as a sourceRef does.
		dns, _, _ := unstructured.NestedString(dm, "namespace")
		if dns == "" {
			dns = namespace
		}
		c.DependsOn = append(c.DependsOn, dependency{
			Name: dn, Namespace: dns, CrossNamespace: dns != namespace,
			Suspended: dependencySuspended(ctx, lister, kind, dns, dn),
		})
	}
	return c
}

// dependencySuspended reports whether a spec.dependsOn entry is itself
// suspended — §31a surfaces it because a suspended dependency is a common
// and otherwise invisible reason a reconciler never runs.
//
// kind is the object's own registry kind, never its bare API Kind: a Flux
// HelmRelease's Kind names §18a's synthetic Helm kind, whose read starts a
// release-Secret informer and returns objects findObject can never match.
func dependencySuspended(ctx context.Context, lister resources.RawLister, kind kube.ResourceKind, namespace, name string) bool {
	objs, err := lister.ListRaw(ctx, kind, namespace)
	if err != nil {
		return false
	}
	dep, ok := findObject(objs, name)
	if !ok {
		return false
	}
	v, _, _ := unstructured.NestedBool(dep.Object, "spec", "suspend")
	return v
}

// appliedRevision is what this reconciler last put on the cluster, across
// the shapes Flux uses for it.
func appliedRevision(u *unstructured.Unstructured) string {
	for _, path := range [][]string{
		{"status", "lastAppliedRevision"},
		{"status", "lastAttemptedRevision"},
	} {
		if v := nested(u, path...); v != "" {
			return kube.ShortFluxRevision(v)
		}
	}
	hist, _, _ := unstructured.NestedSlice(u.Object, "status", "history")
	if len(hist) > 0 {
		if hm, ok := hist[0].(map[string]any); ok {
			v, _, _ := unstructured.NestedString(hm, "chartVersion")
			return v
		}
	}
	return ""
}

// buildInventory resolves status.inventory into rows with live readiness.
//
// Entry ids are "<namespace>_<name>_<group>_<Kind>" with the version in a
// sibling field; a cluster-scoped entry has an empty first segment.
//
// Each entry's (group, Kind) is resolved to its registry key before
// anything else (inventoryKind) — the bare Kind names a different resource
// whenever two groups serve it, and read that way a Flux HelmRelease entry
// listed §18a's Helm releases cluster-wide and a Knative Service entry
// resolved against core Services.
//
// Reads are narrow, because the app pays for the screen you're on: only a
// kind objectReadiness can actually answer for is read at all — a Secret
// or ConfigMap entry has no readiness, so listing it would start that
// kind's informer for a "–" (and an inventory full of SOPS Secrets is the
// norm) — and each such kind is read once per namespace its own entries
// name, never cluster-wide.
//
// pending reports that one of those reads came from a cache still filling,
// so an unresolved row is "not read yet" rather than "no reading".
func buildInventory(ctx context.Context, lister resources.RawLister, reg resources.Registry, u *unstructured.Unstructured, c *chain) (_ []inventoryItem, pending bool) {
	entries, _, _ := unstructured.NestedSlice(u.Object, "status", "inventory", "entries")
	if len(entries) == 0 {
		if strings.EqualFold(u.GetKind(), "HelmRelease") {
			// A HelmRelease delegates storage to Helm itself and publishes
			// no inventory. Saying so beats rendering an empty list that
			// reads as "this manages nothing".
			c.InventoryMissing = "inventory not published by this kind — Helm owns the release's object list"
		}
		return nil, false
	}

	// scope is one read: a kind in one namespace ("" for a cluster-scoped
	// entry, whose kind ignores the namespace anyway).
	type scope struct {
		kind      kube.ResourceKind
		namespace string
	}
	var (
		out    []inventoryItem
		reads  []scope
		toRead = map[scope]bool{}
	)
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		id, _, _ := unstructured.NestedString(em, "id")
		ns, name, group, apiKind, ok := kube.ParseFluxInventoryID(id)
		if !ok {
			continue
		}
		kind, readable := inventoryKind(reg, group, apiKind)
		out = append(out, inventoryItem{Kind: kind, Namespace: ns, Name: name, Ready: "–", Status: resources.StatusNeutral})
		if sc := (scope{kind, ns}); readable && !toRead[sc] {
			toRead[sc] = true
			reads = append(reads, sc)
		}
	}

	type key struct {
		kind            kube.ResourceKind
		namespace, name string
	}
	found := map[key]runtime.Object{}
	for _, sc := range reads {
		objs, err := lister.ListRaw(ctx, sc.kind, sc.namespace)
		if !tui.KindsSynced(lister, sc.namespace, sc.kind) {
			pending = true
		}
		if err != nil {
			continue
		}
		for _, o := range objs {
			if n, ns, ok := metaNameNamespace(o); ok {
				found[key{sc.kind, ns, n}] = o
			}
		}
	}
	for i := range out {
		it := &out[i]
		if o, ok := found[key{it.Kind, it.Namespace, it.Name}]; ok {
			it.Ready, it.Status = objectReadiness(o)
		}
		it.Glyph = inventoryGlyph(it.Status)
	}
	sortInventory(out)
	return out, pending
}

// readableTypedKinds are the built-in kinds objectReadiness has a reading
// for. Every other built-in answers "–", so reading one would be pure cost.
var readableTypedKinds = map[kube.ResourceKind]bool{
	kube.KindDeployment:  true,
	kube.KindStatefulSet: true,
	kube.KindDaemonSet:   true,
	kube.KindPod:         true,
}

// inventoryKind resolves an inventory entry's (group, Kind) to the registry
// key it is read and navigated by (resources.Registry.KindFor), and reports
// whether it is worth reading. A discovered kind is readable (its Ready
// condition or replica counts are objectReadiness's unstructured branch);
// one the registry doesn't hold is not, since nothing could serve the read.
func inventoryKind(reg resources.Registry, group, apiKind string) (kube.ResourceKind, bool) {
	kind := reg.KindFor(group, apiKind)
	if readableTypedKinds[kind] {
		return kind, true
	}
	d, ok := reg.Descriptor(kind)
	return kind, ok && d.Custom
}

// objectReadiness is the inventory's per-object health read. Workloads
// answer with their replica counts, which is the number an operator acts
// on; everything else falls back to a Ready-style condition, then to "–".
//
// It returns the class alone and never a glyph: the glyph is a pure
// function of the class (inventoryGlyph), and returning both invited the
// two to disagree — which they did, every branch answering "●" so that a
// not-ready object was distinguished from a ready one by colour alone. That
// is precisely what the app's glyph vocabulary exists to avoid, since the
// 256-colour and no-colour degradation paths (CLAUDE.md's terminal
// capability rule) render a red dot and a green dot identically.
func objectReadiness(o runtime.Object) (ready string, class resources.StatusClass) {
	// Workload kinds come out of the informer caches *typed*, not
	// unstructured — only discovered CRDs are unstructured. Handling just
	// the unstructured shape silently rendered every Deployment in an
	// inventory as "–", which is the one column the band exists for.
	switch w := o.(type) {
	case *appsv1.Deployment:
		return replicaCell(w.Status.ReadyReplicas, derefInt32(w.Spec.Replicas))
	case *appsv1.StatefulSet:
		return replicaCell(w.Status.ReadyReplicas, derefInt32(w.Spec.Replicas))
	case *appsv1.DaemonSet:
		return replicaCell(w.Status.NumberReady, w.Status.DesiredNumberScheduled)
	}
	if u, ok := o.(*unstructured.Unstructured); ok {
		desired, okD, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
		avail, okA, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas")
		if okD {
			if !okA {
				avail = 0
			}
			text := fmt.Sprintf("%d/%d", avail, desired)
			if avail >= desired {
				return text, resources.StatusOK
			}
			return text, resources.StatusFail
		}
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			typ, _, _ := unstructured.NestedString(cm, "type")
			if typ != "Ready" && typ != "Available" {
				continue
			}
			st, _, _ := unstructured.NestedString(cm, "status")
			if st == "True" {
				return "ready", resources.StatusOK
			}
			return "not ready", resources.StatusFail
		}
	}
	if pod, ok := o.(*corev1.Pod); ok {
		if pod.Status.Phase == corev1.PodRunning {
			return "running", resources.StatusOK
		}
		return strings.ToLower(string(pod.Status.Phase)), resources.StatusFail
	}
	return "–", resources.StatusNeutral
}

// inventoryGlyph is the shared status vocabulary (resources.DefaultHealthGlyph
// — ● ▲ ✕), with one departure: an object the caches couldn't resolve at all
// renders "·" rather than the generic Neutral "○". ○ means *completed* in
// the pod vocabulary, and claiming a workload finished when kute simply has
// no reading on it would be a fact the screen doesn't have.
func inventoryGlyph(class resources.StatusClass) string {
	if class == resources.StatusNeutral {
		return "·"
	}
	return resources.DefaultHealthGlyph(class)
}

// replicaCell renders a workload's ready/desired pair — the number an
// operator acts on, and the one a health check is actually waiting for.
func replicaCell(ready, desired int32) (string, resources.StatusClass) {
	text := fmt.Sprintf("%d/%d", ready, desired)
	if desired > 0 && ready < desired {
		return text, resources.StatusFail
	}
	return text, resources.StatusOK
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 1 // a Deployment with no explicit replicas defaults to one
	}
	return *p
}

// sortInventory puts trouble first — §31a's list exists to be triaged.
func sortInventory(items []inventoryItem) {
	rank := func(c resources.StatusClass) int {
		switch c {
		case resources.StatusFail:
			return 0
		case resources.StatusWarn:
			return 1
		case resources.StatusOK:
			return 2
		default:
			return 3
		}
	}
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && rank(items[j].Status) < rank(items[j-1].Status); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// buildFailure assembles §31a's top band, including the drill-through to
// the object the failure is actually about.
func buildFailure(u *unstructured.Unstructured, inv []inventoryItem, now time.Time) *failure {
	cond, ok := fluxCondition(u, "Ready")
	if !ok || cond.status == "True" {
		return nil
	}
	// A reconciling object is progressing, not failing — the card is for
	// trouble, and §30a's whole point is not to call progress a failure.
	if rec, ok := fluxCondition(u, "Reconciling"); ok && rec.status == "True" {
		return nil
	}

	f := &failure{Reason: cond.reason, Message: cond.message}
	if t, err := time.Parse(time.RFC3339, cond.lastTransition); err == nil {
		f.Since = t
		if d := retryInterval(u); d > 0 {
			f.RetryAt, f.RetryEvery = t.Add(d), d
		}
	}

	// The drill-through: the first not-ready inventory entry is what the
	// health check was waiting on. Nothing is invented when the inventory
	// is empty or entirely healthy — the card then shows the condition
	// message alone, which is still the diagnosis.
	for _, item := range inv {
		if item.Status != resources.StatusFail {
			continue
		}
		f.Culprit, f.CulpritName, f.CulpritNamespace = item.Kind, item.Name, item.Namespace
		f.Detail = item.Ready
		break
	}
	return f
}

// retryInterval resolves how long until Flux tries again: the explicit
// retryInterval when set, otherwise the ordinary interval, which is what
// the controller falls back to.
func retryInterval(u *unstructured.Unstructured) time.Duration {
	for _, key := range []string{"retryInterval", "interval"} {
		if v := nested(u, "spec", key); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				return d
			}
		}
	}
	return 0
}

type condition struct{ status, reason, message, lastTransition string }

func fluxCondition(u *unstructured.Unstructured, typ string) (condition, bool) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cm, "type"); t != typ {
			continue
		}
		st, _, _ := unstructured.NestedString(cm, "status")
		reason, _, _ := unstructured.NestedString(cm, "reason")
		msg, _, _ := unstructured.NestedString(cm, "message")
		lt, _, _ := unstructured.NestedString(cm, "lastTransitionTime")
		return condition{st, reason, msg, lt}, true
	}
	return condition{}, false
}

func nested(u *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedString(u.Object, path...)
	return v
}

func metaNameNamespace(o runtime.Object) (name, namespace string, ok bool) {
	type metaer interface {
		GetName() string
		GetNamespace() string
	}
	if m, is := o.(metaer); is {
		return m.GetName(), m.GetNamespace(), true
	}
	return "", "", false
}

func shortSourceKind(kind string) string {
	switch kind {
	case "GitRepository":
		return "git"
	case "OCIRepository":
		return "oci"
	case "HelmRepository":
		return "helm"
	case "HelmChart":
		return "chart"
	case "Bucket":
		return "bucket"
	default:
		return strings.ToLower(kind)
	}
}
