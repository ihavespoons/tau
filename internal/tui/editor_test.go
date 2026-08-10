package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/keybindings"
)

func newTestEditor() *editor {
	e := newEditor(DefaultTheme(), nil)
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

// Alt+Enter belongs to the app, not the editor: it queues a follow-up. The
// editor has to leave it alone entirely — inserting a newline here would put a
// stray blank line in every message sent that way.
func TestAltEnterIsNotTheEditors(t *testing.T) {
	e := newTestEditor()
	typeText(e, "first")
	if _, ok := e.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true}); ok {
		t.Fatal("Alt+Enter must not submit from the editor")
	}
	if got := e.Value(); got != "first" {
		t.Errorf("Alt+Enter changed the buffer: %q", got)
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
	e := newEditor(DefaultTheme(), nil)
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

// --- undo ---

// A run of typing is one undo step. Undoing a letter at a time would make the
// key useless for the thing people reach for it after: typing the wrong word.
func TestUndoTakesBackAWholeTypingRun(t *testing.T) {
	e := newTestEditor()
	typeText(e, "hello")
	typeText(e, " world")

	e.undo()
	if got := e.Value(); got != "" {
		t.Errorf("after undo = %q, want the buffer empty", got)
	}
}

// Changing the kind of edit ends the run, so each kind can be taken back on
// its own.
func TestUndoSeparatesTypingFromDeleting(t *testing.T) {
	e := newTestEditor()
	typeText(e, "hello")
	e.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	e.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := e.Value(); got != "hel" {
		t.Fatalf("setup produced %q", got)
	}

	e.undo()
	if got := e.Value(); got != "hello" {
		t.Errorf("first undo = %q, want the deletions taken back", got)
	}
	e.undo()
	if got := e.Value(); got != "" {
		t.Errorf("second undo = %q, want the typing taken back", got)
	}
}

func TestUndoRestoresTheCursor(t *testing.T) {
	e := newTestEditor()
	typeText(e, "hello")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlA}) // to line start
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlK}) // kill to line end

	e.undo()
	if e.Value() != "hello" {
		t.Fatalf("undo did not restore the text: %q", e.Value())
	}
	if e.cursor != 0 {
		t.Errorf("cursor = %d, want 0 — where it was when the kill happened", e.cursor)
	}
}

func TestUndoOnAnEmptyStackDoesNothing(t *testing.T) {
	e := newTestEditor()
	e.undo()
	if got := e.Value(); got != "" {
		t.Errorf("value = %q", got)
	}
}

// Submitting ends the undo history: taking back a prompt that has already been
// sent is not what the key means.
func TestUndoDoesNotReachAcrossASubmission(t *testing.T) {
	e := newTestEditor()
	typeText(e, "sent\n")
	e.Reset()

	typeText(e, "next")
	e.undo()
	if got := e.Value(); got != "" {
		t.Errorf("value = %q, want only the new text taken back", got)
	}
	e.undo()
	if got := e.Value(); got != "" {
		t.Errorf("undo reached back into a submitted prompt: %q", got)
	}
}

func TestUndoIsBoundToItsKey(t *testing.T) {
	e := newTestEditor()
	if !e.bound("ctrl+-", keybindings.EditorUndo) {
		t.Error("ctrl+- is not bound to undo")
	}
}

// --- kill ring ---

func TestKillingWordsFillsTheRing(t *testing.T) {
	e := newTestEditor()
	typeText(e, "alpha beta")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW})

	if len(e.kills) != 1 || e.kills[0] != "beta" {
		t.Fatalf("kill ring = %q", e.kills)
	}
	if got := e.Value(); got != "alpha " {
		t.Errorf("buffer = %q", got)
	}
}

// A single character is a correction, not a cut: putting it on the ring would
// push out text the user meant to keep.
func TestSingleCharacterDeletesDoNotFillTheRing(t *testing.T) {
	e := newTestEditor()
	typeText(e, "abc")
	e.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	e.Update(tea.KeyMsg{Type: tea.KeyDelete})

	if len(e.kills) != 0 {
		t.Errorf("kill ring = %q, want it untouched", e.kills)
	}
}

func TestYankInsertsTheMostRecentKill(t *testing.T) {
	e := newTestEditor()
	typeText(e, "alpha beta")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW}) // kill "beta"
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlY}) // yank it back

	if got := e.Value(); got != "alpha beta" {
		t.Errorf("value = %q, want the kill restored", got)
	}
}

func TestYankPopWalksBackThroughTheRing(t *testing.T) {
	e := newTestEditor()
	typeText(e, "one two")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW}) // kills "two"
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW}) // kills "one "
	e.Reset()

	e.Update(tea.KeyMsg{Type: tea.KeyCtrlY}) // yanks "one "
	if got := e.Value(); got != "one " {
		t.Fatalf("yank = %q, want the most recent kill", got)
	}

	e.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}, Alt: true})
	if got := e.Value(); got != "two" {
		t.Errorf("yank-pop = %q, want the kill before it", got)
	}
}

// Yank-pop only means anything straight after a yank. Anywhere else it would
// silently replace text the user typed.
func TestYankPopDoesNothingWithoutAYank(t *testing.T) {
	e := newTestEditor()
	typeText(e, "one two")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	e.Reset()

	typeText(e, "typed")
	e.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}, Alt: true})
	if got := e.Value(); got != "typed" {
		t.Errorf("value = %q, want it left alone", got)
	}
}

// The ring survives a submission, so text killed in one prompt can be yanked
// into the next.
func TestKillRingSurvivesReset(t *testing.T) {
	e := newTestEditor()
	typeText(e, "alpha beta")
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlW})
	e.Reset()

	e.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	if got := e.Value(); got != "beta" {
		t.Errorf("value = %q, want the kill still available", got)
	}
}

func TestYankOnAnEmptyRingDoesNothing(t *testing.T) {
	e := newTestEditor()
	e.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	if got := e.Value(); got != "" {
		t.Errorf("value = %q", got)
	}
}
