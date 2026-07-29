package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// Codex is the ChatGPT login: what turns a subscription into API access.
//
// It is a standard PKCE browser flow with one unusual constraint — the
// redirect URI is registered as a fixed port, so the callback server has to
// listen exactly there or the provider refuses to redirect at all. That makes
// the manual-paste fallback more than a nicety: it is the only route when
// something else already holds the port.

const (
	codexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexAuthBase    = "https://auth.openai.com"
	codexRedirectURI = "http://localhost:1455/auth/callback"
	// codexCallbackPort is fixed by the registered redirect URI, not chosen.
	codexCallbackPort = 1455
	codexCallbackPath = "/auth/callback"
	codexScope        = "openid profile email offline_access"
	// codexRefreshMargin is subtracted from a token's lifetime so a request is
	// never sent with a token that expires while it is in flight.
	codexRefreshMargin = 5 * time.Minute
)

// Codex implements the ChatGPT OAuth flow.
type Codex struct {
	// AuthBase overrides the authorization host, for tests.
	AuthBase string
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
	// DisableCallbackServer forces the manual-paste path.
	DisableCallbackServer bool
	// Now overrides the clock, for tests.
	Now func() time.Time
}

func NewCodex() *Codex { return &Codex{} }

func (c *Codex) Name() string { return "OpenAI Codex (ChatGPT)" }

func (c *Codex) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Codex) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Codex) authBase() string {
	if c.AuthBase != "" {
		return strings.TrimRight(c.AuthBase, "/")
	}
	return codexAuthBase
}

func (c *Codex) authorizeURL() string { return c.authBase() + "/oauth/authorize" }
func (c *Codex) tokenURL() string     { return c.authBase() + "/oauth/token" }

// ToAuth turns a stored credential into request auth.
func (c *Codex) ToAuth(cred *auth.Credential) (auth.ModelAuth, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Access == "" {
		return auth.ModelAuth{}, fmt.Errorf("codex: no access token")
	}
	return auth.ModelAuth{APIKey: cred.OAuth.Access}, nil
}

// Refresh exchanges the refresh token for a new pair.
func (c *Codex) Refresh(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Refresh == "" {
		return nil, fmt.Errorf("codex: no refresh token; log in again")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.OAuth.Refresh},
		"client_id":     {codexClientID},
	}
	updated, err := c.exchange(ctx, form, "refresh")
	if err != nil {
		return nil, err
	}
	// A refresh may return no new refresh token, in which case the old one
	// stays valid — discarding it would log the user out at the next refresh.
	if updated.OAuth.Refresh == "" {
		updated.OAuth.Refresh = cred.OAuth.Refresh
	}
	return updated, nil
}

// Login runs the browser flow, falling back to a manual paste.
func (c *Codex) Login(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomState()
	if err != nil {
		return nil, err
	}

	authorize := c.buildAuthorizeURL(pkce.Challenge, state)

	var srv *callbackServer
	if !c.DisableCallbackServer {
		srv, err = startCallbackServer(callbackConfig{
			Port: codexCallbackPort, Path: codexCallbackPath,
			Provider: "ChatGPT", ExpectedState: state,
		})
		if err != nil {
			// The port is fixed by the registered redirect URI, so a busy one
			// cannot be worked around — but it must not block login either.
			srv = nil
		}
	}
	if srv != nil {
		defer srv.Close()
	}

	if in != nil {
		in.Notify(auth.Event{
			Type:    auth.EventAuthURL,
			Message: "Open this URL to authorize tau with your ChatGPT account:",
			URL:     authorize,
			Instructions: "After approving, you will be redirected back. " +
				"If the redirect does not complete, paste the URL you land on here.",
		})
	}

	code, err := c.awaitCode(ctx, srv, in, state)
	if err != nil {
		return nil, err
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexClientID},
		"code":          {code},
		"code_verifier": {pkce.Verifier},
		"redirect_uri":  {codexRedirectURI},
	}
	return c.exchange(ctx, form, "exchange")
}

