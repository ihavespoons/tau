package googlegenai

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
	"golang.org/x/oauth2/google"
)

// Vertex serves the same Gemini models over the same protocol, so it lives
// beside the direct wire: the request body, the stream state machine, and
// every thinking and signature rule are identical.
//
// What differs is how you are addressed and how you prove who you are. Vertex
// is a Google Cloud service, so a model lives under a project and a region,
// and the caller is normally an IAM principal rather than the holder of an API
// key.

// vertexScope is the OAuth scope a Vertex call needs.
const vertexScope = "https://www.googleapis.com/auth/cloud-platform"

// VertexOptions adds the Cloud addressing the direct wire does not need.
type VertexOptions struct {
	Options
	// Project is the Google Cloud project the model is billed to.
	Project string
	// Location is the region serving it, or "global".
	Location string
}

// StreamVertex runs one turn against Vertex AI.
func StreamVertex(ctx context.Context, model *ai.Model, c ai.Context, opts *VertexOptions) *ai.MessageStream {
	if opts == nil {
		opts = &VertexOptions{}
	}
	stream := ai.NewMessageStream()
	go runVertex(ctx, stream, model, c, opts)
	return stream
}

// StreamSimpleVertex is StreamVertex with normalized cross-provider options.
func StreamSimpleVertex(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	if opts == nil {
		opts = &ai.SimpleStreamOptions{}
	}
	reasoning := opts.Reasoning
	if reasoning != "" {
		if clamped := ai.ClampThinkingLevel(model, ai.ModelThinkingLevel(reasoning)); clamped == ai.ThinkingOff {
			reasoning = ""
		} else {
			reasoning = ai.ThinkingLevel(clamped)
		}
	}
	return StreamVertex(ctx, model, c, &VertexOptions{Options: Options{
		StreamOptions:   opts.StreamOptions,
		Reasoning:       reasoning,
		ThinkingBudgets: opts.ThinkingBudgets,
	}})
}

func runVertex(ctx context.Context, stream *ai.MessageStream, model *ai.Model, c ai.Context, opts *VertexOptions) {
	out := newOutput(model)

	defer func() {
		if r := recover(); r != nil {
			fail(stream, out, ctx, fmt.Errorf("google vertex: %v", r))
		}
	}()

	endpoint, auth, err := vertexTarget(ctx, model, opts)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}

	body, err := encodePayload(buildRequest(model, c, &opts.Options), model, &opts.Options)
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		fail(stream, out, ctx, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	auth(req.Header)
	for k, v := range model.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range opts.Headers {
		if v == nil {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, *v)
	}

	resp, err := httpClientFor(opts.TimeoutMs).Do(req)
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
	if err := consume(ctx, resp.Body, stream, out, model); err != nil {
		fail(stream, out, ctx, err)
		return
	}
	stream.Push(ai.Event{Type: ai.EventDone, Reason: out.StopReason, Message: out})
}

// applyAuth writes credentials onto a request.
type applyAuth func(http.Header)

// vertexTarget resolves the endpoint and how to authenticate to it.
//
// There are two ways in, and they address the model differently. An API key
// reaches the global publisher endpoint with no project at all — Vertex's
// "express mode". Everything else goes through a project and a region, and
// authenticates as a Cloud principal.
func vertexTarget(ctx context.Context, model *ai.Model, opts *VertexOptions) (string, applyAuth, error) {
	if key := vertexAPIKey(opts); key != "" {
		endpoint := vertexPublisherURL(customVertexHost(model.BaseURL), model.ID)
		return endpoint, func(h http.Header) { h.Set("x-goog-api-key", key) }, nil
	}

	project := apishared.EnvValue(opts.Env, "GOOGLE_CLOUD_PROJECT")
	if opts.Project != "" {
		project = opts.Project
	}
	if project == "" {
		project = apishared.EnvValue(opts.Env, "GCLOUD_PROJECT")
	}
	if project == "" {
		return "", nil, fmt.Errorf(
			"vertex needs a project: set GOOGLE_CLOUD_PROJECT or GCLOUD_PROJECT")
	}

	location := opts.Location
	if location == "" {
		location = apishared.EnvValue(opts.Env, "GOOGLE_CLOUD_LOCATION")
	}
	if location == "" {
		return "", nil, fmt.Errorf("vertex needs a location: set GOOGLE_CLOUD_LOCATION")
	}

	token, err := vertexAccessToken(ctx)
	if err != nil {
		return "", nil, err
	}

	endpoint := vertexProjectURL(model.BaseURL, location, project, model.ID)
	return endpoint, func(h http.Header) { h.Set("Authorization", "Bearer "+token) }, nil
}

// placeholderKey matches the "<your key here>" a half-filled config leaves
// behind. Treating one as a real key produces a 401 rather than the clearer
// failure of having no key at all.
var placeholderKey = regexp.MustCompile(`^<[^>]+>$`)

// vertexCredentialsMarker is what the credential store writes when Vertex is
// configured to use ambient Cloud credentials rather than a key.
const vertexCredentialsMarker = "gcp-vertex-credentials"

func vertexAPIKey(opts *VertexOptions) string {
	key := strings.TrimSpace(opts.APIKey)
	if key == "" || key == vertexCredentialsMarker || placeholderKey.MatchString(key) {
		return ""
	}
	return key
}

const vertexGlobalHost = "https://aiplatform.googleapis.com"

// customVertexHost honours a base URL that names a real host.
//
// The catalog's is a {location} template, which is not a host until a region
// is chosen — so it is ignored on the express path, where there is no region.
// Anything else is a deliberate override: a proxy, a gateway, or a test.
func customVertexHost(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" || strings.Contains(trimmed, "{location}") {
		return vertexGlobalHost
	}
	return trimmed
}

func vertexPublisherURL(host, modelID string) string {
	return fmt.Sprintf("%s/v1/publishers/google/models/%s:streamGenerateContent?alt=sse",
		strings.TrimRight(host, "/"), modelID)
}

// vertexProjectURL builds the regional endpoint.
//
// The catalog stores the host with a {location} placeholder, because the region
// is the caller's choice and a model is not pinned to one.
func vertexProjectURL(baseURL, location, project, modelID string) string {
	host := baseURL
	if host == "" {
		host = "https://{location}-aiplatform.googleapis.com"
	}
	host = strings.ReplaceAll(host, "{location}", location)
	// "global" is served by the unregioned host rather than a "global-" prefix.
	if location == "global" {
		host = vertexGlobalHost
	}

	return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
		strings.TrimRight(host, "/"), project, location, modelID)
}

// vertexAccessToken mints a token from Application Default Credentials.
//
// ADC is what `gcloud auth application-default login` writes, what
// GOOGLE_APPLICATION_CREDENTIALS points at, and what a Compute or Cloud Run
// metadata server serves — so the same code works on a laptop and in a job,
// which is the whole point of not asking the user for a key.
func vertexAccessToken(ctx context.Context) (string, error) {
	creds, err := google.FindDefaultCredentials(ctx, vertexScope)
	if err != nil {
		return "", fmt.Errorf(
			"vertex found no credentials: run `gcloud auth application-default login`, "+
				"set GOOGLE_APPLICATION_CREDENTIALS, or give the provider an API key (%w)", err)
	}
	token, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("vertex could not mint an access token: %w", err)
	}
	return token.AccessToken, nil
}
