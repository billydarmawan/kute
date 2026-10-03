package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/tui"
	updatetask "github.com/kute-dev/kute/internal/tui/tasks/update"
	"github.com/kute-dev/kute/internal/update"
)

// updateCheckTimeout bounds one release-feed round trip (release metadata +
// changelog.json) so an unreachable/slow GitHub never hangs the ambient
// check or a forced 'r' recheck.
const updateCheckTimeout = 10 * time.Second

// updateCheckInterval is 28a's "one GET against the releases feed per 24h"
// check hygiene (docs/design README.md §28a).
const updateCheckInterval = 24 * time.Hour

// updateRetryInterval is how soon a launch may try again after a check
// that didn't succeed: a failure never advances LastChecked, and without
// this an offline or proxy-blocked user would GET the feed on every launch.
const updateRetryInterval = time.Hour

// githubRepo is kute's own repo — the feed updateCheckCmd polls and the
// same one scripts/release-notes.sh publishes changelog.json against.
const githubRepo = "kute-dev/kute"

// sessionVersion is the "you run X" side of every 28a/28b comparison:
// cfgVersion (main.go's ldflags-injected build version) verbatim, or
// tui.Version's placeholder for an unlinked `go run`/test build (empty, or
// still carrying main.go's own "dev" default).
func sessionVersion(cfgVersion string) string {
	if cfgVersion == "" || cfgVersion == "dev" {
		return tui.Version
	}
	return cfgVersion
}

// buildChecker returns the real update.Checker every ambient/forced check
// goes through — a single construction point so tests can swap it (none do
// yet; internal/update's own tests cover GitHubChecker directly against an
// httptest.Server).
func buildChecker(Config) update.Checker {
	return update.GitHubChecker{Repo: githubRepo}
}

// updateCheckCmd builds the tea.Cmd that performs one release-feed check —
// shared by RunWithConfig's ambient startup check (force=false) and 28b's
// 'r' key (force=true, the panel's Config.Recheck). Returns nil when
// there's nothing to do: update.check is disabled in config (this always
// wins, even for a forced recheck — "kills it entirely", docs/design
// README.md §28a), or (force=false only) the 24h cache is still fresh or
// a check was started within the last hour (updateRetryInterval).
// Network/parse failures return a tui.UpdateCheckedMsg with Err set, which
// the root shell (and tasks/update) both treat as "do nothing" — no chip,
// no error banner, no retry storm. A non-nil Cmd marks the session's
// UpdateChecking until its UpdateCheckedMsg lands.
func updateCheckCmd(sess *tui.Session, checker update.Checker, force bool) tea.Cmd {
	if sess == nil || !sess.Config.UpdateCheckEnabled() {
		return nil
	}
	last := sess.State.UpdateCheck.LastChecked
	if !force && !last.IsZero() && time.Since(last) < updateCheckInterval {
		return nil
	}
	attempt := sess.State.UpdateCheck.LastAttempt
	if !force && !attempt.IsZero() && time.Since(attempt) < updateRetryInterval {
		return nil
	}
	parent := sess.Context() // the process context: a release check outlives any one kube-context
	sess.UpdateChecking = true
	sess.State.UpdateCheck.LastAttempt = time.Now()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, updateCheckTimeout)
		defer cancel()

		release, err := checker.Latest(ctx)
		if err != nil {
			return tui.UpdateCheckedMsg{Err: err}
		}
		// Best-effort: a release with no changelog.json asset (or a
		// transient fetch failure) still lets the chip/header comparison
		// work — only the panel's CHANGELOG list degrades to empty.
		changelog, _ := checker.Changelog(ctx, release)
		return tui.UpdateCheckedMsg{
			Info: tui.UpdateInfo{
				Latest:    release,
				Changelog: changelog,
				Install:   update.DetectInstall(execPathOrEmpty()),
			},
			LatestVersion: release.Version,
			CheckedAt:     time.Now(),
		}
	}
}

// execPathOrEmpty resolves the running binary's real path (through any
// Homebrew-managed symlink) for update.DetectInstall — "" on any error,
// which DetectInstall reads as "not homebrew" (falls back to the curl
// install-script command).
func execPathOrEmpty() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// buildUpdateFactory returns the closure tui.Model.WithUpdatePanel installs
// for 'U'/':update' (28b) — mirrors buildBrowseFactory's shape. Every
// NewModel branch (real cluster, demo, no-cluster/setup) calls this the
// same way, since 28b must work from all three per its own "from anywhere"
// contract — unlike buildSetupFactory/buildBrowseFactory, which only apply
// to a real, not-yet-reachable cluster.
func buildUpdateFactory(sess *tui.Session, checker update.Checker) func() tui.Task {
	return func() tui.Task {
		u := updatetask.New(updatetask.Config{
			Session: sess,
			Recheck: func() tea.Cmd { return updateCheckCmd(sess, checker, true) },
		})
		return &u
	}
}
