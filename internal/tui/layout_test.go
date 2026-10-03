package tui

import (
	"testing"
)

func TestNormalizeSize(t *testing.T) {
	t.Parallel()

	got := NormalizeSize(0, -1)
	if got.Width != DefaultWidth || got.Height != DefaultHeight {
		t.Fatalf("NormalizeSize() = %+v, want default %dx%d", got, DefaultWidth, DefaultHeight)
	}
}

func TestBodyHeight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		height int
		want   int
	}{
		{name: "normal", height: 24, want: 22},
		{name: "tiny", height: 1, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := BodyHeight(tt.height); got != tt.want {
				t.Fatalf("BodyHeight(%d) = %d, want %d", tt.height, got, tt.want)
			}
		})
	}
}
