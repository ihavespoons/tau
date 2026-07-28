// Package anthropic implements the anthropic-messages wire API — the tau port
// of Pi's packages/ai/src/api/anthropic-messages.ts (snapshot v0.82.1),
// re-based from the @anthropic-ai/sdk onto net/http.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai"
)

// Stealth mode: mimic Claude Code's tool naming and identity exactly.
const claudeCodeVersion = "2.1.75"

const (
	fineGrainedToolStreamingBeta = "fine-grained-tool-streaming-2025-05-14"
	interleavedThinkingBeta      = "interleaved-thinking-2025-05-14"
	anthropicVersion             = "2023-06-01"
)

// Claude Code 2.x tool names (canonical casing).
var claudeCodeTools = []string{
	"Read", "Write", "Edit", "Bash", "Grep", "Glob", "AskUserQuestion",
	"EnterPlanMode", "ExitPlanMode", "KillShell", "NotebookEdit", "Skill",
	"Task", "TaskOutput", "TodoWrite", "WebFetch", "WebSearch",
}

var ccToolLookup = func() map[string]string {
	m := make(map[string]string, len(claudeCodeTools))
	for _, t := range claudeCodeTools {
		m[strings.ToLower(t)] = t
	}
	return m
}()

func toClaudeCodeName(name string) string {
	if canonical, ok := ccToolLookup[strings.ToLower(name)]; ok {
		return canonical
	}
	return name
}

func fromClaudeCodeName(name string, tools []ai.Tool) string {
	lower := strings.ToLower(name)
	for _, tool := range tools {
		if strings.ToLower(tool.Name) == lower {
			return tool.Name
		}
	}
	return name
}

// Options are the anthropic-messages provider options (Pi's AnthropicOptions).
type Options struct {
	ai.StreamOptions
	// ThinkingEnabled: nil omits thinking config entirely; false sends
	// thinking:{type:"disabled"} (when the model supports off); true enables.
	ThinkingEnabled *bool
	// ThinkingBudgetTokens for budget-based models; defaults to 1024.
	ThinkingBudgetTokens int
	// Effort for adaptive-thinking models: low|medium|high|xhigh|max.
	Effort string
	// ThinkingDisplay: summarized (default) or omitted.
	ThinkingDisplay string
	// InterleavedThinking requests the interleaved-thinking beta on
	// non-adaptive models. nil means true.
	InterleavedThinking *bool
	// ToolChoice: "auto"|"any"|"none" or map {"type":"tool","name":...}.
	ToolChoice any
}

// compat is the resolved anthropic-messages compat set (Pi's getAnthropicCompat).
type compat struct {
	supportsEagerToolInputStreaming bool
	supportsLongCacheRetention      bool
	sendSessionAffinityHeaders      bool
	supportsCacheControlOnTools     bool
	supportsTemperature             bool
	allowEmptySignature             bool
	supportsStrictTools             bool
	supportsToolReferences          bool
	forceAdaptiveThinking           bool
}

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

func resolveCompat(model *ai.Model) compat {
	var c *ai.CompatFlags
	if model.Compat != nil {
		c = model.Compat
	} else {
		c = &ai.CompatFlags{}
	}
	return compat{
		supportsEagerToolInputStreaming: boolOr(c.SupportsEagerToolInputStreaming, true),
		supportsLongCacheRetention:      boolOr(c.SupportsLongCacheRetention, true),
		sendSessionAffinityHeaders:      boolOr(c.SendSessionAffinityHeaders, false),
		supportsCacheControlOnTools:     boolOr(c.SupportsCacheControlOnTools, true),
		supportsTemperature:             boolOr(c.SupportsTemperature, true),
		allowEmptySignature:             boolOr(c.AllowEmptySignature, false),
		supportsStrictTools:             boolOr(c.SupportsStrictTools, false),
		supportsToolReferences:          boolOr(c.SupportsToolReferences, defaultSupportsToolReferences(model)),
		forceAdaptiveThinking:           boolOr(c.ForceAdaptiveThinking, false),
	}
}

// defaultSupportsToolReferences: first-party Anthropic models except Haiku and
// models that predate tool search (Claude 3.x, Opus/Sonnet 4.0, Opus 4.1).
func defaultSupportsToolReferences(model *ai.Model) bool {
	if model.Provider != "anthropic" || strings.Contains(model.ID, "haiku") {
		return false
	}
	rest, ok := cutModelFamily(model.ID)
	if !ok {
		return false
	}
	major, minor, ok := parseModelVersion(rest)
	if !ok {
		return false
	}
	return major > 4 || (major == 4 && minor >= 5)
}

// cutModelFamily strips a ^claude-(opus|sonnet|fable)- prefix.
func cutModelFamily(id string) (string, bool) {
	for _, family := range []string{"claude-opus-", "claude-sonnet-", "claude-fable-"} {
		if strings.HasPrefix(id, family) {
			return id[len(family):], true
		}
	}
	return "", false
}

