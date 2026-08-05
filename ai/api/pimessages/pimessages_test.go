package pimessages

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai"
)

func modelFor(baseURL string) *ai.Model {
	return &ai.Model{
		ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", Api: ai.ApiPiMessages,
		Provider: "radius", BaseURL: baseURL,
		Reasoning: true, Input: []string{"text", "image"},
		ContextWindow: 200000, MaxTokens: 64000,
	}
}

func simpleContext() ai.Context {
	return ai.Context{
		SystemPrompt: "be helpful",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}},
	}
}

// sseServer replies to POST /messages with the given SSE frames.
type sseServer struct {
	frames   []string
	status   int
	body     string
	requests []*http.Request
	payloads []map[string]any
}

func (s *sseServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		s.requests = append(s.requests, r)
		s.payloads = append(s.payloads, payload)

		if s.status != 0 && s.status != http.StatusOK {
			w.WriteHeader(s.status)
			_, _ = w.Write([]byte(s.body))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range s.frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func event(t *testing.T, v map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// collect drains a stream into its events and terminal message, bounded.
//
// The bound is the point. A stream ends when its terminal event is pushed, so a
// bug that skips one does not fail a test — it blocks the consumer forever, and
// the whole package sits on the go-test deadline with no clue which case did
// it. Mutation testing produced exactly that: removing the terminal-event
// backstop hung the run for ten minutes. Waiting with a deadline turns the same
// bug into an immediate failure that names itself.
func collect(t *testing.T, stream *ai.MessageStream) ([]ai.Event, *ai.AssistantMessage) {
	t.Helper()

	type result struct {
		events []ai.Event
		final  *ai.AssistantMessage
	}
	done := make(chan result, 1)
	go func() {
		var events []ai.Event
		for ev := range stream.Events() {
			events = append(events, ev)
		}
		done <- result{events, stream.Result()}
	}()

	select {
	case r := <-done:
		return r.events, r.final
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never terminated — every path must end in a done or error event")
		return nil, nil
	}
}

func runTurn(t *testing.T, s *sseServer, opts *Options) ([]ai.Event, *ai.AssistantMessage) {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	if opts.APIKey == "" {
		opts.APIKey = "key-1"
	}
	model := modelFor(s.start(t))
	return collect(t, Stream(context.Background(), model, simpleContext(), opts))
}

func types(events []ai.Event) []ai.EventType {
	out := make([]ai.EventType, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}

// THE POINT: this wire's whole job is rebuilding a message from events that
// only describe changes. Every block kind has to survive the round trip with
// its signature, or a following turn replays a different conversation than the
// one that happened.
func TestAFullTurnIsRebuiltFromItsEvents(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "start"}),
		event(t, map[string]any{"type": "thinking_start", "contentIndex": 0}),
		event(t, map[string]any{"type": "thinking_delta", "contentIndex": 0, "delta": "let me "}),
		event(t, map[string]any{"type": "thinking_delta", "contentIndex": 0, "delta": "think"}),
		event(t, map[string]any{
			"type": "thinking_end", "contentIndex": 0,
			"content": "let me think", "contentSignature": "sig-think",
		}),
		event(t, map[string]any{"type": "text_start", "contentIndex": 1}),
		event(t, map[string]any{"type": "text_delta", "contentIndex": 1, "delta": "hello"}),
		event(t, map[string]any{
			"type": "text_end", "contentIndex": 1,
			"content": "hello", "contentSignature": "sig-text",
		}),
		event(t, map[string]any{
			"type": "toolcall_start", "contentIndex": 2, "id": "call-1", "toolName": "read",
		}),
		event(t, map[string]any{"type": "toolcall_delta", "contentIndex": 2, "delta": `{"path": "/tm`}),
		event(t, map[string]any{"type": "toolcall_delta", "contentIndex": 2, "delta": `p/a.txt"}`}),
		event(t, map[string]any{
			"type": "toolcall_end", "contentIndex": 2,
			"toolCall": map[string]any{
				"id": "call-1", "name": "read", "arguments": map[string]any{"path": "/tmp/a.txt"},
			},
		}),
		event(t, map[string]any{
			"type": "done", "reason": "toolUse", "responseId": "resp-1",
			"usage": map[string]any{
				"input": 100, "output": 20, "cacheRead": 5, "cacheWrite": 0, "totalTokens": 125,
				"cost": map[string]any{
					"input": 0.3, "output": 0.3, "cacheRead": 0.01, "cacheWrite": 0, "total": 0.61,
				},
			},
		}),
	}}

	events, final := runTurn(t, s, nil)

	want := []ai.EventType{
		ai.EventStart,
		ai.EventThinkingStart, ai.EventThinkingDelta, ai.EventThinkingDelta, ai.EventThinkingEnd,
		ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd,
		ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallDelta, ai.EventToolCallEnd,
		ai.EventDone,
	}
	if got := types(events); len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i, w := range want {
		if events[i].Type != w {
			t.Errorf("event %d is %s, want %s", i, events[i].Type, w)
		}
	}

	if final.StopReason != ai.StopToolUse {
		t.Errorf("stop reason %q", final.StopReason)
	}
	if final.ResponseID != "resp-1" {
		t.Errorf("responseId %q", final.ResponseID)
	}
	if len(final.Content) != 3 {
		t.Fatalf("content %+v", final.Content)
	}

	thinking, ok := final.Content[0].(ai.ThinkingContent)
	if !ok || thinking.Thinking != "let me think" || thinking.ThinkingSignature != "sig-think" {
		t.Errorf("thinking block %+v", final.Content[0])
	}
	text, ok := final.Content[1].(ai.TextContent)
	if !ok || text.Text != "hello" || text.TextSignature != "sig-text" {
		t.Errorf("text block %+v", final.Content[1])
	}
	call, ok := final.Content[2].(ai.ToolCall)
	if !ok || call.ID != "call-1" || call.Name != "read" || call.Arguments["path"] != "/tmp/a.txt" {
		t.Errorf("tool call %+v", final.Content[2])
	}

	// The gateway prices its own catalog, so its costing is the one that counts.
	if final.Usage.Input != 100 || final.Usage.TotalTokens != 125 || final.Usage.Cost.Total != 0.61 {
		t.Errorf("usage %+v", final.Usage)
	}
}

