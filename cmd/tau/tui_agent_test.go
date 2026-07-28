package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file drive the whole binary — pty in, real agent loop,
// real wire API — against a local server speaking Anthropic's SSE format. The
// provider honors ANTHROPIC_BASE_URL, so nothing has to be stubbed out inside
// tau: what runs here is exactly what runs against the real endpoint.

// sse formats one Anthropic stream event.
func sse(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// textStream is a complete assistant turn that says text and stops.
func textStream(chunks ...string) string {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	for _, c := range chunks {
		b.WriteString(sse("content_block_delta",
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, c)))
	}
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

// toolCallStream is an assistant turn that calls bash and stops.
func toolCallStream(command string) string {
	var b strings.Builder
	b.WriteString(sse("message_start", `{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`))
	b.WriteString(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}`))
	b.WriteString(sse("content_block_delta",
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`,
			fmt.Sprintf(`{"command":%q}`, command))))
	b.WriteString(sse("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`))
	b.WriteString(sse("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

// fakeAnthropic serves a scripted response per request, repeating the last one
// once the script runs out.
func fakeAnthropic(t *testing.T, responses ...func(w http.ResponseWriter, r *http.Request)) (url string, calls *atomic.Int32) {
	t.Helper()
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(count.Add(1)) - 1
		if i >= len(responses) {
			i = len(responses) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		responses[i](w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &count
}

func writeStream(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// tauEnv points the binary at a scratch state directory and a fake endpoint.
func tauEnv(t *testing.T, baseURL string) []string {
	return []string{
		"TAU_AGENT_DIR=" + t.TempDir(),
		"ANTHROPIC_BASE_URL=" + baseURL,
		"ANTHROPIC_API_KEY=sk-ant-test",
	}
}

// THE P4 GATE, first half: a prompt typed into the TUI runs a real turn and the
// model's answer lands in the transcript.
func TestInteractiveTurnStreamsAnAnswer(t *testing.T) {
	url, calls := fakeAnthropic(t, writeStream(textStream("The answer ", "is 4.")))

	s := startTau(t, t.TempDir(), tauEnv(t, url)...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("what is 2+2\r")

	// The prompt is echoed, then the streamed answer arrives.
	s.waitFor("what is 2+2", 5*time.Second)
	s.waitFor("The answer is 4.", 10*time.Second)

	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly one provider request, got %d", got)
	}

	// Usage accounting reaches the status line.
	s.waitFor("$0.", 5*time.Second)
}

// A tool call has to be visible: the call line, then its output.
func TestInteractiveTurnShowsToolActivity(t *testing.T) {
	url, _ := fakeAnthropic(t,
		writeStream(toolCallStream("echo tau-tool-ran")),
		writeStream(textStream("Done.")),
	)

	s := startTau(t, t.TempDir(), tauEnv(t, url)...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("run echo\r")
	s.waitFor("bash", 10*time.Second)
	s.waitFor("tau-tool-ran", 10*time.Second)
	s.waitFor("Done.", 10*time.Second)
}

// Esc must stop a turn in flight. Without this the TUI is unusable the first
// time the model goes off on a long tangent.
func TestEscapeAbortsATurnInFlight(t *testing.T) {
	// A response that dribbles out slowly, so there is a window to abort in.
	slow := func(w http.ResponseWriter, r *http.Request) {
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte(sse("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)))
		_, _ = w.Write([]byte(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)))
		flush()
		for i := range 200 {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = w.Write([]byte(sse("content_block_delta",
				fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"chunk-%d "}}`, i))))
			flush()
			time.Sleep(60 * time.Millisecond)
		}
	}

	url, _ := fakeAnthropic(t, slow)
	s := startTau(t, t.TempDir(), tauEnv(t, url)...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("tell me a long story\r")
	s.waitFor("chunk-2", 10*time.Second)

	s.send("\x1b") // Esc
	s.waitFor("stop", 5*time.Second)

	// Once stopped, the prompt has to come back to life.
	time.Sleep(1 * time.Second)
	s.send("still alive")
	s.waitFor("still alive", 5*time.Second)
}

// Typing while the agent works is steering, not a second turn: the message is
// acknowledged and queued rather than starting a competing run.
func TestTypingDuringATurnSteers(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) {
		flush := func() {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte(sse("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)))
		_, _ = w.Write([]byte(sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)))
		flush()
		for i := range 40 {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = w.Write([]byte(sse("content_block_delta",
				fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"tick-%d "}}`, i))))
			flush()
			time.Sleep(80 * time.Millisecond)
		}
		_, _ = w.Write([]byte(sse("content_block_stop", `{"type":"content_block_stop","index":0}`)))
		_, _ = w.Write([]byte(sse("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)))
		_, _ = w.Write([]byte(sse("message_stop", `{"type":"message_stop"}`)))
		flush()
	}

	url, _ := fakeAnthropic(t, slow)
	s := startTau(t, t.TempDir(), tauEnv(t, url)...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("start working\r")
	s.waitFor("tick-1", 10*time.Second)

	s.send("also check the tests\r")
	s.waitFor("steering", 5*time.Second)
}

// An authentication failure has to be legible rather than a silent dead end.
func TestProviderErrorIsReported(t *testing.T) {
	url, _ := fakeAnthropic(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	})

	s := startTau(t, t.TempDir(), tauEnv(t, url)...)
	s.waitFor("/help for commands", 10*time.Second)

	s.send("hello\r")
	s.waitFor("invalid x-api-key", 15*time.Second)

	// The session survives the failure.
	s.send("still alive")
	s.waitFor("still alive", 5*time.Second)
}
