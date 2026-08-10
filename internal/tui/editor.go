package tui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/keybindings"
)

// editor is tau's multi-line input.
//
// It is hand-written rather than built on bubbles/textarea because the editor
// is the surface a daily driver touches most: readline motions, submit-versus-
// newline, paste that never submits, and history all have to behave exactly
// right, and owning the key table is the only way to guarantee that.
type editor struct {
	text   []rune
	cursor int
	width  int

	// history holds previously submitted prompts, oldest first.
	history []string
	// histIdx indexes history while browsing; len(history) means "not
	// browsing", and draft holds what was typed before browsing started.
	histIdx int
	draft   string

	// undos holds buffer snapshots, oldest last.
	undos []undoState
	// run names the kind of edit the last key made, so a run of typing or a
	// run of backspaces collapses into one undo step rather than one per
	// keystroke. Undo should take back a word, not a letter.
	run editRun

	// kills holds killed text, most recent last.
	kills []string
	// yank describes the span the last yank inserted, so yank-pop can replace
	// it with the kill before it.
	yank yankState

	theme Theme
	keys  *keybindings.Manager
}

// undoState is a buffer snapshot.
type undoState struct {
	text   []rune
	cursor int
}

// yankState marks the text a yank just inserted. at < 0 means the last action
// was not a yank, which is what makes yank-pop only work straight after one.
type yankState struct{ at, end, idx int }

// editRun groups consecutive edits of the same kind into one undo step.
type editRun int

const (
	runNone editRun = iota
	runType
	runDelete
)

// undoMax and killRingMax bound what the editor remembers. Both are per
// session and small; the point is to survive a mistake, not to be a database.
const (
	undoMax     = 128
	killRingMax = 16
)

// newEditor builds the prompt. A nil manager means tau's defaults, which keeps
// the editor constructible in tests that are not about keys.
func newEditor(theme Theme, keys *keybindings.Manager) *editor {
	if keys == nil {
		keys = keybindings.New(nil)
	}
	return &editor{width: 80, theme: theme, keys: keys, yank: yankState{at: -1}}
}

// Value returns the current text.
func (e *editor) Value() string { return string(e.text) }

// Empty reports whether there is nothing to submit.
func (e *editor) Empty() bool { return strings.TrimSpace(string(e.text)) == "" }

// BashMode reports whether the line will run as a shell command rather than go
// to the model.
func (e *editor) BashMode() bool {
	_, _, ok := coding.ParseUserBash(string(e.text))
	return ok
}

// SetValue replaces the text and puts the cursor at the end.
func (e *editor) SetValue(s string) {
	e.text = []rune(s)
	e.cursor = len(e.text)
}

// Reset clears the editor and stops history browsing.
//
// The undo stack goes with it: undoing across a submission would resurrect a
// prompt that has already been sent, which is not what the key means. The kill
// ring stays — killing text in one prompt and yanking it into the next is the
// reason a kill ring is separate from undo.
func (e *editor) Reset() {
	e.text = nil
	e.cursor = 0
	e.histIdx = len(e.history)
	e.draft = ""
	e.undos = nil
	e.run = runNone
	e.yank = yankState{at: -1}
}

// snapshot records the buffer so the next edit can be undone.
func (e *editor) snapshot() {
	e.undos = append(e.undos, undoState{
		text:   append([]rune(nil), e.text...),
		cursor: e.cursor,
	})
	if len(e.undos) > undoMax {
		e.undos = e.undos[len(e.undos)-undoMax:]
	}
}

// undo restores the buffer to before the last edit.
func (e *editor) undo() {
	if len(e.undos) == 0 {
		return
	}
	last := e.undos[len(e.undos)-1]
	e.undos = e.undos[:len(e.undos)-1]
	e.text, e.cursor = last.text, last.cursor
	e.run = runNone
	e.histIdx = len(e.history)
}

// kill pushes removed text onto the kill ring. Single characters are left out,
// the way readline leaves them out: ctrl+h is a correction, not a cut.
func (e *editor) kill(s string) {
	if s == "" {
		return
	}
	e.kills = append(e.kills, s)
	if len(e.kills) > killRingMax {
		e.kills = e.kills[len(e.kills)-killRingMax:]
	}
}

