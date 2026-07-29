package openairesp

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

// Options are the openai-responses provider options.
type Options struct {
	ai.StreamOptions
	// Reasoning is the effort level to request; empty means off.
	Reasoning ai.ThinkingLevel
	// ReasoningSummary is "auto" | "detailed" | "concise"; empty means auto
	// whenever reasoning is on.
	ReasoningSummary string
	// ServiceTier selects a priced service class ("flex", "priority").
	ServiceTier string
	// ToolChoice is "auto" | "none" | "required", or an object naming a tool.
	ToolChoice any
}

// Stream runs one turn against the responses endpoint.
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
// level is clamped to what this model supports before being requested.
func StreamSimple(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	return Stream(ctx, model, c, &Options{
		StreamOptions: opts.StreamOptions,
		Reasoning:     clampReasoning(model, opts.Reasoning),
	})
}

// clampReasoning holds a requested level to what the model actually supports,
// and turns an unsupported one off rather than sending a name the endpoint
// will reject.
func clampReasoning(model *ai.Model, level ai.ThinkingLevel) ai.ThinkingLevel {
	if level == "" {
		return ""
	}
	clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(level))
	if clamped == ai.ThinkingOff {
		return ""
	}
	return ai.ThinkingLevel(clamped)
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

// run performs the request and drains the stream, converting any failure into
// a terminal error event.
func run(ctx context.Context, stream *ai.MessageStream, model *ai.Model, c ai.Context, opts *Options) {
	out := newOutput(model)

	defer func() {
		if r := recover(); r != nil {
			fail(stream, out, ctx, fmt.Errorf("openai responses: %v", r))
		}
	}()

	cm := resolveCompat(model)
	req := buildRequest(model, c, opts, cm)

	body, err := encodePayload(req, model, opts)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}

	resp, err := doRequest(ctx, model, opts, cm, body)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if opts.OnResponse != nil {
		if err := opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: resp.Header}, model); err != nil {
			fail(stream, out, ctx, err)
			return
		}
	}
	if resp.StatusCode != http.StatusOK {
		fail(stream, out, ctx, providerError(resp))
		return
	}

	stream.Push(ai.Event{Type: ai.EventStart, Partial: out})

	if err := consume(ctx, resp.Body, stream, out, model, opts); err != nil {
		fail(stream, out, ctx, err)
		return
	}

	stream.Push(ai.Event{Type: ai.EventDone, Reason: out.StopReason, Message: out})
}

// encodePayload marshals the request, giving the caller a chance to replace it.
func encodePayload(req request, model *ai.Model, opts *Options) ([]byte, error) {
	var payload any = req
	if opts.OnPayload != nil {
		replaced, err := opts.OnPayload(req, model)
		if err != nil {
			return nil, err
		}
		if replaced != nil {
			payload = replaced
		}
	}
	return json.Marshal(payload)
}

func fail(stream *ai.MessageStream, out *ai.AssistantMessage, ctx context.Context, err error) {
	reason := ai.StopError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		reason = ai.StopAborted
	}
	// Streaming scratch state is never persisted; a partial block keeps only
	// what it has parsed so far.
	out.StopReason = reason
	stream.Push(ai.Event{
		Type:   ai.EventError,
		Reason: reason,
		Error:  ai.ErrorMessage(out, reason, err.Error()),
	})
}

// slot is one live output item. The wire addresses items by index, and an
// index is reused after its item completes, so a slot is deleted on done
// rather than left behind.
type slot struct {
	kind        string // thinking | text | toolCall
	index       int    // index into out.Content
	text        string
	thinking    string
	signature   string
	toolID      string
	toolName    string
	partialJSON string
	args        map[string]any
	hasPartial  bool
}

func (s *slot) materialize() ai.Content {
	switch s.kind {
	case "thinking":
		return ai.ThinkingContent{Thinking: s.thinking, ThinkingSignature: s.signature}
	case "toolCall":
		args := s.args
		if args == nil {
			args = map[string]any{}
		}
		return ai.ToolCall{ID: s.toolID, Name: s.toolName, Arguments: args}
	default:
		return ai.TextContent{Text: s.text, TextSignature: s.signature}
	}
}

// state tracks the live items of one response.
type state struct {
	out      *ai.AssistantMessage
	byIndex  map[int]*slot
	reasonBy map[string]*slot // reasoning item id → slot, for signature backfill
}

