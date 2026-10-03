// Package diag is kute's diagnostics sink: the destination for the
// error/klog stream (--log-file) and the writer of crash reports.
//
// A TUI has nowhere to print. Everything client-go and kute would normally
// log to stderr is discarded, because stderr is the screen (see
// app.configureKlog), which leaves a user who hits a bug with nothing to
// attach to a report. This package is that missing destination: one Sink
// that fans every log line into an optional file *and* a bounded in-memory
// ring, so even a run started without --log-file still has the last few
// hundred lines to put in a crash report.
package diag

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// ringLines is how many recent log lines a crash report carries when the
// run had no --log-file. Enough to hold the reflector/watch errors that
// precede most cluster-side trouble, small enough to never matter.
const ringLines = 300

// Build identifies the running binary — main.go's three ldflags-injected
// vars, verbatim.
type Build struct {
	Version string
	Commit  string
	Date    string
}

func (b Build) String() string {
	version := b.Version
	if version == "" {
		version = "dev"
	}
	switch {
	case b.Commit != "" && b.Date != "":
		return fmt.Sprintf("%s (%s, %s)", version, b.Commit, b.Date)
	case b.Commit != "":
		return fmt.Sprintf("%s (%s)", version, b.Commit)
	default:
		return version
	}
}

// Options configures Open. The zero value is usable: it yields a Sink that
// keeps a ring buffer, writes no file, and reports crashes to the current
// directory.
type Options struct {
	Build Build
	// LogFile is --log-file's path. Empty means no file sink — log lines
	// still fill the ring, and so still reach a crash report.
	LogFile string
	// CrashDir is where crash reports are written. Empty means the process
	// working directory, which is the last resort rather than a default:
	// callers pass the state dir.
	CrashDir string
	// Snapshot returns the live app context (active context, kind, terminal
	// size) for the crash report and footer. Nil yields an empty Snapshot,
	// so a crash still produces a report.
	Snapshot func() Snapshot
	// Now defaults to time.Now. Tests pin it to get a stable report.
	Now func() time.Time
}

// Sink is the diagnostics destination. It is an io.Writer, which is how
// klog is pointed at it; kute's own lines go through Logf. All methods are
// safe to call concurrently, and safe to call on a nil *Sink so callers
// never need a diag check around a log line.
type Sink struct {
	opts Options

	mu      sync.Mutex
	file    *os.File
	ring    []string
	pending string
	// lastReport is the path of the most recent crash report, so run can
	// tell "we already wrote one for this panic" from "the panic happened
	// somewhere we never saw".
	lastReport string
}

// Open builds the Sink. It returns an error only when opts.LogFile was
// given and could not be opened — an explicit --log-file that silently goes
// nowhere is worse than a startup failure. Every other degradation (no
// crash dir, an unwritable one) is handled at report time, where failing is
// not an option.
func Open(opts Options) (*Sink, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Sink{opts: opts}
	if opts.LogFile == "" {
		return s, nil
	}
	if dir := filepath.Dir(opts.LogFile); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create log directory %s: %w", dir, err)
		}
	}
	// 0600: the stream carries context, cluster and namespace names.
	f, err := os.OpenFile(opts.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", opts.LogFile, err)
	}
	s.file = f
	return s, nil
}

// Path returns the log file's path, or "" when the run has no --log-file.
func (s *Sink) Path() string {
	if s == nil {
		return ""
	}
	return s.opts.LogFile
}

// Write makes the Sink an io.Writer so klog can be pointed straight at it
// (klog.SetOutput). Lines fan into the file, when there is one, and always
// into the ring.
func (s *Sink) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	// Both destinations get the redacted text (see Redact): the log file is
	// as likely to be attached to a public issue as the crash report is.
	clean := Redact(string(p))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(clean)
	if s.file != nil {
		// A failed write must not propagate into client-go's logging path,
		// where it would surface as a second error about the first one.
		_, _ = io.WriteString(s.file, clean)
	}
	return len(p), nil
}

// Logf writes one of kute's own diagnostic lines, timestamped the way klog
// lines already are so the two interleave legibly.
func (s *Sink) Logf(format string, args ...any) {
	if s == nil {
		return
	}
	line := s.opts.Now().Format("0102 15:04:05.000") + " kute " + fmt.Sprintf(format, args...)
	_, _ = s.Write([]byte(strings.TrimRight(line, "\n") + "\n"))
}

// Close flushes and closes the file sink. The ring stays readable, so a
// crash during shutdown still reports.
func (s *Sink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Recent returns the buffered log lines, oldest first.
func (s *Sink) Recent() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ring)
}

// LastReport returns the path of the most recently written crash report,
// or "" if this process has not crashed.
func (s *Sink) LastReport() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReport
}

// recordLocked appends chunk to the ring, splitting it into lines and
// holding an unterminated tail back until the write that completes it —
// klog writes a line per call, but nothing guarantees it.
func (s *Sink) recordLocked(chunk string) {
	chunk = s.pending + chunk
	s.pending = ""
	for {
		i := strings.IndexByte(chunk, '\n')
		if i < 0 {
			s.pending = chunk
			return
		}
		s.appendLineLocked(strings.TrimRight(chunk[:i], "\r"))
		chunk = chunk[i+1:]
	}
}

func (s *Sink) appendLineLocked(line string) {
	if line == "" {
		return
	}
	s.ring = append(s.ring, line)
	if len(s.ring) > ringLines {
		s.ring = slices.Delete(s.ring, 0, len(s.ring)-ringLines)
	}
}

// writer exposes the file sink for the report writer, which appends the
// whole crash report to the log file too so a user who has --log-file has
// everything in one place.
func (s *Sink) writer() io.Writer {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	return s.file
}