// cut removes [from,to) and puts it on the kill ring.
func (e *editor) cut(from, to int) {
	if from >= to {
		return
	}
	e.snapshot()
	e.run = runNone
	e.kill(string(e.text[from:to]))
	e.text = append(append([]rune{}, e.text[:from]...), e.text[to:]...)
	e.cursor = from
}

// yankKill inserts a kill and remembers where it went, so yank-pop can swap it.
func (e *editor) yankKill(idx int) {
	text := []rune(e.kills[idx])
	start := e.cursor
	e.insert(text)
	e.yank = yankState{at: start, end: start + len(text), idx: idx}
}

// yankPop replaces what the previous yank inserted with the kill before it,
// walking back through the ring on each press.
//
// It does nothing unless a yank was the immediately preceding action, which is
// what stops the key from mangling text the user typed or pasted.
func (e *editor) yankPop(last yankState) {
	if last.at < 0 || len(e.kills) < 2 {
		return
	}
	if last.at > len(e.text) || last.end > len(e.text) {
		return
	}

	idx := last.idx - 1
	if idx < 0 {
		idx = len(e.kills) - 1
	}
	text := []rune(e.kills[idx])

	out := make([]rune, 0, len(e.text)-(last.end-last.at)+len(text))
	out = append(out, e.text[:last.at]...)
	out = append(out, text...)
	out = append(out, e.text[last.end:]...)

	e.text = out
	e.cursor = last.at + len(text)
	e.yank = yankState{at: last.at, end: e.cursor, idx: idx}
}

// beginRun snapshots when the kind of edit changes, so consecutive keystrokes
// of the same kind share one undo step.
func (e *editor) beginRun(kind editRun) {
	if e.run != kind {
		e.snapshot()
		e.run = kind
	}
}

// typeRunes inserts typed characters as part of a single undo step.
func (e *editor) typeRunes(rs []rune) {
	e.beginRun(runType)
	e.yank = yankState{at: -1}
	e.insert(rs)
}

// Replace swaps the whole buffer, keeping the change undoable. It is what an
// external editor comes back to: the text it returns is one edit, and the user
// has to be able to take it back.
func (e *editor) Replace(s string) {
	e.snapshot()
	e.run = runNone
	e.yank = yankState{at: -1}
	e.SetValue(s)
}

// Cursor is where the caret sits, counted in runes.
func (e *editor) Cursor() int { return e.cursor }

// ReplaceRange swaps the runes in [from,to) and leaves the caret just after
// what went in, as one undoable edit.
//
// Accepting a completion needs this rather than SetValue: the token being
// completed is a span in the middle of a line, and putting the caret at the end
// of the buffer would move it away from what was just typed.
func (e *editor) ReplaceRange(from, to int, s string) {
	from = min(max(from, 0), len(e.text))
	to = min(max(to, from), len(e.text))

	e.snapshot()
	e.run = runNone
	e.yank = yankState{at: -1}

	ins := []rune(s)
	out := make([]rune, 0, len(e.text)-(to-from)+len(ins))
	out = append(out, e.text[:from]...)
	out = append(out, ins...)
	out = append(out, e.text[to:]...)

	e.text = out
	e.cursor = from + len(ins)
}

// SetWidth sets the wrap width for rendering.
func (e *editor) SetWidth(w int) {
	if w < 10 {
		w = 10
	}
	e.width = w
}

// Remember records a submitted line in the history.
func (e *editor) Remember(s string) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == s {
		e.histIdx = n
		return
	}
	e.history = append(e.history, s)
	e.histIdx = len(e.history)
}

// bound reports whether a key triggers a binding.
func (e *editor) bound(key string, id keybindings.Binding) bool {
	return bound(e.keys, key, id)
}

