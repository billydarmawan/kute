package actions

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// beginAt starts action under a fixed policy, bypassing verbs.Resolve — for
// the tests below that pin the controller's own mechanics (execution, state,
// typed-name gating) independent of which verb gets which tier, which
// verbs' TestResolvePolicyTable and TestBeginResolvesPolicy pin instead.
func beginAt(c *Controller, tier Tier, action tui.TaskAction) tea.Cmd {
	return c.begin(verbs.Policy{Tier: tier, TypedName: tier == TierModal}, action)
}

// typeRune feeds a single character into the type-ahead buffer via
// HandleTypeKey — the test-only equivalent of the old TypeRune(string(r)).
func typeRune(c *Controller, r rune) {
	c.HandleTypeKey(tea.KeyPressMsg{Text: string(r)})
}

// backspace feeds a backspace keypress into the type-ahead buffer via
// HandleTypeKey — the test-only equivalent of the old Backspace().
func backspace(c *Controller) {
	c.HandleTypeKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
}

// cronJobTriggerCall/cronJobSuspendCall/cronJobScheduleCall record every
// argument the controller passed into the corresponding kube.Mutator
// CronJob method — including the new Phase 2 parameters (creator/at,
// resourceVersion/generation, the full CronJobScheduleEdit) — so a test can
// assert on the controller's own dispatch, not just that a call happened.
type cronJobTriggerCall struct {
	Namespace, Name, NewJobName, Creator string
	At                                   time.Time
}

type cronJobSuspendCall struct {
	Namespace, Name string
	Suspend         bool
	ResourceVersion string
	Generation      int64
	At              time.Time
}

type cronJobScheduleCall struct {
	Namespace, Name string
	Edit            kube.CronJobScheduleEdit
}

type fakeMutator struct {
	deleted              []string
	forceDeleted         []string
	setImages            []string
	setImageInit         []bool
	metaPatches          []string
	secretDataPatches    []string
	configMapDataPatches []string
	dataResourceVersions []string
	dataNextRV           string
	rolloutRestarts      []string
	cronJobTriggers      []cronJobTriggerCall
	cronJobSuspends      []cronJobSuspendCall
	cronJobSchedules     []cronJobScheduleCall
	// scheduleResult/scheduleErr let a test control SetCronJobSchedule's
	// own return value independently of the generic err field below (e.g.
	// to exercise a Conflict without also breaking every other verb's
	// success-path assertions that share this fakeMutator).
	scheduleResult kube.CronJobScheduleResult
	scheduleErr    error
	// suspendErrByName lets a test give SetCronJobSuspend a per-target
	// error, keyed by CronJob name — TestBulkExecutionReportsPerTargetPartialFailure's
	// only way to exercise a genuine partial failure through the real
	// executeBulk dispatch rather than constructing a BulkResultMsg by hand.
	suspendErrByName map[string]error
	err              error
}

func (f *fakeMutator) DeleteResource(_ context.Context, kind kube.ResourceKind, ns, name string) error {
	f.deleted = append(f.deleted, string(kind)+"/"+ns+"/"+name)
	return f.err
}

func (f *fakeMutator) DeleteResourceForced(_ context.Context, kind kube.ResourceKind, ns, name string) error {
	f.forceDeleted = append(f.forceDeleted, string(kind)+"/"+ns+"/"+name)
	return f.err
}

func (f *fakeMutator) RolloutRestart(_ context.Context, kind kube.ResourceKind, ns, name string) error {
	f.rolloutRestarts = append(f.rolloutRestarts, string(kind)+"/"+ns+"/"+name)
	return f.err
}

func (f *fakeMutator) Cordon(context.Context, string, bool) error { return f.err }

func (f *fakeMutator) Drain(context.Context, string) (int, error)              { return 0, f.err }
func (f *fakeMutator) HelmRollback(context.Context, string, string, int) error { return f.err }
func (f *fakeMutator) RolloutUndo(context.Context, string, string, int) error  { return f.err }
func (f *fakeMutator) Scale(context.Context, kube.ResourceKind, string, string, int32) error {
	return f.err
}
func (f *fakeMutator) SetImage(_ context.Context, kind kube.ResourceKind, namespace, name, container, image string, initContainer bool) error {
	f.setImages = append(f.setImages, string(kind)+"/"+namespace+"/"+name+" "+container+"="+image)
	f.setImageInit = append(f.setImageInit, initContainer)
	return f.err
}
func (f *fakeMutator) SetResources(context.Context, kube.ResourceKind, string, string, string, kube.ResourceEdits, bool) error {
	return f.err
}
func (f *fakeMutator) PatchMeta(_ context.Context, kind kube.ResourceKind, ns, name string, isAnnotation bool, key, value string, remove bool) error {
	field := "labels"
	if isAnnotation {
		field = "annotations"
	}
	entry := key + "=" + value
	if remove {
		entry = key + "-"
	}
	f.metaPatches = append(f.metaPatches, string(kind)+"/"+ns+"/"+name+" "+field+" "+entry)
	return f.err
}

func (f *fakeMutator) PatchSecretData(_ context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error) {
	f.dataResourceVersions = append(f.dataResourceVersions, resourceVersion)
	entry := key + "=" + value
	if remove {
		entry = key + "-"
	}
	f.secretDataPatches = append(f.secretDataPatches, namespace+"/"+name+" "+entry)
	if f.err != nil {
		return "", f.err
	}
	return f.dataNextRV, nil
}

func (f *fakeMutator) PatchConfigMapData(_ context.Context, namespace, name, resourceVersion, key, value string, remove bool) (string, error) {
	f.dataResourceVersions = append(f.dataResourceVersions, resourceVersion)
	entry := key + "=" + value
	if remove {
		entry = key + "-"
	}
	f.configMapDataPatches = append(f.configMapDataPatches, namespace+"/"+name+" "+entry)
	if f.err != nil {
		return "", f.err
	}
	return f.dataNextRV, nil
}

// Flux verbs (§30a). Recorded but inert: no test in this package drives
// them, and the Mutator contract requires them.
func (f *fakeMutator) SetFluxSuspend(_ context.Context, kind kube.ResourceKind, namespace, name string, suspend bool) error {
	return nil
}