// buildAuthorizeURL assembles the authorization request.
//
// The parameter set is ported verbatim, extra flags included: providers
// fingerprint these, and an authorization that omits one is answered
// differently or refused.
func (c *Codex) buildAuthorizeURL(challenge, state string) string {
	q := url.Values{
		"response_type":              {"code"},
		"client_id":                  {codexClientID},
		"redirect_uri":               {codexRedirectURI},
		"scope":                      {codexScope},
		"code_challenge":             {challenge},
		"code_challenge_method":      {"S256"},
		"state":                      {state},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"originator":                 {"tau"},
	}
	return c.authorizeURL() + "?" + q.Encode()
}

// awaitCode takes the code from whichever route produces it first.
func (c *Codex) awaitCode(ctx context.Context, srv *callbackServer, in auth.Interaction, state string) (string, error) {
	if srv == nil {
		return c.promptForCode(ctx, in, state)
	}

	type pasted struct {
		code string
		err  error
	}
	manual := make(chan pasted, 1)
	go func() {
		code, err := c.promptForCode(ctx, in, state)
		manual <- pasted{code, err}
	}()

	select {
	case res := <-srv.Wait():
		if res.Code == "" {
			return "", fmt.Errorf("codex: the browser redirect did not carry an authorization code")
		}
		return res.Code, nil
	case res := <-manual:
		srv.CancelWait()
		return res.code, res.err
	case <-ctx.Done():
		srv.CancelWait()
		return "", ErrLoginCancelled
	}
}

// promptForCode accepts either the bare code or the whole redirect URL, since
// what the user has to hand is usually the address bar.
func (c *Codex) promptForCode(ctx context.Context, in auth.Interaction, state string) (string, error) {
	if in == nil {
		return "", fmt.Errorf("codex: no way to collect the authorization code")
	}
	entered, err := in.Prompt(ctx, auth.Prompt{
		Message: "Paste the authorization code or the full redirect URL:",
	})
	if err != nil {
		return "", err
	}

	code, gotState := parseCodexRedirect(entered)
	if code == "" {
		return "", fmt.Errorf("codex: no authorization code in what was pasted")
	}
	// A pasted URL carries the state, and a mismatch means the code belongs to
	// a different login attempt.
	if gotState != "" && gotState != state {
		return "", fmt.Errorf("codex: the pasted code is from a different login attempt")
	}
	return code, nil
}

// parseCodexRedirect pulls the code out of a bare code, a "code#state" pair, or
// a full redirect URL.
func parseCodexRedirect(input string) (code, state string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", ""
	}

	if strings.Contains(trimmed, "://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return "", ""
		}
		q := u.Query()
		return q.Get("code"), q.Get("state")
	}
	if before, after, found := strings.Cut(trimmed, "#"); found {
		return before, after
	}
	return trimmed, ""
}

// exchange posts a token request and reads the credential out of it.
func (c *Codex) exchange(ctx context.Context, form url.Values, what string) (*auth.Credential, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex %s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("codex %s: unreadable response (%s)", what, resp.Status)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || body.Error != "" {
		detail := body.ErrorDesc
		if detail == "" {
			detail = body.Error
		}
		if detail == "" {
			detail = resp.Status
		}
		return nil, fmt.Errorf("codex %s failed: %s", what, detail)
	}
	if body.AccessToken == "" {
		return nil, fmt.Errorf("codex %s returned no access token", what)
	}

	// The margin is subtracted here rather than at use, so every consumer of
	// the stored deadline gets the same safety without having to know about it.
	expires := c.now().Add(time.Duration(body.ExpiresIn)*time.Second - codexRefreshMargin)
	return &auth.Credential{
		Type: auth.CredentialOAuth,
		OAuth: &auth.OAuthData{
			Access:  body.AccessToken,
			Refresh: body.RefreshToken,
			Expires: expires.UnixMilli(),
		},
	}, nil
}
