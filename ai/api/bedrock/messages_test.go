package bedrock

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// bodyOf runs one turn against a stand-in server and returns the JSON Bedrock
// would have received. Asserting on the wire form rather than on Go structs is
// what makes these tests catch a field the SDK renames or drops.
func bodyOf(t *testing.T, model *ai.Model, c ai.Context, opts *Options) map[string]any {
	t.Helper()
	url, cap := serve(t, encodeFrames(t, []frame{
		messageStart(), textDelta(0, "ok"), blockStop(0), messageStop("end_turn"),
	}))
	model.BaseURL = url
	if opts == nil {
		opts = &Options{}
	}
	if opts.Env == nil {
		opts.Env = testEnv()
	}
	_, msg := collect(t, Stream(t.Context(), model, c, opts))
	if msg.StopReason == ai.StopError {
		t.Fatalf("request failed: %s", msg.ErrorMessage)
	}
	if cap.Body == nil {
		t.Fatal("no request body was captured")
	}
	return cap.Body
}

func messagesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		out = append(out, m.(map[string]any))
	}
	return out
}

func contentOf(t *testing.T, message map[string]any) []map[string]any {
	t.Helper()
	raw, _ := message["content"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, b := range raw {
		out = append(out, b.(map[string]any))
	}
	return out
}

// THE POINT: Bedrock requires every tool result from one round in a single user
// message. Emitting one message per result is rejected outright, so parallel
// tool calls would fail while a single call kept working.
func TestConsecutiveToolResultsCollapseIntoOneMessage(t *testing.T) {
	model := testModel("")
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "do both"}},
		ai.AssistantMessage{
			Provider: "amazon-bedrock", Api: ai.ApiBedrockConverse, Model: model.ID,
			StopReason: ai.StopToolUse,
			Content: ai.ContentList{
				ai.ToolCall{ID: "a", Name: "alpha", Arguments: map[string]any{}},
				ai.ToolCall{ID: "b", Name: "beta", Arguments: map[string]any{}},
			},
		},
		ai.ToolResultMessage{ToolCallID: "a", ToolName: "alpha", Content: ai.ContentList{ai.TextContent{Text: "one"}}},
		ai.ToolResultMessage{ToolCallID: "b", ToolName: "beta", Content: ai.ContentList{ai.TextContent{Text: "two"}}},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	if len(messages) != 3 {
		t.Fatalf("want user, assistant, one combined result message; got %d", len(messages))
	}
	results := contentOf(t, messages[2])
	// The cache breakpoint rides along on the same message.
	toolResults := 0
	for _, block := range results {
		if _, ok := block["toolResult"]; ok {
			toolResults++
		}
	}
	if toolResults != 2 {
		t.Errorf("want both results in one message, got %d: %v", toolResults, results)
	}
}

// An errored tool result must say so: Bedrock uses the status to tell the model
// the call failed, and defaulting to success teaches it the opposite.
func TestAFailedToolResultCarriesErrorStatus(t *testing.T) {
	model := testModel("")
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "go"}},
		ai.AssistantMessage{
			Provider: "amazon-bedrock", Api: ai.ApiBedrockConverse, Model: model.ID,
			StopReason: ai.StopToolUse,
			Content:    ai.ContentList{ai.ToolCall{ID: "a", Name: "alpha", Arguments: map[string]any{}}},
		},
		ai.ToolResultMessage{
			ToolCallID: "a", ToolName: "alpha", IsError: true,
			Content: ai.ContentList{ai.TextContent{Text: "boom"}},
		},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	result := contentOf(t, messages[2])[0]["toolResult"].(map[string]any)
	if result["status"] != "error" {
		t.Errorf("status %v, want error", result["status"])
	}
}

// THE POINT: Bedrock rejects empty text blocks and empty content arrays. A turn
// that legitimately produced nothing still has to say something, or the whole
// conversation becomes unreplayable.
//
// Both user-content shapes are covered because they take different paths: a
// plain string never becomes a block list, so a placeholder applied to only one
// of them leaves the other able to send an empty message.
func TestEmptyContentBecomesAPlaceholder(t *testing.T) {
	cases := []struct {
		name    string
		content ai.UserContent
	}{
		{"block list", ai.UserContent{Blocks: ai.ContentList{ai.TextContent{Text: "   "}}}},
		{"plain string", ai.UserContent{Text: "   "}},
		{"empty block list", ai.UserContent{Blocks: ai.ContentList{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: tc.content}}}
			messages := messagesOf(t, bodyOf(t, testModel(""), c, nil))
			blocks := contentOf(t, messages[0])
			if len(blocks) == 0 {
				t.Fatal("an empty content array was sent")
			}
			if blocks[0]["text"] != emptyTextPlaceholder {
				t.Errorf("content %v, want the placeholder", blocks)
			}
		})
	}
}

