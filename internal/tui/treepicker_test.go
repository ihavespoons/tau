package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/keybindings"
)

// A short session with a branch, a tool result, a label, a bookkeeping entry
// and a silent turn — one of everything the filters have an opinion about.
func treeRows() []coding.TreeEntry {
	return []coding.TreeEntry{
		{ID: "a", Kind: coding.TreeUser, Summary: "user: fix the parser", HasChildren: true},
		{ID: "b", ParentID: "a", Depth: 1, Kind: coding.TreeAssistant, Summary: "assistant: reading it", HasChildren: true},
		{ID: "c", ParentID: "b", Depth: 1, Kind: coding.TreeToolResult, Summary: "tool: read", HasChildren: true},
		{ID: "d", ParentID: "c", Depth: 1, Kind: coding.TreeAssistant, Summary: "assistant: found it",
			Label: "before the refactor", LabelTimestamp: "2026-08-10T09:00:00Z", HasChildren: true},
		{ID: "e", ParentID: "a", Depth: 1, Kind: coding.TreeBookkeeping, Summary: "model: anthropic/claude-opus-5"},
		{ID: "f", ParentID: "d", Depth: 1, Kind: coding.TreeUser, Summary: "user: ship it", Current: true},
	}
}

func newTree(rows []coding.TreeEntry) (*treeDialog, chan dialogResult) {
	reply := newReply()
	d := &treeDialog{baseDialog: baseDialog{reply: reply}, rows: rows, visible: 20}
	d.refilter()
	return d, reply
}

func visibleIDs(d *treeDialog) string {
	var out []string
	for _, i := range d.matches {
		out = append(out, d.rows[i].ID)
	}
	return strings.Join(out, ",")
}

func TestTheDefaultViewHidesBookkeepingOnly(t *testing.T) {
	d, _ := newTree(treeRows())
	if got := visibleIDs(d); got != "a,b,c,d,f" {
		t.Errorf("default view = %s, want everything but the model change", got)
	}
}

func TestEachFilterShowsWhatItSays(t *testing.T) {
	for _, tc := range []struct {
		filter treeFilter
		want   string
	}{
		{treeFilterAll, "a,b,c,d,e,f"},
		{treeFilterNoTools, "a,b,d,f"},
		{treeFilterUserOnly, "a,f"},
		{treeFilterLabeledOnly, "d"},
	} {
		d, _ := newTree(treeRows())
		d.setFilter(tc.filter)
		if got := visibleIDs(d); got != tc.want {
			t.Errorf("%s view = %s, want %s", tc.filter, got, tc.want)
		}
	}
}

// A turn that only called tools has nothing to read on it — except when it is
// where you are standing, which no view may hide.
func TestSilentTurnsAreHiddenButNeverTheCurrentOne(t *testing.T) {
	rows := []coding.TreeEntry{
		{ID: "a", Kind: coding.TreeUser, Summary: "user: go"},
		{ID: "b", ParentID: "a", Kind: coding.TreeAssistant, Summary: "assistant: (read)", ToolOnly: true},
		{ID: "c", ParentID: "b", Kind: coding.TreeAssistant, Summary: "assistant: (edit)", ToolOnly: true, Current: true},
	}
	d, _ := newTree(rows)
	if got := visibleIDs(d); got != "a,c" {
		t.Errorf("view = %s, want the silent turn hidden and the current one kept", got)
	}

	// Even asking for everything does not bring it back: there is nothing on
	// that row to show.
	d.setFilter(treeFilterAll)
	if got := visibleIDs(d); got != "a,c" {
		t.Errorf("all view = %s, want the silent turn still hidden", got)
	}
}

func TestCyclingWalksTheViewsAndWraps(t *testing.T) {
	d, _ := newTree(treeRows())
	for _, want := range []treeFilter{
		treeFilterNoTools, treeFilterUserOnly, treeFilterLabeledOnly, treeFilterAll, treeFilterDefault,
	} {
		d.key(tea.KeyMsg{Type: tea.KeyCtrlO}, keys())
		if d.filter != want {
			t.Fatalf("cycling reached %s, want %s", d.filter, want)
		}
	}
}

// One key both applies a view and takes it back off, so nothing is a trap.
func TestAFilterKeyTogglesBackToTheDefault(t *testing.T) {
	d, _ := newTree(treeRows())

	d.key(tea.KeyMsg{Type: tea.KeyCtrlU}, keys())
	if d.filter != treeFilterUserOnly {
		t.Fatalf("ctrl+u gave %s", d.filter)
	}
	d.key(tea.KeyMsg{Type: tea.KeyCtrlU}, keys())
	if d.filter != treeFilterDefault {
		t.Errorf("pressing it again gave %s, want back to the default", d.filter)
	}
}

// Folding hides a subtree, not one generation — and it has to keep hiding
// through a row the filter had already taken out.
func TestFoldingHidesEveryDescendant(t *testing.T) {
	d, _ := newTree(treeRows())
	d.cursor = 1 // "b"

	d.key(tea.KeyMsg{Type: tea.KeyCtrlLeft}, keys())
	if got := visibleIDs(d); got != "a,b" {
		t.Errorf("after folding b: %s, want its whole subtree gone", got)
	}

	d.key(tea.KeyMsg{Type: tea.KeyCtrlRight}, keys())
	if got := visibleIDs(d); got != "a,b,c,d,f" {
		t.Errorf("after unfolding: %s, want the subtree back", got)
	}
}

