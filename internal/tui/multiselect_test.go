package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/keybindings"
)

func newChecklist(checked ...int) (*multiSelectDialog, chan dialogResult) {
	reply := newReply()
	d := &multiSelectDialog{
		baseDialog: baseDialog{reply: reply},
		options: []extension.SelectOption{
			{Label: "anthropic/claude-opus-5", Value: "anthropic/claude-opus-5"},
			{Label: "anthropic/claude-sonnet-5", Value: "anthropic/claude-sonnet-5"},
			{Label: "openai/gpt-5.2", Value: "openai/gpt-5.2"},
		},
		visible: 10,
		groupOf: func(o extension.SelectOption) string {
			provider, _, _ := strings.Cut(o.Value, "/")
			return provider
		},
	}
	d.checked = make([]bool, len(d.options))
	for _, i := range checked {
		d.checked[i] = true
	}
	d.refilter()
	return d, reply
}

func keys() *keybindings.Manager { return keybindings.New(nil) }

// Space is the primary verb on a checklist, so it must toggle rather than
// disappear into the filter the way it would in a single-select.
func TestChecklistSpaceToggles(t *testing.T) {
	d, _ := newChecklist()

	d.key(tea.KeyMsg{Type: tea.KeySpace}, keys())
	if !d.checked[0] {
		t.Error("space did not check the highlighted row")
	}
	if d.filter != "" {
		t.Errorf("space went into the filter: %q", d.filter)
	}
	d.key(tea.KeyMsg{Type: tea.KeySpace}, keys())
	if d.checked[0] {
		t.Error("space did not uncheck it again")
	}
}

func TestChecklistConfirmReportsSelectionInOrder(t *testing.T) {
	d, reply := newChecklist(2, 0)

	if !d.key(tea.KeyMsg{Type: tea.KeyEnter}, keys()) {
		t.Fatal("enter did not close the checklist")
	}
	res := <-reply
	if !res.OK {
		t.Fatal("enter should confirm")
	}
	// Display order, not the order they happened to be checked in: the list it
	// configures is cycled in the order it is written.
	if len(res.Indices) != 2 || res.Indices[0] != 0 || res.Indices[1] != 2 {
		t.Errorf("indices = %v, want [0 2]", res.Indices)
	}
}

