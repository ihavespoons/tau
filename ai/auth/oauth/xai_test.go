package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// noSleep runs the poll loop without real waits.
func noSleep(ctx context.Context, _ time.Duration) error {
	if ctx.Err() != nil {
		return ErrLoginCancelled
	}
	return nil
}

// maxTestPolls bounds how many times a test will answer the token endpoint.
//
// The poll loop's real deadline is wall-clock, and the tests replace the wait
// with a no-op — so a flow that never reaches a terminal state does not hang
// politely, it busy-spins at full CPU until the device code expires. Mutation
// testing found exactly that: turning one denial into "pending" pinned a core
// for ten minutes. This cap turns that into an immediate, named failure.
const maxTestPolls = 50

// deviceServer replies to the device-code and token endpoints in turn. Each
// token response is used once, so a test can script pending-then-complete.
type deviceServer struct {
	device      map[string]any
	tokens      []response
	tokenIndex  int
	deviceForms []url.Values
	tokenForms  []url.Values
}

type response struct {
	status int
	body   map[string]any
}

func (d *deviceServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(r.URL.Path, "device") && !strings.Contains(r.URL.Path, "token") {
			d.deviceForms = append(d.deviceForms, r.PostForm)
			_ = json.NewEncoder(w).Encode(d.device)
			return
		}

		d.tokenForms = append(d.tokenForms, r.PostForm)
		if len(d.tokenForms) > maxTestPolls {
			w.WriteHeader(http.StatusTeapot)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "test_poll_limit", "error_description": "the flow polled without ever terminating",
			})
			return
		}
		next := d.tokens[min(d.tokenIndex, len(d.tokens)-1)]
		d.tokenIndex++
		w.WriteHeader(next.status)
		_ = json.NewEncoder(w).Encode(next.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func xaiFlow(t *testing.T, d *deviceServer) *XAI {
	t.Helper()
	srv := d.start(t)
	return &XAI{
		AuthBase: srv.URL, HTTPClient: srv.Client(),
		Now:   func() time.Time { return time.Unix(1_700_000_000, 0) },
		Sleep: noSleep,
	}
}

func okDevice() map[string]any {
	return map[string]any{
		"device_code": "device-1", "user_code": "ABCD-EFGH",
		"verification_uri": "https://auth.x.ai/activate",
		"interval":         5, "expires_in": 600,
	}
}

// THE POINT: the device flow's states are the whole protocol. Reading
// authorization_pending as a failure ends a login the user was about to
// complete; reading a real failure as pending polls until the code expires.
func TestXAIPollsThroughPendingToSuccess(t *testing.T) {
	d := &deviceServer{
		device: okDevice(),
		tokens: []response{
			{http.StatusBadRequest, map[string]any{"error": "authorization_pending"}},
			{http.StatusBadRequest, map[string]any{"error": "slow_down", "interval": 10}},
			{http.StatusOK, map[string]any{
				"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 3600,
			}},
		},
	}
	flow := xaiFlow(t, d)

	in := &recordingInteraction{}
	cred, err := flow.Login(context.Background(), in)
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if cred.OAuth.Access != "access-1" || cred.OAuth.Refresh != "refresh-1" {
		t.Errorf("credential %+v", cred.OAuth)
	}
	if len(d.tokenForms) != 3 {
		t.Errorf("want three polls, got %d", len(d.tokenForms))
	}
	if got := d.tokenForms[0].Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("grant_type %q", got)
	}

	// The user cannot approve without being shown the code and where to type it.
	events := in.seen()
	if len(events) == 0 || events[0].UserCode != "ABCD-EFGH" {
		t.Errorf("events %+v", events)
	}
}

