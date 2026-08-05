package bedrock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/ihavespoons/tau/ai"
)

// THE POINT: this is the only test that proves the wire works end to end. The
// request is serialized and SigV4-signed by the real SDK, arrives at a server
// that decodes it as JSON, and the response is real EventStream framing decoded
// by the real deserializer. Nothing between tau and the network is stubbed.
func TestATextTurnRoundTrips(t *testing.T) {
	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(),
		textDelta(0, "Hello"),
		textDelta(0, ", world"),
		blockStop(0),
		messageStop("end_turn"),
		metadata(map[string]any{
			"inputTokens": 12, "outputTokens": 5, "totalTokens": 17,
			"cacheReadInputTokens": 3, "cacheWriteInputTokens": 2,
		}),
	}))

	model := testModel(url)
	events, msg := collect(t, Stream(context.Background(), model, userContext("hi"),
		&Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))

	if msg.StopReason != ai.StopStop {
		t.Fatalf("stop reason %q, error: %q", msg.StopReason, msg.ErrorMessage)
	}
	if got := text(msg); got != "Hello, world" {
		t.Errorf("text %q", got)
	}
	want := []string{"start", "text_start", "text_delta", "text_delta", "text_end", "done"}
	if got := eventTypes(events); !equal(got, want) {
		t.Errorf("event grammar:\n got %v\nwant %v", got, want)
	}

	// Usage must be split the way tau accounts for it: cached input is not
	// billed as input, and a wrong split silently misreports every cost.
	if msg.Usage.Input != 12 || msg.Usage.Output != 5 ||
		msg.Usage.CacheRead != 3 || msg.Usage.CacheWrite != 2 {
		t.Errorf("usage: %+v", msg.Usage)
	}
	if msg.Usage.Cost.Total == 0 {
		t.Error("cost was never calculated")
	}

	// The request must have reached the model's own path, signed.
	if !strings.Contains(cap.Path, "anthropic.claude-sonnet-5") {
		t.Errorf("path %q", cap.Path)
	}
	if auth := cap.Headers.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("request was not SigV4 signed: %q", auth)
	}
}

// THE POINT: a tool call arrives across several deltas and its arguments are
// only valid JSON once the last one lands. Parsing at the end is what makes the
// call usable; salvaging partial JSON on the way is what makes it watchable.
func TestAToolCallAccumulatesItsArguments(t *testing.T) {
	url, _ := serve(t, encodeFrames(t, []frame{
		messageStart(),
		toolUseStart(0, "call-1", "read_file"),
		toolUseDelta(0, `{"path":`),
		toolUseDelta(0, `"/tmp/x"}`),
		blockStop(0),
		messageStop("tool_use"),
	}))

	events, msg := collect(t, Stream(context.Background(), testModel(url), userContext("read it"),
		&Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))

	if msg.StopReason != ai.StopToolUse {
		t.Fatalf("stop reason %q, error: %q", msg.StopReason, msg.ErrorMessage)
	}
	call, ok := msg.Content[0].(ai.ToolCall)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if call.ID != "call-1" || call.Name != "read_file" {
		t.Errorf("call %+v", call)
	}
	if call.Arguments["path"] != "/tmp/x" {
		t.Errorf("arguments %v", call.Arguments)
	}

	want := []string{"start", "toolcall_start", "toolcall_delta", "toolcall_delta", "toolcall_end", "done"}
	if got := eventTypes(events); !equal(got, want) {
		t.Errorf("event grammar:\n got %v\nwant %v", got, want)
	}
	// The terminal event must carry the finished call, not a partial one.
	last := events[len(events)-2]
	if last.ToolCall == nil || last.ToolCall.Arguments["path"] != "/tmp/x" {
		t.Errorf("toolcall_end carried %+v", last.ToolCall)
	}
}

// THE POINT: the Go SDK splits reasoning into a union, so text and signature
// arrive as separate events where the JS SDK puts both on one object. The
// signature must accumulate onto the same block without being pushed as a
// visible delta — it is an opaque handle, and showing it would be noise.
func TestReasoningTextAndSignatureLandOnOneBlock(t *testing.T) {
	url, _ := serve(t, encodeFrames(t, []frame{
		messageStart(),
		reasoningTextDelta(0, "thinking "),
		reasoningTextDelta(0, "hard"),
		reasoningSignatureDelta(0, "sig-part-1"),
		reasoningSignatureDelta(0, "sig-part-2"),
		blockStop(0),
		textDelta(1, "answer"),
		blockStop(1),
		messageStop("end_turn"),
	}))

	events, msg := collect(t, Stream(context.Background(), testModel(url), userContext("think"),
		&Options{StreamOptions: ai.StreamOptions{Env: testEnv()}, Reasoning: ai.ThinkingHigh}))

	if len(msg.Content) != 2 {
		t.Fatalf("content: %+v", msg.Content)
	}
	thinking, ok := msg.Content[0].(ai.ThinkingContent)
	if !ok {
		t.Fatalf("content[0] is %T", msg.Content[0])
	}
	if thinking.Thinking != "thinking hard" {
		t.Errorf("thinking %q", thinking.Thinking)
	}
	if thinking.ThinkingSignature != "sig-part-1sig-part-2" {
		t.Errorf("signature %q — it must accumulate, not overwrite", thinking.ThinkingSignature)
	}

	// Two text deltas, not four: the signature deltas must not surface.
	want := []string{
		"start", "thinking_start", "thinking_delta", "thinking_delta", "thinking_end",
		"text_start", "text_delta", "text_end", "done",
	}
	if got := eventTypes(events); !equal(got, want) {
		t.Errorf("event grammar:\n got %v\nwant %v", got, want)
	}
}

