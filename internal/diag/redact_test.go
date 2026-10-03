package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kute-dev/kute/internal/testutil/testenv"
)

// TestRedactScrubsCredentialsAndHome: the stream feeds a crash report the
// bug form asks users to attach publicly, and an exec plugin's stderr
// reaches it verbatim (14 L6).
func TestRedactScrubsCredentialsAndHome(t *testing.T) {
	home := t.TempDir()
	testenv.SetHome(t, home)
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1In0.c2lnbmF0dXJl"
	for _, tc := range []struct{ in, want string }{
		{"Authorization: Bearer abc.def-123", "Authorization: Bearer [redacted]"},
		{"refresh failed: access_token=s3cr3t&expires=1", "refresh failed: access_token=[redacted]&expires=1"},
		{`{"id-token": "s3cr3t", "user": "me"}`, `{"id-token": "[redacted]", "user": "me"}`},
		{"got " + jwt + " back", "got [redacted] back"},
		{"open " + filepath.Join(home, ".kube", "config"), "open " + filepath.Join("~", ".kube", "config")},
		{"connection reconnecting: dial tcp: i/o timeout", "connection reconnecting: dial tcp: i/o timeout"},
	} {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSinkRedactsTheRingAndTheLogFile: both destinations get the scrubbed
// line, and the report's log-file row names the path relative to ~.
func TestSinkRedactsTheRingAndTheLogFile(t *testing.T) {
	home := t.TempDir()
	testenv.SetHome(t, home)
	logPath := filepath.Join(home, "kute.log")
	sink, err := Open(Options{LogFile: logPath, CrashDir: t.TempDir(), Snapshot: testSnapshot, Now: fixedNow()})
	if err != nil {
		t.Fatal(err)
	}
	sink.Logf("connection unauthenticated: token=hunter2")
	_ = sink.Close()

	logged, _ := os.ReadFile(logPath)
	for name, text := range map[string]string{
		"ring":     strings.Join(sink.Recent(), "\n"),
		"log file": string(logged),
		"report":   sink.render(Crash{Cause: "update", Value: "boom"}),
	} {
		if strings.Contains(text, "hunter2") {
			t.Errorf("%s carries the token:\n%s", name, text)
		}
	}
	if report := sink.render(Crash{Cause: "update"}); strings.Contains(report, home) {
		t.Errorf("report carries the home directory:\n%s", report)
	}
}
