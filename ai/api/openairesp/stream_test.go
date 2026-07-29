package openairesp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// serveSSE replays a recorded body and reports what the request looked like.
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

	return modelFor("openai", srv.URL), &captured
}

func chunk(data string) string { return "data: " + data + "\n\n" }

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

func run1(t *testing.T, body string) ([]ai.EventType, *ai.AssistantMessage) {
	t.Helper()
	model, _ := serveSSE(t, body)
	return collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))
}

const textBody = chunkCreated +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}` + "\n\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"delta":"Hello"}` + "\n\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"delta":" world"}` + "\n\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1",` +
	`"content":[{"type":"output_text","text":"Hello world"}]}}` + "\n\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed",` +
	`"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n"

const chunkCreated = `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n"

func TestStreamsTextAndSettles(t *testing.T) {
	types, msg := run1(t, textBody)

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
	if msg.ResponseID != "resp_1" {
		t.Errorf("response id: %q", msg.ResponseID)
	}
	if msg.Usage.Input != 10 || msg.Usage.Output != 5 {
		t.Errorf("usage: %+v", msg.Usage)
	}
}

// The completed item is authoritative: it carries the item id tau needs to
// replay the turn, and the deltas do not.
func TestTextSignatureIsCapturedFromTheDoneItem(t *testing.T) {
	_, msg := run1(t, textBody)

	sig := msg.Content[0].(ai.TextContent).TextSignature
	id, _ := parseTextSignature(sig)
	if id != "msg_1" {
		t.Errorf("signature %q did not carry the item id", sig)
	}
}

// A tool call ends with parsed arguments and a paired id.
func TestStreamsAToolCall(t *testing.T) {
	body := chunkCreated +
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call",`+
			`"id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`) +
		chunk(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`) +
		chunk(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"a.go\"}"}`) +
		chunk(`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"a.go\"}"}`) +
		chunk(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call",`+
			`"id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"a.go\"}"}}`) +
		chunk(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)

	types, msg := run1(t, body)

	for _, want := range []ai.EventType{ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd} {
		if !hasType(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	if msg.StopReason != ai.StopToolUse {
		t.Errorf("a turn ending in a tool call must say so: %q", msg.StopReason)
	}

	call, ok := msg.Content[0].(ai.ToolCall)
	if !ok {
		t.Fatalf("content: %#v", msg.Content)
	}
	if call.ID != "call_1|fc_1" {
		t.Errorf("id: %q — both halves are needed to replay the pairing", call.ID)
	}
	if call.Arguments["path"] != "a.go" {
		t.Errorf("arguments: %#v", call.Arguments)
	}
}

// THE POINT: the done event repeats the WHOLE argument string. Emitting it as
// a delta would double every character in any consumer that concatenates.
func TestArgumentsDoneEmitsOnlyTheRemainder(t *testing.T) {
	body := chunkCreated +
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call",`+
			`"id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`) +
		chunk(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`) +
		chunk(`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"a.go\"}"}`) +
		chunk(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call",`+
			`"id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"a.go\"}"}}`) +
		chunk(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)

	model, _ := serveSSE(t, body)
	var deltas strings.Builder
	stream := Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}})
	for ev := range stream.Events() {
		if ev.Type == ai.EventToolCallDelta {
			deltas.WriteString(ev.Delta)
		}
	}

	if got := deltas.String(); got != `{"path":"a.go"}` {
		t.Errorf("concatenated deltas produced %q", got)
	}
}

