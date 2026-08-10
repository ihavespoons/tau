package keybindings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The table is Pi's, whole. A binding that quietly went missing would make a
// Pi config that names it look like a typo, so the count is pinned.
func TestDefinitionsCoverPisTable(t *testing.T) {
	const (
		editor = 21
		input  = 4
		sel    = 6
		app    = 42
	)
	if got, want := len(Definitions), editor+input+sel+app; got != want {
		t.Errorf("table has %d bindings, want %d", got, want)
	}

	counts := map[string]int{}
	seen := map[Binding]bool{}
	for _, d := range Definitions {
		if seen[d.ID] {
			t.Errorf("binding %s appears twice", d.ID)
		}
		seen[d.ID] = true
		if d.Description == "" {
			t.Errorf("binding %s has no description", d.ID)
		}
		switch {
		case strings.HasPrefix(string(d.ID), "tui.editor."):
			counts["editor"]++
		case strings.HasPrefix(string(d.ID), "tui.input."):
			counts["input"]++
		case strings.HasPrefix(string(d.ID), "tui.select."):
			counts["select"]++
		case strings.HasPrefix(string(d.ID), "app."):
			counts["app"]++
		default:
			t.Errorf("binding %s is in no known namespace", d.ID)
		}
	}
	for name, want := range map[string]int{"editor": editor, "input": input, "select": sel, "app": app} {
		if counts[name] != want {
			t.Errorf("%s bindings = %d, want %d", name, counts[name], want)
		}
	}
}

func TestDefaultKeysAreParseable(t *testing.T) {
	for _, d := range Definitions {
		for _, key := range d.DefaultKeys {
			if _, ok := ParseKey(key); !ok {
				t.Errorf("%s default key %q does not parse", d.ID, key)
			}
		}
	}
}

// Pi drops ctrl+z on Windows (no job control) and moves the image paste off
// ctrl+v (the terminal's own paste), and lists alt+arrow first on macOS.
func TestPlatformConditionals(t *testing.T) {
	suspend, _ := Lookup(AppSuspend)
	paste, _ := Lookup(AppClipboardPasteImage)
	fold, _ := Lookup(AppTreeFoldOrUp)

	if runtime.GOOS == "windows" {
		if len(suspend.DefaultKeys) != 0 {
			t.Errorf("suspend = %v, want nothing on Windows", suspend.DefaultKeys)
		}
		if got := paste.DefaultKeys[0]; got != "alt+v" {
			t.Errorf("paste image = %q, want alt+v on Windows", got)
		}
	} else {
		if got := suspend.DefaultKeys[0]; got != "ctrl+z" {
			t.Errorf("suspend = %q, want ctrl+z", got)
		}
		if got := paste.DefaultKeys[0]; got != "ctrl+v" {
			t.Errorf("paste image = %q, want ctrl+v", got)
		}
	}

	want := "ctrl+left"
	if runtime.GOOS == "darwin" {
		want = "alt+left"
	}
	if got := fold.DefaultKeys[0]; got != want {
		t.Errorf("tree fold first key = %q, want %q", got, want)
	}
}

func TestLookup(t *testing.T) {
	d, ok := Lookup(AppModelSelect)
	if !ok {
		t.Fatal("app.model.select is missing")
	}
	if d.Description == "" || len(d.DefaultKeys) == 0 {
		t.Errorf("app.model.select = %+v, want keys and a description", d)
	}
	if _, ok := Lookup("app.nope"); ok {
		t.Error("Lookup invented a binding")
	}
	if !IsDefined("tui.input.submit") || IsDefined("submit") {
		t.Error("IsDefined does not match the namespaced ids exactly")
	}
}

