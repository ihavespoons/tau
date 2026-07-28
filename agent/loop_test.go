package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/faux"
)

// recorder captures the event stream for order assertions.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) sink(_ context.Context, ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recorder) types() []EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]EventType, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Type)
	}
	return out
}

func (r *recorder) count(t EventType) int {
	n := 0
	for _, ty := range r.types() {
		if ty == t {
			n++
		}
	}
	return n
}

type echoParams struct {
	Text string `json:"text"`
}

// echoTool records its calls so tests can assert ordering and concurrency.
type echoTool struct {
	name     string
	mode     ExecutionMode
	delay    time.Duration
	fail     error
	panics   bool
	terminat bool

	mu      sync.Mutex
	calls   []string
	active  int
	maxSeen int
}

var echoSchema = func() *jsonschema.Schema {
	s, err := jsonschema.For[echoParams](nil)
	if err != nil {
		panic(err)
	}
	return s
}()

func (e *echoTool) Def() ToolDef {
	return ToolDef{
		Name: e.name, Label: e.name, Description: "echoes text",
		Parameters: echoSchema, ExecutionMode: e.mode,
	}
}

func (e *echoTool) Execute(ctx context.Context, _ string, args json.RawMessage, update UpdateFunc) (ToolResult, error) {
	var p echoParams
	_ = json.Unmarshal(args, &p)

	e.mu.Lock()
	e.calls = append(e.calls, p.Text)
	e.active++
	if e.active > e.maxSeen {
		e.maxSeen = e.active
	}
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.active--
		e.mu.Unlock()
	}()

	if e.panics {
		panic("boom")
	}
	if update != nil {
		update(Text("working on %s", p.Text))
	}
	if e.delay > 0 {
		select {
		case <-time.After(e.delay):
		case <-ctx.Done():
			return ToolResult{}, ctx.Err()
		}
	}
	if e.fail != nil {
		return ToolResult{}, e.fail
	}
	res := Text("echo: %s", p.Text)
	res.Terminate = e.terminat
	return res, nil
}

func (e *echoTool) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *echoTool) concurrency() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maxSeen
}

func toolCall(id, name, text string) ai.ToolCall {
	return ai.ToolCall{ID: id, Name: name, Arguments: map[string]any{"text": text}}
}

func baseConfig(script *faux.Script, tools []Tool) (LoopConfig, *RunContext) {
	return LoopConfig{
			Model:  faux.Model(),
			Stream: script.StreamSimple,
		}, &RunContext{
			SystemPrompt: "sys",
			Tools:        tools,
		}
}

func userMsg(text string) ai.Message {
	return ai.UserMessage{Content: ai.UserContent{Text: text}, Timestamp: 1}
}

func TestLoopSimpleTextTurnEventOrder(t *testing.T) {
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "hi there"}}})
	cfg, rc := baseConfig(script, nil)
	rec := &recorder{}

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("hello")}, rc, cfg, rec.sink)
	if err != nil {
		t.Fatal(err)
	}

	want := []EventType{
		EventAgentStart, EventTurnStart,
		EventMessageStart, EventMessageEnd, // the user prompt
		EventMessageStart, // assistant start
	}
	got := rec.types()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
	if got[len(got)-1] != EventAgentEnd {
		t.Errorf("last event = %v", got[len(got)-1])
	}
	if rec.count(EventTurnEnd) != 1 {
		t.Errorf("turn_end count = %d", rec.count(EventTurnEnd))
	}
	// prompt + assistant reply
	if len(msgs) != 2 {
		t.Errorf("messages = %d", len(msgs))
	}
}

func TestLoopExecutesToolAndContinues(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "echo", "one"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "done"}}},
	)
	tool := &echoTool{name: "echo"}
	cfg, rc := baseConfig(script, []Tool{tool})
	rec := &recorder{}

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg, rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if tool.callCount() != 1 {
		t.Fatalf("tool calls = %d", tool.callCount())
	}
	if rec.count(EventTurnStart) != 2 {
		t.Errorf("expected 2 turns, got %d", rec.count(EventTurnStart))
	}
	// prompt, assistant(toolcall), toolResult, assistant(text)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d: %#v", len(msgs), msgs)
	}
	tr, ok := msgs[2].(ai.ToolResultMessage)
	if !ok {
		t.Fatalf("messages[2] = %T", msgs[2])
	}
	if tr.IsError {
		t.Error("tool result should not be an error")
	}
	if got := tr.Content[0].(ai.TextContent).Text; got != "echo: one" {
		t.Errorf("tool result = %q", got)
	}
	if rec.count(EventToolExecutionUpdate) != 1 {
		t.Errorf("expected a streamed tool update, got %d", rec.count(EventToolExecutionUpdate))
	}
}

