package main

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/ai"
)

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

// openRouterKimiK3 are the ids OpenRouter serves Kimi K3 under. Its output
// limit is reported wrong or not at all, so it is pinned.
var openRouterKimiK3 = map[string]bool{
	"moonshotai/kimi-k3": true, "~moonshotai/kimi-latest": true,
}

const kimiK3MaxTokens = 131072

// tweakOpenRouter pins the handful of OpenRouter entries whose upstream
// metadata is wrong or still moving. Each is a fact checked against the real
// endpoint; without them a request either over-runs a limit the aggregator
// misreports, or the session's cost estimate is wrong.
func tweakOpenRouter(m *ai.Model) {
	if openRouterKimiK3[m.ID] {
		m.MaxTokens = kimiK3MaxTokens
	}
	switch {
	case m.ID == "moonshotai/kimi-k2.5":
		m.Cost.Input, m.Cost.Output, m.Cost.CacheRead = 0.41, 2.06, 0.07
		m.MaxTokens = 4096
	case strings.HasPrefix(m.ID, "moonshotai/kimi-k2.6"):
		mergeCompat(m, &ai.CompatFlags{
			SupportsDeveloperRole:                       boolptr(false),
			RequiresReasoningContentOnAssistantMessages: boolptr(true),
		})
	case m.ID == "z-ai/glm-5":
		m.Cost.Input, m.Cost.Output, m.Cost.CacheRead = 0.6, 1.9, 0.119
	}

	// OpenRouter's "~" prefix marks a floor-priced routing variant of the same
	// upstream model, so ~anthropic/… is still Anthropic and still caches.
	// Recording the flag here is what makes those variants cache at all: the
	// wire's own detection matches the unprefixed form only.
	if anthropicViaOpenRouter.MatchString(m.ID) {
		mergeCompat(m, &ai.CompatFlags{CacheControlFormat: strptr("anthropic")})
	}
}

var anthropicViaOpenRouter = regexp.MustCompile(`^~?anthropic/`)

// tweakVercelGateway applies the gateway's one pinned limit.
func tweakVercelGateway(m *ai.Model) {
	if m.ID == "moonshotai/kimi-k3" {
		m.MaxTokens = kimiK3MaxTokens
	}
}

// openRouterExtraModels covers what the aggregator's own catalog omits.
//
// Fusion is a router alias rather than a model: its metadata does not
// advertise tool support, but the alias resolves to a concrete model that can
// call tools, so filtering on the advertised parameters would hide a usable
// entry.
// Neither carries a rate: both route to whichever model they pick and bill for
// that one, so any figure recorded here would be a guess presented as a fact.
var openRouterExtraModels = []ai.Model{
	{
		ID: "auto", Name: "Auto",
		Api: ai.ApiOpenAICompletions, Provider: openRouterProviderID, BaseURL: openRouterBaseURL,
		Reasoning: true,
		Input:     []string{"text", "image"},
		Cost:      ai.ModelCost{},
		// The window is the largest any routed model offers.
		ContextWindow: 2000000, MaxTokens: 30000,
	},
	{
		ID: "openrouter/fusion", Name: "OpenRouter: Fusion",
		Api: ai.ApiOpenAICompletions, Provider: openRouterProviderID, BaseURL: openRouterBaseURL,
		Reasoning:     true,
		Input:         []string{"text"},
		Cost:          ai.ModelCost{},
		ContextWindow: 1000000, MaxTokens: 30000,
	},
}

// deepseekV4ThinkingLevels is DeepSeek V4's own vocabulary: it reasons at two
// settings and cannot be asked for a lower one.
func deepseekV4ThinkingLevels(providerID ai.ProviderId) ai.ThinkingLevelMap {
	m := ai.ThinkingLevelMap{
		"minimal": nil, "low": nil, "medium": nil,
		"high": strptr("high"), "max": strptr("max"),
	}
	if providerID == openRouterProviderID {
		// OpenRouter maps its own top tier onto the model's, and does not
		// expose the model's "max".
		m["xhigh"] = strptr("xhigh")
		m["max"] = nil
	}
	return m
}