func TestParseKey(t *testing.T) {
	tests := []struct {
		id   string
		want Key
	}{
		{"a", Key{Name: "a"}},
		// A bare capital is a terminal reporting the character shift produced,
		// not a louder spelling of "a": the two are different bytes, and a
		// binding on one must not fire on the other.
		{"A", Key{Name: "a", Shift: true}},
		{"shift+a", Key{Name: "a", Shift: true}},
		{"ctrl+p", Key{Name: "p", Ctrl: true}},
		// With modifiers written out it is a human writing a config, where case
		// is spelling — and a terminal cannot distinguish ctrl+p from ctrl+P
		// anyway, because control codes carry no case.
		{"Ctrl+P", Key{Name: "p", Ctrl: true}},
		{"shift+ctrl+p", Key{Name: "p", Ctrl: true, Shift: true}},
		{"ctrl+shift+p", Key{Name: "p", Ctrl: true, Shift: true}},
		{"alt+left", Key{Name: "left", Alt: true}},
		{"esc", Key{Name: "escape"}},
		{"escape", Key{Name: "escape"}},
		{"pgdown", Key{Name: "pagedown"}},
		{"pageDown", Key{Name: "pagedown"}},
		{"cmd+k", Key{Name: "k", Super: true}},
		{"ctrl+]", Key{Name: "]", Ctrl: true}},
		{"ctrl+-", Key{Name: "-", Ctrl: true}},
		{"+", Key{Name: "+"}},
		{"ctrl++", Key{Name: "+", Ctrl: true}},
	}
	for _, tc := range tests {
		got, ok := ParseKey(tc.id)
		if !ok {
			t.Errorf("ParseKey(%q) failed", tc.id)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseKey(%q) = %+v, want %+v", tc.id, got, tc.want)
		}
	}

	for _, bad := range []string{"", "ctrl", "ctrl+alt", "ctrl+ctrl+a", "hyper+a"} {
		if got, ok := ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) = %+v, want a refusal", bad, got)
		}
	}
}

func TestKeyStringIsCanonical(t *testing.T) {
	k, _ := ParseKey("super+shift+alt+ctrl+esc")
	if got := k.String(); got != "ctrl+alt+shift+super+escape" {
		t.Errorf("String() = %q, want the canonical order", got)
	}
	if (Key{}).String() != "" {
		t.Error("an empty key should render as nothing")
	}
}

func TestSameKey(t *testing.T) {
	if !SameKey("ctrl+shift+p", "shift+ctrl+p") {
		t.Error("modifier order should not matter")
	}
	if !SameKey("esc", "escape") {
		t.Error("aliases should match their canonical name")
	}
	if SameKey("ctrl+p", "ctrl+q") {
		t.Error("different keys matched")
	}
	if SameKey("hyper+a", "hyper+a") {
		t.Error("an unparseable key should match nothing, including itself")
	}
}

