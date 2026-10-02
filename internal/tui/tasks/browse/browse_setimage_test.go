package browse

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
)

func setImageAge(d time.Duration) metav1.Time { return metav1.NewTime(time.Now().Add(-d)) }

func replicasPtr(n int32) *int32 { return &n }

// twoContainerDeployment is "nva-worker": worker (the container under edit)
// plus a sidecar, so tab-cycling has something real to exercise.
func twoContainerDeployment(ns, name, workerImage string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1, CreationTimestamp: setImageAge(30 * 24 * time.Hour)},
		Spec: appsv1.DeploymentSpec{Replicas: replicasPtr(4), Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "worker", Image: workerImage},
				{Name: "metrics-sidecar", Image: "sidecar:0.9.1"},
			}},
		}},
		Status: appsv1.DeploymentStatus{Replicas: 4, ReadyReplicas: 4, UpdatedReplicas: 4, AvailableReplicas: 4, ObservedGeneration: 1},
	}
}

func replicaSetRevision(ns, name, deployment, image string, revision int, created time.Duration) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: setImageAge(created),
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: deployment}},
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": strconv.Itoa(revision)},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker", Image: image}},
		}}},
	}
}

// controllerRevisionFixture builds a StatefulSet/DaemonSet ControllerRevision
// — the apps/v1 rollout-history mechanism those two controllers use in place
// of a Deployment's owned ReplicaSets. Data.Raw mirrors the real patch shape
// (controllerRevisionContainerImage's doc comment) with one container.
func controllerRevisionFixture(ns, name, ownerKind, owner, container, image string, revision int64, created time.Duration) *appsv1.ControllerRevision {
	data, _ := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{{"name": container, "image": image}},
				},
			},
		},
	})
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, CreationTimestamp: setImageAge(created),
			OwnerReferences: []metav1.OwnerReference{{Kind: ownerKind, Name: owner}},
		},
		Revision: revision,
		Data:     runtime.RawExtension{Raw: data},
	}
}

func newSetImageModel(t *testing.T, mut *fakeMutator, objs map[kube.ResourceKind][]runtime.Object, prod bool) Model {
	t.Helper()
	lister := fakeLister{objs: objs}
	mut.setImageObjs = objs
	session := newSession()
	if prod {
		session.Config = config.Config{ProdContexts: []string{session.Location.Context}}
	}
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

func TestIOpensSetImagePrefilledToCurrentTag(t *testing.T) {
	m := newSetImageModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)

	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	if m.pendingSetImage == nil {
		t.Fatal("expected pendingSetImage set after 'i'")
	}
	t2 := m.pendingSetImage
	if t2.repo != "registry.nva.dev/nva-worker" || t2.input.Value() != "3.4.1" {
		t.Fatalf("repo/buffer = %q/%q, want registry.nva.dev/nva-worker/3.4.1", t2.repo, t2.input.Value())
	}
	if !t2.unchanged() {
		t.Fatal("expected the just-opened prefill to read as unchanged (same as current image)")
	}
	if !m.CapturingInput() {
		t.Fatal("expected CapturingInput true while the set-image panel is open")
	}
}

func TestTabCyclesContainersAndRecomputesHistory(t *testing.T) {
	m := newSetImageModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})

	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	t2 := m.pendingSetImage
	if t2.containerIdx != 1 || t2.repo != "sidecar" || t2.input.Value() != "0.9.1" {
		t.Fatalf("after tab: idx=%d repo=%q buffer=%q, want 1/sidecar/0.9.1", t2.containerIdx, t2.repo, t2.input.Value())
	}

	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if m.pendingSetImage.containerIdx != 0 {
		t.Fatalf("expected tab to wrap back to container 0, got %d", m.pendingSetImage.containerIdx)
	}
}

