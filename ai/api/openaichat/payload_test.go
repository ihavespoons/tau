package openaichat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// modelFor builds a model for one provider/base-URL pair, which is all
// detectCompat gets to reason from.
func modelFor(provider ai.ProviderId, baseURL string) *ai.Model {
	return &ai.Model{
		ID: "test-model", Name: "Test", Api: ai.ApiOpenAICompletions,
		Provider: provider, BaseURL: baseURL,
		Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 4096,
	}
}

func reasoningModel(provider ai.ProviderId, baseURL string, levels ai.ThinkingLevelMap) *ai.Model {
	m := modelFor(provider, baseURL)
	m.Reasoning = true
	m.ThinkingLevelMap = levels
	return m
}

func strptr(s string) *string { return &s }

// payloadFor renders the request body as a generic map, the way a provider
// would see it.
func payloadFor(t *testing.T, model *ai.Model, c ai.Context, opts *Options) map[string]any {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	cm := resolveCompat(model)
	grammar, err := apishared.GrammarToolInputProperties(c.Tools, cm.SupportsOpenAIGrammarTools)
	if err != nil {
		t.Fatalf("resolving grammar tools: %v", err)
	}
	req, err := buildRequest(model, c, opts, cm, grammar)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshalling payload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding payload: %v", err)
	}
	return out
}

func simpleContext() ai.Context {
	return ai.Context{
		SystemPrompt: "be helpful",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}},
	}
}

// The compat table is where a porting error would hide, so each row is
// asserted against the provider it was written for.
func TestCompatDetection(t *testing.T) {
	cases := []struct {
		name     string
		provider ai.ProviderId
		baseURL  string
		check    func(*testing.T, compat)
	}{
		{
			name:     "openai proper gets the full standard feature set",
			provider: "openai", baseURL: "https://api.openai.com/v1",
			check: func(t *testing.T, c compat) {
				if !c.SupportsStore {
					t.Error("store should be supported")
				}
				if c.MaxTokensField != maxTokensCompletion {
					t.Errorf("max tokens field: %q", c.MaxTokensField)
				}
				if c.ThinkingFormat != thinkingOpenAI {
					t.Errorf("thinking format: %q", c.ThinkingFormat)
				}
				if !c.SupportsDeveloperRole {
					t.Error("developer role should be supported")
				}
			},
		},
		{
			name:     "groq is standard enough to keep the defaults",
			provider: "groq", baseURL: "https://api.groq.com/openai/v1",
			check: func(t *testing.T, c compat) {
				if c.MaxTokensField != maxTokensCompletion {
					t.Errorf("max tokens field: %q", c.MaxTokensField)
				}
				if !c.SupportsReasoningEffort {
					t.Error("reasoning_effort should be supported")
				}
			},
		},
		{
			name:     "xai rejects reasoning_effort and the store field",
			provider: "xai", baseURL: "https://api.x.ai/v1",
			check: func(t *testing.T, c compat) {
				if c.SupportsReasoningEffort {
					t.Error("grok does not take reasoning_effort")
				}
				if c.SupportsStore {
					t.Error("grok does not take store")
				}
			},
		},
		{
			name:     "deepseek has its own thinking dialect and replay requirement",
			provider: "deepseek", baseURL: "https://api.deepseek.com",
			check: func(t *testing.T, c compat) {
				if c.ThinkingFormat != thinkingDeepSeek {
					t.Errorf("thinking format: %q", c.ThinkingFormat)
				}
				if !c.RequiresReasoningContentOnAssistantMessages {
					t.Error("deepseek needs reasoning_content on replayed assistant turns")
				}
			},
		},
		{
			name:     "together uses legacy max_tokens and its own reasoning shape",
			provider: "together", baseURL: "https://api.together.ai/v1",
			check: func(t *testing.T, c compat) {
				if c.MaxTokensField != maxTokensLegacy {
					t.Errorf("max tokens field: %q", c.MaxTokensField)
				}
				if c.ThinkingFormat != thinkingTogether {
					t.Errorf("thinking format: %q", c.ThinkingFormat)
				}
				if c.SupportsStrictMode {
					t.Error("together rejects strict")
				}
				if c.SupportsLongCacheRetention {
					t.Error("together has no long cache retention")
				}
			},
		},
		{
			name:     "zai uses legacy max_tokens and the zai thinking object",
			provider: "zai", baseURL: "https://api.z.ai/v1",
			check: func(t *testing.T, c compat) {
				if c.ThinkingFormat != thinkingZai {
					t.Errorf("thinking format: %q", c.ThinkingFormat)
				}
				if c.MaxTokensField != maxTokensLegacy {
					t.Errorf("max tokens field: %q", c.MaxTokensField)
				}
			},
		},
		{
			name:     "openrouter routes reasoning through its own object",
			provider: "openrouter", baseURL: "https://openrouter.ai/api/v1",
			check: func(t *testing.T, c compat) {
				if c.ThinkingFormat != thinkingOpenRouter {
					t.Errorf("thinking format: %q", c.ThinkingFormat)
				}
				if c.SessionAffinityFormat != affinityOpenRouter {
					t.Errorf("affinity format: %q", c.SessionAffinityFormat)
				}
				if c.SupportsDeveloperRole {
					t.Error("openrouter defaults to the system role")
				}
			},
		},
		{
			name:     "moonshot rejects strict and reasoning_effort",
			provider: "moonshotai", baseURL: "https://api.moonshot.ai/v1",
			check: func(t *testing.T, c compat) {
				if c.SupportsStrictMode {
					t.Error("moonshot rejects strict")
				}
				if c.SupportsReasoningEffort {
					t.Error("moonshot rejects reasoning_effort")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, resolveCompat(modelFor(tc.provider, tc.baseURL)))
		})
	}
}

