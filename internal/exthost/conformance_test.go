package exthost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
)

// The dual-surface conformance suite.
//
// The same extension is written three times — once against tau's in-process Go
// API, once as a Go program speaking the wire protocol, once as a TypeScript
// extension under the host shim — and every scenario is run against all three.
// A behaviour that differs between them is a bug in the protocol or in one of
// its two implementations, and the only way to find it is to ask the same
// question three ways.
//
// The in-process surface is the reference. It is the one whose composition
// policies were ported from Pi line by line, so where the three disagree, it is
// the one that is right.

// surface is one way of loading the conformance extension.
type surface struct {
	name string
	// load returns a Runner with the extension loaded, or skips.
	load func(t *testing.T) (*extension.Runner, func())
}

func surfaces() []surface {
	return []surface{
		{name: "in-process", load: loadInProcess},
		{name: "go-subprocess", load: loadGoSubprocess},
		{name: "ts-shim", load: loadTSShim},
	}
}

// --- the reference implementation, in process ---

// conformanceExtension is the behaviour every surface must reproduce.
//
// It is deliberately small and deliberately awkward: each hook exercises a
// composition rule where a wire implementation could plausibly differ from an
// in-process one — a block decision, an argument rewrite, a chained
// replacement, and the difference between "no opinion" and an empty answer.
func conformanceExtension() extension.Extension {
	return extension.Extension{
		Name: "conformance",
		Factory: func(api *extension.API) error {
			api.RegisterTool(conformanceTool{})

			api.OnToolCall(func(_ context.Context, ev *extension.ToolCallEvent, _ *extension.Context) (*extension.ToolCallResult, error) {
				text, _ := ev.Args["text"].(string)
				switch text {
				case "blocked":
					return &extension.ToolCallResult{Block: true, Reason: "conformance says no"}, nil
				case "rewrite":
					ev.Args["text"] = "rewritten"
					return nil, nil
				}
				return nil, nil
			})

			api.OnInput(func(_ context.Context, ev *extension.InputEvent, _ *extension.Context) (*extension.InputResult, error) {
				if ev.Text == "transform-me" {
					return &extension.InputResult{Action: extension.InputTransform, Text: "transformed"}, nil
				}
				// No opinion. Returning a continue result and returning nil
				// have to mean the same thing, and both have to differ from
				// returning a transform to the empty string.
				return nil, nil
			})

			api.OnContext(func(_ context.Context, _ *extension.ContextEvent, _ *extension.Context) (*extension.ContextResult, error) {
				return nil, nil
			})

			api.RegisterCommand(extension.Command{
				Name:        "conform",
				Description: "conformance command",
				Handler: func(_ context.Context, args string, _ *extension.CommandContext) error {
					if args == "fail" {
						return fmt.Errorf("conformance failure")
					}
					return nil
				},
			})
			return nil
		},
	}
}

type conformanceTool struct{}

func (conformanceTool) Def() agent.ToolDef {
	return agent.ToolDef{Name: "conform_echo", Description: "Echo the text back", Label: "conform_echo"}
}

func (conformanceTool) Execute(_ context.Context, callID string, args json.RawMessage, _ agent.UpdateFunc) (agent.ToolResult, error) {
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(args, &p)
	if p.Text == "fail" {
		return agent.ToolResult{}, fmt.Errorf("tool failure")
	}
	return agent.ToolResult{
		Content: ai.ContentList{ai.TextContent{Text: "echo:" + p.Text}},
		Details: map[string]any{"callId": callID},
	}, nil
}

func loadInProcess(t *testing.T) (*extension.Runner, func()) {
	t.Helper()
	r := extension.NewRunner(extension.RunnerOptions{
		Mode: extension.ModeTUI, Cwd: ".", Trusted: true, UI: &stubUI{},
	})
	if err := r.Load(conformanceExtension()); err != nil {
		t.Fatalf("load: %v", err)
	}
	r.Bind(&stubRuntime{})
	return r, func() {}
}

// --- the same behaviour, as a Go subprocess ---

var conformGoBin = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "conform-go")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "conformext")
	out, err := exec.Command("go", "build", "-o", bin, "./testdata/conformext").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", out)
	}
	return bin, nil
})

func loadGoSubprocess(t *testing.T) (*extension.Runner, func()) {
	t.Helper()
	bin, err := conformGoBin()
	if err != nil {
		t.Fatalf("build conformext: %v", err)
	}
	return loadSubprocess(t, Spec{Name: "conformance", Path: "conformext", Command: bin})
}

// --- and as a TypeScript extension under the shim ---

func loadTSShim(t *testing.T) (*extension.Runner, func()) {
	t.Helper()
	node := requireNode(t)
	entry, err := filepath.Abs(filepath.Join("testdata", "piexts", "conformance.ts"))
	if err != nil {
		t.Fatal(err)
	}
	return loadSubprocess(t, Spec{
		Name: "conformance", Path: entry,
		Command: node, Args: []string{shimPath(t), entry},
	})
}

