package poddetail

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/actions"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

func plain(s string) string { return ansi.Strip(s) }

type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objs[kind], nil
}

func newSession() *tui.Session {
	return &tui.Session{Theme: tui.Dark(), Location: tui.Location{Context: "test-cluster"}}
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

// crashLoopPod mirrors kube/fake/fixtures.go's demoCrashLoopPod shape: a
// container currently Waiting/CrashLoopBackOff whose LastTerminationState
// carries the exit-137/OOMKilled-style last termination the 5a banner needs.
func crashLoopPod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:            map[string]string{"app": "worker"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "worker-abc123"}},
			CreationTimestamp: metav1.Now(),
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name:  "worker",
				Image: "example.com/worker:v1",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
			}},
			Tolerations: []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
			PodIP:    "10.0.0.5",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "worker",
				Ready:        false,
				RestartCount: 6,
				State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   137,
						Reason:     "OOMKilled",
						FinishedAt: metav1.Now(),
					},
				},
			}},
		},
	}
}

func runningPod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: metav1.Now()},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "app", Image: "example.com/app:v1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

type fakeEvents struct {
	events []kube.Event
}

func (f fakeEvents) ObjectEvents(context.Context, string, kube.ResourceKind, string) ([]kube.Event, error) {
	return f.events, nil
}

type fakeMutator struct {
	deleted      []string
	forceDeleted []string
	err          error // returned by DeleteResource when set
}

func (f *fakeMutator) DeleteResource(_ context.Context, _ kube.ResourceKind, _ string, name string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, name)
	return nil
}
func (f *fakeMutator) DeleteResourceForced(_ context.Context, _ kube.ResourceKind, _ string, name string) error {
	f.forceDeleted = append(f.forceDeleted, name)
	return nil
}
func (f *fakeMutator) RolloutRestart(context.Context, kube.ResourceKind, string, string) error {
	return nil
}
func (f *fakeMutator) Cordon(context.Context, string, bool) error              { return nil }
func (f *fakeMutator) Drain(context.Context, string) (int, error)              { return 0, nil }
func (f *fakeMutator) HelmRollback(context.Context, string, string, int) error { return nil }
func (f *fakeMutator) RolloutUndo(context.Context, string, string, int) error  { return nil }
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
func (f *fakeMutator) PatchConfigMapData(context.Context, string, string, string, string, string, bool) (string, error) {
	return "", nil
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

// completedJobPod mirrors a completed Job's pod: Succeeded phase, one
// container whose current State.Terminated exited cleanly (ExitCode 0,
// Reason "Completed") — findLastTermination (kube/pods.go) excludes this
// from LastTermination, so the 5a banner must not render for it.
func completedJobPod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			OwnerReferences:   []metav1.OwnerReference{{Kind: "Job", Name: "batch-1"}},
			CreationTimestamp: metav1.Now(),
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "app", Image: "example.com/batch:v1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				Ready:        false,
				RestartCount: 0,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, Reason: "Completed", FinishedAt: metav1.Now(),
				}},
			}},
		},
	}
}

// erroredContainerPod is completedJobPod's negative twin: the container is
// currently Terminated but with a real failure (ExitCode 1, Reason
// "Error"), not a clean completion — used to prove the CONTAINERS grid's
// new green-for-Completed carve-out doesn't also turn a genuine failure
// green.
func erroredContainerPod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: metav1.Now()},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "app", Image: "example.com/batch:v1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				Ready:        false,
				RestartCount: 0,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error", FinishedAt: metav1.Now(),
				}},
			}},
		},
	}
}

// TestTerminatedContainerColorMatchesCleanVsRealExit pins 5a's CONTAINERS
// grid: a clean completion (Reason "Completed", ExitCode 0) renders the same
// neutral blue (theme.Info) the pods list and this screen's own title line
// already use for a Completed pod, while a real failure (Reason "Error")
// keeps the yellow warning color it always had.
func TestTerminatedContainerColorMatchesCleanVsRealExit(t *testing.T) {
	neutral := "38;2;106;168;239" // theme.Info (#6aa8ef)
	warn := "38;2;232;199;74"     // theme.Warn (#e8c74a)

	t.Run("clean completion renders neutral blue", func(t *testing.T) {
		lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindPod: {completedJobPod("batch-1-x7f2k", "default", "node-a")},
		}}
		m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "batch-1-x7f2k"})
		m.SetSize(120, 40)
		m = step(t, m, m.Init()())

		// The State column is narrow enough to truncate "Terminated ·
		// Completed" to "Terminated · Comple…", so match on the stable
		// "Terminated ·" prefix rather than the full reason text.
		line := findLine(t, m.Render(), "Terminated · Comple")
		code := statusTextColorCode(t, line, "Terminated")
		if !strings.Contains(code, neutral) {
			t.Errorf("Terminated · Completed color = %q, want to contain theme.Info %q", code, neutral)
		}
	})

	t.Run("real failure stays yellow", func(t *testing.T) {
		lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
			kube.KindPod: {erroredContainerPod("batch-1-x9k2m", "default", "node-a")},
		}}
		m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "batch-1-x9k2m"})
		m.SetSize(120, 40)
		m = step(t, m, m.Init()())

		line := findLine(t, m.Render(), "Terminated · Error")
		code := statusTextColorCode(t, line, "Terminated")
		if !strings.Contains(code, warn) {
			t.Errorf("Terminated · Error color = %q, want to contain theme.Warn %q", code, warn)
		}
	})
}

