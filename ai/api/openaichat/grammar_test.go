package openaichat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
)

// grammarModel supports custom tools. The flag is a catalog fact rather than
// something detected from the base URL, so it is set explicitly — and the URL
// is deliberately unroutable, because a test that gets this wrong must fail
// locally rather than send a request to a real provider.
func grammarModel() *ai.Model {
	m := modelFor("openai", "http://127.0.0.1:9/v1")
	yes := true
	m.Compat = &ai.CompatFlags{SupportsOpenAIGrammarTools: &yes}
	return m
}

// noConstraintsModel supports neither grammar tools nor strict mode.
func noConstraintsModel() *ai.Model {
	m := modelFor("openai", "http://127.0.0.1:9/v1")
	no := false
	m.Compat = &ai.CompatFlags{SupportsOpenAIGrammarTools: &no, SupportsStrictMode: &no}
	return m
}

func sqlTool() ai.Tool {
	return ai.Tool{
		Name:        "sql",
		Description: "run a query",
		Parameters: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"query": {Type: "string"}},
			Required:   []string{"query"},
		},
		ConstrainedSampling: &ai.ConstrainedSampling{
			Type:     "grammar",
			Variants: map[ai.GrammarFormat]string{ai.GrammarOpenAILark: "start: /SELECT .*/"},
		},
	}
}

// THE POINT: a grammar tool is a different KIND of tool on the wire, not a
// function with extra fields. Declared as a function it would be sampled
// against a JSON schema and the grammar would do nothing at all — which is
// exactly the bug tau shipped while the catalog flag said the feature existed.
func TestAGrammarToolIsDeclaredAsACustomTool(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}

	p := payloadFor(t, grammarModel(), c, nil)
	tools := p["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools %+v", tools)
	}

	declared := tools[0].(map[string]any)
	if declared["type"] != "custom" {
		t.Fatalf("tool type %v — a grammar tool is not a function", declared["type"])
	}
	custom := declared["custom"].(map[string]any)
	if custom["name"] != "sql" || custom["description"] != "run a query" {
		t.Errorf("custom %+v", custom)
	}
	format := custom["format"].(map[string]any)
	if format["type"] != "grammar" {
		t.Errorf("format %+v", format)
	}
	grammar := format["grammar"].(map[string]any)
	if grammar["syntax"] != "lark" || grammar["definition"] != "start: /SELECT .*/" {
		t.Errorf("grammar %+v", grammar)
	}
	// The grammar IS the schema: sending parameters as well would describe the
	// same argument twice, in two languages.
	if _, present := declared["function"]; present {
		t.Error("a custom tool must not also carry a function declaration")
	}
}

// On a provider without custom-tool support the same tool still works, as an
// ordinary function. Refusing would make the tool unusable everywhere but
// OpenAI.
func TestAGrammarToolDegradesToAFunctionElsewhere(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}

	p := payloadFor(t, noConstraintsModel(), c, nil)
	declared := p["tools"].([]any)[0].(map[string]any)
	if declared["type"] != "function" {
		t.Errorf("tool type %v", declared["type"])
	}
	if declared["function"].(map[string]any)["parameters"] == nil {
		t.Error("the JSON schema must be sent when the grammar cannot be")
	}
}

// THE POINT: strict is what makes json_schema constrained sampling actually
// constrain anything. tau sent `false` unconditionally, so a tool asking for it
// got nothing.
func TestAJSONSchemaToolAsksForStrictSampling(t *testing.T) {
	constrained := ai.Tool{
		Name:       "extract",
		Parameters: &jsonschema.Schema{Type: "object"},
		ConstrainedSampling: &ai.ConstrainedSampling{
			Type: "json_schema", Strict: "prefer",
		},
	}
	plain := ai.Tool{Name: "read", Parameters: &jsonschema.Schema{Type: "object"}}

	c := simpleContext()
	c.Tools = []ai.Tool{constrained, plain}

	tools := payloadFor(t, grammarModel(), c, nil)["tools"].([]any)
	if got := tools[0].(map[string]any)["function"].(map[string]any)["strict"]; got != true {
		t.Errorf("strict %v for a tool that asked for schema enforcement", got)
	}
	// And a tool that asked for nothing still gets the field, false — several
	// providers reject a request whose tools disagree about its presence.
	if got := tools[1].(map[string]any)["function"].(map[string]any)["strict"]; got != false {
		t.Errorf("strict %v for an unconstrained tool", got)
	}
}

// A tool that REQUIRES constrained sampling must not be sent to a provider that
// will not enforce it: the tool has been promised arguments it will now never
// be guaranteed.
func TestARequiredConstraintFailsTheTurnRatherThanDegrading(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{{
		Name:       "extract",
		Parameters: &jsonschema.Schema{Type: "object"},
		ConstrainedSampling: &ai.ConstrainedSampling{
			Type: "json_schema", Strict: "require",
		},
	}}

	stream := Stream(context.Background(), noConstraintsModel(), c, &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}
	final := stream.Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("stop reason %q", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "requires JSON-schema constrained sampling") {
		t.Errorf("error %q", final.ErrorMessage)
	}
}

