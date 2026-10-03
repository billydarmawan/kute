package configmapdata

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

func plain(s string) string { return ansi.Strip(s) }

type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objs[kind], nil
}

// fakeMutator's PatchConfigMapData mutates the shared configmap object in
// place, and RolloutRestart records every call, so ctrl-r's restart-chaining
// contract is directly assertable — the same fidelity fix secretdata_test.go's
// own fakeMutator makes for 27b's tests.
type fakeMutator struct {
	cm              *corev1.ConfigMap
	err             error
	rolloutRestarts []string // "kind/namespace/name"
	// restartErr fails RolloutRestart for the named "kind/namespace/name".
	restartErr map[string]error
	// dataResourceVersions records every PatchConfigMapData precondition.
	dataResourceVersions []string
}

func (f *fakeMutator) DeleteResource(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeMutator) DeleteResourceForced(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeMutator) RolloutRestart(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	target := string(kind) + "/" + namespace + "/" + name
	if err := f.restartErr[target]; err != nil {
		return err
	}
	f.rolloutRestarts = append(f.rolloutRestarts, target)
	return nil
}
func (f *fakeMutator) HelmRollback(context.Context, string, string, int) error {
	return nil
}
func (f *fakeMutator) RolloutUndo(context.Context, string, string, int) error {
	return nil
}
func (f *fakeMutator) Cordon(context.Context, string, bool) error { return nil }
func (f *fakeMutator) Drain(context.Context, string) (int, error) { return 0, nil }
func (f *fakeMutator) Scale(context.Context, kube.ResourceKind, string, string, int32) error {
	return nil
}
func (f *fakeMutator) SetImage(context.Context, kube.ResourceKind, string, string, string, string, bool) error {
	return nil
}
func (f *fakeMutator) SetResources(context.Context, kube.ResourceKind, string, string, string, kube.ResourceEdits, bool) error {
	return nil
}
func (f *fakeMutator) PatchMeta(context.Context, kube.ResourceKind, string, string, bool, string, string, bool) error {
	return nil
}
func (f *fakeMutator) PatchSecretData(context.Context, string, string, string, string, string, bool) (string, error) {
	return "", nil
}
func (f *fakeMutator) PatchConfigMapData(_ context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error) {
	f.dataResourceVersions = append(f.dataResourceVersions, resourceVersion)
	if f.err != nil {
		return "", f.err
	}
	if f.cm == nil || f.cm.Name != name || f.cm.Namespace != namespace {
		return "", nil
	}
	// A fixture that carries a resourceVersion gets the API server's
	// precondition check: a mismatch is a 409, a match bumps it.
	if f.cm.ResourceVersion != "" {
		if resourceVersion != f.cm.ResourceVersion {
			return "", apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, errors.New("the object has been modified"))
		}
		n, _ := strconv.Atoi(f.cm.ResourceVersion)
		f.cm.ResourceVersion = strconv.Itoa(n + 1)
	}
	if f.cm.Data == nil {
		f.cm.Data = map[string]string{}
	}
	if remove {
		delete(f.cm.Data, key)
	} else {
		f.cm.Data[key] = value
	}
	return f.cm.ResourceVersion, nil
}

// Flux verbs (§30a). Recorded but inert: no test in this package drives
// them, and the Mutator contract requires them.
func (f *fakeMutator) SetFluxSuspend(_ context.Context, kind kube.ResourceKind, namespace, name string, suspend bool) error {
	return nil
}

func (f *fakeMutator) RequestFluxReconcile(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	return nil
}
func (f *fakeMutator) RequestArgoRefresh(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	return nil
}
func (f *fakeMutator) RequestArgoSync(_ context.Context, kind kube.ResourceKind, namespace, name, revision, initiator string) error {
	return nil
}

func (f *fakeMutator) RenewCertificate(_ context.Context, namespace, name string) error {
	return nil
}
func (f *fakeMutator) RetryJob(_ context.Context, namespace, name, newName, creator string, at time.Time) error {
	return nil
}
func (f *fakeMutator) ReplaceJob(_ context.Context, namespace, name string) error { return nil }
func (f *fakeMutator) SetJobSuspend(_ context.Context, namespace, name string, suspend bool) error {
	return nil
}
func (f *fakeMutator) TriggerCronJob(_ context.Context, namespace, name, newJobName, creator string, at time.Time) error {
	return nil
}
func (f *fakeMutator) SetCronJobSuspend(_ context.Context, namespace, name string, suspend bool, resourceVersion string, currentGeneration int64, at time.Time) error {
	return nil
}
func (f *fakeMutator) SetCronJobSchedule(_ context.Context, namespace, name string, edit kube.CronJobScheduleEdit) (kube.CronJobScheduleResult, error) {
	return kube.CronJobScheduleResult{}, nil
}

func newSession() *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster"}}
}

func prodSession() *tui.Session {
	sess := newSession()
	sess.Location.Context = "prod-cluster"
	sess.Config = config.Config{ProdContexts: []string{"prod-cluster"}}
	return sess
}

