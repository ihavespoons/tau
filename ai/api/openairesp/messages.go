package openairesp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// item is one entry in the request's `input` array. The responses wire models
// a conversation as a flat list of typed items rather than as roles, so an
// assistant turn with reasoning, prose, and two tool calls becomes four items.
type item struct {
	Type    string `json:"type,omitempty"`
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"`
	ID      string `json:"id,omitempty"`
	Status  string `json:"status,omitempty"`
	Phase   string `json:"phase,omitempty"`

	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	// Input carries a grammar tool's raw output. It is a separate field from
	// Arguments because the wire uses a different item type for it entirely.
	Input string `json:"input,omitempty"`
	// Arguments is a JSON STRING on a function call and an OBJECT on a tool
	// search. The wire is inconsistent about it, so the field is untyped.
	Arguments any `json:"arguments,omitempty"`
	Output    any `json:"output,omitempty"`

	// Execution and Tools belong to the tool-search exchange, which is how a
	// tool becomes available mid-conversation.
	Execution string `json:"execution,omitempty"`
	Tools     []tool `json:"tools,omitempty"`

	// Raw carries a reasoning item back verbatim. The wire requires the
	// encrypted payload and its own fields to survive a round trip unchanged,
	// so it is replayed as the bytes it arrived as rather than re-encoded from
	// a struct that might drop a field.
	Raw json.RawMessage `json:"-"`
}

// MarshalJSON emits Raw when a reasoning item is being replayed.
func (i item) MarshalJSON() ([]byte, error) {
	if len(i.Raw) > 0 {
		return i.Raw, nil
	}
	type plain item
	return json.Marshal(plain(i))
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// Annotations must be present on assistant output text even when empty.
	Annotations []any `json:"annotations,omitempty"`

	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// textSignature is how tau carries an assistant message's item id across a
// session file, so a replayed turn keeps the id the model assigned it.
type textSignature struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	Phase string `json:"phase,omitempty"`
}

func encodeTextSignature(id, phase string) string {
	b, err := json.Marshal(textSignature{V: 1, ID: id, Phase: phase})
	if err != nil {
		return id
	}
	return string(b)
}

// parseTextSignature reads a signature, tolerating the plain-id form written
// before the versioned envelope existed.
func parseTextSignature(sig string) (id, phase string) {
	if sig == "" {
		return "", ""
	}
	if !strings.HasPrefix(sig, "{") {
		return sig, ""
	}
	var parsed textSignature
	if err := json.Unmarshal([]byte(sig), &parsed); err != nil || parsed.V != 1 || parsed.ID == "" {
		return sig, ""
	}
	if parsed.Phase != "commentary" && parsed.Phase != "final_answer" {
		return parsed.ID, ""
	}
	return parsed.ID, parsed.Phase
}

var nonIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// maxIDLen is OpenAI's limit on an item id.
const maxIDLen = 64

