package openaichat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// Wire types for the chat-completions request. They are hand-written rather
// than generated because several providers need fields OpenAI never defined,
// and Extra carries those without a struct per vendor.
type (
	// message is one entry in the `messages` array.
	message struct {
		Role    string `json:"role"`
		Content any    `json:"content,omitempty"`
		Name    string `json:"name,omitempty"`

		ToolCallID string     `json:"tool_call_id,omitempty"`
		ToolCalls  []toolCall `json:"tool_calls,omitempty"`

		// Extra holds provider-specific keys (reasoning_content,
		// reasoning_details, a signature-named thinking field, Kimi's tools).
		Extra map[string]any `json:"-"`
	}

	textPart struct {
		Type         string        `json:"type"`
		Text         string        `json:"text"`
		CacheControl *cacheControl `json:"cache_control,omitempty"`
	}

	imagePart struct {
		Type     string   `json:"type"`
		ImageURL imageURL `json:"image_url"`
	}

	imageURL struct {
		URL string `json:"url"`
	}

	toolCall struct {
		ID       string        `json:"id"`
		Type     string        `json:"type"`
		Function *toolCallFunc `json:"function,omitempty"`
		Custom   *toolCallCust `json:"custom,omitempty"`
	}

	toolCallFunc struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}

	toolCallCust struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	}
)

// MarshalJSON folds Extra into the object. Providers keep inventing top-level
// message fields (deepseek's reasoning_content, openrouter's
// reasoning_details, llama.cpp's signature-named field), and a map is the only
// honest way to carry them without a struct per vendor.
func (m message) MarshalJSON() ([]byte, error) {
	type plain message
	base, err := json.Marshal(plain(m))
	if err != nil {
		return nil, err
	}
	if len(m.Extra) == 0 {
		return base, nil
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range m.Extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		merged[k] = raw
	}
	return json.Marshal(merged)
}

// nonIDChars matches everything OpenAI rejects in a tool-call id.
var nonIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// maxToolCallID is OpenAI's limit.
const maxToolCallID = 40

// normalizeToolCallID makes an id from another API safe to replay here.
//
// Responses-API providers (github-copilot, openai-codex, opencode) emit
// "{call_id}|{item_id}" pairs where the second half can run to hundreds of
// characters. The call_id alone is not enough: two tool calls in one turn can
// share it and differ only by item_id, and Chat Completions requires distinct
// ids. So both halves are kept, sanitized, and hashed down if too long.
func normalizeToolCallID(id string, provider ai.ProviderId) string {
	if sep := strings.Index(id, "|"); sep >= 0 {
		callID := nonIDChars.ReplaceAllString(id[:sep], "_")
		itemID := nonIDChars.ReplaceAllString(id[sep+1:], "_")

		combined := callID
		if itemID != "" {
			combined = callID + "_" + itemID
		}
		if len(combined) <= maxToolCallID {
			return combined
		}

		sum := sha256.Sum256([]byte(id))
		hash := hex.EncodeToString(sum[:])[:8]
		keep := maxToolCallID - len(hash) - 1
		if keep < 1 {
			keep = 1
		}
		if len(callID) > keep {
			callID = callID[:keep]
		}
		return callID + "_" + hash
	}

	if provider == "openai" && len(id) > maxToolCallID {
		return id[:maxToolCallID]
	}
	return id
}

// convertMessages turns tau's transcript into the chat-completions `messages`
// array, applying every compat quirk that affects message shape.
// convertMessages renders the transcript. grammar maps a tool name to the
// argument that carries its raw output, for the tools declared with a grammar;
// a call to one of those is replayed as a custom tool call rather than as JSON.
func convertMessages(model *ai.Model, c ai.Context, cm compat, grammar map[string]string) ([]message, error) {
	var out []message

	normalize := func(id string, _ ai.AssistantMessage) string { return normalizeToolCallID(id, model.Provider) }
	msgs := apishared.TransformMessages(c.Messages, model, normalize)

	if c.SystemPrompt != "" {
		// Reasoning models take the system prompt in the developer role, but
		// only where the provider understands it.
		role := "system"
		if model.Reasoning && cm.SupportsDeveloperRole {
			role = "developer"
		}
		out = append(out, message{Role: role, Content: apishared.SanitizeSurrogates(c.SystemPrompt)})
	}

	lastRole := ""
	for i := 0; i < len(msgs); i++ {
		msg := msgs[i]

		// Some providers reject a user message directly after tool results;
		// a synthetic assistant turn bridges the gap.
		if cm.RequiresAssistantAfterToolResult && lastRole == "toolResult" {
			if _, isUser := msg.(ai.UserMessage); isUser {
				out = append(out, message{Role: "assistant", Content: bridgeText})
			}
		}

		switch m := msg.(type) {
		case ai.UserMessage:
			if converted, ok := convertUser(m); ok {
				out = append(out, converted)
			}
			lastRole = "user"

		case ai.AssistantMessage:
			converted, ok, err := convertAssistant(m, model, cm, grammar)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, converted)
			}
			lastRole = "assistant"

		case ai.ToolResultMessage:
			var consumed int
			var err error
			out, consumed, lastRole, err = convertToolResults(out, msgs[i:], model, c.Tools, cm)
			if err != nil {
				return nil, err
			}
			i += consumed - 1

		default:
			lastRole = ""
		}
	}
	return out, nil
}