// THE POINT: Converse indexes content by a wire index that is not the position
// in the message, and text blocks get no start event at all. Getting the
// mapping wrong appends deltas to the wrong block.
func TestInterleavedBlocksKeepTheirOwnIndexes(t *testing.T) {
	url, _ := serve(t, encodeFrames(t, []frame{
		messageStart(),
		textDelta(0, "before "),
		toolUseStart(1, "call-a", "alpha"),
		toolUseDelta(1, `{"a":1}`),
		textDelta(0, "after"),
		blockStop(1),
		blockStop(0),
		messageStop("tool_use"),
	}))

	_, msg := collect(t, Stream(context.Background(), testModel(url), userContext("go"),
		&Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))

	if len(msg.Content) != 2 {
		t.Fatalf("content: %+v", msg.Content)
	}
	if got := text(msg); got != "before after" {
		t.Errorf("text %q — a delta landed on the wrong block", got)
	}
	call, ok := msg.Content[1].(ai.ToolCall)
	if !ok || call.Name != "alpha" {
		t.Fatalf("content[1] is %+v", msg.Content[1])
	}
}

// A modelled exception arrives on the stream rather than as an HTTP error, and
// the channel closes normally when it does. Only checking Err() distinguishes a
// failed turn from a complete one.
func TestAStreamExceptionBecomesATerminalError(t *testing.T) {
	body := append(encodeFrames(t, []frame{messageStart(), textDelta(0, "partial")}),
		encodeException(t, "throttlingException", "Too many requests")...)
	url, _ := serve(t, body)

	_, msg := collect(t, Stream(context.Background(), testModel(url), userContext("hi"),
		&Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason %q — an exception must not look like a clean finish", msg.StopReason)
	}
	// The prefix is what the retry logic above this layer matches on.
	if !strings.Contains(msg.ErrorMessage, "Throttling error") {
		t.Errorf("error message %q", msg.ErrorMessage)
	}
}

// A stream that ends before the message starts is not an empty answer, it is a
// broken one, and reporting it as success would hand the agent a blank turn.
func TestATruncatedStreamFails(t *testing.T) {
	url, _ := serve(t, encodeFrames(t, []frame{}))

	_, msg := collect(t, Stream(context.Background(), testModel(url), userContext("hi"),
		&Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "before the message began") {
		t.Errorf("error message %q", msg.ErrorMessage)
	}
}

// A cancelled context must produce an aborted turn, not an error one: the agent
// treats them differently, and an abort the user asked for is not a failure.
func TestACancelledRequestAborts(t *testing.T) {
	url, _ := serve(t, encodeFrames(t, []frame{messageStart(), textDelta(0, "hi"), blockStop(0), messageStop("end_turn")}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, msg := collect(t, Stream(ctx, testModel(url), userContext("hi"),
		&Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))

	if msg.StopReason != ai.StopAborted {
		t.Errorf("stop reason %q, want aborted", msg.StopReason)
	}
}

// THE POINT: this pins the request Bedrock actually receives. Every assertion
// here is a field the service validates, and a silent rename would otherwise
// only show up against the real API.
func TestTheRequestPayloadMatchesConverse(t *testing.T) {
	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(), textDelta(0, "ok"), blockStop(0), messageStop("end_turn"),
	}))

	model := testModel(url)
	schema := &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"path": {Type: "string"}},
	}
	c := ai.Context{
		SystemPrompt: "be brief",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hello"}}},
		Tools:        []ai.Tool{{Name: "read_file", Description: "read a file", Parameters: schema}},
	}

	temp := 0.5
	_, msg := collect(t, Stream(context.Background(), model, c, &Options{
		StreamOptions: ai.StreamOptions{Env: testEnv(), MaxTokens: 4096, Temperature: &temp},
		ToolChoice:    ToolChoice{Type: ToolChoiceAuto},
	}))
	if msg.StopReason != ai.StopStop {
		t.Fatalf("stop reason %q: %s", msg.StopReason, msg.ErrorMessage)
	}

	body := cap.Body
	if body == nil {
		t.Fatal("no request body was captured")
	}

	// The model id travels in the URL path, not the body.
	if _, present := body["modelId"]; present {
		t.Error("modelId must not be in the body")
	}

	// The system prompt, then a cache breakpoint: retention defaults to short,
	// so caching is on unless the caller turns it off. A default of "none"
	// would silently pay full price for every repeated prefix.
	system, _ := body["system"].([]any)
	if len(system) != 2 {
		t.Fatalf("system: %v", body["system"])
	}
	if got := system[0].(map[string]any)["text"]; got != "be brief" {
		t.Errorf("system text %v", got)
	}
	shortPoint, _ := system[1].(map[string]any)["cachePoint"].(map[string]any)
	if shortPoint["type"] != "default" {
		t.Errorf("system cache point %v", shortPoint)
	}
	if ttl, present := shortPoint["ttl"]; present {
		t.Errorf("short retention must not send a ttl, got %v", ttl)
	}

	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages: %v", body["messages"])
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" {
		t.Errorf("role %v", first["role"])
	}

	inference, _ := body["inferenceConfig"].(map[string]any)
	if inference["maxTokens"] != float64(4096) {
		t.Errorf("maxTokens %v", inference["maxTokens"])
	}
	if inference["temperature"] != 0.5 {
		t.Errorf("temperature %v", inference["temperature"])
	}

	toolConfig, _ := body["toolConfig"].(map[string]any)
	tools, _ := toolConfig["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %v", toolConfig["tools"])
	}
	spec, _ := tools[0].(map[string]any)["toolSpec"].(map[string]any)
	if spec["name"] != "read_file" || spec["description"] != "read a file" {
		t.Errorf("toolSpec %v", spec)
	}
	if _, ok := spec["inputSchema"].(map[string]any)["json"]; !ok {
		t.Errorf("inputSchema %v", spec["inputSchema"])
	}
	// strict must be absent unless the tool asked for constrained sampling.
	if _, present := spec["strict"]; present {
		t.Errorf("strict was sent for a tool that did not request it: %v", spec)
	}
	if _, ok := toolConfig["toolChoice"].(map[string]any)["auto"]; !ok {
		t.Errorf("toolChoice %v", toolConfig["toolChoice"])
	}
}

