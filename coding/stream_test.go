package coding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/config"
)

// affinityServer serves one chat-completions turn and records the session
// header the provider attached.
func affinityServer(t *testing.T) (url string, header *string) {
	t.Helper()
	var seen string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("x-session-id")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"id":"c1","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &seen
}

// sessionWithAffinityProvider builds a real session whose only model comes
// from models.json, with the affinity flag turned on so the id becomes
// observable on the wire.
func sessionWithAffinityProvider(t *testing.T, url string, opts Options) *Session {
	t.Helper()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	t.Setenv(config.EnvSessionDir, filepath.Join(agentDir, "sessions"))

	baseURL, err := json.Marshal(url)
	if err != nil {
		t.Fatal(err)
	}
	models := `{"providers":{"aff":{
		"baseUrl": ` + string(baseURL) + `,
		"apiKey": "sk-test",
		"compat": {"sendSessionAffinityHeaders": true, "sessionAffinityFormat": "openrouter"},
		"models": [{"id":"m","contextWindow":1000,"maxTokens":100}]
	}}}`
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}

	opts.ModelID = "aff/m"
	opts.NoTools = true
	if opts.Cwd == "" {
		opts.Cwd = t.TempDir()
	}

	cs, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("building session: %v", err)
	}
	t.Cleanup(func() { cs.Close(context.Background(), "test") })
	return cs
}

// oneTurn drives the session's own stream dispatch, which is the code path the
// agent loop uses.
func oneTurn(t *testing.T, cs *Session, opts *ai.SimpleStreamOptions) {
	t.Helper()
	stream := cs.stream(context.Background(), cs.Model,
		ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}}}},
		opts)
	for range stream.Events() {
	}
	if msg := stream.Result(); msg.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", msg.ErrorMessage)
	}
}

// THE POINT: providers key prompt caches and backend affinity off the session
// id. tau plumbed the id nowhere, so every request looked like a new
// conversation and the compat flags that consume it were unreachable.
func TestSessionIDReachesTheProvider(t *testing.T) {
	url, header := affinityServer(t)
	cs := sessionWithAffinityProvider(t, url, Options{})

	if cs.sessionID == "" {
		t.Fatal("a persisted session must have an id to attach")
	}
	oneTurn(t, cs, &ai.SimpleStreamOptions{})

	if *header != cs.sessionID {
		t.Errorf("session header %q, want the session id %q", *header, cs.sessionID)
	}
}

// An explicit id from the caller wins: the session's own id is a default, not
// an override.
func TestExplicitSessionIDIsNotOverwritten(t *testing.T) {
	url, header := affinityServer(t)
	cs := sessionWithAffinityProvider(t, url, Options{})

	oneTurn(t, cs, &ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{SessionID: "caller-chose-this"},
	})

	if *header != "caller-chose-this" {
		t.Errorf("session header %q, want the caller's id", *header)
	}
}

// Without persistence there is no conversation to pin to, and the header must
// be absent rather than empty-but-present.
func TestNoSessionMeansNoAffinity(t *testing.T) {
	url, header := affinityServer(t)
	cs := sessionWithAffinityProvider(t, url, Options{NoSession: true})

	oneTurn(t, cs, &ai.SimpleStreamOptions{})

	if *header != "" {
		t.Errorf("session header %q, want none", *header)
	}
}

// THE POINT: a bare model id is ambiguous once the compiled catalog spans
// every provider — a dozen of them resell claude-sonnet-5 — and the resolver
// breaks ties by sorting ids, so an unqualified default would silently start
// every session on whichever reseller sorted highest. It did: the default
// resolved to a Bedrock model the moment the full catalog landed.
func TestDefaultModelResolvesToAnthropic(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	reg, warnings, err := BuildRegistry(auth.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings with no models.json: %v", warnings)
	}

	match, err := reg.Resolve(DefaultModel)
	if err != nil {
		t.Fatalf("the default model must resolve: %v", err)
	}
	if match.Model.Provider != "anthropic" {
		t.Errorf("default resolved to %s/%s", match.Model.Provider, match.Model.ID)
	}
	if match.Model.Api != ai.ApiAnthropicMessages {
		t.Errorf("default model wire: %q", match.Model.Api)
	}
}

// The default has to be reachable through the provider that serves it, or the
// first turn fails on a nil stream.
func TestDefaultModelIsStreamable(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	reg, _, err := BuildRegistry(auth.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	match, err := reg.Resolve(DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if p := reg.ProviderFor(match.Model); p == nil || p.StreamSimple == nil {
		t.Fatal("the default model has no provider that can stream it")
	}
}
