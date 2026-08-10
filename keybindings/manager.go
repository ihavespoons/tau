package keybindings

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// FileName is the name of the keybindings file inside the agent directory.
const FileName = "keybindings.json"

// Conflict is one key claimed by more than one binding in the user's config.
//
// Only user overrides are checked. The defaults overlap on purpose — ctrl+c is
// both "copy selection" and "clear editor", ctrl+d both "delete character" and
// "exit" — because those bindings belong to different components and never
// compete for the same keypress. A user who binds two actions in the same
// component to one key gets told; a user who reproduces one of Pi's own
// overlaps does not get nagged about a default they never chose.
type Conflict struct {
	// Key is the key identifier as the user wrote it.
	Key string
	// Bindings are the ids claiming it, in table order.
	Bindings []Binding
}

func (c Conflict) String() string {
	ids := make([]string, len(c.Bindings))
	for i, b := range c.Bindings {
		ids[i] = string(b)
	}
	return fmt.Sprintf("%s is bound to %s", c.Key, strings.Join(ids, " and "))
}

// Manager answers "was this key bound to that action?" for the whole binding
// table, with the user's overrides applied.
type Manager struct {
	path      string
	user      *Config
	resolved  map[Binding]Keys
	parsed    map[Binding][]Key
	conflicts []Conflict
}

// New builds a manager from a config already in hand. A nil config means
// defaults throughout.
func New(user *Config) *Manager {
	m := &Manager{user: user}
	m.rebuild()
	return m
}

// Load reads a keybindings file and builds a manager from it, returning any
// warnings worth showing the user. The path is passed in rather than assembled
// here so config stays the one place that decides where tau's files live.
//
// A missing file is the normal case, not a warning. Every other problem —
// unreadable file, malformed JSON, a binding tau does not know, a key it cannot
// parse — degrades to the default for that one binding and says so. Pi swallows
// all of it; tau would rather start on defaults out loud than leave someone
// wondering why their config did nothing.
//
// When the file used legacy flat binding names it is rewritten in place with
// the current ones, matching Pi's migration.
func Load(path string) (*Manager, []string) {
	m := &Manager{path: path}
	var warnings []string

	data, err := os.ReadFile(m.path)
	switch {
	case os.IsNotExist(err):
		// No file is how most people run: defaults, silently.
	case err != nil:
		warnings = append(warnings, fmt.Sprintf("could not read %s: %v; using default keybindings", m.path, err))
	default:
		cfg := NewConfig()
		if err := json.Unmarshal(data, cfg); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not parse %s: %v; using default keybindings", m.path, err))
		} else {
			migrated, changed := Migrate(cfg)
			m.user = migrated
			if changed {
				if err := m.save(); err != nil {
					warnings = append(warnings, fmt.Sprintf("could not update %s with the current keybinding names: %v", m.path, err))
				}
			}
		}
	}

	return m, append(warnings, m.rebuild()...)
}

// Path is the file the manager was loaded from, empty when it was built from a
// config directly.
func (m *Manager) Path() string { return m.path }

// Reload re-reads the file, returning fresh warnings. A manager built by New
// has nothing to re-read and reports nothing.
func (m *Manager) Reload() []string {
	if m.path == "" {
		return nil
	}
	next, warnings := Load(m.path)
	*m = *next
	return warnings
}

// SetUser replaces the overrides and rebuilds, returning fresh warnings.
func (m *Manager) SetUser(user *Config) []string {
	m.user = user
	return m.rebuild()
}

// User returns the overrides as loaded, after name migration.
func (m *Manager) User() *Config { return m.user }

// Keys are the keys currently bound to an action, defaults included.
func (m *Manager) Keys(id Binding) []string {
	return append([]string(nil), m.resolved[id]...)
}

// Matches reports whether a key identifier triggers a binding. Comparison is
// on the parsed key, so "esc" matches a binding written "escape" and
// "shift+ctrl+p" matches one written "ctrl+shift+p".
func (m *Manager) Matches(key string, id Binding) bool {
	k, ok := ParseKey(key)
	if !ok {
		return false
	}
	for _, bound := range m.parsed[id] {
		if bound == k {
			return true
		}
	}
	return false
}

// Match returns every binding a key triggers, in table order.
func (m *Manager) Match(key string) []Binding {
	k, ok := ParseKey(key)
	if !ok {
		return nil
	}
	var out []Binding
	for _, d := range Definitions {
		for _, bound := range m.parsed[d.ID] {
			if bound == k {
				out = append(out, d.ID)
				break
			}
		}
	}
	return out
}

// Conflicts lists keys the user bound to more than one action.
func (m *Manager) Conflicts() []Conflict {
	return append([]Conflict(nil), m.conflicts...)
}

// Effective is the full resolved binding set as a config, suitable for writing
// out or showing in a settings screen. Every known binding appears, in table
// order, including ones with no keys.
func (m *Manager) Effective() *Config {
	out := NewConfig()
	for _, d := range Definitions {
		out.SetKeys(string(d.ID), m.resolved[d.ID])
	}
	return out
}

// save writes the user's config back to disk in the shape Pi writes it.
func (m *Manager) save() error {
	if m.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(m.user, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, append(data, '\n'), 0o644)
}

// rebuild resolves every binding against the user's overrides and collects the
// warnings that fall out.
func (m *Manager) rebuild() []string {
	m.resolved = make(map[Binding]Keys, len(Definitions))
	m.parsed = make(map[Binding][]Key, len(Definitions))
	m.conflicts = nil

	var warnings []string

	// claims maps a canonical key to the bindings the user gave it, so a
	// conflict is found by construction rather than by comparing strings.
	claims := map[Key][]Binding{}

	for _, d := range Definitions {
		keys := Keys(d.DefaultKeys)
		override := false

		if raw, ok := m.user.Keys(string(d.ID)); ok {
			keys = raw.dedupe()
			override = true
		} else if m.user.Has(string(d.ID)) {
			warnings = append(warnings, fmt.Sprintf("keybinding %s: keys must be a string or an array of strings; using the default", d.ID))
		}

		m.resolved[d.ID] = keys
		for _, key := range keys {
			parsed, ok := ParseKey(key)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("keybinding %s: %q is not a key tau understands", d.ID, key))
				continue
			}
			m.parsed[d.ID] = append(m.parsed[d.ID], parsed)
			if override {
				claims[parsed] = append(claims[parsed], d.ID)
			}
		}
	}

	for _, e := range m.user.Entries() {
		if !IsDefined(e.Name) {
			warnings = append(warnings, fmt.Sprintf("keybinding %s is not an action tau knows; it will be ignored", e.Name))
		}
	}

	for key, ids := range claims {
		if len(ids) > 1 {
			m.conflicts = append(m.conflicts, Conflict{Key: key.String(), Bindings: ids})
		}
	}
	sort.Slice(m.conflicts, func(i, j int) bool { return m.conflicts[i].Key < m.conflicts[j].Key })
	for _, c := range m.conflicts {
		warnings = append(warnings, "keybinding conflict: "+c.String())
	}

	return warnings
}
