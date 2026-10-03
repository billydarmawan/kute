package diag

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// IssueURL is the bug-report form the crash footer points at. The form
// asks for exactly the fields the report and footer print
// (.github/ISSUE_TEMPLATE/bug_report.yml) — keep the two in step.
const IssueURL = "https://github.com/kute-dev/kute/issues/new?template=bug_report.yml"

// Snapshot is the app context a crash report needs and the process itself
// cannot derive: which context and kind the user was on, and how big the
// terminal was. Built by the composition root (app.crashCatcher), which
// tracks it off the update loop so reading it from a crashing goroutine is
// not a race.
type Snapshot struct {
	Context   string
	Namespace string
	Kind      string
	Width     int
	Height    int
	// Screen is the active task's package-level type name ("browse.Model"),
	// the one fact that says which screen the user was actually looking at.
	Screen string
	// Conn is the connection phase at crash time ("connected",
	// "reconnecting", …). Empty when nothing has reported one yet.
	Conn string
	// Demo records whether this was a --demo run, which changes what a
	// maintainer should reproduce against.
	Demo bool
}

// Crash is one panic, as handed to Report.
type Crash struct {
	// Cause names where the panic was caught: "update", "view",
	// "goroutine" (an informer's or kute's own), or "unknown" for a panic Bubble Tea swallowed before kute
	// could see the value.
	Cause string
	// Value is the recovered panic value. Nil when unavailable (the
	// "unknown" cause), which is exactly why Cause exists.
	Value any
	// Stack is debug.Stack() from the recovering frame. May be nil.
	Stack []byte
}

// Report writes a crash report and returns its path, or "" if no file
// could be written anywhere. It never returns an error and never panics:
// it runs while the process is already dying, so every step degrades to
// the next-best destination rather than failing.
//
// When the run has a --log-file, the same report is appended there too, so
// one attachment carries both the crash and the log that led to it.
func (s *Sink) Report(c Crash) string {
	if s == nil {
		return ""
	}
	body := s.render(c)

	if w := s.writer(); w != nil {
		_, _ = w.Write([]byte("\n" + body))
	}

	path := s.writeReportFile(body)
	s.mu.Lock()
	s.lastReport = path
	s.mu.Unlock()
	return path
}

// writeReportFile puts body in its own timestamped file under CrashDir,
// falling back to the OS temp dir when that directory can't be created —
// a crash report the user can't find is barely better than none.
func (s *Sink) writeReportFile(body string) string {
	stamp := "kute-crash-" + s.opts.Now().Format("20060102-150405")
	dirs := []string{s.opts.CrashDir, os.TempDir()}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			continue
		}
		f, err := createReportFile(dir, stamp)
		if err != nil {
			continue
		}
		_, werr := f.WriteString(body)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(f.Name())
			continue
		}
		return f.Name()
	}
	return ""
}

// createReportFile creates <stamp>.log in dir exclusively (O_EXCL, 0600 —
// for the same reason the log file is: context, cluster and namespace
// names). Never opening an existing path matters on the shared temp-dir
// fallback, where another user could have planted a symlink at that
// predictable name; when it is taken (that, or a second crash in the same
// second) a randomised <stamp>-*.log is created instead.
func createReportFile(dir, stamp string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, stamp+".log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return os.CreateTemp(dir, stamp+"-*.log")
	}
	return f, err
}

