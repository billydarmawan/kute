package nodedetail

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// ansiFGCode renders a single character through style and extracts the
// color code lipgloss wraps it in, so color assertions compare against the
// theme's own resolved value rather than a hand-copied hex guess.
func ansiFGCode(t *testing.T, style lipgloss.Style) string {
	t.Helper()
	rendered := style.Render("X")
	re := regexp.MustCompile(`\x1b\[([0-9;]+)mX`)
	m := re.FindStringSubmatch(rendered)
	if m == nil {
		t.Fatalf("could not extract an ANSI color code from %q", rendered)
	}
	return m[1]
}

type fakeLister struct {
	objs map[kube.ResourceKind][]runtime.Object
}

func (f fakeLister) ListRaw(_ context.Context, kind kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objs[kind], nil
}

func plain(s string) string { return ansi.Strip(s) }

func testNode(name string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue, Message: "system is low on memory"},
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
				corev1.ResourcePods:   resource.MustParse("30"),
			},
		},
	}
}

func schedPod(ns, name, node string, memRequest string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "c",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memRequest)},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Ready: true}}},
	}
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

func TestLoadRendersConditionsAllocationAndPods(t *testing.T) {
	// "small" is crashlooping — pins that the sort is unhealthy-first (same
	// as 2a's own Pods list), not alphabetical: "big" would otherwise sort
	// first on name alone.
	small := schedPod("default", "small", "node-a", "512Mi")
	small.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Ready: false,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a", corev1.Taint{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule})},
		kube.KindPod: {
			schedPod("default", "big", "node-a", "2Gi"),
			small,
			schedPod("default", "elsewhere", "other-node", "1Gi"),
		},
	}}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want ready (feedback %q)", m.state, m.feedback)
	}
	if len(m.pods) != 2 {
		t.Fatalf("pods = %d, want 2 (elsewhere's pod must be excluded)", len(m.pods))
	}
	if m.pods[0].pod.Name != "small" {
		t.Fatalf("expected unhealthy-first sort to put crashlooping 'small' first, got %q", m.pods[0].pod.Name)
	}
	if m.allocated.memBytes == 0 {
		t.Fatal("expected allocated memBytes to sum both scheduled pods' requests")
	}

	view := plain(m.Render())
	for _, want := range []string{"node-a", "CONDITIONS", "MemoryPressure", "REQUESTED / ALLOCATABLE", "USED / CAPACITY", "TAINTS", "dedicated=gpu", "big", "small"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

// TestNotReadyConditionRendersRedNotYellow pins 11b: a NotReady condition
// must render the same Bad/red color 11a's own nodes list uses for the
// identical NotReady signal, not Warn/yellow.
func TestNotReadyConditionRendersRedNotYellow(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}},
		},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindNode: {node}}}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := m.Render()
	// [^\x1b]* skips the leading glyph + space conditionLines() now prefixes
	// onto "Ready false" (same styled span, so the captured color code still
	// applies) without hardcoding which glyph character it is.
	re := regexp.MustCompile(`\x1b\[([0-9;]+)m[^\x1b]*Ready false`)
	match := re.FindStringSubmatch(view)
	if match == nil {
		t.Fatalf("could not find the styled 'Ready false' condition line:\n%s", plain(view))
	}
	theme := tui.Dark()
	badCode := ansiFGCode(t, lipgloss.NewStyle().Foreground(theme.Bad))
	warnCode := ansiFGCode(t, lipgloss.NewStyle().Foreground(theme.Warn))
	if !strings.Contains(match[1], badCode) {
		t.Errorf("NotReady condition color = %q, want to contain Bad %q", match[1], badCode)
	}
	if strings.Contains(match[1], warnCode) {
		t.Errorf("NotReady condition color = %q, should not be Warn %q", match[1], warnCode)
	}
}

// TestActiveConditionAppendsAgeWithoutDanglingSeparator pins 11b (docs/design
// README.md:163: "active pressure yellow with kubelet message + age"): an
// active condition's line must append its age, and a condition with no
// kubelet message must not leave a dangling "— " separator with nothing
// after it.
func TestActiveConditionAppendsAgeWithoutDanglingSeparator(t *testing.T) {
	transitioned := time.Now().Add(-5 * time.Minute)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{
					Type:               corev1.NodeMemoryPressure,
					Status:             corev1.ConditionTrue,
					Message:            "system is low on memory",
					LastTransitionTime: metav1.NewTime(transitioned),
				},
				{
					Type:               corev1.NodeDiskPressure,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(transitioned),
				},
			},
		},
	}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{kube.KindNode: {node}}}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	view := plain(m.Render())
	// Kute Spec.dc.html §11b never shows a "true" word on an active
	// condition's line — glyph + color already say so.
	if !strings.Contains(view, "MemoryPressure — system is low on memory · 5m") {
		t.Fatalf("expected the message clause followed by age, got:\n%s", view)
	}
	if strings.Contains(view, "DiskPressure — ") {
		t.Fatalf("expected no dangling '— ' separator for an empty message, got:\n%s", view)
	}
	if !strings.Contains(view, "DiskPressure · 5m") {
		t.Fatalf("expected the age still appended when the message is empty, got:\n%s", view)
	}
}

