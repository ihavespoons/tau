// Package googlegenai implements Gemini's generateContent wire.
//
// Gemini models a conversation as alternating Content turns of typed Parts,
// where a part can be text, inline binary, a function call, or a function
// response. Reasoning is a part flagged `thought`, and — separately — ANY part
// may carry a `thoughtSignature`, an encrypted handle to the model's internal
// reasoning that must be replayed verbatim for multi-turn continuity.
//
// Those two are easy to conflate and mean different things: `thought: true`
// says this part IS thinking; a signature says this part is ATTACHED to some
// thinking. Treating a signature-bearing tool call as a thinking block would
// lose the tool call.
package googlegenai

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// content is one conversational turn.
type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts,omitempty"`
}

// part is one piece of a turn.
type part struct {
	Text string `json:"text,omitempty"`
	// Thought marks this part as thinking content rather than an answer.
	Thought bool `json:"thought,omitempty"`
	// ThoughtSignature is an encrypted handle to the model's reasoning. It can
	// appear on any part type and says nothing about what the part contains.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`

	InlineData       *inlineData       `json:"inlineData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type functionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	// Parts carries images back inside the response, which only Gemini 3 and
	// later accept.
	Parts []part `json:"parts,omitempty"`
}

var nonIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// idModels are the models behind Google's APIs that require explicit tool-call
// ids. Gemini itself pairs calls to responses positionally; the third-party
// models Google resells do not.
func requiresToolCallID(modelID string) bool {
	return strings.HasPrefix(modelID, "claude-") || strings.HasPrefix(modelID, "gpt-oss-")
}

func normalizeToolCallID(id string) string {
	s := nonIDChars.ReplaceAllString(id, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

var geminiMajor = regexp.MustCompile(`^gemini(?:-live)?-(\d+)`)

// geminiMajorVersion returns the model's major version, or 0 when the id is
// not a Gemini one.
func geminiMajorVersion(modelID string) int {
	m := geminiMajor.FindStringSubmatch(strings.ToLower(modelID))
	if m == nil {
		return 0
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n
}

// supportsMultimodalFunctionResponse reports whether images may ride inside a
// function response. Gemini 3 accepts it; earlier ones need a separate turn,
// and a non-Gemini model behind the same API is assumed to accept it.
func supportsMultimodalFunctionResponse(modelID string) bool {
	if v := geminiMajorVersion(modelID); v != 0 {
		return v >= 3
	}
	return true
}

// resolveSignature keeps a thought signature only when it can still mean
// something: same provider and model, and syntactically valid.
//
// A signature from another model is an encrypted handle to reasoning THIS
// model never did. Replaying it is not merely useless — the endpoint rejects
// a signature it cannot decrypt.
func resolveSignature(sameModel bool, signature string) string {
	if !sameModel || signature == "" {
		return ""
	}
	// Google types the field as bytes, so a signature that is not base64 is
	// rejected by the API rather than ignored. Decoding is the check: Pi uses a
	// length test and a regex, which comes to the same thing for real inputs
	// and is more code to get subtly wrong.
	if _, err := base64.StdEncoding.DecodeString(signature); err != nil {
		return ""
	}
	return signature
}

// convertMessages turns tau's transcript into Gemini contents.
func convertMessages(model *ai.Model, c ai.Context) []content {
	var out []content

	needsID := requiresToolCallID(model.ID)
	normalize := func(id string, _ ai.AssistantMessage) string {
		if !needsID {
			return id
		}
		return normalizeToolCallID(id)
	}
	msgs := apishared.TransformMessages(c.Messages, model, normalize)

	for _, msg := range msgs {
		switch m := msg.(type) {
		case ai.UserMessage:
			if turn, ok := convertUser(m); ok {
				out = append(out, turn)
			}
		case ai.AssistantMessage:
			if turn, ok := convertAssistant(m, model); ok {
				out = append(out, turn)
			}
		case ai.ToolResultMessage:
			out = appendToolResult(out, m, model, needsID)
		}
	}
	return out
}

func convertUser(m ai.UserMessage) (content, bool) {
	if m.Content.Blocks == nil {
		return content{Role: "user", Parts: []part{{Text: apishared.SanitizeSurrogates(m.Content.Text)}}}, true
	}

	var parts []part
	for _, block := range m.Content.Blocks {
		switch b := block.(type) {
		case ai.TextContent:
			parts = append(parts, part{Text: apishared.SanitizeSurrogates(b.Text)})
		case ai.ImageContent:
			parts = append(parts, part{InlineData: &inlineData{MimeType: b.MimeType, Data: b.Data}})
		}
	}
	if len(parts) == 0 {
		return content{}, false
	}
	return content{Role: "user", Parts: parts}, true
}

func convertAssistant(m ai.AssistantMessage, model *ai.Model) (content, bool) {
	sameModel := m.Provider == model.Provider && m.Model == model.ID
	needsID := requiresToolCallID(model.ID)

	var parts []part
	for _, block := range m.Content {
		switch b := block.(type) {
		case ai.TextContent:
			if apishared.TrimSpace(b.Text) == "" {
				continue
			}
			parts = append(parts, part{
				Text:             apishared.SanitizeSurrogates(b.Text),
				ThoughtSignature: resolveSignature(sameModel, b.TextSignature),
			})

		case ai.ThinkingContent:
			if apishared.TrimSpace(b.Thinking) == "" {
				continue
			}
			if !sameModel {
				// Another model's reasoning replays as plain text, with no
				// wrapper tags — tags would invite this model to imitate them.
				parts = append(parts, part{Text: apishared.SanitizeSurrogates(b.Thinking)})
				continue
			}
			parts = append(parts, part{
				Thought:          true,
				Text:             apishared.SanitizeSurrogates(b.Thinking),
				ThoughtSignature: resolveSignature(sameModel, b.ThinkingSignature),
			})

		case ai.ToolCall:
			call := &functionCall{Name: b.Name, Args: b.Arguments}
			if call.Args == nil {
				call.Args = map[string]any{}
			}
			if needsID {
				call.ID = b.ID
			}
			parts = append(parts, part{
				FunctionCall:     call,
				ThoughtSignature: resolveSignature(sameModel, b.ThoughtSignature),
			})
		}
	}

	if len(parts) == 0 {
		return content{}, false
	}
	return content{Role: "model", Parts: parts}, true
}

// appendToolResult adds a function response, merging into the previous turn
// when there is one.
//
// Google's API requires every function response from one round to sit in a
// SINGLE user turn. Emitting one turn per result is rejected, which only shows
// up once the model calls two tools at once.
func appendToolResult(out []content, m ai.ToolResultMessage, model *ai.Model, needsID bool) []content {
	var texts []string
	var images []part
	for _, block := range m.Content {
		switch b := block.(type) {
		case ai.TextContent:
			texts = append(texts, b.Text)
		case ai.ImageContent:
			if model.SupportsImageInput() {
				images = append(images, part{InlineData: &inlineData{MimeType: b.MimeType, Data: b.Data}})
			}
		}
	}

	text := strings.Join(texts, "\n")
	value := apishared.SanitizeSurrogates(text)
	if value == "" && len(images) > 0 {
		value = "(see attached image)"
	}

	// The key names the outcome: Google reads "error" differently from
	// "output", and a failed tool reported under "output" reads to the model
	// as a success whose result happens to describe a failure.
	key := "output"
	if m.IsError {
		key = "error"
	}

	resp := &functionResponse{Name: m.ToolName, Response: map[string]any{key: value}}
	if needsID {
		resp.ID = m.ToolCallID
	}
	inline := len(images) > 0 && supportsMultimodalFunctionResponse(model.ID)
	if inline {
		resp.Parts = images
	}

	responsePart := part{FunctionResponse: resp}
	if n := len(out); n > 0 && out[n-1].Role == "user" && hasFunctionResponse(out[n-1]) {
		out[n-1].Parts = append(out[n-1].Parts, responsePart)
	} else {
		out = append(out, content{Role: "user", Parts: []part{responsePart}})
	}

	if len(images) > 0 && !inline {
		// Older Gemini cannot carry images inside the response, so they follow
		// as their own turn — the only way a vision model sees them at all.
		out = append(out, content{
			Role:  "user",
			Parts: append([]part{{Text: "Tool result image:"}}, images...),
		})
	}
	return out
}

func hasFunctionResponse(c content) bool {
	for _, p := range c.Parts {
		if p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

// jsonSchemaMeta are the schema keywords Google's OpenAPI dialect rejects.
var jsonSchemaMeta = map[string]bool{
	"$schema": true, "$id": true, "$anchor": true, "$dynamicAnchor": true,
	"$vocabulary": true, "$comment": true, "$defs": true,
	// The pre-2019-09 spelling of $defs.
	"definitions": true,
}

// sanitizeForOpenAPI strips meta-declarations from a schema.
//
// Only needed on the legacy `parameters` field, which Google validates as
// OpenAPI 3.0 rather than JSON Schema and which rejects the keywords above
// outright.
func sanitizeForOpenAPI(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(obj))
	for k, val := range obj {
		if jsonSchemaMeta[k] {
			continue
		}
		out[k] = sanitizeForOpenAPI(val)
	}
	return out
}

// schemaAsMap renders a tool's schema so it can be sanitized.
func schemaAsMap(t ai.Tool) any {
	if t.Parameters == nil {
		return map[string]any{"type": "object"}
	}
	raw, err := json.Marshal(t.Parameters)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"type": "object"}
	}
	return out
}
