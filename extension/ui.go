package extension

import (
	"context"
	"errors"
)

// ErrNoUI is returned by dialog methods when no interactive surface is
// attached — print, json, and rpc modes. Extensions should check
// Context.HasUI() before asking the user anything, but a mistake returns this
// error rather than hanging forever.
var ErrNoUI = errors.New("extension: no interactive UI is attached")

// Widget is extension-owned chrome the host renders into its live region.
//
// Pi's Component.render(width) => string[] maps here exactly: a widget is
// handed the available width and returns the lines it wants drawn. Returning
// no lines draws nothing. Lines may contain ANSI styling; the host does not
// re-wrap them, so a widget that exceeds width is the widget's own bug.
type Widget interface {
	Render(width int) []string
}

// WidgetFunc adapts a function to Widget.
type WidgetFunc func(width int) []string

// Render implements Widget.
func (f WidgetFunc) Render(width int) []string { return f(width) }

// WidgetPosition places a widget in the host's live region.
type WidgetPosition string

const (
	// WidgetAboveEditor draws between the transcript and the editor.
	WidgetAboveEditor WidgetPosition = "above-editor"
	// WidgetBelowEditor draws between the editor and the status line.
	WidgetBelowEditor WidgetPosition = "below-editor"
)

// SelectOption is one choice in a Select dialog.
type SelectOption struct {
	// Label is the primary text.
	Label string
	// Description is shown dimmed beside or under the label.
	Description string
	// Value is returned to the caller; Select also returns the index, so this
	// is a convenience for callers that key off strings.
	Value string
}

// ConfirmRequest asks a yes/no question.
type ConfirmRequest struct {
	Title   string
	Message string
	// ConfirmLabel and CancelLabel override the default "Yes"/"No".
	ConfirmLabel string
	CancelLabel  string
	// Default is the pre-selected answer.
	Default bool
}

// SelectRequest asks the user to pick one option.
type SelectRequest struct {
	Title   string
	Message string
	Options []SelectOption
	// Initial is the index selected when the dialog opens.
	Initial int
	// Filterable shows a search box over the options.
	Filterable bool
}

// InputRequest asks the user for a line of text.
type InputRequest struct {
	Title       string
	Message     string
	Placeholder string
	// Initial pre-fills the field.
	Initial string
	// Secret masks the typed characters.
	Secret bool
}

// NotifyLevel classifies a notification.
type NotifyLevel string

const (
	NotifyInfo    NotifyLevel = "info"
	NotifyWarning NotifyLevel = "warning"
	NotifyError   NotifyLevel = "error"
)

// Notification is a transient message shown to the user. Unlike the dialogs,
// it does not block.
type Notification struct {
	Level   NotifyLevel
	Title   string
	Message string
}

// UI is the host's interactive surface.
//
// # Concurrency
//
// Dialog methods BLOCK the calling goroutine until the user answers or ctx is
// cancelled. They are safe to call from an extension handler because handlers
// never run on the host's render goroutine — the host posts a request into its
// event loop and waits on a reply channel. Calling a dialog method from the
// render goroutine would deadlock, which is why no such path exists.
//
// The non-blocking methods (Notify, SetStatus, SetTitle, SetWidget) may be
// called from any goroutine at any time.
type UI interface {
	// Confirm asks a yes/no question.
	Confirm(ctx context.Context, req ConfirmRequest) (bool, error)
	// Select asks the user to choose an option, returning its index.
	Select(ctx context.Context, req SelectRequest) (int, error)
	// Input asks for a line of text.
	Input(ctx context.Context, req InputRequest) (string, error)

	// Notify shows a transient message.
	Notify(n Notification)
	// SetStatus replaces the extension's slot in the status line. Empty
	// clears it.
	SetStatus(text string)
	// SetTitle sets the terminal window title.
	SetTitle(title string)
	// SetWidget mounts a widget under an extension-chosen id, replacing any
	// widget already at that id. A nil widget removes it.
	//
	// Ids share one namespace across all extensions, as they do in Pi, so
	// prefix yours with the extension name to avoid collisions.
	SetWidget(id string, pos WidgetPosition, w Widget)
}

// NoUI is the UI for headless modes: dialogs fail with ErrNoUI, everything
// else is discarded. It is the zero-value UI so a Runner without a host UI
// never panics.
type NoUI struct{}

var _ UI = NoUI{}

func (NoUI) Confirm(context.Context, ConfirmRequest) (bool, error) { return false, ErrNoUI }
func (NoUI) Select(context.Context, SelectRequest) (int, error)    { return -1, ErrNoUI }
func (NoUI) Input(context.Context, InputRequest) (string, error)   { return "", ErrNoUI }
func (NoUI) Notify(Notification)                                   {}
func (NoUI) SetStatus(string)                                      {}
func (NoUI) SetTitle(string)                                       {}
func (NoUI) SetWidget(string, WidgetPosition, Widget)              {}

// UI returns the host's interactive surface. It is never nil: without a UI
// host the returned value discards output and fails dialogs with ErrNoUI.
func (c *Context) UI() UI {
	if c.stale() || c.runner == nil || c.runner.ui == nil {
		return NoUI{}
	}
	return c.runner.ui
}

// UI returns the host's interactive surface, for use outside a handler.
func (a *API) UI() UI {
	a.mu.Lock()
	ui := a.ui
	a.mu.Unlock()
	if ui == nil {
		return NoUI{}
	}
	return ui
}

// SetUI attaches the host's interactive surface. Hosts call this before
// loading extensions so a factory can already reach the UI.
func (r *Runner) SetUI(ui UI) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ui = ui
	for _, a := range r.apis {
		a.mu.Lock()
		a.ui = ui
		a.mu.Unlock()
	}
}
