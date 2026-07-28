package coding

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
)

// JSONEvent is one line of `tau --mode json` output. The stream is JSONL:
// exactly one event per line, LF-terminated, so consumers can parse
// incrementally without buffering the whole run.
type JSONEvent struct {
	Type string `json:"type"`

	// Message carries the subject message for message_* and turn_end.
	Message json.RawMessage `json:"message,omitempty"`
	// Delta is the incremental text or thinking for message_update, so
	// consumers can render progressively without reassembling the message.
	Delta string `json:"delta,omitempty"`
	// DeltaKind is "text" or "thinking" when Delta is set.
	DeltaKind string `json:"deltaKind,omitempty"`

	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       map[string]any  `json:"args,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`

	// Usage and Cost accompany the terminal event.
	Usage *ai.Usage `json:"usage,omitempty"`
	// Error is set on the terminal event when the run failed.
	Error string `json:"error,omitempty"`
	// SessionPath is emitted once at start so a caller can resume later.
	SessionPath string `json:"sessionPath,omitempty"`
	Model       string `json:"model,omitempty"`
}

// JSONWriter serializes agent events as JSONL.
type JSONWriter struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
	err error
}

// NewJSONWriter builds a JSONL event writer.
func NewJSONWriter(w io.Writer) *JSONWriter {
	enc := json.NewEncoder(w)
	// One event per line; no HTML escaping so tool output stays readable.
	enc.SetEscapeHTML(false)
	return &JSONWriter{w: w, enc: enc}
}

// Err returns the first write failure, if any.
func (j *JSONWriter) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// Emit writes one event.
func (j *JSONWriter) Emit(ev JSONEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return
	}
	if err := j.enc.Encode(ev); err != nil {
		j.err = err
	}
}

// Sink returns an agent.Sink that renders the run as JSONL.
func (j *JSONWriter) Sink() agent.Sink {
	return func(_ context.Context, ev agent.Event) error {
		switch ev.Type {
		case agent.EventAgentStart:
			j.Emit(JSONEvent{Type: "agent_start"})
		case agent.EventAgentEnd:
			j.Emit(JSONEvent{Type: "agent_end"})
		case agent.EventTurnStart:
			j.Emit(JSONEvent{Type: "turn_start"})
		case agent.EventTurnEnd:
			j.Emit(JSONEvent{Type: "turn_end", Message: marshalMessage(ev.Message)})
		case agent.EventMessageStart:
			j.Emit(JSONEvent{Type: "message_start", Message: marshalMessage(ev.Message)})
		case agent.EventMessageEnd:
			j.Emit(JSONEvent{Type: "message_end", Message: marshalMessage(ev.Message)})
		case agent.EventMessageUpdate:
			// Only deltas are interesting mid-stream; emitting the whole
			// partial message per token would be unreadable and quadratic.
			if ev.StreamEvent == nil {
				return nil
			}
			switch ev.StreamEvent.Type {
			case ai.EventTextDelta:
				j.Emit(JSONEvent{Type: "message_update", DeltaKind: "text", Delta: ev.StreamEvent.Delta})
			case ai.EventThinkingDelta:
				j.Emit(JSONEvent{Type: "message_update", DeltaKind: "thinking", Delta: ev.StreamEvent.Delta})
			}
		case agent.EventToolExecutionStart:
			j.Emit(JSONEvent{
				Type: "tool_execution_start", ToolCallID: ev.ToolCallID,
				ToolName: ev.ToolName, Args: ev.Args,
			})
		case agent.EventToolExecutionEnd:
			j.Emit(JSONEvent{
				Type: "tool_execution_end", ToolCallID: ev.ToolCallID, ToolName: ev.ToolName,
				Result: marshalResult(ev.Result), IsError: ev.IsError,
			})
		}
		return nil
	}
}

func marshalMessage(m ai.Message) json.RawMessage {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

func marshalResult(r *agent.ToolResult) json.RawMessage {
	if r == nil {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}