// THE POINT: a tool call is renderable while it is still arriving only because
// each delta is re-parsed with salvage. Waiting for valid JSON would leave the
// UI blank for the whole call.
func TestToolArgumentsAreSalvagedWhileTheyStream(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "toolcall_start", "contentIndex": 0, "id": "c", "toolName": "write"}),
		event(t, map[string]any{"type": "toolcall_delta", "contentIndex": 0, "delta": `{"path": "a.txt", "content": "hel`}),
		event(t, map[string]any{
			"type": "toolcall_end", "contentIndex": 0,
			"toolCall": map[string]any{
				"id": "c", "name": "write",
				"arguments": map[string]any{"path": "a.txt", "content": "hello"},
			},
		}),
		event(t, map[string]any{"type": "done", "reason": "toolUse"}),
	}}

	events, final := runTurn(t, s, nil)

	var midflight ai.ToolCall
	for _, ev := range events {
		if ev.Type == ai.EventToolCallDelta {
			midflight = ev.Partial.Content[0].(ai.ToolCall)
		}
	}
	if midflight.Arguments["path"] != "a.txt" {
		t.Errorf("mid-flight arguments %+v — a truncated call must still parse", midflight.Arguments)
	}

	// And the completed call wins: the salvaged "hel" must not reach the tool.
	call := final.Content[0].(ai.ToolCall)
	if call.Arguments["content"] != "hello" {
		t.Errorf("final arguments %+v", call.Arguments)
	}
}