// TestCompletedPodTitleStatusRendersNeutralBlue pins statusColor's "neutral"
// class (statusClass's Completed pod) to theme.Info — the same hue the
// browse list's StatusNeutral already renders for a Completed pod — rather
// than silently falling through to theme.TextDim via the unhandled default
// case.
func TestCompletedPodTitleStatusRendersNeutralBlue(t *testing.T) {
	neutral := "38;2;106;168;239" // theme.Info (#6aa8ef)
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {completedJobPod("batch-1-x7f2k", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "batch-1-x7f2k"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	line := findLine(t, m.Render(), "Completed")
	code := statusTextColorCode(t, line, "○")
	if !strings.Contains(code, neutral) {
		t.Errorf("title status color = %q, want to contain theme.Info %q", code, neutral)
	}
}

// findLine returns the first line of view containing substr.
func findLine(t *testing.T, view, substr string) string {
	t.Helper()
	for l := range strings.SplitSeq(view, "\n") {
		if strings.Contains(l, substr) {
			return l
		}
	}
	t.Fatalf("no line containing %q in view:\n%s", substr, plain(view))
	return ""
}

// statusTextColorCode extracts the ANSI color code immediately preceding
// word's own text run in line (an ANSI-styled Render() output), where
// Render wraps each span as "\x1b[<code>m<text>\x1b[0m" with no
// intervening escape between the code and the text it colors.
func statusTextColorCode(t *testing.T, line, word string) string {
	t.Helper()
	re := regexp.MustCompile("\x1b\\[([0-9;]+)m" + regexp.QuoteMeta(word))
	m := re.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("could not find a styled %q run in line:\n%q", word, line)
	}
	return m[1]
}

// TestCompletedJobPodHasNoTerminationBanner pins the fix for a completed
// Job's pod rendering a false "Exit code 0" error banner: a clean
// completion is never "why is it broken," so the banner must be absent and
// only the neutral Completed status line shown.
func TestCompletedJobPodHasNoTerminationBanner(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {completedJobPod("batch-1-x7f2k", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "batch-1-x7f2k"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if m.pod.LastTermination != nil {
		t.Fatalf("expected LastTermination nil for a clean exit 0, got %+v", m.pod.LastTermination)
	}

	view := plain(m.Render())
	if strings.Contains(view, "Last termination") {
		t.Fatalf("expected no termination banner for a clean exit 0 completion:\n%s", view)
	}
	if !strings.Contains(view, "Completed") {
		t.Fatalf("expected the neutral Completed status line:\n%s", view)
	}
}

// TestUndatedTerminationOnRunningPodRendersNoAgeOrBackoff pins the
// ContainerStatusUnknown banner: the record has no finishedAt, which used to
// render "106751d ago", and the container has since restarted and is
// Running, so "Next backoff ~10s" promised a restart that isn't coming.
func TestUndatedTerminationOnRunningPodRendersNoAgeOrBackoff(t *testing.T) {
	pod := runningPod("litellm-0", "default", "node-a")
	pod.Status.ContainerStatuses[0].RestartCount = 1
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown"},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {pod}}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "litellm-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	if !strings.Contains(view, "exit 137 · ContainerStatusUnknown") {
		t.Fatalf("expected the termination facts:\n%s", view)
	}
	for _, bad := range []string{"ContainerStatusUnknown ·", "d ago", "Next backoff"} {
		if strings.Contains(view, bad) {
			t.Fatalf("banner must not render %q for an undated termination on a Running container:\n%s", bad, view)
		}
	}
}

func TestLoadRendersTerminationBannerMetaContainersAndEvents(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {crashLoopPod("worker-0", "default", "node-a")},
	}}
	events := fakeEvents{events: []kube.Event{{Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container"}}}
	m := New(Config{Session: newSession(), Lister: lister, Events: events, Namespace: "default", Name: "worker-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if m.pod.LastTermination == nil {
		t.Fatal("expected LastTermination to be populated from LastTerminationState")
	}

	view := plain(m.Render())
	for _, want := range []string{
		"worker-0", "CrashLoopBackOff", "6 restarts",
		"Last termination", "OOMKilled", "exit 137", "Next backoff ~5m",
		"node-a", "10.0.0.5", "Burstable", "ReplicaSet/worker-abc123",
		"CONTAINERS", "worker", "example.com/worker:v1",
		"EVENTS", "BackOff",
		"RELATED",
		"TOLERATIONS", "dedicated (exists):NoSchedule",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	// LABELS left the resting view when the sidebar was removed — labels and
	// annotations live in 26a's 'm' metapanel now.
	if strings.Contains(view, "LABELS") {
		t.Fatalf("view still shows LABELS, which moved to the metapanel:\n%s", view)
	}
}

// TestConnStateDrivesHeaderBadge pins that the header badge reflects the
// real connection state (mock 5a) instead of the hardcoded "watching · live"
// it used to show: connected renders green-with-latency, an outage flips it
// to the red disconnected badge.
func TestConnStateDrivesHeaderBadge(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected, Latency: 12 * time.Millisecond})
	if view := plain(m.Render()); !strings.Contains(view, "connected · 12ms") {
		t.Fatalf("expected connected badge with latency:\n%s", view)
	}

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	if view := plain(m.Render()); !strings.Contains(view, "disconnected") {
		t.Fatalf("expected disconnected badge mid-outage:\n%s", view)
	}
}

// TestKeybarGoesOfflineAndHidesDelete pins the cross-cutting 4a fix
// (docs/design README.md §52, §301): poddetail must show the OFFLINE pill
// and drop its own mutating verbs — delete, and exec, whose tty handoff is
// mutating too — from the keybar while disconnected, not just browse.
func TestKeybarGoesOfflineAndHidesDelete(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	kb := m.Keybar()
	if kb.PillText != "POD" {
		t.Fatalf("PillText = %q before any outage, want POD", kb.PillText)
	}

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	kb = m.Keybar()
	if kb.Pill != tui.ModeOffline || kb.PillText != "OFFLINE" {
		t.Fatalf("Pill/PillText = %v/%q while offline, want ModeOffline/OFFLINE", kb.Pill, kb.PillText)
	}
	for _, g := range kb.Groups {
		for _, h := range g {
			if h.Key == verbs.Delete.Key || h.Key == verbs.Exec.Key {
				t.Fatalf("expected delete/exec hints hidden while offline, got groups %+v", kb.Groups)
			}
		}
	}
	// The key itself is refused, not just un-advertised.
	if _, cmd := m.Update(tea.KeyPressMsg{Text: verbs.Exec.Key}); cmd != nil {
		t.Error("'x' handed the tty to kubectl while offline")
	}

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected})
	kb = m.Keybar()
	if kb.PillText != "POD" {
		t.Fatalf("PillText = %q after reconnect, want POD", kb.PillText)
	}
}

// failingEvents makes the best-effort events fetch fail — the EVENTS grid
// must say "events unavailable", never a misleading "no events".
type failingEvents struct{}

func (failingEvents) ObjectEvents(context.Context, string, kube.ResourceKind, string) ([]kube.Event, error) {
	return nil, errors.New("client rate limiter: context deadline exceeded")
}

func TestEventsFetchFailureRendersUnavailableNotEmpty(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Events: failingEvents{}, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready — an events failure must not fail the load", m.state)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "events unavailable") {
		t.Fatalf("expected 'events unavailable':\n%s", view)
	}
	if strings.Contains(view, "no events") {
		t.Fatalf("failed fetch must not render as 'no events':\n%s", view)
	}
}

// TestTerminationAgeIsHumanized pins the banner's "· 19d ago" shape — the
// screenshot bug rendered LastTermination.Age's raw Go duration
// ("456h29m47s ago").
func TestTerminationAgeIsHumanized(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{4 * time.Minute, "4m"},
		{3 * time.Hour, "3h"},
		{456*time.Hour + 29*time.Minute + 47*time.Second, "19d"},
	}
	for _, tc := range cases {
		if got := shortDur(tc.d); got != tc.want {
			t.Fatalf("shortDur(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestNoEventsRendersEmptyNotBlank(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if !strings.Contains(plain(m.Render()), "no events") {
		t.Fatalf("expected empty-events placeholder:\n%s", plain(m.Render()))
	}
}

func TestGonePodShowsBannerAndAnyKeyGoesBack(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "ghost"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if !m.gone {
		t.Fatal("expected gone=true for a pod missing from the cache")
	}
	if !m.CapturingInput() {
		t.Fatal("expected CapturingInput true while gone (every key becomes back)")
	}
	if !strings.Contains(plain(m.Render()), "Pod deleted") {
		t.Fatalf("expected gone banner:\n%s", plain(m.Render()))
	}

	_, cmd := m.Update(tea.KeyPressMsg{Text: "x"})
	if cmd == nil {
		t.Fatal("expected any key to return a command while gone")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg, got %T", cmd())
	}
}

// TestGonePodStillQuitsOnQAndCtrlC pins the fix to the "any key" gone banner
// swallowing the app's one global quit expectation: q/ctrl+c must still
// quit even while the banner is showing, not silently navigate back instead.
func TestGonePodStillQuitsOnQAndCtrlC(t *testing.T) {
	for _, key := range []string{"ctrl+q", "ctrl+c"} {
		lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{}}
		m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "ghost"})
		m.SetSize(120, 40)
		m = step(t, m, m.Init()())
		if !m.gone {
			t.Fatalf("%s: expected gone=true for a pod missing from the cache", key)
		}

		_, cmd := m.Update(tea.KeyPressMsg{Text: key})
		if cmd == nil {
			t.Fatalf("%s: expected a command", key)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s: expected tea.QuitMsg, got %T", key, cmd())
		}
	}
}

