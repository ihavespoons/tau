package export

import (
	"strings"
	"testing"

	"github.com/ihavespoons/tau/theme"
)

func TestParseColor(t *testing.T) {
	cases := []struct {
		in   string
		want theme.RGB
		ok   bool
	}{
		{"#343541", theme.RGB{R: 0x34, G: 0x35, B: 0x41}, true},
		{"#FFFFFF", theme.RGB{R: 255, G: 255, B: 255}, true},
		{"rgb(24, 24, 30)", theme.RGB{R: 24, G: 24, B: 30}, true},
		{"rgb( 1,2 , 3 )", theme.RGB{R: 1, G: 2, B: 3}, true},
		{"#abc", theme.RGB{}, false},
		{"343541", theme.RGB{}, false},
		{"rebeccapurple", theme.RGB{}, false},
		{"hsl(0, 0%, 0%)", theme.RGB{}, false},
		{"", theme.RGB{}, false},
	}
	for _, c := range cases {
		got, ok := parseColor(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseColor(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAdjustBrightness(t *testing.T) {
	if got := adjustBrightness("#646464", 0.5); got != "rgb(50, 50, 50)" {
		t.Errorf("darken = %q", got)
	}
	// Clamped at the top rather than wrapping.
	if got := adjustBrightness("#c8c8c8", 2); got != "rgb(255, 255, 255)" {
		t.Errorf("lighten = %q", got)
	}
	// A colour the arithmetic cannot touch is passed through, not replaced.
	if got := adjustBrightness("rebeccapurple", 0.5); got != "rebeccapurple" {
		t.Errorf("unparseable = %q", got)
	}
}

func TestDeriveExportColors(t *testing.T) {
	// Dark base: page and card are both darkened so the card has an edge.
	dark := deriveExportColors("#343541")
	if dark.pageBg != "rgb(36, 37, 46)" {
		t.Errorf("dark pageBg = %q", dark.pageBg)
	}
	if dark.cardBg != "rgb(44, 45, 55)" {
		t.Errorf("dark cardBg = %q", dark.cardBg)
	}
	if dark.infoBg != "rgb(72, 68, 65)" {
		t.Errorf("dark infoBg = %q", dark.infoBg)
	}

	// Light base: the card keeps the base colour untouched.
	light := deriveExportColors("#f0f0f0")
	if light.cardBg != "#f0f0f0" {
		t.Errorf("light cardBg = %q, want the base unchanged", light.cardBg)
	}
	if light.pageBg != "rgb(230, 230, 230)" {
		t.Errorf("light pageBg = %q", light.pageBg)
	}
	if light.infoBg != "rgb(250, 245, 220)" {
		t.Errorf("light infoBg = %q", light.infoBg)
	}

	// Unparseable base falls back to fixed dark colours rather than emitting
	// something CSS will ignore.
	fallback := deriveExportColors("rebeccapurple")
	if fallback.pageBg != "rgb(24, 24, 30)" || fallback.cardBg != "rgb(30, 30, 36)" || fallback.infoBg != "rgb(60, 55, 40)" {
		t.Errorf("fallback = %+v", fallback)
	}
}

func TestResolveColorsFillsEmptyWithDefaultText(t *testing.T) {
	th, err := theme.Parse([]byte(`{
		"name": "spare",
		"colors": {"text": "", "accent": 33, "border": "#ff0000"}
	}`), "")
	if err == nil {
		t.Fatal("a theme missing required tokens should not parse")
	}
	_ = th

	// A complete theme with an empty and an indexed colour.
	full := buildTheme(t, "spare", map[string]string{"text": "", "accent": "33"})
	byToken := map[theme.Token]string{}
	for _, c := range resolveColors(full) {
		byToken[c.token] = c.value
	}
	if byToken[theme.Text] != "#e5e5e7" {
		t.Errorf("empty text on a dark theme = %q, want the off-white default", byToken[theme.Text])
	}
	if !strings.HasPrefix(byToken[theme.Accent], "#") {
		t.Errorf("palette index was not converted to hex: %q", byToken[theme.Accent])
	}

	// The light theme is the one that defaults to black instead.
	lightNamed := buildTheme(t, "light", map[string]string{"text": ""})
	for _, c := range resolveColors(lightNamed) {
		if c.token == theme.Text && c.value != "#000000" {
			t.Errorf("empty text on the light theme = %q, want black", c.value)
		}
	}
}

func TestThemeExportColorsWinOverDerived(t *testing.T) {
	th, err := theme.Parse(themeJSON(t, "custom", nil,
		`"export": {"pageBg": "#010203", "infoBg": 33},`), "")
	if err != nil {
		t.Fatal(err)
	}
	derived := deriveExportColors(baseColor(resolveColors(th)))
	if got := pick(th, theme.PageBg, derived.pageBg); got != "#010203" {
		t.Errorf("pageBg = %q, want the theme's own value", got)
	}
	// An unset export colour falls back to the derived one.
	if got := pick(th, theme.CardBg, derived.cardBg); got != derived.cardBg {
		t.Errorf("cardBg = %q, want the derived %q", got, derived.cardBg)
	}
	// A palette index becomes hex, because CSS cannot read a terminal index.
	if got := pick(th, theme.InfoBg, derived.infoBg); !strings.HasPrefix(got, "#") {
		t.Errorf("infoBg = %q, want hex", got)
	}
}

func TestThemeVarsCoverEveryToken(t *testing.T) {
	th := darkTheme(t)
	vars := themeVars(resolveColors(th), th, deriveExportColors(baseColor(resolveColors(th))))
	for _, tok := range theme.Tokens {
		if !strings.Contains(vars, "--"+string(tok)+": ") {
			t.Errorf("token %s has no custom property", tok)
		}
	}
	for _, name := range []string{"--exportPageBg", "--exportCardBg", "--exportInfoBg"} {
		if !strings.Contains(vars, name+": ") {
			t.Errorf("%s is missing", name)
		}
	}
}

// buildTheme makes a complete theme, overriding the named tokens.
func buildTheme(t *testing.T, name string, overrides map[string]string) *theme.Theme {
	t.Helper()
	th, err := theme.Parse(themeJSON(t, name, overrides, ""), "")
	if err != nil {
		t.Fatal(err)
	}
	return th
}

// themeJSON writes a theme document with every required token present.
func themeJSON(t *testing.T, name string, overrides map[string]string, extra string) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"name": "` + name + `", ` + extra + `"colors": {`)
	for i, tok := range theme.Tokens {
		if i > 0 {
			b.WriteString(",")
		}
		value := `"#808080"`
		if v, ok := overrides[string(tok)]; ok {
			if v == "" {
				value = `""`
			} else if strings.HasPrefix(v, "#") {
				value = `"` + v + `"`
			} else {
				value = v
			}
		}
		b.WriteString(`"` + string(tok) + `": ` + value)
	}
	b.WriteString("}}")
	return []byte(b.String())
}
