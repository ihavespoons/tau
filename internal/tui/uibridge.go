package tui

import (
	"context"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/keybindings"
)

// Messages the bridge posts into the Bubble Tea loop.
type (
	openDialogMsg   struct{ d dialog }
	cancelDialogMsg struct{ d dialog }
	notifyMsg       struct{ n extension.Notification }
	statusMsg       struct{ text string }
	titleMsg        struct{ title string }
	clipboardMsg    struct{ text string }
	widgetMsg       struct {
		id  string
		pos extension.WidgetPosition
		w   extension.Widget
	}
)

// uiBridge implements extension.UI on top of a Bubble Tea program.
//
// This is the only place the two concurrency domains meet, and the rule that
// keeps it deadlock-free is one-directional: everything here runs on a
// non-render goroutine and posts messages *into* the render loop. The render
// loop never calls back into this type, so a blocking dialog can never be
// waiting on the goroutine that would answer it.
type uiBridge struct {
	mu   sync.Mutex
	prog *tea.Program
	// done is closed when the program exits, so callers parked on a dialog
	// are released instead of hanging on a UI that is gone.
	done chan struct{}
	// listRows is how many options a select dialog shows at once, recomputed
	// as the terminal resizes.
	listRows int
}

var _ extension.UI = (*uiBridge)(nil)

func newUIBridge() *uiBridge {
	return &uiBridge{done: make(chan struct{}), listRows: 10}
}

// attach binds the program once it exists. Calls made before this point fail
// with extension.ErrNoUI rather than blocking.
func (b *uiBridge) attach(p *tea.Program) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prog = p
}

// shutdown releases every parked caller.
func (b *uiBridge) shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.done:
	default:
		close(b.done)
	}
}

func (b *uiBridge) setListRows(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listRows = max(3, n)
}

func (b *uiBridge) rows() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listRows
}

// send posts a message, reporting whether a program was there to receive it.
func (b *uiBridge) send(msg tea.Msg) bool {
	b.mu.Lock()
	p := b.prog
	b.mu.Unlock()
	if p == nil {
		return false
	}
	select {
	case <-b.done:
		return false
	default:
	}
	p.Send(msg)
	return true
}

// ask opens a dialog and blocks until the user answers, the caller's context
// is cancelled, or the UI goes away.
func (b *uiBridge) ask(ctx context.Context, d dialog, reply chan dialogResult) (dialogResult, error) {
	if !b.send(openDialogMsg{d: d}) {
		return dialogResult{Index: -1}, extension.ErrNoUI
	}
	select {
	case res := <-reply:
		return res, nil
	case <-ctx.Done():
		// Tell the render loop to take the dialog down; it may already have
		// closed on its own, which the message handles idempotently.
		b.send(cancelDialogMsg{d: d})
		return dialogResult{Index: -1}, ctx.Err()
	case <-b.done:
		return dialogResult{Index: -1}, extension.ErrNoUI
	}
}

// newReply makes the one-slot channel a dialog resolves through. The buffer
// is what lets the render loop resolve a dialog whose waiter has already
// walked away.
func newReply() chan dialogResult { return make(chan dialogResult, 1) }

// Confirm implements extension.UI.
func (b *uiBridge) Confirm(ctx context.Context, req extension.ConfirmRequest) (bool, error) {
	confirm, cancel := req.ConfirmLabel, req.CancelLabel
	if confirm == "" {
		confirm = "Yes"
	}
	if cancel == "" {
		cancel = "No"
	}
	reply := newReply()
	d := &confirmDialog{
		baseDialog:   baseDialog{reply: reply, heading: req.Title, message: req.Message},
		confirmLabel: confirm, cancelLabel: cancel, yes: req.Default,
	}
	res, err := b.ask(ctx, d, reply)
	if err != nil {
		return false, err
	}
	return res.OK, nil
}

// Select implements extension.UI.
func (b *uiBridge) Select(ctx context.Context, req extension.SelectRequest) (int, error) {
	idx, _, err := b.selectWith(ctx, req, selectActions{})
	return idx, err
}

