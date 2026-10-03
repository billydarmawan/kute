package actions

import "github.com/kute-dev/kute/internal/tui/verbs"

// Tier is verbs.Tier, re-exported so a screen reading the active confirm's
// surface (c.Tier() == actions.TierModal) needn't import verbs. Nothing
// outside verbs.Resolve picks one: Begin takes the verb, not a tier.
type Tier = verbs.Tier

// The confirm tiers; see verbs.TierNone/TierInline/TierModal.
const (
	TierNone   = verbs.TierNone
	TierInline = verbs.TierInline
	TierModal  = verbs.TierModal
)
