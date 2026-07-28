package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai"
)

func sseBody(events ...[2]string) string {
	var b strings.Builder
	for _, ev := range events {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", ev[0], ev[1])
	}
	return b.String()
}

func serveSSE(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
}

func runStream(t *testing.T, body string, model *ai.Model) ([]ai.Event, *ai.AssistantMessage) {
	t.Helper()
	srv := serveSSE(t, body)
	defer srv.Close()
	m := *model
	m.BaseURL = srv.URL
	s := Stream(context.Background(), &m, userCtx("go"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k"}})
	var events []ai.Event
	for ev := range s.Events() {
		events = append(events, ev)
	}
	return events, s.Result()
}

func eventTypes(events []ai.Event) []ai.EventType {
	out := make([]ai.EventType, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

func typesEqual(got []ai.EventType, want ...ai.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestSSETextOnly(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":12,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":5}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	events, final := runStream(t, body, testModel())

	if !typesEqual(eventTypes(events), ai.EventStart, ai.EventTextStart, ai.EventTextDelta, ai.EventTextDelta, ai.EventTextEnd, ai.EventDone) {
		t.Fatalf("events = %v", eventTypes(events))
	}
	if final.StopReason != ai.StopStop || final.ResponseID != "msg_1" {
		t.Errorf("final = %+v", final)
	}
	text := final.Content[0].(ai.TextContent)
	if text.Text != "Hello" {
		t.Errorf("text = %q", text.Text)
	}
	if final.Usage.Input != 12 || final.Usage.Output != 5 || final.Usage.TotalTokens != 17 {
		t.Errorf("usage = %+v", final.Usage)
	}
	wantCost := 3.0/1e6*12 + 15.0/1e6*5
	if diff := final.Usage.Cost.Total - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %v want %v", final.Usage.Cost.Total, wantCost)
	}
}

func TestSSEThinkingWithSignature(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1,"output_tokens":0}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig123"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	events, final := runStream(t, body, testModel())

	if !typesEqual(eventTypes(events), ai.EventStart, ai.EventThinkingStart, ai.EventThinkingDelta, ai.EventThinkingEnd, ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd, ai.EventDone) {
		t.Fatalf("events = %v", eventTypes(events))
	}
	thinking := final.Content[0].(ai.ThinkingContent)
	if thinking.Thinking != "hmm" || thinking.ThinkingSignature != "sig123" {
		t.Errorf("thinking = %+v", thinking)
	}
}

func TestSSERedactedThinking(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"opaque-blob"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	_, final := runStream(t, body, testModel())
	thinking := final.Content[0].(ai.ThinkingContent)
	if !thinking.Redacted || thinking.ThinkingSignature != "opaque-blob" || thinking.Thinking != "[Reasoning redacted]" {
		t.Errorf("redacted = %+v", thinking)
	}
}

func TestSSEParallelToolCallsWithStreamedArgs(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":10,"output_tokens":0}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tc_1","name":"bash","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tc_2","name":"read","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\": \"a.txt\"}"}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"and\": \"ls\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	events, final := runStream(t, body, testModel())

	if final.StopReason != ai.StopToolUse {
		t.Fatalf("stop = %v", final.StopReason)
	}
	tc1 := final.Content[0].(ai.ToolCall)
	tc2 := final.Content[1].(ai.ToolCall)
	if tc1.ID != "tc_1" || tc1.Arguments["command"] != "ls" {
		t.Errorf("tc1 = %+v", tc1)
	}
	if tc2.ID != "tc_2" || tc2.Arguments["path"] != "a.txt" {
		t.Errorf("tc2 = %+v", tc2)
	}

	// Mid-stream partial arg salvage: after the first delta of tc_1, partial
	// args should contain the truncated-key parse (empty map), after the full
	// json they parse completely. Verify via toolcall_delta events.
	var tc1Deltas []ai.Event
	for _, ev := range events {
		if ev.Type == ai.EventToolCallDelta && ev.ContentIndex == 1 {
			// content index 1 is tc_2 (block order: tc_1=0? no—tc_1 index 0)
			continue
		}
		if ev.Type == ai.EventToolCallDelta && ev.ContentIndex == 0 {
			tc1Deltas = append(tc1Deltas, ev)
		}
	}
	if len(tc1Deltas) != 2 {
		t.Fatalf("tc1 deltas = %d", len(tc1Deltas))
	}
	// The toolcall_end events must carry final parsed args.
	foundEnd := false
	for _, ev := range events {
		if ev.Type == ai.EventToolCallEnd && ev.ToolCall.ID == "tc_1" {
			foundEnd = true
			if ev.ToolCall.Arguments["command"] != "ls" {
				t.Errorf("end args = %v", ev.ToolCall.Arguments)
			}
		}
	}
	if !foundEnd {
		t.Error("no toolcall_end for tc_1")
	}
}

func TestSSEMaxTokensStop(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"trunc"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	_, final := runStream(t, body, testModel())
	if final.StopReason != ai.StopLength {
		t.Errorf("stop = %v", final.StopReason)
	}
}

func TestSSERefusalStop(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"refusal","stop_details":{"explanation":"nope"}}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	events, final := runStream(t, body, testModel())
	last := events[len(events)-1]
	if last.Type != ai.EventError || last.Reason != ai.StopError {
		t.Fatalf("last = %+v", last)
	}
	if final.StopReason != ai.StopError || final.ErrorMessage != "nope" {
		t.Errorf("final = %v %q", final.StopReason, final.ErrorMessage)
	}
}