// THE POINT: the context goes over verbatim — no per-provider conversion —
// because the backend does that itself. Rewriting it here would mean the
// gateway sees a transcript tau has already mangled.
func TestTheRequestCarriesTheContextAndOptions(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	temp := 0.5
	_, _ = runTurn(t, s, &Options{
		StreamOptions: ai.StreamOptions{
			Temperature: &temp, MaxTokens: 4096, SessionID: "session-7",
			CacheRetention: ai.CacheLong,
		},
		Reasoning:  ai.ThinkingHigh,
		ToolChoice: &ToolChoice{Mode: "required"},
	})

	payload := s.payloads[0]
	if payload["model"] != "claude-sonnet-4-5" {
		t.Errorf("model %v", payload["model"])
	}

	sent := payload["context"].(map[string]any)
	if sent["systemPrompt"] != "be helpful" {
		t.Errorf("systemPrompt %v", sent["systemPrompt"])
	}
	messages := sent["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Errorf("messages %+v", messages)
	}

	opts := payload["options"].(map[string]any)
	for field, want := range map[string]any{
		"temperature":    0.5,
		"maxTokens":      float64(4096),
		"reasoning":      "high",
		"cacheRetention": "long",
		"sessionId":      "session-7",
		"toolChoice":     "required",
	} {
		if opts[field] != want {
			t.Errorf("options.%s = %v, want %v", field, opts[field], want)
		}
	}
}

// An unset option must be absent, not zero: the backend's own default is what
// applies, and a maxTokens of 0 is a request for no output at all.
func TestUnsetOptionsAreOmitted(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, nil)

	opts := s.payloads[0]["options"].(map[string]any)
	for _, field := range []string{"temperature", "maxTokens", "reasoning", "cacheRetention", "sessionId", "toolChoice"} {
		if _, present := opts[field]; present {
			t.Errorf("options.%s was sent as %v, want omitted", field, opts[field])
		}
	}
}

func TestToolChoiceCanNameOneTool(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, &Options{ToolChoice: &ToolChoice{Name: "bash"}})

	choice := s.payloads[0]["options"].(map[string]any)["toolChoice"].(map[string]any)
	if choice["type"] != "function" {
		t.Errorf("toolChoice %+v", choice)
	}
	if fn := choice["function"].(map[string]any); fn["name"] != "bash" {
		t.Errorf("toolChoice.function %+v", fn)
	}
}

// PI_CACHE_RETENTION is the legacy opt-in, and only "long" means anything.
func TestCacheRetentionFallsBackToTheLegacyEnvOptIn(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, &Options{StreamOptions: ai.StreamOptions{
		Env: map[string]string{"PI_CACHE_RETENTION": "long"},
	}})
	if got := s.payloads[0]["options"].(map[string]any)["cacheRetention"]; got != "long" {
		t.Errorf("cacheRetention %v", got)
	}

	s = &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, &Options{StreamOptions: ai.StreamOptions{
		Env: map[string]string{"PI_CACHE_RETENTION": "short"},
	}})
	if got, present := s.payloads[0]["options"].(map[string]any)["cacheRetention"]; present {
		t.Errorf("cacheRetention %v — only \"long\" is mapped", got)
	}
}

// An explicit retention beats the environment: the caller is more specific
// than a variable exported months ago.
func TestAnExplicitCacheRetentionWinsOverTheEnvironment(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, &Options{StreamOptions: ai.StreamOptions{
		CacheRetention: ai.CacheNone,
		Env:            map[string]string{"PI_CACHE_RETENTION": "long"},
	}})
	if got := s.payloads[0]["options"].(map[string]any)["cacheRetention"]; got != "none" {
		t.Errorf("cacheRetention %v", got)
	}
}

func TestDebugAsksTheBackendForRoutingMetadata(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, &Options{Debug: true})
	if got := s.requests[0].URL.Query().Get("debug"); got != "1" {
		t.Errorf("debug query %q", got)
	}
}

