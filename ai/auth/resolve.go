package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrorCode classifies auth failures, mirroring Pi's ModelsErrorCode subset.
type ErrorCode string

const (
	CodeAuth  ErrorCode = "auth"
	CodeOAuth ErrorCode = "oauth"
)

// Error is an auth resolution failure.
type Error struct {
	Code    ErrorCode
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

// DefaultOAuthMinValidity is the remaining-validity window that triggers a
// refresh (Pi: five minutes).
const DefaultOAuthMinValidity = 5 * time.Minute

// Overrides adjust a single resolution.
type Overrides struct {
	// APIKey short-circuits to api-key auth with this key.
	APIKey string
	// HasAPIKey distinguishes an explicitly-empty APIKey from an unset one.
	HasAPIKey bool
	// Env overlays provider-scoped config values.
	Env ProviderEnv
	// MinOAuthValidity requires this much remaining token life. Values below
	// DefaultOAuthMinValidity are raised to it. When set, resolution errors if
	// a refreshed token still expires too soon.
	MinOAuthValidity time.Duration
}

// nowMS is indirected for tests.
var nowMS = func() int64 { return time.Now().UnixMilli() }

// Resolve resolves auth for a provider.
//
// A stored credential owns the provider: ambient/env sources are consulted
// only when nothing is stored. There is no silent env fallback after a failed
// refresh, nor for a credential type with no matching handler. A nil result
// with a nil error means "not configured" (Pi returns undefined).
func Resolve(
	ctx context.Context,
	providerID string,
	providerAuth ProviderAuth,
	store CredentialStore,
	env EnvContext,
	overrides *Overrides,
) (*AuthResult, error) {
	if overrides == nil {
		overrides = &Overrides{}
	}
	requestEnv := WithEnvOverlay(env, overrides.Env)

	if overrides.HasAPIKey && providerAuth.APIKey != nil {
		return resolveAPIKey(requestEnv, providerAuth.APIKey, providerID, &Credential{
			Type: CredentialAPIKey,
			Key:  overrides.APIKey,
			Env:  overrides.Env,
		})
	}

	stored, err := store.Read(ctx, providerID)
	if err != nil {
		return nil, &Error{Code: CodeAuth, Message: "Credential store read failed for " + providerID, Cause: err}
	}

	if stored != nil {
		switch {
		case stored.Type == CredentialOAuth && providerAuth.OAuth != nil:
			return resolveStoredOAuth(ctx, store, providerID, providerAuth.OAuth, stored, overrides.MinOAuthValidity)
		case stored.Type == CredentialAPIKey && providerAuth.APIKey != nil:
			cred := stored
			if len(overrides.Env) > 0 {
				merged := stored.Clone()
				if merged.Env == nil {
					merged.Env = ProviderEnv{}
				}
				for k, v := range overrides.Env {
					merged.Env[k] = v
				}
				cred = &merged
			}
			return resolveAPIKey(requestEnv, providerAuth.APIKey, providerID, cred)
		default:
			// Stored credential with no matching handler: no env fallback.
			return nil, nil
		}
	}

	if providerAuth.APIKey != nil {
		return resolveAPIKey(requestEnv, providerAuth.APIKey, providerID, nil)
	}
	return nil, nil
}

// resolveStoredOAuth applies double-checked locking: a token inside the
// validity window takes the store lock, re-checks expiry under it, refreshes
// once globally, and persists the rotated credential before releasing.
func resolveStoredOAuth(
	ctx context.Context,
	store CredentialStore,
	providerID string,
	oauth OAuthAuth,
	stored *Credential,
	minValidity time.Duration,
) (*AuthResult, error) {
	minimum := DefaultOAuthMinValidity
	if minValidity > minimum {
		minimum = minValidity
	}
	minMS := minimum.Milliseconds()
	expiresSoon := func(c *Credential) bool {
		return c.OAuth == nil || c.OAuth.ExpiresSoon(nowMS(), minMS)
	}

	cred := stored
	if expiresSoon(cred) {
		var refreshErr error
		post, err := store.Modify(ctx, providerID, func(current *Credential) (*Credential, error) {
			if current == nil || current.Type != CredentialOAuth {
				return nil, nil // logged out meanwhile
			}
			if !expiresSoon(current) {
				return nil, nil // another process/request refreshed
			}
			next, err := oauth.Refresh(ctx, current)
			if err != nil {
				refreshErr = &Error{Code: CodeOAuth, Message: "OAuth refresh failed for " + providerID, Cause: err}
				return nil, refreshErr
			}
			return next, nil
		})
		if err != nil {
			if refreshErr != nil && errors.Is(err, refreshErr) {
				return nil, refreshErr
			}
			var authErr *Error
			if errors.As(err, &authErr) {
				return nil, authErr
			}
			return nil, &Error{Code: CodeAuth, Message: "Credential store modify failed for " + providerID, Cause: err}
		}
		if post == nil || post.Type != CredentialOAuth {
			return nil, nil // logged out meanwhile
		}
		cred = post
		// The default five-minute window triggers a refresh but imposes no
		// provider contract. Explicit callers (bearer-token export) do require
		// the requested minimum after refreshing.
		if minValidity > 0 && expiresSoon(cred) {
			return nil, &Error{
				Code:    CodeOAuth,
				Message: "OAuth refresh returned a token that expires too soon for " + providerID,
			}
		}
	}

	auth, err := oauth.ToAuth(cred)
	if err != nil {
		return nil, &Error{Code: CodeOAuth, Message: "OAuth auth derivation failed for " + providerID, Cause: err}
	}
	return &AuthResult{Auth: auth, Source: "OAuth"}, nil
}

func resolveAPIKey(env EnvContext, apiKey *APIKeyAuth, providerID string, cred *Credential) (*AuthResult, error) {
	if apiKey.Resolve == nil {
		return nil, nil
	}
	res, err := apiKey.Resolve(env, cred)
	if err != nil {
		return nil, &Error{Code: CodeAuth, Message: "API key auth failed for provider " + providerID, Cause: err}
	}
	return res, nil
}