// THE POINT: a cache breakpoint belongs on a user turn. Bedrock rejects one on
// an assistant message, so a context that ends mid-turn — which is exactly what
// resuming a conversation looks like — would fail outright.
func TestNoTrailingCachePointOnAnAssistantTurn(t *testing.T) {
	model := testModel("")
	c := ai.Context{
		SystemPrompt: "system",
		Messages: ai.MessageList{
			ai.UserMessage{Content: ai.UserContent{Text: "go"}},
			ai.AssistantMessage{
				Provider: "amazon-bedrock", Api: ai.ApiBedrockConverse, Model: model.ID,
				StopReason: ai.StopStop,
				Content:    ai.ContentList{ai.TextContent{Text: "partial answer"}},
			},
		},
	}

	messages := messagesOf(t, bodyOf(t, model, c, &Options{
		StreamOptions: ai.StreamOptions{Env: testEnv(), CacheRetention: ai.CacheShort},
	}))
	last := messages[len(messages)-1]
	if last["role"] != "assistant" {
		t.Fatalf("expected the transcript to end on an assistant turn: %v", last)
	}
	for _, block := range contentOf(t, last) {
		if _, present := block["cachePoint"]; present {
			t.Errorf("a cache point was placed on an assistant turn: %v", block)
		}
	}
}

// A tool result with nothing in it gets the same treatment.
func TestAnEmptyToolResultBecomesAPlaceholder(t *testing.T) {
	model := testModel("")
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "go"}},
		ai.AssistantMessage{
			Provider: "amazon-bedrock", Api: ai.ApiBedrockConverse, Model: model.ID,
			StopReason: ai.StopToolUse,
			Content:    ai.ContentList{ai.ToolCall{ID: "a", Name: "alpha", Arguments: map[string]any{}}},
		},
		ai.ToolResultMessage{ToolCallID: "a", ToolName: "alpha", Content: ai.ContentList{}},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	result := contentOf(t, messages[2])[0]["toolResult"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != emptyTextPlaceholder {
		t.Errorf("tool result content %v", content)
	}
}

// THE POINT: signatures arrive after the thinking text, so an interrupted turn
// carries thinking with no signature — and Bedrock rejects that. Replaying it
// as plain text keeps the reasoning instead of losing the whole turn.
func TestUnsignedThinkingReplaysAsText(t *testing.T) {
	model := testModel("")
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "go"}},
		ai.AssistantMessage{
			Provider: "amazon-bedrock", Api: ai.ApiBedrockConverse, Model: model.ID,
			StopReason: ai.StopStop,
			Content: ai.ContentList{
				ai.ThinkingContent{Thinking: "unsigned reasoning"},
				ai.TextContent{Text: "answer"},
			},
		},
		ai.UserMessage{Content: ai.UserContent{Text: "again"}},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	blocks := contentOf(t, messages[1])
	if _, present := blocks[0]["reasoningContent"]; present {
		t.Fatalf("unsigned thinking was replayed as reasoning: %v", blocks[0])
	}
	if blocks[0]["text"] != "unsigned reasoning" {
		t.Errorf("blocks: %v", blocks)
	}
}

// Signed thinking from the same model replays as reasoning, signature intact —
// that is what buys multi-turn continuity.
func TestSignedThinkingReplaysWithItsSignature(t *testing.T) {
	model := testModel("")
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "go"}},
		ai.AssistantMessage{
			Provider: "amazon-bedrock", Api: ai.ApiBedrockConverse, Model: model.ID,
			StopReason: ai.StopStop,
			Content: ai.ContentList{
				ai.ThinkingContent{Thinking: "reasoned", ThinkingSignature: "sig-value"},
			},
		},
		ai.UserMessage{Content: ai.UserContent{Text: "again"}},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	reasoning, ok := contentOf(t, messages[1])[0]["reasoningContent"].(map[string]any)
	if !ok {
		t.Fatalf("blocks: %v", contentOf(t, messages[1]))
	}
	text, _ := reasoning["reasoningText"].(map[string]any)
	if text["text"] != "reasoned" || text["signature"] != "sig-value" {
		t.Errorf("reasoningText %v", text)
	}
}

