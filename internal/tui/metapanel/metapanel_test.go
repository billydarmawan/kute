package metapanel

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	all := f.objs[kind]
	if namespace == "" {
		return all, nil
	}
	var out []runtime.Object
	for _, o := range all {
		if a, err := apimeta.Accessor(o); err == nil && a.GetNamespace() == namespace {
			out = append(out, o)
		}
	}
	return out, nil
}

// fakeMutator embeds a nil kube.Mutator for the interface's full surface —
// only PatchMeta is ever exercised by this panel, so anything else reaching
// the fake is itself a bug and the nil embed panics loudly.
type fakeMutator struct {
	kube.Mutator
	err         error
	metaObjs    map[kube.ResourceKind][]runtime.Object
	metaPatches []string // "namespace/name labels|annotations key=value|key-"
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

// metaDeployment builds a Deployment carrying labels/annotations for 26a's
// editor to open on, with an optional own spec.selector.matchLabels (for the
// immutable-selector-key test).
func metaDeployment(ns, name string, labels, annotations, selector map[string]string) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Image: "nva-worker:1.0"}}},
		}},
	}
	if selector != nil {
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
	}
	return dep
}

// host replicates the hosting contract every screen follows (the package doc
// comment): the confirm handler runs first while ctrl.Active(), otherwise
// keys route to Update; a cancel calls CancelConfirm then ctrl.Cancel; an
// actions.ResultMsg goes through ctrl.HandleResult then HandleResult. It
// doubles as a spec of what a new host has to wire.
type host struct {
	t     *testing.T
	panel *Model
	ctrl  actions.Controller
}

func newHost(t *testing.T, mut *fakeMutator, objs map[kube.ResourceKind][]runtime.Object) *host {
	t.Helper()
	mut.metaObjs = objs
	session := &tui.Session{
		Registry: resources.DefaultRegistry(),
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "default", Kind: kube.KindDeployment},
		Theme:    tui.Dark(),
	}
	h := &host{t: t, ctrl: actions.New(mut)}
	panel, ok := Open(Config{Session: session, Lister: fakeLister{objs: objs}}, kube.KindDeployment, "default", "nva-worker")
	if !ok {
		t.Fatal("Open returned false")
	}
	h.panel = panel
	return h
}

func (h *host) step(msg tea.Msg) {
	h.t.Helper()
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			if c != nil {
				h.step(c())
			}
		}
	case actions.ResultMsg:
		h.ctrl.HandleResult(msg)
		if IsActionID(msg.ActionID) && h.panel != nil {
			if h.panel.HandleResult(msg) {
				h.panel = nil
			}
		}
	case tea.KeyPressMsg:
		if h.ctrl.Active() {
			switch msg.String() {
			case "y":
				h.run(h.ctrl.Confirm())
			case "n", "esc":
				if h.panel != nil {
					h.panel.CancelConfirm()
				}
				h.ctrl.Cancel()
			}
			return
		}
		closed, cmd := h.panel.Update(msg, &h.ctrl)
		if closed {
			h.panel = nil
		}
		h.run(cmd)
	}
}

func (h *host) run(cmd tea.Cmd) {
	h.t.Helper()
	if cmd != nil {
		h.step(cmd())
	}
}

func TestEditableGating(t *testing.T) {
	cases := []struct {
		kind kube.ResourceKind
		want bool
	}{
		{kube.KindPod, true},
		{kube.KindDeployment, true},
		{kube.KindNode, true},
		{kube.KindCustomResourceDefinition, true},
		{kube.KindForward, false},
		{kube.KindHelmRelease, false},
		{kube.KindWhoCan, false},
		{kube.KindOverview, false},
	}
	for _, c := range cases {
		if got := Editable(c.kind); got != c.want {
			t.Errorf("Editable(%s) = %v, want %v", c.kind, got, c.want)
		}
	}
}

