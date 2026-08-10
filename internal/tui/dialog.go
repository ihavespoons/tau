package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/keybindings"
)

// dialogResult is what a blocked caller receives when a dialog closes.
type dialogResult struct {
	// Index is the chosen option, or -1.
	Index int
	// Text is the entered text.
	Text string
	// OK is false when the user cancelled.
	OK bool
	// Action names the binding that closed the dialog, when it was closed by
	// one of the actions the caller registered rather than by choosing.
	//
	// Actions close the dialog rather than acting inside it. Opening a confirm
	// or an input from a dialog's own key handler would nest one modal inside
	// another while the asking goroutine is parked on the first one's reply —
	// which is the deadlock this design exists to avoid.
	Action keybindings.Binding
	// Indices are the chosen rows of a checklist, in display order. Order is
	// part of the answer: the list it configures is cycled in the order it is
	// written.
	Indices []int
}

// dialog is a modal that owns the keyboard while it is open.
//
// Dialogs are driven entirely from the render goroutine. The goroutine that
// asked for one is parked on the reply channel until close() sends, which is
// what makes the agent pause while the user is being asked something.
type dialog interface {
	// key handles a key press, reporting whether the dialog is finished. The
	// manager is passed in rather than stored on each dialog because dialogs are
	// constructed from four different places, and a modal that missed the
	// wiring would silently fall back to defaults the user had rebound.
	key(tea.KeyMsg, *keybindings.Manager) bool
	// view renders the dialog body.
	view(width int, theme Theme) []string
	// close resolves the dialog. It is safe to call more than once.
	close(res dialogResult)
	// title is shown above the body.
	title() string
}

// dialogStack is the modal stack. It lives on the render goroutine and is
// never touched from anywhere else, which is the invariant that makes a
// blocking dialog safe: the goroutine that resolves one is not the goroutine
// waiting on it.
type dialogStack struct {
	items []dialog
}

func (s *dialogStack) push(d dialog) { s.items = append(s.items, d) }

func (s *dialogStack) top() dialog {
	if len(s.items) == 0 {
		return nil
	}
	return s.items[len(s.items)-1]
}

// key routes a key to the topmost dialog, popping it once it resolves.
// It reports whether a dialog consumed the key.
func (s *dialogStack) key(msg tea.KeyMsg, km *keybindings.Manager) bool {
	n := len(s.items)
	if n == 0 {
		return false
	}
	if s.items[n-1].key(msg, km) {
		s.items = s.items[:n-1]
	}
	return true
}

// cancel resolves and removes a specific dialog, wherever it sits in the
// stack. It is idempotent: a dialog the user already answered is simply gone.
func (s *dialogStack) cancel(d dialog) {
	for i, open := range s.items {
		if open == d {
			open.close(dialogResult{Index: -1})
			s.items = append(s.items[:i], s.items[i+1:]...)
			return
		}
	}
}

// closeAll releases every open dialog, so nobody stays parked when the UI
// goes away.
func (s *dialogStack) closeAll() {
	for _, d := range s.items {
		d.close(dialogResult{Index: -1})
	}
	s.items = nil
}

// baseDialog carries the reply channel and one-shot resolution.
type baseDialog struct {
	reply    chan dialogResult
	heading  string
	message  string
	resolved bool
}

func (d *baseDialog) close(res dialogResult) {
	if d.resolved {
		return
	}
	d.resolved = true
	// The channel is buffered, so resolving never blocks the render loop even
	// if the waiter has already given up on its context.
	d.reply <- res
}

func (d *baseDialog) title() string { return d.heading }

// --- confirm ---

type confirmDialog struct {
	baseDialog
	confirmLabel string
	cancelLabel  string
	yes          bool
}

