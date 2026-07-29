package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Anthropic credential environment variables, in discovery order.
const (
	AnthropicAuthTokenEnv  = "ANTHROPIC_AUTH_TOKEN"
	AnthropicOAuthTokenEnv = "ANTHROPIC_OAUTH_TOKEN"
	AnthropicAPIKeyEnv     = "ANTHROPIC_API_KEY"
)

// OSContext is the default EnvContext: env vars from the process
// environment (blank-trimmed, like Pi) and real filesystem checks with `~`
// expansion.
type OSContext struct {
	// Overrides take precedence over the process environment (Pi's
	// provider-scoped ProviderEnv overlay).
	Overrides ProviderEnv
}

func (c OSContext) Env(name string) string {
	if v, ok := c.Overrides[name]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	v := os.Getenv(name)
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return v
}

func (c OSContext) FileExists(path string) bool {
	resolved := path
	if strings.HasPrefix(resolved, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		resolved = filepath.Join(home, resolved[1:])
	}
	_, err := os.Stat(resolved)
	return err == nil
}

// MapContext is an EnvContext backed by a map, for tests.
type MapContext map[string]string

func (m MapContext) Env(name string) string {
	v := m[name]
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return v
}

func (MapContext) FileExists(string) bool { return false }

// overlayContext gives ProviderEnv values precedence over a base Context.
type overlayContext struct {
	base EnvContext
	env  ProviderEnv
}

func (o overlayContext) Env(name string) string {
	if v, ok := o.env[name]; ok && v != "" {
		return v
	}
	return o.base.Env(name)
}

func (o overlayContext) FileExists(path string) bool { return o.base.FileExists(path) }

// WithEnvOverlay returns an EnvContext where env values take precedence over base.
func WithEnvOverlay(base EnvContext, env ProviderEnv) EnvContext {
	if len(env) == 0 {
		return base
	}
	return overlayContext{base: base, env: env}
}

// AnthropicAPIKeyAuth is the Anthropic api-key auth, ported from Pi's
// providers/anthropic.ts. Resolution order:
//
//  1. stored credential key            → apiKey
//  2. ANTHROPIC_AUTH_TOKEN             → Authorization: Bearer <token>
//     (never apiKey: the request must send it as a bearer header)
//  3. ANTHROPIC_OAUTH_TOKEN            → apiKey (OAuth-shaped request auth)
//  4. ANTHROPIC_API_KEY                → apiKey
func AnthropicAPIKeyAuth() *APIKeyAuth {
	return &APIKeyAuth{
		Name: "Anthropic API key",
		Login: func(ctx context.Context, in Interaction) (*Credential, error) {
			key, err := in.Prompt(ctx, Prompt{Type: PromptSecret, Message: "Enter Anthropic API key"})
			if err != nil {
				return nil, err
			}
			return &Credential{Type: CredentialAPIKey, Key: key}, nil
		},
		Resolve: func(ac EnvContext, cred *Credential) (*AuthResult, error) {
			if cred != nil && cred.Key != "" {
				return &AuthResult{
					Auth:   ModelAuth{APIKey: cred.Key},
					Env:    cred.Env,
					Source: "stored credential",
				}, nil
			}
			if token := ac.Env(AnthropicAuthTokenEnv); token != "" {
				bearer := "Bearer " + token
				return &AuthResult{
					Auth:   ModelAuth{Headers: map[string]*string{"Authorization": &bearer}},
					Source: AnthropicAuthTokenEnv,
				}, nil
			}
			for _, name := range []string{AnthropicOAuthTokenEnv, AnthropicAPIKeyEnv} {
				if key := ac.Env(name); key != "" {
					return &AuthResult{Auth: ModelAuth{APIKey: key}, Source: name}, nil
				}
			}
			return nil, nil
		},
	}
}

// anthropicEnvVars is Pi's discovery list for the anthropic provider.
var anthropicEnvVars = []string{AnthropicAuthTokenEnv, AnthropicOAuthTokenEnv, AnthropicAPIKeyEnv}

