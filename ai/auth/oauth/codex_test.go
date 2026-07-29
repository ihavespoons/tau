package oauth

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// pastingInteraction answers the manual-paste prompt with a fixed value.
type pastingInteraction struct {
	value  string
	err    error
	events []auth.Event
}

func (p *pastingInteraction) Prompt(context.Context, auth.Prompt) (string, error) {
	return p.value, p.err
}
func (p *pastingInteraction) Notify(ev auth.Event) { p.events = append(p.events, ev) }

func codexTokenServer(t *testing.T, handler http.HandlerFunc) *Codex {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Codex{
		AuthBase: srv.URL, HTTPClient: srv.Client(),
		DisableCallbackServer: true,
		Now:                   func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

func okTokenHandler(access, refresh string, expiresIn int64) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access, "refresh_token": refresh, "expires_in": expiresIn,
		})
	}
}

// THE POINT: the parameter set is what the provider fingerprints. An
// authorization missing one of these is answered differently or refused, and
// the failure is a login that silently never completes.
func TestCodexAuthorizeURLParameters(t *testing.T) {
	flow := &Codex{}
	raw := flow.buildAuthorizeURL("challenge-value", "state-value")

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, codexAuthBase+"/oauth/authorize?") {
		t.Errorf("authorize URL: %q", raw)
	}

	want := map[string]string{
		"response_type":              "code",
		"client_id":                  codexClientID,
		"redirect_uri":               codexRedirectURI,
		"scope":                      codexScope,
		"code_challenge":             "challenge-value",
		"code_challenge_method":      "S256",
		"state":                      "state-value",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 "tau",
	}
	q := u.Query()
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// What the user has to hand is usually the address bar, so all three forms are
// accepted.
func TestParseCodexRedirect(t *testing.T) {
	cases := []struct{ in, code, state string }{
		{"plain-code", "plain-code", ""},
		{"  plain-code  ", "plain-code", ""},
		{"the-code#the-state", "the-code", "the-state"},
		{"http://localhost:1455/auth/callback?code=abc&state=xyz", "abc", "xyz"},
		{"https://example.com/cb?state=xyz&code=abc&other=1", "abc", "xyz"},
		{"", "", ""},
	}
	for _, tc := range cases {
		code, state := parseCodexRedirect(tc.in)
		if code != tc.code || state != tc.state {
			t.Errorf("parseCodexRedirect(%q) = (%q, %q), want (%q, %q)", tc.in, code, state, tc.code, tc.state)
		}
	}
}

// THE POINT: a pasted URL carries the state. A mismatch means the code belongs
// to a different login attempt — accepting it would exchange a code the user
// did not just authorize.
func TestCodexRejectsAMismatchedState(t *testing.T) {
	flow := codexTokenServer(t, okTokenHandler("access", "refresh", 3600))
	in := &pastingInteraction{value: "http://localhost:1455/auth/callback?code=abc&state=someone-elses"}

	_, err := flow.Login(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "different login attempt") {
		t.Errorf("error: %v", err)
	}
}

// A bare code has no state to check, so it is accepted — that is the whole
// point of the fallback.
func TestCodexAcceptsABareCode(t *testing.T) {
	var seenForm url.Values
	flow := codexTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seenForm = r.PostForm
		okTokenHandler("access-token", "refresh-token", 3600)(w, r)
	})

	cred, err := flow.Login(context.Background(), &pastingInteraction{value: "the-code"})
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if cred.OAuth.Access != "access-token" || cred.OAuth.Refresh != "refresh-token" {
		t.Errorf("credential: %#v", cred.OAuth)
	}

	// The exchange has to carry the verifier, or PKCE proves nothing.
	if seenForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type: %q", seenForm.Get("grant_type"))
	}
	if seenForm.Get("code_verifier") == "" {
		t.Error("the exchange omitted the PKCE verifier")
	}
	if seenForm.Get("redirect_uri") != codexRedirectURI {
		t.Errorf("redirect_uri: %q", seenForm.Get("redirect_uri"))
	}
}

// THE POINT: the margin is subtracted at storage, so every consumer of the
// deadline gets the same safety without having to know about it. Without it a
// token can expire while a request is in flight.
func TestCodexSubtractsARefreshMargin(t *testing.T) {
	flow := codexTokenServer(t, okTokenHandler("access", "refresh", 3600))

	cred, err := flow.Login(context.Background(), &pastingInteraction{value: "code"})
	if err != nil {
		t.Fatal(err)
	}

	want := time.Unix(1_700_000_000, 0).Add(3600*time.Second - codexRefreshMargin).UnixMilli()
	if cred.OAuth.Expires != want {
		t.Errorf("expires %d, want %d (lifetime minus the margin)", cred.OAuth.Expires, want)
	}
}

