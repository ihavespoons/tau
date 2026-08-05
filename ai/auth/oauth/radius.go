package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// Radius is a gateway rather than a single vendor, so this flow is a factory:
// the same protocol points at whichever deployment the caller names.
//
// It is also the only login that offers the user a choice. A browser flow needs
// a machine with a browser on it; a device code is what works when tau is
// running over SSH or in a container. Both end at the same token endpoint.

const (
	radiusClientID    = "pi-gateway"
	radiusScope       = "gateway offline_access"
	radiusCallback    = 1456
	radiusPath        = "/oauth/callback"
	radiusSkew        = time.Minute
	radiusDeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
	// radiusLoginTimeout bounds the browser flow. Pi has none; a login that
	// waits forever is indistinguishable from a hang.
	radiusLoginTimeout = 5 * time.Minute
)

const (
	radiusMethodBrowser    = "browser"
	radiusMethodDeviceCode = "device-code"
)

// Radius implements the Radius gateway OAuth flow.
type Radius struct {
	// ProviderName is how the gateway is described to the user.
	ProviderName string
	// Gateway is the gateway base URL.
	Gateway string
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
	// DisableCallbackServer forces the device-code path.
	DisableCallbackServer bool
	// LoginTimeout overrides how long the browser callback is awaited.
	LoginTimeout time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
	// Sleep overrides the poll wait, for tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewRadius builds a flow for one gateway.
func NewRadius(name, gateway string) *Radius {
	return &Radius{ProviderName: name, Gateway: normalizeGateway(gateway)}
}

// normalizeGateway trims a trailing slash so joining paths cannot double it.
func normalizeGateway(gateway string) string { return strings.TrimRight(gateway, "/") }

func (r *Radius) Name() string {
	if r.ProviderName != "" {
		return r.ProviderName
	}
	return "Radius"
}

func (r *Radius) client() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *Radius) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Radius) endpoint(path string) string { return normalizeGateway(r.Gateway) + path }

func (r *Radius) redirectURI() string {
	return fmt.Sprintf("http://%s:%d%s", callbackHost(), radiusCallback, radiusPath)
}

// ToAuth turns a stored credential into request auth.
func (r *Radius) ToAuth(cred *auth.Credential) (auth.ModelAuth, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Access == "" {
		return auth.ModelAuth{}, fmt.Errorf("radius: no access token")
	}
	return auth.ModelAuth{APIKey: cred.OAuth.Access}, nil
}

// Refresh exchanges the refresh token for a new pair.
func (r *Radius) Refresh(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Refresh == "" {
		return nil, fmt.Errorf("radius: no refresh token; log in again")
	}
	return r.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {radiusClientID},
		"refresh_token": {cred.OAuth.Refresh},
	})
}

// Login asks how the user wants to sign in, then runs that flow.
func (r *Radius) Login(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	method, err := r.chooseMethod(ctx, in)
	if err != nil {
		return nil, err
	}
	switch method {
	case radiusMethodDeviceCode:
		return r.loginWithDeviceCode(ctx, in)
	case radiusMethodBrowser:
		return r.loginWithBrowser(ctx, in)
	default:
		return nil, fmt.Errorf("radius: unknown %s sign-in method %q", r.Name(), method)
	}
}