func TestEscSendsBackMsg(t *testing.T) {
	m := New(Config{Session: newSession(), Namespace: "default", Name: "api-0"})
	_, cmd := m.Update(tea.KeyPressMsg{Text: "esc"})
	if cmd == nil {
		t.Fatal("expected esc to return a command")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg, got %T", cmd())
	}
}

func TestDeleteConfirmExecuteAndCancel(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() {
		t.Fatal("expected ctrl+d to open a delete confirmation")
	}

	// n cancels without deleting.
	cancelled := step(t, m, tea.KeyPressMsg{Text: "n"})
	if cancelled.actions.Active() {
		t.Fatal("expected n to close the confirmation")
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("expected no delete on cancel, got %v", mut.deleted)
	}

	confirmed := step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.deleted) != 1 || mut.deleted[0] != "api-0" {
		t.Fatalf("expected api-0 deleted, got %v", mut.deleted)
	}
	_ = confirmed
}

// TestNonProdForceDeleteReachesTypeNameModal is poddetail's H2
// regression: outside PROD the force chord must open the type-the-name
// modal (force delete is always modal), never stage an inline "y".
func TestNonProdForceDeleteReachesTypeNameModal(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, Mutator: mut, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	m = step(t, m, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	if m.actions.Tier() != actions.TierModal || m.actions.Pending().Scope.Verb != "force-delete" {
		t.Fatalf("expected the type-the-name force-delete modal, tier=%v", m.actions.Tier())
	}
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 0 || len(mut.forceDeleted) != 0 {
		t.Fatalf("expected nothing to run before the name is typed, deleted=%v forceDeleted=%v", mut.deleted, mut.forceDeleted)
	}
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	for _, r := range "api-0" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.forceDeleted) != 1 || mut.forceDeleted[0] != "api-0" || len(mut.deleted) != 0 {
		t.Fatalf("deleted=%v forceDeleted=%v, want one force delete of api-0", mut.deleted, mut.forceDeleted)
	}
}

