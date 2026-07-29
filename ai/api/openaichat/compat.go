// Package openaichat implements the openai-completions wire API — the
// /v1/chat/completions shape.
//
// It is by far the most load-bearing wire in the matrix: roughly two dozen of
// Pi's providers speak it, from OpenAI-compatible gateways to Groq, xAI,
// DeepSeek, Together, Moonshot, and OpenRouter. They agree on the envelope and
// disagree on almost everything else, which is what the compat flags are for.
package openaichat

import (
	"strings"

	"github.com/ihavespoons/tau/ai"
)

// Thinking formats. Providers express "reason harder" a dozen different ways;
// each constant names one dialect (Pi's OpenAICompletionsCompat.thinkingFormat).
const (
	thinkingOpenAI      = "openai"             // reasoning_effort: "high"
	thinkingOpenRouter  = "openrouter"         // reasoning: { effort }
	thinkingDeepSeek    = "deepseek"           // thinking: { type } + reasoning_effort
	thinkingTogether    = "together"           // reasoning: { enabled } + reasoning_effort
	thinkingZai         = "zai"                // thinking: { type, clear_thinking }
	thinkingQwen        = "qwen"               // enable_thinking: bool
	thinkingQwenChatTpl = "qwen-chat-template" // chat_template_kwargs
	thinkingChatTpl     = "chat-template"      // configurable chat_template_kwargs
	thinkingString      = "string-thinking"    // thinking: "high"
	thinkingAntLing     = "ant-ling"           // reasoning: { effort }, only when mapped
)

// Max-tokens field names.
const (
	maxTokensCompletion = "max_completion_tokens"
	maxTokensLegacy     = "max_tokens"
)

// Session-affinity header dialects.
const (
	affinityOpenAI          = "openai"
	affinityOpenAINoSession = "openai-nosession"
	affinityOpenRouter      = "openrouter"
)

// compat is the fully resolved quirk set for one model: no pointers, no
// "unset". Everything downstream reads this rather than poking at the model.
type compat struct {
	SupportsStore                               bool
	SupportsDeveloperRole                       bool
	SupportsReasoningEffort                     bool
	SupportsUsageInStreaming                    bool
	MaxTokensField                              string
	RequiresToolResultName                      bool
	RequiresAssistantAfterToolResult            bool
	RequiresThinkingAsText                      bool
	RequiresReasoningContentOnAssistantMessages bool
	ThinkingFormat                              string
	ChatTemplateKwargs                          map[string]any
	OpenRouterRouting                           *ai.OpenRouterRouting
	VercelGatewayRouting                        *ai.VercelGatewayRouting
	ZaiToolStream                               bool
	SupportsOpenAIGrammarTools                  bool
	SupportsStrictMode                          bool
	CacheControlFormat                          string
	SendSessionAffinityHeaders                  bool
	DeferredToolsMode                           string
	SessionAffinityFormat                       string
	SupportsLongCacheRetention                  bool
}

// hostFacts are the provider/URL observations detectCompat branches on. They
// are computed once and named, because Pi's original expresses each of them
// two or three times inside larger boolean expressions and the duplication is
// where a porting error would hide.
type hostFacts struct {
	zai, together, moonshot, openRouter  bool
	cloudflareWorkers, cloudflareGateway bool
	nvidia, antLing, grok, deepSeek      bool
	cerebras, chutes, opencode           bool
}

func observe(model *ai.Model) hostFacts {
	p := string(model.Provider)
	url := model.BaseURL
	has := func(s string) bool { return strings.Contains(url, s) }

	return hostFacts{
		zai:               p == "zai" || p == "zai-coding-cn" || has("api.z.ai") || has("open.bigmodel.cn"),
		together:          p == "together" || has("api.together.ai") || has("api.together.xyz"),
		moonshot:          p == "moonshotai" || p == "moonshotai-cn" || has("api.moonshot."),
		openRouter:        p == "openrouter" || has("openrouter.ai"),
		cloudflareWorkers: p == "cloudflare-workers-ai" || has("api.cloudflare.com"),
		cloudflareGateway: p == "cloudflare-ai-gateway" || has("gateway.ai.cloudflare.com"),
		nvidia:            p == "nvidia" || has("integrate.api.nvidia.com"),
		antLing:           p == "ant-ling" || has("api.ant-ling.com"),
		grok:              p == "xai" || has("api.x.ai"),
		deepSeek:          p == "deepseek" || has("deepseek.com"),
		cerebras:          p == "cerebras" || has("cerebras.ai"),
		chutes:            has("chutes.ai"),
		opencode:          p == "opencode" || has("opencode.ai"),
	}
}

// nonStandard marks the hosts that diverge from OpenAI's own semantics enough
// that the optional request fields have to be withheld.
func (f hostFacts) nonStandard() bool {
	return f.nvidia || f.cerebras || f.grok || f.together || f.chutes ||
		f.deepSeek || f.zai || f.moonshot || f.opencode ||
		f.cloudflareWorkers || f.cloudflareGateway || f.antLing
}

