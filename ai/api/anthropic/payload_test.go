package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
)

const minimalSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

// capture runs one Stream call against a test server and returns the captured
// request headers and decoded JSON payload.
func capture(t *testing.T, model *ai.Model, c ai.Context, opts *Options) (http.Header, map[string]any, *ai.AssistantMessage) {
	t.Helper()
	var headers http.Header
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(minimalSSE))
	}))
	defer srv.Close()
	m := *model
	m.BaseURL = srv.URL
	s := Stream(context.Background(), &m, c, opts)
	final := s.Result()
	return headers, payload, final
}

func testModel() *ai.Model {
	return &ai.Model{
		ID:            "claude-test",
		Name:          "Claude Test",
		Api:           ai.ApiAnthropicMessages,
		Provider:      "anthropic",
		BaseURL:       "https://api.anthropic.com",
		Reasoning:     false,
		Input:         []string{"text"},
		Cost:          ai.ModelCost{ModelCostRates: ai.ModelCostRates{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}},
		ContextWindow: 200000,
		MaxTokens:     8192,
	}
}

func userCtx(text string) ai.Context {
	return ai.Context{
		SystemPrompt: "System prompt.",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: text}, Timestamp: 1}},
	}
}

func bashTool() ai.Tool {
	return ai.Tool{
		Name:        "bash",
		Description: "Run a command",
		Parameters: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"command": {Type: "string"}},
			Required:   []string{"command"},
		},
	}
}

func TestPayloadBasicTextWithCacheControl(t *testing.T) {
	headers, payload, final := capture(t, testModel(), userCtx("Hello"), &Options{StreamOptions: ai.StreamOptions{APIKey: "sk-ant-key"}})

	if got := headers.Get("x-api-key"); got != "sk-ant-key" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := headers.Get("authorization"); got != "" {
		t.Errorf("unexpected authorization header %q", got)
	}
	if got := headers.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	// Interleaved thinking beta on by default for non-adaptive models.
	if got := headers.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta = %q", got)
	}

	if payload["model"] != "claude-test" || payload["stream"] != true {
		t.Errorf("model/stream = %v/%v", payload["model"], payload["stream"])
	}
	if payload["max_tokens"] != float64(8192) {
		t.Errorf("max_tokens = %v", payload["max_tokens"])
	}
	system := payload["system"].([]any)
	sys0 := system[0].(map[string]any)
	if sys0["text"] != "System prompt." {
		t.Errorf("system text = %v", sys0["text"])
	}
	if cc := sys0["cache_control"].(map[string]any); cc["type"] != "ephemeral" || cc["ttl"] != nil {
		t.Errorf("system cache_control = %v", cc)
	}
	msgs := payload["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	// Last user message gets cache_control: string content wrapped into blocks.
	m0 := msgs[0].(map[string]any)
	blocks := m0["content"].([]any)
	b0 := blocks[0].(map[string]any)
	if b0["text"] != "Hello" || b0["cache_control"] == nil {
		t.Errorf("user block = %v", b0)
	}
	if final.StopReason != ai.StopStop {
		t.Errorf("final stop = %v", final.StopReason)
	}
}

func TestPayloadToolsEagerStreaming(t *testing.T) {
	c := userCtx("run ls")
	c.Tools = []ai.Tool{bashTool()}

	// Default: eager on, cache_control on last tool, no fine-grained beta.
	headers, payload, _ := capture(t, testModel(), c, &Options{StreamOptions: ai.StreamOptions{APIKey: "k"}})
	tools := payload["tools"].([]any)
	tool0 := tools[0].(map[string]any)
	if tool0["eager_input_streaming"] != true {
		t.Errorf("eager_input_streaming missing: %v", tool0)
	}
	if tool0["cache_control"] == nil {
		t.Errorf("tool cache_control missing")
	}
	schema := tool0["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["properties"] == nil || schema["required"] == nil {
		t.Errorf("input_schema = %v", schema)
	}
	if got := headers.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("beta = %q", got)
	}

	// Compat off: no eager field, fine-grained beta header present.
	m := testModel()
	f := false
	m.Compat = &ai.CompatFlags{SupportsEagerToolInputStreaming: &f}
	headers, payload, _ = capture(t, m, c, &Options{StreamOptions: ai.StreamOptions{APIKey: "k"}})
	tool0 = payload["tools"].([]any)[0].(map[string]any)
	if _, has := tool0["eager_input_streaming"]; has {
		t.Errorf("eager_input_streaming should be omitted")
	}
	if got := headers.Get("anthropic-beta"); got != "fine-grained-tool-streaming-2025-05-14,interleaved-thinking-2025-05-14" {
		t.Errorf("beta = %q", got)
	}
}

