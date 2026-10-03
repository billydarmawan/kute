package setup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

func plain(s string) string { return ansi.Strip(s) }

func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	updated, _ := m.Update(msg)
	return *updated.(*Model)
}

// TestUnreachableBodyShowsCountdownAndSwitchContext pins 4c: the title
// names the failing context, the raw error renders verbatim, the retry
// countdown reflects Conn, and other kubeconfig contexts are listed.
func TestUnreachableBodyShowsCountdownAndSwitchContext(t *testing.T) {
	m := New(Config{
		Session:       &tui.Session{Theme: tui.Dark()},
		State:         Unreachable,
		ClusterName:   "microk8s-cluster",
		OtherContexts: []string{"prod-eks", "kind-local"},
	})
	m.SetSize(120, 30)
	m = step(t, m, kube.ConnStateMsg{
		Phase:       kube.ConnReconnecting,
		Err:         "dial tcp 10.0.0.5:16443: i/o timeout",
		Attempt:     2,
		NextRetryAt: m.now.Add(8 * time.Second),
	})

	body := plain(m.Render())
	if !strings.Contains(body, "microk8s-cluster is unreachable") {
		t.Errorf("body missing the title:\n%s", body)
	}
	if !strings.Contains(body, "dial tcp 10.0.0.5:16443: i/o timeout") {
		t.Errorf("body missing the verbatim error:\n%s", body)
	}
	if !strings.Contains(body, "retrying in 8s") || !strings.Contains(body, "attempt 2") {
		t.Errorf("body missing the retry countdown:\n%s", body)
	}
	if !strings.Contains(body, "prod-eks") || !strings.Contains(body, "kind-local") {
		t.Errorf("body missing the SWITCH CONTEXT preview:\n%s", body)
	}
	if !strings.Contains(body, "connecting failed") {
		t.Errorf("header badge should read connecting failed:\n%s", body)
	}
	// docs/design README.md §4c: the raw error box is bordered, matching
	// 10b's own LOOKED IN box — previously it rendered as plain tinted text
	// with no border at all.
	if !strings.Contains(body, "╭") || !strings.Contains(body, "╰") {
		t.Errorf("expected the raw error box to render a rounded border:\n%s", body)
	}
}

// TestUnreachableBodyWrapsLongErrorWithoutTruncating pins 4c's word-wrap
// truncation bug: a long connection error that lipgloss wraps onto a second
// line inside the raw-error box must keep that second line's full text, not
// collapse it down to a bare "…" (components.Pad/Truncate previously
// measured the whole wrapped, multi-line string as one run).
func TestUnreachableBodyWrapsLongErrorWithoutTruncating(t *testing.T) {
	longErr := "dial tcp 10.0.0.5:16443: i/o timeout after 30s waiting for the apiserver TLS handshake to complete"
	m := New(Config{
		Session:     &tui.Session{Theme: tui.Dark()},
		State:       Unreachable,
		ClusterName: "microk8s-cluster",
	})
	m.SetSize(120, 30)
	m = step(t, m, kube.ConnStateMsg{
		Phase:       kube.ConnReconnecting,
		Err:         longErr,
		Attempt:     1,
		NextRetryAt: m.now.Add(4 * time.Second),
	})

	body := plain(m.Render())
	if !strings.Contains(body, "apiserver TLS handshake to complete") {
		t.Errorf("expected the wrapped error's tail to survive intact, got:\n%s", body)
	}
}

// TestNoConfigBodyShowsLookedInBox pins 10b: a *kube.ConfigLookupError
// renders as the LOOKED IN box, one row per path checked.
func TestNoConfigBodyShowsLookedInBox(t *testing.T) {
	lookup := &kube.ConfigLookupError{Paths: []kube.PathCheck{
		{Label: "$KUBECONFIG", Reason: "not set"},
		{Label: "~/.kube/config", Path: "/home/x/.kube/config", Reason: "no such file"},
	}}
	m := New(Config{Session: &tui.Session{Theme: tui.Dark()}, State: NoConfig, Err: lookup})
	m.SetSize(120, 30)

	body := plain(m.Render())
	if !strings.Contains(body, "no kubeconfig found") {
		t.Errorf("body missing the headline:\n%s", body)
	}
	if !strings.Contains(body, "$KUBECONFIG") || !strings.Contains(body, "not set") {
		t.Errorf("body missing the $KUBECONFIG row:\n%s", body)
	}
	if !strings.Contains(body, "/home/x/.kube/config") || !strings.Contains(body, "no such file") {
		t.Errorf("body missing the ~/.kube/config row:\n%s", body)
	}
	if !strings.Contains(body, "no cluster") {
		t.Errorf("header badge should read no cluster:\n%s", body)
	}
}

