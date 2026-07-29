package mistralconv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/internal/sse"
	"github.com/ihavespoons/tau/ai/partialjson"
)

// Options are the mistral-conversations provider options.
type Options struct {
	ai.StreamOptions
	// Reasoning is the requested thinking level; empty means off. Mistral
	// exposes reasoning as on or off rather than as tiers.
	Reasoning ai.ThinkingLevel
	// ToolChoice is "auto" | "none" | "any" | "required".
	ToolChoice string
}

// Stream runs one turn against Mistral.
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
	return Stream(ctx, model, c, &Options{StreamOptions: opts.StreamOptions, Reasoning: reasoning})
}

// request is the chat body. Every name is camelCase, which is the single
// biggest difference from the OpenAI shape it otherwise resembles.
type request struct {
	Model    string    `json:"model"`
	Stream   bool      `json:"stream"`
	Messages []message `json:"messages"`

	Tools      []tool `json:"tools,omitempty"`
	ToolChoice string `json:"toolChoice,omitempty"`

	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"maxTokens,omitempty"`

	PromptMode      string `json:"promptMode,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	PromptCacheKey  string `json:"promptCacheKey,omitempty"`
}

type tool struct {
	Type     string   `json:"type"`
	Function toolSpec `json:"function"`
}

type toolSpec struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Parameters  *jsonschema.Schema `json:"parameters,omitempty"`
	Strict      bool               `json:"strict"`
}

