package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/extension"
)

// dialogResult is what a blocked caller receives when a dialog closes.
type dialogResult struct {
	// Index is the chosen option, or -1.
	Index int
	// Text is the entered text.
	Text string
	// OK is false when the user cancelled.
	OK bool
}

// dialog is a modal that owns the keyboard while it is open.
//
// Dialogs are driven entirely from the render goroutine. The goroutine that
// asked for one is parked on the reply channel until close() sends, which is
// what makes the agent pause while the user is being asked something.
type dialog interface {
	// key handles a key press, reporting whether the dialog is finished.
	key(tea.KeyMsg) bool
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
func (s *dialogStack) key(msg tea.KeyMsg) bool {
	n := len(s.items)
	if n == 0 {
		return false
	}
	if s.items[n-1].key(msg) {
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

func (d *confirmDialog) key(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyLeft, tea.KeyRight, tea.KeyTab:
		d.yes = !d.yes
	case tea.KeyEsc:
		d.close(dialogResult{Index: -1})
		return true
	case tea.KeyEnter:
		d.close(dialogResult{Index: boolIndex(d.yes), OK: d.yes})
		return true
	case tea.KeyRunes:
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

func (d *selectDialog) key(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		d.close(dialogResult{Index: -1})
		return true
	case tea.KeyEnter:
		if len(d.matches) == 0 {
			d.close(dialogResult{Index: -1})
			return true
		}
		idx := d.matches[d.cursor]
		d.close(dialogResult{Index: idx, Text: d.options[idx].Value, OK: true})
		return true
	case tea.KeyUp, tea.KeyCtrlP:
		if d.cursor > 0 {
			d.cursor--
		}
	case tea.KeyDown, tea.KeyCtrlN:
		if d.cursor < len(d.matches)-1 {
			d.cursor++
		}
	case tea.KeyBackspace:
		if d.filterable && d.filter != "" {
			r := []rune(d.filter)
			d.filter = string(r[:len(r)-1])
			d.refilter()
		}
	case tea.KeyRunes, tea.KeySpace:
		if !d.filterable {
			return false
		}
		if msg.Type == tea.KeySpace {
			d.filter += " "
		} else {
			d.filter += string(msg.Runes)
		}
		d.refilter()
	}
	return false
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

func (d *inputDialog) key(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		d.close(dialogResult{Index: -1})
		return true
	case tea.KeyEnter:
		d.close(dialogResult{Index: 0, Text: string(d.value), OK: true})
		return true
	case tea.KeyRunes:
		d.value = append(append(append([]rune{}, d.value[:d.cursor]...), msg.Runes...), d.value[d.cursor:]...)
		d.cursor += len(msg.Runes)
	case tea.KeySpace:
		d.value = append(append(append([]rune{}, d.value[:d.cursor]...), ' '), d.value[d.cursor:]...)
		d.cursor++
	case tea.KeyBackspace:
		if d.cursor > 0 {
			d.value = append(append([]rune{}, d.value[:d.cursor-1]...), d.value[d.cursor:]...)
			d.cursor--
		}
	case tea.KeyLeft:
		if d.cursor > 0 {
			d.cursor--
		}
	case tea.KeyRight:
		if d.cursor < len(d.value) {
			d.cursor++
		}
	case tea.KeyCtrlA, tea.KeyHome:
		d.cursor = 0
	case tea.KeyCtrlE, tea.KeyEnd:
		d.cursor = len(d.value)
	case tea.KeyCtrlU:
		d.value, d.cursor = nil, 0
	}
	return false
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
