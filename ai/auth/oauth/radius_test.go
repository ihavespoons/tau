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

// radiusServer stands in for a gateway: discovery, device authorization, and
// the token endpoint all live on one host, as they do in production.
type radiusServer struct {
	authorizeEndpoint string
	device            map[string]any
	tokens            []response
	tokenIndex        int
	tokenForms        []url.Values
	discoveryStatus   int
}

func (s *radiusServer) start(t *testing.T) *Radius {
	t.Helper()
	if s.authorizeEndpoint == "" {
		s.authorizeEndpoint = "https://sso.example.com/authorize"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v1/oauth":
			if s.discoveryStatus != 0 {
				w.WriteHeader(s.discoveryStatus)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"authorizationEndpoint": s.authorizeEndpoint})

		case "/v1/oauth/device":
			_ = json.NewEncoder(w).Encode(s.device)

		case "/v1/oauth/token":
			s.tokenForms = append(s.tokenForms, r.PostForm)
			// See maxTestPolls: a flow that never terminates busy-spins rather
			// than hanging, so the cap is what makes it a visible failure.
			if len(s.tokenForms) > maxTestPolls {
				w.WriteHeader(http.StatusTeapot)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": "test_poll_limit", "error_description": "the flow polled without ever terminating",
				})
				return
			}
			next := s.tokens[min(s.tokenIndex, len(s.tokens)-1)]
			s.tokenIndex++
			w.WriteHeader(next.status)
			_ = json.NewEncoder(w).Encode(next.body)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return &Radius{
		ProviderName: "Radius", Gateway: srv.URL, HTTPClient: srv.Client(),
		Now:   func() time.Time { return time.Unix(1_700_000_000, 0) },
		Sleep: noSleep,
		// Short, so a flow that fails to terminate fails the test instead of
		// stalling the suite. Mutation testing hung a run for exactly that.
		LoginTimeout: 2 * time.Second,
	}
}

func okRadiusDevice() map[string]any {
	return map[string]any{
		"device_code": "device-1", "user_code": "RAD-1234",
		"verification_uri": "https://sso.example.com/device",
		"interval":         5, "expires_in": 600,
	}
}

func okRadiusToken() response {
	return response{http.StatusOK, map[string]any{
		"access_token": "access-1", "refresh_token": "refresh-1",
		"expires_in": 3600, "scope": "gateway offline_access",
	}}
}

// THE POINT: the device code is what works over SSH or in a container, where
// there is no browser and no way to reach a local callback port.
func TestRadiusDeviceCodeLogin(t *testing.T) {
	s := &radiusServer{device: okRadiusDevice(), tokens: []response{
		{http.StatusBadRequest, map[string]any{"error": "authorization_pending"}},
		okRadiusToken(),
	}}
	flow := s.start(t)

	in := &recordingInteraction{}
	in.answer = radiusMethodDeviceCode

	cred, err := flow.Login(context.Background(), in)
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if cred.OAuth.Access != "access-1" || cred.OAuth.Refresh != "refresh-1" {
		t.Errorf("credential %+v", cred.OAuth)
	}
	if got := s.tokenForms[0].Get("grant_type"); got != radiusDeviceGrant {
		t.Errorf("grant_type %q", got)
	}

	// The scope rides in Extra, which is flattened to the same top-level key
	// Pi writes — so the credential file stays readable by both.
	raw, err := json.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["scope"] != "gateway offline_access" {
		t.Errorf("credential JSON %s — scope must be a top-level key", raw)
	}
}

// THE POINT: the authorization endpoint is discovered, because a gateway
// deployment chooses its own identity provider. Hard-coding one would work for
// exactly one installation.
func TestRadiusBrowserLoginUsesTheDiscoveredEndpoint(t *testing.T) {
	if !portFree(t, radiusCallback) {
		t.Skipf("port %d is not bindable here", radiusCallback)
	}
	s := &radiusServer{
		authorizeEndpoint: "https://sso.example.com/oauth2/authorize",
		tokens:            []response{okRadiusToken()},
	}
	flow := s.start(t)

	in := &recordingInteraction{}
	in.answer = radiusMethodBrowser

	done := make(chan struct{})
	var cred *auth.Credential
	var loginErr error
	go func() {
		cred, loginErr = flow.Login(context.Background(), in)
		close(done)
	}()

	waitForEvent(t, in, auth.EventAuthURL)
	var authURL string
	for _, ev := range in.seen() {
		if ev.Type == auth.EventAuthURL {
			authURL = ev.URL
		}
	}
	if !strings.HasPrefix(authURL, "https://sso.example.com/oauth2/authorize?") {
		t.Fatalf("authorize URL %q", authURL)
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	for field, want := range map[string]string{
		"response_type":         "code",
		"client_id":             radiusClientID,
		"scope":                 radiusScope,
		"code_challenge_method": "S256",
		"handoff":               "url",
	} {
		if q.Get(field) != want {
			t.Errorf("%s = %q, want %q", field, q.Get(field), want)
		}
	}
	if q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Errorf("authorize URL is missing PKCE or state: %v", q)
	}

	// Complete the redirect the way the browser would.
	resp, err := http.Get(q.Get("redirect_uri") + "?code=the-code&state=" + q.Get("state"))
	if err != nil {
		t.Fatalf("visiting the callback failed: %v", err)
	}
	_ = resp.Body.Close()
	<-done

	if loginErr != nil {
		t.Fatalf("login failed: %v", loginErr)
	}
	if cred.OAuth.Access != "access-1" {
		t.Errorf("credential %+v", cred.OAuth)
	}
	form := s.tokenForms[0]
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "the-code" {
		t.Errorf("token form %v", form)
	}
	if form.Get("code_verifier") == "" {
		t.Error("the exchange omitted the PKCE verifier")
	}
}

