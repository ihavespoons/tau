package openairesp

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
	m.Compat = &ai.CompatFlags{SupportsOpenAIGrammarTools: boolptr(true)}
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
			Variants: map[ai.GrammarFormat]string{ai.GrammarOpenAIRegex: "^SELECT .*$"},
		},
	}
}

// THE POINT: the responses wire spells a custom tool differently from
// chat-completions — the format is flat on the tool, not nested under a
// `custom` object. Sending the chat shape here is rejected.
func TestAGrammarToolIsDeclaredWithAFlatFormat(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}

	tools := payloadFor(t, grammarModel(), c, nil)["tools"].([]any)
	declared := tools[0].(map[string]any)

	if declared["type"] != "custom" || declared["name"] != "sql" {
		t.Fatalf("tool %+v", declared)
	}
	format := declared["format"].(map[string]any)
	if format["type"] != "grammar" || format["syntax"] != "regex" || format["definition"] != "^SELECT .*$" {
		t.Errorf("format %+v", format)
	}
	// The grammar IS the schema.
	if _, present := declared["parameters"]; present {
		t.Error("a custom tool must not also carry a JSON schema")
	}
}

func TestAJSONSchemaToolAsksForStrictSampling(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{
		{
			Name:       "extract",
			Parameters: &jsonschema.Schema{Type: "object"},
			ConstrainedSampling: &ai.ConstrainedSampling{
				Type: "json_schema", Strict: "prefer",
			},
		},
		{Name: "read", Parameters: &jsonschema.Schema{Type: "object"}},
	}

	model := grammarModel()
	model.Compat.SupportsStrictMode = boolptr(true)

	tools := payloadFor(t, model, c, nil)["tools"].([]any)
	if got := tools[0].(map[string]any)["strict"]; got != true {
		t.Errorf("strict %v for a tool that asked for schema enforcement", got)
	}
	if got := tools[1].(map[string]any)["strict"]; got != false {
		t.Errorf("strict %v for an unconstrained tool", got)
	}
}

// THE POINT: a call and its result are a pair. A function_call_output answering
// a custom_tool_call leaves the call unanswered as far as the host is
// concerned, and the next turn is rejected for it.
func TestAGrammarCallAndItsResultUseTheCustomItems(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}
	c.Messages = append(c.Messages,
		ai.AssistantMessage{
			Content: ai.ContentList{ai.ToolCall{
				ID: "call-1|ctc_1", Name: "sql", Arguments: map[string]any{"query": "SELECT 1"},
			}},
			Api: ai.ApiOpenAIResponses, Provider: "openai", Model: "gpt-5.4",
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{
			ToolCallID: "call-1|ctc_1", ToolName: "sql",
			Content: ai.ContentList{ai.TextContent{Text: "1"}},
		},
	)

	input := payloadFor(t, grammarModel(), c, nil)["input"].([]any)

	var call, output map[string]any
	for _, it := range input {
		item := it.(map[string]any)
		switch item["type"] {
		case "custom_tool_call":
			call = item
		case "custom_tool_call_output":
			output = item
		case "function_call", "function_call_output":
			t.Errorf("a grammar call was replayed as a function item: %+v", item)
		}
	}

	if call == nil {
		t.Fatalf("no custom_tool_call in %+v", input)
	}
	if call["name"] != "sql" || call["input"] != "SELECT 1" || call["call_id"] != "call-1" {
		t.Errorf("call %+v", call)
	}
	// The item id round-trips only when it is a custom-tool id.
	if call["id"] != "ctc_1" {
		t.Errorf("item id %v", call["id"])
	}
	if output == nil {
		t.Fatalf("no custom_tool_call_output in %+v", input)
	}
	if output["call_id"] != "call-1" {
		t.Errorf("output %+v", output)
	}
}

// An item id from another shape (fc_, or a wire that names them differently)
// must not be replayed on a custom item: the host validates the prefix.
func TestAForeignItemIDIsDroppedFromACustomCall(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}
	c.Messages = append(c.Messages, ai.AssistantMessage{
		Content: ai.ContentList{ai.ToolCall{
			ID: "call-1|fc_1", Name: "sql", Arguments: map[string]any{"query": "SELECT 1"},
		}},
		Api: ai.ApiOpenAIResponses, Provider: "openai", Model: "gpt-5.4",
		StopReason: ai.StopToolUse,
	})

	for _, it := range payloadFor(t, grammarModel(), c, nil)["input"].([]any) {
		item := it.(map[string]any)
		if item["type"] != "custom_tool_call" {
			continue
		}
		if _, present := item["id"]; present {
			t.Errorf("an fc_ id was replayed on a custom call: %+v", item)
		}
	}
}