// TestNoConfigBodyFindsLookupErrorInsideAJoin pins the LOOKED IN box against
// an errors.Join'd cause. Unwrap() []error is the tree a hand-rolled
// single-error walk cannot follow, and getting it wrong is silent: the box
// degrades to one "unknown error" line, which is exactly the screen a user
// with no kubeconfig is looking at.
func TestNoConfigBodyFindsLookupErrorInsideAJoin(t *testing.T) {
	lookup := &kube.ConfigLookupError{Paths: []kube.PathCheck{
		{Label: "$KUBECONFIG", Reason: "not set"},
	}}
	joined := fmt.Errorf("build cluster: %w", errors.Join(errors.New("probe failed"), lookup))
	m := New(Config{Session: &tui.Session{Theme: tui.Dark()}, State: NoConfig, Err: joined})
	m.SetSize(120, 30)

	body := plain(m.Render())
	if strings.Contains(body, "unknown error") {
		t.Errorf("joined lookup error was not unwrapped:\n%s", body)
	}
	if !strings.Contains(body, "$KUBECONFIG") || !strings.Contains(body, "not set") {
		t.Errorf("body missing the $KUBECONFIG row:\n%s", body)
	}
}

// TestSwitchContextListShowsPreProbedReachability pins 4c (docs/design
// README.md:65: "pre-probed in the background with reachability + latency
// — ✕ microk8s-cluster (current · timeout), ● prod-eks (reachable · 32ms)"):
// the current context shows its already-known failure with no extra probe,
// a probed-and-reachable sibling shows its latency, and an unprobed sibling
// still shows "probing…" rather than nothing.
func TestSwitchContextListShowsPreProbedReachability(t *testing.T) {
	m := New(Config{
		Session:       &tui.Session{Theme: tui.Dark()},
		State:         Unreachable,
		ClusterName:   "microk8s-cluster",
		OtherContexts: []string{"prod-eks", "kind-local"},
	})
	m.SetSize(120, 36)
	m = step(t, m, kube.ConnStateMsg{Phase: kube.ConnReconnecting, Err: "dial tcp: i/o timeout", Attempt: 1})
	m = step(t, m, switchProbeMsg{gen: m.probeGen, res: kube.ProbeResult{Name: "prod-eks", Latency: 32 * time.Millisecond}})

	body := plain(m.Render())
	for _, want := range []string{
		"microk8s-cluster (current · unreachable)",
		"prod-eks (reachable · 32ms)",
		"kind-local (probing…)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in the SWITCH CONTEXT list:\n%s", want, body)
		}
	}
}

// TestSwitchContextSelectionMovesAndConnectsOnEnter pins 4c's "↵ connect to
// selected · ↑↓ move" list navigation: j/k/up/down move the selection among
// switchContextRows, and ↵ on a selected sibling context calls
// SwitchToContext with that context's name — but is a no-op on the current
// (already-failing, index 0) row.
func TestSwitchContextSelectionMovesAndConnectsOnEnter(t *testing.T) {
	var gotName string
	// A non-nil sentinel Cmd, so "did ↵ return this Cmd" is a meaningful
	// assertion — connectToSelected's own success case can legitimately
	// return (nil, true) when SwitchToContext itself returns nil.
	sentinel := func() tea.Msg { return nil }
	m := New(Config{
		Session:       &tui.Session{Theme: tui.Dark()},
		State:         Unreachable,
		ClusterName:   "microk8s-cluster",
		OtherContexts: []string{"prod-eks", "kind-local"},
		SwitchToContext: func(name string) tea.Cmd {
			gotName = name
			return sentinel
		},
	})
	if m.switchSel != 1 {
		t.Fatalf("switchSel = %d, want 1 (defaults to the first sibling, not current)", m.switchSel)
	}

	m = step(t, m, tea.KeyPressMsg{Text: "j"})
	if m.switchSel != 2 {
		t.Fatalf("switchSel after 'j' = %d, want 2", m.switchSel)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "down"})
	if m.switchSel != 2 {
		t.Fatalf("switchSel after 'down' at the last row = %d, want clamped to 2", m.switchSel)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "k"})
	if m.switchSel != 1 {
		t.Fatalf("switchSel after 'k' = %d, want 1", m.switchSel)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected ↵ on a sibling context to return the connect cmd")
	}
	if gotName != "prod-eks" {
		t.Fatalf("SwitchToContext called with %q, want %q", gotName, "prod-eks")
	}

	m = step(t, m, tea.KeyPressMsg{Text: "up"})
	gotName = ""
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("expected ↵ on the current (index 0) row to be a no-op")
	}
	if gotName != "" {
		t.Fatalf("SwitchToContext must not be called for the current row, got %q", gotName)
	}
}