// apiKeyEnvVars maps a provider id to the env vars that can supply its key.
// Ported from Pi's env-api-keys.ts getApiKeyEnvVars.
var apiKeyEnvVars = map[string][]string{
	"anthropic":              anthropicEnvVars,
	"github-copilot":         {"COPILOT_GITHUB_TOKEN"},
	"ant-ling":               {"ANT_LING_API_KEY"},
	"qwen-token-plan":        {"QWEN_TOKEN_PLAN_API_KEY"},
	"qwen-token-plan-cn":     {"QWEN_TOKEN_PLAN_CN_API_KEY"},
	"openai":                 {"OPENAI_API_KEY"},
	"azure-openai-responses": {"AZURE_OPENAI_API_KEY"},
	"nvidia":                 {"NVIDIA_API_KEY"},
	"deepseek":               {"DEEPSEEK_API_KEY"},
	"google":                 {"GEMINI_API_KEY"},
	"google-vertex":          {"GOOGLE_CLOUD_API_KEY"},
	"groq":                   {"GROQ_API_KEY"},
	"cerebras":               {"CEREBRAS_API_KEY"},
	"xai":                    {"XAI_API_KEY"},
	"radius":                 {"RADIUS_API_KEY"},
	"openrouter":             {"OPENROUTER_API_KEY"},
	"vercel-ai-gateway":      {"AI_GATEWAY_API_KEY"},
	"zai":                    {"ZAI_API_KEY"},
	"zai-coding-cn":          {"ZAI_CODING_CN_API_KEY"},
	"mistral":                {"MISTRAL_API_KEY"},
	"minimax":                {"MINIMAX_API_KEY"},
	"minimax-cn":             {"MINIMAX_CN_API_KEY"},
	"moonshotai":             {"MOONSHOT_API_KEY"},
	"moonshotai-cn":          {"MOONSHOT_API_KEY"},
	"huggingface":            {"HF_TOKEN"},
	"fireworks":              {"FIREWORKS_API_KEY"},
	"together":               {"TOGETHER_API_KEY"},
	"opencode":               {"OPENCODE_API_KEY"},
	"opencode-go":            {"OPENCODE_API_KEY"},
	"kimi-coding":            {"KIMI_API_KEY"},
	"cloudflare-workers-ai":  {"CLOUDFLARE_API_KEY"},
	"cloudflare-ai-gateway":  {"CLOUDFLARE_API_KEY"},
	"xiaomi":                 {"XIAOMI_API_KEY"},
	"xiaomi-token-plan-cn":   {"XIAOMI_TOKEN_PLAN_CN_API_KEY"},
	"xiaomi-token-plan-ams":  {"XIAOMI_TOKEN_PLAN_AMS_API_KEY"},
	"xiaomi-token-plan-sgp":  {"XIAOMI_TOKEN_PLAN_SGP_API_KEY"},
}

// FindEnvKeys reports which known API-key env vars are set for a provider.
// It intentionally excludes ambient sources (AWS profiles, Google ADC).
func FindEnvKeys(providerID string, ac EnvContext) []string {
	names, ok := apiKeyEnvVars[providerID]
	if !ok {
		return nil
	}
	var found []string
	for _, name := range names {
		if ac.Env(name) != "" {
			found = append(found, name)
		}
	}
	return found
}

// EnvAPIKey returns a provider's API key from a known env var, skipping vars
// that must be sent as a bearer header rather than a key (Anthropic's
// ANTHROPIC_AUTH_TOKEN).
func EnvAPIKey(providerID string, ac EnvContext) string {
	found := FindEnvKeys(providerID, ac)
	if len(found) == 0 {
		return ""
	}
	pick := found[0]
	if providerID == "anthropic" {
		pick = ""
		for _, name := range found {
			if name != AnthropicAuthTokenEnv {
				pick = name
				break
			}
		}
		if pick == "" {
			return ""
		}
	}
	return ac.Env(pick)
}

