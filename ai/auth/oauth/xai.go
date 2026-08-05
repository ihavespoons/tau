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

// XAI is the SuperGrok / X Premium login: a plain RFC 8628 device flow, which
// is the simplest shape a terminal login can take — no callback server, no
// port to collide with, nothing to paste back.

const (
	xaiClientID      = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiScope         = "openid profile email offline_access grok-cli:access api:access"
	xaiAuthBase      = "https://auth.x.ai"
	xaiRefreshMargin = 5 * time.Minute
	// xaiDefaultLifetime is used when the server omits expires_in. A token with
	// no recorded deadline would never be refreshed until it failed.
	xaiDefaultLifetime = 3600 * time.Second
)

// XAI implements the xAI device-code flow.
type XAI struct {
	// AuthBase overrides the authorization host, for tests.
	AuthBase string
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
	// Now overrides the clock, for tests.
	Now func() time.Time
	// Sleep overrides the poll wait, for tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

func NewXAI() *XAI { return &XAI{} }

func (x *XAI) Name() string { return "xAI (Grok/X subscription)" }

func (x *XAI) client() *http.Client {
	if x.HTTPClient != nil {
		return x.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (x *XAI) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

func (x *XAI) base() string {
	if x.AuthBase != "" {
		return strings.TrimRight(x.AuthBase, "/")
	}
	return xaiAuthBase
}

func (x *XAI) deviceCodeURL() string { return x.base() + "/oauth2/device/code" }
func (x *XAI) tokenURL() string      { return x.base() + "/oauth2/token" }

// ToAuth turns a stored credential into request auth.
func (x *XAI) ToAuth(cred *auth.Credential) (auth.ModelAuth, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Access == "" {
		return auth.ModelAuth{}, fmt.Errorf("xai: no access token")
	}
	return auth.ModelAuth{APIKey: cred.OAuth.Access}, nil
}

// Refresh exchanges the refresh token for a new pair.
func (x *XAI) Refresh(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Refresh == "" {
		return nil, fmt.Errorf("xai: no refresh token; log in again")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {xaiClientID},
		"refresh_token": {cred.OAuth.Refresh},
	}
	body, err := x.postForm(ctx, x.tokenURL(), form)
	if err != nil {
		return nil, fmt.Errorf("xai token refresh failed: %w", err)
	}
	return x.credentialFrom(body, cred.OAuth.Refresh)
}

// Login runs the device flow.
func (x *XAI) Login(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	device, err := x.requestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}

	// verification_uri_complete embeds the code, so the user only has to
	// approve rather than transcribe. The bare URI is the fallback.
	target := device.VerificationURIComplete
	if target == "" {
		target = device.VerificationURI
	}
	if in != nil {
		in.Notify(auth.Event{
			Type:             auth.EventDeviceCode,
			Message:          fmt.Sprintf("Open %s and enter the code: %s", target, device.UserCode),
			URL:              target,
			VerificationURI:  target,
			UserCode:         device.UserCode,
			IntervalSeconds:  int(device.Interval),
			ExpiresInSeconds: int(device.ExpiresIn),
		})
	}

	return PollDeviceCode(ctx, DeviceCodeOptions[*auth.Credential]{
		IntervalSeconds:     device.Interval,
		ExpiresInSeconds:    device.ExpiresIn,
		WaitBeforeFirstPoll: true,
		Sleep:               x.Sleep,
		Poll: func(ctx context.Context) (PollResult[*auth.Credential], error) {
			return x.poll(ctx, device.DeviceCode)
		},
	})
}

type xaiDeviceCode struct {
	DeviceCode              string  `json:"device_code"`
	UserCode                string  `json:"user_code"`
	VerificationURI         string  `json:"verification_uri"`
	VerificationURIComplete string  `json:"verification_uri_complete"`
	Interval                float64 `json:"interval"`
	ExpiresIn               float64 `json:"expires_in"`
}

func (x *XAI) requestDeviceCode(ctx context.Context) (*xaiDeviceCode, error) {
	form := url.Values{
		"client_id": {xaiClientID},
		"scope":     {xaiScope},
		// referrer identifies the client to xAI; ported verbatim because
		// providers fingerprint these and answer an unknown one differently.
		"referrer": {"pi"},
	}
	body, err := x.postForm(ctx, x.deviceCodeURL(), form)
	if err != nil {
		return nil, fmt.Errorf("xai device authorization failed: %w", err)
	}

	var device xaiDeviceCode
	if err := json.Unmarshal(body, &device); err != nil {
		return nil, fmt.Errorf("xai: unreadable device code response")
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn <= 0 {
		return nil, fmt.Errorf("xai: incomplete device code response")
	}

	verification, err := validateVerificationURI("xai", device.VerificationURI, false)
	if err != nil {
		return nil, err
	}
	device.VerificationURI = verification
	if device.VerificationURIComplete != "" {
		complete, err := validateVerificationURI("xai", device.VerificationURIComplete, false)
		if err != nil {
			return nil, err
		}
		device.VerificationURIComplete = complete
	}
	return &device, nil
}

func (x *XAI) poll(ctx context.Context, deviceCode string) (PollResult[*auth.Credential], error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {xaiClientID},
		"device_code": {deviceCode},
	}
	body, err := x.postForm(ctx, x.tokenURL(), form)
	if err == nil {
		cred, credErr := x.credentialFrom(body, "")
		if credErr != nil {
			return PollResult[*auth.Credential]{Status: PollFailed, Message: credErr.Error()}, nil
		}
		return PollResult[*auth.Credential]{Status: PollComplete, Value: cred}, nil
	}

	var oauthErr *oauthErrorBody
	if !asOAuthError(err, &oauthErr) {
		// A transport failure is not a protocol answer; surfacing it stops the
		// loop rather than polling a dead endpoint until the code expires.
		return PollResult[*auth.Credential]{}, err
	}

	switch oauthErr.Code {
	case "authorization_pending":
		return PollResult[*auth.Credential]{Status: PollPending}, nil
	case "slow_down":
		return PollResult[*auth.Credential]{Status: PollSlowDown, Interval: oauthErr.Interval}, nil
	case "access_denied", "authorization_denied":
		return PollResult[*auth.Credential]{Status: PollFailed, Message: "xAI device authorization was denied"}, nil
	case "expired_token":
		return PollResult[*auth.Credential]{Status: PollFailed, Message: "xAI device code expired"}, nil
	default:
		return PollResult[*auth.Credential]{Status: PollFailed, Message: "xai device token polling failed: " + oauthErr.detail()}, nil
	}
}

// credentialFrom reads a token response. previousRefresh keeps a refresh token
// the server chose not to rotate.
func (x *XAI) credentialFrom(body []byte, previousRefresh string) (*auth.Credential, error) {
	var parsed struct {
		AccessToken  string   `json:"access_token"`
		RefreshToken string   `json:"refresh_token"`
		ExpiresIn    *float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("xai: unreadable token response")
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("xai: token response carries no access token")
	}
	refresh := parsed.RefreshToken
	if refresh == "" {
		// xAI omits the refresh token when it is not rotated; discarding the
		// old one would log the user out at the next refresh.
		refresh = previousRefresh
	}
	if refresh == "" {
		return nil, fmt.Errorf("xai: token response carries no refresh token")
	}

	lifetime := xaiDefaultLifetime
	if parsed.ExpiresIn != nil && *parsed.ExpiresIn > 0 {
		lifetime = time.Duration(*parsed.ExpiresIn * float64(time.Second))
	}
	return &auth.Credential{
		Type: auth.CredentialOAuth,
		OAuth: &auth.OAuthData{
			Access:  parsed.AccessToken,
			Refresh: refresh,
			Expires: x.now().Add(lifetime - xaiRefreshMargin).UnixMilli(),
		},
	}, nil
}

func (x *XAI) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := x.client().Do(req)
	if err != nil {
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