// key answers the prompt. Confirm and cancel come from the select bindings, but
// the left/right toggle is matched literally: there is no tui.* id for "switch
// button", and a two-option row is not a list to navigate.
func (d *confirmDialog) key(msg tea.KeyMsg, km *keybindings.Manager) bool {
	key := keyID(msg)
	switch {
	case bound(km, key, keybindings.SelectCancel):
		d.close(dialogResult{Index: -1})
		return true
	case bound(km, key, keybindings.SelectConfirm):
		d.close(dialogResult{Index: boolIndex(d.yes), OK: d.yes})
		return true
	case key == "left" || key == "right" || bound(km, key, keybindings.InputTab):
		d.yes = !d.yes
	case msg.Type == tea.KeyRunes && !msg.Paste:
		switch strings.ToLower(string(msg.Runes)) {
		case "y":
			d.close(dialogResult{Index: 0, OK: true})
			return true
		case "n":
			d.close(dialogResult{Index: 1})
			return true
		}
	}
	return false
}

func (d *confirmDialog) view(width int, theme Theme) []string {
	var out []string
	if d.message != "" {
		out = append(out, wrapBlock(d.message, width)...)
	}
	var yes, no string
	if d.yes {
		yes = theme.Selected.Render("▸ " + d.confirmLabel + "  ")
		no = theme.Dim.Render("  " + d.cancelLabel + "  ")
	} else {
		yes = theme.Dim.Render("  " + d.confirmLabel + "  ")
		no = theme.Selected.Render("▸ " + d.cancelLabel + "  ")
	}
	return append(out, "", yes+no)
}

func boolIndex(b bool) int {
	if b {
		return 0
	}
	return 1
}

// --- select ---

type selectDialog struct {
	baseDialog
	options    []extension.SelectOption
	filterable bool
	filter     string
	cursor     int
	// visible caps how many rows are drawn at once.
	visible int
	// matches indexes options that pass the filter.
	matches []int
	// actions are bindings that close the dialog and report themselves, so the
	// caller can act on the highlighted row and reopen. Nil for the plain
	// pick-one-thing case, which is every extension's use of it.
	actions []keybindings.Binding
	// emptyQueryActions only fire while the filter is empty, which is how a
	// destructive key can share a chord with a text-editing one.
	emptyQueryActions []keybindings.Binding
	// hint is drawn under the list to advertise the actions.
	hint string
}

func (d *selectDialog) refilter() {
	d.matches = d.matches[:0]
	needle := strings.ToLower(d.filter)
	for i, o := range d.options {
		if needle == "" ||
			strings.Contains(strings.ToLower(o.Label), needle) ||
			strings.Contains(strings.ToLower(o.Description), needle) {
			d.matches = append(d.matches, i)
		}
	}
	if d.cursor >= len(d.matches) {
		d.cursor = max(0, len(d.matches)-1)
	}
}

func (d *selectDialog) key(msg tea.KeyMsg, km *keybindings.Manager) bool {
	// Typing beats bindings, exactly as it does in the editor: while a filter is
	// open every printable key belongs to it, so a model named "p" stays
	// reachable no matter what tui.app.model.cycle is bound to.
	if d.filterable && !msg.Paste && !msg.Alt {
		switch msg.Type {
		case tea.KeyRunes:
			d.filter += string(msg.Runes)
			d.refilter()
			return false
		case tea.KeySpace:
			d.filter += " "
			d.refilter()
			return false
		}
	}

	key := keyID(msg)

	// Actions are checked before navigation: a chord bound to one here was
	// chosen deliberately for this list.
	if len(d.matches) > 0 {
		for _, b := range d.actions {
			if bound(km, key, b) {
				d.closeWithAction(b)
				return true
			}
		}
		// An action that only fires on an empty filter can share a chord with
		// a text-editing key without ever eating a keystroke meant for it.
		if d.filter == "" {
			for _, b := range d.emptyQueryActions {
				if bound(km, key, b) {
					d.closeWithAction(b)
					return true
				}
			}
		}
	}

	switch {
	case bound(km, key, keybindings.SelectCancel):
		d.close(dialogResult{Index: -1})
		return true
	case bound(km, key, keybindings.SelectConfirm):
		if len(d.matches) == 0 {
			d.close(dialogResult{Index: -1})
			return true
		}
		idx := d.matches[d.cursor]
		d.close(dialogResult{Index: idx, Text: d.options[idx].Value, OK: true})
		return true
	case bound(km, key, keybindings.SelectUp):
		d.move(-1)
	case bound(km, key, keybindings.SelectDown):
		d.move(1)
	case bound(km, key, keybindings.SelectPageUp):
		// A page is what is on screen, so the row under the cursor lands where
		// the window's top row was — the motion a reader expects from PgUp.
		d.move(-max(1, d.visible))
	case bound(km, key, keybindings.SelectPageDown):
		d.move(max(1, d.visible))
	case bound(km, key, keybindings.EditorDeleteCharBackward):
		if d.filterable && d.filter != "" {
			r := []rune(d.filter)
			d.filter = string(r[:len(r)-1])
			d.refilter()
		}
	}
	return false
}