// applyDeepSeekV4Compat gives a DeepSeek V4 model the replay rule its family
// needs, wherever it is served from.
//
// The thinking format travels with it only when tau talks to DeepSeek
// directly. An aggregator that already normalises reasoning — OpenRouter and
// opencode both do — would be handed a dialect it does not speak, so those two
// keep their own format and take just the replay requirement.
func applyDeepSeekV4Compat(m *ai.Model) {
	mergeCompat(m, &ai.CompatFlags{
		RequiresReasoningContentOnAssistantMessages: boolptr(true),
	})
	if m.Provider == openRouterProviderID || m.Provider == "opencode" {
		return
	}
	mergeCompat(m, &ai.CompatFlags{ThinkingFormat: strptr("deepseek")})
}

// xiaomiCompat: MiMo speaks DeepSeek's reasoning dialect, including its
// insistence on the field being present on replayed assistant turns.
func xiaomiCompat(c *ai.CompatFlags) {
	c.RequiresReasoningContentOnAssistantMessages = boolptr(true)
	c.ThinkingFormat = strptr("deepseek")
}

// qwenTokenPlanCompat: the Model Studio token-plan endpoints take Qwen's
// enable_thinking toggle and reject the developer role and store.
func qwenTokenPlanCompat(c *ai.CompatFlags) {
	c.ThinkingFormat = strptr("qwen")
	c.SupportsDeveloperRole = boolptr(false)
	c.SupportsStore = boolptr(false)
}

// zaiToolStreamUnsupported are the GLM releases that predate tool streaming.
var zaiToolStreamUnsupported = map[string]bool{
	"glm-4.5": true, "glm-4.5-air": true, "glm-4.5-flash": true, "glm-4.5v": true,
}

// zaiGLM52ThinkingLevels: GLM-5.2 collapses the middle of the range onto one
// setting rather than pretending to distinguish three.
var zaiGLM52ThinkingLevels = ai.ThinkingLevelMap{
	"minimal": nil, "low": strptr("high"), "medium": strptr("high"),
	"high": strptr("high"), "max": strptr("max"),
}

func tweakZai(id string, _ modelsDevModel, out *ai.Model) {
	mergeCompat(out, &ai.CompatFlags{
		SupportsDeveloperRole: boolptr(false),
		ThinkingFormat:        strptr("zai"),
	})
	if !zaiToolStreamUnsupported[id] {
		mergeCompat(out, &ai.CompatFlags{ZaiToolStream: boolptr(true)})
	}
	if id == "glm-5.2" {
		mergeCompat(out, &ai.CompatFlags{SupportsReasoningEffort: boolptr(true)})
		out.ThinkingLevelMap = ai.ThinkingLevelMap{}
		mergeThinkingLevelMap(out, zaiGLM52ThinkingLevels)
	}
}

// Together classifies its models by how each expresses thinking, and the four
// groups need genuinely different requests. A model in the wrong group either
// ignores the thinking setting or rejects the request.
var (
	togetherReasoningOnly = map[string]bool{
		"deepseek-ai/DeepSeek-R1": true, "MiniMaxAI/MiniMax-M2.7": true,
	}
	togetherReasoningEffort = map[string]bool{
		"openai/gpt-oss-20b": true, "openai/gpt-oss-120b": true,
	}
	togetherToggleReasoningEffort = map[string]bool{"deepseek-ai/DeepSeek-V4-Pro": true}
)

// togetherBaseCompat is what every Together model needs regardless of group.
func togetherBaseCompat(c *ai.CompatFlags) {
	c.SupportsStore = boolptr(false)
	c.SupportsDeveloperRole = boolptr(false)
	c.SupportsReasoningEffort = boolptr(false)
	c.MaxTokensField = strptr("max_tokens")
	c.SupportsStrictMode = boolptr(false)
	c.SupportsLongCacheRetention = boolptr(false)
}

