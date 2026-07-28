package coding

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/faux"
	"github.com/ihavespoons/tau/extension"
)

// newLoopWithExtensions builds a bare agent loop wired to a runner, so these
// tests exercise the real hook path without needing a provider or session.
func newLoopWithExtensions(t *testing.T, script *faux.Script, tools []agent.Tool, exts ...extension.Extension) (*agent.Agent, *extension.Runner) {
	t.Helper()
	r := extension.NewRunner(extension.RunnerOptions{Mode: extension.ModePrint, Cwd: t.TempDir()})
	for _, e := range exts {
		if err := r.Load(e); err != nil {
			t.Fatalf("loading %s: %v", e.Name, err)
		}
	}
	all := append(append([]agent.Tool{}, tools...), r.Tools()...)

	cfg := agent.LoopConfig{}
	wireExtensions(&cfg, r)

	a := agent.NewAgent(agent.Options{
		SystemPrompt: "sys", Model: faux.Model(), Tools: all,
		Stream: script.StreamSimple, Config: cfg,
	})
	if sink := extensionSink(r); sink != nil {
		a.Subscribe(sink)
	}
	return a, r
}

func toolCallTurn(name string, args map[string]any) faux.Turn {
	return faux.Turn{Blocks: []faux.Block{{ToolCall: &ai.ToolCall{
		ID: "tc_1", Name: name, Arguments: args,
	}}}}
}

func userMsg(s string) ai.Message {
	return ai.UserMessage{Content: ai.UserContent{Text: s}, Timestamp: 1}
}

// THE P3 GATE: an extension blocks a tool call, and the model sees an error
// result explaining why instead of the tool running.
func TestExtensionGatesToolCall(t *testing.T) {
	dir := t.TempDir()
	secret := dir + "/secret.txt"
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}

	var executed bool
	guarded := agent.MustNew("read_file", "read", "reads a file",
		func(_ context.Context, _ string, p struct {
			Path string `json:"path"`
		}, _ agent.UpdateFunc) (agent.ToolResult, error) {
			executed = true
			b, err := os.ReadFile(p.Path)
			if err != nil {
				return agent.ToolResult{}, err
			}
			return agent.Text("%s", b), nil
		})

	gate := extension.Extension{Name: "permission-gate", Factory: func(api *extension.API) error {
		api.OnToolCall(func(_ context.Context, ev *extension.ToolCallEvent, _ *extension.Context) (*extension.ToolCallResult, error) {
			if path, _ := ev.Args["path"].(string); strings.Contains(path, "secret") {
				return &extension.ToolCallResult{Block: true, Reason: "reading secrets is not permitted"}, nil
			}
			return nil, nil
		})
		return nil
	}}

	script := faux.NewScript(
		toolCallTurn("read_file", map[string]any{"path": secret}),
		faux.Turn{Blocks: []faux.Block{{Text: "understood"}}},
	)
	a, _ := newLoopWithExtensions(t, script, []agent.Tool{guarded}, gate)

	msgs, err := a.Prompt(context.Background(), userMsg("read the secret"))
	if err != nil {
		t.Fatal(err)
	}
	if executed {
		t.Fatal("the tool ran despite being blocked")
	}

	var result *ai.ToolResultMessage
	for _, m := range msgs {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			result = &tr
		}
	}
	if result == nil {
		t.Fatal("no tool result recorded")
	}
	if !result.IsError {
		t.Error("a blocked call must be recorded as an error")
	}
	if got := result.Content[0].(ai.TextContent).Text; !strings.Contains(got, "not permitted") {
		t.Errorf("the model should see the reason, got %q", got)
	}
}

// A non-matching call passes the gate and executes normally.
func TestExtensionGateAllowsUnrelatedCalls(t *testing.T) {
	dir := t.TempDir()
	allowed := dir + "/notes.txt"
	if err := os.WriteFile(allowed, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	var executed bool
	tool := agent.MustNew("read_file", "read", "reads a file",
		func(_ context.Context, _ string, p struct {
			Path string `json:"path"`
		}, _ agent.UpdateFunc) (agent.ToolResult, error) {
			executed = true
			return agent.Text("read %s", p.Path), nil
		})

	gate := extension.Extension{Name: "gate", Factory: func(api *extension.API) error {
		api.OnToolCall(func(_ context.Context, ev *extension.ToolCallEvent, _ *extension.Context) (*extension.ToolCallResult, error) {
			if path, _ := ev.Args["path"].(string); strings.Contains(path, "secret") {
				return &extension.ToolCallResult{Block: true}, nil
			}
			return nil, nil
		})
		return nil
	}}

	script := faux.NewScript(
		toolCallTurn("read_file", map[string]any{"path": allowed}),
		faux.Turn{Blocks: []faux.Block{{Text: "done"}}},
	)
	a, _ := newLoopWithExtensions(t, script, []agent.Tool{tool}, gate)
	if _, err := a.Prompt(context.Background(), userMsg("read notes")); err != nil {
		t.Fatal(err)
	}
	if !executed {
		t.Error("an allowed tool call should execute")
	}
}

// An extension can rewrite a tool's result before the model sees it.
func TestExtensionPatchesToolResult(t *testing.T) {
	tool := agent.MustNew("noisy", "noisy", "returns a lot",
		func(context.Context, string, struct{}, agent.UpdateFunc) (agent.ToolResult, error) {
			return agent.Text("SENSITIVE TOKEN abc123"), nil
		})

	redactor := extension.Extension{Name: "redactor", Factory: func(api *extension.API) error {
		api.OnToolResult(func(_ context.Context, ev *extension.ToolResultEvent, _ *extension.Context) (*extension.ToolResultResult, error) {
			return &extension.ToolResultResult{
				Content: ai.ContentList{ai.TextContent{Text: "[redacted]"}},
			}, nil
		})
		return nil
	}}

	script := faux.NewScript(
		toolCallTurn("noisy", map[string]any{}),
		faux.Turn{Blocks: []faux.Block{{Text: "ok"}}},
	)
	a, _ := newLoopWithExtensions(t, script, []agent.Tool{tool}, redactor)
	msgs, err := a.Prompt(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			if got := tr.Content[0].(ai.TextContent).Text; got != "[redacted]" {
				t.Errorf("tool result = %q, want the patched value", got)
			}
		}
	}
}

