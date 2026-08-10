package tui

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// externalEditorDone carries the result back once the editor has exited.
type externalEditorDone struct {
	path string
	err  error
}

// openExternalEditor writes the prompt to a temporary file and hands the
// terminal to $VISUAL or $EDITOR.
//
// Bubble Tea's ExecProcess is what makes this safe: it restores the terminal,
// runs the child attached to it, and repaints afterwards. Starting the editor
// any other way would leave two programs drawing on the same screen.
func (a *app) openExternalEditor() tea.Cmd {
	name := os.Getenv("VISUAL")
	if name == "" {
		name = os.Getenv("EDITOR")
	}
	if strings.TrimSpace(name) == "" {
		a.notice = "set $EDITOR or $VISUAL to write the prompt in an editor"
		return nil
	}

	// .md because the prompt is markdown, and an editor that colours it is
	// more use than one that does not.
	f, err := os.CreateTemp("", "tau-prompt-*.md")
	if err != nil {
		a.notice = "could not create a file to edit: " + err.Error()
		return nil
	}
	path := f.Name()
	_, werr := f.WriteString(a.ed.Value())
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(path)
		a.notice = "could not write the prompt out for editing"
		return nil
	}

	// $EDITOR carries arguments often enough to matter — "code --wait", "emacs
	// -nw" — and splitting on spaces handles those without going through a
	// shell, which would turn the variable into a way to run arbitrary text.
	parts := strings.Fields(name)
	cmd := exec.Command(parts[0], append(parts[1:], path)...)

	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return externalEditorDone{path: path, err: err}
	})
}

// finishExternalEditor reads back whatever the editor left behind.
//
// A failure keeps the prompt as it was rather than clearing it: the text in the
// editor is the user's, and losing it because their editor exited non-zero
// would be the worst possible response.
func (a *app) finishExternalEditor(msg externalEditorDone) {
	defer func() { _ = os.Remove(msg.path) }()

	if msg.err != nil {
		a.notice = "editor exited with an error: " + msg.err.Error()
		return
	}
	body, err := os.ReadFile(msg.path)
	if err != nil {
		a.notice = "could not read the edited prompt: " + err.Error()
		return
	}

	// The trailing newline is what an editor adds on save, not something the
	// user typed, and keeping it would turn every edit into a blank last line.
	a.ed.Replace(strings.TrimRight(string(body), "\n"))
}
