package jobattempts

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/resources"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
)

// step drains a tea.Cmd chain synchronously — mirrors cronjobdetail_test.go's
// own step, duplicated per the repo's package-local-seam convention. Never
// called on Init()'s own batch: tickCmd/spinner.Tick both recur forever
// under synchronous draining.
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

func newSession(namespace string) *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster", Namespace: namespace}}
}

func int32Ptr(v int32) *int32 { return &v }

func newModel(t *testing.T, c *fake.Cluster, namespace, name string) Model {
	t.Helper()
	m := New(Config{Session: newSession(namespace), Lister: c, Mutator: c, Namespace: namespace, Name: name})
	m.SetSize(120, 40)
	return step(t, m, m.load()())
}

func newFakeJob(namespace, name string) (*fake.Cluster, *batchv1.Job) {
	c := fake.New(namespace, "test-cluster")
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: "job-uid-1"},
		Spec:       batchv1.JobSpec{Completions: int32Ptr(1)},
	}
	c.Seed(kube.KindJob, j)
	return c, j
}

func TestLoadFoundTransitionsToReady(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	m := newModel(t, c, "default", "batch-1")
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready", m.state)
	}
	if !m.found {
		t.Fatalf("expected found = true")
	}
}

func TestLoadNotFoundRendersDeletedState(t *testing.T) {
	c := fake.New("default", "test-cluster")
	m := newModel(t, c, "default", "batch-missing")
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %v, want Ready (found=false still renders, just the deleted banner)", m.state)
	}
	if m.found {
		t.Fatalf("expected found = false for a Job that doesn't exist")
	}
}

func TestBeginRerunStagesCreateChoiceByDefault(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	m := newModel(t, c, "default", "batch-1")
	m.beginRerun()
	if m.pendingRerun == nil {
		t.Fatalf("expected beginRerun to stage a pendingRerun target")
	}
	if m.pendingRerun.NewName != "batch-1-rerun-1" {
		t.Errorf("newName = %q, want %q", m.pendingRerun.NewName, "batch-1-rerun-1")
	}
}

// The create/replace choice has no text buffer, so j/k toggle it like ↑↓.
func TestRerunChoiceTogglesOnJK(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	m := newModel(t, c, "default", "batch-1")
	m.beginRerun()
	m = step(t, m, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.pendingRerun.Choice != resources.JobRerunReplace {
		t.Fatalf("choice = %v after j, want replace", m.pendingRerun.Choice)
	}
	m = step(t, m, tea.KeyPressMsg{Code: 'k', Text: "k"})
	if m.pendingRerun.Choice != resources.JobRerunCreate {
		t.Fatalf("choice = %v after k, want create", m.pendingRerun.Choice)
	}
}

func TestCommitRerunCreateExecutesImmediately(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	m := newModel(t, c, "default", "batch-1")
	m.beginRerun()
	m = step(t, m, m.commitRerun()())
	if m.pendingRerun != nil {
		t.Fatalf("expected commitRerun to clear the staged preview")
	}
	if m.actions.Active() {
		t.Fatalf("expected create to execute at TierNone, not enter a confirm state")
	}
}

func TestCommitRerunReplaceGoesThroughOrdinaryConfirm(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	m := newModel(t, c, "default", "batch-1")
	m.beginRerun()
	m.pendingRerun.Choice = 1 // resources.JobRerunReplace
	if cmd := m.commitRerun(); cmd != nil {
		// TierInline's Begin returns nil (it only moves to TaskStateConfirming,
		// never executes) — this branch would only fire if replace somehow
		// resolved to TierNone, which would itself be the bug under test.
		m = step(t, m, cmd())
	}
	if m.pendingRerun != nil {
		t.Fatalf("expected the staged preview cleared once handed to the ordinary confirm")
	}
	if !m.actions.Active() || m.actions.Tier() != actions.TierInline {
		t.Fatalf("expected replace to land on the ordinary TierInline confirm, tier=%v", m.actions.Tier())
	}
}

// stubTask is a trivial tea.Model good enough to prove a callback fired and
// its return value was threaded through — mirrors browse_nodes_test.go's own
// stubTask rather than constructing a real poddetail.Model.
type stubTask struct{}

func (stubTask) Init() tea.Cmd                       { return nil }
func (stubTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return stubTask{}, nil }
func (stubTask) View() tea.View                      { return tea.NewView("") }

func TestEnterOpensPodDetailForSelectedAttempt(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	pod := kube.Pod{Namespace: "default", Name: "batch-1-attempt-1"}

	var gotPod kube.Pod
	var gotSiblings []string
	var gotIndex int
	openPodDetailCalled := false
	openLogsCalled := false

	m := New(Config{
		Session: newSession("default"),
		Lister:  c,
		Mutator: c,
		OpenPodDetail: func(p kube.Pod, siblings []string, index, w, h int) (tea.Model, tea.Cmd) {
			openPodDetailCalled = true
			gotPod, gotSiblings, gotIndex = p, siblings, index
			return stubTask{}, nil
		},
		OpenLogs: func(kube.Pod, string, int, int) (tea.Model, tea.Cmd) {
			openLogsCalled = true
			return stubTask{}, nil
		},
		Namespace: "default",
		Name:      "batch-1",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())

	// Seed a fabricated attempt so selectedAttemptData resolves to our pod —
	// mirrors how a real load() would populate m.summary/m.pods from the
	// fake cluster's Job+Pod aggregation.
	m.summary.Attempts = []resources.JobAttempt{{PodName: pod.Name}}
	m.pods = map[string]kube.Pod{pod.Name: pod}
	m.selectedAttempt = 0

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "enter", Code: tea.KeyEnter})
	if cmd != nil {
		cmd()
	}
	_ = updated

	if !openPodDetailCalled {
		t.Fatalf("expected enter to invoke OpenPodDetail")
	}
	if openLogsCalled {
		t.Fatalf("expected enter not to invoke OpenLogs")
	}
	if gotPod.Name != pod.Name {
		t.Errorf("OpenPodDetail pod = %q, want %q", gotPod.Name, pod.Name)
	}
	if len(gotSiblings) != 1 || gotSiblings[0] != pod.Name {
		t.Errorf("OpenPodDetail siblings = %v, want [%q] (single-pod handoff)", gotSiblings, pod.Name)
	}
	if gotIndex != 0 {
		t.Errorf("OpenPodDetail index = %d, want 0", gotIndex)
	}
}