func TestSetImageIncludesAndUpdatesInitContainers(t *testing.T) {
	restartAlways := corev1.ContainerRestartPolicyAlways
	dep := twoContainerDeployment("default", "nva-worker", "worker:1.0")
	dep.Spec.Template.Spec.Containers = dep.Spec.Template.Spec.Containers[:1]
	dep.Spec.Template.Spec.InitContainers = []corev1.Container{
		{Name: "prepare", Image: "prepare:1.0"},
		{Name: "mesh", Image: "mesh:1.0", RestartPolicy: &restartAlways},
	}
	mut := &fakeMutator{}
	m := newSetImageModel(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})

	if got := len(m.pendingSetImage.containers); got != 3 {
		t.Fatalf("set-image targets = %d, want regular + init + native sidecar", got)
	}
	rendered := m.Render()
	if !strings.Contains(rendered, "prepare init") || !strings.Contains(rendered, "mesh sidecar") {
		t.Fatalf("init-container labels missing from set-image panel:\n%s", rendered)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if c := m.pendingSetImage.activeContainer(); c.Name != "prepare" || !c.initContainer || c.IsSidecar {
		t.Fatalf("first tab target = %+v, want conventional init container prepare", c)
	}
	m.pendingSetImage.setBuffer("2.0")
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.setImageInit) != 1 || !mut.setImageInit[0] {
		t.Fatalf("setImageInit = %v, want [true]", mut.setImageInit)
	}
	if got := dep.Spec.Template.Spec.InitContainers[0].Image; got != "prepare:2.0" {
		t.Fatalf("prepare image = %q, want prepare:2.0", got)
	}
	if c := m.pendingSetImage.activeContainer(); c.Name != "prepare" || !c.initContainer || c.Image != "prepare:2.0" {
		t.Fatalf("refreshed active target = %+v, want updated prepare init container", c)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if c := m.pendingSetImage.activeContainer(); c.Name != "mesh" || !c.initContainer || !c.IsSidecar {
		t.Fatalf("second tab target = %+v, want native sidecar mesh", c)
	}
	m.pendingSetImage.setBuffer("2.0")
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.setImageInit) != 2 || !mut.setImageInit[1] {
		t.Fatalf("setImageInit = %v, want [true true]", mut.setImageInit)
	}
	if got := dep.Spec.Template.Spec.InitContainers[1].Image; got != "mesh:2.0" {
		t.Fatalf("mesh image = %q, want mesh:2.0", got)
	}
}

func TestInitContainerHistoryReadsWorkloadRevisions(t *testing.T) {
	dep := twoContainerDeployment("default", "nva-worker", "worker:1.0")
	dep.Spec.Template.Spec.Containers = dep.Spec.Template.Spec.Containers[:1]
	dep.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "prepare", Image: "prepare:2.0"}}
	old := replicaSetRevision("default", "nva-worker-r1", "nva-worker", "worker:1.0", 1, 2*time.Hour)
	old.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "prepare", Image: "prepare:1.0"}}
	current := replicaSetRevision("default", "nva-worker-r2", "nva-worker", "worker:1.0", 2, time.Hour)
	current.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "prepare", Image: "prepare:2.0"}}
	m := newSetImageModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep},
		kube.KindReplicaSet: {old, current},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})

	history := m.pendingSetImage.history
	if len(history) != 2 || history[0].tag != "2.0" || history[1].tag != "1.0" {
		t.Fatalf("init-container history = %+v, want current 2.0 and rollback target 1.0", history)
	}
}

func TestControllerRevisionImageReadsInitContainers(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers":     []map[string]any{{"name": "app", "image": "app:1.0"}},
			"initContainers": []map[string]any{{"name": "prepare", "image": "prepare:2.0"}},
		}}},
	})
	if err != nil {
		t.Fatalf("marshal ControllerRevision fixture: %v", err)
	}
	cr := &appsv1.ControllerRevision{Data: runtime.RawExtension{Raw: data}}
	if got := controllerRevisionContainerImage(cr, "prepare", true); got != "prepare:2.0" {
		t.Fatalf("init-container image = %q, want prepare:2.0", got)
	}
}

func TestHistoryUpDownPicksTagIntoBuffer(t *testing.T) {
	dep := twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.2")
	rsCur := replicaSetRevision("default", "nva-worker-r43", "nva-worker", "registry.nva.dev/nva-worker:3.4.2", 43, 2*24*time.Hour)
	rsOld := replicaSetRevision("default", "nva-worker-r42", "nva-worker", "registry.nva.dev/nva-worker:3.4.1", 42, 21*24*time.Hour)

	m := newSetImageModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {dep},
		kube.KindReplicaSet: {rsCur, rsOld},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})

	if len(m.pendingSetImage.history) != 2 {
		t.Fatalf("history = %+v, want 2 entries (current + rollback target)", m.pendingSetImage.history)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "down"})
	if m.pendingSetImage.input.Value() != "3.4.1" {
		t.Fatalf("buffer after down = %q, want 3.4.1", m.pendingSetImage.input.Value())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "up"})
	if m.pendingSetImage.input.Value() != "3.4.2" {
		t.Fatalf("buffer after up = %q, want 3.4.2 (back to current)", m.pendingSetImage.input.Value())
	}
}