// bridgeText is the filler assistant turn for providers that will not accept a
// user message straight after tool results.
const bridgeText = "I have processed the tool results."

func convertUser(m ai.UserMessage) (message, bool) {
	if m.Content.Blocks == nil {
		return message{Role: "user", Content: apishared.SanitizeSurrogates(m.Content.Text)}, true
	}

	var parts []any
	for _, block := range m.Content.Blocks {
		switch b := block.(type) {
		case ai.TextContent:
			parts = append(parts, textPart{Type: "text", Text: apishared.SanitizeSurrogates(b.Text)})
		case ai.ImageContent:
			parts = append(parts, imagePart{Type: "image_url", ImageURL: imageURL{URL: dataURL(b)}})
		}
	}
	if len(parts) == 0 {
		return message{}, false
	}
	return message{Role: "user", Content: parts}, true
}

func dataURL(img ai.ImageContent) string {
	return "data:" + img.MimeType + ";base64," + img.Data
}

// convertAssistant replays a past assistant turn.
//
// The content is sent as a plain string rather than an array of text parts.
// That is deliberate and load-bearing: the array form is non-standard here,
// and some models (DeepSeek V3.2 via NVIDIA NIM among them) mirror the block
// structure literally back into their output, producing nested JSON where
// prose belongs.
func convertAssistant(m ai.AssistantMessage, model *ai.Model, cm compat, grammar map[string]string) (message, bool, error) {
	out := message{Role: "assistant"}
	if cm.RequiresAssistantAfterToolResult {
		out.Content = "" // some providers reject null content
	}

	var textParts []textPart
	var thinking []ai.ThinkingContent
	var toolCalls []ai.ToolCall
	for _, block := range m.Content {
		switch b := block.(type) {
		case ai.TextContent:
			if apishared.TrimSpace(b.Text) != "" {
				textParts = append(textParts, textPart{Type: "text", Text: apishared.SanitizeSurrogates(b.Text)})
			}
		case ai.ThinkingContent:
			if apishared.TrimSpace(b.Thinking) != "" {
				thinking = append(thinking, b)
			}
		case ai.ToolCall:
			toolCalls = append(toolCalls, b)
		}
	}

	var text strings.Builder
	for _, p := range textParts {
		text.WriteString(p.Text)
	}

	switch {
	case len(thinking) > 0 && cm.RequiresThinkingAsText:
		// Reasoning is folded in as plain text with no delimiters — tags would
		// invite the model to imitate them.
		var joined []string
		for _, t := range thinking {
			joined = append(joined, apishared.SanitizeSurrogates(t.Thinking))
		}
		parts := []any{textPart{Type: "text", Text: strings.Join(joined, "\n\n")}}
		for _, p := range textParts {
			parts = append(parts, p)
		}
		out.Content = parts

	case len(thinking) > 0:
		if text.Len() > 0 {
			out.Content = text.String()
		}
		// llama.cpp-style servers name the replay field after the signature the
		// model itself emitted.
		signature := thinking[0].ThinkingSignature
		if model.Provider == "opencode-go" && signature == "reasoning" {
			signature = "reasoning_content"
		}
		if signature != "" {
			var raw []string
			for _, t := range thinking {
				raw = append(raw, t.Thinking)
			}
			out.setExtra(signature, strings.Join(raw, "\n"))
		}

	case text.Len() > 0:
		out.Content = text.String()
	}

	if len(toolCalls) > 0 {
		out.ToolCalls = make([]toolCall, 0, len(toolCalls))
		var reasoningDetails []any
		for _, tc := range toolCalls {
			// A call to a grammar tool goes back as the text the model
			// produced, not as JSON. Replaying it as a function call would
			// contradict the tool declaration in the same request.
			if property, isGrammar := grammar[tc.Name]; isGrammar {
				input, err := apishared.GrammarToolInput(tc.Name, tc.Arguments, property)
				if err != nil {
					return message{}, false, err
				}
				out.ToolCalls = append(out.ToolCalls, toolCall{
					ID:     tc.ID,
					Type:   "custom",
					Custom: &toolCallCust{Name: tc.Name, Input: apishared.SanitizeSurrogates(input)},
				})
				continue
			}

			args, err := json.Marshal(tc.Arguments)
			if err != nil {
				args = []byte("{}")
			}
			out.ToolCalls = append(out.ToolCalls, toolCall{
				ID:   tc.ID,
				Type: "function",
				Function: &toolCallFunc{
					Name:      tc.Name,
					Arguments: string(args),
				},
			})
			if tc.ThoughtSignature != "" {
				var detail any
				if err := json.Unmarshal([]byte(tc.ThoughtSignature), &detail); err == nil {
					reasoningDetails = append(reasoningDetails, detail)
				}
			}
		}
		if len(reasoningDetails) > 0 {
			out.setExtra("reasoning_details", reasoningDetails)
		}
	}

	// DeepSeek rejects a replayed assistant turn that lacks the field entirely
	// once reasoning is on, even when there was no reasoning to replay.
	if cm.RequiresReasoningContentOnAssistantMessages && model.Reasoning {
		if _, present := out.Extra["reasoning_content"]; !present {
			out.setExtra("reasoning_content", "")
		}
	}

	// Providers insist on "content or tool_calls, but not neither" — which is
	// exactly what an aborted turn that produced nothing looks like.
	if !out.hasContent() && len(out.ToolCalls) == 0 {
		return message{}, false, nil
	}
	return out, true, nil
}