// parseModelVersion parses "5", "4-5", "4-5-20250929" into (major, minor).
// A second segment of 8+ digits is a date, not a minor version.
func parseModelVersion(rest string) (major, minor int, ok bool) {
	parts := strings.Split(rest, "-")
	if len(parts) == 0 || parts[0] == "" {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[0], "%d", &major); err != nil {
		return 0, 0, false
	}
	if len(parts) > 1 && len(parts[1]) < 8 {
		if _, err := fmt.Sscanf(parts[1], "%d", &minor); err == nil {
			return major, minor, true
		}
	}
	return major, 0, true
}

func resolveCacheRetention(retention ai.CacheRetention, env map[string]string) ai.CacheRetention {
	if retention != "" {
		return retention
	}
	if v := env["TAU_CACHE_RETENTION"]; v == "long" {
		return ai.CacheLong
	}
	if v := env["PI_CACHE_RETENTION"]; v == "long" {
		return ai.CacheLong
	}
	return ai.CacheShort
}

// cacheControl returns the cache_control marker map, or nil when retention is none.
func cacheControlFor(model *ai.Model, retention ai.CacheRetention, env map[string]string) map[string]any {
	resolved := resolveCacheRetention(retention, env)
	if resolved == ai.CacheNone {
		return nil
	}
	cc := map[string]any{"type": "ephemeral"}
	if resolved == ai.CacheLong && resolveCompat(model).supportsLongCacheRetention {
		cc["ttl"] = "1h"
	}
	return cc
}

func isOAuthToken(apiKey string) bool { return strings.Contains(apiKey, "sk-ant-oat") }

func hasHeaderValue(headers map[string]*string, name string) bool {
	for k, v := range headers {
		if strings.EqualFold(k, name) && v != nil && strings.TrimSpace(*v) != "" {
			return true
		}
	}
	return false
}

func assertRequestAuth(provider, apiKey string, headers map[string]*string) error {
	if apiKey != "" {
		return nil
	}
	if hasHeaderValue(headers, "authorization") || hasHeaderValue(headers, "x-api-key") || hasHeaderValue(headers, "cf-aig-authorization") {
		return nil
	}
	return fmt.Errorf("No API key for provider: %s", provider) //nolint:staticcheck // Pi error-message parity
}

// Stream implements Pi's stream() for anthropic-messages.
func Stream(ctx context.Context, model *ai.Model, c ai.Context, opts *Options) *ai.MessageStream {
	stream := ai.NewMessageStream()
	if opts == nil {
		opts = &Options{}
	}
	go run(ctx, stream, model, c, opts)
	return stream
}

