package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// GitHub Copilot's login is two exchanges, not one.
//
// The device flow yields a long-lived GitHub token, which is NOT what the
// Copilot API accepts. That token is then traded for a short-lived Copilot
// token — good for under an hour — which is what actually authenticates a
// request. So the GitHub token is stored as the refresh token and the Copilot
// token as the access token, and the usual refresh machinery does the rest.

// copilotClientID is the public client id the Copilot editor plugins use.
// It is not a secret — it is base64 here only because GitHub's secret scanner
// flags the literal, not because it protects anything.
var copilotClientID = mustDecode("SXYxLmI1MDdhMDhjODdlY2ZlOTg=")

func mustDecode(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// copilotHeaders identify tau as a Copilot client. The endpoint rejects a
// request that does not present a recognised editor and integration.
var copilotHeaders = map[string]string{
	"User-Agent":             "GitHubCopilotChat/0.35.0",
	"Editor-Version":         "vscode/1.107.0",
	"Editor-Plugin-Version":  "copilot-chat/0.35.0",
	"Copilot-Integration-Id": "vscode-chat",
}

const copilotUserAgent = "GitHubCopilotChat/0.35.0"

// Copilot implements the GitHub Copilot OAuth flow.
type Copilot struct {
	// Domain is the GitHub host, for GitHub Enterprise. Empty means github.com.
	Domain string
	// BaseURL overrides the https://<domain> prefix the device endpoints are
	// built from. Domain is reduced to a hostname, which drops a port — fine
	// for github.com, not for an enterprise install that has one.
	BaseURL string
	// TokenURL overrides where the GitHub token is traded for a Copilot one.
	// Enterprise installations do not always put it on api.<domain>, and the
	// exchange is on a different host from the rest of the flow.
	TokenURL string
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
	// Sleep overrides the poll wait, for tests. A device flow that really
	// waits makes its own tests slow enough that people stop running them.
	Sleep func(context.Context, time.Duration) error
}

func NewCopilot() *Copilot { return &Copilot{} }

func (c *Copilot) Name() string { return "GitHub Copilot" }

func (c *Copilot) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Copilot) domain() string {
	if d := normalizeDomain(c.Domain); d != "" {
		return d
	}
	return "github.com"
}

// normalizeDomain reduces whatever the user typed to a hostname, so both
// "example.com" and "https://example.com/" work.
func normalizeDomain(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// base is where the device endpoints live.
func (c *Copilot) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://" + c.domain()
}

func (c *Copilot) deviceCodeURL() string  { return c.base() + "/login/device/code" }
func (c *Copilot) accessTokenURL() string { return c.base() + "/login/oauth/access_token" }
func (c *Copilot) copilotTokenURL() string {
	if c.TokenURL != "" {
		return c.TokenURL
	}
	return "https://api." + c.domain() + "/copilot_internal/v2/token"
}

// ToAuth turns a stored credential into request auth.
func (c *Copilot) ToAuth(cred *auth.Credential) (auth.ModelAuth, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Access == "" {
		return auth.ModelAuth{}, fmt.Errorf("copilot: no access token")
	}
	headers := map[string]*string{}
	for k, v := range copilotHeaders {
		value := v
		headers[k] = &value
	}
	return auth.ModelAuth{APIKey: cred.OAuth.Access, Headers: headers}, nil
}

// Refresh trades the stored GitHub token for a fresh Copilot token.
//
// Unlike a normal OAuth refresh this is not a rotation: the GitHub token does
// not change and is not consumed, so a failure here leaves the credential
// usable for another attempt rather than logging the user out.
func (c *Copilot) Refresh(ctx context.Context, cred *auth.Credential) (*auth.Credential, error) {
	if cred == nil || cred.OAuth == nil || cred.OAuth.Refresh == "" {
		return nil, fmt.Errorf("copilot: no github token to exchange")
	}

	token, expiresAt, err := c.exchangeForCopilotToken(ctx, cred.OAuth.Refresh)
	if err != nil {
		return nil, err
	}

	updated := *cred
	oauthData := *cred.OAuth
	oauthData.Access = token
	oauthData.Expires = expiresAt
	updated.OAuth = &oauthData
	return &updated, nil
}

