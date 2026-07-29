package googlegenai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func serveSSE(t *testing.T, body string) (*ai.Model, *http.Request, *string) {
	t.Helper()
	var captured string
	var req *http.Request

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req = r.Clone(context.Background())
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		captured = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	model := reasoningModel("gemini-3-pro-preview", srv.URL)
	// The request pointer is filled in by the handler, so it is returned by
	// address rather than value.
	return model, req, &captured
}

func chunkOf(data string) string { return "data: " + data + "\n\n" }

func collect(stream *ai.MessageStream) ([]ai.EventType, *ai.AssistantMessage) {
	var types []ai.EventType
	for ev := range stream.Events() {
		types = append(types, ev.Type)
	}
	return types, stream.Result()
}

func hasType(types []ai.EventType, want ai.EventType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

func runStream(t *testing.T, body string) ([]ai.EventType, *ai.AssistantMessage) {
	t.Helper()
	model, _, _ := serveSSE(t, body)
	return collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))
}

const textStream = `data: {"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}

`

func TestStreamsTextAndSettles(t *testing.T) {
	types, msg := runStream(t, textStream)

	for _, want := range []ai.EventType{
		ai.EventStart, ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd, ai.EventDone,
	} {
		if !hasType(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	if msg.StopReason != ai.StopStop {
		t.Fatalf("stop reason: %q (%s)", msg.StopReason, msg.ErrorMessage)
	}
	if text := msg.Content[0].(ai.TextContent).Text; text != "Hello world" {
		t.Errorf("text: %q", text)
	}
	if msg.ResponseID != "r1" {
		t.Errorf("response id: %q", msg.ResponseID)
	}
}

// THE POINT: Gemini does not delimit blocks. A stream is a sequence of parts,
// and a block ends when a part of a different kind arrives — so the state
// machine has to notice the transition rather than being told about it.
func TestThinkingAndTextSplitIntoSeparateBlocks(t *testing.T) {
	body := chunkOf(`{"candidates":[{"content":{"parts":[{"text":"weighing","thought":true}]}}]}`) +
		chunkOf(`{"candidates":[{"content":{"parts":[{"text":" options","thought":true}]}}]}`) +
		chunkOf(`{"candidates":[{"content":{"parts":[{"text":"The answer"}]}}]}`) +
		chunkOf(`{"candidates":[{"content":{"parts":[{"text":" is 4."}]},"finishReason":"STOP"}]}`)

	types, msg := runStream(t, body)

	if len(msg.Content) != 2 {
		t.Fatalf("expected a thinking block and a text block, got %#v", msg.Content)
	}
	thinking, ok := msg.Content[0].(ai.ThinkingContent)
	if !ok || thinking.Thinking != "weighing options" {
		t.Errorf("thinking block: %#v", msg.Content[0])
	}
	text, ok := msg.Content[1].(ai.TextContent)
	if !ok || text.Text != "The answer is 4." {
		t.Errorf("text block: %#v", msg.Content[1])
	}
	// The thinking block has to be closed before the text one opens, or a
	// consumer sees two open blocks at once.
	if !hasType(types, ai.EventThinkingEnd) || !hasType(types, ai.EventTextStart) {
		t.Errorf("events: %v", types)
	}
}

// A signature may arrive on the first delta only; a later part without one
// must not erase it.
func TestThoughtSignatureSurvivesLaterDeltas(t *testing.T) {
	body := chunkOf(`{"candidates":[{"content":{"parts":[{"text":"a","thought":true,"thoughtSignature":"c2ln"}]}}]}`) +
		chunkOf(`{"candidates":[{"content":{"parts":[{"text":"b","thought":true}]}}]}`) +
		chunkOf(`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`)

	_, msg := runStream(t, body)

	thinking := msg.Content[0].(ai.ThinkingContent)
	if thinking.ThinkingSignature != "c2ln" {
		t.Errorf("the signature was lost: %q", thinking.ThinkingSignature)
	}
	if thinking.Thinking != "ab" {
		t.Errorf("thinking: %q", thinking.Thinking)
	}
}

// Gemini delivers a call whole rather than as argument fragments, so all three
// events fire together — a consumer that renders deltas still sees arguments.
func TestToolCallEmitsAllThreeEvents(t *testing.T) {
	body := chunkOf(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{"path":"a.go"}}}]}}]}`) +
		chunkOf(`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`)

	types, msg := runStream(t, body)

	for _, want := range []ai.EventType{ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd} {
		if !hasType(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	call, ok := msg.Content[0].(ai.ToolCall)
	if !ok {
		t.Fatalf("content: %#v", msg.Content)
	}
	if call.Arguments["path"] != "a.go" {
		t.Errorf("arguments: %#v", call.Arguments)
	}
	// A turn ending in a tool call must say so, or the loop stops instead of
	// executing it.
	if msg.StopReason != ai.StopToolUse {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
}

// THE POINT: two tool calls sharing an id would pair both results to the same
// call. Gemini omits ids for its own models and can repeat them for others.
func TestToolCallIDsAreMadeUnique(t *testing.T) {
	body := chunkOf(`{"candidates":[{"content":{"parts":[`+
		`{"functionCall":{"name":"read","args":{}}},`+
		`{"functionCall":{"name":"read","args":{}}}`+
		`]}}]}`) +
		chunkOf(`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`)

	_, msg := runStream(t, body)

	if len(msg.Content) != 2 {
		t.Fatalf("content: %#v", msg.Content)
	}
	first := msg.Content[0].(ai.ToolCall)
	second := msg.Content[1].(ai.ToolCall)
	if first.ID == "" || second.ID == "" {
		t.Fatal("a tool call with no id cannot be paired with its result")
	}
	if first.ID == second.ID {
		t.Errorf("both calls got the id %q", first.ID)
	}
}

// A duplicate id from the provider gets replaced for the same reason.
func TestDuplicateProviderIDsAreReplaced(t *testing.T) {
	body := chunkOf(`{"candidates":[{"content":{"parts":[`+
		`{"functionCall":{"id":"same","name":"read","args":{}}},`+
		`{"functionCall":{"id":"same","name":"ls","args":{}}}`+
		`]}}]}`) +
		chunkOf(`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}]}`)

	_, msg := runStream(t, body)

	if msg.Content[0].(ai.ToolCall).ID == msg.Content[1].(ai.ToolCall).ID {
		t.Error("a repeated provider id was replayed as-is")
	}
}

// Thinking tokens are reported separately and billed as output, so they are
// summed; cached tokens are inside the prompt count and come out of it.
func TestUsageAccounting(t *testing.T) {
	body := chunkOf(`{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":50,` +
		`"thoughtsTokenCount":200,"cachedContentTokenCount":800,"totalTokenCount":1250}}`)

	_, msg := runStream(t, body)

	if msg.Usage.Input != 200 {
		t.Errorf("input: %d, want 200 (1000 - 800 cached)", msg.Usage.Input)
	}
	if msg.Usage.Output != 250 {
		t.Errorf("output: %d, want 250 (50 candidates + 200 thoughts)", msg.Usage.Output)
	}
	if msg.Usage.CacheRead != 800 {
		t.Errorf("cacheRead: %d", msg.Usage.CacheRead)
	}
	if msg.Usage.Reasoning == nil || *msg.Usage.Reasoning != 200 {
		t.Errorf("reasoning: %v", msg.Usage.Reasoning)
	}
}

