package theme

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimal builds a theme file with every required colour set to fill.
func minimal(name string, fill string) map[string]any {
	colors := map[string]any{}
	for _, tok := range Tokens {
		if optional(tok) {
			continue
		}
		colors[string(tok)] = fill
	}
	return map[string]any{"name": name, "colors": colors}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestBuiltinsParse(t *testing.T) {
	for _, name := range BuiltinNames {
		th, ok := Builtin(name)
		if !ok {
			t.Fatalf("built-in %q missing", name)
		}
		if th.Name != name {
			t.Errorf("built-in %q declares name %q", name, th.Name)
		}
		for _, tok := range Tokens {
			if th.Color(tok) == "" {
				t.Errorf("%s: token %q resolved to no colour", name, tok)
			}
		}
	}
}

// The built-ins are the contract every other theme is written against: they
// must carry the full token set with nothing extra, or a theme copied from one
// of them and edited will not load.
func TestBuiltinsCoverExactlyTheTokenSet(t *testing.T) {
	data, err := builtinFS.ReadFile("builtin/dark.json")
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Colors) != len(Tokens) {
		t.Errorf("dark.json declares %d colours, Tokens lists %d", len(f.Colors), len(Tokens))
	}
	for _, tok := range Tokens {
		if _, ok := f.Colors[tok]; !ok {
			t.Errorf("dark.json is missing %q", tok)
		}
	}
}

func TestBuiltinExportColors(t *testing.T) {
	dark, _ := Builtin("dark")
	if got := dark.ExportColor(PageBg); got != "#18181e" {
		t.Errorf("dark pageBg = %q, want #18181e", got)
	}
	light, _ := Builtin("light")
	if got := light.ExportColor(CardBg); got != "#ffffff" {
		t.Errorf("light cardBg = %q, want #ffffff", got)
	}
}

