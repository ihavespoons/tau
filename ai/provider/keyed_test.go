package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
)

// wireProbe answers both wires and records which path was asked for. The two
// have different request paths, which is exactly how a misroute shows up.
func wireProbe(t *testing.T) (url string, path *string) {
	t.Helper()
	var seen string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")

		if strings.Contains(r.URL.Path, "messages") {
			_, _ = w.Write([]byte(
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		_, _ = w.Write([]byte(
			`data: {"id":"c1","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &seen
}

func model(id string, api ai.Api, baseURL string) ai.Model {
	return ai.Model{
		ID: id, Name: id, Api: api, Provider: "probe", BaseURL: baseURL,
		Input: []string{"text"}, ContextWindow: 10000, MaxTokens: 1000,
	}
}

func drain(t *testing.T, p *Provider, m *ai.Model) *ai.AssistantMessage {
	t.Helper()
	stream := p.StreamSimple(context.Background(), m,
		ai.Context{Messages: ai.MessageList{ai.UserMessage{Content: ai.UserContent{Text: "hi"}}}},
		&ai.SimpleStreamOptions{StreamOptions: ai.StreamOptions{APIKey: "k"}})
	for range stream.Events() {
	}
	return stream.Result()
}

// THE POINT: a provider is not always one wire. Fireworks serves most of its
// catalog over an Anthropic-compatible endpoint and routes GLM 5.2 through
// chat-completions; xAI serves Grok 4.5 over the responses API and the rest
// over chat-completions. Dispatching on the PROVIDER's wire sends those models
// to the wrong parser, and the failure looks like a broken model.
func TestKeyedDispatchesOnTheModelsWire(t *testing.T) {
	url, path := wireProbe(t)
	p := Keyed(auth.NewMemStore(), auth.MapContext{}, KeyedOptions{
		ID: "probe", Name: "Probe", BaseURL: url,
		Models: []ai.Model{
			model("chat-model", ai.ApiOpenAICompletions, url),
			model("messages-model", ai.ApiAnthropicMessages, url),
		},
	})

	cases := []struct {
		model    string
		wantPath string
	}{
		{"chat-model", "chat/completions"},
		{"messages-model", "messages"},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			msg := drain(t, p, p.Model(tc.model))
			if msg.StopReason != ai.StopStop {
				t.Fatalf("stream failed: %s", msg.ErrorMessage)
			}
			if !strings.Contains(*path, tc.wantPath) {
				t.Errorf("routed to %q, want the %q wire", *path, tc.wantPath)
			}
		})
	}
}

// A wire tau cannot speak must name itself. "Not supported" alone leaves the
// user guessing whether the problem is the model, the key, or tau.
//
// Every wire in the compiled catalog is now implemented, so the case this
// covers is a models.json provider declaring an api tau has never heard of —
// which is what a typo in that file produces.
func TestKeyedNamesAnUnimplementedWire(t *testing.T) {
	url, _ := wireProbe(t)
	m := model("future-model", "telepathy-v2", url)
	p := Keyed(auth.NewMemStore(), auth.MapContext{}, KeyedOptions{
		ID: "probe", BaseURL: url, Models: []ai.Model{m},
	})

	msg := drain(t, p, p.Model("future-model"))
	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	for _, want := range []string{"future-model", "telepathy-v2"} {
		if !strings.Contains(msg.ErrorMessage, want) {
			t.Errorf("error should mention %q: %q", want, msg.ErrorMessage)
		}
	}
}

// THE POINT: pi-messages is reachable through the same dispatch as every other
// wire. It arrived last, and a wire that is implemented but not routed fails
// exactly like one that was never written.
func TestKeyedRoutesPiMessages(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"done","reason":"stop"}` + "\n\n"))
	}))
	defer srv.Close()

	p := Keyed(auth.NewMemStore(), auth.MapContext{}, KeyedOptions{
		ID: "probe", BaseURL: srv.URL,
		Models: []ai.Model{model("gateway-model", ai.ApiPiMessages, srv.URL)},
	})

	msg := drain(t, p, p.Model("gateway-model"))
	if msg.StopReason != ai.StopStop {
		t.Fatalf("stream failed: %s", msg.ErrorMessage)
	}
	if !strings.HasSuffix(seen, "/messages") {
		t.Errorf("routed to %q", seen)
	}
	if !StreamableWire(ai.ApiPiMessages) {
		t.Error("pi-messages must report as streamable, or models on it are listed as unreachable")
	}
}

// The provider-level wire is only a label, but a wrong one is misleading in
// `tau models` — it should describe most of the catalog.
func TestDominantApiFollowsTheMajority(t *testing.T) {
	models := []ai.Model{
		model("a", ai.ApiAnthropicMessages, ""),
		model("b", ai.ApiAnthropicMessages, ""),
		model("c", ai.ApiOpenAICompletions, ""),
	}
	if got := dominantApi(models); got != ai.ApiAnthropicMessages {
		t.Errorf("dominant api: %q", got)
	}
}

// Every built-in provider must be able to stream, or it is a catalog entry
// nobody can use.
func TestEveryBuiltinCanStream(t *testing.T) {
	for _, p := range Builtins(auth.NewMemStore(), auth.MapContext{}) {
		if p.StreamSimple == nil {
			t.Errorf("%s cannot stream", p.ID)
		}
		if len(p.Models) == 0 {
			t.Errorf("%s has no models", p.ID)
		}
	}
}