func tweakTogether(id string, _ modelsDevModel, out *ai.Model) {
	flags := &ai.CompatFlags{}
	togetherBaseCompat(flags)

	switch {
	case !out.Reasoning:
		// Nothing further: a non-reasoning model has no thinking dialect.
	case togetherReasoningEffort[id]:
		flags.SupportsReasoningEffort = boolptr(true)
		flags.ThinkingFormat = strptr("openai")
		out.ThinkingLevelMap = ai.ThinkingLevelMap{ai.ThinkingOff: nil, "minimal": nil}
	case togetherToggleReasoningEffort[id]:
		flags.ThinkingFormat = strptr("together")
		flags.SupportsReasoningEffort = boolptr(true)
		out.ThinkingLevelMap = ai.ThinkingLevelMap{
			"minimal": nil, "low": nil, "medium": nil,
			"high": strptr("high"), "xhigh": nil,
		}
	case togetherReasoningOnly[id]:
		// Reasons unconditionally and takes no control at all.
		out.ThinkingLevelMap = ai.ThinkingLevelMap{
			ai.ThinkingOff: nil, "minimal": nil, "low": nil, "medium": nil,
		}
	default:
		flags.ThinkingFormat = strptr("together")
		out.ThinkingLevelMap = ai.ThinkingLevelMap{"minimal": nil, "low": nil, "medium": nil}
	}

	mergeCompat(out, flags)
}

// tweakFireworks splits the provider in two.
//
// Most of its catalog is served over an Anthropic-compatible endpoint where
// caching is automatic prefix matching plus replica affinity — so the
// marker-based controls are unavailable and the affinity header is what makes
// caching work at all. GLM 5.2 is different: it comes through a router that
// speaks chat-completions, and its compat block is replaced rather than
// extended, because none of the Anthropic-side flags mean anything there.
func tweakFireworks(id string, _ modelsDevModel, out *ai.Model) {
	if strings.Contains(id, "glm-5p2") {
		out.Api = ai.ApiOpenAICompletions
		out.Compat = &ai.CompatFlags{
			SupportsStore:         boolptr(false),
			SupportsDeveloperRole: boolptr(false),
		}
		return
	}
	mergeCompat(out, &ai.CompatFlags{
		SendSessionAffinityHeaders:      boolptr(true),
		SupportsEagerToolInputStreaming: boolptr(false),
		SupportsCacheControlOnTools:     boolptr(false),
		SupportsLongCacheRetention:      boolptr(false),
	})
}

// minimaxDirect are the only MiniMax models the direct endpoints serve.
// models.dev lists the whole family, but the rest are reachable only through
// aggregators, so offering them here would produce a 404 on first use.
var minimaxDirect = map[string]bool{
	"MiniMax-M2.7": true, "MiniMax-M2.7-highspeed": true, "MiniMax-M3": true,
}

func skipUndirectMinimax(id string, _ modelsDevModel) bool { return !minimaxDirect[id] }

// kimiAliases are versioned names models.dev exposes for the one model the
// Kimi coding plan actually serves.
var kimiAliases = map[string]bool{"k2p5": true, "k2p6": true, "k2p7": true}

const kimiCanonical = "kimi-for-coding"

// renameKimiAlias folds a versioned alias onto the canonical id.
func renameKimiAlias(id string) (string, string) {
	if kimiAliases[id] {
		return kimiCanonical, "Kimi For Coding"
	}
	return id, ""
}

// kimiImpliedCosts: the coding plan is subscription-backed, so models.dev
// reports zero. The equivalent Moonshot API rates are used instead, which
// makes a session's cost line an estimate of what the usage is worth rather
// than a flat and useless zero.
var kimiImpliedCosts = map[string]ai.ModelCostRates{
	"k3":                        {Input: 3, Output: 15, CacheRead: 0.3},
	"kimi-for-coding":           {Input: 0.95, Output: 4, CacheRead: 0.19},
	"kimi-for-coding-highspeed": {Input: 1.9, Output: 8, CacheRead: 0.38},
	"kimi-k2-thinking":          {Input: 0.6, Output: 2.5, CacheRead: 0.15},
}

