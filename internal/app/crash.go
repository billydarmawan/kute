package app

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/kute-dev/kute/internal/diag"
	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
)

// ErrCrashed is returned by RunWithConfig when the program died of a panic
// and the crash footer has already been printed. main uses it to exit
// non-zero without printing a second, less useful error line over the
// footer.
var ErrCrashed = errors.New("kute crashed")

// crashTestEnv makes the next key press panic on purpose. It is how the
// crash path is exercised against a real terminal (docs/diagnostics.md) —
// the acceptance test for this whole file is "a deliberately-crashed build
// produces a file a maintainer can diagnose from", and a build flag nobody
// can run is a poor way to keep that true.
const crashTestEnv = "KUTE_CRASH_TEST"

// crashOut is where the crash footer is printed. A var only so tests can
// read it back instead of spraying footers over the test log.
var crashOut io.Writer = os.Stderr

// crashCatcher wraps the root model so a panic in Update or View is
// recorded — with the context, kind and terminal size the user was on —
// before it reaches Bubble Tea's own recover, which restores the terminal
// and prints the stack but knows nothing about kute.
//
// It is also where the live crash Snapshot is maintained: every field in it
// is read here, on the update goroutine, and cached, so the crashing
// goroutine (which may be an informer's) reads a copy rather than racing
// the update loop for Session.Location.
type crashCatcher struct {
	inner     tea.Model
	sink      *diag.Sink
	live      *liveState
	crashTest bool
}

// crashInspector is the part of tui.Model a crash report needs. Declared
// here, in the consuming package, like every other read seam in this tree.
type crashInspector interface {
	Session() *tui.Session
	Screen() string
	Conn() kube.ConnState
}

func newCrashCatcher(inner tea.Model, sink *diag.Sink, live *liveState) crashCatcher {
	c := crashCatcher{
		inner:     inner,
		sink:      sink,
		live:      live,
		crashTest: os.Getenv(crashTestEnv) != "",
	}
	c.live.sync(inner, nil)
	return c
}

func (c crashCatcher) Init() tea.Cmd {
	defer c.catch("init")
	return c.inner.Init()
}

func (c crashCatcher) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	defer c.catch("update")
	if c.crashTest {
		if _, ok := msg.(tea.KeyPressMsg); ok {
			panic(crashTestEnv + " is set: crashing on purpose to exercise the crash report")
		}
	}
	inner, cmd := c.inner.Update(msg)
	c.live.sync(inner, msg)
	c.inner = inner
	return c, cmd
}

func (c crashCatcher) View() tea.View {
	defer c.catch("view")
	return c.inner.View()
}

// catch records the panic and re-panics, deliberately: Bubble Tea's own
// recover is what puts the terminal back into a usable state, and it can
// only do that if the panic keeps travelling. All this frame adds is the
// file.
func (c crashCatcher) catch(where string) {
	r := recover()
	if r == nil {
		return
	}
	c.sink.Report(diag.Crash{Cause: where, Value: r, Stack: debug.Stack()})
	panic(r)
}

// liveState caches the crash Snapshot. Written only from the update
// goroutine (crashCatcher.Update), read from wherever the process happens
// to be dying.
type liveState struct {
	sink *diag.Sink
	demo bool

	mu   sync.Mutex
	snap diag.Snapshot
}

func newLiveState(sink *diag.Sink, demo bool) *liveState {
	return &liveState{sink: sink, demo: demo, snap: diag.Snapshot{Demo: demo}}
}

// Snapshot is diag.Options.Snapshot — the crash reporter's view of where
// the user was.
func (l *liveState) Snapshot() diag.Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snap
}

