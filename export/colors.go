package export

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/ihavespoons/tau/theme"
)

// resolvedColor is one theme token in the form the stylesheet wants: a CSS
// colour string, never a palette index and never empty.
type resolvedColor struct {
	token theme.Token
	value string
}

// resolveColors renders every theme token as CSS. Palette indices become hex;
// "no colour" becomes the theme's default text colour, because a CSS custom
// property set to the empty string is not a colour and would leave the rule
// that uses it unstyled.
//
// Port of Pi's getResolvedThemeColors: light themes default to black, every
// other theme to its off-white.
func resolveColors(th *theme.Theme) []resolvedColor {
	defaultText := "#e5e5e7"
	if th.Name == "light" {
		defaultText = "#000000"
	}
	out := make([]resolvedColor, 0, len(theme.Tokens))
	for _, tok := range theme.Tokens {
		v := th.Value(tok)
		switch {
		case v.IsIndex:
			out = append(out, resolvedColor{tok, theme.ANSI256ToHex(v.Idx)})
		case v.Str == "":
			out = append(out, resolvedColor{tok, defaultText})
		default:
			out = append(out, resolvedColor{tok, v.Str})
		}
	}
	return out
}

// baseColor is the colour the export backgrounds are derived from. Pi picks
// userMessageBg because it is the one theme colour guaranteed to be a readable
// surface rather than a foreground accent, and falls back to ChatGPT-grey when
// the theme leaves it unset.
func baseColor(colors []resolvedColor) string {
	for _, c := range colors {
		if c.token == theme.UserMessageBg && c.value != "" {
			return c.value
		}
	}
	return "#343541"
}

// themeVars emits the :root custom-property declarations. Every theme token is
// exposed under its own name, then the three export backgrounds: the theme's
// own values where it declares them, the derived ones otherwise.
func themeVars(colors []resolvedColor, th *theme.Theme, derived exportColors) string {
	var b strings.Builder
	for _, c := range colors {
		fmt.Fprintf(&b, "--%s: %s;\n      ", c.token, c.value)
	}
	fmt.Fprintf(&b, "--exportPageBg: %s;\n      ", pick(th, theme.PageBg, derived.pageBg))
	fmt.Fprintf(&b, "--exportCardBg: %s;\n      ", pick(th, theme.CardBg, derived.cardBg))
	fmt.Fprintf(&b, "--exportInfoBg: %s;", pick(th, theme.InfoBg, derived.infoBg))
	return b.String()
}

// pick returns the theme's own export colour, or the derived fallback.
func pick(th *theme.Theme, k theme.ExportKey, derived string) string {
	if v := th.ExportColor(k); v != "" {
		return v
	}
	return derived
}

type exportColors struct {
	pageBg string
	cardBg string
	infoBg string
}

var (
	hexRE = regexp.MustCompile(`^#([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`)
	rgbRE = regexp.MustCompile(`^rgb\s*\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*\)$`)
)

// parseColor reads "#rrggbb" or "rgb(r, g, b)". Anything else — a named colour,
// an hsl(), a palette index that escaped resolution — is not something the
// arithmetic below can work on, so it reports failure and the caller falls back
// to fixed colours.
func parseColor(s string) (theme.RGB, bool) {
	if m := hexRE.FindStringSubmatch(s); m != nil {
		r, _ := strconv.ParseInt(m[1], 16, 32)
		g, _ := strconv.ParseInt(m[2], 16, 32)
		b, _ := strconv.ParseInt(m[3], 16, 32)
		return theme.RGB{R: int(r), G: int(g), B: int(b)}, true
	}
	if m := rgbRE.FindStringSubmatch(s); m != nil {
		r, errR := strconv.Atoi(m[1])
		g, errG := strconv.Atoi(m[2])
		b, errB := strconv.Atoi(m[3])
		if errR != nil || errG != nil || errB != nil {
			return theme.RGB{}, false
		}
		return theme.RGB{R: r, G: g, B: b}, true
	}
	return theme.RGB{}, false
}

// adjustBrightness scales each channel. Factor > 1 lightens, < 1 darkens. An
// unparseable colour is returned untouched rather than replaced, so a theme
// using a CSS colour tau cannot do arithmetic on still renders as itself.
func adjustBrightness(color string, factor float64) string {
	c, ok := parseColor(color)
	if !ok {
		return color
	}
	adjust := func(channel int) int {
		v := int(math.Round(float64(channel) * factor))
		return min(255, max(0, v))
	}
	return fmt.Sprintf("rgb(%d, %d, %d)", adjust(c.R), adjust(c.G), adjust(c.B))
}

// deriveExportColors builds the page, card and info backgrounds from one base
// colour. A light base is darkened slightly for the page and left alone for the
// card; a dark base is darkened harder, because a dark card floating on an
// equally dark page has no edge.
func deriveExportColors(base string) exportColors {
	c, ok := parseColor(base)
	if !ok {
		return exportColors{
			pageBg: "rgb(24, 24, 30)",
			cardBg: "rgb(30, 30, 36)",
			infoBg: "rgb(60, 55, 40)",
		}
	}
	if c.Luminance() > 0.5 {
		return exportColors{
			pageBg: adjustBrightness(base, 0.96),
			cardBg: base,
			infoBg: fmt.Sprintf("rgb(%d, %d, %d)", min(255, c.R+10), min(255, c.G+5), max(0, c.B-20)),
		}
	}
	return exportColors{
		pageBg: adjustBrightness(base, 0.7),
		cardBg: adjustBrightness(base, 0.85),
		infoBg: fmt.Sprintf("rgb(%d, %d, %d)", min(255, c.R+20), min(255, c.G+15), c.B),
	}
}