func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c != nil {
				m = step(t, m, c())
			}
		}
		return m
	}
	updated, cmd := m.Update(msg)
	next := *updated.(*Model)
	if cmd != nil {
		return step(t, next, cmd())
	}
	return next
}

func cmObj(namespace, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
}

func deploymentEnvFrom(namespace, name, cmName string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "app",
						EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}}},
					}},
				},
			},
		},
	}
}

func statefulSetVolume(namespace, name, cmName string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{{
						Name:         "config",
						VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}},
					}},
				},
			},
		},
	}
}

func newModel(t *testing.T, sess *tui.Session, cm *corev1.ConfigMap, mut *fakeMutator, extra map[kube.ResourceKind][]runtime.Object) Model {
	t.Helper()
	objs := map[kube.ResourceKind][]runtime.Object{kube.KindConfigMap: {cm}}
	for k, v := range extra {
		objs[k] = v
	}
	lister := fakeLister{objs: objs}
	var mutator kube.Mutator
	if mut != nil {
		mutator = mut
	}
	m := New(Config{Session: sess, Lister: lister, Mutator: mutator, Namespace: cm.Namespace, Name: cm.Name})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

func TestLoadRendersRowsAndKeyCount(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{
		"LOG_LEVEL": "info",
		"FEATURE_X": "on",
	})
	m := newModel(t, newSession(), cm, nil, nil)

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if len(m.keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(m.keys))
	}
	body := plain(m.gridBody(m.Theme(), 120, 20))
	if !strings.Contains(body, "info") || !strings.Contains(body, "on") {
		t.Fatalf("expected real plaintext values rendered (ConfigMap values aren't sensitive), got:\n%s", body)
	}
	strip := plain(strings.Join(m.Strips(120), "\n"))
	if !strings.Contains(strip, "2 keys") {
		t.Fatalf("expected the 2 keys strip, got %q", strip)
	}
	if !strings.Contains(strip, "no consumers found") {
		t.Fatalf("expected the empty consumer strip, got %q", strip)
	}
}

func TestConsumerStripListsEnvAndVolumeReferences(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	extra := map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment:  {deploymentEnvFrom("nva-stage", "nva-worker", "nva-config")},
		kube.KindStatefulSet: {statefulSetVolume("nva-stage", "nva-db", "nva-config")},
	}
	m := newModel(t, newSession(), cm, nil, extra)

	if len(m.consumers) != 2 {
		t.Fatalf("expected 2 consumers, got %+v", m.consumers)
	}
	strip := plain(strings.Join(m.Strips(120), "\n"))
	if !strings.Contains(strip, "deploy/nva-worker") || !strings.Contains(strip, "↗ env") {
		t.Fatalf("expected the Deployment env consumer in the strip, got %q", strip)
	}
	if !strings.Contains(strip, "sts/nva-db") || !strings.Contains(strip, "↗ volume") {
		t.Fatalf("expected the StatefulSet volume consumer in the strip, got %q", strip)
	}
}

func TestAddKeyNonProdAppliesImmediatelyAndRefreshes(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	if m.adding == nil {
		t.Fatal("expected 'a' to open the add row")
	}
	for _, r := range "FEATURE_X" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	for _, r := range "on" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.actions.Active() {
		t.Fatal("expected non-prod add to apply immediately, no confirm")
	}
	if m.adding != nil {
		t.Fatal("expected the add row to close after a successful apply")
	}
	if cm.Data["FEATURE_X"] != "on" {
		t.Fatalf("mutator did not receive the add, cm.Data = %+v", cm.Data)
	}
	if m.message != "added FEATURE_X" {
		t.Fatalf("message = %q, want %q", m.message, "added FEATURE_X")
	}
	if len(m.keys) != 2 {
		t.Fatalf("expected the refreshed grid to show 2 keys, got %d", len(m.keys))
	}
	if key, ok := m.selectedKeyRow(); !ok || key.key != "FEATURE_X" {
		t.Fatalf("expected focus to follow the added key, got %+v (ok=%v)", key, ok)
	}
}