// The binding is named foldOrUp: with nothing to fold it is a movement key.
func TestFoldFallsBackToMoving(t *testing.T) {
	d, _ := newTree(treeRows())
	d.cursor = 4 // "f", a leaf

	d.key(tea.KeyMsg{Type: tea.KeyCtrlLeft}, keys())
	if d.cursor != 3 {
		t.Errorf("cursor = %d, want it moved up a row", d.cursor)
	}
	if len(d.folded) != 0 {
		t.Error("a row with no children was folded")
	}

	d.key(tea.KeyMsg{Type: tea.KeyCtrlRight}, keys())
	if d.cursor != 4 {
		t.Errorf("cursor = %d, want it moved back down", d.cursor)
	}
}

// Narrowing a filter usually hides the row under the cursor. Jumping to the
// top would lose the reader's place; the nearest visible ancestor is where they
// effectively still are.
func TestNarrowingAFilterKeepsThePlace(t *testing.T) {
	d, _ := newTree(treeRows())
	d.cursor = 4 // "f", the current position

	d.setFilter(treeFilterLabeledOnly)
	if got := visibleIDs(d); got != "d" {
		t.Fatalf("labeled view = %s", got)
	}
	// "f" is gone; its parent "d" is the closest thing to where the cursor was.
	if e, _ := d.current(); e.ID != "d" {
		t.Errorf("cursor landed on %q, want the nearest visible ancestor", e.ID)
	}
}

func TestTypingSearchesAndEscapeClearsItFirst(t *testing.T) {
	d, reply := newTree(treeRows())

	for _, r := range "ship" {
		d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, keys())
	}
	if got := visibleIDs(d); got != "f" {
		t.Errorf("search = %s, want only the matching row", got)
	}

	// The first escape takes back the search rather than the picker: a mistyped
	// query should not throw away the position you had found.
	if done := d.key(tea.KeyMsg{Type: tea.KeyEsc}, keys()); done {
		t.Fatal("escape closed the picker while a search was up")
	}
	if d.query != "" {
		t.Errorf("query = %q, want it cleared", d.query)
	}
	if !d.key(tea.KeyMsg{Type: tea.KeyEsc}, keys()) {
		t.Fatal("the second escape did not close the picker")
	}
	if res := <-reply; res.OK {
		t.Error("escape should cancel")
	}
}

// The search matches a label as well as the text, which is the entire point of
// putting a label on something.
func TestSearchAlsoMatchesLabels(t *testing.T) {
	d, _ := newTree(treeRows())
	for _, r := range "refactor" {
		d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, keys())
	}
	if got := visibleIDs(d); got != "d" {
		t.Errorf("search = %s, want the labeled row", got)
	}
}

func TestConfirmReportsTheEntry(t *testing.T) {
	d, reply := newTree(treeRows())
	d.cursor = 4 // "f"

	if !d.key(tea.KeyMsg{Type: tea.KeyEnter}, keys()) {
		t.Fatal("enter did not close the picker")
	}
	res := <-reply
	if !res.OK || res.Text != "f" {
		t.Errorf("result = %+v, want entry f", res)
	}
}

// The label key closes and reports itself rather than opening an input inside
// a dialog's own key handler, which is the nested-modal deadlock.
func TestTheLabelKeyClosesWithItsAction(t *testing.T) {
	d, reply := newTree(treeRows())
	d.actions = []keybindings.Binding{keybindings.AppTreeEditLabel}
	d.cursor = 3 // "d"

	if !d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}}, keys()) {
		t.Fatal("shift+l did not close the picker")
	}
	res := <-reply
	if res.Action != keybindings.AppTreeEditLabel {
		t.Errorf("action = %q, want the label binding", res.Action)
	}
	if res.Text != "d" {
		t.Errorf("acted on %q, want the highlighted row", res.Text)
	}
}

func TestLabelTimestampsAreOffUntilAskedFor(t *testing.T) {
	forceColor(t)
	d, _ := newTree(treeRows())

	out := stripANSI(strings.Join(d.view(100, DefaultTheme()), "\n"))
	if !strings.Contains(out, "[before the refactor]") {
		t.Errorf("the label is missing:\n%s", out)
	}
	if strings.Contains(out, "2026-08-10") {
		t.Errorf("the timestamp is shown without being asked for:\n%s", out)
	}

	d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}}, keys())
	out = stripANSI(strings.Join(d.view(100, DefaultTheme()), "\n"))
	if !strings.Contains(out, "2026-08-10") {
		t.Errorf("shift+t did not show the timestamp:\n%s", out)
	}
}

func TestTheViewNamesItselfAndMarksThePosition(t *testing.T) {
	forceColor(t)
	d, _ := newTree(treeRows())
	out := stripANSI(strings.Join(d.view(100, DefaultTheme()), "\n"))

	if !strings.Contains(out, "view: default") {
		t.Errorf("the view is not named:\n%s", out)
	}
	if !strings.Contains(out, "(here)") {
		t.Errorf("the current position is not marked:\n%s", out)
	}
	// A row with children says so, because that is what folding would act on.
	if !strings.Contains(out, "▾ user: fix the parser") {
		t.Errorf("a foldable row carries no marker:\n%s", out)
	}
}
