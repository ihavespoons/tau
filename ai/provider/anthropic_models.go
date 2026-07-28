package provider

import "github.com/ihavespoons/tau/ai"

// Hand-written Anthropic model data — P1 stopgap until cmd/tau-genmodels
// (P5) generates catalogs from models.dev. Pricing is $/M tokens; cache
// rates follow Anthropic convention (read = 0.1x input, 5m write = 1.25x).

func strptr(s string) *string { return &s }

// adaptiveThinkingMap maps pi thinking levels to Anthropic effort strings for
// adaptive-thinking models (Claude 4.6+).
func adaptiveThinkingMap(offSupported bool) ai.ThinkingLevelMap {
	m := ai.ThinkingLevelMap{
		"minimal": strptr("low"),
		"low":     strptr("low"),
		"medium":  strptr("medium"),
		"high":    strptr("high"),
		"xhigh":   strptr("xhigh"),
		"max":     strptr("max"),
	}
	if !offSupported {
		m["off"] = nil
	}
	return m
}

const anthropicBaseURL = "https://api.anthropic.com"

func anthropicModel(id, name string, in, out, cacheRead, cacheWrite float64, ctx, maxTok int, thinking ai.ThinkingLevelMap, compat *ai.CompatFlags) ai.Model {
	return ai.Model{
		ID: id, Name: name, Api: ai.ApiAnthropicMessages, Provider: "anthropic",
		BaseURL: anthropicBaseURL, Reasoning: thinking != nil, ThinkingLevelMap: thinking,
		Input: []string{"text", "image"},
		Cost: ai.ModelCost{ModelCostRates: ai.ModelCostRates{
			Input: in, Output: out, CacheRead: cacheRead, CacheWrite: cacheWrite,
		}},
		ContextWindow: ctx, MaxTokens: maxTok, Compat: compat,
	}
}

func boolptr(b bool) *bool { return &b }

// AnthropicModels returns the built-in Anthropic catalog.
func AnthropicModels() []ai.Model {
	adaptive := &ai.CompatFlags{ForceAdaptiveThinking: boolptr(true)}
	adaptiveNoTemp := &ai.CompatFlags{ForceAdaptiveThinking: boolptr(true), SupportsTemperature: boolptr(false)}
	return []ai.Model{
		// Fable 5: thinking always on ("off" unsupported), sampling params rejected.
		anthropicModel("claude-fable-5", "Claude Fable 5", 10, 50, 1, 12.5, 1_000_000, 128_000,
			adaptiveThinkingMap(false), adaptiveNoTemp),
		anthropicModel("claude-opus-5", "Claude Opus 5", 5, 25, 0.5, 6.25, 1_000_000, 128_000,
			adaptiveThinkingMap(true), adaptiveNoTemp),
		anthropicModel("claude-opus-4-8", "Claude Opus 4.8", 5, 25, 0.5, 6.25, 1_000_000, 128_000,
			adaptiveThinkingMap(true), adaptiveNoTemp),
		anthropicModel("claude-opus-4-7", "Claude Opus 4.7", 5, 25, 0.5, 6.25, 1_000_000, 128_000,
			adaptiveThinkingMap(true), adaptiveNoTemp),
		anthropicModel("claude-opus-4-6", "Claude Opus 4.6", 5, 25, 0.5, 6.25, 1_000_000, 128_000,
			adaptiveThinkingMap(true), adaptive),
		anthropicModel("claude-sonnet-5", "Claude Sonnet 5", 3, 15, 0.3, 3.75, 1_000_000, 128_000,
			adaptiveThinkingMap(true), adaptiveNoTemp),
		anthropicModel("claude-sonnet-4-6", "Claude Sonnet 4.6", 3, 15, 0.3, 3.75, 1_000_000, 128_000,
			adaptiveThinkingMap(true), adaptive),
		anthropicModel("claude-sonnet-4-5", "Claude Sonnet 4.5", 3, 15, 0.3, 3.75, 200_000, 64_000,
			ai.ThinkingLevelMap{}, nil),
		// Haiku 4.5: budget-token thinking, no xhigh/max.
		anthropicModel("claude-haiku-4-5", "Claude Haiku 4.5", 1, 5, 0.1, 1.25, 200_000, 64_000,
			ai.ThinkingLevelMap{}, nil),
	}
}