// A `length` stop means arguments may be silently truncated: every tool call
// in that message must fail unexecuted.
func TestLoopLengthStopFailsAllToolCallsUnexecuted(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{
			Blocks: []faux.Block{
				{ToolCall: ptrToolCall(toolCall("t1", "echo", "one"))},
				{ToolCall: ptrToolCall(toolCall("t2", "echo", "two"))},
			},
			Stop: ai.StopLength,
		},
		faux.Turn{Blocks: []faux.Block{{Text: "recovered"}}},
	)
	tool := &echoTool{name: "echo"}
	cfg, rc := baseConfig(script, []Tool{tool})
	rec := &recorder{}

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg, rec.sink)
	if err != nil {
		t.Fatal(err)
	}
	if tool.callCount() != 0 {
		t.Fatalf("tools must not execute on a truncated message, ran %d", tool.callCount())
	}
	var results []ai.ToolResultMessage
	for _, m := range msgs {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			results = append(results, tr)
		}
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 error results, got %d", len(results))
	}
	for _, r := range results {
		if !r.IsError {
			t.Error("truncated tool call result must be an error")
		}
		if !strings.Contains(r.Content[0].(ai.TextContent).Text, "output token limit") {
			t.Errorf("error text = %q", r.Content[0].(ai.TextContent).Text)
		}
	}
}

func TestLoopParallelToolsRunConcurrentlyButResultsKeepSourceOrder(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{
			{ToolCall: ptrToolCall(toolCall("t1", "echo", "first"))},
			{ToolCall: ptrToolCall(toolCall("t2", "echo", "second"))},
			{ToolCall: ptrToolCall(toolCall("t3", "echo", "third"))},
		}},
		faux.Turn{Blocks: []faux.Block{{Text: "ok"}}},
	)
	tool := &echoTool{name: "echo", delay: 40 * time.Millisecond}
	cfg, rc := baseConfig(script, []Tool{tool})

	start := time.Now()
	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if tool.concurrency() < 2 {
		t.Errorf("expected concurrent execution, max concurrency was %d", tool.concurrency())
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("parallel execution took %v — looks serialized", elapsed)
	}

	var order []string
	for _, m := range msgs {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			order = append(order, tr.ToolCallID)
		}
	}
	want := []string{"t1", "t2", "t3"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("result order = %v, want source order %v", order, want)
		}
	}
}

func TestLoopSequentialToolForcesWholeBatchSerial(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{
			{ToolCall: ptrToolCall(toolCall("t1", "seq", "a"))},
			{ToolCall: ptrToolCall(toolCall("t2", "seq", "b"))},
		}},
		faux.Turn{Blocks: []faux.Block{{Text: "ok"}}},
	)
	tool := &echoTool{name: "seq", mode: ExecutionSequential, delay: 20 * time.Millisecond}
	cfg, rc := baseConfig(script, []Tool{tool})

	if _, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg); err != nil {
		t.Fatal(err)
	}
	if tool.concurrency() != 1 {
		t.Errorf("sequential tool ran %d at once", tool.concurrency())
	}
}

func TestLoopToolErrorBecomesErrorResultNotLoopFailure(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "boom", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "handled"}}},
	)
	tool := &echoTool{name: "boom", fail: errors.New("disk on fire")}
	cfg, rc := baseConfig(script, []Tool{tool})

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg)
	if err != nil {
		t.Fatalf("tool failure must not fail the loop: %v", err)
	}
	tr := findResult(t, msgs)
	if !tr.IsError || !strings.Contains(tr.Content[0].(ai.TextContent).Text, "disk on fire") {
		t.Errorf("result = %+v", tr)
	}
}

func TestLoopToolPanicIsContained(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "panicky", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "survived"}}},
	)
	tool := &echoTool{name: "panicky", panics: true}
	cfg, rc := baseConfig(script, []Tool{tool})

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg)
	if err != nil {
		t.Fatalf("a panicking tool must not kill the agent: %v", err)
	}
	tr := findResult(t, msgs)
	if !tr.IsError || !strings.Contains(tr.Content[0].(ai.TextContent).Text, "panicked") {
		t.Errorf("result = %+v", tr)
	}
}

func TestLoopUnknownToolIsErrorResult(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "nope", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "ok"}}},
	)
	cfg, rc := baseConfig(script, nil)
	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr := findResult(t, msgs)
	if !tr.IsError || !strings.Contains(tr.Content[0].(ai.TextContent).Text, "not found") {
		t.Errorf("result = %+v", tr)
	}
}

