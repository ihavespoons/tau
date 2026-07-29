package googlegenai

import (
	"encoding/json"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func modelFor(id, baseURL string) *ai.Model {
	return &ai.Model{
		ID: id, Name: id, Api: ai.ApiGoogleGenerativeAI,
		Provider: "google", BaseURL: baseURL,
		Input: []string{"text", "image"}, ContextWindow: 1000000, MaxTokens: 8192,
	}
}

func reasoningModel(id, baseURL string) *ai.Model {
	m := modelFor(id, baseURL)
	m.Reasoning = true
	return m
}

func payloadFor(t *testing.T, model *ai.Model, c ai.Context, opts *Options) map[string]any {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	raw, err := json.Marshal(buildRequest(model, c, opts))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// assistantTurn builds a turn as tau records one: provider, api and model all
// set. Omitting the api makes the transform treat it as another model's, which
// is exactly the case these tests want to distinguish.
func assistantTurn(model *ai.Model, content ...ai.Content) ai.AssistantMessage {
	return ai.AssistantMessage{
		Content: content, Provider: model.Provider, Api: model.Api, Model: model.ID,
	}
}

func simpleContext() ai.Context {
	return ai.Context{
		SystemPrompt: "be helpful",
		Messages:     ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}},
	}
}

func contents(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	raw, _ := payload["contents"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		out = append(out, c.(map[string]any))
	}
	return out
}

func partsOf(t *testing.T, turn map[string]any) []map[string]any {
	t.Helper()
	raw, _ := turn["parts"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.(map[string]any))
	}
	return out
}

// The system prompt is its own field here, not a turn — putting it in contents
// would make the model treat it as something the user said.
func TestSystemPromptIsItsOwnField(t *testing.T) {
	payload := payloadFor(t, modelFor("gemini-3-pro-preview", ""), simpleContext(), nil)

	sys, ok := payload["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction: %#v", payload["systemInstruction"])
	}
	if text := partsOf(t, sys)[0]["text"]; text != "be helpful" {
		t.Errorf("text: %v", text)
	}
	if len(contents(t, payload)) != 1 {
		t.Error("the system prompt must not also appear as a turn")
	}
}

// THE POINT: a thought signature is an encrypted handle to reasoning THIS
// model did. Replaying another model's is not merely useless — the endpoint
// cannot decrypt it and rejects the request.
func TestThoughtSignaturesOnlySurviveTheSameModel(t *testing.T) {
	model := reasoningModel("gemini-3-pro-preview", "")
	const signature = "c2lnbmF0dXJl" // valid base64

	same := assistantTurn(model, ai.TextContent{Text: "hello", TextSignature: signature})
	other := same
	other.Model = "gemini-3-flash"

	t.Run("kept for the same model", func(t *testing.T) {
		c := simpleContext()
		c.Messages = append(c.Messages, same)
		turn := contents(t, payloadFor(t, model, c, nil))[1]
		if got := partsOf(t, turn)[0]["thoughtSignature"]; got != signature {
			t.Errorf("signature: %v", got)
		}
	})

	t.Run("dropped for another model", func(t *testing.T) {
		c := simpleContext()
		c.Messages = append(c.Messages, other)
		turn := contents(t, payloadFor(t, model, c, nil))[1]
		if _, present := partsOf(t, turn)[0]["thoughtSignature"]; present {
			t.Error("another model's signature was replayed")
		}
	})
}

// Google types the signature field as bytes, so a value that is not base64 is
// rejected by the API rather than ignored.
func TestInvalidSignaturesAreDropped(t *testing.T) {
	model := reasoningModel("gemini-3-pro-preview", "")
	// Each of these fails to decode: illegal characters, a length that is not
	// a multiple of four, and padding in the middle.
	for _, sig := range []string{"not base64!", "abc", "ab=c"} {
		c := simpleContext()
		c.Messages = append(c.Messages, assistantTurn(model, ai.TextContent{Text: "hello", TextSignature: sig}))
		turn := contents(t, payloadFor(t, model, c, nil))[1]
		if _, present := partsOf(t, turn)[0]["thoughtSignature"]; present {
			t.Errorf("an invalid signature %q was sent", sig)
		}
	}
}