// THE POINT: the call is replayed in the shape the tool was declared in. A
// function_call for a tool declared as custom contradicts the same request.
func TestAGrammarCallIsReplayedAsACustomToolCall(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}
	c.Messages = append(c.Messages,
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{
				ID: "call-1", Name: "sql", Arguments: map[string]any{"query": "SELECT 1"},
			}},
			Api: ai.ApiOpenAICompletions, Provider: "openai", Model: "test-model",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{
			ToolCallID: "call-1", ToolName: "sql",
			Content: ai.ContentList{ai.TextContent{Text: "1"}},
		},
	)

	p := payloadFor(t, grammarModel(), c, nil)
	var assistant map[string]any
	for _, m := range p["messages"].([]any) {
		msg := m.(map[string]any)
		if msg["role"] == "assistant" {
			assistant = msg
		}
	}
	if assistant == nil {
		t.Fatalf("no assistant message in %+v", p["messages"])
	}

	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	if call["type"] != "custom" {
		t.Fatalf("tool call type %v", call["type"])
	}
	custom := call["custom"].(map[string]any)
	if custom["name"] != "sql" || custom["input"] != "SELECT 1" {
		t.Errorf("custom %+v — the raw text goes back, not JSON", custom)
	}
	if _, present := call["function"]; present {
		t.Error("a custom call must not also carry function arguments")
	}
}

// A replayed call whose argument is missing cannot be sent at all: failing
// beats posting "<nil>" as a SQL statement.
func TestAGrammarCallWithoutItsArgumentFailsTheTurn(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}
	c.Messages = append(c.Messages, ai.AssistantMessage{
		Content: ai.ContentList{ai.ToolCall{
			ID: "call-1", Name: "sql", Arguments: map[string]any{"wrong": "SELECT 1"},
		}},
		Api: ai.ApiOpenAICompletions, Provider: "openai", Model: "test-model",
		StopReason: ai.StopToolUse,
	})

	stream := Stream(context.Background(), grammarModel(), c, &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}
	final := stream.Result()
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, `requires argument "query"`) {
		t.Errorf("final %+v", final)
	}
}

// grammarStream replies with the given SSE chunks.
func grammarStream(t *testing.T, chunks ...string) *ai.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	m := grammarModel()
	m.BaseURL = srv.URL
	return m
}

// THE POINT: a grammar tool's output streams as raw text, but everything
// downstream — the transcript renderer, JSON mode, extensions — was written
// against arguments that arrive as JSON. The wire re-wraps it so there is one
// shape of tool call, not two.
func TestACustomToolCallStreamsBackAsJSONArguments(t *testing.T) {
	model := grammarStream(t,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"custom","custom":{"name":"sql","input":"SELECT "}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"custom":{"input":"1"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}

	stream := Stream(context.Background(), model, c, &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})

	var deltas []string
	for ev := range stream.Events() {
		if ev.Type == ai.EventToolCallDelta {
			deltas = append(deltas, ev.Delta)
		}
	}
	final := stream.Result()
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("final %+v", final)
	}

	call, ok := final.Content[0].(ai.ToolCall)
	if !ok || call.Name != "sql" || call.ID != "call-1" {
		t.Fatalf("content %+v", final.Content)
	}
	if call.Arguments["query"] != "SELECT 1" {
		t.Errorf("arguments %+v", call.Arguments)
	}

	// The deltas concatenate into the arguments a consumer would parse.
	assembled := strings.Join(deltas, "")
	if assembled != `{"query":"SELECT 1"}` {
		t.Errorf("assembled deltas %q", assembled)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(assembled), &parsed); err != nil {
		t.Errorf("the assembled deltas do not parse: %v", err)
	}
}

// A model can invent a custom call for a tool tau never declared. Its output
// still needs somewhere to live rather than being dropped on the floor.
func TestAnUndeclaredCustomToolCallFallsBackToInput(t *testing.T) {
	model := grammarStream(t,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"custom","custom":{"name":"invented","input":"hello"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	stream := Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}

	call := stream.Result().Content[0].(ai.ToolCall)
	if call.Name != "invented" || call.Arguments["input"] != "hello" {
		t.Errorf("call %+v", call)
	}
}

// THE POINT: a provider may open a tool call with nothing but an id and reveal
// the custom shape a chunk later. Attaching the buffer only when the block is
// created would leave that call accumulating into the wrong field.
func TestACallCanBecomeCustomAfterItHasOpened(t *testing.T) {
	model := grammarStream(t,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1"}]}}]}`,
		`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"type":"custom","custom":{"name":"sql","input":"SELECT 1"}}]}}]}`,
		`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}

	stream := Stream(context.Background(), model, c, &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}
	final := stream.Result()
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("final %+v", final)
	}

	call := final.Content[0].(ai.ToolCall)
	if call.Name != "sql" || call.ID != "call-1" {
		t.Fatalf("call %+v", call)
	}
	// The declared property, not the fallback: the name arrived late but it
	// still names a tool tau declared.
	if call.Arguments["query"] != "SELECT 1" {
		t.Errorf("arguments %+v", call.Arguments)
	}
}
