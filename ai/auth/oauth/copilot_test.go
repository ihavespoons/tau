package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// recordingInteraction captures what the user would have been told.
type recordingInteraction struct{ events []auth.Event }

func (r *recordingInteraction) Prompt(context.Context, auth.Prompt) (string, error) { return "", nil }
func (r *recordingInteraction) Notify(ev auth.Event)                                { r.events = append(r.events, ev) }

// copilotServer stands in for github.com and the Copilot token endpoint. The
// flow spans three endpoints on two hosts, which is the shape worth testing.
type copilotServer struct {
	pollsBeforeSuccess int
	polls              int
	githubToken        string
	copilotToken       string
	expiresAt          int64
	lastAuthorization  string
	seenHeaders        http.Header
}

func (s *copilotServer) start(t *testing.T) *Copilot {
	t.Helper()
	if s.githubToken == "" {
		s.githubToken = "gho_github"
	}
	if s.copilotToken == "" {
		s.copilotToken = "tid=abc;exp=1;proxy-ep=proxy.individual.githubcopilot.com;"
	}
	if s.expiresAt == 0 {
		s.expiresAt = 1700000000
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/login/device/code"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-code",
				"user_code":        "ABCD-1234",
				"verification_uri": "https://github.com/login/device",
				"interval":         1,
				"expires_in":       900,
			})

		case strings.HasSuffix(r.URL.Path, "/login/oauth/access_token"):
			s.polls++
			if s.polls <= s.pollsBeforeSuccess {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": s.githubToken})

		case strings.HasSuffix(r.URL.Path, "/copilot_internal/v2/token"):
			s.lastAuthorization = r.Header.Get("Authorization")
			s.seenHeaders = r.Header.Clone()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": s.copilotToken, "expires_at": s.expiresAt,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// The token exchange lives on a different host in production, so it is
	// pointed at the same test server explicitly rather than derived.
	return &Copilot{
		BaseURL:    srv.URL,
		TokenURL:   srv.URL + "/copilot_internal/v2/token",
		HTTPClient: srv.Client(),
		Sleep:      func(context.Context, time.Duration) error { return nil },
	}
}

// THE POINT: Copilot's login is two exchanges. The device flow yields a GitHub
// token, which the Copilot API does NOT accept — it has to be traded for a
// short-lived Copilot token. Storing the wrong one produces a login that
// appears to succeed and then 401s on the first turn.
func TestCopilotLoginStoresBothTokens(t *testing.T) {
	server := &copilotServer{pollsBeforeSuccess: 2}
	flow := server.start(t)

	in := &recordingInteraction{}
	cred, err := flow.Login(context.Background(), in)
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}

	if cred.Type != auth.CredentialOAuth || cred.OAuth == nil {
		t.Fatalf("credential: %#v", cred)
	}
	// The Copilot token authenticates requests...
	if cred.OAuth.Access != server.copilotToken {
		t.Errorf("access token: %q, want the copilot token", cred.OAuth.Access)
	}
	// ...and the GitHub token is what mints the next one.
	if cred.OAuth.Refresh != server.githubToken {
		t.Errorf("refresh token: %q, want the github token", cred.OAuth.Refresh)
	}
	if cred.OAuth.Expires != server.expiresAt*1000 {
		t.Errorf("expires: %d, want milliseconds", cred.OAuth.Expires)
	}
	if server.lastAuthorization != "Bearer "+server.githubToken {
		t.Errorf("the exchange presented %q", server.lastAuthorization)
	}
}