func TestTheRequestIsAuthorizedAndAcceptsEvents(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, &Options{StreamOptions: ai.StreamOptions{APIKey: "key-9"}})

	req := s.requests[0]
	if got := req.Header.Get("Authorization"); got != "Bearer key-9" {
		t.Errorf("Authorization %q", got)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept %q", got)
	}
	if !strings.HasSuffix(req.URL.Path, "/messages") {
		t.Errorf("path %q", req.URL.Path)
	}
}

// A nil header value suppresses a default rather than sending an empty one —
// the only way to remove a header tau adds.
func TestHeaderOverridesAddAndSuppress(t *testing.T) {
	custom := "yes"
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, &Options{StreamOptions: ai.StreamOptions{
		Headers: map[string]*string{"X-Custom": &custom, "Accept": nil},
	}})

	req := s.requests[0]
	if got := req.Header.Get("X-Custom"); got != "yes" {
		t.Errorf("X-Custom %q", got)
	}
	if got := req.Header.Get("Accept"); got != "" {
		t.Errorf("Accept %q, want suppressed", got)
	}
}

func TestAMissingAPIKeyFailsBeforeTheRequest(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	model := modelFor(s.start(t))
	_, final := collect(t, Stream(context.Background(), model, simpleContext(), &Options{}))

	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "no API key") {
		t.Errorf("final %+v", final)
	}
	if len(s.requests) != 0 {
		t.Errorf("%d requests were sent without a key", len(s.requests))
	}
}

// THE POINT: a stream that stops early has produced a partial answer, not a
// finished one. Treating the end of the body as the end of the turn would
// commit a truncated message to the session as though the model had stopped.
func TestATruncatedStreamIsAnError(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "text_start", "contentIndex": 0}),
		event(t, map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "half an ans"}),
	}}

	events, final := runTurn(t, s, nil)

	if final.StopReason != ai.StopError {
		t.Errorf("stop reason %q", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "without a terminal event") {
		t.Errorf("error %q", final.ErrorMessage)
	}
	// The text that did arrive is kept: it is what the user watched appear.
	if text := final.Content[0].(ai.TextContent).Text; text != "half an ans" {
		t.Errorf("partial content %q", text)
	}
	if last := events[len(events)-1]; last.Type != ai.EventError {
		t.Errorf("last event %s", last.Type)
	}
}

// THE POINT: the content index is an index into a slice tau allocates. Pi runs
// on a JavaScript array where a gap is a hole; Go would either panic or
// allocate whatever length the server named, so the sequence is checked.
func TestAnOutOfOrderContentIndexIsRejected(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "text_start", "contentIndex": 7}),
		event(t, map[string]any{"type": "done", "reason": "stop"}),
	}}

	_, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "out of order") {
		t.Errorf("final %+v", final)
	}
}

func TestAHugeContentIndexIsRejectedRatherThanAllocated(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "text_start", "contentIndex": 2147483000}),
		event(t, map[string]any{"type": "done", "reason": "stop"}),
	}}

	_, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "out of order") {
		t.Errorf("final %+v", final)
	}
}

// A delta for a block that was never opened would read past the end of the
// content slice.
func TestADeltaForAnUnopenedBlockIsRejected(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "hi"}),
		event(t, map[string]any{"type": "done", "reason": "stop"}),
	}}

	_, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "never started") {
		t.Errorf("final %+v", final)
	}
}

// A block opened as text and written as thinking is a broken stream. Applying
// it would silently move text into the reasoning channel, where it is hidden.
func TestADeltaForTheWrongBlockKindIsRejected(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "text_start", "contentIndex": 0}),
		event(t, map[string]any{"type": "thinking_delta", "contentIndex": 0, "delta": "secret"}),
		event(t, map[string]any{"type": "done", "reason": "stop"}),
	}}

	_, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "not a thinking block") {
		t.Errorf("final %+v", final)
	}
}

