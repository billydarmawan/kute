package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"

	"github.com/kute-dev/kute/internal/diag"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// stubRoot stands in for tui.Model: it satisfies crashInspector and can be
// told to panic, which is the whole point of the seam.
type stubRoot struct {
	sess         *tui.Session
	screen       string
	conn         kube.ConnState
	panicOnKey   bool
	panicOnView  bool
	updateCalled *int
}

func (s stubRoot) Init() tea.Cmd { return nil }

func (s stubRoot) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if s.updateCalled != nil {
		*s.updateCalled++
	}
	if _, ok := msg.(tea.KeyPressMsg); ok && s.panicOnKey {
		panic("boom in update")
	}
	return s, nil
}

func (s stubRoot) View() tea.View {
	if s.panicOnView {
		panic("boom in view")
	}
	return tea.NewView("")
}

func (s stubRoot) Session() *tui.Session { return s.sess }
func (s stubRoot) Screen() string        { return s.screen }
func (s stubRoot) Conn() kube.ConnState  { return s.conn }

func newTestSink(t *testing.T, live *liveState) *diag.Sink {
	t.Helper()
	sink, err := diag.Open(diag.Options{
		Build:    diag.Build{Version: "0.3.0", Commit: "abc1234"},
		CrashDir: t.TempDir(),
		Snapshot: live.Snapshot,
	})
	if err != nil {
		t.Fatalf("diag.Open: %v", err)
	}
	live.sink = sink
	return sink
}

// captureCrashOut redirects the crash footer so it can be asserted on
// instead of sprayed over the test log.
func captureCrashOut(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	previous := crashOut
	crashOut = &buf
	t.Cleanup(func() { crashOut = previous })
	return &buf
}

func sessionAt(context, namespace string, kind kube.ResourceKind) *tui.Session {
	sess := &tui.Session{}
	sess.Location.Context = context
	sess.Location.Namespace = namespace
	sess.Location.Kind = kind
	return sess
}

// The crash footer's four required facts (version, context, active kind,
// terminal size) all have to survive the trip from the update loop to a
// report written on a dying goroutine.
func TestCrashCatcherRecordsWhereTheUserWas(t *testing.T) {
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)
	root := stubRoot{
		sess:       sessionAt("prod-eu-1", "payments", kube.KindPod),
		screen:     "browse.Model",
		conn:       kube.ConnState{Phase: kube.ConnConnected},
		panicOnKey: true,
	}

	var model tea.Model = newCrashCatcher(root, sink, live)
	model, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})

	path := catchPanic(t, func() { model.Update(tea.KeyPressMsg{}) }) // panics
	if path != "boom in update" {
		t.Fatalf("recovered %q, want the original panic value to keep travelling", path)
	}

	report := sink.LastReport()
	if report == "" {
		t.Fatal("no crash report written")
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"0.3.0 (abc1234)", "prod-eu-1", "payments", "Pod", "120x36",
		"browse.Model", "boom in update", "caught in: update",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q; got:\n%s", want, got)
		}
	}
}

func TestCrashCatcherRecordsAViewPanic(t *testing.T) {
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)
	model := newCrashCatcher(stubRoot{panicOnView: true}, sink, live)

	if got := catchPanic(t, func() { model.View() }); got != "boom in view" {
		t.Fatalf("recovered %q, want the original panic value", got)
	}
	data, _ := os.ReadFile(sink.LastReport())
	if !strings.Contains(string(data), "caught in: view") {
		t.Errorf("report does not name the view as the crash site; got:\n%s", data)
	}
}

// The wrapper is on the hot path of every message: a normal run must reach
// the real model and hand its result back unchanged.
func TestCrashCatcherIsTransparentWhenNothingPanics(t *testing.T) {
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)
	updates := 0
	root := stubRoot{sess: sessionAt("dev", "default", kube.KindPod), updateCalled: &updates}

	var model tea.Model = newCrashCatcher(root, sink, live)
	model, cmd := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if cmd != nil {
		t.Errorf("Update() cmd = %v, want the inner model's nil", cmd)
	}
	if updates != 1 {
		t.Errorf("inner Update called %d times, want 1", updates)
	}
	if _, ok := model.(crashCatcher); !ok {
		t.Fatalf("Update() returned %T, want the wrapper to stay wrapped", model)
	}
	if sink.LastReport() != "" {
		t.Errorf("a clean run wrote a crash report at %s", sink.LastReport())
	}
	if snap := live.Snapshot(); snap.Width != 80 || snap.Height != 24 {
		t.Errorf("snapshot size = %dx%d, want 80x24", snap.Width, snap.Height)
	}
}