// An OpenRouter model whose id names an Anthropic upstream gets the developer
// role and Anthropic-style cache control — both keyed off the model id, not
// the provider alone.
func TestOpenRouterAnthropicModelSpecialCases(t *testing.T) {
	m := modelFor("openrouter", "https://openrouter.ai/api/v1")
	m.ID = "anthropic/claude-sonnet-4"
	c := resolveCompat(m)

	if !c.SupportsDeveloperRole {
		t.Error("an anthropic model via openrouter takes the developer role")
	}
	if c.CacheControlFormat != "anthropic" {
		t.Errorf("cache control format: %q", c.CacheControlFormat)
	}
}

// An explicit compat override always beats detection — that is the escape
// hatch a models.json edit relies on.
func TestExplicitCompatOverridesDetection(t *testing.T) {
	m := modelFor("together", "https://api.together.ai/v1")
	yes := true
	field := maxTokensCompletion
	m.Compat = &ai.CompatFlags{SupportsStrictMode: &yes, MaxTokensField: &field}

	c := resolveCompat(m)
	if !c.SupportsStrictMode {
		t.Error("explicit strict override was ignored")
	}
	if c.MaxTokensField != maxTokensCompletion {
		t.Errorf("explicit max-tokens override was ignored: %q", c.MaxTokensField)
	}
}

func TestBasicPayloadShape(t *testing.T) {
	p := payloadFor(t, modelFor("groq", "https://api.groq.com/openai/v1"), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{MaxTokens: 100}})

	if p["model"] != "test-model" {
		t.Errorf("model: %v", p["model"])
	}
	if p["stream"] != true {
		t.Error("stream must be requested")
	}
	if _, ok := p["max_completion_tokens"]; !ok {
		t.Errorf("expected max_completion_tokens, got %v", keys(p))
	}
	msgs := p["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected system + user, got %d", len(msgs))
	}
	if role := msgs[0].(map[string]any)["role"]; role != "system" {
		t.Errorf("non-reasoning model should use the system role, got %v", role)
	}
}

// A reasoning model gets the developer role where the provider takes it, and
// the system role where it does not — sending the wrong one is a hard error on
// several providers.
func TestSystemPromptRoleFollowsCompat(t *testing.T) {
	levels := ai.ThinkingLevelMap{"high": strptr("high")}

	openai := payloadFor(t, reasoningModel("openai", "https://api.openai.com/v1", levels), simpleContext(), nil)
	if role := firstMessageRole(openai); role != "developer" {
		t.Errorf("openai reasoning model should use developer, got %q", role)
	}

	together := payloadFor(t, reasoningModel("together", "https://api.together.ai/v1", levels), simpleContext(), nil)
	if role := firstMessageRole(together); role != "system" {
		t.Errorf("together should stay on system, got %q", role)
	}
}

