package browse

import (
	"net"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/kube/fake"
)

// TestForwardOnPodPushesPicker confirms 'f' on a Pod row calls OpenForward
// with a ForwardTarget built from the selected row (docs/design README.md
// §13a).
func TestForwardOnPodPushesPicker(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	var gotTarget kube.ForwardTarget
	m := New(Config{
		Session: newSession(), Lister: lister,
		OpenForward: func(target kube.ForwardTarget, w, h int) (tea.Model, tea.Cmd) {
			gotTarget = target
			return stubTask{}, nil
		},
	})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, _ := m.Update(tea.KeyPressMsg{Text: "f"})
	if _, ok := updated.(stubTask); !ok {
		t.Fatalf("expected forwardpicker's stub task to be pushed, got %T", updated)
	}
	want := kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: "api-0"}
	if gotTarget != want {
		t.Fatalf("ForwardTarget = %+v, want %+v", gotTarget, want)
	}
}

// TestForwardsBreadcrumbShowsAllNamespacesNotClusterScoped pins 13c: the
// breadcrumb tag reads "all namespaces" (forwards are global, never
// namespace-filtered), not the generic "cluster-scoped" tag Nodes/
// Namespaces get from sharing the same ClusterScoped registry flag.
func TestForwardsBreadcrumbShowsAllNamespacesNotClusterScoped(t *testing.T) {
	session := newSession()
	session.Location.Kind = kube.KindForward
	m := New(Config{Session: session, Lister: fakeLister{}})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // not Init: the Forwards clock tick would loop step forever

	view := plain(m.Render())
	if !strings.Contains(view, "all namespaces") {
		t.Fatalf("expected the 'all namespaces' breadcrumb tag:\n%s", view)
	}
	if strings.Contains(view, "cluster-scoped") {
		t.Fatalf("expected no 'cluster-scoped' tag for Forwards:\n%s", view)
	}
}

// TestForwardNoOpWithoutOpenForward confirms 'f' stays inert when
// OpenForward isn't wired.
func TestForwardNoOpWithoutOpenForward(t *testing.T) {
	lister := fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindPod: {pod("default", "api-0")},
	}}
	m := New(Config{Session: newSession(), Lister: lister})
	m.SetSize(120, 36)
	m = step(t, m, m.Init()())

	updated, cmd := m.Update(tea.KeyPressMsg{Text: "f"})
	if _, ok := updated.(*Model); !ok {
		t.Fatalf("expected browse to stay the active task, got %T", updated)
	}
	if cmd != nil {
		t.Fatal("expected no Cmd when OpenForward isn't wired")
	}
}

