package keybindings

import "testing"

// A terminal sends the same byte for ctrl+backspace and ctrl+h, so a binding
// written either way has to answer to the same press. Pi's defaults spell it
// "ctrl+backspace"; without this they could never fire.
func TestCtrlBackspaceIsCtrlH(t *testing.T) {
	a, ok := ParseKey("ctrl+backspace")
	if !ok {
		t.Fatal("ctrl+backspace did not parse")
	}
	b, ok := ParseKey("ctrl+h")
	if !ok {
		t.Fatal("ctrl+h did not parse")
	}
	if a != b {
		t.Errorf("ctrl+backspace parsed as %+v, ctrl+h as %+v — they are one key", a, b)
	}
}

// Plain backspace is a different byte and must stay a different key.
func TestBackspaceIsNotCtrlH(t *testing.T) {
	plain, _ := ParseKey("backspace")
	ctrlH, _ := ParseKey("ctrl+h")
	if plain == ctrlH {
		t.Error("backspace and ctrl+h collapsed into one key")
	}
}