// THE POINT: the stop reason drives the agent loop — "toolUse" runs tools,
// "stop" ends the turn. A value outside the union would be applied verbatim
// and change what tau does next.
func TestATerminalEventWithABogusReasonIsRejected(t *testing.T) {
	for _, tc := range []struct{ kind, reason string }{
		{"done", "error"},    // an error dressed as a completed turn
		{"done", "pending"},  // not a terminal reason at all
		{"error", "toolUse"}, // tools to run on a failed turn
	} {
		s := &sseServer{frames: []string{
			event(t, map[string]any{"type": tc.kind, "reason": tc.reason}),
		}}
		_, final := runTurn(t, s, nil)
		if final.StopReason != ai.StopError {
			t.Errorf("%s/%s: stop reason %q", tc.kind, tc.reason, final.StopReason)
		}
		if !strings.Contains(final.ErrorMessage, "stop reason") {
			t.Errorf("%s/%s: error %q", tc.kind, tc.reason, final.ErrorMessage)
		}
	}
}

// A backend that has learned a new event must not break an older tau.
func TestAnUnknownEventTypeIsIgnored(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "quantum_start", "contentIndex": 0}),
		event(t, map[string]any{"type": "text_start", "contentIndex": 0}),
		event(t, map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "hi"}),
		event(t, map[string]any{"type": "done", "reason": "stop"}),
	}}

	events, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopStop {
		t.Errorf("final %+v", final)
	}
	if got := types(events); len(got) != 3 {
		t.Errorf("events %v — the unknown event should have been skipped", got)
	}
}

// THE POINT: a gateway rewriting the conversation before it reaches the model
// means the answer came from a context tau did not send, and the user paid for
// that one. It goes on the message, because the session file is the only place
// that record survives.
func TestAServerSideRewriteIsRecordedAsADiagnostic(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{
			"type": "done", "reason": "stop",
			"rewrite": map[string]any{
				"policyId": "redact-secrets", "policyVersion": 3, "changed": true,
				"tokenCountChange": -120, "messageCountChange": -1, "systemPromptChanged": true,
			},
		}),
	}}

	_, final := runTurn(t, s, nil)
	if len(final.Diagnostics) != 1 {
		t.Fatalf("diagnostics %+v", final.Diagnostics)
	}
	d := final.Diagnostics[0]
	if d.Type != "pi_messages_rewrite" {
		t.Errorf("diagnostic type %q", d.Type)
	}
	if d.Details["policyId"] != "redact-secrets" || d.Details["changed"] != true {
		t.Errorf("details %+v", d.Details)
	}
	if d.Details["tokenCountChange"] != -120 {
		t.Errorf("tokenCountChange %v", d.Details["tokenCountChange"])
	}
}

// An error event is the backend reporting a failure it already understands, so
// its message is passed through rather than replaced.
func TestAnErrorEventCarriesItsMessageAndReason(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "text_start", "contentIndex": 0}),
		event(t, map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "partial"}),
		event(t, map[string]any{
			"type": "error", "reason": "error", "errorMessage": "upstream provider timed out",
			"usage": map[string]any{"input": 10, "output": 2, "totalTokens": 12},
		}),
	}}

	events, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopError {
		t.Errorf("stop reason %q", final.StopReason)
	}
	if final.ErrorMessage != "upstream provider timed out" {
		t.Errorf("error message %q", final.ErrorMessage)
	}
	if final.Usage.TotalTokens != 12 {
		t.Errorf("usage %+v — a failed turn was still billed", final.Usage)
	}
	if final.Content[0].(ai.TextContent).Text != "partial" {
		t.Errorf("content %+v", final.Content)
	}
	if last := events[len(events)-1]; last.Type != ai.EventError || last.Reason != ai.StopError {
		t.Errorf("last event %+v", last)
	}
}

