package keybindings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Keys is the key list bound to one action.
//
// Pi's file format lets a single key be written bare — "ctrl+p" — and several
// as an array, and round-trips whichever the user wrote. Keys decodes both and
// re-encodes in the shorter form, so rewriting a config does not churn the
// user's file.
type Keys []string

// UnmarshalJSON accepts a string or an array of strings.
func (k *Keys) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*k = Keys{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("keys must be a string or an array of strings")
	}
	*k = Keys(many)
	return nil
}

// MarshalJSON writes a lone key as a bare string.
func (k Keys) MarshalJSON() ([]byte, error) {
	if len(k) == 1 {
		return json.Marshal(k[0])
	}
	return json.Marshal([]string(k))
}

// dedupe drops repeated keys, keeping the first of each — Pi's normalizeKeys
// (pi-tui keybindings.ts:135-147).
func (k Keys) dedupe() Keys {
	seen := make(map[string]bool, len(k))
	out := make(Keys, 0, len(k))
	for _, key := range k {
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

// Entry is one line of a keybindings.json document.
type Entry struct {
	// Name is the binding id as written, which may be a legacy name or one tau
	// does not know.
	Name string
	// Value is the raw JSON, kept verbatim so rewriting the file cannot
	// destroy something tau failed to understand.
	Value json.RawMessage
}

// Config is a keybindings.json document: binding ids to key lists, in file
// order.
//
// Order is kept rather than being thrown into a map because tau writes this
// file back after migrating legacy names, and a rewrite that shuffled the
// user's lines would be its own kind of damage.
type Config struct {
	entries []Entry
	index   map[string]int
}

// NewConfig builds an empty config.
func NewConfig() *Config { return &Config{index: map[string]int{}} }

// Len is the number of entries.
func (c *Config) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}

// Entries returns the entries in file order.
func (c *Config) Entries() []Entry {
	if c == nil {
		return nil
	}
	return append([]Entry(nil), c.entries...)
}

// Has reports whether the config mentions a binding id.
func (c *Config) Has(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.index[name]
	return ok
}

// Set adds or replaces an entry, keeping an existing entry's position.
func (c *Config) Set(name string, value json.RawMessage) {
	if c.index == nil {
		c.index = map[string]int{}
	}
	if i, ok := c.index[name]; ok {
		c.entries[i].Value = value
		return
	}
	c.index[name] = len(c.entries)
	c.entries = append(c.entries, Entry{Name: name, Value: value})
}

// SetKeys adds or replaces an entry from a key list.
func (c *Config) SetKeys(name string, keys Keys) {
	raw, err := json.Marshal(keys)
	if err != nil {
		return
	}
	c.Set(name, raw)
}

// Keys decodes one entry's key list. It reports false when the entry is absent
// or is neither a string nor an array of strings — Pi's toKeybindingsConfig
// drops those silently (core/keybindings.ts:275-287), and so does tau, because
// the alternative is refusing to start over a stray line.
func (c *Config) Keys(name string) (Keys, bool) {
	if c == nil {
		return nil, false
	}
	i, ok := c.index[name]
	if !ok {
		return nil, false
	}
	var keys Keys
	if err := json.Unmarshal(c.entries[i].Value, &keys); err != nil {
		return nil, false
	}
	return keys, true
}

// UnmarshalJSON decodes a keybindings.json document, preserving line order.
func (c *Config) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("keybindings config must be an object")
	}

	c.entries = nil
	c.index = map[string]int{}
	for dec.More() {
		nameTok, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := nameTok.(string)
		if !ok {
			return fmt.Errorf("keybindings config has a non-string key")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		c.Set(name, raw)
	}
	_, err = dec.Token()
	return err
}