func (s *Sink) render(c Crash) string {
	snap := Snapshot{}
	if s.opts.Snapshot != nil {
		snap = s.opts.Snapshot()
	}
	var b strings.Builder
	b.WriteString("kute crash report\n")
	b.WriteString("=================\n\n")

	for _, f := range s.fields(snap) {
		fmt.Fprintf(&b, "%-12s %s\n", f.label, f.value)
	}

	b.WriteString("\npanic\n-----\n")
	switch {
	case c.Value != nil:
		fmt.Fprintf(&b, "%v\n", c.Value)
	default:
		b.WriteString("(value unavailable — the panic was caught below kute; " +
			"the stack trace was printed to the terminal)\n")
	}
	if c.Cause != "" {
		fmt.Fprintf(&b, "caught in: %s\n", c.Cause)
	}

	if len(c.Stack) > 0 {
		b.WriteString("\nstack\n-----\n")
		b.Write(c.Stack)
		if c.Stack[len(c.Stack)-1] != '\n' {
			b.WriteByte('\n')
		}
	}

	b.WriteString("\nrecent log\n----------\n")
	recent := s.Recent()
	if len(recent) == 0 {
		b.WriteString("(none — run with --log-file to capture the full stream)\n")
	} else {
		for _, line := range recent {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	b.WriteString("\nPlease attach this file to a bug report:\n")
	b.WriteString(IssueURL + "\n")
	return b.String()
}

type field struct{ label, value string }

// fields is the shared fact list behind both the report header and the
// terminal footer, so the two can never disagree about what a crash was.
func (s *Sink) fields(snap Snapshot) []field {
	fields := []field{
		{"time", s.opts.Now().Format(time.RFC3339)},
		{"version", s.opts.Build.String()},
		// Platform is its own field rather than a tail on the Go version
		// because the footer carries it and the Go version stays behind:
		// "does this reproduce on Windows" is the first question a
		// terminal-rendering bug raises, and the toolchain that built a
		// release binary is pinned anyway.
		{"platform", runtime.GOOS + "/" + runtime.GOARCH},
		{"go", runtime.Version()},
		{"terminal", terminalField(snap)},
		{"context", orNone(snap.Context)},
		{"namespace", orNone(snap.Namespace)},
		{"kind", orNone(snap.Kind)},
	}
	if snap.Screen != "" {
		fields = append(fields, field{"screen", snap.Screen})
	}
	if snap.Conn != "" {
		fields = append(fields, field{"connection", snap.Conn})
	}
	if snap.Demo {
		fields = append(fields, field{"mode", "demo (--demo, fake cluster)"})
	}
	if s.opts.LogFile != "" {
		fields = append(fields, field{"log file", tildeHome(s.opts.LogFile)})
	}
	return fields
}

// terminalEnv is what the crash report says about the terminal itself, in
// the order a reader wants it. TERM_PROGRAM names the emulator — WezTerm,
// iTerm.app, vscode, Windows Terminal — which for a rendering bug is often
// worth more than the platform: TERM and COLORTERM describe capabilities
// the emulator *claims*, and the bugs land where a specific one gets those
// claims wrong.
var terminalEnv = []string{"TERM", "COLORTERM", "TERM_PROGRAM"}

func terminalField(snap Snapshot) string {
	size := "unknown"
	if snap.Width > 0 && snap.Height > 0 {
		size = strconv.Itoa(snap.Width) + "x" + strconv.Itoa(snap.Height)
	}
	parts := []string{size}
	for _, name := range terminalEnv {
		if v := os.Getenv(name); v != "" {
			parts = append(parts, name+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// Footer is what the user sees on the restored terminal after a crash: the
// facts the bug-report form asks for, and where the file is. It drops the
// two fields a reader can't act on — the wall clock (the report's filename
// carries it, and they just watched it happen) and the Go toolchain — and
// keeps everything else, so the whole block pastes into the form. Lines end
// CRLF because a crash can reach this with the terminal still in raw mode,
// where a bare LF steps down without returning to column 0 — the same
// reason Bubble Tea's own panic path does it.
func (s *Sink) Footer(reportPath string) string {
	if s == nil {
		return ""
	}
	snap := Snapshot{}
	if s.opts.Snapshot != nil {
		snap = s.opts.Snapshot()
	}
	var b strings.Builder
	b.WriteString("kute crashed. This is a bug.\n\n")
	for _, f := range s.fields(snap) {
		if f.label == "time" || f.label == "go" {
			continue
		}
		fmt.Fprintf(&b, "  %-11s %s\n", f.label, f.value)
	}
	if reportPath != "" {
		fmt.Fprintf(&b, "  %-11s %s\n", "report", reportPath)
		b.WriteString("\nPlease open an issue and attach that file:\n")
	} else {
		b.WriteString("\nNo crash report could be written. Please open an issue with the details above:\n")
	}
	b.WriteString(IssueURL + "\n")
	return strings.ReplaceAll(b.String(), "\n", "\r\n")
}