// TestInitProbesOtherContextsInBackground pins 4c's "pre-probed in the
// background": Init returns a non-nil Cmd as soon as the screen exists,
// with no keypress needed, whenever there are sibling contexts to probe.
func TestInitProbesOtherContextsInBackground(t *testing.T) {
	m := New(Config{State: Unreachable, OtherContexts: []string{"prod-eks"}})
	if m.Init() == nil {
		t.Fatal("expected a non-nil probe Cmd from Init when other contexts exist")
	}

	m = New(Config{State: Unreachable, OtherContexts: nil})
	if m.Init() != nil {
		t.Fatal("expected a nil Cmd from Init with no other contexts")
	}
}

// TestRetryKeyDispatch pins the three "r" behaviors: Unreachable's plain
// retry calls RetryNow (no rebuild); everything else calls Reconnect.
func TestRetryKeyDispatch(t *testing.T) {
	t.Run("Unreachable calls RetryNow, not Reconnect", func(t *testing.T) {
		var retried, reconnected bool
		m := New(Config{
			State:     Unreachable,
			RetryNow:  func() { retried = true },
			Reconnect: func(string) tea.Cmd { reconnected = true; return nil },
		})
		step(t, m, tea.KeyPressMsg{Text: "r"})
		if !retried || reconnected {
			t.Fatalf("retried=%v reconnected=%v, want retried only", retried, reconnected)
		}
	})

	t.Run("NoConfig calls Reconnect", func(t *testing.T) {
		var gotPath string
		called := false
		m := New(Config{
			State:     NoConfig,
			Reconnect: func(p string) tea.Cmd { called, gotPath = true, p; return nil },
		})
		step(t, m, tea.KeyPressMsg{Text: "r"})
		if !called || gotPath != "" {
			t.Fatalf("called=%v gotPath=%q, want called with empty path", called, gotPath)
		}
	})
}

// TestEditKubeconfigPathFlow pins the 'k'/'e' inline path editor: typing
// builds up pathInput, Enter submits it to Reconnect, Esc cancels.
func TestEditKubeconfigPathFlow(t *testing.T) {
	var gotPath string
	m := New(Config{
		State:          NoConfig,
		KubeconfigPath: "/old/path",
		Reconnect:      func(p string) tea.Cmd { gotPath = p; return nil },
	})
	m = step(t, m, tea.KeyPressMsg{Text: "k"})
	if !m.editing || m.pathInput.Value() != "/old/path" {
		t.Fatalf("editing=%v pathInput=%q after 'k', want editing with the current path prefilled", m.editing, m.pathInput.Value())
	}
	if !m.CapturingInput() {
		t.Fatalf("CapturingInput() = false while editing")
	}

	m = step(t, m, tea.KeyPressMsg{Text: "", Code: tea.KeyBackspace})
	if m.pathInput.Value() != "/old/pat" {
		t.Fatalf("pathInput after backspace = %q, want %q", m.pathInput.Value(), "/old/pat")
	}
	m = step(t, m, tea.KeyPressMsg{Text: "h2"})
	if m.pathInput.Value() != "/old/path2" {
		t.Fatalf("pathInput after typing = %q, want %q", m.pathInput.Value(), "/old/path2")
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.editing {
		t.Fatalf("still editing after enter")
	}
	if gotPath != "/old/path2" {
		t.Fatalf("Reconnect path = %q, want %q", gotPath, "/old/path2")
	}
}

// TestPasteIntoKubeconfigPath pins the two ways a paste reaches the path
// buffer: the terminal's own bracketed paste (tea.PasteMsg, not key presses)
// and the ctrl+v chord, both routed by tui.RoutePaste. Enter trims the
// whitespace a pasted path carries (textinput collapses its newlines to
// spaces), since " /path " is not a path.
func TestPasteIntoKubeconfigPath(t *testing.T) {
	var gotPath string
	m := New(Config{
		State:     NoConfig,
		Reconnect: func(p string) tea.Cmd { gotPath = p; return nil },
	})
	m = step(t, m, tea.KeyPressMsg{Text: "k"})

	m = step(t, m, tea.PasteMsg{Content: "/pasted/kube/config\n"})
	if got := m.pathInput.Value(); got != "/pasted/kube/config " {
		t.Fatalf("pathInput after bracketed paste = %q, want %q", got, "/pasted/kube/config ")
	}

	// ctrl+v answers with the clipboard read, whose reply comes back as the
	// same tea.PasteMsg the bracketed path delivers.
	m2 := New(Config{State: NoConfig})
	m2 = step(t, m2, tea.KeyPressMsg{Text: "k"})
	_, cmd := m2.Update(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'v'})
	if cmd == nil {
		t.Fatal("ctrl+v while editing returned no Cmd, want the clipboard read")
	}

	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if gotPath != "/pasted/kube/config" {
		t.Fatalf("Reconnect path = %q, want the trimmed %q", gotPath, "/pasted/kube/config")
	}
}

