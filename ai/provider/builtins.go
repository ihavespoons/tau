package provider

import (
	"sort"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/ai/provider/catalog"
)

// builtinNames gives each compiled provider its display name. A provider with
// compiled models but no entry here still works; it is just listed by its id.
var builtinNames = map[string]string{
	"anthropic":             "Anthropic",
	"vercel-ai-gateway":     "Vercel AI Gateway",
	"openrouter":            "OpenRouter",
	"cerebras":              "Cerebras",
	"cloudflare-workers-ai": "Cloudflare Workers AI",
	"google":                "Google",
	"groq":                  "Groq",
	"huggingface":           "Hugging Face",
	"mistral":               "Mistral",
	"moonshotai":            "Moonshot AI",
	"moonshotai-cn":         "Moonshot AI (CN)",
	"openai":                "OpenAI",
	"xai":                   "xAI",
	"xiaomi":                "Xiaomi",
}

// Builtins returns every provider tau ships with, in id order.
//
// A provider appears here as soon as it has compiled model data, even when tau
// cannot yet talk to its wire API. That is deliberate: a catalogued model is
// visible in `tau models` and selectable, and choosing one produces a clear
// "no wire" error rather than the model simply not existing. The alternative —
// hiding it — makes a supported provider look unsupported.
func Builtins(store auth.CredentialStore, env auth.EnvContext) []*Provider {
	ids := catalog.ProviderIDs()
	sort.Strings(ids)

	out := make([]*Provider, 0, len(ids))
	for _, id := range ids {
		out = append(out, builtin(id, store, env))
	}
	return out
}

func builtin(id string, store auth.CredentialStore, env auth.EnvContext) *Provider {
	// Anthropic has its own factory: it is the only built-in with an OAuth
	// flow and a base-URL redirect, neither of which fits the generic path.
	if id == "anthropic" {
		return Anthropic(store, env)
	}

	models := catalog.Models(id)
	name := builtinNames[id]
	if name == "" {
		name = id
	}

	// Every model of a built-in provider shares its wire, so the first one
	// names it.
	api := ai.Api("")
	baseURL := ""
	if len(models) > 0 {
		api, baseURL = models[0].Api, models[0].BaseURL
	}

	switch api {
	case ai.ApiOpenAICompletions:
		return OpenAICompat(store, env, OpenAICompatOptions{
			ID: id, Name: name, BaseURL: baseURL,
			EnvKeys: auth.EnvKeysFor(id), Models: models,
		})
	case ai.ApiAnthropicMessages:
		return AnthropicCompat(store, env, AnthropicCompatOptions{
			ID: id, Name: name, BaseURL: baseURL,
			EnvKeys: auth.EnvKeysFor(id), Models: models,
		})
	}

	return &Provider{
		ID: ai.ProviderId(id), Name: name, Api: api, BaseURL: baseURL,
		EnvKeys: auth.EnvKeysFor(id), Models: models,
	}
}
