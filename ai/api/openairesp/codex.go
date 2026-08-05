package openairesp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// Codex is the ChatGPT subscription backend. It speaks the responses protocol,
// so it lives here beside the API version — but it is addressed differently,
// authenticated differently, and shaped differently enough to be worth naming.
//
// The differences that matter:
//   - The system prompt goes in `instructions` rather than as a developer
//     message. Sending it as a message is accepted and then ignored.
//   - Every request must name the ChatGPT account, which is only available
//     inside the access token's own claims.
//   - The terminal event is `response.done`, which the API version never sends.
//   - Reasoning is always requested with its encrypted payload, because a
//     subscription conversation is long-running and losing the model's train of
//     thought halfway through is the failure people notice.
//
// NOT ported: Pi also speaks this endpoint over a cached WebSocket with zstd
// request compression. That is a latency optimization with an SSE fallback
// already built into it; tau takes the fallback path, which is the one Pi uses
// whenever the socket is unavailable.

const (
	codexBaseURL = "https://chatgpt.com/backend-api"
	// codexAuthClaim is where the ChatGPT account id hides inside the access
	// token. There is no endpoint that returns it.
	codexAuthClaim = "https://api.openai.com/auth"
)

// CodexOptions configures the Codex transport.
type CodexOptions struct {
	Options
	// TextVerbosity is "low" | "medium" | "high"; empty means low, which is
	// what the official client asks for.
	TextVerbosity string
}

// StreamCodex runs one turn against the ChatGPT backend.
func StreamCodex(ctx context.Context, model *ai.Model, c ai.Context, opts *CodexOptions) *ai.MessageStream {
	if opts == nil {
		opts = &CodexOptions{}
	}
	stream := ai.NewMessageStream()
	go runCodex(ctx, stream, model, c, opts)
	return stream
}

// StreamSimpleCodex is StreamCodex with normalized cross-provider options.
func StreamSimpleCodex(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	return StreamCodex(ctx, model, c, &CodexOptions{Options: Options{
		StreamOptions: opts.StreamOptions,
		Reasoning:     clampReasoning(model, opts.Reasoning),
	}})
}

func runCodex(ctx context.Context, stream *ai.MessageStream, model *ai.Model, c ai.Context, opts *CodexOptions) {
	out := newOutput(model)

	defer func() {
		if r := recover(); r != nil {
			fail(stream, out, ctx, fmt.Errorf("codex: %v", r))
		}
	}()

	if opts.APIKey == "" {
		fail(stream, out, ctx, fmt.Errorf("codex needs a ChatGPT login: run `tau login`"))
		return
	}
	accountID, err := codexAccountID(opts.APIKey)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}

	cm := resolveCodexCompat(model)
	// Which tools are grammar tools is decided once, from the tool definitions.
	// Both the request and the RESPONSE need it: a replayed call carries only a
	// name and arguments, and a streamed custom tool call carries only a name
	// and raw text — neither says which argument that text belongs in.
	grammar, err := apishared.GrammarToolInputProperties(c.Tools, cm.SupportsOpenAIGrammarTools)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}
	req, err := buildCodexRequest(model, c, opts, cm, grammar)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}

	body, err2 := encodeCodexPayload(req, model, &opts.Options)
	if err = err2; err != nil {
		fail(stream, out, ctx, err)
		return
	}

	// Retried on the same policy every other wire uses: a 429 or a 5xx from a
	// gateway is routine, and without this one costs the whole turn.
	resp, err := apishared.RetryRequest(ctx, func() (*http.Response, error) {
		return doCodexRequest(ctx, model, opts, accountID, body)
	}, opts.MaxRetries, opts.MaxRetryDelayMs)
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
	if err := consume(ctx, resp.Body, stream, out, model, &opts.Options, grammar); err != nil {
		fail(stream, out, ctx, err)
		return
	}
	stream.Push(ai.Event{Type: ai.EventDone, Reason: out.StopReason, Message: out})
}

// resolveCodexCompat: the backend accepts strict tool schemas and grammar
// tools, and the catalog does not record that per model because it is a
// property of the surface.
func resolveCodexCompat(model *ai.Model) compat {
	cm := resolveCompat(model)
	if model.Compat == nil || model.Compat.SupportsStrictMode == nil {
		cm.SupportsStrictMode = true
	}
	return cm
}

// codexRequest is the ChatGPT backend's body. It differs from the API version
// enough to be its own type rather than a patched one.
type codexRequest struct {
	Model   string   `json:"model"`
	Store   bool     `json:"store"`
	Stream  bool     `json:"stream"`
	Input   []item   `json:"input"`
	Include []string `json:"include,omitempty"`

	// Instructions is where the system prompt goes. Sent as a message it is
	// accepted and then ignored, which looks like the model disregarding it.
	Instructions string `json:"instructions"`

	Text              *codexText `json:"text,omitempty"`
	PromptCacheKey    string     `json:"prompt_cache_key,omitempty"`
	ToolChoice        any        `json:"tool_choice,omitempty"`
	ParallelToolCalls bool       `json:"parallel_tool_calls"`
	Tools             []tool     `json:"tools,omitempty"`
	Temperature       *float64   `json:"temperature,omitempty"`
	ServiceTier       string     `json:"service_tier,omitempty"`
	Reasoning         *reasoning `json:"reasoning,omitempty"`
}

