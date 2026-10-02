package browse

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/testutil/goldentest"
	"github.com/kute-dev/kute/internal/tui"
)

func nodeObj(name string, ready bool, cordoned bool) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.30.1"},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
		},
	}
}

// fakeMutator is a minimal kube.Mutator test double recording delete/
// cordon/drain calls, for browse's 8b/11a ctrl-d/C/D key tests.
type fakeMutator struct {
	cordoned             map[string]bool
	drained              []string
	deleted              []string
	forceDeleted         []string
	restarted            []string
	scaled               []int32
	setImages            []string // "namespace/name container=image"
	setImageInit         []bool
	setResources         []string // "namespace/name container" of every SetResources call
	fluxSuspends         []string // "namespace/name=true|false" of every SetFluxSuspend call
	fluxReconciles       []string // "namespace/name" of every RequestFluxReconcile call
	argoRefreshes        []string // "namespace/name" of every RequestArgoRefresh call
	argoSyncs            []string // "namespace/name@revision" of every RequestArgoSync call
	certRenews           []string // "namespace/name" of every RenewCertificate call
	retriedJobs          []string // "namespace/name->newName" of every RetryJob call
	replacedJobs         []string // "namespace/name" of every ReplaceJob call
	jobSuspends          []string // "namespace/name=true|false" of every SetJobSuspend call
	triggeredCronJobs    []string // "namespace/name->newJobName" of every TriggerCronJob call
	cronJobSuspends      []string // "namespace/name=true|false" of every SetCronJobSuspend call
	cronJobSchedules     []string // "namespace/name=schedule" of every SetCronJobSchedule call
	dryRun               bool     // true if the most recent SetResources call was a dry-run
	setResourcesApplyErr error    // returned only by the real apply, after a successful dry-run
	metaPatches          []string // "namespace/name labels|annotations key=value" or "...key-" for a removal
	secretDataPatches    []string // "namespace/name key=value" or "...key-" for a removal
	configMapDataPatches []string // "namespace/name key=value" or "...key-" for a removal
	// metaObjs, when set (browse_meta_test.go's newMetaModel wires it to the
	// same store the model's fakeLister reads from), makes PatchMeta also
	// mutate the matching object's labels/annotations in place — so a
	// post-apply refresh (26a's own "confirm → execute → refresh" contract)
	// sees the real change instead of re-reading stale, unpatched data. Left
	// nil everywhere else, where PatchMeta stays a pure recorder.
	metaObjs map[kube.ResourceKind][]runtime.Object
	// setImageObjs is SetImage's own version of metaObjs — wired by
	// browse_setimage_test.go's newSetImageModel to the same store the
	// model's fakeLister reads from, so 24a's post-apply refresh
	// (handleSetImageResult) observes the real new image instead of
	// re-reading a stale container spec. Left nil everywhere else, where
	// SetImage stays a pure recorder.
	setImageObjs map[kube.ResourceKind][]runtime.Object
	err          error
}

func (f *fakeMutator) DeleteResource(_ context.Context, _ kube.ResourceKind, _ string, name string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, name)
	return nil
}
func (f *fakeMutator) DeleteResourceForced(_ context.Context, _ kube.ResourceKind, _ string, name string) error {
	if f.err != nil {
		return f.err
	}
	f.forceDeleted = append(f.forceDeleted, name)
	return nil
}
func (f *fakeMutator) RolloutRestart(_ context.Context, _ kube.ResourceKind, _ string, name string) error {
	if f.err != nil {
		return f.err
	}
	f.restarted = append(f.restarted, name)
	return nil
}
func (f *fakeMutator) Cordon(_ context.Context, node string, cordon bool) error {
	if f.err != nil {
		return f.err
	}
	if f.cordoned == nil {
		f.cordoned = map[string]bool{}
	}
	f.cordoned[node] = cordon
	return nil
}
func (f *fakeMutator) Drain(_ context.Context, node string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.drained = append(f.drained, node)
	return 2, nil
}
func (f *fakeMutator) HelmRollback(context.Context, string, string, int) error { return f.err }
func (f *fakeMutator) RolloutUndo(context.Context, string, string, int) error  { return f.err }
func (f *fakeMutator) Scale(_ context.Context, _ kube.ResourceKind, _, _ string, replicas int32) error {
	if f.err != nil {
		return f.err
	}
	f.scaled = append(f.scaled, replicas)
	return nil
}
func (f *fakeMutator) SetImage(_ context.Context, kind kube.ResourceKind, namespace, name, container, image string, initContainer bool) error {
	if f.err != nil {
		return f.err
	}
	f.setImages = append(f.setImages, namespace+"/"+name+" "+container+"="+image)
	f.setImageInit = append(f.setImageInit, initContainer)
	for _, obj := range f.setImageObjs[kind] {
		acc, err := apimeta.Accessor(obj)
		if err != nil || acc.GetNamespace() != namespace || acc.GetName() != name {
			continue
		}
		setContainerImage(obj, container, image, initContainer)
		break
	}
	return nil
}

