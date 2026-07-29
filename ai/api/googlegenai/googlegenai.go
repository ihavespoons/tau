package googlegenai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/internal/sse"
)

// Options are the google-generative-ai provider options.
type Options struct {
	ai.StreamOptions
	// Reasoning is the requested thinking level; empty means off.
	Reasoning ai.ThinkingLevel
	// ThinkingBudgets overrides the per-level token budgets on the Gemini 2
	// models that take one.
	ThinkingBudgets *ai.ThinkingBudgets
	// ToolChoice is "auto" | "none" | "any".
	ToolChoice string
	// UseLegacyParameters sends tool schemas in the OpenAPI 3.0 `parameters`
	// field rather than as full JSON Schema. Needed where the API translates
	// that field into another vendor's schema format.
	UseLegacyParameters bool
}

// Stream runs one turn against the generateContent endpoint.
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

// StreamSimple is Stream with normalized cross-provider options.
func StreamSimple(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	reasoning := opts.Reasoning
	if reasoning != "" {
		if clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(reasoning)); clamped == ai.ThinkingOff {
			reasoning = ""
		} else {
			reasoning = ai.ThinkingLevel(clamped)
		}
	}
	return Stream(ctx, model, c, &Options{
		StreamOptions:   opts.StreamOptions,
		Reasoning:       reasoning,
		ThinkingBudgets: opts.ThinkingBudgets,
	})
}

const defaultTimeout = 10 * time.Minute

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

func run(ctx context.Context, stream *ai.MessageStream, model *ai.Model, c ai.Context, opts *Options) {
	out := newOutput(model)

	defer func() {
		if r := recover(); r != nil {
			fail(stream, out, ctx, fmt.Errorf("google generative ai: %v", r))
		}
	}()

	if opts.APIKey == "" {
		fail(stream, out, ctx, fmt.Errorf("no API key for provider %s", model.Provider))
		return
	}

	body, err := encodePayload(buildRequest(model, c, opts), model, opts)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}

	resp, err := doRequest(ctx, model, opts, body)
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

	if err := consume(ctx, resp.Body, stream, out, model); err != nil {
		fail(stream, out, ctx, err)
		return
	}
	stream.Push(ai.Event{Type: ai.EventDone, Reason: out.StopReason, Message: out})
}

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
	out.StopReason = reason
	stream.Push(ai.Event{
		Type:   ai.EventError,
		Reason: reason,
		Error:  ai.ErrorMessage(out, reason, err.Error()),
	})
}

// streamURL builds the endpoint. `alt=sse` is what turns the streaming call
// into server-sent events rather than a JSON array delivered in chunks.
func streamURL(baseURL, modelID string) string {
	base := strings.TrimRight(baseURL, "/")
	return fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", base, url.PathEscape(modelID))
}

func doRequest(ctx context.Context, model *ai.Model, opts *Options, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		streamURL(model.BaseURL, model.ID), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// Google authenticates with its own header rather than a bearer token.
	req.Header.Set("x-goog-api-key", opts.APIKey)
	for k, v := range model.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range opts.Headers {
		if v == nil {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, *v)
	}

	return httpClientFor(opts.TimeoutMs).Do(req)
}

// httpClientFor applies the caller's timeout, or tau's default.
func httpClientFor(timeoutMs int) *http.Client {
	timeout := defaultTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	return &http.Client{Timeout: timeout}
}

// chunk is one streamed generateContent response.
type chunk struct {
	ResponseID string `json:"responseId"`
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
}

type usageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

// liveBlock is the open text or thinking block deltas are accumulating into.
//
// Gemini does not delimit blocks: a stream is a sequence of parts, and a block
// ends when a part of a different kind arrives. So the state machine has to
// notice the transition rather than being told about it.
type liveBlock struct {
	kind      string // text | thinking
	index     int
	text      string
	signature string
}

