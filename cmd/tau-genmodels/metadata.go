package main

import (
	"reflect"
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/openaichat"
)

// The passes below run over every finished model, in order. They are a port of
// Pi's apply*Metadata functions, and they are the reason the catalog is worth
// generating at all: models.dev describes a model, but whether that model
// accepts "xhigh", rejects a temperature, or needs strict tools declared is
// knowledge that lives nowhere but here.

// applyMetadata runs the passes in Pi's order, which is load-bearing in two
// places.
//
// The models.dev thinking map is applied BEFORE the hand corrections, so a
// correction can contradict it. And because that pass only applies to models
// whose wire takes a direct effort value — which for Anthropic means the
// adaptive-thinking flag, set later in applyThinkingLevels — models.dev's map
// never reaches an Anthropic model at all. Running the two in the other order
// would quietly give every Claude model a full six-level map it does not have.
func applyMetadata(m *ai.Model, idx *reasoningIndex) {
	applyCompletionsCompat(m)
	applyReasoningMetadata(m, idx)
	applyThinkingLevels(m)
	applyStrictToolCompat(m)
	applyGrammarToolCompat(m)
	applyExplicitPromptCache(m)
}

// applyCompletionsCompat bakes chat-completions detection into the catalog, so
// the compiled data says what the wire will actually do.
func applyCompletionsCompat(m *ai.Model) {
	if m.Api != ai.ApiOpenAICompletions {
		return
	}
	// Detection fills the gaps rather than overwriting: a hand correction is a
	// statement about how the provider actually behaves, and detection — which
	// reasons only from the id and the URL — must not contradict it. Kimi K3 is
	// the case that proves it: everything about Moonshot says no reasoning
	// effort, and K3 alone takes one.
	if delta := openaichat.DetectedCompatDelta(m); delta != nil {
		fillCompat(m, delta)
	}
}

// applyStrictToolCompat declares strict tool schemas where the provider
// honours them.
func applyStrictToolCompat(m *ai.Model) {
	switch {
	case m.Provider == "openai" && m.Api == ai.ApiOpenAIResponses:
		mergeCompat(m, &ai.CompatFlags{SupportsStrictMode: boolptr(true)})
	case m.Provider == "anthropic" && m.Api == ai.ApiAnthropicMessages:
		mergeCompat(m, &ai.CompatFlags{SupportsStrictTools: boolptr(true)})
	}
}

// grammarToolProviders and grammarToolApis are the combinations verified or
// documented to pass OpenAI custom grammar tools through.
var (
	grammarToolProviders = map[ai.ProviderId]bool{
		"openai": true, "openai-codex": true, "azure-openai-responses": true,
		"github-copilot": true, "opencode": true, "cloudflare-ai-gateway": true,
	}
	grammarToolApis = map[ai.Api]bool{
		ai.ApiOpenAIResponses: true, ai.ApiAzureOpenAIResponses: true, ai.ApiOpenAICodexResponses: true,
	}
	gptMajor = regexp.MustCompile(`^gpt-(\d+)`)
)

// applyGrammarToolCompat marks grammar-tool support, which OpenAI rejects for
// anything before the GPT-5 family.
func applyGrammarToolCompat(m *ai.Model) {
	if !grammarToolApis[m.Api] || !grammarToolProviders[m.Provider] {
		return
	}
	match := gptMajor.FindStringSubmatch(m.ID)
	if match == nil || match[1] < "5" || len(match[1]) > 1 {
		return
	}
	mergeCompat(m, &ai.CompatFlags{SupportsOpenAIGrammarTools: boolptr(true)})
}

// applyExplicitPromptCache marks the models that both charge for cache writes
// and accept the parameter that controls them. Older OpenAI models reject it.
func applyExplicitPromptCache(m *ai.Model) {
	if m.Provider != "openai" || m.Api != ai.ApiOpenAIResponses {
		return
	}
	if m.Cost.CacheWrite <= 0 {
		return
	}
	mergeCompat(m, &ai.CompatFlags{SupportsExplicitPromptCacheMode: boolptr(true)})
}

// containsAny reports whether id contains any of the fragments.
func containsAny(id string, fragments ...string) bool {
	for _, f := range fragments {
		if strings.Contains(id, f) {
			return true
		}
	}
	return false
}