// TestDeleteInProdRequiresTypedName exercises 8b's PROD escalation
// end-to-end from poddetail: ctrl-d opens the type-the-name modal (not the
// inline y/N prompt), enter no-ops until the pod's name is typed in full,
// and ctrl-k escalates to force-delete.
func TestDeleteInProdRequiresTypedName(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	mut := &fakeMutator{}
	sess := newSession()
	sess.Config = config.Config{ProdContexts: []string{"test-cluster"}}
	m := New(Config{Session: sess, Lister: lister, Mutator: mut, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if !m.actions.Active() || m.actions.Tier() != actions.TierModal {
		t.Fatalf("expected ctrl+d in a prod context to open the type-the-name modal, tier=%v", m.actions.Tier())
	}

	// A bare "y" must NOT confirm in the modal (unlike the inline tier) —
	// it types the letter "y" into the buffer instead.
	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.deleted) != 0 {
		t.Fatalf("expected 'y' to type, not confirm, in the modal: %v", mut.deleted)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.deleted) != 0 {
		t.Fatalf("expected enter to no-op before the name matches: %v", mut.deleted)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "esc"})
	if m.actions.Active() {
		t.Fatal("expected esc to cancel the modal")
	}

	// Re-open and this time escalate to force-delete via the chord the
	// modal itself advertises.
	m = step(t, m, tea.KeyPressMsg{Text: "D"})
	if view := plain(m.Render()); !strings.Contains(view, verbs.ForceDeleteModalHint()) {
		t.Fatalf("expected the modal to advertise %q:\n%s", verbs.ForceDeleteModalHint(), view)
	}
	m = step(t, m, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	for _, r := range "api-0" {
		m = step(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	m = step(t, m, tea.KeyPressMsg{Text: "enter"})
	if len(mut.forceDeleted) != 1 || mut.forceDeleted[0] != "api-0" {
		t.Fatalf("expected api-0 force-deleted, got %v", mut.forceDeleted)
	}
	if len(mut.deleted) != 0 {
		t.Fatalf("expected the plain delete path untouched, got %v", mut.deleted)
	}
}

func TestSiblingNavigationMovesAndClamps(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			runningPod("a", "default", "node-a"),
			runningPod("b", "default", "node-a"),
		},
	}}
	m := New(Config{
		Session: newSession(), Lister: lister, Namespace: "default", Name: "a",
		Siblings: []SiblingRef{{Namespace: "default", Name: "a"}, {Namespace: "default", Name: "b"}}, SiblingIndex: 0,
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	// [ at the start is a no-op.
	before := m
	m = step(t, m, tea.KeyPressMsg{Text: "["})
	if m.name != before.name || m.siblingIndex != before.siblingIndex {
		t.Fatalf("expected [ at the start to no-op, got name=%q index=%d", m.name, m.siblingIndex)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "]"})
	if m.name != "b" || m.siblingIndex != 1 {
		t.Fatalf("expected ] to move to sibling 'b', got name=%q index=%d", m.name, m.siblingIndex)
	}
	if m.state != tui.TaskStateReady || m.pod.Name != "b" {
		t.Fatalf("expected reload for sibling 'b', got state=%s pod=%q", m.state, m.pod.Name)
	}

	// ] at the end is a no-op.
	afterLast := m
	m = step(t, m, tea.KeyPressMsg{Text: "]"})
	if m.name != afterLast.name || m.siblingIndex != afterLast.siblingIndex {
		t.Fatalf("expected ] at the end to no-op, got name=%q index=%d", m.name, m.siblingIndex)
	}
}

// TestSiblingNavigationCrossesNamespaces covers 6b's all-namespaces list:
// stepping onto a same-named sibling in another namespace must load that
// namespace's pod, not look the name up in the one detail opened on.
func TestSiblingNavigationCrossesNamespaces(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			runningPod("postgres-0", "aaa", "node-a"),
			runningPod("postgres-0", "bbb", "node-b"),
		},
	}}
	m := New(Config{
		Session: newSession(), Lister: lister, Namespace: "aaa", Name: "postgres-0",
		Siblings: []SiblingRef{{Namespace: "aaa", Name: "postgres-0"}, {Namespace: "bbb", Name: "postgres-0"}}, SiblingIndex: 0,
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "]"})
	if m.namespace != "bbb" || m.state != tui.TaskStateReady || m.pod.Namespace != "bbb" {
		t.Fatalf("expected ] to load bbb/postgres-0, got namespace=%q state=%s pod=%s/%s", m.namespace, m.state, m.pod.Namespace, m.pod.Name)
	}
}

