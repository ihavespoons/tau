package openaichat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ihavespoons/tau/ai"
)

// flakyServer fails the first n requests, then streams a complete turn.
type flakyServer struct {
	mu       sync.Mutex
	failures int
	status   int
	headers  map[string]string
	attempts int
}

func (f *flakyServer) start(t *testing.T) *ai.Model {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			`data: {"id":"c1","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)

	m := modelFor("openai", srv.URL)
	return m
}

func (f *flakyServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func retries(n int) *int { return &n }

// drain consumes a stream, bounded.
//
// The bound is the point. A retry policy that ignores its own delay cap does
// not fail a test — it WAITS, for however long the server asked for, and the
// package sits on the go-test deadline with no clue which case did it.
// Mutation testing produced exactly that: removing the cap turned a failing
// assertion into a five-minute hang.
func drain(t *testing.T, stream *ai.MessageStream) *ai.AssistantMessage {
	t.Helper()
	done := make(chan *ai.AssistantMessage, 1)
	go func() {
		for range stream.Events() {
		}
		done <- stream.Result()
	}()
	select {
	case final := <-done:
		return final
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never finished — a retry waited longer than any test should")
		return nil
	}
}

// THE POINT: a 429 from a gateway is routine. Without a retry it costs the
// whole turn — and this wire had none at all, while the Anthropic one has had
// the policy since P1.
func TestARateLimitIsRetried(t *testing.T) {
	// retry-after-ms keeps the test fast and pins that the server's own
	// requested delay is what gets honoured.
	srv := &flakyServer{failures: 2, status: http.StatusTooManyRequests,
		headers: map[string]string{"retry-after-ms": "1"}}
	model := srv.start(t)

	if final := drain(t, Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: retries(3)},
	})); final.StopReason != ai.StopStop {
		t.Fatalf("final %+v", final)
	}
	if srv.count() != 3 {
		t.Errorf("%d attempts, want 3", srv.count())
	}
}

// A 400 is the request being wrong. Retrying it wastes the user's time and
// money to get the identical answer.
func TestAClientErrorIsNotRetried(t *testing.T) {
	srv := &flakyServer{failures: 5, status: http.StatusBadRequest}
	model := srv.start(t)

	if final := drain(t, Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: retries(3)},
	})); final.StopReason != ai.StopError {
		t.Errorf("final %+v", final)
	}
	if srv.count() != 1 {
		t.Errorf("%d attempts — a 400 must not be retried", srv.count())
	}
}

// x-should-retry overrides the status: the provider knows things the code
// cannot infer.
func TestTheProviderCanForbidARetry(t *testing.T) {
	srv := &flakyServer{failures: 5, status: http.StatusServiceUnavailable,
		headers: map[string]string{"x-should-retry": "false"}}
	model := srv.start(t)

	drain(t, Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: retries(3)},
	}))

	if srv.count() != 1 {
		t.Errorf("%d attempts — x-should-retry: false must be obeyed", srv.count())
	}
}

// Retries are opt-in: without MaxRetries the request is made once, which is
// what a caller doing its own retrying expects.
func TestNoRetriesByDefault(t *testing.T) {
	srv := &flakyServer{failures: 5, status: http.StatusTooManyRequests}
	model := srv.start(t)

	stream := Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}

	if srv.count() != 1 {
		t.Errorf("%d attempts, want 1", srv.count())
	}
}

// THE POINT: a server asking for a five-minute wait has effectively ended the
// session. Failing immediately hands the decision back to the caller instead of
// hanging with nothing on screen.
func TestAnExcessiveServerDelayFailsImmediately(t *testing.T) {
	srv := &flakyServer{failures: 5, status: http.StatusTooManyRequests,
		headers: map[string]string{"retry-after": "300"}}
	model := srv.start(t)

	final := drain(t, Stream(context.Background(), model, simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k", MaxRetries: retries(3), MaxRetryDelayMs: 1000},
	}))
	if final.StopReason != ai.StopError || !strings.Contains(final.ErrorMessage, "retry delay") {
		t.Errorf("final %+v", final)
	}
	if srv.count() != 1 {
		t.Errorf("%d attempts", srv.count())
	}
}

// THE POINT: the Copilot headers are derived per request, so they have to be
// built where the request is — not once at construction. A turn that gains an
// image mid-session must gain the header with it.
func TestCopilotHeadersReachTheRequest(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"id":"c1","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"))
	}))
	defer srv.Close()

	model := modelFor("github-copilot", srv.URL)
	c := simpleContext()
	c.Messages = ai.MessageList{
		ai.UserMessage{Content: ai.UserContent{Blocks: ai.ContentList{
			ai.TextContent{Text: "what is this"},
			ai.ImageContent{Data: "aGk=", MimeType: "image/png"},
		}}},
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "a cat"}}},
	}

	stream := Stream(context.Background(), model, c, &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}

	if got.Get("X-Initiator") != "agent" {
		t.Errorf("X-Initiator %q — the last message is not the user's", got.Get("X-Initiator"))
	}
	if got.Get("Copilot-Vision-Request") != "true" {
		t.Errorf("Copilot-Vision-Request %q", got.Get("Copilot-Vision-Request"))
	}
	if got.Get("Openai-Intent") != "conversation-edits" {
		t.Errorf("Openai-Intent %q", got.Get("Openai-Intent"))
	}
}

// Everyone else must not receive them: they are Copilot's own vocabulary.
func TestCopilotHeadersAreNotSentToOtherProviders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"id":"c1","choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"))
	}))
	defer srv.Close()

	stream := Stream(context.Background(), modelFor("groq", srv.URL), simpleContext(), &Options{
		StreamOptions: ai.StreamOptions{APIKey: "k"},
	})
	for range stream.Events() {
	}

	if got.Get("X-Initiator") != "" || got.Get("Openai-Intent") != "" {
		t.Errorf("copilot headers leaked: %+v", got)
	}
}
