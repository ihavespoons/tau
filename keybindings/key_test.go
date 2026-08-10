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

// The same shape of bug one letter over: a terminal cannot say "shift+l", it
// sends the character shift produced and the TUI layer reports "L". Pi's
// defaults for the tree picker spell it "shift+l", so without folding the two
// together neither app.tree.editLabel nor app.tree.toggleLabelTimestamp could
// ever fire.
func TestACapitalLetterIsShiftPlusThatLetter(t *testing.T) {
	if !SameKey("L", "shift+l") {
		t.Error("a press reported as L does not match a binding written shift+l")
	}
	if !New(nil).Matches("L", AppTreeEditLabel) {
		t.Error("shift+l is the default for app.tree.editLabel, but pressing it does nothing")
	}
	if !New(nil).Matches("T", AppTreeToggleLabelTimestamp) {
		t.Error("shift+t is the default for app.tree.toggleLabelTimestamp, but pressing it does nothing")
	}
}

// Folding shift onto the letter must not fold the letter onto itself: these
// are two different bytes and a binding on one must not answer the other.
func TestALowercaseLetterIsNotItsCapital(t *testing.T) {
	if SameKey("l", "L") {
		t.Error("l and L collapsed into one key")
	}
}
