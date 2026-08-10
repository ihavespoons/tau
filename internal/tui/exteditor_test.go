package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newExtEditorApp(t *testing.T) *app {
	t.Helper()
	return &app{ed: newTestEditor(), theme: DefaultTheme()}
}

// writeTemp stands in for what the editor left behind.
func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tau-prompt.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExternalEditorAdoptsWhatWasWritten(t *testing.T) {
	a := newExtEditorApp(t)
	a.ed.SetValue("before")

	// The trailing newline is the editor's, not the user's.
	path := writeTemp(t, "written in the editor\n")
	a.finishExternalEditor(externalEditorDone{path: path})

	if got := a.ed.Value(); got != "written in the editor" {
		t.Errorf("value = %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the temporary file was left behind at %s", path)
	}
}

// Multi-line prompts are the reason to reach for an editor at all, so only the
// final newline goes.
func TestExternalEditorKeepsInteriorNewlines(t *testing.T) {
	a := newExtEditorApp(t)
	path := writeTemp(t, "one\n\ntwo\n")
	a.finishExternalEditor(externalEditorDone{path: path})

	if got := a.ed.Value(); got != "one\n\ntwo" {
		t.Errorf("value = %q", got)
	}
}

// The text in the editor is the user's. Losing it because the editor exited
// non-zero would be the worst available response.
func TestExternalEditorFailureKeepsThePrompt(t *testing.T) {
	a := newExtEditorApp(t)
	a.ed.SetValue("typed by hand")

	path := writeTemp(t, "should be ignored")
	a.finishExternalEditor(externalEditorDone{path: path, err: os.ErrPermission})

	if got := a.ed.Value(); got != "typed by hand" {
		t.Errorf("value = %q, want the prompt untouched", got)
	}
	if a.notice == "" {
		t.Error("the failure was not reported")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the temporary file was left behind after a failure")
	}
}

func TestExternalEditorReportsAnUnreadableFile(t *testing.T) {
	a := newExtEditorApp(t)
	a.ed.SetValue("kept")

	a.finishExternalEditor(externalEditorDone{path: filepath.Join(t.TempDir(), "gone.md")})
	if got := a.ed.Value(); got != "kept" {
		t.Errorf("value = %q", got)
	}
	if !strings.Contains(a.notice, "could not read") {
		t.Errorf("notice = %q", a.notice)
	}
}

// An edit is one action, and the user has to be able to take it back.
func TestExternalEditorIsUndoable(t *testing.T) {
	a := newExtEditorApp(t)
	a.ed.SetValue("original")

	a.finishExternalEditor(externalEditorDone{path: writeTemp(t, "replaced\n")})
	if got := a.ed.Value(); got != "replaced" {
		t.Fatalf("value = %q", got)
	}

	a.ed.undo()
	if got := a.ed.Value(); got != "original" {
		t.Errorf("after undo = %q, want the text from before the edit", got)
	}
}

func TestExternalEditorNeedsAnEditorConfigured(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")

	a := newExtEditorApp(t)
	a.ed.SetValue("kept")

	if cmd := a.openExternalEditor(); cmd != nil {
		t.Error("no editor is configured, so nothing should be run")
	}
	if !strings.Contains(a.notice, "EDITOR") {
		t.Errorf("notice = %q, want it to say what to set", a.notice)
	}
	if got := a.ed.Value(); got != "kept" {
		t.Errorf("the prompt was disturbed: %q", got)
	}
}

// $VISUAL wins over $EDITOR, and the prompt reaches the file the editor opens.
func TestExternalEditorWritesThePromptOut(t *testing.T) {
	t.Setenv("VISUAL", "true")
	t.Setenv("EDITOR", "false")

	a := newExtEditorApp(t)
	a.ed.SetValue("carried into the editor")

	if cmd := a.openExternalEditor(); cmd == nil {
		t.Fatalf("no command was produced (notice: %q)", a.notice)
	}

	// The file is named after tau and holds the prompt; find it rather than
	// reaching into the command, which Bubble Tea owns from here.
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "tau-prompt-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range matches {
		body, rerr := os.ReadFile(m)
		if rerr == nil && string(body) == "carried into the editor" {
			found = true
			_ = os.Remove(m)
		}
	}
	if !found {
		t.Error("the prompt was not written to a file for the editor to open")
	}
}
