package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func newTestSession(t *testing.T) *Session {
	t.Helper()
	return NewSession(NewMemStorage(Metadata{ID: "s1", Cwd: "/tmp/proj"}))
}

func userMsg(text string) ai.Message {
	return ai.UserMessage{Content: ai.UserContent{Text: text}, Timestamp: 1}
}

func assistantMsg(text, provider, model string) ai.Message {
	return ai.AssistantMessage{
		Content:  ai.ContentList{ai.TextContent{Text: text}},
		Api:      "anthropic-messages",
		Provider: provider,
		Model:    model,
		Usage: ai.Usage{
			Input: 10, Output: 5, CacheRead: 2, CacheWrite: 3, TotalTokens: 20,
			Cost: ai.Cost{Total: 0.5},
		},
		StopReason: ai.StopStop,
		Timestamp:  2,
	}
}

func TestAppendAndLeafTracking(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)

	leaf, err := s.LeafID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if leaf != nil {
		t.Errorf("new session should have no leaf, got %v", *leaf)
	}

	id1, err := s.AppendMessage(ctx, userMsg("hello"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ = s.LeafID(ctx)
	if leaf == nil || *leaf != id1 {
		t.Errorf("leaf = %v, want %s", leaf, id1)
	}

	id2, err := s.AppendMessage(ctx, assistantMsg("hi", "anthropic", "claude-sonnet-5"))
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := s.Entry(ctx, id2)
	if !ok {
		t.Fatal("entry not found")
	}
	if entry.Base().ParentID == nil || *entry.Base().ParentID != id1 {
		t.Errorf("parent = %v, want %s", entry.Base().ParentID, id1)
	}
}

// Moving the leaf appends a leaf entry rather than rewriting history, and the
// redirect is what later reads follow.
func TestMoveToAppendsLeafEntry(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)

	id1, _ := s.AppendMessage(ctx, userMsg("first"))
	_, _ = s.AppendMessage(ctx, assistantMsg("reply", "anthropic", "m"))
	before := len(s.Entries(ctx, nil))

	if _, err := s.MoveTo(ctx, &id1, nil); err != nil {
		t.Fatal(err)
	}

	entries := s.Entries(ctx, nil)
	if len(entries) != before+1 {
		t.Fatalf("MoveTo should append one entry, got %d new", len(entries)-before)
	}
	last := entries[len(entries)-1]
	if _, ok := last.(*LeafEntry); !ok {
		t.Fatalf("last entry type = %T, want *LeafEntry", last)
	}
	leaf, _ := s.LeafID(ctx)
	if leaf == nil || *leaf != id1 {
		t.Errorf("leaf = %v, want %s", leaf, id1)
	}
}

func TestMoveToWithSummaryWritesBranchSummary(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	id1, _ := s.AppendMessage(ctx, userMsg("first"))
	_, _ = s.AppendMessage(ctx, userMsg("second"))

	summaryID, err := s.MoveTo(ctx, &id1, &BranchSummary{Summary: "explored A"})
	if err != nil {
		t.Fatal(err)
	}
	if summaryID == "" {
		t.Fatal("expected a branch-summary entry id")
	}
	entry, ok := s.Entry(ctx, summaryID)
	if !ok {
		t.Fatal("branch summary not found")
	}
	bs := entry.(*BranchSummaryEntry)
	if bs.FromID != id1 || bs.Summary != "explored A" {
		t.Errorf("branch summary = %+v", bs)
	}
}

func TestMoveToUnknownEntryFails(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	missing := "nope1234"
	if _, err := s.MoveTo(ctx, &missing, nil); !IsCode(err, CodeNotFound) {
		t.Errorf("err = %v, want not_found", err)
	}
}

func TestBranchWalksToRoot(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	id1, _ := s.AppendMessage(ctx, userMsg("one"))
	id2, _ := s.AppendMessage(ctx, userMsg("two"))
	id3, _ := s.AppendMessage(ctx, userMsg("three"))

	branch, err := s.Branch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := entryIDs(branch)
	want := []string{id1, id2, id3}
	if !equalStrings(got, want) {
		t.Errorf("branch = %v, want %v (root first)", got, want)
	}
}

