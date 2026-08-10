package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/keybindings"
)

// The two vocabularies have to agree or every binding silently stops working:
// bindings are written in keybindings.ParseKey's dialect, and the keys they are
// matched against come out of Bubble Tea.
func TestKeyIDIsWrittenInTheBindingDialect(t *testing.T) {
	cases := []struct {
		msg  tea.KeyMsg
		want string
	}{
		{tea.KeyMsg{Type: tea.KeySpace}, "space"},
		{tea.KeyMsg{Type: tea.KeySpace, Alt: true}, "alt+space"},
		{tea.KeyMsg{Type: tea.KeyCtrlP}, "ctrl+p"},
		{tea.KeyMsg{Type: tea.KeyShiftTab}, "shift+tab"},
		{tea.KeyMsg{Type: tea.KeyEsc}, "esc"},
		{tea.KeyMsg{Type: tea.KeyPgUp}, "pgup"},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}, Alt: true}, "alt+b"},
	}
	for _, c := range cases {
		got := keyID(c.msg)
		if got != c.want {
			t.Errorf("keyID(%v) = %q, want %q", c.msg, got, c.want)
			continue
		}
		if _, ok := keybindings.ParseKey(got); !ok {
			t.Errorf("keyID produced %q, which no binding can name", got)
		}
	}
}

// A paste arrives as a key event carrying the pasted text. Giving it an
// identifier would fire whatever the first character happens to be bound to.
func TestPasteHasNoKeyIdentifier(t *testing.T) {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("yes please"), Paste: true}
	if got := keyID(msg); got != "" {
		t.Errorf("a paste got the identifier %q", got)
	}
	if bound(nil, keyID(msg), keybindings.SelectConfirm) {
		t.Error("a paste triggered a binding")
	}
}

func TestPrettyKey(t *testing.T) {
	cases := map[string]string{
		"ctrl+c":       "Ctrl+C",
		"shift+ctrl+p": "Ctrl+Shift+P",
		"escape":       "Esc",
		"esc":          "Esc",
		"pageup":       "PgUp",
		"alt+enter":    "Alt+Enter",
		"up":           "↑",
	}
	for id, want := range cases {
		if got := prettyKey(id); got != want {
			t.Errorf("prettyKey(%q) = %q, want %q", id, got, want)
		}
	}
}

// --- editor ---

func TestEditorReadlineMotions(t *testing.T) {
	e := newTestEditor()
	typeText(e, "alpha beta")

	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	if got := e.Value(); got != "alpha " {
		t.Errorf("ctrl+w: %q", got)
	}

	e.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	typeText(e, "x")
	if got := e.Value(); got != "xalpha " {
		t.Errorf("ctrl+a then typing: %q", got)
	}

	e.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if got := e.Value(); got != "" {
		t.Errorf("ctrl+u: %q", got)
	}
}

// Alt+b and Alt+f are word motions, so an alt-modified rune must not be typed
// into the buffer the way a bare one is.
func TestAltRunesAreMotionsNotText(t *testing.T) {
	e := newTestEditor()
	typeText(e, "alpha beta")

	e.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}, Alt: true})
	typeText(e, "-")
	if got := e.Value(); got != "alpha -beta" {
		t.Errorf("alt+b: %q", got)
	}

	e.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}, Alt: true})
	typeText(e, "!")
	if got := e.Value(); got != "alpha -beta!" {
		t.Errorf("alt+f: %q", got)
	}
}

func TestCtrlDDeletesForward(t *testing.T) {
	e := newTestEditor()
	typeText(e, "abc")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if got := e.Value(); got != "bc" {
		t.Errorf("ctrl+d: %q", got)
	}
}

// A rebound key has to reach the editor under its new name, and its old name
// has to stop working — otherwise the file was decoration.
func TestEditorHonoursRebinding(t *testing.T) {
	user := keybindings.NewConfig()
	user.SetKeys(string(keybindings.InputSubmit), keybindings.Keys{"ctrl+s"})
	e := newEditor(DefaultTheme(), keybindings.New(user))
	e.SetWidth(60)

	typeText(e, "hello")
	if _, ok := e.Update(tea.KeyMsg{Type: tea.KeyEnter}); ok {
		t.Error("enter still submitted after being rebound away")
	}
	got, ok := e.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !ok || got != "hello" {
		t.Errorf("ctrl+s submitted %q, ok=%v", got, ok)
	}
}

