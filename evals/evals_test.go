package evals

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/faux"
)

// writeParams is the argument to the stand-in tool below.
type writeParams struct {
	Path string `json:"path"`
	Body string `json:"body"`
}

// writeTool actually writes, so a FileContains check is testing the file the
// agent produced rather than a recording of an intention.
func writeTool(cwd string) agent.Tool {
	return agent.MustNew("write", "write", "write a file",
		func(_ context.Context, _ string, p writeParams, _ agent.UpdateFunc) (agent.ToolResult, error) {
			path := filepath.Join(cwd, filepath.FromSlash(p.Path))
			if err := os.WriteFile(path, []byte(p.Body), 0o644); err != nil {
				return agent.ToolResult{}, err
			}
			return agent.ToolResult{
				Content: ai.ContentList{ai.TextContent{Text: "wrote " + p.Path}},
			}, nil
		})
}

// runner builds a runner whose agent replays a script per task.
func runner(t *testing.T, script func() *faux.Script, withTool bool) *Runner {
	t.Helper()
	return &Runner{
		Dir: t.TempDir(),
		NewAgent: func(_ context.Context, cwd string) (*agent.Agent, error) {
			opts := agent.Options{Model: faux.Model(), Stream: script().StreamSimple}
			if withTool {
				opts.Tools = []agent.Tool{writeTool(cwd)}
			}
			return agent.NewAgent(opts), nil
		},
	}
}

func says(text string) func() *faux.Script {
	return func() *faux.Script {
		return faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: text}}})
	}
}

func TestAPassingTask(t *testing.T) {
	r := runner(t, says("the answer is 42"), false)

	results := r.Run(context.Background(), []Task{
		{Name: "asks a question", Prompt: "what is it", Check: Contains("42")},
	})

	if len(results) != 1 {
		t.Fatalf("%d results", len(results))
	}
	if !results[0].Passed() {
		t.Errorf("task failed: err=%v failure=%v", results[0].Err, results[0].Failure)
	}
	if results[0].Output != "the answer is 42" {
		t.Errorf("output = %q", results[0].Output)
	}
}

// A check rejecting the run and the run breaking are different problems, and
// the result keeps them apart.
func TestAFailedCheckIsNotAnError(t *testing.T) {
	r := runner(t, says("no idea"), false)

	res := r.Run(context.Background(), []Task{
		{Name: "wrong answer", Prompt: "what is it", Check: Contains("42")},
	})[0]

	if res.Passed() {
		t.Fatal("the task should have failed its check")
	}
	if res.Err != nil {
		t.Errorf("a failed check was reported as an error: %v", res.Err)
	}
	if res.Failure == nil {
		t.Error("no reason was recorded")
	}
}

func TestAProviderFailureIsAnError(t *testing.T) {
	r := runner(t, func() *faux.Script {
		return faux.NewScript(faux.Turn{ErrorMessage: "the provider hung up"})
	}, false)

	res := r.Run(context.Background(), []Task{{Name: "broken", Prompt: "go"}})[0]
	if res.Err == nil {
		t.Fatal("a provider failure was not reported as an error")
	}
	if res.Failure != nil {
		t.Errorf("a broken run also recorded a check failure: %v", res.Failure)
	}
}