// THE POINT: every non-Claude model on Bedrock rejects the signature field with
// an explicit validation error, so it must be omitted for them even when the
// transcript has one.
func TestNonClaudeModelsGetNoThinkingSignature(t *testing.T) {
	model := testModel("")
	model.ID, model.Name = "deepseek.r1-v1:0", "DeepSeek R1"

	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "go"}},
		ai.AssistantMessage{
			Provider: "amazon-bedrock", Api: ai.ApiBedrockConverse, Model: model.ID,
			StopReason: ai.StopStop,
			Content: ai.ContentList{
				ai.ThinkingContent{Thinking: "reasoned", ThinkingSignature: "sig-value"},
			},
		},
		ai.UserMessage{Content: ai.UserContent{Text: "again"}},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	reasoning, ok := contentOf(t, messages[1])[0]["reasoningContent"].(map[string]any)
	if !ok {
		t.Fatalf("blocks: %v", contentOf(t, messages[1]))
	}
	text, _ := reasoning["reasoningText"].(map[string]any)
	if text["text"] != "reasoned" {
		t.Errorf("reasoningText %v", text)
	}
	if sig, present := text["signature"]; present {
		t.Errorf("a signature was sent to a non-Claude model: %v", sig)
	}
}

// An image travels as raw bytes, not the base64 the transcript stores.
func TestImagesAreSentAsBytes(t *testing.T) {
	model := testModel("")
	// A one-pixel PNG is enough; what matters is that it decodes.
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Blocks: ai.ContentList{
			ai.TextContent{Text: "look"},
			ai.ImageContent{MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(raw)},
		}}},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	blocks := contentOf(t, messages[0])
	image, ok := blocks[1]["image"].(map[string]any)
	if !ok {
		t.Fatalf("blocks: %v", blocks)
	}
	if image["format"] != "png" {
		t.Errorf("format %v", image["format"])
	}
	// The SDK re-encodes bytes as base64 on the wire; what matters is that they
	// round-trip to the original bytes.
	source, _ := image["source"].(map[string]any)
	got, err := base64.StdEncoding.DecodeString(source["bytes"].(string))
	if err != nil {
		t.Fatalf("bytes did not decode: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("image bytes round-tripped to %v", got)
	}
}

// An image tau cannot name a format for must fail loudly rather than being
// dropped: a silently missing image makes the model answer about nothing.
func TestAnUnknownImageTypeFails(t *testing.T) {
	url, _ := serve(t, encodeFrames(t, []frame{messageStart(), messageStop("end_turn")}))
	model := testModel(url)
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Blocks: ai.ContentList{
			ai.ImageContent{MimeType: "image/tiff", Data: "AAAA"},
		}}},
	}}

	_, msg := collect(t, Stream(t.Context(), model, c, &Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))
	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "image/tiff") {
		t.Errorf("error should name the type: %q", msg.ErrorMessage)
	}
}

// THE POINT: Bedrock accepts only word characters and dashes in a toolUseId,
// capped at 64. A call replayed from another provider carries ids in other
// shapes, and an unsanitized one is rejected for the whole conversation.
func TestForeignToolCallIDsAreSanitized(t *testing.T) {
	cases := []struct{ in, want string }{
		{"call_abc-123", "call_abc-123"},
		{"call:abc.123", "call_abc_123"},
		{"toolu_01ABC/xyz+", "toolu_01ABC_xyz_"},
		{strings.Repeat("x", 80), strings.Repeat("x", 64)},
	}
	for _, tc := range cases {
		if got := normalizeToolCallID(tc.in, ai.AssistantMessage{}); got != tc.want {
			t.Errorf("normalizeToolCallID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A sanitized id has to reach the matching tool result too, or the pairing
// breaks and Bedrock rejects the turn.
func TestASanitizedIDReachesItsToolResult(t *testing.T) {
	model := testModel("")
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "go"}},
		// A different provider's turn, so the ids get normalized.
		ai.AssistantMessage{
			Provider: "openai", Api: ai.ApiOpenAICompletions, Model: "gpt-5",
			StopReason: ai.StopToolUse,
			Content:    ai.ContentList{ai.ToolCall{ID: "call:abc.123", Name: "alpha", Arguments: map[string]any{}}},
		},
		ai.ToolResultMessage{ToolCallID: "call:abc.123", ToolName: "alpha", Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	use := contentOf(t, messages[1])[0]["toolUse"].(map[string]any)
	result := contentOf(t, messages[2])[0]["toolResult"].(map[string]any)
	if use["toolUseId"] != "call_abc_123" {
		t.Errorf("toolUseId %v", use["toolUseId"])
	}
	if result["toolUseId"] != use["toolUseId"] {
		t.Errorf("result id %v does not match the call id %v", result["toolUseId"], use["toolUseId"])
	}
}

// An aborted turn can leave an assistant message with no content, and Bedrock
// rejects an empty content array. Dropping the message is the only way to keep
// the conversation replayable.
func TestEmptyAssistantMessagesAreDropped(t *testing.T) {
	model := testModel("")
	c := ai.Context{Messages: ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Text: "go"}},
		ai.AssistantMessage{
			Provider: "amazon-bedrock", Api: ai.ApiBedrockConverse, Model: model.ID,
			StopReason: ai.StopStop, Content: ai.ContentList{ai.TextContent{Text: "  "}},
		},
		ai.UserMessage{Content: ai.UserContent{Text: "again"}},
	}}

	messages := messagesOf(t, bodyOf(t, model, c, nil))
	for _, m := range messages {
		if m["role"] == "assistant" {
			t.Fatalf("an all-whitespace assistant turn was sent: %v", m)
		}
	}
	if len(messages) != 2 {
		t.Errorf("want the two user turns, got %d", len(messages))
	}
}

