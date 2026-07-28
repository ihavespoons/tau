package osenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ihavespoons/tau/agent/env"
)

func TestExecExitCodes(t *testing.T) {
	e, _ := newTestEnv(t)
	tests := []struct {
		name     string
		command  string
		wantCode int
		wantOut  string
	}{
		{"success", "echo hello", 0, "hello\n"},
		{"non-zero exit is data", "exit 3", 3, ""},
		{"stderr captured", "echo oops >&2", 0, "oops\n"},
		{"both streams", "echo out; echo err >&2", 0, "out\nerr\n"},
		{"failing command", "ls /definitely-does-not-exist-xyz", 2, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := e.Exec(context.Background(), tc.command, env.ExecOptions{})
			if err != nil {
				t.Fatalf("Exec returned an error for a non-zero exit: %v", err)
			}
			if tc.wantOut != "" && res.Output != tc.wantOut {
				t.Errorf("output = %q, want %q", res.Output, tc.wantOut)
			}
			if tc.name == "non-zero exit is data" && res.ExitCode != tc.wantCode {
				t.Errorf("exitCode = %d, want %d", res.ExitCode, tc.wantCode)
			}
			if tc.wantCode == 0 && res.ExitCode != 0 {
				t.Errorf("exitCode = %d, want 0", res.ExitCode)
			}
		})
	}
}

