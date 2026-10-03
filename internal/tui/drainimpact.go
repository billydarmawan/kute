package tui

import (
	"strconv"

	"charm.land/lipgloss/v2"

	"github.com/kute-dev/kute/internal/kube"
)

// DrainImpactLine renders the drain confirm card's consequence line from
// kube.DrainImpact — "4 evictable · 1 unmanaged (will be lost) · 1 emptyDir
// (data lost)" — the one place both drain surfaces (browse's Nodes list and
// 11b node detail) spell it, so they can't drift. The evictable count is
// always shown; each risk segment appears only when non-zero, unmanaged in
// Bad (the pod is gone for good) and emptyDir in Warn (the pod comes back,
// its scratch data doesn't). Every span carries its own style, so the line
// survives being wrapped in ConfirmStyles.Detail and truncated by
// components.ConfirmCard at narrow widths.
func DrainImpactLine(impact kube.DrainImpact, theme Theme) string {
	bg := theme.ConfirmHeaderBg
	plain := lipgloss.NewStyle().Foreground(theme.TextSecondary).Background(bg)
	sep := lipgloss.NewStyle().Foreground(theme.TextFaint).Background(bg).Render(" · ")

	line := plain.Render(strconv.Itoa(impact.Evict) + " evictable")
	if impact.Unmanaged > 0 {
		line += sep + lipgloss.NewStyle().Foreground(theme.Bad).Background(bg).
			Render(strconv.Itoa(impact.Unmanaged)+" unmanaged (will be lost)")
	}
	if impact.EmptyDir > 0 {
		line += sep + lipgloss.NewStyle().Foreground(theme.Warn).Background(bg).
			Render(strconv.Itoa(impact.EmptyDir)+" emptyDir (data lost)")
	}
	return line
}