// TestSiblingMoveDropsStaleLoad covers a load issued before ] (a watch
// reload, an earlier move) landing after it: it must not paint the previous
// pod under the new pod's name, where l would stream one pod while x/D/y
// target the other.
func TestSiblingMoveDropsStaleLoad(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {
			runningPod("alpha", "default", "node-a"),
			runningPod("bravo", "default", "node-b"),
		},
	}}
	m := New(Config{
		Session: newSession(), Lister: lister, Namespace: "default", Name: "alpha",
		Siblings: []SiblingRef{{Namespace: "default", Name: "alpha"}, {Namespace: "default", Name: "bravo"}}, SiblingIndex: 0,
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	stale := m.load()
	m = step(t, m, tea.KeyPressMsg{Text: "]"})
	if m.pod.Name != "bravo" {
		t.Fatalf("expected ] to load bravo, got %q", m.pod.Name)
	}
	m = step(t, m, stale())
	if m.name != "bravo" || m.pod.Name != "bravo" || m.pod.Node != "node-b" {
		t.Fatalf("stale alpha load applied over bravo: name=%q pod=%q node=%q", m.name, m.pod.Name, m.pod.Node)
	}
}

func TestOpenLogsHandoff(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	var openedFor string
	openLogs := func(pod kube.Pod, _ string, _, _ int) (tea.Model, tea.Cmd) {
		openedFor = pod.Name
		return sentinelTask{}, nil
	}
	m := New(Config{Session: newSession(), Lister: lister, OpenLogs: openLogs, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "l"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected 'l' to hand off to the logs task, got %T", updated)
	}
	if openedFor != "api-0" {
		t.Fatalf("openLogs called for %q, want api-0", openedFor)
	}
}

// TestOpenLogsUsesSelectedContainer covers the CONTAINERS grid → 'l' handoff
// (docs/design README.md §5a): moving the selection with ↓/j before pressing
// 'l' must open logs on that container, not always index 0.
func TestOpenLogsUsesSelectedContainer(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {multiContainerPod("api-0", "default", "node-a")},
	}}
	var openedContainer string
	openLogs := func(_ kube.Pod, container string, _, _ int) (tea.Model, tea.Cmd) {
		openedContainer = container
		return sentinelTask{}, nil
	}
	m := New(Config{Session: newSession(), Lister: lister, OpenLogs: openLogs, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: "down"})
	updated, _ := m.Update(tea.KeyPressMsg{Text: "l"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected 'l' to hand off to the logs task, got %T", updated)
	}
	if openedContainer != "sidecar" {
		t.Fatalf("openLogs called with container %q, want sidecar", openedContainer)
	}
}

func TestInitContainerRendersAndCanOpenLogs(t *testing.T) {
	pod := runningPod("api-0", "default", "node-a")
	pod.Spec.InitContainers = []corev1.Container{{Name: "prepare-ssh", Image: "registry/software-escrow:1.0.0"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "prepare-ssh", RestartCount: 5,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {pod}}}
	var openedContainer string
	m := New(Config{
		Session: newSession(), Lister: lister,
		OpenLogs: func(_ kube.Pod, container string, _, _ int) (tea.Model, tea.Cmd) {
			openedContainer = container
			return sentinelTask{}, nil
		},
		Namespace: "default", Name: "api-0",
	})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	for _, want := range []string{"INIT CONTAINERS", "prepare-ssh", "Waiting"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if got := m.pod.InitContainerInfos[0].Reason; got != "ImagePullBackOff" {
		t.Fatalf("init-container reason = %q, want ImagePullBackOff", got)
	}
	m.moveContainerSelection(1)
	updated, _ := m.Update(tea.KeyPressMsg{Text: "l"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected init-container logs task, got %T", updated)
	}
	if openedContainer != "prepare-ssh" {
		t.Fatalf("OpenLogs called with %q, want prepare-ssh", openedContainer)
	}
}

func TestLongImagePullEventWrapsAndCanBePagedIntoView(t *testing.T) {
	pod := runningPod("software-escrow-manual-1148-gwn9l", "software-escrow", "dharma")
	pod.Spec.InitContainers = []corev1.Container{{Name: "prepare-ssh", Image: "r.vayner.systems:30080/aim/software-escrow:1.0.0"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "prepare-ssh", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}
	detailed := `Failed to pull image "r.vayner.systems:30080/aim/software-escrow:1.0.0": rpc error: code = NotFound desc = failed to pull and unpack image: no match for platform in manifest: not found`
	events := fakeEvents{events: []kube.Event{
		{Type: "Warning", Reason: "Failed", Message: "Error: ImagePullBackOff"},
		{Type: "Warning", Reason: "Failed", Message: "Error: ErrImagePull"},
		{Type: "Warning", Reason: "Failed", Message: detailed},
	}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {pod}}}
	m := New(Config{Session: newSession(), Lister: lister, Events: events, Namespace: "software-escrow", Name: pod.Name})
	m.SetSize(80, 24)
	m = step(t, m, m.Init()())

	eventsView := plain(m.eventsBlock(m.Theme(), 56))
	for _, want := range []string{"image: no match", "for platform in", "manifest: not", "found"} {
		if !strings.Contains(eventsView, want) {
			t.Fatalf("wrapped event lost %q:\n%s", want, eventsView)
		}
	}
	if !m.bodyOverflows() {
		t.Fatal("expected the init-container detail and wrapped events to overflow a 80x24 body")
	}
	if !hasKeyHint(m.Keybar().RightHints, verbs.PageDown.Key) {
		t.Fatalf("overflowing detail keybar does not advertise %s", verbs.PageDown.Key)
	}
	m = step(t, m, tea.KeyPressMsg{Text: verbs.PageDown.Key})
	if m.bodyOffset == 0 {
		t.Fatal("pgdown did not move the pod-detail viewport")
	}
	m = step(t, m, tea.KeyPressMsg{Text: verbs.PageDown.Key})
	if !hasKeyHint(m.Keybar().RightHints, verbs.PageUp.Key) {
		t.Fatalf("paged detail keybar does not advertise %s", verbs.PageUp.Key)
	}
	view := plain(m.Render())
	if !strings.Contains(view, "manifest: not") {
		t.Fatalf("paged view does not expose the detailed image-pull diagnosis:\n%s", view)
	}
	m = step(t, m, tea.KeyPressMsg{Text: verbs.PageUp.Key})
	m = step(t, m, tea.KeyPressMsg{Text: verbs.PageUp.Key})
	if m.bodyOffset != 0 {
		t.Fatalf("pgup left bodyOffset=%d, want 0", m.bodyOffset)
	}
}

func hasKeyHint(hints []tui.KeyHint, key string) bool {
	for _, hint := range hints {
		if hint.Key == key {
			return true
		}
	}
	return false
}

// TestOpenRelatedJumpsToOwner covers the RELATED section's numbered jump
// (docs/design README.md §5a): pressing '1' on a pod whose owner resolves to
// a Deployment must fire tui.GotoResource for that Deployment — the digit
// keys' replacement for the old 'o' shortcut. tui.GotoResource is the same
// navigation the root shell's 'g' palette fires (model.go's routeGoto pushes
// a fresh browse view and keeps poddetail one esc-back away, rather than
// this screen needing to pop itself via a BackMsg first).
func TestOpenRelatedJumpsToOwner(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {crashLoopPod("worker-0", "default", "node-a")},
		kube.KindReplicaSet: {&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-abc123", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "worker"}},
			},
		}},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "worker-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if len(m.related) != 1 {
		t.Fatalf("related = %+v, want exactly one entry (the owning Deployment)", m.related)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Text: "1"})
	if cmd == nil {
		t.Fatalf("expected '1' to return a command")
	}
	gotoMsg, ok := cmd().(tui.GotoResourceMsg)
	if !ok {
		t.Fatalf("expected a GotoResourceMsg, got %T", cmd())
	}
	if gotoMsg.Kind != kube.KindDeployment || gotoMsg.Namespace != "default" || gotoMsg.Name != "worker" {
		t.Fatalf("GotoResourceMsg = %+v, want Deployment/default/worker", gotoMsg)
	}
}

