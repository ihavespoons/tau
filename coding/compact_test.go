package coding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/session"
)

// summarizerServer serves one chat-completions turn carrying summaryText and
// records every request body it saw.
func summarizerServer(t *testing.T, summaryText string) (string, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body)

		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(map[string]any{
			"id": "c1",
			"choices": []any{map[string]any{
				"delta":         map[string]any{"content": summaryText},
				"finish_reason": "stop",
			}},
		})
		_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &seen
}

// persistedSession builds a real session with a real session file and a model
// served by url, so compaction runs the whole chain the CLI would.
//
// The retained tail is set small in settings rather than by writing 20k tokens
// of filler: the default is about what a real conversation looks like, and the
// cut-point arithmetic is covered where it lives.
func persistedSession(t *testing.T, url string, contextWindow int) *Session {
	t.Helper()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	t.Setenv(config.EnvSessionDir, filepath.Join(agentDir, "sessions"))

	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"),
		[]byte(`{"compaction":{"keepRecentTokens":300,"reserveTokens":500}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	baseURL, err := json.Marshal(url)
	if err != nil {
		t.Fatal(err)
	}
	window, err := json.Marshal(contextWindow)
	if err != nil {
		t.Fatal(err)
	}
	models := `{"providers":{"stub":{
		"baseUrl": ` + string(baseURL) + `,
		"apiKey": "sk-test",
		"models": [{"id":"m","contextWindow":` + string(window) + `,"maxTokens":4096}]
	}}}`
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}

	cs, err := New(context.Background(), Options{
		Cwd: t.TempDir(), NoTools: true, ModelID: "stub/m",
	})
	if err != nil {
		t.Fatalf("building session: %v", err)
	}
	t.Cleanup(func() { cs.Close(context.Background(), "test") })
	return cs
}

func addHistory(t *testing.T, cs *Session, messages ...ai.Message) {
	t.Helper()
	ctx := context.Background()
	for _, m := range messages {
		if _, err := cs.Session.AppendMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := cs.reloadContext(ctx); err != nil {
		t.Fatal(err)
	}
}

func user(text string) ai.Message {
	return ai.UserMessage{Content: ai.UserContent{Text: text}, Timestamp: 1}
}

func assistant(text string) ai.Message {
	return ai.AssistantMessage{
		Content:    ai.ContentList{ai.TextContent{Text: text}},
		Api:        "openai-completions",
		Provider:   "stub",
		Model:      "m",
		StopReason: ai.StopStop,
	}
}

func longHistory(t *testing.T, cs *Session) {
	t.Helper()
	addHistory(t, cs,
		user("the original request"),
		assistant("working on it"),
		user("keep going"),
		assistant("still working"),
		user(strings.Repeat("the most recent thing ", 2000)),
	)
}

// The end-to-end shape: a checkpoint entry lands in the file, and the agent's
// in-memory context is rebuilt from it rather than left as it was.
func TestCompactWritesACheckpointAndRebuildsTheContext(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "## Goal\nfinish the port")
	cs := persistedSession(t, url, 200000)
	longHistory(t, cs)

	before := len(cs.Agent.Messages())
	result, err := cs.Compact(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("nothing was compacted")
		return
	}
	if !strings.Contains(result.Summary, "finish the port") {
		t.Errorf("summary = %q", result.Summary)
	}

	entries := cs.Session.Entries(ctx, nil)
	found := false
	for _, e := range entries {
		if c, ok := e.(*session.CompactionEntry); ok {
			found = true
			if c.FirstKeptEntryID == "" {
				t.Error("the checkpoint does not say what it kept")
			}
			// Self-contained: reconstructing the tail later would depend on the
			// tree still having this shape, and a fork may change it.
			if len(c.RetainedTail) == 0 {
				t.Error("the checkpoint carries no retained tail")
			}
		}
	}
	if !found {
		t.Fatal("no compaction entry was written")
	}

	after := cs.Agent.Messages()
	if len(after) >= before {
		t.Errorf("context went from %d to %d messages; compaction should shrink it", before, len(after))
	}
	if len(after) == 0 {
		t.Fatal("compaction emptied the context")
	}
	// The summary has to be in the context or the checkpoint bought nothing.
	joined := ""
	for _, m := range after {
		if um, ok := m.(ai.UserMessage); ok {
			joined += um.Content.String()
		}
	}
	if !strings.Contains(joined, "finish the port") {
		t.Errorf("the summary is not in the rebuilt context:\n%s", joined)
	}
}

// A short session is not an error condition. /compact on one should say so.
func TestCompactingAShortSessionIsANoOp(t *testing.T) {
	url, seen := summarizerServer(t, "unused")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs, user("hello"), assistant("hi"))

	result, err := cs.Compact(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Errorf("compacted %+v, want nothing", result)
	}
	if len(*seen) != 0 {
		t.Errorf("made %d requests for a session with nothing to compact", len(*seen))
	}
}

// Automatic compaction is the one that matters: it fires without anyone asking,
// so the threshold has to be right in both directions.
func TestAutomaticCompactionFiresOnlyOnceTheWindowIsFull(t *testing.T) {
	ctx := context.Background()
	url, seen := summarizerServer(t, "summary")

	// A window far larger than the conversation: nothing should happen.
	roomy := persistedSession(t, url, 200000)
	longHistory(t, roomy)
	compacted, err := roomy.MaybeCompact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if compacted {
		t.Error("compacted a conversation that still fits")
	}
	if len(*seen) != 0 {
		t.Errorf("made %d requests when nothing was needed", len(*seen))
	}

	// A window the conversation has outgrown.
	tight := persistedSession(t, url, 3000)
	longHistory(t, tight)
	compacted, err = tight.MaybeCompact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !compacted {
		t.Error("a conversation past its reserve should compact")
	}
}

func TestCompactionCanBeTurnedOff(t *testing.T) {
	url, _ := summarizerServer(t, "summary")
	cs := persistedSession(t, url, 3000)

	if err := os.WriteFile(filepath.Join(config.AgentDir(), "settings.json"),
		[]byte(`{"compaction":{"enabled":false,"keepRecentTokens":300,"reserveTokens":500}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, set, err := loadSettings(cs.Cwd, cs.Trust.Trusted)
	if err != nil {
		t.Fatal(err)
	}
	cs.Settings, cs.setMgr = set, mgr
	longHistory(t, cs)

	compacted, err := cs.MaybeCompact(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if compacted {
		t.Error("compaction is disabled and should not have run")
	}
}

// A session with no file has no history to compact, and saying so beats a nil
// dereference.
func TestCompactingAnUnpersistedSessionIsRefused(t *testing.T) {
	cs := newTestSession(t, Options{NoSession: true})
	if _, err := cs.Compact(context.Background(), ""); err != ErrNoSession {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
	if _, err := cs.MoveTo(context.Background(), "x", false); err != ErrNoSession {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

// ---------------------------------------------------------------------------
// Tree navigation
// ---------------------------------------------------------------------------

// Moving back must change what the model sees. If the context still held the
// abandoned branch, navigation would be decorative.
func TestMovingBackDropsTheAbandonedBranchFromContext(t *testing.T) {
	ctx := context.Background()
	url, seen := summarizerServer(t, "branch summary text")
	cs := persistedSession(t, url, 200000)

	target, err := cs.Session.AppendMessage(ctx, user("the fork point"))
	if err != nil {
		t.Fatal(err)
	}
	addHistory(t, cs, assistant("down the wrong path"), user("still wrong"))

	if _, err := cs.MoveTo(ctx, target, false); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 0 {
		t.Errorf("made %d requests with summarizing turned off", len(*seen))
	}

	for _, m := range cs.Agent.Messages() {
		if am, ok := m.(ai.AssistantMessage); ok {
			for _, c := range am.Content {
				if tc, ok := c.(ai.TextContent); ok && strings.Contains(tc.Text, "wrong path") {
					t.Error("the abandoned branch is still in context")
				}
			}
		}
	}
}

// With summarizing on, the abandoned work leaves a trace — otherwise the next
// turn has no idea the exploration happened.
func TestMovingBackWithASummaryKeepsTheExplorationInContext(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "tried the wrong path, it failed")
	cs := persistedSession(t, url, 200000)

	target, err := cs.Session.AppendMessage(ctx, user("the fork point"))
	if err != nil {
		t.Fatal(err)
	}
	addHistory(t, cs, assistant("down the wrong path"), user("still wrong"))

	result, err := cs.MoveTo(ctx, target, true)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("no branch summary was produced")
		return
	}
	if !strings.Contains(result.Summary, "tried the wrong path") {
		t.Errorf("summary = %q", result.Summary)
	}

	joined := ""
	for _, m := range cs.Agent.Messages() {
		if um, ok := m.(ai.UserMessage); ok {
			joined += um.Content.String()
		}
	}
	if !strings.Contains(joined, "tried the wrong path") {
		t.Errorf("the branch summary is not in context:\n%s", joined)
	}
}

func TestMovingToAnUnknownEntryIsRefused(t *testing.T) {
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs, user("hello"))

	if _, err := cs.MoveTo(context.Background(), "nosuchid", false); err == nil {
		t.Error("expected an error for an unknown entry")
	}
}

// ---------------------------------------------------------------------------
// Forking
// ---------------------------------------------------------------------------

// The whole point of a fork is that the original survives it.
func TestForkingLeavesTheOriginalUntouched(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)

	addHistory(t, cs, user("first request"), assistant("first answer"))
	forkPoint, err := cs.Session.AppendMessage(ctx, user("second request"))
	if err != nil {
		t.Fatal(err)
	}
	addHistory(t, cs, assistant("second answer"))

	original := cs.Path
	originalBytes, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}

	if err := cs.Fork(ctx, forkPoint); err != nil {
		t.Fatal(err)
	}
	if cs.Path == original {
		t.Fatal("the fork did not switch to a new file")
	}

	after, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(originalBytes) != string(after) {
		t.Error("forking modified the source session")
	}

	// The fork stops before the chosen message so it can be made differently.
	joined := ""
	for _, m := range cs.Agent.Messages() {
		if um, ok := m.(ai.UserMessage); ok {
			joined += um.Content.String() + "\n"
		}
	}
	if !strings.Contains(joined, "first request") {
		t.Errorf("the fork lost the history before the fork point:\n%s", joined)
	}
	if strings.Contains(joined, "second request") {
		t.Errorf("the fork kept the message it was forked at:\n%s", joined)
	}
}

func TestCloningCopiesTheWholeSession(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs, user("first request"), assistant("first answer"))

	original := cs.Path
	if err := cs.Fork(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if cs.Path == original {
		t.Fatal("the clone did not switch to a new file")
	}

	joined := ""
	for _, m := range cs.Agent.Messages() {
		if um, ok := m.(ai.UserMessage); ok {
			joined += um.Content.String()
		}
	}
	if !strings.Contains(joined, "first request") {
		t.Errorf("the clone lost history:\n%s", joined)
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// The tree is what /tree prints. A fork has to be visible as a fork.
func TestTheRenderedTreeShowsBothBranchesAndThePosition(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)

	root, err := cs.Session.AppendMessage(ctx, user("the shared start"))
	if err != nil {
		t.Fatal(err)
	}
	addHistory(t, cs, user("branch one"))
	if _, err := cs.MoveTo(ctx, root, false); err != nil {
		t.Fatal(err)
	}
	addHistory(t, cs, user("branch two"))

	roots, err := cs.TreeNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderTree(ctx, cs.Session, roots)
	for _, want := range []string{"the shared start", "branch one", "branch two"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "> ") {
		t.Errorf("the current position is not marked:\n%s", out)
	}
}

// A tool result adds a line saying nothing; the question /tree answers is
// where the conversation can go back to.
func TestTheRenderedTreeLeavesOutEntriesWithNothingToSay(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs,
		user("do it"),
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "read",
			Content: ai.ContentList{ai.TextContent{Text: "file contents"}}},
	)

	roots, err := cs.TreeNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderTree(ctx, cs.Session, roots)
	if strings.Contains(out, "file contents") {
		t.Errorf("tool results should not be tree nodes:\n%s", out)
	}
	if !strings.Contains(out, "do it") {
		t.Errorf("the user message should be:\n%s", out)
	}
}

