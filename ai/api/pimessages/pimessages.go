// Package pimessages implements the pi-messages wire API: Pi's own protocol,
// spoken by the Radius gateway and by any backend that chooses to implement it
// (a models.json provider with "api": "pi-messages" reaches one).
//
// It is the only wire where the server already speaks tau's vocabulary. The
// request is a single POST of {model, context, options} — the context goes over
// verbatim, with no per-provider message conversion — and the response is an
// SSE stream of assistant-message events. That makes this the thinnest wire in
// tau, and moves the work to the two places where a remote party is trusted
// with tau's own data structures: rebuilding the message from indexed events
// (events.go), and reporting a failure the gateway describes in its own terms.
package pimessages

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

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
	"github.com/ihavespoons/tau/ai/internal/sse"
)

const (
	defaultTimeout = 10 * time.Minute
	// maxDiagnosticBody bounds an unparseable error body kept for diagnostics.
	// Diagnostics are written into the session file, so an HTML error page from
	// a proxy must not become part of the transcript forever.
	maxDiagnosticBody = 8192
)

// ToolChoice constrains which tool the model may call: Mode is "auto", "none"
// or "required", or Name selects one specific tool.
type ToolChoice struct {
	Mode string
	Name string
}

// MarshalJSON emits the plain string form, or the function object when a tool
// is named.
func (t ToolChoice) MarshalJSON() ([]byte, error) {
	if t.Name != "" {
		return json.Marshal(struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}{Type: "function", Function: struct {
			Name string `json:"name"`
		}{Name: t.Name}})
	}
	return json.Marshal(t.Mode)
}

// Options are the pi-messages provider options.
type Options struct {
	ai.StreamOptions
	// Reasoning is the requested thinking level; empty means off. It is passed
	// through unmapped: the backend owns the model catalog and does its own
	// per-model mapping, which is the whole point of a gateway.
	Reasoning ai.ThinkingLevel
	// ToolChoice constrains tool selection.
	ToolChoice *ToolChoice
	// Debug asks the backend for routing metadata in its response headers.
	Debug bool
}

// Stream runs one turn against a pi-messages backend.
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
	o := &Options{StreamOptions: opts.StreamOptions, Reasoning: opts.Reasoning}
	// Tool choice and debug have no place in the normalized options, so they
	// travel in Extra — the same escape hatch Pi uses when it casts its simple
	// options back to the provider's own.
	if debug, ok := opts.Extra["debug"].(bool); ok {
		o.Debug = debug
	}
	switch choice := opts.Extra["toolChoice"].(type) {
	case string:
		o.ToolChoice = &ToolChoice{Mode: choice}
	case ToolChoice:
		o.ToolChoice = &choice
	case *ToolChoice:
		o.ToolChoice = choice
	}
	return Stream(ctx, model, c, o)
}

// request is the whole request body. The context is Pi's own structure, sent
// as-is: a pi-messages backend does the provider-specific conversion.
type request struct {
	Model   string        `json:"model"`
	Context ai.Context    `json:"context"`
	Options requestParams `json:"options"`
}

type requestParams struct {
	Temperature    *float64          `json:"temperature,omitempty"`
	MaxTokens      int               `json:"maxTokens,omitempty"`
	Reasoning      ai.ThinkingLevel  `json:"reasoning,omitempty"`
	CacheRetention ai.CacheRetention `json:"cacheRetention,omitempty"`
	SessionID      string            `json:"sessionId,omitempty"`
	ToolChoice     *ToolChoice       `json:"toolChoice,omitempty"`
}

func buildRequest(model *ai.Model, c ai.Context, opts *Options) request {
	return request{
		Model:   model.ID,
		Context: c,
		Options: requestParams{
			Temperature:    opts.Temperature,
			MaxTokens:      opts.MaxTokens,
			Reasoning:      opts.Reasoning,
			CacheRetention: resolveCacheRetention(opts),
			SessionID:      opts.SessionID,
			ToolChoice:     opts.ToolChoice,
		},
	}
}

// resolveCacheRetention leaves the field unset unless something asked for a
// retention, so the backend's own default applies. PI_CACHE_RETENTION is the
// legacy opt-in Pi still honours, and only its "long" value means anything.
func resolveCacheRetention(opts *Options) ai.CacheRetention {
	if opts.CacheRetention != "" {
		return opts.CacheRetention
	}
	if apishared.EnvValue(opts.Env, "PI_CACHE_RETENTION") == string(ai.CacheLong) {
		return ai.CacheLong
	}
	return ""
}

func messagesURL(baseURL string, debug bool) string {
	url := strings.TrimRight(baseURL, "/") + "/messages"
	if debug {
		url += "?debug=1"
	}
	return url
}

