package oauth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

func kimiFlow(t *testing.T, d *deviceServer) *Kimi {
	t.Helper()
	srv := d.start(t)
	return &Kimi{
		AuthBase: srv.URL, HTTPClient: srv.Client(),
		Now:   func() time.Time { return time.Unix(1_700_000_000, 0) },
		Sleep: noSleep,
	}
}

func okKimiDevice() map[string]any {
	return map[string]any{
		"device_code": "device-1", "user_code": "WXYZ-1234",
		"verification_uri":          "https://auth.kimi.com/device",
		"verification_uri_complete": "https://auth.kimi.com/device?code=WXYZ-1234",
		"interval":                  5, "expires_in": 900,
	}
}

func TestKimiPollsThroughPendingToSuccess(t *testing.T) {
	d := &deviceServer{
		device: okKimiDevice(),
		tokens: []response{
			{http.StatusBadRequest, map[string]any{"error": "authorization_pending"}},
			{http.StatusOK, map[string]any{
				"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 3600,
			}},
		},
	}
	flow := kimiFlow(t, d)

	in := &recordingInteraction{}
	cred, err := flow.Login(context.Background(), in)
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if cred.OAuth.Access != "access-1" || cred.OAuth.Refresh != "refresh-1" {
		t.Errorf("credential %+v", cred.OAuth)
	}

	// The complete URI embeds the code, so the user approves instead of typing.
	events := in.seen()
	if len(events) == 0 || !strings.Contains(events[0].URL, "code=WXYZ-1234") {
		t.Errorf("the complete verification URI was not preferred: %+v", events)
	}
}

// THE POINT: a 5xx is the server failing, not the user hesitating. Treating it
// as pending would poll a broken endpoint until the device code expired.
func TestKimiStopsOnAServerError(t *testing.T) {
	d := &deviceServer{
		device: okKimiDevice(),
		tokens: []response{{http.StatusInternalServerError, map[string]any{"message": "boom"}}},
	}
	flow := kimiFlow(t, d)

	_, err := flow.Login(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("error %v", err)
	}
	if len(d.tokenForms) != 1 {
		t.Errorf("want one poll before giving up, got %d", len(d.tokenForms))
	}
}

// THE POINT: this is the case that makes the status check load-bearing rather
// than decorative. A gateway in front of the token endpoint can answer 5xx with
// a body that still parses as a protocol state — and reading that as "the user
// has not approved yet" polls a broken endpoint until the code expires.
//
// Mutation testing found this gap: removing the status check left every other
// test passing, because their error bodies had no recognised code either way.
func TestKimiTreatsAServerErrorAsFatalEvenWhenTheBodyLooksPending(t *testing.T) {
	d := &deviceServer{
		device: okKimiDevice(),
		tokens: []response{{http.StatusBadGateway, map[string]any{"error": "authorization_pending"}}},
	}
	flow := kimiFlow(t, d)

	_, err := flow.Login(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("error %v — a 5xx must fail whatever the body says", err)
	}
	if len(d.tokenForms) != 1 {
		t.Errorf("want one poll before giving up, got %d", len(d.tokenForms))
	}
}

// THE POINT: the refresh retries a throttle or a blip, because the token is
// short-lived enough that one transient failure would end the session.
func TestKimiRefreshRetriesTransientFailures(t *testing.T) {
	d := &deviceServer{tokens: []response{
		{http.StatusTooManyRequests, map[string]any{"error": "rate_limited"}},
		{http.StatusServiceUnavailable, map[string]any{"error": "unavailable"}},
		{http.StatusOK, map[string]any{
			"access_token": "fresh", "refresh_token": "rotated", "expires_in": 3600,
		}},
	}}
	flow := kimiFlow(t, d)

	updated, err := flow.Refresh(context.Background(), &auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Refresh: "original"},
	})
	if err != nil {
		t.Fatalf("refresh should have retried: %v", err)
	}
	if updated.OAuth.Access != "fresh" || updated.OAuth.Refresh != "rotated" {
		t.Errorf("credential %+v", updated.OAuth)
	}
	if len(d.tokenForms) != 3 {
		t.Errorf("want three attempts, got %d", len(d.tokenForms))
	}
}

