package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai/auth"
)

// --- PKCE ---

func TestGeneratePKCE(t *testing.T) {
	p, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	// 32 random bytes, base64url, unpadded.
	if strings.ContainsAny(p.Verifier, "+/=") {
		t.Errorf("verifier not base64url-clean: %q", p.Verifier)
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Verifier)
	if err != nil || len(raw) != 32 {
		t.Errorf("verifier decode: %d bytes, err=%v", len(raw), err)
	}
	if strings.ContainsAny(p.Challenge, "+/=") {
		t.Errorf("challenge not base64url-clean: %q", p.Challenge)
	}
	if p.Challenge != Challenge(p.Verifier) {
		t.Error("challenge does not match verifier")
	}
	other, _ := GeneratePKCE()
	if other.Verifier == p.Verifier {
		t.Error("verifier is not random")
	}
}

func TestChallengeKnownVector(t *testing.T) {
	// RFC 7636 appendix B.
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := Challenge(verifier); got != want {
		t.Errorf("Challenge = %q, want %q", got, want)
	}
}

// --- token requests ---

type capturedRequest struct {
	method      string
	contentType string
	accept      string
	body        map[string]any
}

func tokenServer(t *testing.T, resp map[string]any, captured *capturedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if captured != nil {
			captured.method = r.Method
			captured.contentType = r.Header.Get("Content-Type")
			captured.accept = r.Header.Get("Accept")
			_ = json.Unmarshal(b, &captured.body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestExchangeCodeRequestShape(t *testing.T) {
	var got capturedRequest
	srv := tokenServer(t, map[string]any{
		"access_token":  "sk-ant-oat-new",
		"refresh_token": "sk-ant-ort-new",
		"expires_in":    3600,
	}, &got)
	defer srv.Close()

	fixed := time.Unix(1_700_000_000, 0)
	a := &Anthropic{TokenURL: srv.URL, HTTPClient: srv.Client(), Now: func() time.Time { return fixed }}

	cred, err := a.exchangeCode(context.Background(), "the-code", "the-state", "the-verifier", anthropicRedirectURI)
	if err != nil {
		t.Fatal(err)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %s", got.method)
	}
	if got.contentType != "application/json" || got.accept != "application/json" {
		t.Errorf("headers: content-type=%q accept=%q", got.contentType, got.accept)
	}
	want := map[string]any{
		"grant_type":    "authorization_code",
		"client_id":     anthropicClientID,
		"code":          "the-code",
		"state":         "the-state",
		"redirect_uri":  anthropicRedirectURI,
		"code_verifier": "the-verifier",
	}
	if len(got.body) != len(want) {
		t.Errorf("body has %d fields, want %d: %+v", len(got.body), len(want), got.body)
	}
	for k, v := range want {
		if got.body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, got.body[k], v)
		}
	}

	// expires = now + expires_in - 5min margin.
	wantExpires := fixed.Add(3600*time.Second - 5*time.Minute).UnixMilli()
	if cred.OAuth.Expires != wantExpires {
		t.Errorf("expires = %d, want %d", cred.OAuth.Expires, wantExpires)
	}
	if cred.Type != auth.CredentialOAuth || cred.OAuth.Access != "sk-ant-oat-new" || cred.OAuth.Refresh != "sk-ant-ort-new" {
		t.Errorf("cred = %+v / %+v", cred, cred.OAuth)
	}
}

func TestRefreshRequestShape(t *testing.T) {
	var got capturedRequest
	srv := tokenServer(t, map[string]any{
		"access_token":  "access-2",
		"refresh_token": "refresh-2",
		"expires_in":    7200,
		"scope":         "user:inference",
	}, &got)
	defer srv.Close()

	fixed := time.Unix(1_700_000_000, 0)
	a := &Anthropic{TokenURL: srv.URL, HTTPClient: srv.Client(), Now: func() time.Time { return fixed }}

	cred, err := a.Refresh(context.Background(), &auth.Credential{
		Type:  auth.CredentialOAuth,
		OAuth: &auth.OAuthData{Access: "old", Refresh: "the-refresh-token", Expires: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"grant_type":    "refresh_token",
		"client_id":     anthropicClientID,
		"refresh_token": "the-refresh-token",
	}
	if len(got.body) != len(want) {
		t.Errorf("body has %d fields, want %d: %+v", len(got.body), len(want), got.body)
	}
	for k, v := range want {
		if got.body[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, got.body[k], v)
		}
	}
	if cred.OAuth.Access != "access-2" || cred.OAuth.Refresh != "refresh-2" {
		t.Errorf("cred = %+v", cred.OAuth)
	}
	if want := fixed.Add(7200*time.Second - 5*time.Minute).UnixMilli(); cred.OAuth.Expires != want {
		t.Errorf("expires = %d, want %d", cred.OAuth.Expires, want)
	}
}

func TestRefreshHTTPErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()

	a := &Anthropic{TokenURL: srv.URL, HTTPClient: srv.Client()}
	_, err := a.Refresh(context.Background(), &auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Refresh: "r"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "status=400") {
		t.Errorf("err = %v", err)
	}
}

func TestRefreshMissingTokenErrors(t *testing.T) {
	a := NewAnthropic()
	if _, err := a.Refresh(context.Background(), nil); err == nil {
		t.Error("expected error for nil credential")
	}
	if _, err := a.Refresh(context.Background(), &auth.Credential{Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{}}); err == nil {
		t.Error("expected error for empty refresh token")
	}
}

func TestToAuthUsesAccessTokenAsAPIKey(t *testing.T) {
	a := NewAnthropic()
	ma, err := a.ToAuth(&auth.Credential{Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "sk-ant-oat-x"}})
	if err != nil {
		t.Fatal(err)
	}
	if ma.APIKey != "sk-ant-oat-x" || len(ma.Headers) != 0 || ma.BaseURL != "" {
		t.Errorf("ModelAuth = %+v", ma)
	}
	if _, err := a.ToAuth(nil); err == nil {
		t.Error("expected error for nil credential")
	}
}