func TestUserPromptsAreTheOfferedForkPoints(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs,
		user("first\nwith a second line"),
		assistant("an answer"),
		user("second"),
	)

	points, err := cs.UserPrompts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d fork points, want 2", len(points))
	}
	// One line, because the picker shows one line.
	if points[0].Text != "first" {
		t.Errorf("text = %q, want just the first line", points[0].Text)
	}
	if points[0].EntryID == "" {
		t.Error("a fork point needs an entry id to fork at")
	}
}

// ---------------------------------------------------------------------------
// Overflow recovery
// ---------------------------------------------------------------------------

// The turn the provider rejects for length is the one compaction exists to
// save. Detecting the rejection and retrying is the whole point; without the
// retry the user just sees a failed turn.
func TestATurnRejectedForLengthIsCompactedAndRetried(t *testing.T) {
	ctx := context.Background()

	var conversational int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		// A summarization request is recognizable by its system prompt, which
		// is what lets one server play both parts.
		isSummary := false
		if msgs, ok := body["messages"].([]any); ok {
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] == "system" {
					if text, _ := mm["content"].(string); strings.Contains(text, "context summarization assistant") {
						isSummary = true
					}
				}
			}
		}

		write := func(text string) {
			w.Header().Set("Content-Type", "text/event-stream")
			payload, _ := json.Marshal(map[string]any{
				"id": "c1",
				"choices": []any{map[string]any{
					"delta":         map[string]any{"content": text},
					"finish_reason": "stop",
				}},
			})
			_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
		}

		if isSummary {
			write("the story so far")
			return
		}
		conversational++
		if conversational == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"This endpoint's maximum context length is 65536 tokens. However, you requested about 90000 tokens"}}`))
			return
		}
		write("recovered and answered")
	}))
	t.Cleanup(srv.Close)

	// The declared window is generous, so the pre-turn estimate does not fire
	// and the only thing that can trigger compaction is the rejection itself.
	cs := persistedSession(t, srv.URL, 200000)
	longHistory(t, cs)

	out, err := cs.Prompt(ctx, "carry on")
	if err != nil {
		t.Fatal(err)
	}
	if conversational != 2 {
		t.Errorf("made %d conversational requests, want 2 (the rejection and the retry)", conversational)
	}

	answered := false
	for _, m := range out {
		if am, ok := m.(ai.AssistantMessage); ok {
			for _, c := range am.Content {
				if text, ok := c.(ai.TextContent); ok && strings.Contains(text.Text, "recovered and answered") {
					answered = true
				}
			}
		}
	}
	if !answered {
		t.Error("the retried turn did not produce an answer")
	}

	// A checkpoint must have been written, or the retry would have been sent
	// with the same oversized context.
	compacted := false
	for _, e := range cs.Session.Entries(ctx, nil) {
		if _, ok := e.(*session.CompactionEntry); ok {
			compacted = true
		}
	}
	if !compacted {
		t.Error("no compaction entry was written before the retry")
	}
}

// An ordinary failure must not cost the user their history.
func TestAnOrdinaryFailureDoesNotTriggerCompaction(t *testing.T) {
	ctx := context.Background()

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	t.Cleanup(srv.Close)

	cs := persistedSession(t, srv.URL, 200000)
	longHistory(t, cs)

	if _, err := cs.Prompt(ctx, "carry on"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Errorf("made %d requests; a bad key is not retried by compacting", requests)
	}
	for _, e := range cs.Session.Entries(ctx, nil) {
		if _, ok := e.(*session.CompactionEntry); ok {
			t.Error("an unrelated failure caused a compaction")
		}
	}
}

// Compaction has to happen before the request, not only after a rejection.
// The overflow path recovers a turn that was already refused; this is what
// stops the refusal happening in the first place.
func TestAnOversizedConversationIsCompactedBeforeTheTurnIsSent(t *testing.T) {
	ctx := context.Background()

	var conversational, summaries int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		isSummary := false
		if msgs, ok := body["messages"].([]any); ok {
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] == "system" {
					if text, _ := mm["content"].(string); strings.Contains(text, "context summarization assistant") {
						isSummary = true
					}
				}
			}
		}
		if isSummary {
			summaries++
		} else {
			conversational++
		}

		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(map[string]any{
			"id": "c1",
			"choices": []any{map[string]any{
				"delta":         map[string]any{"content": "ok"},
				"finish_reason": "stop",
			}},
		})
		_, _ = w.Write([]byte("data: " + string(payload) + "\n\n"))
	}))
	t.Cleanup(srv.Close)

	// A window the conversation has already outgrown, and a provider that would
	// happily accept the oversized request — so the only thing that can
	// compact is the pre-turn check.
	cs := persistedSession(t, srv.URL, 3000)
	longHistory(t, cs)

	if _, err := cs.Prompt(ctx, "carry on"); err != nil {
		t.Fatal(err)
	}
	if summaries != 1 {
		t.Errorf("summarization requests = %d, want 1 before the turn", summaries)
	}
	if conversational != 1 {
		t.Errorf("conversational requests = %d, want 1", conversational)
	}

	compacted := false
	for _, e := range cs.Session.Entries(ctx, nil) {
		if _, ok := e.(*session.CompactionEntry); ok {
			compacted = true
		}
	}
	if !compacted {
		t.Error("no checkpoint was written before the turn")
	}
}