// No config gets to make a letter untypeable. A binding that shadowed "a" would
// leave its author unable to type the command that undoes it.
func TestTypingBeatsBindings(t *testing.T) {
	user := keybindings.NewConfig()
	user.SetKeys(string(keybindings.InputSubmit), keybindings.Keys{"a"})
	e := newEditor(DefaultTheme(), keybindings.New(user))
	e.SetWidth(60)

	if _, ok := typeText(e, "a"); ok {
		t.Fatal("a bare letter submitted")
	}
	if got := e.Value(); got != "a" {
		t.Errorf("the letter did not reach the buffer: %q", got)
	}
}

// --- dialogs ---

func newTestSelect(n int, filterable bool) *selectDialog {
	opts := make([]extension.SelectOption, n)
	for i := range opts {
		opts[i] = extension.SelectOption{Label: string(rune('a' + i%26)), Value: string(rune('a' + i%26))}
	}
	d := &selectDialog{
		baseDialog: baseDialog{reply: make(chan dialogResult, 1)},
		options:    opts,
		filterable: filterable,
		visible:    5,
	}
	d.refilter()
	return d
}

func TestSelectPageMotionStopsAtTheEnds(t *testing.T) {
	d := newTestSelect(12, false)

	d.key(tea.KeyMsg{Type: tea.KeyPgDown}, nil)
	if d.cursor != 5 {
		t.Errorf("page down moved to %d, want 5", d.cursor)
	}
	for range 5 {
		d.key(tea.KeyMsg{Type: tea.KeyPgDown}, nil)
	}
	if d.cursor != 11 {
		t.Errorf("paging past the bottom left the cursor at %d, want 11", d.cursor)
	}
	for range 5 {
		d.key(tea.KeyMsg{Type: tea.KeyPgUp}, nil)
	}
	if d.cursor != 0 {
		t.Errorf("paging past the top left the cursor at %d, want 0", d.cursor)
	}
}

// While a filter is open every printable key belongs to it, so an option named
// "p" stays reachable no matter what ctrl+p-style bindings exist.
func TestSelectFilterTakesPrintableKeysFirst(t *testing.T) {
	d := newTestSelect(6, true)
	d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}}, nil)
	if d.filter != "c" {
		t.Fatalf("filter = %q", d.filter)
	}
	if len(d.matches) != 1 {
		t.Fatalf("filter matched %d options", len(d.matches))
	}
	d.key(tea.KeyMsg{Type: tea.KeyBackspace}, nil)
	if d.filter != "" || len(d.matches) != 6 {
		t.Errorf("backspace left filter %q with %d matches", d.filter, len(d.matches))
	}
}

func TestSelectConfirmAndCancel(t *testing.T) {
	d := newTestSelect(3, false)
	d.key(tea.KeyMsg{Type: tea.KeyDown}, nil)
	if !d.key(tea.KeyMsg{Type: tea.KeyEnter}, nil) {
		t.Fatal("enter did not resolve the dialog")
	}
	if res := <-d.reply; !res.OK || res.Index != 1 {
		t.Errorf("resolved with %+v", res)
	}

	d = newTestSelect(3, false)
	if !d.key(tea.KeyMsg{Type: tea.KeyEsc}, nil) {
		t.Fatal("esc did not resolve the dialog")
	}
	if res := <-d.reply; res.OK || res.Index != -1 {
		t.Errorf("cancel produced %+v", res)
	}
}