// A compaction with a retained tail is a self-contained checkpoint: the walk
// stops there instead of replaying older entries.
func TestPathStopsAtCompactionWithRetainedTail(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	_, _ = s.AppendMessage(ctx, userMsg("ancient"))
	_, _ = s.AppendMessage(ctx, userMsg("old"))
	compactionID, err := s.AppendCompaction(ctx, "summary of history", 50000, CompactionOptions{
		RetainedTail: []ai.Message{userMsg("recent")},
	})
	if err != nil {
		t.Fatal(err)
	}
	afterID, _ := s.AppendMessage(ctx, userMsg("after"))

	branch, err := s.Branch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := entryIDs(branch)
	want := []string{compactionID, afterID}
	if !equalStrings(got, want) {
		t.Errorf("branch = %v, want %v", got, want)
	}
}

// The older compaction form has no retained tail, so the walk continues back
// to the first kept entry and includes it.
func TestPathWalksToFirstKeptEntry(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	ancientID, _ := s.AppendMessage(ctx, userMsg("ancient"))
	keptID, _ := s.AppendMessage(ctx, userMsg("kept"))
	compactionID, _ := s.AppendCompaction(ctx, "summary", 50000, CompactionOptions{
		FirstKeptEntryID: keptID,
	})
	afterID, _ := s.AppendMessage(ctx, userMsg("after"))

	branch, err := s.Branch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := entryIDs(branch)
	// The walk stops at the first kept entry, so the ancient one is excluded.
	want := []string{keptID, compactionID, afterID}
	if !equalStrings(got, want) {
		t.Errorf("branch = %v, want %v", got, want)
	}
	for _, id := range got {
		if id == ancientID {
			t.Error("entries before firstKeptEntryId must not be walked")
		}
	}
}

func TestBuildContextLinear(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	_, _ = s.AppendMessage(ctx, userMsg("hello"))
	_, _ = s.AppendMessage(ctx, assistantMsg("hi there", "anthropic", "claude-opus-5"))

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(sc.Messages))
	}
	if sc.Model == nil || sc.Model.ModelID != "claude-opus-5" {
		t.Errorf("model = %+v, want it derived from the assistant message", sc.Model)
	}
	if sc.ThinkingLevel != "off" {
		t.Errorf("thinking = %q, want off by default", sc.ThinkingLevel)
	}
}

func TestBuildContextDerivesState(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	_, _ = s.AppendMessage(ctx, userMsg("hi"))
	_, _ = s.AppendThinkingLevelChange(ctx, "high")
	_, _ = s.AppendModelChange(ctx, "openai", "gpt-5")
	_, _ = s.AppendActiveToolsChange(ctx, []string{"read", "bash"})

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ThinkingLevel != "high" {
		t.Errorf("thinking = %q", sc.ThinkingLevel)
	}
	if sc.Model == nil || sc.Model.Provider != "openai" || sc.Model.ModelID != "gpt-5" {
		t.Errorf("model = %+v", sc.Model)
	}
	if !equalStrings(sc.ActiveToolNames, []string{"read", "bash"}) {
		t.Errorf("tools = %v", sc.ActiveToolNames)
	}
	// State entries themselves carry no model context.
	if len(sc.Messages) != 1 {
		t.Errorf("messages = %d, want only the user message", len(sc.Messages))
	}
}

// State is derived from the walked path, and a retained-tail compaction stops
// that walk — so a thinking level or model set before such a compaction is not
// visible in the derived context. This mirrors Pi (buildSessionContext derives
// state from getPathToRootOrCompaction's already-truncated list), and callers
// that need durable state track it outside the session.
func TestBuildContextStateIsLostBeforeRetainedTailCompaction(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	_, _ = s.AppendThinkingLevelChange(ctx, "high")
	_, _ = s.AppendMessage(ctx, userMsg("old"))
	_, _ = s.AppendCompaction(ctx, "summary", 100, CompactionOptions{
		RetainedTail: []ai.Message{userMsg("recent")},
	})

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ThinkingLevel != "off" {
		t.Errorf("thinking = %q, want off: the walk stops at the compaction", sc.ThinkingLevel)
	}

	// State set after the compaction is on the walked path and does survive.
	_, _ = s.AppendThinkingLevelChange(ctx, "xhigh")
	sc, err = s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ThinkingLevel != "xhigh" {
		t.Errorf("thinking = %q, want xhigh", sc.ThinkingLevel)
	}
}

