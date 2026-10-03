package secretdata

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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

// fakeMutator's PatchSecretData mutates the shared secret object in place,
// so a post-commit refresh (the "confirm → execute → refresh" contract)
// reads the real change rather than stale data — the same fidelity fix
// browse_nodes_test.go's own fakeMutator makes for 26a's meta.go tests.
type fakeMutator struct {
	secret *corev1.Secret
	err    error
	// dataResourceVersions records every PatchSecretData precondition.
	dataResourceVersions []string
}

func (f *fakeMutator) DeleteResource(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeMutator) DeleteResourceForced(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeMutator) RolloutRestart(context.Context, kube.ResourceKind, string, string) error {
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
func (f *fakeMutator) PatchSecretData(_ context.Context, namespace, name, resourceVersion, key, value string, remove bool) error {
	f.dataResourceVersions = append(f.dataResourceVersions, resourceVersion)
	if f.err != nil {
		return f.err
	}
	if f.secret == nil || f.secret.Name != name || f.secret.Namespace != namespace {
		return nil
	}
	// A fixture that carries a resourceVersion gets the API server's
	// precondition check: a mismatch is a 409, a match bumps it.
	if f.secret.ResourceVersion != "" {
		if resourceVersion != f.secret.ResourceVersion {
			return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, name, errors.New("the object has been modified"))
		}
		n, _ := strconv.Atoi(f.secret.ResourceVersion)
		f.secret.ResourceVersion = strconv.Itoa(n + 1)
	}
	if f.secret.Data == nil {
		f.secret.Data = map[string][]byte{}
	}
	if remove {
		delete(f.secret.Data, key)
	} else {
		f.secret.Data[key] = []byte(value)
	}
	return nil
}
func (f *fakeMutator) PatchConfigMapData(context.Context, string, string, string, string, string, bool) error {
	return nil
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
func (f *fakeMutator) RequestArgoSync(_ context.Context, kind kube.ResourceKind, namespace, name, revision string) error {
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

func secretObj(namespace, name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func newModel(t *testing.T, sess *tui.Session, secret *corev1.Secret, mut *fakeMutator) Model {
	t.Helper()
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindSecret: {secret}}}
	var mutator kube.Mutator
	if mut != nil {
		mutator = mut
	}
	m := New(Config{Session: sess, Lister: lister, Mutator: mutator, Namespace: secret.Namespace, Name: secret.Name})
	m.SetSize(120, 36)
	return step(t, m, m.Init()())
}

func TestLoadRendersMaskedRowsAndKeyCount(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{
		"DATABASE_URL": []byte("postgres://old"),
		"API_TOKEN":    []byte("abcdef1234567890"),
	})
	m := newModel(t, newSession(), secret, nil)

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if len(m.keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(m.keys))
	}
	body := plain(m.gridBody(m.Theme(), 120, 20))
	if !strings.Contains(body, secretDataMaskGlyph) {
		t.Fatalf("expected the mask glyph in the rendered grid, got:\n%s", body)
	}
	if strings.Contains(body, "postgres://old") || strings.Contains(body, "abcdef1234567890") {
		t.Fatalf("expected no plaintext secret value ever rendered, got:\n%s", body)
	}
	strip := plain(strings.Join(m.Strips(120), "\n"))
	if !strings.Contains(strip, "Opaque · 2 keys") {
		t.Fatalf("expected the Opaque · 2 keys strip, got %q", strip)
	}
}

func TestAddKeyNonProdAppliesImmediatelyAndRefreshes(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"DATABASE_URL": []byte("postgres://old")})
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	if m.adding == nil {
		t.Fatal("expected 'a' to open the add row")
	}
	for _, r := range "SMTP_PASSWORD" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	for _, r := range "hunter2-staging" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.actions.Active() {
		t.Fatal("expected non-prod add to apply immediately, no confirm")
	}
	if m.adding != nil {
		t.Fatal("expected the add row to close after a successful apply")
	}
	if string(secret.Data["SMTP_PASSWORD"]) != "hunter2-staging" {
		t.Fatalf("mutator did not receive the add, secret.Data = %+v", secret.Data)
	}
	if m.message != "added SMTP_PASSWORD" {
		t.Fatalf("message = %q, want %q", m.message, "added SMTP_PASSWORD")
	}
	if len(m.keys) != 2 {
		t.Fatalf("expected the refreshed grid to show 2 keys, got %d", len(m.keys))
	}
	if key, ok := m.selectedKeyRow(); !ok || key.key != "SMTP_PASSWORD" {
		t.Fatalf("expected focus to follow the added key, got %+v (ok=%v)", key, ok)
	}
	// docs/design README.md §27b's own no-leak rule: the result names the
	// key alone, never the value.
	if strings.Contains(m.message, "hunter2-staging") {
		t.Fatalf("message leaked the value: %q", m.message)
	}
}