// chooseMethod asks the user, defaulting to the device code when there is
// nobody to ask or no browser to open.
func (r *Radius) chooseMethod(ctx context.Context, in auth.Interaction) (string, error) {
	if in == nil || r.DisableCallbackServer {
		// The device code is the safe default: it needs nothing local, where
		// the browser flow needs a specific port on this machine.
		return radiusMethodDeviceCode, nil
	}
	choice, err := in.Prompt(ctx, auth.Prompt{
		Type:    auth.PromptSelect,
		Message: "Sign in to " + r.Name() + ":",
		Options: []auth.PromptOption{
			{ID: radiusMethodBrowser, Label: "Sign in with browser (recommended)"},
			{ID: radiusMethodDeviceCode, Label: "Sign in with device code (when signing in from another device)"},
		},
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(choice) == "" {
		return radiusMethodBrowser, nil
	}
	return choice, nil
}

// loginWithBrowser runs the PKCE flow against the discovered endpoint.
func (r *Radius) loginWithBrowser(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	// The authorization endpoint is discovered rather than hard-coded, because
	// a gateway deployment chooses its own identity provider.
	authorizeEndpoint, err := r.discover(ctx)
	if err != nil {
		return nil, err
	}

	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomState()
	if err != nil {
		return nil, err
	}

	srv, err := startCallbackServer(callbackConfig{
		Port: radiusCallback, Path: radiusPath,
		Provider: r.Name(), ExpectedState: state,
	})
	if err != nil {
		return nil, fmt.Errorf(
			"radius: could not listen on port %d for the OAuth callback: %w — sign in with a device code instead",
			radiusCallback, err)
	}
	defer srv.Close()

	if in != nil {
		in.Notify(auth.Event{
			Type:    auth.EventProgress,
			Message: "Listening for the OAuth callback on " + r.redirectURI(),
		})
		in.Notify(auth.Event{
			Type:         auth.EventAuthURL,
			Message:      "Open this URL to authorize tau with " + r.Name() + ":",
			URL:          r.buildAuthorizeURL(authorizeEndpoint, pkce.Challenge, state),
			Instructions: "Continue in your browser.",
		})
	}

	// A browser flow that is never completed must not wait forever. Without a
	// deadline `tau login` sits there with nothing to show and no way out but
	// Ctrl-C, which is indistinguishable from a hang — and the user has a
	// working alternative in the device code, so it is worth telling them.
	timeout := time.NewTimer(r.loginTimeout())
	defer timeout.Stop()

	select {
	case res := <-srv.Wait():
		if res.Err != nil {
			return nil, res.Err
		}
		if res.Code == "" {
			return nil, fmt.Errorf("radius: the OAuth callback did not complete")
		}
		return r.requestToken(ctx, url.Values{
			"grant_type":    {"authorization_code"},
			"client_id":     {radiusClientID},
			"redirect_uri":  {r.redirectURI()},
			"code":          {res.Code},
			"code_verifier": {pkce.Verifier},
		})
	case <-ctx.Done():
		srv.CancelWait()
		return nil, ErrLoginCancelled
	case <-timeout.C:
		srv.CancelWait()
		return nil, fmt.Errorf(
			"radius: the browser sign-in was not completed within %s — sign in with a device code instead",
			r.loginTimeout())
	}
}

func (r *Radius) loginTimeout() time.Duration {
	if r.LoginTimeout > 0 {
		return r.LoginTimeout
	}
	return radiusLoginTimeout
}

// buildAuthorizeURL assembles the authorization request.
func (r *Radius) buildAuthorizeURL(endpoint, challenge, state string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {radiusClientID},
		"redirect_uri":          {r.redirectURI()},
		"scope":                 {radiusScope},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		// handoff tells the gateway to redirect rather than display a code;
		// ported verbatim because the response shape depends on it.
		"handoff": {"url"},
		"state":   {state},
	}
	separator := "?"
	if strings.Contains(endpoint, "?") {
		separator = "&"
	}
	return endpoint + separator + q.Encode()
}

// discover reads the gateway's authorization endpoint.
func (r *Radius) discover(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint("/v1/oauth"), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("could not load the %s OAuth config from %s: %w", r.Name(), r.Gateway, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("could not load the %s OAuth config from %s: %d %s",
			r.Name(), r.Gateway, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var discovery struct {
		AuthorizationEndpoint string `json:"authorizationEndpoint"`
	}
	if err := json.Unmarshal(body, &discovery); err != nil || discovery.AuthorizationEndpoint == "" {
		return "", fmt.Errorf("invalid %s OAuth config from %s", r.Name(), r.Gateway)
	}
	// The endpoint is opened in the user's browser, so it gets the same
	// scrutiny as a device flow's verification URI.
	return validateVerificationURI("radius", discovery.AuthorizationEndpoint, true)
}

// loginWithDeviceCode runs the device flow.
func (r *Radius) loginWithDeviceCode(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	device, err := r.requestDeviceAuthorization(ctx)
	if err != nil {
		return nil, err
	}

	if in != nil {
		in.Notify(auth.Event{
			Type:             auth.EventDeviceCode,
			Message:          fmt.Sprintf("Open %s and enter the code: %s", device.VerificationURI, device.UserCode),
			URL:              device.VerificationURI,
			VerificationURI:  device.VerificationURI,
			UserCode:         device.UserCode,
			IntervalSeconds:  int(device.Interval),
			ExpiresInSeconds: int(device.ExpiresIn),
		})
	}

	return PollDeviceCode(ctx, DeviceCodeOptions[*auth.Credential]{
		IntervalSeconds:  device.Interval,
		ExpiresInSeconds: device.ExpiresIn,
		Sleep:            r.Sleep,
		Poll: func(ctx context.Context) (PollResult[*auth.Credential], error) {
			cred, err := r.requestToken(ctx, url.Values{
				"grant_type":  {radiusDeviceGrant},
				"client_id":   {radiusClientID},
				"device_code": {device.DeviceCode},
			})
			if err == nil {
				return PollResult[*auth.Credential]{Status: PollComplete, Value: cred}, nil
			}

			var oauthErr *oauthErrorBody
			if !asOAuthError(err, &oauthErr) {
				return PollResult[*auth.Credential]{}, err
			}
			switch oauthErr.Code {
			case "authorization_pending":
				return PollResult[*auth.Credential]{Status: PollPending}, nil
			case "slow_down":
				return PollResult[*auth.Credential]{Status: PollSlowDown, Interval: oauthErr.Interval}, nil
			case "expired_token":
				return PollResult[*auth.Credential]{Status: PollFailed, Message: "Device authorization expired."}, nil
			case "access_denied":
				return PollResult[*auth.Credential]{Status: PollFailed, Message: "Device authorization was denied."}, nil
			default:
				// An unrecognised error is not a poll state. Returning it stops
				// the loop instead of retrying something that will never change.
				return PollResult[*auth.Credential]{}, err
			}
		},
	})
}

type radiusDeviceCode struct {
	DeviceCode      string  `json:"device_code"`
	UserCode        string  `json:"user_code"`
	VerificationURI string  `json:"verification_uri"`
	Interval        float64 `json:"interval"`
	ExpiresIn       float64 `json:"expires_in"`
}

func (r *Radius) requestDeviceAuthorization(ctx context.Context) (*radiusDeviceCode, error) {
	form := url.Values{"client_id": {radiusClientID}, "scope": {radiusScope}}
	body, err := r.postForm(ctx, r.endpoint("/v1/oauth/device"), form)
	if err != nil {
		return nil, fmt.Errorf("%s OAuth device authorization failed: %w", r.Name(), err)
	}

	var device radiusDeviceCode
	if err := json.Unmarshal(body, &device); err != nil {
		return nil, fmt.Errorf("radius: unreadable device authorization response")
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" || device.ExpiresIn <= 0 {
		return nil, fmt.Errorf("%s OAuth device authorization response is missing required fields", r.Name())
	}

	// A self-hosted gateway may be reached over http on an internal network.
	verification, err := validateVerificationURI("radius", device.VerificationURI, true)
	if err != nil {
		return nil, err
	}
	device.VerificationURI = verification
	return &device, nil
}

// requestToken posts to the gateway's token endpoint.
func (r *Radius) requestToken(ctx context.Context, form url.Values) (*auth.Credential, error) {
	body, err := r.postForm(ctx, r.endpoint("/v1/oauth/token"), form)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
		Scope        string  `json:"scope"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("radius: unreadable token response")
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("radius: token response carries no access token")
	}

	data := &auth.OAuthData{
		Access:  parsed.AccessToken,
		Refresh: parsed.RefreshToken,
		Expires: r.now().Add(time.Duration(parsed.ExpiresIn*float64(time.Second)) - radiusSkew).UnixMilli(),
	}
	if parsed.Scope != "" {
		// Extra is flattened into the credential file's top level, so this
		// lands as the same `scope` key Pi writes.
		encoded, err := json.Marshal(parsed.Scope)
		if err != nil {
			return nil, err
		}
		data.Extra = map[string]json.RawMessage{"scope": encoded}
	}
	return &auth.Credential{Type: auth.CredentialOAuth, OAuth: data}, nil
}

func (r *Radius) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := r.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrLoginCancelled
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newOAuthError(resp.StatusCode, body)
	}
	return body, nil
}