func TestOpenBuildsSortedRowsFromCurrentValues(t *testing.T) {
	dep := metaDeployment("default", "nva-worker",
		map[string]string{"team": "platform", "app": "nva-worker"},
		map[string]string{"kute.dev/owner": "platform-oncall"},
		nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	t2 := h.panel.t
	if len(t2.labels) != 2 || t2.labels[0].key != "app" || t2.labels[1].key != "team" {
		t.Fatalf("labels = %+v, want sorted [app team]", t2.labels)
	}
	if t2.labels[0].current != "nva-worker" || t2.labels[0].input.Value() != "nva-worker" {
		t.Errorf("app row = %+v, want current/buffer nva-worker", t2.labels[0])
	}
	if len(t2.annotations) != 1 || t2.annotations[0].key != "kute.dev/owner" {
		t.Fatalf("annotations = %+v, want [kute.dev/owner]", t2.annotations)
	}
}

func TestJoinedLabelGetsSelectorWarning(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"app": "nva-worker", "env": "stage"}, nil, nil)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "nva-worker"}},
	}
	pods := []runtime.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default", Labels: map[string]string{"app": "nva-worker"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "default", Labels: map[string]string{"app": "nva-worker"}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "default", Labels: map[string]string{"app": "other"}}},
	}
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep}, kube.KindService: {svc}, kube.KindPod: pods,
	})
	var app, env *metaRow
	for i := range h.panel.t.labels {
		switch h.panel.t.labels[i].key {
		case "app":
			app = &h.panel.t.labels[i]
		case "env":
			env = &h.panel.t.labels[i]
		}
	}
	if app.joinService != "nva-worker" || app.joinPodCount != 2 {
		t.Errorf("app row join = %q/%d, want nva-worker/2", app.joinService, app.joinPodCount)
	}
	if env.joinService != "" {
		t.Errorf("env row should carry no join, got %q", env.joinService)
	}
}

func TestImmutableSelectorLabelIsReadOnlyAndBlocksEdits(t *testing.T) {
	dep := metaDeployment("default", "nva-worker",
		map[string]string{"app": "nva-worker", "team": "platform"}, nil,
		map[string]string{"app": "nva-worker"})
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})
	if !h.panel.t.labels[0].readOnly { // "app" sorts first
		t.Fatal("expected the immutable selector key's row to be read-only")
	}

	// labelIdx starts at 0 ("app"); ↵ (enter editing) and D must both be
	// no-ops on a read-only row.
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	if h.panel.t.editing {
		t.Error("↵ on a read-only row should not enter editing mode")
	}
	_, cmd := h.panel.Update(tea.KeyPressMsg{Text: "D"}, &h.ctrl)
	if cmd != nil {
		t.Error("D on a read-only row should be a no-op, got a cmd")
	}
}

func TestNavigationModeIgnoresPrintableCharacters(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	// A printable character with no navigation-mode binding (unlike 'a'
	// add, 'y' copy) must not leak into the row buffer — navigation mode has
	// no text-entry context at all, so an unrecognized key is simply a
	// no-op (the row is untouched, the panel stays open).
	closed, cmd := h.panel.Update(tea.KeyPressMsg{Text: "z"}, &h.ctrl)
	if closed || cmd != nil || h.panel.t.adding != metaAddNone {
		t.Error("'z' in navigation mode should be a plain no-op")
	}
	if h.panel.t.labels[0].input.Value() != "platform" {
		t.Errorf("typing in navigation mode changed the row buffer: %q", h.panel.t.labels[0].input.Value())
	}
}

// TestAddHotkeyStartsAddModeAndShiftTabReturnsToKey: 'a' (not 'n') opens the
// focused grid's add-row, and shift+tab — not just tab — moves focus back to
// the key buffer once the value buffer has it, mirroring the rest of the
// panel's own bidirectional tab convention instead of only going key -> value.
func TestAddHotkeyStartsAddModeAndShiftTabReturnsToKey(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Text: "a"})
	if h.panel.t.adding != metaAddLabel {
		t.Fatal("'a' should start add-mode on the focused grid")
	}

	h.step(tea.KeyPressMsg{Text: "k"})
	h.step(tea.KeyPressMsg{Text: "tab"})
	h.step(tea.KeyPressMsg{Text: "v"})
	if h.panel.t.addKeyInput.Value() != "k" || h.panel.t.addValueInput.Value() != "v" || !h.panel.t.addOnValue {
		t.Fatalf("after tab, key/value = %q/%q, addOnValue = %v", h.panel.t.addKeyInput.Value(), h.panel.t.addValueInput.Value(), h.panel.t.addOnValue)
	}

	h.step(tea.KeyPressMsg{Text: "shift+tab"})
	if h.panel.t.addOnValue {
		t.Fatal("shift+tab should move focus back to the key buffer")
	}
	h.step(tea.KeyPressMsg{Text: "2"})
	if h.panel.t.addKeyInput.Value() != "k2" || h.panel.t.addValueInput.Value() != "v" {
		t.Fatalf("after shift+tab, key/value = %q/%q, want k2/v", h.panel.t.addKeyInput.Value(), h.panel.t.addValueInput.Value())
	}
}