func run(ctx context.Context, stream *ai.MessageStream, model *ai.Model, c ai.Context, opts *Options) {
	conv := newConverter(model)

	defer func() {
		if r := recover(); r != nil {
			fail(ctx, stream, conv.out, fmt.Errorf("pi-messages: %v", r))
		}
	}()

	if opts.APIKey == "" {
		fail(ctx, stream, conv.out, fmt.Errorf("no API key for provider %s", model.Provider))
		return
	}

	var payload any = buildRequest(model, c, opts)
	if opts.OnPayload != nil {
		replaced, err := opts.OnPayload(payload, model)
		if err != nil {
			fail(ctx, stream, conv.out, err)
			return
		}
		if replaced != nil {
			payload = replaced
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fail(ctx, stream, conv.out, err)
		return
	}

	url := messagesURL(model.BaseURL, opts.Debug)
	resp, err := doRequest(ctx, url, model, opts, body)
	if err != nil {
		fail(ctx, stream, conv.out, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if opts.OnResponse != nil {
		if err := opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: resp.Header}, model); err != nil {
			fail(ctx, stream, conv.out, err)
			return
		}
	}
	if resp.StatusCode != http.StatusOK {
		responseError := readResponseError(model, url, resp)
		// The gateway's own account of the failure — its error code, the
		// routing it attempted — is the only record of why a request tau
		// considers well-formed was refused, so it is kept on the message
		// rather than flattened into the error string.
		conv.out.Diagnostics = append(conv.out.Diagnostics, responseError.diagnostic())
		fail(ctx, stream, conv.out, responseError)
		return
	}

	err = consume(ctx, resp.Body, stream, conv)
	if err == nil && conv.out.StopReason == ai.StopPending {
		// A stream that stopped early produced a partial answer, not a finished
		// one, and saying so is only half the reason this check is here: the
		// terminal event is what ENDS the stream, and a consumer blocked on a
		// stream that never ends waits forever. That is the difference between
		// a bad turn and a hung agent, so the invariant is enforced where the
		// stream is owned rather than trusted to the loop below.
		err = fmt.Errorf("%s stream ended without a terminal event", conv.out.Provider)
	}
	if err != nil {
		fail(ctx, stream, conv.out, err)
	}
}

// consume reads the event stream, emitting each converted event. The terminal
// event is pushed from here because the backend chooses the stop reason; run
// checks that one actually arrived.
func consume(ctx context.Context, body io.Reader, stream *ai.MessageStream, conv *converter) error {
	reader := sse.NewReader(body)
	for {
		if ctx.Err() != nil {
			return errors.New("Request was aborted") //nolint:staticcheck // Pi error-message parity
		}
		frame, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		wire, err := decodeEvent(frame.Data)
		if err != nil {
			return err
		}
		if wire == nil {
			continue
		}
		event, err := conv.convert(wire)
		if err != nil {
			return err
		}
		if event == nil {
			continue
		}
		stream.Push(*event)
		if event.IsTerminal() {
			return nil
		}
	}
	// Reaching the end of the body without a terminal event is not reported
	// here: run decides that from the accumulated message, so there is exactly
	// one place that knows whether the stream was ended.
	return nil
}

func fail(ctx context.Context, stream *ai.MessageStream, out *ai.AssistantMessage, err error) {
	reason := ai.StopError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		reason = ai.StopAborted
	}
	out.StopReason = reason
	stream.Push(ai.Event{Type: ai.EventError, Reason: reason, Error: ai.ErrorMessage(out, reason, err.Error())})
}

func doRequest(ctx context.Context, url string, model *ai.Model, opts *Options, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+opts.APIKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
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

	timeout := defaultTimeout
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	return (&http.Client{Timeout: timeout}).Do(req)
}

// responseError is a non-2xx response, decoded as far as the gateway allows.
type responseError struct {
	message string
	code    string
	details map[string]any
}

func (e *responseError) Error() string { return e.message }

func (e *responseError) diagnostic() ai.Diagnostic {
	return ai.Diagnostic{
		Type:      "pi_messages_response_failure",
		Timestamp: time.Now().UnixMilli(),
		Error: &ai.DiagnosticErrorInfo{
			Name:    "PiMessagesResponseError",
			Message: e.message,
			Code:    e.code,
		},
		Details: e.details,
	}
}

// readResponseError builds the failure from the response body.
//
// A pi-messages backend reports errors as {"error": {message, code, ...}}, but
// the response may equally come from a proxy or load balancer in front of it,
// which will not. Both paths have to produce something the user can act on, so
// an unparseable body is kept verbatim (bounded) rather than discarded for not
// matching the schema.
func readResponseError(model *ai.Model, url string, resp *http.Response) *responseError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := string(raw)

	var parsed struct {
		Error map[string]any `json:"error"`
	}
	structured := json.Unmarshal(raw, &parsed) == nil && parsed.Error != nil

	message, _ := parsed.Error["message"].(string)
	code, _ := parsed.Error["code"].(string)

	suffix := body
	if message != "" {
		suffix = message
	}
	if code != "" {
		suffix += " (" + code + ")"
	}

	details := map[string]any{
		"version":     1,
		"provider":    model.Provider,
		"model":       model.ID,
		"url":         url,
		"status":      resp.StatusCode,
		"statusText":  statusText(resp),
		"timestampMs": time.Now().UnixMilli(),
	}
	if structured {
		details["error"] = parsed.Error
	} else {
		details["body"] = truncate(body)
	}

	return &responseError{
		message: fmt.Sprintf("%d %s: %s", resp.StatusCode, statusText(resp), suffix),
		code:    code,
		details: details,
	}
}

// statusText is the reason phrase alone: Response.Status carries the code as
// well, and the diagnostic records that separately.
func statusText(resp *http.Response) string {
	if text := strings.TrimPrefix(resp.Status, fmt.Sprintf("%d ", resp.StatusCode)); text != resp.Status {
		return text
	}
	return http.StatusText(resp.StatusCode)
}

func truncate(body string) string {
	if len(body) <= maxDiagnosticBody {
		return body
	}
	return body[:maxDiagnosticBody] + "…"
}
