package mistralconv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func modelFor(baseURL string) *ai.Model {
	return &ai.Model{
		ID: "mistral-large-latest", Name: "Mistral Large", Api: ai.ApiMistralConversations,
		Provider: "mistral", BaseURL: baseURL,
		Input: []string{"text", "image"}, ContextWindow: 128000, MaxTokens: 8192,
	}
}

func payloadFor(t *testing.T, model *ai.Model, c ai.Context, opts *Options) map[string]any {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	raw, err := json.Marshal(buildRequest(model, c, opts))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func simpleContext() ai.Context {
	return ai.Context{
		SystemPrompt: "be helpful",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}},
	}
}

func messages(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	raw, _ := payload["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

func assistantTurn(model *ai.Model, content ...ai.Content) ai.AssistantMessage {
	return ai.AssistantMessage{
		Content: content, Provider: model.Provider, Api: model.Api, Model: model.ID,
	}
}

// THE POINT: Mistral accepts tool-call ids of EXACTLY nine alphanumeric
// characters — not "up to". An id from any other provider is rejected, so it
// has to be rewritten rather than passed through, and the same original must
// always map to the same rewrite or a result pairs to the wrong call.
func TestToolCallIDsAreRewrittenToNineChars(t *testing.T) {
	n := newIDNormalizer()

	cases := []string{
		"call_abc123XYZ_long_openai_style",
		"toolu_01A2B3C4D5E6F7G8H9",
		"short",
		"",
		"has-dashes-and_underscores",
	}
	seen := map[string]string{}
	for _, id := range cases {
		got := n.normalize(id)
		if len(got) != toolCallIDLength {
			t.Errorf("normalize(%q) = %q, length %d, want %d", id, got, len(got), toolCallIDLength)
		}
		if nonAlphanumeric.MatchString(got) {
			t.Errorf("normalize(%q) = %q contains a non-alphanumeric", id, got)
		}
		if prior, ok := seen[got]; ok {
			t.Errorf("%q and %q both mapped to %q", prior, id, got)
		}
		seen[got] = id
	}
}

// The mapping has to be stable within a conversation, or the assistant's call
// and the tool's result get different ids.
func TestToolCallIDMappingIsStable(t *testing.T) {
	n := newIDNormalizer()
	first := n.normalize("call_abc")
	if second := n.normalize("call_abc"); second != first {
		t.Errorf("the same id mapped to %q then %q", first, second)
	}
}

// An id that is already the right shape passes through, so a Mistral-native
// conversation replays with its own ids intact.
func TestAlreadyValidIDsPassThrough(t *testing.T) {
	n := newIDNormalizer()
	if got := n.normalize("abc123XYZ"); got != "abc123XYZ" {
		t.Errorf("got %q, want the id unchanged", got)
	}
}

// A collision must be resolved rather than silently reused: two calls sharing
// an id would pair one call's result to the other.
func TestCollisionsAreResolved(t *testing.T) {
	n := newIDNormalizer()
	// Both normalize to the same nine characters before hashing.
	a := n.normalize("abc123XYZ")
	b := n.normalize("abc-123-XYZ")

	if a == b {
		t.Errorf("two distinct ids both became %q", a)
	}
	if len(b) != toolCallIDLength {
		t.Errorf("the resolved id is the wrong length: %q", b)
	}
}

// A tool call and its result must carry the same rewritten id, or the model
// cannot tell which call was answered.
func TestToolCallAndResultShareTheirID(t *testing.T) {
	model := modelFor("")
	c := simpleContext()
	c.Messages = append(c.Messages,
		assistantTurn(model, ai.ToolCall{ID: "call_original_long", Name: "read", Arguments: map[string]any{}}),
		ai.ToolResultMessage{ToolCallID: "call_original_long", ToolName: "read",
			Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	)

	msgs := messages(t, payloadFor(t, model, c, nil))

	var callID, resultID string
	for _, m := range msgs {
		if calls, ok := m["toolCalls"].([]any); ok && len(calls) > 0 {
			callID = calls[0].(map[string]any)["id"].(string)
		}
		if m["role"] == "tool" {
			resultID = m["toolCallId"].(string)
		}
	}
	if callID == "" || resultID == "" {
		t.Fatalf("missing ids: call=%q result=%q", callID, resultID)
	}
	if callID != resultID {
		t.Errorf("call id %q, result id %q — they must match", callID, resultID)
	}
}

// Field names are camelCase here, which is the whole reason this is not the
// chat-completions wire.
func TestFieldNamesAreCamelCase(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{{Name: "read", Description: "read a file"}}

	payload := payloadFor(t, modelFor(""), c, &Options{
		StreamOptions: ai.StreamOptions{MaxTokens: 100, SessionID: "s1"},
	})

	for _, want := range []string{"maxTokens", "promptCacheKey"} {
		if _, present := payload[want]; !present {
			t.Errorf("missing %q in %v", want, keysOf(payload))
		}
	}
	for _, unwanted := range []string{"max_tokens", "prompt_cache_key"} {
		if _, present := payload[unwanted]; present {
			t.Errorf("snake_case %q was sent", unwanted)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Thinking is a nested chunk type, not a sibling field.
func TestThinkingIsANestedChunk(t *testing.T) {
	model := modelFor("")
	c := simpleContext()
	c.Messages = append(c.Messages, assistantTurn(model,
		ai.ThinkingContent{Thinking: "weighing"}, ai.TextContent{Text: "the answer"}))

	msgs := messages(t, payloadFor(t, model, c, nil))
	assistant := msgs[len(msgs)-1]
	chunks := assistant["content"].([]any)

	thinking := chunks[0].(map[string]any)
	if thinking["type"] != "thinking" {
		t.Fatalf("first chunk: %#v", thinking)
	}
	nested, ok := thinking["thinking"].([]any)
	if !ok || len(nested) != 1 {
		t.Fatalf("thinking should nest its own text chunks: %#v", thinking)
	}
	if nested[0].(map[string]any)["text"] != "weighing" {
		t.Errorf("nested text: %#v", nested[0])
	}
}

// Reasoning is a mode plus an effort, and Mistral has only two settings — so
// every level tau offers maps onto the one that is on.
func TestReasoningIsAMode(t *testing.T) {
	model := modelFor("")
	model.Reasoning = true

	t.Run("on", func(t *testing.T) {
		payload := payloadFor(t, model, simpleContext(), &Options{Reasoning: "low"})
		if payload["promptMode"] != "reasoning" {
			t.Errorf("promptMode: %v", payload["promptMode"])
		}
		if payload["reasoningEffort"] != "high" {
			t.Errorf("reasoningEffort: %v", payload["reasoningEffort"])
		}
	})

	t.Run("off", func(t *testing.T) {
		payload := payloadFor(t, model, simpleContext(), nil)
		if _, present := payload["promptMode"]; present {
			t.Error("reasoning is off; the mode must not be sent")
		}
	})

	t.Run("a non-reasoning model never gets it", func(t *testing.T) {
		payload := payloadFor(t, modelFor(""), simpleContext(), &Options{Reasoning: "high"})
		if _, present := payload["promptMode"]; present {
			t.Error("a non-reasoning model was put into reasoning mode")
		}
	})
}

// A model that cannot see images gets a placeholder instead — the shared
// transform does the substitution, and what matters here is that the result
// still reaches the wire as a real turn rather than an empty one.
func TestImagesForANonVisionModel(t *testing.T) {
	model := modelFor("")
	model.Input = []string{"text"}

	c := simpleContext()
	c.Messages = ai.MessageList{ai.UserMessage{Content: ai.UserContent{
		Blocks: ai.ContentList{ai.ImageContent{MimeType: "image/png", Data: "AAAA"}},
	}}}

	msgs := messages(t, payloadFor(t, model, c, nil))
	last := msgs[len(msgs)-1]

	chunks, ok := last["content"].([]any)
	if !ok || len(chunks) == 0 {
		t.Fatalf("an image-only turn produced no content: %#v", last["content"])
	}
	text, _ := chunks[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "omitted") {
		t.Errorf("content: %#v", last["content"])
	}
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return collect(Stream(context.Background(), modelFor(srv.URL), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))
}

func TestStreamsText(t *testing.T) {
	body := chunkOf(`{"id":"r1","choices":[{"delta":{"content":"Hello"}}]}`) +
		chunkOf(`{"choices":[{"delta":{"content":" world"},"finishReason":"stop"}],`+
			`"usage":{"promptTokens":10,"completionTokens":5,"totalTokens":15}}`)

	types, msg := runStream(t, body)

	for _, want := range []ai.EventType{ai.EventTextStart, ai.EventTextDelta, ai.EventTextEnd, ai.EventDone} {
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

// THE POINT: content is a plain string on some turns and an array of typed
// chunks on others. Decoding only one shape drops half the stream.
func TestContentIsDecodedInBothShapes(t *testing.T) {
	body := chunkOf(`{"choices":[{"delta":{"content":"plain "}}]}`) +
		chunkOf(`{"choices":[{"delta":{"content":[{"type":"text","text":"chunked"}]}}]}`) +
		chunkOf(`{"choices":[{"delta":{},"finishReason":"stop"}]}`)

	_, msg := runStream(t, body)

	if text := msg.Content[0].(ai.TextContent).Text; text != "plain chunked" {
		t.Errorf("text: %q", text)
	}
}

// Thinking arrives as a nested chunk and has to become its own block.
func TestStreamsThinkingAsItsOwnBlock(t *testing.T) {
	body := chunkOf(`{"choices":[{"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"weighing"}]}]}}]}`) +
		chunkOf(`{"choices":[{"delta":{"content":[{"type":"text","text":"answer"}]}}]}`) +
		chunkOf(`{"choices":[{"delta":{},"finishReason":"stop"}]}`)

	types, msg := runStream(t, body)

	if len(msg.Content) != 2 {
		t.Fatalf("expected a thinking and a text block: %#v", msg.Content)
	}
	if thinking, ok := msg.Content[0].(ai.ThinkingContent); !ok || thinking.Thinking != "weighing" {
		t.Errorf("thinking block: %#v", msg.Content[0])
	}
	if text, ok := msg.Content[1].(ai.TextContent); !ok || text.Text != "answer" {
		t.Errorf("text block: %#v", msg.Content[1])
	}
	if !hasType(types, ai.EventThinkingEnd) {
		t.Error("the thinking block was never closed")
	}
}

// Tool arguments stream as fragments and settle into parsed arguments.
func TestStreamsAToolCall(t *testing.T) {
	body := chunkOf(`{"choices":[{"delta":{"toolCalls":[{"id":"abc123XYZ","index":0,`+
		`"function":{"name":"read","arguments":"{\"path\":"}}]}}]}`) +
		chunkOf(`{"choices":[{"delta":{"toolCalls":[{"id":"abc123XYZ","index":0,`+
			`"function":{"arguments":"\"a.go\"}"}}]}}]}`) +
		chunkOf(`{"choices":[{"delta":{},"finishReason":"tool_calls"}]}`)

	types, msg := runStream(t, body)

	for _, want := range []ai.EventType{ai.EventToolCallStart, ai.EventToolCallDelta, ai.EventToolCallEnd} {
		if !hasType(types, want) {
			t.Errorf("missing %s in %v", want, types)
		}
	}
	if msg.StopReason != ai.StopToolUse {
		t.Errorf("stop reason: %q", msg.StopReason)
	}
	call, ok := msg.Content[0].(ai.ToolCall)
	if !ok {
		t.Fatalf("content: %#v", msg.Content)
	}
	// One call, not two: the fragments belong to the same index.
	if len(msg.Content) != 1 {
		t.Errorf("fragments produced %d blocks", len(msg.Content))
	}
	if call.Arguments["path"] != "a.go" {
		t.Errorf("arguments: %#v", call.Arguments)
	}
}

// A call with no id has to be given one, or its result cannot be paired.
func TestToolCallWithoutAnIDGetsOne(t *testing.T) {
	body := chunkOf(`{"choices":[{"delta":{"toolCalls":[{"index":0,`+
		`"function":{"name":"read","arguments":"{}"}}]}}]}`) +
		chunkOf(`{"choices":[{"delta":{},"finishReason":"tool_calls"}]}`)

	_, msg := runStream(t, body)

	call := msg.Content[0].(ai.ToolCall)
	if len(call.ID) != toolCallIDLength {
		t.Errorf("id %q is not a valid mistral id", call.ID)
	}
}

// Cached tokens come out of the prompt count, and Mistral has published them
// under several names — each is read so a rename does not silently zero it.
func TestUsageReadsEveryCachedTokenSpelling(t *testing.T) {
	cases := []struct{ name, usage string }{
		{"camelCase details", `"promptTokensDetails":{"cachedTokens":800}`},
		{"snake_case details", `"prompt_tokens_details":{"cached_tokens":800}`},
		{"a flat count", `"numCachedTokens":800`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := chunkOf(`{"choices":[{"delta":{},"finishReason":"stop"}],` +
				`"usage":{"promptTokens":1000,"completionTokens":50,"totalTokens":1050,` + tc.usage + `}}`)

			_, msg := runStream(t, body)

			if msg.Usage.CacheRead != 800 {
				t.Errorf("cacheRead: %d", msg.Usage.CacheRead)
			}
			if msg.Usage.Input != 200 {
				t.Errorf("input: %d, want 200 (1000 - 800 cached)", msg.Usage.Input)
			}
		})
	}
}

// A cached count larger than the prompt would produce negative input.
func TestCachedTokensAreCapped(t *testing.T) {
	body := chunkOf(`{"choices":[{"delta":{},"finishReason":"stop"}],` +
		`"usage":{"promptTokens":100,"completionTokens":5,"numCachedTokens":500}}`)

	_, msg := runStream(t, body)

	if msg.Usage.Input < 0 {
		t.Errorf("input went negative: %d", msg.Usage.Input)
	}
	if msg.Usage.CacheRead > 100 {
		t.Errorf("cacheRead exceeded the prompt: %d", msg.Usage.CacheRead)
	}
}

// Mistral keys its prefix cache off this header; without it a conversation
// lands on a different replica each turn and re-reads the whole prompt.
func TestAffinityHeader(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(chunkOf(`{"choices":[{"delta":{},"finishReason":"stop"}]}`)))
	}))
	defer srv.Close()

	collect(Stream(context.Background(), modelFor(srv.URL), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k", SessionID: "sess-1"}}))
	if seen.Get("x-affinity") != "sess-1" {
		t.Errorf("x-affinity: %q", seen.Get("x-affinity"))
	}

	collect(Stream(context.Background(), modelFor(srv.URL), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k", SessionID: "sess-1", CacheRetention: ai.CacheNone}}))
	if seen.Get("x-affinity") != "" {
		t.Error("caching is off; the affinity header must not be sent")
	}
}

func TestChatURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.mistral.ai", "https://api.mistral.ai/v1/chat/completions"},
		{"https://api.mistral.ai/", "https://api.mistral.ai/v1/chat/completions"},
		{"https://api.mistral.ai/v1", "https://api.mistral.ai/v1/chat/completions"},
		{"https://proxy/v1/chat/completions", "https://proxy/v1/chat/completions"},
	}
	for _, tc := range cases {
		if got := chatURL(tc.in); got != tc.want {
			t.Errorf("chatURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A very long validation error in a terminal is unreadable, so it is capped —
// but the cap has to say that it happened.
func TestLongErrorsAreTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Repeat("x", maxErrorBody*2)))
	}))
	defer srv.Close()

	_, msg := collect(Stream(context.Background(), modelFor(srv.URL), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{APIKey: "k"}}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if len(msg.ErrorMessage) > maxErrorBody+200 {
		t.Errorf("the error was not truncated: %d chars", len(msg.ErrorMessage))
	}
	if !strings.Contains(msg.ErrorMessage, "truncated") {
		t.Error("truncation must be visible, or the message reads as complete")
	}
}

func TestMissingCredentialsFailCleanly(t *testing.T) {
	_, msg := collect(Stream(context.Background(),
		modelFor("https://example.invalid"), simpleContext(), &Options{}))

	if msg.StopReason != ai.StopError || !strings.Contains(msg.ErrorMessage, "API key") {
		t.Errorf("stop=%q error=%q", msg.StopReason, msg.ErrorMessage)
	}
}

// A stream that ends with no finish reason is truncated, not successful.
func TestNoFinishReasonIsAnError(t *testing.T) {
	_, msg := runStream(t, chunkOf(`{"choices":[{"delta":{"content":"partial"}}]}`))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "finish reason") {
		t.Errorf("error: %q", msg.ErrorMessage)
	}
}