func TestTabSwitchesFocusedSectionIndependentOfCursor(t *testing.T) {
	dep := metaDeployment("default", "nva-worker",
		map[string]string{"team": "platform"}, map[string]string{"kute.dev/owner": "platform-oncall"}, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})
	if h.panel.t.section != metaSectionLabels {
		t.Fatal("expected the panel to open focused on LABELS")
	}

	h.step(tea.KeyPressMsg{Text: "tab"})
	if h.panel.t.section != metaSectionAnnotations {
		t.Fatal("tab should switch focus to ANNOTATIONS")
	}
	r := h.panel.t.selectedRow()
	if r == nil || r.key != "kute.dev/owner" {
		t.Fatalf("selected row after tab = %+v, want kute.dev/owner", r)
	}

	h.step(tea.KeyPressMsg{Text: "shift+tab"})
	if h.panel.t.section != metaSectionLabels {
		t.Fatal("shift+tab should switch focus back to LABELS")
	}
}

func TestControllerManagedAnnotationIsReadOnly(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", nil,
		map[string]string{
			"deployment.kubernetes.io/revision":                "42",
			"kubectl.kubernetes.io/last-applied-configuration": "{}",
			"kute.dev/owner": "platform-oncall",
		}, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})
	for _, a := range h.panel.t.annotations {
		want := a.key != "kute.dev/owner"
		if a.readOnly != want {
			t.Errorf("annotation %q readOnly = %v, want %v", a.key, a.readOnly, want)
		}
	}
}

func TestHelmOwnedNoteOnManagedByRowOnly(t *testing.T) {
	dep := metaDeployment("default", "nva-worker",
		map[string]string{"app.kubernetes.io/managed-by": "Helm", "team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})
	for _, l := range h.panel.t.labels {
		want := l.key == "app.kubernetes.io/managed-by"
		if l.helmOwnedNote != want {
			t.Errorf("label %q helmOwnedNote = %v, want %v", l.key, l.helmOwnedNote, want)
		}
	}
}

func TestEditNonJoinedLabelAppliesImmediately(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"env": "stage"}, nil, nil)
	mut := &fakeMutator{}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // navigation -> editing mode
	h.step(tea.KeyPressMsg{Text: "g"})          // "stage" -> "stageg"
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // save

	if h.ctrl.Active() {
		t.Error("a non-joined edit is TierNone and should execute immediately, not confirm")
	}
	if len(mut.metaPatches) != 1 || mut.metaPatches[0] != "default/nva-worker labels env=stageg" {
		t.Errorf("metaPatches = %v, want one env=stageg label patch", mut.metaPatches)
	}
	// docs/design README.md §26a: "confirm → execute → refresh → show
	// result → remain on screen" — the panel stays open, the row is
	// re-fetched from the object (never an optimistic local patch) and no
	// longer reads as changed, and an inline success message appears.
	if h.panel == nil {
		t.Fatal("the panel should stay open after a successful apply")
	}
	if h.panel.t.editing {
		t.Error("editing should end once the apply succeeds")
	}
	if got := h.panel.t.labels[0].current; got != "stageg" {
		t.Errorf("refreshed current = %q, want stageg", got)
	}
	if h.panel.t.labels[0].changed() {
		t.Error("the refreshed row should no longer read as changed")
	}
	if want := "updated env=stageg"; h.panel.t.message != want {
		t.Errorf("message = %q, want %q", h.panel.t.message, want)
	}
}

// The PATCH result usually beats the watch event for it, so HandleResult's
// refetch reads the pre-commit value; the panel must catch up when the
// event lands, without losing the result line or the user's place.
func TestRefreshPicksUpTheWriteOnceTheCacheSeesIt(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"env": "stage", "team": "a"}, nil, nil)
	mut := &fakeMutator{}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})
	mut.metaObjs = nil // the patch lands server-side, not yet in the cache

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	h.step(tea.KeyPressMsg{Text: "g"})
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := h.panel.t.labels[0].current; got != "stage" {
		t.Fatalf("precondition: refetch before the watch event should read the stale %q, got %q", "stage", got)
	}

	dep.Labels["env"] = "stageg"
	h.panel.Refresh(kube.KindPod, &h.ctrl) // another kind: no-op
	if got := h.panel.t.labels[0].current; got != "stage" {
		t.Fatalf("a change to an unrelated kind refreshed the panel: current = %q", got)
	}
	h.panel.Refresh(kube.KindDeployment, &h.ctrl)
	if got := h.panel.t.labels[0].current; got != "stageg" {
		t.Errorf("env current after refresh = %q, want stageg", got)
	}
	if want := "updated env=stageg"; h.panel.t.message != want {
		t.Errorf("message = %q, want %q kept across the refresh", h.panel.t.message, want)
	}

	// A refresh mid-edit keeps the cursor and the typed buffer.
	h.step(tea.KeyPressMsg{Code: tea.KeyDown})
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // start editing team
	h.step(tea.KeyPressMsg{Text: "x"})
	dep.Labels["team"] = "b"
	h.panel.Refresh(kube.KindDeployment, &h.ctrl)
	if h.panel.t.labelIdx != 1 || !h.panel.t.editing {
		t.Fatalf("refresh lost the cursor or editing state: idx=%d editing=%v", h.panel.t.labelIdx, h.panel.t.editing)
	}
	team := h.panel.t.labels[1]
	if team.current != "b" || team.input.Value() != "ax" {
		t.Errorf("team row = current %q buffer %q, want current b with the typed buffer ax kept", team.current, team.input.Value())
	}
}