// Another model's reasoning replays as plain text — with no wrapper tags,
// which would invite this model to imitate them.
func TestForeignThinkingBecomesPlainText(t *testing.T) {
	model := reasoningModel("gemini-3-pro-preview", "")
	c := simpleContext()
	c.Messages = append(c.Messages, ai.AssistantMessage{
		Content:  ai.ContentList{ai.ThinkingContent{Thinking: "considering"}},
		Provider: "anthropic", Model: "claude-sonnet-5", Api: ai.ApiAnthropicMessages,
	})

	part := partsOf(t, contents(t, payloadFor(t, model, c, nil))[1])[0]
	if part["text"] != "considering" {
		t.Errorf("text: %v", part["text"])
	}
	if _, present := part["thought"]; present {
		t.Error("another model's reasoning must not claim to be this model's thinking")
	}
}

// THE POINT: Google requires every function response from one round in a
// SINGLE user turn. One turn per result is rejected — which only shows up once
// the model calls two tools at once.
func TestParallelToolResultsMergeIntoOneTurn(t *testing.T) {
	model := modelFor("gemini-3-pro-preview", "")
	c := simpleContext()
	c.Messages = append(c.Messages,
		assistantTurn(model,
			ai.ToolCall{ID: "a", Name: "read", Arguments: map[string]any{}},
			ai.ToolCall{ID: "b", Name: "ls", Arguments: map[string]any{}}),
		ai.ToolResultMessage{ToolCallID: "a", ToolName: "read", Content: ai.ContentList{ai.TextContent{Text: "one"}}},
		ai.ToolResultMessage{ToolCallID: "b", ToolName: "ls", Content: ai.ContentList{ai.TextContent{Text: "two"}}},
	)

	turns := contents(t, payloadFor(t, model, c, nil))
	last := turns[len(turns)-1]
	if last["role"] != "user" {
		t.Fatalf("role: %v", last["role"])
	}
	parts := partsOf(t, last)
	if len(parts) != 2 {
		t.Fatalf("both responses must share one turn, got %d turns of parts: %#v", len(parts), turns)
	}
}

// A failed tool reported under "output" reads to the model as a success whose
// result happens to describe a failure.
func TestToolErrorsUseTheErrorKey(t *testing.T) {
	model := modelFor("gemini-3-pro-preview", "")
	c := simpleContext()
	c.Messages = append(c.Messages,
		assistantTurn(model, ai.ToolCall{ID: "a", Name: "read", Arguments: map[string]any{}}),
		ai.ToolResultMessage{
			ToolCallID: "a", ToolName: "read", IsError: true,
			Content: ai.ContentList{ai.TextContent{Text: "no such file"}},
		},
	)

	turns := contents(t, payloadFor(t, model, c, nil))
	resp := partsOf(t, turns[len(turns)-1])[0]["functionResponse"].(map[string]any)
	response := resp["response"].(map[string]any)
	if _, ok := response["error"]; !ok {
		t.Errorf("a failed tool must report under error: %#v", response)
	}
}

// Tool-call ids are only sent for the models that need them; Gemini pairs
// calls to responses positionally and rejects an unexpected id.
func TestToolCallIDsOnlyForModelsThatNeedThem(t *testing.T) {
	cases := []struct {
		id     string
		wantID bool
	}{
		{"gemini-3-pro-preview", false},
		{"claude-sonnet-4-5", true},
		{"gpt-oss-120b", true},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			model := modelFor(tc.id, "")
			c := simpleContext()
			c.Messages = append(c.Messages, ai.AssistantMessage{
				Content:  ai.ContentList{ai.ToolCall{ID: "call_1", Name: "read", Arguments: map[string]any{}}},
				Provider: "google", Model: model.ID,
			})

			part := partsOf(t, contents(t, payloadFor(t, model, c, nil))[1])[0]
			call := part["functionCall"].(map[string]any)
			_, present := call["id"]
			if present != tc.wantID {
				t.Errorf("id present = %v, want %v", present, tc.wantID)
			}
		})
	}
}

