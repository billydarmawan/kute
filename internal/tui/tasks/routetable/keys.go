package routetable

import (
	"github.com/kute-dev/kute/internal/tui"
	"github.com/kute-dev/kute/internal/tui/verbs"
)

// Keybar composes the bottom band from verb references only, per the
// registry invariant. Pill is ROUTES for the Ingress/HTTPRoute flavors
// (docs/design README.md §23a), GATEWAY for Gateway's own listener view.
func (m Model) Keybar() tui.Keybar {
	pill, pillText := tui.ModeBrowse, "ROUTES"
	if m.flavor == flavorGateway {
		pillText = "GATEWAY"
	}

	if m.state == tui.TaskStateLoading {
		return tui.Keybar{
			Pill:      pill,
			PillText:  pillText,
			RightNote: "facts & rows enable when data lands",
		}
	}

	groups := [][]tui.KeyHint{}
	if m.rowCount() > 0 {
		switch m.flavor {
		case flavorRoute:
			hints := []tui.KeyHint{}
			if m.parentGatewayName != "" {
				hints = append(hints, verbs.OpenParentGateway.Hint())
			}
			if len(hints) > 0 {
				groups = append(groups, hints)
			}
		case flavorIngress:
			hints := []tui.KeyHint{}
			if row, ok := m.selectedRouteRow(); ok && row.url != "" {
				hints = append(hints, verbs.CopyRouteURL.Hint())
			}
			if len(m.tlsFacts) > 0 {
				if m.tlsFocused {
					hints = append(hints, verbs.OpenTLSSecret.Hint())
				} else {
					hints = append(hints, verbs.FocusTLSStrip.Hint())
				}
			}
			if len(hints) > 0 {
				groups = append(groups, hints)
			}
		}
	}
	// §23a/§23b's "Y copy yaml" — acts on the viewed object itself, so it's
	// offered whenever the seam is wired, independent of rowCount (unlike the
	// row-scoped verbs above). Events (e) is global grammar as of
	// v.0.3.0.dc.html §29a, so it no longer renders here.
	if m.yaml != nil {
		groups = append(groups, []tui.KeyHint{verbs.CopyRouteYAML.Hint()})
	}

	return tui.Keybar{
		Pill:     pill,
		PillText: pillText,
		Groups:   groups,
	}
}

// CapturingInput reports whether routetable has an open free-text input —
// it never does (a read-only routing table, no filter/confirm), so the root
// shell's global shortcuts always reach it as-is.
func (m Model) CapturingInput() bool { return false }