// unsyncedServiceLister is a fakeLister whose Service cache hasn't filled —
// the first read of a lazy kind, which returns an empty list.
type unsyncedServiceLister struct{ fakeLister }

func (unsyncedServiceLister) KindSynced(kind kube.ResourceKind, _ string) bool {
	return kind != kube.KindService
}

// An empty Service list from a cache that hasn't filled is not evidence of
// no join: the label edit must still ask before it can detach pods.
func TestEditLabelWithUnsyncedServicesRequiresConfirm(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"app": "nva-worker"}, nil, nil)
	mut := &fakeMutator{}
	objs := map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}}
	h := newHost(t, mut, objs)
	panel, ok := Open(Config{Session: h.panel.cfg.Session, Lister: unsyncedServiceLister{fakeLister{objs: objs}}}, kube.KindDeployment, "default", "nva-worker")
	if !ok {
		t.Fatal("Open returned false")
	}
	h.panel = panel

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	h.step(tea.KeyPressMsg{Text: "2"})
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(mut.metaPatches) != 0 {
		t.Fatalf("label edit applied without confirm while Services were unsynced: %v", mut.metaPatches)
	}
	if !h.ctrl.Active() {
		t.Fatal("a label edit whose Service joins can't be known yet should require confirmation")
	}
	if strip := ansi.Strip(h.panel.WillRunStrip(160, &h.ctrl)); !strings.Contains(strip, "service joins unknown") {
		t.Errorf("will-run strip = %q, want it to say service joins are unknown", strip)
	}
}

func TestEditJoinedLabelRequiresConfirmThenApplies(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"app": "nva-worker"}, nil, nil)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "nva-worker"}},
	}
	mut := &fakeMutator{}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep}, kube.KindService: {svc},
	})

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // navigation -> editing mode
	h.step(tea.KeyPressMsg{Text: "2"})          // "nva-worker" -> "nva-worker2"
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // save (join-escalated, so this opens the confirm)

	if len(mut.metaPatches) != 0 {
		t.Fatalf("expected no patch yet (confirm pending), got %v", mut.metaPatches)
	}
	if !h.ctrl.Active() {
		t.Fatal("editing a Service-selector-joined label should require confirmation")
	}
	if h.panel == nil {
		t.Fatal("the panel should stay open under the inline confirm")
	}
	pending := h.ctrl.Pending()
	if pending == nil || pending.Scope.MetaJoinService != "nva-worker" {
		t.Fatalf("pending scope = %+v, want MetaJoinService nva-worker", pending)
	}

	h.step(tea.KeyPressMsg{Text: "y"})
	if len(mut.metaPatches) != 1 || mut.metaPatches[0] != "default/nva-worker labels app=nva-worker2" {
		t.Errorf("metaPatches after confirm = %v, want one app=nva-worker2 label patch", mut.metaPatches)
	}
	if h.panel == nil {
		t.Fatal("the panel should stay open after the confirmed edit applies")
	}
	if got := h.panel.t.labels[0].current; got != "nva-worker2" {
		t.Errorf("refreshed current = %q, want nva-worker2", got)
	}
	if want := "updated app=nva-worker2"; h.panel.t.message != want {
		t.Errorf("message = %q, want %q", h.panel.t.message, want)
	}
}

