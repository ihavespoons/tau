// Package auth implements provider credential storage and auth resolution —
// the tau port of Pi's packages/ai/src/auth (snapshot v0.82.1) plus the
// file-backed store from coding-agent's auth-storage.ts. The on-disk shape is
// byte-compatible with Pi's ~/.pi/agent/auth.json so tau can read it directly.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
)

// ProviderEnv holds provider-scoped environment/config values (e.g. Cloudflare
// account ids). Values take precedence over process env.
type ProviderEnv map[string]string

// ModelAuth is the request auth for a single model call. Anything that cannot
// be expressed as APIKey, Headers, or BaseURL is provider config, not auth.
//
// A nil value in Headers suppresses a provider default header of that name
// (Pi's `null`).
type ModelAuth struct {
	APIKey  string             `json:"apiKey,omitempty"`
	Headers map[string]*string `json:"headers,omitempty"`
	BaseURL string             `json:"baseUrl,omitempty"`
}

// CredentialType is the stored credential discriminator.
type CredentialType string

const (
	CredentialAPIKey CredentialType = "api_key"
	CredentialOAuth  CredentialType = "oauth"
)

// Credential is one type-tagged credential per provider — the shape of
// auth.json. Exactly one of APIKey/OAuth is set, per Type.
type Credential struct {
	Type  CredentialType
	Key   string      // api_key: the key, or a $ENV / !command reference
	Env   ProviderEnv // api_key: provider-scoped env values
	OAuth *OAuthData  // oauth
}

// OAuthData is a stored OAuth credential. Extra preserves any additional
// fields Pi (or an extension flow) wrote, so a round-trip is lossless.
type OAuthData struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	// Expires is a unix-millisecond deadline that already has the provider
	// flow's safety margin subtracted (Anthropic: 5 minutes).
	Expires int64
	Extra   map[string]json.RawMessage
}

// ExpiresSoon reports whether the token has less than minValidity ms of life
// left as of nowMS.
func (o *OAuthData) ExpiresSoon(nowMS, minValidityMS int64) bool {
	return nowMS+minValidityMS >= o.Expires
}

func (c Credential) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": string(c.Type)}
	switch c.Type {
	case CredentialOAuth:
		if c.OAuth == nil {
			return nil, fmt.Errorf("auth: oauth credential has no data")
		}
		for k, v := range c.OAuth.Extra {
			out[k] = v
		}
		out["access"] = c.OAuth.Access
		out["refresh"] = c.OAuth.Refresh
		out["expires"] = c.OAuth.Expires
	default:
		if c.Key != "" {
			out["key"] = c.Key
		}
		if len(c.Env) > 0 {
			out["env"] = c.Env
		}
	}
	return json.Marshal(out)
}

func (c *Credential) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var typ string
	if v, ok := raw["type"]; ok {
		if err := json.Unmarshal(v, &typ); err != nil {
			return err
		}
	}
	switch CredentialType(typ) {
	case CredentialOAuth:
		o := &OAuthData{Extra: map[string]json.RawMessage{}}
		if v, ok := raw["access"]; ok {
			if err := json.Unmarshal(v, &o.Access); err != nil {
				return err
			}
		}
		if v, ok := raw["refresh"]; ok {
			if err := json.Unmarshal(v, &o.Refresh); err != nil {
				return err
			}
		}
		if v, ok := raw["expires"]; ok {
			if err := json.Unmarshal(v, &o.Expires); err != nil {
				return err
			}
		}
		for k, v := range raw {
			switch k {
			case "type", "access", "refresh", "expires":
			default:
				o.Extra[k] = v
			}
		}
		if len(o.Extra) == 0 {
			o.Extra = nil
		}
		*c = Credential{Type: CredentialOAuth, OAuth: o}
	case CredentialAPIKey:
		out := Credential{Type: CredentialAPIKey}
		if v, ok := raw["key"]; ok {
			if err := json.Unmarshal(v, &out.Key); err != nil {
				return err
			}
		}
		if v, ok := raw["env"]; ok {
			if err := json.Unmarshal(v, &out.Env); err != nil {
				return err
			}
		}
		*c = out
	default:
		return fmt.Errorf("auth: unknown credential type %q", typ)
	}
	return nil
}

// Clone returns a deep copy.
func (c Credential) Clone() Credential {
	out := c
	if c.Env != nil {
		out.Env = maps.Clone(c.Env)
	}
	if c.OAuth != nil {
		o := *c.OAuth
		if c.OAuth.Extra != nil {
			o.Extra = maps.Clone(c.OAuth.Extra)
		}
		out.OAuth = &o
	}
	return out
}

