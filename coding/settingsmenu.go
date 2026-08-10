package coding

import (
	"context"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/settings"
	"github.com/ihavespoons/tau/theme"
)

// SettingKind is how a menu row is changed.
type SettingKind string

const (
	// SettingToggle flips on Enter with nothing further to ask.
	SettingToggle SettingKind = "toggle"
	// SettingChoice opens a list of the allowed values.
	SettingChoice SettingKind = "choice"
	// SettingText opens an input. An emptied one unsets the key.
	SettingText SettingKind = "text"
)

// SettingRow is one row of the settings menu.
type SettingRow struct {
	Key   string
	Label string
	Kind  SettingKind
	// Value is what is in effect now — the written value where there is one,
	// tau's default where there is not.
	Value string
	// Choices are the allowed values, for SettingChoice.
	Choices []string
}

// thinkingLevels are the reasoning efforts a model can be asked for, weakest
// first. "" is offered as the first choice because leaving the level to the
// model is different from asking it not to think.
var thinkingLevels = []string{
	"", string(ai.ThinkingOff), string(ai.ThinkingMinimal), string(ai.ThinkingLow),
	string(ai.ThinkingMedium), string(ai.ThinkingHigh), string(ai.ThinkingXHigh),
	string(ai.ThinkingMax),
}

// SettingsMenu is the curated list of settings worth changing from a menu.
//
// It is curated rather than generated from the known keys, for the same reason
// SettingsList prints only what is set: a menu of forty rows, most of them
// holding a default nobody chose, is documentation rather than a menu. What is
// here is what someone changes while using tau.
//
// Nested objects — compaction, retry, thinkingBudgets — are deliberately out.
// They are configuration you write once in a file, and a menu that could only
// edit them as pasted JSON would be worse than the typed command.
func (s *Session) SettingsMenu() []SettingRow {
	if s.Settings == nil {
		return nil
	}
	r := s.Settings
	raw := r.Raw()

	return []SettingRow{
		{Key: "theme", Label: "Theme", Kind: SettingChoice,
			Value: r.ThemeSetting(), Choices: s.themeChoices()},
		{Key: "defaultThinkingLevel", Label: "Thinking level", Kind: SettingChoice,
			Value: r.DefaultThinkingLevel(), Choices: thinkingLevels},
		{Key: "steeringMode", Label: "Steering", Kind: SettingChoice,
			Value: string(r.SteeringMode()), Choices: queueModes},
		{Key: "followUpMode", Label: "Follow-ups", Kind: SettingChoice,
			Value: string(r.FollowUpMode()), Choices: queueModes},
		// Written to the global file, which is the only scope it is read from:
		// a project that could raise its own trust would not be a trust gate.
		{Key: "defaultProjectTrust", Label: "Untrusted projects", Kind: SettingChoice,
			Value: trustValue(raw.DefaultProjectTrust), Choices: trustModes},

		{Key: "hideThinkingBlock", Label: "Hide thinking blocks", Kind: SettingToggle,
			Value: boolValue(raw.HideThinkingBlock, false)},
		{Key: "showCacheMissNotices", Label: "Cache-miss notices", Kind: SettingToggle,
			Value: boolValue(raw.ShowCacheMissNotices, false)},
		{Key: "quietStartup", Label: "Quiet startup", Kind: SettingToggle,
			Value: boolValue(raw.QuietStartup, false)},
		{Key: "collapseChangelog", Label: "Collapse the changelog", Kind: SettingToggle,
			Value: boolValue(raw.CollapseChangelog, false)},
		{Key: "enableSkillCommands", Label: "Skills as commands", Kind: SettingToggle,
			Value: boolValue(raw.EnableSkillCommands, true)},

		{Key: "externalEditor", Label: "External editor", Kind: SettingText,
			Value: strValue(raw.ExternalEditor)},
		{Key: "shellPath", Label: "Shell", Kind: SettingText,
			Value: strValue(raw.ShellPath)},
		{Key: "sessionDir", Label: "Session directory", Kind: SettingText,
			Value: r.SessionDir()},
		{Key: "httpProxy", Label: "HTTP proxy", Kind: SettingText,
			Value: r.HTTPProxy()},
	}
}

var queueModes = []string{string(settings.QueueAll), string(settings.QueueOneAtATime)}

var trustModes = []string{
	string(settings.TrustAsk), string(settings.TrustAlways), string(settings.TrustNever),
}

// themeChoices lists what the theme setting will accept: the automatic
// selection first, then every discovered theme.
func (s *Session) themeChoices() []string {
	set := theme.Discover(theme.Options{Dir: config.ThemesDir(), Paths: s.ThemePaths()})
	return append([]string{"auto"}, set.Names()...)
}

// ApplySetting writes one row of the menu.
//
// An emptied text field unsets the key rather than writing an empty string:
// "" and "not set" mean different things to every setting that has a default,
// and the menu's only way to say the second is to leave the field blank.
func (s *Session) ApplySetting(ctx context.Context, row SettingRow, value string) (string, error) {
	if row.Kind == SettingText && strings.TrimSpace(value) == "" {
		return s.SettingsUnset(ctx, row.Key)
	}
	return s.SettingsSet(ctx, row.Key, value)
}

// ToggleSetting flips a boolean row and reports what it became.
//
// The new value is written even when it matches tau's default. "off" written
// down and "off because nobody said otherwise" behave identically today, but
// only the first survives tau changing its mind about a default.
func (s *Session) ToggleSetting(ctx context.Context, row SettingRow) (string, error) {
	next := "true"
	if row.Value == "on" {
		next = "false"
	}
	return s.SettingsSet(ctx, row.Key, next)
}

func boolValue(p *bool, def bool) string {
	if p != nil {
		def = *p
	}
	if def {
		return "on"
	}
	return "off"
}

func strValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func trustValue(p *settings.DefaultProjectTrust) string {
	if p == nil {
		return string(settings.TrustAsk)
	}
	return string(*p)
}
