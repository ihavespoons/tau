package compaction

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ihavespoons/tau/ai"
)

// SummarizationSystemPrompt is the summarizer's system prompt.
//
// The second paragraph is load-bearing: the request hands a model a
// conversation, and the default thing to do with a conversation is continue it.
// Saying so explicitly is what makes the reply a summary rather than a turn.
const SummarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// toolResultMaxChars caps one tool result inside a summarization request. The
// full text is not what makes a summary good, and a single large file read
// would otherwise consume the budget meant for the whole history.
const toolResultMaxChars = 2000

func truncateForSummary(text string, maxChars int) string {
	if len(text) <= maxChars {
		return text
	}
	return fmt.Sprintf("%s\n\n[... %d more characters truncated]", text[:maxChars], len(text)-maxChars)
}

// SerializeConversation flattens messages to plain text for summarization.
//
// The conversation is sent as one user message of text rather than as messages,
// for the same reason as the system prompt: a model handed a transcript
// summarizes it, a model handed a conversation answers it.
//
// Call session.ConvertToLLM first; this handles only the three wire roles.
func SerializeConversation(messages []ai.Message) string {
	var parts []string

	for _, m := range messages {
		switch msg := m.(type) {
		case ai.UserMessage:
			if text := userText(msg.Content); text != "" {
				parts = append(parts, "[User]: "+text)
			}
		case *ai.UserMessage:
			if text := userText(msg.Content); text != "" {
				parts = append(parts, "[User]: "+text)
			}
		case ai.AssistantMessage:
			parts = append(parts, assistantParts(msg.Content)...)
		case *ai.AssistantMessage:
			parts = append(parts, assistantParts(msg.Content)...)
		case ai.ToolResultMessage:
			if text := contentText(msg.Content); text != "" {
				parts = append(parts, "[Tool result]: "+truncateForSummary(text, toolResultMaxChars))
			}
		case *ai.ToolResultMessage:
			if text := contentText(msg.Content); text != "" {
				parts = append(parts, "[Tool result]: "+truncateForSummary(text, toolResultMaxChars))
			}
		}
	}

	return strings.Join(parts, "\n\n")
}

func assistantParts(blocks ai.ContentList) []string {
	var thinking, calls []string
	hasText := false

	for _, b := range blocks {
		switch block := b.(type) {
		case ai.ThinkingContent:
			thinking = append(thinking, block.Thinking)
		case *ai.ThinkingContent:
			thinking = append(thinking, block.Thinking)
		case ai.TextContent, *ai.TextContent:
			hasText = true
		case ai.ToolCall:
			calls = append(calls, formatCall(&block))
		case *ai.ToolCall:
			calls = append(calls, formatCall(block))
		}
	}

	var parts []string
	if len(thinking) > 0 {
		parts = append(parts, "[Assistant thinking]: "+strings.Join(thinking, "\n"))
	}
	if hasText {
		parts = append(parts, "[Assistant]: "+contentText(blocks))
	}
	if len(calls) > 0 {
		parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
	}
	return parts
}

// formatCall renders a tool call as name(k=v, ...). Arguments are sorted
// because Go map iteration is not: an unsorted rendering would make two
// summarizations of the same conversation differ for no reason.
func formatCall(c *ai.ToolCall) string {
	keys := make([]string, 0, len(c.Arguments))
	for k := range c.Arguments {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make([]string, 0, len(keys))
	for _, k := range keys {
		encoded, err := json.Marshal(c.Arguments[k])
		if err != nil {
			continue
		}
		args = append(args, k+"="+string(encoded))
	}
	return c.Name + "(" + strings.Join(args, ", ") + ")"
}

func userText(c ai.UserContent) string {
	if c.Blocks == nil {
		return c.Text
	}
	return contentText(c.Blocks)
}

// contentText joins the text blocks, skipping images.
func contentText(blocks ai.ContentList) string {
	var out []string
	for _, b := range blocks {
		switch block := b.(type) {
		case ai.TextContent:
			out = append(out, block.Text)
		case *ai.TextContent:
			out = append(out, block.Text)
		}
	}
	return strings.Join(out, "\n")
}
