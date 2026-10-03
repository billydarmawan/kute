package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Theme is the semantic color source for every inverted-layout screen (the
// redesign in mvp-plan.md). Every view renders through a Theme value — no hex
// literal ever appears in view code. Both Dark() and Light() populate every
// field; a theme swap is a struct swap, never a per-color
// lipgloss.AdaptiveColor. See docs/design/README.md §Design Tokens for the
// source table.
type Theme struct {
	// Backgrounds
	Bg, BgChrome, BgStrip, BgSidebar, BgLog color.Color

	// BgPalette/BgInput back every floating dialog panel — goto/namespace/
	// context palette, the help overlay, exec/forward pickers — and are the
	// empty lipgloss.Color in both themes (a lipgloss.Color("") resolves to
	// no background escape at all, same as lipgloss.NoColor{}), so dialogs
	// render on the terminal's own background instead of a themed fill.
	// Never give these an actual hex value; add a new token instead if some
	// future dialog surface needs one.
	BgPalette, BgInput color.Color

	// Borders
	Border, BorderSubtle, BorderPalette color.Color

	// Text ramp (contrast descends from Text to TextGhost2; on light themes
	// the ramp still runs bright-to-faint in the same role order — never
	// re-ordered).
	Text, TextPrimary, TextSecondary, TextDim, TextFaint, TextGhost, TextGhost2 color.Color

	// Accent + selection
	Accent, AccentHi, SelBg color.Color

	// AccentMuted is the breadcrumb's de-emphasized c/n/g key-hint color — a
	// step darker/duller than Accent so the key recedes behind the path text
	// it prefixes (docs/design README.md §2a).
	AccentMuted color.Color

	// MarkBg is 20a's bulk-operations marked-row tint — quieter than SelBg,
	// since a marked row's cursor-independent state must read as distinct
	// from (and never overpower) the selection background.
	MarkBg color.Color

	// Status
	Good, Warn, Bad, BadSoft, BadText, BadMuted, Info color.Color

	// Error surfaces
	ErrBannerBg, ErrBannerBorder, ErrCardBg, ErrCardBorder color.Color

	// WarnBanner* backs §36b's run-now overlap card (docs/design README.md
	// §36b/0.8.0 plan Phase 5 theme requirement 1) — the one non-destructive
	// filled-background banner in the app, reserved for a genuine
	// operational warning (an active Job under Forbid/Replace
	// concurrencyPolicy) rather than the ordinary dim keybar text every
	// other informational note uses. Never reused for a destructive confirm
	// — those stay ConfirmBorder/ConfirmHeaderBg's red exclusively (the
	// invariant "red borders are reserved for destructive confirms").
	WarnBannerBg, WarnBannerBorder, WarnBannerText, WarnBannerMuted color.Color

	// Bars, YAML syntax, PROD tag, ALL NS pill
	BarTrack                              color.Color
	YamlKey, YamlStr, YamlPunct, YamlFold color.Color
	ProdBorder, ProdText                  color.Color
	AllNsPillBg, AllNsPillText            color.Color

	// ConfirmBorder/ConfirmHeaderBg/ConfirmPillBg back the destructive-confirm
	// modal only (components/confirmmodal.go, Phase 5). Red borders are
	// reserved exclusively for that surface — never reuse these tokens
	// elsewhere. ConfirmHeaderBg is the modal panel's own fill and, like
	// BgPalette/BgInput above, is the empty color.Color in both themes so
	// the modal renders on the terminal's own background; ConfirmPillBg
	// (the persistent keybar's CONFIRM pill, not part of the floating panel)
	// keeps a real color.
	ConfirmBorder, ConfirmHeaderBg, ConfirmPillBg color.Color

	// Muted is the 4a-offline color ramp. Each theme declares its own muted
	// values (dark: dim; light: wash toward gray) rather than computing them
	// from the live colors above.
	Muted struct {
		Good, Warn, Bad, Info, Text color.Color
	}
}

// Dark is the dark-theme token set (docs/design/README.md §Design Tokens,
// dark column).
func Dark() Theme {
	t := Theme{
		Bg:        lipgloss.Color("#0b0b10"),
		BgChrome:  lipgloss.Color("#0e0e15"),
		BgStrip:   lipgloss.Color("#0c0c12"),
		BgSidebar: lipgloss.Color("#0a0a0f"),
		BgLog:     lipgloss.Color("#08080d"),
		BgPalette: lipgloss.Color(""), // dialogs render on the terminal's own background
		BgInput:   lipgloss.Color(""),

		Border:        lipgloss.Color("#26263a"),
		BorderSubtle:  lipgloss.Color("#1c1c2c"),
		BorderPalette: lipgloss.Color("#3b3b58"),

		Text:          lipgloss.Color("#f0f0fa"),
		TextPrimary:   lipgloss.Color("#d8d8e8"),
		TextSecondary: lipgloss.Color("#9a9ab2"),
		TextDim:       lipgloss.Color("#818199"),
		TextFaint:     lipgloss.Color("#55556e"),
		TextGhost:     lipgloss.Color("#44445c"),
		TextGhost2:    lipgloss.Color("#33334a"),

		Accent:      lipgloss.Color("#a78bfa"),
		AccentHi:    lipgloss.Color("#c4b5fd"),
		AccentMuted: lipgloss.Color("#7c6bb0"),
		SelBg:       lipgloss.Color("#1d1633"),
		MarkBg:      lipgloss.Color("#14101f"),

		Good:     lipgloss.Color("#34d17b"),
		Warn:     lipgloss.Color("#e8c74a"),
		Bad:      lipgloss.Color("#ef6a6a"),
		BadSoft:  lipgloss.Color("#ef8a8a"),
		BadText:  lipgloss.Color("#f0b7b7"),
		BadMuted: lipgloss.Color("#c98a8a"),
		Info:     lipgloss.Color("#6aa8ef"),

		ErrBannerBg:     lipgloss.Color("#2a1518"),
		ErrBannerBorder: lipgloss.Color("#4a2228"),
		ErrCardBg:       lipgloss.Color("#16121a"),
		ErrCardBorder:   lipgloss.Color("#3a2a30"),

		WarnBannerBg:     lipgloss.Color("#16130b"),
		WarnBannerBorder: lipgloss.Color("#4a4224"),
		WarnBannerText:   lipgloss.Color("#f0e0a8"),
		WarnBannerMuted:  lipgloss.Color("#c9b578"),

		BarTrack: lipgloss.Color("#1c1c2c"),

		YamlKey:   lipgloss.Color("#c98fde"),
		YamlStr:   lipgloss.Color("#b8d78f"),
		YamlPunct: lipgloss.Color("#55556e"),
		YamlFold:  lipgloss.Color("#44445c"),

		ProdBorder: lipgloss.Color("#4a2a2a"),
		ProdText:   lipgloss.Color("#ef9a9a"),

		AllNsPillBg:   lipgloss.Color("#12203a"),
		AllNsPillText: lipgloss.Color("#8ab8ef"),

		ConfirmBorder:   lipgloss.Color("#5c2a2a"),
		ConfirmHeaderBg: lipgloss.Color(""), // modal renders on the terminal's own background
		ConfirmPillBg:   lipgloss.Color("#2a1418"),
	}
	t.Muted.Good = lipgloss.Color("#2a7a52")
	t.Muted.Warn = lipgloss.Color("#8a742e")
	t.Muted.Bad = lipgloss.Color("#9e4e4e")
	t.Muted.Info = lipgloss.Color("#3f6a94")
	t.Muted.Text = lipgloss.Color("#656582")
	return t
}

// Light is the light-theme token set (docs/design/README.md §Design Tokens,
// light column). Values are decided equivalents, not naive inversions —
// status colors darken/saturate to hold contrast on light backgrounds.
func Light() Theme {
	t := Theme{
		Bg:        lipgloss.Color("#f7f7fa"),
		BgChrome:  lipgloss.Color("#eef0f4"),
		BgStrip:   lipgloss.Color("#f2f3f7"),
		BgSidebar: lipgloss.Color("#f5f5f9"),
		BgLog:     lipgloss.Color("#fbfbfd"),
		BgPalette: lipgloss.Color(""), // dialogs render on the terminal's own background
		BgInput:   lipgloss.Color(""),

		Border:        lipgloss.Color("#d5d7e0"),
		BorderSubtle:  lipgloss.Color("#e4e6ee"),
		BorderPalette: lipgloss.Color("#b9bcd0"),

		Text:          lipgloss.Color("#14141c"),
		TextPrimary:   lipgloss.Color("#2a2a38"),
		TextSecondary: lipgloss.Color("#565668"),
		TextDim:       lipgloss.Color("#67677c"),
		TextFaint:     lipgloss.Color("#9a9aae"),
		TextGhost:     lipgloss.Color("#b4b4c6"),
		TextGhost2:    lipgloss.Color("#c6c6d4"),

		Accent:      lipgloss.Color("#6b46d9"),
		AccentHi:    lipgloss.Color("#5936b8"),
		AccentMuted: lipgloss.Color("#8a7fac"),
		SelBg:       lipgloss.Color("#ece5fb"),
		MarkBg:      lipgloss.Color("#f5f1fc"),

		Good:     lipgloss.Color("#117844"),
		Warn:     lipgloss.Color("#866208"),
		Bad:      lipgloss.Color("#bc3131"),
		BadSoft:  lipgloss.Color("#c52e2e"),
		BadText:  lipgloss.Color("#a02c2c"),
		BadMuted: lipgloss.Color("#9d5252"),
		Info:     lipgloss.Color("#2767bf"),

		ErrBannerBg:     lipgloss.Color("#fbe9ea"),
		ErrBannerBorder: lipgloss.Color("#e5b7bb"),
		ErrCardBg:       lipgloss.Color("#f6f1f4"),
		ErrCardBorder:   lipgloss.Color("#dcc9cd"),

		WarnBannerBg:     lipgloss.Color("#fbf3e0"),
		WarnBannerBorder: lipgloss.Color("#e8d29a"),
		WarnBannerText:   lipgloss.Color("#7a5a12"),
		WarnBannerMuted:  lipgloss.Color("#826d3d"),

		BarTrack: lipgloss.Color("#e2e4ec"),

		YamlKey:   lipgloss.Color("#8a3fb8"),
		YamlStr:   lipgloss.Color("#48741c"),
		YamlPunct: lipgloss.Color("#9a9aae"),
		YamlFold:  lipgloss.Color("#b4b4c6"),

		ProdBorder: lipgloss.Color("#e3b9b9"),
		ProdText:   lipgloss.Color("#b03030"),

		AllNsPillBg:   lipgloss.Color("#e0ebfa"),
		AllNsPillText: lipgloss.Color("#1f5cad"),

		ConfirmBorder:   lipgloss.Color("#d98a8a"),
		ConfirmHeaderBg: lipgloss.Color(""), // modal renders on the terminal's own background
		ConfirmPillBg:   lipgloss.Color("#f5dede"),
	}
	t.Muted.Good = lipgloss.Color("#4d926d")
	t.Muted.Warn = lipgloss.Color("#998245")
	t.Muted.Bad = lipgloss.Color("#bf6f6f")
	t.Muted.Info = lipgloss.Color("#5d88bd")
	t.Muted.Text = lipgloss.Color("#83839c")
	return t
}
