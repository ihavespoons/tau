package openairesp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai"
)

// defaultTimeout matches the other wires.
const defaultTimeout = 10 * time.Minute

// responsesURL appends the endpoint unless the base URL already names it.
// Providers configure base URLs with and without the /v1 suffix, and some
// point straight at the full path.
func responsesURL(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(trimmed, "/responses") {
		return trimmed
	}
	return trimmed + "/responses"
}

// baseHeaders are what every request on this protocol carries.
func baseHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	return h
}

// applyHeaderOverrides merges the caller's headers last, so they win. A nil
// value suppresses a default, which is how a gateway drops the Authorization
// header it does not want.
func applyHeaderOverrides(h http.Header, overrides map[string]*string) {
	for k, v := range overrides {
		if v == nil {
			h.Del(k)
			continue
		}
		h.Set(k, *v)
	}
}

// newJSONRequest builds the POST every variant sends.
func newJSONRequest(ctx context.Context, endpoint string, body []byte) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
}

// httpClient applies the caller's timeout, or tau's default.
func httpClient(timeoutMs int) *http.Client {
	timeout := defaultTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}
	return &http.Client{Timeout: timeout}
}

// buildHeaders assembles tau's defaults, the model's own, then the caller's
// overrides.
func buildHeaders(model *ai.Model, opts *Options, cm compat) http.Header {
	h := baseHeaders()
	if opts.APIKey != "" {
		h.Set("Authorization", "Bearer "+opts.APIKey)
	}
	for k, v := range model.Headers {
		h.Set(k, v)
	}

	// Session affinity keeps a multi-turn conversation on one backend, which
	// is what makes the prompt cache pay off on a routed provider.
	if opts.SessionID != "" {
		switch cm.SessionAffinityFormat {
		case affinityOpenRouter:
			h.Set("x-session-id", opts.SessionID)
		case affinityOpenAINoSession:
			h.Set("x-client-request-id", opts.SessionID)
		default:
			h.Set("session_id", opts.SessionID)
			h.Set("x-client-request-id", opts.SessionID)
		}
	}

	applyHeaderOverrides(h, opts.Headers)
	return h
}

// doRequest sends the payload and returns the streaming response.
func doRequest(ctx context.Context, model *ai.Model, opts *Options, cm compat, body []byte) (*http.Response, error) {
	if opts.APIKey == "" && !hasAuthHeader(opts) {
		return nil, fmt.Errorf("no API key for provider %s", model.Provider)
	}

	req, err := newJSONRequest(ctx, responsesURL(model.BaseURL), body)
	if err != nil {
		return nil, err
	}
	req.Header = buildHeaders(model, opts, cm)
	return httpClient(opts.TimeoutMs).Do(req)
}

// hasAuthHeader reports whether the caller supplied authorization itself,
// which is how a gateway in front of the endpoint authenticates.
func hasAuthHeader(opts *Options) bool {
	for k, v := range opts.Headers {
		if v == nil || strings.TrimSpace(*v) == "" {
			continue
		}
		switch strings.ToLower(k) {
		case "authorization", "cf-aig-authorization":
			return true
		}
	}
	return false
}
