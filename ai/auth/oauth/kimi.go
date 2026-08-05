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

	"github.com/ihavespoons/tau/ai/api/apishared"
	"github.com/ihavespoons/tau/ai/auth"
)

// Kimi is the Kimi For Coding subscription login. Another device flow, with one
// difference worth keeping: its refresh retries on throttling and server
// errors, because the token is short-lived enough that a single transient
// failure would otherwise end the session.

const (
	kimiClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	kimiAuthHost = "https://auth.kimi.com"
	// kimiDeviceCodeTimeout is the fallback validity when the server omits one.
	kimiDeviceCodeTimeout = 15 * 60
	kimiDefaultInterval   = 5
	kimiRefreshRetries    = 3
)

// Kimi implements the Kimi For Coding device-code flow.
type Kimi struct {
	// AuthBase overrides the authorization host, for tests.
	AuthBase string
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
	// Env overrides process environment lookups.
	Env map[string]string
	// Now overrides the clock, for tests.
	Now func() time.Time
	// Sleep overrides the poll and retry waits, for tests.
	Sleep func(ctx context.Context, d time.Duration) error
}

func NewKimi() *Kimi { return &Kimi{} }

func (k *Kimi) Name() string { return "Kimi For Coding" }

func (k *Kimi) client() *http.Client {
	if k.HTTPClient != nil {
		return k.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (k *Kimi) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Kimi) sleep(ctx context.Context, d time.Duration) error {
	if k.Sleep != nil {
		return k.Sleep(ctx, d)
	}
	return sleepCtx(ctx, d)
}

// base resolves the OAuth host. Kimi ships a separate mainland deployment, so
// the host is configurable rather than fixed.
func (k *Kimi) base() string {
	if k.AuthBase != "" {
		return strings.TrimRight(k.AuthBase, "/")
	}
	for _, name := range []string{"KIMI_CODE_OAUTH_HOST", "KIMI_OAUTH_HOST"} {
		if v := apishared.EnvValue(k.Env, name); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return kimiAuthHost
}

func (k *Kimi) deviceCodeURL() string { return k.base() + "/api/oauth/device_authorization" }
func (k *Kimi) tokenURL() string      { return k.base() + "/api/oauth/token" }

// ToAuth turns a stored credential into request auth.
func (k *Kimi) ToAuth(cred *auth.Credential) (auth.ModelAuth, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Access == "" {
		return auth.ModelAuth{}, fmt.Errorf("kimi: no access token")
	}
	return auth.ModelAuth{APIKey: cred.OAuth.Access}, nil
}

// Refresh exchanges the refresh token, retrying transient failures.
func (k *Kimi) Refresh(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Refresh == "" {
		return nil, fmt.Errorf("kimi: no refresh token; log in again")
	}

	form := url.Values{
		"client_id":     {kimiClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.OAuth.Refresh},
	}

	var lastErr error
	for attempt := 0; attempt <= kimiRefreshRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff from one second. A refresh that fails for
			// good still fails; this only covers the throttle or the blip.
			if err := k.sleep(ctx, time.Duration(1<<(attempt-1))*time.Second); err != nil {
				return nil, err
			}
		}
		body, err := k.postForm(ctx, k.tokenURL(), form)
		if err == nil {
			return k.credentialFrom(body, "refresh")
		}
		lastErr = err

		var oauthErr *oauthErrorBody
		if !asOAuthError(err, &oauthErr) || !retryableRefreshStatus(oauthErr.Status) {
			// A rejected refresh token will be rejected again; retrying only
			// delays telling the user to log in.
			return nil, fmt.Errorf("kimi token refresh failed: %w", err)
		}
	}
	return nil, fmt.Errorf("kimi token refresh failed after %d retries: %w", kimiRefreshRetries, lastErr)
}

// retryableRefreshStatus reports whether a status is worth another attempt.
func retryableRefreshStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// Login runs the device flow.
func (k *Kimi) Login(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	device, err := k.requestDeviceCode(ctx)
	if err != nil {
		return nil, err
	}

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
		Sleep:               k.Sleep,
		Poll: func(ctx context.Context) (PollResult[*auth.Credential], error) {
			return k.poll(ctx, device.DeviceCode)
		},
	})
}