// --- manual input parsing ---

func TestParseAuthorizationInput(t *testing.T) {
	cases := []struct {
		in         string
		code, want string
	}{
		{"http://localhost:53692/callback?code=abc&state=xyz", "abc", "xyz"},
		{"abc#xyz", "abc", "xyz"},
		{"code=abc&state=xyz", "abc", "xyz"},
		{"  bare-code  ", "bare-code", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		got := parseAuthorizationInput(c.in)
		if got.Code != c.code || got.State != c.want {
			t.Errorf("parse(%q) = %+v, want code=%q state=%q", c.in, got, c.code, c.want)
		}
	}
}

// --- login ---

type scriptedInteraction struct {
	mu      sync.Mutex
	events  []auth.Event
	input   string
	err     error
	delay   time.Duration
	prompts atomic.Int32
}

func (s *scriptedInteraction) Prompt(ctx context.Context, _ auth.Prompt) (string, error) {
	s.prompts.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return s.input, s.err
}

func (s *scriptedInteraction) Notify(ev auth.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *scriptedInteraction) authURL(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.Type == auth.EventAuthURL {
			return ev.URL
		}
	}
	t.Fatal("no auth_url event emitted")
	return ""
}

func TestLoginManualPasteFlow(t *testing.T) {
	var got capturedRequest
	srv := tokenServer(t, map[string]any{
		"access_token": "acc", "refresh_token": "ref", "expires_in": 3600,
	}, &got)
	defer srv.Close()

	a := &Anthropic{TokenURL: srv.URL, HTTPClient: srv.Client(), DisableCallbackServer: true}
	in := &scriptedInteraction{input: "the-code"}

	cred, err := a.Login(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if cred.OAuth.Access != "acc" {
		t.Errorf("cred = %+v", cred.OAuth)
	}

	// The authorize URL must carry Pi's exact parameter set.
	u := in.authURL(t)
	if !strings.HasPrefix(u, anthropicAuthorizeURL+"?") {
		t.Fatalf("authorize URL = %q", u)
	}
	q := u[len(anthropicAuthorizeURL)+1:]
	for _, want := range []string{
		"code=true",
		"client_id=" + anthropicClientID,
		"response_type=code",
		"code_challenge_method=S256",
		"redirect_uri=http%3A%2F%2Flocalhost%3A53692%2Fcallback",
		"scope=org%3Acreate_api_key",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("authorize query missing %q: %s", want, q)
		}
	}
	// state is the PKCE verifier, and the exchange must send the same value.
	if got.body["state"] != got.body["code_verifier"] {
		t.Errorf("state (%v) should equal code_verifier (%v)", got.body["state"], got.body["code_verifier"])
	}
	if got.body["code"] != "the-code" {
		t.Errorf("exchanged code = %v", got.body["code"])
	}
}

func TestLoginManualPasteStateMismatchRejected(t *testing.T) {
	srv := tokenServer(t, map[string]any{"access_token": "a", "refresh_token": "r", "expires_in": 1}, nil)
	defer srv.Close()

	a := &Anthropic{TokenURL: srv.URL, HTTPClient: srv.Client(), DisableCallbackServer: true}
	in := &scriptedInteraction{input: "the-code#wrong-state"}

	if _, err := a.Login(context.Background(), in); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("err = %v, want state mismatch", err)
	}
}

