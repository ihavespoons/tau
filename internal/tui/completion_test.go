package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/slashcmd"
)

func completionApp(t *testing.T) *app {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"main.go", "internal/parser.go"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	reg := slashcmd.NewRegistry()
	noop := func(context.Context, string) (slashcmd.Result, error) { return slashcmd.Result{}, nil }
	reg.Register(slashcmd.New(slashcmd.Info{Name: "help", Description: "Show help"}, noop))
	reg.Register(slashcmd.NewWithCompleter(
		slashcmd.Info{Name: "model", ArgumentHint: "<provider/model>"}, noop,
		func(prefix string) []slashcmd.Item {
			var out []slashcmd.Item
			for _, m := range []string{"anthropic/claude-opus-5", "openai/gpt-5.2"} {
				if strings.HasPrefix(m, prefix) {
					out = append(out, slashcmd.Item{Value: m, Label: m})
				}
			}
			return out
		}))

	return &app{
		cs:    &coding.Session{Cwd: root, Commands: reg},
		ed:    newEditor(DefaultTheme(), nil),
		theme: DefaultTheme(),
		width: 80,
	}
}

// typeInto puts text in the editor with the caret at the end of it, which is
// where it is while someone is typing.
func typeInto(a *app, text string) {
	a.ed.SetValue(text)
	a.refreshCompletions()
}

func completionValues(a *app) []string {
	var out []string
	for _, c := range a.completions {
		out = append(out, c.value)
	}
	return out
}

func TestCommandNamesStillComplete(t *testing.T) {
	a := completionApp(t)
	typeInto(a, "/he")

	if got := completionValues(a); len(got) != 1 || got[0] != "/help " {
		t.Errorf("completions = %v, want /help with its trailing space", got)
	}
}

// The registry has had a Complete method since P3 and nothing in the interface
// ever called it, so /model's own completer and every extension's argument
// completions were unreachable.
func TestCommandArgumentsComplete(t *testing.T) {
	a := completionApp(t)
	typeInto(a, "/model anthropic")

	got := completionValues(a)
	if len(got) != 1 || got[0] != "anthropic/claude-opus-5" {
		t.Fatalf("completions = %v, want the matching model", got)
	}

	a.acceptCompletion()
	if v := a.ed.Value(); v != "/model anthropic/claude-opus-5" {
		t.Errorf("accepted into %q", v)
	}
}

func TestAtCompletesAFilePath(t *testing.T) {
	a := completionApp(t)
	typeInto(a, "@mai")

	got := completionValues(a)
	if len(got) != 1 || got[0] != "@main.go" {
		t.Errorf("completions = %v, want @main.go", got)
	}
}

// A path reference is not always at the start of a line, and accepting one must
// leave everything after it alone.
func TestAtCompletesMidLineAndKeepsTheRest(t *testing.T) {
	a := completionApp(t)
	a.ed.SetValue("look at @mai and fix it")
	a.ed.cursor = len([]rune("look at @mai"))
	a.refreshCompletions()

	if got := completionValues(a); len(got) != 1 || got[0] != "@main.go" {
		t.Fatalf("completions = %v", got)
	}

	a.acceptCompletion()
	if v := a.ed.Value(); v != "look at @main.go and fix it" {
		t.Errorf("accepted into %q", v)
	}
	// The caret has to stay where the path ended, or the next keystroke lands
	// at the end of the line.
	if want := len([]rune("look at @main.go")); a.ed.cursor != want {
		t.Errorf("caret at %d, want %d", a.ed.cursor, want)
	}
}

// An email address in a sentence is not a file reference.
func TestAnAtInsideAWordIsNotAPathReference(t *testing.T) {
	a := completionApp(t)
	typeInto(a, "mail ben@gittins")

	if got := completionValues(a); len(got) != 0 {
		t.Errorf("completions = %v, want none", got)
	}
}

// "@" beats a command argument, because it is only ever typed to name a path.
func TestAPathReferenceWinsInsideACommandArgument(t *testing.T) {
	a := completionApp(t)
	typeInto(a, "/model @mai")

	if got := completionValues(a); len(got) != 1 || got[0] != "@main.go" {
		t.Errorf("completions = %v, want the file rather than a model", got)
	}
}

// A directory is half an answer: it should offer what is inside rather than
// sit there looking finished.
func TestAcceptingADirectoryOffersItsContents(t *testing.T) {
	a := completionApp(t)
	typeInto(a, "@intern")

	got := completionValues(a)
	if len(got) == 0 || !strings.HasSuffix(got[0], "/") {
		t.Fatalf("completions = %v, want the directory", got)
	}

	a.acceptCompletion()
	if len(a.completions) == 0 {
		t.Fatalf("accepting a directory offered nothing next, value = %q", a.ed.Value())
	}
	for _, c := range a.completions {
		if !strings.HasPrefix(c.value, "@internal/") {
			t.Errorf("offered %q, want something inside the directory", c.value)
		}
	}
}

func TestPlainProseCompletesNothing(t *testing.T) {
	a := completionApp(t)
	typeInto(a, "what does this function do")

	if got := completionValues(a); len(got) != 0 {
		t.Errorf("completions = %v, want none", got)
	}
}

func TestFindingTheAtPrefix(t *testing.T) {
	for _, tc := range []struct {
		line   string
		want   string
		wantOK bool
	}{
		{"@src", "src", true},
		{"look at @src/ma", "src/ma", true},
		{"@", "", true},
		{"ben@example", "", false},
		{"no reference here", "", false},
		{"@src then more", "", false},
	} {
		text := []rune(tc.line)
		_, prefix, ok := atPrefix(text, len(text))
		if ok != tc.wantOK || prefix != tc.want {
			t.Errorf("atPrefix(%q) = %q,%v want %q,%v", tc.line, prefix, ok, tc.want, tc.wantOK)
		}
	}
}
