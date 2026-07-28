package ai

// CompatFlags is the superset of Pi's per-API compat interfaces
// (OpenAICompletionsCompat, OpenAIResponsesCompat, AnthropicMessagesCompat,
// BedrockCompat). Go has no conditional types, so every flag lives here as a
// pointer whose nil value means "auto-detect from BaseURL" — each wire-API
// package owns its resolution defaults, ported verbatim from Pi.
type CompatFlags struct {
	// --- openai-completions ---
	SupportsStore                               *bool                       `json:"supportsStore,omitempty"`
	SupportsDeveloperRole                       *bool                       `json:"supportsDeveloperRole,omitempty"`
	SupportsReasoningEffort                     *bool                       `json:"supportsReasoningEffort,omitempty"`
	SupportsUsageInStreaming                    *bool                       `json:"supportsUsageInStreaming,omitempty"`
	MaxTokensField                              *string                     `json:"maxTokensField,omitempty"` // "max_completion_tokens" | "max_tokens"
	RequiresToolResultName                      *bool                       `json:"requiresToolResultName,omitempty"`
	RequiresAssistantAfterToolResult            *bool                       `json:"requiresAssistantAfterToolResult,omitempty"`
	RequiresThinkingAsText                      *bool                       `json:"requiresThinkingAsText,omitempty"`
	RequiresReasoningContentOnAssistantMessages *bool                       `json:"requiresReasoningContentOnAssistantMessages,omitempty"`
	ThinkingFormat                              *string                     `json:"thinkingFormat,omitempty"`
	ChatTemplateKwargs                          map[string]any              `json:"chatTemplateKwargs,omitempty"`
	OpenRouterRouting                           *OpenRouterRouting          `json:"openRouterRouting,omitempty"`
	VercelGatewayRouting                        *VercelGatewayRouting       `json:"vercelGatewayRouting,omitempty"`
	ZaiToolStream                               *bool                       `json:"zaiToolStream,omitempty"`
	SupportsOpenAIGrammarTools                  *bool                       `json:"supportsOpenAIGrammarTools,omitempty"`
	SupportsStrictMode                          *bool                       `json:"supportsStrictMode,omitempty"`
	CacheControlFormat                          *string                     `json:"cacheControlFormat,omitempty"` // "anthropic"
	SendSessionAffinityHeaders                  *bool                       `json:"sendSessionAffinityHeaders,omitempty"`
	DeferredToolsMode                           *string                     `json:"deferredToolsMode,omitempty"` // "kimi"
	SessionAffinityFormat                       *string                     `json:"sessionAffinityFormat,omitempty"`
	SupportsLongCacheRetention                  *bool                       `json:"supportsLongCacheRetention,omitempty"`

	// --- openai-responses family additions ---
	SupportsToolSearch              *bool `json:"supportsToolSearch,omitempty"`
	SupportsExplicitPromptCacheMode *bool `json:"supportsExplicitPromptCacheMode,omitempty"`

	// --- anthropic-messages additions ---
	SupportsEagerToolInputStreaming *bool `json:"supportsEagerToolInputStreaming,omitempty"`
	SupportsCacheControlOnTools     *bool `json:"supportsCacheControlOnTools,omitempty"`
	SupportsTemperature             *bool `json:"supportsTemperature,omitempty"`
	ForceAdaptiveThinking           *bool `json:"forceAdaptiveThinking,omitempty"`
	AllowEmptySignature             *bool `json:"allowEmptySignature,omitempty"`
	SupportsStrictTools             *bool `json:"supportsStrictTools,omitempty"`
	SupportsToolReferences          *bool `json:"supportsToolReferences,omitempty"`
}

// OpenRouterRouting mirrors Pi's OpenRouterRouting request field.
type OpenRouterRouting struct {
	AllowFallbacks         *bool          `json:"allow_fallbacks,omitempty"`
	RequireParameters      *bool          `json:"require_parameters,omitempty"`
	DataCollection         string         `json:"data_collection,omitempty"` // "deny" | "allow"
	ZDR                    *bool          `json:"zdr,omitempty"`
	EnforceDistillableText *bool          `json:"enforce_distillable_text,omitempty"`
	Order                  []string       `json:"order,omitempty"`
	Only                   []string       `json:"only,omitempty"`
	Ignore                 []string       `json:"ignore,omitempty"`
	Quantizations          []string       `json:"quantizations,omitempty"`
	Sort                   any            `json:"sort,omitempty"`      // string or {by, partition}
	MaxPrice               map[string]any `json:"max_price,omitempty"` // prompt/completion/image/audio/request → number|string
	PreferredMinThroughput any            `json:"preferred_min_throughput,omitempty"`
	PreferredMaxLatency    any            `json:"preferred_max_latency,omitempty"`
}

// VercelGatewayRouting mirrors Pi's Vercel AI Gateway routing preferences.
type VercelGatewayRouting struct {
	Only  []string `json:"only,omitempty"`
	Order []string `json:"order,omitempty"`
}
