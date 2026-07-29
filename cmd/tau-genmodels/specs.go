package main

import (
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/openaichat"
)

// specs is the provider table: everything tau's compiled catalog contains and
// where each entry comes from.
//
// Providers that need more than a filter and a field correction — the ones
// whose model list comes from their own endpoint rather than models.dev — are
// built separately and appended by buildExtra.
func specs() []providerSpec {
	return []providerSpec{
		{
			Source: "anthropic", ID: "anthropic", Name: "Anthropic",
			Api: ai.ApiAnthropicMessages, BaseURL: "https://api.anthropic.com",
		},
		{
			Source: "google", ID: "google", Name: "Google",
			Api: ai.ApiGoogleGenerativeAI, BaseURL: "https://generativelanguage.googleapis.com/v1beta",
		},
		{
			Source: "openai", ID: "openai", Name: "OpenAI",
			Api: ai.ApiOpenAIResponses, BaseURL: "https://api.openai.com/v1",
			// models.dev lists this alias, but the OpenAI API rejects it.
			Skip:  func(id string, _ modelsDevModel) bool { return id == "gpt-5.6" },
			Tweak: tweakOpenAI,
			Extra: openAIExtraModels,
		},
		{
			Source: "groq", ID: "groq", Name: "Groq",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.groq.com/openai/v1",
		},
		{
			Source: "cerebras", ID: "cerebras", Name: "Cerebras",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.cerebras.ai/v1",
		},
		{
			Source: "xai", ID: "xai", Name: "xAI",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.x.ai/v1",
			Skip:  func(id string, _ modelsDevModel) bool { return xaiExcluded[id] },
			Tweak: tweakXAI,
		},
		{
			Source: "huggingface", ID: "huggingface", Name: "Hugging Face",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://router.huggingface.co/v1",
			// The router forwards to whichever backend serves the model, and
			// not all of them understand the developer role.
			Tweak: withCompat(func(c *ai.CompatFlags) { c.SupportsDeveloperRole = boolptr(false) }),
		},
		{
			Source: "mistral", ID: "mistral", Name: "Mistral",
			Api: ai.ApiMistralConversations, BaseURL: "https://api.mistral.ai",
			Tweak: tweakMistral,
			Extra: mistralExtraModels,
		},
		{
			Source: "moonshotai", ID: "moonshotai", Name: "Moonshot AI",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.moonshot.ai/v1",
			Tweak: tweakMoonshot,
		},
		{
			Source: "moonshotai-cn", ID: "moonshotai-cn", Name: "Moonshot AI (CN)",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.moonshot.cn/v1",
			Tweak: tweakMoonshot,
		},
		{
			Source: "xiaomi", ID: "xiaomi", Name: "Xiaomi",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.xiaomimimo.com/v1",
			Tweak: withCompat(xiaomiCompat),
		},
		{
			Source: "cloudflare-workers-ai", ID: "cloudflare-workers-ai", Name: "Cloudflare Workers AI",
			Api: ai.ApiOpenAICompletions, BaseURL: cloudflareWorkersBaseURL,
			// Workers AI routes each request to an edge node; without the
			// affinity header a multi-turn conversation lands on a different
			// one each time.
			Tweak: withCompat(func(c *ai.CompatFlags) { c.SendSessionAffinityHeaders = boolptr(true) }),
		},
		{
			Source: "zai-coding-plan", ID: "zai", Name: "Z.ai",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.z.ai/api/coding/paas/v4",
			Tweak: tweakZai,
		},
		{
			Source: "zai-coding-plan", ID: "zai-coding-cn", Name: "Z.ai (CN)",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4",
			Tweak: tweakZai,
		},
		{
			Source: "together", SourceAlts: []string{"togetherai", "together-ai"},
			ID: "together", Name: "Together AI",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.together.ai/v1",
			Skip:  func(_ string, m modelsDevModel) bool { return m.Status == "deprecated" },
			Tweak: tweakTogether,
		},
		{
			Source: "fireworks-ai", ID: "fireworks", Name: "Fireworks",
			// GLM 5.2 is served through a router that speaks chat-completions
			// rather than the Anthropic-compatible endpoint the rest use.
			PerModelBaseURL: func(id string) string {
				if strings.Contains(id, "glm-5p2") {
					return "https://api.fireworks.ai/inference/v1"
				}
				return ""
			},
			// Fireworks' Anthropic-compatible endpoint; the wire appends
			// /v1/messages itself.
			Api: ai.ApiAnthropicMessages, BaseURL: "https://api.fireworks.ai/inference",
			Tweak: tweakFireworks,
		},
		{
			Source: "alibaba-token-plan", ID: "qwen-token-plan", Name: "Qwen Token Plan",
			Api:     ai.ApiOpenAICompletions,
			BaseURL: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1",
			Tweak:   withCompat(qwenTokenPlanCompat),
		},
		{
			Source: "alibaba-token-plan-cn", ID: "qwen-token-plan-cn", Name: "Qwen Token Plan (CN)",
			Api:     ai.ApiOpenAICompletions,
			BaseURL: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
			Tweak:   withCompat(qwenTokenPlanCompat),
		},
		{
			Source: "xiaomi-token-plan-cn", ID: "xiaomi-token-plan-cn", Name: "Xiaomi Token Plan (CN)",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://token-plan-cn.xiaomimimo.com/v1",
			Tweak: withCompat(xiaomiCompat),
		},
		{
			Source: "xiaomi-token-plan-ams", ID: "xiaomi-token-plan-ams", Name: "Xiaomi Token Plan (AMS)",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://token-plan-ams.xiaomimimo.com/v1",
			Tweak: withCompat(xiaomiCompat),
		},
		{
			Source: "xiaomi-token-plan-sgp", ID: "xiaomi-token-plan-sgp", Name: "Xiaomi Token Plan (SGP)",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://token-plan-sgp.xiaomimimo.com/v1",
			Tweak: withCompat(xiaomiCompat),
		},
		{
			Source: "minimax", ID: "minimax", Name: "MiniMax",
			// MiniMax's Anthropic-compatible API; the wire appends /v1/messages.
			Api: ai.ApiAnthropicMessages, BaseURL: "https://api.minimax.io/anthropic",
			Skip: skipUndirectMinimax,
		},
		{
			Source: "minimax-cn", ID: "minimax-cn", Name: "MiniMax (CN)",
			Api: ai.ApiAnthropicMessages, BaseURL: "https://api.minimaxi.com/anthropic",
			Skip: skipUndirectMinimax,
		},
		{
			Source: "kimi-for-coding", ID: "kimi-coding", Name: "Kimi For Coding",
			Api: ai.ApiAnthropicMessages, BaseURL: "https://api.kimi.com/coding",
			Rename: renameKimiAlias,
			Tweak:  tweakKimiCoding,
		},
		{
			Source: "nvidia", ID: "nvidia", Name: "NVIDIA NIM",
			Api: ai.ApiOpenAICompletions, BaseURL: nvidiaBaseURL,
			// NIM only serves what is deployed, under whatever id it deployed
			// it as, so models.dev ids are mapped onto the live ones and
			// anything not deployed is dropped rather than offered.
			Skip:   skipUndeployedNvidia,
			Rename: renameToNvidiaLiveID,
			Tweak:  tweakNvidia,
		},
		{
			ID: "deepseek", Name: "DeepSeek",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.deepseek.com",
			Extra: deepseekModels,
		},
		{
			ID: "ant-ling", Name: "Ant Ling",
			Api: ai.ApiOpenAICompletions, BaseURL: "https://api.ant-ling.com/v1",
			Extra: antLingModels,
		},
		{
			Source: "opencode", ID: "opencode", Name: "OpenCode Zen",
			BaseURL:      "https://opencode.ai/zen/v1",
			Skip:         skipOpencodeModel,
			PerModelWire: opencodeWire("https://opencode.ai/zen"),
			Tweak:        tweakOpencode("opencode", "https://opencode.ai/zen"),
		},
		{
			Source: "opencode-go", ID: "opencode-go", Name: "OpenCode Zen Go",
			BaseURL:      "https://opencode.ai/zen/go/v1",
			Skip:         skipOpencodeModel,
			PerModelWire: opencodeWire("https://opencode.ai/zen/go"),
			Tweak:        tweakOpencode("opencode-go", "https://opencode.ai/zen/go"),
		},
		{
			Source: "github-copilot", ID: "github-copilot", Name: "GitHub Copilot",
			BaseURL: copilotBaseURL,
			// Copilot bills the extended window at a second rate, and is one of
			// the few providers whose published tiers match what it charges.
			CostTiers:     true,
			Skip:          func(_ string, m modelsDevModel) bool { return m.Status == "deprecated" },
			PerModelWire:  copilotWire,
			Tweak:         tweakCopilot,
			DefaultLimits: modelLimits{ContextWindow: 128000, MaxTokens: 8192},
		},
		{
			Source: "cloudflare-ai-gateway", ID: "cloudflare-ai-gateway", Name: "Cloudflare AI Gateway",
			BaseURL:      cloudflareGatewayCompatURL,
			Skip:         skipUnroutableGatewayModel,
			Rename:       renameGatewayModel,
			PerModelWire: gatewayWire,
			Tweak:        tweakCloudflareGateway,
		},
		{
			Source: "google-vertex", ID: "google-vertex", Name: "Google Vertex AI",
			Api: ai.ApiGoogleVertex, BaseURL: "https://{location}-aiplatform.googleapis.com",
			// The Vertex catalog also carries Claude, OpenAI, and other managed
			// models that do not use the Gemini streaming path.
			Skip:  skipNonGeminiVertex,
			Tweak: tweakVertex,
		},
		{
			Source: "amazon-bedrock", ID: "amazon-bedrock", Name: "Amazon Bedrock",
			Api:             ai.ApiBedrockConverse,
			BaseURL:         bedrockUSBaseURL,
			Skip:            skipUnusableBedrockModel,
			PerModelBaseURL: bedrockBaseURL,
			Tweak:           strictToolsFromStructuredOutput,
		},
		{
			ID: "openai-codex", Name: "OpenAI Codex",
			Api: ai.ApiOpenAICodexResponses, BaseURL: codexBaseURL,
			Extra: codexModels,
		},
	}
}

