package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// exportedSessionFile writes a session out and returns the file, so the import
// tests work against something tau really produced rather than a hand-built
// fixture that could drift from the format.
func exportedSessionFile(t *testing.T, cs *Session) string {
	t.Helper()
	ctx := context.Background()
	if _, err := cs.Session.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "exported.jsonl")
	if _, err := cs.ExportSession(ctx, path); err != nil {
		t.Fatalf("exporting a session to import: %v", err)
	}
	return path
}

func TestImportAdoptsASessionFile(t *testing.T) {
	ctx := context.Background()

	source := newTestSession(t, Options{})
	file := exportedSessionFile(t, source)

	cs := newTestSession(t, Options{})
	before := cs.Path

	out, err := cs.ImportSession(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, file) {
		t.Errorf("output = %q, want it to name the file", out)
	}
	if cs.Path == before {
		t.Error("the session was not replaced")
	}

	// The copy lands in this directory's session folder, which is what makes
	// it show up in /resume afterwards.
	if filepath.Dir(cs.Path) == filepath.Dir(file) {
		t.Errorf("the session was opened in place, at %s", cs.Path)
	}
	metas, err := cs.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range metas {
		if m.Path == cs.Path {
			found = true
		}
	}
	if !found {
		t.Errorf("the imported session is not listed for this directory: %+v", metas)
	}

	// Writes go to tau's copy, not to the file that was handed over.
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Session.AppendMessage(ctx, ai.UserMessage{Content: ai.UserContent{Text: "after importing"}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(original) {
		t.Error("importing wrote back to the source file")
	}
}

func TestImportRefusesWhatIsNotASession(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	junk := filepath.Join(t.TempDir(), "notes.jsonl")
	if err := os.WriteFile(junk, []byte("this is not a session\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := cs.ImportSession(ctx, junk); err == nil {
		t.Fatal("expected an error")
	}
	// And nothing was copied on the way to finding that out.
	entries, err := os.ReadDir(filepath.Dir(cs.Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "notes.jsonl" {
			t.Error("a rejected file was copied into the session directory")
		}
	}
}

func TestImportReportsAMissingFile(t *testing.T) {
	cs := newTestSession(t, Options{})

	_, err := cs.ImportSession(context.Background(), "no/such/session.jsonl")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no/such/session.jsonl") {
		t.Errorf("err = %v, want it to name the path", err)
	}
}

func TestImportNeedsAPath(t *testing.T) {
	cs := newTestSession(t, Options{})
	if _, err := cs.ImportSession(context.Background(), "   "); err == nil {
		t.Fatal("expected an error")
	}
}

// Importing the same file twice would otherwise overwrite the first copy, and
// a session file is not something to clobber quietly.
func TestImportRefusesToOverwriteAnExistingCopy(t *testing.T) {
	ctx := context.Background()
	source := newTestSession(t, Options{})
	file := exportedSessionFile(t, source)

	cs := newTestSession(t, Options{})
	if _, err := cs.ImportSession(ctx, file); err != nil {
		t.Fatal(err)
	}
	_, err := cs.ImportSession(ctx, file)
	if err == nil {
		t.Fatal("expected the second import to be refused")
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Errorf("err = %v, want it to say what to do", err)
	}
}

func TestImportCommandReplacesTheSession(t *testing.T) {
	ctx := context.Background()
	source := newTestSession(t, Options{})
	file := exportedSessionFile(t, source)

	cs := newTestSession(t, Options{})
	res, err := cs.RunCommand(ctx, "/import "+file)
	if err != nil {
		t.Fatal(err)
	}
	if !res.SessionChanged {
		t.Error("the host was not told the session changed")
	}
}