// TestAllocationTextTurnsYellowWhenHot pins 11b: the "used / total" text
// next to a bar must also turn yellow at the same ≥70% "hot" threshold the
// bar's own fill segment already uses — previously only the bar changed
// color.
func TestAllocationTextTurnsYellowWhenHot(t *testing.T) {
	theme := tui.Dark()
	// 8m / 10m = 80% ⇒ hot.
	hot := allocationBarLine("cpu", 8, 10, theme, func(v int64) string { return fmt.Sprintf("%dm", v) })
	// 1m / 10m = 10% ⇒ not hot.
	cool := allocationBarLine("cpu", 1, 10, theme, func(v int64) string { return fmt.Sprintf("%dm", v) })

	warnCode := ansiFGCode(t, lipgloss.NewStyle().Foreground(theme.Warn))
	dimCode := ansiFGCode(t, lipgloss.NewStyle().Foreground(theme.TextDim))

	hotTextCode := textColorBefore(t, hot, "8m / 10m")
	coolTextCode := textColorBefore(t, cool, "1m / 10m")
	if !strings.Contains(hotTextCode, warnCode) {
		t.Errorf("hot usage text color = %q, want to contain Warn %q", hotTextCode, warnCode)
	}
	if !strings.Contains(coolTextCode, dimCode) {
		t.Errorf("cool usage text color = %q, want to contain TextDim %q", coolTextCode, dimCode)
	}
}

// textColorBefore extracts the ANSI color code directly preceding word in
// line, same technique as ansiFGCode but reading an already-rendered line
// instead of rendering fresh.
func textColorBefore(t *testing.T, line, word string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta("\x1b[") + `([0-9;]+)m` + regexp.QuoteMeta(word))
	m := re.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("could not find a styled %q run in line:\n%q", word, line)
	}
	return m[1]
}

// TestLoadingStateRender pins 11b's applied-15a shape: the shell
// (breadcrumb, facts-panel titles, pods columns, esc-back key) paints in the
// first frame, the header/strip name what's loading, and skeleton content
// fills both panes — never a bare spinner-only blank screen (docs/design
// README.md §15a, applied to a detail screen the same way browse's own
// loading state is).
func TestLoadingStateRender(t *testing.T) {
	m := New(Config{Session: newSession(), Lister: fakeLister{}, NodeName: "node-a"})
	m.SetSize(120, 30)
	if m.state != tui.TaskStateLoading {
		t.Fatalf("state = %s, want loading", m.state)
	}
	view := plain(m.Render())

	for _, want := range []string{
		"Nodes", "node-a", // shell breadcrumb
		"loading node-a",                                                  // header timer
		"fetching node-a…", "conditions, allocation & pods load together", // strip
		"CONDITIONS", "REQUESTED / ALLOCATABLE", "USED / CAPACITY", "TAINTS", // real facts-panel titles
		"NAME", "NAMESPACE", "MEM", "CPU", "AGE", // real pods-table columns
		"facts & pods enable when data lands", // disabled-verbs note
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("loading view missing %q:\n%s", want, view)
		}
	}
	// Node/row-scoped verbs (cordon, drain, yaml, exec…) must not appear
	// while the node/pods don't exist yet.
	for _, unwanted := range []string{"cordon", "drain", "yaml", "exec", "node shell", "events"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("loading view unexpectedly shows verb %q:\n%s", unwanted, view)
		}
	}
}

// TestLoadingStateHeaderTimerAdvances checks 11b's header "· 0.4s" counting
// timer actually ticks off SpinnerTickMsg rather than staying frozen at 0s —
// same contract as browse's own loading timer.
func TestLoadingStateHeaderTimerAdvances(t *testing.T) {
	m := New(Config{Session: newSession(), Lister: fakeLister{}, NodeName: "node-a"})
	m.SetSize(120, 30)
	m.loadStartedAt = m.loadStartedAt.Add(-2 * time.Second)

	updated, _ := m.Update(spinner.TickMsg{Time: time.Now()})
	view := plain(updated.(*Model).Render())
	if !strings.Contains(view, "loading node-a · 2.") {
		t.Fatalf("expected header timer to show ~2s elapsed:\n%s", view)
	}
}

func TestUnknownNodeReturnsError(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{}}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "ghost"})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	if m.state != tui.TaskStateError {
		t.Fatalf("state = %s, want error", m.state)
	}
}

func TestEscSendsBackMsg(t *testing.T) {
	m := New(Config{Session: newSession(), NodeName: "node-a"})
	_, cmd := m.Update(tea.KeyPressMsg{Text: "esc"})
	if cmd == nil {
		t.Fatal("expected esc to return a command")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg, got %T", cmd())
	}
}

