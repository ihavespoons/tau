package pimessages

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/partialjson"
)

// wireEvent is one serialized assistant-message event as a pi-messages backend
// sends it. It is the flattened union of every event shape: which fields carry
// meaning depends on Type.
//
// The backend sends no `partial` — that is the client's running accumulation,
// rebuilt here from the deltas. Sending it would multiply the payload by the
// length of the message on every single delta.
type wireEvent struct {
	Type string `json:"type"`

	ContentIndex int    `json:"contentIndex"`
	Delta        string `json:"delta"`
	Content      string `json:"content"`
	// ContentSignature is the provider's opaque signature for the block. It
	// lands on TextSignature or ThinkingSignature depending on the block kind,
	// which is why the wire spells it neutrally.
	ContentSignature string `json:"contentSignature"`
	Redacted         bool   `json:"redacted"`

	ID       string       `json:"id"`
	ToolName string       `json:"toolName"`
	ToolCall *ai.ToolCall `json:"toolCall"`

	Reason       ai.StopReason `json:"reason"`
	Usage        *ai.Usage     `json:"usage"`
	ResponseID   string        `json:"responseId"`
	ErrorMessage string        `json:"errorMessage"`
	Rewrite      *rewrite      `json:"rewrite"`
}

// rewrite summarizes a server-side rewrite of the request — a gateway policy
// that edited the conversation before it reached the model. It is recorded as a
// diagnostic because the user is being billed for, and answered from, a context
// that is not the one tau sent.
type rewrite struct {
	PolicyID            string `json:"policyId"`
	PolicyVersion       int    `json:"policyVersion"`
	Changed             bool   `json:"changed"`
	TokenCountChange    int    `json:"tokenCountChange"`
	MessageCountChange  int    `json:"messageCountChange"`
	SystemPromptChanged bool   `json:"systemPromptChanged"`
}

func (r *rewrite) details() map[string]any {
	return map[string]any{
		"policyId":            r.PolicyID,
		"policyVersion":       r.PolicyVersion,
		"changed":             r.Changed,
		"tokenCountChange":    r.TokenCountChange,
		"messageCountChange":  r.MessageCountChange,
		"systemPromptChanged": r.SystemPromptChanged,
	}
}

// converter turns wire events into tau events, accumulating the message they
// describe.
//
// Every other wire in tau parses a foreign format, so a malformed stream fails
// on a shape mismatch. Here the server speaks tau's own vocabulary, and the
// events index directly into the content it is building — so a bad index or an
// unknown stop reason would be applied rather than rejected. Pi is written
// against a JavaScript array, where an out-of-range write silently creates a
// hole and the next delta throws "cannot read property of undefined". Go would
// either panic or allocate whatever length the server named. Both are worth
// replacing with a protocol error that says which event was wrong.
type converter struct {
	out      *ai.AssistantMessage
	toolJSON map[int]string
}

func newConverter(model *ai.Model) *converter {
	return &converter{
		out: &ai.AssistantMessage{
			Content:  ai.ContentList{},
			Api:      model.Api,
			Provider: model.Provider,
			Model:    model.ID,
			// Pi starts at "stop" and overwrites on the terminal event; tau
			// starts at "pending" like its other wires, so a stream that dies
			// mid-flight is never mistaken for one that finished cleanly.
			StopReason: ai.StopPending,
			Timestamp:  time.Now().UnixMilli(),
		},
		toolJSON: map[int]string{},
	}
}