// Half the "kute showed me nothing" reports are an outage, so every
// connection transition belongs in the log — once per transition, not once
// per message.
func TestCrashCatcherLogsConnectionChanges(t *testing.T) {
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)
	root := stubRoot{
		sess: sessionAt("prod-eu-1", "payments", kube.KindPod),
		conn: kube.ConnState{Phase: kube.ConnReconnecting, Err: "dial tcp: i/o timeout"},
	}

	var model tea.Model = newCrashCatcher(root, sink, live)
	model, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 36}) // same state again

	var logged []string
	for _, line := range sink.Recent() {
		if strings.Contains(line, "connection") {
			logged = append(logged, line)
		}
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d connection lines, want exactly one per transition: %#v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "reconnecting") || !strings.Contains(logged[0], "i/o timeout") {
		t.Errorf("connection line = %q, want the phase and the error", logged[0])
	}
}

// A panic inside a tea.Cmd goroutine is caught by Bubble Tea itself, so
// run never sees the value — only ErrProgramPanic coming back out of
// Run. The report still has to exist.
func TestReportProgramCrashCoversAPanicKuteNeverSaw(t *testing.T) {
	live := newLiveState(nil, false)
	live.snap = diag.Snapshot{Context: "prod-eu-1", Kind: "Pod", Width: 120, Height: 36}
	sink := newTestSink(t, live)
	footer := captureCrashOut(t)

	err := reportProgramCrash(sink, errors.Join(tea.ErrProgramKilled, tea.ErrProgramPanic))
	if !errors.Is(err, ErrCrashed) {
		t.Fatalf("reportProgramCrash() = %v, want ErrCrashed", err)
	}
	report := sink.LastReport()
	if report == "" {
		t.Fatal("no crash report written for a panic caught below kute")
	}
	data, _ := os.ReadFile(report)
	got := string(data)
	if !strings.Contains(got, "value unavailable") || !strings.Contains(got, "prod-eu-1") {
		t.Errorf("report is not diagnosable; got:\n%s", got)
	}
	// The footer is the only thing the user sees, so it has to carry the
	// four facts and the path to the file.
	for _, want := range []string{"0.3.0 (abc1234)", "prod-eu-1", "Pod", "120x36", report, diag.IssueURL} {
		if !strings.Contains(footer.String(), want) {
			t.Errorf("crash footer missing %q; got:\n%s", want, footer.String())
		}
	}
}

// When crashCatcher already wrote the report — the common case, since most
// panics happen in Update or View — reportProgramCrash must point at that
// one rather than writing a second, emptier file over it.
func TestReportProgramCrashReusesTheRecordedReport(t *testing.T) {
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)
	captureCrashOut(t)
	first := sink.Report(diag.Crash{Cause: "update", Value: "boom"})

	// The returned path is deliberately ignored: this test asserts on
	// LastReport() precisely because the call must *not* produce a new one.
	//nolint:errcheck // the assertion below is about LastReport, not this return
	reportProgramCrash(sink, errors.Join(tea.ErrProgramKilled, tea.ErrProgramPanic))
	if sink.LastReport() != first {
		t.Errorf("LastReport() = %q, want the already-written %q", sink.LastReport(), first)
	}
	entries, err := os.ReadDir(filepath.Dir(first))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("crash dir holds %d files, want just the one report", len(entries))
	}
}

func TestReportProgramCrashLeavesOrdinaryErrorsAlone(t *testing.T) {
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)

	want := errors.New("no kubeconfig found")
	if got := reportProgramCrash(sink, want); !errors.Is(got, want) {
		t.Errorf("reportProgramCrash() = %v, want the original error", got)
	}
	if got := reportProgramCrash(sink, nil); got != nil {
		t.Errorf("reportProgramCrash(nil) = %v, want nil", got)
	}
	if sink.LastReport() != "" {
		t.Error("a non-panic error wrote a crash report")
	}
}