// THE POINT: cache breakpoints are how prompt caching happens at all, and the
// trailing one has to sit on the last message so it covers the whole prefix.
func TestCacheBreakpointsAreSentForClaude(t *testing.T) {
	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(), textDelta(0, "ok"), blockStop(0), messageStop("end_turn"),
	}))

	_, _ = collect(t, Stream(context.Background(), testModel(url), ai.Context{
		SystemPrompt: "system",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hello"}}},
	}, &Options{StreamOptions: ai.StreamOptions{Env: testEnv(), CacheRetention: ai.CacheLong}}))

	system, _ := cap.Body["system"].([]any)
	if len(system) != 2 {
		t.Fatalf("system blocks: %v", cap.Body["system"])
	}
	point, _ := system[1].(map[string]any)["cachePoint"].(map[string]any)
	if point["type"] != "default" {
		t.Errorf("system cache point %v", point)
	}
	// Long retention is a different price and a different TTL; sending the
	// default silently gives five-minute caching at one-hour cost.
	if point["ttl"] != "1h" {
		t.Errorf("ttl %v, want 1h", point["ttl"])
	}

	messages, _ := cap.Body["messages"].([]any)
	content, _ := messages[len(messages)-1].(map[string]any)["content"].([]any)
	if _, ok := content[len(content)-1].(map[string]any)["cachePoint"]; !ok {
		t.Errorf("the last message carries no cache point: %v", content)
	}
}

// A model that caches automatically rejects an explicit cache point, so a wrong
// answer here is a request error rather than a lost optimization.
func TestNoCacheBreakpointsForNova(t *testing.T) {
	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(), textDelta(0, "ok"), blockStop(0), messageStop("end_turn"),
	}))

	model := testModel(url)
	model.ID, model.Name = "amazon.nova-pro-v1:0", "Nova Pro"

	_, _ = collect(t, Stream(context.Background(), model, ai.Context{
		SystemPrompt: "system",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hello"}}},
	}, &Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))

	if raw, _ := json.Marshal(cap.Body); strings.Contains(string(raw), "cachePoint") {
		t.Errorf("a cache point was sent to Nova: %s", raw)
	}
}

func text(msg *ai.AssistantMessage) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.(ai.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func equal(a, b []string) bool {
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
