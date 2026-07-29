// Package mistralconv implements Mistral's chat wire.
//
// It looks like chat-completions and is not: the field names are camelCase,
// message content is an array of typed chunks with its own `thinking` kind,
// and tool-call ids must be exactly nine alphanumeric characters — a
// constraint no other provider has and the main reason this cannot be folded
// into the openaichat package.
package mistralconv

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// toolCallIDLength is exactly what Mistral accepts — not a maximum. An id of
// any other length is rejected, which is why ids from every other provider
// have to be rewritten rather than passed through.
const toolCallIDLength = 9

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`

	ToolCalls  []toolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// chunk is one piece of message content. Mistral gives thinking its own chunk
// type rather than a separate field.
type chunk struct {
	Type     string  `json:"type"`
	Text     string  `json:"text,omitempty"`
	Thinking []chunk `json:"thinking,omitempty"`
	ImageURL string  `json:"imageUrl,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]`)

// idNormalizer rewrites tool-call ids to Mistral's fixed width.
//
// It is stateful because COLLISIONS matter: two different originals that
// reduce to the same nine characters would pair one call's result to the
// other, so `taken` records which original owns which id and a clash retries
// with a different seed. `assigned` is only a cache — derivation is
// deterministic, so stability does not depend on it — but normalize runs once
// per message and a long transcript rehashes otherwise.
type idNormalizer struct {
	assigned map[string]string
	taken    map[string]string
}

func newIDNormalizer() *idNormalizer {
	return &idNormalizer{assigned: map[string]string{}, taken: map[string]string{}}
}

func (n *idNormalizer) normalize(id string) string {
	if existing, ok := n.assigned[id]; ok {
		return existing
	}
	// The retry is bounded. Each attempt reseeds the hash so a collision is
	// vanishingly unlikely to repeat, but an unbounded loop here would hang
	// the whole turn if that assumption ever stopped holding — and a hang is
	// far worse to diagnose than a duplicate id.
	for attempt := 0; attempt < maxIDAttempts; attempt++ {
		candidate := deriveToolCallID(id, attempt)
		if owner, ok := n.taken[candidate]; !ok || owner == id {
			n.assigned[id] = candidate
			n.taken[candidate] = id
			return candidate
		}
	}

	// Give up and take the first form. A duplicate id mispairs one tool
	// result; refusing to return would lose the turn entirely.
	candidate := deriveToolCallID(id, 0)
	n.assigned[id] = candidate
	return candidate
}

// maxIDAttempts bounds the collision search.
const maxIDAttempts = 64

// deriveToolCallID produces a nine-character id. An id that is already the
// right shape passes through, so a Mistral-native conversation replays with
// its own ids intact.
func deriveToolCallID(id string, attempt int) string {
	normalized := nonAlphanumeric.ReplaceAllString(id, "")
	if attempt == 0 && len(normalized) == toolCallIDLength {
		return normalized
	}

	seed := normalized
	if seed == "" {
		seed = id
	}
	if attempt > 0 {
		seed = seed + ":" + string(rune('0'+attempt%10)) + strings.Repeat("x", attempt/10)
	}
	sum := sha256.Sum256([]byte(seed))
	hashed := nonAlphanumeric.ReplaceAllString(hex.EncodeToString(sum[:]), "")
	return hashed[:toolCallIDLength]
}

// convertMessages turns tau's transcript into Mistral messages.
func convertMessages(model *ai.Model, c ai.Context) []message {
	ids := newIDNormalizer()
	msgs := apishared.TransformMessages(c.Messages, model,
		func(id string, _ ai.AssistantMessage) string { return ids.normalize(id) })

	supportsImages := model.SupportsImageInput()
	var out []message

	if c.SystemPrompt != "" {
		out = append(out, message{Role: "system", Content: apishared.SanitizeSurrogates(c.SystemPrompt)})
	}

	for _, msg := range msgs {
		switch m := msg.(type) {
		case ai.UserMessage:
			if converted, ok := convertUser(m); ok {
				out = append(out, converted)
			}
		case ai.AssistantMessage:
			if converted, ok := convertAssistant(m, ids); ok {
				out = append(out, converted)
			}
		case ai.ToolResultMessage:
			out = append(out, convertToolResult(m, supportsImages, ids))
		}
	}
	return out
}