func TestAnAbortReportedByTheBackendStaysAborted(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "error", "reason": "aborted"}),
	}}
	_, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopAborted {
		t.Errorf("stop reason %q", final.StopReason)
	}
}

// THE POINT: a gateway's error body is the only account of why a well-formed
// request was refused — which key, which policy, which upstream. Flattening it
// to a status code loses the one thing that would tell the user what to fix.
func TestAStructuredErrorBodyIsReportedAndDiagnosed(t *testing.T) {
	s := &sseServer{
		status: http.StatusForbidden,
		body:   `{"error":{"message":"model not enabled for this key","code":"model_forbidden","upstream":"anthropic"}}`,
	}

	_, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopError {
		t.Fatalf("final %+v", final)
	}
	for _, want := range []string{"403", "model not enabled for this key", "model_forbidden"} {
		if !strings.Contains(final.ErrorMessage, want) {
			t.Errorf("error %q is missing %q", final.ErrorMessage, want)
		}
	}

	if len(final.Diagnostics) != 1 {
		t.Fatalf("diagnostics %+v", final.Diagnostics)
	}
	d := final.Diagnostics[0]
	if d.Type != "pi_messages_response_failure" || d.Error == nil || d.Error.Code != "model_forbidden" {
		t.Errorf("diagnostic %+v", d)
	}
	if d.Details["status"] != 403 || d.Details["provider"] != "radius" {
		t.Errorf("details %+v", d.Details)
	}
	// The fields tau does not model are still recorded: a gateway names its
	// upstream, and that is exactly what makes the failure diagnosable.
	body := d.Details["error"].(map[string]any)
	if body["upstream"] != "anthropic" {
		t.Errorf("error details %+v", body)
	}
}

// The response may come from a proxy in front of the gateway, which has never
// heard of the error schema.
func TestAnUnparseableErrorBodyIsKeptVerbatim(t *testing.T) {
	s := &sseServer{status: http.StatusBadGateway, body: "<html><body>502 Bad Gateway</body></html>"}

	_, final := runTurn(t, s, nil)
	if !strings.Contains(final.ErrorMessage, "502 Bad Gateway") {
		t.Errorf("error %q", final.ErrorMessage)
	}
	body, ok := final.Diagnostics[0].Details["body"].(string)
	if !ok || !strings.Contains(body, "<html>") {
		t.Errorf("details %+v", final.Diagnostics[0].Details)
	}
}

// A proxy that answers with a whole error page must not paste it into the
// session file forever.
func TestAHugeErrorBodyIsTruncated(t *testing.T) {
	s := &sseServer{status: http.StatusInternalServerError, body: strings.Repeat("x", maxDiagnosticBody*2)}

	_, final := runTurn(t, s, nil)
	body := final.Diagnostics[0].Details["body"].(string)
	if len(body) > maxDiagnosticBody+4 {
		t.Errorf("body kept %d bytes, want it bounded at %d", len(body), maxDiagnosticBody)
	}
	if !strings.HasSuffix(body, "…") {
		t.Error("a truncated body must say so")
	}
}

// THE POINT: Esc must stop a turn. An abort has to end as "aborted", not
// "error" — the agent loop retries one and reports the other.
func TestCancellationEndsTheStreamAsAborted(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", event(t, map[string]any{"type": "text_start", "contentIndex": 0}))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := Stream(ctx, modelFor(srv.URL), simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "key-1"},
	})

	for ev := range stream.Events() {
		if ev.Type == ai.EventTextStart {
			cancel()
		}
	}
	if final := stream.Result(); final.StopReason != ai.StopAborted {
		t.Errorf("stop reason %q, want aborted", final.StopReason)
	}
}

