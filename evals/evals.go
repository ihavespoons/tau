// Package evals runs scripted tasks against an agent and scores what it did.
//
// It is a satellite: nothing in the binary imports it. What it is for is the
// question no unit test answers — whether a change to the prompt, the tools, or
// the model made the agent better or worse at actual work. A task is a prompt, a
// working directory to do it in, and a check over the result.
//
// The agent is supplied by the caller, which is what lets the same task table
// run against ai/faux offline and a real provider when it matters.
package evals

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
)

// Task is one thing to ask the agent to do.
type Task struct {
	Name   string
	Prompt string
	// Files seeds the working directory the task runs in. Paths are relative
	// to it; a task never sees the directory another task ran in.
	Files map[string]string
	// Check decides whether the run passed. A nil check scores any run that
	// did not error as a pass, which is the right default for a task that is
	// only there to see whether the agent falls over.
	Check Check
}

// Check inspects a finished run. Returning an error fails the task, and the
// error is the reason shown in the report.
type Check func(Result) error

// Result is what one task produced.
type Result struct {
	Task string
	// Cwd is the directory the task ran in, kept so a check can read what the
	// agent wrote and a failure can be inspected afterwards.
	Cwd      string
	Messages []ai.Message
	// Output is the assistant's prose, concatenated.
	Output string
	// ToolCalls names every tool the agent invoked, in order.
	ToolCalls []string
	Usage     ai.Usage
	Duration  time.Duration
	// Err is a failure to run at all — a provider error, a cancelled context.
	Err error
	// Failure is the check rejecting an otherwise successful run. The two are
	// separate because "the agent broke" and "the agent did the wrong thing"
	// are different problems.
	Failure error
}

// Passed reports a task that ran and satisfied its check.
func (r Result) Passed() bool { return r.Err == nil && r.Failure == nil }

// NewAgentFunc builds the agent for one task, in its own working directory.
type NewAgentFunc func(ctx context.Context, cwd string) (*agent.Agent, error)

// Runner executes a task table.
type Runner struct {
	NewAgent NewAgentFunc
	// Parallel is how many tasks run at once. Zero means one at a time, which
	// is what a live run against a rate-limited provider wants.
	Parallel int
	// Dir is where task working directories are made. Empty uses a temporary
	// directory that is left behind, because the whole point of a failure is
	// being able to look at what the agent did.
	Dir string
}

// Run executes every task and returns the results in table order.
//
// A task that panics or errors is a result, not an abort: one broken task must
// not cost the report of the rest.
func (r *Runner) Run(ctx context.Context, tasks []Task) []Result {
	results := make([]Result, len(tasks))

	parallel := r.Parallel
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = r.runOne(ctx, task)
		}()
	}
	wg.Wait()
	return results
}

func (r *Runner) runOne(ctx context.Context, task Task) (res Result) {
	res.Task = task.Name

	// A task that panics is a failed task, not a failed run. Recovering here
	// is what keeps one bad check from taking the report with it.
	defer func() {
		if p := recover(); p != nil {
			res.Err = fmt.Errorf("panic: %v", p)
		}
	}()

	cwd, err := r.workdir(task)
	if err != nil {
		res.Err = err
		return res
	}
	res.Cwd = cwd

	ag, err := r.NewAgent(ctx, cwd)
	if err != nil {
		res.Err = err
		return res
	}

	// The tool log is collected from events rather than reconstructed from the
	// messages afterwards, so a check can ask what the agent *did* and not
	// only what it said.
	var mu sync.Mutex
	ag.Subscribe(func(_ context.Context, ev agent.Event) error {
		if ev.Type == agent.EventToolExecutionStart {
			mu.Lock()
			res.ToolCalls = append(res.ToolCalls, ev.ToolName)
			mu.Unlock()
		}
		return nil
	})

	start := time.Now()
	messages, err := ag.Prompt(ctx, ai.UserMessage{
		Content:   ai.UserContent{Text: task.Prompt},
		Timestamp: time.Now().UnixMilli(),
	})
	res.Duration = time.Since(start)
	res.Messages = messages
	res.Output = assistantText(messages)
	res.Usage = totalUsage(messages)

	if err != nil {
		res.Err = err
		return res
	}
	// A stream never returns an error: a provider failure arrives as a terminal
	// event carried on the assistant message. A harness that trusted the error
	// return alone would score a turn that never happened as a pass, which is
	// the one thing an eval must not do.
	if failure := terminalFailure(messages); failure != nil {
		res.Err = failure
		return res
	}
	if task.Check != nil {
		res.Failure = task.Check(res)
	}
	return res
}