func TestConfigPreservesOrderAndRawValues(t *testing.T) {
	cfg := parseConfig(t, `{"app.exit":"ctrl+q","tui.input.submit":["enter","ctrl+m"],"zz.custom":{"weird":true}}`)

	var names []string
	for _, e := range cfg.Entries() {
		names = append(names, e.Name)
	}
	want := []string{"app.exit", "tui.input.submit", "zz.custom"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("entry order = %v, want %v", names, want)
	}

	if keys, ok := cfg.Keys("app.exit"); !ok || len(keys) != 1 || keys[0] != "ctrl+q" {
		t.Errorf("app.exit = %v/%v, want [ctrl+q]", keys, ok)
	}
	if keys, ok := cfg.Keys("tui.input.submit"); !ok || len(keys) != 2 {
		t.Errorf("tui.input.submit = %v, want two keys", keys)
	}
	if _, ok := cfg.Keys("zz.custom"); ok {
		t.Error("an object value should not decode as a key list")
	}

	// A value tau cannot use must still survive a round trip: rewriting the
	// file after a name migration must not delete a line the user typed.
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"zz.custom":{"weird":true}`) {
		t.Errorf("round trip = %s, want the unusable value kept verbatim", out)
	}
}

func TestKeysMarshalShortForm(t *testing.T) {
	one, err := json.Marshal(Keys{"ctrl+p"})
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != `"ctrl+p"` {
		t.Errorf("one key marshalled as %s, want a bare string", one)
	}
	many, err := json.Marshal(Keys{"enter", "ctrl+m"})
	if err != nil {
		t.Fatal(err)
	}
	if string(many) != `["enter","ctrl+m"]` {
		t.Errorf("two keys marshalled as %s, want an array", many)
	}
}

func TestMigrateRenamesLegacyNames(t *testing.T) {
	cfg := parseConfig(t, `{"cycleThinkingLevel":"ctrl+b","submit":"ctrl+m","toggleThinking":"ctrl+y"}`)

	got, migrated := Migrate(cfg)
	if !migrated {
		t.Fatal("migration was not reported")
	}
	for legacy, id := range map[string]Binding{
		"cycleThinkingLevel": AppThinkingCycle,
		"submit":             InputSubmit,
		"toggleThinking":     AppThinkingToggle,
	} {
		if got.Has(legacy) {
			t.Errorf("legacy name %q survived", legacy)
		}
		if !got.Has(string(id)) {
			t.Errorf("%s was not written", id)
		}
	}
}

// A legacy name whose modern id is already set is dropped: the user has moved
// on, and the stale line is the one to lose.
func TestMigrateDropsLegacyWhenNewNameExists(t *testing.T) {
	cfg := parseConfig(t, `{"submit":"ctrl+m","tui.input.submit":"ctrl+enter"}`)

	got, migrated := Migrate(cfg)
	if !migrated {
		t.Fatal("migration was not reported")
	}
	if got.Len() != 1 {
		t.Fatalf("config has %d entries, want 1", got.Len())
	}
	if keys, _ := got.Keys(string(InputSubmit)); keys[0] != "ctrl+enter" {
		t.Errorf("tui.input.submit = %v, want the entry the user already had", keys)
	}
}

func TestMigrateLeavesCurrentNamesAlone(t *testing.T) {
	cfg := parseConfig(t, `{"app.exit":"ctrl+q"}`)
	if _, migrated := Migrate(cfg); migrated {
		t.Error("a config already using current names should not be reported as migrated")
	}
}

func TestMigrateOrdersByTableThenUnknownsAlphabetically(t *testing.T) {
	cfg := parseConfig(t, `{"zeta":"ctrl+z","app.exit":"ctrl+q","alpha":"ctrl+a","tui.input.submit":"ctrl+m"}`)

	got, _ := Migrate(cfg)
	var names []string
	for _, e := range got.Entries() {
		names = append(names, e.Name)
	}
	want := "tui.input.submit,app.exit,alpha,zeta"
	if strings.Join(names, ",") != want {
		t.Errorf("order = %v, want %s", names, want)
	}
}

func TestManagerResolvesOverridesAndDefaults(t *testing.T) {
	m := New(parseConfig(t, `{"app.model.select":["ctrl+m","ctrl+m"]}`))

	if got := m.Keys(AppModelSelect); len(got) != 1 || got[0] != "ctrl+m" {
		t.Errorf("app.model.select = %v, want [ctrl+m] with the duplicate dropped", got)
	}
	if m.Matches("ctrl+l", AppModelSelect) {
		t.Error("the default key still fires after an override")
	}
	if !m.Matches("ctrl+m", AppModelSelect) {
		t.Error("the override does not fire")
	}
	if !m.Matches("shift+tab", AppThinkingCycle) {
		t.Error("an untouched binding lost its default")
	}
}

func TestManagerMatchesIgnoresModifierOrderAndAliases(t *testing.T) {
	m := New(nil)
	if !m.Matches("shift+ctrl+p", AppModelCycleBackward) {
		t.Error("ctrl+shift+p should match the default shift+ctrl+p")
	}
	if !m.Matches("esc", AppInterrupt) {
		t.Error("esc should match the default escape")
	}
	if m.Matches("nonsense", AppInterrupt) {
		t.Error("an unparseable key matched")
	}
}

func TestManagerMatchListsEveryBinding(t *testing.T) {
	got := New(nil).Match("ctrl+c")
	var found []string
	for _, id := range got {
		found = append(found, string(id))
	}
	joined := strings.Join(found, ",")
	for _, want := range []string{"tui.input.copy", "tui.select.cancel", "app.clear"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Match(ctrl+c) = %v, want it to include %s", found, want)
		}
	}
}

// Defaults overlap on purpose across components; only the user's own doubled
// binding is a conflict.
func TestManagerReportsOnlyUserConflicts(t *testing.T) {
	if got := New(nil).Conflicts(); len(got) != 0 {
		t.Errorf("defaults reported conflicts %v, want none", got)
	}

	m := New(parseConfig(t, `{"app.clear":"ctrl+q","app.exit":"Ctrl+Q"}`))
	conflicts := m.Conflicts()
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %v, want one", conflicts)
	}
	if conflicts[0].Key != "ctrl+q" || len(conflicts[0].Bindings) != 2 {
		t.Errorf("conflict = %+v, want ctrl+q claimed by two bindings", conflicts[0])
	}
}

func TestManagerWarnsAboutBadEntries(t *testing.T) {
	m := &Manager{user: parseConfig(t, `{"app.exit":{"nope":1},"app.clear":"hyper+q","made.up":"ctrl+j"}`)}
	warnings := m.rebuild()

	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"app.exit", "hyper+q", "made.up"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings = %v, want one mentioning %s", warnings, want)
		}
	}
	if got := m.Keys(AppExit); len(got) == 0 || got[0] != "ctrl+d" {
		t.Errorf("app.exit = %v, want the default after an unusable value", got)
	}
}

func TestLoadMissingFileIsSilent(t *testing.T) {
	m, warnings := Load(filepath.Join(t.TempDir(), FileName))
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for a missing file", warnings)
	}
	if !m.Matches("escape", AppInterrupt) {
		t.Error("defaults were not applied")
	}
}

func TestLoadBrokenFileWarnsAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, warnings := Load(path)
	if len(warnings) != 1 || !strings.Contains(warnings[0], path) {
		t.Errorf("warnings = %v, want one naming %s", warnings, path)
	}
	if !m.Matches("escape", AppInterrupt) {
		t.Error("defaults were not applied after a broken file")
	}
}

func TestLoadRewritesLegacyNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{"cycleThinkingLevel":"ctrl+b","zz.custom":{"weird":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	m, warnings := Load(path)
	if !m.Matches("ctrl+b", AppThinkingCycle) {
		t.Error("the migrated binding is not in effect")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "cycleThinkingLevel") {
		t.Errorf("file still holds the legacy name:\n%s", text)
	}
	if !strings.Contains(text, string(AppThinkingCycle)) {
		t.Errorf("file does not hold the new name:\n%s", text)
	}
	if !strings.Contains(text, `"weird": true`) {
		t.Errorf("rewrite dropped a value tau does not understand:\n%s", text)
	}
	if !strings.HasSuffix(text, "\n") {
		t.Error("rewrite did not end with a newline")
	}
	if !strings.Contains(joinWarnings(warnings), "zz.custom") {
		t.Errorf("warnings = %v, want one about the unknown binding", warnings)
	}
}

func TestLoadLeavesACurrentFileUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	original := `{"app.exit": "ctrl+q"}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, warnings := Load(path); len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Errorf("file was rewritten to %s, want it left alone", data)
	}
}

