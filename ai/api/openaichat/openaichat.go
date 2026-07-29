package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/internal/sse"
	"github.com/ihavespoons/tau/ai/partialjson"
)

// Options are the openai-completions provider options (Pi's
// OpenAICompletionsOptions).
type Options struct {
	ai.StreamOptions
	// Reasoning is the effort level to request; empty means off.
	Reasoning ai.ThinkingLevel
	// ToolChoice is "auto" | "none" | "required", or an object naming a tool.
	ToolChoice any
}

// Stream runs one turn against the chat-completions endpoint.
//
// Like every wire API in tau it never returns an error: failures arrive as a
// terminal error event carrying an AssistantMessage whose StopReason says what
// went wrong.
func Stream(ctx context.Context, model *ai.Model, c ai.Context, opts *Options) *ai.MessageStream {
	if opts == nil {
		opts = &Options{}
	}
	stream := ai.NewMessageStream()
	go run(ctx, stream, model, c, opts)
	return stream
}

// StreamSimple is Stream with normalized cross-provider options: the thinking
// level is clamped to what this model supports before being translated into
// whichever dialect the provider speaks.
func StreamSimple(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	reasoning := opts.Reasoning
	if reasoning != "" {
		clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(reasoning))
		if clamped == ai.ThinkingOff {
			reasoning = ""
		} else {
			reasoning = ai.ThinkingLevel(clamped)
		}
	}
	return Stream(ctx, model, c, &Options{
		StreamOptions: opts.StreamOptions,
		Reasoning:     reasoning,
	})
}