// 'a' with a key that already exists would replace its value — it must ask,
// even outside PROD, and say "updated" rather than "added".
func TestAddExistingKeyAsksBeforeOverwriting(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"MODE": "orig"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	for _, r := range "MODE" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	m = step(t, m, tea.KeyPressMsg{Text: "x"})
	if strip := plain(m.willRunStrip(m.Theme(), 120)); !strings.Contains(strip, "overwrites existing MODE") {
		t.Fatalf("will-run strip = %q, want the overwrite warning", strip)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if cm.Data["MODE"] != "orig" {
		t.Fatalf("an add over an existing key applied without a confirm: %q", cm.Data["MODE"])
	}
	if !m.actions.Active() {
		t.Fatal("expected an add over an existing key to ask y/N")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if cm.Data["MODE"] != "x" {
		t.Fatalf("confirmed overwrite not applied: %q", cm.Data["MODE"])
	}
	if m.message != "updated MODE" {
		t.Fatalf("message = %q, want %q", m.message, "updated MODE")
	}
}

func TestAddKeyProdRequiresConfirmThenApplies(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", nil)
	mut := &fakeMutator{cm: cm}
	m := newModel(t, prodSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	for _, r := range "LOG_LEVEL" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	for _, r := range "debug" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if !m.actions.Active() {
		t.Fatal("expected a PROD add to require confirmation before applying")
	}
	if _, ok := cm.Data["LOG_LEVEL"]; ok {
		t.Fatal("expected the mutator not to be called before confirming")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if cm.Data["LOG_LEVEL"] != "debug" {
		t.Fatalf("expected the add applied after confirming, cm.Data = %+v", cm.Data)
	}
	if m.message != "added LOG_LEVEL" {
		t.Fatalf("message = %q, want %q", m.message, "added LOG_LEVEL")
	}
}

func TestEnterOnSingleLineRowEditsInPlaceNonProdAppliesImmediately(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if m.editing == nil {
		t.Fatal("expected '↵' to open the in-place edit row")
	}
	if m.editing.valueInput.Value() != "info" {
		t.Fatalf("expected the buffer pre-filled with the current value, got %q", m.editing.valueInput.Value())
	}
	for range "info" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range "debug" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.editing != nil {
		t.Fatal("expected the edit row to close after a successful apply")
	}
	if cm.Data["LOG_LEVEL"] != "debug" {
		t.Fatalf("expected the edit applied, cm.Data = %+v", cm.Data)
	}
	if m.message != "updated LOG_LEVEL" {
		t.Fatalf("message = %q, want %q", m.message, "updated LOG_LEVEL")
	}
}

func TestCtrlRChainsRolloutRestartOfEveryConsumer(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm}
	extra := map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment:  {deploymentEnvFrom("nva-stage", "nva-worker", "nva-config")},
		kube.KindStatefulSet: {statefulSetVolume("nva-stage", "nva-db", "nva-config")},
	}
	m := newModel(t, newSession(), cm, mut, extra)
	if len(m.consumers) != 2 {
		t.Fatalf("expected 2 consumers wired for the test, got %+v", m.consumers)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	for range "info" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range "debug" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = step(t, m, tea.KeyPressMsg{Text: "y"})

	if cm.Data["LOG_LEVEL"] != "debug" {
		t.Fatalf("expected the edit applied, cm.Data = %+v", cm.Data)
	}
	want := map[string]bool{"Deployment/nva-stage/nva-worker": true, "StatefulSet/nva-stage/nva-db": true}
	if len(mut.rolloutRestarts) != 2 {
		t.Fatalf("rolloutRestarts = %v, want 2 entries", mut.rolloutRestarts)
	}
	for _, r := range mut.rolloutRestarts {
		if !want[r] {
			t.Fatalf("unexpected restart target %q, want one of %v", r, want)
		}
	}
	if !strings.Contains(m.message, "restarted 2 consumers") {
		t.Fatalf("message = %q, want it to mention restarting 2 consumers", m.message)
	}
}

// When the patch lands but one consumer's restart fails, the screen must say
// exactly that — not restore the editor as if nothing applied, which would
// invite a retry that restarts every consumer a second time.
func TestCtrlRPartialRestartFailureReportsWhatLanded(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm, restartErr: map[string]error{
		"StatefulSet/nva-stage/nva-db": errors.New("forbidden"),
	}}
	extra := map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment:  {deploymentEnvFrom("nva-stage", "nva-worker", "nva-config")},
		kube.KindStatefulSet: {statefulSetVolume("nva-stage", "nva-db", "nva-config")},
	}
	m := newModel(t, newSession(), cm, mut, extra)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	m = step(t, m, tea.KeyPressMsg{Text: "x"})
	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = step(t, m, tea.KeyPressMsg{Text: "y"})

	if cm.Data["LOG_LEVEL"] != "infox" {
		t.Fatalf("expected the edit applied, cm.Data = %+v", cm.Data)
	}
	if m.editing != nil || m.multiline != nil {
		t.Fatal("the editor re-opened as if the patch had failed")
	}
	want := "updated LOG_LEVEL · restarted 1/2 consumers · StatefulSet/nva-db failed: forbidden"
	if m.lastError != want {
		t.Fatalf("lastError = %q, want %q", m.lastError, want)
	}
	if row, ok := m.selectedKeyRow(); !ok || row.value != "infox" {
		t.Fatalf("expected the grid refreshed to the applied value, got %+v", row)
	}
}

// TestCapitalRInEveryBufferIsText is the regression for apply+restart being
// bound to a bare 'R': every capital R typed into a value ("Release",
// "REDIS_URL") committed the buffer and rollout-restarted every consumer.
// While a buffer has focus every printable key must be text — in the edit
// row, the add row's key and value buffers, and the multi-line editor.
func TestCapitalRInEveryBufferIsText(t *testing.T) {
	typeText := func(m Model, s string) Model {
		for _, r := range s {
			m = step(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		return m
	}
	setup := func() (*corev1.ConfigMap, *fakeMutator, Model) {
		cm := cmObj("nva-stage", "nva-config", map[string]string{"MODE": "x", "nginx.conf": "a\nb"})
		mut := &fakeMutator{cm: cm}
		extra := map[kube.ResourceKind][]runtime.Object{
			kube.KindDeployment: {deploymentEnvFrom("nva-stage", "web", "nva-config")},
		}
		return cm, mut, newModel(t, newSession(), cm, mut, extra)
	}
	assertNothingApplied := func(t *testing.T, cm *corev1.ConfigMap, mut *fakeMutator, m Model) {
		t.Helper()
		if len(mut.rolloutRestarts) != 0 {
			t.Fatalf("typing text restarted consumers: %v", mut.rolloutRestarts)
		}
		if m.actions.Active() || m.pendingCommit != nil {
			t.Fatal("typing text began a commit")
		}
		if cm.Data["MODE"] != "x" || cm.Data["nginx.conf"] != "a\nb" || len(cm.Data) != 2 {
			t.Fatalf("typing text patched the ConfigMap: %+v", cm.Data)
		}
	}

	t.Run("edit row", func(t *testing.T) {
		cm, mut, m := setup()
		m.selected = m.rowIndex(t, "MODE")
		m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
		m = typeText(m, "Release")
		if m.editing == nil || m.editing.valueInput.Value() != "xRelease" {
			t.Fatalf("expected the edit row still open holding %q, got %+v", "xRelease", m.editing)
		}
		assertNothingApplied(t, cm, mut, m)
	})
	t.Run("add row", func(t *testing.T) {
		cm, mut, m := setup()
		m = step(t, m, tea.KeyPressMsg{Code: 'a', Text: "a"})
		m = typeText(m, "REDIS_URL")
		m = step(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
		m = typeText(m, "Release")
		if m.adding == nil || m.adding.keyInput.Value() != "REDIS_URL" || m.adding.valueInput.Value() != "Release" {
			t.Fatalf("expected the add row still open holding REDIS_URL=Release, got %+v", m.adding)
		}
		assertNothingApplied(t, cm, mut, m)
	})
	t.Run("buffer editor", func(t *testing.T) {
		cm, mut, m := setup()
		m.selected = m.rowIndex(t, "nginx.conf")
		m = step(t, m, tea.KeyPressMsg{Code: 'e', Text: "e"})
		m = typeText(m, "Release")
		if m.multiline == nil || m.multiline.value() != "a\nbRelease" {
			t.Fatalf("expected the buffer editor still open holding %q, got %+v", "a\nbRelease", m.multiline)
		}
		assertNothingApplied(t, cm, mut, m)
	})
}

// rowIndex is the grid index of key, failing the test if it isn't listed.
func (m Model) rowIndex(t *testing.T, key string) int {
	t.Helper()
	for i, row := range m.keys {
		if row.key == key {
			return i
		}
	}
	t.Fatalf("key %q not in %+v", key, m.keys)
	return 0
}

func TestRemoveKeyAlwaysRequiresConfirmRegardlessOfProd(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{
		"LOG_LEVEL": "info",
		"FEATURE_X": "on",
	})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil) // non-prod

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() {
		t.Fatal("expected removal to always require an inline y/N, even outside PROD")
	}
	if m.actions.Tier() != actions.TierInline {
		t.Fatalf("Tier() = %v, want TierInline (never the type-the-name modal)", m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if _, ok := cm.Data["FEATURE_X"]; ok {
		t.Fatalf("expected FEATURE_X removed, cm.Data = %+v", cm.Data)
	}
	if m.message != "removed FEATURE_X" {
		t.Fatalf("message = %q, want %q", m.message, "removed FEATURE_X")
	}
	if len(m.keys) != 1 || m.keys[0].key != "LOG_LEVEL" {
		t.Fatalf("expected only LOG_LEVEL left, got %+v", m.keys)
	}
}

func TestFailedEditRestoresEditModeWithErrorAndAttemptedValue(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm, err: fakeError{}}
	m := newModel(t, newSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	for range "info" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range "debug" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.editing == nil {
		t.Fatal("expected a failed edit to restore the edit row")
	}
	if m.editing.valueInput.Value() != "debug" {
		t.Fatalf("expected the attempted value intact, got %q", m.editing.valueInput.Value())
	}
	if m.lastError == "" {
		t.Fatal("expected the server error to be surfaced")
	}
	if cm.Data["LOG_LEVEL"] != "info" {
		t.Fatalf("expected no local mutation on failure, cm.Data = %+v", cm.Data)
	}
}

type fakeError struct{}

func (fakeError) Error() string { return "configmap is immutable" }

func TestMultilineRowFoldsAndEOpensBufferEditor(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{
		"nginx.conf": "server {\n  listen 80;\n}",
	})
	m := newModel(t, newSession(), cm, &fakeMutator{cm: cm}, nil)

	body := plain(m.gridBody(m.Theme(), 120, 20))
	if !strings.Contains(body, "3 lines") {
		t.Fatalf("expected a folded '3 lines' summary, got:\n%s", body)
	}
	if strings.Contains(body, "listen 80") {
		t.Fatalf("expected the multi-line value not rendered inline in the grid, got:\n%s", body)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "e"})
	if m.multiline == nil {
		t.Fatal("expected 'e' to open the buffer editor on a multi-line row")
	}
	lines := strings.Split(m.multiline.value(), "\n")
	if len(lines) != 3 || lines[1] != "  listen 80;" {
		t.Fatalf("expected the buffer split into 3 lines, got %+v", lines)
	}
}

func TestEnterOnMultilineRowAlsoOpensBufferEditor(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"nginx.conf": "a\nb"})
	m := newModel(t, newSession(), cm, &fakeMutator{cm: cm}, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if m.multiline == nil {
		t.Fatal("expected '↵' on a multi-line row to open the buffer editor too, not leave a dead key")
	}
	if m.editing != nil {
		t.Fatal("expected the single-line edit row not to open for a multi-line value")
	}
}

// A value neither buffer can hold unchanged would open already normalized
// (tabs expanded, CRLF doubled), so any apply — even of an untouched buffer —
// would rewrite the whole value. ↵ and e refuse it with a notice instead.
func TestEditRefusesValuesTheBuffersWouldNormalize(t *testing.T) {
	for name, value := range map[string]string{
		"tabbed multi-line": "all:\n\tgo build\n",
		"crlf":              "a\r\nb\r\n",
		"tabbed single":     "id\tname",
		"binary":            "\x00\xff\n",
	} {
		t.Run(name, func(t *testing.T) {
			cm := cmObj("nva-stage", "nva-config", map[string]string{"VALUE": value})
			mut := &fakeMutator{cm: cm}
			m := newModel(t, newSession(), cm, mut, nil)
			for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Text: "e", Code: 'e'}} {
				m = step(t, m, key)
				if m.multiline != nil || m.editing != nil {
					t.Fatalf("%s opened an editor on a value it can't hold unchanged", key.String())
				}
				// e is only offered on a multi-line row, so it's silent elsewhere.
				if key.Code == tea.KeyEnter || strings.Contains(value, "\n") {
					if strip := plain(m.willRunStrip(m.Theme(), 120)); !strings.Contains(strip, "VALUE has tabs, CRLF line endings or binary content") {
						t.Fatalf("after %s will-run strip = %q, want the refusal notice", key.String(), strip)
					}
				}
			}
			if cm.Data["VALUE"] != value {
				t.Fatalf("value changed to %q", cm.Data["VALUE"])
			}
		})
	}
}

