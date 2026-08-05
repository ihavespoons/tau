package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// openRouterFlow wires a flow to a stand-in key endpoint and returns the
// request bodies it received.
func openRouterFlow(t *testing.T, status int, body map[string]any) (*OpenRouter, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		seen = append(seen, parsed)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return &OpenRouter{
		AuthorizeURL: "https://openrouter.example/auth",
		KeysURL:      srv.URL, HTTPClient: srv.Client(),
		LoginTimeout: 5 * time.Second,
	}, &seen
}

// visitCallback drives the browser half of the flow: it reads the callback URL
// out of the auth_url the user was shown, and requests it.
func visitCallback(t *testing.T, in *recordingInteraction, query string) *http.Response {
	t.Helper()
	var authURL string
	for _, ev := range in.seen() {
		if ev.Type == auth.EventAuthURL {
			authURL = ev.URL
		}
	}
	if authURL == "" {
		t.Fatalf("no authorize URL was shown: %+v", in.seen())
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	callback := parsed.Query().Get("callback_url")
	if callback == "" {
		t.Fatalf("the authorize URL carries no callback_url: %s", authURL)
	}

	resp, err := http.Get(callback + "?" + query)
	if err != nil {
		t.Fatalf("visiting the callback failed: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// THE POINT: OpenRouter returns a permanent API key rather than a token pair.
// The whole flow exists to get that key, and the exchange must carry the PKCE
// verifier or the challenge proved nothing.
func TestOpenRouterExchangesACodeForAKey(t *testing.T) {
	flow, seen := openRouterFlow(t, http.StatusOK, map[string]any{"key": "sk-or-v1-example"})
	in := &recordingInteraction{}

	done := make(chan struct{})
	var cred *auth.Credential
	var loginErr error
	go func() {
		cred, loginErr = flow.Login(context.Background(), in)
		close(done)
	}()

	waitForEvent(t, in, auth.EventAuthURL)
	resp := visitCallback(t, in, "code=the-code")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("callback status %d", resp.StatusCode)
	}
	<-done

	if loginErr != nil {
		t.Fatalf("login failed: %v", loginErr)
	}
	if cred.OAuth.Access != "sk-or-v1-example" {
		t.Errorf("key %q", cred.OAuth.Access)
	}
	// The key does not expire, and the deadline must be the one Pi records.
	if cred.OAuth.Expires != neverExpires {
		t.Errorf("expires %d, want %d", cred.OAuth.Expires, neverExpires)
	}

	if len(*seen) != 1 {
		t.Fatalf("want one exchange, got %d", len(*seen))
	}
	body := (*seen)[0]
	if body["code"] != "the-code" {
		t.Errorf("code %v", body["code"])
	}
	if body["code_verifier"] == "" || body["code_verifier"] == nil {
		t.Error("the exchange omitted the PKCE verifier")
	}
	if body["code_challenge_method"] != "S256" {
		t.Errorf("challenge method %v", body["code_challenge_method"])
	}
}

// THE POINT: the callback path is the only thing guarding this endpoint, since
// OpenRouter round-trips no state. A fixed path would let any page the user
// visits afterwards replay a code at a server that is still listening.
func TestOpenRouterUsesAFreshUnguessableCallbackPath(t *testing.T) {
	seenPaths := map[string]bool{}
	for range 3 {
		flow, _ := openRouterFlow(t, http.StatusOK, map[string]any{"key": "k"})
		in := &recordingInteraction{}
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() { _, _ = flow.Login(ctx, in); close(done) }()
		waitForEvent(t, in, auth.EventAuthURL)

		var authURL string
		for _, ev := range in.seen() {
			if ev.Type == auth.EventAuthURL {
				authURL = ev.URL
			}
		}
		parsed, _ := url.Parse(authURL)
		callback, _ := url.Parse(parsed.Query().Get("callback_url"))
		if !strings.HasPrefix(callback.Path, "/oauth/callback/") || len(callback.Path) < 30 {
			t.Errorf("callback path %q is not an unguessable per-login value", callback.Path)
		}
		if seenPaths[callback.Path] {
			t.Errorf("callback path %q was reused across logins", callback.Path)
		}
		seenPaths[callback.Path] = true

		cancel()
		<-done
	}
}

// A wrong path must not be served, or the unguessable path buys nothing.
func TestOpenRouterRejectsTheWrongCallbackPath(t *testing.T) {
	flow, _ := openRouterFlow(t, http.StatusOK, map[string]any{"key": "k"})
	in := &recordingInteraction{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { _, _ = flow.Login(ctx, in); close(done) }()
	waitForEvent(t, in, auth.EventAuthURL)

	var authURL string
	for _, ev := range in.seen() {
		if ev.Type == auth.EventAuthURL {
			authURL = ev.URL
		}
	}
	parsed, _ := url.Parse(authURL)
	callback, _ := url.Parse(parsed.Query().Get("callback_url"))

	resp, err := http.Get("http://" + callback.Host + "/oauth/callback/guessed?code=x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a wrong callback path returned %d, want 404", resp.StatusCode)
	}

	cancel()
	<-done
}

// THE POINT: the exchange runs inside the handler so a failure is shown on the
// page the user is looking at, not only in a terminal they may have left.
func TestOpenRouterReportsAFailedExchangeInTheBrowser(t *testing.T) {
	flow, _ := openRouterFlow(t, http.StatusForbidden, map[string]any{
		"error": "invalid_grant", "error_description": "code already used",
	})
	in := &recordingInteraction{}

	done := make(chan struct{})
	var loginErr error
	go func() {
		_, loginErr = flow.Login(context.Background(), in)
		close(done)
	}()

	waitForEvent(t, in, auth.EventAuthURL)
	resp := visitCallback(t, in, "code=stale")
	body, _ := io.ReadAll(resp.Body)
	<-done

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("callback status %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(body), "code already used") {
		t.Errorf("the browser page did not explain the failure: %s", body)
	}
	if loginErr == nil || !strings.Contains(loginErr.Error(), "code already used") {
		t.Errorf("login error %v", loginErr)
	}
}

// A denial from the provider ends the login rather than hanging until timeout.
func TestOpenRouterSurfacesADeniedAuthorization(t *testing.T) {
	flow, _ := openRouterFlow(t, http.StatusOK, map[string]any{"key": "k"})
	in := &recordingInteraction{}

	done := make(chan struct{})
	var loginErr error
	go func() {
		_, loginErr = flow.Login(context.Background(), in)
		close(done)
	}()

	waitForEvent(t, in, auth.EventAuthURL)
	visitCallback(t, in, "error=access_denied")
	<-done

	if loginErr == nil || !strings.Contains(loginErr.Error(), "access_denied") {
		t.Errorf("login error %v — a denial must be reported, not waited out", loginErr)
	}
}

// A response with no key is a failed login, not an empty credential.
func TestOpenRouterRejectsAResponseWithNoKey(t *testing.T) {
	flow, _ := openRouterFlow(t, http.StatusOK, map[string]any{"not_a_key": "value"})
	in := &recordingInteraction{}

	done := make(chan struct{})
	var loginErr error
	go func() {
		_, loginErr = flow.Login(context.Background(), in)
		close(done)
	}()

	waitForEvent(t, in, auth.EventAuthURL)
	visitCallback(t, in, "code=the-code")
	<-done

	if loginErr == nil || !strings.Contains(loginErr.Error(), "no key") {
		t.Errorf("login error %v", loginErr)
	}
}

// The key cannot be refreshed and does not expire; returning it unchanged is
// the honest implementation.
func TestOpenRouterRefreshReturnsTheKeyUnchanged(t *testing.T) {
	cred := &auth.Credential{Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "sk-or-v1-x"}}
	got, err := NewOpenRouter().Refresh(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuth.Access != "sk-or-v1-x" {
		t.Errorf("access %q", got.OAuth.Access)
	}
	if _, err := NewOpenRouter().Refresh(context.Background(), nil); err == nil {
		t.Error("a nil credential must not refresh")
	}
}

func TestOpenRouterToAuth(t *testing.T) {
	got, err := NewOpenRouter().ToAuth(&auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "sk-or-v1-x"},
	})
	if err != nil || got.APIKey != "sk-or-v1-x" {
		t.Errorf("ToAuth = %+v, %v", got, err)
	}
	if _, err := NewOpenRouter().ToAuth(nil); err == nil {
		t.Error("a nil credential must not produce auth")
	}
}

// waitForEvent blocks until the login has told the user something of this kind.
func waitForEvent(t *testing.T, in *recordingInteraction, kind auth.EventType) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range in.seen() {
			if ev.Type == kind {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s event arrived", kind)
}
