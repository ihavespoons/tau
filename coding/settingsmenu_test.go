package coding

import (
	"context"
	"strings"
	"testing"
)

func menuRow(t *testing.T, rows []SettingRow, key string) SettingRow {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("%s is not on the menu", key)
	return SettingRow{}
}

// The menu shows what is in effect, which for an untouched setting is tau's
// default rather than a blank.
func TestTheMenuShowsEffectiveValues(t *testing.T) {
	cs := newTestSession(t, Options{})

	rows := cs.SettingsMenu()
	if len(rows) == 0 {
		t.Fatal("the menu is empty")
	}
	if got := menuRow(t, rows, "hideThinkingBlock").Value; got != "off" {
		t.Errorf("hideThinkingBlock = %q, want the default off", got)
	}
	// This one defaults to on, so a menu that assumed false would lie about it.
	if got := menuRow(t, rows, "enableSkillCommands").Value; got != "on" {
		t.Errorf("enableSkillCommands = %q, want the default on", got)
	}
	if got := menuRow(t, rows, "defaultProjectTrust").Value; got != "ask" {
		t.Errorf("defaultProjectTrust = %q, want the default ask", got)
	}
}

func TestTogglingWritesTheOppositeValue(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	row := menuRow(t, cs.SettingsMenu(), "quietStartup")
	if row.Value != "off" {
		t.Fatalf("quietStartup starts at %q", row.Value)
	}
	if _, err := cs.ToggleSetting(ctx, row); err != nil {
		t.Fatal(err)
	}
	if got := menuRow(t, cs.SettingsMenu(), "quietStartup").Value; got != "on" {
		t.Errorf("after toggling, quietStartup = %q, want on", got)
	}

	// And back, which is the case a toggle that only ever wrote true would get
	// wrong.
	row = menuRow(t, cs.SettingsMenu(), "quietStartup")
	if _, err := cs.ToggleSetting(ctx, row); err != nil {
		t.Fatal(err)
	}
	if got := menuRow(t, cs.SettingsMenu(), "quietStartup").Value; got != "off" {
		t.Errorf("after toggling back, quietStartup = %q, want off", got)
	}
}

// "" and "not set" mean different things to a setting with a default, and an
// emptied field is the menu's only way to say the second.
func TestAnEmptiedTextFieldUnsetsTheKey(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	row := menuRow(t, cs.SettingsMenu(), "externalEditor")
	if _, err := cs.ApplySetting(ctx, row, "hx"); err != nil {
		t.Fatal(err)
	}
	if got := menuRow(t, cs.SettingsMenu(), "externalEditor").Value; got != "hx" {
		t.Fatalf("externalEditor = %q, want hx", got)
	}

	row = menuRow(t, cs.SettingsMenu(), "externalEditor")
	if _, err := cs.ApplySetting(ctx, row, "  "); err != nil {
		t.Fatal(err)
	}
	if got, err := cs.SettingsGet("externalEditor"); err != nil || !strings.Contains(got, "not set") {
		t.Errorf("externalEditor = %q (err %v), want it unset", got, err)
	}
}

// A choice row that wrote an empty string would be indistinguishable from an
// unset one, so the empty option has to go through as a real write.
func TestAChoiceCanBeSetToEachOfItsOptions(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	row := menuRow(t, cs.SettingsMenu(), "steeringMode")
	for _, want := range row.Choices {
		if _, err := cs.ApplySetting(ctx, row, want); err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if got := menuRow(t, cs.SettingsMenu(), "steeringMode").Value; got != want {
			t.Errorf("steeringMode = %q, want %q", got, want)
		}
	}
}

// The theme list is what the setting will accept, so "auto" has to be in it —
// it is the default and there is no theme by that name.
func TestTheThemeChoicesLeadWithAuto(t *testing.T) {
	cs := newTestSession(t, Options{})

	row := menuRow(t, cs.SettingsMenu(), "theme")
	if len(row.Choices) < 2 || row.Choices[0] != "auto" {
		t.Errorf("theme choices = %v, want auto first and some themes after", row.Choices)
	}
}

// Nested objects are deliberately absent: a menu that could only edit them as
// pasted JSON would be worse than the typed command.
func TestNestedSettingsAreNotOnTheMenu(t *testing.T) {
	cs := newTestSession(t, Options{})

	for _, r := range cs.SettingsMenu() {
		switch r.Key {
		case "compaction", "retry", "thinkingBudgets", "terminal", "markdown":
			t.Errorf("%s is on the menu but cannot be edited as one value", r.Key)
		}
	}
}