// 'a' with a key that already exists would replace — for a Secret, often
// destroy the only copy of — its value. It must ask, even outside PROD, and
// say "updated" rather than "added".
func TestAddExistingKeyAsksBeforeOverwriting(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"PASSWORD": []byte("orig")})
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	for _, r := range "PASSWORD" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	m = step(t, m, tea.KeyPressMsg{Text: "x"})
	if strip := ansi.Strip(m.willRunStrip(m.Theme(), 120)); !strings.Contains(strip, "overwrites existing PASSWORD") {
		t.Fatalf("will-run strip = %q, want the overwrite warning", strip)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if string(secret.Data["PASSWORD"]) != "orig" {
		t.Fatalf("an add over an existing key applied without a confirm: %q", secret.Data["PASSWORD"])
	}
	if !m.actions.Active() {
		t.Fatal("expected an add over an existing key to ask y/N")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if string(secret.Data["PASSWORD"]) != "x" {
		t.Fatalf("confirmed overwrite not applied: %q", secret.Data["PASSWORD"])
	}
	if m.message != "updated PASSWORD" {
		t.Fatalf("message = %q, want %q", m.message, "updated PASSWORD")
	}
}

func TestAddKeyProdRequiresConfirmThenApplies(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", nil)
	mut := &fakeMutator{secret: secret}
	m := newModel(t, prodSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	for _, r := range "TOKEN" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	for _, r := range "secretvalue" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if !m.actions.Active() {
		t.Fatal("expected a PROD add to require confirmation before applying")
	}
	if _, ok := secret.Data["TOKEN"]; ok {
		t.Fatal("expected the mutator not to be called before confirming")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if string(secret.Data["TOKEN"]) != "secretvalue" {
		t.Fatalf("expected the add applied after confirming, secret.Data = %+v", secret.Data)
	}
	if m.message != "added TOKEN" {
		t.Fatalf("message = %q, want %q", m.message, "added TOKEN")
	}
}

func TestRemoveKeyAlwaysRequiresConfirmRegardlessOfProd(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{
		"DATABASE_URL": []byte("postgres://old"),
		"API_TOKEN":    []byte("abcdef"),
	})
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut) // non-prod

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() {
		t.Fatal("expected removal to always require an inline y/N, even outside PROD")
	}
	if m.actions.Tier() != actions.TierInline {
		t.Fatalf("Tier() = %v, want TierInline (never the type-the-name modal)", m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if _, ok := secret.Data["API_TOKEN"]; ok {
		t.Fatalf("expected API_TOKEN removed, secret.Data = %+v", secret.Data)
	}
	if m.message != "removed API_TOKEN" {
		t.Fatalf("message = %q, want %q", m.message, "removed API_TOKEN")
	}
	if len(m.keys) != 1 || m.keys[0].key != "DATABASE_URL" {
		t.Fatalf("expected only DATABASE_URL left, got %+v", m.keys)
	}
	if key, ok := m.selectedKeyRow(); !ok || key.key != "DATABASE_URL" {
		t.Fatalf("expected focus to land on the nearest remaining row, got %+v (ok=%v)", key, ok)
	}
}

func TestFailedAddRestoresAddModeWithErrorAndAttemptedValues(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", nil)
	mut := &fakeMutator{secret: secret, err: fakeError{}}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	for _, r := range "TOKEN" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	for _, r := range "secretvalue" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.adding == nil {
		t.Fatal("expected a failed add to restore the add row")
	}
	if m.adding.keyInput.Value() != "TOKEN" || m.adding.valueInput.Value() != "secretvalue" {
		t.Fatalf("expected the attempted key/value intact, got %+v", m.adding)
	}
	if m.lastError == "" {
		t.Fatal("expected the server error to be surfaced")
	}
	strip := plain(m.willRunStrip(m.Theme(), 120))
	if !strings.Contains(strip, "error") {
		t.Fatalf("expected the will-run strip to show the error, got %q", strip)
	}
	if strings.Contains(strip, "secretvalue") {
		t.Fatalf("expected the will-run strip to never show the plaintext value, got %q", strip)
	}
}

type fakeError struct{}

func (fakeError) Error() string { return "secret is immutable" }

func TestShiftTabReturnsFocusFromValueToKey(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", nil)
	m := newModel(t, newSession(), secret, &fakeMutator{secret: secret})

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	if !m.adding.onValue {
		t.Fatal("expected tab to move focus onto the value buffer")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "shift+tab"})
	if m.adding.onValue {
		t.Fatal("expected shift+tab to move focus back onto the key buffer")
	}
}

func TestCtrlXTogglesMaskAndPlainXStaysTypeable(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", nil)
	m := newModel(t, newSession(), secret, &fakeMutator{secret: secret})

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	// Plain 'x' is always a literal character now — ctrl-x is the re-mask
	// chord, so 'x' never needs special-casing by focus.
	m = step(t, m, tea.KeyPressMsg{Text: "x"})
	if m.adding.masked {
		t.Fatal("expected 'x' on the key buffer to type a literal character, not toggle mask")
	}
	if m.adding.keyInput.Value() != "x" {
		t.Fatalf("key = %q, want the literal 'x' inserted", m.adding.keyInput.Value())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "tab"})
	m = step(t, m, tea.KeyPressMsg{Text: "x"})
	if m.adding.masked || m.adding.valueInput.Value() != "x" {
		t.Fatalf("expected 'x' on the value buffer to type literally too, got value=%q masked=%v", m.adding.valueInput.Value(), m.adding.masked)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "ctrl+x"})
	if !m.adding.masked {
		t.Fatal("expected ctrl-x on the value buffer to toggle the mask")
	}
}

