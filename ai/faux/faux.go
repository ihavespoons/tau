// Package faux is a scripted in-memory provider — the backbone of tau's
// offline test suite (tau's analogue of Pi's providers/faux.ts). A Script
// describes assistant turns declaratively; faux synthesizes the canonical
// stream-event grammar (start → block start/delta/end → done/error) exactly as
// a real wire API would, so everything downstream (agent loop, modes, TUI)
// can be tested without network or keys.
package faux

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ihavespoons/tau/ai"
)

// Block is one scripted content block.
type Block struct {
	// Exactly one of Text, Thinking, ToolCall is used.
	Text     string
	Thinking string
	ToolCall *ai.ToolCall
	// DeltaSize splits Text/Thinking into deltas of this many bytes (default:
	// whole string in one delta). Tool calls stream their arguments JSON in
	// one delta.
	DeltaSize int
}

// Turn is one scripted assistant response.
type Turn struct {
	Blocks []Block
	// Stop is the final stop reason; defaults to "stop", or "toolUse" when
	// the turn contains tool calls.
	Stop ai.StopReason
	// ErrorMessage, when set, makes the turn terminate with an error event
	// (Stop should be "error" or "aborted"; defaults to "error").
	ErrorMessage string
	// FailAfterBlocks emits the error after this many complete blocks
	// (only meaningful with ErrorMessage; default: after all blocks).
	FailAfterBlocks int
	// Usage overrides the synthesized usage.
	Usage *ai.Usage
	// Delay sleeps before each event (for abort/timing tests).
	Delay time.Duration
}

// Script is a queue of turns consumed by successive Stream calls.
type Script struct {
	mu    sync.Mutex
	turns []Turn
	calls int
}

// NewScript builds a script from turns.
func NewScript(turns ...Turn) *Script { return &Script{turns: turns} }

// Calls reports how many times Stream was invoked.
func (s *Script) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// Model returns a plausible faux model.
func Model() *ai.Model {
	return &ai.Model{
		ID: "faux-1", Name: "Faux 1", Api: "faux", Provider: "faux",
		BaseURL: "faux://", Reasoning: true, Input: []string{"text", "image"},
		Cost:          ai.ModelCost{ModelCostRates: ai.ModelCostRates{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25}},
		ContextWindow: 200000, MaxTokens: 8192,
	}
}

// Stream implements ai.StreamFunc over the script.
func (s *Script) Stream(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.StreamOptions) *ai.MessageStream {
	s.mu.Lock()
	var turn Turn
	if len(s.turns) > 0 {
		turn = s.turns[0]
		s.turns = s.turns[1:]
	} else {
		turn = Turn{Blocks: []Block{{Text: "(faux: script exhausted)"}}}
	}
	s.calls++
	s.mu.Unlock()

	stream := ai.NewMessageStream()
	go run(ctx, stream, model, turn)
	return stream
}

// StreamSimple implements ai.SimpleStreamFunc over the script.
func (s *Script) StreamSimple(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	var base *ai.StreamOptions
	if opts != nil {
		base = &opts.StreamOptions
	}
	return s.Stream(ctx, model, c, base)
}

func run(ctx context.Context, stream *ai.MessageStream, model *ai.Model, turn Turn) {
	now := time.Now().UnixMilli()
	partial := &ai.AssistantMessage{
		Content: ai.ContentList{}, Api: model.Api, Provider: model.Provider,
		Model: model.ID, StopReason: ai.StopPending, Timestamp: now,
	}
	abort := func() bool {
		if turn.Delay > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(turn.Delay):
			}
		}
		if ctx.Err() != nil {
			stream.Push(ai.Event{Type: ai.EventError, Reason: ai.StopAborted,
				Error: ai.ErrorMessage(partial, ai.StopAborted, "aborted")})
			return true
		}
		return false
	}

	if abort() {
		return
	}
	stream.Push(ai.Event{Type: ai.EventStart, Partial: partial})

	fail := func() {
		reason := turn.Stop
		if reason != ai.StopAborted {
			reason = ai.StopError
		}
		stream.Push(ai.Event{Type: ai.EventError, Reason: reason,
			Error: ai.ErrorMessage(partial, reason, turn.ErrorMessage)})
	}

	hasTool := false
	for i, b := range turn.Blocks {
		if turn.ErrorMessage != "" && turn.FailAfterBlocks > 0 && i == turn.FailAfterBlocks {
			fail()
			return
		}
		if abort() {
			return
		}
		idx := len(partial.Content)
		switch {
		case b.ToolCall != nil:
			hasTool = true
			tc := *b.ToolCall
			partial.Content = append(partial.Content, tc)
			stream.Push(ai.Event{Type: ai.EventToolCallStart, ContentIndex: idx, Partial: partial})
			stream.Push(ai.Event{Type: ai.EventToolCallDelta, ContentIndex: idx, Delta: argsJSON(tc), Partial: partial})
			stream.Push(ai.Event{Type: ai.EventToolCallEnd, ContentIndex: idx, ToolCall: &tc, Partial: partial})
		case b.Thinking != "":
			partial.Content = append(partial.Content, ai.ThinkingContent{})
			stream.Push(ai.Event{Type: ai.EventThinkingStart, ContentIndex: idx, Partial: partial})
			for _, d := range chunks(b.Thinking, b.DeltaSize) {
				if abort() {
					return
				}
				tc := partial.Content[idx].(ai.ThinkingContent)
				tc.Thinking += d
				partial.Content[idx] = tc
				stream.Push(ai.Event{Type: ai.EventThinkingDelta, ContentIndex: idx, Delta: d, Partial: partial})
			}
			stream.Push(ai.Event{Type: ai.EventThinkingEnd, ContentIndex: idx, Content: b.Thinking, Partial: partial})
		default:
			partial.Content = append(partial.Content, ai.TextContent{})
			stream.Push(ai.Event{Type: ai.EventTextStart, ContentIndex: idx, Partial: partial})
			for _, d := range chunks(b.Text, b.DeltaSize) {
				if abort() {
					return
				}
				tc := partial.Content[idx].(ai.TextContent)
				tc.Text += d
				partial.Content[idx] = tc
				stream.Push(ai.Event{Type: ai.EventTextDelta, ContentIndex: idx, Delta: d, Partial: partial})
			}
			stream.Push(ai.Event{Type: ai.EventTextEnd, ContentIndex: idx, Content: b.Text, Partial: partial})
		}
	}

	if turn.ErrorMessage != "" {
		fail()
		return
	}

	if turn.Usage != nil {
		partial.Usage = *turn.Usage
	} else {
		partial.Usage = ai.Usage{Input: 10, Output: 20, TotalTokens: 30}
	}
	ai.CalculateCost(model, &partial.Usage)

	stop := turn.Stop
	if stop == "" {
		if hasTool {
			stop = ai.StopToolUse
		} else {
			stop = ai.StopStop
		}
	}
	partial.StopReason = stop
	stream.Push(ai.Event{Type: ai.EventDone, Reason: stop, Message: partial})
}

// argsJSON renders the arguments object — only tool arguments stream as
// deltas on real APIs.
func argsJSON(tc ai.ToolCall) string {
	b, err := json.Marshal(tc.Arguments)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func chunks(s string, size int) []string {
	if size <= 0 || size >= len(s) {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var out []string
	for len(s) > size {
		out = append(out, s[:size])
		s = s[size:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}