func (f *fakeMutator) RequestArgoRefresh(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
	return nil
}

func (f *fakeMutator) RequestArgoSync(_ context.Context, kind kube.ResourceKind, namespace, name, revision string) error {
	return nil
}

func (f *fakeMutator) RenewCertificate(_ context.Context, namespace, name string) error { return nil }

func (f *fakeMutator) RequestFluxReconcile(_ context.Context, kind kube.ResourceKind, namespace, name string) error {
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
	f.cronJobTriggers = append(f.cronJobTriggers, cronJobTriggerCall{
		Namespace: namespace, Name: name, NewJobName: newJobName, Creator: creator, At: at,
	})
	return f.err
}
func (f *fakeMutator) SetCronJobSuspend(_ context.Context, namespace, name string, suspend bool, resourceVersion string, currentGeneration int64, at time.Time) error {
	f.cronJobSuspends = append(f.cronJobSuspends, cronJobSuspendCall{
		Namespace: namespace, Name: name, Suspend: suspend,
		ResourceVersion: resourceVersion, Generation: currentGeneration, At: at,
	})
	if err, ok := f.suspendErrByName[name]; ok {
		return err
	}
	return f.err
}
func (f *fakeMutator) SetCronJobSchedule(_ context.Context, namespace, name string, edit kube.CronJobScheduleEdit) (kube.CronJobScheduleResult, error) {
	f.cronJobSchedules = append(f.cronJobSchedules, cronJobScheduleCall{Namespace: namespace, Name: name, Edit: edit})
	if f.scheduleErr != nil {
		return kube.CronJobScheduleResult{}, f.scheduleErr
	}
	return f.scheduleResult, nil
}

func deleteAction() tui.TaskAction {
	return tui.TaskAction{
		ID:    "delete-pod",
		Label: "delete pod api",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindPod),
			ResourceName: "api",
			Namespace:    "prod",
			Verb:         "delete",
			IsMutating:   true,
		},
	}
}

func TestBeginRequiresConfirmationBeforeExecuting(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	if cmd := beginAt(&c, TierInline, deleteAction()); cmd != nil {
		t.Fatal("Begin with TierInline should not execute immediately")
	}
	if !c.Active() {
		t.Fatalf("state = %q, want confirming", c.State())
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("mutator called before confirmation: %v", mut.deleted)
	}
	if !strings.Contains(c.Prompt(), "prod/api") {
		t.Fatalf("prompt %q missing target", c.Prompt())
	}
}

func TestConfirmExecutesThroughMutator(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	beginAt(&c, TierInline, deleteAction())
	cmd := c.Confirm()
	if cmd == nil {
		t.Fatal("Confirm should return an execution command")
	}
	msg, ok := cmd().(ResultMsg)
	if !ok {
		t.Fatalf("command returned %T, want ResultMsg", cmd())
	}
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.deleted) != 1 || mut.deleted[0] != "Pod/prod/api" {
		t.Fatalf("deleted = %v, want [Pod/prod/api]", mut.deleted)
	}
	c.HandleResult(msg)
	if c.State() != tui.TaskStateSuccess {
		t.Fatalf("state = %q, want success", c.State())
	}
}

func TestCancelDoesNotExecute(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	beginAt(&c, TierInline, deleteAction())
	c.Cancel()
	if c.Active() {
		t.Fatal("still active after cancel")
	}
	if c.State() != tui.TaskStateCancelled {
		t.Fatalf("state = %q, want cancelled", c.State())
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("mutator called after cancel: %v", mut.deleted)
	}
}

func TestHandleResultSurfacesError(t *testing.T) {
	mut := &fakeMutator{err: errors.New("forbidden")}
	c := New(mut)
	beginAt(&c, TierInline, deleteAction())
	msg := c.Confirm()().(ResultMsg)
	c.HandleResult(msg)
	if c.State() != tui.TaskStateError {
		t.Fatalf("state = %q, want error", c.State())
	}
	if !strings.Contains(c.Message(), "forbidden") {
		t.Fatalf("message %q missing cause", c.Message())
	}
}

func TestBeginRejectsIncompleteScope(t *testing.T) {
	c := New(&fakeMutator{})
	beginAt(&c, TierInline, tui.TaskAction{ID: "x", Label: "bad", Scope: tui.TaskScope{Verb: "delete"}})
	if c.State() != tui.TaskStateError {
		t.Fatalf("state = %q, want error for incomplete scope", c.State())
	}
	if c.Active() {
		t.Fatal("incomplete action should not enter confirmation")
	}
}

// TestBeginAcceptsBulkActionWithEmptyResourceName pins a real bug the 0.8.0
// plan's browse-side marked-set suspend/resume caught: a bulk action has no
// single Scope.ResourceName (every target's name lives in BulkTargets
// instead), so Begin's incomplete-scope guard must not reject it the same
// way it rejects a genuinely-empty single-target action.
func TestBeginAcceptsBulkActionWithEmptyResourceName(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	action := tui.TaskAction{
		ID:    "cronjob-bulk-suspend",
		Label: "Suspend 2 cronjobs",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), Verb: "cronjob-suspend", IsMutating: true,
			BulkTargets: []tui.BulkTarget{
				{Namespace: "default", ResourceName: "a"},
				{Namespace: "default", ResourceName: "b"},
			},
		},
	}
	cmd := beginAt(&c, TierInline, action)
	if c.State() != tui.TaskStateConfirming {
		t.Fatalf("state = %q, want confirming (empty ResourceName must not fail a bulk action)", c.State())
	}
	if cmd != nil {
		t.Fatal("TierInline should not execute immediately")
	}
	if !c.Active() {
		t.Fatal("expected the bulk confirm to be active")
	}
}

func TestNilMutatorReportsUnconfigured(t *testing.T) {
	c := New(nil)
	beginAt(&c, TierInline, deleteAction())
	if c.State() != tui.TaskStateError {
		t.Fatalf("state = %q, want error", c.State())
	}
	if !strings.Contains(c.Message(), "not configured") {
		t.Fatalf("message %q", c.Message())
	}
}