// Without a retained tail the walk continues past the compaction to the first
// kept entry, so state recorded in that range is still derived.
func TestBuildContextStateSurvivesFirstKeptCompaction(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	levelID, _ := s.AppendThinkingLevelChange(ctx, "high")
	_, _ = s.AppendMessage(ctx, userMsg("kept"))
	_, _ = s.AppendCompaction(ctx, "summary", 100, CompactionOptions{FirstKeptEntryID: levelID})

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ThinkingLevel != "high" {
		t.Errorf("thinking = %q, want high", sc.ThinkingLevel)
	}
}

func TestBuildContextCompactionProjection(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	_, _ = s.AppendMessage(ctx, userMsg("ancient"))
	_, _ = s.AppendCompaction(ctx, "the summary", 50000, CompactionOptions{
		RetainedTail: []ai.Message{userMsg("retained one"), userMsg("retained two")},
	})
	_, _ = s.AppendMessage(ctx, userMsg("after"))

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// summary + 2 retained + 1 after
	if len(sc.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(sc.Messages))
	}
	summary, ok := sc.Messages[0].(*CompactionSummaryMessage)
	if !ok {
		t.Fatalf("first message type = %T, want *CompactionSummaryMessage", sc.Messages[0])
	}
	if summary.Summary != "the summary" || summary.TokensBefore != 50000 {
		t.Errorf("summary = %+v", summary)
	}
}

func TestBuildContextBranchSummaryProjection(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	id1, _ := s.AppendMessage(ctx, userMsg("first"))
	_, _ = s.AppendMessage(ctx, userMsg("abandoned"))
	if _, err := s.MoveTo(ctx, &id1, &BranchSummary{Summary: "tried A, failed"}); err != nil {
		t.Fatal(err)
	}

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *BranchSummaryMessage
	for _, m := range sc.Messages {
		if bs, ok := m.(*BranchSummaryMessage); ok {
			found = bs
		}
	}
	if found == nil {
		t.Fatal("branch summary missing from context")
	}
	if found.Summary != "tried A, failed" {
		t.Errorf("summary = %q", found.Summary)
	}
}

// Custom entries persist but never reach the model; custom_message entries do.
func TestCustomEntryNotProjectedButCustomMessageIs(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	_, _ = s.AppendCustomEntry(ctx, "todo-tracker", map[string]any{"open": 3})
	_, _ = s.AppendCustomMessage(ctx, "linter", ai.UserContent{Text: "2 issues"}, true, nil)

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (custom entries carry no model context)", len(sc.Messages))
	}
	cm, ok := sc.Messages[0].(*CustomMessage)
	if !ok {
		t.Fatalf("type = %T, want *CustomMessage", sc.Messages[0])
	}
	if cm.CustomType != "linter" {
		t.Errorf("customType = %q", cm.CustomType)
	}

	// Both entries are still persisted.
	if len(s.Entries(ctx, nil)) != 2 {
		t.Error("both entries should be stored")
	}
}

func TestCustomEntryProjector(t *testing.T) {
	ctx := context.Background()
	s := NewSession(NewMemStorage(Metadata{ID: "s1"}), BuildOptions{
		EntryProjectors: map[string]CustomEntryProjector{
			"todo-tracker": func(e *CustomEntry, _ int, _ []Entry) []ai.Message {
				return []ai.Message{userMsg("todos: " + e.CustomType)}
			},
		},
	})
	_, _ = s.AppendCustomEntry(ctx, "todo-tracker", nil)

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 from the projector", len(sc.Messages))
	}
}