func normalizeIDPart(part string) string {
	s := nonIDChars.ReplaceAllString(part, "_")
	if len(s) > maxIDLen {
		s = s[:maxIDLen]
	}
	return strings.TrimRight(s, "_")
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// toolCallProviders are the providers whose tool-call ids carry a responses
// item id after a pipe. For anyone else the id is a plain string and the
// pairing rules below do not apply.
var toolCallProviders = map[ai.ProviderId]bool{
	"openai": true, "openai-codex": true, "opencode": true,
}

// normalizeToolCallID rewrites a replayed tool-call id for this wire.
//
// The wire pairs a function_call item with the reasoning item that produced
// it, and validates that pairing. A call replayed from ANOTHER provider or API
// has an item id the endpoint never issued, so its id is rehashed into a fresh
// fc_ id that cannot collide with a real one — which sidesteps the validation
// instead of failing it. A call from the same provider keeps its own id, which
// is what lets the model resume its own reasoning.
func normalizeToolCallID(id string, source ai.AssistantMessage, model *ai.Model) string {
	if !toolCallProviders[model.Provider] {
		return normalizeIDPart(id)
	}
	sep := strings.Index(id, "|")
	if sep < 0 {
		return normalizeIDPart(id)
	}

	callID := normalizeIDPart(id[:sep])
	itemID := id[sep+1:]

	foreign := source.Provider != model.Provider || source.Api != model.Api
	var normalizedItem string
	if foreign {
		normalizedItem = "fc_" + shortHash(itemID)
		if len(normalizedItem) > maxIDLen {
			normalizedItem = normalizedItem[:maxIDLen]
		}
	} else {
		normalizedItem = normalizeIDPart(itemID)
	}
	// The wire requires a function_call item id to start with fc_.
	if !strings.HasPrefix(normalizedItem, "fc_") {
		normalizedItem = normalizeIDPart("fc_" + normalizedItem)
	}
	return callID + "|" + normalizedItem
}

// convertMessages turns tau's transcript into the `input` array.
// convertMessages renders the transcript. grammar maps a tool name to the
// argument carrying its raw output, for the tools declared with a grammar; a
// call to one of those, and its result, use the custom_tool_call items rather
// than the function_call ones.
func convertMessages(model *ai.Model, c ai.Context, cm compat, deferred map[string]ai.Tool, grammar map[string]string) ([]item, error) {
	var out []item

	normalize := func(id string, source ai.AssistantMessage) string {
		return normalizeToolCallID(id, source, model)
	}
	msgs := apishared.TransformMessages(c.Messages, model, normalize)

	if c.SystemPrompt != "" {
		role := "system"
		if model.Reasoning && cm.SupportsDeveloperRole {
			role = "developer"
		}
		out = append(out, item{Role: role, Content: apishared.SanitizeSurrogates(c.SystemPrompt)})
	}

	loaded := map[string]bool{}
	for msgIndex, msg := range msgs {
		switch m := msg.(type) {
		case ai.UserMessage:
			if converted, ok := convertUser(m); ok {
				out = append(out, converted)
			}
		case ai.AssistantMessage:
			converted, err := convertAssistant(m, model, msgIndex, grammar)
			if err != nil {
				return nil, err
			}
			out = append(out, converted...)
		case ai.ToolResultMessage:
			out = append(out, convertToolResult(m, model, grammar))
			loadedItems, err := loadDeferredTools(m, deferred, loaded, cm)
			if err != nil {
				return nil, err
			}
			out = append(out, loadedItems...)
		}
	}
	return out, nil
}

func convertUser(m ai.UserMessage) (item, bool) {
	if m.Content.Blocks == nil {
		return item{Role: "user", Content: []contentPart{{
			Type: "input_text", Text: apishared.SanitizeSurrogates(m.Content.Text),
		}}}, true
	}

	var parts []contentPart
	for _, block := range m.Content.Blocks {
		switch b := block.(type) {
		case ai.TextContent:
			parts = append(parts, contentPart{Type: "input_text", Text: apishared.SanitizeSurrogates(b.Text)})
		case ai.ImageContent:
			parts = append(parts, contentPart{Type: "input_image", Detail: "auto", ImageURL: dataURL(b)})
		}
	}
	if len(parts) == 0 {
		return item{}, false
	}
	return item{Role: "user", Content: parts}, true
}

func dataURL(img ai.ImageContent) string {
	return "data:" + img.MimeType + ";base64," + img.Data
}

// convertAssistant expands one assistant turn into its output items.
func convertAssistant(m ai.AssistantMessage, model *ai.Model, msgIndex int, grammar map[string]string) ([]item, error) {
	// A turn from a different model of the SAME provider and API is the awkward
	// case: its ids are real ids the endpoint issued, but for another model, so
	// the reasoning-pairing validation would reject them. Dropping the item id
	// leaves the call replayable without claiming a pairing.
	differentModel := m.Model != model.ID && m.Provider == model.Provider && m.Api == model.Api

	var out []item
	textIndex := 0
	for _, block := range m.Content {
		switch b := block.(type) {
		case ai.ThinkingContent:
			// Reasoning is replayed only as the opaque item the wire gave us.
			// A reconstructed one would be missing the encrypted payload the
			// model needs to continue its own train of thought.
			if b.ThinkingSignature != "" && json.Valid([]byte(b.ThinkingSignature)) {
				out = append(out, item{Raw: json.RawMessage(b.ThinkingSignature)})
			}

		case ai.TextContent:
			id, phase := parseTextSignature(b.TextSignature)
			if id == "" {
				id = fmt.Sprintf("msg_tau_%d", msgIndex)
				if textIndex > 0 {
					id = fmt.Sprintf("msg_tau_%d_%d", msgIndex, textIndex)
				}
			} else if len(id) > maxIDLen {
				id = "msg_" + shortHash(id)
			}
			textIndex++

			out = append(out, item{
				Type: "message", Role: "assistant", Status: "completed", ID: id, Phase: phase,
				Content: []contentPart{{
					Type: "output_text", Text: apishared.SanitizeSurrogates(b.Text), Annotations: []any{},
				}},
			})

		case ai.ToolCall:
			callID, itemID := splitToolCallID(b.ID)
			if property, isGrammar := grammar[b.Name]; isGrammar {
				input, err := apishared.GrammarToolInput(b.Name, b.Arguments, property)
				if err != nil {
					return nil, err
				}
				// A custom-tool item id is ctc_, not fc_, so an id from any
				// other shape is dropped rather than replayed as a mismatch.
				if differentModel || !strings.HasPrefix(itemID, "ctc_") {
					itemID = ""
				}
				out = append(out, item{
					Type: "custom_tool_call", ID: itemID, CallID: callID,
					Name: b.Name, Input: apishared.SanitizeSurrogates(input),
				})
				continue
			}
			// Only an fc_ id is a real function-call item; anything else came
			// from a wire that names them differently and must not be replayed
			// as one.
			if differentModel || !strings.HasPrefix(itemID, "fc_") {
				itemID = ""
			}
			args, err := json.Marshal(b.Arguments)
			if err != nil {
				args = []byte("{}")
			}
			out = append(out, item{
				Type: "function_call", ID: itemID, CallID: callID,
				Name: b.Name, Arguments: string(args),
			})
		}
	}
	return out, nil
}

func splitToolCallID(id string) (callID, itemID string) {
	if sep := strings.Index(id, "|"); sep >= 0 {
		return id[:sep], id[sep+1:]
	}
	return id, ""
}

func convertToolResult(m ai.ToolResultMessage, model *ai.Model, grammar map[string]string) item {
	callID, _ := splitToolCallID(m.ToolCallID)
	// The result of a grammar call must be paired with the item type the call
	// used. Sending a function_call_output for a custom_tool_call leaves the
	// call unanswered as far as the host is concerned.
	if _, isGrammar := grammar[m.ToolName]; isGrammar {
		return item{Type: "custom_tool_call_output", CallID: callID, Output: toolResultOutput(m, model)}
	}
	return item{Type: "function_call_output", CallID: callID, Output: toolResultOutput(m, model)}
}

// toolResultOutput renders a tool result, attaching images only where the
// model can see them. A silent result gets an explicit placeholder: several
// hosts reject an empty output outright.
func toolResultOutput(m ai.ToolResultMessage, model *ai.Model) any {
	var texts []string
	var images []ai.ImageContent
	for _, block := range m.Content {
		switch b := block.(type) {
		case ai.TextContent:
			texts = append(texts, b.Text)
		case ai.ImageContent:
			images = append(images, b)
		}
	}
	text := strings.Join(texts, "\n")

	if len(images) == 0 || !model.SupportsImageInput() {
		switch {
		case text != "":
			return apishared.SanitizeSurrogates(text)
		case len(images) > 0:
			return "(see attached image)"
		default:
			return "(no tool output)"
		}
	}

	var parts []contentPart
	if text != "" {
		parts = append(parts, contentPart{Type: "input_text", Text: apishared.SanitizeSurrogates(text)})
	}
	for _, img := range images {
		parts = append(parts, contentPart{Type: "input_image", Detail: "auto", ImageURL: dataURL(img)})
	}
	return parts
}

// loadDeferredTools replays the tool-search exchange that made a tool
// available mid-conversation.
//
// Deferred tools are not sent up front; the transcript records that a tool
// result introduced them. Replaying the search call and its output is what
// tells the model those tools exist on this turn — without it the model would
// see itself calling a tool it was never offered.
func loadDeferredTools(m ai.ToolResultMessage, deferred map[string]ai.Tool, loaded map[string]bool, cm compat) ([]item, error) {
	if len(deferred) == 0 || len(m.AddedToolNames) == 0 {
		return nil, nil
	}

	var tools []ai.Tool
	var names []string
	for _, name := range m.AddedToolNames {
		tool, ok := deferred[name]
		if !ok || loaded[name] {
			continue
		}
		loaded[name] = true
		tools = append(tools, tool)
		names = append(names, name)
	}
	if len(tools) == 0 {
		return nil, nil
	}

	converted, err := convertTools(tools, cm, true)
	if err != nil {
		return nil, err
	}

	searchID := "tau_tool_load_" + shortHash(m.ToolCallID+":"+strings.Join(names, ","))
	return []item{
		{
			Type: "tool_search_call", CallID: searchID,
			Status: "completed", Execution: "client",
			Arguments: map[string]any{"query": strings.Join(names, " "), "limit": len(names)},
		},
		{
			Type: "tool_search_output", CallID: searchID,
			Status: "completed", Execution: "client",
			Tools: converted,
		},
	}, nil
}