// MarshalJSON writes the document in entry order.
func (c *Config) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range c.entries {
		if i > 0 {
			b.WriteByte(',')
		}
		name, err := json.Marshal(e.Name)
		if err != nil {
			return nil, err
		}
		b.Write(name)
		b.WriteByte(':')
		b.Write(e.Value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// nameMigrations maps the flat names Pi used before bindings were namespaced
// onto their current ids (core/keybindings.ts:209-270).
var nameMigrations = map[string]Binding{
	"cursorUp":                 EditorCursorUp,
	"cursorDown":               EditorCursorDown,
	"cursorLeft":               EditorCursorLeft,
	"cursorRight":              EditorCursorRight,
	"cursorWordLeft":           EditorCursorWordLeft,
	"cursorWordRight":          EditorCursorWordRight,
	"cursorLineStart":          EditorCursorLineStart,
	"cursorLineEnd":            EditorCursorLineEnd,
	"jumpForward":              EditorJumpForward,
	"jumpBackward":             EditorJumpBackward,
	"pageUp":                   EditorPageUp,
	"pageDown":                 EditorPageDown,
	"deleteCharBackward":       EditorDeleteCharBackward,
	"deleteCharForward":        EditorDeleteCharForward,
	"deleteWordBackward":       EditorDeleteWordBackward,
	"deleteWordForward":        EditorDeleteWordForward,
	"deleteToLineStart":        EditorDeleteToLineStart,
	"deleteToLineEnd":          EditorDeleteToLineEnd,
	"yank":                     EditorYank,
	"yankPop":                  EditorYankPop,
	"undo":                     EditorUndo,
	"newLine":                  InputNewLine,
	"submit":                   InputSubmit,
	"tab":                      InputTab,
	"copy":                     InputCopy,
	"selectUp":                 SelectUp,
	"selectDown":               SelectDown,
	"selectPageUp":             SelectPageUp,
	"selectPageDown":           SelectPageDown,
	"selectConfirm":            SelectConfirm,
	"selectCancel":             SelectCancel,
	"interrupt":                AppInterrupt,
	"clear":                    AppClear,
	"exit":                     AppExit,
	"suspend":                  AppSuspend,
	"cycleThinkingLevel":       AppThinkingCycle,
	"cycleModelForward":        AppModelCycleForward,
	"cycleModelBackward":       AppModelCycleBackward,
	"selectModel":              AppModelSelect,
	"expandTools":              AppToolsExpand,
	"toggleThinking":           AppThinkingToggle,
	"toggleSessionNamedFilter": AppSessionToggleNamedFilter,
	"externalEditor":           AppEditorExternal,
	"followUp":                 AppMessageFollowUp,
	"dequeue":                  AppMessageDequeue,
	"pasteImage":               AppClipboardPasteImage,
	"newSession":               AppSessionNew,
	"tree":                     AppSessionTree,
	"fork":                     AppSessionFork,
	"resume":                   AppSessionResume,
	"treeFoldOrUp":             AppTreeFoldOrUp,
	"treeUnfoldOrDown":         AppTreeUnfoldOrDown,
	"treeEditLabel":            AppTreeEditLabel,
	"treeToggleLabelTimestamp": AppTreeToggleLabelTimestamp,
	"toggleSessionPath":        AppSessionTogglePath,
	"toggleSessionSort":        AppSessionToggleSort,
	"renameSession":            AppSessionRename,
	"deleteSession":            AppSessionDelete,
	"deleteSessionNoninvasive": AppSessionDeleteNoninvasive,
}

// Migrate renames legacy flat binding names to their namespaced ids and returns
// the result in canonical order, reporting whether anything changed.
//
// A legacy name whose modern id is already present is dropped rather than
// overwriting it: the user has clearly moved on, and the stale line is the one
// to lose.
func Migrate(c *Config) (*Config, bool) {
	out := NewConfig()
	migrated := false

	for _, e := range c.Entries() {
		next := e.Name
		if id, ok := nameMigrations[e.Name]; ok {
			next = string(id)
		}
		if next != e.Name {
			migrated = true
			if c.Has(next) {
				continue
			}
		}
		out.Set(next, e.Value)
	}
	return order(out), migrated
}

// order sorts entries into the binding table's declaration order, with names
// tau does not recognise kept at the end in alphabetical order.
func order(c *Config) *Config {
	out := NewConfig()
	for _, d := range Definitions {
		if e, ok := c.entry(string(d.ID)); ok {
			out.Set(e.Name, e.Value)
		}
	}

	var extras []Entry
	for _, e := range c.Entries() {
		if !out.Has(e.Name) {
			extras = append(extras, e)
		}
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].Name < extras[j].Name })
	for _, e := range extras {
		out.Set(e.Name, e.Value)
	}
	return out
}

func (c *Config) entry(name string) (Entry, bool) {
	i, ok := c.index[name]
	if !ok {
		return Entry{}, false
	}
	return c.entries[i], true
}