func tweakKimiCoding(id string, _ modelsDevModel, out *ai.Model) {
	out.Headers = map[string]string{"User-Agent": "KimiCLI/1.5"}

	flags := &ai.CompatFlags{ForceAdaptiveThinking: boolptr(true)}
	if out.ID == "k3" || out.ID == kimiCanonical {
		// These two return thinking blocks with no signature, which the
		// Anthropic wire otherwise rejects on replay.
		flags.AllowEmptySignature = boolptr(true)
	}
	mergeCompat(out, flags)

	if out.ID == "k3" {
		out.Reasoning = true
	}
	if implied, ok := kimiImpliedCosts[out.ID]; ok {
		if out.Cost.Input == 0 {
			out.Cost.Input = implied.Input
		}
		if out.Cost.Output == 0 {
			out.Cost.Output = implied.Output
		}
		if out.Cost.CacheRead == 0 {
			out.Cost.CacheRead = implied.CacheRead
		}
	}
}

const nvidiaBaseURL = "https://integrate.api.nvidia.com/v1"

// nvidiaCompat: NIM is OpenAI-shaped but supports almost none of the optional
// parameters.
func nvidiaCompat(c *ai.CompatFlags) {
	c.SupportsStore = boolptr(false)
	c.SupportsDeveloperRole = boolptr(false)
	c.SupportsReasoningEffort = boolptr(false)
	c.MaxTokensField = strptr("max_tokens")
	c.SupportsStrictMode = boolptr(false)
	c.SupportsLongCacheRetention = boolptr(false)
}

// deepseekCompatFlags is the dialect DeepSeek's own endpoint speaks.
func deepseekCompatFlags() *ai.CompatFlags {
	return &ai.CompatFlags{
		RequiresReasoningContentOnAssistantMessages: boolptr(true),
		ThinkingFormat: strptr("deepseek"),
	}
}

// deepseekModels are hand-written: models.dev does not list the V4 family for
// the direct endpoint.
var deepseekModels = []ai.Model{
	{
		ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash",
		Api: ai.ApiOpenAICompletions, Provider: "deepseek", BaseURL: "https://api.deepseek.com",
		Reasoning: true, Input: []string{"text"},
		Cost: ai.ModelCost{ModelCostRates: ai.ModelCostRates{
			Input: 0.14, Output: 0.28, CacheRead: 0.0028,
		}},
		ContextWindow: 1000000, MaxTokens: 384000,
		Compat: deepseekCompatFlags(),
	},
	{
		ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro",
		Api: ai.ApiOpenAICompletions, Provider: "deepseek", BaseURL: "https://api.deepseek.com",
		Reasoning: true, Input: []string{"text"},
		Cost: ai.ModelCost{ModelCostRates: ai.ModelCostRates{
			Input: 0.435, Output: 0.87, CacheRead: 0.003625,
		}},
		ContextWindow: 1000000, MaxTokens: 384000,
		Compat: deepseekCompatFlags(),
	},
}

// antLingCompatFlags is shared by the whole Ling family.
func antLingCompatFlags() *ai.CompatFlags {
	return &ai.CompatFlags{
		SupportsStore:              boolptr(false),
		SupportsDeveloperRole:      boolptr(false),
		SupportsReasoningEffort:    boolptr(false),
		MaxTokensField:             strptr("max_tokens"),
		SupportsLongCacheRetention: boolptr(false),
	}
}

func antLingModel(id, name string, in, out float64, reasoning bool) ai.Model {
	m := ai.Model{
		ID: id, Name: name,
		Api: ai.ApiOpenAICompletions, Provider: "ant-ling", BaseURL: "https://api.ant-ling.com/v1",
		Reasoning: reasoning, Input: []string{"text"},
		Cost:          ai.ModelCost{ModelCostRates: ai.ModelCostRates{Input: in, Output: out}},
		ContextWindow: 262144, MaxTokens: 65536,
		Compat: antLingCompatFlags(),
	}
	if reasoning {
		m.Compat.ThinkingFormat = strptr("ant-ling")
	}
	return m
}