// TestPasteOutsideEditIsIgnored: a stray paste with no buffer open must not
// panic or leak into the path, and must leave the screen alone.
func TestPasteOutsideEditIsIgnored(t *testing.T) {
	m := New(Config{State: NoConfig, KubeconfigPath: "/x"})
	m = step(t, m, tea.PasteMsg{Content: "/junk"})
	if m.editing || m.pathInput.Value() != "" {
		t.Fatalf("editing=%v pathInput=%q after a paste with no buffer open, want both zero", m.editing, m.pathInput.Value())
	}
}

func TestEditEscCancels(t *testing.T) {
	m := New(Config{State: NoConfig, KubeconfigPath: "/x"})
	m = step(t, m, tea.KeyPressMsg{Text: "k"})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.editing {
		t.Fatalf("still editing after esc")
	}
}

// TestRetryFailedMsgShowsError pins Reconnect's failure path: setup stays
// on screen (no task swap — that's the root shell's job on success only)
// and surfaces the new error.
func TestRetryFailedMsgShowsError(t *testing.T) {
	m := New(Config{State: NoConfig})
	m.retrying = true
	m = step(t, m, RetryFailedMsg{Err: errors.New("still no kubeconfig")})
	if m.retrying {
		t.Fatalf("retrying should clear on RetryFailedMsg")
	}
	if !strings.Contains(plain(m.Render()), "still no kubeconfig") {
		t.Fatalf("body should show the retry error:\n%s", plain(m.Render()))
	}
}

// 4c for an expired credential: the cluster is answering, so "unreachable"
// would send the user off checking VPNs and DNS. The screen names the real
// problem, drops the retry countdown (nothing is retrying), and keeps 'r' as
// the way back in once they've re-authenticated elsewhere.
func TestUnreachableScreenNamesExpiredCredentials(t *testing.T) {
	m := New(Config{
		Session:     &tui.Session{Theme: tui.Dark()},
		State:       Unreachable,
		ClusterName: "prod-eks",
		Conn: kube.ConnState{
			Phase: kube.ConnUnauthenticated,
			Err:   "Error loading SSO Token: Token has expired",
		},
	})
	m.SetSize(120, 36)

	view := plain(m.Render())
	if !strings.Contains(view, "prod-eks needs re-authentication") {
		t.Errorf("view lacks the re-authentication headline:\n%s", view)
	}
	if strings.Contains(view, "prod-eks is unreachable") {
		t.Errorf("view still calls the cluster unreachable:\n%s", view)
	}
	if !strings.Contains(view, "Token has expired") {
		t.Errorf("view lacks the credential plugin's own message:\n%s", view)
	}
	if !strings.Contains(view, "not retrying") {
		t.Errorf("view lacks the not-retrying note:\n%s", view)
	}
	if !strings.Contains(view, "credentials expired") {
		t.Errorf("view lacks the header badge's credentials-expired text:\n%s", view)
	}
	if !strings.Contains(view, "re-authenticate in another shell, then press r") {
		t.Errorf("view lacks the keybar's recovery note:\n%s", view)
	}
}