type kimiDeviceCode struct {
	DeviceCode              string  `json:"device_code"`
	UserCode                string  `json:"user_code"`
	VerificationURI         string  `json:"verification_uri"`
	VerificationURIComplete string  `json:"verification_uri_complete"`
	Interval                float64 `json:"interval"`
	ExpiresIn               float64 `json:"expires_in"`
}

func (k *Kimi) requestDeviceCode(ctx context.Context) (*kimiDeviceCode, error) {
	body, err := k.postForm(ctx, k.deviceCodeURL(), url.Values{"client_id": {kimiClientID}})
	if err != nil {
		return nil, fmt.Errorf("kimi device authorization failed: %w", err)
	}

	var device kimiDeviceCode
	if err := json.Unmarshal(body, &device); err != nil {
		return nil, fmt.Errorf("kimi: unreadable device code response")
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" {
		return nil, fmt.Errorf("kimi: incomplete device code response")
	}

	// A self-hosted or mainland deployment is still reached over https; the
	// URI goes to the browser opener either way.
	verification, err := validateVerificationURI("kimi", device.VerificationURI, false)
	if err != nil {
		return nil, err
	}
	device.VerificationURI = verification
	if device.VerificationURIComplete != "" {
		complete, err := validateVerificationURI("kimi", device.VerificationURIComplete, false)
		if err != nil {
			return nil, err
		}
		device.VerificationURIComplete = complete
	}

	if device.Interval <= 0 {
		device.Interval = kimiDefaultInterval
	}
	if device.ExpiresIn <= 0 {
		device.ExpiresIn = kimiDeviceCodeTimeout
	}
	return &device, nil
}

func (k *Kimi) poll(ctx context.Context, deviceCode string) (PollResult[*auth.Credential], error) {
	form := url.Values{
		"client_id":   {kimiClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	body, err := k.postForm(ctx, k.tokenURL(), form)
	if err == nil {
		cred, credErr := k.credentialFrom(body, "poll")
		if credErr != nil {
			return PollResult[*auth.Credential]{Status: PollFailed, Message: credErr.Error()}, nil
		}
		return PollResult[*auth.Credential]{Status: PollComplete, Value: cred}, nil
	}

	var oauthErr *oauthErrorBody
	if !asOAuthError(err, &oauthErr) {
		return PollResult[*auth.Credential]{}, err
	}

	// A 5xx is the server failing, not the user hesitating. Treating it as
	// pending would poll a broken endpoint until the code expired.
	if oauthErr.Status >= 500 {
		return PollResult[*auth.Credential]{
			Status:  PollFailed,
			Message: fmt.Sprintf("kimi device token request failed with status %d: %s", oauthErr.Status, oauthErr.detail()),
		}, nil
	}

	switch oauthErr.Code {
	case "authorization_pending":
		return PollResult[*auth.Credential]{Status: PollPending}, nil
	case "slow_down":
		return PollResult[*auth.Credential]{Status: PollSlowDown, Interval: oauthErr.Interval}, nil
	case "expired_token":
		return PollResult[*auth.Credential]{
			Status: PollFailed, Message: "Kimi device authorization expired. Please restart login.",
		}, nil
	case "access_denied":
		return PollResult[*auth.Credential]{Status: PollFailed, Message: "Kimi login was denied."}, nil
	default:
		return PollResult[*auth.Credential]{
			Status:  PollFailed,
			Message: fmt.Sprintf("kimi device token request failed (status %d): %s", oauthErr.Status, oauthErr.detail()),
		}, nil
	}
}

func (k *Kimi) credentialFrom(body []byte, operation string) (*auth.Credential, error) {
	var parsed struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("kimi: unreadable token %s response", operation)
	}
	// Kimi always rotates the refresh token, so a response missing one is
	// incomplete rather than an unchanged token.
	if parsed.AccessToken == "" || parsed.RefreshToken == "" || parsed.ExpiresIn <= 0 {
		return nil, fmt.Errorf("kimi token %s response is missing fields", operation)
	}
	return &auth.Credential{
		Type: auth.CredentialOAuth,
		OAuth: &auth.OAuthData{
			Access:  parsed.AccessToken,
			Refresh: parsed.RefreshToken,
			Expires: k.now().Add(time.Duration(parsed.ExpiresIn * float64(time.Second))).UnixMilli(),
		},
	}, nil
}

func (k *Kimi) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := k.client().Do(req)
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
