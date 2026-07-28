package ai

import (
	"encoding/json"
	"reflect"
	"testing"
)

// roundTrip unmarshals raw into a Message, re-marshals it, and asserts the
// result is semantically identical JSON (field-for-field, Pi wire shape).
func roundTrip(t *testing.T, raw string) {
	t.Helper()
	msg, err := UnmarshalMessage(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round trip mismatch:\n want %s\n got  %s", raw, out)
	}
}

func TestUserMessageStringContentRoundTrip(t *testing.T) {
	roundTrip(t, `{"role":"user","content":"hello","timestamp":1753600000000}`)
}

func TestUserMessageBlockContentRoundTrip(t *testing.T) {
	roundTrip(t, `{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","data":"aGk=","mimeType":"image/png"}],"timestamp":1753600000000}`)
}

func TestAssistantMessageRoundTrip(t *testing.T) {
	roundTrip(t, `{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","thinkingSignature":"sig1"},{"type":"text","text":"hi"},{"type":"toolCall","id":"tc_1","name":"bash","arguments":{"command":"ls"}}],"api":"anthropic-messages","provider":"anthropic","model":"claude-sonnet-5","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"cacheWrite1h":0,"totalTokens":15,"cost":{"input":0.1,"output":0.2,"cacheRead":0,"cacheWrite":0,"total":0.3}},"stopReason":"toolUse","timestamp":1753600000001}`)
}

func TestAssistantErrorMessageRoundTrip(t *testing.T) {
	roundTrip(t, `{"role":"assistant","content":[],"api":"anthropic-messages","provider":"anthropic","model":"m","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"error","errorMessage":"boom","timestamp":1}`)
}

func TestToolResultMessageRoundTrip(t *testing.T) {
	roundTrip(t, `{"role":"toolResult","toolCallId":"tc_1","toolName":"bash","content":[{"type":"text","text":"ok"}],"details":{"exitCode":0},"isError":false,"timestamp":2}`)
}

func TestUnknownContentTypeErrors(t *testing.T) {
	_, err := UnmarshalContent(json.RawMessage(`{"type":"martian"}`))
	if err == nil {
		t.Fatal("expected error for unknown content type")
	}
}

func TestUserContentString(t *testing.T) {
	u := UserContent{Blocks: ContentList{TextContent{Text: "a"}, ImageContent{Data: "x", MimeType: "image/png"}, TextContent{Text: "b"}}}
	if got := u.String(); got != "a\nb" {
		t.Errorf("String() = %q", got)
	}
}
