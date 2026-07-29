package openaichat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// serveSSE replays a recorded body, and reports what the request looked like.
func serveSSE(t *testing.T, body string) (*ai.Model, *string) {
	t.Helper()
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		captured = string(buf)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	m := modelFor("groq", srv.URL)
	return m, &captured
}

func chunk(data string) string { return "data: " + data + "\n\n" }

// collect drains a stream into its event types and the final message.
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

const textBody = `data: {"id":"c1","model":"test-model","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`

func TestStreamsTextAndSettles(t *testing.T) {
	model, _ := serveSSE(t, textBody)
	types, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	for _, want := range []ai.EventType{ai.EventStart, ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd, ai.EventDone} {
		if !hasType(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	if msg.StopReason != ai.StopStop {
		t.Errorf("stop reason: %q (%s)", msg.StopReason, msg.ErrorMessage)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected one text block, got %d", len(msg.Content))
	}
	if text := msg.Content[0].(ai.TextContent).Text; text != "Hello world" {
		t.Errorf("text: %q", text)
	}
	if msg.ResponseID != "c1" {
		t.Errorf("response id: %q", msg.ResponseID)
	}
}

func TestUsageAndCost(t *testing.T) {
	body := chunk(`{"id":"c1","choices":[{"delta":{"content":"hi"}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":7}}}`)

	model, _ := serveSSE(t, body)
	model.Cost = ai.ModelCost{ModelCostRates: ai.ModelCostRates{Input: 1, Output: 2, CacheRead: 0.5}}

	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	// cached_tokens is a cache READ and must not be subtracted twice.
	if msg.Usage.Input != 60 {
		t.Errorf("input should exclude the cache read: %d", msg.Usage.Input)
	}
	if msg.Usage.CacheRead != 40 {
		t.Errorf("cache read: %d", msg.Usage.CacheRead)
	}
	if msg.Usage.Reasoning == nil || *msg.Usage.Reasoning != 7 {
		t.Errorf("reasoning tokens: %v", msg.Usage.Reasoning)
	}
	if msg.Usage.TotalTokens != 120 {
		t.Errorf("total: %d", msg.Usage.TotalTokens)
	}
	if msg.Usage.Cost.Total <= 0 {
		t.Errorf("cost was not calculated: %+v", msg.Usage.Cost)
	}
}

// Moonshot reports usage on the choice rather than the chunk; both have to work.
func TestUsageOnTheChoice(t *testing.T) {
	body := chunk(`{"id":"c1","choices":[{"delta":{"content":"hi"},"finish_reason":"stop","usage":{"prompt_tokens":9,"completion_tokens":3}}]}`)
	model, _ := serveSSE(t, body)

	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))
	if msg.Usage.Input != 9 || msg.Usage.Output != 3 {
		t.Errorf("usage from the choice was not read: %+v", msg.Usage)
	}
}