// detectCompat derives defaults from the provider id and base URL, exactly as
// Pi does (openai-completions.ts:1386-1474).
func detectCompat(model *ai.Model) compat {
	f := observe(model)
	nonStandard := f.nonStandard()

	useLegacyMaxTokens := f.chutes || f.moonshot || f.cloudflareGateway ||
		f.together || f.nvidia || f.antLing || f.zai

	// OpenRouter passes the developer role through only for the upstreams that
	// understand it.
	openRouterDeveloperRole := f.openRouter &&
		(strings.HasPrefix(model.ID, "anthropic/") || strings.HasPrefix(model.ID, "openai/"))

	cacheControlFormat := ""
	if model.Provider == "openrouter" && strings.HasPrefix(model.ID, "anthropic/") {
		cacheControlFormat = "anthropic"
	}

	thinkingFormat := thinkingOpenAI
	switch {
	case f.deepSeek:
		thinkingFormat = thinkingDeepSeek
	case f.zai:
		thinkingFormat = thinkingZai
	case f.together:
		thinkingFormat = thinkingTogether
	case f.antLing:
		thinkingFormat = thinkingAntLing
	case f.openRouter:
		thinkingFormat = thinkingOpenRouter
	}

	affinity := affinityOpenAI
	if f.openRouter {
		affinity = affinityOpenRouter
	}

	maxTokensField := maxTokensCompletion
	if useLegacyMaxTokens {
		maxTokensField = maxTokensLegacy
	}

	return compat{
		SupportsStore:         !nonStandard,
		SupportsDeveloperRole: openRouterDeveloperRole || (!nonStandard && !f.openRouter),
		SupportsReasoningEffort: !f.grok && !f.zai && !f.moonshot && !f.together &&
			!f.cloudflareGateway && !f.nvidia && !f.antLing,
		SupportsUsageInStreaming:                    true,
		MaxTokensField:                              maxTokensField,
		RequiresReasoningContentOnAssistantMessages: f.deepSeek,
		ThinkingFormat:                              thinkingFormat,
		ChatTemplateKwargs:                          map[string]any{},
		SupportsStrictMode:                          !f.moonshot && !f.together && !f.cloudflareGateway && !f.nvidia,
		CacheControlFormat:                          cacheControlFormat,
		SessionAffinityFormat:                       affinity,
		SupportsLongCacheRetention: !f.together && !f.cloudflareWorkers &&
			!f.cloudflareGateway && !f.nvidia && !f.antLing,
	}
}

// resolveCompat layers the model's explicit overrides onto the detected
// defaults. A nil override field keeps the detected value.
func resolveCompat(model *ai.Model) compat {
	c := detectCompat(model)
	o := model.Compat
	if o == nil {
		return c
	}

	c.SupportsStore = boolOr(o.SupportsStore, c.SupportsStore)
	c.SupportsDeveloperRole = boolOr(o.SupportsDeveloperRole, c.SupportsDeveloperRole)
	c.SupportsReasoningEffort = boolOr(o.SupportsReasoningEffort, c.SupportsReasoningEffort)
	c.SupportsUsageInStreaming = boolOr(o.SupportsUsageInStreaming, c.SupportsUsageInStreaming)
	c.MaxTokensField = strOr(o.MaxTokensField, c.MaxTokensField)
	c.RequiresToolResultName = boolOr(o.RequiresToolResultName, c.RequiresToolResultName)
	c.RequiresAssistantAfterToolResult = boolOr(o.RequiresAssistantAfterToolResult, c.RequiresAssistantAfterToolResult)
	c.RequiresThinkingAsText = boolOr(o.RequiresThinkingAsText, c.RequiresThinkingAsText)
	c.RequiresReasoningContentOnAssistantMessages = boolOr(
		o.RequiresReasoningContentOnAssistantMessages, c.RequiresReasoningContentOnAssistantMessages)
	c.ThinkingFormat = strOr(o.ThinkingFormat, c.ThinkingFormat)
	c.ZaiToolStream = boolOr(o.ZaiToolStream, c.ZaiToolStream)
	c.SupportsStrictMode = boolOr(o.SupportsStrictMode, c.SupportsStrictMode)
	c.SupportsOpenAIGrammarTools = boolOr(o.SupportsOpenAIGrammarTools, c.SupportsOpenAIGrammarTools)
	c.CacheControlFormat = strOr(o.CacheControlFormat, c.CacheControlFormat)
	c.SendSessionAffinityHeaders = boolOr(o.SendSessionAffinityHeaders, c.SendSessionAffinityHeaders)
	c.DeferredToolsMode = strOr(o.DeferredToolsMode, c.DeferredToolsMode)
	c.SessionAffinityFormat = strOr(o.SessionAffinityFormat, c.SessionAffinityFormat)
	c.SupportsLongCacheRetention = boolOr(o.SupportsLongCacheRetention, c.SupportsLongCacheRetention)

	if o.ChatTemplateKwargs != nil {
		c.ChatTemplateKwargs = o.ChatTemplateKwargs
	}
	// Routing preferences are only ever sent when the model declares them, so
	// they carry the override through rather than a detected default.
	c.OpenRouterRouting = o.OpenRouterRouting
	c.VercelGatewayRouting = o.VercelGatewayRouting

	return c
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// resolveCacheRetention defaults to short-lived caching; only the environment
// opts into long retention (Pi's resolveCacheRetention). PI_ is accepted
// alongside TAU_ so a migrating user's environment keeps working.
func resolveCacheRetention(retention ai.CacheRetention, env map[string]string) ai.CacheRetention {
	if retention != "" {
		return retention
	}
	if env["TAU_CACHE_RETENTION"] == "long" || env["PI_CACHE_RETENTION"] == "long" {
		return ai.CacheLong
	}
	return ai.CacheShort
}
