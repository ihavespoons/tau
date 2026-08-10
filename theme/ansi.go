package theme

import (
	"fmt"
	"math"
	"strings"
)

// basicANSI approximates the 16 system colours, which a terminal is free to
// redefine. Same table Pi uses.
var basicANSI = [16]string{
	"#000000", "#800000", "#008000", "#808000",
	"#000080", "#800080", "#008080", "#c0c0c0",
	"#808080", "#ff0000", "#00ff00", "#ffff00",
	"#0000ff", "#ff00ff", "#00ffff", "#ffffff",
}

// ANSI256ToHex converts a 256-colour palette index to a hex colour: 0-15 are
// the system colours, 16-231 the 6×6×6 cube, 232-255 the grayscale ramp. An
// index outside 0-255 clamps to the nearest end.
func ANSI256ToHex(index int) string {
	switch {
	case index < 0:
		index = 0
	case index > 255:
		index = 255
	}
	if index < 16 {
		return basicANSI[index]
	}
	if index < 232 {
		cube := index - 16
		component := func(n int) int {
			if n == 0 {
				return 0
			}
			return 55 + n*40
		}
		return fmt.Sprintf("#%02x%02x%02x",
			component(cube/36), component((cube%36)/6), component(cube%6))
	}
	gray := 8 + (index-232)*10
	return fmt.Sprintf("#%02x%02x%02x", gray, gray, gray)
}

// RGB is an 8-bit-per-channel colour.
type RGB struct{ R, G, B int }

// ParseHex reads a "#rrggbb" colour. The leading "#" is optional.
func ParseHex(hex string) (RGB, error) {
	cleaned := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(cleaned) != 6 {
		return RGB{}, fmt.Errorf("invalid hex color: %s", hex)
	}
	var c RGB
	if _, err := fmt.Sscanf(strings.ToLower(cleaned), "%02x%02x%02x", &c.R, &c.G, &c.B); err != nil {
		return RGB{}, fmt.Errorf("invalid hex color: %s", hex)
	}
	return c, nil
}

// Luminance returns the sRGB relative luminance of a colour, 0 (black) to 1
// (white).
func (c RGB) Luminance() float64 {
	linear := func(channel int) float64 {
		v := float64(channel) / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(c.R) + 0.7152*linear(c.G) + 0.0722*linear(c.B)
}