func TestLabels(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	id, _ := s.AppendMessage(ctx, userMsg("hi"))

	if _, err := s.AppendLabel(ctx, id, ptr("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if label, ok := s.Label(ctx, id); !ok || label != "checkpoint" {
		t.Errorf("label = %q, %v", label, ok)
	}

	// A nil label clears it.
	if _, err := s.AppendLabel(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Label(ctx, id); ok {
		t.Error("label should be cleared")
	}

	if _, err := s.AppendLabel(ctx, "missing1", ptr("x")); !IsCode(err, CodeNotFound) {
		t.Errorf("labelling a missing entry: err = %v, want not_found", err)
	}
}

func TestSessionNameSanitizedAndLatestWins(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)

	if _, ok := s.Name(ctx); ok {
		t.Error("new session should have no name")
	}
	if _, err := s.AppendName(ctx, "  first\nname  "); err != nil {
		t.Fatal(err)
	}
	name, ok := s.Name(ctx)
	if !ok || name != "first name" {
		t.Errorf("name = %q, want newlines collapsed and trimmed", name)
	}

	_, _ = s.AppendName(ctx, "second")
	name, _ = s.Name(ctx)
	if name != "second" {
		t.Errorf("name = %q, want the latest", name)
	}
}

func TestStats(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	_, _ = s.AppendMessage(ctx, userMsg("hi"))
	_, _ = s.AppendMessage(ctx, assistantMsg("reply", "anthropic", "m"))
	_, _ = s.AppendMessage(ctx, assistantMsg("reply2", "anthropic", "m"))

	stats := s.Stats(ctx)
	if stats.MessageCount != 3 {
		t.Errorf("messages = %d, want 3", stats.MessageCount)
	}
	if stats.CachedTokens != 4 {
		t.Errorf("cached = %d, want 4 (2 per assistant message)", stats.CachedTokens)
	}
	if stats.UncachedTokens != 26 {
		t.Errorf("uncached = %d, want 26 (input+cacheWrite per assistant)", stats.UncachedTokens)
	}
	if stats.CostTotal != 1.0 {
		t.Errorf("cost = %v, want 1.0", stats.CostTotal)
	}
}

func TestEntriesCursor(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	for i := 0; i < 5; i++ {
		if _, err := s.AppendMessage(ctx, userMsg("m")); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.Entries(ctx, nil)); got != 5 {
		t.Errorf("all entries = %d", got)
	}
	if got := len(s.Entries(ctx, &CursorOptions{AfterEntrySeq: 2})); got != 3 {
		t.Errorf("after seq 2 = %d, want 3", got)
	}
	if got := len(s.Entries(ctx, &CursorOptions{Limit: 2})); got != 2 {
		t.Errorf("limit 2 = %d", got)
	}
	if got := len(s.Entries(ctx, &CursorOptions{AfterEntrySeq: 10})); got != 0 {
		t.Errorf("past end = %d, want 0", got)
	}
}

func TestEntryIDsAreShortAndUnique(t *testing.T) {
	ctx := context.Background()
	s := newTestSession(t)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := s.AppendMessage(ctx, userMsg("m"))
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 8 {
			t.Fatalf("id %q has length %d, want 8", id, len(id))
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

// --- JSONL storage ---

func TestJSONLRoundTripThroughDisk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	storage, err := CreateJSONL(path, CreateOptions{Cwd: "/tmp/proj", SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(storage)
	id1, _ := s.AppendMessage(ctx, userMsg("hello"))
	_, _ = s.AppendMessage(ctx, assistantMsg("hi", "anthropic", "claude-sonnet-5"))
	_, _ = s.AppendLabel(ctx, id1, ptr("start"))
	_, _ = s.AppendName(ctx, "my session")

	reopened, err := OpenJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	if errs := reopened.SoftErrors(); len(errs) != 0 {
		t.Errorf("unexpected soft errors: %v", errs)
	}
	s2 := NewSession(reopened)

	if got := len(s2.Entries(ctx, nil)); got != 4 {
		t.Errorf("entries = %d, want 4", got)
	}
	if label, ok := s2.Label(ctx, id1); !ok || label != "start" {
		t.Errorf("label lost across reload: %q %v", label, ok)
	}
	if name, _ := s2.Name(ctx); name != "my session" {
		t.Errorf("name = %q", name)
	}
	meta, _ := s2.Metadata(ctx)
	if meta.ID != "sess-1" || meta.Cwd != "/tmp/proj" || meta.Path != path {
		t.Errorf("metadata = %+v", meta)
	}
	sc, err := s2.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Messages) != 2 {
		t.Errorf("messages after reload = %d, want 2", len(sc.Messages))
	}
}

func TestJSONLHeaderIsFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if _, err := CreateJSONL(path, CreateOptions{Cwd: "/tmp/x", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(string(data), "\n", 2)[0]
	var probe map[string]any
	if err := json.Unmarshal([]byte(first), &probe); err != nil {
		t.Fatal(err)
	}
	if probe["type"] != "session" {
		t.Errorf("header type = %v", probe["type"])
	}
	if probe["version"] != float64(3) {
		t.Errorf("version = %v, want 3", probe["version"])
	}
	if !strings.HasPrefix(first, `{"type":"session"`) {
		t.Errorf("type should be the first field: %s", first)
	}
}

func TestOpenJSONLRejectsBadHeader(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, content, wantCode string
	}{
		{"empty file", "", CodeInvalidSession},
		{"no header", `{"type":"message","id":"a","parentId":null,"timestamp":"t"}` + "\n", CodeInvalidSession},
		{"old version", `{"type":"session","version":2,"id":"s","timestamp":"t","cwd":"/x"}` + "\n", CodeInvalidSession},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".jsonl")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := OpenJSONL(path)
			if !IsCode(err, tc.wantCode) {
				t.Errorf("err = %v, want code %s", err, tc.wantCode)
			}
		})
	}
}

