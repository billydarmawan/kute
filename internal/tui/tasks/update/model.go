// Package update is 28b (docs/design/README.md §28b): the what's-new panel
// — changelog plus the exact upgrade command, reachable via the root
// shell's 'U' key or the goto palette's synthetic ":update" item from
// anywhere in the app.
//
// It renders straight from *tui.Session.Update, populated once by the
// app-level ambient release-feed check (one GET per 24h, per docs/design
// README.md §28a's "check hygiene"). That 24h throttle lives entirely in
// app.updateCheckCmd and is keyed off the *persisted*
// Session.State.UpdateCheck.LastChecked, so on most launches (any second-
// or-later one within a day) the ambient check is skipped outright —
// Session.Update stays nil for the whole process, not just "for now".
// Init bypasses that throttle exactly once per open, the same "force" path
// 'r' uses (Config.Recheck), whenever there's nothing in memory yet to
// render — otherwise the panel would render "checking for updates…"
// forever with no check ever actually running to resolve it. Recheck is
// still the only *manual* escape hatch, wired to 'r' in the empty state
// ("the only place a manual check exists" per §28b) — Init's own fetch is
// silent, same UI as if the ambient check just hadn't finished yet.
package update

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kute-dev/kute/internal/kube"
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/update"
)

// Config are update's dependencies, per repo convention (package-local
// Config struct, New fills zero values).
type Config struct {
	Session *tui.Session
	// Recheck triggers a fresh release-feed check that bypasses 28a's 24h
	// cache — built at the composition root, wrapping the same
	// update.Checker the ambient startup check uses. Its result flows back
	// as a tui.UpdateCheckedMsg (handled by the root shell first, then
	// forwarded here — see model.go's doc comment on that message).
	Recheck func() tea.Cmd
	// OpenBrowser is 'o's release-notes seam — defaults to the real
	// update.OpenBrowser (an OS process spawn) when nil, so tests can
	// inject a no-op fake instead of actually launching a browser. Never
	// leave this nil in a test that presses 'o'.
	OpenBrowser func(url string) error
}

type Model struct {
	width, height int

	session     *tui.Session
	recheck     func() tea.Cmd
	openBrowser func(url string) error

	// checking is true while an 'r'-forced recheck is in flight — cleared
	// on the next UpdateCheckedMsg, regardless of outcome.
	checking bool
	// feedback is the keybar's transient RightNote after y/o/x/r — "command
	// copied", "opened in browser", "0.2.1 skipped", "checking…", or a
	// recheck failure note.
	feedback string

	// conn is the last kube.ConnStateMsg forwarded by the root shell — 28b
	// deliberately doesn't render a Conn badge in its own header (matches
	// the mockup; see Header()), but SetSize/Update still track it like
	// every other screen for consistency, in case that ever changes.
	conn kube.ConnState

	// nowAt is captured once at construction (New), never read live inside
	// Body/Header — Render functions stay pure per CLAUDE.md's "no clock
	// reads in render paths" invariant, the same reasoning events' own
	// fetchedAt field documents.
	nowAt time.Time
}

func New(cfg Config) Model {
	openBrowser := cfg.OpenBrowser
	if openBrowser == nil {
		openBrowser = update.OpenBrowser
	}
	return Model{
		width:       tui.DefaultWidth,
		height:      tui.DefaultHeight,
		session:     cfg.Session,
		recheck:     cfg.Recheck,
		openBrowser: openBrowser,
		nowAt:       time.Now(),
	}
}

func (m Model) now() time.Time { return m.nowAt }

// Init fires exactly one bypass-the-24h-cache check (the same Recheck 'r'
// uses) when the panel opens with nothing resolved yet this session — see
// the package doc comment for why that's necessary, not just belt-and-
// suspenders. A no-op (nil Cmd) once info/checkErr already hold a result,
// when update.check is disabled (checkDisabled), or when Config.Recheck
// itself was never wired — all three are exactly the states state() also
// treats as "nothing further to wait for", so this never fires a check
// state() would then still report as stuck loading.
func (m Model) Init() tea.Cmd {
	if _, ok := m.info(); ok {
		return nil
	}
	if m.checkErr() != nil || m.checkDisabled() || m.recheck == nil {
		return nil
	}
	if m.session != nil && m.session.UpdateChecking {
		// The ambient check is still in flight: its UpdateCheckedMsg
		// reaches this panel too, so a second GET would only race it.
		return nil
	}
	return m.recheck()
}

func (m *Model) SetSize(width, height int) {
	size := tui.NormalizeSize(width, height)
	m.width, m.height = size.Width, size.Height
}

// currentVersion is the "you run X" side of every comparison this screen
// renders.
func (m Model) currentVersion() string {
	if m.session == nil {
		return ""
	}
	return m.session.Version
}

// info is m.session.Update, or the zero value when no check has resolved
// yet this session (still loading, or update.check is disabled and no
// Recheck has ever succeeded).
func (m Model) info() (tui.UpdateInfo, bool) {
	if m.session == nil || m.session.Update == nil {
		return tui.UpdateInfo{}, false
	}
	return *m.session.Update, true
}

// available reports whether info holds a genuinely newer release than
// currentVersion — the "ready" vs "empty" state split every other method
// here reads off.
func (m Model) available() (update.Release, bool) {
	info, ok := m.info()
	if !ok || !update.IsNewer(m.currentVersion(), info.Latest.Version) {
		return update.Release{}, false
	}
	return info.Latest, true
}

// checkErr is the most recently resolved check's error (nil on success, and
// nil before any check has resolved) — see Session.UpdateCheckErr's doc
// comment for why this can't just be inferred from info() returning false.
func (m Model) checkErr() error {
	if m.session == nil {
		return nil
	}
	return m.session.UpdateCheckErr
}

// checkDisabled reports whether update.check is turned off in config
// (docs/design README.md §28a) — Init must not fire a check that
// updateCheckCmd would just discard anyway, and 'r' must not either (a
// discarded Cmd is nil, so setting m.checking=true first would strand the
// panel exactly like the bug this file exists to avoid).
func (m Model) checkDisabled() bool {
	return m.session == nil || !m.session.Config.UpdateCheckEnabled()
}

// state is this screen's TaskState: loading while a check (ambient, Init's
// own bypass fetch, or an 'r'-forced one) is genuinely in flight, ready
// when an update is available, empty otherwise — current (a resolved check
// with nothing newer), the most recent check failed, or checks are
// disabled. All three empty cases share one property Loading doesn't:
// nothing is going to change on its own, so 'r' (where it applies) is the
// only way out.
func (m Model) state() tui.TaskState {
	if m.checking {
		return tui.TaskStateLoading
	}
	if _, ok := m.info(); !ok {
		if m.checkErr() != nil || m.checkDisabled() {
			return tui.TaskStateEmpty
		}
		return tui.TaskStateLoading
	}
	if _, ok := m.available(); ok {
		return tui.TaskStateReady
	}
	return tui.TaskStateEmpty
}
