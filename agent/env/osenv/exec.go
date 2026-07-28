package osenv

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/tau/agent/env"
)

// DefaultMaxOutputBytes caps captured command output before it spills to a
// temp file. Matches Pi's DEFAULT_MAX_BYTES.
const DefaultMaxOutputBytes = 50 * 1024

// Exec implements env.Shell.
//
// Semantics ported from Pi: a non-zero exit code is data, not an error — only
// spawn and environment failures return an error. Cancellation and timeouts
// kill the whole process group and preserve whatever output arrived first.
func (e *OSEnv) Exec(ctx context.Context, command string, opts env.ExecOptions) (env.ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return env.ExecResult{}, env.Errorf(env.CodeInvalidCommand, "", "empty command")
	}

	cwd := e.cwd
	if opts.Cwd != "" {
		abs, err := e.Abs(opts.Cwd)
		if err != nil {
			return env.ExecResult{}, err
		}
		cwd = abs
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return env.ExecResult{}, env.Errorf(env.CodeSpawnFailed, cwd,
			"working directory does not exist: %s", cwd)
	}

	shell, args, err := e.shellConfig()
	if err != nil {
		return env.ExecResult{}, err
	}

	maxBytes := opts.MaxOutputBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxOutputBytes
	}

	// The command context is separate from the caller's so a timeout can be
	// distinguished from an upstream cancellation.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.Command(shell, append(args, command)...) //nolint:gosec // executing a shell command is this function's purpose
	cmd.Dir = cwd
	cmd.Env = e.commandEnv(opts.Env)
	setProcessGroup(cmd)
	if opts.Stdin != "" {
		cmd.Stdin = strings.NewReader(opts.Stdin)
	}

	sink := newOutputSink(maxBytes, opts.OnOutput)
	cmd.Stdout = sink
	cmd.Stderr = sink

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return env.ExecResult{}, env.Errorf(env.CodeSpawnFailed, cwd, "starting command: %v", err)
	}
	pid := cmd.Process.Pid

	var (
		timedOut  bool
		killMu    sync.Mutex
		waitDone  = make(chan struct{})
		watchStop = make(chan struct{})
	)
	markTimedOut := func() {
		killMu.Lock()
		timedOut = true
		killMu.Unlock()
	}

	// Watchdog: kill the process group on cancellation or timeout. Killing the
	// group (not just cmd.Process) is what reaps grandchildren.
	var timeoutCh <-chan time.Time
	if opts.Timeout > 0 {
		timer := time.NewTimer(opts.Timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}
	go func() {
		select {
		case <-runCtx.Done():
			killProcessTree(pid)
		case <-timeoutCh:
			markTimedOut()
			killProcessTree(pid)
		case <-watchStop:
		}
	}()

	waitErr := cmd.Wait()
	close(watchStop)
	close(waitDone)

	sink.close()
	killMu.Lock()
	didTimeout := timedOut
	killMu.Unlock()

	cancelled := ctx.Err() != nil
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
			if exitCode < 0 {
				// Killed by a signal: report the conventional 128+SIGKILL.
				exitCode = 137
			}
		} else {
			return env.ExecResult{}, env.Errorf(env.CodeSpawnFailed, cwd, "running command: %v", waitErr)
		}
	}

	content, truncated, fullPath, spillErr := sink.result()
	if spillErr != nil {
		return env.ExecResult{}, spillErr
	}

	return env.ExecResult{
		Output:         content,
		ExitCode:       exitCode,
		TimedOut:       didTimeout,
		Cancelled:      cancelled,
		Truncated:      truncated,
		FullOutputPath: fullPath,
		Duration:       time.Since(started),
	}, nil
}

// commandEnv layers the environment: process env (when inheriting), then the
// OSEnv defaults, then per-call overrides.
func (e *OSEnv) commandEnv(extra []string) []string {
	var out []string
	if e.inherit {
		out = append(out, os.Environ()...)
	}
	out = append(out, e.extraEnv...)
	out = append(out, extra...)
	return out
}