// sync folds one update's worth of state into the cached Snapshot: the
// terminal size off the resize message (the only place it exists that isn't
// private to tui.Model) and everything else off the model itself.
func (l *liveState) sync(model tea.Model, msg tea.Msg) {
	snap := l.Snapshot()

	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		snap.Width, snap.Height = m.Width, m.Height
	case tui.WindowSizeMsg:
		snap.Width, snap.Height = m.Width, m.Height
	}

	if in, ok := model.(crashInspector); ok {
		snap.Screen = in.Screen()
		if sess := in.Session(); sess != nil {
			snap.Context = sess.Location.Context
			snap.Namespace = sess.Location.Namespace
			snap.Kind = string(sess.Location.Kind)
		}
		conn := in.Conn()
		if phase := string(conn.Phase); phase != "" && phase != snap.Conn {
			// The connection's own history is diagnostic in its own right —
			// half the "kute showed me nothing" reports are an outage — and
			// this is the one place that sees every transition without
			// another goroutine plumbed into the sink.
			snap.Conn = phase
			if conn.Err != "" {
				l.sink.Logf("connection %s: %s", phase, capLogText(conn.Err))
			} else {
				l.sink.Logf("connection %s", phase)
			}
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.snap = snap
}

// installPanicHandler puts the crash reporter in front of client-go's own
// crash handling, which is the only way a panic on a goroutine outside
// Bubble Tea — an informer's, or one of kute's own, each of which defers
// utilruntime.HandleCrash — leaves anything behind. utilruntime re-panics
// after its handlers run, so this cannot keep the process alive; what it
// adds is the file, and the terminal back: Bubble Tea never sees this panic,
// so without release the process died in raw mode on the alt screen, with
// the footer printed into a buffer the user's `reset` then cleared. The
// returned func restores the previous handler list.
func installPanicHandler(sink *diag.Sink, release func()) func() {
	previous := utilruntime.PanicHandlers
	utilruntime.PanicHandlers = append(append([]func(context.Context, any){}, previous...),
		func(_ context.Context, r any) {
			path := sink.Report(diag.Crash{Cause: "goroutine", Value: r, Stack: debug.Stack()})
			release()
			_, _ = io.WriteString(crashOut, sink.Footer(path))
		})
	return func() { utilruntime.PanicHandlers = previous }
}

// terminalOwner hands installPanicHandler the program once it exists — the
// handler is installed first, so a panic during NewModel is still reported.
type terminalOwner struct {
	program atomic.Pointer[tea.Program]
}

func (t *terminalOwner) set(p *tea.Program) { t.program.Store(p) }

// release restores the terminal Bubble Tea took over. Kill is synchronous
// and idempotent (Bubble Tea's shutdown runs once), so two goroutines
// panicking together both return with the terminal already restored.
func (t *terminalOwner) release() {
	if p := t.program.Load(); p != nil {
		p.Kill()
	}
}

// reportProgramCrash turns a Bubble Tea run error into the crash path when
// it was a panic, and leaves every other error alone. A panic Bubble Tea
// caught in a tea.Cmd goroutine never reaches crashCatcher, so it lands
// here with no value and no stack — the report says so rather than
// pretending the run ended cleanly, since a report naming the context,
// kind and terminal size is still most of what a maintainer needs.
func reportProgramCrash(sink *diag.Sink, err error) error {
	if err == nil || !errors.Is(err, tea.ErrProgramPanic) {
		return err
	}
	path := sink.LastReport()
	if path == "" {
		path = sink.Report(diag.Crash{Cause: "unknown"})
	}
	_, _ = io.WriteString(crashOut, sink.Footer(path))
	return ErrCrashed
}

// maxConnErrLog caps a connection error in the diagnostics stream. For an
// exec credential plugin ConnState.Err is the plugin's own stderr, which
// can run long (device-code banners, cloud CLI help text); the head says
// what failed, and the sink redacts anything token-shaped either way.
const maxConnErrLog = 1024

// capLogText truncates s to maxConnErrLog bytes on a rune boundary.
func capLogText(s string) string {
	if len(s) <= maxConnErrLog {
		return s
	}
	cut := maxConnErrLog
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "… (truncated)"
}
