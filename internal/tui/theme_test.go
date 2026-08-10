package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/ihavespoons/tau/theme"
)

func TestFromThemeUsesLoadedColors(t *testing.T) {
	loaded, ok := theme.Builtin("dark")
	if !ok {
		t.Fatal("built-in dark theme missing")
	}
	got := FromTheme(loaded)

	if got.Name != "dark" {
		t.Errorf("Name = %q, want dark", got.Name)
	}
	if got.Colors != loaded {
		t.Error("Colors does not point at the theme the styles came from")
	}
	if want := loaded.Color(theme.Error); got.Error.GetForeground() != lipgloss.Color(want) {
		t.Errorf("Error foreground = %v, want %s", got.Error.GetForeground(), want)
	}
	if want := loaded.Color(theme.ToolOutput); got.ToolOut.GetForeground() != lipgloss.Color(want) {
		t.Errorf("ToolOut foreground = %v, want %s", got.ToolOut.GetForeground(), want)
	}
	if !got.ToolName.GetBold() {
		t.Error("ToolName should be bold")
	}
	if !got.Thinking.GetItalic() {
		t.Error("Thinking should be italic")
	}
}

func TestFromThemeNilFallsBackToDark(t *testing.T) {
	got := FromTheme(nil)
	if got.Name != "dark" {
		t.Errorf("Name = %q, want the built-in dark theme", got.Name)
	}
}

// A token whose value is the empty string means "no colour", which must leave
// the style unset rather than emitting an escape for colour "".
func TestFromThemeLeavesEmptyColorsUnset(t *testing.T) {
	blank := writeThemeFile(t, t.TempDir(), "blank", func(colors map[string]any) {
		colors["toolOutput"] = ""
	})
	set := theme.Discover(theme.Options{Paths: []string{blank}})
	loaded, ok := set.Get("blank")
	if !ok {
		t.Fatalf("theme did not load: %+v", set.Diagnostics())
	}

	got := FromTheme(loaded)
	if fg := got.ToolOut.GetForeground(); fg != lipgloss.NoColor(struct{}{}) && fg != nil {
		if s, isColor := fg.(lipgloss.Color); !isColor || s != "" {
			t.Errorf("ToolOut foreground = %#v, want unset", fg)
		}
	}
	if rendered := got.ToolOut.Render("x"); rendered != "x" {
		t.Errorf("rendered %q, want an unstyled x", rendered)
	}
}

func TestLoadThemePicksTheConfiguredTheme(t *testing.T) {
	dir := t.TempDir()
	writeThemeFile(t, dir, "acid", func(colors map[string]any) {
		colors["error"] = "#00ff00"
	})

	got, warnings := LoadTheme("acid", theme.Options{Dir: dir})
	if got.Name != "acid" {
		t.Errorf("Name = %q, want acid", got.Name)
	}
	if got.Error.GetForeground() != lipgloss.Color("#00ff00") {
		t.Errorf("error colour = %v, want #00ff00", got.Error.GetForeground())
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestLoadThemeReportsAMissingTheme(t *testing.T) {
	got, warnings := LoadTheme("nope", theme.Options{Dir: t.TempDir()})
	if got.Colors == nil {
		t.Fatal("no palette was built")
	}
	if got.Name != "dark" && got.Name != "light" {
		t.Errorf("fell back to %q, want a built-in", got.Name)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "nope") {
		t.Errorf("warnings = %v, want one naming the missing theme", warnings)
	}
}

func TestLoadThemeReportsABrokenThemeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(path, []byte(`{"name":"broken","colors":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, warnings := LoadTheme("", theme.Options{Dir: dir})
	if len(warnings) != 1 || !strings.Contains(warnings[0], path) {
		t.Errorf("warnings = %v, want one naming %s", warnings, path)
	}
}

func TestDetectBackgroundPrefersColorFgBg(t *testing.T) {
	t.Setenv("COLORFGBG", "0;15")
	defer stubTerminal(t, func() bool {
		t.Error("the terminal was queried even though COLORFGBG answered")
		return true
	})()

	if got := detectBackground(); got != theme.Light {
		t.Errorf("background = %q, want light", got)
	}
}

func TestDetectBackgroundFallsBackToTheTerminal(t *testing.T) {
	t.Setenv("COLORFGBG", "")

	defer stubTerminal(t, func() bool { return false })()
	if got := detectBackground(); got != theme.Light {
		t.Errorf("background = %q, want light from the terminal query", got)
	}

	defer stubTerminal(t, func() bool { return true })()
	if got := detectBackground(); got != theme.Dark {
		t.Errorf("background = %q, want dark from the terminal query", got)
	}
}

func stubTerminal(t *testing.T, fn func() bool) func() {
	t.Helper()
	prev := terminalIsDark
	terminalIsDark = fn
	return func() { terminalIsDark = prev }
}

// writeThemeFile writes a complete theme whose every colour is #101010, with
// mutate applied first so a test can single out the tokens it cares about.
func writeThemeFile(t *testing.T, dir, name string, mutate func(colors map[string]any)) string {
	t.Helper()
	colors := map[string]any{}
	for _, tok := range theme.Tokens {
		colors[string(tok)] = "#101010"
	}
	if mutate != nil {
		mutate(colors)
	}
	data, err := json.Marshal(map[string]any{"name": name, "colors": colors})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