// A pasted secret lands in the field whole. If a paste answered the prompt, an
// API key copied with a trailing newline would submit itself half-typed.
func TestInputDialogPasteDoesNotSubmit(t *testing.T) {
	d := &inputDialog{baseDialog: baseDialog{reply: make(chan dialogResult, 1)}}
	if d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("sk-abc\n"), Paste: true}, nil) {
		t.Fatal("a paste resolved the dialog")
	}
	if string(d.value) != "sk-abc\n" {
		t.Errorf("paste mangled the value: %q", string(d.value))
	}
}

func TestInputDialogEditing(t *testing.T) {
	d := &inputDialog{baseDialog: baseDialog{reply: make(chan dialogResult, 1)}}
	for _, r := range "hello" {
		d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, nil)
	}
	d.key(tea.KeyMsg{Type: tea.KeySpace}, nil)
	d.key(tea.KeyMsg{Type: tea.KeyCtrlA}, nil)
	d.key(tea.KeyMsg{Type: tea.KeyCtrlD}, nil)
	if string(d.value) != "ello " {
		t.Fatalf("value = %q", string(d.value))
	}
	d.key(tea.KeyMsg{Type: tea.KeyCtrlU}, nil)
	if len(d.value) != 0 {
		t.Errorf("ctrl+u left %q", string(d.value))
	}
}

func TestConfirmDialogTogglesAndAnswers(t *testing.T) {
	d := &confirmDialog{baseDialog: baseDialog{reply: make(chan dialogResult, 1)}}
	d.key(tea.KeyMsg{Type: tea.KeyRight}, nil)
	if !d.yes {
		t.Error("right did not move to the confirm button")
	}
	if !d.key(tea.KeyMsg{Type: tea.KeyEnter}, nil) {
		t.Fatal("enter did not resolve the dialog")
	}
	if res := <-d.reply; !res.OK {
		t.Errorf("resolved with %+v", res)
	}

	d = &confirmDialog{baseDialog: baseDialog{reply: make(chan dialogResult, 1)}}
	if !d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}, nil) {
		t.Fatal("n did not resolve the dialog")
	}
	if res := <-d.reply; res.OK {
		t.Errorf("n answered yes: %+v", res)
	}
}

// --- hints ---

// The greeting advertises keys, so it has to read them from the live bindings
// rather than repeat what the defaults used to be.
func TestHintsFollowTheBindings(t *testing.T) {
	user := keybindings.NewConfig()
	user.SetKeys(string(keybindings.AppInterrupt), keybindings.Keys{"ctrl+x"})
	a := &app{keys: keybindings.New(user)}
	if got := a.hints(); !strings.Contains(got, "Ctrl+X stops the agent") {
		t.Errorf("hints did not follow the rebinding: %q", got)
	}

	user = keybindings.NewConfig()
	user.SetKeys(string(keybindings.AppClear), nil)
	a = &app{keys: keybindings.New(user)}
	if got := a.hints(); strings.Contains(got, "twice to quit") {
		t.Errorf("hints advertised an unbound action: %q", got)
	}
}

// hasRow reports whether the table has a row pairing a key with a description.
// The columns are padded to the widest label, so the gap between them is not
// something a test should pin down.
func hasRow(table, key, desc string) bool {
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, key+" ") && strings.HasSuffix(line, desc) {
			return true
		}
	}
	return false
}

// /help prints the key table, so it has to be generated from the bindings too.
// A help screen naming a key the user rebound away is worse than none.
func TestHotkeysFollowTheBindings(t *testing.T) {
	h := &host{}
	if got := h.Hotkeys(); !strings.Contains(got, "Enter") || !strings.Contains(got, "send the message") {
		t.Fatalf("default table is missing the basics:\n%s", got)
	}

	user := keybindings.NewConfig()
	user.SetKeys(string(keybindings.InputSubmit), keybindings.Keys{"ctrl+s"})
	user.SetKeys(string(keybindings.AppSuspend), nil)
	h = &host{keys: keybindings.New(user)}
	got := h.Hotkeys()
	if !hasRow(got, "Ctrl+S", "send the message") {
		t.Errorf("the rebound submit key is not listed:\n%s", got)
	}
	if strings.Contains(got, "suspend tau") {
		t.Errorf("an unbound action is still listed:\n%s", got)
	}
}