func TestEnterOnExistingRowDecodesAndEditsNonProdAppliesImmediately(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"DATABASE_URL": []byte("postgres://old")})
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if m.editing == nil {
		t.Fatal("expected '↵' to open the decode-then-edit row")
	}
	if m.editing.valueInput.Value() != "postgres://old" {
		t.Fatalf("expected the buffer pre-filled with the real decoded value, got %q", m.editing.valueInput.Value())
	}
	// Clear the pre-filled buffer and type a new value.
	for range "postgres://old" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range "postgres://new" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.actions.Active() {
		t.Fatal("expected non-prod edit to apply immediately, no confirm")
	}
	if m.editing != nil {
		t.Fatal("expected the edit row to close after a successful apply")
	}
	if string(secret.Data["DATABASE_URL"]) != "postgres://new" {
		t.Fatalf("mutator did not receive the edit, secret.Data = %+v", secret.Data)
	}
	if m.message != "updated DATABASE_URL" {
		t.Fatalf("message = %q, want %q", m.message, "updated DATABASE_URL")
	}
	if strings.Contains(m.message, "postgres://new") {
		t.Fatalf("message leaked the value: %q", m.message)
	}
}

func TestEnterOnUnchangedValueExitsEditingWithoutApplying(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"DATABASE_URL": []byte("postgres://old")})
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"}) // unchanged value

	if m.editing != nil {
		t.Fatal("expected enter on an unchanged value to exit editing")
	}
	if m.actions.Active() {
		t.Fatal("expected no commit for an unchanged value")
	}
	if string(secret.Data["DATABASE_URL"]) != "postgres://old" {
		t.Fatalf("expected no mutation, secret.Data = %+v", secret.Data)
	}
}

