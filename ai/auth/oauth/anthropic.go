package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// Anthropic OAuth (Claude Pro/Max) constants, ported verbatim from Pi's
// auth/oauth/anthropic.ts. Providers fingerprint these — do not "improve" them.
const (
	anthropicClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	anthropicAuthorizeURL = "https://claude.ai/oauth/authorize"
	anthropicTokenURL     = "https://platform.claude.com/v1/oauth/token"
	anthropicCallbackPort = 53692
	anthropicCallbackPath = "/callback"
	anthropicRedirectURI  = "http://localhost:53692/callback"
	anthropicScopes       = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	// tokenRequestTimeout matches Pi's AbortSignal.timeout(30_000).
	tokenRequestTimeout = 30 * time.Second
	// expiryMargin is subtracted from expires_in when storing (Pi: 5 minutes).
	expiryMargin = 5 * time.Minute
)

func callbackHost() string {
	if v := os.Getenv("TAU_OAUTH_CALLBACK_HOST"); v != "" {
		return v
	}
	// Accept Pi's variable so a migrated setup keeps working.
	if v := os.Getenv("PI_OAUTH_CALLBACK_HOST"); v != "" {
		return v
	}
	return "127.0.0.1"
}

// Anthropic is the Claude Pro/Max OAuth flow.
type Anthropic struct {
	// HTTPClient overrides the client used for token requests (tests).
	HTTPClient *http.Client
	// TokenURL overrides the token endpoint (tests).
	TokenURL string
	// Now overrides the clock (tests).
	Now func() time.Time
	// DisableCallbackServer skips the local callback listener, forcing the
	// manual paste path. Useful on headless hosts and in tests.
	DisableCallbackServer bool
}

// NewAnthropic returns the Anthropic OAuth flow with production settings.
func NewAnthropic() *Anthropic { return &Anthropic{} }

var _ auth.OAuthAuth = (*Anthropic)(nil)

func (a *Anthropic) Name() string { return "Anthropic (Claude Pro/Max)" }

func (a *Anthropic) client() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return http.DefaultClient
}

func (a *Anthropic) tokenURL() string {
	if a.TokenURL != "" {
		return a.TokenURL
	}
	return anthropicTokenURL
}

func (a *Anthropic) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// ToAuth derives request auth: the access token is passed as the API key, and
// the Anthropic wire layer applies OAuth request shaping for it.
func (a *Anthropic) ToAuth(cred *auth.Credential) (auth.ModelAuth, error) {
	if cred == nil || cred.OAuth == nil {
		return auth.ModelAuth{}, errors.New("anthropic oauth: missing credential")
	}
	return auth.ModelAuth{APIKey: cred.OAuth.Access}, nil
}

