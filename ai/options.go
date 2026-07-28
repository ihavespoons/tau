package ai

import "net/http"

// CacheRetention is a prompt-cache retention preference.
type CacheRetention string

const (
	CacheNone  CacheRetention = "none"
	CacheShort CacheRetention = "short"
	CacheLong  CacheRetention = "long"
)

// Transport is a provider transport preference.
type Transport string

const (
	TransportSSE             Transport = "sse"
	TransportWebSocket       Transport = "websocket"
	TransportWebSocketCached Transport = "websocket-cached"
	TransportAuto            Transport = "auto"
)

// ProviderResponse is passed to OnResponse after HTTP headers are received.
type ProviderResponse struct {
	Status  int
	Headers http.Header
}

// StreamOptions are the base options all wire APIs share. Cancellation is
// carried by the context passed to StreamFunc (Pi's options.signal).
type StreamOptions struct {
	Temperature *float64
	MaxTokens   int
	APIKey      string
	Transport   Transport
	// CacheRetention defaults to "short".
	CacheRetention CacheRetention
	// SessionID enables session-based caching/routing on supporting providers.
	SessionID string
	// OnPayload may inspect or replace the provider payload before sending.
	// Return (nil, nil) to keep the payload unchanged.
	OnPayload func(payload any, model *Model) (any, error)
	// OnResponse is invoked after HTTP headers arrive, before the body streams.
	OnResponse func(resp ProviderResponse, model *Model) error
	// Headers merge over provider defaults; a nil value suppresses a default
	// header with the same name (Pi's `null`).
	Headers map[string]*string
	// TimeoutMs is the HTTP request timeout.
	TimeoutMs int
	// WebsocketConnectTimeoutMs covers the connect/open handshake only.
	WebsocketConnectTimeoutMs int
	// MaxRetries caps client-side retries (nil = provider default).
	MaxRetries *int
	// MaxRetryDelayMs caps server-requested retry waits; beyond it the request
	// fails immediately so higher-level retry logic can surface it.
	// 0 means the default (60s); negative disables the cap.
	MaxRetryDelayMs int
	// Metadata is passed through to providers that understand it
	// (e.g. Anthropic user_id).
	Metadata map[string]any
	// Env overrides process env for provider configuration.
	Env map[string]string
	// Extra carries provider-specific options (Pi's ProviderStreamOptions
	// index signature).
	Extra map[string]any
}

// SimpleStreamOptions adds normalized cross-provider reasoning controls.
type SimpleStreamOptions struct {
	StreamOptions
	// Reasoning is the requested thinking level; empty means off.
	Reasoning ThinkingLevel
	// ThinkingBudgets customizes token budgets (token-based providers only).
	ThinkingBudgets *ThinkingBudgets
}
