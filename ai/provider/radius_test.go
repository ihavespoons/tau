package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
)

// gatewayServer serves /v1/config.
type gatewayServer struct {
	config   any
	status   int
	body     string
	requests []*http.Request
}

func (g *gatewayServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests = append(g.requests, r)
		if g.status != 0 && g.status != http.StatusOK {
			w.WriteHeader(g.status)
			_, _ = w.Write([]byte(g.body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.config)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func okCatalog(baseURL string) map[string]any {
	return map[string]any{
		"baseUrl": baseURL,
		"models": []map[string]any{{
			"id": "claude-sonnet-4-5", "name": "Claude Sonnet 4.5", "reasoning": true,
			"input": []string{"text", "image"},
			"cost": map[string]any{
				"input": 3.0, "output": 15.0, "cacheRead": 0.3, "cacheWrite": 3.75,
			},
			"contextWindow": 200000, "maxTokens": 64000,
			"thinkingLevelMap": map[string]any{"medium": "medium"},
		}},
	}
}

func fetch(t *testing.T, g *gatewayServer, apiKey string) (*RadiusCatalog, error) {
	t.Helper()
	url := g.start(t)
	if g.config == nil && g.status == 0 {
		g.config = okCatalog(url)
	}
	return FetchRadiusCatalog(context.Background(), RadiusOptions{Gateway: url}, apiKey)
}

// THE POINT: Radius ships no compiled catalog — which models a user can reach
// is a property of their account, so the gateway's list IS the catalog.
func TestTheGatewayCatalogBecomesModels(t *testing.T) {
	g := &gatewayServer{}
	catalog, err := fetch(t, g, "token-1")
	if err != nil {
		t.Fatal(err)
	}

	if len(catalog.Models) != 1 {
		t.Fatalf("models %+v", catalog.Models)
	}
	m := catalog.Models[0]
	if m.ID != "claude-sonnet-4-5" || m.Name != "Claude Sonnet 4.5" {
		t.Errorf("model %+v", m)
	}
	// The wire is not something the gateway gets to choose: a Radius model is
	// reached over pi-messages by definition.
	if m.Api != ai.ApiPiMessages || m.Provider != RadiusProviderID {
		t.Errorf("model %+v", m)
	}
	if m.BaseURL != catalog.BaseURL {
		t.Errorf("baseURL %q, want the catalog's %q", m.BaseURL, catalog.BaseURL)
	}
	if !m.Reasoning || m.ContextWindow != 200000 || m.MaxTokens != 64000 {
		t.Errorf("model %+v", m)
	}
	if m.Cost.Input != 3.0 || m.Cost.CacheWrite != 3.75 {
		t.Errorf("cost %+v", m.Cost)
	}
	if levels := ai.SupportedThinkingLevels(&m); len(levels) == 0 {
		t.Error("the thinking-level map did not survive the fetch")
	}

	// The token is what makes the list the user's own rather than a public one.
	if got := g.requests[0].Header.Get("Authorization"); got != "Bearer token-1" {
		t.Errorf("Authorization %q", got)
	}
	if !strings.HasSuffix(g.requests[0].URL.Path, "/v1/config") {
		t.Errorf("path %q", g.requests[0].URL.Path)
	}
}

// A gateway may publish its list unauthenticated, so no credential is not a
// reason to skip the fetch — but tau must not send an empty bearer token.
func TestAnUnauthenticatedFetchSendsNoAuthorization(t *testing.T) {
	g := &gatewayServer{}
	if _, err := fetch(t, g, ""); err != nil {
		t.Fatal(err)
	}
	if got := g.requests[0].Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization %q, want none", got)
	}
}

// THE POINT: a model with no context window or output budget breaks the
// arithmetic every turn depends on, and one with no id cannot be selected.
// Dropping it beats catalogueing something unusable — but the count is
// reported, because a model silently missing from the picker is a bug report
// that starts with "tau can't see the model I'm paying for".
func TestIncompleteModelsAreSkippedAndCounted(t *testing.T) {
	g := &gatewayServer{}
	url := g.start(t)
	g.config = map[string]any{
		"baseUrl": url,
		"models": []map[string]any{
			{"id": "", "name": "No Id", "contextWindow": 1000, "maxTokens": 100},
			{"id": "no-name", "contextWindow": 1000, "maxTokens": 100},
			{"id": "no-window", "name": "No Window", "maxTokens": 100},
			{"id": "no-output", "name": "No Output", "contextWindow": 1000},
			{"id": "good", "name": "Good", "contextWindow": 1000, "maxTokens": 100},
		},
	}

	catalog, err := FetchRadiusCatalog(context.Background(), RadiusOptions{Gateway: url}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].ID != "good" {
		t.Errorf("models %+v", catalog.Models)
	}
	if catalog.Skipped != 4 {
		t.Errorf("skipped %d, want 4", catalog.Skipped)
	}
}

// THE POINT: the base URL arrives from the network, and every request to it
// carries the user's access token and the whole transcript. An https gateway
// that answers with an http endpoint would put a live token on the wire in
// clear — so that answer is refused rather than followed.
func TestABaseURLThatDowngradesTheGatewayIsRefused(t *testing.T) {
	_, err := FetchRadiusCatalog(context.Background(), RadiusOptions{
		Gateway:    "https://radius.example.com",
		HTTPClient: stubClient(t, okCatalog("http://radius.example.com/v1")),
	}, "token")
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Errorf("error %v", err)
	}
}

// Each of these is refused by a different rule, and each says which — "no
// baseUrl" and "not an absolute URL" send an operator to different places in
// their gateway config.
func TestABaseURLThatIsNotAURLIsRefused(t *testing.T) {
	cases := map[string]string{
		"":                    "no baseUrl",
		"not a url":           "not an absolute URL",
		"file:///etc/passwd":  "not an absolute URL",
		"javascript:alert(1)": "not an absolute URL",
		// An https URL with no host at all: it passes the scheme check and the
		// downgrade check, and would be POSTed to as "https:///v1/messages".
		"https:///v1": "not an absolute URL",
	}
	for bad, want := range cases {
		_, err := FetchRadiusCatalog(context.Background(), RadiusOptions{
			Gateway:    "https://radius.example.com",
			HTTPClient: stubClient(t, okCatalog(bad)),
		}, "token")
		if err == nil {
			t.Errorf("baseUrl %q was accepted", bad)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("baseUrl %q: error %q, want it to say %q", bad, err, want)
		}
	}
}

// THE POINT: the https-downgrade check hides this one. A URL with a real host
// and a scheme that is not http — ftp://, ws://, anything a Go transport will
// not POST to — is refused on its own merits, and an http gateway is where that
// matters, because there is no downgrade for the other check to catch.
//
// Mutation testing found this: widening the scheme list left every test
// passing, because each case in them was rejected by a different rule.
func TestANonHTTPSchemeIsRefusedEvenWithoutADowngrade(t *testing.T) {
	for _, bad := range []string{"ftp://gateway.internal/v1", "ws://gateway.internal/v1"} {
		_, err := FetchRadiusCatalog(context.Background(), RadiusOptions{
			Gateway:    "http://gateway.internal",
			HTTPClient: stubClient(t, okCatalog(bad)),
		}, "token")
		if err == nil || !strings.Contains(err.Error(), "http or https") {
			t.Errorf("baseUrl %q: error %v", bad, err)
		}
	}
}

// An http gateway is a self-hosted deployment on an internal network; it may
// legitimately serve an http endpoint.
func TestAnHTTPGatewayMayServeAnHTTPBaseURL(t *testing.T) {
	catalog, err := FetchRadiusCatalog(context.Background(), RadiusOptions{
		Gateway:    "http://gateway.internal",
		HTTPClient: stubClient(t, okCatalog("http://gateway.internal/v1")),
	}, "token")
	if err != nil {
		t.Fatal(err)
	}
	if catalog.BaseURL != "http://gateway.internal/v1" {
		t.Errorf("baseURL %q", catalog.BaseURL)
	}
}

func TestAFailedFetchNamesTheGateway(t *testing.T) {
	g := &gatewayServer{status: http.StatusUnauthorized, body: `{"error":"expired token"}`}
	url := g.start(t)

	_, err := FetchRadiusCatalog(context.Background(), RadiusOptions{Gateway: url}, "stale")
	if err == nil {
		t.Fatal("a 401 must fail")
	}
	for _, want := range []string{"Radius", url, "401", "expired token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err.Error(), want)
		}
	}
}