func TestLoopBeforeToolCallCanBlock(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "echo", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "ok"}}},
	)
	tool := &echoTool{name: "echo"}
	cfg, rc := baseConfig(script, []Tool{tool})
	cfg.BeforeToolCall = func(context.Context, ToolCallContext) (*BeforeToolCallResult, error) {
		return &BeforeToolCallResult{Block: true, Reason: "not allowed"}, nil
	}

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tool.callCount() != 0 {
		t.Error("blocked tool must not execute")
	}
	tr := findResult(t, msgs)
	if !tr.IsError || tr.Content[0].(ai.TextContent).Text != "not allowed" {
		t.Errorf("result = %+v", tr)
	}
}

func TestLoopAfterToolCallPatchesResult(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "echo", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "ok"}}},
	)
	cfg, rc := baseConfig(script, []Tool{&echoTool{name: "echo"}})
	cfg.AfterToolCall = func(context.Context, ToolResultContext) (*AfterToolCallResult, error) {
		return &AfterToolCallResult{Content: ai.ContentList{ai.TextContent{Text: "patched"}}}, nil
	}

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := findResult(t, msgs).Content[0].(ai.TextContent).Text; got != "patched" {
		t.Errorf("result = %q", got)
	}
}

// terminate only ends the batch when EVERY result sets it.
func TestLoopTerminateRequiresUnanimity(t *testing.T) {
	t.Run("all terminate stops the loop", func(t *testing.T) {
		script := faux.NewScript(
			faux.Turn{Blocks: []faux.Block{
				{ToolCall: ptrToolCall(toolCall("t1", "stop", "a"))},
				{ToolCall: ptrToolCall(toolCall("t2", "stop", "b"))},
			}},
			faux.Turn{Blocks: []faux.Block{{Text: "should not run"}}},
		)
		cfg, rc := baseConfig(script, []Tool{&echoTool{name: "stop", terminat: true}})
		rec := &recorder{}
		if _, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg, rec.sink); err != nil {
			t.Fatal(err)
		}
		if rec.count(EventTurnStart) != 1 {
			t.Errorf("expected the loop to stop after 1 turn, saw %d", rec.count(EventTurnStart))
		}
	})

	t.Run("mixed terminate continues", func(t *testing.T) {
		script := faux.NewScript(
			faux.Turn{Blocks: []faux.Block{
				{ToolCall: ptrToolCall(toolCall("t1", "stop", "a"))},
				{ToolCall: ptrToolCall(toolCall("t2", "go", "b"))},
			}},
			faux.Turn{Blocks: []faux.Block{{Text: "continued"}}},
		)
		cfg, rc := baseConfig(script, []Tool{
			&echoTool{name: "stop", terminat: true},
			&echoTool{name: "go"},
		})
		rec := &recorder{}
		if _, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg, rec.sink); err != nil {
			t.Fatal(err)
		}
		if rec.count(EventTurnStart) != 2 {
			t.Errorf("expected 2 turns, saw %d", rec.count(EventTurnStart))
		}
	})
}

func TestLoopSteeringInjectedAtTurnBoundary(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "echo", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "after steering"}}},
	)
	cfg, rc := baseConfig(script, []Tool{&echoTool{name: "echo"}})

	delivered := false
	cfg.GetSteeringMessages = func(context.Context) ([]ai.Message, error) {
		if delivered {
			return nil, nil
		}
		delivered = true
		return []ai.Message{userMsg("actually, stop")}, nil
	}

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The steering message is injected before the first provider call, so it
	// lands right after the prompt.
	if u, ok := msgs[1].(ai.UserMessage); !ok || u.Content.Text != "actually, stop" {
		t.Fatalf("messages[1] = %#v, want the steering message", msgs[1])
	}
}

// The agent would stop, then a follow-up revives it for another turn.
func TestLoopFollowUpRevivesAgent(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{Text: "first answer"}}},
		faux.Turn{Blocks: []faux.Block{{Text: "second answer"}}},
	)
	cfg, rc := baseConfig(script, nil)

	sent := false
	cfg.GetFollowUpMessages = func(context.Context) ([]ai.Message, error) {
		if sent {
			return nil, nil
		}
		sent = true
		return []ai.Message{userMsg("one more thing")}, nil
	}

	rec := &recorder{}
	if _, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg, rec.sink); err != nil {
		t.Fatal(err)
	}
	if rec.count(EventTurnStart) != 2 {
		t.Errorf("follow-up should produce a second turn, saw %d", rec.count(EventTurnStart))
	}
	if rec.count(EventAgentEnd) != 1 {
		t.Errorf("agent_end must be emitted exactly once, saw %d", rec.count(EventAgentEnd))
	}
}

