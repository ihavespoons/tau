package openaichat

import "github.com/ihavespoons/tau/ai"

// cacheControl is Anthropic's prompt-cache marker riding inside an
// OpenAI-shaped body.
//
// OpenRouter proxies Anthropic models on the chat-completions wire but does
// not translate caching for you: without these markers every turn re-reads the
// whole prefix at full price, which on a long coding session is the difference
// between a few cents and a few dollars.
type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// compatCacheControl returns the marker to place, or nil when this provider
// would not understand one.
func compatCacheControl(cm compat, retention ai.CacheRetention) *cacheControl {
	if cm.CacheControlFormat != "anthropic" || retention == ai.CacheNone {
		return nil
	}
	cc := &cacheControl{Type: "ephemeral"}
	if retention == ai.CacheLong && cm.SupportsLongCacheRetention {
		cc.TTL = "1h"
	}
	return cc
}

// applyAnthropicCacheControl places the three breakpoints Anthropic reads.
//
// The positions are what make caching work rather than merely happen: the
// system prompt and the tool list are the stable head of every request, and
// the last conversation message is the growing tail, so marking it extends the
// cached prefix one turn at a time instead of re-establishing it from scratch.
func applyAnthropicCacheControl(msgs []message, tools *[]tool, cc *cacheControl) {
	markSystemPrompt(msgs, cc)
	markLastTool(tools, cc)
	markLastConversationMessage(msgs, cc)
}

// markSystemPrompt marks the first instruction message, whichever role it
// arrived under.
func markSystemPrompt(msgs []message, cc *cacheControl) {
	for i := range msgs {
		if msgs[i].Role == "system" || msgs[i].Role == "developer" {
			markTextContent(&msgs[i], cc)
			return
		}
	}
}

// markLastTool marks the end of the tool list, which caches all of it.
func markLastTool(tools *[]tool, cc *cacheControl) {
	if tools == nil || len(*tools) == 0 {
		return
	}
	(*tools)[len(*tools)-1].CacheControl = cc
}

// markLastConversationMessage walks back to the newest message that can carry
// a marker. Instruction messages are skipped: they were handled already, and
// marking one here would waste a breakpoint on text that is not the tail.
func markLastConversationMessage(msgs []message, cc *cacheControl) {
	for i := len(msgs) - 1; i >= 0; i-- {
		switch msgs[i].Role {
		case "user", "assistant", "tool":
			if markTextContent(&msgs[i], cc) {
				return
			}
		}
	}
}

// markTextContent attaches the marker to a message's last text part, promoting
// plain-string content to the block form that can hold one.
//
// It reports whether a marker was actually placed — a message with no text at
// all (an assistant turn that produced only tool calls, say) cannot carry one,
// and the caller has to keep looking.
func markTextContent(m *message, cc *cacheControl) bool {
	switch c := m.Content.(type) {
	case string:
		if c == "" {
			return false
		}
		m.Content = []any{textPart{Type: "text", Text: c, CacheControl: cc}}
		return true

	case []any:
		for i := len(c) - 1; i >= 0; i-- {
			// Parts are stored as values, so the marked copy has to be written
			// back into the slice.
			if tp, ok := c[i].(textPart); ok {
				tp.CacheControl = cc
				c[i] = tp
				return true
			}
		}
		return false

	default:
		return false
	}
}