// closeWithAction reports the highlighted row together with the key that was
// pressed on it, so the caller can act and reopen.
func (d *selectDialog) closeWithAction(b keybindings.Binding) {
	idx := d.matches[d.cursor]
	d.close(dialogResult{
		Index:  idx,
		Text:   d.options[idx].Value,
		OK:     true,
		Action: b,
	})
}

// move walks the cursor by n rows, stopping at either end rather than wrapping:
// a long catalog scrolled past the bottom should sit at the bottom, not jump
// back to the top under the reader.
func (d *selectDialog) move(n int) {
	d.cursor = min(max(d.cursor+n, 0), max(0, len(d.matches)-1))
}

func (d *selectDialog) view(width int, theme Theme) []string {
	var out []string
	if d.message != "" {
		out = append(out, wrapBlock(d.message, width)...)
	}
	if d.filterable {
		out = append(out, theme.Dim.Render("filter: ")+d.filter+theme.Dim.Render("▏"))
	}
	if len(d.matches) == 0 {
		return append(out, theme.Dim.Render("  no matches"))
	}

	// Scroll a window around the cursor rather than redrawing everything: a
	// provider catalog can be hundreds of models long.
	start := 0
	if d.cursor >= d.visible {
		start = d.cursor - d.visible + 1
	}
	end := min(start+d.visible, len(d.matches))

	for i := start; i < end; i++ {
		o := d.options[d.matches[i]]
		row := o.Label
		if o.Description != "" {
			row += theme.Dim.Render("  " + o.Description)
		}
		if i == d.cursor {
			out = append(out, theme.Selected.Render("▸ ")+truncateCells(row, width-2))
		} else {
			out = append(out, "  "+truncateCells(row, width-2))
		}
	}
	if len(d.matches) > d.visible {
		out = append(out, theme.Dim.Render(counter(d.cursor+1, len(d.matches))))
	}
	if d.hint != "" {
		out = append(out, theme.Dim.Render("  "+d.hint))
	}
	return out
}

func counter(pos, total int) string {
	return "  " + strconv.Itoa(pos) + "/" + strconv.Itoa(total)
}

// --- input ---

type inputDialog struct {
	baseDialog
	placeholder string
	secret      bool
	value       []rune
	cursor      int
}

func (d *inputDialog) key(msg tea.KeyMsg, km *keybindings.Manager) bool {
	// A paste is text, not a decision: an API key arriving from the clipboard
	// lands in the field whole and never answers the prompt.
	if msg.Paste {
		d.insert(msg.Runes)
		return false
	}
	switch {
	case msg.Type == tea.KeyRunes && !msg.Alt:
		d.insert(msg.Runes)
		return false
	case msg.Type == tea.KeySpace && !msg.Alt:
		d.insert([]rune{' '})
		return false
	}

	key := keyID(msg)
	switch {
	case bound(km, key, keybindings.SelectCancel):
		d.close(dialogResult{Index: -1})
		return true
	case bound(km, key, keybindings.SelectConfirm):
		d.close(dialogResult{Index: 0, Text: string(d.value), OK: true})
		return true
	case bound(km, key, keybindings.EditorDeleteCharBackward):
		if d.cursor > 0 {
			d.value = append(append([]rune{}, d.value[:d.cursor-1]...), d.value[d.cursor:]...)
			d.cursor--
		}
	case bound(km, key, keybindings.EditorDeleteCharForward):
		if d.cursor < len(d.value) {
			d.value = append(append([]rune{}, d.value[:d.cursor]...), d.value[d.cursor+1:]...)
		}
	case bound(km, key, keybindings.EditorCursorLeft):
		if d.cursor > 0 {
			d.cursor--
		}
	case bound(km, key, keybindings.EditorCursorRight):
		if d.cursor < len(d.value) {
			d.cursor++
		}
	case bound(km, key, keybindings.EditorCursorLineStart):
		d.cursor = 0
	case bound(km, key, keybindings.EditorCursorLineEnd):
		d.cursor = len(d.value)
	case bound(km, key, keybindings.EditorDeleteToLineStart):
		// The field is one line, so deleting to its start is a clear — which is
		// the whole point of the key when a mistyped secret is on screen.
		d.value, d.cursor = nil, 0
	case bound(km, key, keybindings.EditorDeleteToLineEnd):
		d.value = append([]rune{}, d.value[:d.cursor]...)
	}
	return false
}

