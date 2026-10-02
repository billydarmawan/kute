package forwardpicker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
	"github.com/kute-dev/kute/internal/tui"
)

func plain(s string) string { return ansi.Strip(s) }

type fakeLister struct{ objs []runtime.Object }

func (f fakeLister) ListRaw(_ context.Context, _ kube.ResourceKind, _ string) ([]runtime.Object, error) {
	return f.objs, nil
}

type stubResolver struct{ pod string }

func (r stubResolver) ResolveForwardPod(context.Context, kube.ForwardTarget) (string, error) {
	if r.pod == "" {
		return "", context.DeadlineExceeded
	}
	return r.pod, nil
}

func (r stubResolver) ResolveForwardPort(_ context.Context, _, _ string, target intstr.IntOrString, protocol string) (int32, error) {
	return kube.PodTargetPort(nil, target, protocol)
}

func podWithPort(ns, name string, port int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "web", Ports: []corev1.ContainerPort{{ContainerPort: port, Name: "http"}}},
		}},
	}
}

func newModel(objs ...runtime.Object) Model {
	return New(Config{
		Session: &tui.Session{Theme: tui.Dark()},
		Lister:  fakeLister{objs: objs},
		Target:  kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "web-0"},
	})
}

func loadPorts(t *testing.T, m Model) Model {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("expected a non-nil Init Cmd")
	}
	msg := cmd()
	m.applyPortsLoaded(msg.(portsLoadedMsg))
	return m
}

// TestHeaderShowsForwardChipWhenActive pins 13d: every screen's Header()
// carries the ambient forward chip — forwardpicker was one of two omitting
// it.
func TestHeaderShowsForwardChipWhenActive(t *testing.T) {
	mgr := kube.NewForwardManager()
	target := kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "web-0"}
	mgr.Start(fake.NewForwardDialer(), fake.NewPodResolver(fake.New("default", "test")), target, "web-0", 18080, 80, "")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(mgr.List()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if len(mgr.List()) == 0 {
		t.Fatal("forward session never registered")
	}

	m := newModel(podWithPort("default", "web-0", 80))
	m.session.Forwards = mgr
	if got := m.Header().ForwardChip.Text; got == "" {
		t.Fatal("expected a non-empty forward chip while a forward is active")
	}
}

func TestInitLoadsPortsAndResolvesPod(t *testing.T) {
	t.Parallel()
	m := newModel(podWithPort("default", "web-0", 80))
	m = loadPorts(t, m)
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want Ready", m.state)
	}
	if len(m.rows) != 1 || m.rows[0].Port != 80 {
		t.Fatalf("rows = %+v", m.rows)
	}
	if m.resolvedPod != "web-0" {
		t.Fatalf("resolvedPod = %q, want web-0 (a Pod target resolves to itself)", m.resolvedPod)
	}
	// Port 80 is privileged, so the pre-fill should be 8080 (or bumped past
	// it if 8080 happens to be busy in the test environment).
	if m.rows[0].localPort < 8080 {
		t.Fatalf("localPort = %d, want >= 8080 for a privileged remote port", m.rows[0].localPort)
	}
}

func TestInitNotFoundYieldsError(t *testing.T) {
	t.Parallel()
	m := newModel() // no objects seeded
	m = loadPorts(t, m)
	if m.state != tui.TaskStateError {
		t.Fatalf("state = %s, want Error", m.state)
	}
	if !strings.Contains(m.feedback, "not found") {
		t.Fatalf("feedback = %q, want it to mention not found", m.feedback)
	}
}

func TestPreferredLocalPort(t *testing.T) {
	t.Parallel()
	if got := preferredLocalPort(80); got != 8080 {
		t.Errorf("preferredLocalPort(80) = %d, want 8080", got)
	}
	if got := preferredLocalPort(9090); got != 9090 {
		t.Errorf("preferredLocalPort(9090) = %d, want 9090 (non-privileged ports pre-fill unchanged)", got)
	}
}

func TestPickLocalPortSkipsBusyPort(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	busy := ln.Addr().(*net.TCPAddr).Port

	chosen, busyFrom := pickLocalPort(busy)
	if chosen == busy {
		t.Fatalf("pickLocalPort(%d) chose the busy port itself", busy)
	}
	if busyFrom != busy {
		t.Fatalf("busyFrom = %d, want %d", busyFrom, busy)
	}
}

func TestEscPopsBack(t *testing.T) {
	t.Parallel()
	m := newModel(podWithPort("default", "web-0", 80))
	m = loadPorts(t, m)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected a Cmd from esc")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg, got %T", cmd())
	}
}