// setContainerImage mutates obj's pod-template container (regular or native
// sidecar) named container to image in place — setImageObjs's own version of
// PatchMeta's local-object mutation, covering every kind 24a supports.
func setContainerImage(obj runtime.Object, container, image string, initContainer bool) {
	var containers, initContainers *[]corev1.Container
	switch o := obj.(type) {
	case *appsv1.Deployment:
		containers, initContainers = &o.Spec.Template.Spec.Containers, &o.Spec.Template.Spec.InitContainers
	case *appsv1.StatefulSet:
		containers, initContainers = &o.Spec.Template.Spec.Containers, &o.Spec.Template.Spec.InitContainers
	case *appsv1.DaemonSet:
		containers, initContainers = &o.Spec.Template.Spec.Containers, &o.Spec.Template.Spec.InitContainers
	case *batchv1.CronJob:
		containers, initContainers = &o.Spec.JobTemplate.Spec.Template.Spec.Containers, &o.Spec.JobTemplate.Spec.Template.Spec.InitContainers
	default:
		return
	}
	target := containers
	if initContainer {
		target = initContainers
	}
	for i := range *target {
		if (*target)[i].Name == container {
			(*target)[i].Image = image
			return
		}
	}
}
func (f *fakeMutator) SetResources(_ context.Context, _ kube.ResourceKind, namespace, name, container string, _ kube.ResourceEdits, dryRun bool) error {
	if f.err != nil {
		return f.err
	}
	f.dryRun = dryRun
	f.setResources = append(f.setResources, namespace+"/"+name+" "+container)
	if !dryRun && f.setResourcesApplyErr != nil {
		return f.setResourcesApplyErr
	}
	return nil
}
func (f *fakeMutator) PatchMeta(_ context.Context, kind kube.ResourceKind, namespace, name string, isAnnotation bool, key, value string, remove bool) error {
	if f.err != nil {
		return f.err
	}
	field := "labels"
	if isAnnotation {
		field = "annotations"
	}
	entry := key + "=" + value
	if remove {
		entry = key + "-"
	}
	f.metaPatches = append(f.metaPatches, namespace+"/"+name+" "+field+" "+entry)
	for _, obj := range f.metaObjs[kind] {
		acc, err := apimeta.Accessor(obj)
		if err != nil || acc.GetNamespace() != namespace || acc.GetName() != name {
			continue
		}
		values := acc.GetLabels()
		if isAnnotation {
			values = acc.GetAnnotations()
		}
		if remove {
			delete(values, key)
		} else {
			if values == nil {
				values = map[string]string{}
			}
			values[key] = value
		}
		if isAnnotation {
			acc.SetAnnotations(values)
		} else {
			acc.SetLabels(values)
		}
		break
	}
	return nil
}
func (f *fakeMutator) PatchSecretData(_ context.Context, namespace, name, key, value string, remove bool) error {
	if f.err != nil {
		return f.err
	}
	entry := key + "=" + value
	if remove {
		entry = key + "-"
	}
	f.secretDataPatches = append(f.secretDataPatches, namespace+"/"+name+" "+entry)
	return nil
}
func (f *fakeMutator) PatchConfigMapData(_ context.Context, namespace, name, key, value string, remove bool) error {
	if f.err != nil {
		return f.err
	}
	entry := key + "=" + value
	if remove {
		entry = key + "-"
	}
	f.configMapDataPatches = append(f.configMapDataPatches, namespace+"/"+name+" "+entry)
	return nil
}

