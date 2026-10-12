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
	// The one declared port, plus the trailing free-port row.
	if m.declaredRows() != 1 || len(m.rows) != 2 || m.rows[0].Port != 80 {
		t.Fatalf("rows = %+v", m.rows)
	}
	if !m.rows[1].manual {
		t.Fatalf("last row = %+v, want the free-port row", m.rows[1])
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

	chosen, busyFrom := pickLocalPort(busy, nil)
	if chosen == busy {
		t.Fatalf("pickLocalPort(%d) chose the busy port itself", busy)
	}
	if busyFrom != busy {
		t.Fatalf("busyFrom = %d, want %d", busyFrom, busy)
	}
}

// TestEnterOnAPortAnotherForwardOwnsBumpsInsteadOfStarting pins that a
// local port an existing forward holds — even one Reconnecting with no
// listener bound — is skipped by the pre-fill and refused on enter, rather
// than starting a second forward that can never bind.
func TestEnterOnAPortAnotherForwardOwnsBumpsInsteadOfStarting(t *testing.T) {
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
	defer mgr.StopAll()
	m = loadPorts(t, m)
	owned := m.rows[0].localPort
	// Another forward takes the pre-filled port after the picker loaded.
	mgr.Start(fake.NewForwardDialer(), stubResolver{pod: "web-0"}, kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "web-0"}, "web-0", owned, 80, "")

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	next := updated.(*Model)
	if cmd != nil {
		t.Fatalf("expected enter on an owned port to stay on the picker, got %T", cmd())
	}
	if len(mgr.List()) != 1 {
		t.Fatalf("List() = %d sessions, want only the original forward", len(mgr.List()))
	}
	if next.rows[0].busyFrom != owned || next.rows[0].localPort == owned {
		t.Fatalf("row = local %d busyFrom %d, want bumped past %d", next.rows[0].localPort, next.rows[0].busyFrom, owned)
	}

	// A fresh picker never pre-fills the owned port in the first place.
	if chosen, _ := pickLocalPort(owned, map[int]bool{owned: true}); chosen == owned {
		t.Fatalf("pickLocalPort chose port %d an existing forward owns", owned)
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
	defer mgr.StopAll()
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

// newForwardingModel is the picker wired to a real ForwardManager and a dialer
// that records what it was asked for, which is how these tests prove a forward
// actually started on the port they typed.
func newForwardingModel(t *testing.T, objs ...runtime.Object) (Model, *kube.ForwardManager, *recordingDialer) {
	t.Helper()
	mgr := kube.NewForwardManager()
	t.Cleanup(mgr.StopAll)
	dialer := &recordingDialer{}
	m := New(Config{
		Session:  &tui.Session{Theme: tui.Dark()},
		Lister:   fakeLister{objs: objs},
		Resolver: stubResolver{pod: "web-0"},
		Dialer:   dialer,
		Manager:  mgr,
		Target:   kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "web-0"},
	})
	m.SetSize(120, 36)
	return loadPorts(t, m), mgr, dialer
}

func typeOnRow(t *testing.T, m Model, typed string) Model {
	t.Helper()
	cur := &m
	for _, r := range typed {
		updated, _ := cur.Update(tea.KeyPressMsg{Text: string(r)})
		next, ok := updated.(*Model)
		if !ok {
			t.Fatalf("expected *Model, got %T", updated)
		}
		cur = next
	}
	return *cur
}

// barePod declares no containerPorts at all — the ordinary case this whole
// free-port row exists for, since a container listening on a port it never
// declared is perfectly legal and used to leave the picker with nothing to
// forward.
func barePod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
}

// TestPodWithNoDeclaredPortsIsStillForwardable pins the reason this row
// exists: an object that declares no ports used to render "no forwardable
// ports found" and leave no way forward at all.
func TestPodWithNoDeclaredPortsIsStillForwardable(t *testing.T) {
	t.Parallel()
	m := newModel(barePod("default", "web-0"))
	m.SetSize(120, 36)
	m = loadPorts(t, m)
	if m.state != tui.TaskStateReady {
		t.Fatalf("state = %s, want Ready — a pod declaring no ports is not an empty state", m.state)
	}
	if len(m.rows) != 1 || !m.rows[0].manual {
		t.Fatalf("rows = %+v, want just the free-port row", m.rows)
	}
	out := plain(m.Render())
	if !strings.Contains(out, "no declared ports") {
		t.Errorf("header should say the object declares none:\n%s", out)
	}
	if !strings.Contains(out, "other port") {
		t.Errorf("expected the free-port row in the panel:\n%s", out)
	}
	if !strings.Contains(out, "type a port the object doesn't declare") {
		t.Errorf("expected the will-run line to ask for a port:\n%s", out)
	}
}