func newOutput(model *ai.Model) *ai.AssistantMessage {
	return &ai.AssistantMessage{
		Content:    ai.ContentList{},
		Api:        model.Api,
		Provider:   model.Provider,
		Model:      model.ID,
		StopReason: ai.StopPending,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// liveBlock tracks one streaming content block. The materialized value is
// mirrored into output.Content after every mutation so a partial message is
// always coherent.
type liveBlock struct {
	kind        string // text | thinking | toolCall
	text        string
	thinking    string
	signature   string
	toolID      string
	toolName    string
	partialJSON string
	args        map[string]any
	thought     string
}

func (b *liveBlock) materialize() ai.Content {
	switch b.kind {
	case "text":
		return ai.TextContent{Text: b.text}
	case "thinking":
		return ai.ThinkingContent{Thinking: b.thinking, ThinkingSignature: b.signature}
	default:
		return ai.ToolCall{
			ID: b.toolID, Name: b.toolName,
			Arguments: b.args, ThoughtSignature: b.thought,
		}
	}
}

// hasHeader reports whether a non-empty header of this name was supplied.
func hasHeader(headers map[string]*string, name string) bool {
	want := strings.ToLower(name)
	for k, v := range headers {
		if strings.ToLower(k) == want && v != nil && strings.TrimSpace(*v) != "" {
			return true
		}
	}
	return false
}

// assertRequestAuth mirrors Pi's getClientApiKey: a gateway may carry
// credentials in a header instead of an API key, but something must be there.
func assertRequestAuth(provider ai.ProviderId, apiKey string, headers map[string]*string) error {
	if apiKey != "" {
		return nil
	}
	if hasHeader(headers, "authorization") || hasHeader(headers, "cf-aig-authorization") {
		return nil
	}
	return fmt.Errorf("no API key for provider: %s", provider)
}

func run(ctx context.Context, stream *ai.MessageStream, model *ai.Model, c ai.Context, opts *Options) {
	output := newOutput(model)

	fail := func(err error) {
		if ctx.Err() != nil {
			output.StopReason = ai.StopAborted
		} else {
			output.StopReason = ai.StopError
		}
		output.ErrorMessage = err.Error()
		stream.Push(ai.Event{Type: ai.EventError, Reason: output.StopReason, Error: output})
	}

	if err := assertRequestAuth(model.Provider, opts.APIKey, opts.Headers); err != nil {
		fail(err)
		return
	}

	cm := resolveCompat(model)
	req := buildRequest(model, c, opts, cm)

	var payload any = req
	if opts.OnPayload != nil {
		replaced, err := opts.OnPayload(payload, model)
		if err != nil {
			fail(err)
			return
		}
		if replaced != nil {
			payload = replaced
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		fail(fmt.Errorf("encoding request: %w", err))
		return
	}

	resp, err := doRequest(ctx, model, buildHeaders(model, opts, cm), body, opts)
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if opts.OnResponse != nil {
		if err := opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: resp.Header}, model); err != nil {
			fail(err)
			return
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fail(providerError(resp))
		return
	}

	stream.Push(ai.Event{Type: ai.EventStart, Partial: output})
	consume(ctx, stream, resp.Body, model, output, fail)
}

// state is the accumulator for one streamed response.
type state struct {
	blocks []*liveBlock
	// byIndex and byID both point at the same blocks: providers identify a
	// tool call by streaming index, by id, or by whichever they remembered to
	// send in a given chunk.
	byIndex map[int]*liveBlock
	byID    map[string]*liveBlock

	text     *liveBlock
	thinking *liveBlock

	// pendingThoughts holds reasoning details that arrived before the tool
	// call they belong to.
	pendingThoughts map[string]string

	hasFinishReason bool
}

func consume(ctx context.Context, stream *ai.MessageStream, body io.Reader, model *ai.Model, output *ai.AssistantMessage, fail func(error)) {
	st := &state{
		byIndex:         map[int]*liveBlock{},
		byID:            map[string]*liveBlock{},
		pendingThoughts: map[string]string{},
	}

	sync := func(b *liveBlock) int {
		for i, existing := range st.blocks {
			if existing == b {
				output.Content[i] = b.materialize()
				return i
			}
		}
		return -1
	}
	add := func(b *liveBlock) int {
		st.blocks = append(st.blocks, b)
		output.Content = append(output.Content, b.materialize())
		return len(st.blocks) - 1
	}

	reader := sse.NewReader(body)
	for {
		if ctx.Err() != nil {
			fail(fmt.Errorf("Request was aborted")) //nolint:staticcheck // Pi error-message parity
			return
		}

		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(err)
			return
		}
		if ev == nil || ev.Data == "" || ev.Data == "[DONE]" {
			continue
		}

		var chunk chunkPayload
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			// A malformed chunk is not fatal: providers occasionally interleave
			// keep-alives and comments that are not completion chunks.
			continue
		}

		if output.ResponseID == "" {
			output.ResponseID = chunk.ID
		}
		if chunk.Model != "" && chunk.Model != model.ID && output.ResponseModel == "" {
			output.ResponseModel = chunk.Model
		}
		if chunk.Usage != nil {
			output.Usage = parseUsage(chunk.Usage, model)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		// Moonshot puts usage on the choice rather than the chunk.
		if chunk.Usage == nil && choice.Usage != nil {
			output.Usage = parseUsage(choice.Usage, model)
		}

		if choice.FinishReason != "" {
			reason, errMsg := mapStopReason(choice.FinishReason)
			output.StopReason = reason
			if errMsg != "" {
				output.ErrorMessage = errMsg
			}
			st.hasFinishReason = true
		}

		d := choice.Delta
		if d == nil {
			continue
		}

		if d.Content != "" {
			if st.text == nil {
				st.text = &liveBlock{kind: "text"}
				idx := add(st.text)
				stream.Push(ai.Event{Type: ai.EventTextStart, ContentIndex: idx, Partial: output})
			}
			st.text.text += d.Content
			idx := sync(st.text)
			stream.Push(ai.Event{
				Type: ai.EventTextDelta, ContentIndex: idx,
				Delta: d.Content, Partial: output,
			})
		}

		// Reasoning arrives under one of three field names depending on the
		// server. Take the first non-empty one: chutes.ai sends the same text
		// in two of them, and streaming both would duplicate the thinking.
		if field, delta := d.firstReasoning(); delta != "" {
			signature := field
			if model.Provider == "opencode-go" && field == "reasoning" {
				signature = "reasoning_content"
			}
			if st.thinking == nil {
				st.thinking = &liveBlock{kind: "thinking", signature: signature}
				idx := add(st.thinking)
				stream.Push(ai.Event{Type: ai.EventThinkingStart, ContentIndex: idx, Partial: output})
			}
			st.thinking.thinking += delta
			idx := sync(st.thinking)
			stream.Push(ai.Event{
				Type: ai.EventThinkingDelta, ContentIndex: idx,
				Delta: delta, Partial: output,
			})
		}

		for _, tc := range d.ToolCalls {
			block, isNew := st.ensureToolCall(tc)
			if isNew {
				idx := add(block)
				stream.Push(ai.Event{Type: ai.EventToolCallStart, ContentIndex: idx, Partial: output})
			}
			if tc.Function != nil && tc.Function.Arguments != "" {
				block.partialJSON += tc.Function.Arguments
				block.args = partialjson.ParseStreaming(block.partialJSON)
				idx := sync(block)
				stream.Push(ai.Event{
					Type: ai.EventToolCallDelta, ContentIndex: idx,
					Delta: tc.Function.Arguments, Partial: output,
				})
			}
		}

		st.applyReasoningDetails(d.ReasoningDetails, sync)
	}

	// Close every block that is still open.
	for _, b := range st.blocks {
		idx := sync(b)
		switch b.kind {
		case "text":
			stream.Push(ai.Event{Type: ai.EventTextEnd, ContentIndex: idx, Content: b.text, Partial: output})
		case "thinking":
			stream.Push(ai.Event{Type: ai.EventThinkingEnd, ContentIndex: idx, Content: b.thinking, Partial: output})
		default:
			stream.Push(ai.Event{Type: ai.EventToolCallEnd, ContentIndex: idx, Partial: output})
		}
	}

	switch {
	case ctx.Err() != nil, output.StopReason == ai.StopAborted:
		fail(fmt.Errorf("Request was aborted")) //nolint:staticcheck // Pi error-message parity
		return
	case output.StopReason == ai.StopError:
		msg := output.ErrorMessage
		if msg == "" {
			msg = "Provider returned an error stop reason"
		}
		fail(errors.New(msg))
		return
	case !st.hasFinishReason || output.StopReason == ai.StopPending:
		// A stream that stops without saying why is a failure, not a result:
		// treating it as success would silently truncate the turn.
		fail(errors.New("Stream ended without finish_reason"))
		return
	}

	stream.Push(ai.Event{Type: ai.EventDone, Reason: output.StopReason, Message: output})
}

// ensureToolCall finds or creates the block a tool-call delta belongs to.
func (s *state) ensureToolCall(tc *toolCallDelta) (*liveBlock, bool) {
	var block *liveBlock
	if tc.Index != nil {
		block = s.byIndex[*tc.Index]
	}
	if block == nil && tc.ID != "" {
		block = s.byID[tc.ID]
	}

	name := ""
	if tc.Function != nil {
		name = tc.Function.Name
	}

	isNew := false
	if block == nil {
		block = &liveBlock{kind: "toolCall", toolID: tc.ID, toolName: name, args: map[string]any{}}
		isNew = true
	}
	if tc.Index != nil {
		s.byIndex[*tc.Index] = block
	}
	if tc.ID != "" {
		if block.toolID == "" {
			block.toolID = tc.ID
		}
		s.byID[tc.ID] = block
	}
	if block.toolName == "" && name != "" {
		block.toolName = name
	}
	// A reasoning detail may have arrived before the call it annotates.
	if block.toolID != "" {
		if pending, ok := s.pendingThoughts[block.toolID]; ok {
			block.thought = pending
			delete(s.pendingThoughts, block.toolID)
		}
	}
	return block, isNew
}

// applyReasoningDetails attaches OpenRouter's encrypted reasoning payloads to
// the tool call they belong to, holding any that arrive early.
func (s *state) applyReasoningDetails(details []json.RawMessage, sync func(*liveBlock) int) {
	for _, raw := range details {
		var detail struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if err := json.Unmarshal(raw, &detail); err != nil {
			continue
		}
		// Only the encrypted form is a replayable signature, and only when it
		// actually carries a payload. The plaintext forms (OpenRouter emits
		// "reasoning.text" for Anthropic) are already surfaced as thinking
		// content, and replaying a detail with no data would send the upstream
		// a signature that verifies against nothing.
		if detail.Type != "reasoning.encrypted" || detail.ID == "" || detail.Data == "" {
			continue
		}
		if block, ok := s.byID[detail.ID]; ok {
			block.thought = string(raw)
			sync(block)
			continue
		}
		s.pendingThoughts[detail.ID] = string(raw)
	}
}

// mapStopReason translates the provider's finish_reason.
func mapStopReason(reason string) (ai.StopReason, string) {
	switch reason {
	case "stop", "end":
		return ai.StopStop, ""
	case "length":
		return ai.StopLength, ""
	case "function_call", "tool_calls":
		return ai.StopToolUse, ""
	case "content_filter":
		return ai.StopError, "Provider finish_reason: content_filter"
	case "network_error":
		return ai.StopError, "Provider finish_reason: network_error"
	default:
		return ai.StopError, "Provider finish_reason: " + reason
	}
}

// parseUsage maps the provider's token counts onto tau's usage shape.
//
// cached_tokens is cache-READ, per OpenAI's and OpenRouter's documented
// semantics, and writes are a separate count where a provider reports them.
// Subtracting writes from reads would under-report every spec-compliant
// provider, so it is deliberately not done.
func parseUsage(u *usagePayload, model *ai.Model) ai.Usage {
	cacheRead := 0
	cacheWrite := 0
	if u.PromptTokensDetails != nil {
		cacheRead = u.PromptTokensDetails.CachedTokens
		cacheWrite = u.PromptTokensDetails.CacheWriteTokens
	}
	if cacheRead == 0 {
		cacheRead = u.PromptCacheHitTokens
	}

	input := u.PromptTokens - cacheRead - cacheWrite
	if input < 0 {
		input = 0
	}
	var reasoning *int
	if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens > 0 {
		n := u.CompletionTokensDetails.ReasoningTokens
		reasoning = &n
	}

	usage := ai.Usage{
		Input: input,
		// completion_tokens already includes reasoning tokens.
		Output:      u.CompletionTokens,
		CacheRead:   cacheRead,
		CacheWrite:  cacheWrite,
		Reasoning:   reasoning,
		TotalTokens: input + u.CompletionTokens + cacheRead + cacheWrite,
	}
	usage.Cost = ai.CalculateCost(model, &usage)
	return usage
}

// providerError turns a non-2xx response into a readable error.
func providerError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	text := strings.TrimSpace(string(body))

	// Most providers wrap the useful part in {"error": {...}}.
	var wrapper struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Error) > 0 {
		var msg struct {
			Message  string          `json:"message"`
			Metadata json.RawMessage `json:"metadata"`
		}
		if err := json.Unmarshal(wrapper.Error, &msg); err == nil && msg.Message != "" {
			text = msg.Message
			// OpenRouter tucks the upstream's own error under metadata.raw.
			if len(msg.Metadata) > 0 {
				var meta struct {
					Raw string `json:"raw"`
				}
				if json.Unmarshal(msg.Metadata, &meta) == nil && meta.Raw != "" &&
					!strings.Contains(text, meta.Raw) {
					text += "\n" + meta.Raw
				}
			}
		} else {
			text = string(wrapper.Error)
		}
	}

	if text == "" {
		text = resp.Status
	}
	return fmt.Errorf("%s: %s", resp.Status, text)
}
