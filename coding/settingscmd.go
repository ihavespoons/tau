package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ihavespoons/tau/settings"
)

// errNoSettings is returned by the /settings surface when the session was
// built without a settings manager.
var errNoSettings = errors.New("this session has no settings file")

// SettingsKeys lists the keys tau models, for completion.
func (s *Session) SettingsKeys() []string { return settings.Keys() }

// SettingsList renders every configured setting with the scope it came from.
//
// Only what is actually set is listed. The alternative — every key tau knows
// about, most of them showing a default nobody chose — is the same information
// as the documentation, and buries the handful of lines that answer "what did
// I change?".
func (s *Session) SettingsList() string {
	if s.setMgr == nil {
		return errNoSettings.Error()
	}
	raw, err := s.mergedSettings()
	if err != nil {
		return err.Error()
	}

	var b strings.Builder
	if len(raw) == 0 {
		b.WriteString("Nothing is configured.\n")
	} else {
		keys := make([]string, 0, len(raw))
		width := 0
		for k := range raw {
			keys = append(keys, k)
			if len(k) > width {
				width = len(k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			origin := ""
			if scope, ok := s.setMgr.Origin(k); ok {
				origin = "  (" + string(scope) + ")"
			}
			fmt.Fprintf(&b, "%-*s  %s%s\n", width, k, compactJSON(raw[k]), origin)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "global:  %s\n", s.setMgr.Path(settings.Global))
	fmt.Fprintf(&b, "project: %s", s.setMgr.Path(settings.Project))
	if !s.Trust.Trusted {
		b.WriteString("  (not loaded — project is untrusted)")
	}
	b.WriteString("\n\nSet a value with /settings <key> <value>, remove one with /settings unset <key>.")
	return b.String()
}

// SettingsGet renders one key. A dotted key reaches one level into a nested
// object, which is as far as the settings file nests.
func (s *Session) SettingsGet(key string) (string, error) {
	if s.setMgr == nil {
		return "", errNoSettings
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("/settings needs a key")
	}

	value, ok := s.setMgr.Get(key)
	if !ok {
		out := key + " is not set"
		if !settings.Known(topKey(key)) {
			out += " (and tau does not know that key — check the spelling)"
		}
		return out, nil
	}
	out := key + " " + compactJSON(value)
	if scope, ok := s.setMgr.Origin(topKey(key)); ok {
		out += "  (" + string(scope) + ")"
	}
	return out, nil
}

// SettingsSet writes a key to global settings and applies it to this session.
//
// The value is read as JSON when it parses as JSON, and as a plain string when
// it does not: `theme dark` means "dark", `quietStartup true` means true, and
// `npmCommand ["pnpm","add"]` means the array. Guessing this way is what makes
// the command usable without quoting every string.
//
// Writes go to the global scope. Project settings are refused outright in an
// untrusted directory, and choosing the scope from a one-line command would
// need a flag that earns its keep only for the rare project-specific value —
// which can be edited in the file the listing names.
func (s *Session) SettingsSet(ctx context.Context, key, value string) (string, error) {
	if s.setMgr == nil {
		return "", errNoSettings
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("/settings needs a key")
	}

	parsed := parseSettingValue(value)
	if err := s.setMgr.Set(ctx, settings.Global, key, parsed); err != nil {
		return "", err
	}
	if err := s.refreshSettings(); err != nil {
		return "", err
	}

	out := "Set " + key + " to " + compactJSON(mustJSON(parsed)) + " in " + s.setMgr.Path(settings.Global)
	if !settings.Known(topKey(key)) {
		out += "\ntau does not know this key — it will be kept in the file, but nothing reads it."
	}
	if scope, ok := s.setMgr.Origin(topKey(key)); ok && scope == settings.Project {
		out += "\nProject settings still override it for this directory."
	}
	return out, nil
}

// SettingsUnset removes a key from global settings.
func (s *Session) SettingsUnset(ctx context.Context, key string) (string, error) {
	if s.setMgr == nil {
		return "", errNoSettings
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("/settings unset needs a key")
	}
	if err := s.setMgr.Unset(ctx, settings.Global, key); err != nil {
		return "", err
	}
	if err := s.refreshSettings(); err != nil {
		return "", err
	}
	return "Removed " + key + " from " + s.setMgr.Path(settings.Global), nil
}

// mergedSettings renders the merged view as raw keys. It goes through the
// typed struct on purpose: that is where the unmodelled keys in Extra are
// reunited with the ones tau knows, so the listing shows the whole file.
func (s *Session) mergedSettings() (map[string]json.RawMessage, error) {
	set, err := s.setMgr.Settings()
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(set)
	if err != nil {
		return nil, fmt.Errorf("rendering settings: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("rendering settings: %w", err)
	}
	return raw, nil
}

// parseSettingValue reads a typed value out of what was typed, falling back to
// a string. A bare word is far more often a string than a syntax error.
func parseSettingValue(value string) any {
	value = strings.TrimSpace(value)
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err == nil {
		return parsed
	}
	return value
}

// topKey is the part of a dotted key that names a file-level entry.
func topKey(key string) string {
	top, _, _ := strings.Cut(key, ".")
	return top
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`"?"`)
	}
	return b
}