func TestEffectiveCoversEveryBinding(t *testing.T) {
	m := New(parseConfig(t, `{"app.exit":"ctrl+q"}`))
	eff := m.Effective()
	if eff.Len() != len(Definitions) {
		t.Errorf("effective config has %d entries, want %d", eff.Len(), len(Definitions))
	}
	if keys, ok := eff.Keys(string(AppExit)); !ok || keys[0] != "ctrl+q" {
		t.Errorf("app.exit = %v, want the override", keys)
	}
	if keys, ok := eff.Keys(string(AppThinkingCycle)); !ok || keys[0] != "shift+tab" {
		t.Errorf("app.thinking.cycle = %v, want the default", keys)
	}
}

func TestReloadPicksUpChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{"app.exit":"ctrl+q"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	m, _ := Load(path)
	if !m.Matches("ctrl+q", AppExit) {
		t.Fatal("the first load did not take")
	}

	if err := os.WriteFile(path, []byte(`{"app.exit":"ctrl+shift+q"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m.Reload()
	if m.Matches("ctrl+q", AppExit) || !m.Matches("ctrl+shift+q", AppExit) {
		t.Errorf("app.exit = %v after reload, want ctrl+shift+q", m.Keys(AppExit))
	}
}

func parseConfig(t *testing.T, raw string) *Config {
	t.Helper()
	cfg := NewConfig()
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func joinWarnings(warnings []string) string { return strings.Join(warnings, "\n") }