// anthropicAdaptiveThinking models take an effort string rather than a token
// budget.
func anthropicAdaptiveThinking(id string) bool {
	return containsAny(id,
		"opus-4-6", "opus-4.6", "opus-4-7", "opus-4.7", "opus-4-8", "opus-4.8",
		"opus-5", "opus.5", "sonnet-4-6", "sonnet-4.6", "sonnet-5", "sonnet.5", "fable-5")
}

// anthropicTemperatureUnsupported models reject the temperature parameter
// outright rather than clamping it.
func anthropicTemperatureUnsupported(id string) bool {
	return containsAny(strings.ToLower(id),
		"opus-4-7", "opus-4.7", "opus-4-8", "opus-4.8", "opus-5", "opus.5")
}

func supportsOpenAIXhigh(id string) bool {
	return containsAny(id, "gpt-5.2", "gpt-5.3", "gpt-5.4", "gpt-5.5", "gpt-5.6")
}

func supportsOpenAIMax(m *ai.Model) bool {
	if !strings.Contains(m.ID, "gpt-5.6") {
		return false
	}
	switch m.Api {
	case ai.ApiOpenAIResponses, ai.ApiAzureOpenAIResponses, ai.ApiOpenAICodexResponses, ai.ApiOpenAICompletions:
		return true
	}
	return false
}

func isGoogleThinkingApi(m *ai.Model) bool {
	return m.Api == ai.ApiGoogleGenerativeAI || m.Api == ai.ApiGoogleVertex
}

var (
	gemini3Pro   = regexp.MustCompile(`gemini-3(?:\.\d+)?-pro`)
	gemini3Flash = regexp.MustCompile(`gemini-3(?:\.\d+)?-flash`)
	gemma4       = regexp.MustCompile(`gemma-?4`)
)

func isGemini3Pro(id string) bool { return gemini3Pro.MatchString(strings.ToLower(id)) }

func isGemini3Flash(id string) bool {
	low := strings.ToLower(id)
	return gemini3Flash.MatchString(low) || low == "gemini-flash-latest" || low == "gemini-flash-lite-latest"
}

func isGemma4(id string) bool { return gemma4.MatchString(strings.ToLower(id)) }

