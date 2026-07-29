// Package openairesp implements OpenAI's responses wire.
//
// It is a different protocol from chat-completions, not a variation on it. The
// response is a sequence of typed OUTPUT ITEMS — reasoning, message, function
// call — each with its own lifecycle events, rather than a single stream of
// deltas against one choice. Reasoning comes back as an opaque item that must
// be replayed verbatim on the next turn for the model to keep its own train of
// thought, which is why so much of this file is about preserving ids.
package openairesp

import (
	"strings"

	"github.com/ihavespoons/tau/ai"
)

// compat is the resolved quirk set for one model. There are seven flags rather
// than the chat wire's twenty-one because this protocol is served by far fewer
// hosts, and they agree on more.
type compat struct {
	SupportsDeveloperRole           bool
	SessionAffinityFormat           string
	SupportsLongCacheRetention      bool
	SupportsStrictMode              bool
	SupportsOpenAIGrammarTools      bool
	SupportsToolSearch              bool
	SupportsExplicitPromptCacheMode bool
}

const (
	affinityOpenAI     = "openai"
	affinityOpenRouter = "openrouter"
	// affinityOpenAINoSession sends the correlation header but not session_id,
	// for hosts that reject the field.
	affinityOpenAINoSession = "openai-nosession"
)

// detectCompat derives the defaults. Only session affinity varies by host; the
// rest default the same way everywhere and are set from the catalog.
func detectCompat(model *ai.Model) compat {
	affinity := affinityOpenAI
	if model.Provider == "openrouter" || strings.Contains(model.BaseURL, "openrouter.ai") {
		affinity = affinityOpenRouter
	}
	return compat{
		SupportsDeveloperRole:      true,
		SessionAffinityFormat:      affinity,
		SupportsLongCacheRetention: true,
		// Strict mode and grammar tools are opt-in: a host that does not know
		// them rejects the whole request rather than ignoring the field.
		SupportsStrictMode:              false,
		SupportsOpenAIGrammarTools:      false,
		SupportsToolSearch:              false,
		SupportsExplicitPromptCacheMode: false,
	}
}

// resolveCompat layers the model's declared overrides onto detection.
func resolveCompat(model *ai.Model) compat {
	c := detectCompat(model)
	o := model.Compat
	if o == nil {
		return c
	}
	c.SupportsDeveloperRole = boolOr(o.SupportsDeveloperRole, c.SupportsDeveloperRole)
	c.SessionAffinityFormat = strOr(o.SessionAffinityFormat, c.SessionAffinityFormat)
	c.SupportsLongCacheRetention = boolOr(o.SupportsLongCacheRetention, c.SupportsLongCacheRetention)
	c.SupportsStrictMode = boolOr(o.SupportsStrictMode, c.SupportsStrictMode)
	c.SupportsOpenAIGrammarTools = boolOr(o.SupportsOpenAIGrammarTools, c.SupportsOpenAIGrammarTools)
	c.SupportsToolSearch = boolOr(o.SupportsToolSearch, c.SupportsToolSearch)
	c.SupportsExplicitPromptCacheMode = boolOr(o.SupportsExplicitPromptCacheMode, c.SupportsExplicitPromptCacheMode)
	return c
}

func boolOr(override *bool, fallback bool) bool {
	if override == nil {
		return fallback
	}
	return *override
}

func strOr(override *string, fallback string) string {
	if override == nil || *override == "" {
		return fallback
	}
	return *override
}

// resolveCacheRetention defaults to short-lived caching; only the environment
// opts into long retention. TAU_ is accepted alongside Pi's PI_ so an existing
// setup keeps working.
func resolveCacheRetention(retention ai.CacheRetention, env map[string]string) ai.CacheRetention {
	if retention != "" {
		return retention
	}
	for _, key := range []string{"TAU_CACHE_RETENTION", "PI_CACHE_RETENTION"} {
		if v, ok := env[key]; ok && v == "long" {
			return ai.CacheLong
		}
	}
	return ai.CacheShort
}