// TestTypedPortOnTheFreeRowStartsAForward: one port derives the local one the
// same way a declared port does (9090 → 9090) and dials what was typed.
func TestTypedPortOnTheFreeRowStartsAForward(t *testing.T) {
	t.Parallel()
	m, mgr, dialer := newForwardingModel(t, barePod("default", "web-0"))
	m = typeOnRow(t, m, "9090")
	if got := m.rows[0].Port; got != 9090 {
		t.Fatalf("row Port = %d, want the typed 9090", got)
	}
	if got := m.rows[0].localPort; got != 9090 {
		t.Fatalf("localPort = %d, want 9090 (non-privileged ports pre-fill unchanged)", got)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if cmd == nil {
		t.Fatal("expected a Cmd from enter")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg after starting the forward, got %T", cmd())
	}
	if len(mgr.List()) != 1 {
		t.Fatalf("List() = %+v, want one started session", mgr.List())
	}
	session := mgr.List()[0]
	if session.RemotePort != 9090 || session.LocalPort != 9090 || session.ResolvedPod != "web-0" {
		t.Fatalf("session = %+v, want 9090 → 9090 on web-0", session)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && dialer.first() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := dialer.first(); got != 9090 {
		t.Fatalf("dialed pod port %d, want the typed 9090", got)
	}
}

// TestPortPairTypingSetsBothHalves pins kubectl's own `local:remote` spelling
// on the free-port row: the left half is the local bind and wins over the
// pre-fill — mid-type included, since the pair is live.
func TestPortPairTypingSetsBothHalves(t *testing.T) {
	t.Parallel()
	m, mgr, _ := newForwardingModel(t, barePod("default", "web-0"))
	m = typeOnRow(t, m, "8080:3000")
	if m.rows[0].Port != 3000 || m.rows[0].localPort != 8080 {
		t.Fatalf("row = remote %d local %d, want 3000 on 8080", m.rows[0].Port, m.rows[0].localPort)
	}
	if out := plain(m.Render()); !strings.Contains(out, "3000") {
		t.Fatalf("the typed port should render on the free-port row:\n%s", out)
	}

	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if len(mgr.List()) != 1 {
		t.Fatalf("List() = %+v, want one started session", mgr.List())
	}
	session := mgr.List()[0]
	if session.LocalPort != 8080 || session.RemotePort != 3000 {
		t.Fatalf("session = %+v, want localhost:8080 → pod 3000", session)
	}
}

// TestHalfTypedPairNeverForwardsNothing: a local port with no remote behind it
// — the `8080:` you can see mid-type and can commit — is refused rather than
// forwarded, so no session can ever be left aimed at port 0.
func TestHalfTypedPairNeverForwardsNothing(t *testing.T) {
	t.Parallel()
	m, mgr, _ := newForwardingModel(t, barePod("default", "web-0"))
	m = typeOnRow(t, m, "8080:")
	if m.rows[0].Port != 0 || m.rows[0].localPort != 8080 {
		t.Fatalf("row = remote %d local %d, want the remote unset and 8080 kept", m.rows[0].Port, m.rows[0].localPort)
	}
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if cmd != nil {
		t.Fatalf("expected no Cmd, got %T", cmd())
	}
	if got := updated.(*Model).feedback; !strings.Contains(got, "type a port") {
		t.Fatalf("feedback = %q, want it to ask for the remote port", got)
	}
	if len(mgr.List()) != 0 {
		t.Fatalf("List() = %+v, want no session started", mgr.List())
	}
}

// TestEnterOnAnUntypedFreeRowSaysSoInsteadOfForwardingPortZero: the refuse is
// an inline note on the keybar, not a modal, and no session is started.
func TestEnterOnAnUntypedFreeRowSaysSoInsteadOfForwardingPortZero(t *testing.T) {
	t.Parallel()
	m, mgr, _ := newForwardingModel(t, barePod("default", "web-0"))
	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if cmd != nil {
		t.Fatalf("expected no Cmd, got %T", cmd())
	}
	next := updated.(*Model)
	if !strings.Contains(next.feedback, "type a port") {
		t.Fatalf("feedback = %q, want it to ask for a port", next.feedback)
	}
	if len(mgr.List()) != 0 {
		t.Fatalf("List() = %+v, want no session started", mgr.List())
	}
	// The note is stale the moment another key lands.
	typed := typeOnRow(t, *next, "3")
	if typed.feedback != "" {
		t.Fatalf("feedback = %q, want it cleared by the next keypress", typed.feedback)
	}
}

// TestStartsWithoutABackingPodYet: a Service whose backing pod can't be
// resolved yet (scaled to zero, or one transient resolve error) still starts —
// the manager's reconnect loop re-resolves the pod each retry and connects
// once one is Ready, rather than making the user reopen the picker.
func TestStartsWithoutABackingPodYet(t *testing.T) {
	t.Parallel()
	mgr := kube.NewForwardManager()
	defer mgr.StopAll()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "api"},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 80}},
		},
	}
	m := New(Config{
		Session:  &tui.Session{Theme: tui.Dark()},
		Lister:   fakeLister{objs: []runtime.Object{svc}},
		Resolver: stubResolver{}, // resolves to no pod
		Dialer:   fake.NewForwardDialer(),
		Manager:  mgr,
		Target:   kube.ForwardTarget{Kind: kube.KindService, Namespace: "default", Name: "web"},
	})
	m.SetSize(120, 36)
	m = loadPorts(t, m)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if cmd == nil {
		t.Fatal("expected enter to start the forward and pop back")
	}
	if _, ok := cmd().(tui.BackMsg); !ok {
		t.Fatalf("expected tui.BackMsg, got %T", cmd())
	}
	if len(mgr.List()) != 1 {
		t.Fatalf("List() = %+v, want one session left to the reconnect loop", mgr.List())
	}
}