func newState(out *ai.AssistantMessage) *state {
	return &state{out: out, byIndex: map[int]*slot{}, reasonBy: map[string]*slot{}}
}

// sync writes a slot's current value back into the message.
func (s *state) sync(sl *slot) {
	s.out.Content[sl.index] = sl.materialize()
}

// open creates the slot for an item, if it is one tau represents.
func (s *state) open(index int, it outputItem) *slot {
	var sl *slot
	switch it.Type {
	case "reasoning":
		sl = &slot{kind: "thinking"}
	case "message":
		sl = &slot{kind: "text"}
	case "function_call":
		sl = &slot{
			kind: "toolCall", toolID: it.CallID + "|" + it.ID, toolName: it.Name,
			partialJSON: it.Arguments, hasPartial: true,
		}
	default:
		return nil
	}

	s.out.Content = append(s.out.Content, sl.materialize())
	sl.index = len(s.out.Content) - 1
	s.byIndex[index] = sl
	return sl
}

func (s *state) get(index int, kind string) *slot {
	sl, ok := s.byIndex[index]
	if !ok || sl.kind != kind {
		return nil
	}
	return sl
}

// consume drains the SSE stream.
func consume(ctx context.Context, body io.Reader, stream *ai.MessageStream, out *ai.AssistantMessage, model *ai.Model, opts *Options) error {
	st := newState(out)
	sawTerminal := false

	reader := sse.NewReader(body)
	for {
		if ctx.Err() != nil {
			return errors.New("Request was aborted") //nolint:staticcheck // Pi error-message parity
		}
		ev, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if ev == nil || ev.Data == "" || ev.Data == "[DONE]" {
			continue
		}

		var e streamEvent
		if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
			continue // a frame tau does not understand is not a failure
		}

		switch e.Type {
		case "response.created":
			if e.Response != nil {
				out.ResponseID = e.Response.ID
			}

		case "response.output_item.added":
			it, err := decodeOutputItem(e.Item)
			if err != nil {
				continue
			}
			if sl := st.open(e.OutputIndex, it); sl != nil {
				stream.Push(ai.Event{Type: startEvent(sl.kind), ContentIndex: sl.index, Partial: out})
			}

		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			appendThinking(st, stream, e.OutputIndex, e.Delta)

		case "response.reasoning_summary_part.done":
			// A summary part ends; the blank line keeps successive parts from
			// running together into one paragraph.
			appendThinking(st, stream, e.OutputIndex, "\n\n")

		case "response.output_text.delta", "response.refusal.delta":
			sl := st.get(e.OutputIndex, "text")
			if sl == nil {
				continue
			}
			sl.text += e.Delta
			st.sync(sl)
			stream.Push(ai.Event{
				Type: ai.EventTextDelta, ContentIndex: sl.index, Delta: e.Delta, Partial: out,
			})

		case "response.function_call_arguments.delta":
			sl := st.get(e.OutputIndex, "toolCall")
			if sl == nil || !sl.hasPartial {
				continue
			}
			sl.partialJSON += e.Delta
			sl.args = partialjson.ParseStreaming(sl.partialJSON)
			st.sync(sl)
			stream.Push(ai.Event{
				Type: ai.EventToolCallDelta, ContentIndex: sl.index, Delta: e.Delta, Partial: out,
			})

		case "response.function_call_arguments.done":
			sl := st.get(e.OutputIndex, "toolCall")
			if sl == nil || !sl.hasPartial {
				continue
			}
			previous := sl.partialJSON
			sl.partialJSON = e.Arguments
			sl.args = partialjson.ParseStreaming(sl.partialJSON)
			st.sync(sl)
			// Emit only what the caller has not seen. The done event repeats
			// the whole argument string, and replaying it would double every
			// character in a UI that concatenates deltas.
			if strings.HasPrefix(e.Arguments, previous) && len(e.Arguments) > len(previous) {
				stream.Push(ai.Event{
					Type: ai.EventToolCallDelta, ContentIndex: sl.index,
					Delta: e.Arguments[len(previous):], Partial: out,
				})
			}

		case "response.output_item.done":
			it, err := decodeOutputItem(e.Item)
			if err != nil {
				continue
			}
			closeItem(st, stream, e.OutputIndex, it, e.Item)

		// response.done is the ChatGPT backend's spelling of response.completed.
		// Without it that stream ends with no terminal event and the turn is
		// reported as truncated — a failure that reads like a network problem.
		case "response.completed", "response.incomplete", "response.done":
			sawTerminal = true
			finalize(st, e.Response, model, opts)

		case "response.failed":
			// No sawTerminal here: the error return IS the terminal outcome,
			// and setting it would only be read on a path that cannot run.
			return responseFailure(e.Response)

		case "error":
			return fmt.Errorf("error code %s: %s", e.Code, e.Message)
		}
	}

	if !sawTerminal {
		return errors.New("the responses stream ended before a terminal event")
	}
	return nil
}

