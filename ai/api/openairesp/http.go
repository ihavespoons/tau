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

// buildHeaders assembles tau's defaults, the model's own, then the caller's
// overrides. A nil override suppresses a default, which is how a gateway drops
// the Authorization header it does not want.
func buildHeaders(model *ai.Model, opts *Options, cm compat) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
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

	for k, v := range opts.Headers {
		if v == nil {
			h.Del(k)
			continue
		}
		h.Set(k, *v)
	}
	return h
}

// doRequest sends the payload and returns the streaming response.
func doRequest(ctx context.Context, model *ai.Model, opts *Options, cm compat, body []byte) (*http.Response, error) {
	if opts.APIKey == "" && !hasAuthHeader(opts) {
		return nil, fmt.Errorf("no API key for provider %s", model.Provider)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL(model.BaseURL), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = buildHeaders(model, opts, cm)

	timeout := defaultTimeout
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	client := &http.Client{Timeout: timeout}
	return client.Do(req)
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
