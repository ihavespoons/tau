package tui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

	theme Theme
	keys  *keybindings.Manager
}

// newEditor builds the prompt. A nil manager means tau's defaults, which keeps
// the editor constructible in tests that are not about keys.
func newEditor(theme Theme, keys *keybindings.Manager) *editor {
	if keys == nil {
		keys = keybindings.New(nil)
	}
	return &editor{width: 80, theme: theme, keys: keys}
}

// Value returns the current text.
func (e *editor) Value() string { return string(e.text) }

// Empty reports whether there is nothing to submit.
func (e *editor) Empty() bool { return strings.TrimSpace(string(e.text)) == "" }

// SetValue replaces the text and puts the cursor at the end.
func (e *editor) SetValue(s string) {
	e.text = []rune(s)
	e.cursor = len(e.text)
}

// Reset clears the editor and stops history browsing.
func (e *editor) Reset() {
	e.text = nil
	e.cursor = 0
	e.histIdx = len(e.history)
	e.draft = ""
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
		e.insert(msg.Runes)
		return "", false
	}

	// Typing beats bindings. A key that produces a character inserts it, and no
	// keybindings.json gets a say: a config that shadowed the letter "a" would
	// leave its author unable to type the command that undoes it. Alt-modified
	// runes are held back, because that is where the word motions live.
	switch {
	case msg.Type == tea.KeyRunes && !msg.Alt:
		e.insert(msg.Runes)
		return "", false
	case msg.Type == tea.KeySpace && !msg.Alt:
		e.insert([]rune{' '})
		return "", false
	}

	// First match wins. The order below resolves the overlaps in Pi's defaults
	// — ctrl+u is delete-to-line-start here rather than a tree filter, and the
	// word motions are checked before the character ones.
	key := keyID(msg)
	switch {
	case e.bound(key, keybindings.InputSubmit):
		if e.Empty() {
			return "", false
		}
		return e.Value(), true

	case e.bound(key, keybindings.InputNewLine):
		e.insert([]rune{'\n'})

	case e.bound(key, keybindings.EditorDeleteCharBackward):
		e.deleteBackward()
	case e.bound(key, keybindings.EditorDeleteCharForward):
		e.deleteForward()
	case e.bound(key, keybindings.EditorDeleteWordBackward):
		start := e.wordStart()
		e.text = append(append([]rune{}, e.text[:start]...), e.text[e.cursor:]...)
		e.cursor = start
	case e.bound(key, keybindings.EditorDeleteWordForward):
		e.text = append(append([]rune{}, e.text[:e.cursor]...), e.text[e.wordEnd():]...)
	case e.bound(key, keybindings.EditorDeleteToLineStart):
		// Both bounds have to be read before the buffer shrinks: recomputing
		// one afterwards would measure the new text with the old cursor.
		start := e.lineStart()
		e.text = append(append([]rune{}, e.text[:start]...), e.text[e.cursor:]...)
		e.cursor = start
	case e.bound(key, keybindings.EditorDeleteToLineEnd):
		e.text = append(append([]rune{}, e.text[:e.cursor]...), e.text[e.lineEnd():]...)

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

func (e *editor) deleteBackward() {
	if e.cursor == 0 {
		return
	}
	e.text = append(append([]rune{}, e.text[:e.cursor-1]...), e.text[e.cursor:]...)
	e.cursor--
}

func (e *editor) deleteForward() {
	if e.cursor >= len(e.text) {
		return
	}
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
	prompt := e.theme.Prompt.Render("› ")
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
