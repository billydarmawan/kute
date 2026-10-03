package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kute-dev/kute/internal/config"
	"github.com/kute-dev/kute/internal/state"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/update"
)

func TestSessionVersionFallsBackToPlaceholder(t *testing.T) {
	t.Parallel()
	if got := sessionVersion(""); got != tui.Version {
		t.Fatalf("sessionVersion(\"\") = %q, want tui.Version placeholder", got)
	}
	if got := sessionVersion("dev"); got != tui.Version {
		t.Fatalf("sessionVersion(\"dev\") = %q, want tui.Version placeholder", got)
	}
}

func TestSessionVersionUsesRealBuildVersion(t *testing.T) {
	t.Parallel()
	if got := sessionVersion("0.2.0"); got != "0.2.0" {
		t.Fatalf("sessionVersion(\"0.2.0\") = %q, want 0.2.0", got)
	}
}

type fakeChecker struct {
	release   update.Release
	changelog []update.ChangelogEntry
	err       error
	calls     int
}

func (f *fakeChecker) Latest(context.Context) (update.Release, error) {
	f.calls++
	if f.err != nil {
		return update.Release{}, f.err
	}
	return f.release, nil
}

func (f *fakeChecker) Changelog(context.Context, update.Release) ([]update.ChangelogEntry, error) {
	return f.changelog, nil
}

func testAppSession() *tui.Session {
	return &tui.Session{
		Theme:   tui.Dark(),
		Styles:  tui.NewStyles(tui.Dark()),
		Version: "0.2.0",
		State:   state.State{PerContext: map[string]state.PerContext{}},
	}
}

func TestUpdateCheckCmdNilWhenDisabled(t *testing.T) {
	t.Parallel()
	disabled := false
	sess := testAppSession()
	sess.Config = config.Config{Update: config.UpdateConfig{Check: &disabled}}
	checker := &fakeChecker{}

	if cmd := updateCheckCmd(sess, checker, false); cmd != nil {
		t.Fatal("expected nil Cmd when update.check is disabled")
	}
	// Disabled must win even for a forced ('r') recheck.
	if cmd := updateCheckCmd(sess, checker, true); cmd != nil {
		t.Fatal("expected nil Cmd for a forced recheck when update.check is disabled")
	}
}

func TestUpdateCheckCmdNilWhenCacheFresh(t *testing.T) {
	t.Parallel()
	sess := testAppSession()
	sess.State.UpdateCheck.LastChecked = time.Now().Add(-time.Hour)
	checker := &fakeChecker{}

	if cmd := updateCheckCmd(sess, checker, false); cmd != nil {
		t.Fatal("expected nil Cmd within the 24h cache window")
	}
}

func TestUpdateCheckCmdForcedBypassesCache(t *testing.T) {
	t.Parallel()
	sess := testAppSession()
	sess.State.UpdateCheck.LastChecked = time.Now().Add(-time.Hour)
	checker := &fakeChecker{release: update.Release{Version: "0.2.1"}}

	cmd := updateCheckCmd(sess, checker, true)
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd for a forced recheck even within the cache window")
	}
	msg, ok := cmd().(tui.UpdateCheckedMsg)
	if !ok || msg.LatestVersion != "0.2.1" || checker.calls != 1 {
		t.Fatalf("cmd() = %+v (calls=%d), want a resolved 0.2.1 check", msg, checker.calls)
	}
}

func TestUpdateCheckCmdRunsWhenCacheStale(t *testing.T) {
	t.Parallel()
	sess := testAppSession()
	sess.State.UpdateCheck.LastChecked = time.Now().Add(-25 * time.Hour)
	checker := &fakeChecker{release: update.Release{Version: "0.2.1"}}

	cmd := updateCheckCmd(sess, checker, false)
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd once the cache is stale")
	}
}

func TestUpdateCheckCmdRunsWhenNeverChecked(t *testing.T) {
	t.Parallel()
	sess := testAppSession()
	checker := &fakeChecker{release: update.Release{Version: "0.2.1"}}

	cmd := updateCheckCmd(sess, checker, false)
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd when LastChecked is zero (never checked)")
	}
}

// TestUpdateCheckCmdMarksTheCheckInFlight: a handed-out check marks the
// session so 28b's Init doesn't fire a second one alongside it (14 L11b).
func TestUpdateCheckCmdMarksTheCheckInFlight(t *testing.T) {
	t.Parallel()
	sess := testAppSession()
	if updateCheckCmd(sess, &fakeChecker{}, false) == nil || !sess.UpdateChecking {
		t.Fatalf("UpdateChecking = %t after handing out a check, want true", sess.UpdateChecking)
	}
}

// TestUpdateCheckCmdBacksOffAfterARecentAttempt: a failed check never
// advances LastChecked, so the ambient check waits out updateRetryInterval
// after any attempt instead of hitting the feed on every launch (14 L11a);
// 'r' still forces one.
func TestUpdateCheckCmdBacksOffAfterARecentAttempt(t *testing.T) {
	t.Parallel()
	sess := testAppSession()
	sess.State.UpdateCheck.LastAttempt = time.Now().Add(-10 * time.Minute)
	if cmd := updateCheckCmd(sess, &fakeChecker{}, false); cmd != nil {
		t.Fatal("expected no ambient check within an hour of the last attempt")
	}
	if cmd := updateCheckCmd(sess, &fakeChecker{}, true); cmd == nil {
		t.Fatal("expected a forced recheck to ignore the backoff")
	}

	sess = testAppSession()
	sess.State.UpdateCheck.LastAttempt = time.Now().Add(-2 * time.Hour)
	if cmd := updateCheckCmd(sess, &fakeChecker{}, false); cmd == nil {
		t.Fatal("expected a check once the retry interval has passed")
	}
	if time.Since(sess.State.UpdateCheck.LastAttempt) > time.Minute {
		t.Fatalf("LastAttempt = %v, want it stamped when the check was handed out", sess.State.UpdateCheck.LastAttempt)
	}
}

func TestUpdateCheckCmdErrYieldsErrMsg(t *testing.T) {
	t.Parallel()
	sess := testAppSession()
	checker := &fakeChecker{err: errors.New("offline")}

	cmd := updateCheckCmd(sess, checker, false)
	msg, ok := cmd().(tui.UpdateCheckedMsg)
	if !ok || msg.Err == nil {
		t.Fatalf("cmd() = %+v, want UpdateCheckedMsg with Err set", msg)
	}
}

// TestBuildUpdateFactoryProducesAWorkingTask pins the fix for the panel
// hanging on "checking for updates…" forever: a fresh session (nothing
// resolved yet — the common case, since updateCheckCmd's own 24h cache
// means the ambient check is skipped outright on most launches) must have
// tasks/update's Init fetch once via Recheck, bypassing that cache exactly
// like 'r' does — see tasks/update's package doc comment.
func TestBuildUpdateFactoryProducesAWorkingTask(t *testing.T) {
	t.Parallel()
	sess := testAppSession()
	checker := &fakeChecker{release: update.Release{Version: "0.2.1"}}
	task := buildUpdateFactory(sess, checker)()
	if task == nil {
		t.Fatal("expected a non-nil Task")
	}
	task.SetSize(120, 36)

	cmd := task.Init()
	if cmd == nil {
		t.Fatal("expected tasks/update's Init to fetch once when nothing has resolved yet")
	}
	if _, ok := cmd().(tui.UpdateCheckedMsg); !ok {
		t.Fatal("expected Init's Cmd to resolve to an UpdateCheckedMsg")
	}
	if checker.calls != 1 {
		t.Fatalf("expected exactly one Latest() call, got %d", checker.calls)
	}
}
