package coding

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/settings"
)

func TestSettingsSetReadsTypedValues(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	cases := []struct {
		key, typed, want string
	}{
		// A bare word is a string, which is what makes the command usable
		// without quoting.
		{"theme", "dark", `"dark"`},
		{"quietStartup", "true", `true`},
		{"outputPad", "2", `2`},
		{"npmCommand", `["pnpm","add"]`, `["pnpm","add"]`},
		// Dotted keys reach one level in, and must not replace the object.
		{"compaction.enabled", "false", `false`},
	}
	for _, c := range cases {
		if _, err := cs.SettingsSet(ctx, c.key, c.typed); err != nil {
			t.Fatalf("%s: %v", c.key, err)
		}
		got, err := cs.SettingsGet(c.key)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s = %q, want it to carry %s", c.key, got, c.want)
		}
	}

	// Every value landed in one file, and the nested write did not clobber
	// its siblings.
	raw, err := os.ReadFile(cs.setMgr.Path(settings.Global))
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, raw)
	}
	for _, k := range []string{"theme", "quietStartup", "outputPad", "npmCommand", "compaction"} {
		if _, ok := file[k]; !ok {
			t.Errorf("%s missing from settings.json:\n%s", k, raw)
		}
	}
}

func TestSettingsTakesEffectInThisSession(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	if _, err := cs.SettingsSet(ctx, "quietStartup", "true"); err != nil {
		t.Fatal(err)
	}
	if !cs.Settings.QuietStartup() {
		t.Error("the merged view was not refreshed after the write")
	}
}

func TestSettingsUnsetRemovesTheKey(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	if _, err := cs.SettingsSet(ctx, "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.SettingsUnset(ctx, "theme"); err != nil {
		t.Fatal(err)
	}
	got, err := cs.SettingsGet("theme")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "not set") {
		t.Errorf("theme = %q, want it reported as unset", got)
	}
}

// A key tau does not model is written and preserved, but the caller is told
// nothing will read it — otherwise a typo looks exactly like a success.
func TestSettingsWarnsAboutUnknownKeys(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	out, err := cs.SettingsSet(ctx, "qietStartup", "true")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "does not know this key") {
		t.Errorf("output = %q", out)
	}
	// Written all the same: an unmodelled key may belong to an extension.
	if got, _ := cs.SettingsGet("qietStartup"); !strings.Contains(got, "true") {
		t.Errorf("the key should still have been written, got %q", got)
	}

	if got, _ := cs.SettingsGet("noSuchKey"); !strings.Contains(got, "check the spelling") {
		t.Errorf("reading an unknown key should say so, got %q", got)
	}
}

func TestSettingsListShowsWhatIsConfiguredAndWhere(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	if got := cs.SettingsList(); !strings.Contains(got, "Nothing is configured") {
		t.Errorf("a fresh session should say so:\n%s", got)
	}

	if _, err := cs.SettingsSet(ctx, "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	out := cs.SettingsList()
	if !strings.Contains(out, `theme  "dark"`) {
		t.Errorf("the value should be listed:\n%s", out)
	}
	if !strings.Contains(out, "(global)") {
		t.Errorf("the scope should be listed:\n%s", out)
	}
	if !strings.Contains(out, cs.setMgr.Path(settings.Global)) {
		t.Errorf("the file should be named:\n%s", out)
	}
}

func TestSettingsCommand(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	// A value keeps its spaces: it is one argument that happens to look like
	// several.
	if _, err := cs.RunCommand(ctx, `/settings externalEditor code --wait`); err != nil {
		t.Fatal(err)
	}
	res, err := cs.RunCommand(ctx, "/settings externalEditor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, `"code --wait"`) {
		t.Errorf("output = %q", res.Output)
	}

	if _, err := cs.RunCommand(ctx, "/settings unset externalEditor"); err != nil {
		t.Fatal(err)
	}
	if res, _ := cs.RunCommand(ctx, "/settings externalEditor"); !strings.Contains(res.Output, "not set") {
		t.Errorf("output = %q", res.Output)
	}

	if res, err := cs.RunCommand(ctx, "/settings"); err != nil || res.Output == "" {
		t.Errorf("bare /settings should list: %q %v", res.Output, err)
	}
}

func TestSettingsCompletesKeys(t *testing.T) {
	cs := newTestSession(t, Options{})

	items := cs.Commands.Complete("settings", "default")
	if len(items) == 0 {
		t.Fatal("no completions for a known prefix")
	}
	for _, it := range items {
		if !strings.HasPrefix(it.Value, "default") {
			t.Errorf("completion %q does not match the prefix", it.Value)
		}
	}

	// Completing after "unset" keeps the word, so accepting a suggestion
	// leaves a runnable line.
	items = cs.Commands.Complete("settings", "unset the")
	if len(items) == 0 {
		t.Fatal("no completions after unset")
	}
	if !strings.HasPrefix(items[0].Value, "unset ") {
		t.Errorf("completion = %q, want the unset word kept", items[0].Value)
	}
}