// Refresh exchanges the refresh token for a new credential.
func (a *Anthropic) Refresh(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Refresh == "" {
		return nil, errors.New("anthropic oauth: missing refresh token")
	}
	body, err := a.postJSON(ctx, map[string]any{
		"grant_type":    "refresh_token",
		"client_id":     anthropicClientID,
		"refresh_token": cred.OAuth.Refresh,
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic token refresh request failed. url=%s: %w", a.tokenURL(), err)
	}
	return a.credentialFromTokenResponse(body, "refresh")
}

// Login runs the interactive authorization-code flow with PKCE. It starts a
// local callback listener and, in parallel, offers a manual paste prompt for
// the redirect URL or code — whichever resolves first wins.
func (a *Anthropic) Login(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("anthropic oauth: %w", err)
	}

	// Pi sends the verifier as the `state` parameter.
	state := pkce.Verifier

	params := url.Values{}
	params.Set("code", "true")
	params.Set("client_id", anthropicClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", anthropicRedirectURI)
	params.Set("scope", anthropicScopes)
	params.Set("code_challenge", pkce.Challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)

	var srv *callbackServer
	if !a.DisableCallbackServer {
		srv, err = startCallbackServer(callbackConfig{
			Port: anthropicCallbackPort, Path: anthropicCallbackPath,
			Provider: "Anthropic", ExpectedState: state,
		})
		if err != nil {
			// A busy port must not block login: fall back to manual paste.
			srv = nil
		}
	}
	if srv != nil {
		defer srv.Close()
	}

	in.Notify(auth.Event{
		Type: auth.EventAuthURL,
		URL:  anthropicAuthorizeURL + "?" + params.Encode(),
		Instructions: "Complete login in your browser. If the browser is on another machine, " +
			"paste the final redirect URL here.",
	})

	promptCtx, cancelPrompt := context.WithCancel(ctx)
	defer cancelPrompt()

	type manualResult struct {
		input string
		err   error
	}
	manualCh := make(chan manualResult, 1)
	go func() {
		input, err := in.Prompt(promptCtx, auth.Prompt{
			Type:        auth.PromptManualCode,
			Message:     "Complete login in your browser, or paste the authorization code / redirect URL here:",
			Placeholder: anthropicRedirectURI,
			Ctx:         promptCtx,
		})
		manualCh <- manualResult{input: input, err: err}
		if srv != nil {
			srv.CancelWait()
		}
	}()

	var code string
	var gotState string

	var serverCh <-chan callbackResult
	if srv != nil {
		serverCh = srv.Wait()
	}

	select {
	case res := <-serverCh:
		if res.Err != nil {
			// The provider said why it refused; repeating that beats falling
			// through to a generic "no code" message.
			return nil, res.Err
		}
		if res.Code != "" {
			code, gotState = res.Code, res.State
		}
	case m := <-manualCh:
		if m.err != nil {
			return nil, m.err
		}
		parsed := parseAuthorizationInput(m.input)
		if parsed.State != "" && parsed.State != state {
			return nil, errors.New("OAuth state mismatch")
		}
		code = parsed.Code
		gotState = parsed.State
		if gotState == "" {
			gotState = state
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if code == "" {
		// The callback resolved empty (cancelled): fall back to the manual input.
		select {
		case m := <-manualCh:
			if m.err != nil {
				return nil, m.err
			}
			parsed := parseAuthorizationInput(m.input)
			if parsed.State != "" && parsed.State != state {
				return nil, errors.New("OAuth state mismatch")
			}
			code = parsed.Code
			gotState = parsed.State
			if gotState == "" {
				gotState = state
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if code == "" {
		return nil, errors.New("missing authorization code")
	}
	if gotState == "" {
		return nil, errors.New("missing OAuth state")
	}

	in.Notify(auth.Event{Type: auth.EventProgress, Message: "Exchanging authorization code for tokens..."})
	return a.exchangeCode(ctx, code, gotState, pkce.Verifier, anthropicRedirectURI)
}

func (a *Anthropic) exchangeCode(ctx context.Context, code, state, verifier, redirectURI string) (*auth.Credential, error) {
	body, err := a.postJSON(ctx, map[string]any{
		"grant_type":    "authorization_code",
		"client_id":     anthropicClientID,
		"code":          code,
		"state":         state,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"token exchange request failed. url=%s; redirect_uri=%s; response_type=authorization_code: %w",
			a.tokenURL(), redirectURI, err)
	}
	return a.credentialFromTokenResponse(body, "exchange")
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope,omitempty"`
}

func (a *Anthropic) credentialFromTokenResponse(body []byte, what string) (*auth.Credential, error) {
	var data tokenResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("token %s returned invalid JSON. url=%s; body=%s: %w",
			what, a.tokenURL(), string(body), err)
	}
	expires := a.now().Add(time.Duration(data.ExpiresIn)*time.Second - expiryMargin).UnixMilli()
	return &auth.Credential{
		Type: auth.CredentialOAuth,
		OAuth: &auth.OAuthData{
			Access:  data.AccessToken,
			Refresh: data.RefreshToken,
			Expires: expires,
		},
	}, nil
}

func (a *Anthropic) postJSON(ctx context.Context, payload map[string]any) ([]byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, a.tokenURL(), bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP request failed. status=%d; url=%s; body=%s",
			resp.StatusCode, a.tokenURL(), string(body))
	}
	return body, nil
}

// authorizationInput is a parsed manual paste.
type authorizationInput struct {
	Code  string
	State string
}

// parseAuthorizationInput accepts a full redirect URL, a `code#state` pair, a
// query fragment containing code=, or a bare code. Ported from Pi.
func parseAuthorizationInput(input string) authorizationInput {
	value := strings.TrimSpace(input)
	if value == "" {
		return authorizationInput{}
	}
	if u, err := url.Parse(value); err == nil && u.Scheme != "" && u.Host != "" {
		q := u.Query()
		return authorizationInput{Code: q.Get("code"), State: q.Get("state")}
	}
	if strings.Contains(value, "#") {
		parts := strings.SplitN(value, "#", 2)
		return authorizationInput{Code: parts[0], State: parts[1]}
	}
	if strings.Contains(value, "code=") {
		q, err := url.ParseQuery(value)
		if err == nil {
			return authorizationInput{Code: q.Get("code"), State: q.Get("state")}
		}
	}
	return authorizationInput{Code: value}
}

// callbackResult is what the local listener captured.

// Access returns a valid access token for the provider, refreshing inside the
// store's Modify lock when it is within the validity window. Concurrent calls
// collapse to a single refresh because Modify serializes per provider.
func Access(ctx context.Context, flow auth.OAuthAuth, store auth.CredentialStore, providerID string) (string, error) {
	res, err := auth.Resolve(ctx, providerID, auth.ProviderAuth{OAuth: flow}, store, auth.OSContext{}, nil)
	if err != nil {
		return "", err
	}
	if res == nil || res.Auth.APIKey == "" {
		return "", fmt.Errorf("oauth: no credential for %s", providerID)
	}
	return res.Auth.APIKey, nil
}

// Login runs a provider's OAuth login and persists the credential.
func Login(ctx context.Context, flow auth.OAuthAuth, store auth.CredentialStore, providerID string, in auth.Interaction) error {
	cred, err := flow.Login(ctx, in)
	if err != nil {
		return err
	}
	_, err = store.Modify(ctx, providerID, func(*auth.Credential) (*auth.Credential, error) {
		return cred, nil
	})
	return err
}