// Each Gemini generation exposes a different thinking control, and using the
// wrong one is rejected rather than ignored.
func TestThinkingControlPerGeneration(t *testing.T) {
	cases := []struct {
		name, model  string
		level        ai.ThinkingLevel
		wantLevel    string
		wantBudget   int
		expectBudget bool
	}{
		{"gemini 3 pro takes a level", "gemini-3-pro-preview", "high", "HIGH", 0, false},
		{"gemini 3 pro collapses low", "gemini-3-pro-preview", "minimal", "LOW", 0, false},
		{"gemma 4 takes a level", "gemma-4-31b-it", "low", "MINIMAL", 0, false},
		{"gemini 2.5 pro takes a budget", "gemini-2.5-pro", "medium", "", 8192, true},
		{"gemini 2.5 flash has its own budgets", "gemini-2.5-flash", "high", "", 24576, true},
		{"an unknown model lets the model decide", "gemini-2.0-experiment", "high", "", -1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := payloadFor(t, reasoningModel(tc.model, ""), simpleContext(),
				&Options{Reasoning: tc.level})
			gen := payload["generationConfig"].(map[string]any)
			cfg := gen["thinkingConfig"].(map[string]any)

			if cfg["includeThoughts"] != true {
				t.Error("thinking was requested but summaries were not")
			}
			if tc.expectBudget {
				if got := cfg["thinkingBudget"]; got != float64(tc.wantBudget) {
					t.Errorf("thinkingBudget: %v, want %d", got, tc.wantBudget)
				}
				return
			}
			if got := cfg["thinkingLevel"]; got != tc.wantLevel {
				t.Errorf("thinkingLevel: %v, want %q", got, tc.wantLevel)
			}
		})
	}
}

// THE POINT: Gemini 3 cannot be stopped from thinking. Asking for a zero
// budget is rejected, so "off" means the lowest level with summaries withheld
// — the model still reasons, tau just does not show it.
func TestThinkingOffPerGeneration(t *testing.T) {
	cases := []struct {
		model, wantLevel string
		wantZeroBudget   bool
	}{
		{"gemini-3-pro-preview", "LOW", false},
		{"gemini-3-flash", "MINIMAL", false},
		{"gemini-2.5-pro", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			payload := payloadFor(t, reasoningModel(tc.model, ""), simpleContext(), nil)
			cfg := payload["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)

			if _, present := cfg["includeThoughts"]; present {
				t.Error("thinking is off, so summaries must not be requested")
			}
			if tc.wantZeroBudget {
				if got := cfg["thinkingBudget"]; got != float64(0) {
					t.Errorf("thinkingBudget: %v, want 0", got)
				}
				return
			}
			if got := cfg["thinkingLevel"]; got != tc.wantLevel {
				t.Errorf("thinkingLevel: %v, want %q", got, tc.wantLevel)
			}
		})
	}
}

// A non-reasoning model gets no thinking config at all.
func TestNoThinkingConfigForNonReasoningModels(t *testing.T) {
	payload := payloadFor(t, modelFor("gemini-2.0-flash", ""), simpleContext(), &Options{Reasoning: "high"})
	if gen, ok := payload["generationConfig"].(map[string]any); ok {
		if _, present := gen["thinkingConfig"]; present {
			t.Error("a non-reasoning model was sent a thinking config")
		}
	}
}