func TestBeginRefusesWhileOffline(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	c.SetOffline(true)

	if cmd := beginAt(&c, TierNone, deleteAction()); cmd != nil {
		t.Fatal("Begin should not return an execution command while offline")
	}
	if c.Active() {
		t.Fatal("Begin should not enter the confirming state while offline")
	}
	if c.State() != tui.TaskStateError {
		t.Fatalf("state = %q, want error", c.State())
	}
	if !strings.Contains(c.Message(), "offline") {
		t.Fatalf("message %q, want mention of offline", c.Message())
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("mutator must not run while offline, got %v", mut.deleted)
	}

	c.SetOffline(false)
	if cmd := beginAt(&c, TierNone, deleteAction()); cmd == nil {
		t.Fatal("expected Begin to execute again once back online")
	}
}

func TestBeginTierNoneExecutesImmediately(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	cmd := beginAt(&c, TierNone, deleteAction())
	if cmd == nil {
		t.Fatal("expected TierNone to return an execution command immediately")
	}
	if c.Active() {
		t.Fatal("expected TierNone to skip the confirming state entirely")
	}
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.deleted) != 1 {
		t.Fatalf("expected the mutator called once, got %v", mut.deleted)
	}
}

func TestExecuteDispatchesSetMetaToPatchMeta(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID:    "set-meta-default/nva-worker/env",
		Label: "Set label env on nva-worker?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindDeployment), ResourceName: "nva-worker", Namespace: "default",
			Verb: "set-meta", IsMutating: true,
			MetaKey: "env", MetaValue: "staging", MetaOverwrite: true,
		},
	})
	if cmd == nil {
		t.Fatal("expected a TierNone set-meta action to return an execution command")
	}
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.metaPatches) != 1 || mut.metaPatches[0] != "Deployment/default/nva-worker labels env=staging" {
		t.Fatalf("metaPatches = %v, want one Deployment/default/nva-worker labels env=staging patch", mut.metaPatches)
	}
}

func TestExecuteDispatchesInitContainerSetImage(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID:    "set-image-default/api/prepare",
		Label: "Set image for api?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindDeployment), ResourceName: "api", Namespace: "default",
			Verb: "set-image", IsMutating: true,
			Container: "prepare", Image: "prepare:2.0", InitContainer: true,
		},
	})
	if cmd == nil {
		t.Fatal("expected a TierNone set-image action to return an execution command")
	}
	if msg := cmd().(ResultMsg); msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.setImages) != 1 || mut.setImages[0] != "Deployment/default/api prepare=prepare:2.0" {
		t.Fatalf("setImages = %v, want the prepare init-container update", mut.setImages)
	}
	if len(mut.setImageInit) != 1 || !mut.setImageInit[0] {
		t.Fatalf("setImageInit = %v, want [true]", mut.setImageInit)
	}
}

func TestExecuteDispatchesSecretDataToPatchSecretData(t *testing.T) {
	mut := &fakeMutator{dataNextRV: "42"}
	c := New(mut)
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID:    "add-secret-key-default/nva-secrets/SMTP_PASSWORD",
		Label: "Add key SMTP_PASSWORD to nva-secrets?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindSecret), ResourceName: "nva-secrets", Namespace: "default",
			Verb: "secret-data", IsMutating: true,
			SecretKey: "SMTP_PASSWORD", SecretValue: "hunter2-staging", DataResourceVersion: "41",
		},
	})
	if cmd == nil {
		t.Fatal("expected a TierNone secret-data action to return an execution command")
	}
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.secretDataPatches) != 1 || mut.secretDataPatches[0] != "default/nva-secrets SMTP_PASSWORD=hunter2-staging" {
		t.Fatalf("secretDataPatches = %v, want one default/nva-secrets SMTP_PASSWORD=hunter2-staging patch", mut.secretDataPatches)
	}
	if len(mut.dataResourceVersions) != 1 || mut.dataResourceVersions[0] != "41" {
		t.Fatalf("precondition = %v, want the scope's resourceVersion 41", mut.dataResourceVersions)
	}
	if msg.DataResourceVersion != "42" {
		t.Fatalf("ResultMsg.DataResourceVersion = %q, want the patch's returned 42", msg.DataResourceVersion)
	}
}

func TestExecuteDispatchesConfigMapDataToPatchConfigMapData(t *testing.T) {
	mut := &fakeMutator{dataNextRV: "42"}
	c := New(mut)
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID:    "add-configmap-key-default/nva-config/LOG_LEVEL",
		Label: "Add key LOG_LEVEL to nva-config?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindConfigMap), ResourceName: "nva-config", Namespace: "default",
			Verb: "configmap-data", IsMutating: true,
			ConfigMapKey: "LOG_LEVEL", ConfigMapValue: "debug", DataResourceVersion: "41",
		},
	})
	if cmd == nil {
		t.Fatal("expected a TierNone configmap-data action to return an execution command")
	}
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.configMapDataPatches) != 1 || mut.configMapDataPatches[0] != "default/nva-config LOG_LEVEL=debug" {
		t.Fatalf("configMapDataPatches = %v, want one default/nva-config LOG_LEVEL=debug patch", mut.configMapDataPatches)
	}
	if len(mut.rolloutRestarts) != 0 {
		t.Fatalf("rolloutRestarts = %v, want none for a plain apply", mut.rolloutRestarts)
	}
	if len(mut.dataResourceVersions) != 1 || mut.dataResourceVersions[0] != "41" {
		t.Fatalf("precondition = %v, want the scope's resourceVersion 41", mut.dataResourceVersions)
	}
	if msg.DataResourceVersion != "42" {
		t.Fatalf("ResultMsg.DataResourceVersion = %q, want the patch's returned 42", msg.DataResourceVersion)
	}
}

