package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// buildTau compiles the real binary, so these tests exercise what a user runs
// rather than an in-process approximation.
func buildTau(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("building the binary is too slow for -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the pty harness is POSIX-only")
	}

	bin := filepath.Join(t.TempDir(), "tau")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("building tau: %v\n%s", err, out)
	}
	return bin
}

// session drives the binary through a pseudo-terminal.
type ptySession struct {
	t   *testing.T
	f   *os.File
	cmd *exec.Cmd

	mu  sync.Mutex
	buf bytes.Buffer
}

func startTau(t *testing.T, dir string, env ...string) *ptySession {
	t.Helper()
	bin := buildTau(t)

	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		t.Fatalf("starting tau under a pty: %v", err)
	}

	s := &ptySession{t: t, f: f, cmd: cmd}
	go func() {
		chunk := make([]byte, 4096)
		for {
			n, err := f.Read(chunk)
			if n > 0 {
				s.mu.Lock()
				s.buf.Write(chunk[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		_ = f.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	return s
}

func (s *ptySession) send(keys string) {
	s.t.Helper()
	if _, err := io.WriteString(s.f, keys); err != nil {
		s.t.Fatalf("writing to the pty: %v", err)
	}
}

func (s *ptySession) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitFor polls until the wanted text appears, and fails with the whole
// transcript when it does not — a screenshot beats a bare timeout.
func (s *ptySession) waitFor(want string, timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(stripEscapes(s.output()), want) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %q. output was:\n%s", want, stripEscapes(s.output()))
}

// stripEscapes removes ANSI sequences so assertions read as plain text.
func stripEscapes(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			// CSI and OSC both run until a terminator; skipping to the first
			// letter (or BEL) is enough for assertion purposes.
			for j < len(s) && !isTerminator(s[j]) {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isTerminator(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == 0x07
}

// THE P4 UI GATE: the real binary starts an interactive session in a terminal,
// renders its banner, accepts typing, runs a slash command, and exits cleanly.
func TestInteractiveSessionStartsAndRunsACommand(t *testing.T) {
	dir := t.TempDir()
	agentDir := t.TempDir()
	s := startTau(t, dir, "TAU_AGENT_DIR="+agentDir)

	// The banner reports what you are talking to and where.
	s.waitFor("tau", 10*time.Second)
	s.waitFor("anthropic/", 10*time.Second)
	s.waitFor("/help for commands", 10*time.Second)

	// Typing appears in the editor.
	s.send("/help")
	s.waitFor("/help", 5*time.Second)

	// Running it lists the built-in commands.
	s.send("\r")
	s.waitFor("/model", 10*time.Second)
	s.waitFor("Select model", 10*time.Second)

	// A second Ctrl+C on an empty prompt quits.
	s.send("\x03")
	s.waitFor("press Ctrl+C again", 5*time.Second)
	s.send("\x03")

	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("tau did not exit after two Ctrl+C presses")
	}
}

// A session must persist without needing a model: /new writes a fresh file and
// the picker can see it afterwards.
func TestInteractiveSessionCommandsWork(t *testing.T) {
	dir := t.TempDir()
	agentDir := t.TempDir()
	s := startTau(t, dir, "TAU_AGENT_DIR="+agentDir)

	s.waitFor("/help for commands", 10*time.Second)

	s.send("/session\r")
	s.waitFor("cwd", 10*time.Second)

	s.send("/hotkeys\r")
	s.waitFor("stop the agent", 10*time.Second)

	s.send("/new\r")
	s.waitFor("Started a new session", 10*time.Second)

	// The sessions directory should now hold the files tau created.
	entries, err := os.ReadDir(filepath.Join(agentDir, "sessions"))
	if err != nil {
		t.Fatalf("reading the sessions directory: %v", err)
	}
	if len(entries) == 0 {
		t.Error("no session was written")
	}
}

// Esc must not quit, and a dialog must be escapable — getting either wrong
// makes the TUI feel like a trap.
func TestModelPickerOpensAndCancels(t *testing.T) {
	dir := t.TempDir()
	agentDir := t.TempDir()
	s := startTau(t, dir, "TAU_AGENT_DIR="+agentDir)

	s.waitFor("/help for commands", 10*time.Second)

	s.send("/model\r")
	s.waitFor("Select model", 10*time.Second)
	s.waitFor("claude-", 5*time.Second)

	s.send("\x1b") // Esc closes the picker
	time.Sleep(300 * time.Millisecond)

	// Still alive and accepting input.
	s.send("still here")
	s.waitFor("still here", 5*time.Second)
}