func (d *inputDialog) insert(rs []rune) {
	if len(rs) == 0 {
		return
	}
	out := make([]rune, 0, len(d.value)+len(rs))
	out = append(out, d.value[:d.cursor]...)
	out = append(out, rs...)
	out = append(out, d.value[d.cursor:]...)
	d.value = out
	d.cursor += len(rs)
}

func (d *inputDialog) view(width int, theme Theme) []string {
	var out []string
	if d.message != "" {
		out = append(out, wrapBlock(d.message, width)...)
	}
	shown := string(d.value)
	if d.secret {
		shown = strings.Repeat("•", len(d.value))
	}
	if len(d.value) == 0 && d.placeholder != "" {
		return append(out, theme.Dim.Render(d.placeholder))
	}
	return append(out, shown+theme.Accent.Render("▏"))
}

// --- checklist ---

// multiSelectDialog is a checklist: several rows are toggled and then
// committed together.
//
// It is a separate type from selectDialog rather than a mode on it, because
// almost every key means something different. Enter here commits a set instead
// of picking a row, and space toggles rather than typing — a filterable
// single-select would have swallowed it into the filter.
type multiSelectDialog struct {
	baseDialog
	options []extension.SelectOption
	// checked runs parallel to options and is reordered with it.
	checked []bool
	cursor  int
	visible int
	filter  string
	matches []int
	// groupOf names the group a row belongs to, for the toggle-the-whole-group
	// key. Nil disables that key rather than guessing at a grouping.
	groupOf func(extension.SelectOption) string
	hint    string
}

func (d *multiSelectDialog) refilter() {
	d.matches = d.matches[:0]
	needle := strings.ToLower(d.filter)
	for i, o := range d.options {
		if needle == "" ||
			strings.Contains(strings.ToLower(o.Label), needle) ||
			strings.Contains(strings.ToLower(o.Description), needle) {
			d.matches = append(d.matches, i)
		}
	}
	if d.cursor >= len(d.matches) {
		d.cursor = max(0, len(d.matches)-1)
	}
}

// current is the option index under the cursor, or -1 when nothing matches.
func (d *multiSelectDialog) current() int {
	if len(d.matches) == 0 {
		return -1
	}
	return d.matches[d.cursor]
}

func (d *multiSelectDialog) key(msg tea.KeyMsg, km *keybindings.Manager) bool {
	// Space toggles rather than joining the filter: on a checklist it is the
	// primary verb, and a filter can always be typed with the other keys.
	if !msg.Paste && !msg.Alt && msg.Type == tea.KeySpace {
		if at := d.current(); at >= 0 {
			d.checked[at] = !d.checked[at]
		}
		return false
	}
	if !msg.Paste && !msg.Alt && msg.Type == tea.KeyRunes {
		d.filter += string(msg.Runes)
		d.refilter()
		return false
	}

	key := keyID(msg)
	switch {
	case bound(km, key, keybindings.SelectCancel):
		d.close(dialogResult{Index: -1})
		return true

	// tau has no session-only model scope, so applying and saving are the same
	// act and both keys do it. Pi separates them because its picker can scope a
	// session without writing to settings.
	case bound(km, key, keybindings.SelectConfirm), bound(km, key, keybindings.AppModelsSave):
		d.close(dialogResult{Index: d.current(), OK: true, Indices: d.selected()})
		return true

	case bound(km, key, keybindings.AppModelsEnableAll):
		d.setAll(true)
	case bound(km, key, keybindings.AppModelsClearAll):
		d.setAll(false)
	case bound(km, key, keybindings.AppModelsToggleProvider):
		d.toggleGroup()

	case bound(km, key, keybindings.AppModelsReorderUp):
		d.reorder(-1)
	case bound(km, key, keybindings.AppModelsReorderDown):
		d.reorder(1)

	case bound(km, key, keybindings.SelectUp):
		d.move(-1)
	case bound(km, key, keybindings.SelectDown):
		d.move(1)
	case bound(km, key, keybindings.SelectPageUp):
		d.move(-max(1, d.visible))
	case bound(km, key, keybindings.SelectPageDown):
		d.move(max(1, d.visible))
	case bound(km, key, keybindings.EditorDeleteCharBackward):
		if d.filter != "" {
			r := []rune(d.filter)
			d.filter = string(r[:len(r)-1])
			d.refilter()
		}
	}
	return false
}