func TestEditProdRequiresConfirmThenApplies(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"DATABASE_URL": []byte("postgres://old")})
	mut := &fakeMutator{secret: secret}
	m := newModel(t, prodSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	for range "postgres://old" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range "postgres://new" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if !m.actions.Active() {
		t.Fatal("expected a PROD edit to require confirmation before applying")
	}
	if string(secret.Data["DATABASE_URL"]) != "postgres://old" {
		t.Fatal("expected the mutator not to be called before confirming")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if string(secret.Data["DATABASE_URL"]) != "postgres://new" {
		t.Fatalf("expected the edit applied after confirming, secret.Data = %+v", secret.Data)
	}
}

func TestFailedEditRestoresEditingModeWithErrorAndAttemptedValue(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"DATABASE_URL": []byte("postgres://old")})
	mut := &fakeMutator{secret: secret, err: fakeError{}}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	for range "postgres://old" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range "postgres://new" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if m.editing == nil {
		t.Fatal("expected a failed edit to restore the edit row")
	}
	if m.editing.valueInput.Value() != "postgres://new" || m.editing.original != "postgres://old" {
		t.Fatalf("expected the attempted value intact against the real original, got %+v", m.editing)
	}
	if m.lastError == "" {
		t.Fatal("expected the server error to be surfaced")
	}
}

func TestEscCancelsEditWithoutApplying(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"DATABASE_URL": []byte("postgres://old")})
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	for _, r := range "-changed" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})

	if m.editing != nil {
		t.Fatal("expected esc to cancel editing")
	}
	if string(secret.Data["DATABASE_URL"]) != "postgres://old" {
		t.Fatalf("expected no mutation, secret.Data = %+v", secret.Data)
	}
}

// TestWillRunStripHiddenWhenIdle pins the "hide the will-run block if
// there are no changes" behavior: a plain idle navigation state renders no
// band at all, not a static "no changes" placeholder line.
func TestWillRunStripHiddenWhenIdle(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"K": []byte("v")})
	m := newModel(t, newSession(), secret, &fakeMutator{secret: secret})

	if strip := m.willRunStrip(m.Theme(), 120); strip != "" {
		t.Fatalf("expected an empty will-run strip while idle, got %q", plain(strip))
	}
	body := plain(m.gridBody(m.Theme(), 120, 20))
	if strings.Contains(body, "will run") {
		t.Fatalf("expected no will-run band in the idle grid body, got:\n%s", body)
	}
}

func TestWillRunStripShowsWhileAdding(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", nil)
	m := newModel(t, newSession(), secret, &fakeMutator{secret: secret})

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	if strip := plain(m.willRunStrip(m.Theme(), 120)); !strings.Contains(strip, "type a key to add") {
		t.Fatalf("expected the will-run strip to show a placeholder while adding, got %q", strip)
	}
}

func TestEscDiscardsAddRowWithoutApplying(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", nil)
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "a"})
	for _, r := range "TOKEN" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "esc"})

	if m.adding != nil {
		t.Fatal("expected esc to discard the add row")
	}
	if len(secret.Data) != 0 {
		t.Fatalf("expected no mutation, secret.Data = %+v", secret.Data)
	}
}

func TestEscFromNavigationReturnsToPreviousTask(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", nil)
	m := newModel(t, newSession(), secret, nil)

	_, cmd := m.Update(tea.KeyPressMsg{Text: "esc"})
	if cmd == nil {
		t.Fatal("expected a Cmd from esc")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected esc to produce tui.BackMsg, got %T", cmd())
	}
}

// TestKeybarGoesOfflineAndHidesAddRemove pins the cross-cutting 4a fix
// (docs/design README.md §52, §301): secretdata must show the OFFLINE pill
// and drop add/remove from the keybar while disconnected, not just browse.
func TestKeybarGoesOfflineAndHidesAddRemove(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"K": []byte("v")})
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

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

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected})
	kb = m.Keybar()
	if kb.PillText != "DATA" {
		t.Fatalf("PillText = %q after reconnect, want DATA", kb.PillText)
	}
}

// A value the single-line buffer can't hold verbatim — a PEM's newlines, a
// keystore's control/invalid-UTF-8 bytes — must never open for an in-place
// edit: the buffer would start already flattened, read as "changed", and a
// second ↵ would write the corruption back (review 09 H1).
func TestEnterRefusesValuesThatCannotRoundTripTheSingleLineBuffer(t *testing.T) {
	cases := map[string][]byte{
		"pem":    []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
		"binary": {0x00, 0xff, 0x10, 0x61},
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"tls.crt": value})
			mut := &fakeMutator{secret: secret}
			m := newModel(t, newSession(), secret, mut)

			m = step(t, m, tea.KeyPressMsg{Text: "enter"})
			m = step(t, m, tea.KeyPressMsg{Text: "enter"})

			if m.editing != nil {
				t.Fatal("expected ↵ to refuse opening a multi-line/binary value in the single-line buffer")
			}
			if m.actions.Active() || m.pendingCommit != nil {
				t.Fatal("expected no commit")
			}
			if got := secret.Data["tls.crt"]; string(got) != string(value) {
				t.Fatalf("value was rewritten: got %q, want %q", got, value)
			}
			strip := plain(m.willRunStrip(m.Theme(), 120))
			if !strings.Contains(strip, "tls.crt is multi-line or binary") {
				t.Fatalf("expected an inline refusal naming the key, got %q", strip)
			}
			if strings.Contains(strip, "MIIB") || strings.Contains(strip, "BEGIN") {
				t.Fatalf("refusal leaked the value: %q", strip)
			}
		})
	}
}