func TestMultilineBufferEditorEditsAndAppliesWithCtrlO(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"nginx.conf": "a\nb"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "e"})
	// Cursor starts at the end of the last line ("b") — append 'c'.
	m = step(t, m, tea.KeyPressMsg{Text: "c"})
	m = step(t, m, tea.KeyPressMsg{Text: "alt+a"})

	if m.multiline != nil {
		t.Fatal("expected the buffer editor to close after a successful apply")
	}
	if cm.Data["nginx.conf"] != "a\nbc" {
		t.Fatalf("cm.Data[nginx.conf] = %q, want %q", cm.Data["nginx.conf"], "a\nbc")
	}
	if m.message != "updated nginx.conf" {
		t.Fatalf("message = %q, want %q", m.message, "updated nginx.conf")
	}
}

func TestMultilineBufferEditorEnterInsertsNewline(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"nginx.conf": "ab\ncd"})
	m := newModel(t, newSession(), cm, &fakeMutator{cm: cm}, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "e"})
	if m.multiline.textarea.Line() != 1 || m.multiline.textarea.Column() != 2 {
		t.Fatalf("expected the cursor to start at the end of the last line (row=1 col=2), got row=%d col=%d", m.multiline.textarea.Line(), m.multiline.textarea.Column())
	}
	// Move up onto "ab" (cursor col preserved, capped to line length), then
	// left once so the split lands between 'a' and 'b'.
	m = step(t, m, tea.KeyPressMsg{Text: "up"})
	m = step(t, m, tea.KeyPressMsg{Text: "left"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	lines := strings.Split(m.multiline.value(), "\n")
	if len(lines) != 3 || lines[0] != "a" || lines[1] != "b" || lines[2] != "cd" {
		t.Fatalf("expected enter to split \"ab\" into [a b cd], got %+v", lines)
	}
	if m.multiline.textarea.Line() != 1 || m.multiline.textarea.Column() != 0 {
		t.Fatalf("expected cursor at row=1 col=0, got row=%d col=%d", m.multiline.textarea.Line(), m.multiline.textarea.Column())
	}
}

func TestMultilineBufferEditorEscDiscardsWithoutApplying(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"nginx.conf": "a\nb"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "e"})
	m = step(t, m, tea.KeyPressMsg{Text: "c"})
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})

	if m.multiline != nil {
		t.Fatal("expected esc to close the buffer editor")
	}
	if cm.Data["nginx.conf"] != "a\nb" {
		t.Fatalf("expected no mutation on esc, cm.Data = %+v", cm.Data)
	}
}

