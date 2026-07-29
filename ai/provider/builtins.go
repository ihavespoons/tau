package provider

import (
	"sort"

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
	"xiaomi-token-plan-ams": "Xiaomi Token Plan (AMS)",
	"xiaomi-token-plan-cn":  "Xiaomi Token Plan (CN)",
	"xiaomi-token-plan-sgp": "Xiaomi Token Plan (SGP)",
	"qwen-token-plan":       "Qwen Token Plan",
	"qwen-token-plan-cn":    "Qwen Token Plan (CN)",
	"together":              "Together AI",
	"fireworks":             "Fireworks",
	"zai":                   "Z.ai",
	"zai-coding-cn":         "Z.ai (CN)",
	"minimax":               "MiniMax",
	"minimax-cn":            "MiniMax (CN)",
	"kimi-coding":           "Kimi For Coding",
	"nvidia":                "NVIDIA NIM",
	"deepseek":              "DeepSeek",
	"ant-ling":              "Ant Ling",
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

	baseURL := ""
	if len(models) > 0 {
		baseURL = models[0].BaseURL
	}

	// Keyed dispatches per model rather than per provider, so a catalog that
	// spans two wires — Fireworks and xAI both do — routes each model to the
	// right one, and a model on a wire tau has not built yet fails by name.
	return Keyed(store, env, KeyedOptions{
		ID: id, Name: name, BaseURL: baseURL,
		EnvKeys: auth.EnvKeysFor(id), Models: models,
	})
}