func TestSSEErrorEventMidStream(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par"}}`},
		[2]string{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
	)
	events, final := runStream(t, body, testModel())
	last := events[len(events)-1]
	if last.Type != ai.EventError {
		t.Fatalf("last = %v", last.Type)
	}
	if final.StopReason != ai.StopError {
		t.Errorf("stop = %v", final.StopReason)
	}
	// Partial content preserved on error.
	if text := final.Content[0].(ai.TextContent); text.Text != "par" {
		t.Errorf("partial text = %q", text.Text)
	}
}

func TestSSEStreamEndsWithoutMessageStop(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	)
	_, final := runStream(t, body, testModel())
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "before message_stop") {
		t.Errorf("final = %v %q", final.StopReason, final.ErrorMessage)
	}
}

func TestSSEAbortMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody(
			[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":7}}}`},
			[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`},
		)))
		w.(http.Flusher).Flush()
		<-release // hold the stream open until the client aborts
	}))
	defer srv.Close()
	defer close(release)

	m := testModel()
	m.BaseURL = srv.URL
	s := Stream(ctx, m, userCtx("go"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k"}})

	var final *ai.AssistantMessage
	done := make(chan struct{})
	go func() {
		sawDelta := false
		for ev := range s.Events() {
			if ev.Type == ai.EventTextDelta && !sawDelta {
				sawDelta = true
				cancel()
			}
		}
		final = s.Result()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not terminate after abort")
	}
	if final.StopReason != ai.StopAborted {
		t.Errorf("stop = %v (%q)", final.StopReason, final.ErrorMessage)
	}
	// Partial content and usage from message_start survive the abort.
	if text := final.Content[0].(ai.TextContent); text.Text != "partial" {
		t.Errorf("partial text = %q", text.Text)
	}
	if final.Usage.Input != 7 {
		t.Errorf("usage.input = %d", final.Usage.Input)
	}
}

func TestSSECacheWrite1hSplitAndCost(t *testing.T) {
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":100,"output_tokens":0,"cache_read_input_tokens":50,"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":600,"ephemeral_1h_input_tokens":400}}}}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	_, final := runStream(t, body, testModel())

	if final.Usage.CacheWrite != 1000 || final.Usage.CacheWrite1h == nil || *final.Usage.CacheWrite1h != 400 {
		t.Fatalf("usage = %+v", final.Usage)
	}
	// 1h writes billed at 2x input rate: (3.75*600 + 3*2*400)/1e6
	wantCacheWrite := (3.75*600 + 3.0*2*400) / 1e6
	if diff := final.Usage.Cost.CacheWrite - wantCacheWrite; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cacheWrite cost = %v want %v", final.Usage.Cost.CacheWrite, wantCacheWrite)
	}
}

func TestHTTPErrorBecomesErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
	}))
	defer srv.Close()
	m := testModel()
	m.BaseURL = srv.URL
	final := Stream(context.Background(), m, userCtx("x"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "invalid_request_error") {
		t.Errorf("final = %v %q", final.StopReason, final.ErrorMessage)
	}
}

func TestRetryOn429ThenSuccess(t *testing.T) {
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			w.Header().Set("retry-after-ms", "10")
			w.WriteHeader(429)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(minimalSSE))
	}))
	defer srv.Close()
	m := testModel()
	m.BaseURL = srv.URL
	one := 1
	final := Stream(context.Background(), m, userCtx("r"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: &one}}).Result()
	if final.StopReason != ai.StopStop {
		t.Errorf("final = %v %q", final.StopReason, final.ErrorMessage)
	}
	if attempt != 2 {
		t.Errorf("attempts = %d", attempt)
	}
}

func TestToolNameNormalizationRoundTrip(t *testing.T) {
	// OAuth: outgoing "bash" → "Bash"; incoming "Bash" → matched back to "bash".
	c := userCtx("call a tool")
	c.Tools = []ai.Tool{bashTool()}
	body := sseBody(
		[2]string{"message_start", `{"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"Bash","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	srv := serveSSE(t, body)
	defer srv.Close()
	m := testModel()
	m.BaseURL = srv.URL
	final := Stream(context.Background(), m, c, &Options{StreamOptions: ai.StreamOptions{APIKey: "sk-ant-oat01-x"}}).Result()
	tc := final.Content[0].(ai.ToolCall)
	if tc.Name != "bash" {
		t.Errorf("tool name = %q, want mapped back to %q", tc.Name, "bash")
	}
}