// A gateway written as a bare host must still work: it is a value users type.
func TestGatewayURLsAreNormalized(t *testing.T) {
	cases := map[string]string{
		"radius.pi.dev":              "https://radius.pi.dev",
		"https://radius.pi.dev/":     "https://radius.pi.dev",
		"https://radius.pi.dev///":   "https://radius.pi.dev",
		"http://gateway.internal:80": "http://gateway.internal:80",
		"  radius.pi.dev  ":          "https://radius.pi.dev",
		"":                           "",
	}
	for in, want := range cases {
		if got := NormalizeGatewayURL(in); got != want {
			t.Errorf("NormalizeGatewayURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE POINT: building the provider must not do I/O. tau assembles its whole
// provider list on every start, and a gateway that is slow or unreachable would
// delay a session that has nothing to do with Radius.
func TestBuildingTheProviderDoesNotCallTheGateway(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	p := Radius(auth.NewMemStore(), auth.MapContext{}, RadiusOptions{Gateway: srv.URL})
	if called {
		t.Error("building the provider reached the gateway")
	}
	if p.ID != RadiusProviderID || p.Name != "Radius" {
		t.Errorf("provider %s/%s", p.ID, p.Name)
	}
	// An empty catalog must still report the wire: a provider with no API at
	// all is listed as unreachable rather than as awaiting a login.
	if p.Api != ai.ApiPiMessages {
		t.Errorf("api %q", p.Api)
	}
	if len(p.Models) != 0 {
		t.Errorf("models %+v — an unlogged-in Radius has none", p.Models)
	}
}

// The provider streams against the base URL the gateway published, not the
// gateway's own host: they need not be the same service.
func TestTheProviderUsesTheCatalogsBaseURL(t *testing.T) {
	models := []ai.Model{{
		ID: "m", Name: "M", Api: ai.ApiPiMessages, Provider: RadiusProviderID,
		BaseURL: "https://edge.radius.example/v1", ContextWindow: 1000, MaxTokens: 100,
	}}
	p := Radius(auth.NewMemStore(), auth.MapContext{}, RadiusOptions{
		Gateway: "https://radius.example.com", Models: models,
	})
	if p.BaseURL != "https://edge.radius.example/v1" {
		t.Errorf("baseURL %q", p.BaseURL)
	}
	if p.Model("m") == nil {
		t.Error("the cached catalog is not selectable")
	}
}

// stubClient answers any request with the given JSON body, so a test can drive
// the parsing without caring which host it names.
func stubClient(t *testing.T, body any) *http.Client {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