// cloudflareWorkersBaseURL carries a placeholder the provider substitutes at
// request time: the account id is part of the path, not a header.
const cloudflareWorkersBaseURL = "https://api.cloudflare.com/client/v4/accounts/{CLOUDFLARE_ACCOUNT_ID}/ai/v1"

// applyReasoningMetadata layers models.dev's verified effort values onto a
// finished model.
//
// It runs last and conditionally, because a thinking-level map is only
// meaningful where the wire API takes an effort string directly. Sending one
// to a provider that expresses thinking as a token budget or a boolean would
// name a level the provider has never heard of.
func applyReasoningMetadata(m *ai.Model, idx *reasoningIndex) {
	opts := idx.get(string(m.Provider), m.ID)
	if len(opts) == 0 || !supportsDirectReasoningEffort(m) {
		return
	}
	mergeThinkingLevelMap(m, effortThinkingLevelMap(opts))
}

// supportsDirectReasoningEffort reports whether the model's wire API takes a
// thinking level as a plain effort value.
func supportsDirectReasoningEffort(m *ai.Model) bool {
	switch m.Api {
	case ai.ApiAnthropicMessages:
		// Only the adaptive-thinking models take an effort string; the rest
		// take a token budget.
		return m.Compat != nil && m.Compat.ForceAdaptiveThinking != nil && *m.Compat.ForceAdaptiveThinking
	case ai.ApiOpenAIResponses, ai.ApiAzureOpenAIResponses, ai.ApiOpenAICodexResponses:
		return true
	case ai.ApiOpenAICompletions:
		// Whether chat-completions takes one depends on the host, which is
		// what compat detection decides.
		return openaichat.SupportsDirectReasoningEffort(m)
	default:
		return false
	}
}

// goIdent turns a provider id into the exported Go identifier its catalog is
// bound to: "cloudflare-workers-ai" becomes CloudflareWorkersAIModels.
func goIdent(providerID string) string {
	var b strings.Builder
	for _, part := range strings.Split(providerID, "-") {
		switch part {
		case "ai", "api", "cn", "go":
			b.WriteString(strings.ToUpper(part))
		default:
			if part == "" {
				continue
			}
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String() + "Models"
}
