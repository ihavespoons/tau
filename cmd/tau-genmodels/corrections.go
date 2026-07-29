package main

import "github.com/ihavespoons/tau/ai"

// The tables here are hand-verified facts about specific models that models.dev
// either does not record or records differently from how the provider behaves.
// Each one exists because a request failed, or would have.

// openAIToolSearch lists the models that accept OpenAI's tool-search parameter.
var openAIToolSearch = map[string]bool{
	"gpt-5.4": true, "gpt-5.4-mini": true, "gpt-5.4-pro": true, "gpt-5.5": true,
	"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
}

// openAINoneReasoning lists the models that can be asked not to reason at all.
// For the rest, "off" is unexpressible rather than merely unset.
var openAINoneReasoning = map[string]bool{
	"gpt-5.1": true, "gpt-5.2": true, "gpt-5.3-codex": true, "gpt-5.4": true,
	"gpt-5.4-mini": true, "gpt-5.4-nano": true, "gpt-5.5": true,
	"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
}

// openAIShortContextCapped models are held at the short-context window by
// default. The larger window is available but priced differently, so opting
// into it is left to the user through a model override rather than being the
// silent default.
var openAIShortContextCapped = map[string]bool{
	"gpt-5.4": true, "gpt-5.5": true,
	"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
}

// openAILongContextPricing models charge a higher rate past the threshold.
var openAILongContextPricing = map[string]bool{
	"gpt-5.4": true, "gpt-5.4-pro": true, "gpt-5.5": true, "gpt-5.5-pro": true,
	"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true,
}

// openAILongContextThreshold is where the second pricing tier begins.
const openAILongContextThreshold = 272000

// withLongContextPricing adds the tier that applies past the threshold: double
// input and cache rates, and half again on output.
func withLongContextPricing(c ai.ModelCost) ai.ModelCost {
	c.Tiers = []ai.ModelCostTier{{
		InputTokensAbove: openAILongContextThreshold,
		ModelCostRates: ai.ModelCostRates{
			Input:      c.Input * 2,
			Output:     c.Output * 1.5,
			CacheRead:  c.CacheRead * 2,
			CacheWrite: c.CacheWrite * 2,
		},
	}}
	return c
}

func tweakOpenAI(id string, _ modelsDevModel, out *ai.Model) {
	if openAIShortContextCapped[id] {
		out.ContextWindow = openAILongContextThreshold
		out.MaxTokens = 128000
	}
	if openAILongContextPricing[id] {
		out.Cost = withLongContextPricing(out.Cost)
	}
	// models.dev reports gpt-5-pro's output limit as a copy of its input
	// sub-limit; the real ceiling is lower.
	if id == "gpt-5-pro" {
		out.MaxTokens = 128000
	}
	if openAIToolSearch[id] {
		mergeCompat(out, &ai.CompatFlags{SupportsToolSearch: boolptr(true)})
	}
}

// xaiExcluded lists the models tau does not offer: superseded releases and the
// split reasoning/non-reasoning variants that the unified model replaced.
var xaiExcluded = map[string]bool{
	"grok-3": true, "grok-3-fast": true, "grok-code-fast-1": true,
	"grok-4.20-0309-non-reasoning": true, "grok-4.20-0309-reasoning": true,
}

// xaiResponsesModel is the one xAI model served over the responses wire rather
// than chat-completions.
const xaiResponsesModel = "grok-4.5"

func tweakXAI(id string, _ modelsDevModel, out *ai.Model) {
	if id != xaiResponsesModel {
		return
	}
	out.Api = ai.ApiOpenAIResponses
	mergeCompat(out, &ai.CompatFlags{SupportsLongCacheRetention: boolptr(false)})
}

// Moonshot's endpoint is OpenAI-shaped but behaves like DeepSeek's: it speaks
// the deepseek thinking dialect, takes the legacy token field, and rejects
// strict tools, the developer role, and store.
func tweakMoonshot(id string, _ modelsDevModel, out *ai.Model) {
	mergeCompat(out, &ai.CompatFlags{
		SupportsStore:           boolptr(false),
		SupportsDeveloperRole:   boolptr(false),
		SupportsReasoningEffort: boolptr(false),
		MaxTokensField:          strptr("max_tokens"),
		SupportsStrictMode:      boolptr(false),
		ThinkingFormat:          strptr("deepseek"),
	})
	if id != "kimi-k3" {
		return
	}

	// K3 is the exception on every count: it reasons unconditionally, takes
	// OpenAI's effort field, replays reasoning like DeepSeek, and delivers
	// tools mid-conversation through Kimi's deferred-tools protocol.
	out.Reasoning = true
	mergeCompat(out, &ai.CompatFlags{
		RequiresReasoningContentOnAssistantMessages: boolptr(true),
		DeferredToolsMode:       strptr("kimi"),
		ThinkingFormat:          strptr("openai"),
		SupportsReasoningEffort: boolptr(true),
	})
	// models.dev reports no price for K3; the Moonshot API rates apply.
	if out.Cost.Input == 0 {
		out.Cost.Input = 3
	}
	if out.Cost.Output == 0 {
		out.Cost.Output = 15
	}
	if out.Cost.CacheRead == 0 {
		out.Cost.CacheRead = 0.3
	}
}

// openAIExtraModels are models the OpenAI API serves but models.dev does not
// describe.
var openAIExtraModels = []ai.Model{{
	ID: "gpt-5-chat-latest", Name: "GPT-5 Chat Latest",
	Api: ai.ApiOpenAIResponses, Provider: "openai", BaseURL: "https://api.openai.com/v1",
	Input: []string{"text", "image"},
	Cost: ai.ModelCost{ModelCostRates: ai.ModelCostRates{
		Input: 1.25, Output: 10, CacheRead: 0.125,
	}},
	ContextWindow: 128000, MaxTokens: 16384,
}}

// Mistral prices cache reads at a tenth of input but does not publish the rate
// per model. Leaving it at zero would make a cached turn look free and put the
// session's cost estimate out by however much it actually cached.
func tweakMistral(_ string, m modelsDevModel, out *ai.Model) {
	if m.Cost.CacheRead != nil || m.Cost.Input == nil {
		return
	}
	out.Cost.CacheRead = roundCost(*m.Cost.Input * 0.1)
}

// mistralExtraModels covers what models.dev has not caught up with. The guard
// is in the spec: if models.dev starts listing it, the derived entry wins and
// this one is dropped rather than duplicated.
var mistralExtraModels = []ai.Model{{
	ID: "mistral-medium-3.5", Name: "Mistral Medium 3.5",
	Api: ai.ApiMistralConversations, Provider: "mistral", BaseURL: "https://api.mistral.ai",
	Reasoning: true,
	Input:     []string{"text", "image"},
	Cost: ai.ModelCost{ModelCostRates: ai.ModelCostRates{
		Input: 1.5, Output: 7.5,
	}},
	ContextWindow: 262144, MaxTokens: 262144,
}}
