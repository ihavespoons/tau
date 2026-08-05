package openairesp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/api/apishared"
)

func codexModel(baseURL string) *ai.Model {
	m := modelFor("openai-codex", baseURL)
	m.Api = ai.ApiOpenAICodexResponses
	m.Reasoning = true
	return m
}

// codexToken builds a JWT-shaped token carrying an account id.
func codexToken(t *testing.T, accountID string) string {
	t.Helper()
	claims := map[string]any{}
	if accountID != "" {
		claims[codexAuthClaim] = map[string]any{"chatgpt_account_id": accountID}
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

// THE POINT: every request must name the ChatGPT account, and the id exists
// nowhere but inside the token's own claims. A token tau cannot read is a
// login that cannot be used — worth saying plainly rather than 401ing later.
func TestCodexAccountIDFromToken(t *testing.T) {
	t.Run("read from the claim", func(t *testing.T) {
		got, err := codexAccountID(codexToken(t, "acct-123"))
		if err != nil {
			t.Fatal(err)
		}
		if got != "acct-123" {
			t.Errorf("account id: %q", got)
		}
	})

	cases := []struct{ name, token string }{
		{"not a jwt", "not-a-token"},
		{"unreadable claims", "header.!!!not-base64!!!.sig"},
		{"no auth claim", codexToken(t, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := codexAccountID(tc.token)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "log in again") {
				t.Errorf("the error should say what to do: %v", err)
			}
		})
	}
}

// codexPayload renders a codex request the way the backend would see it.
func codexPayload(t *testing.T, model *ai.Model, c ai.Context, opts *CodexOptions) map[string]any {
	t.Helper()
	cm := resolveCodexCompat(model)
	grammar, err := apishared.GrammarToolInputProperties(c.Tools, cm.SupportsOpenAIGrammarTools)
	if err != nil {
		t.Fatalf("resolving grammar tools: %v", err)
	}
	req, err := buildCodexRequest(model, c, opts, cm, grammar)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// THE POINT: the system prompt travels in `instructions`. Sent as a message it
// is accepted and then ignored, which reads as the model disregarding it — and
// sending it in both places pays for the largest item in the request twice.
func TestCodexSystemPromptGoesInInstructions(t *testing.T) {
	payload := codexPayload(t, codexModel(""), simpleContext(), &CodexOptions{})

	if payload["instructions"] != "be helpful" {
		t.Errorf("instructions: %v", payload["instructions"])
	}
	for _, it := range payload["input"].([]any) {
		role, _ := it.(map[string]any)["role"].(string)
		if role == "system" || role == "developer" {
			t.Errorf("the system prompt was also sent as a message: %#v", it)
		}
	}
}

// The backend rejects an empty instructions field.
func TestCodexAlwaysSendsInstructions(t *testing.T) {
	c := simpleContext()
	c.SystemPrompt = ""

	payload := codexPayload(t, codexModel(""), c, &CodexOptions{})

	if payload["instructions"] != defaultInstructions {
		t.Errorf("instructions: %v", payload["instructions"])
	}
}

// A subscription conversation runs long, and losing the model's train of
// thought halfway through is the failure people notice — so the encrypted
// payload is always requested, not only when reasoning was asked for.
func TestCodexAlwaysRequestsEncryptedReasoning(t *testing.T) {
	for _, opts := range []*CodexOptions{{}, {Options: Options{Reasoning: "high"}}} {
		payload := codexPayload(t, codexModel(""), simpleContext(), opts)

		include, _ := payload["include"].([]any)
		if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
			t.Errorf("include: %#v (reasoning=%q)", payload["include"], opts.Reasoning)
		}
	}
}

func TestCodexURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", codexBaseURL + "/codex/responses"},
		{"https://chatgpt.com/backend-api", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://proxy/codex", "https://proxy/codex/responses"},
		{"https://proxy/codex/responses", "https://proxy/codex/responses"},
	}
	for _, tc := range cases {
		if got := codexURL(tc.in); got != tc.want {
			t.Errorf("codexURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// THE POINT: the backend's terminal event is response.done. Without treating
// it as terminal the stream ends with nothing and the turn is reported as
// truncated — a failure that reads like a network problem.
func TestCodexTerminalEventIsAccepted(t *testing.T) {
	body := chunkCreated +
		chunk(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1"}}`) +
		chunk(`{"type":"response.output_text.delta","output_index":0,"delta":"hi"}`) +
		chunk(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1",`+
			`"content":[{"type":"output_text","text":"hi"}]}}`) +
		chunk(`{"type":"response.done","response":{"id":"r1","status":"completed",`+
			`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)

	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	_, msg := collect(StreamCodex(context.Background(), codexModel(srv.URL), simpleContext(),
		&CodexOptions{Options: Options{StreamOptions: ai.StreamOptions{
			APIKey: codexToken(t, "acct-1"), SessionID: "s1",
		}}}))

	if msg.StopReason != ai.StopStop {
		t.Fatalf("stop reason: %q (%s)", msg.StopReason, msg.ErrorMessage)
	}
	if text := msg.Content[0].(ai.TextContent).Text; text != "hi" {
		t.Errorf("text: %q", text)
	}

	// Without the account header the backend cannot tell which subscription to
	// bill and refuses the request.
	if seen.Get("chatgpt-account-id") != "acct-1" {
		t.Errorf("chatgpt-account-id: %q", seen.Get("chatgpt-account-id"))
	}
	if seen.Get("OpenAI-Beta") != "responses=experimental" {
		t.Errorf("OpenAI-Beta: %q", seen.Get("OpenAI-Beta"))
	}
	if seen.Get("session-id") != "s1" {
		t.Errorf("session-id: %q", seen.Get("session-id"))
	}
	if seen.Get("originator") == "" {
		t.Error("the backend expects an originator")
	}
}

// A login that never happened has to say so, not fail as a 401 later.
func TestCodexWithoutALoginSaysSo(t *testing.T) {
	_, msg := collect(StreamCodex(context.Background(), codexModel("https://example.invalid"),
		simpleContext(), &CodexOptions{}))

	if msg.StopReason != ai.StopError {
		t.Fatalf("stop reason: %q", msg.StopReason)
	}
	if !strings.Contains(msg.ErrorMessage, "tau login") {
		t.Errorf("the error should say how to fix it: %q", msg.ErrorMessage)
	}
}