// The parameter set is what the provider fingerprints.
func TestXAIDeviceRequestParameters(t *testing.T) {
	d := &deviceServer{
		device: okDevice(),
		tokens: []response{{http.StatusOK, map[string]any{
			"access_token": "a", "refresh_token": "r", "expires_in": 3600,
		}}},
	}
	flow := xaiFlow(t, d)
	if _, err := flow.Login(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	form := d.deviceForms[0]
	if form.Get("client_id") != xaiClientID {
		t.Errorf("client_id %q", form.Get("client_id"))
	}
	if form.Get("scope") != xaiScope {
		t.Errorf("scope %q", form.Get("scope"))
	}
	if form.Get("referrer") == "" {
		t.Error("the referrer identifies the client and must be sent")
	}
}

// THE POINT: the verification URI is handed to the platform's browser opener.
// An unconstrained scheme could name a local executable or a file.
func TestXAIRejectsAnUntrustedVerificationURI(t *testing.T) {
	for _, bad := range []string{"file:///etc/passwd", "http://auth.x.ai/activate", "not a url", "javascript:alert(1)"} {
		device := okDevice()
		device["verification_uri"] = bad
		flow := xaiFlow(t, &deviceServer{device: device, tokens: []response{{http.StatusOK, map[string]any{}}}})

		_, err := flow.Login(context.Background(), nil)
		if err == nil || !strings.Contains(err.Error(), "untrusted verification_uri") {
			t.Errorf("verification_uri %q was accepted: %v", bad, err)
		}
	}
}

// A denial is final; polling on would waste the user's time and the code.
func TestXAIStopsOnDenial(t *testing.T) {
	d := &deviceServer{
		device: okDevice(),
		tokens: []response{{http.StatusBadRequest, map[string]any{"error": "access_denied"}}},
	}
	flow := xaiFlow(t, d)

	_, err := flow.Login(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("error %v", err)
	}
	if len(d.tokenForms) != 1 {
		t.Errorf("want one poll after a denial, got %d", len(d.tokenForms))
	}
}

// THE POINT: xAI omits the refresh token when it is not rotated. Discarding
// the old one would log the user out at the next refresh.
func TestXAIRefreshKeepsAnUnrotatedToken(t *testing.T) {
	d := &deviceServer{tokens: []response{{http.StatusOK, map[string]any{
		"access_token": "fresh", "expires_in": 3600,
	}}}}
	flow := xaiFlow(t, d)

	updated, err := flow.Refresh(context.Background(), &auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Refresh: "original"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.OAuth.Refresh != "original" {
		t.Errorf("refresh token %q — an unrotated token must be kept", updated.OAuth.Refresh)
	}
	if updated.OAuth.Access != "fresh" {
		t.Errorf("access %q", updated.OAuth.Access)
	}
}

func TestXAIRefreshTakesARotatedToken(t *testing.T) {
	d := &deviceServer{tokens: []response{{http.StatusOK, map[string]any{
		"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600,
	}}}}
	flow := xaiFlow(t, d)

	updated, err := flow.Refresh(context.Background(), &auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Refresh: "original"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.OAuth.Refresh != "rotated" {
		t.Errorf("refresh token %q", updated.OAuth.Refresh)
	}
}

// A margin is subtracted so a token cannot expire while a request is in flight.
func TestXAISubtractsARefreshMargin(t *testing.T) {
	d := &deviceServer{
		device: okDevice(),
		tokens: []response{{http.StatusOK, map[string]any{
			"access_token": "a", "refresh_token": "r", "expires_in": 3600,
		}}},
	}
	flow := xaiFlow(t, d)

	cred, err := flow.Login(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(1_700_000_000, 0).Add(3600*time.Second - xaiRefreshMargin).UnixMilli()
	if cred.OAuth.Expires != want {
		t.Errorf("expires %d, want %d", cred.OAuth.Expires, want)
	}
}

// A response with no expires_in still needs a deadline, or the token is never
// refreshed until it fails mid-request.
func TestXAIDefaultsTheTokenLifetime(t *testing.T) {
	d := &deviceServer{
		device: okDevice(),
		tokens: []response{{http.StatusOK, map[string]any{
			"access_token": "a", "refresh_token": "r",
		}}},
	}
	flow := xaiFlow(t, d)

	cred, err := flow.Login(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(1_700_000_000, 0).Add(xaiDefaultLifetime - xaiRefreshMargin).UnixMilli()
	if cred.OAuth.Expires != want {
		t.Errorf("expires %d, want the default lifetime %d", cred.OAuth.Expires, want)
	}
}

func TestXAIToAuthAndMissingRefresh(t *testing.T) {
	got, err := NewXAI().ToAuth(&auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "tok"},
	})
	if err != nil || got.APIKey != "tok" {
		t.Errorf("ToAuth = %+v, %v", got, err)
	}
	if _, err := NewXAI().ToAuth(nil); err == nil {
		t.Error("a nil credential must not produce auth")
	}
	if _, err := NewXAI().Refresh(context.Background(), nil); err == nil {
		t.Error("a nil credential must not refresh")
	}
}
