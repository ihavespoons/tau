package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/ai/auth/oauth"
)

// DefaultRadiusGateway is the gateway a Radius login talks to unless told
// otherwise.
const DefaultRadiusGateway = "https://radius.pi.dev"

// RadiusProviderID is the provider id Radius credentials and catalogs are
// stored under. It matches Pi's, so an imported ~/.pi auth entry keeps working.
const RadiusProviderID = "radius"

// RadiusOptions configures a Radius gateway provider.
type RadiusOptions struct {
	// ID and Name default to "radius" and "Radius". They are configurable
	// because the gateway is a product someone else can deploy.
	ID   string
	Name string
	// Gateway is the gateway base URL; empty means DefaultRadiusGateway.
	Gateway string
	// Models is the catalog as last fetched from the gateway. Radius publishes
	// no static list — see FetchRadiusCatalog.
	Models []ai.Model
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
}

func (o RadiusOptions) id() string {
	if o.ID != "" {
		return o.ID
	}
	return RadiusProviderID
}

func (o RadiusOptions) name() string {
	if o.Name != "" {
		return o.Name
	}
	return "Radius"
}

func (o RadiusOptions) gateway() string {
	if o.Gateway == "" {
		return DefaultRadiusGateway
	}
	return NormalizeGatewayURL(o.Gateway)
}

func (o RadiusOptions) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// NormalizeGatewayURL accepts a bare host and trims trailing slashes, so a
// gateway written as "radius.example.com/" joins paths the same way one written
// in full does.
//
// A scheme-less value becomes https, never http: the value is about to receive
// an access token.
func NormalizeGatewayURL(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(trimmed), "http://") &&
		!strings.HasPrefix(strings.ToLower(trimmed), "https://") {
		trimmed = "https://" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

// Radius builds the gateway provider from a catalog someone else fetched.
//
// The catalog is an argument rather than something this function loads, because
// building a provider must not do I/O: tau assembles its whole provider list on
// every start, and a gateway that is slow or unreachable would delay a session
// that has nothing to do with Radius. The models come from the last successful
// fetch, and a login refreshes them.
func Radius(store auth.CredentialStore, env auth.EnvContext, o RadiusOptions) *Provider {
	id, name := o.id(), o.name()
	baseURL := o.gateway()
	if len(o.Models) > 0 {
		// The gateway names the base URL its models are served from, which is
		// not necessarily the gateway's own host.
		baseURL = o.Models[0].BaseURL
	}

	p := Keyed(store, env, KeyedOptions{
		ID: id, Name: name, BaseURL: baseURL,
		EnvKeys: auth.EnvKeysFor(id), Models: o.Models,
		OAuth: oauth.NewRadius(name, o.gateway()),
	})
	// With an empty catalog there is nothing to infer a wire from, and the
	// provider would report no API at all. Radius is pi-messages by definition.
	p.Api = ai.ApiPiMessages
	return p
}

// RadiusCatalog is one fetch of the gateway's model list.
type RadiusCatalog struct {
	// BaseURL is where the gateway serves its messages endpoint.
	BaseURL string
	Models  []ai.Model
	// Skipped counts entries the gateway published that tau could not use.
	// Reported rather than swallowed: a model silently missing from the picker
	// is a bug report that starts with "tau can't see the model I'm paying for".
	Skipped int
}

// gatewayConfig is the /v1/config response.
type gatewayConfig struct {
	BaseURL string `json:"baseUrl"`
	Models  []struct {
		ID               string              `json:"id"`
		Name             string              `json:"name"`
		Reasoning        bool                `json:"reasoning"`
		ThinkingLevelMap ai.ThinkingLevelMap `json:"thinkingLevelMap"`
		Input            []string            `json:"input"`
		Cost             ai.ModelCost        `json:"cost"`
		ContextWindow    int                 `json:"contextWindow"`
		MaxTokens        int                 `json:"maxTokens"`
	} `json:"models"`
}

// FetchRadiusCatalog reads the gateway's published model list.
//
// Radius has no compiled catalog: which models a user can reach is a property
// of their account, decided by the gateway. An API key is optional because a
// gateway may publish its list unauthenticated, but with one the list is the
// user's own.
func FetchRadiusCatalog(ctx context.Context, o RadiusOptions, apiKey string) (*RadiusCatalog, error) {
	gateway := o.gateway()
	endpoint := gateway + "/v1/config"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not load the %s catalog from %s: %w", o.name(), gateway, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("could not load the %s catalog from %s: %d %s",
			o.name(), gateway, resp.StatusCode, truncateBody(string(body)))
	}

	var cfg gatewayConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("invalid %s catalog from %s: %w", o.name(), gateway, err)
	}
	if err := validateCatalogBaseURL(gateway, cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("invalid %s catalog from %s: %w", o.name(), gateway, err)
	}

	out := &RadiusCatalog{BaseURL: NormalizeGatewayURL(cfg.BaseURL)}
	for _, m := range cfg.Models {
		// A model missing its identity cannot be selected, and one with no
		// context window or output budget breaks the arithmetic every turn
		// depends on. Dropping it beats catalogueing something unusable.
		if m.ID == "" || m.Name == "" || m.ContextWindow <= 0 || m.MaxTokens <= 0 {
			out.Skipped++
			continue
		}
		out.Models = append(out.Models, ai.Model{
			ID: m.ID, Name: m.Name, Api: ai.ApiPiMessages, Provider: o.id(),
			BaseURL:          out.BaseURL,
			Reasoning:        m.Reasoning,
			ThinkingLevelMap: m.ThinkingLevelMap,
			Input:            m.Input,
			Cost:             m.Cost,
			ContextWindow:    m.ContextWindow,
			MaxTokens:        m.MaxTokens,
		})
	}
	return out, nil
}

// validateCatalogBaseURL checks where the gateway is asking tau to send
// conversations.
//
// The base URL arrives from the network and every request to it carries the
// user's access token and the whole transcript. Pi takes it verbatim. tau
// requires an absolute http(s) URL, and refuses to be downgraded from an https
// gateway to a plaintext endpoint — which is the one case where a compromised
// or misconfigured response would put a live token on the wire in clear.
func validateCatalogBaseURL(gateway, baseURL string) error {
	if baseURL == "" {
		return fmt.Errorf("no baseUrl")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("baseUrl %q is not an absolute URL", baseURL)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("baseUrl %q does not use http or https", baseURL)
	}
	if strings.HasPrefix(strings.ToLower(gateway), "https://") && parsed.Scheme != "https" {
		return fmt.Errorf("baseUrl %q would downgrade an https gateway to plaintext", baseURL)
	}
	return nil
}

func truncateBody(body string) string {
	trimmed := strings.TrimSpace(body)
	if len(trimmed) > 512 {
		return trimmed[:512] + "…"
	}
	return trimmed
}