// workdir makes the directory a task runs in and seeds it.
func (r *Runner) workdir(task Task) (string, error) {
	base := r.Dir
	if base == "" {
		dir, err := os.MkdirTemp("", "tau-eval-")
		if err != nil {
			return "", err
		}
		base = dir
	}
	cwd := filepath.Join(base, safeName(task.Name))
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return "", err
	}

	for name, body := range task.Files {
		path := filepath.Join(cwd, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	return cwd, nil
}

// safeName turns a task name into something that can be a directory.
func safeName(s string) string {
	if s == "" {
		return "task"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// terminalFailure reports a turn that ended badly, reading the last assistant
// message rather than the error return.
func terminalFailure(messages []ai.Message) error {
	for i := len(messages) - 1; i >= 0; i-- {
		am, ok := messages[i].(ai.AssistantMessage)
		if !ok {
			continue
		}
		switch am.StopReason {
		case ai.StopError:
			if am.ErrorMessage != "" {
				return errors.New(am.ErrorMessage)
			}
			return errors.New("the turn ended in an error")
		case ai.StopAborted:
			return errors.New("the turn was aborted")
		}
		return nil
	}
	return nil
}

func assistantText(messages []ai.Message) string {
	var b strings.Builder
	for _, m := range messages {
		am, ok := m.(ai.AssistantMessage)
		if !ok {
			continue
		}
		for _, c := range am.Content {
			if t, ok := c.(ai.TextContent); ok && t.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t.Text)
			}
		}
	}
	return b.String()
}

func totalUsage(messages []ai.Message) ai.Usage {
	var out ai.Usage
	for _, m := range messages {
		if am, ok := m.(ai.AssistantMessage); ok {
			out.Input += am.Usage.Input
			out.Output += am.Usage.Output
			out.CacheRead += am.Usage.CacheRead
			out.CacheWrite += am.Usage.CacheWrite
			out.Cost.Total += am.Usage.Cost.Total
		}
	}
	return out
}

// --- checks ---

// All passes only when every check does, reporting all the reasons rather than
// the first: a task that failed three ways is more informative than one.
func All(checks ...Check) Check {
	return func(res Result) error {
		var errs []error
		for _, c := range checks {
			if c == nil {
				continue
			}
			if err := c(res); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// Contains requires the agent to have said something, case-insensitively.
func Contains(want string) Check {
	return func(res Result) error {
		if !strings.Contains(strings.ToLower(res.Output), strings.ToLower(want)) {
			return fmt.Errorf("output does not mention %q", want)
		}
		return nil
	}
}

// NotContains requires the agent not to have said something.
func NotContains(unwanted string) Check {
	return func(res Result) error {
		if strings.Contains(strings.ToLower(res.Output), strings.ToLower(unwanted)) {
			return fmt.Errorf("output mentions %q", unwanted)
		}
		return nil
	}
}

// FileExists requires a path, relative to the task's directory, to be there.
func FileExists(name string) Check {
	return func(res Result) error {
		if _, err := os.Stat(filepath.Join(res.Cwd, filepath.FromSlash(name))); err != nil {
			return fmt.Errorf("%s was not created", name)
		}
		return nil
	}
}

// FileContains requires a file the agent wrote to hold something. This is the
// check that matters most: what the agent changed on disk is the work, and
// what it said about the change is commentary.
func FileContains(name, want string) Check {
	return func(res Result) error {
		body, err := os.ReadFile(filepath.Join(res.Cwd, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("%s could not be read: %w", name, err)
		}
		if !strings.Contains(string(body), want) {
			return fmt.Errorf("%s does not contain %q", name, want)
		}
		return nil
	}
}

// ToolCalled requires the agent to have used a tool.
func ToolCalled(name string) Check {
	return func(res Result) error {
		for _, called := range res.ToolCalls {
			if called == name {
				return nil
			}
		}
		return fmt.Errorf("the %s tool was never called (called: %v)", name, res.ToolCalls)
	}
}

// NoTools requires the agent to have answered without doing anything, which is
// what a question rather than a task should produce.
func NoTools() Check {
	return func(res Result) error {
		if len(res.ToolCalls) > 0 {
			return fmt.Errorf("expected no tool use, got %v", res.ToolCalls)
		}
		return nil
	}
}

// --- reporting ---

// Summary aggregates a run.
type Summary struct {
	Total    int
	Passed   int
	Failed   int
	Errored  int
	Duration time.Duration
	Usage    ai.Usage
}

// Summarize counts a set of results.
func Summarize(results []Result) Summary {
	var s Summary
	s.Total = len(results)
	for _, r := range results {
		s.Duration += r.Duration
		s.Usage.Input += r.Usage.Input
		s.Usage.Output += r.Usage.Output
		s.Usage.Cost.Total += r.Usage.Cost.Total
		switch {
		case r.Err != nil:
			s.Errored++
		case r.Failure != nil:
			s.Failed++
		default:
			s.Passed++
		}
	}
	return s
}

// Report renders results as text, failures first.
//
// Failures lead because a report is read to find out what broke, and a passing
// task's only useful detail is that it passed.
func Report(results []Result) string {
	var b strings.Builder
	s := Summarize(results)

	for _, r := range results {
		if r.Passed() {
			continue
		}
		reason := r.Failure
		label := "FAIL"
		if r.Err != nil {
			reason, label = r.Err, "ERROR"
		}
		fmt.Fprintf(&b, "%-5s %s\n      %v\n", label, r.Task, reason)
		if r.Cwd != "" {
			fmt.Fprintf(&b, "      left in %s\n", r.Cwd)
		}
	}
	for _, r := range results {
		if r.Passed() {
			fmt.Fprintf(&b, "ok    %s (%s)\n", r.Task, r.Duration.Round(time.Millisecond))
		}
	}

	fmt.Fprintf(&b, "\n%d/%d passed", s.Passed, s.Total)
	if s.Failed > 0 {
		fmt.Fprintf(&b, ", %d failed", s.Failed)
	}
	if s.Errored > 0 {
		fmt.Fprintf(&b, ", %d errored", s.Errored)
	}
	if s.Usage.Cost.Total > 0 {
		fmt.Fprintf(&b, " · $%.4f", s.Usage.Cost.Total)
	}
	b.WriteByte('\n')
	return b.String()
}