func TestRemoveKeyRequiresConfirmThenApplies(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	mut := &fakeMutator{}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Text: "D"})
	if !h.ctrl.Active() {
		t.Fatal("removing a key should always require inline confirmation")
	}
	if tier := h.ctrl.Tier(); tier != actions.TierInline {
		t.Errorf("remove tier = %v, want TierInline", tier)
	}
	if h.panel == nil {
		t.Fatal("the panel should stay open under the inline confirm")
	}

	h.step(tea.KeyPressMsg{Text: "y"})
	if len(mut.metaPatches) != 1 || mut.metaPatches[0] != "default/nva-worker labels team-" {
		t.Errorf("metaPatches = %v, want one team removal", mut.metaPatches)
	}
	// docs/design README.md §26a: the panel stays open after a removal too
	// — the row is gone (re-fetched, not locally spliced out) and focus
	// lands on the nearest remaining row (here, none — the grid is empty).
	if h.panel == nil {
		t.Fatal("the panel should stay open once the confirmed removal applies")
	}
	if len(h.panel.t.labels) != 0 {
		t.Errorf("labels = %+v, want none left after removal", h.panel.t.labels)
	}
	if want := "removed team"; h.panel.t.message != want {
		t.Errorf("message = %q, want %q", h.panel.t.message, want)
	}
}

// TestCancellingRemoveConfirmKeepsPanelOpen covers 'n'/'esc' during the
// removal confirm: the row survives (nothing was ever applied) and the panel
// stays open in navigation mode rather than closing.
func TestCancellingRemoveConfirmKeepsPanelOpen(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	mut := &fakeMutator{}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Text: "D"})
	h.step(tea.KeyPressMsg{Text: "n"})

	if h.ctrl.Active() {
		t.Error("cancel should end the confirm")
	}
	if h.panel == nil {
		t.Fatal("cancelling the removal confirm should leave the panel open")
	}
	if len(h.panel.t.labels) != 1 || h.panel.t.labels[0].key != "team" {
		t.Error("the row should survive a cancelled removal")
	}
	if len(mut.metaPatches) != 0 {
		t.Errorf("metaPatches = %v, want none", mut.metaPatches)
	}
}

// TestJoinedLabelEditKeepsPanelOpenThroughConfirm covers the other
// TierInline path — editing a Service-selector-joined label — verifying the
// panel stays open under the confirm, and that cancelling reverts the buffer
// (CancelConfirm) instead of leaving a half-applied edit behind.
func TestJoinedLabelEditKeepsPanelOpenThroughConfirm(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"app": "nva-worker"}, nil, nil)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "nva-worker", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "nva-worker"}},
	}
	mut := &fakeMutator{}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep}, kube.KindService: {svc},
	})

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // navigation -> editing mode
	h.step(tea.KeyPressMsg{Text: "2"})          // "nva-worker" -> "nva-worker2"
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // save -> opens the confirm

	if h.panel == nil {
		t.Fatal("the panel should stay open under the inline confirm")
	}
	if h.panel.t.editing {
		t.Error("editing should end once input hands off to the confirm")
	}
	if got := h.panel.t.labels[0].input.Value(); got != "nva-worker2" {
		t.Errorf("buffer while confirming = %q, want nva-worker2", got)
	}

	h.step(tea.KeyPressMsg{Text: "n"}) // cancel
	if h.panel == nil {
		t.Fatal("cancelling should leave the panel open")
	}
	if got := h.panel.t.labels[0].input.Value(); got != "nva-worker" {
		t.Errorf("buffer after cancel = %q, want reverted to nva-worker", got)
	}
	if len(mut.metaPatches) != 0 {
		t.Fatalf("metaPatches after cancel = %v, want none", mut.metaPatches)
	}

	// Redo the edit and this time confirm it.
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	h.step(tea.KeyPressMsg{Text: "2"})
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	h.step(tea.KeyPressMsg{Text: "y"})

	if h.panel == nil {
		t.Fatal("the panel should stay open once the confirmed edit applies")
	}
	if len(mut.metaPatches) != 1 || mut.metaPatches[0] != "default/nva-worker labels app=nva-worker2" {
		t.Errorf("metaPatches = %v, want one app=nva-worker2 label patch", mut.metaPatches)
	}
	if got := h.panel.t.labels[0].current; got != "nva-worker2" {
		t.Errorf("refreshed current = %q, want nva-worker2", got)
	}
}

