package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/keybindings"
)

// treeFilter is which entries a tree view shows.
type treeFilter int

const (
	// treeFilterDefault hides the bookkeeping entries — the ones that record a
	// setting rather than a turn. They are how an append-only log persists
	// state, and reading them is almost never why anyone opened the tree.
	treeFilterDefault treeFilter = iota
	treeFilterNoTools
	treeFilterUserOnly
	treeFilterLabeledOnly
	treeFilterAll
)

// treeFilterCycle is the order the cycle keys walk, widening from the default
// through to everything.
var treeFilterCycle = []treeFilter{
	treeFilterDefault, treeFilterNoTools, treeFilterUserOnly, treeFilterLabeledOnly, treeFilterAll,
}

func (f treeFilter) String() string {
	switch f {
	case treeFilterNoTools:
		return "no tools"
	case treeFilterUserOnly:
		return "user only"
	case treeFilterLabeledOnly:
		return "labeled only"
	case treeFilterAll:
		return "all"
	default:
		return "default"
	}
}

// keeps reports whether an entry survives this filter.
func (f treeFilter) keeps(e coding.TreeEntry) bool {
	switch f {
	case treeFilterUserOnly:
		return e.Kind == coding.TreeUser
	case treeFilterNoTools:
		return e.Kind != coding.TreeBookkeeping && e.Kind != coding.TreeToolResult
	case treeFilterLabeledOnly:
		return e.Label != ""
	case treeFilterAll:
		return true
	default:
		return e.Kind != coding.TreeBookkeeping
	}
}

// treeDialog picks a place in the session tree to go back to.
//
// It is a separate dialog rather than a selectDialog over pre-rendered rows
// because its filters are over what an entry *is*: a picker holding nothing but
// strings would have to guess a message's role back out of its prefix, and
// folding needs the parent links a flat option list has thrown away.
type treeDialog struct {
	baseDialog
	// rows is the whole tree, unfiltered. Filtering is recomputed from it on
	// every keystroke rather than applied destructively, so widening a filter
	// can bring rows back.
	rows   []coding.TreeEntry
	filter treeFilter
	query  string
	// folded holds the entries whose descendants are hidden.
	folded map[string]bool
	// labelTimes shows when each bookmark was set. Off by default: it is the
	// answer to "which of these two labels is newer", not to "where am I".
	labelTimes bool
	cursor     int
	visible    int
	// matches indexes rows that survived the filters, in display order.
	matches []int
	// anchor is the entry the cursor was on. Changing a filter must not
	// teleport the cursor, so the row is remembered by identity and found
	// again afterwards.
	anchor  string
	actions []keybindings.Binding
	hint    string
}

func (d *treeDialog) refilter() {
	if d.cursor < len(d.matches) {
		d.anchor = d.rows[d.matches[d.cursor]].ID
	}

	tokens := strings.Fields(strings.ToLower(d.query))

	d.matches = d.matches[:0]
	for i, e := range d.rows {
		// A turn that only called tools has nothing to read on it. The current
		// position is exempt in every view: where you are now is the one row
		// that must never be filtered away.
		if e.ToolOnly && !e.Current {
			continue
		}
		if !d.filter.keeps(e) {
			continue
		}
		if !matchesTokens(e, tokens) {
			continue
		}
		d.matches = append(d.matches, i)
	}
	d.hideFolded()
	d.cursor = d.nearestVisible(d.anchor)
}