func buildRequest(model *ai.Model, c ai.Context, opts *Options) request {
	req := request{
		Model:       model.ID,
		Stream:      true,
		Messages:    convertMessages(model, c),
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
	}

	for _, t := range c.Tools {
		req.Tools = append(req.Tools, tool{
			Type:     "function",
			Function: toolSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	if opts.ToolChoice != "" {
		req.ToolChoice = opts.ToolChoice
	}

	// Mistral's reasoning is a mode plus an effort, and it only has two
	// settings — anything tau would call low or high is the same "high" here.
	if model.Reasoning && opts.Reasoning != "" {
		req.PromptMode = "reasoning"
		req.ReasoningEffort = "high"
	}

	if cachingEnabled(opts) {
		req.PromptCacheKey = opts.SessionID
	}
	return req
}

// cachingEnabled reports whether this turn should participate in prefix
// caching. It needs a session id to key on, and the user may have turned
// caching off.
func cachingEnabled(opts *Options) bool {
	return opts.CacheRetention != ai.CacheNone && opts.SessionID != ""
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
			fail(stream, out, ctx, fmt.Errorf("mistral: %v", r))
		}
	}()

	if opts.APIKey == "" {
		fail(stream, out, ctx, fmt.Errorf("no API key for provider %s", model.Provider))
		return
	}

	var payload any = buildRequest(model, c, opts)
	if opts.OnPayload != nil {
		replaced, err := opts.OnPayload(payload, model)
		if err != nil {
			fail(stream, out, ctx, err)
			return
		}
		if replaced != nil {
			payload = replaced
		}
	}
	body, err := json.Marshal(payload)
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

func fail(stream *ai.MessageStream, out *ai.AssistantMessage, ctx context.Context, err error) {
	reason := ai.StopError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		reason = ai.StopAborted
	}
	out.StopReason = reason
	stream.Push(ai.Event{Type: ai.EventError, Reason: reason, Error: ai.ErrorMessage(out, reason, err.Error())})
}

func chatURL(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(trimmed, "/chat/completions") {
		return trimmed
	}
	if !strings.HasSuffix(trimmed, "/v1") {
		trimmed += "/v1"
	}
	return trimmed + "/chat/completions"
}

func doRequest(ctx context.Context, model *ai.Model, opts *Options, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(model.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+opts.APIKey)
	for k, v := range model.Headers {
		req.Header.Set(k, v)
	}
	// Mistral's infrastructure keys its prefix cache off this header: without
	// it a multi-turn conversation lands on a different replica each turn and
	// re-reads the whole prompt.
	if cachingEnabled(opts) {
		req.Header.Set("x-affinity", opts.SessionID)
	}
	for k, v := range opts.Headers {
		if v == nil {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, *v)
	}

	timeout := defaultTimeout
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	return (&http.Client{Timeout: timeout}).Do(req)
}

// streamChunk is one streamed completion event.
type streamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			// Content is a plain string on some turns and an array of chunks
			// on others, so it is decoded loosely.
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Index    *int   `json:"index"`
				Function struct {
					Name string `json:"name"`
					// Arguments is a string on most turns and an object when
					// the model emits them whole.
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"toolCalls"`
		} `json:"delta"`
		FinishReason string `json:"finishReason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

type usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`

	// Mistral has published the cached-token count under several names; each
	// is read so a rename does not silently start reporting zero.
	PromptTokensDetails *struct {
		CachedTokens int `json:"cachedTokens"`
	} `json:"promptTokensDetails"`
	PromptTokensDetailsSnake *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	NumCachedTokens *int `json:"numCachedTokens"`
}

// cached returns the cached prompt tokens, capped at the prompt total.
func (u *usage) cached() int {
	n := 0
	switch {
	case u.PromptTokensDetails != nil:
		n = u.PromptTokensDetails.CachedTokens
	case u.PromptTokensDetailsSnake != nil:
		n = u.PromptTokensDetailsSnake.CachedTokens
	case u.NumCachedTokens != nil:
		n = *u.NumCachedTokens
	}
	return max(0, min(n, u.PromptTokens))
}

// liveBlock is the open text or thinking block deltas accumulate into.
type liveBlock struct {
	kind  string // text | thinking
	index int
	text  string
}

func consume(ctx context.Context, body io.Reader, stream *ai.MessageStream, out *ai.AssistantMessage, model *ai.Model) error {
	reader := sse.NewReader(body)
	var live *liveBlock
	toolIndexes := map[string]int{}
	partial := map[string]string{}

	closeLive := func() {
		if live == nil {
			return
		}
		evType := ai.EventTextEnd
		if live.kind == "thinking" {
			evType = ai.EventThinkingEnd
		}
		stream.Push(ai.Event{Type: evType, ContentIndex: live.index, Content: live.text, Partial: out})
		live = nil
	}
	appendText := func(kind, text string) {
		if text == "" {
			return
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
		live.text += text
		if live.kind == "thinking" {
			out.Content[live.index] = ai.ThinkingContent{Thinking: live.text}
			stream.Push(ai.Event{Type: ai.EventThinkingDelta, ContentIndex: live.index, Delta: text, Partial: out})
			return
		}
		out.Content[live.index] = ai.TextContent{Text: live.text}
		stream.Push(ai.Event{Type: ai.EventTextDelta, ContentIndex: live.index, Delta: text, Partial: out})
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

		var ch streamChunk
		if err := json.Unmarshal([]byte(ev.Data), &ch); err != nil {
			continue
		}
		if out.ResponseID == "" {
			out.ResponseID = ch.ID
		}
		if ch.Usage != nil {
			applyUsage(out, ch.Usage, model)
		}
		if len(ch.Choices) == 0 {
			continue
		}
		choice := ch.Choices[0]

		for _, part := range decodeContent(choice.Delta.Content) {
			appendText(part.kind, part.text)
		}

		for _, call := range choice.Delta.ToolCalls {
			closeLive()

			// A tool call arrives across several deltas keyed by index, and
			// the id only comes with the first. Keying on both keeps two
			// parallel calls apart when they share neither.
			idx := 0
			if call.Index != nil {
				idx = *call.Index
			}
			id := call.ID
			if id == "" || id == "null" {
				id = deriveToolCallID(fmt.Sprintf("toolcall:%d", idx), 0)
			}
			key := fmt.Sprintf("%s:%d", id, idx)

			contentIndex, seen := toolIndexes[key]
			if !seen {
				contentIndex = len(out.Content)
				out.Content = append(out.Content, ai.ToolCall{
					ID: id, Name: call.Function.Name, Arguments: map[string]any{},
				})
				toolIndexes[key] = contentIndex
				stream.Push(ai.Event{Type: ai.EventToolCallStart, ContentIndex: contentIndex, Partial: out})
			}

			delta := argumentsDelta(call.Function.Arguments)
			partial[key] += delta
			existing, _ := out.Content[contentIndex].(ai.ToolCall)
			existing.Arguments = partialjson.ParseStreaming(partial[key])
			if existing.Arguments == nil {
				existing.Arguments = map[string]any{}
			}
			out.Content[contentIndex] = existing
			stream.Push(ai.Event{
				Type: ai.EventToolCallDelta, ContentIndex: contentIndex, Delta: delta, Partial: out,
			})
		}

		if choice.FinishReason != "" {
			out.StopReason = mapStopReason(choice.FinishReason)
		}
	}

	closeLive()

	// Every open tool call is finished once, after the stream ends: Mistral
	// sends no per-call terminator.
	for _, contentIndex := range sortedIndexes(toolIndexes) {
		call, _ := out.Content[contentIndex].(ai.ToolCall)
		stream.Push(ai.Event{
			Type: ai.EventToolCallEnd, ContentIndex: contentIndex, ToolCall: &call, Partial: out,
		})
	}

	if out.StopReason == ai.StopPending {
		return errors.New("the mistral stream ended without a finish reason")
	}
	return nil
}

func sortedIndexes(m map[string]int) []int {
	out := make([]int, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	// Content order, so a consumer sees calls finish in the order they opened.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// textPart is one decoded content delta.
type textPart struct{ kind, text string }

// decodeContent reads a content delta, which is a plain string on some turns
// and an array of typed chunks on others.
func decodeContent(raw json.RawMessage) []textPart {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return []textPart{{kind: "text", text: plain}}
	}

	var chunks []chunk
	if err := json.Unmarshal(raw, &chunks); err != nil {
		return nil
	}

	var out []textPart
	for _, c := range chunks {
		switch c.Type {
		case "text":
			out = append(out, textPart{kind: "text", text: c.Text})
		case "thinking":
			var b strings.Builder
			for _, inner := range c.Thinking {
				b.WriteString(inner.Text)
			}
			out = append(out, textPart{kind: "thinking", text: b.String()})
		}
	}
	return out
}

// argumentsDelta reads a tool-argument fragment, which is a JSON string when
// streamed and an object when the model emits the call whole.
func argumentsDelta(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func applyUsage(out *ai.AssistantMessage, u *usage, model *ai.Model) {
	cached := u.cached()
	usage := ai.Usage{
		Input:       max(0, u.PromptTokens-cached),
		Output:      u.CompletionTokens,
		CacheRead:   cached,
		TotalTokens: u.TotalTokens,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.Input + usage.Output + usage.CacheRead
	}
	usage.Cost = ai.CalculateCost(model, &usage)
	out.Usage = usage
}

func mapStopReason(reason string) ai.StopReason {
	switch reason {
	case "stop":
		return ai.StopStop
	case "length", "model_length":
		return ai.StopLength
	case "tool_calls":
		return ai.StopToolUse
	case "error":
		return ai.StopError
	default:
		return ai.StopStop
	}
}

// maxErrorBody caps how much of a failure body is quoted. Mistral can return a
// very long validation error, and the whole of it in a terminal is unreadable.
const maxErrorBody = 4000

func providerError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	text := strings.TrimSpace(string(body))

	var wrapper struct {
		Message string `json:"message"`
		Detail  any    `json:"detail"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Message != "" {
		text = wrapper.Message
	}
	if len(text) > maxErrorBody {
		text = fmt.Sprintf("%s... [truncated %d chars]", text[:maxErrorBody], len(text)-maxErrorBody)
	}
	if text == "" {
		text = resp.Status
	}
	return fmt.Errorf("%s: %s", resp.Status, text)
}
