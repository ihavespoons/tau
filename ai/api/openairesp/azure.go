package openairesp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

// Azure resells OpenAI's models over the same responses protocol, so it lives
// here rather than in a package of its own: the message conversion, the tool
// conversion, and the whole stream state machine are identical, and a second
// copy would drift.
//
// What differs is entirely transport and addressing — how the endpoint is
// spelled, what authenticates, and what a model is called once deployed.

// azureAPIVersion is the default; Azure pins its surface by version and a
// deployment may be on an older one.
const azureAPIVersion = "v1"

// AzureOptions configures the Azure transport. Everything not named here is
// shared with the OpenAI wire.
type AzureOptions struct {
	Options
	// APIVersion overrides the api-version query parameter.
	APIVersion string
	// ResourceName names the Azure resource, from which the endpoint is
	// derived when no explicit base URL is given.
	ResourceName string
	// BaseURL is the full endpoint, overriding ResourceName.
	BaseURL string
	// DeploymentName is what the model is called in this Azure resource.
	DeploymentName string
}

// StreamAzure runs one turn against an Azure OpenAI deployment.
func StreamAzure(ctx context.Context, model *ai.Model, c ai.Context, opts *AzureOptions) *ai.MessageStream {
	if opts == nil {
		opts = &AzureOptions{}
	}
	stream := ai.NewMessageStream()
	go runAzure(ctx, stream, model, c, opts)
	return stream
}

// StreamSimpleAzure is StreamAzure with normalized cross-provider options.
func StreamSimpleAzure(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	return StreamAzure(ctx, model, c, &AzureOptions{
		Options: Options{
			StreamOptions: opts.StreamOptions,
			Reasoning:     clampReasoning(model, opts.Reasoning),
		},
	})
}

func runAzure(ctx context.Context, stream *ai.MessageStream, model *ai.Model, c ai.Context, opts *AzureOptions) {
	out := newOutput(model)

	defer func() {
		if r := recover(); r != nil {
			fail(stream, out, ctx, fmt.Errorf("azure openai responses: %v", r))
		}
	}()

	endpoint, err := azureEndpoint(model, opts)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}
	if opts.APIKey == "" {
		fail(stream, out, ctx, fmt.Errorf("no API key for provider %s", model.Provider))
		return
	}

	cm := resolveAzureCompat(model)
	// Which tools are grammar tools is decided once, from the tool definitions.
	// Both the request and the RESPONSE need it: a replayed call carries only a
	// name and arguments, and a streamed custom tool call carries only a name
	// and raw text — neither says which argument that text belongs in.
	grammar, err := apishared.GrammarToolInputProperties(c.Tools, cm.SupportsOpenAIGrammarTools)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}
	req, err := buildRequest(model, c, &opts.Options, cm, grammar)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}
	// A deployment is addressed by its own name, which need not match the
	// model id — the same model can be deployed twice under different names.
	req.Model = azureDeployment(model, opts)
	// Azure's responses surface does not carry the retention controls; sending
	// them is rejected rather than ignored.
	req.PromptCacheRetention = ""
	req.PromptCacheOptions = nil

	body, err2 := encodePayload(req, model, &opts.Options)
	if err = err2; err != nil {
		fail(stream, out, ctx, err)
		return
	}

	// Retried on the same policy every other wire uses: a 429 or a 5xx from a
	// gateway is routine, and without this one costs the whole turn.
	resp, err := apishared.RetryRequest(ctx, func() (*http.Response, error) {
		return doAzureRequest(ctx, endpoint, model, opts, body)
	}, opts.MaxRetries, opts.MaxRetryDelayMs)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if opts.OnResponse != nil {
		if err := opts.OnResponse(ai.ProviderResponse{Status: resp.StatusCode, Headers: resp.Header}, model); err != nil {
			fail(stream, out, ctx, err)
			return
		}
	}
	if resp.StatusCode != http.StatusOK {
		fail(stream, out, ctx, providerError(resp))
		return
	}

	stream.Push(ai.Event{Type: ai.EventStart, Partial: out})

	if err := consume(ctx, resp.Body, stream, out, model, &opts.Options, grammar); err != nil {
		fail(stream, out, ctx, err)
		return
	}
	stream.Push(ai.Event{Type: ai.EventDone, Reason: out.StopReason, Message: out})
}

