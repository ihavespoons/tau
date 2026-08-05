package bedrock

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/partialjson"
)

// Converse addresses content by a wire index that is not the position in the
// output. Text blocks in particular have no start event at all — the first
// delta is what opens them — so the two numbering schemes have to be tracked
// separately and mapped.
type blockState struct {
	// contentIndex is the block's position in the assistant message.
	contentIndex int
	kind         string // text | thinking | toolCall
	text         string
	partialJSON  string
}

type blockTable struct {
	byWireIndex map[int32]*blockState
	order       []*blockState
}

func newBlockTable() *blockTable {
	return &blockTable{byWireIndex: map[int32]*blockState{}}
}

func (t *blockTable) get(wireIndex int32) *blockState { return t.byWireIndex[wireIndex] }

func (t *blockTable) add(wireIndex int32, b *blockState) {
	t.byWireIndex[wireIndex] = b
	t.order = append(t.order, b)
}

// consume drains the event stream into tau events.
func consume(
	ctx context.Context,
	events *bedrockruntime.ConverseStreamEventStream,
	stream *ai.MessageStream,
	out *ai.AssistantMessage,
	model *ai.Model,
) error {
	blocks := newBlockTable()
	started := false

	for event := range events.Events() {
		if ctx.Err() != nil {
			return errors.New("Request was aborted") //nolint:staticcheck // Pi error-message parity
		}

		switch ev := event.(type) {
		case *types.ConverseStreamOutputMemberMessageStart:
			if ev.Value.Role != types.ConversationRoleAssistant {
				return errors.New("bedrock started a " + string(ev.Value.Role) + " message, not an assistant one")
			}
			started = true
			stream.Push(ai.Event{Type: ai.EventStart, Partial: out})

		case *types.ConverseStreamOutputMemberContentBlockStart:
			handleBlockStart(ev.Value, blocks, stream, out)

		case *types.ConverseStreamOutputMemberContentBlockDelta:
			handleBlockDelta(ev.Value, blocks, stream, out)

		case *types.ConverseStreamOutputMemberContentBlockStop:
			handleBlockStop(ev.Value, blocks, stream, out)

		case *types.ConverseStreamOutputMemberMessageStop:
			reason, errMessage := mapStopReason(ev.Value.StopReason)
			out.StopReason = reason
			out.ErrorMessage = errMessage

		case *types.ConverseStreamOutputMemberMetadata:
			applyUsage(ev.Value, out, model)
		}
	}

	// Modelled exceptions (throttling, validation, model stream errors) arrive
	// here rather than as events, and the channel closes normally when one
	// does. Checking it is the only thing that distinguishes a failed turn from
	// a complete one.
	if err := events.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errors.New("Request was aborted") //nolint:staticcheck // Pi error-message parity
	}
	if !started {
		return errors.New("the bedrock stream ended before the message began")
	}
	if out.StopReason == ai.StopError {
		if out.ErrorMessage != "" {
			return errors.New(out.ErrorMessage)
		}
		return errors.New("An unknown error occurred") //nolint:staticcheck // Pi error-message parity
	}
	return nil
}

func handleBlockStart(ev types.ContentBlockStartEvent, blocks *blockTable, stream *ai.MessageStream, out *ai.AssistantMessage) {
	start, ok := ev.Start.(*types.ContentBlockStartMemberToolUse)
	if !ok || ev.ContentBlockIndex == nil {
		return
	}
	block := &blockState{contentIndex: len(out.Content), kind: "toolCall"}
	out.Content = append(out.Content, ai.ToolCall{
		ID:        deref(start.Value.ToolUseId),
		Name:      deref(start.Value.Name),
		Arguments: map[string]any{},
	})
	blocks.add(*ev.ContentBlockIndex, block)
	stream.Push(ai.Event{Type: ai.EventToolCallStart, ContentIndex: block.contentIndex, Partial: out})
}

func handleBlockDelta(ev types.ContentBlockDeltaEvent, blocks *blockTable, stream *ai.MessageStream, out *ai.AssistantMessage) {
	if ev.ContentBlockIndex == nil {
		return
	}
	wireIndex := *ev.ContentBlockIndex

	switch delta := ev.Delta.(type) {
	case *types.ContentBlockDeltaMemberText:
		block := blocks.get(wireIndex)
		if block == nil {
			// Text blocks get no start event, so the first delta opens one.
			block = &blockState{contentIndex: len(out.Content), kind: "text"}
			out.Content = append(out.Content, ai.TextContent{})
			blocks.add(wireIndex, block)
			stream.Push(ai.Event{Type: ai.EventTextStart, ContentIndex: block.contentIndex, Partial: out})
		}
		if block.kind != "text" {
			return
		}
		block.text += delta.Value
		out.Content[block.contentIndex] = ai.TextContent{Text: block.text}
		stream.Push(ai.Event{
			Type: ai.EventTextDelta, ContentIndex: block.contentIndex, Delta: delta.Value, Partial: out,
		})

	case *types.ContentBlockDeltaMemberToolUse:
		block := blocks.get(wireIndex)
		if block == nil || block.kind != "toolCall" {
			return
		}
		fragment := deref(delta.Value.Input)
		block.partialJSON += fragment
		call, _ := out.Content[block.contentIndex].(ai.ToolCall)
		call.Arguments = partialjson.ParseStreaming(block.partialJSON)
		if call.Arguments == nil {
			call.Arguments = map[string]any{}
		}
		out.Content[block.contentIndex] = call
		stream.Push(ai.Event{
			Type: ai.EventToolCallDelta, ContentIndex: block.contentIndex, Delta: fragment, Partial: out,
		})

	case *types.ContentBlockDeltaMemberReasoningContent:
		handleReasoningDelta(wireIndex, delta.Value, blocks, stream, out)
	}
}

