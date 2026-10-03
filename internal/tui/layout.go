package tui

const (
	DefaultWidth  = 80
	DefaultHeight = 24
	HeaderHeight  = 1
	LegendHeight  = 1
)

type Size struct {
	Width  int
	Height int
}

func NormalizeSize(width, height int) Size {
	if width <= 0 {
		width = DefaultWidth
	}
	if height <= 0 {
		height = DefaultHeight
	}

	return Size{Width: width, Height: height}
}

func BodyHeight(height int) int {
	available := NormalizeSize(DefaultWidth, height).Height - HeaderHeight - LegendHeight
	if available < 1 {
		return 1
	}

	return available
}

func ChromeWidth(width int) int {
	width = NormalizeSize(width, DefaultHeight).Width
	if width < 20 {
		return width
	}
	return width - 2
}
