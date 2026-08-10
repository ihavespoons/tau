package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/keybindings"
	"github.com/ihavespoons/tau/session"
)

func newActionDialog(actions, empty []keybindings.Binding) (*selectDialog, chan dialogResult) {
	reply := newReply()
	d := &selectDialog{
		baseDialog: baseDialog{reply: reply},
		options: []extension.SelectOption{
			{Label: "first", Value: "/a.jsonl"},
			{Label: "second", Value: "/b.jsonl"},
		},
		filterable:        true,
		visible:           5,
		actions:           actions,
		emptyQueryActions: empty,
	}
	d.refilter()
	return d, reply
}

// An action closes the picker and reports which key was pressed on which row,
// so the caller can act and reopen. Acting inside the key handler would nest a
// dialog inside a dialog while a goroutine is parked on the first one's reply.
func TestSelectActionClosesWithTheHighlightedRow(t *testing.T) {
	d, reply := newActionDialog([]keybindings.Binding{keybindings.AppSessionDelete}, nil)
	d.move(1)

	if !d.key(tea.KeyMsg{Type: tea.KeyCtrlD}, keybindings.New(nil)) {
		t.Fatal("the action did not close the dialog")
	}
	res := <-reply
	if res.Action != keybindings.AppSessionDelete {
		t.Errorf("action = %q, want the delete binding", res.Action)
	}
	if res.Index != 1 || !res.OK {
		t.Errorf("index = %d, ok = %v, want the highlighted row", res.Index, res.OK)
	}
}

// Confirming normally reports no action, so the caller can tell "resume this"
// from "do something to this".
func TestSelectConfirmReportsNoAction(t *testing.T) {
	d, reply := newActionDialog([]keybindings.Binding{keybindings.AppSessionDelete}, nil)

	if !d.key(tea.KeyMsg{Type: tea.KeyEnter}, keybindings.New(nil)) {
		t.Fatal("enter did not close the dialog")
	}
	if res := <-reply; res.Action != "" {
		t.Errorf("action = %q, want empty", res.Action)
	}
}

// An empty-query action shares its chord with a text-editing key, so it must
// stay out of the way while the user is typing a filter.
func TestEmptyQueryActionWaitsForAnEmptyFilter(t *testing.T) {
	d, reply := newActionDialog(nil, []keybindings.Binding{keybindings.AppSessionDeleteNoninvasive})
	km := keybindings.New(nil)

	d.filter = "sec"
	d.refilter()
	if d.key(tea.KeyMsg{Type: tea.KeyCtrlH}, km) {
		t.Fatal("the action fired while a filter was being typed")
	}

	d.filter = ""
	d.refilter()
	if !d.key(tea.KeyMsg{Type: tea.KeyCtrlH}, km) {
		t.Fatal("the action did not fire on an empty filter")
	}
	if res := <-reply; res.Action != keybindings.AppSessionDeleteNoninvasive {
		t.Errorf("action = %q", res.Action)
	}
}

// A picker with no actions registered behaves exactly as it did before, which
// is what every extension's use of it depends on.
func TestSelectWithoutActionsIgnoresThoseKeys(t *testing.T) {
	d, _ := newActionDialog(nil, nil)
	if d.key(tea.KeyMsg{Type: tea.KeyCtrlD}, keybindings.New(nil)) {
		t.Error("a key with no action registered closed the dialog")
	}
}

// An action on an empty list would have no row to act on.
func TestSelectActionNeedsARow(t *testing.T) {
	d, _ := newActionDialog([]keybindings.Binding{keybindings.AppSessionDelete}, nil)
	d.filter = "no such session"
	d.refilter()

	if d.key(tea.KeyMsg{Type: tea.KeyCtrlD}, keybindings.New(nil)) {
		t.Error("the action fired with nothing highlighted")
	}
}

func TestSessionViewOrder(t *testing.T) {
	metas := []session.Metadata{{Path: "/newest"}, {Path: "/middle"}, {Path: "/oldest"}}

	if got := (sessionView{}).order(metas); got[0].Path != "/newest" {
		t.Errorf("default order starts with %q, want the newest", got[0].Path)
	}
	got := (sessionView{oldestFirst: true}).order(metas)
	if got[0].Path != "/oldest" {
		t.Errorf("reversed order starts with %q, want the oldest", got[0].Path)
	}
	// The caller's slice must not be reordered underneath it.
	if metas[0].Path != "/newest" {
		t.Error("order mutated its input")
	}
}

func TestSessionViewOptions(t *testing.T) {
	metas := []session.Metadata{
		{Path: "/dir/a.jsonl", CreatedAt: "2026-08-10T09:00:00Z"},
		{Path: "/dir/b.jsonl", CreatedAt: "2026-08-10T10:00:00Z"},
	}

	rows := (sessionView{}).options(metas, "/dir/b.jsonl")
	if rows[0].Description != "a.jsonl" {
		t.Errorf("description = %q, want just the file name", rows[0].Description)
	}
	if !strings.Contains(rows[1].Description, "(current)") {
		t.Errorf("the session in progress should be marked: %q", rows[1].Description)
	}

	rows = (sessionView{showPath: true}).options(metas, "")
	if rows[0].Description != "/dir/a.jsonl" {
		t.Errorf("description = %q, want the whole path", rows[0].Description)
	}
}