// TestExecuteConfigMapDataChainsRolloutRestart pins 27a's ctrl-r behavior:
// the patch runs, then every consumer in ConfigMapConsumers gets its own
// RolloutRestart call, kind carried through so a StatefulSet/DaemonSet
// consumer restarts correctly rather than being coerced to Deployment.
func TestExecuteConfigMapDataChainsRolloutRestart(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID:    "edit-configmap-key-default/nva-config/LOG_LEVEL",
		Label: "Update key LOG_LEVEL on nva-config?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindConfigMap), ResourceName: "nva-config", Namespace: "default",
			Verb: "configmap-data", IsMutating: true,
			ConfigMapKey: "LOG_LEVEL", ConfigMapValue: "debug",
			ConfigMapRestartConsumers: true,
			ConfigMapConsumers: []kube.ConfigMapConsumerRef{
				{Kind: kube.KindDeployment, Name: "nva-worker"},
				{Kind: kube.KindStatefulSet, Name: "nva-db"},
			},
		},
	})
	if cmd == nil {
		t.Fatal("expected a TierNone configmap-data action to return an execution command")
	}
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	want := []string{"Deployment/default/nva-worker", "StatefulSet/default/nva-db"}
	if len(mut.rolloutRestarts) != len(want) || mut.rolloutRestarts[0] != want[0] || mut.rolloutRestarts[1] != want[1] {
		t.Fatalf("rolloutRestarts = %v, want %v", mut.rolloutRestarts, want)
	}
}

func TestBeginTierInlineConfirmsWithoutTypedName(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	beginAt(&c, TierInline, deleteAction())
	if c.Tier() != TierInline {
		t.Fatalf("Tier() = %v, want TierInline", c.Tier())
	}
	// TierInline's Confirm must work with no typed name at all — it's a
	// bare y/N prompt, not the type-the-name modal.
	cmd := c.Confirm()
	if cmd == nil {
		t.Fatal("expected TierInline Confirm to execute without a typed name")
	}
	cmd()
	if len(mut.deleted) != 1 {
		t.Fatalf("expected the mutator called once, got %v", mut.deleted)
	}
}

func TestBeginTierModalRequiresNameMatch(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	beginAt(&c, TierModal, deleteAction())
	if c.Tier() != TierModal {
		t.Fatalf("Tier() = %v, want TierModal", c.Tier())
	}

	if cmd := c.Confirm(); cmd != nil {
		t.Fatal("expected Confirm to no-op before any name is typed")
	}
	for _, r := range "ap" {
		typeRune(&c, r)
	}
	if cmd := c.Confirm(); cmd != nil {
		t.Fatal("expected Confirm to no-op on a partial match")
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("expected no delete yet, got %v", mut.deleted)
	}

	typeRune(&c, 'i')
	if !c.NameMatches() {
		t.Fatalf("expected NameMatches once typed == %q, got typed %q", "api", c.TypedName())
	}
	cmd := c.Confirm()
	if cmd == nil {
		t.Fatal("expected Confirm to execute once the name matches")
	}
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.deleted) != 1 || mut.deleted[0] != "Pod/prod/api" {
		t.Fatalf("deleted = %v, want [Pod/prod/api]", mut.deleted)
	}
}

func TestBackspaceRemovesLastRuneUnicodeSafe(t *testing.T) {
	c := New(&fakeMutator{})
	beginAt(&c, TierModal, deleteAction())
	for _, r := range "aβc" {
		typeRune(&c, r)
	}
	if c.TypedName() != "aβc" {
		t.Fatalf("TypedName() = %q, want %q", c.TypedName(), "aβc")
	}
	backspace(&c)
	if c.TypedName() != "aβ" {
		t.Fatalf("TypedName() after Backspace = %q, want %q", c.TypedName(), "aβ")
	}
}

// TestTypedCursorTracksMotionKeys pins the caret position the modal draws
// its block cursor at. The motion keys always reached the buffer — it was
// TypedCursor that didn't exist, so components.TypeNameModal pinned "█" to
// the end of the string and every left/right/Home/End press looked like a
// no-op.
func TestTypedCursorTracksMotionKeys(t *testing.T) {
	c := New(&fakeMutator{})
	beginAt(&c, TierModal, deleteAction())
	for _, r := range "aβc" {
		typeRune(&c, r)
	}
	if got := c.TypedCursor(); got != 3 {
		t.Fatalf("TypedCursor() after typing = %d, want 3 (end of buffer)", got)
	}

	c.HandleTypeKey(tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := c.TypedCursor(); got != 2 {
		t.Fatalf("TypedCursor() after left = %d, want 2", got)
	}
	if c.TypedName() != "aβc" {
		t.Fatalf("left moved the caret but changed the text to %q", c.TypedName())
	}

	c.HandleTypeKey(tea.KeyPressMsg{Code: tea.KeyHome})
	if got := c.TypedCursor(); got != 0 {
		t.Fatalf("TypedCursor() after home = %d, want 0", got)
	}

	// A rune index, not a byte offset: β is two bytes, so an End that
	// reported 4 here would put the modal's cursor past the last rune.
	c.HandleTypeKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	if got := c.TypedCursor(); got != 3 {
		t.Fatalf("TypedCursor() after end = %d, want 3", got)
	}

	// Typing at a moved caret inserts there, which is exactly what the
	// pinned-to-the-end cursor used to misreport.
	c.HandleTypeKey(tea.KeyPressMsg{Code: tea.KeyLeft})
	typeRune(&c, 'x')
	if c.TypedName() != "aβxc" {
		t.Fatalf("TypedName() = %q, want %q", c.TypedName(), "aβxc")
	}
	if got := c.TypedCursor(); got != 3 {
		t.Fatalf("TypedCursor() after mid-buffer insert = %d, want 3", got)
	}
}

func TestTypeRuneAndBackspaceNoOpOutsideTierModal(t *testing.T) {
	c := New(&fakeMutator{})
	beginAt(&c, TierInline, deleteAction())
	typeRune(&c, 'x')
	if c.TypedName() != "" {
		t.Fatalf("expected TypeRune to no-op for TierInline, got %q", c.TypedName())
	}
}

func TestEscalateSwitchesToForceDelete(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	beginAt(&c, TierModal, deleteAction())
	typeRune(&c, 'a')
	c.EscalateForceDelete(false)
	// Escalating inside an already-open modal keeps the typed progress.
	for _, r := range "pi" {
		typeRune(&c, r)
	}
	cmd := c.Confirm()
	if cmd == nil {
		t.Fatal("expected Confirm to execute after Escalate + name match")
	}
	cmd()
	if len(mut.forceDeleted) != 1 || mut.forceDeleted[0] != "Pod/prod/api" {
		t.Fatalf("forceDeleted = %v, want [Pod/prod/api]", mut.forceDeleted)
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("expected the plain delete path untouched, got %v", mut.deleted)
	}
}

// TestEscalateFromInlineRaisesToTypedModal is the controller half of the
// "force delete is always modal" rule: escalating an inline y/N must land on
// TierModal, where "y"-style Confirm without the typed name runs nothing.
func TestEscalateFromInlineRaisesToTypedModal(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	beginAt(&c, TierInline, deleteAction())
	c.EscalateForceDelete(false)
	if c.Tier() != TierModal {
		t.Fatalf("Tier() = %v, want TierModal after escalating an inline confirm", c.Tier())
	}
	if c.Pending().Scope.Verb != "force-delete" {
		t.Fatalf("verb = %q, want force-delete", c.Pending().Scope.Verb)
	}
	if cmd := c.Confirm(); cmd != nil {
		t.Fatal("expected Confirm without the typed name to run nothing")
	}
	for _, r := range "api" {
		typeRune(&c, r)
	}
	cmd := c.Confirm()
	if cmd == nil {
		t.Fatal("expected Confirm to execute once the name matches")
	}
	cmd()
	if len(mut.forceDeleted) != 1 || len(mut.deleted) != 0 {
		t.Fatalf("forceDeleted=%v deleted=%v, want one force delete only", mut.forceDeleted, mut.deleted)
	}
}

func TestEscalateNeverLowersTier(t *testing.T) {
	c := New(&fakeMutator{})
	beginAt(&c, TierModal, deleteAction())
	c.escalate("force-delete", verbs.Policy{Tier: TierInline})
	if c.Tier() != TierModal {
		t.Fatalf("Tier() = %v, want TierModal kept", c.Tier())
	}
}

func TestEscalateNoOpsWhenNotConfirming(t *testing.T) {
	c := New(&fakeMutator{})
	beginAt(&c, TierModal, deleteAction())
	c.Cancel()
	c.EscalateForceDelete(false)
	if c.Active() || c.Pending() != nil {
		t.Fatal("expected Escalate to no-op once the confirm is gone")
	}
}

// --- 0.8.0 plan Phase 2: CronJob mutation dispatch ------------------------

func strPtr(s string) *string { return &s }

func cronJobRunNowAction() tui.TaskAction {
	at := time.Date(2026, 8, 11, 3, 4, 0, 0, time.UTC)
	return tui.TaskAction{
		ID:    "cronjob-run-now-default/nightly",
		Label: "Run nightly now?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: "nightly",
			Namespace: "default", Verb: "cronjob-run-now", IsMutating: true,
			NewName: "nightly-manual-0304", TriggerCreator: "michael", StagedAt: at,
		},
	}
}

