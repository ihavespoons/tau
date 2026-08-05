package apishared

import "github.com/ihavespoons/tau/ai"

// Port of Pi's api/github-copilot-headers.ts. Copilot is reached over three
// different wires — chat-completions, responses, and Anthropic messages — and
// wants the same per-request headers on all of them, so they live here rather
// than in whichever wire happened to need them first.

// CopilotHeaderProvider is the provider id these headers apply to.
const CopilotHeaderProvider = "github-copilot"

// CopilotDynamicHeaders are the headers Copilot derives from the turn itself.
//
// They are per-request, not per-session: whether this turn was started by the
// user or by the agent continuing its own work, and whether it carries images.
// Copilot bills and rate-limits against the first, and refuses image content
// outright without the second — so a vision request with no header fails with
// an error about the model rather than about the missing header.
func CopilotDynamicHeaders(messages ai.MessageList) map[string]string {
	headers := map[string]string{
		"X-Initiator":   copilotInitiator(messages),
		"Openai-Intent": "conversation-edits",
	}
	if hasImageInput(messages) {
		headers["Copilot-Vision-Request"] = "true"
	}
	return headers
}

// copilotInitiator reports who started this turn. A transcript ending in
// anything but a user message is the agent continuing on its own.
func copilotInitiator(messages ai.MessageList) string {
	if len(messages) == 0 {
		return "user"
	}
	if _, isUser := messages[len(messages)-1].(ai.UserMessage); isUser {
		return "user"
	}
	return "agent"
}

// hasImageInput reports whether any user message or tool result carries an
// image. Assistant messages are not checked: a model does not send images.
func hasImageInput(messages ai.MessageList) bool {
	for _, msg := range messages {
		var blocks ai.ContentList
		switch m := msg.(type) {
		case ai.UserMessage:
			blocks = m.Content.Blocks
		case ai.ToolResultMessage:
			blocks = m.Content
		default:
			continue
		}
		for _, block := range blocks {
			if _, isImage := block.(ai.ImageContent); isImage {
				return true
			}
		}
	}
	return false
}