func TestOpenJSONLMissingFile(t *testing.T) {
	_, err := OpenJSONL(filepath.Join(t.TempDir(), "nope.jsonl"))
	if !IsCode(err, CodeNotFound) {
		t.Errorf("err = %v, want not_found", err)
	}
}

// One unreadable line must not cost the user their whole history.
func TestOpenJSONLKeepsUnknownLinesAsSoftFailures(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	content := strings.Join([]string{
		`{"type":"session","version":3,"id":"s","timestamp":"2026-07-28T14:00:00.000Z","cwd":"/x"}`,
		`{"type":"message","id":"a1","parentId":null,"timestamp":"2026-07-28T14:00:01.000Z","message":{"role":"user","content":"hi","timestamp":1}}`,
		`{"type":"telepathy","id":"a2","parentId":"a1","timestamp":"2026-07-28T14:00:02.000Z","thought":"???"}`,
		`{"type":"message","id":"a3","parentId":"a2","timestamp":"2026-07-28T14:00:03.000Z","message":{"role":"user","content":"still here","timestamp":3}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	storage, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("unknown entry types must not fail the load: %v", err)
	}
	if len(storage.SoftErrors()) != 1 {
		t.Errorf("soft errors = %v, want 1", storage.SoftErrors())
	}
	entries := storage.Entries(ctx, nil)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (the unknown line is kept)", len(entries))
	}
	// The tree still links through the unknown entry.
	branch, err := storage.PathToRootOrCompaction(ctx, ptr("a3"))
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(entryIDs(branch), []string{"a1", "a2", "a3"}) {
		t.Errorf("branch = %v", entryIDs(branch))
	}
}

func TestSetLeafIDRejectsUnknownEntry(t *testing.T) {
	ctx := context.Background()
	storage := NewMemStorage(Metadata{ID: "s"})
	if err := storage.SetLeafID(ctx, ptr("missing1")); !IsCode(err, CodeNotFound) {
		t.Errorf("err = %v, want not_found", err)
	}
}

// --- fixture ---

// A session shaped like one Pi actually writes must load and project cleanly.
func TestParsePiFixture(t *testing.T) {
	ctx := context.Background()
	storage, err := OpenJSONL(filepath.Join("testdata", "pi-session.jsonl"))
	if err != nil {
		t.Fatalf("failed to parse a Pi-shaped session: %v", err)
	}
	if errs := storage.SoftErrors(); len(errs) != 0 {
		t.Errorf("unexpected soft errors: %v", errs)
	}

	header := storage.Header()
	if header.Version != 3 || header.Cwd != "/Users/ben/Code/tau" {
		t.Errorf("header = %+v", header)
	}

	s := NewSession(storage)
	entries := s.Entries(ctx, nil)
	if len(entries) != 10 {
		t.Fatalf("entries = %d, want 10", len(entries))
	}

	if name, _ := s.Name(ctx); name != "Fix failing test" {
		t.Errorf("name = %q", name)
	}
	if label, ok := s.Label(ctx, "a1b2c3d4"); !ok || label != "checkpoint-1" {
		t.Errorf("label = %q %v", label, ok)
	}

	sc, err := s.BuildContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant, toolResult, custom_message — the custom entry and the
	// state-change entries contribute nothing.
	if len(sc.Messages) != 4 {
		t.Fatalf("context messages = %d, want 4", len(sc.Messages))
	}
	if sc.ThinkingLevel != "high" {
		t.Errorf("thinking = %q", sc.ThinkingLevel)
	}
	if sc.Model == nil || sc.Model.ModelID != "claude-opus-5" {
		t.Errorf("model = %+v, want the later model_change to win", sc.Model)
	}
	if !equalStrings(sc.ActiveToolNames, []string{"read", "bash", "edit"}) {
		t.Errorf("tools = %v", sc.ActiveToolNames)
	}

	stats := s.Stats(ctx)
	if stats.MessageCount != 3 {
		t.Errorf("message count = %d, want 3", stats.MessageCount)
	}
	if stats.CostTotal == 0 {
		t.Error("cost should be summed from the assistant message")
	}

	// The assistant message keeps its thinking block and tool call.
	llm := ConvertToLLM(sc.Messages)
	if len(llm) != 4 {
		t.Fatalf("llm messages = %d", len(llm))
	}
	assistant, ok := llm[1].(ai.AssistantMessage)
	if !ok {
		t.Fatalf("type = %T", llm[1])
	}
	if len(assistant.Content) != 3 {
		t.Errorf("assistant content blocks = %d, want 3", len(assistant.Content))
	}
}

// Every line of a Pi-authored session must survive a decode/encode cycle
// byte-for-byte, which is what makes fork lossless.
func TestPiFixtureRoundTripsByteForByte(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "pi-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")

	var header Header
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != lines[0] {
		t.Errorf("header round trip:\n got %s\nwant %s", out, lines[0])
	}

	for i, line := range lines[1:] {
		entry, err := UnmarshalEntry([]byte(line))
		if err != nil {
			t.Fatalf("line %d: %v", i+2, err)
		}
		out, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("line %d: %v", i+2, err)
		}
		if string(out) != line {
			t.Errorf("line %d round trip:\n got %s\nwant %s", i+2, out, line)
		}
	}
}

// --- helpers ---

func entryIDs(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Base().ID
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- concurrency ---

// A single process may drive a session from several goroutines. Appends are
// serialized internally, so every entry lands exactly once with an intact
// parent chain and no torn lines. Coordination *between* processes is out of
// scope: like Pi, one writer owns a session file.
func TestConcurrentAppendsAreSerialized(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	storage, err := CreateJSONL(path, CreateOptions{Cwd: "/proj", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(storage)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.AppendMessage(ctx, userMsg(fmt.Sprintf("msg-%d", i))); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append failed: %v", err)
	}

	reopened, err := OpenJSONL(path)
	if err != nil {
		t.Fatalf("file should be well-formed after concurrent appends: %v", err)
	}
	if errs := reopened.SoftErrors(); len(errs) != 0 {
		t.Errorf("soft errors after concurrent appends: %v", errs)
	}
	entries := reopened.Entries(ctx, nil)
	if len(entries) != n {
		t.Fatalf("entries = %d, want %d", len(entries), n)
	}

	// Ids are unique and the chain is intact: each entry's parent is the one
	// written before it.
	seen := map[string]bool{}
	for i, e := range entries {
		id := e.Base().ID
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true

		parent := e.Base().ParentID
		if i == 0 {
			if parent != nil {
				t.Errorf("first entry parent = %v, want nil", *parent)
			}
			continue
		}
		if parent == nil || *parent != entries[i-1].Base().ID {
			t.Errorf("entry %d parent = %v, want %s", i, parent, entries[i-1].Base().ID)
		}
	}
}