// TestCronJobRunNowPassesNewNameCreatorAndStagedAtThrough pins that
// executeScope's "cronjob-run-now" branch forwards Scope.NewName/
// TriggerCreator/StagedAt to TriggerCronJob unchanged — the precomputed
// ManualJobName value the confirm preview already showed, and the identity
// of who/when staged the run (0.8.0 plan Phase 2 task 1/4, §36b).
func TestCronJobRunNowPassesNewNameCreatorAndStagedAtThrough(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	cmd := beginAt(&c, TierNone, cronJobRunNowAction())
	if cmd == nil {
		t.Fatal("expected TierNone cronjob-run-now to return an execution command")
	}
	msg, ok := cmd().(ResultMsg)
	if !ok {
		t.Fatalf("command returned %T, want ResultMsg", cmd())
	}
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.cronJobTriggers) != 1 {
		t.Fatalf("cronJobTriggers = %v, want one entry", mut.cronJobTriggers)
	}
	got := mut.cronJobTriggers[0]
	want := cronJobTriggerCall{
		Namespace: "default", Name: "nightly", NewJobName: "nightly-manual-0304",
		Creator: "michael", At: time.Date(2026, 8, 11, 3, 4, 0, 0, time.UTC),
	}
	if got != want {
		t.Fatalf("TriggerCronJob call = %+v, want %+v", got, want)
	}
}

// TestCronJobSuspendPassesResourceVersionGenerationAndStagedAtThrough pins
// executeScope's "cronjob-suspend" branch and, specifically, the generation
// math contract: the controller passes Scope.CronJobGeneration straight
// through as the CronJob's *current* (pre-patch) generation. Mutator
// implementations (real and fake) are the ones that add 1 when stamping
// kute.dev/suspended-generation (§3.3) — a controller that pre-incremented
// here would double-count against bumpCronJob/the real API server's own
// generation bump.
func TestCronJobSuspendPassesResourceVersionGenerationAndStagedAtThrough(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	at := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID: "cronjob-suspend-nightly", Label: "Suspend nightly?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: "nightly", Namespace: "default",
			Verb: "cronjob-suspend", IsMutating: true,
			CronJobResourceVersion: "42", CronJobGeneration: 3, StagedAt: at,
		},
	})
	if cmd == nil {
		t.Fatal("expected TierNone cronjob-suspend to return an execution command")
	}
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.cronJobSuspends) != 1 {
		t.Fatalf("cronJobSuspends = %v, want one entry", mut.cronJobSuspends)
	}
	got := mut.cronJobSuspends[0]
	if got.Namespace != "default" || got.Name != "nightly" || !got.Suspend {
		t.Fatalf("unexpected suspend call: %+v", got)
	}
	if got.Generation != 3 {
		t.Fatalf("Generation passed to SetCronJobSuspend = %d, want the pre-patch generation 3 unchanged", got.Generation)
	}
	if got.ResourceVersion != "42" {
		t.Fatalf("ResourceVersion = %q, want %q", got.ResourceVersion, "42")
	}
	if !got.At.Equal(at) {
		t.Fatalf("At = %v, want %v", got.At, at)
	}
}

// TestCronJobResumePassesResourceVersionAndGenerationThrough mirrors the
// suspend test for the "cronjob-resume" direction.
func TestCronJobResumePassesResourceVersionAndGenerationThrough(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID: "cronjob-resume-nightly", Label: "Resume nightly?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: "nightly", Namespace: "default",
			Verb: "cronjob-resume", IsMutating: true,
			CronJobResourceVersion: "43", CronJobGeneration: 4,
		},
	})
	cmd()
	if len(mut.cronJobSuspends) != 1 || mut.cronJobSuspends[0].Suspend {
		t.Fatalf("expected one resume (suspend=false) call, got %v", mut.cronJobSuspends)
	}
	got := mut.cronJobSuspends[0]
	if got.ResourceVersion != "43" || got.Generation != 4 {
		t.Fatalf("unexpected resume preconditions: %+v", got)
	}
}