// VALIDATED enforcement stops a model inventing a call with half its arguments
// missing, but only Gemini 3 accepts the mode.
func TestFunctionCallingMode(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{{Name: "read", Description: "read a file"}}

	t.Run("gemini 3 validates", func(t *testing.T) {
		payload := payloadFor(t, modelFor("gemini-3-pro-preview", ""), c, nil)
		cfg := payload["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
		if cfg["mode"] != "VALIDATED" {
			t.Errorf("mode: %v", cfg["mode"])
		}
	})

	t.Run("gemini 2 does not", func(t *testing.T) {
		payload := payloadFor(t, modelFor("gemini-2.5-pro", ""), c, nil)
		if _, present := payload["toolConfig"]; present {
			t.Errorf("toolConfig: %#v", payload["toolConfig"])
		}
	})

	t.Run("an explicit choice wins", func(t *testing.T) {
		payload := payloadFor(t, modelFor("gemini-3-pro-preview", ""), c, &Options{ToolChoice: "none"})
		cfg := payload["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
		if cfg["mode"] != "NONE" {
			t.Errorf("mode: %v", cfg["mode"])
		}
	})
}

// Full JSON Schema is the default; the legacy field is validated as OpenAPI
// 3.0 and rejects the meta-keywords a schema generator emits.
func TestToolSchemaFields(t *testing.T) {
	c := simpleContext()
	c.Tools = []ai.Tool{{Name: "read", Description: "read a file"}}

	t.Run("json schema by default", func(t *testing.T) {
		payload := payloadFor(t, modelFor("gemini-3-pro-preview", ""), c, nil)
		decl := payload["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
		if _, present := decl["parametersJsonSchema"]; !present {
			t.Errorf("declaration: %#v", decl)
		}
	})

	t.Run("legacy parameters when asked", func(t *testing.T) {
		payload := payloadFor(t, modelFor("gemini-3-pro-preview", ""), c, &Options{UseLegacyParameters: true})
		decl := payload["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
		if _, present := decl["parameters"]; !present {
			t.Errorf("declaration: %#v", decl)
		}
		if _, present := decl["parametersJsonSchema"]; present {
			t.Error("both schema fields were sent")
		}
	})
}

// The OpenAPI dialect rejects schema meta-keywords outright.
func TestSchemaMetaKeywordsAreStripped(t *testing.T) {
	raw := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"type":        "object",
		"$defs":       map[string]any{"x": true},
		"definitions": map[string]any{"y": true},
		"properties": map[string]any{
			"a": map[string]any{"$comment": "note", "type": "string"},
		},
	}
	cleaned := sanitizeForOpenAPI(raw).(map[string]any)

	for _, gone := range []string{"$schema", "$defs", "definitions"} {
		if _, present := cleaned[gone]; present {
			t.Errorf("%s survived", gone)
		}
	}
	if cleaned["type"] != "object" {
		t.Error("a real keyword was stripped")
	}
	// Nested too: a meta-keyword anywhere in the tree is rejected.
	nested := cleaned["properties"].(map[string]any)["a"].(map[string]any)
	if _, present := nested["$comment"]; present {
		t.Error("a nested meta-keyword survived")
	}
	if nested["type"] != "string" {
		t.Error("a nested real keyword was stripped")
	}
}

// resolveSignature is a second line of defence: by the time a message reaches
// it, the shared transform has already stripped signatures from anything not
// from this exact model. Asserting it directly is the only way to cover it,
// and it stays because a change to that transform must not silently start
// leaking signatures the endpoint cannot decrypt.
func TestResolveSignature(t *testing.T) {
	const valid = "c2lnbmF0dXJl"

	cases := []struct {
		name      string
		sameModel bool
		signature string
		want      string
	}{
		{"kept for the same model", true, valid, valid},
		{"dropped for another model", false, valid, ""},
		{"empty stays empty", true, "", ""},
		{"undecodable is dropped", true, "not base64!", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveSignature(tc.sameModel, tc.signature); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