func TestLoginPromptErrorPropagates(t *testing.T) {
	a := &Anthropic{DisableCallbackServer: true}
	in := &scriptedInteraction{err: errors.New("user cancelled")}
	if _, err := a.Login(context.Background(), in); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v", err)
	}
}

func TestLoginCallbackServerWins(t *testing.T) {
	var got capturedRequest
	srv := tokenServer(t, map[string]any{
		"access_token": "cb-acc", "refresh_token": "cb-ref", "expires_in": 3600,
	}, &got)
	defer srv.Close()

	a := &Anthropic{TokenURL: srv.URL, HTTPClient: srv.Client()}
	// The manual prompt never resolves on its own; the callback must win.
	in := &scriptedInteraction{input: "unused", delay: 10 * time.Second}

	done := make(chan struct{})
	var cred *auth.Credential
	var loginErr error
	go func() {
		defer close(done)
		cred, loginErr = a.Login(context.Background(), in)
	}()

	// Drive the callback once the authorize URL (and thus the listener) exists.
	var callbackURL string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		in.mu.Lock()
		for _, ev := range in.events {
			if ev.Type == auth.EventAuthURL {
				callbackURL = ev.URL
			}
		}
		in.mu.Unlock()
		if callbackURL != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if callbackURL == "" {
		t.Fatal("no auth_url emitted")
	}
	state := stateFromAuthorizeURL(t, callbackURL)

	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		resp, err = http.Get(anthropicRedirectURI + "?code=cb-code&state=" + state)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Skipf("callback port unavailable in this environment: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Authentication complete") {
		t.Errorf("callback response: status=%d body=%s", resp.StatusCode, body)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("login did not complete after callback")
	}
	if loginErr != nil {
		t.Fatal(loginErr)
	}
	if cred.OAuth.Access != "cb-acc" {
		t.Errorf("cred = %+v", cred.OAuth)
	}
	if got.body["code"] != "cb-code" {
		t.Errorf("exchanged code = %v, want the callback code", got.body["code"])
	}
}

func TestCallbackServerRejectsBadState(t *testing.T) {
	cs, err := startCallbackServer("expected-state")
	if err != nil {
		t.Skipf("callback port unavailable: %v", err)
	}
	defer cs.Close()

	resp, err := http.Get(anthropicRedirectURI + "?code=c&state=wrong")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "State mismatch") {
		t.Errorf("status=%d body=%s", resp.StatusCode, body)
	}

	resp, err = http.Get(anthropicRedirectURI + "?error=access_denied")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "access_denied") {
		t.Errorf("status=%d body=%s", resp.StatusCode, body)
	}
}

func stateFromAuthorizeURL(t *testing.T, u string) string {
	t.Helper()
	idx := strings.Index(u, "?")
	if idx < 0 {
		t.Fatalf("no query in %q", u)
	}
	for _, kv := range strings.Split(u[idx+1:], "&") {
		if strings.HasPrefix(kv, "state=") {
			return strings.TrimPrefix(kv, "state=")
		}
	}
	t.Fatalf("no state in %q", u)
	return ""
}

// --- store integration ---

func TestLoginPersistsAndAccessRefreshesOnce(t *testing.T) {
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if body["grant_type"] == "refresh_token" {
			refreshes.Add(1)
			time.Sleep(5 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access", "refresh_token": "fresh-refresh", "expires_in": 7200,
		})
	}))
	defer srv.Close()

	a := &Anthropic{TokenURL: srv.URL, HTTPClient: srv.Client(), DisableCallbackServer: true}
	store := auth.NewMemStore()
	ctx := context.Background()

	if err := Login(ctx, a, store, "anthropic", &scriptedInteraction{input: "code"}); err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Read(ctx, "anthropic")
	if stored == nil || stored.Type != auth.CredentialOAuth || stored.OAuth.Access != "fresh-access" {
		t.Fatalf("stored = %+v", stored)
	}

	// Force expiry, then hammer Access concurrently: exactly one refresh.
	if _, err := store.Modify(ctx, "anthropic", func(c *auth.Credential) (*auth.Credential, error) {
		c.OAuth.Expires = time.Now().Add(time.Minute).UnixMilli()
		return c, nil
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	tokens := make([]string, 5)
	errs := make([]error, 5)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tokens[i], errs[i] = Access(ctx, a, store, "anthropic")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("access %d: %v", i, err)
		}
		if tokens[i] != "fresh-access" {
			t.Errorf("access %d token = %q", i, tokens[i])
		}
	}
	if n := refreshes.Load(); n != 1 {
		t.Errorf("refreshed %d times, want exactly 1", n)
	}
}

func TestAccessWithoutCredentialErrors(t *testing.T) {
	_, err := Access(context.Background(), NewAnthropic(), auth.NewMemStore(), "anthropic")
	if err == nil {
		t.Error("expected error when no credential is stored")
	}
}