// Reasoning arrives as summary deltas and settles into an opaque signature.
func TestStreamsReasoning(t *testing.T) {
	body := chunkCreated +
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`) +
		chunk(`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"weighing"}`) +
		chunk(`{"type":"response.output_item.done","output_index":0,"item":`+reasoningItem+`}`) +
		chunk(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`)

	types, msg := run1(t, body)

	for _, want := range []ai.EventType{ai.EventThinkingStart, ai.EventThinkingDelta, ai.EventThinkingEnd} {
		if !hasType(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	thinking, ok := msg.Content[0].(ai.ThinkingContent)
	if !ok {
		t.Fatalf("content: %#v", msg.Content)
	}
	if thinking.Thinking != "thinking" {
		t.Errorf("the done item is authoritative over the deltas: %q", thinking.Thinking)
	}
	if !strings.Contains(thinking.ThinkingSignature, "ENCRYPTED_PAYLOAD") {
		t.Errorf("the signature must carry the payload verbatim: %q", thinking.ThinkingSignature)
	}
}

// THE POINT: Azure omits the encrypted payload from the per-item event and
// supplies it only on the terminal response. Without the backfill a replayed
// turn carries a reasoning item with no payload — and the session fails on the
// SECOND turn, which is a miserable thing to diagnose.
func TestEncryptedReasoningIsBackfilledFromTheTerminalResponse(t *testing.T) {
	body := chunkCreated +
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`) +
		chunk(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1",`+
			`"summary":[{"type":"summary_text","text":"weighing"}]}}`) +
		chunk(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[`+
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"weighing"}],`+
			`"encrypted_content":"LATE_PAYLOAD"}]}}`)

	_, msg := run1(t, body)

	thinking := msg.Content[0].(ai.ThinkingContent)
	if !strings.Contains(thinking.ThinkingSignature, "LATE_PAYLOAD") {
		t.Errorf("the late payload was not backfilled: %q", thinking.ThinkingSignature)
	}
}

// Usage: input_tokens INCLUDES cached and cache-write tokens here, so both are
// subtracted to leave the uncached input every other wire reports.
func TestUsageSubtractsBothCacheFigures(t *testing.T) {
	body := chunkCreated +
		chunk(`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{`+
			`"input_tokens":1000,"output_tokens":50,"total_tokens":1050,`+
			`"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":100},`+
			`"output_tokens_details":{"reasoning_tokens":30}}}}`)

	_, msg := run1(t, body)

	if msg.Usage.Input != 100 {
		t.Errorf("input: %d, want 100 (1000 - 800 cached - 100 written)", msg.Usage.Input)
	}
	if msg.Usage.CacheRead != 800 || msg.Usage.CacheWrite != 100 {
		t.Errorf("cache: read=%d write=%d", msg.Usage.CacheRead, msg.Usage.CacheWrite)
	}
	if msg.Usage.Reasoning == nil || *msg.Usage.Reasoning != 30 {
		t.Errorf("reasoning tokens: %v", msg.Usage.Reasoning)
	}
}

// An incomplete response means the model ran out of room, not that it failed.
func TestIncompleteMapsToLength(t *testing.T) {
	_, msg := run1(t, chunkCreated+
		chunk(`{"type":"response.incomplete","response":{"id":"r","status":"incomplete"}}`))

	if msg.StopReason != ai.StopLength {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
}

// A failed response has to surface whatever detail it carries.
func TestResponseFailedSurfacesItsError(t *testing.T) {
	_, msg := run1(t, chunkCreated+
		chunk(`{"type":"response.failed","response":{"id":"r","status":"failed",`+
			`"error":{"code":"server_error","message":"upstream exploded"}}}`))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	for _, want := range []string{"server_error", "upstream exploded"} {
		if !strings.Contains(msg.ErrorMessage, want) {
			t.Errorf("error should mention %q: %q", want, msg.ErrorMessage)
		}
	}
}

// A stream that stops without a terminal event is a truncated response, not a
// successful empty one.
func TestTruncatedStreamIsAnError(t *testing.T) {
	_, msg := run1(t, chunkCreated+
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m"}}`))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "terminal") {
		t.Errorf("error: %q", msg.ErrorMessage)
	}
}

// A non-2xx must read legibly rather than as a status code alone.
func TestProviderErrorIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	_, msg := collect(Stream(context.Background(), modelFor("openai", srv.URL), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "rate limit exceeded") {
		t.Errorf("the provider's message was lost: %q", msg.ErrorMessage)
	}
}

// Missing credentials fail as a terminal event, never as a panic or a silently
// empty turn.
func TestMissingCredentialsFailCleanly(t *testing.T) {
	_, msg := collect(Stream(context.Background(),
		modelFor("openai", "https://example.invalid"), simpleContext(), &Options{}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "API key") {
		t.Errorf("error: %q", msg.ErrorMessage)
	}
}

// A gateway in front of the endpoint authenticates with its own header, and
// tau must not demand a key it will never use.
func TestAGatewayAuthHeaderCountsAsCredentials(t *testing.T) {
	model, _ := serveSSE(t, textBody)
	auth := "Bearer gateway-token"

	_, msg := collect(Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{Headers: map[string]*string{"Authorization": &auth}},
	}))
	if msg.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", msg.ErrorMessage)
	}
}

// Aborting mid-turn is not an error the user needs explaining.
func TestAbortIsReportedAsAborted(t *testing.T) {
	model, _ := serveSSE(t, textBody)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, msg := collect(Stream(ctx, model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))
	if msg.StopReason != ai.StopAborted {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
}

// Session affinity keeps a conversation on one backend. The header differs by
// host, and the wrong one is silently ignored — costing cache hits nobody
// notices losing.
func TestSessionAffinityHeaders(t *testing.T) {
	cases := []struct {
		name     string
		compat   *ai.CompatFlags
		provider ai.ProviderId
		want     string
	}{
		{"openai sends session_id", nil, "openai", "session_id"},
		{"openrouter sends x-session-id", nil, "openrouter", "x-session-id"},
		{"the nosession variant sends only the correlation id",
			&ai.CompatFlags{SessionAffinityFormat: strptr("openai-nosession")}, "opencode", "x-client-request-id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Clone()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(textBody))
			}))
			defer srv.Close()

			model := modelFor(tc.provider, srv.URL)
			model.Compat = tc.compat
			_, msg := collect(Stream(context.Background(), model, simpleContext(), &Options{
				StreamOptions: ai.StreamOptions{APIKey: "k", SessionID: "sess-1"},
			}))
			if msg.StopReason != ai.StopStop {
				t.Fatalf("stream failed: %s", msg.ErrorMessage)
			}
			if seen.Get(tc.want) != "sess-1" {
				t.Errorf("missing %s: %v", tc.want, seen)
			}
			if tc.want == "x-client-request-id" && seen.Get("session_id") != "" {
				t.Error("the nosession variant must not send session_id")
			}
		})
	}
}

// Service tier changes what a turn actually costs; the catalog records the
// standard rate, so a session on flex or priority would otherwise be misreported.
func TestServiceTierScalesCost(t *testing.T) {
	body := chunkCreated +
		chunk(`{"type":"response.completed","response":{"id":"r","status":"completed","service_tier":"flex",`+
			`"usage":{"input_tokens":1000,"output_tokens":1000,"total_tokens":2000}}}`)

	model, _ := serveSSE(t, body)
	model.Cost = ai.ModelCost{ModelCostRates: ai.ModelCostRates{Input: 1, Output: 1}}

	_, msg := collect(Stream(context.Background(), model, simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	// 1000 in + 1000 out at $1/Mtok is $0.002; flex is billed at half.
	if got := msg.Usage.Cost.Total; got != 0.001 {
		t.Errorf("cost: %v, want half the standard rate", got)
	}
}