// KUTE_CRASH_TEST is how the crash path gets exercised against a real
// terminal, which is the acceptance criterion for the whole feature.
func TestCrashTestEnvCrashesOnPurpose(t *testing.T) {
	t.Setenv(crashTestEnv, "1")
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)
	model := newCrashCatcher(stubRoot{sess: sessionAt("dev", "default", kube.KindPod)}, sink, live)

	got := catchPanic(t, func() { model.Update(tea.KeyPressMsg{}) })
	if !strings.Contains(got, crashTestEnv) {
		t.Errorf("panic value = %q, want it to name %s", got, crashTestEnv)
	}
	if sink.LastReport() == "" {
		t.Fatal("the deliberate crash produced no report file")
	}
}

// klog is what client-go logs reflector and watch failures through, and it
// is the reason --log-file exists at all: those lines used to go to
// io.Discard because stderr is the screen.
func TestConfigureKlogRoutesIntoTheSink(t *testing.T) {
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)
	configureKlog(sink)
	t.Cleanup(func() { klog.SetOutput(io.Discard) })

	klog.Error("reflector: failed to watch *v1.Pod: connection refused")
	klog.Flush()

	var found bool
	for _, line := range sink.Recent() {
		if strings.Contains(line, "failed to watch *v1.Pod") {
			found = true
		}
	}
	if !found {
		t.Errorf("klog output never reached the sink; got %#v", sink.Recent())
	}
}

// catchPanic runs fn and returns the panic value formatted as a string,
// failing the test if fn did not panic.
func catchPanic(t *testing.T, fn func()) (recovered string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Error("want a panic, got none")
			return
		}
		recovered, _ = r.(string)
	}()
	fn()
	return ""
}

// A panic on a goroutine Bubble Tea never sees (an informer's, or one of
// kute's own behind utilruntime.HandleCrash) must give the terminal back
// before the footer is printed — otherwise the footer lands on the alt
// screen and the process dies in raw mode — and must still leave a report.
func TestPanicHandlerReleasesTheTerminalBeforeTheFooter(t *testing.T) {
	live := newLiveState(nil, false)
	sink := newTestSink(t, live)
	footer := captureCrashOut(t)

	releasedAt := -1
	previousHandlers := utilruntime.PanicHandlers
	restore := installPanicHandler(sink, func() { releasedAt = footer.Len() })
	reallyCrash := utilruntime.ReallyCrash
	utilruntime.ReallyCrash = false
	defer func() { utilruntime.ReallyCrash = reallyCrash }()

	func() {
		defer utilruntime.HandleCrash()
		panic("boom on a background goroutine")
	}()

	restore()
	if len(utilruntime.PanicHandlers) != len(previousHandlers) {
		t.Errorf("PanicHandlers not restored: %d handlers, want %d", len(utilruntime.PanicHandlers), len(previousHandlers))
	}
	if releasedAt != 0 {
		t.Fatalf("terminal released at footer offset %d, want 0 (before the footer)", releasedAt)
	}
	report := sink.LastReport()
	if report == "" {
		t.Fatal("no crash report written for a background goroutine panic")
	}
	if !strings.Contains(footer.String(), report) {
		t.Errorf("footer does not name the report; got:\n%s", footer.String())
	}
}

// TestCapLogTextBoundsPluginStderr: an exec plugin's stderr reaches the
// stream through ConnState.Err, so it's capped (on a rune boundary) before
// it's logged (14 L6).
func TestCapLogTextBoundsPluginStderr(t *testing.T) {
	if got := capLogText("short"); got != "short" {
		t.Fatalf("capLogText(short) = %q", got)
	}
	got := capLogText(strings.Repeat("é", maxConnErrLog))
	if !strings.HasSuffix(got, "… (truncated)") || len(got) > maxConnErrLog+len("… (truncated)") || !utf8.ValidString(got) {
		t.Fatalf("capLogText didn't cap cleanly: len %d, valid %t", len(got), utf8.ValidString(got))
	}
}