func TestCtrlETogglesFullRefEditing(t *testing.T) {
	m := newSetImageModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})

	m = step(t, m, tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	t2 := m.pendingSetImage
	if !t2.fullRef || t2.input.Value() != "registry.nva.dev/nva-worker:3.4.1" {
		t.Fatalf("after ctrl-e: fullRef=%v buffer=%q, want true/registry.nva.dev/nva-worker:3.4.1", t2.fullRef, t2.input.Value())
	}

	m = step(t, m, tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	t2 = m.pendingSetImage
	if t2.fullRef || t2.input.Value() != "3.4.1" || t2.repo != "registry.nva.dev/nva-worker" {
		t.Fatalf("after second ctrl-e: fullRef=%v buffer=%q repo=%q, want false/3.4.1/registry.nva.dev/nva-worker", t2.fullRef, t2.input.Value(), t2.repo)
	}
}

func TestDigestPinnedImageTagEditDropsOldDigest(t *testing.T) {
	const digest = "sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"
	mut := &fakeMutator{}
	m := newSetImageModel(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "worker", "busybox:1.37@"+digest)},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})

	if got := m.pendingSetImage.repo; got != "busybox" {
		t.Fatalf("repo = %q, want busybox", got)
	}
	if got := m.pendingSetImage.input.Value(); got != "1.37" {
		t.Fatalf("tag buffer = %q, want 1.37", got)
	}
	if !m.pendingSetImage.unchanged() {
		t.Fatal("an untouched tag+digest reference should remain unchanged")
	}

	m.pendingSetImage.setBuffer("1.36")
	if got := m.pendingSetImage.composedImage(); got != "busybox:1.36" {
		t.Fatalf("composed image = %q, want busybox:1.36 without the old digest", got)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.setImages) != 1 || mut.setImages[0] != "default/worker worker=busybox:1.36" {
		t.Fatalf("setImages = %v, want digest-free tag update", mut.setImages)
	}
}

func TestIIsTypeableInsideSetImageEditor(t *testing.T) {
	m := newSetImageModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "worker", "worker:1.0")},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	if got := m.pendingSetImage.input.Value(); got != "1.0i" {
		t.Fatalf("tag buffer = %q, want typed i appended", got)
	}
}

// TestLeftRightMoveCursorForMidBufferEditing pins the fix for a reported
// bug: ←/→ did nothing, since the original implementation only ever
// appended/backspaced at the end of the buffer. Prefilled tag is "3.4.1"
// (cursor starts at the end, position 5); ← twice parks it between the two
// "4"s... i.e. after "3." (position 2), where a typed digit must insert
// in the middle, not append at the end.
func TestLeftRightMoveCursorForMidBufferEditing(t *testing.T) {
	m := newSetImageModel(t, &fakeMutator{}, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	if m.pendingSetImage.input.Position() != len("3.4.1") {
		t.Fatalf("cursor = %d, want prefilled cursor at the end (%d)", m.pendingSetImage.input.Position(), len("3.4.1"))
	}

	for range 3 {
		m = step(t, m, tea.KeyPressMsg{Text: "left"})
	}
	if m.pendingSetImage.input.Position() != 2 {
		t.Fatalf("cursor after 3x left = %d, want 2", m.pendingSetImage.input.Position())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "9"})
	if m.pendingSetImage.input.Value() != "3.94.1" {
		t.Fatalf("buffer after mid-cursor insert = %q, want 3.94.1", m.pendingSetImage.input.Value())
	}
	if m.pendingSetImage.input.Position() != 3 {
		t.Fatalf("cursor after insert = %d, want 3 (advances past the inserted rune)", m.pendingSetImage.input.Position())
	}

	m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	if m.pendingSetImage.input.Value() != "3.4.1" || m.pendingSetImage.input.Position() != 2 {
		t.Fatalf("after backspace: buffer=%q cursor=%d, want 3.4.1/2", m.pendingSetImage.input.Value(), m.pendingSetImage.input.Position())
	}

	for range 5 {
		m = step(t, m, tea.KeyPressMsg{Text: "right"})
	}
	if m.pendingSetImage.input.Position() != len("3.4.1") {
		t.Fatalf("cursor after overshooting right = %d, want clamped at %d", m.pendingSetImage.input.Position(), len("3.4.1"))
	}
}