// The code is useless without somewhere to type it, so the user must be told
// both — and told before the poll loop starts, not after.
func TestCopilotTellsTheUserTheCode(t *testing.T) {
	flow := (&copilotServer{}).start(t)

	in := &recordingInteraction{}
	if _, err := flow.Login(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	if len(in.events) == 0 {
		t.Fatal("the user was told nothing")
	}
	ev := in.events[0]
	if ev.UserCode != "ABCD-1234" {
		t.Errorf("user code: %q", ev.UserCode)
	}
	if ev.VerificationURI != "https://github.com/login/device" {
		t.Errorf("verification uri: %q", ev.VerificationURI)
	}
	if !strings.Contains(ev.Message, "ABCD-1234") {
		t.Errorf("the message should carry the code for a plain-text host: %q", ev.Message)
	}
}

// Refresh mints a new Copilot token from the stored GitHub one — and must not
// discard the GitHub token, which is not rotated and is needed again.
func TestCopilotRefreshKeepsTheGitHubToken(t *testing.T) {
	server := &copilotServer{copilotToken: "fresh-copilot-token", expiresAt: 1800000000}
	flow := server.start(t)

	cred := &auth.Credential{
		Type:  auth.CredentialOAuth,
		OAuth: &auth.OAuthData{Access: "stale", Refresh: "gho_github", Expires: 1},
	}

	updated, err := flow.Refresh(context.Background(), cred)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if updated.OAuth.Access != "fresh-copilot-token" {
		t.Errorf("access: %q", updated.OAuth.Access)
	}
	if updated.OAuth.Refresh != "gho_github" {
		t.Errorf("the github token was lost: %q", updated.OAuth.Refresh)
	}
	// The original must not be mutated: a failed write should leave the stored
	// credential exactly as it was.
	if cred.OAuth.Access != "stale" {
		t.Error("refresh mutated the credential it was given")
	}
}

// Every Copilot request has to present a recognised editor and integration, or
// the endpoint refuses it.
func TestCopilotAuthCarriesTheEditorHeaders(t *testing.T) {
	flow := NewCopilot()
	got, err := flow.ToAuth(&auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "tok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "tok" {
		t.Errorf("api key: %q", got.APIKey)
	}
	for _, header := range []string{"Editor-Version", "Copilot-Integration-Id"} {
		if got.Headers[header] == nil {
			t.Errorf("missing %s: %v", header, got.Headers)
		}
	}
}

func TestCopilotWithoutATokenFails(t *testing.T) {
	if _, err := NewCopilot().ToAuth(nil); err == nil {
		t.Error("a nil credential must not produce auth")
	}
	if _, err := NewCopilot().ToAuth(&auth.Credential{Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{}}); err == nil {
		t.Error("an empty token must not produce auth")
	}
}

// THE POINT: the verification URI is handed to the user's browser opener.
// Anything that is not a web URL could name a local executable.
func TestCopilotRejectsAnUntrustedVerificationURI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dev",
			"user_code":        "CODE",
			"verification_uri": "file:///bin/sh",
			"expires_in":       900,
		})
	}))
	defer srv.Close()

	flow := &Copilot{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := flow.Login(context.Background(), &recordingInteraction{})
	if err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Errorf("error: %v", err)
	}
}

// An incomplete response is a broken flow, not one to poll forever on.
func TestCopilotRejectsAnIncompleteDeviceResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"user_code": "CODE"})
	}))
	defer srv.Close()

	flow := &Copilot{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if _, err := flow.Login(context.Background(), &recordingInteraction{}); err == nil {
		t.Error("an incomplete device code response must fail")
	}
}

// A GitHub Enterprise host is accepted however the user spells it.
func TestCopilotDomainNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "github.com"},
		{"github.example.com", "github.example.com"},
		{"https://github.example.com", "github.example.com"},
		{"https://github.example.com/", "github.example.com"},
		{"  github.example.com  ", "github.example.com"},
	}
	for _, tc := range cases {
		if got := (&Copilot{Domain: tc.in}).domain(); got != tc.want {
			t.Errorf("domain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// THE POINT: a Copilot token names the proxy serving THIS account —
// individual, business, or enterprise. Using the wrong host is a 404, and the
// account type is not knowable any other way.
func TestBaseURLFromToken(t *testing.T) {
	cases := []struct{ token, want string }{
		{"tid=x;exp=1;proxy-ep=proxy.individual.githubcopilot.com;", "https://api.individual.githubcopilot.com"},
		{"tid=x;proxy-ep=proxy.business.githubcopilot.com;other=1", "https://api.business.githubcopilot.com"},
		{"tid=x;exp=1", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := BaseURLFromToken(tc.token); got != tc.want {
			t.Errorf("BaseURLFromToken(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}