// convert applies one wire event and returns the tau event to emit. A nil event
// means there is nothing to emit (the wire event was consumed silently).
func (c *converter) convert(ev *wireEvent) (*ai.Event, error) {
	switch ev.Type {
	case "start":
		return &ai.Event{Type: ai.EventStart, Partial: c.out}, nil

	case "text_start":
		if err := c.open(ev.ContentIndex, ai.TextContent{}); err != nil {
			return nil, err
		}
		return &ai.Event{Type: ai.EventTextStart, ContentIndex: ev.ContentIndex, Partial: c.out}, nil

	case "text_delta":
		block, err := blockAt[ai.TextContent](c, ev, "text")
		if err != nil {
			return nil, err
		}
		block.Text += ev.Delta
		c.out.Content[ev.ContentIndex] = block
		return &ai.Event{
			Type: ai.EventTextDelta, ContentIndex: ev.ContentIndex, Delta: ev.Delta, Partial: c.out,
		}, nil

	case "text_end":
		block, err := blockAt[ai.TextContent](c, ev, "text")
		if err != nil {
			return nil, err
		}
		// The end event carries the authoritative text, not another delta: a
		// provider may normalize what it streamed.
		block.Text = ev.Content
		block.TextSignature = ev.ContentSignature
		c.out.Content[ev.ContentIndex] = block
		return &ai.Event{
			Type: ai.EventTextEnd, ContentIndex: ev.ContentIndex, Content: ev.Content, Partial: c.out,
		}, nil

	case "thinking_start":
		if err := c.open(ev.ContentIndex, ai.ThinkingContent{}); err != nil {
			return nil, err
		}
		return &ai.Event{Type: ai.EventThinkingStart, ContentIndex: ev.ContentIndex, Partial: c.out}, nil

	case "thinking_delta":
		block, err := blockAt[ai.ThinkingContent](c, ev, "thinking")
		if err != nil {
			return nil, err
		}
		block.Thinking += ev.Delta
		c.out.Content[ev.ContentIndex] = block
		return &ai.Event{
			Type: ai.EventThinkingDelta, ContentIndex: ev.ContentIndex, Delta: ev.Delta, Partial: c.out,
		}, nil

	case "thinking_end":
		block, err := blockAt[ai.ThinkingContent](c, ev, "thinking")
		if err != nil {
			return nil, err
		}
		block.Thinking = ev.Content
		// The signature is what lets the next turn replay this reasoning, and
		// redacted thinking is nothing BUT its signature — dropping either
		// silently breaks multi-turn continuity rather than failing.
		block.ThinkingSignature = ev.ContentSignature
		block.Redacted = ev.Redacted
		c.out.Content[ev.ContentIndex] = block
		return &ai.Event{
			Type: ai.EventThinkingEnd, ContentIndex: ev.ContentIndex, Content: ev.Content, Partial: c.out,
		}, nil

	case "toolcall_start":
		call := ai.ToolCall{ID: ev.ID, Name: ev.ToolName, Arguments: map[string]any{}}
		if err := c.open(ev.ContentIndex, call); err != nil {
			return nil, err
		}
		c.toolJSON[ev.ContentIndex] = ""
		return &ai.Event{Type: ai.EventToolCallStart, ContentIndex: ev.ContentIndex, Partial: c.out}, nil

	case "toolcall_delta":
		block, err := blockAt[ai.ToolCall](c, ev, "toolCall")
		if err != nil {
			return nil, err
		}
		// Arguments stream as JSON text that is only valid once complete, so
		// each delta is re-parsed with salvage — the same treatment every other
		// wire gives them, and what makes a partial call renderable while it
		// is still arriving.
		accumulated := c.toolJSON[ev.ContentIndex] + ev.Delta
		c.toolJSON[ev.ContentIndex] = accumulated
		block.Arguments = partialjson.ParseStreaming(accumulated)
		c.out.Content[ev.ContentIndex] = block
		return &ai.Event{
			Type: ai.EventToolCallDelta, ContentIndex: ev.ContentIndex, Delta: ev.Delta, Partial: c.out,
		}, nil

	case "toolcall_end":
		if _, err := blockAt[ai.ToolCall](c, ev, "toolCall"); err != nil {
			return nil, err
		}
		if ev.ToolCall == nil {
			return nil, fmt.Errorf("toolcall_end at index %d carries no toolCall", ev.ContentIndex)
		}
		// The complete call replaces the salvaged one: its arguments parsed as
		// whole JSON, so a partial parse never reaches the tool.
		call := *ev.ToolCall
		c.out.Content[ev.ContentIndex] = call
		delete(c.toolJSON, ev.ContentIndex)
		return &ai.Event{
			Type: ai.EventToolCallEnd, ContentIndex: ev.ContentIndex, ToolCall: &call, Partial: c.out,
		}, nil

	case "done":
		if !terminalReason(ev.Reason, ai.StopStop, ai.StopLength, ai.StopToolUse) {
			return nil, fmt.Errorf("done event carries stop reason %q", ev.Reason)
		}
		c.finish(ev)
		return &ai.Event{Type: ai.EventDone, Reason: ev.Reason, Message: c.out}, nil

	case "error":
		if !terminalReason(ev.Reason, ai.StopError, ai.StopAborted) {
			return nil, fmt.Errorf("error event carries stop reason %q", ev.Reason)
		}
		c.finish(ev)
		c.out.ErrorMessage = ev.ErrorMessage
		return &ai.Event{Type: ai.EventError, Reason: ev.Reason, Error: c.out}, nil

	default:
		// An unknown event type is a newer backend, not a broken one. Ignoring
		// it keeps tau working against a gateway that has learned a new event.
		return nil, nil
	}
}