func TestAddLabelHappyPath(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	mut := &fakeMutator{}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Text: "a"})
	if h.panel.t.adding != metaAddLabel {
		t.Fatal("'a' should add to the focused (LABELS, by default) grid")
	}
	for _, r := range "tier" {
		h.step(tea.KeyPressMsg{Text: string(r)})
	}
	h.step(tea.KeyPressMsg{Text: "tab"})
	for _, r := range "gold" {
		h.step(tea.KeyPressMsg{Text: string(r)})
	}
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(mut.metaPatches) != 1 || mut.metaPatches[0] != "default/nva-worker labels tier=gold" {
		t.Errorf("metaPatches = %v, want one tier=gold label patch", mut.metaPatches)
	}
	if h.panel == nil {
		t.Fatal("the panel should stay open after adding")
	}
	if h.panel.t.adding != metaAddNone {
		t.Error("add-mode should end once the add succeeds")
	}
	if len(h.panel.t.labels) != 2 || h.panel.t.labels[1].key != "tier" || h.panel.t.labels[1].current != "gold" {
		t.Errorf("labels = %+v, want [team tier=gold] with tier re-fetched from the object", h.panel.t.labels)
	}
	if want := "added tier=gold"; h.panel.t.message != want {
		t.Errorf("message = %q, want %q", h.panel.t.message, want)
	}
}

func TestAddAnnotationHappyPath(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", nil, map[string]string{"kute.dev/owner": "platform-oncall"}, nil)
	mut := &fakeMutator{}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Text: "tab"}) // focus ANNOTATIONS
	h.step(tea.KeyPressMsg{Text: "a"})
	if h.panel.t.adding != metaAddAnnotation {
		t.Fatal("'a' should add to the focused (ANNOTATIONS, after tab) grid")
	}
	for _, r := range "note" {
		h.step(tea.KeyPressMsg{Text: string(r)})
	}
	h.step(tea.KeyPressMsg{Text: "tab"})
	for _, r := range "hi" {
		h.step(tea.KeyPressMsg{Text: string(r)})
	}
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(mut.metaPatches) != 1 || mut.metaPatches[0] != "default/nva-worker annotations note=hi" {
		t.Errorf("metaPatches = %v, want one note=hi annotation patch", mut.metaPatches)
	}
	if want := "added note=hi"; h.panel == nil || h.panel.t.message != want {
		t.Errorf("message = %q, want %q", h.panel.t.message, want)
	}
}

func TestAddModeEscCancelsWithoutClosingPanel(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Text: "a"})
	h.step(tea.KeyPressMsg{Text: "esc"})
	if h.panel == nil {
		t.Fatal("esc while adding should cancel add-mode, not close the panel")
	}
	if h.panel.t.adding != metaAddNone {
		t.Error("expected add-mode to be cleared")
	}
}

func TestEditingModeInsertsReservedLettersLiterally(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // navigation -> editing mode
	if !h.panel.t.editing {
		t.Fatal("↵ on an editable row should enter editing mode")
	}
	// 'a' (add) and 'y' (copy) are navigation-mode shortcuts, 'A' isn't
	// bound to anything — all three still must insert literally inside
	// editing mode, never be swallowed as a shortcut.
	for _, r := range "aAy" {
		h.step(tea.KeyPressMsg{Text: string(r)})
	}
	if got := h.panel.t.labels[0].input.Value(); got != "platformaAy" {
		t.Errorf("buffer after typing aAy in editing mode = %q, want platformaAy", got)
	}
	if h.panel.t.adding != metaAddNone {
		t.Error("typing 'a' while editing should not have started add-mode")
	}
}

func TestEditingModeEscRevertsBufferAndReturnsToNavigation(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	h.step(tea.KeyPressMsg{Text: "z"})
	h.step(tea.KeyPressMsg{Text: "esc"})

	if h.panel == nil {
		t.Fatal("esc while editing should return to navigation, not close the panel")
	}
	if h.panel.t.editing {
		t.Error("esc while editing should leave editing mode")
	}
	if h.panel.t.labels[0].input.Value() != "platform" {
		t.Errorf("buffer after esc = %q, want reverted to platform", h.panel.t.labels[0].input.Value())
	}
}

func TestCopyKeyEqualsValueReturnsClipboardCmd(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	_, cmd := h.panel.Update(tea.KeyPressMsg{Text: "y"}, &h.ctrl)
	if cmd == nil {
		t.Fatal("'y' should return a SetClipboard cmd")
	}
}

func TestEscClosesPanel(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	closed, _ := h.panel.Update(tea.KeyPressMsg{Text: "esc"}, &h.ctrl)
	if !closed {
		t.Error("esc should report the panel closed")
	}
}

