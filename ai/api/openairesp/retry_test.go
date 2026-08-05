package openairesp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// flakyResponses fails the first n requests, then streams a complete turn.
type flakyResponses struct {
	mu       sync.Mutex
	failures int
	status   int
	headers  map[string]string
	attempts int
}

func (f *flakyResponses) handler(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.attempts++
	fail := f.attempts <= f.failures
	f.mu.Unlock()

	if fail {
		for k, v := range f.headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte(
		chunk(`{"type":"response.completed","response":{"id":"r","status":"completed"}}`)))
}

func (f *flakyResponses) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *flakyResponses) start(t *testing.T) *ai.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return modelFor("openai", srv.URL)
}

// THE POINT: a 429 or a 5xx from a gateway is routine, and this wire had no
// retry at all — one blip ended the turn.
func TestARateLimitIsRetried(t *testing.T) {
	srv := &flakyResponses{failures: 2, status: http.StatusTooManyRequests,
		headers: map[string]string{"retry-after-ms": "1"}}
	model := srv.start(t)

	n := 3
	stream := Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: &n},
	})
	for range stream.Events() {
	}

	if final := stream.Result(); final.StopReason != ai.StopStop {
		t.Fatalf("final %+v", final)
	}
	if srv.count() != 3 {
		t.Errorf("%d attempts, want 3", srv.count())
	}
}

func TestAClientErrorIsNotRetried(t *testing.T) {
	srv := &flakyResponses{failures: 5, status: http.StatusBadRequest}
	model := srv.start(t)

	n := 3
	stream := Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: &n},
	})
	for range stream.Events() {
	}

	if final := stream.Result(); final.StopReason != ai.StopError {
		t.Errorf("final %+v", final)
	}
	if srv.count() != 1 {
		t.Errorf("%d attempts — a 400 must not be retried", srv.count())
	}
}

// Azure and the Codex backend go through their own request functions, so each
// needs its own proof that the policy is applied.
func TestAzureRetriesToo(t *testing.T) {
	srv := &flakyResponses{failures: 1, status: http.StatusServiceUnavailable,
		headers: map[string]string{"retry-after-ms": "1"}}
	handler := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer handler.Close()

	model := modelFor("azure-openai-responses", handler.URL)
	n := 2
	stream := StreamAzure(context.Background(), model, simpleContext(), &AzureOptions{
		Options: Options{StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: &n}},
	})
	for range stream.Events() {
	}

	if final := stream.Result(); final.StopReason != ai.StopStop {
		t.Fatalf("final %+v", final)
	}
	if srv.count() != 2 {
		t.Errorf("%d attempts, want 2", srv.count())
	}
}
