package diag

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testSnapshot() Snapshot {
	return Snapshot{
		Context:   "prod-eu-1",
		Namespace: "payments",
		Kind:      "Pod",
		Width:     120,
		Height:    36,
		Screen:    "browse.Model",
		Conn:      "connected",
	}
}

// The acceptance criterion for the whole diagnostics path: a crash produces
// a file, and that file names the build, the cluster context, the kind on
// screen and the terminal size.
func TestReportWritesADiagnosableFile(t *testing.T) {
	crashDir := t.TempDir()
	sink, err := Open(Options{
		Build:    Build{Version: "0.3.0", Commit: "abc1234", Date: "2026-07-28"},
		CrashDir: crashDir,
		Snapshot: testSnapshot,
		Now:      fixedNow(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sink.Logf("connection reconnecting: dial tcp: i/o timeout")

	path := sink.Report(Crash{
		Cause: "update",
		Value: "runtime error: index out of range [5] with length 3",
		Stack: []byte("goroutine 1 [running]:\nbrowse.(*Model).Update(...)"),
	})
	if path == "" {
		t.Fatal("Report() = \"\", want a written file")
	}
	if filepath.Dir(path) != crashDir {
		t.Errorf("report written to %s, want it under %s", path, crashDir)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"0.3.0 (abc1234, 2026-07-28)",
		runtime.GOOS + "/" + runtime.GOARCH,
		runtime.Version(),
		"prod-eu-1",
		"payments",
		"Pod",
		"120x36",
		"browse.Model",
		"runtime error: index out of range",
		"browse.(*Model).Update",
		"connection reconnecting: dial tcp: i/o timeout",
		IssueURL,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q; got:\n%s", want, got)
		}
	}
	if sink.LastReport() != path {
		t.Errorf("LastReport() = %q, want %q", sink.LastReport(), path)
	}
}

// A user who ran with --log-file should be able to attach one file, not
// two, so the report goes into the log stream as well as its own file.
func TestReportAlsoAppendsToTheLogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kute.log")
	sink, err := Open(Options{LogFile: logPath, CrashDir: dir, Snapshot: testSnapshot, Now: fixedNow()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sink.Report(Crash{Cause: "view", Value: "boom"})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "kute crash report") || !strings.Contains(got, "boom") {
		t.Errorf("log file has no crash report appended; got:\n%s", got)
	}
	if !strings.Contains(got, logPath) {
		t.Errorf("report does not name the log file it is in; got:\n%s", got)
	}
}

// Bubble Tea catches panics inside a tea.Cmd goroutine before kute sees
// them, so the value is genuinely unavailable. The report still has to be
// written — the context/kind/size in it is most of what a maintainer needs
// — and has to say why the panic section is empty rather than imply the
// run ended cleanly.
func TestReportWithoutAPanicValueSaysSo(t *testing.T) {
	sink, _ := Open(Options{CrashDir: t.TempDir(), Snapshot: testSnapshot, Now: fixedNow()})
	path := sink.Report(Crash{Cause: "unknown"})
	if path == "" {
		t.Fatal("Report() = \"\", want a file even with no panic value")
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.Contains(got, "value unavailable") {
		t.Errorf("report does not explain the missing panic value; got:\n%s", got)
	}
	if !strings.Contains(got, "prod-eu-1") {
		t.Errorf("report dropped the app context; got:\n%s", got)
	}
}

// A report the user can't find is barely better than none, so an
// unwritable crash dir falls back to the temp dir rather than giving up.
func TestReportFallsBackToTempDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sink, _ := Open(Options{CrashDir: filepath.Join(blocker, "kute"), Snapshot: testSnapshot, Now: fixedNow()})
	path := sink.Report(Crash{Cause: "update", Value: "boom"})
	if path == "" {
		t.Fatal("Report() = \"\", want a fallback destination")
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	if filepath.Dir(path) != filepath.Clean(os.TempDir()) {
		t.Errorf("report written to %s, want a file in %s", path, os.TempDir())
	}
}

// TestReportNeverWritesThroughAPlantedSymlink: the report's name is
// predictable, and the temp-dir fallback is shared, so a symlink planted at
// that name must not be followed and its target truncated (14 L5). The
// report lands under a fresh name beside it instead.
func TestReportNeverWritesThroughAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "kute-crash-"+fixedNow()().Format("20060102-150405")+".log")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sink, _ := Open(Options{CrashDir: dir, Snapshot: testSnapshot, Now: fixedNow()})
	path := sink.Report(Crash{Cause: "update", Value: "boom"})
	if got, _ := os.ReadFile(victim); string(got) != "precious" {
		t.Fatalf("report wrote through the symlink: victim now %q", got)
	}
	if filepath.Dir(path) != dir || !strings.HasPrefix(filepath.Base(path), "kute-crash-") {
		t.Fatalf("Report() = %q, want a fresh kute-crash-* file in %s", path, dir)
	}
	if body, _ := os.ReadFile(path); !strings.Contains(string(body), "boom") {
		t.Fatalf("report at %s missing the panic value:\n%s", path, body)
	}
}

func TestFooterNamesTheFactsTheBugFormAsksFor(t *testing.T) {
	sink, _ := Open(Options{
		Build:    Build{Version: "0.3.0", Commit: "abc1234"},
		Snapshot: testSnapshot,
		Now:      fixedNow(),
	})
	footer := sink.Footer("/tmp/kute-crash-20260731-153012.log")

	for _, want := range []string{
		"kute crashed",
		"0.3.0 (abc1234)",
		runtime.GOOS + "/" + runtime.GOARCH,
		"prod-eu-1",
		"Pod",
		"120x36",
		"/tmp/kute-crash-20260731-153012.log",
		IssueURL,
	} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer missing %q; got:\n%s", want, footer)
		}
	}
	// The two fields the footer deliberately drops: a reader can act on
	// neither, and the whole block is meant to paste into the bug form.
	if strings.Contains(footer, runtime.Version()) {
		t.Errorf("footer carries the Go toolchain version; got:\n%s", footer)
	}
	if strings.Contains(footer, "time ") {
		t.Errorf("footer carries a wall clock; got:\n%s", footer)
	}
	// A crash can reach the footer with the terminal still in raw mode,
	// where a bare LF steps down without returning to column 0.
	if strings.Contains(strings.ReplaceAll(footer, "\r\n", ""), "\n") {
		t.Error("footer has a bare LF; every line must end CRLF")
	}
}