// jobOwnedPod builds a bare-bones pod owned directly by a Job — no
// ReplicaSet in between, so resolveControllerDisplay's hop is a no-op and
// pod.Owner passes straight through as "Job/<name>-job".
func jobOwnedPod(name, ns string, labels map[string]string, volumes []corev1.Volume) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: name + "-job"}},
		},
		Spec:   corev1.PodSpec{Volumes: volumes},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestRelatedIncludesJobOwner(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {jobOwnedPod("worker-abc", "default", nil, nil)},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "worker-abc"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if len(m.related) != 1 || m.related[0].Kind != kube.KindJob || m.related[0].Name != "worker-abc-job" {
		t.Fatalf("related = %+v, want exactly one Job entry (Job/worker-abc-job)", m.related)
	}
}

func TestRelatedIncludesDaemonSetOwner(t *testing.T) {
	pod := jobOwnedPod("agent-abc", "default", nil, nil)
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "agent"}}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "agent-abc"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if len(m.related) != 1 || m.related[0].Kind != kube.KindDaemonSet || m.related[0].Name != "agent" {
		t.Fatalf("related = %+v, want exactly one DaemonSet entry (DaemonSet/agent)", m.related)
	}
}

func TestRelatedIncludesExistingPVCsOnly(t *testing.T) {
	volumes := []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-pvc"}}},
		{Name: "gone", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "deleted-pvc"}}},
		{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	pod := jobOwnedPod("worker-abc", "default", nil, volumes)
	pod.OwnerReferences = nil
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod},
		kube.KindPersistentVolumeClaim: {&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-pvc", Namespace: "default"},
		}},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "worker-abc"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if len(m.related) != 1 || m.related[0].Kind != kube.KindPersistentVolumeClaim || m.related[0].Name != "data-pvc" {
		t.Fatalf("related = %+v, want exactly one PVC entry (PersistentVolumeClaim/data-pvc) — deleted-pvc and the non-PVC volume must be skipped", m.related)
	}
}