func startEvent(kind string) ai.EventType {
	switch kind {
	case "thinking":
		return ai.EventThinkingStart
	case "toolCall":
		return ai.EventToolCallStart
	default:
		return ai.EventTextStart
	}
}

func appendThinking(st *state, stream *ai.MessageStream, index int, delta string) {
	sl := st.get(index, "thinking")
	if sl == nil {
		return
	}
	sl.thinking += delta
	st.sync(sl)
	stream.Push(ai.Event{
		Type: ai.EventThinkingDelta, ContentIndex: sl.index, Delta: delta, Partial: st.out,
	})
}

// closeItem finalizes a slot from the item's completed form, which is
// authoritative over anything assembled from deltas.
func closeItem(st *state, stream *ai.MessageStream, index int, it outputItem, raw json.RawMessage) {
	sl, ok := st.byIndex[index]
	if !ok {
		// The wire may report an item tau never saw start.
		if sl = st.open(index, it); sl == nil {
			return
		}
	}

	switch {
	case it.Type == "reasoning" && sl.kind == "thinking":
		if text := joinParts(it.Summary); text != "" {
			sl.thinking = text
		} else if text := joinParts(it.Content); text != "" {
			sl.thinking = text
		}
		// The whole item is kept as the signature: replaying a reconstruction
		// would lose the encrypted payload the model needs to continue.
		sl.signature = string(raw)
		st.sync(sl)
		st.reasonBy[it.ID] = sl
		stream.Push(ai.Event{
			Type: ai.EventThinkingEnd, ContentIndex: sl.index, Content: sl.thinking, Partial: st.out,
		})
		delete(st.byIndex, index)

	case it.Type == "message" && sl.kind == "text":
		if text := joinMessageContent(it.MessageContent); text != "" {
			sl.text = text
		}
		sl.signature = encodeTextSignature(it.ID, it.Phase)
		st.sync(sl)
		stream.Push(ai.Event{
			Type: ai.EventTextEnd, ContentIndex: sl.index, Content: sl.text, Partial: st.out,
		})
		delete(st.byIndex, index)

	case it.Type == "function_call" && sl.kind == "toolCall":
		source := it.Arguments
		if source == "" {
			source = sl.partialJSON
		}
		if source == "" {
			source = "{}"
		}
		sl.args = partialjson.ParseStreaming(source)
		sl.hasPartial = false
		st.sync(sl)
		call, _ := st.out.Content[sl.index].(ai.ToolCall)
		stream.Push(ai.Event{
			Type: ai.EventToolCallEnd, ContentIndex: sl.index, ToolCall: &call, Partial: st.out,
		})
		delete(st.byIndex, index)
	}
}

func joinParts(parts []summaryPart) string {
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func joinMessageContent(parts []outputContent) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "output_text" {
			b.WriteString(p.Text)
		} else {
			b.WriteString(p.Refusal)
		}
	}
	return b.String()
}

// finalize records usage and the stop reason from the terminal response.
func finalize(st *state, resp *responseBody, model *ai.Model, opts *Options) {
	if resp == nil {
		st.out.StopReason = ai.StopStop
		return
	}
	backfillReasoningSignatures(st, resp.Output)

	if resp.ID != "" {
		st.out.ResponseID = resp.ID
	}
	if resp.Usage != nil {
		st.out.Usage = parseUsage(resp.Usage, model)
	}
	applyServiceTierPricing(&st.out.Usage, serviceTierOf(resp, opts), model)

	st.out.StopReason = mapStopReason(resp.Status)
	if st.out.StopReason == ai.StopStop && hasToolCall(st.out.Content) {
		st.out.StopReason = ai.StopToolUse
	}
}