// Seeded files are what makes a task about existing code rather than about a
// blank directory.
func TestFilesAreSeededIntoTheWorkingDirectory(t *testing.T) {
	r := runner(t, says("read it"), false)

	res := r.Run(context.Background(), []Task{{
		Name:   "with a repo",
		Prompt: "look",
		Files:  map[string]string{"main.go": "package main", "internal/x.go": "package internal"},
	}})[0]

	for _, name := range []string{"main.go", "internal/x.go"} {
		if _, err := os.Stat(filepath.Join(res.Cwd, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s was not seeded: %v", name, err)
		}
	}
}

// Each task gets its own directory, or one task's mess would score another.
func TestTasksDoNotShareADirectory(t *testing.T) {
	r := runner(t, says("ok"), false)

	results := r.Run(context.Background(), []Task{
		{Name: "first", Prompt: "go", Files: map[string]string{"a.txt": "a"}},
		{Name: "second", Prompt: "go", Files: map[string]string{"b.txt": "b"}},
	})

	if results[0].Cwd == results[1].Cwd {
		t.Fatal("both tasks ran in the same directory")
	}
	if _, err := os.Stat(filepath.Join(results[0].Cwd, "b.txt")); err == nil {
		t.Error("the second task's files leaked into the first")
	}
}

// What the agent changed on disk is the work; what it said about it is
// commentary.
func TestCheckingWhatTheAgentWrote(t *testing.T) {
	r := runner(t, func() *faux.Script {
		return faux.NewScript(
			faux.Turn{Blocks: []faux.Block{{ToolCall: &ai.ToolCall{
				ID: "c1", Name: "write",
				Arguments: map[string]any{"path": "out.txt", "body": "hello from the tool"},
			}}}},
			faux.Turn{Blocks: []faux.Block{{Text: "done"}}},
		)
	}, true)

	res := r.Run(context.Background(), []Task{{
		Name:   "writes a file",
		Prompt: "write it",
		Check: All(
			FileExists("out.txt"),
			FileContains("out.txt", "hello from the tool"),
			ToolCalled("write"),
		),
	}})[0]

	if !res.Passed() {
		t.Fatalf("failed: err=%v failure=%v", res.Err, res.Failure)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0] != "write" {
		t.Errorf("tool calls = %v", res.ToolCalls)
	}
}

func TestChecksThatShouldFail(t *testing.T) {
	r := runner(t, says("I used no tools"), false)
	res := r.Run(context.Background(), []Task{{Name: "t", Prompt: "go"}})[0]

	for name, check := range map[string]Check{
		"missing file":  FileExists("nope.txt"),
		"unread file":   FileContains("nope.txt", "x"),
		"uncalled tool": ToolCalled("write"),
		"not contains":  Contains("something else"),
		"forbidden":     NotContains("no tools"),
	} {
		if err := check(res); err == nil {
			t.Errorf("%s: check passed when it should not have", name)
		}
	}
	// And the one that should pass, since nothing was called.
	if err := NoTools()(res); err != nil {
		t.Errorf("NoTools failed on a run with no tools: %v", err)
	}
}

// A report is read to find out what broke, so every reason is worth having
// rather than only the first.
func TestAllReportsEveryReason(t *testing.T) {
	err := All(Contains("alpha"), Contains("beta"))(Result{Output: "neither"})
	if err == nil {
		t.Fatal("expected a failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "alpha") || !strings.Contains(msg, "beta") {
		t.Errorf("only some reasons were reported: %v", msg)
	}
}

// One broken check must not cost the report of every other task.
func TestAPanickingCheckIsJustAFailedTask(t *testing.T) {
	r := runner(t, says("fine"), false)

	results := r.Run(context.Background(), []Task{
		{Name: "explodes", Prompt: "go", Check: func(Result) error { panic("boom") }},
		{Name: "fine", Prompt: "go", Check: Contains("fine")},
	})

	if results[0].Err == nil {
		t.Error("the panicking task was not recorded as an error")
	}
	if !results[1].Passed() {
		t.Errorf("the panic took the other task with it: %+v", results[1])
	}
}

// Results come back in table order however they were scheduled, or a report
// would shuffle between runs.
func TestResultsKeepTableOrderWhenParallel(t *testing.T) {
	r := runner(t, says("ok"), false)
	r.Parallel = 4

	var tasks []Task
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		tasks = append(tasks, Task{Name: name, Prompt: "go"})
	}

	results := r.Run(context.Background(), tasks)
	for i, want := range []string{"one", "two", "three", "four", "five"} {
		if results[i].Task != want {
			t.Errorf("result %d is %q, want %q", i, results[i].Task, want)
		}
	}
}

func TestSummarizeCountsEachOutcome(t *testing.T) {
	s := Summarize([]Result{
		{Task: "a"},
		{Task: "b", Failure: errors.New("wrong")},
		{Task: "c", Err: errors.New("broke")},
	})

	if s.Total != 3 || s.Passed != 1 || s.Failed != 1 || s.Errored != 1 {
		t.Errorf("summary = %+v", s)
	}
}

func TestReportLeadsWithFailures(t *testing.T) {
	out := Report([]Result{
		{Task: "passed-one"},
		{Task: "failed-one", Failure: errors.New("did not mention 42")},
	})

	failAt := strings.Index(out, "failed-one")
	passAt := strings.Index(out, "passed-one")
	if failAt < 0 || passAt < 0 {
		t.Fatalf("report is missing tasks:\n%s", out)
	}
	if failAt > passAt {
		t.Errorf("passing tasks came first:\n%s", out)
	}
	if !strings.Contains(out, "did not mention 42") {
		t.Errorf("the reason is missing:\n%s", out)
	}
	if !strings.Contains(out, "1/2 passed") {
		t.Errorf("the tally is missing:\n%s", out)
	}
}