// seedForward starts one active session against mgr, using kube/fake's
// stand-in dialer/resolver, and blocks briefly until it's observably active.
func seedForward(t *testing.T, mgr *kube.ForwardManager, name string) kube.ForwardSession {
	t.Helper()
	target := kube.ForwardTarget{Kind: kube.KindPod, Namespace: "default", Name: name}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	session := mgr.Start(fake.NewForwardDialer(), fake.NewPodResolver(fake.New("default", "test")), target, name, localPort, 80, "")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range mgr.List() {
			if s.ID == session.ID && s.State == kube.ForwardActive {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s never became active", session.ID)
	return kube.ForwardSession{}
}

func forwardsLister(mgr *kube.ForwardManager) fakeLister {
	return fakeLister{objs: map[kube.ResourceKind][]runtime.Object{
		kube.KindForward: mgr.ListRaw(),
	}}
}

// TestStopSelectedForwardEndsSession confirms 'x' on a Forwards row stops
// that session via the shared kube.ForwardManager, not a screen push.
func TestStopSelectedForwardEndsSession(t *testing.T) {
	mgr := kube.NewForwardManager()
	session := seedForward(t, mgr, "web-1")
	sess := newSession()
	sess.Location.Kind = kube.KindForward
	m := New(Config{Session: sess, Lister: forwardsLister(mgr), Forwards: mgr})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // not Init: the Forwards clock tick would loop step forever

	if m.kind != kube.KindForward {
		t.Fatalf("kind = %s, want Forward", m.kind)
	}
	m.Update(tea.KeyPressMsg{Text: "x"})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, s := range mgr.List() {
			if s.ID == session.ID {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s was not stopped", session.ID)
}

// TestStopAllForwardsRequiresConfirm confirms 'X' arms the inline y/N gate
// and only 'y' actually stops every session (docs/design README.md §13c:
// "only X stop all gets the inline y/N").
func TestStopAllForwardsRequiresConfirm(t *testing.T) {
	mgr := kube.NewForwardManager()
	seedForward(t, mgr, "web-1")
	sess := newSession()
	sess.Location.Kind = kube.KindForward
	m := New(Config{Session: sess, Lister: forwardsLister(mgr), Forwards: mgr})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // not Init: the Forwards clock tick would loop step forever

	updated, _ := m.Update(tea.KeyPressMsg{Text: "X"})
	next := updated.(*Model)
	if !next.pendingStopAllForwards {
		t.Fatal("expected pendingStopAllForwards to be armed after X")
	}
	if len(mgr.List()) == 0 {
		t.Fatal("sessions must not be stopped before confirming")
	}

	updated, _ = next.Update(tea.KeyPressMsg{Text: "n"})
	next = updated.(*Model)
	if next.pendingStopAllForwards {
		t.Fatal("expected 'n' to cancel the confirm")
	}
	if len(mgr.List()) == 0 {
		t.Fatal("'n' must not stop any sessions")
	}

	updated, _ = next.Update(tea.KeyPressMsg{Text: "X"})
	next = updated.(*Model)
	updated, _ = next.Update(tea.KeyPressMsg{Text: "y"})
	next = updated.(*Model)
	if next.pendingStopAllForwards {
		t.Fatal("expected 'y' to clear the confirm")
	}
	if len(mgr.List()) != 0 {
		t.Fatalf("expected 'y' to stop every session, got %+v", mgr.List())
	}
}

// TestForwardSummaryText checks 13c's health-strip right side wording.
func TestForwardSummaryText(t *testing.T) {
	mgr := kube.NewForwardManager()
	seedForward(t, mgr, "web-1")
	sess := newSession()
	sess.Location.Kind = kube.KindForward
	m := New(Config{Session: sess, Lister: forwardsLister(mgr), Forwards: mgr})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // not Init: the Forwards clock tick would loop step forever

	got := m.forwardSummaryText()
	want := "1 forwards · 1 namespaces · forwards end when kute exits"
	if got != want {
		t.Fatalf("forwardSummaryText() = %q, want %q", got, want)
	}
}

// TestForwardsListTicksTheClock pins review 11 L10: UPTIME, "idle 12m" and a
// Reconnecting row's "next in Ns" are derived at projection time and the
// manager only notifies on a state change, so the Forwards list must ride
// the same one-second UI clock as CronJobs — each tick re-projects the rows
// and re-arms the next one.
func TestForwardsListTicksTheClock(t *testing.T) {
	session := newSession()
	session.Location.Kind = kube.KindForward
	m := New(Config{Session: session, Lister: fakeLister{}})
	m.SetSize(120, 36)
	m = step(t, m, m.load()()) // not Init: the Forwards clock tick would loop step forever
	if !m.ticksCronJobClock() {
		t.Fatal("expected the Forwards list to run the one-second UI clock")
	}

	_, cmd := m.Update(cronJobTickMsg{epoch: m.clockEpoch})
	msgs := drainCmd(cmd, 1500*time.Millisecond)
	var reloaded bool
	for _, msg := range msgs {
		if _, ok := msg.(rowsLoadedMsg); ok {
			reloaded = true
		}
	}
	if !reloaded {
		t.Fatalf("expected a Forwards tick to re-project the rows, got %#v", msgs)
	}
	if len(clockTicks(msgs)) != 1 {
		t.Fatalf("expected the tick to re-arm exactly one next tick, got %#v", msgs)
	}
}
