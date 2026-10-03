package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/kute-dev/kute/internal/kube"
)

func TestDrainImpactLine(t *testing.T) {
	tests := []struct {
		impact kube.DrainImpact
		want   string
	}{
		{kube.DrainImpact{}, "0 evictable"},
		{kube.DrainImpact{Evict: 4}, "4 evictable"},
		{kube.DrainImpact{Evict: 4, Unmanaged: 1}, "4 evictable · 1 unmanaged (will be lost)"},
		{kube.DrainImpact{Evict: 4, EmptyDir: 2}, "4 evictable · 2 emptyDir (data lost)"},
		{kube.DrainImpact{Evict: 4, Unmanaged: 1, EmptyDir: 1}, "4 evictable · 1 unmanaged (will be lost) · 1 emptyDir (data lost)"},
	}
	for _, theme := range []Theme{Dark(), Light()} {
		for _, tt := range tests {
			if got := ansi.Strip(DrainImpactLine(tt.impact, theme)); got != tt.want {
				t.Errorf("DrainImpactLine(%+v) = %q, want %q", tt.impact, got, tt.want)
			}
		}
	}
}