func TestDKeyConfirmsThenDrains(t *testing.T) {
	if verbs.Drain.Key != "ctrl+d" {
		t.Fatalf("Drain.Key = %q, want Bubble Tea key spelling ctrl+d", verbs.Drain.Key)
	}
	// Drain leaves DaemonSet and mirror (static) pods in place, so the
	// confirm must not count them as evicted.
	ds := schedPod("kube-system", "kube-proxy-x", "node-a", "64Mi")
	ds.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "kube-proxy"}}
	mirror := schedPod("kube-system", "etcd-node-a", "node-a", "64Mi")
	mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod:  {schedPod("default", "big", "node-a", "1Gi"), ds, mirror},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a", Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, tea.KeyPressMsg{Text: verbs.Drain.Key})
	if !m.actions.Active() {
		t.Fatal("expected D to open a drain confirmation")
	}
	if !strings.Contains(plain(m.Render()), "1 pods will be evicted.") {
		t.Fatalf("expected evicted-pod count in confirm body:\n%s", plain(m.Render()))
	}

	m = step(t, m, tea.KeyPressMsg{Text: "y"})
	if len(mut.drained) != 1 || mut.drained[0] != "node-a" {
		t.Fatalf("expected node-a drained, got %v", mut.drained)
	}
}

// TestKeybarGoesOfflineAndHidesCordonDrain pins the cross-cutting 4a fix
// (docs/design README.md §52, §301): nodedetail must show the OFFLINE pill
// and drop its mutating verbs while disconnected, not just browse — cordon
// and drain, plus the two tty-handoff verbs (exec, node shell), whose
// kubectl subprocesses write to the cluster just as much.
func TestKeybarGoesOfflineAndHidesCordonDrain(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
	}}
	mut := &fakeMutator{}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a", Mutator: mut})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial timeout"})
	kb := m.Keybar()
	if kb.Pill != tui.ModeOffline || kb.PillText != "OFFLINE" {
		t.Fatalf("Pill/PillText = %v/%q while offline, want ModeOffline/OFFLINE", kb.Pill, kb.PillText)
	}
	for _, g := range kb.Groups {
		for _, h := range g {
			switch h.Key {
			case verbs.Cordon.Key, verbs.Drain.Key, verbs.Exec.Key, verbs.NodeDebugDetail.Key:
				t.Fatalf("expected mutating hints hidden while offline, got groups %+v", kb.Groups)
			}
		}
	}
	// The keys themselves are refused, not just un-advertised — 's' in
	// particular would otherwise create a privileged node-debugger pod.
	if _, cmd := m.Update(tea.KeyPressMsg{Text: verbs.NodeDebugDetail.Key}); cmd != nil {
		t.Error("'s' spawned a node-debugger pod while offline")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Text: verbs.Exec.Key}); cmd != nil {
		t.Error("'x' handed the tty to kubectl while offline")
	}

	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnConnected})
	kb = m.Keybar()
	if kb.PillText != "NODE" {
		t.Fatalf("PillText = %q after reconnect, want NODE", kb.PillText)
	}
}

func TestOpenEventsHandoff(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
	}}
	var openedKind kube.ResourceKind
	var openedName string
	openEvents := func(kind kube.ResourceKind, ns, name string, _, _ int) (tea.Model, tea.Cmd) {
		openedKind, openedName = kind, name
		return sentinelTask{}, nil
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a", OpenEvents: openEvents})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "e"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected 'e' to hand off to the events task, got %T", updated)
	}
	if openedKind != kube.KindNode || openedName != "node-a" {
		t.Fatalf("openEvents called with (%s, %s), want (Node, node-a)", openedKind, openedName)
	}
}

// TestOpenForwardHandoff pins the cross-cutting missing-verb fix (docs/design
// README.md §304, §308: "on any object row") — 'f' must push the forward
// picker for the selected pod row, the same as browse's own Pod rows
// already do.
func TestOpenForwardHandoff(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindNode: {testNode("node-a")},
		kube.KindPod:  {schedPod("default", "big", "node-a", "1Gi")},
	}}
	var openedTarget kube.ForwardTarget
	openForward := func(target kube.ForwardTarget, _, _ int) (tea.Model, tea.Cmd) {
		openedTarget = target
		return sentinelTask{}, nil
	}
	m := New(Config{Session: newSession(), Lister: lister, NodeName: "node-a", OpenForward: openForward})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "f"})
	if _, ok := updated.(sentinelTask); !ok {
		t.Fatalf("expected 'f' to hand off to the forward picker, got %T", updated)
	}
	want := kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "big"}
	if openedTarget != want {
		t.Fatalf("openForward called with %+v, want %+v", openedTarget, want)
	}
}

type sentinelTask struct{}

func (sentinelTask) Init() tea.Cmd                       { return nil }
func (sentinelTask) Update(tea.Msg) (tea.Model, tea.Cmd) { return sentinelTask{}, nil }
func (sentinelTask) View() tea.View                      { return tea.View{} }

type fakeMutator struct {
	drained []string
	err     error // returned by Drain and Cordon when set
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
func (f *fakeMutator) Cordon(context.Context, string, bool) error              { return f.err }
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
func (f *fakeMutator) PatchSecretData(context.Context, string, string, string, string, bool) error {
	return nil
}
func (f *fakeMutator) PatchConfigMapData(context.Context, string, string, string, string, bool) error {
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
func (f *fakeMutator) Drain(_ context.Context, node string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.drained = append(f.drained, node)
	return 1, nil
}
