package openaichat

import (
	"encoding/json"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// request is the chat-completions body. Extra carries the fields that are not
// OpenAI's — every provider's reasoning dialect ends up there.
type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`

	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
	Store               *bool          `json:"store,omitempty"`
	MaxTokens           int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	// Tools is a pointer so an EMPTY array can be sent distinctly from no
	// array at all. Anthropic behind a proxy requires the field to be present
	// once the transcript contains tool calls, and omitempty on a plain slice
	// would silently drop it.
	Tools      *[]tool `json:"tools,omitempty"`
	ToolChoice any     `json:"tool_choice,omitempty"`

	PromptCacheKey       string `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string `json:"prompt_cache_retention,omitempty"`

	Extra map[string]any `json:"-"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type tool struct {
	Type         string        `json:"type"`
	Function     *toolFunc     `json:"function,omitempty"`
	Custom       *toolCustom   `json:"custom,omitempty"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type toolFunc struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Parameters  *jsonschema.Schema `json:"parameters,omitempty"`
	Strict      *bool              `json:"strict,omitempty"`
}

type toolCustom struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Format      *customFormat `json:"format,omitempty"`
}

type customFormat struct {
	Type    string         `json:"type"`
	Grammar *grammarFormat `json:"grammar,omitempty"`
}

type grammarFormat struct {
	// Syntax is "lark" or "regex".
	Syntax     string `json:"syntax"`
	Definition string `json:"definition"`
}

// MarshalJSON folds Extra in, the same way message does.
func (r request) MarshalJSON() ([]byte, error) {
	type plain request
	base, err := json.Marshal(plain(r))
	if err != nil {
		return nil, err
	}
	if len(r.Extra) == 0 {
		return base, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range r.Extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		merged[k] = raw
	}
	return json.Marshal(merged)
}

func (r *request) setExtra(key string, value any) {
	if r.Extra == nil {
		r.Extra = map[string]any{}
	}
	r.Extra[key] = value
}