func firstMessageRole(p map[string]any) string {
	msgs, _ := p["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	role, _ := msgs[0].(map[string]any)["role"].(string)
	return role
}

// Each provider expresses "think harder" differently. Getting the dialect
// wrong means either an ignored request or a rejected one.
func TestThinkingDialects(t *testing.T) {
	levels := ai.ThinkingLevelMap{
		"off":  strptr("none"),
		"high": strptr("high"),
	}
	opts := &Options{Reasoning: "high"}

	cases := []struct {
		name     string
		provider ai.ProviderId
		baseURL  string
		check    func(*testing.T, map[string]any)
	}{
		{
			name: "openai sends reasoning_effort", provider: "openai", baseURL: "https://api.openai.com/v1",
			check: func(t *testing.T, p map[string]any) {
				if p["reasoning_effort"] != "high" {
					t.Errorf("reasoning_effort: %v", p["reasoning_effort"])
				}
			},
		},
		{
			name: "openrouter nests effort under reasoning", provider: "openrouter", baseURL: "https://openrouter.ai/api/v1",
			check: func(t *testing.T, p map[string]any) {
				r, ok := p["reasoning"].(map[string]any)
				if !ok || r["effort"] != "high" {
					t.Errorf("reasoning: %v", p["reasoning"])
				}
				if _, leaked := p["reasoning_effort"]; leaked {
					t.Error("openrouter must not also get a top-level reasoning_effort")
				}
			},
		},
		{
			name: "deepseek sends a thinking object plus effort", provider: "deepseek", baseURL: "https://api.deepseek.com",
			check: func(t *testing.T, p map[string]any) {
				th, ok := p["thinking"].(map[string]any)
				if !ok || th["type"] != "enabled" {
					t.Errorf("thinking: %v", p["thinking"])
				}
				if p["reasoning_effort"] != "high" {
					t.Errorf("reasoning_effort: %v", p["reasoning_effort"])
				}
			},
		},
		{
			name: "together sends reasoning.enabled and withholds effort", provider: "together", baseURL: "https://api.together.ai/v1",
			check: func(t *testing.T, p map[string]any) {
				r, ok := p["reasoning"].(map[string]any)
				if !ok || r["enabled"] != true {
					t.Errorf("reasoning: %v", p["reasoning"])
				}
				// together is detected as not supporting reasoning_effort.
				if _, leaked := p["reasoning_effort"]; leaked {
					t.Error("together does not take reasoning_effort")
				}
			},
		},
		{
			name: "zai sends its own thinking object", provider: "zai", baseURL: "https://api.z.ai/v1",
			check: func(t *testing.T, p map[string]any) {
				th, ok := p["thinking"].(map[string]any)
				if !ok || th["type"] != "enabled" || th["clear_thinking"] != false {
					t.Errorf("thinking: %v", p["thinking"])
				}
			},
		},
		{
			name: "xai gets no reasoning field at all", provider: "xai", baseURL: "https://api.x.ai/v1",
			check: func(t *testing.T, p map[string]any) {
				for _, k := range []string{"reasoning", "reasoning_effort", "thinking"} {
					if _, leaked := p[k]; leaked {
						t.Errorf("grok must not receive %q", k)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, payloadFor(t, reasoningModel(tc.provider, tc.baseURL, levels), simpleContext(), opts))
		})
	}
}

// Turning thinking off has to be expressible too, and a model that cannot be
// turned off must not be told to.
func TestThinkingOff(t *testing.T) {
	offable := ai.ThinkingLevelMap{"off": strptr("none"), "high": strptr("high")}
	p := payloadFor(t, reasoningModel("openrouter", "https://openrouter.ai/api/v1", offable), simpleContext(), &Options{})
	r, ok := p["reasoning"].(map[string]any)
	if !ok || r["effort"] != "none" {
		t.Errorf("expected reasoning effort none, got %v", p["reasoning"])
	}

	// A nil "off" entry means the model cannot stop thinking.
	alwaysOn := ai.ThinkingLevelMap{"off": nil, "high": strptr("high")}
	p = payloadFor(t, reasoningModel("openrouter", "https://openrouter.ai/api/v1", alwaysOn), simpleContext(), &Options{})
	if _, leaked := p["reasoning"]; leaked {
		t.Errorf("a model that cannot disable thinking must not be told to: %v", p["reasoning"])
	}
}

// A non-reasoning model never receives a reasoning field, whatever is asked.
func TestNonReasoningModelNeverGetsThinking(t *testing.T) {
	p := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), simpleContext(),
		&Options{Reasoning: "high"})
	for _, k := range []string{"reasoning", "reasoning_effort", "thinking"} {
		if _, leaked := p[k]; leaked {
			t.Errorf("non-reasoning model received %q", k)
		}
	}
}