// StreamSimple implements Pi's streamSimple(): maps normalized reasoning
// levels onto thinking budgets or adaptive effort.
func StreamSimple(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	base := opts.StreamOptions
	base.MaxTokens = clampMaxTokensToContext(model, c, orDefault(opts.MaxTokens, model.MaxTokens))

	if err := assertRequestAuth(model.Provider, opts.APIKey, opts.Headers); err != nil {
		return failedStream(model, err)
	}

	if opts.Reasoning == "" {
		f := false
		return Stream(ctx, model, c, &Options{StreamOptions: base, ThinkingEnabled: &f})
	}

	if model.Compat != nil && boolOr(model.Compat.ForceAdaptiveThinking, false) {
		tr := true
		return Stream(ctx, model, c, &Options{
			StreamOptions:   base,
			ThinkingEnabled: &tr,
			Effort:          mapThinkingLevelToEffort(model, opts.Reasoning),
		})
	}

	maxTokens, thinkingBudget := adjustMaxTokensForThinking(opts.MaxTokens, model.MaxTokens, opts.Reasoning, opts.ThinkingBudgets)
	maxTokens = clampMaxTokensToContext(model, c, maxTokens)
	base.MaxTokens = maxTokens
	tr := true
	return Stream(ctx, model, c, &Options{
		StreamOptions:        base,
		ThinkingEnabled:      &tr,
		ThinkingBudgetTokens: minInt(thinkingBudget, maxInt(0, maxTokens-1024)),
	})
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// mapThinkingLevelToEffort ports Pi's mapThinkingLevelToEffort.
func mapThinkingLevelToEffort(model *ai.Model, level ai.ThinkingLevel) string {
	if mapped, ok := model.ThinkingLevelMap[ai.ModelThinkingLevel(level)]; ok && mapped != nil {
		return *mapped
	}
	switch level {
	case ai.ThinkingMinimal, ai.ThinkingLow:
		return "low"
	case ai.ThinkingMedium:
		return "medium"
	case ai.ThinkingHigh:
		return "high"
	default:
		return "high"
	}
}

func failedStream(model *ai.Model, err error) *ai.MessageStream {
	s := ai.NewMessageStream()
	out := newOutput(model)
	out.StopReason = ai.StopError
	out.ErrorMessage = err.Error()
	s.Push(ai.Event{Type: ai.EventError, Reason: ai.StopError, Error: out})
	return s
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

// liveBlock tracks one streaming content block; the materialized value is
// mirrored into output.Content[pos] after every mutation.
type liveBlock struct {
	apiIndex    int
	kind        string // text | thinking | toolCall
	text        string
	thinking    string
	signature   string
	redacted    bool
	toolID      string
	toolName    string
	partialJSON string
	args        map[string]any
}

func (b *liveBlock) materialize() ai.Content {
	switch b.kind {
	case "text":
		return ai.TextContent{Text: b.text}
	case "thinking":
		return ai.ThinkingContent{Thinking: b.thinking, ThinkingSignature: b.signature, Redacted: b.redacted}
	default:
		return ai.ToolCall{ID: b.toolID, Name: b.toolName, Arguments: b.args}
	}
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

	oauth := opts.APIKey != "" && model.Provider != "github-copilot" && isOAuthToken(opts.APIKey)

	params, err := buildParams(model, c, oauth, opts)
	if err != nil {
		fail(err)
		return
	}
	var payload any = params
	if opts.OnPayload != nil {
		next, perr := opts.OnPayload(payload, model)
		if perr != nil {
			fail(perr)
			return
		}
		if next != nil {
			payload = next
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fail(err)
		return
	}

	headers := buildHeaders(model, c, oauth, opts)

	resp, err := retryRequest(ctx, func() (*http.Response, error) {
		return doRequest(ctx, model.BaseURL, headers, body, opts.TimeoutMs)
	}, opts.MaxRetries, opts.MaxRetryDelayMs)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("Request was aborted") //nolint:staticcheck // Pi error-message parity
		}
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

	stream.Push(ai.Event{Type: ai.EventStart, Partial: output})

	if err := consumeSSE(ctx, stream, resp, model, c, oauth, output); err != nil {
		fail(err)
		return
	}

	if ctx.Err() != nil {
		fail(fmt.Errorf("Request was aborted")) //nolint:staticcheck // Pi error-message parity
		return
	}
	if output.StopReason == ai.StopPending {
		fail(fmt.Errorf("Anthropic stream ended without a stop reason")) //nolint:staticcheck // Pi error-message parity
		return
	}
	if output.StopReason == ai.StopAborted || output.StopReason == ai.StopError {
		msg := output.ErrorMessage
		if msg == "" {
			msg = "An unknown error occurred"
		}
		fail(fmt.Errorf("%s", msg))
		return
	}
	stream.Push(ai.Event{Type: ai.EventDone, Reason: output.StopReason, Message: output})
}

func doRequest(ctx context.Context, baseURL string, headers http.Header, body []byte, timeoutMs int) (*http.Response, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	client := http.DefaultClient
	if timeoutMs > 0 {
		// Response-headers timeout only; the body may stream far longer.
		client = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: time.Duration(timeoutMs) * time.Millisecond}}
	}
	return client.Do(req)
}

// buildHeaders ports createClient's header assembly (SDK defaults + Pi's).
func buildHeaders(model *ai.Model, c ai.Context, oauth bool, opts *Options) http.Header {
	betas := []string{}
	useFineGrained := len(c.Tools) > 0 && !resolveCompat(model).supportsEagerToolInputStreaming
	if useFineGrained {
		betas = append(betas, fineGrainedToolStreamingBeta)
	}
	interleaved := boolOr(opts.InterleavedThinking, true)
	if interleaved && !resolveCompat(model).forceAdaptiveThinking {
		betas = append(betas, interleavedThinkingBeta)
	}

	merged := map[string]string{
		"accept":       "application/json",
		"content-type": "application/json",
		"anthropic-dangerous-direct-browser-access": "true",
		"anthropic-version":                         anthropicVersion,
	}

	switch {
	case model.Provider == "github-copilot":
		if len(betas) > 0 {
			merged["anthropic-beta"] = strings.Join(betas, ",")
		}
		if opts.APIKey != "" {
			merged["authorization"] = "Bearer " + opts.APIKey
		}
	case oauth:
		merged["anthropic-beta"] = strings.Join(append([]string{"claude-code-20250219", "oauth-2025-04-20"}, betas...), ",")
		merged["user-agent"] = "claude-cli/" + claudeCodeVersion
		merged["x-app"] = "cli"
		merged["authorization"] = "Bearer " + opts.APIKey
	default:
		if len(betas) > 0 {
			merged["anthropic-beta"] = strings.Join(betas, ",")
		}
		retention := resolveCacheRetention(opts.CacheRetention, opts.Env)
		if retention != ai.CacheNone && opts.SessionID != "" && resolveCompat(model).sendSessionAffinityHeaders {
			merged["x-session-affinity"] = opts.SessionID
		}
		if opts.APIKey != "" {
			merged["x-api-key"] = opts.APIKey
		}
	}

	for k, v := range model.Headers {
		merged[strings.ToLower(k)] = v
	}
	for k, v := range opts.Headers {
		if v == nil {
			delete(merged, strings.ToLower(k))
		} else {
			merged[strings.ToLower(k)] = *v
		}
	}

	h := http.Header{}
	for k, v := range merged {
		h.Set(k, v)
	}
	return h
}

func normalizeToolCallID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