// selectActions registers keys that close the picker and report themselves,
// so the caller can act on the highlighted row and open it again.
type selectActions struct {
	// on fires whatever the filter contains.
	on []keybindings.Binding
	// onEmptyQuery fires only while the filter is empty, which lets a
	// destructive key share a chord with a text-editing one.
	onEmptyQuery []keybindings.Binding
	// hint is drawn under the list to advertise them.
	hint string
}

// selectWith is Select with per-row actions. It is internal: extensions get
// the plain pick-one-thing form, because an extension has no way to say what a
// key should do to a row it did not build.
func (b *uiBridge) selectWith(ctx context.Context, req extension.SelectRequest, acts selectActions) (int, keybindings.Binding, error) {
	reply := newReply()
	d := &selectDialog{
		baseDialog:        baseDialog{reply: reply, heading: req.Title, message: req.Message},
		options:           req.Options,
		filterable:        req.Filterable,
		visible:           b.rows(),
		actions:           acts.on,
		emptyQueryActions: acts.onEmptyQuery,
		hint:              acts.hint,
	}
	d.refilter()
	if req.Initial > 0 && req.Initial < len(d.matches) {
		d.cursor = req.Initial
	}
	res, err := b.ask(ctx, d, reply)
	if err != nil {
		return -1, "", err
	}
	if !res.OK {
		return -1, "", nil
	}
	return res.Index, res.Action, nil
}

// multiSelect opens a checklist and returns the rows that were ticked, in
// display order, plus whether the user committed rather than cancelled.
//
// It is internal for the same reason selectWith is: an extension has no way to
// say what a group is, and the checklist's group key would have nothing to act
// on.
func (b *uiBridge) multiSelect(ctx context.Context, req extension.SelectRequest, checked []bool, groupOf func(extension.SelectOption) string, hint string) ([]int, bool, error) {
	reply := newReply()
	d := &multiSelectDialog{
		baseDialog: baseDialog{reply: reply, heading: req.Title, message: req.Message},
		options:    req.Options,
		checked:    checked,
		visible:    b.rows(),
		groupOf:    groupOf,
		hint:       hint,
	}
	d.refilter()

	res, err := b.ask(ctx, d, reply)
	if err != nil {
		return nil, false, err
	}
	return res.Indices, res.OK, nil
}

// Input implements extension.UI.
func (b *uiBridge) Input(ctx context.Context, req extension.InputRequest) (string, error) {
	reply := newReply()
	d := &inputDialog{
		baseDialog:  baseDialog{reply: reply, heading: req.Title, message: req.Message},
		placeholder: req.Placeholder,
		secret:      req.Secret,
		value:       []rune(req.Initial),
	}
	d.cursor = len(d.value)
	res, err := b.ask(ctx, d, reply)
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", context.Canceled
	}
	return res.Text, nil
}

// Notify implements extension.UI.
func (b *uiBridge) Notify(n extension.Notification) { b.send(notifyMsg{n: n}) }

// SetStatus implements extension.UI.
func (b *uiBridge) SetStatus(text string) { b.send(statusMsg{text: text}) }

// SetTitle implements extension.UI.
func (b *uiBridge) SetTitle(title string) { b.send(titleMsg{title: title}) }

// SetWidget implements extension.UI.
func (b *uiBridge) SetWidget(id string, pos extension.WidgetPosition, w extension.Widget) {
	b.send(widgetMsg{id: id, pos: pos, w: w})
}

// print pushes lines straight into the scrollback. It is how the login flows
// surface an authorization URL without holding a dialog open.
func (b *uiBridge) print(lines []string) {
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) > 0 {
		b.send(printMsg{lines: kept})
	}
}

// Copy puts text on the clipboard via OSC 52, which works over SSH and inside
// multiplexers where a local clipboard API would not.
func (b *uiBridge) Copy(text string) error {
	if !b.send(clipboardMsg{text: text}) {
		return extension.ErrNoUI
	}
	return nil
}