// applyThinkingLevels layers the per-model thinking corrections. Order matters
// throughout: later merges overwrite earlier ones, and several models are
// matched by more than one rule.
func applyThinkingLevels(m *ai.Model) {
	if (m.Api == ai.ApiOpenAIResponses || m.Api == ai.ApiAzureOpenAIResponses) && strings.HasPrefix(m.ID, "gpt-5") {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{ai.ThinkingOff: nil})
	}
	if supportsOpenAIXhigh(m.ID) {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{"xhigh": strptr("xhigh")})
	}
	if supportsOpenAIMax(m) {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{"max": strptr("max")})
	}
	if m.Provider == "openai" && m.ID == "gpt-5.5" {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{"minimal": nil})
	}
	if m.Api == ai.ApiOpenAIResponses && m.Provider == "openai" && openAINoneReasoning[m.ID] {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{ai.ThinkingOff: strptr("none")})
	}
	if m.Provider == "xai" && m.Api == ai.ApiOpenAIResponses && m.ID == xaiResponsesModel {
		// Grok's responses endpoint reasons by default and rejects the two
		// lowest settings outright.
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{ai.ThinkingOff: nil, "minimal": nil})
	}
	if strings.HasSuffix(m.ID, "gpt-5.5-pro") {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{ai.ThinkingOff: nil, "minimal": nil, "low": nil})
	}

	// Anthropic adaptive thinking: "max" everywhere it applies, "xhigh" only
	// on the newer Opus and Sonnet lines.
	if containsAny(m.ID, "opus-4-6", "opus-4.6", "sonnet-4-6", "sonnet-4.6") {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{"max": strptr("max")})
	}
	if containsAny(m.ID, "opus-4-7", "opus-4.7", "opus-4-8", "opus-4.8",
		"opus-5", "opus.5", "sonnet-5", "sonnet.5") {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{"xhigh": strptr("xhigh"), "max": strptr("max")})
	}
	if strings.Contains(m.ID, "fable-5") {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{
			ai.ThinkingOff: nil, "xhigh": strptr("xhigh"), "max": strptr("max"),
		})
	}
	if m.Api == ai.ApiAnthropicMessages && anthropicAdaptiveThinking(m.ID) {
		mergeCompat(m, &ai.CompatFlags{ForceAdaptiveThinking: boolptr(true)})
	}
	if m.Api == ai.ApiAnthropicMessages && anthropicTemperatureUnsupported(m.ID) {
		mergeCompat(m, &ai.CompatFlags{SupportsTemperature: boolptr(false)})
	}

	// Gemini and Gemma name their levels in upper case and support only some.
	if isGoogleThinkingApi(m) && isGemini3Pro(m.ID) {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{
			ai.ThinkingOff: nil, "minimal": nil, "low": strptr("LOW"),
			"medium": nil, "high": strptr("HIGH"),
		})
	}
	if isGoogleThinkingApi(m) && isGemini3Flash(m.ID) {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{ai.ThinkingOff: nil})
	}
	if isGoogleThinkingApi(m) && isGemma4(m.ID) {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{
			ai.ThinkingOff: nil, "minimal": strptr("MINIMAL"), "low": nil,
			"medium": nil, "high": strptr("HIGH"),
		})
	}

	if m.Api == ai.ApiOpenAICompletions && strings.Contains(m.ID, "deepseek-v4") {
		mergeThinkingLevelMap(m, deepseekV4ThinkingLevels(m.Provider))
		applyDeepSeekV4Compat(m)
	}
	if m.Provider == openRouterProviderID && strings.HasPrefix(m.ID, "inception/mercury-2") {
		// Mercury 2's instant mode — reasoning effort "none" — turns off tool
		// calling. Marking "off" unsupported makes the wire omit the parameter
		// rather than default to it, which would silently disarm every tool.
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{ai.ThinkingOff: nil})
	}
	if m.Provider == openRouterProviderID && m.ID == "z-ai/glm-5.2" {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{"xhigh": strptr("xhigh")})
	}
	if m.Provider == "groq" && m.ID == "qwen/qwen3-32b" {
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{
			"minimal": nil, "low": nil, "medium": nil, "high": strptr("default"),
		})
	}
	if (m.Provider == "moonshotai" || m.Provider == "moonshotai-cn") &&
		(m.ID == "kimi-k2.7-code" || m.ID == "kimi-k2.7-code-highspeed") {
		// K2.7 Code always thinks: the docs say a disable request is rejected,
		// and omitting the parameter is how you get the enabled default.
		mergeThinkingLevelMap(m, ai.ThinkingLevelMap{ai.ThinkingOff: nil})
	}
}

// mergeCompat layers flags onto a model, allocating the block on first use.
// Only non-nil fields are copied, so a later pass cannot erase an earlier one.
func mergeCompat(m *ai.Model, flags *ai.CompatFlags) {
	if flags == nil {
		return
	}
	if m.Compat == nil {
		m.Compat = &ai.CompatFlags{}
	}
	overlayCompat(m.Compat, flags)
}

// fillCompat copies set fields of src onto dst only where dst has none.
func fillCompat(m *ai.Model, flags *ai.CompatFlags) {
	if flags == nil {
		return
	}
	if m.Compat == nil {
		m.Compat = &ai.CompatFlags{}
	}
	d := reflect.ValueOf(m.Compat).Elem()
	s := reflect.ValueOf(flags).Elem()
	for i := 0; i < s.NumField(); i++ {
		f := s.Field(i)
		if f.Kind() == reflect.Pointer && f.IsNil() {
			continue
		}
		if f.Kind() != reflect.Pointer && f.IsZero() {
			continue
		}
		if dst := d.Field(i); dst.IsZero() {
			dst.Set(f)
		}
	}
}

// overlayCompat copies every set field of src onto dst.
//
// It reflects rather than listing fields, because a compat flag added to
// ai.CompatFlags and forgotten here would be silently dropped from every
// generated catalog — a failure with no symptom until a provider rejects a
// request months later.
func overlayCompat(dst, src *ai.CompatFlags) {
	d := reflect.ValueOf(dst).Elem()
	s := reflect.ValueOf(src).Elem()

	for i := 0; i < s.NumField(); i++ {
		f := s.Field(i)
		if f.Kind() == reflect.Pointer && f.IsNil() {
			continue
		}
		if f.Kind() != reflect.Pointer && f.IsZero() {
			continue
		}
		d.Field(i).Set(f)
	}
}