// antLingModels are hand-written: models.dev does not list this provider.
var antLingModels = []ai.Model{
	antLingModel("Ling-2.6-flash", "Ling 2.6 Flash", 0.01, 0.02, false),
	antLingModel("Ling-2.6-1T", "Ling 2.6 1T", 0.06, 0.25, false),
	antLingModel("Ring-2.6-1T", "Ring 2.6 1T", 0.06, 0.25, true),
}

// antLingRingThinkingLevels: Ring reasons by default, and only the top two
// settings have a documented explicit control.
var antLingRingThinkingLevels = ai.ThinkingLevelMap{
	ai.ThinkingOff: nil, "minimal": nil, "low": nil, "medium": nil,
	"high": strptr("high"), "xhigh": strptr("xhigh"),
}

// nvidiaLiveIDs maps a models.dev id, and its normalised form, onto the id NIM
// actually deploys. It is populated from the vendored /models snapshot.
var nvidiaLiveIDs = map[string]string{}

// normalizeNvidiaID is how NIM's own ids differ from models.dev's: case and
// the separator inside a version.
func normalizeNvidiaID(id string) string {
	return strings.ReplaceAll(strings.ToLower(id), "_", ".")
}

// loadNvidiaLiveIDs reads the vendored NIM catalog.
func loadNvidiaLiveIDs(dataDir string) error {
	var cat struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := readJSON(filepath.Join(dataDir, "nvidia.json"), &cat); err != nil {
		return err
	}
	for _, m := range cat.Data {
		nvidiaLiveIDs[m.ID] = m.ID
		nvidiaLiveIDs[normalizeNvidiaID(m.ID)] = m.ID
	}
	return nil
}

func nvidiaLiveID(id string) (string, bool) {
	if live, ok := nvidiaLiveIDs[id]; ok {
		return live, true
	}
	live, ok := nvidiaLiveIDs[normalizeNvidiaID(id)]
	return live, ok
}

// nvidiaUnsupported are deployed but do not work through the OpenAI-compatible
// endpoint — each was tried.
var nvidiaUnsupported = map[string]bool{
	"abacusai/dracarys-llama-3.1-70b-instruct": true,
	"bytedance/seed-oss-36b-instruct":          true,
	"deepseek-ai/deepseek-v4-flash":            true,
	"deepseek-ai/deepseek-v4-pro":              true,
	"google/gemma-2-2b-it":                     true,
	"google/gemma-3n-e2b-it":                   true,
	"google/gemma-3n-e4b-it":                   true,
	"google/gemma-4-31b-it":                    true,
	"meta/llama-3.2-1b-instruct":               true,
	"meta/llama-4-maverick-17b-128e-instruct":  true,
	"microsoft/phi-4-mini-instruct":            true,
	"minimaxai/minimax-m2.7":                   true,
	"mistralai/mistral-nemotron":               true,
	"nvidia/nemotron-mini-4b-instruct":         true,
	"qwen/qwen3-next-80b-a3b-instruct":         true,
	"qwen/qwen3.5-397b-a17b":                   true,
	"sarvamai/sarvam-m":                        true,
	"upstage/solar-10.7b-instruct":             true,
}

// skipUndeployedNvidia drops anything NIM does not serve, does not serve as
// text, or serves in a form the OpenAI-compatible endpoint cannot use.
func skipUndeployedNvidia(id string, m modelsDevModel) bool {
	if !hasModality(m.Modalities.Input, "text") || !hasModality(m.Modalities.Output, "text") {
		return true
	}
	live, ok := nvidiaLiveID(id)
	return !ok || nvidiaUnsupported[live]
}

func renameToNvidiaLiveID(id string) (string, string) {
	if live, ok := nvidiaLiveID(id); ok {
		return live, ""
	}
	return id, ""
}

func tweakNvidia(_ string, _ modelsDevModel, out *ai.Model) {
	// NIM queues cold models; without a long poll the request returns before
	// the model has loaded.
	out.Headers = map[string]string{"NVCF-POLL-SECONDS": "3600"}
	if out.Compat == nil {
		out.Compat = &ai.CompatFlags{}
	}
	nvidiaCompat(out.Compat)
}

func hasModality(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
