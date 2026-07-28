package anthropic

// SSE event consumption — port of iterateAnthropicEvents and the event loop
// body of Pi's stream().

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/internal/sse"
	"github.com/ihavespoons/tau/ai/partialjson"
)

var anthropicMessageEvents = map[string]bool{
	"message_start":       true,
	"message_delta":       true,
	"message_stop":        true,
	"content_block_start": true,
	"content_block_delta": true,
	"content_block_stop":  true,
}

type wireUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral1hInputTokens *int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokensDetails *struct {
		ThinkingTokens *int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type wireEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string    `json:"id"`
		Usage wireUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type  string         `json:"type"`
		Text  string         `json:"text"`
		Data  string         `json:"data"`
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
		StopDetails *struct {
			Explanation string `json:"explanation"`
		} `json:"stop_details"`
	} `json:"delta"`
	Usage *wireUsage `json:"usage"`
}

func parseWireEvent(data string) (*wireEvent, error) {
	var ev wireEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		repaired := partialjson.Repair(data)
		if repaired != data {
			if rerr := json.Unmarshal([]byte(repaired), &ev); rerr == nil {
				return &ev, nil
			}
		}
		return nil, err
	}
	return &ev, nil
}

func intOr0(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func consumeSSE(ctx context.Context, stream *ai.MessageStream, resp *http.Response, model *ai.Model, c ai.Context, oauth bool, output *ai.AssistantMessage) error {
	reader := sse.NewReader(resp.Body)
	var blocks []*liveBlock
	sawMessageStart, sawMessageEnd := false, false

	syncBlock := func(pos int) {
		output.Content[pos] = blocks[pos].materialize()
	}
	findBlock := func(apiIndex int) (int, *liveBlock) {
		for i, b := range blocks {
			if b.apiIndex == apiIndex {
				return i, b
			}
		}
		return -1, nil
	}

	for {
		if ctx.Err() != nil {
			return fmt.Errorf("Request was aborted") //nolint:staticcheck // Pi error-message parity
		}
		sseEvent, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if ctx.Err() != nil {
				return fmt.Errorf("Request was aborted") //nolint:staticcheck // Pi error-message parity
			}
			return err
		}
		if sseEvent.Name == "error" {
			return fmt.Errorf("%s", sseEvent.Data)
		}
		if !anthropicMessageEvents[sseEvent.Name] {
			continue
		}
		event, perr := parseWireEvent(sseEvent.Data)
		if perr != nil {
			//nolint:staticcheck // Pi error-message parity
			return fmt.Errorf("Could not parse Anthropic SSE event %s: %v; data=%s; raw=%s",
				sseEvent.Name, perr, sseEvent.Data, strings.Join(sseEvent.Raw, "\\n"))
		}

		switch event.Type {
		case "message_start":
			sawMessageStart = true
			if event.Message != nil {
				output.ResponseID = event.Message.ID
				u := event.Message.Usage
				output.Usage.Input = intOr0(u.InputTokens)
				output.Usage.Output = intOr0(u.OutputTokens)
				output.Usage.CacheRead = intOr0(u.CacheReadInputTokens)
				output.Usage.CacheWrite = intOr0(u.CacheCreationInputTokens)
				oneHour := 0
				if u.CacheCreation != nil {
					oneHour = intOr0(u.CacheCreation.Ephemeral1hInputTokens)
				}
				output.Usage.CacheWrite1h = &oneHour
				output.Usage.TotalTokens = output.Usage.Input + output.Usage.Output + output.Usage.CacheRead + output.Usage.CacheWrite
				ai.CalculateCost(model, &output.Usage)
			}

		case "content_block_start":
			if event.ContentBlock == nil {
				continue
			}
			var b *liveBlock
			var evType ai.EventType
			switch event.ContentBlock.Type {
			case "text":
				b = &liveBlock{apiIndex: event.Index, kind: "text"}
				evType = ai.EventTextStart
			case "thinking":
				b = &liveBlock{apiIndex: event.Index, kind: "thinking"}
				evType = ai.EventThinkingStart
			case "redacted_thinking":
				b = &liveBlock{apiIndex: event.Index, kind: "thinking", thinking: "[Reasoning redacted]", signature: event.ContentBlock.Data, redacted: true}
				evType = ai.EventThinkingStart
			case "tool_use":
				name := event.ContentBlock.Name
				if oauth {
					name = fromClaudeCodeName(name, c.Tools)
				}
				args := event.ContentBlock.Input
				if args == nil {
					args = map[string]any{}
				}
				b = &liveBlock{apiIndex: event.Index, kind: "toolCall", toolID: event.ContentBlock.ID, toolName: name, args: args}
				evType = ai.EventToolCallStart
			default:
				continue
			}
			blocks = append(blocks, b)
			output.Content = append(output.Content, b.materialize())
			stream.Push(ai.Event{Type: evType, ContentIndex: len(output.Content) - 1, Partial: output})

		case "content_block_delta":
			if event.Delta == nil {
				continue
			}
			pos, b := findBlock(event.Index)
			if b == nil {
				continue
			}
			switch event.Delta.Type {
			case "text_delta":
				if b.kind != "text" {
					continue
				}
				b.text += event.Delta.Text
				syncBlock(pos)
				stream.Push(ai.Event{Type: ai.EventTextDelta, ContentIndex: pos, Delta: event.Delta.Text, Partial: output})
			case "thinking_delta":
				if b.kind != "thinking" {
					continue
				}
				b.thinking += event.Delta.Thinking
				syncBlock(pos)
				stream.Push(ai.Event{Type: ai.EventThinkingDelta, ContentIndex: pos, Delta: event.Delta.Thinking, Partial: output})
			case "input_json_delta":
				if b.kind != "toolCall" {
					continue
				}
				b.partialJSON += event.Delta.PartialJSON
				b.args = partialjson.ParseStreaming(b.partialJSON)
				syncBlock(pos)
				stream.Push(ai.Event{Type: ai.EventToolCallDelta, ContentIndex: pos, Delta: event.Delta.PartialJSON, Partial: output})
			case "signature_delta":
				if b.kind != "thinking" {
					continue
				}
				b.signature += event.Delta.Signature
				syncBlock(pos)
			}

		case "content_block_stop":
			pos, b := findBlock(event.Index)
			if b == nil {
				continue
			}
			switch b.kind {
			case "text":
				stream.Push(ai.Event{Type: ai.EventTextEnd, ContentIndex: pos, Content: b.text, Partial: output})
			case "thinking":
				stream.Push(ai.Event{Type: ai.EventThinkingEnd, ContentIndex: pos, Content: b.thinking, Partial: output})
			case "toolCall":
				b.args = partialjson.ParseStreaming(b.partialJSON)
				b.partialJSON = ""
				syncBlock(pos)
				tc := ai.ToolCall{ID: b.toolID, Name: b.toolName, Arguments: b.args}
				stream.Push(ai.Event{Type: ai.EventToolCallEnd, ContentIndex: pos, ToolCall: &tc, Partial: output})
			}

		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				stopReason, errMsg, merr := mapStopReason(event.Delta.StopReason, event.Delta.StopDetails)
				if merr != nil {
					return merr
				}
				output.StopReason = stopReason
				if errMsg != "" {
					output.ErrorMessage = errMsg
				}
			}
			// Only update fields present (non-null) — proxies may omit
			// input_tokens in message_delta.
			if event.Usage != nil {
				u := event.Usage
				if u.InputTokens != nil {
					output.Usage.Input = *u.InputTokens
				}
				if u.OutputTokens != nil {
					output.Usage.Output = *u.OutputTokens
				}
				if u.CacheReadInputTokens != nil {
					output.Usage.CacheRead = *u.CacheReadInputTokens
				}
				if u.CacheCreationInputTokens != nil {
					output.Usage.CacheWrite = *u.CacheCreationInputTokens
				}
				if u.OutputTokensDetails != nil && u.OutputTokensDetails.ThinkingTokens != nil {
					output.Usage.Reasoning = u.OutputTokensDetails.ThinkingTokens
				}
			}
			output.Usage.TotalTokens = output.Usage.Input + output.Usage.Output + output.Usage.CacheRead + output.Usage.CacheWrite
			ai.CalculateCost(model, &output.Usage)

		case "message_stop":
			sawMessageEnd = true
		}
	}

	if sawMessageStart && !sawMessageEnd {
		return fmt.Errorf("Anthropic stream ended before message_stop") //nolint:staticcheck // Pi error-message parity
	}
	return nil
}

type stopDetails = struct {
	Explanation string `json:"explanation"`
}

func mapStopReason(reason string, details *stopDetails) (ai.StopReason, string, error) {
	switch reason {
	case "end_turn":
		return ai.StopStop, "", nil
	case "max_tokens":
		return ai.StopLength, "", nil
	case "tool_use":
		return ai.StopToolUse, "", nil
	case "refusal":
		msg := "The model refused to complete the request"
		if details != nil && details.Explanation != "" {
			msg = details.Explanation
		}
		return ai.StopError, msg, nil
	case "pause_turn":
		return ai.StopStop, "", nil // stop is good enough → resubmit
	case "stop_sequence":
		return ai.StopStop, "", nil
	case "sensitive":
		return ai.StopError, "", nil
	default:
		return "", "", fmt.Errorf("Unhandled stop reason: %s", reason) //nolint:staticcheck // Pi error-message parity
	}
}
