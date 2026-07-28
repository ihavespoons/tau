package ai

import (
	"context"
	"encoding/json"
	"iter"
	"sync"
)

// EventType enumerates the assistant-message stream event kinds.
type EventType string

const (
	EventStart         EventType = "start"
	EventTextStart     EventType = "text_start"
	EventTextDelta     EventType = "text_delta"
	EventTextEnd       EventType = "text_end"
	EventThinkingStart EventType = "thinking_start"
	EventThinkingDelta EventType = "thinking_delta"
	EventThinkingEnd   EventType = "thinking_end"
	EventToolCallStart EventType = "toolcall_start"
	EventToolCallDelta EventType = "toolcall_delta"
	EventToolCallEnd   EventType = "toolcall_end"
	EventDone          EventType = "done"
	EventError         EventType = "error"
)

// Event is one assistant-message stream event. Which fields are set depends on
// Type, mirroring Pi's AssistantMessageEvent union:
//
//   - start:                       Partial
//   - text/thinking/toolcall_start: ContentIndex, Partial
//   - text/thinking/toolcall_delta: ContentIndex, Delta, Partial
//   - text_end, thinking_end:      ContentIndex, Content, Partial
//   - toolcall_end:                ContentIndex, ToolCall, Partial
//   - done:                        Reason (stop|length|toolUse), Message
//   - error:                       Reason (error|aborted), Error
type Event struct {
	Type         EventType
	ContentIndex int
	Delta        string
	Content      string
	ToolCall     *ToolCall
	Partial      *AssistantMessage
	Reason       StopReason
	Message      *AssistantMessage
	Error        *AssistantMessage
}

// IsTerminal reports whether the event ends the stream.
func (e Event) IsTerminal() bool { return e.Type == EventDone || e.Type == EventError }

// FinalMessage returns the terminal message of a done/error event, or nil.
func (e Event) FinalMessage() *AssistantMessage {
	switch e.Type {
	case EventDone:
		return e.Message
	case EventError:
		return e.Error
	default:
		return nil
	}
}

// MarshalJSON emits the exact Pi wire shape for each event kind.
func (e Event) MarshalJSON() ([]byte, error) {
	switch e.Type {
	case EventStart:
		return json.Marshal(struct {
			Type    EventType         `json:"type"`
			Partial *AssistantMessage `json:"partial"`
		}{e.Type, e.Partial})
	case EventTextStart, EventThinkingStart, EventToolCallStart:
		return json.Marshal(struct {
			Type         EventType         `json:"type"`
			ContentIndex int               `json:"contentIndex"`
			Partial      *AssistantMessage `json:"partial"`
		}{e.Type, e.ContentIndex, e.Partial})
	case EventTextDelta, EventThinkingDelta, EventToolCallDelta:
		return json.Marshal(struct {
			Type         EventType         `json:"type"`
			ContentIndex int               `json:"contentIndex"`
			Delta        string            `json:"delta"`
			Partial      *AssistantMessage `json:"partial"`
		}{e.Type, e.ContentIndex, e.Delta, e.Partial})
	case EventTextEnd, EventThinkingEnd:
		return json.Marshal(struct {
			Type         EventType         `json:"type"`
			ContentIndex int               `json:"contentIndex"`
			Content      string            `json:"content"`
			Partial      *AssistantMessage `json:"partial"`
		}{e.Type, e.ContentIndex, e.Content, e.Partial})
	case EventToolCallEnd:
		return json.Marshal(struct {
			Type         EventType         `json:"type"`
			ContentIndex int               `json:"contentIndex"`
			ToolCall     *ToolCall         `json:"toolCall"`
			Partial      *AssistantMessage `json:"partial"`
		}{e.Type, e.ContentIndex, e.ToolCall, e.Partial})
	case EventDone:
		return json.Marshal(struct {
			Type    EventType         `json:"type"`
			Reason  StopReason        `json:"reason"`
			Message *AssistantMessage `json:"message"`
		}{e.Type, e.Reason, e.Message})
	case EventError:
		return json.Marshal(struct {
			Type   EventType         `json:"type"`
			Reason StopReason        `json:"reason"`
			Error  *AssistantMessage `json:"error"`
		}{e.Type, e.Reason, e.Error})
	default:
		type alias Event
		return json.Marshal(alias(e))
	}
}

// MessageStream is the tau port of Pi's AssistantMessageEventStream: an
// unbounded event queue with a blocking final result. Producers Push events;
// the stream completes when a done or error event is pushed. Like Pi's,
// consuming the events is optional — Result alone never deadlocks a producer.
//
// The producer contract (Pi's StreamFunction contract): a stream function must
// never fail out-of-band. All failures are encoded as a terminal error event
// whose Error message has StopReason "error" or "aborted" and an ErrorMessage.
type MessageStream struct {
	mu     sync.Mutex
	queue  []Event
	closed bool
	wake   chan struct{} // 1-buffered; signals waiting consumer
	done   chan struct{} // closed on terminal event
	final  *AssistantMessage
}

// NewMessageStream creates an open stream.
func NewMessageStream() *MessageStream {
	return &MessageStream{
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
}

// Push appends an event. Pushing a terminal event completes the stream;
// events pushed after completion are dropped (mirroring Pi).
//
// Partial is snapshotted here. Producers accumulate into one message and
// mutate it as deltas arrive — which is safe in Pi because JavaScript is
// single-threaded, but a data race in Go where the producer runs on its own
// goroutine. Snapshotting centrally makes every provider safe by construction
// and makes Event.Partial's contract true: it is a copy owned by the consumer,
// valid as of the moment it was emitted.
func (s *MessageStream) Push(ev Event) {
	if ev.Partial != nil {
		ev.Partial = ev.Partial.Clone()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, ev)
	if ev.IsTerminal() {
		s.closed = true
		s.final = ev.FinalMessage()
	}
	closed := s.closed
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
	if closed {
		close(s.done)
	}
}

// Events iterates the stream's events in order, ending after the terminal
// done/error event. Intended for a single consumer.
func (s *MessageStream) Events() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for {
			s.mu.Lock()
			if len(s.queue) > 0 {
				ev := s.queue[0]
				s.queue = s.queue[1:]
				s.mu.Unlock()
				if !yield(ev) {
					return
				}
				continue
			}
			if s.closed {
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
			<-s.wake
		}
	}
}

// Result blocks until the stream completes and returns the terminal message
// (the successful message for done, the error-carrying message for error).
func (s *MessageStream) Result() *AssistantMessage {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.final
}

// Done returns a channel closed when the stream has completed.
func (s *MessageStream) Done() <-chan struct{} { return s.done }

// StreamFunc is the uniform contract of a wire-API implementation. Once
// invoked it must not fail out-of-band: request, model, and runtime failures
// are encoded in the returned stream as a terminal error event. Cancellation
// of ctx must terminate the stream with StopReason "aborted".
type StreamFunc func(ctx context.Context, model *Model, c Context, opts *StreamOptions) *MessageStream

// SimpleStreamFunc is StreamFunc with normalized cross-provider options
// (reasoning levels mapped per model).
type SimpleStreamFunc func(ctx context.Context, model *Model, c Context, opts *SimpleStreamOptions) *MessageStream

// ErrorMessage builds the terminal AssistantMessage for a failure, preserving
// any partial content accumulated so far (Pi does the same).
func ErrorMessage(partial *AssistantMessage, reason StopReason, errMsg string) *AssistantMessage {
	m := partial
	if m == nil {
		m = &AssistantMessage{Content: ContentList{}}
	}
	m.StopReason = reason
	m.ErrorMessage = errMsg
	return m
}
