package openairesp

import (
	"encoding/json"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func modelFor(provider ai.ProviderId, baseURL string) *ai.Model {
	return &ai.Model{
		ID: "gpt-5.4", Name: "Test", Api: ai.ApiOpenAIResponses,
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
func boolptr(b bool) *bool    { return &b }

// payloadFor renders the request body the way the endpoint would see it.
func payloadFor(t *testing.T, model *ai.Model, c ai.Context, opts *Options) map[string]any {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	raw, err := json.Marshal(buildRequest(model, c, opts, resolveCompat(model)))
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

// items returns the input array as maps.
func items(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	raw, ok := payload["input"].([]any)
	if !ok {
		t.Fatalf("input is not an array: %#v", payload["input"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("input item is not an object: %#v", it)
		}
		out = append(out, m)
	}
	return out
}

// The store flag must be present and false. tau keeps its own transcript, and
// omitting the field lets the endpoint keep one too — which would make a
// session's history depend on server state tau cannot see or delete.
func TestStoreIsAlwaysSentAsFalse(t *testing.T) {
	payload := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), simpleContext(), nil)

	store, present := payload["store"]
	if !present {
		t.Fatal("store must be sent explicitly, not omitted")
	}
	if store != false {
		t.Errorf("store: %#v", store)
	}
}

// A reasoning model takes the system prompt in the developer role, unless the
// host does not understand it.
func TestSystemPromptRole(t *testing.T) {
	cases := []struct {
		name  string
		model *ai.Model
		want  string
	}{
		{"a reasoning model gets developer", reasoningModel("openai", "https://api.openai.com/v1", nil), "developer"},
		{"a non-reasoning model gets system", modelFor("openai", "https://api.openai.com/v1"), "system"},
		{"a host that rejects developer gets system", func() *ai.Model {
			m := reasoningModel("openai", "https://api.openai.com/v1", nil)
			m.Compat = &ai.CompatFlags{SupportsDeveloperRole: boolptr(false)}
			return m
		}(), "system"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := payloadFor(t, tc.model, simpleContext(), nil)
			if role := items(t, payload)[0]["role"]; role != tc.want {
				t.Errorf("role: %v, want %q", role, tc.want)
			}
		})
	}
}

// Asking for reasoning without asking for the encrypted payload produces
// reasoning items tau cannot replay, and the model loses its train of thought
// on the very next turn.
func TestReasoningRequestsTheEncryptedPayload(t *testing.T) {
	model := reasoningModel("openai", "https://api.openai.com/v1", nil)
	payload := payloadFor(t, model, simpleContext(), &Options{Reasoning: "high"})

	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning: %#v", payload["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Errorf("effort: %v", reasoning["effort"])
	}
	if reasoning["summary"] != "auto" {
		t.Errorf("summary: %v", reasoning["summary"])
	}

	include, ok := payload["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Errorf("include: %#v", payload["include"])
	}
}

// With thinking off the request says so — but only where that is expressible.
func TestReasoningOff(t *testing.T) {
	t.Run("sends none by default", func(t *testing.T) {
		payload := payloadFor(t, reasoningModel("openai", "https://api.openai.com/v1", nil), simpleContext(), nil)
		reasoning, ok := payload["reasoning"].(map[string]any)
		if !ok || reasoning["effort"] != "none" {
			t.Errorf("reasoning: %#v", payload["reasoning"])
		}
		if _, present := payload["include"]; present {
			t.Error("nothing to include when there is no reasoning")
		}
	})

	t.Run("sends the model's own name for off", func(t *testing.T) {
		model := reasoningModel("openai", "https://api.openai.com/v1",
			ai.ThinkingLevelMap{ai.ThinkingOff: strptr("minimal")})
		payload := payloadFor(t, model, simpleContext(), nil)
		reasoning := payload["reasoning"].(map[string]any)
		if reasoning["effort"] != "minimal" {
			t.Errorf("effort: %v", reasoning["effort"])
		}
	})

	t.Run("omits the field when off is unexpressible", func(t *testing.T) {
		model := reasoningModel("openai", "https://api.openai.com/v1",
			ai.ThinkingLevelMap{ai.ThinkingOff: nil})
		payload := payloadFor(t, model, simpleContext(), nil)
		if _, present := payload["reasoning"]; present {
			t.Errorf("reasoning: %#v", payload["reasoning"])
		}
	})

	t.Run("omits the field for copilot, which rejects it", func(t *testing.T) {
		payload := payloadFor(t, reasoningModel("github-copilot", "https://api.individual.githubcopilot.com", nil),
			simpleContext(), nil)
		if _, present := payload["reasoning"]; present {
			t.Errorf("reasoning: %#v", payload["reasoning"])
		}
	})
}

// The thinking level goes through the model's own vocabulary.
func TestThinkingLevelIsMapped(t *testing.T) {
	model := reasoningModel("openai", "https://api.openai.com/v1",
		ai.ThinkingLevelMap{"high": strptr("xhigh")})
	payload := payloadFor(t, model, simpleContext(), &Options{Reasoning: "high"})

	if effort := payload["reasoning"].(map[string]any)["effort"]; effort != "xhigh" {
		t.Errorf("effort: %v", effort)
	}
}

// The endpoint rejects a max_output_tokens below its floor rather than
// clamping, so a small ask has to be raised here.
func TestMaxOutputTokensFloor(t *testing.T) {
	payload := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), simpleContext(),
		&Options{StreamOptions: ai.StreamOptions{MaxTokens: 4}})

	if got := payload["max_output_tokens"]; got != float64(minOutputTokens) {
		t.Errorf("max_output_tokens: %v, want %d", got, minOutputTokens)
	}
}

