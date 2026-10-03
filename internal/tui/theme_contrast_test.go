package tui

import (
	"fmt"
	"image/color"
	"math"
	"testing"
)

// relativeLuminance is WCAG 2.x relative luminance of an opaque color.
func relativeLuminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	lin := func(v uint32) float64 {
		s := float64(v>>8) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// contrastRatio is the WCAG 2.x contrast ratio between two opaque colors.
func contrastRatio(a, b color.Color) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func hexOf(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

type namedColor struct {
	name string
	c    color.Color
}

// TestThemeContrast pins the contrast floor of every token that carries
// meaning: status, error and body text reach WCAG AA (4.5:1) on every
// surface they're drawn on — the selected (SelBg) and marked (MarkBg) rows
// included — and the deliberately muted 4a offline ramp and the breadcrumb's
// AccentMuted reach 3:1. Both themes.
//
// TextDim is excluded on purpose: whether it should clear 4.5:1 (it doesn't
// in either theme today) is a pending design decision. The decorative tokens
// (TextFaint, TextGhost, TextGhost2, YamlPunct, YamlFold) are exempt by
// design.
func TestThemeContrast(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme Theme
	}{{"dark", Dark()}, {"light", Light()}} {
		th := tc.theme
		surfaces := []namedColor{
			{"Bg", th.Bg}, {"BgChrome", th.BgChrome}, {"BgStrip", th.BgStrip},
			{"BgSidebar", th.BgSidebar}, {"BgLog", th.BgLog},
			{"SelBg", th.SelBg}, {"MarkBg", th.MarkBg},
		}
		check := func(fg namedColor, bgs []namedColor, min float64) {
			t.Helper()
			for _, bg := range bgs {
				if got := contrastRatio(fg.c, bg.c); got < min {
					t.Errorf("%s: %s %s on %s %s = %.2f:1, want >= %.1f:1",
						tc.name, fg.name, hexOf(fg.c), bg.name, hexOf(bg.c), got, min)
				}
			}
		}
		with := func(extra ...namedColor) []namedColor {
			return append(append([]namedColor{}, surfaces...), extra...)
		}
		errBanner := namedColor{"ErrBannerBg", th.ErrBannerBg}
		errCard := namedColor{"ErrCardBg", th.ErrCardBg}
		confirmPill := namedColor{"ConfirmPillBg", th.ConfirmPillBg}

		// Essential text: 4.5:1.
		for _, fg := range []namedColor{
			{"Text", th.Text}, {"TextPrimary", th.TextPrimary}, {"TextSecondary", th.TextSecondary},
			{"Accent", th.Accent}, {"Info", th.Info}, {"BadSoft", th.BadSoft}, {"YamlStr", th.YamlStr},
		} {
			check(fg, surfaces, 4.5)
		}
		check(namedColor{"Good", th.Good}, with(errCard), 4.5)
		check(namedColor{"Warn", th.Warn}, with(errBanner, errCard), 4.5)
		check(namedColor{"Bad", th.Bad}, with(errBanner, errCard, confirmPill), 4.5)
		check(namedColor{"BadText", th.BadText}, with(errBanner), 4.5)
		check(namedColor{"BadMuted", th.BadMuted}, with(errBanner), 4.5)
		check(namedColor{"WarnBannerMuted", th.WarnBannerMuted},
			[]namedColor{{"WarnBannerBg", th.WarnBannerBg}}, 4.5)

		// Deliberately muted: 3:1.
		for _, fg := range []namedColor{
			{"Muted.Good", th.Muted.Good}, {"Muted.Warn", th.Muted.Warn}, {"Muted.Bad", th.Muted.Bad},
			{"Muted.Info", th.Muted.Info}, {"Muted.Text", th.Muted.Text}, {"AccentMuted", th.AccentMuted},
		} {
			check(fg, surfaces, 3.0)
		}
	}
}