// Update applies a key. submit is the text to send when the key was bound to
// tui.input.submit on a non-empty buffer; ok reports whether a submission
// happened.
func (e *editor) Update(msg tea.KeyMsg) (submit string, ok bool) {
	// A paste arrives as one key event whose runes may contain newlines. It
	// must never submit — pasting a multi-line snippet is not a decision to
	// send it.
	if msg.Paste {
		// A paste is one action however much it carries, so it gets its own
		// undo step rather than joining the typing run around it.
		e.snapshot()
		e.run = runNone
		e.yank = yankState{at: -1}
		e.insert(msg.Runes)
		return "", false
	}

	// Typing beats bindings. A key that produces a character inserts it, and no
	// keybindings.json gets a say: a config that shadowed the letter "a" would
	// leave its author unable to type the command that undoes it. Alt-modified
	// runes are held back, because that is where the word motions live.
	switch {
	case msg.Type == tea.KeyRunes && !msg.Alt:
		e.typeRunes(msg.Runes)
		return "", false
	case msg.Type == tea.KeySpace && !msg.Alt:
		e.typeRunes([]rune{' '})
		return "", false
	}

	// First match wins. The order below resolves the overlaps in Pi's defaults
	// — ctrl+u is delete-to-line-start here rather than a tree filter, and the
	// word motions are checked before the character ones.
	key := keyID(msg)

	// Yank-pop is the one key that depends on what came just before it, so the
	// state is read here and cleared for every other branch in one place.
	lastYank := e.yank
	e.yank = yankState{at: -1}

	switch {
	case e.bound(key, keybindings.InputSubmit):
		if e.Empty() {
			return "", false
		}
		return e.Value(), true

	case e.bound(key, keybindings.InputNewLine):
		e.snapshot()
		e.run = runNone
		e.insert([]rune{'\n'})

	case e.bound(key, keybindings.EditorUndo):
		e.undo()
	case e.bound(key, keybindings.EditorYank):
		if len(e.kills) > 0 {
			e.snapshot()
			e.run = runNone
			e.yankKill(len(e.kills) - 1)
		}
	case e.bound(key, keybindings.EditorYankPop):
		e.yankPop(lastYank)

	case e.bound(key, keybindings.EditorDeleteCharBackward):
		e.deleteBackward()
	case e.bound(key, keybindings.EditorDeleteCharForward):
		e.deleteForward()
	case e.bound(key, keybindings.EditorDeleteWordBackward):
		e.cut(e.wordStart(), e.cursor)
	case e.bound(key, keybindings.EditorDeleteWordForward):
		// The cursor stays put, so the end is read before the buffer shrinks.
		e.cut(e.cursor, e.wordEnd())
		e.cursor = min(e.cursor, len(e.text))
	case e.bound(key, keybindings.EditorDeleteToLineStart):
		// Both bounds have to be read before the buffer shrinks: recomputing
		// one afterwards would measure the new text with the old cursor.
		e.cut(e.lineStart(), e.cursor)
	case e.bound(key, keybindings.EditorDeleteToLineEnd):
		at := e.cursor
		e.cut(e.cursor, e.lineEnd())
		e.cursor = at

	case e.bound(key, keybindings.EditorCursorWordLeft):
		e.cursor = e.wordStart()
	case e.bound(key, keybindings.EditorCursorWordRight):
		e.cursor = e.wordEnd()
	case e.bound(key, keybindings.EditorCursorLeft):
		if e.cursor > 0 {
			e.cursor--
		}
	case e.bound(key, keybindings.EditorCursorRight):
		if e.cursor < len(e.text) {
			e.cursor++
		}
	case e.bound(key, keybindings.EditorCursorLineStart):
		e.cursor = e.lineStart()
	case e.bound(key, keybindings.EditorCursorLineEnd):
		e.cursor = e.lineEnd()
	case e.bound(key, keybindings.EditorCursorUp):
		e.moveVertical(-1)
	case e.bound(key, keybindings.EditorCursorDown):
		e.moveVertical(1)
	}
	return "", false
}

func (e *editor) insert(rs []rune) {
	if len(rs) == 0 {
		return
	}
	out := make([]rune, 0, len(e.text)+len(rs))
	out = append(out, e.text[:e.cursor]...)
	out = append(out, rs...)
	out = append(out, e.text[e.cursor:]...)
	e.text = out
	e.cursor += len(rs)
	e.histIdx = len(e.history)
}

// deleteBackward and deleteForward remove one character. They do not touch the
// kill ring: readline treats a single character as a correction rather than a
// cut, and putting one there would push out text the user meant to keep.
func (e *editor) deleteBackward() {
	if e.cursor == 0 {
		return
	}
	e.beginRun(runDelete)
	e.text = append(append([]rune{}, e.text[:e.cursor-1]...), e.text[e.cursor:]...)
	e.cursor--
}

func (e *editor) deleteForward() {
	if e.cursor >= len(e.text) {
		return
	}
	e.beginRun(runDelete)
	e.text = append(append([]rune{}, e.text[:e.cursor]...), e.text[e.cursor+1:]...)
}