// shellConfig resolves the shell to run commands with. Pi prefers /bin/bash,
// falls back to bash on PATH, then sh; on Windows it hunts for Git Bash.
func (e *OSEnv) shellConfig() (string, []string, error) {
	if e.shellPath != "" {
		if _, err := os.Stat(e.shellPath); err != nil {
			return "", nil, env.Errorf(env.CodeSpawnFailed, e.shellPath,
				"custom shell path not found: %s", e.shellPath)
		}
		return e.shellPath, []string{"-c"}, nil
	}

	if runtime.GOOS == "windows" {
		var candidates []string
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			candidates = append(candidates, filepath.Join(pf, "Git", "bin", "bash.exe"))
		}
		if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
			candidates = append(candidates, filepath.Join(pf86, "Git", "bin", "bash.exe"))
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, []string{"-c"}, nil
			}
		}
		if p, err := exec.LookPath("bash.exe"); err == nil {
			return p, []string{"-c"}, nil
		}
		return "", nil, env.Errorf(env.CodeSpawnFailed, "",
			"no bash shell found. Install Git for Windows (https://git-scm.com/download/win), "+
				"add bash to PATH, or configure an explicit shell path")
	}

	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash", []string{"-c"}, nil
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p, []string{"-c"}, nil
	}
	return "sh", []string{"-c"}, nil
}

// outputSink captures command output with bounded memory: it keeps a rolling
// tail for display and spills the complete stream to a temp file once the cap
// is exceeded (Pi's OutputAccumulator).
type outputSink struct {
	mu       sync.Mutex
	maxBytes int
	onOutput func(string)

	buf       []byte
	totalSize int
	spill     *os.File
	spillPath string
	spillErr  error
	closed    bool
}

var _ io.Writer = (*outputSink)(nil)

func newOutputSink(maxBytes int, onOutput func(string)) *outputSink {
	return &outputSink{maxBytes: maxBytes, onOutput: onOutput}
}

// Write is called concurrently by the stdout and stderr pipes, so it locks.
func (s *outputSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return len(p), nil
	}
	s.totalSize += len(p)

	// Once over the cap, the full stream goes to a temp file and memory keeps
	// only the tail.
	if s.totalSize > s.maxBytes && s.spill == nil && s.spillErr == nil {
		s.openSpill()
	}
	if s.spill != nil {
		if _, err := s.spill.Write(p); err != nil && s.spillErr == nil {
			s.spillErr = err
		}
	}

	s.buf = append(s.buf, p...)
	if over := len(s.buf) - s.rollingCap(); over > 0 {
		s.buf = s.buf[over:]
	}
	onOutput := s.onOutput
	s.mu.Unlock()

	if onOutput != nil {
		onOutput(string(p))
	}
	return len(p), nil
}

// rollingCap keeps twice the display cap in memory so tail truncation can
// still land on a line boundary.
func (s *outputSink) rollingCap() int {
	if s.maxBytes*2 < 1 {
		return 1
	}
	return s.maxBytes * 2
}

// openSpill must be called with the lock held.
func (s *outputSink) openSpill() {
	f, err := os.CreateTemp("", "tau-bash-*.log")
	if err != nil {
		s.spillErr = err
		return
	}
	// Everything buffered so far belongs at the head of the spill file.
	if _, err := f.Write(s.buf); err != nil {
		s.spillErr = err
		_ = f.Close()
		return
	}
	s.spill = f
	s.spillPath = f.Name()
}

func (s *outputSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.spill != nil {
		if err := s.spill.Close(); err != nil && s.spillErr == nil {
			s.spillErr = err
		}
		s.spill = nil
	}
}

// result returns the captured tail plus the spill path when truncated.
func (s *outputSink) result() (content string, truncated bool, fullPath string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spillErr != nil {
		return "", false, "", env.Errorf(env.CodeIO, s.spillPath,
			"writing full output: %v", s.spillErr)
	}
	truncated = s.totalSize > s.maxBytes
	out := string(s.buf)
	if truncated {
		out = tailBytes(out, s.maxBytes)
		fullPath = s.spillPath
	}
	return out, truncated, fullPath, nil
}

// tailBytes keeps the last maxBytes of text, snapped forward to a UTF-8
// boundary so the result is never mojibake.
func tailBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && s[start]&0xc0 == 0x80 {
		start++
	}
	return s[start:]
}