func loadSubprocess(t *testing.T, spec Spec) (*extension.Runner, func()) {
	t.Helper()
	cap := &capture{}
	h, err := Spawn(context.Background(), spec, Options{
		Cwd: t.TempDir(), Trusted: true, Stderr: &cap.stderr,
	})
	if err != nil {
		t.Fatalf("spawn: %v\nstderr: %s", err, cap.stderr.String())
	}
	r := extension.NewRunner(extension.RunnerOptions{
		Mode: extension.ModeTUI, Cwd: ".", Trusted: true, UI: &stubUI{},
	})
	if err := r.Load(h.Extension()); err != nil {
		h.Stop("exit")
		t.Fatalf("load: %v\nstderr: %s", err, cap.stderr.String())
	}
	r.Bind(&stubRuntime{})
	return r, func() { h.Stop("exit") }
}

// --- the scenarios ---

func TestConformanceToolRegistration(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		tool := findTool(t, r, "conform_echo")
		if tool.Def().Description != "Echo the text back" {
			t.Fatalf("description = %q", tool.Def().Description)
		}
	})
}

func TestConformanceToolExecution(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		tool := findTool(t, r, "conform_echo")
		res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"text":"hi"}`), nil)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if got := textOf(res); got != "echo:hi" {
			t.Fatalf("output = %q", got)
		}
		details, _ := res.Details.(map[string]any)
		if details["callId"] != "call-1" {
			t.Fatalf("details = %+v", res.Details)
		}
	})
}

// A tool that fails must fail the same way everywhere, or the model is told
// something different depending on which language the extension was in.
func TestConformanceToolFailure(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		tool := findTool(t, r, "conform_echo")
		_, err := tool.Execute(context.Background(), "c", json.RawMessage(`{"text":"fail"}`), nil)
		if err == nil {
			t.Fatal("a failing tool reported success")
		}
		if !strings.Contains(err.Error(), "tool failure") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestConformanceToolCallBlocks(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		got := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
			ToolCallID: "c", ToolName: "conform_echo",
			Args: map[string]any{"text": "blocked"},
		})
		if got == nil || !got.Block {
			t.Fatalf("not blocked: %+v", got)
		}
		if got.Reason != "conformance says no" {
			t.Fatalf("reason = %q", got.Reason)
		}
	})
}

func TestConformanceToolCallAllows(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		got := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
			ToolCallID: "c", ToolName: "conform_echo",
			Args: map[string]any{"text": "fine"},
		})
		if got != nil && got.Block {
			t.Fatalf("blocked: %+v", got)
		}
	})
}

// In process a handler edits the shared map; over a wire the edit has to
// travel back. The observable result must be identical.
func TestConformanceArgumentRewrite(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		ev := &extension.ToolCallEvent{
			ToolCallID: "c", ToolName: "conform_echo",
			Args: map[string]any{"text": "rewrite"},
		}
		r.EmitToolCall(context.Background(), ev)
		if ev.Args["text"] != "rewritten" {
			t.Fatalf("args = %+v", ev.Args)
		}
	})
}

func TestConformanceInputTransform(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		got := r.EmitInput(context.Background(), "transform-me", nil, "tui", "")
		if got == nil || got.Action != extension.InputTransform || got.Text != "transformed" {
			t.Fatalf("result = %+v", got)
		}
	})
}

// "No opinion" must not become a decision on any surface. An input handler
// that declines has to leave the text alone, not replace it with empty.
func TestConformanceInputPassThrough(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		got := r.EmitInput(context.Background(), "leave-me", nil, "tui", "")
		if got == nil || got.Action != extension.InputContinue {
			t.Fatalf("result = %+v", got)
		}
	})
}

// The same rule for the transcript, where getting it wrong deletes the
// conversation rather than one line of input.
func TestConformanceContextUntouched(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		msgs := []ai.Message{
			ai.UserMessage{Content: ai.UserContent{Text: "one"}, Timestamp: 1},
			ai.UserMessage{Content: ai.UserContent{Text: "two"}, Timestamp: 2},
		}
		got := r.EmitContext(context.Background(), msgs)
		if len(got) != 2 {
			t.Fatalf("%d messages survived, want 2", len(got))
		}
	})
}

func TestConformanceCommandRegistration(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		cmds := r.Commands()
		if len(cmds) != 1 || cmds[0].Name != "conform" {
			t.Fatalf("commands = %+v", cmds)
		}
		if err := cmds[0].Handler(context.Background(), "ok", r.NewCommandContext()); err != nil {
			t.Fatalf("command: %v", err)
		}
		err := cmds[0].Handler(context.Background(), "fail", r.NewCommandContext())
		if err == nil || !strings.Contains(err.Error(), "conformance failure") {
			t.Fatalf("err = %v", err)
		}
	})
}

// An event the extension never subscribed to must reach no surface.
func TestConformanceUnsubscribedEvent(t *testing.T) {
	forEachSurface(t, func(t *testing.T, r *extension.Runner) {
		if got := r.EmitUserBash(context.Background(), &extension.UserBashEvent{Command: "ls"}); got != nil {
			t.Fatalf("an unsubscribed event produced a result: %+v", got)
		}
		if errs := r.Errors(); len(errs) != 0 {
			t.Fatalf("errors = %v", errs)
		}
	})
}

func forEachSurface(t *testing.T, run func(*testing.T, *extension.Runner)) {
	t.Helper()
	for _, s := range surfaces() {
		t.Run(s.name, func(t *testing.T) {
			r, stop := s.load(t)
			defer stop()
			run(t, r)
		})
	}
}