func (m *message) setExtra(key string, value any) {
	if m.Extra == nil {
		m.Extra = map[string]any{}
	}
	m.Extra[key] = value
}

func (m message) hasContent() bool {
	switch c := m.Content.(type) {
	case nil:
		return false
	case string:
		return c != ""
	case []any:
		return len(c) > 0
	default:
		return true
	}
}

// convertToolResults consumes the run of consecutive tool results starting at
// msgs[0], returning the appended output, how many messages it took, and the
// role to attribute to the last thing emitted.
//
// Images cannot ride along in a tool message, so they are re-attached as a
// following user message — the only way a vision model gets to see what a tool
// produced.
func convertToolResults(out []message, msgs ai.MessageList, model *ai.Model, tools []ai.Tool, cm compat) ([]message, int, string, error) {
	var images []any
	deferredNames := map[string]bool{}

	consumed := 0
	for _, msg := range msgs {
		tr, ok := msg.(ai.ToolResultMessage)
		if !ok {
			break
		}
		consumed++

		var texts []string
		hasImages := false
		for _, block := range tr.Content {
			switch b := block.(type) {
			case ai.TextContent:
				texts = append(texts, b.Text)
			case ai.ImageContent:
				hasImages = true
				if model.SupportsImageInput() {
					images = append(images, imagePart{
						Type: "image_url", ImageURL: imageURL{URL: dataURL(b)},
					})
				}
			}
		}

		// A tool message with empty content is rejected by several providers,
		// so silence gets an explicit placeholder.
		text := strings.Join(texts, "\n")
		if text == "" {
			text = "(no tool output)"
			if hasImages {
				text = "(see attached image)"
			}
		}

		tm := message{
			Role:       "tool",
			Content:    apishared.SanitizeSurrogates(text),
			ToolCallID: tr.ToolCallID,
		}
		if cm.RequiresToolResultName && tr.ToolName != "" {
			tm.Name = tr.ToolName
		}
		out = append(out, tm)

		if cm.DeferredToolsMode == "kimi" {
			for _, name := range tr.AddedToolNames {
				deferredNames[name] = true
			}
		}
	}

	lastRole := "toolResult"
	if len(images) > 0 {
		if cm.RequiresAssistantAfterToolResult {
			out = append(out, message{Role: "assistant", Content: bridgeText})
		}
		parts := append([]any{textPart{Type: "text", Text: "Attached image(s) from tool result:"}}, images...)
		out = append(out, message{Role: "user", Content: parts})
		lastRole = "user"
	}

	if len(deferredNames) > 0 {
		var deferred []ai.Tool
		for _, t := range tools {
			if deferredNames[t.Name] {
				deferred = append(deferred, t)
			}
		}
		if len(deferred) > 0 {
			// Kimi takes newly-available tools as a system message carrying a
			// `tools` field and no content at all.
			converted, err := convertTools(deferred, cm)
			if err != nil {
				return nil, 0, "", err
			}
			sys := message{Role: "system"}
			sys.setExtra("tools", converted)
			out = append(out, sys)
		}
	}

	return out, consumed, lastRole, nil
}
