package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/testutil/testenv"
	"github.com/kute-dev/kute/internal/tui"
)

// TestLogConfigLoadRecordsBrokenConfig: a config.yaml that failed to parse
// reaches the diagnostics stream with the parser's reason, so a crash
// report or --log-file says why every context turned PROD.
func TestLogConfigLoadRecordsBrokenConfig(t *testing.T) {
	testenv.SetHome(t, t.TempDir())
	if err := os.MkdirAll(filepath.Dir(config.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte("prodContexts: [unterminated"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := newTestSink(t, newLiveState(nil, false))

	logConfigLoad(sink, &tui.Session{Config: config.Load()})

	recent := strings.Join(sink.Recent(), "\n")
	if !strings.Contains(recent, filepath.Join("~", ".config", "kute", "config.yaml")) || !strings.Contains(recent, "treating every context as PROD") {
		t.Fatalf("expected the config parse error in the diag stream, got:\n%s", recent)
	}
}

func TestLogConfigLoadQuietForCleanConfig(t *testing.T) {
	sink := newTestSink(t, newLiveState(nil, false))
	logConfigLoad(sink, &tui.Session{})
	if got := sink.Recent(); len(got) != 0 {
		t.Fatalf("expected nothing logged for a clean config, got %q", got)
	}
}

// TestSaveSessionStateSkipsTheCrashedLocation: after a panic the location
// the user was on is not persisted, so a screen that crashes can't be
// restored into on every launch after it (14 L2). A clean exit still saves it.
func TestSaveSessionStateSkipsTheCrashedLocation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runErr   error
		wantKind string
	}{
		{"clean exit", nil, "Pod"},
		{"panic", fmt.Errorf("%w: boom", tea.ErrProgramPanic), "Service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testenv.SetHome(t, t.TempDir())
			sess := &tui.Session{
				Location: tui.Location{Context: "dev", Kind: "Pod"},
				State:    state.State{PerContext: map[string]state.PerContext{"dev": {Kind: "Service"}}},
			}
			saveSessionState(newTestSink(t, newLiveState(nil, false)), sess, tc.runErr)
			if got := state.Load().PerContext["dev"].Kind; got != tc.wantKind {
				t.Fatalf("persisted kind = %q, want %q", got, tc.wantKind)
			}
		})
	}
}