// Retention is a user-facing setting and "none" has to mean none.
func TestCacheRetention(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	model.Compat = &ai.CompatFlags{SupportsExplicitPromptCacheMode: boolptr(true)}
	ctxWithSession := &Options{StreamOptions: ai.StreamOptions{SessionID: "sess-1"}}

	t.Run("short sends the key and no retention", func(t *testing.T) {
		payload := payloadFor(t, model, simpleContext(), ctxWithSession)
		if payload["prompt_cache_key"] != "sess-1" {
			t.Errorf("prompt_cache_key: %v", payload["prompt_cache_key"])
		}
		if _, present := payload["prompt_cache_retention"]; present {
			t.Error("short retention should not name a retention")
		}
	})

	t.Run("long asks for the extended window", func(t *testing.T) {
		payload := payloadFor(t, model, simpleContext(), &Options{
			StreamOptions: ai.StreamOptions{SessionID: "sess-1", CacheRetention: ai.CacheLong},
		})
		if payload["prompt_cache_retention"] != "24h" {
			t.Errorf("prompt_cache_retention: %v", payload["prompt_cache_retention"])
		}
	})

	t.Run("none drops the key and turns caching off explicitly", func(t *testing.T) {
		payload := payloadFor(t, model, simpleContext(), &Options{
			StreamOptions: ai.StreamOptions{SessionID: "sess-1", CacheRetention: ai.CacheNone},
		})
		if _, present := payload["prompt_cache_key"]; present {
			t.Error("retention none must not send a cache key")
		}
		opts, ok := payload["prompt_cache_options"].(map[string]any)
		if !ok || opts["mode"] != "explicit" {
			t.Errorf("prompt_cache_options: %#v", payload["prompt_cache_options"])
		}
	})

	t.Run("none omits the option where the host rejects it", func(t *testing.T) {
		plain := modelFor("openai", "https://api.openai.com/v1")
		payload := payloadFor(t, plain, simpleContext(), &Options{
			StreamOptions: ai.StreamOptions{CacheRetention: ai.CacheNone},
		})
		if _, present := payload["prompt_cache_options"]; present {
			t.Error("the option must only go to hosts that accept it")
		}
	})
}

// Strict mode is opt-in: a host that does not know the field rejects the whole
// request rather than ignoring it.
func TestStrictModeIsOptIn(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{{Name: "read", Description: "read a file"}}

	t.Run("absent by default", func(t *testing.T) {
		payload := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), c, nil)
		tools := payload["tools"].([]any)
		if _, present := tools[0].(map[string]any)["strict"]; present {
			t.Error("strict must not be sent unless declared supported")
		}
	})

	t.Run("present when declared", func(t *testing.T) {
		model := modelFor("openai", "https://api.openai.com/v1")
		model.Compat = &ai.CompatFlags{SupportsStrictMode: boolptr(true)}
		payload := payloadFor(t, model, c, nil)
		tools := payload["tools"].([]any)
		if _, present := tools[0].(map[string]any)["strict"]; !present {
			t.Error("strict should be declared")
		}
	})
}

// A user message is always sent as content parts. The wire has no plain-string
// form for one, and sending the string would be dropped silently.
func TestUserMessageIsAlwaysParts(t *testing.T) {
	payload := payloadFor(t, modelFor("openai", "https://api.openai.com/v1"), simpleContext(), nil)

	user := items(t, payload)[1]
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content: %#v", user["content"])
	}
	part := parts[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "hi" {
		t.Errorf("part: %#v", part)
	}
}