type codexText struct {
	Verbosity string `json:"verbosity"`
}

// defaultInstructions is what the backend gets when the caller supplies no
// system prompt. It rejects an empty instructions field.
const defaultInstructions = "You are a helpful assistant."

func buildCodexRequest(model *ai.Model, c ai.Context, opts *CodexOptions, cm compat, grammar map[string]string) (codexRequest, error) {
	immediate, deferredTools := apishared.SplitDeferredTools(c, cm.SupportsToolSearch, nil)
	deferred := make(map[string]ai.Tool, len(deferredTools))
	for _, t := range deferredTools {
		deferred[t.Name] = t
	}

	verbosity := opts.TextVerbosity
	if verbosity == "" {
		verbosity = "low"
	}
	instructions := c.SystemPrompt
	if instructions == "" {
		instructions = defaultInstructions
	}

	// The system prompt is deliberately excluded from the message list: it
	// travels in `instructions` instead, and sending it twice wastes the
	// tokens of the largest single item in the request.
	withoutSystem := c
	withoutSystem.SystemPrompt = ""

	input, err := convertMessages(model, withoutSystem, cm, deferred, grammar)
	if err != nil {
		return codexRequest{}, err
	}

	req := codexRequest{
		Model:  model.ID,
		Store:  false,
		Stream: true,
		Input:  input,
		// Always: a subscription conversation runs long, and losing the
		// model's train of thought halfway through is the failure people
		// actually notice.
		Include:           []string{"reasoning.encrypted_content"},
		Instructions:      instructions,
		Text:              &codexText{Verbosity: verbosity},
		PromptCacheKey:    clampPromptCacheKey(opts.SessionID),
		ParallelToolCalls: true,
		Temperature:       opts.Temperature,
		ServiceTier:       opts.ServiceTier,
	}

	req.ToolChoice = opts.ToolChoice
	if req.ToolChoice == nil {
		req.ToolChoice = "auto"
	}
	if len(immediate) > 0 {
		tools, err := convertTools(immediate, cm, false)
		if err != nil {
			return codexRequest{}, err
		}
		req.Tools = tools
	}

	if opts.Reasoning != "" {
		if effort, ok := mappedEffort(model, opts.Reasoning); ok {
			summary := opts.ReasoningSummary
			if summary == "" {
				summary = "auto"
			}
			req.Reasoning = &reasoning{Effort: effort, Summary: summary}
		}
	}
	return req, nil
}

func encodeCodexPayload(req codexRequest, model *ai.Model, opts *Options) ([]byte, error) {
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

// codexURL builds the endpoint, tolerating a base URL that already names part
// of the path.
func codexURL(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		raw = codexBaseURL
	}
	normalized := strings.TrimRight(raw, "/")
	switch {
	case strings.HasSuffix(normalized, "/codex/responses"):
		return normalized
	case strings.HasSuffix(normalized, "/codex"):
		return normalized + "/responses"
	default:
		return normalized + "/codex/responses"
	}
}

func doCodexRequest(ctx context.Context, model *ai.Model, opts *CodexOptions, accountID string, body []byte) (*http.Response, error) {
	req, err := newJSONRequest(ctx, codexURL(model.BaseURL), body)
	if err != nil {
		return nil, err
	}

	h := baseHeaders()
	h.Set("Authorization", "Bearer "+opts.APIKey)
	// Without the account header the backend cannot tell which subscription to
	// bill, and rejects the request.
	h.Set("chatgpt-account-id", accountID)
	h.Set("originator", "tau")
	h.Set("User-Agent", codexUserAgent())
	h.Set("OpenAI-Beta", "responses=experimental")

	if opts.SessionID != "" {
		h.Set("session-id", opts.SessionID)
		h.Set("x-client-request-id", opts.SessionID)
	}
	for k, v := range model.Headers {
		h.Set(k, v)
	}
	applyHeaderOverrides(h, opts.Headers)
	req.Header = h

	return httpClient(opts.TimeoutMs).Do(req)
}

func codexUserAgent() string {
	return fmt.Sprintf("tau (%s; %s)", runtime.GOOS, runtime.GOARCH)
}

// codexAccountID reads the ChatGPT account id out of the access token.
//
// It is a JWT and the id lives in a namespaced claim. There is no endpoint
// that returns it, so a token tau cannot parse is a login that cannot be used
// — which is worth saying plainly rather than failing later with a 401.
func codexAccountID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("codex: the access token is not a JWT; log in again")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("codex: the access token's claims are unreadable; log in again")
	}

	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("codex: the access token's claims are unreadable; log in again")
	}

	raw, ok := claims[codexAuthClaim]
	if !ok {
		return "", fmt.Errorf("codex: the access token names no ChatGPT account; log in again")
	}
	var auth struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil || auth.AccountID == "" {
		return "", fmt.Errorf("codex: the access token names no ChatGPT account; log in again")
	}
	return auth.AccountID, nil
}

// codexAccountIDClaim documentation lives on codexAccountID above.
