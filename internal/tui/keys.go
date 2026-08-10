package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/keybindings"
)

// keyID turns a Bubble Tea key event into the identifier the keybindings table
// is written in.
//
// The two vocabularies were built to agree — keybindings.ParseKey accepts
// Bubble Tea's spellings alongside Pi's — so this is almost msg.String(). Space
// is the exception: Bubble Tea renders it as a literal " " for backwards
// compatibility (key.go:302), which nobody would write in a config file.
//
// A paste has no identifier at all. Pasted text arrives as a key event whose
// runes are the pasted content, and a paste that happened to begin with "y"
// must not fire whatever "y" is bound to.
func keyID(msg tea.KeyMsg) string {
	if msg.Paste {
		return ""
	}
	if msg.Type == tea.KeySpace {
		if msg.Alt {
			return "alt+space"
		}
		return "space"
	}
	return msg.String()
}

// fallbackKeys is used when a component was built without a manager, which
// happens in tests that are not about keys. It is tau's defaults, never a
// user's file.
var fallbackKeys = keybindings.New(nil)

// bound reports whether a key identifier triggers a binding. The empty
// identifier — a paste — triggers nothing.
func bound(km *keybindings.Manager, key string, id keybindings.Binding) bool {
	if key == "" {
		return false
	}
	if km == nil {
		km = fallbackKeys
	}
	return km.Matches(key, id)
}

// keyLabel renders the first key bound to an action, for a hint line. An action
// with no key returns "", so the hint can be dropped rather than pointing at
// nothing — which is what happens to app.suspend on Windows.
func keyLabel(km *keybindings.Manager, id keybindings.Binding) string {
	keys := km.Keys(id)
	if len(keys) == 0 {
		return ""
	}
	return prettyKey(keys[0])
}

// keyNames are the display spellings of keys whose identifier reads badly in a
// sentence. Anything absent is title-cased, which covers the letters and the
// keys whose names are already words.
var keyNames = map[string]string{
	"escape":    "Esc",
	"delete":    "Del",
	"pageup":    "PgUp",
	"pagedown":  "PgDn",
	"backspace": "Bksp",
	"up":        "↑",
	"down":      "↓",
	"left":      "←",
	"right":     "→",
}

// prettyKey renders a key identifier the way a hint should show it: "ctrl+c"
// becomes "Ctrl+C". An identifier tau cannot parse is shown verbatim, because a
// key the user wrote themselves is more useful on screen than nothing.
func prettyKey(id string) string {
	k, ok := keybindings.ParseKey(id)
	if !ok {
		return id
	}

	var parts []string
	for _, mod := range []struct {
		on   bool
		name string
	}{{k.Ctrl, "Ctrl"}, {k.Alt, "Alt"}, {k.Shift, "Shift"}, {k.Super, "Super"}} {
		if mod.on {
			parts = append(parts, mod.name)
		}
	}

	name := k.Name
	if display, ok := keyNames[name]; ok {
		name = display
	} else if r := []rune(name); len(r) == 1 {
		name = strings.ToUpper(name)
	} else {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return strings.Join(append(parts, name), "+")
}
