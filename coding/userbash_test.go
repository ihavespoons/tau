package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/session"
)

func TestParseUserBash(t *testing.T) {
	cases := []struct {
		in      string
		command string
		exclude bool
		ok      bool
	}{
		{"!ls -la", "ls -la", false, true},
		{"  !ls", "ls", false, true},
		{"! ls", "ls", false, true},
		{"!!go test ./...", "go test ./...", true, true},
		{"!", "", false, true},
		{"not a command", "", false, false},
		{"tell me about !important things", "", false, false},
	}
	for _, c := range cases {
		command, exclude, ok := ParseUserBash(c.in)
		if ok != c.ok || command != c.command || exclude != c.exclude {
			t.Errorf("ParseUserBash(%q) = (%q, %v, %v), want (%q, %v, %v)",
				c.in, command, exclude, ok, c.command, c.exclude, c.ok)
		}
	}
}

func TestRunUserBashRecordsTheExecution(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	var streamed strings.Builder
	msg, err := cs.RunUserBash(ctx, "echo hello", false, func(chunk string) {
		streamed.WriteString(chunk)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(msg.Output, "hello") {
		t.Errorf("output = %q", msg.Output)
	}
	if msg.ExitCode == nil || *msg.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", msg.ExitCode)
	}
	if !strings.Contains(streamed.String(), "hello") {
		t.Errorf("output was not streamed as it arrived: %q", streamed.String())
	}

	// The model has to see it on the next turn.
	found := false
	for _, m := range cs.Agent.Messages() {
		if u, ok := m.(ai.UserMessage); ok && strings.Contains(u.Content.String(), "echo hello") {
			found = true
		}
	}
	if !found {
		t.Error("the execution did not reach the model's context")
	}

	// And it has to survive in the session.
	entries := cs.Session.Entries(ctx, nil)
	if len(entries) == 0 {
		t.Fatal("nothing was persisted")
	}
}

// A non-zero exit is data, not an error: the user asked to run it, and what it
// printed is the answer.
func TestRunUserBashKeepsFailedCommands(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	msg, err := cs.RunUserBash(ctx, "exit 3", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ExitCode == nil || *msg.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", msg.ExitCode)
	}
}

// The doubled prefix keeps the run out of the model's context while leaving it
// in the transcript.
func TestRunUserBashCanBeExcludedFromContext(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})
	before := len(cs.Agent.Messages())

	msg, err := cs.RunUserBash(ctx, "echo secret", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !msg.ExcludeFromContext {
		t.Error("the message is not marked excluded")
	}
	if got := len(cs.Agent.Messages()); got != before {
		t.Errorf("the model's context grew by %d, want 0", got-before)
	}

	// Still persisted, so the transcript and any export show what was run.
	sctx, err := cs.Session.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, m := range sctx.Messages {
		if b, ok := m.(*session.BashExecutionMessage); ok && b.Command == "echo secret" {
			seen = true
		}
	}
	if !seen {
		t.Error("an excluded execution should still be recorded in the session")
	}
}

func TestRunUserBashNeedsACommand(t *testing.T) {
	cs := newTestSession(t, Options{})
	if _, err := cs.RunUserBash(context.Background(), "   ", false, nil); err == nil {
		t.Fatal("expected an error")
	}
}
