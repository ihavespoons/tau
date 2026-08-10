package tui

import (
	"strings"
	"testing"

	"github.com/ihavespoons/tau/session"
)

func TestEditorReportsBashMode(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"!ls", true},
		{"  !ls", true},
		{"!!go test", true},
		{"ls", false},
		{"", false},
		{"tell me about !important things", false},
	}
	for _, c := range cases {
		e := newTestEditor()
		e.SetValue(c.text)
		if got := e.BashMode(); got != c.want {
			t.Errorf("BashMode(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// The gutter has to change the moment the line becomes a command, so there is
// never a question about where Enter will send it.
func TestBashModeChangesThePrompt(t *testing.T) {
	forceColor(t)

	plain := newTestEditor()
	plain.SetValue("a message")

	bash := newTestEditor()
	bash.SetValue("!a command")

	if plain.View(true) == bash.View(true) {
		t.Error("the prompt looks the same in both modes")
	}
}

func TestUserBashHeaderMarksExcludedRuns(t *testing.T) {
	forceColor(t)
	r := newRenderer(DefaultTheme(), 80, false)

	shown := strings.Join(r.userBash("go test ./...", false), "\n")
	hidden := strings.Join(r.userBash("go test ./...", true), "\n")

	if !strings.Contains(stripANSI(shown), "$ go test ./...") {
		t.Errorf("header = %q", stripANSI(shown))
	}
	if shown == hidden {
		t.Error("a run the model will never see should not look identical to one it will")
	}
}

func TestUserBashResultReportsHowItWent(t *testing.T) {
	r := newRenderer(DefaultTheme(), 80, false)
	code := 2

	out := stripANSI(strings.Join(r.userBashResult(&session.BashExecutionMessage{
		Command:  "go build ./...",
		Output:   "some output",
		ExitCode: &code,
	}), "\n"))

	if !strings.Contains(out, "some output") {
		t.Errorf("output is missing:\n%s", out)
	}
	if !strings.Contains(out, "exit 2") {
		t.Errorf("a non-zero exit should be called out:\n%s", out)
	}
}

func TestUserBashResultIsQuietOnSuccess(t *testing.T) {
	r := newRenderer(DefaultTheme(), 80, false)
	zero := 0

	out := stripANSI(strings.Join(r.userBashResult(&session.BashExecutionMessage{
		Command:  "true",
		Output:   "fine",
		ExitCode: &zero,
	}), "\n"))

	if strings.Contains(out, "exit") {
		t.Errorf("a successful command should not report its exit code:\n%s", out)
	}
}

// Truncated output is only useful if the reader is told where the rest went.
func TestUserBashResultPointsAtTruncatedOutput(t *testing.T) {
	r := newRenderer(DefaultTheme(), 80, false)
	zero := 0

	out := stripANSI(strings.Join(r.userBashResult(&session.BashExecutionMessage{
		Command:        "cat big",
		Output:         "the first part",
		ExitCode:       &zero,
		Truncated:      true,
		FullOutputPath: "/tmp/tau-out-123",
	}), "\n"))

	if !strings.Contains(out, "/tmp/tau-out-123") {
		t.Errorf("the full output was not pointed at:\n%s", out)
	}
}

func TestUserBashResultReportsCancellation(t *testing.T) {
	r := newRenderer(DefaultTheme(), 80, false)
	code := 130

	out := stripANSI(strings.Join(r.userBashResult(&session.BashExecutionMessage{
		Command:   "sleep 100",
		Cancelled: true,
		ExitCode:  &code,
	}), "\n"))

	if !strings.Contains(out, "cancelled") {
		t.Errorf("cancellation was not reported:\n%s", out)
	}
	if strings.Contains(out, "exit 130") {
		t.Errorf("a cancelled command should not also report an exit code:\n%s", out)
	}
}