// selected reports the checked rows in display order.
func (d *multiSelectDialog) selected() []int {
	var out []int
	for i, on := range d.checked {
		if on {
			out = append(out, i)
		}
	}
	return out
}

// setAll checks or clears every row, filtered or not: the keys are named for
// all of them, and a hidden row silently keeping its state would be a trap.
func (d *multiSelectDialog) setAll(on bool) {
	for i := range d.checked {
		d.checked[i] = on
	}
}

// toggleGroup flips every row sharing the highlighted row's group, turning the
// whole group off when all of it is already on.
func (d *multiSelectDialog) toggleGroup() {
	at := d.current()
	if at < 0 || d.groupOf == nil {
		return
	}
	group := d.groupOf(d.options[at])

	allOn := true
	for i, o := range d.options {
		if d.groupOf(o) == group && !d.checked[i] {
			allOn = false
			break
		}
	}
	for i, o := range d.options {
		if d.groupOf(o) == group {
			d.checked[i] = !allOn
		}
	}
}

// reorder moves the highlighted row, carrying its checkbox with it.
//
// It only works on an unfiltered list. With a filter up, "up" would mean the
// previous visible row while the move happened between two rows that are not
// adjacent, and the list would rearrange itself in a way nobody asked for.
func (d *multiSelectDialog) reorder(dir int) {
	if d.filter != "" {
		return
	}
	at := d.current()
	to := at + dir
	if at < 0 || to < 0 || to >= len(d.options) {
		return
	}
	d.options[at], d.options[to] = d.options[to], d.options[at]
	d.checked[at], d.checked[to] = d.checked[to], d.checked[at]
	d.cursor += dir
	d.refilter()
}

func (d *multiSelectDialog) move(n int) {
	d.cursor = min(max(d.cursor+n, 0), max(0, len(d.matches)-1))
}

func (d *multiSelectDialog) view(width int, theme Theme) []string {
	var out []string
	if d.message != "" {
		out = append(out, wrapBlock(d.message, width)...)
	}
	out = append(out, theme.Dim.Render("filter: ")+d.filter+theme.Dim.Render("▏"))
	if len(d.matches) == 0 {
		return append(out, theme.Dim.Render("  no matches"))
	}

	start := 0
	if d.cursor >= d.visible {
		start = d.cursor - d.visible + 1
	}
	end := min(start+d.visible, len(d.matches))

	for i := start; i < end; i++ {
		at := d.matches[i]
		box := "[ ] "
		if d.checked[at] {
			box = "[x] "
		}
		row := box + d.options[at].Label
		if desc := d.options[at].Description; desc != "" {
			row += theme.Dim.Render("  " + desc)
		}
		if i == d.cursor {
			out = append(out, theme.Selected.Render("▸ ")+truncateCells(row, width-2))
			continue
		}
		out = append(out, "  "+truncateCells(row, width-2))
	}

	out = append(out, theme.Dim.Render(counter(d.cursor+1, len(d.matches))+
		"  ·  "+strconv.Itoa(len(d.selected()))+" selected"))
	if d.hint != "" {
		out = append(out, theme.Dim.Render("  "+d.hint))
	}
	return out
}
