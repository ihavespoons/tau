package keybindings

import (
	"strings"
	"unicode"
)

// Key is a parsed key identifier: a base key plus its modifiers.
//
// Identifiers are written modifier-first with "+" separators — "ctrl+p",
// "shift+ctrl+p", "alt+left". Modifier order carries no meaning, so
// "shift+ctrl+p" and "ctrl+shift+p" are the same key; comparing parsed Keys
// rather than the strings is what makes that true.
type Key struct {
	// Name is the base key, lower-cased and canonical: "escape" not "esc",
	// "pagedown" not "pgdown", "a" not "A".
	Name string

	Ctrl  bool
	Alt   bool
	Shift bool
	Super bool
}

// baseAliases maps the spellings tau accepts onto the canonical base name.
//
// Both dialects are here on purpose: the left column holds Pi's names, so a
// keybindings.json copied from Pi parses, and Bubble Tea's names, so a key the
// terminal just produced can be parsed by the very same function. Anything not
// listed is already canonical.
var baseAliases = map[string]string{
	"esc":      "escape",
	"return":   "enter",
	"del":      "delete",
	"ins":      "insert",
	"pgup":     "pageup",
	"pgdn":     "pagedown",
	"pgdown":   "pagedown",
	"spacebar": "space",
}

// modifierAliases maps modifier spellings onto the four tau recognises.
var modifierAliases = map[string]string{
	"control": "ctrl",
	"option":  "alt",
	"meta":    "super",
	"cmd":     "super",
	"command": "super",
	"win":     "super",
}

// ParseKey parses a key identifier. It reports false for the empty string, for
// an identifier that is nothing but modifiers, and for a repeated or unknown
// modifier — all of which are typos rather than keys nothing can press.
func ParseKey(id string) (Key, bool) {
	if id == "" {
		return Key{}, false
	}

	var k Key
	parts := strings.Split(id, "+")
	for i, part := range parts {
		// The base key is whatever is last, so a lone "+" or a trailing
		// "ctrl++" binds the plus key rather than parsing as a modifier.
		if i == len(parts)-1 || part == "" {
			base := strings.Join(parts[i:], "+")
			k.Name = canonicalBase(base)
			// A modifier in the base position means the identifier named no
			// key at all — "ctrl", "ctrl+alt". Nothing can press that.
			if k.Name == "" || isModifier(k.Name) {
				return Key{}, false
			}
			// Only when nothing was written in front of it: an identifier that
			// spells its own modifiers is a human writing a config, where case
			// is spelling — "Ctrl+Q" means ctrl+q. A bare capital is the other
			// thing, a terminal reporting the character shift produced.
			if i == 0 && isUpperLetter(base) {
				k.Shift = true
			}
			return k.normalize(), true
		}

		mod := canonicalModifier(part)
		switch mod {
		case "ctrl":
			if k.Ctrl {
				return Key{}, false
			}
			k.Ctrl = true
		case "alt":
			if k.Alt {
				return Key{}, false
			}
			k.Alt = true
		case "shift":
			if k.Shift {
				return Key{}, false
			}
			k.Shift = true
		case "super":
			if k.Super {
				return Key{}, false
			}
			k.Super = true
		default:
			return Key{}, false
		}
	}
	return Key{}, false
}

func canonicalModifier(name string) string {
	mod := strings.ToLower(name)
	if alias, ok := modifierAliases[mod]; ok {
		return alias
	}
	return mod
}

// isUpperLetter reports a base key written as a single capital letter.
//
// A terminal has no way to say "shift+l": it sends the character the shift key
// produced, and the TUI layer reports that as "L". So an uppercase letter and
// "shift+l" are two spellings of one press, and folding them together here is
// what lets Pi's shift+ defaults fire at all — without it they parse to a key
// with Shift set that nothing a terminal sends can ever match.
//
// Only letters. A shifted digit or symbol produces a different character
// entirely ("!" not "shift+1"), which is already its own base key.
func isUpperLetter(base string) bool {
	r := []rune(base)
	return len(r) == 1 && unicode.IsUpper(r[0]) && unicode.IsLetter(r[0])
}

func isModifier(name string) bool {
	switch canonicalModifier(name) {
	case "ctrl", "alt", "shift", "super":
		return true
	}
	return false
}

// normalize folds spellings that name the same byte onto one key.
//
// A terminal sends BS (0x08) for ctrl+backspace and DEL (0x7f) for backspace,
// and the TUI layer reports the first as "ctrl+h" — they are the same byte, so
// a binding written either way has to answer to the same press. Pi's defaults
// spell it "ctrl+backspace"; without this, nothing a terminal sends could ever
// trigger one.
//
// The two therefore name one binding, and binding them to different actions
// binds the same key twice.
func (k Key) normalize() Key {
	if k.Ctrl && k.Name == "backspace" {
		k.Name = "h"
	}
	return k
}

func canonicalBase(name string) string {
	// Symbol keys such as "]" and "-" are case-less; letters and named keys
	// are matched case-insensitively so "Ctrl+P" and "ctrl+p" are one key.
	lower := strings.ToLower(name)
	if alias, ok := baseAliases[lower]; ok {
		return alias
	}
	return lower
}

// String renders the key back to an identifier in tau's canonical order. It is
// for display: matching goes through the parsed form, which does not care.
func (k Key) String() string {
	if k.Name == "" {
		return ""
	}
	var b strings.Builder
	for _, mod := range []struct {
		on   bool
		name string
	}{{k.Ctrl, "ctrl"}, {k.Alt, "alt"}, {k.Shift, "shift"}, {k.Super, "super"}} {
		if mod.on {
			b.WriteString(mod.name)
			b.WriteByte('+')
		}
	}
	b.WriteString(k.Name)
	return b.String()
}

// SameKey reports whether two identifiers name the same key. Unparseable
// identifiers match nothing, including each other.
func SameKey(a, b string) bool {
	ka, ok := ParseKey(a)
	if !ok {
		return false
	}
	kb, ok := ParseKey(b)
	if !ok {
		return false
	}
	return ka == kb
}