// One rebuild at a time: a second ↵ or r while one is running would race the
// first to replace the root, and the losing cluster would never be stopped.
func TestRebuildKeysIgnoredWhileRetrying(t *testing.T) {
	var switches, reconnects int
	m := New(Config{
		Session:         &tui.Session{Theme: tui.Dark()},
		State:           Unreachable,
		ClusterName:     "microk8s-cluster",
		OtherContexts:   []string{"prod-eks"},
		SwitchToContext: func(string) tea.Cmd { switches++; return func() tea.Msg { return nil } },
		Reconnect:       func(string) tea.Cmd { reconnects++; return func() tea.Msg { return nil } },
	})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if switches != 1 {
		t.Errorf("SwitchToContext called %d times for two ↵ presses during one rebuild, want 1", switches)
	}
	m = step(t, m, tea.KeyPressMsg{Text: "e"})
	m.pathInput.SetValue("/tmp/kubeconfig")
	m = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if reconnects != 0 {
		t.Errorf("Reconnect called %d times while a switch was still running, want 0", reconnects)
	}

	m = step(t, m, RetryFailedMsg{Err: errors.New("unreachable")})
	_ = step(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if switches != 2 {
		t.Errorf("SwitchToContext called %d times after the first rebuild failed, want ↵ to work again", switches)
	}
}

// 02 M8: 10b said "no kubeconfig found" for every load error — including a
// mistyped --context against a perfectly good file.
func TestNoConfigTitleNamesWhatWentWrong(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&kube.ConfigLookupError{Paths: []kube.PathCheck{{Label: "$KUBECONFIG", Reason: "not set"}}}, "no kubeconfig found"},
		{fmt.Errorf("build cluster: %w", &kube.ContextNotFoundError{Name: "typo"}), "context typo not found in kubeconfig"},
		{kube.NewKubeconfigLoadError("/home/dev/.kube/config", errors.New("yaml: line 4: did not find expected node content")), "kubeconfig couldn't be loaded"},
		{errors.New("open /home/dev/.kube/config: permission denied"), "kubeconfig couldn't be loaded"},
	}
	for _, tc := range cases {
		m := New(Config{State: NoConfig, Err: tc.err})
		m.SetSize(120, 36)
		view := ansi.Strip(m.Render())
		if !strings.Contains(view, tc.want) {
			t.Errorf("%v: view lacks title %q:\n%s", tc.err, tc.want, view)
		}
		if tc.want != "no kubeconfig found" && strings.Contains(view, "no kubeconfig found") {
			t.Errorf("%v: view still says no kubeconfig found", tc.err)
		}
		if _, isLookup := errors.AsType[*kube.ConfigLookupError](tc.err); !isLookup && !strings.Contains(view, tc.err.Error()) {
			t.Errorf("%v: LOOKED IN box lacks the real message:\n%s", tc.err, view)
		}
	}
}

// TestSwitchContextListSkipsCredentialPluginContexts pins 4c's version of
// 7a's credential-plugin rule: opening the screen must not run a sibling's
// exec/auth-provider plugin (a browser login or SSO prompt per context), so
// such a context reads "not probed" — and ↵ on it still switches.
func TestSwitchContextListSkipsCredentialPluginContexts(t *testing.T) {
	var probed [][]string
	origProbe, origPlugin := probeContexts, credentialPluginContexts
	t.Cleanup(func() { probeContexts, credentialPluginContexts = origProbe, origPlugin })
	probeContexts = func(_ context.Context, names []string) <-chan kube.ProbeResult {
		probed = append(probed, slices.Clone(names))
		ch := make(chan kube.ProbeResult, len(names))
		for _, n := range names {
			ch <- kube.ProbeResult{Name: n, Latency: 4 * time.Millisecond}
		}
		close(ch)
		return ch
	}
	credentialPluginContexts = func() map[string]bool { return map[string]bool{"eks-prod": true} }

	var switched string
	m := New(Config{
		Session:         &tui.Session{Theme: tui.Dark()},
		State:           Unreachable,
		ClusterName:     "microk8s-cluster",
		OtherContexts:   []string{"eks-prod", "kind-local"},
		SwitchToContext: func(name string) tea.Cmd { switched = name; return func() tea.Msg { return nil } },
	})
	m.SetSize(120, 36)
	for cmd := m.Init(); cmd != nil; {
		msg := cmd()
		if _, done := msg.(switchProbesDoneMsg); done {
			break
		}
		_, cmd = m.Update(msg)
	}

	if want := [][]string{{"kind-local"}}; !slices.EqualFunc(probed, want, slices.Equal) {
		t.Fatalf("probed %v, want only the static-credential sibling %v", probed, want)
	}
	body := plain(m.Render())
	for _, want := range []string{"eks-prod (not probed)", "kind-local (reachable · 4ms)"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in the SWITCH CONTEXT list:\n%s", want, body)
		}
	}
	if m.switchSel != 1 {
		t.Fatalf("switchSel = %d, want 1 (eks-prod)", m.switchSel)
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd == nil || switched != "eks-prod" {
		t.Fatalf("↵ on a not-probed context should switch to it, got %q", switched)
	}
}