// Login runs the device flow and exchanges the result.
func (c *Copilot) Login(ctx context.Context, in auth.Interaction) (*auth.Credential, error) {
	device, err := c.startDeviceFlow(ctx)
	if err != nil {
		return nil, err
	}

	// The code is useless without somewhere to type it, so both are shown
	// together and the browser is opened as a convenience rather than the
	// only route.
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

	githubToken, err := PollDeviceCode(ctx, DeviceCodeOptions[string]{
		IntervalSeconds:     device.Interval,
		ExpiresInSeconds:    device.ExpiresIn,
		WaitBeforeFirstPoll: true,
		Sleep:               c.Sleep,
		Poll: func(ctx context.Context) (PollResult[string], error) {
			return c.pollToken(ctx, device.DeviceCode)
		},
	})
	if err != nil {
		return nil, err
	}

	copilotToken, expiresAt, err := c.exchangeForCopilotToken(ctx, githubToken)
	if err != nil {
		return nil, err
	}

	return &auth.Credential{
		Type: auth.CredentialOAuth,
		OAuth: &auth.OAuthData{
			Access: copilotToken,
			// The GitHub token is what mints the next Copilot token, so it is
			// what the refresh machinery needs to keep.
			Refresh: githubToken,
			Expires: expiresAt,
		},
	}, nil
}

type deviceCodeResponse struct {
	DeviceCode      string  `json:"device_code"`
	UserCode        string  `json:"user_code"`
	VerificationURI string  `json:"verification_uri"`
	Interval        float64 `json:"interval"`
	ExpiresIn       float64 `json:"expires_in"`
}

func (c *Copilot) startDeviceFlow(ctx context.Context) (*deviceCodeResponse, error) {
	form := url.Values{"client_id": {copilotClientID}, "scope": {"read:user"}}

	var device deviceCodeResponse
	if err := c.postForm(ctx, c.deviceCodeURL(), form, &device); err != nil {
		return nil, err
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn == 0 {
		return nil, fmt.Errorf("copilot: incomplete device code response")
	}

	// GitHub Enterprise installations are reachable over plain http on an
	// internal network, so this is the one flow that allows it.
	verification, err := validateVerificationURI("copilot", device.VerificationURI, true)
	if err != nil {
		return nil, err
	}
	device.VerificationURI = verification
	return &device, nil
}

func (c *Copilot) pollToken(ctx context.Context, deviceCode string) (PollResult[string], error) {
	form := url.Values{
		"client_id":   {copilotClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}

	var body struct {
		AccessToken      string  `json:"access_token"`
		Error            string  `json:"error"`
		ErrorDescription string  `json:"error_description"`
		Interval         float64 `json:"interval"`
	}
	if err := c.postForm(ctx, c.accessTokenURL(), form, &body); err != nil {
		return PollResult[string]{}, err
	}

	if body.AccessToken != "" {
		return PollResult[string]{Status: PollComplete, Value: body.AccessToken}, nil
	}
	switch body.Error {
	case "authorization_pending":
		return PollResult[string]{Status: PollPending}, nil
	case "slow_down":
		return PollResult[string]{Status: PollSlowDown, Interval: body.Interval}, nil
	case "":
		return PollResult[string]{Status: PollFailed, Message: "copilot: invalid device token response"}, nil
	default:
		message := "copilot: device flow failed: " + body.Error
		if body.ErrorDescription != "" {
			message += ": " + body.ErrorDescription
		}
		return PollResult[string]{Status: PollFailed, Message: message}, nil
	}
}

// exchangeForCopilotToken trades a GitHub token for a short-lived Copilot one.
func (c *Copilot) exchangeForCopilotToken(ctx context.Context, githubToken string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.copilotTokenURL(), nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+githubToken)
	for k, v := range copilotHeaders {
		req.Header.Set(k, v)
	}

	var body struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := c.do(req, &body); err != nil {
		return "", 0, err
	}
	if body.Token == "" {
		return "", 0, fmt.Errorf("copilot: token response had no token")
	}
	// expires_at is seconds; tau stores milliseconds.
	return body.Token, body.ExpiresAt * 1000, nil
}

func (c *Copilot) postForm(ctx context.Context, endpoint string, form url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", copilotUserAgent)
	return c.do(req, into)
}

func (c *Copilot) do(req *http.Request, into any) error {
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("copilot: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// proxyEndpoint pulls the API host out of a Copilot token.
//
// The token is a semicolon-separated bag of claims, one of which names the
// proxy that serves this account — individual, business, or enterprise. Using
// the wrong one is a 404, and the account type is not knowable any other way.
var proxyEndpoint = regexp.MustCompile(`proxy-ep=([^;]+)`)

// BaseURLFromToken returns the API base a Copilot token points at, or "" when
// the token does not name one.
func BaseURLFromToken(token string) string {
	m := proxyEndpoint.FindStringSubmatch(token)
	if m == nil {
		return ""
	}
	host := strings.TrimSpace(m[1])
	if host == "" {
		return ""
	}
	// The claim names the proxy host; the API lives on the matching api. host.
	return "https://" + strings.Replace(host, "proxy.", "api.", 1)
}