// backfillReasoningSignatures fills in an encrypted payload that arrived only
// on the terminal response.
//
// Azure omits it from the per-item done event and supplies it once at the end.
// Without this a replayed turn carries a reasoning item with no payload, which
// the endpoint rejects — and the session only fails on the SECOND turn, which
// makes it a miserable thing to diagnose.
func backfillReasoningSignatures(st *state, output []json.RawMessage) {
	for _, raw := range output {
		it, err := decodeOutputItem(raw)
		if err != nil || it.Type != "reasoning" || it.EncryptedContent == "" {
			continue
		}
		sl, ok := st.reasonBy[it.ID]
		if !ok || sl.signature == "" {
			continue
		}
		var stored outputItem
		if err := json.Unmarshal([]byte(sl.signature), &stored); err != nil || stored.EncryptedContent != "" {
			continue
		}
		sl.signature = string(raw)
		st.sync(sl)
	}
}

func hasToolCall(content ai.ContentList) bool {
	for _, block := range content {
		if _, ok := block.(ai.ToolCall); ok {
			return true
		}
	}
	return false
}

func serviceTierOf(resp *responseBody, opts *Options) string {
	if resp != nil && resp.ServiceTier != "" {
		return resp.ServiceTier
	}
	return opts.ServiceTier
}

// applyServiceTierPricing scales cost by the tier actually served.
//
// The catalog records standard rates; flex is billed at half and priority at a
// premium, so a session on either would otherwise report a cost that is simply
// wrong.
func applyServiceTierPricing(usage *ai.Usage, tier string, model *ai.Model) {
	multiplier := 1.0
	switch tier {
	case "flex":
		multiplier = 0.5
	case "priority":
		multiplier = 2
		if model.ID == "gpt-5.5" {
			multiplier = 2.5
		}
	}
	if multiplier == 1 {
		return
	}
	usage.Cost.Input *= multiplier
	usage.Cost.Output *= multiplier
	usage.Cost.CacheRead *= multiplier
	usage.Cost.CacheWrite *= multiplier
	usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
}

// parseUsage normalizes the endpoint's token counts.
//
// input_tokens INCLUDES both cached and cache-write tokens here, so both are
// subtracted to leave the uncached input — which is what every other wire in
// tau reports and what the cost calculation expects.
func parseUsage(u *usagePayload, model *ai.Model) ai.Usage {
	cacheRead, cacheWrite := 0, 0
	if u.InputTokensDetails != nil {
		cacheRead = u.InputTokensDetails.CachedTokens
		cacheWrite = u.InputTokensDetails.CacheWriteTokens
	}
	input := u.InputTokens - cacheRead - cacheWrite
	if input < 0 {
		input = 0
	}

	var reasoningTokens *int
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ReasoningTokens > 0 {
		n := u.OutputTokensDetails.ReasoningTokens
		reasoningTokens = &n
	}

	usage := ai.Usage{
		Input:       input,
		Output:      u.OutputTokens,
		CacheRead:   cacheRead,
		CacheWrite:  cacheWrite,
		Reasoning:   reasoningTokens,
		TotalTokens: u.TotalTokens,
	}
	usage.Cost = ai.CalculateCost(model, &usage)
	return usage
}

func mapStopReason(status string) ai.StopReason {
	switch status {
	case "completed", "":
		return ai.StopStop
	case "incomplete":
		return ai.StopLength
	case "failed", "cancelled":
		return ai.StopError
	default:
		// in_progress and queued arrive on a terminal event only when the
		// endpoint is confused; treat them as a normal stop rather than an
		// error the user cannot act on.
		return ai.StopStop
	}
}

// responseFailure turns a response.failed event into the most specific message
// available.
func responseFailure(resp *responseBody) error {
	if resp == nil {
		return errors.New("the response failed with no details")
	}
	if resp.Error != nil {
		code := resp.Error.Code
		if code == "" {
			code = "unknown"
		}
		message := resp.Error.Message
		if message == "" {
			message = "no message"
		}
		return fmt.Errorf("%s: %s", code, message)
	}
	if resp.Incomplete != nil && resp.Incomplete.Reason != "" {
		return fmt.Errorf("incomplete: %s", resp.Incomplete.Reason)
	}
	return errors.New("the response failed with no error details")
}

// providerError turns a non-2xx response into a readable error.
func providerError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	text := strings.TrimSpace(string(body))

	var wrapper struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && len(wrapper.Error) > 0 {
		var msg struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(wrapper.Error, &msg); err == nil && msg.Message != "" {
			text = msg.Message
		} else {
			text = string(wrapper.Error)
		}
	}
	if text == "" {
		text = resp.Status
	}
	return fmt.Errorf("%s: %s", resp.Status, text)
}