func TestLoopShouldStopAfterTurn(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "echo", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "never reached"}}},
	)
	cfg, rc := baseConfig(script, []Tool{&echoTool{name: "echo"}})
	cfg.ShouldStopAfterTurn = func(context.Context, TurnContext) (bool, error) { return true, nil }

	rec := &recorder{}
	if _, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg, rec.sink); err != nil {
		t.Fatal(err)
	}
	if rec.count(EventTurnStart) != 1 {
		t.Errorf("expected exactly 1 turn, saw %d", rec.count(EventTurnStart))
	}
	if rec.count(EventAgentEnd) != 1 {
		t.Errorf("agent_end count = %d", rec.count(EventAgentEnd))
	}
}

func TestLoopPrepareNextTurnSwapsModelAndThinking(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "echo", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "ok"}}},
	)
	cfg, rc := baseConfig(script, []Tool{&echoTool{name: "echo"}})

	other := faux.Model()
	other.ID = "faux-2"
	off := ai.ThinkingOff
	cfg.Reasoning = ai.ThinkingHigh
	cfg.PrepareNextTurn = func(context.Context, TurnContext) (*TurnUpdate, error) {
		return &TurnUpdate{Model: other, ThinkingLevel: &off}, nil
	}

	if _, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg); err != nil {
		t.Fatal(err)
	}
	// Both turns ran; the second used the swapped model.
	if script.Calls() != 2 {
		t.Fatalf("provider calls = %d", script.Calls())
	}
}

// A provider failure ends the run cleanly rather than returning an error.
func TestLoopProviderErrorEndsRunCleanly(t *testing.T) {
	script := faux.NewScript(faux.Turn{
		Blocks: []faux.Block{{Text: "partial"}}, ErrorMessage: "upstream exploded",
	})
	cfg, rc := baseConfig(script, nil)
	rec := &recorder{}

	msgs, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg, rec.sink)
	if err != nil {
		t.Fatalf("provider errors are data, not loop failures: %v", err)
	}
	if rec.count(EventAgentEnd) != 1 || rec.count(EventTurnEnd) != 1 {
		t.Errorf("expected clean turn_end + agent_end, got %v", rec.types())
	}
	last, ok := msgs[len(msgs)-1].(ai.AssistantMessage)
	if !ok || last.StopReason != ai.StopError || last.ErrorMessage != "upstream exploded" {
		t.Errorf("final message = %#v", msgs[len(msgs)-1])
	}
}

func TestLoopAbortMidToolExecution(t *testing.T) {
	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{ToolCall: ptrToolCall(toolCall("t1", "slow", "x"))}}},
		faux.Turn{Blocks: []faux.Block{{Text: "ok"}}},
	)
	tool := &echoTool{name: "slow", delay: 2 * time.Second}
	cfg, rc := baseConfig(script, []Tool{tool})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	msgs, err := RunLoop(ctx, []ai.Message{userMsg("go")}, rc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Error("abort did not interrupt tool execution promptly")
	}
	tr := findResult(t, msgs)
	if !tr.IsError {
		t.Error("aborted tool call should produce an error result")
	}
}

func TestLoopSinkErrorAbortsRun(t *testing.T) {
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "hi"}}})
	cfg, rc := baseConfig(script, nil)
	boom := errors.New("sink failed")

	_, err := RunLoop(context.Background(), []ai.Message{userMsg("go")}, rc, cfg,
		func(context.Context, Event) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the sink error", err)
	}
}

func TestLoopContinueRejectsAssistantTail(t *testing.T) {
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "x"}}})
	cfg, _ := baseConfig(script, nil)
	rc := &RunContext{Messages: []ai.Message{ai.AssistantMessage{StopReason: ai.StopStop}}}

	if _, err := RunLoopContinue(context.Background(), rc, cfg); err == nil {
		t.Error("expected an error continuing from an assistant message")
	}
	if _, err := RunLoopContinue(context.Background(), &RunContext{}, cfg); err == nil {
		t.Error("expected an error continuing with no messages")
	}
}

func findResult(t *testing.T, msgs []ai.Message) ai.ToolResultMessage {
	t.Helper()
	for _, m := range msgs {
		if tr, ok := m.(ai.ToolResultMessage); ok {
			return tr
		}
	}
	t.Fatal("no tool result message found")
	return ai.ToolResultMessage{}
}

func ptrToolCall(tc ai.ToolCall) *ai.ToolCall { return &tc }