// Everything that is not a clean stop or a length cap is a condition the user
// needs to see — a safety block that silently ended the turn reads as the
// model simply choosing to stop.
func TestFinishReasonMapping(t *testing.T) {
	cases := []struct {
		reason string
		want   ai.StopReason
	}{
		{"STOP", ai.StopStop},
		{"MAX_TOKENS", ai.StopLength},
		{"SAFETY", ai.StopError},
		{"RECITATION", ai.StopError},
		{"MALFORMED_FUNCTION_CALL", ai.StopError},
	}

	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			if got := mapStopReason(tc.reason); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A stream that ends with no finish reason is a truncated response, not a
// successful empty one.
func TestNoFinishReasonIsAnError(t *testing.T) {
	_, msg := runStream(t, chunkOf(`{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "finish reason") {
		t.Errorf("error: %q", msg.ErrorMessage)
	}
}

// Google authenticates with its own header; a bearer token is ignored.
func TestAuthUsesTheGoogleHeader(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(textStream))
	}))
	defer srv.Close()

	model := modelFor("gemini-3-pro-preview", srv.URL)
	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "google-key"}}))

	if msg.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", msg.ErrorMessage)
	}
	if seen.Get("x-goog-api-key") != "google-key" {
		t.Errorf("x-goog-api-key: %q", seen.Get("x-goog-api-key"))
	}
	if seen.Get("Authorization") != "" {
		t.Errorf("a bearer token was sent to google: %q", seen.Get("Authorization"))
	}
}

// alt=sse is what makes the endpoint stream events rather than deliver a JSON
// array in chunks — without it the whole parse is wrong.
func TestRequestURLAsksForSSE(t *testing.T) {
	var path, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(textStream))
	}))
	defer srv.Close()

	model := modelFor("gemini-3-pro-preview", srv.URL)
	collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if !strings.HasSuffix(path, "/models/gemini-3-pro-preview:streamGenerateContent") {
		t.Errorf("path: %q", path)
	}
	if !strings.Contains(query, "alt=sse") {
		t.Errorf("query: %q", query)
	}
}

// A non-2xx must read legibly. Google nests the useful part two levels deep.
func TestProviderErrorIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`))
	}))
	defer srv.Close()

	_, msg := collect(Stream(context.Background(), modelFor("gemini-3-pro-preview", srv.URL),
		simpleContext(), &Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	for _, want := range []string{"Quota exceeded", "RESOURCE_EXHAUSTED"} {
		if !strings.Contains(msg.ErrorMessage, want) {
			t.Errorf("error should mention %q: %q", want, msg.ErrorMessage)
		}
	}
}

func TestMissingCredentialsFailCleanly(t *testing.T) {
	_, msg := collect(Stream(context.Background(),
		modelFor("gemini-3-pro-preview", "https://example.invalid"), simpleContext(), &Options{}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "API key") {
		t.Errorf("error: %q", msg.ErrorMessage)
	}
}

func TestAbortIsReportedAsAborted(t *testing.T) {
	model, _, _ := serveSSE(t, textStream)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, msg := collect(Stream(ctx, model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))
	if msg.StopReason != ai.StopAborted {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
}