// An extension can register a tool the model then calls.
func TestExtensionRegisteredToolIsCallable(t *testing.T) {
	var ran bool
	provider := extension.Extension{Name: "provider", Factory: func(api *extension.API) error {
		api.RegisterTool(agent.MustNew("weather", "weather", "gets weather",
			func(context.Context, string, struct {
				City string `json:"city"`
			}, agent.UpdateFunc) (agent.ToolResult, error) {
				ran = true
				return agent.Text("sunny"), nil
			}))
		return nil
	}}

	script := faux.NewScript(
		toolCallTurn("weather", map[string]any{"city": "Paris"}),
		faux.Turn{Blocks: []faux.Block{{Text: "it is sunny"}}},
	)
	a, _ := newLoopWithExtensions(t, script, nil, provider)
	if _, err := a.Prompt(context.Background(), userMsg("weather in Paris?")); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("extension-registered tool was never called")
	}
}

// An extension can rewrite the transcript the provider sees without touching
// what is recorded in the session.
func TestExtensionRewritesContext(t *testing.T) {
	var sawInProvider int
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "ok"}}})

	trimmer := extension.Extension{Name: "trimmer", Factory: func(api *extension.API) error {
		api.OnContext(func(_ context.Context, ev *extension.ContextEvent, _ *extension.Context) (*extension.ContextResult, error) {
			sawInProvider = len(ev.Messages)
			// Drop everything but the last message.
			return &extension.ContextResult{Messages: ev.Messages[len(ev.Messages)-1:]}, nil
		})
		return nil
	}}

	a, _ := newLoopWithExtensions(t, script, nil, trimmer)
	a.SetMessages([]ai.Message{userMsg("old one"), userMsg("old two")})

	if _, err := a.Prompt(context.Background(), userMsg("new")); err != nil {
		t.Fatal(err)
	}
	if sawInProvider != 3 {
		t.Errorf("context handler saw %d messages, want 3", sawInProvider)
	}
	// The agent's own transcript is untouched by the rewrite.
	if got := len(a.Messages()); got != 4 {
		t.Errorf("transcript = %d messages, want 4 (rewrite must not mutate it)", got)
	}
}

// Lifecycle events reach extensions in the right order.
func TestExtensionObservesLifecycle(t *testing.T) {
	var seen []extension.EventType
	observer := extension.Extension{Name: "observer", Factory: func(api *extension.API) error {
		api.OnAgentStart(func(context.Context, *extension.AgentStartEvent, *extension.Context) error {
			seen = append(seen, extension.EventAgentStart)
			return nil
		})
		api.OnTurnStart(func(context.Context, *extension.TurnStartEvent, *extension.Context) error {
			seen = append(seen, extension.EventTurnStart)
			return nil
		})
		api.OnToolExecutionStart(func(context.Context, *extension.ToolExecutionStartEvent, *extension.Context) error {
			seen = append(seen, extension.EventToolExecutionStart)
			return nil
		})
		api.OnAgentEnd(func(context.Context, *extension.AgentEndEvent, *extension.Context) error {
			seen = append(seen, extension.EventAgentEnd)
			return nil
		})
		return nil
	}}

	tool := agent.MustNew("ping", "ping", "pings",
		func(context.Context, string, struct{}, agent.UpdateFunc) (agent.ToolResult, error) {
			return agent.Text("pong"), nil
		})
	script := faux.NewScript(
		toolCallTurn("ping", map[string]any{}),
		faux.Turn{Blocks: []faux.Block{{Text: "done"}}},
	)
	a, _ := newLoopWithExtensions(t, script, []agent.Tool{tool}, observer)
	if _, err := a.Prompt(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}

	want := []extension.EventType{
		extension.EventAgentStart, extension.EventTurnStart,
		extension.EventToolExecutionStart, extension.EventTurnStart,
		extension.EventAgentEnd,
	}
	if len(seen) != len(want) {
		t.Fatalf("events = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("events = %v, want %v", seen, want)
		}
	}
}

// A broken extension must not stop the agent from working.
func TestBrokenExtensionDoesNotBreakTheRun(t *testing.T) {
	broken := extension.Extension{Name: "broken", Factory: func(api *extension.API) error {
		api.OnTurnStart(func(context.Context, *extension.TurnStartEvent, *extension.Context) error {
			panic("extension exploded")
		})
		return nil
	}}
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "still works"}}})
	a, r := newLoopWithExtensions(t, script, nil, broken)

	msgs, err := a.Prompt(context.Background(), userMsg("go"))
	if err != nil {
		t.Fatalf("a broken extension must not fail the run: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("messages = %d", len(msgs))
	}
	if len(r.Errors()) == 0 {
		t.Error("the failure should be reported")
	}
}