// TestKeybarGoesOfflineAndHidesAddRemove pins the cross-cutting 4a fix
// (docs/design README.md §52, §301): configmapdata must show the OFFLINE
// pill and drop add/remove from the keybar while disconnected, not just
// browse.
func TestKeybarGoesOfflineAndHidesAddRemove(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	m := newModel(t, newSession(), cm, &fakeMutator{cm: cm}, nil)

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	kb := m.Keybar()
	if kb.Pill != tui.ModeOffline || kb.PillText != "OFFLINE" {
		t.Fatalf("Pill/PillText = %v/%q while offline, want ModeOffline/OFFLINE", kb.Pill, kb.PillText)
	}
	for _, g := range kb.Groups {
		for _, h := range g {
			if h.Label == "add key" || h.Label == "remove key" {
				t.Fatalf("expected add/remove hints hidden while offline, got groups %+v", kb.Groups)
			}
		}
	}

	// Begin() itself refuses to execute while offline (actions.Controller's
	// own gate) — a PROD add's confirm never even applies.
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected})
	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	for _, r := range "FEATURE_X" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	for _, r := range "on" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	_ = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if cm.Data["FEATURE_X"] != "" {
		t.Fatalf("expected the add refused while offline, cm.Data = %+v", cm.Data)
	}
}