func TestLogsKeyStillOpensLogsForSelectedAttempt(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	pod := kube.Pod{Namespace: "default", Name: "batch-1-attempt-1"}

	openPodDetailCalled := false
	openLogsCalled := false
	var gotPod kube.Pod

	m := New(Config{
		Session: newSession("default"),
		Lister:  c,
		Mutator: c,
		OpenPodDetail: func(kube.Pod, []string, int, int, int) (tea.Model, tea.Cmd) {
			openPodDetailCalled = true
			return stubTask{}, nil
		},
		OpenLogs: func(p kube.Pod, container string, w, h int) (tea.Model, tea.Cmd) {
			openLogsCalled = true
			gotPod = p
			return stubTask{}, nil
		},
		Namespace: "default",
		Name:      "batch-1",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())

	m.summary.Attempts = []resources.JobAttempt{{PodName: pod.Name}}
	m.pods = map[string]kube.Pod{pod.Name: pod}
	m.selectedAttempt = 0

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "l", Code: 'l'})
	if cmd != nil {
		cmd()
	}
	_ = updated

	if !openLogsCalled {
		t.Fatalf("expected 'l' to invoke OpenLogs")
	}
	if openPodDetailCalled {
		t.Fatalf("expected 'l' not to invoke OpenPodDetail")
	}
	if gotPod.Name != pod.Name {
		t.Errorf("OpenLogs pod = %q, want %q", gotPod.Name, pod.Name)
	}
}

func TestMoveSiblingLoadsNextJob(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	c.Seed(kube.KindJob, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "batch-2", Namespace: "default", UID: "job-uid-2"},
		Spec:       batchv1.JobSpec{Completions: int32Ptr(1)},
	})
	m := newModel(t, c, "default", "batch-1")
	m.siblings = []SiblingRef{{Namespace: "default", Name: "batch-1"}, {Namespace: "default", Name: "batch-2"}}
	m.siblingIndex = 0
	cmd := m.moveSibling(1)
	if cmd == nil {
		t.Fatalf("expected moveSibling to return a load command")
	}
	m = step(t, m, cmd())
	if m.name != "batch-2" {
		t.Fatalf("name = %q, want batch-2 after moving to the next sibling", m.name)
	}
	if m.state != tui.TaskStateReady || !m.found {
		t.Fatalf("expected the sibling load to settle Ready/found, got state=%v found=%v", m.state, m.found)
	}
}

// TestBeginRerunSkipsExistingRerunNames pins review 06 M6: once
// batch-1-rerun-1 exists, the ledger's next rerun must propose
// batch-1-rerun-2, as browse's copy does, rather than a name whose create
// fails AlreadyExists after the user commits.
func TestBeginRerunSkipsExistingRerunNames(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	c.Seed(kube.KindJob, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "batch-1-rerun-1", Namespace: "default", UID: "job-uid-2"}})
	m := newModel(t, c, "default", "batch-1")
	m.beginRerun()
	if m.pendingRerun == nil {
		t.Fatalf("expected beginRerun to stage a pendingRerun target")
	}
	if m.pendingRerun.NewName != "batch-1-rerun-2" {
		t.Errorf("newName = %q, want %q — batch-1-rerun-1 already exists", m.pendingRerun.NewName, "batch-1-rerun-2")
	}
}

// TestKeysWhileLoadingNeverPopTheScreen pins review 06 M7 — see
// cronjobdetail's own copy: a key during the initial load, or a second ']'
// while a sibling loads, must not hit the deleted banner's "any key goes
// back".
func TestKeysWhileLoadingNeverPopTheScreen(t *testing.T) {
	c, _ := newFakeJob("default", "batch-1")
	m := New(Config{Session: newSession("default"), Lister: c, Mutator: c, Namespace: "default", Name: "batch-1",
		Siblings: []SiblingRef{
			{Namespace: "default", Name: "batch-1"}, {Namespace: "default", Name: "batch-2"}, {Namespace: "default", Name: "batch-3"},
		},
	})
	m.SetSize(120, 40)
	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %v, want Loading before the first load lands", m.state)
	}
	for _, key := range []string{"j", "]", "]"} {
		updated, cmd := m.Update(tea.KeyPressMsg{Text: key})
		m = *updated.(*Model)
		if cmd != nil {
			if msg := cmd(); msg != nil {
				if _, back := msg.(tui.BackMsg); back {
					t.Fatalf("%q while loading sent BackMsg, want the screen to stay", key)
				}
			}
		}
	}
	if m.name != "batch-3" {
		t.Fatalf("name = %q, want batch-3 — ']' while a sibling loads still moves", m.name)
	}
}