func TestStreamsToolCalls(t *testing.T) {
	body := chunk(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":""}}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)

	model, _ := serveSSE(t, body)
	types, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	for _, want := range []ai.EventType{ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd} {
		if !hasType(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	if msg.StopReason != ai.StopToolUse {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
	tc, ok := msg.Content[0].(ai.ToolCall)
	if !ok {
		t.Fatalf("expected a tool call, got %T", msg.Content[0])
	}
	if tc.ID != "call_1" || tc.Name != "read" {
		t.Errorf("tool call identity: %+v", tc)
	}
	if tc.Arguments["path"] != "a.go" {
		t.Errorf("arguments were not assembled: %+v", tc.Arguments)
	}
}

// Two calls in one turn must stay separate even though their deltas interleave.
func TestParallelToolCallsStayDistinct(t *testing.T) {
	body := chunk(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{\"p\":1}"}}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"write","arguments":"{\"p\":2}"}}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)

	model, _ := serveSSE(t, body)
	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if len(msg.Content) != 2 {
		t.Fatalf("expected two tool calls, got %d: %+v", len(msg.Content), msg.Content)
	}
	first := msg.Content[0].(ai.ToolCall)
	second := msg.Content[1].(ai.ToolCall)
	if first.Name != "read" || second.Name != "write" {
		t.Errorf("calls were merged or reordered: %q %q", first.Name, second.Name)
	}
}

// Reasoning has three field names in the wild; a provider that sends the same
// text in two of them must not have it counted twice.
func TestReasoningFieldsAreNotDoubled(t *testing.T) {
	body := chunk(`{"id":"c1","choices":[{"delta":{"reasoning_content":"thinking hard","reasoning":"thinking hard"}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`)

	model, _ := serveSSE(t, body)
	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	var thinking string
	count := 0
	for _, block := range msg.Content {
		if t, ok := block.(ai.ThinkingContent); ok {
			thinking = t.Thinking
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one thinking block, got %d", count)
	}
	if thinking != "thinking hard" {
		t.Errorf("thinking was duplicated or lost: %q", thinking)
	}
}

func TestReasoningUnderEachFieldName(t *testing.T) {
	for _, field := range []string{"reasoning_content", "reasoning", "reasoning_text"} {
		t.Run(field, func(t *testing.T) {
			body := chunk(`{"id":"c1","choices":[{"delta":{"`+field+`":"pondering"}}]}`) +
				chunk(`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`)
			model, _ := serveSSE(t, body)
			_, msg := collect(Stream(context.Background(), model, simpleContext(),
				&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

			if len(msg.Content) == 0 {
				t.Fatal("no content")
			}
			th, ok := msg.Content[0].(ai.ThinkingContent)
			if !ok || th.Thinking != "pondering" {
				t.Errorf("reasoning under %q was not read: %+v", field, msg.Content[0])
			}
			if th.ThinkingSignature != field {
				t.Errorf("signature should record which field it came from: %q", th.ThinkingSignature)
			}
		})
	}
}

// A stream that stops without a finish_reason is a truncated turn, not a
// successful one. Treating it as success would silently lose the tail.
func TestMissingFinishReasonIsAnError(t *testing.T) {
	model, _ := serveSSE(t, chunk(`{"id":"c1","choices":[{"delta":{"content":"partial"}}]}`))
	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if msg.StopReason != ai.StopError {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "finish_reason") {
		t.Errorf("error should name the cause: %q", msg.ErrorMessage)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]ai.StopReason{
		"stop":           ai.StopStop,
		"end":            ai.StopStop,
		"length":         ai.StopLength,
		"tool_calls":     ai.StopToolUse,
		"function_call":  ai.StopToolUse,
		"content_filter": ai.StopError,
	}
	for reason, want := range cases {
		got, _ := mapStopReason(reason)
		if got != want {
			t.Errorf("%q mapped to %q, want %q", reason, got, want)
		}
	}
	if _, msg := mapStopReason("something_new"); msg == "" {
		t.Error("an unknown finish_reason should carry an explanatory message")
	}
}

// An HTTP error must surface the provider's own message, not just a status.
func TestProviderErrorIsReadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit"}}`))
	}))
	defer srv.Close()

	_, msg := collect(Stream(context.Background(), modelFor("groq", srv.URL), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if msg.StopReason != ai.StopError {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "rate limit exceeded") {
		t.Errorf("the provider's message was lost: %q", msg.ErrorMessage)
	}
}

// OpenRouter hides the upstream's real error under metadata.raw.
//
// The body below is a real 400 from openrouter.ai, trimmed. Its top-level
// message is "Provider returned error" and nothing else — everything that
// would let anyone fix the request is inside metadata.raw, which is why the
// field is read at all.
func TestOpenRouterMetadataIsSurfaced(t *testing.T) {
	const body = `{"error":{"message":"Provider returned error","code":400,"metadata":{` +
		`"raw":"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",` +
		`\"message\":\"tools.0.custom.name: String should match pattern '^[a-zA-Z0-9_-]{1,128}$'\"}}",` +
		`"provider_name":"Azure","is_byok":false}}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	_, msg := collect(Stream(context.Background(), modelFor("openrouter", srv.URL), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if !strings.Contains(msg.ErrorMessage, "Provider returned error") {
		t.Errorf("the outer message was dropped: %q", msg.ErrorMessage)
	}
	if !strings.Contains(msg.ErrorMessage, "tools.0.custom.name") {
		t.Errorf("the only actionable detail was dropped: %q", msg.ErrorMessage)
	}
}

// Missing credentials must fail as a terminal event, never as a panic or a
// silently empty turn.
func TestMissingCredentialsFailCleanly(t *testing.T) {
	_, msg := collect(Stream(context.Background(), modelFor("groq", "https://example.invalid"), simpleContext(), &Options{}))
	if msg.StopReason != ai.StopError {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "No API key") && !strings.Contains(msg.ErrorMessage, "no API key") {
		t.Errorf("error should name the cause: %q", msg.ErrorMessage)
	}
}

// A gateway may carry credentials in a header instead of an API key.
func TestHeaderAuthIsAccepted(t *testing.T) {
	model, _ := serveSSE(t, textBody)
	auth := "Bearer gateway-token"
	_, msg := collect(Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{Headers: map[string]*string{"Authorization": &auth}},
	}))
	if msg.StopReason != ai.StopStop {
		t.Errorf("header auth should be accepted, got %q: %s", msg.StopReason, msg.ErrorMessage)
	}
}

func TestAbortIsReportedAsAborted(t *testing.T) {
	model, _ := serveSSE(t, textBody)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, msg := collect(Stream(ctx, model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))
	if msg.StopReason != ai.StopAborted {
		t.Errorf("a cancelled context should abort, got %q", msg.StopReason)
	}
}

// StreamSimple clamps the requested level to what the model supports before
// any dialect translation happens.
//
// Note what "unsupported" means here: a MISSING map entry is supported and
// falls back to the level's own name. Only an explicit null disables a level.
// Getting that backwards would silently downgrade every model whose map lists
// a subset of the levels.
func TestStreamSimpleClampsThinking(t *testing.T) {
	model, captured := serveSSE(t, textBody)
	model.Reasoning = true
	// "high" is explicitly disabled; the nearest supported level is medium.
	model.ThinkingLevelMap = ai.ThinkingLevelMap{"off": strptr("none"), "high": nil}

	_, msg := collect(StreamSimple(context.Background(), model, simpleContext(),
		&ai.SimpleStreamOptions{
			StreamOptions: ai.StreamOptions{APIKey: "k"},
			Reasoning:     "high",
		}))
	if msg.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", msg.ErrorMessage)
	}
	if strings.Contains(*captured, `"reasoning_effort":"high"`) {
		t.Errorf("a disabled level reached the provider: %s", *captured)
	}
	if !strings.Contains(*captured, `"reasoning_effort":"medium"`) {
		t.Errorf("expected a clamp down to medium, got: %s", *captured)
	}
}

// A level the map does not mention is still sent, under its own name — the
// map is an override table, not an allowlist.
func TestUnmappedLevelPassesThroughByName(t *testing.T) {
	model, captured := serveSSE(t, textBody)
	model.Reasoning = true
	model.ThinkingLevelMap = ai.ThinkingLevelMap{"off": strptr("none")}

	collect(StreamSimple(context.Background(), model, simpleContext(),
		&ai.SimpleStreamOptions{
			StreamOptions: ai.StreamOptions{APIKey: "k"},
			Reasoning:     "high",
		}))
	if !strings.Contains(*captured, `"reasoning_effort":"high"`) {
		t.Errorf("an unmapped level should pass through by name: %s", *captured)
	}
}

// The response model is recorded when the provider answers with a different
// one than was asked for — routed providers substitute freely.
func TestResponseModelIsRecorded(t *testing.T) {
	body := chunk(`{"id":"c1","model":"actually-served-model","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`)
	model, _ := serveSSE(t, body)

	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))
	if msg.ResponseModel != "actually-served-model" {
		t.Errorf("response model: %q", msg.ResponseModel)
	}
}

// reasoningDetailBody streams a tool call alongside a reasoning detail of the
// caller's choosing.
func reasoningDetailBody(detail string) string {
	return chunk(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":""}}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{"reasoning_details":[`+detail+`],"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a\"}"}}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`) +
		"data: [DONE]\n\n"
}

// thoughtOf returns the signature attached to the message's single tool call.
func thoughtOf(t *testing.T, msg *ai.AssistantMessage) string {
	t.Helper()
	for _, block := range msg.Content {
		if tc, ok := block.(ai.ToolCall); ok {
			return tc.ThoughtSignature
		}
	}
	t.Fatalf("no tool call in %#v", msg.Content)
	return ""
}

// An encrypted detail is a replayable signature and has to survive the round
// trip attached to the tool call it belongs to.
func TestEncryptedReasoningDetailAttachesToItsToolCall(t *testing.T) {
	model, _ := serveSSE(t, reasoningDetailBody(
		`{"type":"reasoning.encrypted","id":"call_1","data":"c2VjcmV0"}`))

	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if got := thoughtOf(t, msg); !strings.Contains(got, "c2VjcmV0") {
		t.Errorf("signature %q should carry the encrypted payload", got)
	}
}

// The detail can arrive before the tool call it names, so it is held rather
// than dropped.
func TestReasoningDetailArrivingEarlyIsStillAttached(t *testing.T) {
	body := chunk(`{"id":"c1","choices":[{"delta":{"reasoning_details":[{"type":"reasoning.encrypted","id":"call_1","data":"early"}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`) +
		chunk(`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`) +
		"data: [DONE]\n\n"

	model, _ := serveSSE(t, body)
	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if got := thoughtOf(t, msg); !strings.Contains(got, "early") {
		t.Errorf("a detail arriving before its tool call was dropped: %q", got)
	}
}

// Only the encrypted form is replayable. Anything else either duplicates
// thinking content already surfaced, or would be replayed as a signature that
// verifies against nothing.
func TestNonReplayableReasoningDetailsAreIgnored(t *testing.T) {
	cases := []struct {
		name   string
		detail string
	}{
		{"plaintext, which OpenRouter sends for Anthropic",
			`{"type":"reasoning.text","id":"call_1","text":"thinking out loud","format":"anthropic-claude-v1"}`},
		{"encrypted but carrying no payload",
			`{"type":"reasoning.encrypted","id":"call_1","data":""}`},
		{"a future encrypted-ish type tau does not know how to replay",
			`{"type":"reasoning.encrypted_v2","id":"call_1","data":"c2VjcmV0"}`},
		{"encrypted with no tool call to attach to",
			`{"type":"reasoning.encrypted","id":"","data":"c2VjcmV0"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, _ := serveSSE(t, reasoningDetailBody(tc.detail))
			_, msg := collect(Stream(context.Background(), model, simpleContext(),
				&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

			if got := thoughtOf(t, msg); got != "" {
				t.Errorf("signature should be empty, got %q", got)
			}
		})
	}
}