// TestFreeRowAcceptsOneColonOnly: a second colon, or a leading one, is never
// part of a port pair — `1:2:3` used to reach parsePort, which read ':' as the
// digit 10 and forwarded to port 303.
func TestFreeRowAcceptsOneColonOnly(t *testing.T) {
	t.Parallel()
	m, _, _ := newForwardingModel(t, barePod("default", "web-0"))
	m = typeOnRow(t, m, "1:2:3")
	if got := m.rows[0].editInput.Value(); got != "1:23" {
		t.Fatalf("buffer = %q, want the second colon dropped", got)
	}
	if m.rows[0].Port != 23 || m.rows[0].localPort != 1 {
		t.Fatalf("row = local %d remote %d, want 1 → 23", m.rows[0].localPort, m.rows[0].Port)
	}
}

// TestEscOnFreeRowRestoresThePreviousPort: the buffer drives the row live, so
// cancelling it must put back what was there — not leave the half-typed port
// for a later enter to forward.
func TestEscOnFreeRowRestoresThePreviousPort(t *testing.T) {
	t.Parallel()
	m, mgr, _ := newForwardingModel(t, barePod("default", "web-0"))
	m = typeOnRow(t, m, "30")
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Text: "esc"})
	m = *updated.(*Model)
	if m.rows[0].editing || m.rows[0].Port != 0 || m.rows[0].localPort != 0 {
		t.Fatalf("row = %+v, want the untyped free row back", m.rows[0])
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	if len(mgr.List()) != 0 {
		t.Fatalf("List() = %+v, want no forward to the cancelled port", mgr.List())
	}

	// And a committed port survives a cancelled edit of itself.
	m = *updated.(*Model)
	m = typeOnRow(t, m, "9000:3000")
	m.commitEdit()
	m = typeOnRow(t, m, "1")
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape, Text: "esc"})
	m = *updated.(*Model)
	if m.rows[0].Port != 3000 || m.rows[0].localPort != 9000 || !m.rows[0].localEdited {
		t.Fatalf("row = local %d remote %d, want 9000 → 3000 restored", m.rows[0].localPort, m.rows[0].Port)
	}
}

// TestTypingOnACommittedFreeRowEditsIt: a digit appends to the committed
// spelling instead of starting over, so the local override isn't silently lost.
func TestTypingOnACommittedFreeRowEditsIt(t *testing.T) {
	t.Parallel()
	m, _, _ := newForwardingModel(t, barePod("default", "web-0"))
	m = typeOnRow(t, m, "9000:300")
	m.commitEdit()
	m = typeOnRow(t, m, "1")
	if got := m.rows[0].editInput.Value(); got != "9000:3001" {
		t.Fatalf("buffer = %q, want the committed pair with the digit appended", got)
	}
	if m.rows[0].localPort != 9000 || m.rows[0].Port != 3001 {
		t.Fatalf("row = local %d remote %d, want 9000 → 3001", m.rows[0].localPort, m.rows[0].Port)
	}
}