// typeOver replaces the open single-line edit buffer's value with value.
func typeOver(t *testing.T, m Model, old, value string) Model {
	t.Helper()
	for range old {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range value {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	return m
}

// TestConcurrentEditConflictsKeepsBufferAndReloads pins review finding 09
// M5: the patch carries the resourceVersion observed when the buffer
// opened — not whatever a watch event refreshed it to while the user was
// typing — so a concurrent writer's change to the same key is a 409, not a
// silent lost update. The 409 keeps the typed buffer, says the ConfigMap
// changed, reloads, runs no restarts, and a second ctrl-r applies over the
// latest version.
func TestConcurrentEditConflictsKeepsBufferAndReloads(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	cm.ResourceVersion = "1"
	mut := &fakeMutator{cm: cm}
	extra := map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment: {deploymentEnvFrom("nva-stage", "nva-worker", "nva-config")},
	}
	m := newModel(t, newSession(), cm, mut, extra)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	m = typeOver(t, m, "info", "debug")

	// Someone else writes the same key while the buffer is open, and the
	// watch event lands before the user commits.
	cm.Data["LOG_LEVEL"] = "warn"
	cm.ResourceVersion = "2"
	m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindConfigMap})

	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = step(t, m, tea.KeyPressMsg{Text: "y"})

	if got := mut.dataResourceVersions; len(got) != 1 || got[0] != "1" {
		t.Fatalf("patch preconditions = %v, want [1] (the version the buffer opened on)", got)
	}
	if cm.Data["LOG_LEVEL"] != "warn" {
		t.Fatalf("the concurrent write was overwritten: cm.Data = %+v", cm.Data)
	}
	if len(mut.rolloutRestarts) != 0 {
		t.Fatalf("a conflicted apply must not restart consumers, restarted %v", mut.rolloutRestarts)
	}
	if m.editing == nil || m.editing.valueInput.Value() != "debug" {
		t.Fatalf("expected the typed buffer kept open after a conflict, editing = %+v", m.editing)
	}
	if !strings.Contains(m.lastError, "changed on the server since you opened it") {
		t.Fatalf("lastError = %q, want the conflict message", m.lastError)
	}
	if !strings.Contains(plain(m.View().Content), "changed on the server since you opened it") {
		t.Fatalf("expected the conflict message on screen:\n%s", plain(m.View().Content))
	}
	if m.resourceVersion != "2" {
		t.Fatalf("expected the conflict to reload the ConfigMap, resourceVersion = %q", m.resourceVersion)
	}

	// Retrying applies over the version the user has now been told about.
	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if cm.Data["LOG_LEVEL"] != "debug" {
		t.Fatalf("expected the retry applied, cm.Data = %+v (preconditions %v, lastError %q)", cm.Data, mut.dataResourceVersions, m.lastError)
	}
	if len(mut.rolloutRestarts) != 1 {
		t.Fatalf("expected the retry to restart the consumer, restarted %v", mut.rolloutRestarts)
	}
}

// TestRemoveKeyCarriesObservedResourceVersion: a removal's precondition is
// the ConfigMap as last loaded.
func TestRemoveKeyCarriesObservedResourceVersion(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	cm.ResourceVersion = "4"
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	_ = step(t, m, tea.KeyPressMsg{Text: "y"})

	if got := mut.dataResourceVersions; len(got) != 1 || got[0] != "4" {
		t.Fatalf("patch preconditions = %v, want [4]", got)
	}
	if _, ok := cm.Data["LOG_LEVEL"]; ok {
		t.Fatalf("expected LOG_LEVEL removed, cm.Data = %+v", cm.Data)
	}
}