// Flux verbs (§30a). Recorded but inert: no test in this package drives
// them, and the Mutator contract requires them.
func (f *fakeMutator) SetFluxSuspend(_ context.Context, _ kube.ResourceKind, namespace, name string, suspend bool) error {
	if f.err != nil {
		return f.err
	}
	f.fluxSuspends = append(f.fluxSuspends, fmt.Sprintf("%s/%s=%t", namespace, name, suspend))
	return nil
}

func (f *fakeMutator) RequestFluxReconcile(_ context.Context, _ kube.ResourceKind, namespace, name string) error {
	if f.err != nil {
		return f.err
	}
	f.fluxReconciles = append(f.fluxReconciles, namespace+"/"+name)
	return nil
}

func (f *fakeMutator) RequestArgoRefresh(_ context.Context, _ kube.ResourceKind, namespace, name string) error {
	if f.err != nil {
		return f.err
	}
	f.argoRefreshes = append(f.argoRefreshes, namespace+"/"+name)
	return nil
}

func (f *fakeMutator) RequestArgoSync(_ context.Context, _ kube.ResourceKind, namespace, name, revision string) error {
	if f.err != nil {
		return f.err
	}
	f.argoSyncs = append(f.argoSyncs, namespace+"/"+name+"@"+revision)
	return nil
}

func (f *fakeMutator) RenewCertificate(_ context.Context, namespace, name string) error {
	if f.err != nil {
		return f.err
	}
	f.certRenews = append(f.certRenews, namespace+"/"+name)
	return nil
}

func (f *fakeMutator) RetryJob(_ context.Context, namespace, name, newName, creator string, at time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.retriedJobs = append(f.retriedJobs, namespace+"/"+name+"->"+newName)
	return nil
}

func (f *fakeMutator) ReplaceJob(_ context.Context, namespace, name string) error {
	if f.err != nil {
		return f.err
	}
	f.replacedJobs = append(f.replacedJobs, namespace+"/"+name)
	return nil
}

func (f *fakeMutator) SetJobSuspend(_ context.Context, namespace, name string, suspend bool) error {
	if f.err != nil {
		return f.err
	}
	f.jobSuspends = append(f.jobSuspends, fmt.Sprintf("%s/%s=%t", namespace, name, suspend))
	return nil
}

func (f *fakeMutator) TriggerCronJob(_ context.Context, namespace, name, newJobName, _ string, _ time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.triggeredCronJobs = append(f.triggeredCronJobs, namespace+"/"+name+"->"+newJobName)
	return nil
}

func (f *fakeMutator) SetCronJobSuspend(_ context.Context, namespace, name string, suspend bool, _ string, _ int64, _ time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.cronJobSuspends = append(f.cronJobSuspends, fmt.Sprintf("%s/%s=%t", namespace, name, suspend))
	return nil
}

func (f *fakeMutator) SetCronJobSchedule(_ context.Context, namespace, name string, edit kube.CronJobScheduleEdit) (kube.CronJobScheduleResult, error) {
	if f.err != nil {
		return kube.CronJobScheduleResult{}, f.err
	}
	f.cronJobSchedules = append(f.cronJobSchedules, namespace+"/"+name+"="+edit.Schedule)
	return kube.CronJobScheduleResult{Schedule: edit.Schedule, ResourceVersion: "2"}, nil
}

func TestNodeColumnsRenderStatusPodsAndVersion(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false)},
		kube.KindPod:  {pod("default", "api-1")},
	}}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	for _, want := range []string{"node-a", "Ready", "PODS", "VERSION", "v1.30.1", "cluster-scoped"} {
		if !strings.Contains(view, want) {
			t.Fatalf("node view missing %q:\n%s", want, view)
		}
	}
}

// TestNodeHealthColumnTalliesScheduledPods pins 11a's HEALTH column: a
// per-node glyph tally of that node's own pods (OK/Warn/Fail order, zero
// classes skipped), independent of the unrelated node-b's own pods — the
// same classification projectPod already uses for the Pods list itself, so
// a node's HEALTH tally always agrees with what the Pods list would show
// filtered to that node.
func TestNodeHealthColumnTalliesScheduledPods(t *testing.T) {
	crashing := crashPod("default", "crashing")
	crashing.Spec.NodeName = "node-a"
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false), nodeObj("node-b", true, false)},
		kube.KindPod: {
			schedPod("default", "healthy", "node-a"),
			crashing,
			schedPod("default", "other", "node-b"),
		},
	}}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	if !strings.Contains(view, "HEALTH") {
		t.Fatalf("node view missing HEALTH column header:\n%s", view)
	}
	nodeA := m.nodePodHealth["node-a"]
	if nodeA.OK != 1 || nodeA.Fail != 1 {
		t.Fatalf("node-a health = %+v, want OK:1 Fail:1", nodeA)
	}
	nodeB := m.nodePodHealth["node-b"]
	if nodeB.OK != 1 || nodeB.Fail != 0 {
		t.Fatalf("node-b health = %+v, want OK:1 Fail:0", nodeB)
	}
}