// CredentialInfo is non-secret credential metadata for status enumeration.
type CredentialInfo struct {
	ProviderID string         `json:"providerId"`
	Type       CredentialType `json:"type"`
}

// AuthResult is the outcome of resolving auth for a provider.
type AuthResult struct {
	Auth ModelAuth `json:"auth"`
	// Env are provider-scoped config values resolved from the credential and
	// ambient context.
	Env ProviderEnv `json:"env,omitempty"`
	// Source is a human-readable label for status UI: "ANTHROPIC_API_KEY",
	// "OAuth", "stored credential".
	Source string `json:"source,omitempty"`
}

// AuthCheck is a side-effect-free availability report.
type AuthCheck struct {
	Source string         `json:"source,omitempty"`
	Type   CredentialType `json:"type"`
}

// PromptType enumerates login prompt kinds.
type PromptType string

const (
	PromptText       PromptType = "text"
	PromptSecret     PromptType = "secret"
	PromptSelect     PromptType = "select"
	PromptManualCode PromptType = "manual_code"
)

// PromptOption is one choice of a select prompt.
type PromptOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Prompt is a request for user input during login. Ctx, when non-nil, lets the
// flow cancel this individual prompt when an out-of-band event resolves the
// step (Pi's per-prompt AbortSignal) — e.g. a manual_code prompt raced against
// a callback server.
type Prompt struct {
	Type        PromptType
	Message     string
	Placeholder string
	Options     []PromptOption
	Ctx         context.Context
}

// EventType enumerates login notification kinds.
type EventType string

const (
	EventInfo       EventType = "info"
	EventAuthURL    EventType = "auth_url"
	EventDeviceCode EventType = "device_code"
	EventProgress   EventType = "progress"
)

// InfoLink is a labeled URL attached to an info event.
type InfoLink struct {
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

// Event is a login progress notification.
type Event struct {
	Type    EventType
	Message string
	Links   []InfoLink
	// auth_url
	URL          string
	Instructions string
	// device_code
	UserCode         string
	VerificationURI  string
	IntervalSeconds  int
	ExpiresInSeconds int
}

// Interaction supplies the login UX for both api-key and OAuth flows.
// Prompt returns the entered/selected value (a select returns the option id)
// and errors on cancel. Notify is fire-and-forget.
type Interaction interface {
	Prompt(ctx context.Context, p Prompt) (string, error)
	Notify(ev Event)
}

// EnvContext is environment access for auth resolution, injectable for tests.
type EnvContext interface {
	Env(name string) string
	FileExists(path string) bool
}

// APIKeyAuth is api-key auth: a stored key/provider env plus ambient sources
// (env vars, AWS profiles, ADC files). Ambient-only providers have no Login.
type APIKeyAuth struct {
	// Name is the display name, e.g. "Anthropic API key".
	Name string
	// Login prompts for a key interactively. Nil means ambient-only.
	Login func(ctx context.Context, in Interaction) (*Credential, error)
	// Check is an optional side-effect-free availability probe.
	Check func(ac EnvContext, cred *Credential) *AuthCheck
	// Resolve merges the stored credential with ambient sources per field.
	// A nil result means "not configured" (Pi's undefined).
	Resolve func(ac EnvContext, cred *Credential) (*AuthResult, error)
}

// OAuthAuth is OAuth auth. The Refresh/ToAuth split lets resolution own the
// locked-refresh pattern: Refresh produces a credential, ToAuth derives
// request auth from whatever credential ends up stored.
type OAuthAuth interface {
	// Name is the display name, e.g. "Anthropic (Claude Pro/Max)".
	Name() string
	// Login performs the interactive flow and returns a fresh credential.
	Login(ctx context.Context, in Interaction) (*Credential, error)
	// Refresh exchanges the refresh token. Network call; errors on failure
	// (invalid_grant etc.). Callers run it under the store lock.
	Refresh(ctx context.Context, cred *Credential) (*Credential, error)
	// ToAuth derives request auth from a valid credential, side-effect free.
	ToAuth(cred *Credential) (ModelAuth, error)
}

// ProviderAuth is a provider's auth capability. At least one field must be
// set: even ambient-credential providers and keyless local servers provide
// APIKey auth whose Resolve reports whether the provider is configured.
type ProviderAuth struct {
	APIKey *APIKeyAuth
	OAuth  OAuthAuth
}