func convertUser(m ai.UserMessage) (message, bool) {
	if m.Content.Blocks == nil {
		return message{Role: "user", Content: apishared.SanitizeSurrogates(m.Content.Text)}, true
	}

	// Images reaching here are already known to be viewable: the shared
	// transform replaces them with a placeholder text block for a model that
	// cannot see them, before this runs.
	var chunks []chunk
	for _, block := range m.Content.Blocks {
		switch b := block.(type) {
		case ai.TextContent:
			chunks = append(chunks, chunk{Type: "text", Text: apishared.SanitizeSurrogates(b.Text)})
		case ai.ImageContent:
			chunks = append(chunks, chunk{Type: "image_url", ImageURL: dataURL(b)})
		}
	}
	if len(chunks) == 0 {
		return message{}, false
	}
	return message{Role: "user", Content: chunks}, true
}

func dataURL(img ai.ImageContent) string {
	return "data:" + img.MimeType + ";base64," + img.Data
}

func convertAssistant(m ai.AssistantMessage, ids *idNormalizer) (message, bool) {
	var chunks []chunk
	var calls []toolCall

	for _, block := range m.Content {
		switch b := block.(type) {
		case ai.TextContent:
			if apishared.TrimSpace(b.Text) == "" {
				continue
			}
			chunks = append(chunks, chunk{Type: "text", Text: apishared.SanitizeSurrogates(b.Text)})

		case ai.ThinkingContent:
			if apishared.TrimSpace(b.Thinking) == "" {
				continue
			}
			// Thinking nests: the chunk carries its own list of text chunks.
			chunks = append(chunks, chunk{
				Type:     "thinking",
				Thinking: []chunk{{Type: "text", Text: apishared.SanitizeSurrogates(b.Thinking)}},
			})

		case ai.ToolCall:
			args, err := json.Marshal(b.Arguments)
			if err != nil {
				args = []byte("{}")
			}
			calls = append(calls, toolCall{
				ID: ids.normalize(b.ID), Type: "function",
				Function: toolCallFunc{Name: b.Name, Arguments: string(args)},
			})
		}
	}

	out := message{Role: "assistant"}
	if len(chunks) > 0 {
		out.Content = chunks
	}
	out.ToolCalls = calls
	// A turn with neither content nor calls — an aborted one — is rejected.
	if len(chunks) == 0 && len(calls) == 0 {
		return message{}, false
	}
	return out, true
}

func convertToolResult(m ai.ToolResultMessage, supportsImages bool, ids *idNormalizer) message {
	var texts []string
	var images []ai.ImageContent
	for _, block := range m.Content {
		switch b := block.(type) {
		case ai.TextContent:
			texts = append(texts, apishared.SanitizeSurrogates(b.Text))
		case ai.ImageContent:
			images = append(images, b)
		}
	}

	chunks := []chunk{{Type: "text", Text: toolResultText(strings.Join(texts, "\n"), len(images) > 0, supportsImages)}}
	if supportsImages {
		for _, img := range images {
			chunks = append(chunks, chunk{Type: "image_url", ImageURL: dataURL(img)})
		}
	}

	return message{
		Role: "tool", ToolCallID: ids.normalize(m.ToolCallID),
		Name: m.ToolName, Content: chunks,
	}
}

// toolResultText makes sure a result always says something. Several providers
// reject an empty tool message, and silence tells the model nothing anyway.
func toolResultText(text string, hasImages, supportsImages bool) string {
	if text != "" {
		return text
	}
	if hasImages {
		if supportsImages {
			return "(see attached image)"
		}
		return "(image omitted: model does not support images)"
	}
	return "(no tool output)"
}