// resolveAzureCompat differs from the OpenAI default in one place: Azure
// deployments accept strict tool schemas, and the catalog does not say so per
// model because it is a property of the surface rather than the model.
func resolveAzureCompat(model *ai.Model) compat {
	cm := resolveCompat(model)
	if model.Compat == nil || model.Compat.SupportsStrictMode == nil {
		cm.SupportsStrictMode = true
	}
	// Tool search and session affinity are not part of Azure's surface.
	cm.SupportsToolSearch = false
	return cm
}

// azureDeployment resolves what this model is called in the target resource.
//
// AZURE_OPENAI_DEPLOYMENT_NAME_MAP maps model ids to deployment names —
// "gpt-5.4=my-gpt54,gpt-5.5=my-gpt55" — because an organisation's deployment
// names rarely match the model ids, and requiring a models.json entry per
// deployment would be a lot of ceremony for a rename.
func azureDeployment(model *ai.Model, opts *AzureOptions) string {
	if opts.DeploymentName != "" {
		return opts.DeploymentName
	}
	for _, entry := range strings.Split(apishared.EnvValue(opts.Env, "AZURE_OPENAI_DEPLOYMENT_NAME_MAP"), ",") {
		id, name, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(id) == model.ID {
			if name = strings.TrimSpace(name); name != "" {
				return name
			}
		}
	}
	return model.ID
}

// azureEndpoint builds the full request URL, api-version included.
func azureEndpoint(model *ai.Model, opts *AzureOptions) (string, error) {
	base := firstNonEmpty(
		strings.TrimSpace(opts.BaseURL),
		strings.TrimSpace(apishared.EnvValue(opts.Env, "AZURE_OPENAI_BASE_URL")),
	)
	if base == "" {
		if resource := firstNonEmpty(opts.ResourceName, apishared.EnvValue(opts.Env, "AZURE_OPENAI_RESOURCE_NAME")); resource != "" {
			base = "https://" + resource + ".openai.azure.com/openai/v1"
		}
	}
	if base == "" {
		base = model.BaseURL
	}
	if base == "" {
		return "", fmt.Errorf(
			"azure needs an endpoint: set AZURE_OPENAI_BASE_URL or AZURE_OPENAI_RESOURCE_NAME, " +
				"or give the model a baseUrl in models.json")
	}

	normalized, err := normalizeAzureBaseURL(base)
	if err != nil {
		return "", err
	}

	version := firstNonEmpty(opts.APIVersion, apishared.EnvValue(opts.Env, "AZURE_OPENAI_API_VERSION"), azureAPIVersion)
	return normalized + "/responses?api-version=" + url.QueryEscape(version), nil
}

// azureHosts are the domains whose path layout tau knows how to correct.
var azureHosts = []string{".openai.azure.com", ".cognitiveservices.azure.com", ".ai.azure.com"}

// normalizeAzureBaseURL repairs the base URLs people actually paste.
//
// The Azure portal shows a bare resource endpoint, and the docs show the full
// path to the responses endpoint; neither is what the request needs. Both are
// accepted and rewritten to the /openai/v1 base, so a user who copied either
// one gets a working session instead of a 404.
func normalizeAzureBaseURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid azure base URL: %s", base)
	}

	isAzure := false
	for _, suffix := range azureHosts {
		if strings.HasSuffix(u.Hostname(), suffix) {
			isAzure = true
			break
		}
	}
	if !isAzure {
		// A gateway or proxy in front of Azure is addressed exactly as given.
		return strings.TrimRight(u.String(), "/"), nil
	}

	switch strings.TrimRight(u.Path, "/") {
	case "", "/openai", "/openai/v1/responses":
		u.Path = "/openai/v1"
		u.RawQuery = ""
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func doAzureRequest(ctx context.Context, endpoint string, model *ai.Model, opts *AzureOptions, body []byte) (*http.Response, error) {
	req, err := newJSONRequest(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}

	h := baseHeaders()
	// Azure authenticates with its own header rather than a bearer token.
	h.Set("api-key", opts.APIKey)
	for k, v := range model.Headers {
		h.Set(k, v)
	}
	applyHeaderOverrides(h, opts.Headers)
	req.Header = h

	return httpClient(opts.TimeoutMs).Do(req)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
