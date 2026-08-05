package bedrock

import (
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// Bedrock hosts models from several vendors behind one API, and most of what
// this file decides is "which vendor is this, really". The answer is not always
// in the model id: an application inference profile is an ARN that names the
// profile and not the model behind it, so both the id and the catalog name are
// searched, and there are environment escape hatches for the cases where
// neither says anything useful.

// matchCandidates returns the strings a model predicate should search: the id
// and name, each also with separators folded to dashes so `claude 4.5`,
// `claude_4_5` and `claude-4-5` all match one pattern.
func matchCandidates(id, name string) []string {
	values := []string{id}
	if name != "" {
		values = append(values, name)
	}
	out := make([]string, 0, len(values)*2)
	for _, v := range values {
		lower := strings.ToLower(v)
		out = append(out, lower, foldSeparators(lower))
	}
	return out
}

func foldSeparators(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range s {
		if r == ' ' || r == '_' || r == '.' || r == ':' {
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	return b.String()
}

func anyCandidateContains(candidates []string, needles ...string) bool {
	for _, c := range candidates {
		for _, n := range needles {
			if strings.Contains(c, n) {
				return true
			}
		}
	}
	return false
}

// isAnthropicClaudeModel reports whether the model behind this id is a Claude.
// It gates the thinking fields, the signature field, and the max-token default.
func isAnthropicClaudeModel(model *ai.Model) bool {
	id := strings.ToLower(model.ID)
	name := strings.ToLower(model.Name)
	return strings.Contains(id, "anthropic.claude") ||
		strings.Contains(id, "anthropic/claude") ||
		strings.Contains(name, "anthropic.claude") ||
		strings.Contains(name, "anthropic/claude") ||
		strings.Contains(name, "claude")
}

// supportsAdaptiveThinking reports whether the model takes an effort level
// rather than a token budget.
func supportsAdaptiveThinking(model *ai.Model) bool {
	return anyCandidateContains(matchCandidates(model.ID, model.Name),
		"opus-4-6", "opus-4-7", "opus-4-8", "opus-5", "sonnet-4-6", "sonnet-5", "fable-5")
}

// supportsNativeXhighEffort reports whether "xhigh" is a real effort level for
// this model rather than one that has to be clamped down to "high".
func supportsNativeXhighEffort(model *ai.Model) bool {
	return anyCandidateContains(matchCandidates(model.ID, model.Name),
		"opus-4-7", "opus-4-8", "opus-5", "sonnet-5", "fable-5")
}

// supportsThinkingSignature reports whether the model accepts a signature on
// replayed reasoning. Only Claude does; everything else rejects the field with
// an explicit validation error.
func supportsThinkingSignature(model *ai.Model) bool { return isAnthropicClaudeModel(model) }

// supportsPromptCaching reports whether to emit cache breakpoints.
//
// Nova models cache automatically and need no breakpoints, so a wrong answer
// here is not merely a lost optimization — an explicit cache point on a model
// that does not take one is a request error.
func supportsPromptCaching(model *ai.Model, env map[string]string) bool {
	candidates := matchCandidates(model.ID, model.Name)

	if !anyCandidateContains(candidates, "claude") {
		// An application inference profile's ARN names neither the vendor nor
		// the model, so there is nothing left to detect from. This is the
		// manual override for that case.
		return apishared.EnvValue(env, "AWS_BEDROCK_FORCE_CACHE") == "1"
	}
	switch {
	case anyCandidateContains(candidates, "fable-5", "opus-5", "sonnet-5"):
		return true
	case anyCandidateContains(candidates, "-4-"):
		return true
	case anyCandidateContains(candidates, "claude-3-7-sonnet"):
		return true
	case anyCandidateContains(candidates, "claude-3-5-haiku"):
		return true
	}
	return false
}

// isGovCloudTarget reports whether the request is bound for GovCloud, whose
// Converse schema lags the commercial one.
func isGovCloudTarget(model *ai.Model, opts *Options) bool {
	if strings.HasPrefix(strings.ToLower(configuredRegion(opts)), "us-gov-") {
		return true
	}
	id := strings.ToLower(model.ID)
	return strings.HasPrefix(id, "us-gov.") || strings.HasPrefix(id, "arn:aws-us-gov:")
}

// mapThinkingLevelToEffort converts a tau thinking level to a Claude effort.
func mapThinkingLevelToEffort(model *ai.Model, level ai.ThinkingLevel) string {
	if level == ai.ThinkingXHigh && supportsNativeXhighEffort(model) {
		return "xhigh"
	}
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

// defaultThinkingBudgets are the token budgets for budget-based Claude models.
// xhigh and max are clamped to high because those levels have no budget of
// their own — they only exist on the effort-based models.
var defaultThinkingBudgets = map[ai.ThinkingLevel]int{
	ai.ThinkingMinimal: 1024,
	ai.ThinkingLow:     2048,
	ai.ThinkingMedium:  8192,
	ai.ThinkingHigh:    16384,
	ai.ThinkingXHigh:   16384,
	ai.ThinkingMax:     16384,
}

// buildAdditionalModelRequestFields returns the vendor-specific request fields,
// which is where thinking is configured. Everything here is Claude-only:
// Converse has no portable thinking parameter.
func buildAdditionalModelRequestFields(model *ai.Model, opts *Options) map[string]any {
	if opts.Reasoning == "" || !model.Reasoning || !isAnthropicClaudeModel(model) {
		return nil
	}

	// GovCloud rejects the thinking.display field, so it is omitted there until
	// that schema catches up.
	display := opts.ThinkingDisplay
	if display == "" {
		display = ThinkingDisplaySummarized
	}
	if isGovCloudTarget(model, opts) {
		display = ""
	}

	if supportsAdaptiveThinking(model) {
		thinking := map[string]any{"type": "adaptive"}
		if display != "" {
			thinking["display"] = string(display)
		}
		return map[string]any{
			"thinking":      thinking,
			"output_config": map[string]any{"effort": mapThinkingLevelToEffort(model, opts.Reasoning)},
		}
	}

	budget := defaultThinkingBudgets[opts.Reasoning]
	if custom, ok := customBudget(opts.ThinkingBudgets, apishared.ClampReasoning(opts.Reasoning)); ok {
		budget = custom
	}
	thinking := map[string]any{"type": "enabled", "budget_tokens": budget}
	if display != "" {
		thinking["display"] = string(display)
	}
	fields := map[string]any{"thinking": thinking}
	if opts.InterleavedThinking == nil || *opts.InterleavedThinking {
		fields["anthropic_beta"] = []any{"interleaved-thinking-2025-05-14"}
	}
	return fields
}

// customBudget reads a caller-supplied budget for one level.
func customBudget(budgets *ai.ThinkingBudgets, level ai.ThinkingLevel) (int, bool) {
	if budgets == nil {
		return 0, false
	}
	switch level {
	case ai.ThinkingMinimal:
		if budgets.Minimal != nil {
			return *budgets.Minimal, true
		}
	case ai.ThinkingLow:
		if budgets.Low != nil {
			return *budgets.Low, true
		}
	case ai.ThinkingMedium:
		if budgets.Medium != nil {
			return *budgets.Medium, true
		}
	case ai.ThinkingHigh:
		if budgets.High != nil {
			return *budgets.High, true
		}
	}
	return 0, false
}

// resolveCacheRetention defaults to short retention, honouring the legacy
// PI_CACHE_RETENTION variable Pi reads.
func resolveCacheRetention(retention ai.CacheRetention, env map[string]string) ai.CacheRetention {
	if retention != "" {
		return retention
	}
	if apishared.EnvValue(env, "PI_CACHE_RETENTION") == "long" ||
		apishared.EnvValue(env, "TAU_CACHE_RETENTION") == "long" {
		return ai.CacheLong
	}
	return ai.CacheShort
}
