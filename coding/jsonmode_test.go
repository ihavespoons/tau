package coding

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/faux"
)

// decodeJSONL parses the emitted stream, asserting each line is a complete
// JSON object — the property downstream consumers rely on.
func decodeJSONL(t *testing.T, out string) []JSONEvent {
	t.Helper()
	var evs []JSONEvent
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev JSONEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", line, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func TestJSONModeEmitsOneEventPerLine(t *testing.T) {
	var buf bytes.Buffer
	jw := NewJSONWriter(&buf)

	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "hello world", DeltaSize: 5}}})
	a := agent.NewAgent(agent.Options{
		Model: faux.Model(), Stream: script.StreamSimple,
	})
	a.Subscribe(jw.Sink())

	if _, err := a.Prompt(context.Background(), userMsg("hi")); err != nil {
		t.Fatal(err)
	}
	if err := jw.Err(); err != nil {
		t.Fatal(err)
	}

	evs := decodeJSONL(t, buf.String())
	if len(evs) == 0 {
		t.Fatal("no events emitted")
	}
	if evs[0].Type != "agent_start" {
		t.Errorf("first event = %s, want agent_start", evs[0].Type)
	}
	if evs[len(evs)-1].Type != "agent_end" {
		t.Errorf("last event = %s, want agent_end", evs[len(evs)-1].Type)
	}

	// Streaming deltas arrive as text, and concatenate to the full response.
	var text strings.Builder
	for _, ev := range evs {
		if ev.Type == "message_update" && ev.DeltaKind == "text" {
			text.WriteString(ev.Delta)
		}
	}
	if text.String() != "hello world" {
		t.Errorf("concatenated deltas = %q, want the full text", text.String())
	}
}

func TestJSONModeEmitsToolLifecycle(t *testing.T) {
	var buf bytes.Buffer
	jw := NewJSONWriter(&buf)

	tool := agent.MustNew("ping", "ping", "pings",
		func(context.Context, string, struct{}, agent.UpdateFunc) (agent.ToolResult, error) {
			return agent.Text("pong"), nil
		})
	script := faux.NewScript(
		toolCallTurn("ping", map[string]any{}),
		faux.Turn{Blocks: []faux.Block{{Text: "done"}}},
	)
	a := agent.NewAgent(agent.Options{
		Model: faux.Model(), Stream: script.StreamSimple, Tools: []agent.Tool{tool},
	})
	a.Subscribe(jw.Sink())

	if _, err := a.Prompt(context.Background(), userMsg("go")); err != nil {
		t.Fatal(err)
	}

	evs := decodeJSONL(t, buf.String())
	var start, end *JSONEvent
	for i := range evs {
		switch evs[i].Type {
		case "tool_execution_start":
			start = &evs[i]
		case "tool_execution_end":
			end = &evs[i]
		}
	}
	if start == nil || end == nil {
		t.Fatal("missing tool lifecycle events")
	}
	if start.ToolName != "ping" || start.ToolCallID == "" {
		t.Errorf("start = %+v", start)
	}
	if end.IsError {
		t.Error("tool should have succeeded")
	}
	if !strings.Contains(string(end.Result), "pong") {
		t.Errorf("result = %s", end.Result)
	}
}

func TestJSONModeMessagesRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	jw := NewJSONWriter(&buf)

	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "hi"}}})
	a := agent.NewAgent(agent.Options{Model: faux.Model(), Stream: script.StreamSimple})
	a.Subscribe(jw.Sink())
	if _, err := a.Prompt(context.Background(), userMsg("hello")); err != nil {
		t.Fatal(err)
	}

	// Every emitted message must decode back into a real message.
	for _, ev := range decodeJSONL(t, buf.String()) {
		if len(ev.Message) == 0 {
			continue
		}
		m, err := ai.UnmarshalMessage(ev.Message)
		if err != nil {
			t.Errorf("%s carried an undecodable message: %v", ev.Type, err)
			continue
		}
		if m.Role() == "" {
			t.Errorf("%s message has no role", ev.Type)
		}
	}
}

// Tool output containing HTML or angle brackets must survive verbatim —
// escaping would corrupt code the consumer needs to read.
func TestJSONModeDoesNotEscapeHTML(t *testing.T) {
	var buf bytes.Buffer
	jw := NewJSONWriter(&buf)
	src := `if a < b && c > d { "x" }`
	jw.Emit(JSONEvent{Type: "test", Delta: src})

	// Go's encoder escapes < > & into \uXXXX form by default, which would
	// corrupt code the consumer needs to read back.
	escaped := "\\u00"
	if strings.Contains(buf.String(), escaped) {
		t.Errorf("output was HTML-escaped: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "a < b") {
		t.Errorf("literal angle bracket missing: %s", buf.String())
	}

	var ev JSONEvent
	if err := json.Unmarshal(buf.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Delta != src {
		t.Errorf("delta = %q, want %q", ev.Delta, src)
	}
}