// A strict tool is only declared strict when the model can enforce it.
func TestStrictSamplingFollowsTheModel(t *testing.T) {
	tool := ai.Tool{
		Name: "structured", Description: "d",
		ConstrainedSampling: &ai.ConstrainedSampling{Type: "json_schema", Strict: "prefer"},
	}

	model := testModel("")
	yes := true
	model.Compat = &ai.CompatFlags{SupportsStrictMode: &yes}
	body := bodyOf(t, model, ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "go"}}},
		Tools:    []ai.Tool{tool},
	}, nil)
	spec := firstToolSpec(t, body)
	if spec["strict"] != true {
		t.Errorf("strict was not sent to a model that supports it: %v", spec)
	}

	plain := testModel("")
	body = bodyOf(t, plain, ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "go"}}},
		Tools:    []ai.Tool{tool},
	}, nil)
	spec = firstToolSpec(t, body)
	if _, present := spec["strict"]; present {
		t.Errorf("strict was sent to a model that cannot enforce it: %v", spec)
	}
}

// A tool that REQUIRES constrained sampling must not degrade quietly: the tool
// has been told it will never receive unconstrained arguments.
func TestARequiredStrictToolFailsOnAModelThatCannotEnforceIt(t *testing.T) {
	url, _ := serve(t, encodeFrames(t, []frame{messageStart(), messageStop("end_turn")}))
	_, msg := collect(t, Stream(t.Context(), testModel(url), ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "go"}}},
		Tools: []ai.Tool{{
			Name:                "structured",
			ConstrainedSampling: &ai.ConstrainedSampling{Type: "json_schema", Strict: "require"},
		}},
	}, &Options{StreamOptions: ai.StreamOptions{Env: testEnv()}}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "structured") {
		t.Errorf("error should name the tool: %q", msg.ErrorMessage)
	}
}

// toolChoice "none" means send no tools at all, which is how Converse expresses
// it — there is no "none" choice value.
func TestToolChoiceNoneSendsNoTools(t *testing.T) {
	body := bodyOf(t, testModel(""), ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "go"}}},
		Tools:    []ai.Tool{{Name: "alpha", Description: "d"}},
	}, &Options{ToolChoice: ToolChoice{Type: ToolChoiceNone}})

	if _, present := body["toolConfig"]; present {
		t.Errorf("toolConfig was sent for choice none: %v", body["toolConfig"])
	}
}

// A named tool choice has to carry the name, or the model picks freely.
func TestANamedToolChoiceCarriesItsName(t *testing.T) {
	body := bodyOf(t, testModel(""), ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "go"}}},
		Tools:    []ai.Tool{{Name: "alpha", Description: "d"}},
	}, &Options{ToolChoice: ToolChoice{Type: ToolChoiceTool, Name: "alpha"}})

	config, _ := body["toolConfig"].(map[string]any)
	choice, _ := config["toolChoice"].(map[string]any)
	named, ok := choice["tool"].(map[string]any)
	if !ok || named["name"] != "alpha" {
		t.Errorf("toolChoice %v", choice)
	}
}

func firstToolSpec(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	config, _ := body["toolConfig"].(map[string]any)
	tools, _ := config["tools"].([]any)
	if len(tools) == 0 {
		raw, _ := json.Marshal(body)
		t.Fatalf("no tools in payload: %s", raw)
	}
	spec, _ := tools[0].(map[string]any)["toolSpec"].(map[string]any)
	return spec
}
