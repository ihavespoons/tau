// Package apishared holds the helpers every wire API needs: the message
// transform Pi applies before any provider-specific conversion, deferred-tool
// splitting, and unicode sanitization.
//
// These lived inside the anthropic package until a second wire API needed
// them. Keeping one copy matters more than the import: transformMessages
// decides what a provider is even allowed to see — dropping errored turns,
// synthesizing results for orphaned tool calls, downgrading images for
// non-vision models — and two copies would drift into two different
// conversations for the same transcript.
package apishared

import (
	"os"
	"time"
	"unicode/utf8"

	"github.com/ihavespoons/tau/ai"
)

const (
	nonVisionUserImagePlaceholder = "(image omitted: model does not support images)"
	nonVisionToolImagePlaceholder = "(tool image omitted: model does not support images)"
)

// sanitizeSurrogates removes unpaired UTF-16 surrogate code points and invalid
// UTF-8 bytes. Go strings normally cannot contain lone surrogates (the JSON
// decoder replaces them with U+FFFD), so this mostly guards strings built from
// raw bytes.
func SanitizeSurrogates(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == utf8.RuneError {
			continue
		}
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func replaceImagesWithPlaceholder(content ai.ContentList, placeholder string) ai.ContentList {
	result := make(ai.ContentList, 0, len(content))
	previousWasPlaceholder := false
	for _, block := range content {
		if _, isImage := block.(ai.ImageContent); isImage {
			if !previousWasPlaceholder {
				result = append(result, ai.TextContent{Text: placeholder})
			}
			previousWasPlaceholder = true
			continue
		}
		result = append(result, block)
		if t, ok := block.(ai.TextContent); ok {
			previousWasPlaceholder = t.Text == placeholder
		} else {
			previousWasPlaceholder = false
		}
	}
	return result
}

func downgradeUnsupportedImages(messages ai.MessageList, model *ai.Model) ai.MessageList {
	if model.SupportsImageInput() {
		return messages
	}
	out := make(ai.MessageList, len(messages))
	for i, msg := range messages {
		switch m := msg.(type) {
		case ai.UserMessage:
			if m.Content.Blocks != nil {
				m.Content.Blocks = replaceImagesWithPlaceholder(m.Content.Blocks, nonVisionUserImagePlaceholder)
			}
			out[i] = m
		case ai.ToolResultMessage:
			m.Content = replaceImagesWithPlaceholder(m.Content, nonVisionToolImagePlaceholder)
			out[i] = m
		default:
			out[i] = msg
		}
	}
	return out
}

// TransformMessages is the port of Pi's transformMessages: downgrades images
// for non-vision models, converts cross-model thinking blocks to text, drops
// redacted thinking cross-model, normalizes cross-model tool-call ids, skips
// errored/aborted assistant turns, and synthesizes tool results for orphaned
// tool calls.
// NormalizeIDFunc rewrites a replayed tool-call id.
//
// It receives the assistant message the call came from, not just the id,
// because a wire may need to know whether the call is FOREIGN — from a
// different provider or API — as opposed to merely from a different model of
// the same provider. The responses wire distinguishes them: a foreign call's
// item id is rehashed to avoid OpenAI's reasoning-pairing validation, while a
// same-provider one keeps its own.
type NormalizeIDFunc func(id string, source ai.AssistantMessage) string

func TransformMessages(messages ai.MessageList, model *ai.Model, normalizeID NormalizeIDFunc) ai.MessageList {
	toolCallIDMap := map[string]string{}
	imageAware := downgradeUnsupportedImages(messages, model)

	transformed := make(ai.MessageList, 0, len(imageAware))
	for _, msg := range imageAware {
		switch m := msg.(type) {
		case ai.UserMessage:
			transformed = append(transformed, m)
		case ai.ToolResultMessage:
			if normalizedID, ok := toolCallIDMap[m.ToolCallID]; ok && normalizedID != m.ToolCallID {
				m.ToolCallID = normalizedID
			}
			transformed = append(transformed, m)
		case ai.AssistantMessage:
			isSameModel := m.Provider == model.Provider && m.Api == model.Api && m.Model == model.ID
			content := make(ai.ContentList, 0, len(m.Content))
			for _, block := range m.Content {
				switch b := block.(type) {
				case ai.ThinkingContent:
					if b.Redacted {
						if isSameModel {
							content = append(content, b)
						}
						continue
					}
					if isSameModel && b.ThinkingSignature != "" {
						content = append(content, b)
						continue
					}
					if TrimSpace(b.Thinking) == "" {
						continue
					}
					if isSameModel {
						content = append(content, b)
					} else {
						content = append(content, ai.TextContent{Text: b.Thinking})
					}
				case ai.TextContent:
					if isSameModel {
						content = append(content, b)
					} else {
						content = append(content, ai.TextContent{Text: b.Text})
					}
				case ai.ToolCall:
					if !isSameModel && b.ThoughtSignature != "" {
						b.ThoughtSignature = ""
					}
					if !isSameModel && normalizeID != nil {
						normalized := normalizeID(b.ID, m)
						if normalized != b.ID {
							toolCallIDMap[b.ID] = normalized
							b.ID = normalized
						}
					}
					content = append(content, b)
				default:
					content = append(content, block)
				}
			}
			m.Content = content
			transformed = append(transformed, m)
		default:
			transformed = append(transformed, msg)
		}
	}

	// Second pass: skip errored/aborted assistant turns and synthesize empty
	// tool results for orphaned tool calls.
	result := make(ai.MessageList, 0, len(transformed))
	var pendingToolCalls []ai.ToolCall
	existingToolResultIDs := map[string]bool{}
	insertSynthetic := func() {
		if len(pendingToolCalls) == 0 {
			return
		}
		for _, tc := range pendingToolCalls {
			if existingToolResultIDs[tc.ID] {
				continue
			}
			result = append(result, ai.ToolResultMessage{
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Content:    ai.ContentList{ai.TextContent{Text: "No result provided"}},
				IsError:    true,
				Timestamp:  time.Now().UnixMilli(),
			})
		}
		pendingToolCalls = nil
		existingToolResultIDs = map[string]bool{}
	}

	for _, msg := range transformed {
		switch m := msg.(type) {
		case ai.AssistantMessage:
			insertSynthetic()
			if m.StopReason == ai.StopError || m.StopReason == ai.StopAborted {
				continue
			}
			var toolCalls []ai.ToolCall
			for _, block := range m.Content {
				if tc, ok := block.(ai.ToolCall); ok {
					toolCalls = append(toolCalls, tc)
				}
			}
			if len(toolCalls) > 0 {
				pendingToolCalls = toolCalls
				existingToolResultIDs = map[string]bool{}
			}
			result = append(result, m)
		case ai.ToolResultMessage:
			existingToolResultIDs[m.ToolCallID] = true
			result = append(result, m)
		case ai.UserMessage:
			insertSynthetic()
			result = append(result, m)
		default:
			result = append(result, msg)
		}
	}
	insertSynthetic()
	return result
}

// SplitDeferredTools ports Pi's deferred-tools.ts: tools first referenced by a
// transcript ToolResultMessage.AddedToolNames (and not yet used) are deferred.
func SplitDeferredTools(c ai.Context, enabled bool, normalizeName func(string) string) (immediate []ai.Tool, deferred []ai.Tool) {
	if normalizeName == nil {
		normalizeName = func(s string) string { return s }
	}
	type entry struct {
		name string
		tool ai.Tool
	}
	var order []entry
	seen := map[string]int{}
	for _, tool := range c.Tools {
		name := normalizeName(tool.Name)
		if idx, ok := seen[name]; ok {
			order[idx] = entry{name, tool}
			continue
		}
		seen[name] = len(order)
		order = append(order, entry{name, tool})
	}
	if !enabled {
		for _, e := range order {
			immediate = append(immediate, e.tool)
		}
		return immediate, nil
	}

	deferredNames := map[string]bool{}
	usedNames := map[string]bool{}
	for _, msg := range c.Messages {
		switch m := msg.(type) {
		case ai.AssistantMessage:
			for _, block := range m.Content {
				if tc, ok := block.(ai.ToolCall); ok {
					usedNames[normalizeName(tc.Name)] = true
				}
			}
		case ai.ToolResultMessage:
			for _, name := range m.AddedToolNames {
				n := normalizeName(name)
				if !usedNames[n] {
					deferredNames[n] = true
				}
			}
		}
	}
	for _, e := range order {
		if deferredNames[e.name] {
			deferred = append(deferred, e.tool)
		} else {
			immediate = append(immediate, e.tool)
		}
	}
	return immediate, deferred
}

// TrimSpace trims ASCII whitespace, matching JavaScript's String.trim() for
// the byte range Pi actually encounters.
func TrimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// EnvValue resolves a provider configuration value.
//
// The scoped override map wins, then the real process environment. Checking
// only the map — which is what tau did before — means a variable the user
// exported in their shell is silently ignored, and the symptom is a setting
// that appears to do nothing.
func EnvValue(env map[string]string, name string) string {
	if v, ok := env[name]; ok && v != "" {
		return v
	}
	return os.Getenv(name)
}
