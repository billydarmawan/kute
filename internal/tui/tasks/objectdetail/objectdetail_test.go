package objectdetail

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objs[kind], nil
}

type fakeEvents struct {
	rows []kube.Event
	err  error
}

func (f fakeEvents) ObjectEvents(_ context.Context, _ string, _ kube.ResourceKind, _ string) ([]kube.Event, error) {
	return f.rows, f.err
}

func certificateKind() kube.ResourceKind { return kube.ResourceKind("Certificate") }

// testSession builds Certificate's Descriptor via the plain generic
// resources.CustomDescriptor rather than resources.BuildDiscoveredRegistry:
// as of §35b, Certificate gets its own curated Descriptor from the latter
// (resources/certmanager.go), and a Certificate row's own ↵ never actually
// reaches 14d in the real app any more (browse/certchain.go's isCertificateKind
// gate always routes it to tasks/certchain instead — see app.go's
// unconditional OpenCertChain wiring). This package's own worked example
// (14d's own design-doc-quoted "Issuing certificate as Secret does not
// exist") is a generic-CRD-detail illustration, not a claim that Certificate
// itself still reaches this screen — so it keeps exercising 14d's actual
// generic path (CustomDescriptor) directly, bypassing the curated dispatch
// entirely, rather than switching to an unrelated example kind.
func testSession() *tui.Session {
	dk := kube.DiscoveredKind{
		Kind: "Certificate", Plural: "certificates", Group: "cert-manager.io",
		Versions:      []kube.CRDVersion{{Name: "v1", Served: true, Storage: true}},
		ClusterScoped: false,
		PrinterColumns: []kube.PrinterColumn{
			{Name: "Ready", Type: "string", JSONPath: `.status.conditions[?(@.type=="Ready")].status`},
			{Name: "Secret", Type: "string", JSONPath: ".spec.secretName"},
		},
		Established: true,
	}
	reg := resources.DefaultRegistry()
	reg.Register(resources.CustomDescriptor(dk))
	return &tui.Session{Theme: tui.Dark(), Registry: reg, Location: tui.Location{Context: "test-cluster"}}
}

func certObj(name string, cond map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec":       map[string]any{"secretName": name + "-secret"},
	}
	if cond != nil {
		obj["status"] = map[string]any{"conditions": []any{cond}}
	}
	return &unstructured.Unstructured{Object: obj}
}

// step drains a returned tea.Cmd synchronously against a *Model, mirroring
// poddetail's own test helper — objectdetail's Update has the same pointer-
// receiver shape.
func step(t *testing.T, m *Model, msg tea.Msg) (tea.Model, tea.Cmd) {
	t.Helper()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var last tea.Model = m
		for _, c := range batch {
			if c == nil {
				continue
			}
			next, cmd := step(t, last.(*Model), c())
			last = next
			if cmd != nil {
				return step(t, last.(*Model), cmd())
			}
		}
		return last, nil
	}
	return m.Update(msg)
}

func TestApplyLoadedReadyState(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		certificateKind(): {certObj("api-tls", map[string]any{"type": "Ready", "status": "True", "message": "all good"})},
	}}
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{},
		Kind: certificateKind(), Namespace: "default", Name: "api-tls",
	})
	updated, _ := step(t, &m, m.load()())
	got := updated.(*Model)
	if got.state != tui.TaskStateReady || !got.found {
		t.Fatalf("expected ready+found, got state=%s found=%v feedback=%q", got.state, got.found, got.feedback)
	}
	if len(got.conditions) != 1 || got.conditions[0].Status != "True" {
		t.Fatalf("unexpected conditions: %+v", got.conditions)
	}
	if got.row.Cells[2] != "api-tls-secret" {
		t.Fatalf("unexpected meta cell: %+v", got.row.Cells)
	}
}

func TestApplyLoadedObjectGone(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{}}
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{},
		Kind: certificateKind(), Namespace: "default", Name: "missing",
	})
	updated, _ := step(t, &m, m.load()())
	got := updated.(*Model)
	if !got.gone || got.state != tui.TaskStateReady {
		t.Fatalf("expected gone=true/ready, got gone=%v state=%s", got.gone, got.state)
	}

	// q/ctrl+c must still quit even while the "deleted" banner is showing —
	// mirrors poddetail's own TestGonePodStillQuitsOnQAndCtrlC.
	for _, key := range []string{"ctrl+q", "ctrl+c"} {
		_, cmd := got.Update(tea.KeyPressMsg{Text: key})
		if cmd == nil {
			t.Fatalf("%s: expected a command", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s: expected tea.QuitMsg, got %T", key, cmd())
		}
	}
}

func TestApplyLoadedEmptyObjectRedirectsToYAML(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		certificateKind(): {certObj("bare", nil)}, // no conditions
	}}
	yamlCalled := false
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{}, // no events either
		Kind: certificateKind(), Namespace: "default", Name: "bare",
		OpenYAML: func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
			yamlCalled = true
			return sentinelTask{}, nil
		},
	})
	updated, _ := step(t, &m, m.load()())
	if _, ok := updated.(*Model); ok {
		t.Fatalf("expected the empty-object redirect to swap away from *Model, still got one")
	}
	if !yamlCalled {
		t.Fatalf("expected OpenYAML to be called for an object with no conditions and no events")
	}
	// The redirecting instance must report itself transient, or the root
	// pushes it under the YAML view and esc pops back into a load that
	// redirects again — an inescapable screen.
	var tr tui.Transient = &m
	if !tr.Transient() {
		t.Fatal("a redirected objectdetail must report Transient() true so the root drops it from the stack")
	}
}