// TestNodeStatusReadyRendersDimNotGreen pins 11a: STATUS "Ready" renders
// TextDim, matching the ROLLOUT column's own "healthy state renders dim,
// not green" carve-out — NotReady still gets the usual Bad/red status color.
func TestNodeStatusReadyRendersDimNotGreen(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false), nodeObj("node-b", false, false)},
	}}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := m.Render()
	var readyLine, notReadyLine string
	for _, l := range strings.Split(view, "\n") {
		switch {
		case strings.Contains(l, "node-a"):
			readyLine = l
		case strings.Contains(l, "node-b"):
			notReadyLine = l
		}
	}
	if readyLine == "" || notReadyLine == "" {
		t.Fatalf("expected both node rows in the rendered view:\n%s", plain(view))
	}
	// Isolate the STATUS column's own text run (the color code immediately
	// preceding "Ready"/"NotReady") rather than scanning the whole row,
	// which also legitimately contains the leading status glyph column in
	// theme.Good — that column is untouched by this fix.
	readyCode := statusTextColorCode(t, readyLine, "Ready")
	notReadyCode := statusTextColorCode(t, notReadyLine, "NotReady")
	dim := "38;2;103;103;128" // theme.TextDim
	bad := "38;2;239;106;106" // theme.Bad (#ef6a6a; 0x6a = 106 — lipgloss v1 rounded this channel down by 1, v2 renders the exact hex value)
	if !strings.Contains(readyCode, dim) {
		t.Errorf("Ready's STATUS cell color = %q, want to contain TextDim %q", readyCode, dim)
	}
	if !strings.Contains(notReadyCode, bad) {
		t.Errorf("NotReady's STATUS cell color = %q, want to contain Bad %q", notReadyCode, bad)
	}
}

// statusTextColorCode extracts the ANSI color code immediately preceding
// word's own text run in line (an ANSI-styled Render() output), where
// Render wraps each span as "\x1b[<code>m<text>\x1b[0m" with no
// intervening escape between the code and the text it colors.
func statusTextColorCode(t *testing.T, line, word string) string {
	t.Helper()
	re := regexp.MustCompile("\x1b\\[([0-9;]+)m" + word)
	m := re.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("could not find a styled %q run in line:\n%q", word, line)
	}
	return m[1]
}

func TestCKeyCordonsAndUncordonsNode(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false)},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "C"})
	if cordoned, ok := mut.cordoned["node-a"]; !ok || !cordoned {
		t.Fatalf("expected node-a cordoned=true, got %v", mut.cordoned)
	}
	if m.state != tui.TaskStateReady {
		t.Fatalf("expected state to return to ready after cordon, got %s", m.state)
	}
}

func TestCKeyOnCordonedNodeUncordons(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, true)}, // already cordoned
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "C"})
	if cordoned, ok := mut.cordoned["node-a"]; !ok || cordoned {
		t.Fatalf("expected node-a cordoned=false (uncordon), got %v", mut.cordoned)
	}
}

func schedPod(ns, name, node string) *corev1.Pod {
	p := pod(ns, name)
	p.Spec.NodeName = node
	return p
}

func TestDKeyShowsDrainConfirmAndYExecutes(t *testing.T) {
	// Drain leaves DaemonSet and mirror (static) pods in place, so the
	// confirm must not count them as evicted.
	ds := schedPod("kube-system", "kube-proxy-x", "node-a")
	ds.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "kube-proxy"}}
	mirror := schedPod("kube-system", "etcd-node-a", "node-a")
	mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false)},
		kube.KindPod: {
			schedPod("default", "p1", "node-a"), schedPod("default", "p2", "node-a"), ds, mirror,
		},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "ctrl+d"})
	if !m.actions.Active() {
		t.Fatal("expected D to open a drain confirmation")
	}
	view := plain(m.Render())
	if !strings.Contains(view, "node-a") || !strings.Contains(view, "2 pods will be evicted.") {
		t.Fatalf("drain confirm missing evicted-pod count:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.drained) != 1 || mut.drained[0] != "node-a" {
		t.Fatalf("expected node-a drained, got %v", mut.drained)
	}
}