// TestConcurrentEditConflictsKeepsBufferAndReloads pins review finding 09
// M5 for 27b: the patch carries the resourceVersion observed when the edit
// opened, so a concurrent writer's change is a 409 rather than a silent
// lost update. The 409 keeps the typed buffer, says the Secret changed
// (never the value), reloads, and ↵ again applies over the latest version.
func TestConcurrentEditConflictsKeepsBufferAndReloads(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"DATABASE_URL": []byte("postgres://old")})
	secret.ResourceVersion = "1"
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	for range "postgres://old" {
		m = step(t, m, tea.KeyPressMsg{Text: "backspace"})
	}
	for _, r := range "postgres://new" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}

	// Someone else rotates the same key while the buffer is open, and the
	// watch event lands before the user commits.
	secret.Data["DATABASE_URL"] = []byte("postgres://rotated")
	secret.ResourceVersion = "2"
	m = step(t, m, kube.ResourceChangedMsg{Kind: kube.KindSecret})

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})

	if got := mut.dataResourceVersions; len(got) != 1 || got[0] != "1" {
		t.Fatalf("patch preconditions = %v, want [1] (the version the buffer opened on)", got)
	}
	if string(secret.Data["DATABASE_URL"]) != "postgres://rotated" {
		t.Fatalf("the concurrent write was overwritten: secret.Data = %q", secret.Data)
	}
	if m.editing == nil || m.editing.valueInput.Value() != "postgres://new" {
		t.Fatalf("expected the typed buffer kept open after a conflict, editing = %+v", m.editing)
	}
	if !strings.Contains(m.lastError, "changed on the server since you opened it") {
		t.Fatalf("lastError = %q, want the conflict message", m.lastError)
	}
	if strings.Contains(m.lastError, "postgres://") {
		t.Fatalf("conflict message leaked a value: %q", m.lastError)
	}
	if m.resourceVersion != "2" {
		t.Fatalf("expected the conflict to reload the Secret, resourceVersion = %q", m.resourceVersion)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if string(secret.Data["DATABASE_URL"]) != "postgres://new" {
		t.Fatalf("expected the retry applied, secret.Data = %q (preconditions %v, lastError %q)", secret.Data, mut.dataResourceVersions, m.lastError)
	}
	if m.editing != nil {
		t.Fatal("expected the edit row to close after the retry applied")
	}
}

// TestRemoveKeyConflictSaysSoAndReloads: a removal racing a concurrent
// write is refused with the conflict message, and the reload lets D again
// succeed against the new version.
func TestRemoveKeyConflictSaysSoAndReloads(t *testing.T) {
	secret := secretObj("nva-stage", "nva-secrets", map[string][]byte{"DATABASE_URL": []byte("postgres://old")})
	secret.ResourceVersion = "3"
	mut := &fakeMutator{secret: secret}
	m := newModel(t, newSession(), secret, mut)

	secret.ResourceVersion = "4" // changed, and no watch event yet
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	m = step(t, m, tea.KeyPressMsg{Text: "y"})

	if _, ok := secret.Data["DATABASE_URL"]; !ok {
		t.Fatal("a conflicted removal must not remove the key")
	}
	if !strings.Contains(m.lastError, "D again removes DATABASE_URL") {
		t.Fatalf("lastError = %q, want the removal conflict message", m.lastError)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if _, ok := secret.Data["DATABASE_URL"]; ok {
		t.Fatalf("expected the retry to remove the key, preconditions %v", mut.dataResourceVersions)
	}
}
