package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
)

// fakeCompletions serves one chat-completions turn and records the request.
func fakeCompletions(t *testing.T) (url string, body *string) {
	t.Helper()
	var capturedBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		capturedBody = string(buf)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"id":"c1","choices":[{"delta":{"content":"pong"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":3,"completion_tokens":1}}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)

	return srv.URL, &capturedBody
}

func ptr[T any](v T) *T { return &v }

func configFor(baseURL, apiKey string) *Config {
	name := "Local"
	base := baseURL
	def := ProviderDef{Name: &name, BaseURL: &base, Models: []ModelDef{{
		ID: "my-model", Name: ptr("My Model"),
		ContextWindow: ptr(8192), MaxTokens: ptr(1024),
	}}}
	if apiKey != "" {
		def.APIKey = &apiKey
	}
	return &Config{Providers: map[string]ProviderDef{"local": def}}
}

// THE POINT OF A CUSTOM PROVIDER: declaring one in models.json has to produce
// something that actually streams, not just a catalog entry. Before the wire
// API was bound in, a custom provider resolved to a model with no way to call
// it.
func TestCustomProviderStreams(t *testing.T) {
	url, body := fakeCompletions(t)

	reg, err := NewRegistry(nil, configFor(url, "sk-test"), Deps{
		Store: auth.NewMemStore(), Env: auth.MapContext{},
	})
	if err != nil {
		t.Fatal(err)
	}

	match, err := reg.Resolve("local/my-model")
	if err != nil {
		t.Fatalf("resolving the custom model: %v", err)
	}
	p := reg.ProviderFor(match.Model)
	if p == nil || p.StreamSimple == nil {
		t.Fatal("a custom provider must be able to stream")
	}

	stream := p.StreamSimple(context.Background(), match.Model, ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "ping"}}},
	}, &ai.SimpleStreamOptions{})

	for range stream.Events() {
	}
	msg := stream.Result()

	if msg.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", msg.ErrorMessage)
	}
	if text := msg.Content[0].(ai.TextContent).Text; text != "pong" {
		t.Errorf("text: %q", text)
	}
	if !strings.Contains(*body, `"model":"my-model"`) {
		t.Errorf("the model id did not reach the provider: %s", *body)
	}
}

// An api tau does not implement is still catalogued and still selectable —
// hiding it would make a real provider look unsupported — but using it fails
// naming the wire and the model, not with a nil dereference and not with a
// bare "unsupported" the user cannot act on.
func TestUnsupportedAPIIsCataloguedButNotCallable(t *testing.T) {
	base := "https://example.invalid"
	api := "some-future-wire"
	cfg := &Config{Providers: map[string]ProviderDef{"exotic": {
		BaseURL: &base, Api: &api,
		Models: []ModelDef{{ID: "m", ContextWindow: ptr(1000), MaxTokens: ptr(100)}},
	}}}

	reg, err := NewRegistry(nil, cfg, Deps{Store: auth.NewMemStore(), Env: auth.MapContext{}})
	if err != nil {
		t.Fatal(err)
	}
	match, err := reg.Resolve("exotic/m")
	if err != nil {
		t.Fatalf("the model should still be catalogued: %v", err)
	}

	p := reg.ProviderFor(match.Model)
	if p == nil || p.StreamSimple == nil {
		t.Fatal("a catalogued model needs a provider that can report why it fails")
	}

	stream := p.StreamSimple(context.Background(), match.Model,
		ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}}}},
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}})
	for range stream.Events() {
	}

	msg := stream.Result()
	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	for _, want := range []string{"m", "some-future-wire"} {
		if !strings.Contains(msg.ErrorMessage, want) {
			t.Errorf("the error should name %q: %q", want, msg.ErrorMessage)
		}
	}
}

// A provider with no api declared is assumed to speak chat-completions, which
// is what every self-hosted and proxy endpoint exposes.
func TestMissingAPIDefaultsToChatCompletions(t *testing.T) {
	url, _ := fakeCompletions(t)
	reg, err := NewRegistry(nil, configFor(url, "sk-test"), Deps{
		Store: auth.NewMemStore(), Env: auth.MapContext{},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Provider("local")
	if p.Api != ai.ApiOpenAICompletions {
		t.Errorf("api: %q", p.Api)
	}
	if p.StreamSimple == nil {
		t.Error("the default api should be streamable")
	}
}

// The apiKey from models.json is what authenticates a custom provider — this
// is the gap that left CustomAPIKey exposed but unused.
func TestConfiguredAPIKeyAuthenticates(t *testing.T) {
	url, _ := fakeCompletions(t)

	reg, err := NewRegistry(nil, configFor(url, "sk-configured"), Deps{
		Store: auth.NewMemStore(), Env: auth.MapContext{},
	})
	if err != nil {
		t.Fatal(err)
	}
	match, _ := reg.Resolve("local/my-model")

	stream := reg.ProviderFor(match.Model).StreamSimple(context.Background(), match.Model,
		ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "ping"}}}},
		&ai.SimpleStreamOptions{})
	for range stream.Events() {
	}
	if msg := stream.Result(); msg.StopReason != ai.StopStop {
		t.Fatalf("the configured key did not authenticate: %s", msg.ErrorMessage)
	}
}

// `"apiKey": "$VAR"` resolves against the environment rather than being sent
// as a literal — otherwise a config file would have to hold the secret.
func TestConfiguredAPIKeySupportsEnvReference(t *testing.T) {
	t.Setenv("TAU_TEST_PROVIDER_KEY", "sk-from-env")
	url, _ := fakeCompletions(t)

	reg, err := NewRegistry(nil, configFor(url, "$TAU_TEST_PROVIDER_KEY"), Deps{
		Store: auth.NewMemStore(), Env: auth.OSContext{},
	})
	if err != nil {
		t.Fatal(err)
	}
	match, _ := reg.Resolve("local/my-model")

	stream := reg.ProviderFor(match.Model).StreamSimple(context.Background(), match.Model,
		ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "ping"}}}},
		&ai.SimpleStreamOptions{})
	for range stream.Events() {
	}
	if msg := stream.Result(); msg.StopReason != ai.StopStop {
		t.Fatalf("the env reference did not resolve: %s", msg.ErrorMessage)
	}
}

// With no key anywhere, the failure has to name what is missing.
func TestMissingCredentialsAreExplained(t *testing.T) {
	url, _ := fakeCompletions(t)

	reg, err := NewRegistry(nil, configFor(url, ""), Deps{
		Store: auth.NewMemStore(), Env: auth.MapContext{},
	})
	if err != nil {
		t.Fatal(err)
	}
	match, _ := reg.Resolve("local/my-model")

	stream := reg.ProviderFor(match.Model).StreamSimple(context.Background(), match.Model,
		ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "ping"}}}},
		&ai.SimpleStreamOptions{})
	for range stream.Events() {
	}
	msg := stream.Result()
	if msg.StopReason != ai.StopError {
		t.Fatalf("expected an auth failure, got %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "local") {
		t.Errorf("the error should name the provider: %q", msg.ErrorMessage)
	}
}

// Without deps a registry still catalogues, so catalog-only callers and tests
// are unaffected.
func TestRegistryWithoutDepsStillCatalogues(t *testing.T) {
	reg, err := NewRegistry(nil, configFor("https://example.invalid", "k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve("local/my-model"); err != nil {
		t.Errorf("catalog lookup should work without deps: %v", err)
	}
}