// TestPasteLandsInFocusedBuffer walks 26a's three buffers via PasteTarget:
// the add row's key and value (paste follows tab focus) and a row's own edit
// buffer — the resolver contract every host's pasteTarget() forwards to.
func TestPasteLandsInFocusedBuffer(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	h := newHost(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	if h.panel.PasteTarget() != nil {
		t.Fatal("navigation mode should expose no paste target")
	}

	h.step(tea.KeyPressMsg{Text: "a"})
	h.panel.PasteTarget()("env")
	if got := h.panel.t.addKeyInput.Value(); got != "env" {
		t.Fatalf("add key buffer = %q, want %q", got, "env")
	}
	h.step(tea.KeyPressMsg{Text: "tab"})
	h.panel.PasteTarget()("staging")
	if got := h.panel.t.addValueInput.Value(); got != "staging" {
		t.Fatalf("add value buffer = %q, want %q", got, "staging")
	}

	h.step(tea.KeyPressMsg{Code: tea.KeyEscape})
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !h.panel.t.editing {
		t.Fatal("expected '↵' on a row to enter editing mode")
	}
	h.panel.t.selectedRow().setBuffer("")
	h.panel.PasteTarget()("infra")
	if got := h.panel.t.selectedRow().input.Value(); got != "infra" {
		t.Fatalf("row edit buffer = %q, want %q", got, "infra")
	}
}

// TestFailedEditRestoresEditingModeWithErrorAndAttemptedValue covers
// docs/design README.md §26a's failure contract: "remain in edit mode with
// the attempted value intact and show the server error" — nothing is
// refetched, so the object's real (unchanged) value never overwrites the
// still-in-progress edit.
func TestFailedEditRestoresEditingModeWithErrorAndAttemptedValue(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"env": "stage"}, nil, nil)
	mut := &fakeMutator{err: errors.New("admission webhook denied the request")}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // navigation -> editing mode
	h.step(tea.KeyPressMsg{Text: "g"})          // "stage" -> "stageg"
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter}) // save (TierNone, executes and fails)

	if h.panel == nil {
		t.Fatal("the panel should stay open after a failed apply")
	}
	if !h.panel.t.editing {
		t.Error("a failed edit should re-enter editing mode, not fall back to navigation")
	}
	if got := h.panel.t.labels[0].input.Value(); got != "stageg" {
		t.Errorf("buffer after failure = %q, want the attempted stageg intact", got)
	}
	if got := h.panel.t.labels[0].current; got != "stage" {
		t.Errorf("current after failure = %q, want unchanged stage (nothing refetched on failure)", got)
	}
	if h.panel.t.lastError == "" {
		t.Error("expected the server error to be surfaced")
	}
	if h.panel.t.message != "" {
		t.Error("a failure should not also carry a success message")
	}
}

// TestFailedAddRestoresAddModeWithErrorAndAttemptedValues mirrors the edit
// case for the add sub-flow.
func TestFailedAddRestoresAddModeWithErrorAndAttemptedValues(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	mut := &fakeMutator{err: errors.New("field is immutable")}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Text: "a"})
	for _, r := range "tier" {
		h.step(tea.KeyPressMsg{Text: string(r)})
	}
	h.step(tea.KeyPressMsg{Text: "tab"})
	for _, r := range "gold" {
		h.step(tea.KeyPressMsg{Text: string(r)})
	}
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})

	if h.panel == nil {
		t.Fatal("the panel should stay open after a failed add")
	}
	if h.panel.t.adding != metaAddLabel {
		t.Error("a failed add should re-enter add-mode, not fall back to navigation")
	}
	if h.panel.t.addKeyInput.Value() != "tier" || h.panel.t.addValueInput.Value() != "gold" {
		t.Errorf("add buffers after failure = %q/%q, want tier/gold intact", h.panel.t.addKeyInput.Value(), h.panel.t.addValueInput.Value())
	}
	if len(h.panel.t.labels) != 1 {
		t.Errorf("labels = %+v, want no new row added on failure", h.panel.t.labels)
	}
	if h.panel.t.lastError == "" {
		t.Error("expected the server error to be surfaced")
	}
}

// TestFailedRemoveRestoresRowWithError covers the removal failure path: the
// row survives (never spliced out locally) and the error shows in
// navigation mode, since there's no buffer to fall back into editing.
func TestFailedRemoveRestoresRowWithError(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	mut := &fakeMutator{err: errors.New("etcdserver: request timed out")}
	h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

	h.step(tea.KeyPressMsg{Text: "D"})
	h.step(tea.KeyPressMsg{Text: "y"}) // confirm -> executes and fails

	if h.panel == nil {
		t.Fatal("the panel should stay open after a failed removal")
	}
	if len(h.panel.t.labels) != 1 || h.panel.t.labels[0].key != "team" {
		t.Errorf("labels = %+v, want team to survive the failed removal", h.panel.t.labels)
	}
	if h.panel.t.lastError == "" {
		t.Error("expected the server error to be surfaced")
	}
}