// TestCronJobSetScheduleReturnsResultOnSuccess pins that a successful
// "cronjob-set-schedule" action reaches ResultMsg.CronJobSchedule with the
// mutator's own returned CronJobScheduleResult (0.8.0 plan Phase 2 task 10)
// — never a mere echo of the request — and that scheduleEdit's translation
// carries Schedule/TimeZone/ResourceVersion into the CronJobScheduleEdit
// unchanged.
func TestCronJobSetScheduleReturnsResultOnSuccess(t *testing.T) {
	result := kube.CronJobScheduleResult{Schedule: "*/15 * * * *", TimeZone: "America/New_York", ResourceVersion: "99"}
	mut := &fakeMutator{scheduleResult: result}
	c := New(mut)
	tz := "America/New_York"
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID: "cronjob-set-schedule-nightly", Label: "Update schedule?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: "nightly", Namespace: "default",
			Verb: "cronjob-set-schedule", IsMutating: true,
			Schedule: "*/15 * * * *", CronJobTimeZone: &tz, CronJobResourceVersion: "98",
		},
	})
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if msg.CronJobSchedule == nil {
		t.Fatal("expected ResultMsg.CronJobSchedule populated on success")
	}
	if *msg.CronJobSchedule != result {
		t.Fatalf("CronJobSchedule = %+v, want %+v", *msg.CronJobSchedule, result)
	}
	if len(mut.cronJobSchedules) != 1 {
		t.Fatalf("cronJobSchedules = %v, want one call", mut.cronJobSchedules)
	}
	edit := mut.cronJobSchedules[0].Edit
	if edit.Schedule != "*/15 * * * *" || edit.ResourceVersion != "98" {
		t.Fatalf("unexpected edit: %+v", edit)
	}
	if edit.TimeZone == nil || *edit.TimeZone != "America/New_York" {
		t.Fatalf("TimeZone = %v, want pointer to America/New_York", edit.TimeZone)
	}
}

// TestCronJobSetScheduleReturnsNilResultOnFailure pins the failure half of
// the same contract: ResultMsg.CronJobSchedule stays nil, never a
// zero-valued struct a caller could mistake for a real (if empty) result.
func TestCronJobSetScheduleReturnsNilResultOnFailure(t *testing.T) {
	mut := &fakeMutator{scheduleErr: errors.New("Conflict")}
	c := New(mut)
	cmd := beginAt(&c, TierNone, tui.TaskAction{
		ID: "cronjob-set-schedule-nightly", Label: "Update schedule?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: "nightly", Namespace: "default",
			Verb: "cronjob-set-schedule", IsMutating: true,
			Schedule: "*/15 * * * *", CronJobResourceVersion: "98",
		},
	})
	msg := cmd().(ResultMsg)
	if msg.Err == nil {
		t.Fatal("expected an error")
	}
	if msg.CronJobSchedule != nil {
		t.Fatalf("expected nil CronJobSchedule on failure, got %+v", *msg.CronJobSchedule)
	}
}

// TestScheduleEditTimeZoneThreeStateTranslation pins §3.8/§3.9's three-state
// timezone contract at the one place the translation happens (scheduleEdit):
// nil leaves spec.timeZone untouched, a pointer to "" clears it, a pointer
// to a non-empty IANA name sets it. Also confirms scheduleEdit copies the
// pointer rather than aliasing Scope's own, so a later mutation to the
// screen's buffer can't retroactively change an edit already dispatched.
func TestScheduleEditTimeZoneThreeStateTranslation(t *testing.T) {
	cases := []struct {
		name string
		tz   *string
	}{
		{"nil leaves untouched", nil},
		{"pointer to empty clears", strPtr("")},
		{"pointer to iana name sets", strPtr("America/New_York")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := tui.TaskScope{Schedule: "* * * * *", CronJobResourceVersion: "1", CronJobTimeZone: tc.tz}
			edit := scheduleEdit(scope)
			if (edit.TimeZone == nil) != (tc.tz == nil) {
				t.Fatalf("TimeZone nilness = %v, want %v", edit.TimeZone == nil, tc.tz == nil)
			}
			if tc.tz != nil {
				if edit.TimeZone == nil || *edit.TimeZone != *tc.tz {
					t.Fatalf("TimeZone = %v, want %v", edit.TimeZone, *tc.tz)
				}
				if edit.TimeZone == tc.tz {
					t.Fatal("expected scheduleEdit to copy the timezone pointer, not alias Scope's own")
				}
			}
		})
	}
}

// bulkSuspendAction builds a "cronjob-suspend" bulk action: the base Scope
// (ResourceName/Namespace/StagedAt) is shared across every target, and
// targets supplies the per-target Namespace/ResourceName/ResourceVersion/
// Generation substitution (0.8.0 plan Phase 2 task 11).
func bulkSuspendAction(targets ...tui.BulkTarget) tui.TaskAction {
	return tui.TaskAction{
		ID:    "cronjob-suspend-bulk",
		Label: "Suspend 2 cronjobs?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: "nightly", Namespace: "default",
			Verb: "cronjob-suspend", IsMutating: true,
			StagedAt:    time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC),
			BulkTargets: targets,
		},
	}
}