func TestEnterOnUnchangedTagIsNoOp(t *testing.T) {
	mut := &fakeMutator{}
	m := newSetImageModel(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if m.pendingSetImage == nil {
		t.Fatal("expected the panel to stay open — re-entering the current tag must no-op, not apply")
	}
	if len(mut.setImages) != 0 {
		t.Fatalf("expected no SetImage call, got %v", mut.setImages)
	}
}

func TestEnterCommitsSetImageThroughMutatorNonProd(t *testing.T) {
	mut := &fakeMutator{}
	m := newSetImageModel(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	m = step(t, m, tea.KeyPressMsg{Text: "2"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.actions.Active() {
		t.Fatal("non-PROD set-image is TierNone and should execute immediately, not show a confirm")
	}
	want := "default/nva-worker worker=registry.nva.dev/nva-worker:3.4.12"
	if len(mut.setImages) != 1 || mut.setImages[0] != want {
		t.Fatalf("setImages = %v, want [%q]", mut.setImages, want)
	}
	// docs/design README.md §26a's contract, retrofitted onto 24a: "confirm
	// → execute → refresh → show result → remain on screen" — the panel
	// stays open, refreshed from the real object (now reading unchanged
	// since the applied tag is the new current one), with an inline success
	// message.
	if m.pendingSetImage == nil {
		t.Fatal("the panel should stay open after a successful apply")
	}
	if !m.pendingSetImage.unchanged() {
		t.Error("the refreshed buffer should read as unchanged (matches the just-applied image)")
	}
	if wantMsg := "set image: worker=registry.nva.dev/nva-worker:3.4.12"; m.pendingSetImage.message != wantMsg {
		t.Errorf("message = %q, want %q", m.pendingSetImage.message, wantMsg)
	}
}

func TestEnterInProdShowsInlineConfirmBeforeApplying(t *testing.T) {
	mut := &fakeMutator{}
	m := newSetImageModel(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, true)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	m = step(t, m, tea.KeyPressMsg{Text: "2"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.pendingSetImage == nil {
		t.Fatal("the panel should stay open under the inline confirm, not close to the generic table+confirm view")
	}
	if !m.actions.Active() {
		t.Fatal("expected a PROD set-image apply to land in actions.Controller's inline y/N confirm")
	}
	if len(mut.setImages) != 0 {
		t.Fatal("expected no SetImage call yet — still awaiting the y/N confirm")
	}

	kb := m.Keybar()
	want := "kubectl set image deploy/nva-worker worker=registry.nva.dev/nva-worker:3.4.12 -n default"
	if !strings.Contains(kb.RightNote, want) {
		t.Fatalf("RightNote = %q, want it to contain %q", kb.RightNote, want)
	}
	if kb.PillText != "CONFIRM" {
		t.Errorf("PillText = %q, want CONFIRM while the y/N is showing", kb.PillText)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.setImages) != 1 {
		t.Fatalf("expected the confirm's 'y' to execute SetImage, got %v", mut.setImages)
	}
	if m.pendingSetImage == nil {
		t.Fatal("the panel should stay open after the confirmed apply")
	}
	if wantMsg := "set image: worker=registry.nva.dev/nva-worker:3.4.12"; m.pendingSetImage.message != wantMsg {
		t.Errorf("message = %q, want %q", m.pendingSetImage.message, wantMsg)
	}
}

// TestFailedNonProdApplyKeepsPanelOpenWithError covers handleSetImageResult's
// failure path: the buffer keeps the attempted value, nothing is refetched,
// and the server error surfaces via the will-run strip instead of vanishing
// silently.
func TestFailedNonProdApplyKeepsPanelOpenWithError(t *testing.T) {
	mut := &fakeMutator{err: errors.New("admission webhook denied the request")}
	m := newSetImageModel(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	m = step(t, m, tea.KeyPressMsg{Text: "2"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.pendingSetImage == nil {
		t.Fatal("the panel should stay open after a failed apply")
	}
	if m.pendingSetImage.input.Value() != "3.4.12" {
		t.Errorf("buffer = %q, want the attempted value 3.4.12 to survive", m.pendingSetImage.input.Value())
	}
	if m.pendingSetImage.lastError == "" {
		t.Error("expected lastError to carry the server's error")
	}
	if m.pendingSetImage.message != "" {
		t.Error("expected no success message on failure")
	}
}

// TestCancellingProdConfirmRevertsBufferAndKeepsPanelOpen covers 'n' during
// the PROD y/N: no mutator call, the panel stays open, and the buffer reverts
// to the real current tag rather than leaving the attempted edit sitting
// there unconfirmed.
func TestCancellingProdConfirmRevertsBufferAndKeepsPanelOpen(t *testing.T) {
	mut := &fakeMutator{}
	m := newSetImageModel(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, true)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	m = step(t, m, tea.KeyPressMsg{Text: "2"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	m = step(t, m, tea.KeyPressMsg{Text: "n"})

	if m.actions.Active() {
		t.Error("cancel should end the confirm")
	}
	if m.pendingSetImage == nil {
		t.Fatal("cancelling the confirm should leave the panel open")
	}
	if m.pendingSetImage.input.Value() != "3.4.1" {
		t.Errorf("buffer after cancel = %q, want reverted to the real current tag 3.4.1", m.pendingSetImage.input.Value())
	}
	if len(mut.setImages) != 0 {
		t.Errorf("setImages = %v, want none", mut.setImages)
	}
}

func TestEscCancelsSetImagePanel(t *testing.T) {
	mut := &fakeMutator{}
	m := newSetImageModel(t, mut, map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}, false)
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})

	if m.pendingSetImage != nil {
		t.Fatal("expected pendingSetImage cleared after esc")
	}
	if len(mut.setImages) != 0 {
		t.Fatalf("expected no SetImage call after cancel, got %v", mut.setImages)
	}
}

func TestIAppliesToStatefulSetsAndDaemonSets(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", CreationTimestamp: setImageAge(10 * 24 * time.Hour)},
		Spec: appsv1.StatefulSetSpec{Replicas: replicasPtr(2), Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "postgres:15"}}},
		}},
		Status: appsv1.StatefulSetStatus{Replicas: 2, ReadyReplicas: 2},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindStatefulSet: {sts}}}
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindStatefulSet
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	if m.pendingSetImage == nil || m.pendingSetImage.input.Value() != "15" {
		t.Fatalf("expected StatefulSet's 'i' to open prefilled to tag 15, got %+v", m.pendingSetImage)
	}
	// No ControllerRevision fixtures seeded — the single-row "current"
	// fallback ownRevisionHistory takes when no revision object exists yet.
	if len(m.pendingSetImage.history) != 1 || m.pendingSetImage.history[0].tag != "15" {
		t.Fatalf("history = %+v, want a single fallback current-tag row", m.pendingSetImage.history)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "6"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	want := "default/db db=postgres:156"
	if len(mut.setImages) != 1 || mut.setImages[0] != want {
		t.Fatalf("setImages = %v, want [%q]", mut.setImages, want)
	}
}

func TestIAppliesToCronJobFutureJobs(t *testing.T) {
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", CreationTimestamp: setImageAge(10 * 24 * time.Hour)},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 2 * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "job", Image: "backup:1.0"}}},
			}}},
		},
	}
	objs := map[kube.ResourceKind][]runtime.Object{kube.KindCronJob: {cronJob}}
	lister := fakeLister{objs: objs}
	mut := &fakeMutator{setImageObjs: objs}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())

	keybar := m.Keybar()
	foundSetImage := false
	for _, group := range keybar.Groups {
		for _, hint := range group {
			foundSetImage = foundSetImage || hint.Key == "i" && hint.Label == "set image"
		}
	}
	if !foundSetImage {
		t.Fatalf("CronJob keybar = %+v, want set-image hint", keybar)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	if m.pendingSetImage == nil || m.pendingSetImage.input.Value() != "1.0" {
		t.Fatalf("expected CronJob's 'i' to open prefilled to tag 1.0, got %+v", m.pendingSetImage)
	}
	if got := m.Keybar().RightNote; !strings.Contains(got, "future jobs") || !strings.Contains(got, "running jobs unaffected") {
		t.Fatalf("set-image RightNote = %q, want future/running Job consequence", got)
	}
	if got := m.Render(); !strings.Contains(got, "same image — apply is a no-op") || strings.Contains(got, "use rollout restart") {
		t.Fatalf("CronJob unchanged-image message is not CronJob-specific:\n%s", got)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "1"})
	if got := m.Render(); !strings.Contains(got, "kubectl set image cronjob/nightly job=backup:1.01 -n default") || !strings.Contains(got, "future jobs only · running jobs unaffected") {
		t.Fatalf("CronJob command/consequence missing from set-image panel:\n%s", got)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	want := "default/nightly job=backup:1.01"
	if len(mut.setImages) != 1 || mut.setImages[0] != want {
		t.Fatalf("setImages = %v, want [%q]", mut.setImages, want)
	}
	if m.pendingSetImage == nil || !m.pendingSetImage.unchanged() {
		t.Fatal("CronJob set-image should refresh and remain open on the applied image")
	}
}

