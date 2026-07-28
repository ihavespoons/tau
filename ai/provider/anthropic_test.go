package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
)

// sseBody is a minimal well-formed Anthropic message stream.
const sseBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// testProvider points the Anthropic provider at a local server, exercising the
// real auth-resolution + wire-API path.
func testProvider(t *testing.T, store auth.CredentialStore, env auth.EnvContext, handler http.HandlerFunc) (*Provider, *ai.Model) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p := Anthropic(store, env)
	m := *p.Model("claude-sonnet-5")
	m.BaseURL = srv.URL
	return p, &m
}

func collectText(stream *ai.MessageStream) string {
	var sb strings.Builder
	for ev := range stream.Events() {
		if ev.Type == ai.EventTextDelta {
			sb.WriteString(ev.Delta)
		}
	}
	return sb.String()
}

func TestAnthropicProviderResolvesStoredAPIKey(t *testing.T) {
	var gotKey, gotVersion string
	store := auth.NewMemStore()
	if _, err := store.Modify(context.Background(), "anthropic", func(*auth.Credential) (*auth.Credential, error) {
		return &auth.Credential{Type: auth.CredentialAPIKey, Key: "sk-ant-test-key"}, nil
	}); err != nil {
		t.Fatal(err)
	}

	p, model := testProvider(t, store, auth.MapContext{}, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody))
	})

	stream := p.StreamSimple(context.Background(), model, ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}},
	}, nil)

	if got := collectText(stream); got != "hello world" {
		t.Errorf("text = %q", got)
	}
	final := stream.Result()
	if final.StopReason != ai.StopStop {
		t.Errorf("stop = %v (%s)", final.StopReason, final.ErrorMessage)
	}
	if gotKey != "sk-ant-test-key" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header missing")
	}
	// Cost must be computed from the model's rates.
	if final.Usage.Cost.Total == 0 {
		t.Error("expected non-zero cost")
	}
}

func TestAnthropicProviderFallsBackToEnvAPIKey(t *testing.T) {
	var gotKey string
	env := auth.MapContext{"ANTHROPIC_API_KEY": "sk-ant-from-env"}
	p, model := testProvider(t, auth.NewMemStore(), env, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody))
	})

	stream := p.StreamSimple(context.Background(), model, ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}},
	}, nil)
	_ = collectText(stream)

	if stream.Result().StopReason != ai.StopStop {
		t.Fatalf("stop = %v", stream.Result().StopReason)
	}
	if gotKey != "sk-ant-from-env" {
		t.Errorf("x-api-key = %q", gotKey)
	}
}

// An OAuth access token must ride Authorization: Bearer, never x-api-key.
func TestAnthropicProviderOAuthUsesBearer(t *testing.T) {
	var gotAuth, gotKey string
	store := auth.NewMemStore()
	future := time.Now().Add(time.Hour).UnixMilli()
	if _, err := store.Modify(context.Background(), "anthropic", func(*auth.Credential) (*auth.Credential, error) {
		return &auth.Credential{
			Type: auth.CredentialOAuth,
			OAuth: &auth.OAuthData{
				Access: "sk-ant-oat01-live", Refresh: "refresh-token", Expires: future,
			},
		}, nil
	}); err != nil {
		t.Fatal(err)
	}

	p, model := testProvider(t, store, auth.MapContext{}, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody))
	})

	stream := p.StreamSimple(context.Background(), model, ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}},
	}, nil)
	_ = collectText(stream)

	if stream.Result().StopReason != ai.StopStop {
		t.Fatalf("stop = %v (%s)", stream.Result().StopReason, stream.Result().ErrorMessage)
	}
	if gotAuth != "Bearer sk-ant-oat01-live" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotKey != "" {
		t.Errorf("x-api-key must be empty for OAuth, got %q", gotKey)
	}
}

// Missing credentials must surface as a terminal error event, never a panic
// or an out-of-band error (Pi's never-throw stream contract).
func TestAnthropicProviderMissingCredentialsIsTerminalEvent(t *testing.T) {
	p, model := testProvider(t, auth.NewMemStore(), auth.MapContext{}, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("request must not be made without credentials")
		w.WriteHeader(http.StatusInternalServerError)
	})

	stream := p.StreamSimple(context.Background(), model, ai.Context{
		Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}},
	}, nil)

	var sawError bool
	for ev := range stream.Events() {
		if ev.Type == ai.EventError {
			sawError = true
		}
	}
	final := stream.Result()
	if !sawError || final.StopReason != ai.StopError {
		t.Fatalf("expected terminal error event, got %v", final.StopReason)
	}
	if !strings.Contains(final.ErrorMessage, "tau login") {
		t.Errorf("error should guide the user to login: %q", final.ErrorMessage)
	}
}

func TestAnthropicProviderSendsPromptAndModel(t *testing.T) {
	var body map[string]any
	p, model := testProvider(t, auth.NewMemStore(), auth.MapContext{"ANTHROPIC_API_KEY": "k"},
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sseBody))
		})

	stream := p.StreamSimple(context.Background(), model, ai.Context{
		SystemPrompt: "be terse",
		Messages: ai.MessageList{ai.UserMessage{
			Content: ai.UserContent{Text: "what is 2+2"}, Timestamp: 1,
		}},
	}, nil)
	_ = collectText(stream)

	if body["model"] != "claude-sonnet-5" {
		t.Errorf("model = %v", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v (must always stream)", body["stream"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
	if body["system"] == nil {
		t.Error("system prompt not sent")
	}
}