// TestOutOfRangePortSaysSo: a half past 65535 is an error the user can read,
// not an untyped port or a silent fall back to the pre-fill.
func TestOutOfRangePortSaysSo(t *testing.T) {
	t.Parallel()
	for _, typed := range []string{"99999", "70000:3000"} {
		m, mgr, _ := newForwardingModel(t, barePod("default", "web-0"))
		m = typeOnRow(t, m, typed)
		if out := plain(m.Render()); !strings.Contains(out, "port must be 1-65535") {
			t.Errorf("%s: will-run line should name the range:\n%s", typed, out)
		}
		updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
		if cmd != nil {
			t.Fatalf("%s: expected no Cmd, got %T", typed, cmd())
		}
		if got := updated.(*Model).feedback; got != "port must be 1-65535" {
			t.Errorf("%s: feedback = %q, want the range note", typed, got)
		}
		if len(mgr.List()) != 0 {
			t.Fatalf("%s: List() = %+v, want no session", typed, mgr.List())
		}
	}
}

// TestFreeRowSitsBelowDeclaredPorts: the escape hatch never displaces a real
// candidate, and the header's port count still describes the object.
func TestFreeRowSitsBelowDeclaredPorts(t *testing.T) {
	t.Parallel()
	m := newModel(podWithPort("default", "web-0", 80))
	m = loadPorts(t, m)
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown, Text: "down"})
	next := updated.(*Model)
	if next.selected != len(next.rows)-1 || !next.rows[next.selected].manual {
		t.Fatalf("selected = %d of %d rows, want the trailing free-port row", next.selected, len(next.rows))
	}
	if out := plain(next.Render()); !strings.Contains(out, "1 ports") {
		t.Fatalf("header should count declared ports only:\n%s", out)
	}
	// A digit on the free-port row opens the remote-port buffer, not the local
	// one the declared rows edit.
	opened, _ := next.Update(tea.KeyPressMsg{Text: "5"})
	row := opened.(*Model).rows[next.selected]
	if !row.editing || !row.editRemote {
		t.Fatalf("row = editing %v editRemote %v, want the remote-port buffer", row.editing, row.editRemote)
	}
	if row.editInput.CharLimit != portPairLimit {
		t.Fatalf("CharLimit = %d, want %d (the `local:remote` spelling)", row.editInput.CharLimit, portPairLimit)
	}
}

func TestParsePortPair(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in     string
		local  int
		remote int
		err    bool
	}{
		{in: "3000", local: 0, remote: 3000},
		{in: "8080:3000", local: 8080, remote: 3000},
		{in: "8080:", local: 8080, remote: 0}, // half-typed pair, remote unusable
		{in: ":3000", local: 0, remote: 3000}, // likewise the other half
		{in: "80:9090:", err: true},           // a second colon is no port
		{in: "", local: 0, remote: 0},
		{in: "70000", err: true},      // out of range says so, never a target
		{in: "70000:3000", err: true}, // and never falls back to the pre-fill
	}
	for _, tc := range tests {
		local, remote, err := parsePortPair(tc.in)
		if (err != nil) != tc.err || local != tc.local || remote != tc.remote {
			t.Errorf("parsePortPair(%q) = (%d, %d, %v), want (%d, %d, err=%v)", tc.in, local, remote, err, tc.local, tc.remote, tc.err)
		}
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
	if m.declaredRows() != 1 || m.rows[0].Port != 80 {
		t.Fatalf("rows = %d declared, want the one Service port 80", m.declaredRows())
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

// TestServiceFreeRowReadsAServicePort: on a Service, kubectl's `svc/<name>
// local:remote` names a Service port — so a typed port the Service declares
// dials its targetPort, exactly as that port's own row does.
func TestServiceFreeRowReadsAServicePort(t *testing.T) {
	t.Parallel()
	cluster := fake.New("shop", "test")
	cluster.Seed(kube.KindService, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "api"},
			Ports:    []corev1.ServicePort{{Name: "web", Port: 80, TargetPort: intstr.FromString("http")}},
		},
	})
	pod := podWithPort("shop", "api-0", 8080)
	pod.Labels = map[string]string{"app": "api"}
	cluster.Seed(kube.KindPod, pod)

	mgr := kube.NewForwardManager()
	defer mgr.StopAll()
	dialer := &recordingDialer{}
	m := New(Config{
		Session:  &tui.Session{Theme: tui.Dark()},
		Lister:   cluster,
		Resolver: fake.NewPodResolver(cluster),
		Dialer:   dialer,
		Manager:  mgr,
		Target:   kube.ForwardTarget{Kind: kube.KindService, Namespace: "shop", Name: "api"},
	})
	m.SetSize(120, 36)
	m = loadPorts(t, m)
	m.moveSelection(1)
	m = typeOnRow(t, m, "80")
	if got := m.rows[1].podPort; got != 8080 {
		t.Fatalf("podPort = %d, want Service port 80's targetPort 8080", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "enter"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && dialer.first() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := dialer.first(); got != 8080 {
		t.Fatalf("dialed pod port %d, want 8080 (Service port 80's targetPort)", got)
	}
}