func TestAGrammarCallWithoutItsArgumentFailsTheTurn(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}
	c.Messages = append(c.Messages, ai.AssistantMessage{
		Content: ai.ContentList{ai.ToolCall{
			ID: "call-1", Name: "sql", Arguments: map[string]any{"wrong": "SELECT 1"},
		}},
		Api: ai.ApiOpenAIResponses, Provider: "openai", Model: "gpt-5.4",
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

// grammarSSE serves a recorded stream to a grammar-capable model.
func grammarSSE(t *testing.T, body string) *ai.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	m := grammarModel()
	m.BaseURL = srv.URL
	return m
}

// THE POINT: the grammar output streams as raw text on its own event type, and
// is re-wrapped as JSON-argument deltas so consumers see one shape of tool call
// rather than two.
func TestACustomToolCallStreamsBackAsJSONArguments(t *testing.T) {
	model := grammarSSE(t, strings.Join([]string{
		chunk(`{"type":"response.created","response":{"id":"resp_1"}}`),
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"sql","input":""}}`),
		chunk(`{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"SELECT "}`),
		chunk(`{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"1"}`),
		chunk(`{"type":"response.custom_tool_call_input.done","output_index":0,"input":"SELECT 1"}`),
		chunk(`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"sql","input":"SELECT 1"}}`),
		chunk(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`),
	}, ""))

	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}

	stream := Stream(context.Background(), model, c, &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})

	var deltas []string
	var sawEnd bool
	for ev := range stream.Events() {
		switch ev.Type {
		case ai.EventToolCallDelta:
			deltas = append(deltas, ev.Delta)
		case ai.EventToolCallEnd:
			sawEnd = true
		}
	}
	final := stream.Result()
	if final.StopReason != ai.StopToolUse {
		t.Fatalf("final %+v", final)
	}
	if !sawEnd {
		t.Error("the call never ended")
	}

	call := final.Content[0].(ai.ToolCall)
	if call.Name != "sql" || call.ID != "call_1|ctc_1" {
		t.Fatalf("call %+v", call)
	}
	if call.Arguments["query"] != "SELECT 1" {
		t.Errorf("arguments %+v", call.Arguments)
	}

	assembled := strings.Join(deltas, "")
	if assembled != `{"query":"SELECT 1"}` {
		t.Errorf("assembled deltas %q", assembled)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(assembled), &parsed); err != nil {
		t.Errorf("the assembled deltas do not parse: %v", err)
	}
}

// The done event carries the complete input, which is authoritative: a stream
// that dropped a delta still ends with the whole value.
func TestTheDoneEventSuppliesWhatTheDeltasMissed(t *testing.T) {
	model := grammarSSE(t, strings.Join([]string{
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"sql","input":""}}`),
		chunk(`{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"SEL"}`),
		chunk(`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"sql","input":"SELECT 1"}}`),
		chunk(`{"type":"response.completed","response":{"id":"r","status":"completed"}}`),
	}, ""))

	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}

	stream := Stream(context.Background(), model, c, &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}

	call := stream.Result().Content[0].(ai.ToolCall)
	if call.Arguments["query"] != "SELECT 1" {
		t.Errorf("arguments %+v", call.Arguments)
	}
}

// THE POINT: the done event REPLACES the accumulated value. One that
// contradicts what was streamed means the arguments would be silently wrong,
// which is worse than a failed turn.
func TestAnInputThatContradictsTheDeltasFailsTheTurn(t *testing.T) {
	model := grammarSSE(t, strings.Join([]string{
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"sql","input":""}}`),
		chunk(`{"type":"response.custom_tool_call_input.delta","output_index":0,"delta":"SELECT 1"}`),
		chunk(`{"type":"response.custom_tool_call_input.done","output_index":0,"input":"DROP TABLE users"}`),
		chunk(`{"type":"response.completed","response":{"id":"r","status":"completed"}}`),
	}, ""))

	c := simpleContext()
	c.Tools = []ai.Tool{sqlTool()}

	stream := Stream(context.Background(), model, c, &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}
	final := stream.Result()
	if final.StopReason != ai.StopError {
		t.Fatalf("stop reason %q", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "non-monotonically") {
		t.Errorf("error %q", final.ErrorMessage)
	}
}

// A model can invent a custom call for a tool tau never declared; its output
// still needs somewhere to live.
func TestAnUndeclaredCustomToolCallFallsBackToInput(t *testing.T) {
	model := grammarSSE(t, strings.Join([]string{
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"invented","input":""}}`),
		chunk(`{"type":"response.custom_tool_call_input.done","output_index":0,"input":"hello"}`),
		chunk(`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"invented","input":"hello"}}`),
		chunk(`{"type":"response.completed","response":{"id":"r","status":"completed"}}`),
	}, ""))

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