func TestPayloadThinkingBudget(t *testing.T) {
	m := testModel()
	m.Reasoning = true
	tr := true
	_, payload, _ := capture(t, m, userCtx("think"), &Options{
		StreamOptions:        ai.StreamOptions{APIKey: "k"},
		ThinkingEnabled:      &tr,
		ThinkingBudgetTokens: 8192,
	})
	thinking := payload["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(8192) || thinking["display"] != "summarized" {
		t.Errorf("thinking = %v", thinking)
	}
}

func TestPayloadThinkingDisabled(t *testing.T) {
	m := testModel()
	m.Reasoning = true
	f := false
	_, payload, _ := capture(t, m, userCtx("no think"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k"}, ThinkingEnabled: &f})
	thinking := payload["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Errorf("thinking = %v", thinking)
	}

	// off:null in the map marks "off" unsupported → omit thinking entirely.
	m.ThinkingLevelMap = ai.ThinkingLevelMap{"off": nil}
	_, payload, _ = capture(t, m, userCtx("no think"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k"}, ThinkingEnabled: &f})
	if _, has := payload["thinking"]; has {
		t.Errorf("thinking should be omitted when off is null: %v", payload["thinking"])
	}
}

func TestPayloadAdaptiveThinking(t *testing.T) {
	m := testModel()
	m.Reasoning = true
	tr := true
	m.Compat = &ai.CompatFlags{ForceAdaptiveThinking: &tr}
	headers, payload, _ := capture(t, m, userCtx("think"), &Options{
		StreamOptions:   ai.StreamOptions{APIKey: "k"},
		ThinkingEnabled: &tr,
		Effort:          "xhigh",
	})
	thinking := payload["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Errorf("thinking = %v", thinking)
	}
	if oc := payload["output_config"].(map[string]any); oc["effort"] != "xhigh" {
		t.Errorf("output_config = %v", payload["output_config"])
	}
	// Adaptive models skip the interleaved-thinking beta.
	if got := headers.Get("anthropic-beta"); got != "" {
		t.Errorf("beta = %q", got)
	}
}

func TestPayloadTemperatureCompat(t *testing.T) {
	temp := 0.7
	_, payload, _ := capture(t, testModel(), userCtx("t"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k", Temperature: &temp}})
	if payload["temperature"] != 0.7 {
		t.Errorf("temperature = %v", payload["temperature"])
	}

	m := testModel()
	f := false
	m.Compat = &ai.CompatFlags{SupportsTemperature: &f}
	_, payload, _ = capture(t, m, userCtx("t"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k", Temperature: &temp}})
	if _, has := payload["temperature"]; has {
		t.Errorf("temperature should be dropped: %v", payload["temperature"])
	}

	// Temperature also dropped when thinking is enabled.
	m2 := testModel()
	m2.Reasoning = true
	tr := true
	_, payload, _ = capture(t, m2, userCtx("t"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k", Temperature: &temp}, ThinkingEnabled: &tr})
	if _, has := payload["temperature"]; has {
		t.Errorf("temperature should be dropped with thinking: %v", payload["temperature"])
	}
}

func TestPayloadLongCacheRetention(t *testing.T) {
	_, payload, _ := capture(t, testModel(), userCtx("c"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k", CacheRetention: ai.CacheLong}})
	sys0 := payload["system"].([]any)[0].(map[string]any)
	cc := sys0["cache_control"].(map[string]any)
	if cc["ttl"] != "1h" {
		t.Errorf("cache_control = %v", cc)
	}

	// Gated off by compat.
	m := testModel()
	f := false
	m.Compat = &ai.CompatFlags{SupportsLongCacheRetention: &f}
	_, payload, _ = capture(t, m, userCtx("c"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k", CacheRetention: ai.CacheLong}})
	cc = payload["system"].([]any)[0].(map[string]any)["cache_control"].(map[string]any)
	if cc["ttl"] != nil {
		t.Errorf("ttl should be absent: %v", cc)
	}

	// none → no cache_control at all.
	_, payload, _ = capture(t, testModel(), userCtx("c"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k", CacheRetention: ai.CacheNone}})
	sys0 = payload["system"].([]any)[0].(map[string]any)
	if _, has := sys0["cache_control"]; has {
		t.Errorf("cache_control should be absent")
	}
}

func TestPayloadOAuthClaudeCode(t *testing.T) {
	c := userCtx("hi")
	c.Tools = []ai.Tool{bashTool()}
	headers, payload, _ := capture(t, testModel(), c, &Options{StreamOptions: ai.StreamOptions{APIKey: "sk-ant-oat01-token"}})

	if got := headers.Get("authorization"); got != "Bearer sk-ant-oat01-token" {
		t.Errorf("authorization = %q", got)
	}
	if got := headers.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key should be absent, got %q", got)
	}
	if got := headers.Get("anthropic-beta"); got != "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta = %q", got)
	}
	if got := headers.Get("user-agent"); got != "claude-cli/"+claudeCodeVersion {
		t.Errorf("user-agent = %q", got)
	}
	if got := headers.Get("x-app"); got != "cli" {
		t.Errorf("x-app = %q", got)
	}

	system := payload["system"].([]any)
	if len(system) != 2 {
		t.Fatalf("system = %v", system)
	}
	if system[0].(map[string]any)["text"] != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Errorf("system[0] = %v", system[0])
	}
	// Tool names canonicalized to Claude Code casing.
	tool0 := payload["tools"].([]any)[0].(map[string]any)
	if tool0["name"] != "Bash" {
		t.Errorf("tool name = %v", tool0["name"])
	}
}

func TestPayloadMetadataUserID(t *testing.T) {
	_, payload, _ := capture(t, testModel(), userCtx("m"), &Options{StreamOptions: ai.StreamOptions{APIKey: "k", Metadata: map[string]any{"user_id": "u-1", "other": "x"}}})
	md := payload["metadata"].(map[string]any)
	if md["user_id"] != "u-1" || len(md) != 1 {
		t.Errorf("metadata = %v", md)
	}
}

func TestPayloadHeaderOverrideAndDelete(t *testing.T) {
	nilVal := (*string)(nil)
	custom := "custom"
	headers, _, _ := capture(t, testModel(), userCtx("h"), &Options{StreamOptions: ai.StreamOptions{
		APIKey:  "k",
		Headers: map[string]*string{"x-custom": &custom, "anthropic-beta": nilVal},
	}})
	if got := headers.Get("x-custom"); got != "custom" {
		t.Errorf("x-custom = %q", got)
	}
	if got := headers.Get("anthropic-beta"); got != "" {
		t.Errorf("anthropic-beta should be deleted, got %q", got)
	}
}

func TestNoAPIKeyFailsAsErrorEvent(t *testing.T) {
	s := Stream(context.Background(), testModel(), userCtx("x"), &Options{})
	final := s.Result()
	if final.StopReason != ai.StopError || final.ErrorMessage != "No API key for provider: anthropic" {
		t.Errorf("final = %v %q", final.StopReason, final.ErrorMessage)
	}
}

func TestStreamSimpleThinkingLevels(t *testing.T) {
	m := testModel()
	m.Reasoning = true
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(minimalSSE))
	}))
	defer srv.Close()
	m.BaseURL = srv.URL

	// medium → budget 8192, max_tokens = model cap (no explicit caller cap).
	StreamSimple(context.Background(), m, userCtx("s"), &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
		Reasoning:     ai.ThinkingMedium,
	}).Result()
	thinking := payload["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(7168) {
		// model.MaxTokens 8192; budget medium 8192 > maxTokens-1024 → min(8192, 8192-1024)=7168
		t.Errorf("thinking = %v", thinking)
	}

	// No reasoning → thinking disabled.
	StreamSimple(context.Background(), m, userCtx("s"), &ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}}).Result()
	if got := payload["thinking"].(map[string]any)["type"]; got != "disabled" {
		t.Errorf("thinking = %v", got)
	}
}