func TestOnPayloadCanReplaceTheRequest(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	_, _ = runTurn(t, s, &Options{StreamOptions: ai.StreamOptions{
		OnPayload: func(payload any, model *ai.Model) (any, error) {
			return map[string]any{"model": model.ID, "replaced": true}, nil
		},
	}})
	if s.payloads[0]["replaced"] != true {
		t.Errorf("payload %+v", s.payloads[0])
	}
}

func TestOnResponseSeesTheStatus(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	var seen ai.ProviderResponse
	_, _ = runTurn(t, s, &Options{StreamOptions: ai.StreamOptions{
		OnResponse: func(resp ai.ProviderResponse, _ *ai.Model) error {
			seen = resp
			return nil
		},
	}})
	if seen.Status != http.StatusOK {
		t.Errorf("status %d", seen.Status)
	}
}

// An unreadable frame is a broken backend, not something to skip: skipping it
// would drop content out of a message that still looked complete.
func TestAnUnreadableEventFailsTheStream(t *testing.T) {
	s := &sseServer{frames: []string{"{not json"}}
	_, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "unreadable event") {
		t.Errorf("final %+v", final)
	}
}

// The SSE terminator is not an event.
func TestTheDoneMarkerIsNotAnEvent(t *testing.T) {
	s := &sseServer{frames: []string{
		event(t, map[string]any{"type": "done", "reason": "stop"}),
		"[DONE]",
	}}
	_, final := runTurn(t, s, nil)
	if final.StopReason != ai.StopStop {
		t.Errorf("final %+v", final)
	}
}

// A base URL with a trailing slash must not produce "//messages": some
// gateways route on the exact path.
func TestTheMessagesPathIsJoinedCleanly(t *testing.T) {
	for _, base := range []string{"https://radius.pi.dev/v1", "https://radius.pi.dev/v1/", "https://radius.pi.dev/v1//"} {
		if got := messagesURL(base, false); got != "https://radius.pi.dev/v1/messages" {
			t.Errorf("messagesURL(%q) = %q", base, got)
		}
	}
	if got := messagesURL("https://radius.pi.dev/v1", true); got != "https://radius.pi.dev/v1/messages?debug=1" {
		t.Errorf("debug URL %q", got)
	}
}

// StreamSimple is what the provider layer calls, so the options that have no
// home in the normalized shape have to survive the trip through Extra.
func TestStreamSimpleCarriesReasoningAndExtras(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	model := modelFor(s.start(t))

	stream := StreamSimple(context.Background(), model, simpleContext(), &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{
			APIKey: "key-1",
			Extra:  map[string]any{"debug": true, "toolChoice": "none"},
		},
		Reasoning: ai.ThinkingLow,
	})
	if final := stream.Result(); final.StopReason != ai.StopStop {
		t.Fatalf("final %+v", final)
	}

	if got := s.requests[0].URL.Query().Get("debug"); got != "1" {
		t.Errorf("debug %q", got)
	}
	opts := s.payloads[0]["options"].(map[string]any)
	if opts["reasoning"] != "low" || opts["toolChoice"] != "none" {
		t.Errorf("options %+v", opts)
	}
}

// THE POINT: the thinking level is NOT clamped against tau's copy of the
// catalog. The gateway owns the catalog — its models are fetched from it — so
// clamping here would refuse a level the backend supports and tau has stale
// data about.
func TestReasoningIsPassedThroughUnmapped(t *testing.T) {
	s := &sseServer{frames: []string{event(t, map[string]any{"type": "done", "reason": "stop"})}}
	model := modelFor(s.start(t))
	// A model with no thinking-level map: every other wire would clamp "max"
	// away entirely.
	model.ThinkingLevelMap = nil

	stream := StreamSimple(context.Background(), model, simpleContext(), &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{APIKey: "key-1"},
		Reasoning:     ai.ThinkingMax,
	})
	stream.Result()

	if got := s.payloads[0]["options"].(map[string]any)["reasoning"]; got != "max" {
		t.Errorf("reasoning %v, want it passed through", got)
	}
}