// DefaultConfigValue resolves `$NAME` / `${NAME}` references in a stored
// credential value against the provider env then the process environment.
// `$$` and `$!` are literal escapes. Values with no reference are returned
// unchanged.
//
// Pi additionally supports `!command` shell indirection
// (resolve-config-value.ts); that belongs to the app layer and is not
// implemented here — such values are returned verbatim.
func DefaultConfigValue(value string, env ProviderEnv) string {
	if !strings.Contains(value, "$") {
		return value
	}
	var b strings.Builder
	for i := 0; i < len(value); {
		c := value[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(value) {
			b.WriteByte('$')
			break
		}
		next := value[i+1]
		if next == '$' || next == '!' {
			b.WriteByte(next)
			i += 2
			continue
		}
		if next == '{' {
			end := strings.IndexByte(value[i+2:], '}')
			if end < 0 {
				b.WriteByte('$')
				i++
				continue
			}
			name := value[i+2 : i+2+end]
			if isEnvName(name) {
				b.WriteString(lookupEnv(name, env))
			} else {
				b.WriteString(value[i : i+2+end+1])
			}
			i += 2 + end + 1
			continue
		}
		name := envNamePrefix(value[i+1:])
		if name == "" {
			b.WriteByte('$')
			i++
			continue
		}
		b.WriteString(lookupEnv(name, env))
		i += 1 + len(name)
	}
	return b.String()
}

func lookupEnv(name string, env ProviderEnv) string {
	if v, ok := env[name]; ok && v != "" {
		return v
	}
	return os.Getenv(name)
}

func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	return envNamePrefix(s) == s
}

func envNamePrefix(s string) string {
	i := 0
	for i < len(s) {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			break
		}
		i++
	}
	return s[:i]
}

// EnvAPIKeyAuth is the generic api-key auth used by every provider that
// authenticates with a bearer key and nothing more exotic (Pi's
// envApiKeyAuth).
//
// Resolution order is stored credential, then the configured value from
// models.json, then the ambient environment. The configured value goes through
// DefaultConfigValue, so `"apiKey": "$GROQ_API_KEY"` in models.json resolves
// against the environment rather than being sent literally.
func EnvAPIKeyAuth(providerID, name string, envVars []string, configured string) *APIKeyAuth {
	return &APIKeyAuth{
		Name: name,
		Login: func(ctx context.Context, in Interaction) (*Credential, error) {
			key, err := in.Prompt(ctx, Prompt{
				Type: PromptSecret, Message: "Enter " + name,
			})
			if err != nil {
				return nil, err
			}
			return &Credential{Type: CredentialAPIKey, Key: key}, nil
		},
		Resolve: func(ac EnvContext, cred *Credential) (*AuthResult, error) {
			if cred != nil && cred.Key != "" {
				return &AuthResult{
					Auth:   ModelAuth{APIKey: cred.Key},
					Env:    cred.Env,
					Source: "stored credential",
				}, nil
			}
			if configured != "" {
				if key := DefaultConfigValue(configured, nil); key != "" && !strings.HasPrefix(key, "$") {
					return &AuthResult{Auth: ModelAuth{APIKey: key}, Source: "models.json"}, nil
				}
			}
			for _, envVar := range envVars {
				if key := ac.Env(envVar); key != "" {
					return &AuthResult{Auth: ModelAuth{APIKey: key}, Source: envVar}, nil
				}
			}
			// Fall back to the provider's own known env vars, so a built-in
			// provider works with no configuration at all.
			if key := EnvAPIKey(providerID, ac); key != "" {
				return &AuthResult{Auth: ModelAuth{APIKey: key}, Source: "environment"}, nil
			}
			return nil, nil
		},
	}
}

// EnvKeysFor returns the environment variables that can supply a provider's
// API key, most specific first. An unknown provider has none.
func EnvKeysFor(providerID string) []string {
	return append([]string(nil), apiKeyEnvVars[providerID]...)
}
