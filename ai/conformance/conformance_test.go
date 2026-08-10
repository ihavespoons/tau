// Package conformance checks that every wire API normalizes to the same event
// grammar.
//
// The per-wire tests each assert their own module's output. What none of them
// can assert is the claim the whole `ai` package rests on: that a caller can
// swap providers and see the same sequence of events. This suite runs one
// scenario table across several dialects and compares them against each other
// rather than against a written-down expectation — the claim is that the wires
// agree, so the test is that they agree.
package conformance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/anthropic"
	"github.com/ihavespoons/tau/ai/api/googlegenai"
	"github.com/ihavespoons/tau/ai/api/openaichat"
)

// scenario is one behaviour every wire has to normalize identically.
type scenario string

const (
	// scenarioText is prose arriving in two deltas.
	scenarioText scenario = "text"
	// scenarioToolCall is a single tool call with streamed arguments.
	scenarioToolCall scenario = "tool call"
	// scenarioLengthStop is a turn cut off by the output limit, which every
	// wire spells differently and which the agent loop treats specially.
	scenarioLengthStop scenario = "length stop"
	// scenarioHTTPError is a 4xx before any event, the shape a bad key takes.
	scenarioHTTPError scenario = "http error"
)

var scenarios = []scenario{scenarioText, scenarioToolCall, scenarioLengthStop, scenarioHTTPError}

// wire is one API module under test.
type wire struct {
	name string
	// serve writes the module's own dialect for a scenario, or an HTTP status.
	serve func(w http.ResponseWriter, s scenario)
	// stream calls the module against the test server.
	stream func(ctx context.Context, baseURL string) *ai.MessageStream
}

func sse(w http.ResponseWriter, body string) {
	w.Header().Set("content-type", "text/event-stream")
	_, _ = w.Write([]byte(body))
}

func events(pairs ...[2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", p[0], p[1])
	}
	return b.String()
}

func lines(datas ...string) string {
	var b strings.Builder
	for _, d := range datas {
		fmt.Fprintf(&b, "data: %s\n\n", d)
	}
	return b.String()
}

func userCtx() ai.Context {
	return ai.Context{
		SystemPrompt: "System prompt.",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "go"}, Timestamp: 1}},
	}
}

// --- the wires ---