func TestToolsCarryStrictOnlyWhereSupported(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{{Name: "read", Description: "read a file"}}

	supported := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), c, nil)
	fn := supported["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, ok := fn["strict"]; !ok {
		t.Error("openai should receive strict")
	}

	unsupported := payloadFor(t, modelFor("together", "https://api.together.ai/v1"), c, nil)
	fn = unsupported["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, ok := fn["strict"]; ok {
		t.Error("together rejects unknown fields, so strict must be withheld")
	}
}

// Anthropic behind a proxy rejects a transcript containing tool calls unless
// the tools field is present, even with nothing in it.
func TestToolHistoryForcesAnEmptyToolsField(t *testing.T) {
	c := simpleContext()
	c.Messages = append(c.Messages,
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "1", Name: "read"}}},
		ai.ToolResultMessage{ToolCallID: "1", ToolName: "read",
			Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	)

	p := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), c, nil)
	tools, present := p["tools"]
	if !present {
		t.Fatal("a transcript with tool calls must still send a tools field")
	}
	if len(tools.([]any)) != 0 {
		t.Errorf("expected an empty tools array, got %v", tools)
	}
}

func TestMaxTokensFieldFollowsCompat(t *testing.T) {
	opts := &Options{StreamOptions: ai.StreamOptions{MaxTokens: 512}}

	modern := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), simpleContext(), opts)
	if modern["max_completion_tokens"] != float64(512) {
		t.Errorf("expected max_completion_tokens, got %v", keys(modern))
	}
	if _, leaked := modern["max_tokens"]; leaked {
		t.Error("both max-token fields were sent")
	}

	legacy := payloadFor(t, modelFor("together", "https://api.together.ai/v1"), simpleContext(), opts)
	if legacy["max_tokens"] != float64(512) {
		t.Errorf("expected max_tokens, got %v", keys(legacy))
	}
}

// DeepSeek rejects a replayed assistant turn that omits reasoning_content once
// reasoning is on, even when there was no reasoning to replay.
func TestDeepSeekAssistantReplayCarriesReasoningContent(t *testing.T) {
	levels := ai.ThinkingLevelMap{"high": strptr("high")}
	c := simpleContext()
	c.Messages = append(c.Messages,
		ai.AssistantMessage{
			Content:  ai.ContentList{ai.TextContent{Text: "an answer"}},
			Provider: "deepseek", Api: ai.ApiOpenAICompletions, Model: "test-model",
		},
		ai.UserMessage{Content: ai.UserContent{Text: "and again"}},
	)

	p := payloadFor(t, reasoningModel("deepseek", "https://api.deepseek.com", levels), c, &Options{Reasoning: "high"})
	for _, raw := range p["messages"].([]any) {
		m := raw.(map[string]any)
		if m["role"] != "assistant" {
			continue
		}
		if _, ok := m["reasoning_content"]; !ok {
			t.Errorf("assistant replay is missing reasoning_content: %v", m)
		}
	}
}

// An assistant turn that produced nothing at all — an aborted run — must be
// dropped, because providers reject a message with neither content nor calls.
func TestEmptyAssistantTurnIsDropped(t *testing.T) {
	c := simpleContext()
	c.Messages = append(c.Messages,
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "   "}}},
		ai.UserMessage{Content: ai.UserContent{Text: "again"}},
	)

	p := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), c, nil)
	for _, raw := range p["messages"].([]any) {
		if raw.(map[string]any)["role"] == "assistant" {
			t.Errorf("an empty assistant turn was sent: %v", raw)
		}
	}
}

// Tool results are their own role, and empty output still needs a body.
func TestToolResultsBecomeToolMessages(t *testing.T) {
	c := simpleContext()
	c.Messages = append(c.Messages,
		ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "call_1", Name: "read"}}},
		ai.ToolResultMessage{ToolCallID: "call_1", ToolName: "read", Content: ai.ContentList{}},
	)

	p := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), c, nil)
	found := false
	for _, raw := range p["messages"].([]any) {
		m := raw.(map[string]any)
		if m["role"] != "tool" {
			continue
		}
		found = true
		if m["tool_call_id"] != "call_1" {
			t.Errorf("tool_call_id: %v", m["tool_call_id"])
		}
		if m["content"] != "(no tool output)" {
			t.Errorf("silent tool output needs a placeholder, got %v", m["content"])
		}
	}
	if !found {
		t.Error("no tool message was produced")
	}
}