// finish applies the fields a terminal event carries.
func (c *converter) finish(ev *wireEvent) {
	c.out.StopReason = ev.Reason
	c.out.ResponseID = ev.ResponseID
	if ev.Usage != nil {
		// The usage arrives costed. The gateway prices its own catalog — for
		// Radius the model's rates come from the gateway's config in the first
		// place — so recomputing here from tau's copy would be the guess.
		c.out.Usage = *ev.Usage
	}
	if ev.Rewrite != nil {
		c.out.Diagnostics = append(c.out.Diagnostics, ai.Diagnostic{
			Type:      "pi_messages_rewrite",
			Timestamp: time.Now().UnixMilli(),
			Details:   ev.Rewrite.details(),
		})
	}
}

func terminalReason(reason ai.StopReason, allowed ...ai.StopReason) bool {
	for _, a := range allowed {
		if reason == a {
			return true
		}
	}
	return false
}

// open installs a new content block at index, which must be the next one.
//
// Requiring contiguity is what keeps a content index from being a length: an
// event naming index 2^31 would otherwise ask Go to allocate a slice that big
// before anything noticed the stream was nonsense.
func (c *converter) open(index int, block ai.Content) error {
	if index != len(c.out.Content) {
		return fmt.Errorf("content index %d is out of order; expected %d", index, len(c.out.Content))
	}
	c.out.Content = append(c.out.Content, block)
	return nil
}

// blockAt returns the block an event addresses, requiring that it exists and is
// the kind the event is for. A delta for a block that was never opened, or one
// that opened as text and is now being written as thinking, is a broken stream
// rather than something to guess at.
func blockAt[T ai.Content](c *converter, ev *wireEvent, kind string) (T, error) {
	var zero T
	if ev.ContentIndex < 0 || ev.ContentIndex >= len(c.out.Content) {
		return zero, fmt.Errorf("%s addresses content index %d, which was never started", ev.Type, ev.ContentIndex)
	}
	block, ok := c.out.Content[ev.ContentIndex].(T)
	if !ok {
		return zero, fmt.Errorf("%s addresses content index %d, which is not a %s block", ev.Type, ev.ContentIndex, kind)
	}
	return block, nil
}

// decodeEvent parses one SSE data payload. The stream's own end marker is not
// an event.
func decodeEvent(data string) (*wireEvent, error) {
	if data == "" || data == "[DONE]" {
		return nil, nil
	}
	var ev wireEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, fmt.Errorf("unreadable event: %w", err)
	}
	return &ev, nil
}