// tau has no session-only model scope, so applying and saving are one act.
func TestChecklistSaveIsConfirm(t *testing.T) {
	d, reply := newChecklist(1)

	if !d.key(tea.KeyMsg{Type: tea.KeyCtrlS}, keys()) {
		t.Fatal("ctrl+s did not close the checklist")
	}
	if res := <-reply; !res.OK || len(res.Indices) != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestChecklistCancelKeepsNothing(t *testing.T) {
	d, reply := newChecklist(0, 1)

	if !d.key(tea.KeyMsg{Type: tea.KeyEsc}, keys()) {
		t.Fatal("esc did not close the checklist")
	}
	if res := <-reply; res.OK {
		t.Error("esc should cancel")
	}
}

func TestChecklistEnableAndClearAll(t *testing.T) {
	d, _ := newChecklist()

	d.key(tea.KeyMsg{Type: tea.KeyCtrlA}, keys())
	for i, on := range d.checked {
		if !on {
			t.Errorf("row %d was not checked by enable-all", i)
		}
	}
	d.key(tea.KeyMsg{Type: tea.KeyCtrlX}, keys())
	for i, on := range d.checked {
		if on {
			t.Errorf("row %d survived clear-all", i)
		}
	}
}

// Enable-all means all of them, not just the ones a filter happens to be
// showing — a hidden row quietly keeping its state would be a trap.
func TestChecklistEnableAllIgnoresTheFilter(t *testing.T) {
	d, _ := newChecklist()
	d.filter = "openai"
	d.refilter()

	d.key(tea.KeyMsg{Type: tea.KeyCtrlA}, keys())
	if !d.checked[0] {
		t.Error("a filtered-out row was left unchecked")
	}
}

func TestChecklistToggleGroup(t *testing.T) {
	d, _ := newChecklist()

	// Cursor is on the first anthropic row; the whole provider goes on.
	d.key(tea.KeyMsg{Type: tea.KeyCtrlP}, keys())
	if !d.checked[0] || !d.checked[1] {
		t.Error("the provider was not enabled as a group")
	}
	if d.checked[2] {
		t.Error("another provider was caught up in it")
	}

	// Pressing it again with the group fully on turns it off.
	d.key(tea.KeyMsg{Type: tea.KeyCtrlP}, keys())
	if d.checked[0] || d.checked[1] {
		t.Error("the group did not turn back off")
	}
}

func TestChecklistReorderCarriesTheCheckbox(t *testing.T) {
	d, _ := newChecklist(0)

	d.key(tea.KeyMsg{Type: tea.KeyDown, Alt: true}, keys())
	if d.options[1].Value != "anthropic/claude-opus-5" {
		t.Fatalf("row did not move: %v", d.options[1].Value)
	}
	if !d.checked[1] || d.checked[0] {
		t.Error("the checkbox did not travel with its row")
	}
	// The cursor follows the row it was on, or the next press moves a
	// different one.
	if d.current() != 1 {
		t.Errorf("cursor = %d, want it still on the moved row", d.current())
	}
}

func TestChecklistReorderStopsAtTheEnds(t *testing.T) {
	d, _ := newChecklist()

	d.key(tea.KeyMsg{Type: tea.KeyUp, Alt: true}, keys())
	if d.options[0].Value != "anthropic/claude-opus-5" {
		t.Error("the top row moved off the top")
	}
}

// With a filter up, "up" means the previous visible row while the swap would
// happen between rows that are not adjacent.
func TestChecklistReorderIsDisabledWhileFiltering(t *testing.T) {
	d, _ := newChecklist()
	d.filter = "anthropic"
	d.refilter()

	before := d.options[0].Value
	d.key(tea.KeyMsg{Type: tea.KeyDown, Alt: true}, keys())
	if d.options[0].Value != before {
		t.Error("rows were reordered while a filter was up")
	}
}

func TestChecklistFilters(t *testing.T) {
	d, _ := newChecklist()

	for _, r := range "openai" {
		d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, keys())
	}
	if len(d.matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(d.matches))
	}
	if d.options[d.matches[0]].Value != "openai/gpt-5.2" {
		t.Errorf("matched %q", d.options[d.matches[0]].Value)
	}

	d.key(tea.KeyMsg{Type: tea.KeyBackspace}, keys())
	if len(d.matches) != 1 {
		t.Errorf("backspace should widen the filter, matches = %d", len(d.matches))
	}
}

func TestChecklistViewShowsBoxesAndCount(t *testing.T) {
	d, _ := newChecklist(1)
	out := stripANSI(strings.Join(d.view(80, DefaultTheme()), "\n"))

	if !strings.Contains(out, "[x] anthropic/claude-sonnet-5") {
		t.Errorf("a checked row is not marked:\n%s", out)
	}
	if !strings.Contains(out, "[ ] anthropic/claude-opus-5") {
		t.Errorf("an unchecked row is not marked:\n%s", out)
	}
	if !strings.Contains(out, "1 selected") {
		t.Errorf("the count is missing:\n%s", out)
	}
}

func TestScopedPatterns(t *testing.T) {
	opts := []extension.SelectOption{
		{Value: "anthropic/claude-opus-5"},
		{Value: "anthropic/claude-sonnet-5"},
		{Value: "openai/gpt-5.2"},
	}

	// Everything and nothing both mean "no scope": writing a pattern per model
	// to describe the default would put the whole catalog in settings.json.
	if got := scopedPatterns(opts, []int{0, 1, 2}); got != nil {
		t.Errorf("ticking everything = %v, want nil", got)
	}
	if got := scopedPatterns(opts, nil); got != nil {
		t.Errorf("ticking nothing = %v, want nil", got)
	}

	// A real subset is written in display order, which is the order the
	// cycling shortcut walks.
	got := scopedPatterns(opts, []int{2, 0})
	want := []string{"openai/gpt-5.2", "anthropic/claude-opus-5"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}