func TestNonRedirectingModelIsNotTransient(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		certificateKind(): {certObj("ready", map[string]any{"type": "Ready", "status": "True"})},
	}}
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{},
		Kind: certificateKind(), Namespace: "default", Name: "ready",
	})
	updated, _ := step(t, &m, m.load()())
	got := updated.(*Model)
	if got.Transient() {
		t.Fatal("a detail screen that rendered normally must stay on the navigation stack")
	}
}

func TestApplyLoadedEventsErrorDoesNotRedirect(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		certificateKind(): {certObj("bare", nil)},
	}}
	yamlCalled := false
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{err: context.DeadlineExceeded},
		Kind: certificateKind(), Namespace: "default", Name: "bare",
		OpenYAML: func(kind kube.ResourceKind, namespace, name string, width, height int) (tea.Model, tea.Cmd) {
			yamlCalled = true
			return sentinelTask{}, nil
		},
	})
	updated, _ := step(t, &m, m.load()())
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("an events-fetch error is not \"empty\" — should not redirect to YAML")
	}
	if yamlCalled {
		t.Fatalf("OpenYAML should not be called when the events fetch merely failed")
	}
}

// sentinelTask is a minimal tea.Model distinct from *Model, so tests can
// assert a redirect actually swapped the active task.
type sentinelTask struct{}

func (sentinelTask) Init() tea.Cmd                       { return nil }
func (sentinelTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
func (sentinelTask) View() tea.View                      { return tea.NewView("") }

func TestMoveSiblingLoadsNextObject(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		certificateKind(): {
			certObj("a", map[string]any{"type": "Ready", "status": "True"}),
			certObj("b", map[string]any{"type": "Ready", "status": "False"}),
		},
	}}
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{},
		Kind: certificateKind(), Namespace: "default", Name: "a",
		Siblings: []SiblingRef{{Namespace: "default", Name: "a"}, {Namespace: "default", Name: "b"}}, SiblingIndex: 0,
	})
	cmd := m.moveSibling(1)
	if cmd == nil {
		t.Fatalf("expected a load cmd from moveSibling")
	}
	if m.name != "b" || m.siblingIndex != 1 {
		t.Fatalf("expected name=b index=1, got name=%s index=%d", m.name, m.siblingIndex)
	}
}

type fakeMutator struct {
	deleted []string
	err     error // returned by DeleteResource when set
}

func (f *fakeMutator) DeleteResource(_ context.Context, _ kube.ResourceKind, _, name string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, name)
	return nil
}
func (f *fakeMutator) DeleteResourceForced(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeMutator) RolloutRestart(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeMutator) Cordon(context.Context, string, bool) error { return nil }
func (f *fakeMutator) Drain(context.Context, string) (int, error) { return 0, nil }
func (f *fakeMutator) HelmRollback(context.Context, string, string, int) error {
	return nil
}
func (f *fakeMutator) RolloutUndo(context.Context, string, string, int) error {
	return nil
}
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
func (f *fakeMutator) PatchSecretData(context.Context, string, string, string, string, string, bool) error {
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

// TestKeybarGoesOfflineAndHidesDelete pins the cross-cutting 4a fix
// (docs/design README.md §52, §301): objectdetail must show the OFFLINE
// pill and drop delete from the keybar while disconnected, not just browse.
func TestKeybarGoesOfflineAndHidesDelete(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		certificateKind(): {certObj("api-tls", map[string]any{"type": "Ready", "status": "True"})},
	}}
	mut := &fakeMutator{}
	m := New(Config{
		Session: testSession(), Lister: lister, Events: fakeEvents{}, Mutator: mut,
		Kind: certificateKind(), Namespace: "default", Name: "api-tls",
	})
	updated, _ := step(t, &m, m.load()())
	got := updated.(*Model)

	got2, _ := step(t, got, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	got = got2.(*Model)

	kb := got.Keybar()
	if kb.Pill != tui.ModeOffline || kb.PillText != "OFFLINE" {
		t.Fatalf("Pill/PillText = %v/%q while offline, want ModeOffline/OFFLINE", kb.Pill, kb.PillText)
	}
	for _, g := range kb.Groups {
		for _, h := range g {
			if h.Key == verbs.Delete.Key {
				t.Fatalf("expected delete hint hidden while offline, got groups %+v", kb.Groups)
			}
		}
	}

	got3, _ := step(t, got, kube.ConnStateMsg{Phase: kube.ConnConnected})
	got = got3.(*Model)
	kb = got.Keybar()
	if kb.PillText != "DETAIL" {
		t.Fatalf("PillText = %q after reconnect, want DETAIL", kb.PillText)
	}
}
