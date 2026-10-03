package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/update"
)

// ModeUpdate is 28b's keybar pill mode — default (purple) pillStyle, no new
// case needed there.
const ModeUpdate Mode = "update"

// UpdateInfo is the ambient check's in-memory result (docs/design
// README.md §28a/28b) — richer than what's persisted to state.json
// (state.UpdateCheckState only keeps {lastChecked, latestVersion,
// seenVersions}, exactly enough for 28a's chip to survive a session that
// never re-checks). tasks/update (28b) renders straight from this; nil
// means no check has resolved yet this session (still loading, or
// update.check is disabled).
type UpdateInfo struct {
	Latest    update.Release
	Changelog []update.ChangelogEntry
	Install   update.InstallInfo
}

// UpdateChip reports the ambient header chip's text (28a's "↑ 0.2.1") and
// whether it should render at all — sourced from the persisted
// State.UpdateCheck trio, not Update, so the chip works even in a session
// that never re-checks itself (see UpdateInfo's doc comment). False when no
// update is cached, the cached version isn't actually newer, the user
// has already dismissed it (opened 28b for it, or hit 'x' there — docs/
// design README.md §28a: "never re-nags for a version already seen"), or
// update checks are off (update.check: false or --no-update-check), where
// the cache is inert and a stale chip would advertise a key whose screen
// only says checks are disabled.
func (s *Session) UpdateChip() (version string, ok bool) {
	if s == nil || !s.Config.UpdateCheckEnabled() {
		return "", false
	}
	latest := s.State.UpdateCheck.LatestVersion
	if latest == "" || !update.IsNewer(s.Version, latest) {
		return "", false
	}
	if s.State.UpdateSeen(latest) {
		return "", false
	}
	return latest, true
}

// BuildUpdateChip renders 28a's ambient chip: a quiet yellow "↑ 0.2.1" left
// of the header's connection cluster, or nothing at all — the same "zero
// chrome when inert" contract BuildForwardChip already establishes for
// 13d's chip.
func BuildUpdateChip(theme Theme, session *Session) ConnBadge {
	v, ok := session.UpdateChip()
	if !ok {
		return ConnBadge{}
	}
	return ConnBadge{Text: "↑ " + v, Style: lipgloss.NewStyle().Foreground(theme.Warn)}
}

// UpdateRightHints is 28a's keybar-right addition ("the keybar's right slot
// names the key while live: `U 0.2.1 available — what's new`") — nil when
// BuildUpdateChip would also render nothing, so callers can unconditionally
// prepend it to their own RightHints.
func UpdateRightHints(session *Session) []KeyHint {
	v, ok := session.UpdateChip()
	if !ok {
		return nil
	}
	return []KeyHint{{Key: "U", Label: v + " available — what's new"}}
}
