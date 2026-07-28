package agent

import (
	"context"

	"github.com/ihavespoons/tau/ai"
)

// EventType enumerates agent-loop events.
type EventType string

const (
	EventAgentStart EventType = "agent_start"
	EventAgentEnd   EventType = "agent_end"
	EventTurnStart  EventType = "turn_start"
	EventTurnEnd    EventType = "turn_end"

	EventMessageStart  EventType = "message_start"
	EventMessageUpdate EventType = "message_update"
	EventMessageEnd    EventType = "message_end"

	EventToolExecutionStart  EventType = "tool_execution_start"
	EventToolExecutionUpdate EventType = "tool_execution_update"
	EventToolExecutionEnd    EventType = "tool_execution_end"
)

// Event is one agent-loop event. Which fields are set depends on Type:
//
//   - agent_start:            (none)
//   - agent_end:              Messages
//   - turn_start:             (none)
//   - turn_end:               Message, ToolResults
//   - message_start/end:      Message
//   - message_update:         Message, StreamEvent
//   - tool_execution_start:   ToolCallID, ToolName, Args
//   - tool_execution_update:  ToolCallID, ToolName, Args, PartialResult
//   - tool_execution_end:     ToolCallID, ToolName, Result, IsError
type Event struct {
	Type EventType

	// Messages is the full set of messages produced by the run (agent_end).
	Messages []ai.Message
	// Message is the subject message (turn_end, message_*).
	Message ai.Message
	// ToolResults accompany turn_end.
	ToolResults []ai.ToolResultMessage
	// StreamEvent is the underlying provider event (message_update).
	StreamEvent *ai.Event

	ToolCallID    string
	ToolName      string
	Args          map[string]any
	PartialResult *ToolResult
	Result        *ToolResult
	IsError       bool
}

// Sink receives agent events. It is invoked synchronously and in order, so a
// slow sink backpressures the loop — the Go equivalent of Pi's awaited
// listeners. A sink that must not block should hand off to its own goroutine.
//
// A sink returning an error aborts the run.
type Sink func(ctx context.Context, ev Event) error

// emitTo fans one event out to sinks in registration order, stopping at the
// first error.
func emitTo(ctx context.Context, sinks []Sink, ev Event) error {
	for _, s := range sinks {
		if s == nil {
			continue
		}
		if err := s(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}
