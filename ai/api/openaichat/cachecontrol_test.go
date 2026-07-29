package openaichat

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
)

// anthropicViaOpenRouter is the one combination that gets cache markers: the
// provider speaks chat-completions, the upstream is Anthropic, and nothing in
// between translates caching.
func anthropicViaOpenRouter() *ai.Model {
	m := modelFor("openrouter", "https://openrouter.ai/api/v1")
	m.ID = "anthropic/claude-sonnet-4.5"
	return m
}

// cachedContext has all three breakpoint sites: a system prompt, tools, and a
// conversation whose tail is a user message.
func cachedContext() ai.Context {
	return ai.Context{
		SystemPrompt: "be helpful",
		Tools: []ai.Tool{
			{Name: "read", Description: "read a file", Parameters: &jsonschema.Schema{Type: "object"}},
			{Name: "write", Description: "write a file", Parameters: &jsonschema.Schema{Type: "object"}},
		},
		Messages: ai.MessageList{
			ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1},
		},
	}
}

// cacheMarkerOf pulls the cache_control object off a message's last text part,
// returning nil when there is none.
func cacheMarkerOf(t *testing.T, msg any) map[string]any {
	t.Helper()
	m, ok := msg.(map[string]any)
	if !ok {
		t.Fatalf("message is not an object: %#v", msg)
	}
	parts, ok := m["content"].([]any)
	if !ok {
		return nil
	}
	for i := len(parts) - 1; i >= 0; i-- {
		part, ok := parts[i].(map[string]any)
		if !ok || part["type"] != "text" {
			continue
		}
		if cc, ok := part["cache_control"].(map[string]any); ok {
			return cc
		}
		return nil
	}
	return nil
}

// THE POINT: without these markers every OpenRouter-to-Anthropic turn re-reads
// the whole prefix at full price. The compat flag was already detected; the
// markers were what was missing.
func TestAnthropicCacheControlIsPlaced(t *testing.T) {
	payload := payloadFor(t, anthropicViaOpenRouter(), cachedContext(), nil)

	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected a system and a user message: %#v", payload["messages"])
	}

	system := cacheMarkerOf(t, msgs[0])
	if system == nil {
		t.Fatal("the system prompt is the stable head of every request and must be marked")
	}
	if system["type"] != "ephemeral" {
		t.Errorf("system marker: %#v", system)
	}
	if _, hasTTL := system["ttl"]; hasTTL {
		t.Errorf("short retention should not name a ttl: %#v", system)
	}

	if cacheMarkerOf(t, msgs[1]) == nil {
		t.Error("the last conversation message must be marked, or the cached prefix never grows")
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools: %#v", payload["tools"])
	}
	if _, marked := tools[0].(map[string]any)["cache_control"]; marked {
		t.Error("only the last tool carries the marker; it covers the whole list")
	}
	if _, marked := tools[1].(map[string]any)["cache_control"]; !marked {
		t.Error("the last tool must be marked")
	}
}

// A string content field cannot hold a marker, so it is promoted to the block
// form. That promotion is the mechanism, and it has to actually happen.
func TestCacheControlPromotesStringContent(t *testing.T) {
	payload := payloadFor(t, anthropicViaOpenRouter(), cachedContext(), nil)
	msgs := payload["messages"].([]any)

	user := msgs[1].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("content should have been promoted to blocks: %#v", user["content"])
	}
	if len(parts) != 1 {
		t.Fatalf("promotion should preserve exactly the original text: %#v", parts)
	}
	if text := parts[0].(map[string]any)["text"]; text != "hi" {
		t.Errorf("promotion lost the text: %#v", text)
	}
}

// Every other provider on this wire must see a byte-identical body to before —
// an unrecognised cache_control key is a 400 from most of them.
func TestCacheControlIsAbsentElsewhere(t *testing.T) {
	cases := []struct {
		name  string
		model *ai.Model
	}{
		{"openai proper", modelFor("openai", "https://api.openai.com/v1")},
		{"groq", modelFor("groq", "https://api.groq.com/openai/v1")},
		{"an openai model via openrouter", func() *ai.Model {
			m := modelFor("openrouter", "https://openrouter.ai/api/v1")
			m.ID = "openai/gpt-5"
			return m
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := payloadFor(t, tc.model, cachedContext(), nil)

			for _, msg := range payload["messages"].([]any) {
				if cacheMarkerOf(t, msg) != nil {
					t.Errorf("a cache marker reached %s", tc.name)
				}
			}
			for _, tl := range payload["tools"].([]any) {
				if _, marked := tl.(map[string]any)["cache_control"]; marked {
					t.Errorf("a tool cache marker reached %s", tc.name)
				}
			}
		})
	}
}

// Retention is a user-facing setting, and "none" has to mean none.
func TestCacheControlHonoursRetention(t *testing.T) {
	t.Run("none suppresses every marker", func(t *testing.T) {
		payload := payloadFor(t, anthropicViaOpenRouter(), cachedContext(),
			&Options{StreamOptions: ai.StreamOptions{CacheRetention: ai.CacheNone}})

		for _, msg := range payload["messages"].([]any) {
			if cacheMarkerOf(t, msg) != nil {
				t.Error("retention none must not send markers")
			}
		}
	})

	t.Run("long asks for the extended ttl", func(t *testing.T) {
		payload := payloadFor(t, anthropicViaOpenRouter(), cachedContext(),
			&Options{StreamOptions: ai.StreamOptions{CacheRetention: ai.CacheLong}})

		system := cacheMarkerOf(t, payload["messages"].([]any)[0])
		if system == nil || system["ttl"] != "1h" {
			t.Errorf("long retention should carry a 1h ttl: %#v", system)
		}
	})
}

// A tool-call-only assistant turn has no text to mark. The walk has to keep
// going back rather than silently dropping the tail breakpoint.
func TestCacheControlSkipsMessagesWithNoText(t *testing.T) {
	c := cachedContext()
	c.Messages = ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "read foo"}, Timestamp: 1},
		ai.AssistantMessage{
			Content: []ai.Content{ai.ToolCall{
				ID: "call_1", Name: "read", Arguments: map[string]any{"path": "foo"},
			}},
			StopReason: ai.StopToolUse,
		},
		ai.ToolResultMessage{
			ToolCallID: "call_1", ToolName: "read",
			Content: []ai.Content{ai.TextContent{Text: "contents of foo"}},
		},
	}

	payload := payloadFor(t, anthropicViaOpenRouter(), c, nil)
	msgs := payload["messages"].([]any)

	// system, user, assistant (tool call only), tool
	toolMsg := msgs[len(msgs)-1].(map[string]any)
	if toolMsg["role"] != "tool" {
		t.Fatalf("expected the tool result last: %#v", toolMsg)
	}
	if cacheMarkerOf(t, toolMsg) == nil {
		t.Error("the tool result is the tail and must carry the marker")
	}

	assistant := msgs[len(msgs)-2].(map[string]any)
	if cacheMarkerOf(t, assistant) != nil {
		t.Error("only one tail marker: the walk should have stopped at the tool result")
	}
}

// With no tools in play there is simply nothing to mark, and the empty-tools
// array Anthropic-behind-a-proxy requires must not grow a marker.
func TestCacheControlWithNoTools(t *testing.T) {
	c := cachedContext()
	c.Tools = nil

	payload := payloadFor(t, anthropicViaOpenRouter(), c, nil)
	if _, present := payload["tools"]; present {
		t.Errorf("tools: %#v", payload["tools"])
	}
	if cacheMarkerOf(t, payload["messages"].([]any)[0]) == nil {
		t.Error("the system prompt is still marked without tools")
	}
}