func TestRelatedIncludesMatchingServicesSorted(t *testing.T) {
	pod := jobOwnedPod("worker-abc", "default", map[string]string{"app": "worker"}, nil)
	pod.OwnerReferences = nil
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod},
		kube.KindService: {
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-b", Namespace: "default"},
				Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "worker"}},
			},
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Namespace: "default"},
				Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "worker"}},
			},
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "worker-abc"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if len(m.related) != 2 || m.related[0].Name != "worker-a" || m.related[1].Name != "worker-b" {
		t.Fatalf("related = %+v, want [Service/worker-a Service/worker-b] sorted by name", m.related)
	}
}

func TestRelatedListsEveryMatchingIngress(t *testing.T) {
	pod := jobOwnedPod("worker-abc", "default", map[string]string{"app": "worker"}, nil)
	pod.OwnerReferences = nil
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod},
		kube.KindService: {&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "worker"}},
		}},
		kube.KindIngress: {
			&networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{Name: "ingress-b", Namespace: "default"},
				Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{
					Service: &networkingv1.IngressServiceBackend{Name: "worker"},
				}},
			},
			&networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{Name: "ingress-a", Namespace: "default"},
				Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{
					Service: &networkingv1.IngressServiceBackend{Name: "worker"},
				}},
			},
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "worker-abc"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	// One Service entry plus both Ingresses, sorted — previously an
	// ambiguous (>1) Ingress match was dropped entirely.
	if len(m.related) != 3 {
		t.Fatalf("related = %+v, want 3 entries (1 Service + 2 Ingresses)", m.related)
	}
	if m.related[1].Kind != kube.KindIngress || m.related[1].Name != "ingress-a" {
		t.Fatalf("related[1] = %+v, want Ingress/ingress-a", m.related[1])
	}
	if m.related[2].Kind != kube.KindIngress || m.related[2].Name != "ingress-b" {
		t.Fatalf("related[2] = %+v, want Ingress/ingress-b", m.related[2])
	}
}

func TestRelatedCapsAtMaxItems(t *testing.T) {
	pod := jobOwnedPod("worker-abc", "default", map[string]string{"app": "worker"}, nil)
	var services []runtime.Object
	for i := 0; i < 15; i++ {
		services = append(services, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("svc-%02d", i), Namespace: "default"},
			Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "worker"}},
		})
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod:     {pod},
		kube.KindService: services,
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "worker-abc"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	if len(m.related) != maxRelatedItems {
		t.Fatalf("related has %d entries, want the cap of %d", len(m.related), maxRelatedItems)
	}
	// The digit-key mapping (update.go's openRelated) must stay in range for
	// every entry the cap actually kept.
	if _, _, ok := m.openRelated(maxRelatedItems - 1); !ok {
		t.Fatalf("openRelated(%d) should still resolve at the cap", maxRelatedItems-1)
	}
	if _, _, ok := m.openRelated(maxRelatedItems); ok {
		t.Fatalf("openRelated(%d) should be out of range past the cap", maxRelatedItems)
	}
}

func TestOpenEventsHandoff(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	var openedKind kube.ResourceKind
	var openedNS, openedName string
	openEvents := func(kind kube.ResourceKind, ns, name string, _, _ int) (tea.Model, tea.Cmd) {
		openedKind, openedNS, openedName = kind, ns, name
		return sentinelTask{}, nil
	}
	m := New(Config{Session: newSession(), Lister: lister, OpenEvents: openEvents, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "e"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected 'e' to hand off to the events task, got %T", updated)
	}
	if openedKind != kube.KindPod || openedNS != "default" || openedName != "api-0" {
		t.Fatalf("openEvents called with (%s, %s, %s), want (Pod, default, api-0)", openedKind, openedNS, openedName)
	}
}

// TestOpenForwardHandoff pins the cross-cutting missing-verb fix (docs/design
// README.md §304, §308: "on any object row") — 'f' must push the forward
// picker for the loaded pod, the same as browse's own Pod rows already do.
func TestOpenForwardHandoff(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {runningPod("api-0", "default", "node-a")},
	}}
	var openedTarget kube.ForwardTarget
	openForward := func(target kube.ForwardTarget, _, _ int) (tea.Model, tea.Cmd) {
		openedTarget = target
		return sentinelTask{}, nil
	}
	m := New(Config{Session: newSession(), Lister: lister, OpenForward: openForward, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "f"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected 'f' to hand off to the forward picker, got %T", updated)
	}
	want := kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "api-0"}
	if openedTarget != want {
		t.Fatalf("openForward called with %+v, want %+v", openedTarget, want)
	}
}