// TestBulkExecutionRunsOncePerTargetSubstitutingPerTargetFields pins
// executeBulk's own contract: one mutator call per Scope.BulkTargets entry,
// each substituting Namespace/ResourceName/CronJobResourceVersion/
// CronJobGeneration from the target while every other Scope field (here,
// StagedAt) stays shared and unchanged across every call.
func TestBulkExecutionRunsOncePerTargetSubstitutingPerTargetFields(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	targets := []tui.BulkTarget{
		{Namespace: "default", ResourceName: "nightly", ResourceVersion: "10", Generation: 1},
		{Namespace: "batch", ResourceName: "hourly", ResourceVersion: "20", Generation: 2},
	}
	cmd := beginAt(&c, TierNone, bulkSuspendAction(targets...))
	if cmd == nil {
		t.Fatal("expected a bulk TierNone action to return an execution command")
	}
	msg, ok := cmd().(BulkResultMsg)
	if !ok {
		t.Fatalf("command returned %T, want BulkResultMsg", cmd())
	}
	if len(msg.Results) != 2 {
		t.Fatalf("Results = %v, want 2 entries", msg.Results)
	}
	if len(mut.cronJobSuspends) != 2 {
		t.Fatalf("cronJobSuspends = %v, want 2 calls", mut.cronJobSuspends)
	}
	want := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	for i, target := range targets {
		got := mut.cronJobSuspends[i]
		if got.Namespace != target.Namespace || got.Name != target.ResourceName {
			t.Fatalf("call %d target = %+v, want namespace/name %s/%s", i, got, target.Namespace, target.ResourceName)
		}
		if got.ResourceVersion != target.ResourceVersion || got.Generation != target.Generation {
			t.Fatalf("call %d precondition = %+v, want version=%s generation=%d", i, got, target.ResourceVersion, target.Generation)
		}
		if !got.At.Equal(want) {
			t.Fatalf("call %d At = %v, want the shared StagedAt %v", i, got.At, want)
		}
		if msg.Results[i].Namespace != target.Namespace || msg.Results[i].ResourceName != target.ResourceName {
			t.Fatalf("Results[%d] = %+v, want namespace/name %s/%s", i, msg.Results[i], target.Namespace, target.ResourceName)
		}
		if msg.Results[i].Err != nil {
			t.Fatalf("Results[%d].Err = %v, want nil", i, msg.Results[i].Err)
		}
	}
}

// TestBulkExecutionReportsPerTargetPartialFailure drives one target to
// failure through the real mutator dispatch (not a hand-built BulkResultMsg)
// and checks both BulkResultMsg.Failed() and HandleBulkResult's partial-
// failure message/state.
func TestBulkExecutionReportsPerTargetPartialFailure(t *testing.T) {
	mut := &fakeMutator{suspendErrByName: map[string]error{"hourly": errors.New("conflict")}}
	c := New(mut)
	targets := []tui.BulkTarget{
		{Namespace: "default", ResourceName: "nightly", ResourceVersion: "10", Generation: 1},
		{Namespace: "batch", ResourceName: "hourly", ResourceVersion: "20", Generation: 2},
	}
	cmd := beginAt(&c, TierNone, bulkSuspendAction(targets...))
	msg := cmd().(BulkResultMsg)
	if len(msg.Results) != 2 {
		t.Fatalf("Results = %v, want 2 entries", msg.Results)
	}
	if msg.Results[0].Err != nil {
		t.Fatalf("expected nightly to succeed, got %v", msg.Results[0].Err)
	}
	if msg.Results[1].Err == nil {
		t.Fatal("expected hourly to fail")
	}
	failed := msg.Failed()
	if len(failed) != 1 || failed[0].ResourceName != "hourly" {
		t.Fatalf("Failed() = %v, want only hourly", failed)
	}

	c.HandleBulkResult(msg)
	if c.State() != tui.TaskStateError {
		t.Fatalf("state = %q, want error", c.State())
	}
	if !strings.Contains(c.Message(), "1 of 2") {
		t.Fatalf("message %q, want mention of 1 of 2 targets failing", c.Message())
	}
}

// TestHandleBulkResultAllSuccess pins HandleBulkResult's success path: no
// failed targets means TaskStateSuccess with a count in the message.
func TestHandleBulkResultAllSuccess(t *testing.T) {
	c := New(&fakeMutator{})
	msg := BulkResultMsg{ActionID: "x", Label: "Suspend", Results: []TargetResult{
		{Namespace: "default", ResourceName: "nightly"},
		{Namespace: "batch", ResourceName: "hourly"},
	}}
	c.HandleBulkResult(msg)
	if c.State() != tui.TaskStateSuccess {
		t.Fatalf("state = %q, want success", c.State())
	}
	if !strings.Contains(c.Message(), "2") {
		t.Fatalf("message %q, want a mention of the 2 succeeded targets", c.Message())
	}
	if len(msg.Failed()) != 0 {
		t.Fatalf("Failed() = %v, want none", msg.Failed())
	}
}

// TestHandleBulkResultAllFail pins the fully-failed path: every target
// failing is worded distinctly from a partial failure ("all N targets"),
// per HandleBulkResult's own doc comment.
func TestHandleBulkResultAllFail(t *testing.T) {
	c := New(&fakeMutator{})
	msg := BulkResultMsg{ActionID: "x", Label: "Suspend", Results: []TargetResult{
		{Namespace: "default", ResourceName: "nightly", Err: errors.New("conflict")},
		{Namespace: "batch", ResourceName: "hourly", Err: errors.New("forbidden")},
	}}
	c.HandleBulkResult(msg)
	if c.State() != tui.TaskStateError {
		t.Fatalf("state = %q, want error", c.State())
	}
	if !strings.Contains(c.Message(), "all 2") {
		t.Fatalf("message %q, want mention of all targets failing", c.Message())
	}
	if len(msg.Failed()) != 2 {
		t.Fatalf("Failed() = %v, want both entries", msg.Failed())
	}
}

// --- 0.8.0 plan Phase 3: verb registry and action policy ------------------