func anthropicWire() wire {
	return wire{
		name: "anthropic",
		serve: func(w http.ResponseWriter, s scenario) {
			switch s {
			case scenarioText:
				sse(w, events(
					[2]string{"message_start", `{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":0}}}`},
					[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
					[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
					[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
					[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
					[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5}}`},
					[2]string{"message_stop", `{"type":"message_stop"}`},
				))
			case scenarioToolCall:
				sse(w, events(
					[2]string{"message_start", `{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":0}}}`},
					[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"read","input":{}}}`},
					[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"x\"}"}}`},
					[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
					[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":10,"output_tokens":5}}`},
					[2]string{"message_stop", `{"type":"message_stop"}`},
				))
			case scenarioLengthStop:
				sse(w, events(
					[2]string{"message_start", `{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":0}}}`},
					[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
					[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
					[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
					[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
					[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":10,"output_tokens":5}}`},
					[2]string{"message_stop", `{"type":"message_stop"}`},
				))
			case scenarioHTTPError:
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			}
		},
		stream: func(ctx context.Context, baseURL string) *ai.MessageStream {
			m := model(baseURL)
			return anthropic.Stream(ctx, m, userCtx(), &anthropic.Options{
				StreamOptions: ai.StreamOptions{APIKey: "k"},
			})
		},
	}
}

func openaiChatWire() wire {
	return wire{
		name: "openai-completions",
		serve: func(w http.ResponseWriter, s scenario) {
			switch s {
			case scenarioText:
				sse(w, lines(
					`{"id":"c1","choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`,
					`{"id":"c1","choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`,
					`{"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
					`[DONE]`,
				))
			case scenarioToolCall:
				sse(w, lines(
					`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"read","arguments":""}}]},"finish_reason":null}]}`,
					`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"x\"}"}}]},"finish_reason":null}]}`,
					`{"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
					`[DONE]`,
				))
			case scenarioLengthStop:
				sse(w, lines(
					`{"id":"c1","choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`,
					`{"id":"c1","choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`,
					`{"id":"c1","choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
					`[DONE]`,
				))
			case scenarioHTTPError:
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			}
		},
		stream: func(ctx context.Context, baseURL string) *ai.MessageStream {
			m := model(baseURL)
			return openaichat.Stream(ctx, m, userCtx(), &openaichat.Options{
				StreamOptions: ai.StreamOptions{APIKey: "k"},
			})
		},
	}
}

func googleWire() wire {
	return wire{
		name: "google-genai",
		serve: func(w http.ResponseWriter, s scenario) {
			switch s {
			case scenarioText:
				sse(w, lines(
					`{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
					`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`,
				))
			case scenarioToolCall:
				sse(w, lines(
					`{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read","args":{"path":"x"}}}]}}]}`,
					`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`,
				))
			case scenarioLengthStop:
				sse(w, lines(
					`{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
					`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`,
				))
			case scenarioHTTPError:
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
			}
		},
		stream: func(ctx context.Context, baseURL string) *ai.MessageStream {
			m := model(baseURL)
			return googlegenai.Stream(ctx, m, userCtx(), &googlegenai.Options{
				StreamOptions: ai.StreamOptions{APIKey: "k"},
			})
		},
	}
}

func model(baseURL string) *ai.Model {
	return &ai.Model{
		ID: "test-model", Provider: "test", BaseURL: baseURL,
		ContextWindow: 100000, MaxTokens: 4096,
	}
}

// wires is the coverage. Adding one is a serve function and a stream call.
//
// Three dialects rather than all ten on purpose: what this suite proves is that
// normalization holds across genuinely different shapes — Anthropic's named SSE
// events, OpenAI's chunk deltas, Google's candidate parts. The wires not here
// are covered by their own package tests but do not yet take part in the
// cross-wire comparison.
func wires() []wire {
	return []wire{anthropicWire(), openaiChatWire(), googleWire()}
}

// --- the suite ---

type outcome struct {
	types  []ai.EventType
	stop   ai.StopReason
	text   string
	tools  []string
	errMsg bool
}

func run(t *testing.T, w wire, s scenario) outcome {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		w.serve(rw, s)
	}))
	defer srv.Close()

	stream := w.stream(context.Background(), srv.URL)
	var got outcome
	for ev := range stream.Events() {
		got.types = append(got.types, ev.Type)
	}

	msg := stream.Result()
	if msg != nil {
		got.stop = msg.StopReason
		got.errMsg = msg.ErrorMessage != ""
		for _, c := range msg.Content {
			switch v := c.(type) {
			case ai.TextContent:
				got.text += v.Text
			case ai.ToolCall:
				got.tools = append(got.tools, v.Name)
			}
		}
	}
	return got
}

// The claim the ai package rests on is that swapping providers does not change
// what a caller sees. This is that claim, tested.
func TestEveryWireNormalizesToTheSameGrammar(t *testing.T) {
	for _, s := range scenarios {
		t.Run(string(s), func(t *testing.T) {
			all := wires()
			first := run(t, all[0], s)

			for _, w := range all[1:] {
				got := run(t, w, s)

				if !sameTypes(got.types, first.types) {
					t.Errorf("%s produced %v\n%s produced %v",
						w.name, got.types, all[0].name, first.types)
				}
				if got.stop != first.stop {
					t.Errorf("%s stopped with %q, %s with %q",
						w.name, got.stop, all[0].name, first.stop)
				}
			}
		})
	}
}

// Normalizing the grammar is not enough on its own: the wires also have to
// agree about what was actually said.
func TestEveryWireProducesTheSameContent(t *testing.T) {
	for _, w := range wires() {
		if got := run(t, w, scenarioText); got.text != "Hello" {
			t.Errorf("%s produced text %q, want Hello", w.name, got.text)
		}
		if got := run(t, w, scenarioToolCall); len(got.tools) != 1 || got.tools[0] != "read" {
			t.Errorf("%s produced tools %v, want [read]", w.name, got.tools)
		}
	}
}

// The agent loop treats a length stop specially — it is the one stop reason
// that means the answer is incomplete — so every wire has to report it as
// itself rather than as a normal end.
func TestALengthStopIsNotAnOrdinaryStop(t *testing.T) {
	for _, w := range wires() {
		got := run(t, w, scenarioLengthStop)
		if got.stop != ai.StopLength {
			t.Errorf("%s reported %q for a truncated turn, want %q", w.name, got.stop, ai.StopLength)
		}
	}
}

// A stream never returns an error: a 4xx has to arrive as a terminal error
// event carrying a message, on every wire.
func TestAnHTTPErrorIsATerminalEvent(t *testing.T) {
	for _, w := range wires() {
		got := run(t, w, scenarioHTTPError)

		if len(got.types) == 0 || got.types[len(got.types)-1] != ai.EventError {
			t.Errorf("%s ended with %v, want a terminal error event", w.name, got.types)
		}
		if got.stop != ai.StopError {
			t.Errorf("%s reported %q, want %q", w.name, got.stop, ai.StopError)
		}
		if !got.errMsg {
			t.Errorf("%s reported an error with no message to show", w.name)
		}
	}
}

func sameTypes(a, b []ai.EventType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
