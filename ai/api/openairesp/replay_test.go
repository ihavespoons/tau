package openairesp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// reasoningItem is the shape the endpoint returns and tau must give back
// unchanged.
const reasoningItem = `{"type":"reasoning","id":"rs_abc","summary":[{"type":"summary_text","text":"thinking"}],` +
	`"encrypted_content":"ENCRYPTED_PAYLOAD"}`

func assistantTurn(model *ai.Model, content ...ai.Content) ai.AssistantMessage {
	return ai.AssistantMessage{
		Content:  content,
		Api:      model.Api,
		Provider: model.Provider,
		Model:    model.ID,
	}
}

// THE POINT: reasoning is opaque. The model continues its own train of thought
// only if the item comes back byte-for-byte, encrypted payload included —
// re-encoding it from a struct drops whatever tau does not model, and the
// failure surfaces a turn later as degraded reasoning nobody can trace.
func TestReasoningIsReplayedVerbatim(t *testing.T) {
	model := reasoningModel("openai", "https://api.openai.com/v1", nil)
	c := simpleContext()
	c.Messages = append(c.Messages, assistantTurn(model,
		ai.ThinkingContent{Thinking: "thinking", ThinkingSignature: reasoningItem}))

	payload := payloadFor(t, model, c, nil)
	raw, err := json.Marshal(payload["input"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ENCRYPTED_PAYLOAD") {
		t.Fatalf("the encrypted payload was lost: %s", raw)
	}

	var found map[string]any
	for _, it := range items(t, payload) {
		if it["type"] == "reasoning" {
			found = it
		}
	}
	if found == nil {
		t.Fatal("no reasoning item was replayed")
	}
	if found["id"] != "rs_abc" {
		t.Errorf("the item id changed: %v", found["id"])
	}
}

// Thinking with no signature cannot be replayed as reasoning: a reconstructed
// item has no payload and the endpoint rejects it.
func TestUnsignedThinkingIsNotReplayed(t *testing.T) {
	model := reasoningModel("openai", "https://api.openai.com/v1", nil)
	c := simpleContext()
	c.Messages = append(c.Messages, assistantTurn(model, ai.ThinkingContent{Thinking: "no signature"}))

	// Counting is the assertion: an unsigned block must contribute NOTHING,
	// not merely something that fails to look like reasoning. An empty item is
	// still an item, and the endpoint rejects one.
	baseline := len(items(t, payloadFor(t, model, simpleContext(), nil)))
	got := items(t, payloadFor(t, model, c, nil))
	if len(got) != baseline {
		t.Errorf("an unsigned thinking block produced %d extra items: %#v", len(got)-baseline, got)
	}
}

// An assistant message carries its own item id forward, so the endpoint sees
// the same conversation it produced.
func TestAssistantTextKeepsItsItemID(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	c := simpleContext()
	c.Messages = append(c.Messages, assistantTurn(model,
		ai.TextContent{Text: "hello", TextSignature: encodeTextSignature("msg_real", "final_answer")}))

	var message map[string]any
	for _, it := range items(t, payloadFor(t, model, c, nil)) {
		if it["type"] == "message" {
			message = it
		}
	}
	if message == nil {
		t.Fatal("no assistant message was replayed")
	}
	if message["id"] != "msg_real" {
		t.Errorf("id: %v", message["id"])
	}
	if message["phase"] != "final_answer" {
		t.Errorf("phase: %v", message["phase"])
	}
}

// A message with no signature still needs an id, and two blocks in one turn
// need different ones.
func TestUnsignedAssistantTextGetsDistinctIDs(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	c := simpleContext()
	c.Messages = append(c.Messages, assistantTurn(model,
		ai.TextContent{Text: "first"}, ai.TextContent{Text: "second"}))

	var ids []string
	for _, it := range items(t, payloadFor(t, model, c, nil)) {
		if it["type"] == "message" {
			ids = append(ids, it["id"].(string))
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected two messages, got %v", ids)
	}
	if ids[0] == ids[1] {
		t.Errorf("both blocks got the same id: %v", ids)
	}
}

// THE POINT: the endpoint validates that a function_call is paired with the
// reasoning item that produced it. A call replayed from another provider names
// an item id it never issued, so the id is rehashed into one that cannot
// collide — sidestepping the validation instead of failing it.
func TestForeignToolCallIDIsRehashed(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	c := simpleContext()

	foreign := assistantTurn(model, ai.ToolCall{
		ID: "call_1|fc_from_elsewhere", Name: "read", Arguments: map[string]any{},
	})
	foreign.Provider = "anthropic"
	foreign.Api = ai.ApiAnthropicMessages
	c.Messages = append(c.Messages,
		foreign,
		ai.ToolResultMessage{ToolCallID: "call_1|fc_from_elsewhere", ToolName: "read",
			Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	)

	var call map[string]any
	for _, it := range items(t, payloadFor(t, model, c, nil)) {
		if it["type"] == "function_call" {
			call = it
		}
	}
	if call == nil {
		t.Fatal("no function call was replayed")
	}
	id, _ := call["id"].(string)
	if id == "fc_from_elsewhere" {
		t.Error("a foreign item id was replayed as if the endpoint had issued it")
	}
	if id != "" && !strings.HasPrefix(id, "fc_") {
		t.Errorf("a function_call item id must start with fc_: %q", id)
	}
}

// A call from a DIFFERENT MODEL of the same provider has a real id, but for
// another model — so the pairing would still be rejected. Dropping the item id
// leaves the call replayable without claiming a pairing.
func TestDifferentModelToolCallDropsItsItemID(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	c := simpleContext()

	other := assistantTurn(model, ai.ToolCall{
		ID: "call_1|fc_real", Name: "read", Arguments: map[string]any{},
	})
	other.Model = "gpt-5.5"
	c.Messages = append(c.Messages,
		other,
		ai.ToolResultMessage{ToolCallID: "call_1|fc_real", ToolName: "read",
			Content: ai.ContentList{ai.TextContent{Text: "ok"}}},
	)

	for _, it := range items(t, payloadFor(t, model, c, nil)) {
		if it["type"] != "function_call" {
			continue
		}
		if _, present := it["id"]; present {
			t.Errorf("a different model's item id was replayed: %#v", it["id"])
		}
		if it["call_id"] != "call_1" {
			t.Errorf("call_id: %v", it["call_id"])
		}
	}
}

// A tool result is addressed by call id alone; the item id half is not part of
// the pairing and the endpoint rejects it.
func TestToolResultUsesOnlyTheCallID(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	c := simpleContext()
	c.Messages = append(c.Messages,
		assistantTurn(model, ai.ToolCall{ID: "call_1|fc_1", Name: "read", Arguments: map[string]any{}}),
		ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "read",
			Content: ai.ContentList{ai.TextContent{Text: "contents"}}},
	)

	var result map[string]any
	for _, it := range items(t, payloadFor(t, model, c, nil)) {
		if it["type"] == "function_call_output" {
			result = it
		}
	}
	if result == nil {
		t.Fatal("no tool result was replayed")
	}
	if result["call_id"] != "call_1" {
		t.Errorf("call_id: %v", result["call_id"])
	}
	if result["output"] != "contents" {
		t.Errorf("output: %v", result["output"])
	}
}

// A tool result with no text has to say something: several hosts reject an
// empty output outright.
func TestEmptyToolResultGetsAPlaceholder(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	c := simpleContext()
	c.Messages = append(c.Messages,
		assistantTurn(model, ai.ToolCall{ID: "call_1|fc_1", Name: "read", Arguments: map[string]any{}}),
		ai.ToolResultMessage{ToolCallID: "call_1|fc_1", ToolName: "read", Content: ai.ContentList{}},
	)

	for _, it := range items(t, payloadFor(t, model, c, nil)) {
		if it["type"] == "function_call_output" && it["output"] == "" {
			t.Error("an empty output was sent")
		}
	}
}

// Deferred tools become available mid-conversation, and the model has to be
// told so — otherwise it sees itself calling a tool it was never offered.
func TestDeferredToolsAreReplayedAsASearch(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	model.Compat = &ai.CompatFlags{SupportsToolSearch: boolptr(true)}

	c := simpleContext()
	c.Tools = []ai.Tool{{Name: "grep", Description: "search"}}
	c.Messages = append(c.Messages,
		assistantTurn(model, ai.ToolCall{ID: "call_1|fc_1", Name: "read", Arguments: map[string]any{}}),
		ai.ToolResultMessage{
			ToolCallID: "call_1|fc_1", ToolName: "read",
			Content:        ai.ContentList{ai.TextContent{Text: "ok"}},
			AddedToolNames: []string{"grep"},
		},
	)

	payload := payloadFor(t, model, c, nil)

	var call, output map[string]any
	for _, it := range items(t, payload) {
		switch it["type"] {
		case "tool_search_call":
			call = it
		case "tool_search_output":
			output = it
		}
	}
	if call == nil || output == nil {
		t.Fatal("the tool-search exchange was not replayed")
	}
	if call["call_id"] != output["call_id"] {
		t.Errorf("the search and its output must share a call id: %v vs %v", call["call_id"], output["call_id"])
	}
	tools, ok := output["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools: %#v", output["tools"])
	}
	if tool := tools[0].(map[string]any); tool["name"] != "grep" || tool["defer_loading"] != true {
		t.Errorf("tool: %#v", tool)
	}

	// A deferred tool must not also be offered up front, or the search that
	// introduces it is meaningless.
	if _, present := payload["tools"]; present {
		t.Errorf("a deferred tool was also sent as immediate: %#v", payload["tools"])
	}
}

// Without the compat flag there is no search mechanism, so every tool has to
// go up front or it is simply unavailable.
func TestWithoutToolSearchEverythingIsImmediate(t *testing.T) {
	model := modelFor("openai", "https://api.openai.com/v1")
	c := simpleContext()
	c.Tools = []ai.Tool{{Name: "grep", Description: "search"}}
	c.Messages = append(c.Messages,
		assistantTurn(model, ai.ToolCall{ID: "call_1|fc_1", Name: "read", Arguments: map[string]any{}}),
		ai.ToolResultMessage{
			ToolCallID: "call_1|fc_1", ToolName: "read",
			Content:        ai.ContentList{ai.TextContent{Text: "ok"}},
			AddedToolNames: []string{"grep"},
		},
	)

	payload := payloadFor(t, model, c, nil)
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools: %#v", payload["tools"])
	}
	for _, it := range items(t, payload) {
		if it["type"] == "tool_search_call" {
			t.Error("a tool search was replayed to a host that does not support one")
		}
	}
}

// A text signature written before the versioned envelope existed is a plain
// id, and a session recorded then still has to replay.
func TestLegacyTextSignatureStillParses(t *testing.T) {
	id, phase := parseTextSignature("msg_plain")
	if id != "msg_plain" || phase != "" {
		t.Errorf("id=%q phase=%q", id, phase)
	}
}

// An unrecognised phase is dropped rather than sent on: the endpoint validates
// the value.
func TestUnknownPhaseIsDropped(t *testing.T) {
	id, phase := parseTextSignature(`{"v":1,"id":"msg_1","phase":"speculation"}`)
	if id != "msg_1" {
		t.Errorf("id: %q", id)
	}
	if phase != "" {
		t.Errorf("an unknown phase should not be replayed: %q", phase)
	}
}
