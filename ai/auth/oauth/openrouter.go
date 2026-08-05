package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ihavespoons/tau/ai/auth"
)

// OpenRouter's login is the odd one out: the exchange returns a permanent,
// user-controlled API key rather than an expiring token pair. There is nothing
// to refresh and nothing to expire, so the credential is stored as OAuth only
// because that is where a login's result lives.
//
// It is also the only flow that chooses its own callback: the URL is a request
// parameter rather than something pre-registered, so the server takes an
// ephemeral port and a fresh random path each time. That path is unguessable
// and single-use, which is what makes a separate state parameter unnecessary.

// neverExpires is JavaScript's Number.MAX_SAFE_INTEGER, which is what Pi
// records for a credential that has no expiry.
const neverExpires int64 = 1<<53 - 1

const (
	openRouterAuthorizeURL = "https://openrouter.ai/auth"
	openRouterKeysURL      = "https://openrouter.ai/api/v1/auth/keys"
	openRouterLoginTimeout = 5 * time.Minute
)

// OpenRouter implements the OpenRouter PKCE flow.
type OpenRouter struct {
	// AuthorizeURL overrides the authorization page, for tests.
	AuthorizeURL string
	// KeysURL overrides the exchange endpoint, for tests.
	KeysURL string
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
	// LoginTimeout overrides how long the callback is awaited.
	LoginTimeout time.Duration
}

func NewOpenRouter() *OpenRouter { return &OpenRouter{} }

func (o *OpenRouter) Name() string { return "OpenRouter" }

func (o *OpenRouter) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (o *OpenRouter) authorizeURL() string {
	if o.AuthorizeURL != "" {
		return o.AuthorizeURL
	}
	return openRouterAuthorizeURL
}

func (o *OpenRouter) keysURL() string {
	if o.KeysURL != "" {
		return o.KeysURL
	}
	return openRouterKeysURL
}

func (o *OpenRouter) timeout() time.Duration {
	if o.LoginTimeout > 0 {
		return o.LoginTimeout
	}
	return openRouterLoginTimeout
}

// ToAuth turns a stored credential into request auth.
func (o *OpenRouter) ToAuth(cred *auth.Credential) (auth.ModelAuth, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Access == "" {
		return auth.ModelAuth{}, fmt.Errorf("openrouter: no API key")
	}
	return auth.ModelAuth{APIKey: cred.OAuth.Access}, nil
}

// Refresh returns the credential unchanged.
//
// The key OpenRouter issues does not expire and cannot be rotated by this flow;
// the user revokes it from their dashboard. Returning it as-is is the honest
// implementation, not a stub.
func (o *OpenRouter) Refresh(_ context.Context, cred *auth.Credential) (*auth.Credential, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Access == "" {
		return nil, fmt.Errorf("openrouter: no stored key; log in again")
	}
	return cred, nil
}

// Login runs the PKCE flow and stores the key it yields.
func (o *OpenRouter) Login(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}

	// A fresh random path per login. Nothing else guards this endpoint, so a
	// fixed one would let any page the user visits afterwards replay a code at
	// it while the server is still listening.
	callbackPath := "/oauth/callback/" + uuid.NewString()

	var key string
	srv, err := startCallbackServer(callbackConfig{
		Port: 0, Path: callbackPath, Provider: "OpenRouter",
		Exchange: func(ctx context.Context, code string) error {
			exchanged, err := o.exchange(ctx, code, pkce.Verifier)
			if err != nil {
				return err
			}
			key = exchanged
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("openrouter: could not listen for the OAuth callback: %w", err)
	}
	defer srv.Close()

	callbackURL := srv.URL(callbackPath)
	if callbackURL == "" {
		return nil, fmt.Errorf("openrouter: could not determine the OAuth callback port")
	}

	if in != nil {
		in.Notify(auth.Event{
			Type:    auth.EventProgress,
			Message: "Listening for the OpenRouter callback on " + callbackURL,
		})
		in.Notify(auth.Event{
			Type:         auth.EventAuthURL,
			Message:      "Open this URL to authorize tau with your OpenRouter account:",
			URL:          o.buildAuthorizeURL(callbackURL, pkce.Challenge),
			Instructions: "Complete sign-in in your browser.",
		})
	}

	timeout := time.NewTimer(o.timeout())
	defer timeout.Stop()

	select {
	case res := <-srv.Wait():
		if res.Err != nil {
			return nil, res.Err
		}
		if key == "" {
			return nil, fmt.Errorf("openrouter: the callback completed without a key")
		}
	case <-ctx.Done():
		return nil, ErrLoginCancelled
	case <-timeout.C:
		return nil, fmt.Errorf("openrouter: login timed out after %s", o.timeout())
	}

	return &auth.Credential{
		Type: auth.CredentialOAuth,
		OAuth: &auth.OAuthData{
			Access: key,
			// No refresh token exists and the key does not expire, so the
			// deadline is the one Pi writes. Matching it keeps the credential
			// file readable by both tools, and it is deliberately not MaxInt64:
			// anything that adds a margin to it would overflow.
			Expires: neverExpires,
		},
	}, nil
}

// buildAuthorizeURL assembles the authorization request. OpenRouter takes the
// callback as a parameter rather than matching a registered redirect URI.
func (o *OpenRouter) buildAuthorizeURL(callbackURL, challenge string) string {
	q := url.Values{
		"callback_url":          {callbackURL},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return o.authorizeURL() + "?" + q.Encode()
}

// exchange trades the authorization code for a permanent API key.
func (o *OpenRouter) exchange(ctx context.Context, code, verifier string) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"code":                  code,
		"code_verifier":         verifier,
		"code_challenge_method": "S256",
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.keysURL(), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := o.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter key exchange failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("openrouter key exchange failed (HTTP %d): %s",
			resp.StatusCode, newOAuthError(resp.StatusCode, body).detail())
	}

	var parsed struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("openrouter key exchange returned invalid JSON")
	}
	if strings.TrimSpace(parsed.Key) == "" {
		return "", fmt.Errorf("openrouter key exchange response carries no key")
	}
	return parsed.Key, nil
}
