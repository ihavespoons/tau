package coding

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/session"
)

// otherSession creates a second session in the same directory, so the tests
// have something to act on that is not the one in progress.
func otherSession(t *testing.T, cs *Session) session.Metadata {
	t.Helper()
	ctx := context.Background()
	sess, err := cs.repo.Create(ctx, session.CreateSessionOptions{Cwd: cs.Cwd})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := sess.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestDeleteSessionRemovesTheFile(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})
	other := otherSession(t, cs)

	out, err := cs.DeleteSession(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, other.Path) {
		t.Errorf("output = %q, want it to name the file", out)
	}
	if _, err := os.Stat(other.Path); !os.IsNotExist(err) {
		t.Errorf("the session file is still there: %v", err)
	}
}

// Deleting the file being appended to would leave the agent writing into
// nothing.
func TestDeleteSessionRefusesTheCurrentOne(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	meta := session.Metadata{Path: cs.Path, Cwd: cs.Cwd}
	if _, err := cs.DeleteSession(ctx, meta); err == nil {
		t.Fatal("expected the current session to be refused")
	}
	if _, err := os.Stat(cs.Path); err != nil {
		t.Errorf("the current session was deleted anyway: %v", err)
	}
}

func TestRenameSessionNamesAnotherSession(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})
	other := otherSession(t, cs)

	if _, err := cs.RenameSession(ctx, other, "the other one"); err != nil {
		t.Fatal(err)
	}

	// Read it back through a fresh handle: the name has to be in the file, not
	// in memory somewhere.
	reopened, err := cs.repo.Open(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	name, ok := reopened.Name(ctx)
	if !ok || name != "the other one" {
		t.Errorf("name = %q (ok=%v), want %q", name, ok, "the other one")
	}
}

// The session in progress is already open; naming it through a second handle
// would append behind the open one's back.
func TestRenameSessionUsesTheOpenHandleForTheCurrentOne(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	meta := session.Metadata{Path: cs.Path, Cwd: cs.Cwd}
	if _, err := cs.RenameSession(ctx, meta, "this one"); err != nil {
		t.Fatal(err)
	}
	name, ok := cs.Session.Name(ctx)
	if !ok || name != "this one" {
		t.Errorf("name = %q (ok=%v)", name, ok)
	}
}

func TestSessionOpsNeedAPath(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	if _, err := cs.DeleteSession(ctx, session.Metadata{}); err == nil {
		t.Error("expected an error for an empty path")
	}
	if _, err := cs.RenameSession(ctx, session.Metadata{}, "x"); err == nil {
		t.Error("expected an error for an empty path")
	}
}
