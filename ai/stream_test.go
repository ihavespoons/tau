package ai

import (
	"encoding/json"
	"testing"
	"time"
)

func unmarshalStrict(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

func TestMessageStreamOrderAndTermination(t *testing.T) {
	s := NewMessageStream()
	final := &AssistantMessage{StopReason: StopStop}
	go func() {
		s.Push(Event{Type: EventStart, Partial: final})
		s.Push(Event{Type: EventTextDelta, ContentIndex: 0, Delta: "hi", Partial: final})
		s.Push(Event{Type: EventDone, Reason: StopStop, Message: final})
		// Dropped after terminal:
		s.Push(Event{Type: EventTextDelta, Delta: "late"})
	}()
	var types []EventType
	for ev := range s.Events() {
		types = append(types, ev.Type)
	}
	if len(types) != 3 || types[0] != EventStart || types[1] != EventTextDelta || types[2] != EventDone {
		t.Errorf("events = %v", types)
	}
	if s.Result() != final {
		t.Error("Result() should return the terminal message")
	}
}

func TestMessageStreamResultWithoutConsumingEvents(t *testing.T) {
	s := NewMessageStream()
	final := &AssistantMessage{StopReason: StopError, ErrorMessage: "boom"}
	go func() {
		for i := 0; i < 100; i++ {
			s.Push(Event{Type: EventTextDelta, Delta: "x"})
		}
		s.Push(Event{Type: EventError, Reason: StopError, Error: final})
	}()
	done := make(chan *AssistantMessage, 1)
	go func() { done <- s.Result() }()
	select {
	case m := <-done:
		if m.ErrorMessage != "boom" {
			t.Errorf("final = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Result() deadlocked without an event consumer")
	}
}

func TestMessageStreamLateConsumerSeesAllEvents(t *testing.T) {
	s := NewMessageStream()
	final := &AssistantMessage{StopReason: StopStop}
	s.Push(Event{Type: EventStart, Partial: final})
	s.Push(Event{Type: EventDone, Reason: StopStop, Message: final})
	count := 0
	for range s.Events() {
		count++
	}
	if count != 2 {
		t.Errorf("late consumer saw %d events", count)
	}
}

func TestEventJSONShapes(t *testing.T) {
	partial := &AssistantMessage{Content: ContentList{}, Api: "a", Provider: "p", Model: "m", StopReason: StopPending}
	cases := []struct {
		ev   Event
		want map[string]bool // keys that must be present
	}{
		{Event{Type: EventStart, Partial: partial}, map[string]bool{"type": true, "partial": true}},
		{Event{Type: EventTextDelta, ContentIndex: 0, Delta: "d", Partial: partial}, map[string]bool{"type": true, "contentIndex": true, "delta": true, "partial": true}},
		{Event{Type: EventToolCallEnd, ContentIndex: 1, ToolCall: &ToolCall{ID: "t", Name: "n", Arguments: map[string]any{}}, Partial: partial}, map[string]bool{"type": true, "contentIndex": true, "toolCall": true, "partial": true}},
		{Event{Type: EventDone, Reason: StopStop, Message: partial}, map[string]bool{"type": true, "reason": true, "message": true}},
		{Event{Type: EventError, Reason: StopAborted, Error: partial}, map[string]bool{"type": true, "reason": true, "error": true}},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.ev)
		if err != nil {
			t.Fatalf("%s: %v", c.ev.Type, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for k := range c.want {
			if _, ok := m[k]; !ok {
				t.Errorf("%s: missing key %q in %s", c.ev.Type, k, b)
			}
		}
		if len(m) != len(c.want) {
			t.Errorf("%s: extra keys in %s", c.ev.Type, b)
		}
	}
}

// Producers accumulate into one message and mutate it as deltas arrive. The
// stream must hand consumers a snapshot, or every provider races the consumer
// (this is safe in Pi only because JavaScript is single-threaded).
func TestPushSnapshotsPartial(t *testing.T) {
	s := NewMessageStream()
	live := &AssistantMessage{Content: ContentList{TextContent{Text: "a"}}}

	s.Push(Event{Type: EventTextDelta, Delta: "a", Partial: live})

	// Producer keeps mutating the message it owns.
	live.Content[0] = TextContent{Text: "ab"}
	live.Content = append(live.Content, TextContent{Text: "second block"})
	live.StopReason = StopStop

	var seen Event
	for ev := range s.Events() {
		seen = ev
		break
	}
	if seen.Partial == live {
		t.Fatal("consumer received the producer's live message, not a snapshot")
	}
	if len(seen.Partial.Content) != 1 {
		t.Errorf("snapshot content len = %d, want 1 (later appends must not leak)", len(seen.Partial.Content))
	}
	if got := seen.Partial.Content[0].(TextContent).Text; got != "a" {
		t.Errorf("snapshot text = %q, want %q (later mutation must not leak)", got, "a")
	}
	if seen.Partial.StopReason == StopStop {
		t.Error("later stop-reason mutation leaked into the snapshot")
	}
}