// A refresh that returns no new refresh token leaves the old one valid;
// discarding it would log the user out at the next refresh.
func TestCodexRefreshKeepsTheOldTokenWhenNoneIsReturned(t *testing.T) {
	flow := codexTokenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access", "expires_in": 3600,
		})
	})

	cred := &auth.Credential{
		Type:  auth.CredentialOAuth,
		OAuth: &auth.OAuthData{Access: "stale", Refresh: "original-refresh"},
	}
	updated, err := flow.Refresh(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if updated.OAuth.Access != "fresh-access" {
		t.Errorf("access: %q", updated.OAuth.Access)
	}
	if updated.OAuth.Refresh != "original-refresh" {
		t.Errorf("the refresh token was discarded: %q", updated.OAuth.Refresh)
	}
}

// A rotated refresh token replaces the old one.
func TestCodexRefreshTakesARotatedToken(t *testing.T) {
	flow := codexTokenServer(t, okTokenHandler("fresh-access", "rotated-refresh", 3600))

	updated, err := flow.Refresh(context.Background(), &auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Refresh: "original"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.OAuth.Refresh != "rotated-refresh" {
		t.Errorf("refresh: %q", updated.OAuth.Refresh)
	}
}

// An OAuth error body says what went wrong; a bare status leaves the user
// guessing whether it was their code, their clock, or the provider.
func TestCodexSurfacesTheProviderError(t *testing.T) {
	flow := codexTokenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_grant", "error_description": "authorization code expired",
		})
	})

	_, err := flow.Login(context.Background(), &pastingInteraction{value: "code"})
	if err == nil || !strings.Contains(err.Error(), "authorization code expired") {
		t.Errorf("error: %v", err)
	}
}

func TestCodexWithoutARefreshTokenFails(t *testing.T) {
	if _, err := NewCodex().Refresh(context.Background(), nil); err == nil {
		t.Error("a nil credential must not refresh")
	}
	_, err := NewCodex().Refresh(context.Background(), &auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "a"},
	})
	if err == nil || !strings.Contains(err.Error(), "log in again") {
		t.Errorf("the error should say what to do: %v", err)
	}
}

func TestCodexToAuth(t *testing.T) {
	got, err := NewCodex().ToAuth(&auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "tok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "tok" {
		t.Errorf("api key: %q", got.APIKey)
	}
	if _, err := NewCodex().ToAuth(nil); err == nil {
		t.Error("a nil credential must not produce auth")
	}
}

// THE POINT: the callback port is registered with the provider, so it cannot
// be moved. When something already holds it, the manual paste is not a nicety
// — it is the only route, and login must reach it rather than failing.
func TestCodexFallsBackWhenThePortIsTaken(t *testing.T) {
	blocker, err := net.Listen("tcp", net.JoinHostPort(callbackHost(), strconv.Itoa(codexCallbackPort)))
	if err != nil {
		t.Skipf("port %d is not bindable here: %v", codexCallbackPort, err)
	}
	defer func() { _ = blocker.Close() }()

	srv := httptest.NewServer(okTokenHandler("access", "refresh", 3600))
	defer srv.Close()

	flow := &Codex{
		AuthBase: srv.URL, HTTPClient: srv.Client(),
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	cred, err := flow.Login(context.Background(), &pastingInteraction{value: "pasted-code"})
	if err != nil {
		t.Fatalf("login must fall back to the paste: %v", err)
	}
	if cred.OAuth.Access != "access" {
		t.Errorf("credential: %#v", cred.OAuth)
	}
}

// The user cannot authorize without being shown where to go.
func TestCodexShowsTheAuthorizeURL(t *testing.T) {
	flow := codexTokenServer(t, okTokenHandler("a", "r", 3600))
	in := &pastingInteraction{value: "code"}

	if _, err := flow.Login(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(in.events) == 0 {
		t.Fatal("the user was shown nothing")
	}
	if !strings.Contains(in.events[0].URL, "/oauth/authorize") {
		t.Errorf("event URL: %q", in.events[0].URL)
	}
}