// TestVanishedObjectClosesPanelOnSuccess: when the object is gone by the
// time a successful commit tries to refresh, HandleResult reports closed —
// nothing left to refresh into.
func TestVanishedObjectClosesPanelOnSuccess(t *testing.T) {
	dep := metaDeployment("default", "nva-worker", map[string]string{"team": "platform"}, nil, nil)
	mut := &fakeMutator{}
	objs := map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}}
	h := newHost(t, mut, objs)

	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
	h.step(tea.KeyPressMsg{Text: "x"})
	objs[kube.KindDeployment] = nil // deleted concurrently
	mut.metaObjs = objs
	h.panel.cfg.Lister = fakeLister{objs: objs}
	h.step(tea.KeyPressMsg{Code: tea.KeyEnter})

	if h.panel != nil {
		t.Fatal("a successful commit against a vanished object should close the panel")
	}
}

// A multi-line (or non-UTF-8) annotation can't round-trip through the
// single-line buffer, so ↵ must refuse to open it rather than open it
// already flattened — where a second ↵ would patch the corruption back
// (review 09 H1).
func TestEnterRefusesAnnotationsThatCannotRoundTripTheSingleLineBuffer(t *testing.T) {
	cases := map[string]string{
		"pem":    "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
		"binary": string([]byte{0x00, 0xff, 0x10, 0x61}),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			dep := metaDeployment("default", "nva-worker", nil, map[string]string{"kute.dev/cert": value}, nil)
			mut := &fakeMutator{}
			h := newHost(t, mut, map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {dep}})

			h.step(tea.KeyPressMsg{Code: tea.KeyTab}) // focus ANNOTATIONS
			h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
			if h.panel.t.editing {
				t.Fatal("expected ↵ to refuse opening a multi-line/binary annotation in the single-line buffer")
			}
			h.step(tea.KeyPressMsg{Code: tea.KeyEnter})
			if len(mut.metaPatches) != 0 {
				t.Fatalf("expected no patch, got %v", mut.metaPatches)
			}
			if strip := ansi.Strip(h.panel.WillRunStrip(120, &h.ctrl)); !strings.Contains(strip, "can't edit it in place") {
				t.Errorf("expected the will-run strip to explain the refusal, got %q", strip)
			}
		})
	}
}

// TestSecretLastAppliedAnnotationIsMaskedAndUncopyable pins 09 H3 for 26a:
// on a Secret, kubectl's last-applied-configuration is the whole manifest —
// data and plaintext stringData — so its value must never reach the panel
// or the clipboard.
func TestSecretLastAppliedAnnotationIsMaskedAndUncopyable(t *testing.T) {
	const plaintext = "hunter2-plaintext"
	const b64 = "c3VwZXItc2VjcmV0LWI2NA=="
	lastApplied := `{"apiVersion":"v1","data":{"token":"` + b64 + `"},"kind":"Secret","metadata":{"name":"nva-worker","namespace":"default"},"stringData":{"password":"` + plaintext + `"}}`
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "nva-worker", Namespace: "default",
		Annotations: map[string]string{corev1.LastAppliedConfigAnnotation: lastApplied},
	}}
	session := &tui.Session{
		Registry: resources.DefaultRegistry(),
		Location: tui.Location{Context: "microk8s-cluster", Namespace: "default", Kind: kube.KindSecret},
		Theme:    tui.Dark(),
	}
	panel, ok := Open(Config{Session: session, Lister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindSecret: {sec}}}}, kube.KindSecret, "default", "nva-worker")
	if !ok {
		t.Fatal("Open returned false")
	}
	ctrl := actions.New(&fakeMutator{})
	panel.t.section = metaSectionAnnotations
	panel.t.annotationIdx = 0

	view := ansi.Strip(strings.Join(panel.PanelLines(160, &ctrl), "\n"))
	for _, leak := range []string{plaintext, b64, `"stringData"`} {
		if strings.Contains(view, leak) {
			t.Fatalf("panel shows Secret material %q:\n%s", leak, view)
		}
	}
	if !strings.Contains(view, "contains secret data") {
		t.Fatalf("expected the masked placeholder:\n%s", view)
	}
	if _, cmd := panel.Update(tea.KeyPressMsg{Text: "y"}, &ctrl); cmd != nil {
		t.Fatal("'y' on the masked last-applied row must not copy anything")
	}
	if _, cmd := panel.Update(tea.KeyPressMsg{Code: tea.KeyEnter}, &ctrl); cmd != nil || panel.t.editing {
		t.Fatal("the masked row must not open for editing")
	}
}