func twoConsumers() map[kube.ResourceKind][]runtime.Object {
	return map[kube.ResourceKind][]runtime.Object{
		kube.KindDeployment:  {deploymentEnvFrom("nva-stage", "nva-worker", "nva-config")},
		kube.KindStatefulSet: {statefulSetVolume("nva-stage", "nva-db", "nva-config")},
	}
}

// TestCtrlRAsksYNBeforeRestartingOutsideProd: ctrl-r restarts every consumer
// in one key, so outside PROD it gets the inline y/N a single rollout
// restart gets — never an immediate apply. The y/N names the restarts, the
// will-run strip lists every command, esc walks back to the buffer with
// the typed value intact, and y applies + restarts and stays on screen.
func TestCtrlRAsksYNBeforeRestartingOutsideProd(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, twoConsumers())

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	m = typeOver(t, m, "info", "debug")
	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})

	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected ctrl-r to stop at an inline y/N outside PROD, active=%v tier=%v", m.actions.Active(), m.actions.Tier())
	}
	if cm.Data["LOG_LEVEL"] != "info" || len(mut.rolloutRestarts) != 0 {
		t.Fatalf("nothing may run before y: cm.Data = %+v, restarts = %v", cm.Data, mut.rolloutRestarts)
	}
	view := plain(m.View().Content)
	for _, want := range []string{
		"confirm to update + restart 2 consumers · y/N",
		`kubectl patch cm/nva-config --type merge -p '{"data":{"LOG_LEVEL":"debug"}}' -n nva-stage`,
		"kubectl rollout restart deploy/nva-worker -n nva-stage",
		"kubectl rollout restart sts/nva-db -n nva-stage",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in the y/N view:\n%s", want, view)
		}
	}
	if strings.Contains(view, "to confirm") {
		t.Fatalf("outside PROD ctrl-r is a y/N, not the type-the-name modal:\n%s", view)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.actions.Active() || m.editing == nil || m.editing.valueInput.Value() != "debug" {
		t.Fatalf("expected esc to reopen the edit with the typed value, editing = %+v", m.editing)
	}
	if cm.Data["LOG_LEVEL"] != "info" || len(mut.rolloutRestarts) != 0 {
		t.Fatalf("a cancelled ctrl-r must not run: cm.Data = %+v, restarts = %v", cm.Data, mut.rolloutRestarts)
	}

	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if cm.Data["LOG_LEVEL"] != "debug" || len(mut.rolloutRestarts) != 2 {
		t.Fatalf("expected y to apply and restart both: cm.Data = %+v, restarts = %v", cm.Data, mut.rolloutRestarts)
	}
	if m.message != "updated LOG_LEVEL · restarted 2 consumers" {
		t.Fatalf("message = %q", m.message)
	}
}