// A rendering bug is usually one emulator getting its own capability
// claims wrong, so the emulator's name has to be in the report — TERM and
// COLORTERM only say what it claims.
func TestTerminalFieldNamesTheEmulator(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("TERM_PROGRAM", "WezTerm")

	got := terminalField(Snapshot{Width: 120, Height: 36})
	want := "120x36 TERM=xterm-256color COLORTERM=truecolor TERM_PROGRAM=WezTerm"
	if got != want {
		t.Errorf("terminalField() = %q, want %q", got, want)
	}
}

// Nothing guarantees a terminal sets any of these — an unset one is left
// out rather than reported as empty, which would read as a clnva.
func TestTerminalFieldOmitsUnsetVariables(t *testing.T) {
	for _, name := range terminalEnv {
		t.Setenv(name, "")
	}
	t.Setenv("TERM", "dumb")

	if got := terminalField(Snapshot{Width: 80, Height: 24}); got != "80x24 TERM=dumb" {
		t.Errorf("terminalField() = %q, want just the size and TERM", got)
	}
}

func TestFooterWithoutAReportSaysSo(t *testing.T) {
	sink, _ := Open(Options{Snapshot: testSnapshot, Now: fixedNow()})
	footer := sink.Footer("")
	if !strings.Contains(footer, "No crash report could be written") {
		t.Errorf("footer does not admit the missing file; got:\n%s", footer)
	}
	if !strings.Contains(footer, IssueURL) {
		t.Errorf("footer dropped the issue link; got:\n%s", footer)
	}
}

func TestReportUnknownFieldsReadAsNone(t *testing.T) {
	sink, _ := Open(Options{CrashDir: t.TempDir(), Now: fixedNow()})
	path := sink.Report(Crash{Cause: "update", Value: "boom"})
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.Contains(got, "terminal     unknown") {
		t.Errorf("report does not mark the unknown terminal size; got:\n%s", got)
	}
	if !strings.Contains(got, "context      (none)") {
		t.Errorf("report does not mark the absent context; got:\n%s", got)
	}
	if !strings.Contains(got, "run with --log-file") {
		t.Errorf("report does not point at --log-file for the full stream; got:\n%s", got)
	}
}