// A responses-API id must be reshaped before it can be replayed here, and two
// calls sharing a call_id must stay distinct.
func TestToolCallIDNormalization(t *testing.T) {
	short := normalizeToolCallID("call_abc", "openai")
	if short != "call_abc" {
		t.Errorf("a plain id should pass through, got %q", short)
	}

	long := normalizeToolCallID(strings.Repeat("x", 60), "openai")
	if len(long) != maxToolCallID {
		t.Errorf("an over-long openai id should be clipped to %d, got %d", maxToolCallID, len(long))
	}

	a := normalizeToolCallID("call_1|item_aaa", "github-copilot")
	b := normalizeToolCallID("call_1|item_bbb", "github-copilot")
	if a == b {
		t.Error("two calls sharing a call_id must not collapse to one id")
	}
	for _, id := range []string{a, b} {
		if len(id) > maxToolCallID {
			t.Errorf("normalized id is too long: %q", id)
		}
		if nonIDChars.MatchString(id) {
			t.Errorf("normalized id has illegal characters: %q", id)
		}
	}

	// A pair too long to keep whole still has to come out unique and legal.
	huge := normalizeToolCallID("call_"+strings.Repeat("y", 30)+"|"+strings.Repeat("z", 400), "opencode")
	if len(huge) > maxToolCallID {
		t.Errorf("hashed id is too long: %q", huge)
	}
}

func TestCachePolicy(t *testing.T) {
	opts := &Options{StreamOptions: ai.StreamOptions{SessionID: "session-abc"}}

	openai := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), simpleContext(), opts)
	if openai["prompt_cache_key"] != "session-abc" {
		t.Errorf("openai should key its cache off the session: %v", openai["prompt_cache_key"])
	}

	// A provider without long retention gets neither field on a short default.
	groq := payloadFor(t, modelFor("groq", "https://api.groq.com/openai/v1"), simpleContext(), opts)
	if _, leaked := groq["prompt_cache_key"]; leaked {
		t.Error("a short-retention provider should not get a cache key")
	}

	long := &Options{StreamOptions: ai.StreamOptions{SessionID: "s", CacheRetention: ai.CacheLong}}
	p := payloadFor(t, modelFor("groq", "https://api.groq.com/openai/v1"), simpleContext(), long)
	if p["prompt_cache_retention"] != "24h" {
		t.Errorf("long retention should be requested: %v", p["prompt_cache_retention"])
	}

	// together is detected as having no long retention at all.
	tp := payloadFor(t, modelFor("together", "https://api.together.ai/v1"), simpleContext(), long)
	if _, leaked := tp["prompt_cache_retention"]; leaked {
		t.Error("together does not support long retention")
	}
}

func TestCacheRetentionFromEnvironment(t *testing.T) {
	if got := resolveCacheRetention("", nil); got != ai.CacheShort {
		t.Errorf("default should be short, got %q", got)
	}
	if got := resolveCacheRetention("", map[string]string{"PI_CACHE_RETENTION": "long"}); got != ai.CacheLong {
		t.Errorf("PI_ prefix should be honored for migrating users, got %q", got)
	}
	if got := resolveCacheRetention("", map[string]string{"TAU_CACHE_RETENTION": "long"}); got != ai.CacheLong {
		t.Errorf("TAU_ prefix should be honored, got %q", got)
	}
	if got := resolveCacheRetention(ai.CacheNone, map[string]string{"TAU_CACHE_RETENTION": "long"}); got != ai.CacheNone {
		t.Errorf("an explicit setting should win, got %q", got)
	}
}

// Images are inlined as data URLs, which is the only form this API accepts.
func TestImagesBecomeDataURLs(t *testing.T) {
	m := modelFor("openai", "https://api.openai.com/v1")
	m.Input = []string{"text", "image"}

	c := ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{
		Blocks: ai.ContentList{
			ai.TextContent{Text: "look"},
			ai.ImageContent{Data: "AAAA", MimeType: "image/png"},
		},
	}}}}

	p := payloadFor(t, m, c, nil)
	parts := p["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("expected text + image, got %v", parts)
	}
	img := parts[1].(map[string]any)["image_url"].(map[string]any)
	if img["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image url: %v", img["url"])
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