func matchesTokens(e coding.TreeEntry, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	hay := strings.ToLower(e.Summary + " " + e.Label)
	for _, t := range tokens {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// hideFolded drops everything below a folded entry.
//
// Folding hides a subtree rather than one generation, so this walks the full
// listing and not the filtered one: a descendant whose intermediate parent a
// filter had already hidden must still go, or folding a branch would leave its
// grandchildren behind. That works because TreeEntries is depth-first — a
// parent is always seen before anything under it.
func (d *treeDialog) hideFolded() {
	if len(d.folded) == 0 {
		return
	}
	hidden := make(map[string]bool)
	for _, e := range d.rows {
		if e.ParentID != "" && (d.folded[e.ParentID] || hidden[e.ParentID]) {
			hidden[e.ID] = true
		}
	}
	kept := d.matches[:0]
	for _, i := range d.matches {
		if !hidden[d.rows[i].ID] {
			kept = append(kept, i)
		}
	}
	d.matches = kept
}

// nearestVisible finds an entry, or the closest ancestor still on screen.
//
// Narrowing a filter usually hides the row under the cursor. Jumping to the top
// would lose the reader's place entirely; its parent is the nearest thing to
// where they were, and is what folding a branch leaves them sitting on.
func (d *treeDialog) nearestVisible(id string) int {
	if len(d.matches) == 0 {
		return 0
	}
	pos := make(map[string]int, len(d.matches))
	for i, row := range d.matches {
		pos[d.rows[row].ID] = i
	}
	parent := make(map[string]string, len(d.rows))
	for _, e := range d.rows {
		parent[e.ID] = e.ParentID
	}
	// The seen set guards a self-parented entry, which would otherwise be its
	// own ancestor forever.
	seen := make(map[string]bool, len(d.rows))
	for id != "" && !seen[id] {
		if i, ok := pos[id]; ok {
			return i
		}
		seen[id] = true
		id = parent[id]
	}
	return len(d.matches) - 1
}

func (d *treeDialog) current() (coding.TreeEntry, bool) {
	if d.cursor >= len(d.matches) {
		return coding.TreeEntry{}, false
	}
	return d.rows[d.matches[d.cursor]], true
}

// key handles a press, reporting whether the dialog is finished.
//
// Bindings are checked before typing here, which is the opposite of every other
// picker. It has to be: two of the tree's keys are shift+letter, and a terminal
// spells those as the capital letter itself — checking the filter first would
// swallow them. The cost is that a capital L or T cannot be typed into the
// filter, and it is no cost at all, because matching is case-insensitive.
func (d *treeDialog) key(msg tea.KeyMsg, km *keybindings.Manager) bool {
	key := keyID(msg)

	if len(d.matches) > 0 {
		for _, b := range d.actions {
			if bound(km, key, b) {
				e := d.rows[d.matches[d.cursor]]
				d.close(dialogResult{Index: d.matches[d.cursor], Text: e.ID, OK: true, Action: b})
				return true
			}
		}
	}

	switch {
	case bound(km, key, keybindings.SelectCancel):
		// Esc backs out of a filter before it backs out of the picker, so a
		// mistyped search does not throw away the position you had found.
		if d.query != "" || len(d.folded) > 0 {
			d.query = ""
			d.folded = nil
			d.refilter()
			return false
		}
		d.close(dialogResult{Index: -1})
		return true

	case bound(km, key, keybindings.SelectConfirm):
		e, ok := d.current()
		if !ok {
			d.close(dialogResult{Index: -1})
			return true
		}
		d.close(dialogResult{Index: d.matches[d.cursor], Text: e.ID, OK: true})
		return true

	case bound(km, key, keybindings.AppTreeFoldOrUp):
		d.foldOrMove(-1)
	case bound(km, key, keybindings.AppTreeUnfoldOrDown):
		d.unfoldOrMove(1)
	case bound(km, key, keybindings.AppTreeToggleLabelTimestamp):
		d.labelTimes = !d.labelTimes

	case bound(km, key, keybindings.AppTreeFilterDefault):
		d.setFilter(treeFilterDefault)
	case bound(km, key, keybindings.AppTreeFilterNoTools):
		d.toggleFilter(treeFilterNoTools)
	case bound(km, key, keybindings.AppTreeFilterUserOnly):
		d.toggleFilter(treeFilterUserOnly)
	case bound(km, key, keybindings.AppTreeFilterLabeledOnly):
		d.toggleFilter(treeFilterLabeledOnly)
	case bound(km, key, keybindings.AppTreeFilterAll):
		d.toggleFilter(treeFilterAll)
	case bound(km, key, keybindings.AppTreeFilterCycleForward):
		d.setFilter(d.cycled(1))
	case bound(km, key, keybindings.AppTreeFilterCycleBackward):
		d.setFilter(d.cycled(-1))

	case bound(km, key, keybindings.SelectUp):
		d.move(-1)
	case bound(km, key, keybindings.SelectDown):
		d.move(1)
	case bound(km, key, keybindings.SelectPageUp):
		d.move(-max(1, d.visible))
	case bound(km, key, keybindings.SelectPageDown):
		d.move(max(1, d.visible))

	case bound(km, key, keybindings.EditorDeleteCharBackward):
		if d.query != "" {
			r := []rune(d.query)
			d.query = string(r[:len(r)-1])
			d.refilter()
		}

	default:
		// Anything else printable searches. Paste is excluded because a pasted
		// block is not a search term, and alt-chords because they are keys.
		if !msg.Paste && !msg.Alt {
			switch msg.Type {
			case tea.KeyRunes:
				d.query += string(msg.Runes)
				d.refilter()
			case tea.KeySpace:
				d.query += " "
				d.refilter()
			}
		}
	}
	return false
}

// foldOrMove collapses the row under the cursor, or moves when there is
// nothing there to collapse.
func (d *treeDialog) foldOrMove(n int) {
	if e, ok := d.current(); ok && e.HasChildren && !d.folded[e.ID] {
		if d.folded == nil {
			d.folded = make(map[string]bool)
		}
		d.folded[e.ID] = true
		d.refilter()
		return
	}
	d.move(n)
}

func (d *treeDialog) unfoldOrMove(n int) {
	if e, ok := d.current(); ok && d.folded[e.ID] {
		delete(d.folded, e.ID)
		d.refilter()
		return
	}
	d.move(n)
}

// setFilter switches the view. Folds are dropped with it: a fold is a statement
// about a shape the reader can see, and keeping them across a filter would hide
// rows for a reason that is no longer on screen.
func (d *treeDialog) setFilter(f treeFilter) {
	d.filter = f
	d.folded = nil
	d.refilter()
}

// toggleFilter switches to a view, or back to the default if already there, so
// one key both applies and undoes it.
func (d *treeDialog) toggleFilter(f treeFilter) {
	if d.filter == f {
		f = treeFilterDefault
	}
	d.setFilter(f)
}

func (d *treeDialog) cycled(step int) treeFilter {
	for i, f := range treeFilterCycle {
		if f == d.filter {
			n := (i + step + len(treeFilterCycle)) % len(treeFilterCycle)
			return treeFilterCycle[n]
		}
	}
	return treeFilterDefault
}

func (d *treeDialog) move(n int) {
	d.cursor = min(max(d.cursor+n, 0), max(0, len(d.matches)-1))
	if d.cursor < len(d.matches) {
		d.anchor = d.rows[d.matches[d.cursor]].ID
	}
}

func (d *treeDialog) view(width int, theme Theme) []string {
	var out []string
	if d.message != "" {
		out = append(out, wrapBlock(d.message, width)...)
	}
	out = append(out, theme.Dim.Render("filter: ")+d.query+theme.Dim.Render("▏"))

	if len(d.matches) == 0 {
		return append(out, theme.Dim.Render("  nothing in this view"), d.status(theme))
	}

	start := 0
	if d.cursor >= d.visible {
		start = d.cursor - d.visible + 1
	}
	end := min(start+d.visible, len(d.matches))

	for i := start; i < end; i++ {
		row := d.renderRow(d.rows[d.matches[i]], theme)
		if i == d.cursor {
			out = append(out, theme.Selected.Render("▸ ")+truncateCells(row, width-2))
		} else {
			out = append(out, "  "+truncateCells(row, width-2))
		}
	}
	if len(d.matches) > d.visible {
		out = append(out, theme.Dim.Render(counter(d.cursor+1, len(d.matches))))
	}
	out = append(out, d.status(theme))
	if d.hint != "" {
		out = append(out, theme.Dim.Render("  "+d.hint))
	}
	return out
}

func (d *treeDialog) renderRow(e coding.TreeEntry, theme Theme) string {
	var b strings.Builder
	b.WriteString(strings.Repeat("  ", e.Depth))

	// The marker says what folding would do, so a branch that is hiding
	// something is distinguishable from a leaf at a glance.
	switch {
	case d.folded[e.ID]:
		b.WriteString(theme.Dim.Render("▸ "))
	case e.HasChildren:
		b.WriteString(theme.Dim.Render("▾ "))
	default:
		b.WriteString("  ")
	}

	b.WriteString(e.Summary)
	if e.Label != "" {
		b.WriteString(theme.Dim.Render("  [" + e.Label + "]"))
		if d.labelTimes && e.LabelTimestamp != "" {
			b.WriteString(theme.Dim.Render(" " + e.LabelTimestamp))
		}
	}
	if e.Current {
		b.WriteString(theme.Dim.Render("  (here)"))
	}
	return b.String()
}

// status names the view, because four of the five filters look like a session
// with less in it than it has.
func (d *treeDialog) status(theme Theme) string {
	return theme.Dim.Render("  view: " + d.filter.String())
}