// TestConfirmRequiresTypedNameForCronJobSuspendInProd pins the typed-name
// policy actually gating Confirm() — the same
// name-match requirement TestBeginTierModalRequiresNameMatch pins for
// delete, now exercised through cronjob-suspend's own verb string.
func TestConfirmRequiresTypedNameForCronJobSuspendInProd(t *testing.T) {
	mut := &fakeMutator{}
	c := New(mut)
	action := tui.TaskAction{
		ID:    "cronjob-suspend-nightly",
		Label: "Suspend nightly?",
		Scope: tui.TaskScope{
			ResourceKind: string(kube.KindCronJob), ResourceName: "nightly", Namespace: "default",
			Verb: "cronjob-suspend", IsMutating: true,
		},
	}
	c.Begin(Request{Verb: verbs.CronJobSuspend, Prod: true}, action)
	if cmd := c.Confirm(); cmd != nil {
		t.Fatal("expected Confirm to no-op before any name is typed")
	}
	for _, r := range "night" {
		typeRune(&c, r)
	}
	if cmd := c.Confirm(); cmd != nil {
		t.Fatal("expected Confirm to no-op on a partial match")
	}
	if len(mut.cronJobSuspends) != 0 {
		t.Fatalf("expected no suspend call yet, got %v", mut.cronJobSuspends)
	}

	for _, r := range "ly" {
		typeRune(&c, r)
	}
	if !c.NameMatches() {
		t.Fatalf("expected NameMatches once typed == %q, got typed %q", "nightly", c.TypedName())
	}
	cmd := c.Confirm()
	if cmd == nil {
		t.Fatal("expected Confirm to execute once the name matches")
	}
	msg := cmd().(ResultMsg)
	if msg.Err != nil {
		t.Fatalf("unexpected error: %v", msg.Err)
	}
	if len(mut.cronJobSuspends) != 1 {
		t.Fatalf("cronJobSuspends = %v, want one call", mut.cronJobSuspends)
	}
}

// --- review 10 M1: Begin resolves the confirm policy itself ---------------

// TestBeginResolvesPolicy: Begin derives the target from the action's own
// Scope — kind (cascade escalation), direction (Verb.ReverseID) and key
// removal — so a call site can't hand it a tier that disagrees with what
// executes.
func TestBeginResolvesPolicy(t *testing.T) {
	scope := func(kind kube.ResourceKind, verb string) tui.TaskAction {
		return tui.TaskAction{ID: "x", Label: "x?", Scope: tui.TaskScope{
			ResourceKind: string(kind), ResourceName: "api", Namespace: "dev", Verb: verb, IsMutating: true,
		}}
	}
	metaRemove := scope(kube.KindPod, "set-meta")
	metaRemove.Scope.MetaRemove = true
	tests := []struct {
		name      string
		req       Request
		action    tui.TaskAction
		wantTier  Tier
		wantTyped bool
	}{
		{"pod delete", Request{Verb: verbs.Delete}, scope(kube.KindPod, "delete"), TierInline, false},
		{"pod delete prod", Request{Verb: verbs.Delete, Prod: true}, scope(kube.KindPod, "delete"), TierModal, true},
		{"namespace delete", Request{Verb: verbs.Delete}, scope(kube.KindNamespace, "delete"), TierModal, true},
		{"cronjob suspend prod", Request{Verb: verbs.CronJobSuspend, Prod: true}, scope(kube.KindCronJob, "cronjob-suspend"), TierModal, true},
		{"cronjob resume prod", Request{Verb: verbs.CronJobSuspend, Prod: true}, scope(kube.KindCronJob, "cronjob-resume"), TierNone, false},
		{"drain", Request{Verb: verbs.Drain}, scope(kube.KindNode, "drain"), TierModal, false},
		{"argo sync prod", Request{Verb: verbs.ArgoSync, Prod: true}, scope(kube.KindPod, "argo-sync"), TierModal, false},
		{"meta removal", Request{Verb: verbs.Meta}, metaRemove, TierInline, false},
		{"meta joined", Request{Verb: verbs.Meta, Joined: true}, scope(kube.KindPod, "set-meta"), TierInline, false},
		{"secret overwrite", Request{Verb: verbs.AddSecretKey, Overwrite: true}, scope(kube.KindSecret, "secret-data"), TierInline, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(&fakeMutator{})
			cmd := c.Begin(tt.req, tt.action)
			if tt.wantTier == TierNone {
				if cmd == nil || c.Active() {
					t.Fatalf("expected immediate execution, got cmd=%v active=%v", cmd != nil, c.Active())
				}
				return
			}
			if !c.Active() || c.Tier() != tt.wantTier || c.TypeToConfirm() != tt.wantTyped {
				t.Fatalf("active=%v tier=%v typed=%v, want tier=%v typed=%v",
					c.Active(), c.Tier(), c.TypeToConfirm(), tt.wantTier, tt.wantTyped)
			}
		})
	}
}

func deleteOf(kind kube.ResourceKind) tui.TaskAction {
	return tui.TaskAction{
		ID: "delete", Label: "Delete api?",
		Scope: tui.TaskScope{ResourceKind: string(kind), ResourceName: "api", Namespace: "dev", Verb: verbs.Delete.ID, IsMutating: true},
	}
}

// TestEscalateForceDeleteIsModalOutsideProd: escalating a non-prod inline
// Pod delete must reach the type-the-name modal, never stay an inline y/N.
func TestEscalateForceDeleteIsModalOutsideProd(t *testing.T) {
	c := New(&fakeMutator{})
	c.Begin(Request{Verb: verbs.Delete}, deleteOf(kube.KindPod))
	if !c.EscalateForceDelete(false) {
		t.Fatal("expected EscalateForceDelete to claim a pending Pod delete")
	}
	if c.Tier() != TierModal || c.Pending().Scope.Verb != verbs.ForceDelete.ID {
		t.Fatalf("tier=%v verb=%q, want TierModal force-delete", c.Tier(), c.Pending().Scope.Verb)
	}
	if !c.TypeToConfirm() {
		t.Fatal("force-delete's modal must be type-the-name")
	}
}

func TestEscalateForceDeleteIgnoresOtherKindsAndVerbs(t *testing.T) {
	c := New(&fakeMutator{})
	c.Begin(Request{Verb: verbs.Delete}, deleteOf(kube.KindDeployment))
	if c.EscalateForceDelete(false) || c.Pending().Scope.Verb != verbs.Delete.ID || c.Tier() != TierInline {
		t.Fatalf("expected a Deployment delete to stay an inline delete, got tier=%v verb=%q", c.Tier(), c.Pending().Scope.Verb)
	}

	drain := deleteOf(kube.KindNode)
	drain.Scope.Verb = verbs.Drain.ID
	c.Begin(Request{Verb: verbs.Drain}, drain)
	if c.EscalateForceDelete(false) || c.Pending().Scope.Verb != verbs.Drain.ID {
		t.Fatalf("expected a drain to be left alone, got %q", c.Pending().Scope.Verb)
	}
}