// THE POINT: a mismatched state means the code belongs to a different login
// attempt. Accepting it would exchange a code the user did not just authorize.
func TestRadiusRejectsAMismatchedState(t *testing.T) {
	if !portFree(t, radiusCallback) {
		t.Skipf("port %d is not bindable here", radiusCallback)
	}
	s := &radiusServer{tokens: []response{okRadiusToken()}}
	flow := s.start(t)

	in := &recordingInteraction{}
	in.answer = radiusMethodBrowser

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
	redirect := parsed.Query().Get("redirect_uri")

	resp, err := http.Get(redirect + "?code=the-code&state=someone-elses")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a mismatched state returned %d, want 400", resp.StatusCode)
	}
	if len(s.tokenForms) != 0 {
		t.Error("a code from a different login attempt was exchanged")
	}

	cancel()
	<-done
}

// A gateway that cannot describe itself must say so, not fall through to a
// hard-coded endpoint.
func TestRadiusSurfacesADiscoveryFailure(t *testing.T) {
	s := &radiusServer{discoveryStatus: http.StatusInternalServerError, tokens: []response{okRadiusToken()}}
	flow := s.start(t)

	in := &recordingInteraction{}
	in.answer = radiusMethodBrowser

	_, err := flow.Login(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "OAuth config") {
		t.Errorf("error %v", err)
	}
}

// The discovered endpoint goes to the browser opener, so it gets the same
// scrutiny as a device flow's verification URI.
func TestRadiusRejectsAnUntrustedAuthorizationEndpoint(t *testing.T) {
	s := &radiusServer{authorizeEndpoint: "file:///etc/passwd", tokens: []response{okRadiusToken()}}
	flow := s.start(t)

	in := &recordingInteraction{}
	in.answer = radiusMethodBrowser

	_, err := flow.Login(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Errorf("error %v", err)
	}
}

// With nobody to ask, the device code is the safe default: it needs nothing
// local, where the browser flow needs one specific port on this machine.
func TestRadiusDefaultsToTheDeviceCodeWithoutAnInteraction(t *testing.T) {
	s := &radiusServer{device: okRadiusDevice(), tokens: []response{okRadiusToken()}}
	flow := s.start(t)

	cred, err := flow.Login(context.Background(), nil)
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if cred.OAuth.Access != "access-1" {
		t.Errorf("credential %+v", cred.OAuth)
	}
	if got := s.tokenForms[0].Get("grant_type"); got != radiusDeviceGrant {
		t.Errorf("grant_type %q, want the device grant", got)
	}
}

// An unknown answer must not silently pick a flow the user did not choose.
func TestRadiusRejectsAnUnknownMethod(t *testing.T) {
	s := &radiusServer{device: okRadiusDevice(), tokens: []response{okRadiusToken()}}
	flow := s.start(t)

	in := &recordingInteraction{}
	in.answer = "carrier-pigeon"

	_, err := flow.Login(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("error %v", err)
	}
}

// A gateway URL with a trailing slash must not produce a doubled path.
func TestRadiusNormalizesTheGatewayURL(t *testing.T) {
	flow := NewRadius("Radius", "https://gateway.example.com/")
	if got := flow.endpoint("/v1/oauth"); got != "https://gateway.example.com/v1/oauth" {
		t.Errorf("endpoint %q", got)
	}
}

// An authorization endpoint that already carries a query keeps it.
func TestRadiusAppendsToAnEndpointWithAQuery(t *testing.T) {
	flow := NewRadius("Radius", "https://gateway.example.com")
	got := flow.buildAuthorizeURL("https://sso.example.com/authorize?tenant=acme", "challenge", "state")
	if !strings.Contains(got, "tenant=acme") || !strings.Contains(got, "&response_type=code") {
		t.Errorf("authorize URL %q dropped or mangled the existing query", got)
	}
}

func TestRadiusToAuthAndMissingRefresh(t *testing.T) {
	flow := NewRadius("Radius", "https://gateway.example.com")
	got, err := flow.ToAuth(&auth.Credential{
		Type: auth.CredentialOAuth, OAuth: &auth.OAuthData{Access: "tok"},
	})
	if err != nil || got.APIKey != "tok" {
		t.Errorf("ToAuth = %+v, %v", got, err)
	}
	if _, err := flow.ToAuth(nil); err == nil {
		t.Error("a nil credential must not produce auth")
	}
	if _, err := flow.Refresh(context.Background(), nil); err == nil {
		t.Error("a nil credential must not refresh")
	}
}

// portFree reports whether the fixed callback port can be bound here.
func portFree(t *testing.T, port int) bool {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(callbackHost(), strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// THE POINT: a browser sign-in that is never completed must give up and say so.
// Waiting forever leaves `tau login` with nothing on screen and no way out but
// Ctrl-C — and the user has a working alternative in the device code.
//
// Mutation testing found this: routing an unknown method to the browser flow
// hung the whole test suite, because nothing bounded the wait.
func TestRadiusBrowserLoginTimesOut(t *testing.T) {
	if !portFree(t, radiusCallback) {
		t.Skipf("port %d is not bindable here", radiusCallback)
	}
	s := &radiusServer{tokens: []response{okRadiusToken()}}
	flow := s.start(t)
	flow.LoginTimeout = 150 * time.Millisecond

	in := &recordingInteraction{}
	in.answer = radiusMethodBrowser

	// The context deadline is a backstop well past the login timeout: if the
	// timeout is ever removed this test fails in a second instead of stalling
	// the suite until Go's own ten-minute limit.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	_, err := flow.Login(ctx, in)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "device code") {
		t.Errorf("error %v — the timeout should point at the working alternative", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the browser flow waited %s before giving up", elapsed)
	}
}
