package openaichat

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ihavespoons/tau/ai"
)

// defaultTimeout matches Pi's provider default.
const defaultTimeout = 10 * time.Minute

// completionsPath is appended to a base URL that does not already name an
// endpoint. Providers configure base URLs with and without the /v1 suffix, and
// some point straight at the full path.
func completionsURL(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(trimmed, "/chat/completions") {
		return trimmed
	}
	return trimmed + "/chat/completions"
}

// buildHeaders assembles the request headers: tau's defaults, the model's own,
// then the caller's overrides. A nil override value suppresses a default,
// which is Pi's way of letting a gateway drop the Authorization header.
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
	// is what makes prompt caching pay off on routed providers.
	if cm.SendSessionAffinityHeaders && opts.SessionID != "" {
		switch cm.SessionAffinityFormat {
		case affinityOpenRouter:
			h.Set("x-session-id", opts.SessionID)
		case affinityOpenAINoSession:
			h.Set("x-client-request-id", opts.SessionID)
			h.Set("x-session-affinity", opts.SessionID)
		default:
			h.Set("session_id", opts.SessionID)
			h.Set("x-client-request-id", opts.SessionID)
			h.Set("x-session-affinity", opts.SessionID)
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

// doRequest posts the payload and returns the streaming response.
func doRequest(ctx context.Context, model *ai.Model, headers http.Header, body []byte, opts *Options) (*http.Response, error) {
	url := completionsURL(model.BaseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header = headers

	timeout := defaultTimeout
	if opts.TimeoutMs > 0 {
		timeout = time.Duration(opts.TimeoutMs) * time.Millisecond
	}
	client := &http.Client{Timeout: timeout}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", model.Provider, err)
	}
	return resp, nil
}