func consume(ctx context.Context, body io.Reader, stream *ai.MessageStream, out *ai.AssistantMessage, model *ai.Model) error {
	reader := sse.NewReader(body)
	var live *liveBlock
	toolCallSeq := 0

	closeLive := func() {
		if live == nil {
			return
		}
		evType, content := ai.EventTextEnd, live.text
		if live.kind == "thinking" {
			evType = ai.EventThinkingEnd
		}
		stream.Push(ai.Event{Type: evType, ContentIndex: live.index, Content: content, Partial: out})
		live = nil
	}

	sync := func() {
		if live == nil {
			return
		}
		if live.kind == "thinking" {
			out.Content[live.index] = ai.ThinkingContent{Thinking: live.text, ThinkingSignature: live.signature}
			return
		}
		out.Content[live.index] = ai.TextContent{Text: live.text, TextSignature: live.signature}
	}

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

		var ch chunk
		if err := json.Unmarshal([]byte(ev.Data), &ch); err != nil {
			continue
		}
		if out.ResponseID == "" {
			out.ResponseID = ch.ResponseID
		}
		if len(ch.Candidates) == 0 {
			applyUsage(out, ch.UsageMetadata, model)
			continue
		}
		candidate := ch.Candidates[0]

		for _, p := range candidate.Content.Parts {
			switch {
			case p.Text != "":
				kind := "text"
				if p.Thought {
					kind = "thinking"
				}
				if live == nil || live.kind != kind {
					closeLive()
					live = &liveBlock{kind: kind, index: len(out.Content)}
					if kind == "thinking" {
						out.Content = append(out.Content, ai.ThinkingContent{})
						stream.Push(ai.Event{Type: ai.EventThinkingStart, ContentIndex: live.index, Partial: out})
					} else {
						out.Content = append(out.Content, ai.TextContent{})
						stream.Push(ai.Event{Type: ai.EventTextStart, ContentIndex: live.index, Partial: out})
					}
				}
				live.text += p.Text
				// A signature may arrive on the first delta only, so an absent
				// one must not erase what was already captured.
				if p.ThoughtSignature != "" {
					live.signature = p.ThoughtSignature
				}
				sync()

				evType := ai.EventTextDelta
				if kind == "thinking" {
					evType = ai.EventThinkingDelta
				}
				stream.Push(ai.Event{Type: evType, ContentIndex: live.index, Delta: p.Text, Partial: out})

			case p.FunctionCall != nil:
				closeLive()
				toolCallSeq++
				call := ai.ToolCall{
					ID:               toolCallID(p.FunctionCall, out.Content, model, toolCallSeq),
					Name:             p.FunctionCall.Name,
					Arguments:        p.FunctionCall.Args,
					ThoughtSignature: p.ThoughtSignature,
				}
				if call.Arguments == nil {
					call.Arguments = map[string]any{}
				}
				out.Content = append(out.Content, call)
				index := len(out.Content) - 1

				// Gemini delivers a call whole rather than as a stream of
				// argument fragments, so the three events fire together and a
				// consumer that renders deltas still sees the arguments.
				args, _ := json.Marshal(call.Arguments)
				stream.Push(ai.Event{Type: ai.EventToolCallStart, ContentIndex: index, Partial: out})
				stream.Push(ai.Event{Type: ai.EventToolCallDelta, ContentIndex: index, Delta: string(args), Partial: out})
				stream.Push(ai.Event{Type: ai.EventToolCallEnd, ContentIndex: index, ToolCall: &call, Partial: out})
			}
		}

		if candidate.FinishReason != "" {
			out.StopReason = mapStopReason(candidate.FinishReason)
		}
		applyUsage(out, ch.UsageMetadata, model)
	}

	closeLive()

	if out.StopReason == ai.StopPending {
		return errors.New("the google stream ended without a finish reason")
	}
	if hasToolCall(out.Content) && out.StopReason == ai.StopStop {
		out.StopReason = ai.StopToolUse
	}
	return nil
}

// toolCallID keeps ids unique.
//
// Gemini only sends one for the models that require it, and even then can
// repeat it within a turn — so a missing or duplicate id gets a synthetic one.
// Two tool calls sharing an id would pair both results to the same call.
func toolCallID(call *functionCall, existing ai.ContentList, model *ai.Model, seq int) string {
	id := call.ID
	if id != "" && !hasToolCallID(existing, id) {
		return id
	}
	return fmt.Sprintf("%s_%d", call.Name, seq)
}

func hasToolCallID(content ai.ContentList, id string) bool {
	for _, block := range content {
		if tc, ok := block.(ai.ToolCall); ok && tc.ID == id {
			return true
		}
	}
	return false
}

func hasToolCall(content ai.ContentList) bool {
	for _, block := range content {
		if _, ok := block.(ai.ToolCall); ok {
			return true
		}
	}
	return false
}

// applyUsage records token counts.
//
// Gemini reports thinking tokens SEPARATELY from candidate tokens, and both
// are billed as output — so they are summed. Cached tokens are included in the
// prompt count and subtracted out, matching every other wire in tau.
func applyUsage(out *ai.AssistantMessage, u *usageMetadata, model *ai.Model) {
	if u == nil {
		return
	}
	input := u.PromptTokenCount - u.CachedContentTokenCount
	if input < 0 {
		input = 0
	}
	reasoning := u.ThoughtsTokenCount

	usage := ai.Usage{
		Input:       input,
		Output:      u.CandidatesTokenCount + u.ThoughtsTokenCount,
		CacheRead:   u.CachedContentTokenCount,
		TotalTokens: u.TotalTokenCount,
	}
	if reasoning > 0 {
		usage.Reasoning = &reasoning
	}
	usage.Cost = ai.CalculateCost(model, &usage)
	out.Usage = usage
}

// mapStopReason translates Gemini's finish reason.
//
// Everything that is not a clean stop or a length cap is an error: the rest of
// the enum is safety blocks, recitation, and malformed calls — conditions the
// user needs to see rather than a turn that silently ends early.
func mapStopReason(reason string) ai.StopReason {
	switch reason {
	case "STOP":
		return ai.StopStop
	case "MAX_TOKENS":
		return ai.StopLength
	default:
		return ai.StopError
	}
}

// providerError turns a non-2xx response into a readable error.
func providerError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	text := strings.TrimSpace(string(body))

	var wrapper struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Error.Message != "" {
		text = wrapper.Error.Message
		if wrapper.Error.Status != "" {
			text = wrapper.Error.Status + ": " + text
		}
	}
	if text == "" {
		text = resp.Status
	}
	return fmt.Errorf("%s: %s", resp.Status, text)
}