// lineStart is the index just after the previous newline.
func (e *editor) lineStart() int {
	for i := e.cursor - 1; i >= 0; i-- {
		if e.text[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// lineEnd is the index of the next newline, or the end of the text.
func (e *editor) lineEnd() int {
	for i := e.cursor; i < len(e.text); i++ {
		if e.text[i] == '\n' {
			return i
		}
	}
	return len(e.text)
}

// wordStart is the start of the word before the cursor.
func (e *editor) wordStart() int {
	i := e.cursor
	for i > 0 && isWordBreak(e.text[i-1]) {
		i--
	}
	for i > 0 && !isWordBreak(e.text[i-1]) {
		i--
	}
	return i
}

// wordEnd is the end of the word after the cursor.
func (e *editor) wordEnd() int {
	i := e.cursor
	for i < len(e.text) && isWordBreak(e.text[i]) {
		i++
	}
	for i < len(e.text) && !isWordBreak(e.text[i]) {
		i++
	}
	return i
}

func isWordBreak(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("/\\.,;:()[]{}\"'`", r)
}

// moveVertical moves between logical lines, or steps through history when
// there is no line to move to. Sharing Up/Down this way is what makes a
// one-line prompt feel like a shell and a multi-line one feel like an editor.
func (e *editor) moveVertical(dir int) {
	col := e.cursor - e.lineStart()

	if dir < 0 {
		if start := e.lineStart(); start > 0 {
			prevStart := 0
			for i := start - 2; i >= 0; i-- {
				if e.text[i] == '\n' {
					prevStart = i + 1
					break
				}
			}
			e.cursor = min(prevStart+col, start-1)
			return
		}
		e.historyPrev()
		return
	}

	if end := e.lineEnd(); end < len(e.text) {
		nextStart := end + 1
		nextEnd := len(e.text)
		for i := nextStart; i < len(e.text); i++ {
			if e.text[i] == '\n' {
				nextEnd = i
				break
			}
		}
		e.cursor = min(nextStart+col, nextEnd)
		return
	}
	e.historyNext()
}

func (e *editor) historyPrev() {
	if len(e.history) == 0 || e.histIdx == 0 {
		return
	}
	if e.histIdx == len(e.history) {
		e.draft = e.Value()
	}
	e.histIdx--
	e.SetValue(e.history[e.histIdx])
}

func (e *editor) historyNext() {
	if e.histIdx >= len(e.history) {
		return
	}
	e.histIdx++
	if e.histIdx == len(e.history) {
		e.SetValue(e.draft)
		return
	}
	e.SetValue(e.history[e.histIdx])
}

// View renders the editor with a prompt gutter and a block cursor.
func (e *editor) View(focused bool) string {
	// The gutter changes colour the moment the line becomes a shell command,
	// so there is never a question about where Enter is going to send it.
	style := e.theme.Prompt
	if e.BashMode() {
		style = e.theme.BashMode
	}
	prompt := style.Render("› ")
	cont := e.theme.Dim.Render("  ")
	avail := e.width - 2
	if avail < 10 {
		avail = 10
	}

	// Insert the cursor as a styled cell so it survives wrapping.
	var b strings.Builder
	for i, r := range e.text {
		if focused && i == e.cursor {
			b.WriteString(cursorCell(r, e.theme))
			continue
		}
		b.WriteRune(r)
	}
	if focused && e.cursor >= len(e.text) {
		b.WriteString(cursorCell(' ', e.theme))
	}

	var out []string
	for i, logical := range strings.Split(b.String(), "\n") {
		wrapped := wrapANSI(logical, avail)
		for j, line := range wrapped {
			gutter := cont
			if i == 0 && j == 0 {
				gutter = prompt
			}
			out = append(out, gutter+line)
		}
	}
	if len(out) == 0 {
		out = []string{prompt}
	}
	return strings.Join(out, "\n")
}

// cursorCell renders one character as the cursor. A newline under the cursor
// is drawn as a space so the block stays visible at end of line.
func cursorCell(r rune, theme Theme) string {
	if r == '\n' || r == 0 {
		r = ' '
	}
	return lipgloss.NewStyle().Reverse(true).Render(string(r))
}

// Lines counts the rendered height of the editor.
func (e *editor) Lines(focused bool) int {
	return strings.Count(e.View(focused), "\n") + 1
}