// handleReasoningDelta accumulates thinking text and its signature.
//
// The Go SDK models the reasoning delta as a union, so text and signature
// arrive as separate events where the JS SDK would put both on one object. The
// signature carries no text and must not be pushed as a delta — it is an opaque
// handle that has to replay verbatim, and showing it would be noise.
func handleReasoningDelta(
	wireIndex int32,
	delta types.ReasoningContentBlockDelta,
	blocks *blockTable,
	stream *ai.MessageStream,
	out *ai.AssistantMessage,
) {
	block := blocks.get(wireIndex)
	if block == nil {
		block = &blockState{contentIndex: len(out.Content), kind: "thinking"}
		out.Content = append(out.Content, ai.ThinkingContent{})
		blocks.add(wireIndex, block)
		stream.Push(ai.Event{Type: ai.EventThinkingStart, ContentIndex: block.contentIndex, Partial: out})
	}
	if block.kind != "thinking" {
		return
	}
	current, _ := out.Content[block.contentIndex].(ai.ThinkingContent)

	switch d := delta.(type) {
	case *types.ReasoningContentBlockDeltaMemberText:
		if d.Value == "" {
			return
		}
		block.text += d.Value
		current.Thinking = block.text
		out.Content[block.contentIndex] = current
		stream.Push(ai.Event{
			Type: ai.EventThinkingDelta, ContentIndex: block.contentIndex, Delta: d.Value, Partial: out,
		})
	case *types.ReasoningContentBlockDeltaMemberSignature:
		current.ThinkingSignature += d.Value
		out.Content[block.contentIndex] = current
	}
}

func handleBlockStop(ev types.ContentBlockStopEvent, blocks *blockTable, stream *ai.MessageStream, out *ai.AssistantMessage) {
	if ev.ContentBlockIndex == nil {
		return
	}
	block := blocks.get(*ev.ContentBlockIndex)
	if block == nil {
		return
	}
	switch block.kind {
	case "text":
		stream.Push(ai.Event{
			Type: ai.EventTextEnd, ContentIndex: block.contentIndex, Content: block.text, Partial: out,
		})
	case "thinking":
		stream.Push(ai.Event{
			Type: ai.EventThinkingEnd, ContentIndex: block.contentIndex, Content: block.text, Partial: out,
		})
	case "toolCall":
		call, _ := out.Content[block.contentIndex].(ai.ToolCall)
		call.Arguments = partialjson.ParseStreaming(block.partialJSON)
		if call.Arguments == nil {
			call.Arguments = map[string]any{}
		}
		out.Content[block.contentIndex] = call
		stream.Push(ai.Event{
			Type: ai.EventToolCallEnd, ContentIndex: block.contentIndex, ToolCall: &call, Partial: out,
		})
	}
}

func applyUsage(ev types.ConverseStreamMetadataEvent, out *ai.AssistantMessage, model *ai.Model) {
	if ev.Usage == nil {
		return
	}
	usage := ai.Usage{
		Input:       int(deref(ev.Usage.InputTokens)),
		Output:      int(deref(ev.Usage.OutputTokens)),
		CacheRead:   int(deref(ev.Usage.CacheReadInputTokens)),
		CacheWrite:  int(deref(ev.Usage.CacheWriteInputTokens)),
		TotalTokens: int(deref(ev.Usage.TotalTokens)),
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.Input + usage.Output
	}
	usage.Cost = ai.CalculateCost(model, &usage)
	out.Usage = usage
}

func mapStopReason(reason types.StopReason) (ai.StopReason, string) {
	switch reason {
	case types.StopReasonEndTurn, types.StopReasonStopSequence:
		return ai.StopStop, ""
	case types.StopReasonMaxTokens, types.StopReasonModelContextWindowExceeded:
		return ai.StopLength, ""
	case types.StopReasonToolUse:
		return ai.StopToolUse, ""
	default:
		return ai.StopError, string(reason)
	}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
