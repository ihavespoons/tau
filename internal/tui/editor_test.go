package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newTestEditor() *editor {
	e := newEditor(DefaultTheme())
	e.SetWidth(60)
	return e
}

// typeText feeds a string as individual key events, the way a terminal
// delivers typing.
func typeText(e *editor, s string) (submitted string, ok bool) {
	for _, r := range s {
		switch r {
		case '\n':
			submitted, ok = e.Update(tea.KeyMsg{Type: tea.KeyEnter})
		case ' ':
			submitted, ok = e.Update(tea.KeyMsg{Type: tea.KeySpace})
		default:
			submitted, ok = e.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
		if ok {
			return submitted, true
		}
	}
	return "", false
}

func TestEnterSubmits(t *testing.T) {
	e := newTestEditor()
	got, ok := typeText(e, "hello world\n")
	if !ok {
		t.Fatal("Enter did not submit")
	}
	if got != "hello world" {
		t.Errorf("submitted %q", got)
	}
}

func TestEnterOnEmptyDoesNotSubmit(t *testing.T) {
	e := newTestEditor()
	if _, ok := e.Update(tea.KeyMsg{Type: tea.KeyEnter}); ok {
		t.Error("an empty prompt must not submit")
	}
	typeText(e, "   ")
	if _, ok := e.Update(tea.KeyMsg{Type: tea.KeyEnter}); ok {
		t.Error("whitespace-only input must not submit")
	}
}

// Pasting a multi-line snippet is not a decision to send it. If a paste
// containing a newline submitted, half of every pasted stack trace would be
// fired at the model.
func TestPasteWithNewlinesNeverSubmits(t *testing.T) {
	e := newTestEditor()
	_, ok := e.Update(tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune("line one\nline two\nline three"),
		Paste: true,
	})
	if ok {
		t.Fatal("a paste must never submit")
	}
	if got := e.Value(); got != "line one\nline two\nline three" {
		t.Errorf("paste mangled the text: %q", got)
	}
}

func TestAltEnterInsertsNewline(t *testing.T) {
	e := newTestEditor()
	typeText(e, "first")
	if _, ok := e.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true}); ok {
		t.Fatal("Alt+Enter must not submit")
	}
	typeText(e, "second")
	if got := e.Value(); got != "first\nsecond" {
		t.Errorf("got %q", got)
	}
}

func TestCtrlJInsertsNewline(t *testing.T) {
	e := newTestEditor()
	typeText(e, "a")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	typeText(e, "b")
	if got := e.Value(); got != "a\nb" {
		t.Errorf("got %q", got)
	}
}

func TestWordDeleteAndKillMotions(t *testing.T) {
	e := newTestEditor()
	typeText(e, "alpha beta gamma")

	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	if got := e.Value(); got != "alpha beta " {
		t.Errorf("Ctrl+W: %q", got)
	}

	e.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if got := e.Value(); got != "" {
		t.Errorf("Ctrl+U should kill to line start, got %q", got)
	}

	typeText(e, "keep this")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if got := e.Value(); got != "" {
		t.Errorf("Ctrl+K should kill to line end, got %q", got)
	}
}

// Ctrl+K on a multi-line buffer must stop at the newline, not eat the rest of
// the message.
func TestKillToEndStopsAtTheLineBreak(t *testing.T) {
	e := newTestEditor()
	e.SetValue("first line\nsecond line")
	e.cursor = 5 // inside "first"

	e.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	if got := e.Value(); got != "first\nsecond line" {
		t.Errorf("got %q", got)
	}
}

func TestHistoryNavigatesSubmittedPrompts(t *testing.T) {
	e := newTestEditor()
	for _, prompt := range []string{"first", "second", "third"} {
		e.Remember(prompt)
		e.Reset()
	}

	e.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "third" {
		t.Errorf("first Up should recall the newest prompt, got %q", got)
	}
	e.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "second" {
		t.Errorf("second Up: %q", got)
	}
	e.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := e.Value(); got != "third" {
		t.Errorf("Down should walk back toward the present, got %q", got)
	}
	e.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := e.Value(); got != "" {
		t.Errorf("Down past the newest entry should restore the draft, got %q", got)
	}
}

// Browsing history must not destroy what was already typed.
func TestHistoryRestoresTheDraft(t *testing.T) {
	e := newTestEditor()
	e.Remember("old prompt")
	e.Reset()
	typeText(e, "half written")

	e.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "old prompt" {
		t.Fatalf("Up should show history, got %q", got)
	}
	e.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := e.Value(); got != "half written" {
		t.Errorf("the draft was lost: %q", got)
	}
}

// Up inside a multi-line message moves the cursor rather than replacing the
// text with a history entry.
func TestUpMovesWithinAMultiLineMessage(t *testing.T) {
	e := newTestEditor()
	e.Remember("history entry")
	e.Reset()
	e.SetValue("line one\nline two")

	e.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "line one\nline two" {
		t.Fatalf("Up replaced a multi-line draft with history: %q", got)
	}
	if e.cursor > len("line one") {
		t.Errorf("cursor should be on the first line, got %d", e.cursor)
	}
}

func TestRememberSkipsConsecutiveDuplicates(t *testing.T) {
	e := newTestEditor()
	e.Remember("same")
	e.Remember("same")
	if len(e.history) != 1 {
		t.Errorf("consecutive duplicates should collapse, got %v", e.history)
	}
}

func TestViewShowsPromptGutterAndWraps(t *testing.T) {
	e := newEditor(DefaultTheme())
	e.SetWidth(20)
	e.SetValue(strings.Repeat("word ", 12))

	view := e.View(true)
	if !strings.Contains(view, "›") {
		t.Error("the prompt gutter is missing")
	}
	if strings.Count(view, "\n") < 2 {
		t.Errorf("long input should wrap across lines:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if w := displayWidth(line); w > 20 {
			t.Errorf("line exceeds the terminal width (%d): %q", w, line)
		}
	}
}

func TestCursorMotions(t *testing.T) {
	e := newTestEditor()
	typeText(e, "abc")

	e.Update(tea.KeyMsg{Type: tea.KeyLeft})
	e.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := e.Value(); got != "ac" {
		t.Errorf("backspace at an interior cursor: %q", got)
	}

	e.Update(tea.KeyMsg{Type: tea.KeyHome})
	e.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if got := e.Value(); got != "c" {
		t.Errorf("delete forward from line start: %q", got)
	}

	e.Update(tea.KeyMsg{Type: tea.KeyEnd})
	typeText(e, "d")
	if got := e.Value(); got != "cd" {
		t.Errorf("typing at end of line: %q", got)
	}
}

// Multi-byte input must move by rune, not by byte, or the cursor lands inside
// a character.
func TestUnicodeIsHandledByRune(t *testing.T) {
	e := newTestEditor()
	e.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("héllo — 世界")})
	e.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := e.Value(); got != "héllo — 世" {
		t.Errorf("backspace removed the wrong amount: %q", got)
	}
}