// TestCtrlRInProdRequiresTypingTheConfigMapName: in PROD ctrl-r gets the
// type-the-name modal a single rollout restart gets there, against the
// ConfigMap's name. A bare y types into the buffer rather than confirming,
// the modal lists the consumers and every command, a paste reaches the
// buffer through tui.RoutePaste, and only the matching name + enter runs.
func TestCtrlRInProdRequiresTypingTheConfigMapName(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, prodSession(), cm, mut, twoConsumers())

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	m = typeOver(t, m, "info", "debug")
	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})

	if !m.actions.TypeToConfirm() {
		t.Fatal("expected ctrl-r in PROD to open the type-the-name modal")
	}
	view := plain(m.View().Content)
	for _, want := range []string{
		`type "nva-config" to confirm`,
		"PROD CONTEXT",
		"Update key LOG_LEVEL on nva-config and restart 2 consumers?",
		"restarts deploy/nva-worker · sts/nva-db",
		`kubectl patch cm/nva-config --type merge -p '{"data":{"LOG_LEVEL":"debug"}}' -n nva-stage`,
		"kubectl rollout restart deploy/nva-worker -n nva-stage",
		"kubectl rollout restart sts/nva-db -n nva-stage",
		"apply + restart (when name matches)",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in the PROD modal:\n%s", want, view)
		}
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if cm.Data["LOG_LEVEL"] != "info" || len(mut.rolloutRestarts) != 0 {
		t.Fatalf("y + enter must not run in PROD: cm.Data = %+v, restarts = %v", cm.Data, mut.rolloutRestarts)
	}
	if m.actions.TypedName() != "y" {
		t.Fatalf("expected y typed into the modal's buffer, got %q", m.actions.TypedName())
	}

	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.actions.Active() || m.editing == nil || m.editing.valueInput.Value() != "debug" {
		t.Fatalf("expected esc to reopen the edit with the typed value, editing = %+v", m.editing)
	}

	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	for _, r := range "nva-" {
		m = step(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m = step(t, m, tea.PasteMsg{Content: "config"})
	if m.editing != nil {
		t.Fatal("the paste must reach the modal, not a buffer behind it")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if cm.Data["LOG_LEVEL"] != "debug" || len(mut.rolloutRestarts) != 2 {
		t.Fatalf("expected the matching name to apply and restart both: cm.Data = %+v, restarts = %v", cm.Data, mut.rolloutRestarts)
	}
	if m.message != "updated LOG_LEVEL · restarted 2 consumers" {
		t.Fatalf("message = %q", m.message)
	}
	if m.state != tui.TaskStateReady || !strings.Contains(plain(m.View().Content), "updated LOG_LEVEL · restarted 2 consumers") {
		t.Fatalf("expected the screen to remain with the inline result:\n%s", plain(m.View().Content))
	}
}

// TestCtrlRWithNoConsumersIsThePlainApply: with nothing to restart, ctrl-r
// is just the apply, and gets the apply's friction.
func TestCtrlRWithNoConsumersIsThePlainApply(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	m = typeOver(t, m, "info", "debug")
	m = step(t, m, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	if m.actions.Active() || cm.Data["LOG_LEVEL"] != "debug" {
		t.Fatalf("expected ctrl-r with no consumers to apply immediately outside PROD, cm.Data = %+v", cm.Data)
	}
}

// TestEditAgainBeforeCacheCatchesUpUsesWrittenResourceVersion: right after
// the user's own successful apply, the reload reads an informer cache that
// still holds the pre-write ConfigMap. A buffer opened in that window must
// precondition on the resourceVersion the patch returned, not the cached
// pre-write one — otherwise its first commit is a 409 against the user's own
// change. Once the cache reports anything else, it's trusted again.
func TestEditAgainBeforeCacheCatchesUpUsesWrittenResourceVersion(t *testing.T) {
	server := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	server.ResourceVersion = "1"
	cached := server.DeepCopy() // the informer cache: never sees the write
	mut := &fakeMutator{cm: server}
	m := newModel(t, newSession(), cached, mut, nil)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	m = typeOver(t, m, "info", "debug")
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if server.Data["LOG_LEVEL"] != "debug" || m.lastError != "" {
		t.Fatalf("first apply: server.Data = %+v, lastError %q", server.Data, m.lastError)
	}

	// The cache still says resourceVersion 1 and LOG_LEVEL=info.
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if m.editing == nil {
		t.Fatal("expected ↵ to reopen the edit row")
	}
	m = typeOver(t, m, m.editing.valueInput.Value(), "trace")
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if got := mut.dataResourceVersions; len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("patch preconditions = %v, want [1 2] (the second on the version the first write returned)", got)
	}
	if m.lastError != "" {
		t.Fatalf("second apply failed: %q", m.lastError)
	}
	if server.Data["LOG_LEVEL"] != "trace" {
		t.Fatalf("second apply did not land: server.Data = %+v", server.Data)
	}

	// The cache catches up: its resourceVersion is trusted again.
	cached.ResourceVersion = server.ResourceVersion
	cached.Data = map[string]string{"LOG_LEVEL": "trace"}
	m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindConfigMap})
	if m.resourceVersion != server.ResourceVersion || m.ownWrites.Latest() != "" {
		t.Fatalf("after the cache caught up: resourceVersion = %q, remembered write %q; want %q and none",
			m.resourceVersion, m.ownWrites.Latest(), server.ResourceVersion)
	}
}

// binaryData keys are listed (read-only, with their size) rather than
// hidden — a binaryData-only ConfigMap is not "no keys" — and an add that
// collides with one is refused before it reaches the server.
func TestBinaryDataKeysAreListedReadOnly(t *testing.T) {
	cm := cmObj("nva-stage", "nva-config", map[string]string{"LOG_LEVEL": "info"})
	cm.BinaryData = map[string][]byte{"cert.der": make([]byte, 300)}
	mut := &fakeMutator{cm: cm}
	m := newModel(t, newSession(), cm, mut, nil)

	body := plain(m.Render())
	for _, want := range []string{"cert.der", "binary · read-only", "300 B", "2 keys"} {
		if !strings.Contains(body, want) {
			t.Fatalf("render missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "no keys") {
		t.Fatalf("render claims no keys:\n%s", body)
	}

	// cert.der sorts after LOG_LEVEL.
	m = step(t, m, tea.KeyPressMsg{Text: "j", Code: 'j'})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.editing != nil || m.multiline != nil {
		t.Fatal("↵ opened an editor on a binaryData key")
	}
	if strip := plain(m.willRunStrip(m.Theme(), 120)); !strings.Contains(strip, "cert.der is binaryData") {
		t.Fatalf("will-run strip = %q, want the binaryData notice", strip)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "D", Code: 'D'})
	if m.actions.Active() {
		t.Fatal("D began a removal of a binaryData key")
	}

	m = step(t, m, tea.KeyPressMsg{Text: "a", Code: 'a'})
	for _, r := range "cert.der" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r), Code: r})
	}
	if strip := plain(m.willRunStrip(m.Theme(), 120)); !strings.Contains(strip, "already a binaryData key") {
		t.Fatalf("will-run strip = %q, want the collision warning", strip)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(mut.dataResourceVersions) != 0 || m.adding == nil {
		t.Fatalf("add over a binaryData key was sent (patches %d) or closed the row", len(mut.dataResourceVersions))
	}
}