func TestEnterStartsForwardAndPopsBack(t *testing.T) {
	t.Parallel()
	mgr := kube.NewForwardManager()
	m := New(Config{
		Session:  &tui.Session{Theme: tui.Dark()},
		Lister:   fakeLister{objs: []runtime.Object{podWithPort("default", "web-0", 80)}},
		Resolver: stubResolver{pod: "web-0"},
		Dialer:   fake.NewForwardDialer(),
		Manager:  mgr,
		Target:   kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "web-0"},
	})
	m = loadPorts(t, m)

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	next := updated.(*Model)
	if cmd == nil {
		t.Fatal("expected a Cmd from enter")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg after starting the forward, got %T", cmd())
	}
	_ = next
	if len(mgr.List()) != 1 {
		t.Fatalf("List() = %+v, want one started session", mgr.List())
	}
}

func TestDigitBeginsLocalPortEdit(t *testing.T) {
	t.Parallel()
	m := newModel(podWithPort("default", "web-0", 80))
	m = loadPorts(t, m)

	updated, _ := m.Update(tea.KeyPressMsg{Text: "9"})
	next := updated.(*Model)
	if !next.rows[0].editing {
		t.Fatal("expected typing a digit to begin editing the selected row's local port")
	}
	updated, _ = next.Update(tea.KeyPressMsg{Text: "0"})
	next = updated.(*Model)
	if next.rows[0].editInput.Value() != "90" {
		t.Fatalf("editBuf = %q, want %q", next.rows[0].editInput.Value(), "90")
	}

	updated, _ = next.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	// enter commits the edit and starts the forward (no manager wired here,
	// so startSelected no-ops after commit) — assert the commit itself took.
	next2, ok := updated.(*Model)
	if !ok {
		t.Fatalf("expected *Model, got %T", updated)
	}
	if next2.rows[0].editing {
		t.Fatal("expected enter to commit and clear editing state")
	}
}

func TestViewRendersPortsAndWillRunLine(t *testing.T) {
	t.Parallel()
	m := New(Config{
		Session:  &tui.Session{Theme: tui.Dark()},
		Lister:   fakeLister{objs: []runtime.Object{podWithPort("default", "web-0", 80)}},
		Resolver: stubResolver{pod: "web-0"},
		Target:   kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "web-0"},
	})
	m.SetSize(120, 36)
	m = loadPorts(t, m)
	out := plain(m.Render())
	if !strings.Contains(out, "80") {
		t.Fatalf("expected the port number in the rendered view, got:\n%s", out)
	}
	if !strings.Contains(out, "kubectl port-forward") {
		t.Fatalf("expected the 'will run' kubectl command in the rendered view, got:\n%s", out)
	}
}

type recordingDialer struct {
	mu    sync.Mutex
	dials []int32
}

func (d *recordingDialer) Dial(_, _ string, _ int, remotePort int32) (kube.Tunnel, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials = append(d.dials, remotePort)
	return nil, errors.New("recorded")
}

func (d *recordingDialer) first() int32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.dials) == 0 {
		return 0
	}
	return d.dials[0]
}

// TestServiceForwardTargetsPodContainerPort pins review finding H1 end to
// end through the picker against the fake provider: Service web (port 80 →
// targetPort "http") backed by api-0 (http = 8080) offers 80, documents
// `pod/api-0 8080:8080` (pre-filled local 8080 → the pod's 8080, never 80),
// and the manager dials the pod on 8080.
func TestServiceForwardTargetsPodContainerPort(t *testing.T) {
	t.Parallel()
	cluster := fake.New("shop", "test")
	cluster.Seed(kube.KindService, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "api"},
			Ports:    []corev1.ServicePort{{Name: "web", Port: 80, TargetPort: intstr.FromString("http")}},
		},
	})
	pod := podWithPort("shop", "api-0", 8080)
	pod.Labels = map[string]string{"app": "api"}
	cluster.Seed(kube.KindPod, pod)

	mgr := kube.NewForwardManager()
	dialer := &recordingDialer{}
	m := New(Config{
		Session:  &tui.Session{Theme: tui.Dark()},
		Lister:   cluster,
		Resolver: fake.NewPodResolver(cluster),
		Dialer:   dialer,
		Manager:  mgr,
		Target:   kube.ForwardTarget{Kind: kube.KindService, Namespace: "shop", Name: "web"},
	})
	m.SetSize(120, 36)
	m = loadPorts(t, m)
	if len(m.rows) != 1 || m.rows[0].Port != 80 {
		t.Fatalf("rows = %+v, want the one Service port 80", m.rows)
	}
	local := m.rows[0].localPort
	out := plain(m.Render())
	want := fmt.Sprintf("kubectl port-forward pod/api-0 %d:8080 -n shop", local)
	if !strings.Contains(out, want) {
		t.Fatalf("will-run line missing %q, got:\n%s", want, out)
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	defer mgr.StopAll()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && dialer.first() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := dialer.first(); got != 8080 {
		t.Fatalf("dialed pod port %d, want 8080 (targetPort http), not service port 80", got)
	}
}