func TestDKeyThenNCancelsWithoutDraining(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false)},
	}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "ctrl+d"})
	m = step(t, m, tea.KeyPressMsg{Text: "n"})
	if m.actions.Active() {
		t.Fatal("expected n to cancel the drain confirmation")
	}
	if len(mut.drained) != 0 {
		t.Fatalf("expected no drain, got %v", mut.drained)
	}
}

func TestNodeHealthStripShowsReadyPressureCordoned(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {
			nodeObj("node-a", true, false),
			nodeObj("node-b", true, true), // cordoned
		},
	}}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	for _, want := range []string{"ready", "cordoned", "2 nodes"} {
		if !strings.Contains(view, want) {
			t.Fatalf("node health strip missing %q:\n%s", want, view)
		}
	}
}

func TestEnterOpensNodeDetail(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false)},
	}}
	var openedNode string
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{
		Session: session, Lister: lister,
		OpenNodeDetail: func(name string, w, h int) (tea.Model, tea.Cmd) {
			openedNode = name
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "enter"})
	if openedNode != "node-a" {
		t.Fatalf("expected node-a to be opened, got %q", openedNode)
	}
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected Update to return the pushed stub task, got %T", updated)
	}
}

// stubTask is a minimal tea.Model standing in for a pushed screen.
type stubTask struct{}

func (stubTask) Init() tea.Cmd                       { return nil }
func (stubTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return stubTask{}, nil }
func (stubTask) View() tea.View                      { return tea.NewView("") }

// TestNodeMajorityVersionIsStableAcrossTies pins the tie-break — see
// tasks/overview's own copy of this algorithm and test.
//
// Map iteration order is randomised, so an evenly split cluster used to pick a
// different "majority" version per call, and 11a's ▲ markers (which flag every
// node differing from it) moved between redraws.
func TestNodeMajorityVersionIsStableAcrossTies(t *testing.T) {
	t.Parallel()
	m := Model{
		desc: resources.Descriptor{Columns: []string{"Name", "Version"}},
		rows: []resources.Row{
			{Cells: []string{"node-a", "v1.31.0"}},
			{Cells: []string{"node-b", "v1.31.0"}},
			{Cells: []string{"node-c", "v1.32.0"}},
			{Cells: []string{"node-d", "v1.32.0"}},
			{Cells: []string{"node-e", "v1.30.0"}},
			{Cells: []string{"node-f", "v1.30.0"}},
		},
	}

	first := m.nodeMajorityVersion()
	if first == "" {
		t.Fatal("nodeMajorityVersion returned empty for populated rows")
	}
	for range 200 {
		if got := m.nodeMajorityVersion(); got != first {
			t.Fatalf("nodeMajorityVersion is unstable: got %q then %q", first, got)
		}
	}

	// An outright majority must still win.
	m.rows = append(m.rows, resources.Row{Cells: []string{"node-g", "v1.32.0"}})
	if got := m.nodeMajorityVersion(); got != "v1.32.0" {
		t.Fatalf("nodeMajorityVersion = %q, want the outright majority v1.32.0", got)
	}
}

// TestDrainConfirmBorderIsRed pins review 10 M6: the drain card styled its
// border with Foreground, which lipgloss v2 never applies to a border, so
// one of the most disruptive confirms rendered with an uncolored border
// instead of Theme.ConfirmBorder's destructive red.
func TestDrainConfirmBorderIsRed(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {nodeObj("node-a", true, false)},
	}}
	session := newSession()
	session.Location.Kind = kube.KindNode
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "ctrl+d"})
	if !m.actions.Active() {
		t.Fatal("expected ctrl+d to open a drain confirmation")
	}
	red := lipgloss.NewStyle().Foreground(m.Theme().ConfirmBorder).Render("╭")
	red = red[:strings.Index(red, "╭")+len("╭")]
	if !strings.Contains(goldentest.Truecolor(m.Render()), red) {
		t.Fatalf("expected the drain card's top-left corner drawn in ConfirmBorder (%q)", red)
	}
}