// A rejected refresh token will be rejected again; retrying only delays telling
// the user to log in.
func TestKimiRefreshDoesNotRetryARejection(t *testing.T) {
	d := &deviceServer{tokens: []response{
		{http.StatusBadRequest, map[string]any{"error": "invalid_grant"}},
	}}
	flow := kimiFlow(t, d)

	_, err := flow.Refresh(context.Background(), &auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Refresh: "expired"},
	})
	if err == nil {
		t.Fatal("a rejected refresh must fail")
	}
	if len(d.tokenForms) != 1 {
		t.Errorf("want one attempt for a permanent failure, got %d", len(d.tokenForms))
	}
}

// Kimi always rotates the refresh token, so a response without one is
// incomplete rather than an unchanged token to keep.
func TestKimiRequiresARefreshToken(t *testing.T) {
	d := &deviceServer{
		device: okKimiDevice(),
		tokens: []response{{http.StatusOK, map[string]any{
			"access_token": "access-1", "expires_in": 3600,
		}}},
	}
	flow := kimiFlow(t, d)

	_, err := flow.Login(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "missing fields") {
		t.Errorf("error %v", err)
	}
}

// The OAuth host is configurable because Kimi ships a separate mainland
// deployment; a hard-coded host would make that unreachable.
func TestKimiHostIsConfigurable(t *testing.T) {
	flow := &Kimi{Env: map[string]string{"KIMI_CODE_OAUTH_HOST": "https://auth.kimi.cn/"}}
	if got := flow.base(); got != "https://auth.kimi.cn" {
		t.Errorf("base %q — the trailing slash must be trimmed", got)
	}

	flow = &Kimi{Env: map[string]string{"KIMI_OAUTH_HOST": "https://alt.example"}}
	if got := flow.base(); got != "https://alt.example" {
		t.Errorf("base %q", got)
	}

	if got := (&Kimi{}).base(); got != kimiAuthHost {
		t.Errorf("default base %q", got)
	}
}

// A device response with no interval or validity still has to poll sensibly
// rather than spinning or giving up at once.
func TestKimiSuppliesDeviceDefaults(t *testing.T) {
	device := okKimiDevice()
	delete(device, "interval")
	delete(device, "expires_in")

	d := &deviceServer{
		device: device,
		tokens: []response{{http.StatusOK, map[string]any{
			"access_token": "a", "refresh_token": "r", "expires_in": 3600,
		}}},
	}
	flow := kimiFlow(t, d)

	in := &recordingInteraction{}
	if _, err := flow.Login(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	ev := in.seen()[0]
	if ev.IntervalSeconds != kimiDefaultInterval {
		t.Errorf("interval %d, want the default", ev.IntervalSeconds)
	}
	if ev.ExpiresInSeconds != kimiDeviceCodeTimeout {
		t.Errorf("expires %d, want the default", ev.ExpiresInSeconds)
	}
}

func TestKimiRejectsAnUntrustedVerificationURI(t *testing.T) {
	device := okKimiDevice()
	device["verification_uri"] = "file:///etc/passwd"
	delete(device, "verification_uri_complete")

	flow := kimiFlow(t, &deviceServer{device: device, tokens: []response{{http.StatusOK, map[string]any{}}}})
	_, err := flow.Login(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "untrusted verification_uri") {
		t.Errorf("error %v", err)
	}
}

func TestKimiToAuthAndMissingRefresh(t *testing.T) {
	got, err := NewKimi().ToAuth(&auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "tok"},
	})
	if err != nil || got.APIKey != "tok" {
		t.Errorf("ToAuth = %+v, %v", got, err)
	}
	if _, err := NewKimi().ToAuth(nil); err == nil {
		t.Error("a nil credential must not produce auth")
	}
	if _, err := NewKimi().Refresh(context.Background(), nil); err == nil {
		t.Error("a nil credential must not refresh")
	}
}