// buildRequest assembles the payload for one turn.
//
// It can fail, which no other wire's does: a tool may declare constrained
// sampling this provider cannot honour, and the tool asked for the constraint
// to be required. Sending the request anyway would let the model answer in a
// shape the tool has been told it will never receive.
// grammar maps a tool name to the argument carrying its raw output, for the
// tools declared with a grammar. It is resolved once by the caller because the
// streaming side needs the same answer, and two derivations of "which tools are
// grammar tools" would eventually disagree.
func buildRequest(model *ai.Model, c ai.Context, opts *Options, cm compat, grammar map[string]string) (request, error) {
	retention := resolveCacheRetention(opts.CacheRetention, opts.Env)

	messages, err := convertMessages(model, c, cm, grammar)
	if err != nil {
		return request{}, err
	}

	req := request{
		Model:    model.ID,
		Messages: messages,
		Stream:   true,
	}

	// The cache key is only meaningful where the provider actually keys a
	// cache off it: OpenAI proper always, everyone else only for long
	// retention they support.
	wantsCacheKey := (strings.Contains(model.BaseURL, "api.openai.com") && retention != ai.CacheNone) ||
		(retention == ai.CacheLong && cm.SupportsLongCacheRetention)
	if wantsCacheKey {
		req.PromptCacheKey = clampPromptCacheKey(opts.SessionID)
	}
	if retention == ai.CacheLong && cm.SupportsLongCacheRetention {
		req.PromptCacheRetention = "24h"
	}

	if cm.SupportsUsageInStreaming {
		req.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if cm.SupportsStore {
		no := false
		req.Store = &no
	}

	if opts.MaxTokens > 0 {
		if cm.MaxTokensField == maxTokensLegacy {
			req.MaxTokens = opts.MaxTokens
		} else {
			req.MaxCompletionTokens = opts.MaxTokens
		}
	}
	if opts.Temperature != nil {
		req.Temperature = opts.Temperature
	}

	if err := applyTools(&req, model, c, cm); err != nil {
		return request{}, err
	}
	// After applyTools, because the markers land on the tool list it produced.
	if cc := compatCacheControl(cm, retention); cc != nil {
		applyAnthropicCacheControl(req.Messages, req.Tools, cc)
	}
	applyThinking(&req, model, opts, cm)
	applyRouting(&req, model)

	return req, nil
}

// applyTools converts the active tool set, holding back any tool the
// transcript has deferred.
func applyTools(req *request, model *ai.Model, c ai.Context, cm compat) error {
	active := c.Tools
	if cm.DeferredToolsMode == "kimi" {
		deferred := deferredToolNames(c.Messages)
		if len(deferred) > 0 {
			active = nil
			for _, t := range c.Tools {
				if !deferred[t.Name] {
					active = append(active, t)
				}
			}
		}
	}

	if len(active) > 0 {
		converted, err := convertTools(active, cm)
		if err != nil {
			return err
		}
		req.Tools = &converted
		if cm.ZaiToolStream {
			req.setExtra("tool_stream", true)
		}
		return nil
	}

	// Anthropic behind a proxy rejects a conversation containing tool calls
	// unless the tools field is present, even empty.
	if hasToolHistory(c.Messages) {
		req.Tools = &[]tool{}
	}
	return nil
}

func convertTools(tools []ai.Tool, cm compat) ([]tool, error) {
	out := make([]tool, 0, len(tools))
	for _, t := range tools {
		// A grammar tool is declared as a different KIND of tool, not a
		// function with extra fields: it takes no JSON schema at all, because
		// the grammar is the schema.
		grammar, err := apishared.ResolveGrammarConstrainedSampling(t, cm.SupportsOpenAIGrammarTools)
		if err != nil {
			return nil, err
		}
		if grammar != nil {
			out = append(out, tool{
				Type: "custom",
				Custom: &toolCustom{
					Name:        t.Name,
					Description: t.Description,
					Format: &customFormat{
						Type:    "grammar",
						Grammar: &grammarFormat{Syntax: grammar.Format, Definition: grammar.Definition},
					},
				},
			})
			continue
		}

		strict, err := apishared.ResolveJSONSchemaStrictSampling(t, cm.SupportsStrictMode)
		if err != nil {
			return nil, err
		}
		converted := tool{
			Type: "function",
			Function: &toolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
		// `strict` is not universally understood, and providers that do not
		// know it reject the whole request rather than ignoring the field. Where
		// it IS understood the field is always sent, false by default, so a tool
		// that asked for schema enforcement is the only one that gets it.
		if cm.SupportsStrictMode {
			converted.Function.Strict = &strict
		}
		out = append(out, converted)
	}
	return out, nil
}

func hasToolHistory(msgs ai.MessageList) bool {
	for _, msg := range msgs {
		switch m := msg.(type) {
		case ai.ToolResultMessage:
			return true
		case ai.AssistantMessage:
			for _, block := range m.Content {
				if _, ok := block.(ai.ToolCall); ok {
					return true
				}
			}
		}
	}
	return false
}

func deferredToolNames(msgs ai.MessageList) map[string]bool {
	names := map[string]bool{}
	for _, msg := range msgs {
		if tr, ok := msg.(ai.ToolResultMessage); ok {
			for _, n := range tr.AddedToolNames {
				names[n] = true
			}
		}
	}
	return names
}

// mappedEffort translates tau's thinking level through the model's own map.
// A model may rename levels ("high" → "medium"), or mark one unsupported with
// a nil entry.
func mappedEffort(model *ai.Model, level ai.ThinkingLevel) (string, bool) {
	if model.ThinkingLevelMap == nil {
		return string(level), level != ""
	}
	mapped, present := model.ThinkingLevelMap[ai.ModelThinkingLevel(level)]
	if !present {
		return string(level), level != ""
	}
	if mapped == nil {
		return "", false
	}
	return *mapped, true
}

// offSupported reports whether the model allows thinking to be turned off. A
// nil "off" entry means it cannot.
func offSupported(model *ai.Model) bool {
	if model.ThinkingLevelMap == nil {
		return true
	}
	mapped, present := model.ThinkingLevelMap[ai.ThinkingOff]
	if !present {
		return true
	}
	return mapped != nil
}

// offValue is what to send for "off", when the model names it.
func offValue(model *ai.Model) (string, bool) {
	if model.ThinkingLevelMap == nil {
		return "", false
	}
	mapped, present := model.ThinkingLevelMap[ai.ThinkingOff]
	if !present || mapped == nil {
		return "", false
	}
	return *mapped, true
}

// applyThinking writes the reasoning request in whichever dialect this
// provider speaks. Ten of them, and they disagree on the field name, the
// shape, and whether "off" is even expressible.
func applyThinking(req *request, model *ai.Model, opts *Options, cm compat) {
	if !model.Reasoning {
		return
	}
	level := opts.Reasoning
	on := level != ""

	switch cm.ThinkingFormat {
	case thinkingZai:
		if on {
			req.setExtra("thinking", map[string]any{"type": "enabled", "clear_thinking": false})
		} else {
			req.setExtra("thinking", map[string]any{"type": "disabled"})
		}
		if on && cm.SupportsReasoningEffort {
			if effort, ok := mappedEffort(model, level); ok {
				req.setExtra("reasoning_effort", effort)
			}
		}

	case thinkingQwen:
		req.setExtra("enable_thinking", on)

	case thinkingQwenChatTpl:
		req.setExtra("chat_template_kwargs", map[string]any{
			"enable_thinking": on, "preserve_thinking": true,
		})

	case thinkingChatTpl:
		if kwargs := buildChatTemplateKwargs(model, opts, cm); len(kwargs) > 0 {
			req.setExtra("chat_template_kwargs", kwargs)
		}

	case thinkingDeepSeek:
		if on {
			req.setExtra("thinking", map[string]any{"type": "enabled"})
		} else if offSupported(model) {
			req.setExtra("thinking", map[string]any{"type": "disabled"})
		}
		if on && cm.SupportsReasoningEffort {
			if effort, ok := mappedEffort(model, level); ok {
				req.setExtra("reasoning_effort", effort)
			}
		}

	case thinkingOpenRouter:
		if on {
			if effort, ok := mappedEffort(model, level); ok {
				req.setExtra("reasoning", map[string]any{"effort": effort})
			}
		} else if offSupported(model) {
			effort := "none"
			if v, ok := offValue(model); ok {
				effort = v
			}
			req.setExtra("reasoning", map[string]any{"effort": effort})
		}

	case thinkingAntLing:
		// ant-ling only accepts the field when the level maps to something.
		if on {
			if effort, ok := mappedEffort(model, level); ok {
				req.setExtra("reasoning", map[string]any{"effort": effort})
			}
		}

	case thinkingTogether:
		req.setExtra("reasoning", map[string]any{"enabled": on})
		if on && cm.SupportsReasoningEffort {
			if effort, ok := mappedEffort(model, level); ok {
				req.setExtra("reasoning_effort", effort)
			}
		}

	case thinkingString:
		if on {
			if effort, ok := mappedEffort(model, level); ok {
				req.setExtra("thinking", effort)
			}
		} else if offSupported(model) {
			effort := "none"
			if v, ok := offValue(model); ok {
				effort = v
			}
			req.setExtra("thinking", effort)
		}

	default: // thinkingOpenAI
		if !cm.SupportsReasoningEffort {
			return
		}
		if on {
			if effort, ok := mappedEffort(model, level); ok {
				req.setExtra("reasoning_effort", effort)
			}
			return
		}
		// With reasoning off, only send a value the model actually names.
		if v, ok := offValue(model); ok {
			req.setExtra("reasoning_effort", v)
		}
	}
}

// buildChatTemplateKwargs resolves the configurable chat-template variables,
// substituting tau's thinking state for the `$var` placeholders.
func buildChatTemplateKwargs(model *ai.Model, opts *Options, cm compat) map[string]any {
	if len(cm.ChatTemplateKwargs) == 0 {
		return nil
	}
	out := make(map[string]any, len(cm.ChatTemplateKwargs))
	for key, value := range cm.ChatTemplateKwargs {
		resolved, ok := resolveChatTemplateValue(value, model, opts)
		if ok {
			out[key] = resolved
		}
	}
	return out
}

// resolveChatTemplateValue expands {"$var": "thinking.enabled"} and
// {"$var": "thinking.effort"}; anything else passes through literally.
func resolveChatTemplateValue(value any, model *ai.Model, opts *Options) (any, bool) {
	obj, isObj := value.(map[string]any)
	if !isObj {
		return value, true
	}
	ref, hasVar := obj["$var"].(string)
	if !hasVar {
		return value, true
	}

	switch ref {
	case "thinking.enabled":
		return opts.Reasoning != "", true
	case "thinking.effort":
		if opts.Reasoning == "" {
			return nil, false
		}
		effort, ok := mappedEffort(model, opts.Reasoning)
		return effort, ok
	default:
		return nil, false
	}
}

// applyRouting attaches provider-routing preferences. These are only ever sent
// when the model explicitly declares them.
func applyRouting(req *request, model *ai.Model) {
	if model.Compat == nil {
		return
	}
	if model.Compat.OpenRouterRouting != nil {
		req.setExtra("provider", model.Compat.OpenRouterRouting)
	}
	if r := model.Compat.VercelGatewayRouting; r != nil && (len(r.Only) > 0 || len(r.Order) > 0) {
		gateway := map[string]any{}
		if len(r.Only) > 0 {
			gateway["only"] = r.Only
		}
		if len(r.Order) > 0 {
			gateway["order"] = r.Order
		}
		req.setExtra("providerOptions", map[string]any{"gateway": gateway})
	}
}

// maxPromptCacheKey is OpenAI's documented limit for prompt_cache_key.
const maxPromptCacheKey = 64

// clampPromptCacheKey keeps the session id inside the provider's limit.
func clampPromptCacheKey(sessionID string) string {
	if len(sessionID) <= maxPromptCacheKey {
		return sessionID
	}
	return sessionID[:maxPromptCacheKey]
}