// TestInitCrashLoopMatchesTheListAndGetsItsBanner: a pod whose init
// container is crash-looping (a failing migration) reads Init:CrashLoopBackOff
// in 5a's title — the Pods list's own wording — not the main containers'
// PodInitializing, and its banner names the init container's failure.
func TestInitCrashLoopMatchesTheListAndGetsItsBanner(t *testing.T) {
	pod := runningPod("api-0", "default", "node-a")
	pod.Status.Phase = corev1.PodPending
	pod.Spec.InitContainers = []corev1.Container{{Name: "migrate", Image: "example.com/migrate:v1"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "migrate", RestartCount: 4,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 3, Reason: "Error", FinishedAt: metav1.NewTime(time.Now().Add(-time.Minute)),
		}},
	}}
	for i := range pod.Status.ContainerStatuses {
		pod.Status.ContainerStatuses[i].Ready = false
		pod.Status.ContainerStatuses[i].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {pod}}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	if !strings.Contains(view, "Init:CrashLoopBackOff") || strings.Contains(view, "PodInitializing") {
		t.Fatalf("title should read the list's Init:CrashLoopBackOff:\n%s", view)
	}
	if !strings.Contains(view, "Last termination") || !strings.Contains(view, "Container migrate exited with code 3") {
		t.Fatalf("expected the init container's termination banner:\n%s", view)
	}
}

type sentinelTask struct{}

func (sentinelTask) Init() tea.Cmd                       { return nil }
func (sentinelTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
func (sentinelTask) View() tea.View                      { return tea.View{} }

type countingMetrics struct {
	calls int
	cpu   string
}

func (c *countingMetrics) PodMetricsByNamespace(_ context.Context, ns string) (map[string]kube.PodMetrics, error) {
	c.calls++
	return map[string]kube.PodMetrics{kube.PodKey(ns, "api-0"): {CPU: c.cpu, CPUMilli: 1}}, nil
}

// TestUsagePollsOnTheSyncIntervalNotOnWatchEvents: a watch-driven reload
// reads only the caches — never metrics-server, which a busy namespace would
// otherwise hit once per event — keeps the last usage on the bars, and the
// bars refresh from the poll chain's own tick even when nothing changes.
func TestUsagePollsOnTheSyncIntervalNotOnWatchEvents(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindPod: {runningPod("api-0", "default", "node-a")}}}
	metrics := &countingMetrics{cpu: "4m"}
	m := New(Config{Session: newSession(), Lister: lister, Metrics: metrics, Namespace: "default", Name: "api-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.load()())
	updated, tick := m.Update(m.loadMetrics()())
	m = *updated.(*Model)
	if metrics.calls != 1 || m.pod.CPU != "4m" || tick == nil {
		t.Fatalf("after open: calls=%d cpu=%q tick armed=%v", metrics.calls, m.pod.CPU, tick != nil)
	}

	for range 3 {
		updated, cmd := m.Update(kube.ResourceChangedMsg{Kind: kube.KindPod})
		m = *updated.(*Model)
		m = step(t, m, cmd())
	}
	if metrics.calls != 1 {
		t.Fatalf("watch reloads polled metrics-server: %d calls, want 1", metrics.calls)
	}
	if m.pod.CPU != "4m" {
		t.Fatalf("reload dropped the last usage reading: cpu=%q", m.pod.CPU)
	}

	metrics.cpu = "9m"
	updated, cmd := m.Update(metricsTickMsg{epoch: m.metricsEpoch})
	m = *updated.(*Model)
	updated, _ = m.Update(cmd())
	m = *updated.(*Model)
	if metrics.calls != 2 || m.pod.CPU != "9m" {
		t.Fatalf("tick poll: calls=%d cpu=%q, want 2 and 9m", metrics.calls, m.pod.CPU)
	}
	if _, cmd := m.Update(metricsTickMsg{epoch: m.metricsEpoch - 1}); cmd != nil {
		t.Fatal("a retired chain's tick must not poll")
	}
}

// TestEventAgeRendersFromStampedNow pins L4: render is pure — EVENTS ages
// come from the model's own now (stamped on load), not the wall clock.
func TestEventAgeRendersFromStampedNow(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {crashLoopPod("worker-0", "default", "node-a")},
	}}
	m := New(Config{Session: newSession(), Lister: lister, Namespace: "default", Name: "worker-0"})
	m.SetSize(120, 40)
	m = step(t, m, m.Init()())
	m.now = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	m.eventRows = []kube.Event{{Type: "Warning", Reason: "BackOff", Message: "x", LastSeen: m.now.Add(-7 * time.Minute)}}
	if out := plain(m.Render()); !strings.Contains(out, "7m") {
		t.Fatalf("event age should render from m.now (7m):\n%s", out)
	}
}