func TestSetImageDoesNotRevertWhileInformerCacheCatchesUp(t *testing.T) {
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", CreationTimestamp: setImageAge(10 * 24 * time.Hour)},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 2 * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "job", Image: "backup:1.0.0"}}},
			}}},
		},
	}
	objs := map[kube.ResourceKind][]runtime.Object{kube.KindCronJob: {cronJob}}
	lister := fakeLister{objs: objs}
	// Deliberately leave setImageObjs nil: the API result arrives while this
	// lister still contains the pre-write informer snapshot.
	mut := &fakeMutator{}
	session := newSession()
	session.Location.Kind = kube.KindCronJob
	m := New(Config{Session: session, Lister: lister, Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.load()())
	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	m = step(t, m, tea.KeyPressMsg{Text: "1"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if got := m.pendingSetImage.input.Value(); got != "1.0.01" {
		t.Fatalf("input after successful apply = %q, want submitted tag 1.0.01", got)
	}
	if m.pendingSetImage.awaitingRefresh == nil {
		t.Fatal("expected the panel to await informer confirmation of the applied image")
	}
	if !m.pendingSetImage.unchanged() {
		t.Fatal("the submitted image should be treated as current while awaiting the watch update")
	}

	// A same-kind event for another object must not confirm stale data.
	updated, _ := m.Update(kube.ResourceChangedMsg{Kind: kube.KindCronJob})
	m = *updated.(*Model)
	if m.pendingSetImage.awaitingRefresh == nil || m.pendingSetImage.input.Value() != "1.0.01" {
		t.Fatal("an unrelated CronJob event should leave the submitted image awaiting confirmation")
	}

	setContainerImage(cronJob, "job", "backup:1.0.01", false)
	updated, _ = m.Update(kube.ResourceChangedMsg{Kind: kube.KindCronJob})
	m = *updated.(*Model)
	if m.pendingSetImage.awaitingRefresh != nil {
		t.Fatal("expected matching informer update to confirm the applied image")
	}
	if got := m.pendingSetImage.input.Value(); got != "1.0.01" {
		t.Fatalf("input after watch-confirmed refresh = %q, want 1.0.01", got)
	}
	if got := m.pendingSetImage.activeContainer().Image; got != "backup:1.0.01" {
		t.Fatalf("cached container image = %q, want backup:1.0.01", got)
	}
}

// TestStatefulSetHistoryReadsControllerRevisions pins the fix for a reported
// bug: the Deployments screen showed "rollout history · rollback target"
// rows (from owned ReplicaSets), but StatefulSets only ever showed the
// current tag — because ownRevisionHistory special-cased every non-
// Deployment kind to a single row. StatefulSet/DaemonSet use
// ControllerRevisions (apps/v1), not ReplicaSets, for the same rollout
// history; this confirms that source now feeds real rollback-target rows.
func TestStatefulSetHistoryReadsControllerRevisions(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default", CreationTimestamp: setImageAge(30 * 24 * time.Hour)},
		Spec: appsv1.StatefulSetSpec{Replicas: replicasPtr(2), Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "postgres:15.2"}}},
		}},
		Status: appsv1.StatefulSetStatus{Replicas: 2, ReadyReplicas: 2},
	}
	crOld := controllerRevisionFixture("default", "db-abc12", "StatefulSet", "db", "db", "postgres:15.1", 41, 20*24*time.Hour)
	crCur := controllerRevisionFixture("default", "db-def34", "StatefulSet", "db", "db", "postgres:15.2", 42, 2*24*time.Hour)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindStatefulSet:        {sts},
		kube.KindControllerRevision: {crOld, crCur},
	}}
	session := newSession()
	session.Location.Kind = kube.KindStatefulSet
	m := New(Config{Session: session, Lister: lister, Mutator: &fakeMutator{}})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())
	m = step(t, m, tea.KeyPressMsg{Text: "i"})

	h := m.pendingSetImage.history
	if len(h) != 2 {
		t.Fatalf("history = %+v, want 2 entries (current + rollback target)", h)
	}
	if h[0].tag != "15.2" || h[0].from != "rev 42 · this statefulset" {
		t.Fatalf("history[0] = %+v, want current rev 42 (15.2)", h[0])
	}
	if h[1].tag != "15.1" || h[1].from != "rollout history · rollback target" {
		t.Fatalf("history[1] = %+v, want rev 41 rollback target (15.1)", h[1])
	}
}

