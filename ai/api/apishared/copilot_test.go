package apishared

import (
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func user(text string) ai.UserMessage {
	return ai.UserMessage{Content: ai.UserContent{Text: text}}
}

func userWithImage() ai.UserMessage {
	return ai.UserMessage{Content: ai.UserContent{Blocks: ai.ContentList{
		ai.TextContent{Text: "what is this"},
		ai.ImageContent{Data: "aGk=", MimeType: "image/png"},
	}}}
}

// THE POINT: Copilot bills and rate-limits against X-Initiator. A follow-up
// turn the agent started on its own is not a user request, and reporting every
// turn as user-initiated misstates what the subscription is being used for.
func TestTheInitiatorDistinguishesUserFromAgentTurns(t *testing.T) {
	cases := map[string]struct {
		messages ai.MessageList
		want     string
	}{
		"a user message": {
			ai.MessageList{user("hi")}, "user",
		},
		"the agent continuing after a tool result": {
			ai.MessageList{
				user("hi"),
				ai.AssistantMessage{Content: ai.ContentList{ai.ToolCall{ID: "c", Name: "bash"}}},
				ai.ToolResultMessage{ToolCallID: "c", ToolName: "bash"},
			},
			"agent",
		},
		"the agent following up on its own answer": {
			ai.MessageList{user("hi"), ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "ok"}}}},
			"agent",
		},
		"an empty transcript": {nil, "user"},
	}

	for name, tc := range cases {
		got := CopilotDynamicHeaders(tc.messages)["X-Initiator"]
		if got != tc.want {
			t.Errorf("%s: X-Initiator %q, want %q", name, got, tc.want)
		}
	}
}

// THE POINT: Copilot refuses image content unless the request says it is a
// vision request — and the refusal names the model, not the missing header, so
// it reads as "this model cannot see images" and sends the user looking in the
// wrong place entirely.
func TestTheVisionHeaderIsSetWhenTheTurnCarriesImages(t *testing.T) {
	withImage := CopilotDynamicHeaders(ai.MessageList{userWithImage()})
	if withImage["Copilot-Vision-Request"] != "true" {
		t.Errorf("headers %+v", withImage)
	}

	// An image in a TOOL RESULT counts: a screenshot tool is the common way
	// one gets there.
	fromTool := CopilotDynamicHeaders(ai.MessageList{
		user("screenshot it"),
		ai.ToolResultMessage{
			ToolCallID: "c", ToolName: "screenshot",
			Content: ai.ContentList{ai.ImageContent{Data: "aGk=", MimeType: "image/png"}},
		},
	})
	if fromTool["Copilot-Vision-Request"] != "true" {
		t.Errorf("headers %+v", fromTool)
	}
}

// The header is absent, not false: it is a request for a capability, and
// sending it on every turn asks for vision handling the turn does not need.
func TestTheVisionHeaderIsAbsentWithoutImages(t *testing.T) {
	headers := CopilotDynamicHeaders(ai.MessageList{
		user("hi"),
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "hello"}}},
	})
	if _, present := headers["Copilot-Vision-Request"]; present {
		t.Errorf("headers %+v", headers)
	}
	// The other two are unconditional.
	if headers["Openai-Intent"] != "conversation-edits" || headers["X-Initiator"] == "" {
		t.Errorf("headers %+v", headers)
	}
}

// A plain-text user message has no blocks at all, and reading its content as a
// block list would panic rather than report no images.
func TestAPlainTextTranscriptHasNoImages(t *testing.T) {
	if hasImageInput(ai.MessageList{user("hi"), user("still no images")}) {
		t.Error("a text-only transcript reported images")
	}
}