func TestParseResolvesVars(t *testing.T) {
	f := minimal("vars", "base")
	f["vars"] = map[string]any{
		"base":  "alias",
		"alias": "#123456",
		"idx":   200,
	}
	colors := f["colors"].(map[string]any)
	colors["accent"] = "idx"
	colors["dim"] = ""

	th, err := Parse(mustJSON(t, f), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := th.Color(Text); got != "#123456" {
		t.Errorf("text = %q, want #123456 through two hops", got)
	}
	if got := th.Color(Accent); got != "200" {
		t.Errorf("accent = %q, want the palette index 200", got)
	}
	if got := th.Color(Dim); got != "" {
		t.Errorf("dim = %q, want the empty value preserved", got)
	}
}

func TestParseRejectsCircularVars(t *testing.T) {
	f := minimal("loop", "a")
	f["vars"] = map[string]any{"a": "b", "b": "a"}

	_, err := Parse(mustJSON(t, f), "loop.json")
	if err == nil {
		t.Fatal("expected an error for a variable cycle")
	}
	if !strings.Contains(err.Error(), "circular variable reference detected") {
		t.Errorf("error = %v, want a circular-reference message", err)
	}
}

func TestParseRejectsSelfReferencingVar(t *testing.T) {
	f := minimal("self", "a")
	f["vars"] = map[string]any{"a": "a"}

	_, err := Parse(mustJSON(t, f), "")
	if err == nil || !strings.Contains(err.Error(), "circular") {
		t.Fatalf("err = %v, want a circular-reference message", err)
	}
}

func TestParseRejectsUnknownVar(t *testing.T) {
	f := minimal("missing", "nope")

	_, err := Parse(mustJSON(t, f), "")
	if err == nil {
		t.Fatal("expected an error for an undefined variable")
	}
	if !strings.Contains(err.Error(), "variable reference not found: nope") {
		t.Errorf("error = %v, want a not-found message naming the variable", err)
	}
}

func TestParseReportsEveryMissingColour(t *testing.T) {
	f := minimal("holes", "#ffffff")
	colors := f["colors"].(map[string]any)
	delete(colors, "accent")
	delete(colors, "bashMode")

	_, err := Parse(mustJSON(t, f), "holes.json")
	if err == nil {
		t.Fatal("expected an error for missing colours")
	}
	msg := err.Error()
	for _, want := range []string{"holes.json", "accent", "bashMode", "2 colour(s)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestThinkingMaxFallsBackToXhigh(t *testing.T) {
	f := minimal("fallback", "#101010")
	f["colors"].(map[string]any)["thinkingXhigh"] = "#abcdef"

	th, err := Parse(mustJSON(t, f), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := th.Color(ThinkingMax); got != "#abcdef" {
		t.Errorf("thinkingMax = %q, want it inherited from thinkingXhigh", got)
	}
}

func TestThinkingMaxKeptWhenDeclared(t *testing.T) {
	f := minimal("explicit", "#101010")
	f["colors"].(map[string]any)["thinkingXhigh"] = "#abcdef"
	f["colors"].(map[string]any)["thinkingMax"] = "#fedcba"

	th, err := Parse(mustJSON(t, f), "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := th.Color(ThinkingMax); got != "#fedcba" {
		t.Errorf("thinkingMax = %q, want the declared value", got)
	}
}

func TestParseRejectsUnknownColour(t *testing.T) {
	f := minimal("extra", "#101010")
	f["colors"].(map[string]any)["notAToken"] = "#101010"

	_, err := Parse(mustJSON(t, f), "")
	if err == nil || !strings.Contains(err.Error(), "notAToken") {
		t.Fatalf("err = %v, want the unknown token named", err)
	}
}

func TestParseRejectsSlashInName(t *testing.T) {
	_, err := Parse(mustJSON(t, minimal("light/dark", "#101010")), "")
	if err == nil {
		t.Fatal("expected an error for a theme name containing a slash")
	}
	if !strings.Contains(err.Error(), "light/dark") {
		t.Errorf("error = %v, want the offending name quoted", err)
	}
}

func TestParseRejectsNamelessTheme(t *testing.T) {
	f := minimal("", "#101010")
	if _, err := Parse(mustJSON(t, f), ""); err == nil {
		t.Fatal("expected an error for a theme with no name")
	}
}

func TestValueJSON(t *testing.T) {
	cases := []struct {
		in      string
		want    Value
		wantErr bool
	}{
		{in: `"#ff0000"`, want: Str("#ff0000")},
		{in: `""`, want: Str("")},
		{in: `"accent"`, want: Str("accent")},
		{in: `0`, want: Index(0)},
		{in: `255`, want: Index(255)},
		{in: `256`, wantErr: true},
		{in: `-1`, wantErr: true},
		{in: `12.5`, wantErr: true},
		{in: `true`, wantErr: true},
	}
	for _, tc := range cases {
		var got Value
		err := json.Unmarshal([]byte(tc.in), &got)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.in, got, tc.want)
			continue
		}
		out, err := json.Marshal(got)
		if err != nil {
			t.Errorf("%s: marshal: %v", tc.in, err)
			continue
		}
		if string(out) != tc.in {
			t.Errorf("%s: round-tripped to %s", tc.in, out)
		}
	}
}

func TestANSI256ToHex(t *testing.T) {
	cases := map[int]string{
		0:   "#000000",
		15:  "#ffffff",
		16:  "#000000",
		21:  "#0000ff",
		196: "#ff0000",
		231: "#ffffff",
		232: "#080808",
		255: "#eeeeee",
	}
	for index, want := range cases {
		if got := ANSI256ToHex(index); got != want {
			t.Errorf("ANSI256ToHex(%d) = %s, want %s", index, got, want)
		}
	}
}

func TestLuminance(t *testing.T) {
	black, _ := ParseHex("#000000")
	white, _ := ParseHex("#ffffff")
	if black.Luminance() != 0 {
		t.Errorf("black luminance = %v, want 0", black.Luminance())
	}
	if white.Luminance() != 1 {
		t.Errorf("white luminance = %v, want 1", white.Luminance())
	}
	if _, err := ParseHex("#fff"); err == nil {
		t.Error("expected an error for a three-digit hex colour")
	}
}

func writeTheme(t *testing.T, dir, file, name string, fill string) string {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, mustJSON(t, minimal(name, fill)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverOrderAndDedup(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "solarized.json", "solarized", "#002b36")
	// A custom theme may not shadow a built-in.
	writeTheme(t, dir, "dark.json", "dark", "#ff00ff")

	extraDir := t.TempDir()
	writeTheme(t, extraDir, "nord.json", "nord", "#2e3440")
	loose := writeTheme(t, t.TempDir(), "gruvbox.json", "gruvbox", "#282828")

	set := Discover(Options{Dir: dir, Paths: []string{extraDir, loose}})

	got := set.Names()
	want := []string{"dark", "gruvbox", "light", "nord", "solarized"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", got, want)
	}
	if d, _ := set.Get("dark"); d.Color(Accent) == "#ff00ff" {
		t.Error("a custom theme shadowed the built-in dark theme")
	}
	if d, _ := set.Get("dark"); d.Path != "" {
		t.Errorf("built-in dark has path %q, want none", d.Path)
	}
	if g, _ := set.Get("gruvbox"); g.Path != loose {
		t.Errorf("gruvbox path = %q, want %q", g.Path, loose)
	}
	if len(set.Diagnostics()) != 0 {
		t.Errorf("unexpected diagnostics: %+v", set.Diagnostics())
	}
}

func TestDiscoverReportsBadThemes(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(bad, []byte(`{"name":"broken","colors":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTheme(t, dir, "ok.json", "ok", "#111111")

	set := Discover(Options{Dir: dir})

	if _, found := set.Get("broken"); found {
		t.Error("a theme that failed to parse was offered anyway")
	}
	if _, found := set.Get("ok"); !found {
		t.Error("one bad theme suppressed a good one in the same directory")
	}
	diags := set.Diagnostics()
	if len(diags) != 1 || diags[0].Path != bad {
		t.Fatalf("diagnostics = %+v, want one naming %s", diags, bad)
	}
	if !strings.Contains(diags[0].Message, "missing") {
		t.Errorf("diagnostic %q does not say what is wrong", diags[0].Message)
	}
}

func TestDiscoverMissingPathIsADiagnosticNotAFailure(t *testing.T) {
	set := Discover(Options{Dir: filepath.Join(t.TempDir(), "nope"), Paths: []string{"/nonexistent/theme.json"}})
	if len(set.Names()) != len(BuiltinNames) {
		t.Errorf("names = %v, want just the built-ins", set.Names())
	}
	if len(set.Diagnostics()) != 1 {
		t.Errorf("diagnostics = %+v, want one for the missing path", set.Diagnostics())
	}
}

func TestParseAutoSetting(t *testing.T) {
	cases := []struct {
		setting string
		want    AutoSetting
		ok      bool
	}{
		{setting: "light/dark", want: AutoSetting{Light: "light", Dark: "dark"}, ok: true},
		{setting: " solarized / nord ", want: AutoSetting{Light: "solarized", Dark: "nord"}, ok: true},
		{setting: "dark"},
		{setting: ""},
		{setting: "/dark"},
		{setting: "light/"},
		{setting: "a/b/c"},
	}
	for _, tc := range cases {
		got, ok := ParseAutoSetting(tc.setting)
		if ok != tc.ok {
			t.Errorf("ParseAutoSetting(%q) ok = %v, want %v", tc.setting, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("ParseAutoSetting(%q) = %+v, want %+v", tc.setting, got, tc.want)
		}
	}
}

func TestResolveSetting(t *testing.T) {
	cases := []struct {
		setting    string
		background Mode
		want       string
	}{
		{"light/dark", Light, "light"},
		{"light/dark", Dark, "dark"},
		{"nord", Dark, "nord"},
		{"", Dark, ""},
		{"a/b/c", Dark, ""},
		{"/dark", Dark, ""},
	}
	for _, tc := range cases {
		if got := ResolveSetting(tc.setting, tc.background); got != tc.want {
			t.Errorf("ResolveSetting(%q, %q) = %q, want %q", tc.setting, tc.background, got, tc.want)
		}
	}
}

func TestSetResolve(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "nord.json", "nord", "#2e3440")
	set := Discover(Options{Dir: dir})

	if th, ok := set.Resolve("nord", Dark); !ok || th.Name != "nord" {
		t.Errorf("Resolve(nord) = %v, %v", th, ok)
	}
	if th, ok := set.Resolve("", Light); !ok || th.Name != "light" {
		t.Errorf("empty setting should give the built-in matching the background, got %v", th)
	}
	th, ok := set.Resolve("gone", Dark)
	if ok {
		t.Error("Resolve reported success for a theme that is not installed")
	}
	if th == nil || th.Name != "dark" {
		t.Errorf("missing theme fell back to %v, want dark", th)
	}
}

func TestDetectBackground(t *testing.T) {
	cases := []struct {
		colorfgbg string
		want      Mode
		source    string
	}{
		{colorfgbg: "15;0", want: Dark, source: "COLORFGBG"},
		{colorfgbg: "0;15", want: Light, source: "COLORFGBG"},
		{colorfgbg: "0;default;15", want: Light, source: "COLORFGBG"},
		{colorfgbg: "", want: Dark, source: "fallback"},
		{colorfgbg: "nonsense", want: Dark, source: "fallback"},
		{colorfgbg: "15;999", want: Light, source: "COLORFGBG"}, // 999 is out of range, 15 wins
	}
	for _, tc := range cases {
		got := DetectBackground(func(string) string { return tc.colorfgbg })
		if got.Mode != tc.want || got.Source != tc.source {
			t.Errorf("COLORFGBG=%q gave %+v, want %s from %s", tc.colorfgbg, got, tc.want, tc.source)
		}
		if got.Confident != (tc.source == "COLORFGBG") {
			t.Errorf("COLORFGBG=%q confidence = %v", tc.colorfgbg, got.Confident)
		}
	}
}
