package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ihavespoons/tau/session"
)

// userBashDone carries a finished shell command back to the interface.
type userBashDone struct {
	msg *session.BashExecutionMessage
	err error
}

// runUserBash runs a `!` command off the interface's goroutine.
//
// The header goes out before the command starts, so the transcript shows what
// is running while it runs. The output arrives in one piece when the command
// finishes rather than streaming: the interface stays responsive either way,
// and incremental output needs a channel back into the Bubble Tea loop that
// nothing else here needs yet.
func (a *app) runUserBash(command string, exclude bool) tea.Cmd {
	a.emit(a.rend.userBash(command, exclude))
	a.notice = "running…"

	cs := a.cs
	return func() tea.Msg {
		msg, err := cs.RunUserBash(context.Background(), command, exclude, nil)
		return userBashDone{msg: msg, err: err}
	}
}

// finishUserBash prints what the command produced.
func (a *app) finishUserBash(done userBashDone) {
	a.notice = ""
	if done.err != nil {
		a.emit([]string{a.theme.Error.Render("⨯ " + done.err.Error()), ""})
		return
	}
	if done.msg == nil {
		return
	}
	a.emit(a.rend.userBashResult(done.msg))
}