func TestINoOpsWithoutMutator(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {twoContainerDeployment("default", "nva-worker", "registry.nva.dev/nva-worker:3.4.1")},
	}}
	session := newSession()
	session.Location.Kind = kube.KindDeployment
	m := New(Config{Session: session, Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "i"})
	if m.pendingSetImage != nil {
		t.Fatal("expected 'i' to no-op without a mutator wired")
	}
}

// startedKindsLister records every ListRaw and reports only started's
// (kind, namespace) caches as running — the shape *kube.Cluster gives a
// breadth-first reader through tui.KindStartedChecker.
type startedKindsLister struct {
	fakeLister
	started map[kube.ResourceKind]string
	reads   []string
}

func (l *startedKindsLister) KindStarted(kind kube.ResourceKind, namespace string) bool {
	ns, ok := l.started[kind]
	return ok && ns == namespace
}

func (l *startedKindsLister) ListRaw(ctx context.Context, kind kube.ResourceKind, namespace string) ([]runtime.Object, error) {
	l.reads = append(l.reads, string(kind)+"@"+namespace)
	return l.fakeLister.ListRaw(ctx, kind, namespace)
}

// TestCrossWorkloadHistoryReadsOnlyStartedCaches pins review 06 M5: set
// image's FROM sightings must never start an informer for a kind the screen
// isn't showing — editing a Deployment used to list StatefulSets and
// DaemonSets cluster-wide — and under --namespace-scoped, where only the
// edited namespace's cache runs, must read that one rather than the
// cluster-wide cache the mode exists to avoid.
func TestCrossWorkloadHistoryReadsOnlyStartedCaches(t *testing.T) {
	other := twoContainerDeployment("nva-stage", "nva-worker-canary", "registry.example/nva-worker:v1.4.0")
	for _, tc := range []struct {
		name    string
		started map[kube.ResourceKind]string
		want    []string
	}{
		{"cluster-wide", map[kube.ResourceKind]string{kube.KindDeployment: ""}, []string{"Deployment@"}},
		{"namespace-scoped", map[kube.ResourceKind]string{kube.KindDeployment: "nva-stage"}, []string{"Deployment@nva-stage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lister := &startedKindsLister{
				fakeLister: fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindDeployment: {other}}},
				started:    tc.started,
			}
			got := crossWorkloadHistory(t.Context(), lister, kube.KindDeployment, "nva-stage", "nva-worker", "registry.example/nva-worker", "v1.3.0")
			if len(got) != 1 || got[0].tag != "v1.4.0" {
				t.Errorf("sightings = %+v, want the started Deployment cache's v1.4.0", got)
			}
			if strings.Join(lister.reads, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("reads = %v, want %v — only already-started caches may be read", lister.reads, tc.want)
			}
		})
	}
}
