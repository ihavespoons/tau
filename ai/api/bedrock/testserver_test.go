package bedrock

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/ihavespoons/tau/ai"
)

// The whole point of this file: Bedrock is reached through the AWS SDK, so a
// test that stubs the SDK would prove nothing about what tau actually sends or
// how it reads a real response. Instead the server below speaks genuine AWS
// EventStream framing, and the SDK's own serializer and deserializer stay in
// the loop — the request arrives here exactly as it would at AWS, and the
// response is decoded by the same code that would decode a real one.

// frame is one ConverseStream event: the event name and its JSON payload.
type frame struct {
	event   string
	payload map[string]any
}

func textDelta(index int, text string) frame {
	return frame{"contentBlockDelta", map[string]any{
		"contentBlockIndex": index,
		"delta":             map[string]any{"text": text},
	}}
}

func reasoningTextDelta(index int, text string) frame {
	return frame{"contentBlockDelta", map[string]any{
		"contentBlockIndex": index,
		"delta":             map[string]any{"reasoningContent": map[string]any{"text": text}},
	}}
}

func reasoningSignatureDelta(index int, signature string) frame {
	return frame{"contentBlockDelta", map[string]any{
		"contentBlockIndex": index,
		"delta":             map[string]any{"reasoningContent": map[string]any{"signature": signature}},
	}}
}

func toolUseStart(index int, id, name string) frame {
	return frame{"contentBlockStart", map[string]any{
		"contentBlockIndex": index,
		"start":             map[string]any{"toolUse": map[string]any{"toolUseId": id, "name": name}},
	}}
}

func toolUseDelta(index int, fragment string) frame {
	return frame{"contentBlockDelta", map[string]any{
		"contentBlockIndex": index,
		"delta":             map[string]any{"toolUse": map[string]any{"input": fragment}},
	}}
}

func blockStop(index int) frame {
	return frame{"contentBlockStop", map[string]any{"contentBlockIndex": index}}
}

func messageStart() frame {
	return frame{"messageStart", map[string]any{"role": "assistant"}}
}

func messageStop(reason string) frame {
	return frame{"messageStop", map[string]any{"stopReason": reason}}
}

func metadata(usage map[string]any) frame {
	return frame{"metadata", map[string]any{"usage": usage}}
}

// encodeFrames renders frames as an AWS EventStream body.
func encodeFrames(t *testing.T, frames []frame) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := eventstream.NewEncoder()
	for _, f := range frames {
		payload, err := json.Marshal(f.payload)
		if err != nil {
			t.Fatalf("encoding %s payload: %v", f.event, err)
		}
		var headers eventstream.Headers
		headers.Set(":message-type", eventstream.StringValue("event"))
		headers.Set(":event-type", eventstream.StringValue(f.event))
		headers.Set(":content-type", eventstream.StringValue("application/json"))
		if err := enc.Encode(&buf, eventstream.Message{Headers: headers, Payload: payload}); err != nil {
			t.Fatalf("encoding %s frame: %v", f.event, err)
		}
	}
	return buf.Bytes()
}

// encodeException renders a modelled Bedrock exception frame.
func encodeException(t *testing.T, name, message string) []byte {
	t.Helper()
	var buf bytes.Buffer
	payload, err := json.Marshal(map[string]any{"message": message})
	if err != nil {
		t.Fatal(err)
	}
	var headers eventstream.Headers
	headers.Set(":message-type", eventstream.StringValue("exception"))
	headers.Set(":exception-type", eventstream.StringValue(name))
	headers.Set(":content-type", eventstream.StringValue("application/json"))
	if err := eventstream.NewEncoder().Encode(&buf, eventstream.Message{Headers: headers, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// capture records what the wire actually sent.
type capture struct {
	Body    map[string]any
	Raw     []byte
	Headers http.Header
	Path    string
}

// serve starts a Bedrock stand-in returning body, and returns its URL plus the
// capture the request lands in.
func serve(t *testing.T, body []byte) (string, *capture) {
	t.Helper()
	return serveFunc(t, func(http.ResponseWriter) []byte { return body })
}

func serveFunc(t *testing.T, respond func(w http.ResponseWriter) []byte) (string, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			if _, err := r.Body.Read(raw); err != nil && len(raw) == 0 {
				t.Errorf("reading request body: %v", err)
			}
		}
		cap.Raw = raw
		cap.Headers = r.Header.Clone()
		cap.Path = r.URL.Path
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &cap.Body); err != nil {
				t.Errorf("request body is not JSON: %v\n%s", err, raw)
			}
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Header().Set("x-amzn-RequestId", "test-request-id")
		body := respond(w)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, cap
}

// testEnv supplies static credentials so the request is really SigV4 signed,
// without reaching the shared config files or instance metadata of whatever
// machine the tests run on.
func testEnv() map[string]string {
	return map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIDEXAMPLE",
		"AWS_SECRET_ACCESS_KEY": "secret",
		"AWS_REGION":            "us-east-1",
	}
}

func testModel(url string) *ai.Model {
	return &ai.Model{
		ID: "anthropic.claude-sonnet-5", Name: "Claude Sonnet 5",
		Api: ai.ApiBedrockConverse, Provider: "amazon-bedrock", BaseURL: url,
		Reasoning: true, Input: []string{"text", "image"},
		ContextWindow: 200000, MaxTokens: 8192,
		Cost: ai.ModelCost{ModelCostRates: ai.ModelCostRates{
			Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75,
		}},
	}
}

func userContext(text string) ai.Context {
	return ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: text}}}}
}

// collect drains a stream into its events and final message.
func collect(t *testing.T, stream *ai.MessageStream) ([]ai.Event, *ai.AssistantMessage) {
	t.Helper()
	var events []ai.Event
	for ev := range stream.Events() {
		events = append(events, ev)
	}
	return events, stream.Result()
}

// eventTypes reduces a stream to its event grammar.
func eventTypes(events []ai.Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, string(ev.Type))
	}
	return out
}