func TestExecCwdAndEnv(t *testing.T) {
	e, dir := newTestEnv(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := e.Exec(context.Background(), "pwd", env.ExecOptions{Cwd: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.Output); got != filepath.Join(dir, "sub") {
		t.Errorf("pwd = %q, want %q", got, filepath.Join(dir, "sub"))
	}

	res, err = e.Exec(context.Background(), "echo $TAU_TEST_VAR", env.ExecOptions{
		Env: []string{"TAU_TEST_VAR=hello-from-env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.Output); got != "hello-from-env" {
		t.Errorf("env var = %q", got)
	}
}

func TestExecMissingCwdIsSpawnError(t *testing.T) {
	e, _ := newTestEnv(t)
	_, err := e.Exec(context.Background(), "echo hi", env.ExecOptions{Cwd: "nope/not/here"})
	if !env.IsCode(err, env.CodeSpawnFailed) {
		t.Errorf("got %v, want CodeSpawnFailed", err)
	}
}

func TestExecEmptyCommand(t *testing.T) {
	e, _ := newTestEnv(t)
	if _, err := e.Exec(context.Background(), "   ", env.ExecOptions{}); !env.IsCode(err, env.CodeInvalidCommand) {
		t.Errorf("got %v, want CodeInvalidCommand", err)
	}
}

func TestExecTimeoutKillsAndKeepsPartialOutput(t *testing.T) {
	e, _ := newTestEnv(t)
	start := time.Now()
	res, err := e.Exec(context.Background(), "echo partial; sleep 30", env.ExecOptions{
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut")
	}
	if !strings.Contains(res.Output, "partial") {
		t.Errorf("partial output lost: %q", res.Output)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout did not kill the process: took %v", elapsed)
	}
}

func TestExecContextCancel(t *testing.T) {
	e, _ := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := e.Exec(ctx, "sleep 30", env.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Cancelled {
		t.Error("expected Cancelled")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancel did not kill the process: took %v", elapsed)
	}
}

// The load-bearing test: killing the shell must reap its *grandchildren*.
// A background sleep spawned by the command is a different process from the
// shell; without a process-group kill it survives as an orphan.
func TestExecCancelKillsGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics differ on Windows")
	}
	e, dir := newTestEnv(t)
	marker := filepath.Join(dir, "grandchild.pid")

	ctx, cancel := context.WithCancel(context.Background())
	// The shell backgrounds a sleeper (a grandchild), records its pid, then
	// blocks. Cancelling must kill both.
	command := fmt.Sprintf(`sh -c 'sleep 60 & echo $! > %s; wait' `, marker)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := e.Exec(ctx, command, env.ExecOptions{}); err != nil {
			t.Errorf("Exec: %v", err)
		}
	}()

	// Wait for the grandchild's pid to appear.
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil {
			if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err == nil && pid > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		cancel()
		<-done
		t.Fatal("grandchild pid never recorded")
	}
	if !processAlive(pid) {
		cancel()
		<-done
		t.Fatalf("grandchild %d was not running before cancel", pid)
	}

	cancel()

	// Check for the grandchild's death BEFORE waiting on Exec to return.
	//
	// Waiting on Exec first would make this test vacuous: the orphan inherits
	// the command's stdout pipe, so cmd.Wait() blocks until the sleeper exits
	// on its own — by which point any implementation "passes", just 60s later.
	// Polling here proves the kill actually propagated to the process group.
	killed := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			killed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !killed {
		// Reap the orphan so it does not outlive the test run, and unblock Exec.
		killProcessTree(pid)
		<-done
		t.Fatalf("grandchild %d survived cancellation: the process group was not killed", pid)
	}
	<-done
}

// processAlive reports whether a pid is still running. Signal 0 performs the
// existence and permission checks without delivering a signal.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func TestExecOutputCapSpillsToFile(t *testing.T) {
	e, _ := newTestEnv(t)
	// Emit well past the cap so truncation and spilling both trigger.
	res, err := e.Exec(context.Background(), "for i in $(seq 1 3000); do echo line-$i; done",
		env.ExecOptions{MaxOutputBytes: 2048})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.Truncated {
		t.Fatal("expected Truncated")
	}
	if len(res.Output) > 2048 {
		t.Errorf("output = %d bytes, want <= 2048", len(res.Output))
	}
	if res.FullOutputPath == "" {
		t.Fatal("expected FullOutputPath when truncated")
	}
	t.Cleanup(func() { _ = os.Remove(res.FullOutputPath) })

	full, err := os.ReadFile(res.FullOutputPath)
	if err != nil {
		t.Fatalf("reading spill file: %v", err)
	}
	// The spill file holds the complete stream, head included.
	if !strings.Contains(string(full), "line-1\n") {
		t.Error("spill file is missing the start of the output")
	}
	if !strings.Contains(string(full), "line-3000") {
		t.Error("spill file is missing the end of the output")
	}
	// The in-memory capture keeps the tail, where errors and results live.
	if !strings.Contains(res.Output, "line-3000") {
		t.Error("captured output should retain the tail")
	}
}

func TestExecStreamsOutput(t *testing.T) {
	e, _ := newTestEnv(t)
	var (
		mu     sync.Mutex
		chunks []string
	)
	res, err := e.Exec(context.Background(), "echo one; echo two; echo three", env.ExecOptions{
		OnOutput: func(chunk string) {
			mu.Lock()
			defer mu.Unlock()
			chunks = append(chunks, chunk)
		},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	mu.Lock()
	streamed := strings.Join(chunks, "")
	mu.Unlock()

	if streamed == "" {
		t.Fatal("OnOutput never fired")
	}
	if streamed != res.Output {
		t.Errorf("streamed %q != final %q", streamed, res.Output)
	}
}

func TestExecStdin(t *testing.T) {
	e, _ := newTestEnv(t)
	res, err := e.Exec(context.Background(), "cat", env.ExecOptions{Stdin: "piped input"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Output != "piped input" {
		t.Errorf("output = %q", res.Output)
	}
}

func TestTailBytesUTF8Boundary(t *testing.T) {
	// "é" is two bytes; cutting mid-rune would produce mojibake.
	s := strings.Repeat("é", 10)
	got := tailBytes(s, 5)
	if !utf8ValidString(got) {
		t.Errorf("tailBytes produced invalid UTF-8: %q", got)
	}
	if len(got) > 5 {
		t.Errorf("tailBytes returned %d bytes, want <= 5", len(got))
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
