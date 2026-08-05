package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/faux"
	"github.com/ihavespoons/tau/session"
)

func newSession(t *testing.T) (*session.Session, context.Context) {
	t.Helper()
	return session.NewSession(session.NewMemStorage(session.Metadata{ID: "s", Cwd: "/w"})), context.Background()
}

func userMsg(text string) ai.Message {
	return ai.UserMessage{Content: ai.UserContent{Text: text}, Timestamp: 1}
}

func assistantMsg(text string, usage *ai.Usage) ai.Message {
	m := ai.AssistantMessage{
		Content:    ai.ContentList{ai.TextContent{Text: text}},
		Api:        "faux",
		Provider:   "faux",
		Model:      "faux-1",
		StopReason: ai.StopStop,
	}
	if usage != nil {
		m.Usage = *usage
	}
	return m
}

func toolCallMsg(name string, args map[string]any) ai.Message {
	return ai.AssistantMessage{
		Content:    ai.ContentList{ai.ToolCall{ID: "c1", Name: name, Arguments: args}},
		Api:        "faux",
		Provider:   "faux",
		Model:      "faux-1",
		StopReason: ai.StopToolUse,
	}
}

func toolResultMsg(name, text string) ai.Message {
	return ai.ToolResultMessage{
		ToolCallID: "c1", ToolName: name,
		Content: ai.ContentList{ai.TextContent{Text: text}},
	}
}

func branch(t *testing.T, s *session.Session, ctx context.Context) []session.Entry {
	t.Helper()
	entries, err := s.Branch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func appendAll(t *testing.T, s *session.Session, ctx context.Context, messages ...ai.Message) {
	t.Helper()
	for _, m := range messages {
		if _, err := s.AppendMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// Token estimation
// ---------------------------------------------------------------------------

// The provider's own count is the only measurement available; everything else
// is a guess. Preferring the guess would throw away the one true number.
func TestTheEstimateTrustsReportedUsageOverCountingCharacters(t *testing.T) {
	messages := []ai.Message{
		userMsg(strings.Repeat("x", 40000)),
		assistantMsg("ok", &ai.Usage{Input: 900, Output: 100, TotalTokens: 1000}),
	}
	est := EstimateContextTokens(messages)
	if est.Tokens != 1000 {
		t.Errorf("tokens = %d, want the reported 1000", est.Tokens)
	}
	if est.TrailingTokens != 0 {
		t.Errorf("trailing = %d, want 0", est.TrailingTokens)
	}
}

// Only what came after the measurement is guessed, so the error stays bounded
// to the tail instead of compounding over the whole history.
func TestOnlyMessagesAfterTheLastUsageAreEstimated(t *testing.T) {
	messages := []ai.Message{
		userMsg("early"),
		assistantMsg("ok", &ai.Usage{TotalTokens: 1000}),
		userMsg(strings.Repeat("y", 400)),
	}
	est := EstimateContextTokens(messages)
	if est.UsageTokens != 1000 {
		t.Errorf("usage tokens = %d, want 1000", est.UsageTokens)
	}
	if est.TrailingTokens != 100 {
		t.Errorf("trailing = %d, want 100", est.TrailingTokens)
	}
	if est.Tokens != 1100 {
		t.Errorf("total = %d, want 1100", est.Tokens)
	}
}

// An aborted turn's usage describes a partial request. Believing it would make
// the estimate drop and cancel a compaction that was due.
func TestUsageFromAnAbortedTurnIsNotBelieved(t *testing.T) {
	aborted := ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: "part"}},
		Usage:   ai.Usage{TotalTokens: 50},
		// A real abort reports whatever was consumed before the cut.
		StopReason: ai.StopAborted,
	}
	messages := []ai.Message{
		assistantMsg("full", &ai.Usage{TotalTokens: 9000}),
		aborted,
	}
	est := EstimateContextTokens(messages)
	if est.UsageTokens != 9000 {
		t.Errorf("usage tokens = %d, want the last complete turn's 9000", est.UsageTokens)
	}
}

// A conversation of screenshots has almost no characters. Weighing images at
// zero would let it overflow the window while the estimate read near empty.
func TestAnImageCountsTowardTheEstimate(t *testing.T) {
	withImage := ai.UserMessage{Content: ai.UserContent{Blocks: ai.ContentList{
		ai.ImageContent{Data: "iVBOR", MimeType: "image/png"},
	}}}
	if got := EstimateTokens(withImage); got != estimatedImageChars/4 {
		t.Errorf("image tokens = %d, want %d", got, estimatedImageChars/4)
	}
}

func TestShouldCompactRespectsTheReserve(t *testing.T) {
	s := Settings{Enabled: true, ReserveTokens: 1000}
	if ShouldCompact(8999, 10000, s) {
		t.Error("9k of a 10k window with 1k reserved is not yet over")
	}
	if !ShouldCompact(9001, 10000, s) {
		t.Error("past the reserve should compact")
	}
	if ShouldCompact(99999, 10000, Settings{Enabled: false, ReserveTokens: 1000}) {
		t.Error("disabled means never")
	}
}

// ---------------------------------------------------------------------------
// Cut points
// ---------------------------------------------------------------------------

// A tool result with no tool call before it is not a conversation a provider
// will accept — it is a 400. This is the constraint the whole cut-point search
// exists to respect.
func TestTheCutNeverLandsOnAToolResult(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx,
		userMsg(strings.Repeat("a", 8000)),
		toolCallMsg("read", map[string]any{"path": "/a.go"}),
		toolResultMsg("read", strings.Repeat("b", 8000)),
		assistantMsg("done", nil),
	)

	entries := branch(t, s, ctx)
	for keep := 1; keep < 6000; keep += 137 {
		cut := FindCutPoint(entries, 0, len(entries), keep)
		kept := entries[cut.FirstKeptEntryIndex]
		me, ok := kept.(*session.MessageEntry)
		if !ok {
			continue
		}
		if _, isResult := me.Message.(ai.ToolResultMessage); isResult {
			t.Fatalf("keepRecentTokens=%d cut at a tool result", keep)
		}
	}
}

// Cutting mid-turn is allowed, but the turn's opening request has to be
// findable — the retained work is unintelligible without what asked for it.
func TestASplitTurnReportsWhereTheTurnBegan(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx,
		userMsg("old history"),
		assistantMsg("old reply", nil),
		userMsg("the request"),
		toolCallMsg("read", map[string]any{"path": "/a.go"}),
		toolResultMsg("read", strings.Repeat("b", 200)),
		assistantMsg(strings.Repeat("c", 8000), nil),
	)

	entries := branch(t, s, ctx)
	cut := FindCutPoint(entries, 0, len(entries), 1500)
	if !cut.IsSplitTurn {
		t.Fatalf("expected a split turn, got %+v", cut)
	}
	if cut.TurnStartIndex < 0 || cut.TurnStartIndex >= cut.FirstKeptEntryIndex {
		t.Errorf("turn start %d is not before the cut at %d", cut.TurnStartIndex, cut.FirstKeptEntryIndex)
	}
	me := entries[cut.TurnStartIndex].(*session.MessageEntry)
	if _, isUser := me.Message.(ai.UserMessage); !isUser {
		t.Errorf("turn start is %T, want a user message", me.Message)
	}
}

// Snapping forward keeps the tail no larger than asked for. Snapping backwards
// would leave the context above the window the compaction was called to clear.
func TestTheKeptTailDoesNotExceedTheBudget(t *testing.T) {
	s, ctx := newSession(t)
	for i := 0; i < 12; i++ {
		appendAll(t, s, ctx, userMsg(strings.Repeat("u", 4000)), assistantMsg(strings.Repeat("a", 4000), nil))
	}

	entries := branch(t, s, ctx)
	const budget = 3000
	cut := FindCutPoint(entries, 0, len(entries), budget)

	kept := 0
	for i := cut.FirstKeptEntryIndex; i < len(entries); i++ {
		for _, m := range entryMessages(entries[i]) {
			kept += EstimateTokens(m)
		}
	}
	// One message can exceed the budget on its own; the guarantee is that the
	// tail is not budget plus a whole extra message.
	if kept > budget*2 {
		t.Errorf("kept %d tokens for a %d budget", kept, budget)
	}
}

// A model change carries no message but changes how everything after it was
// produced. Leaving it on the discarded side loses that.
func TestMetadataEntriesBeforeTheCutAreKept(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx, userMsg(strings.Repeat("a", 8000)))
	if _, err := s.AppendModelChange(ctx, "anthropic", "claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, ctx, userMsg(strings.Repeat("b", 8000)))

	entries := branch(t, s, ctx)
	cut := FindCutPoint(entries, 0, len(entries), 100)
	if _, isChange := entries[cut.FirstKeptEntryIndex].(*session.ModelChangeEntry); !isChange {
		t.Errorf("cut at %T, want the model change to be pulled in", entries[cut.FirstKeptEntryIndex])
	}
}

// ---------------------------------------------------------------------------
// Preparation
// ---------------------------------------------------------------------------

func TestPrepareDeclinesABranchThatAlreadyEndsInACheckpoint(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx, userMsg("hello"), assistantMsg("hi", nil))
	if _, err := s.AppendCompaction(ctx, "summary", 100, session.CompactionOptions{}); err != nil {
		t.Fatal(err)
	}
	if prep := Prepare(branch(t, s, ctx), smallTail); prep != nil {
		t.Errorf("prepared %+v, want nil", prep)
	}
}

func TestPrepareDeclinesWhenEverythingFitsInTheTail(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx, userMsg("hello"), assistantMsg("hi", nil))
	if prep := Prepare(branch(t, s, ctx), smallTail); prep != nil {
		t.Errorf("prepared %+v, want nil", prep)
	}
}

// The point of a checkpoint is that everything before it is replaced. If the
// second compaction re-summarized the first one's history, the summary would
// be a summary of a summary and the detail would erode every time.
func TestASecondCompactionOnlySummarizesWhatCameAfterTheFirst(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx, userMsg("ancient"), assistantMsg("ancient reply", nil))
	if _, err := s.AppendCompaction(ctx, "the first summary", 100, session.CompactionOptions{
		Details: FileLists{ReadFiles: []string{"/old.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, ctx,
		userMsg(strings.Repeat("n", 40000)),
		assistantMsg(strings.Repeat("m", 40000), nil),
		userMsg("recent"),
	)

	prep := Prepare(branch(t, s, ctx), smallTail)
	if prep == nil {
		t.Fatal("expected a preparation")
		return
	}
	if prep.PreviousSummary != "the first summary" {
		t.Errorf("previous summary = %q, want the first checkpoint's", prep.PreviousSummary)
	}
	for _, m := range prep.MessagesToSummarize {
		if text, ok := m.(ai.UserMessage); ok && text.Content.Text == "ancient" {
			t.Error("history from before the checkpoint was queued for summarizing again")
		}
	}
	// The earlier checkpoint's file list has to carry forward, or a file read
	// twenty turns ago stops being mentioned at all.
	if !prep.FileOps.Read["/old.go"] {
		t.Error("the previous checkpoint's read files were dropped")
	}
}

// An extension writes whatever it likes into details. Reading it as tau's shape
// would either fail or, worse, half-succeed.
func TestAnExtensionCheckpointsDetailsAreNotReinterpreted(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx, userMsg("ancient"))
	if _, err := s.AppendCompaction(ctx, "hook summary", 100, session.CompactionOptions{
		FromHook: true,
		Details:  map[string]any{"readFiles": []string{"/hook.go"}},
	}); err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, ctx,
		userMsg("after the checkpoint"),
		assistantMsg("ok", nil),
		userMsg(strings.Repeat("recent ", 500)),
	)

	prep := Prepare(branch(t, s, ctx), smallTail)
	if prep == nil {
		t.Fatal("expected a preparation")
		return
	}
	if prep.FileOps.Read["/hook.go"] {
		t.Error("an extension checkpoint's details were read as tau's own")
	}
}

func TestFileOperationsAreCollectedFromToolCalls(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx,
		userMsg("go"),
		toolCallMsg("read", map[string]any{"path": "/z-read.go"}),
		toolResultMsg("read", "contents"),
		toolCallMsg("read", map[string]any{"path": "/read-only.go"}),
		toolResultMsg("read", "contents"),
		toolCallMsg("read", map[string]any{"path": "/a-read.go"}),
		toolResultMsg("read", "contents"),
		toolCallMsg("edit", map[string]any{"path": "/changed.go"}),
		toolResultMsg("edit", "ok"),
		toolCallMsg("read", map[string]any{"path": "/changed.go"}),
		toolResultMsg("read", "contents"),
		userMsg(strings.Repeat("recent ", 500)),
	)

	prep := Prepare(branch(t, s, ctx), smallTail)
	if prep == nil {
		t.Fatal("expected a preparation")
		return
	}
	lists := prep.FileOps.Lists()
	// Sorted, because a summary of the same work twice should read the same
	// twice. Go map iteration is not an order.
	want := []string{"/a-read.go", "/read-only.go", "/z-read.go"}
	if strings.Join(lists.ReadFiles, ",") != strings.Join(want, ",") {
		t.Errorf("read files = %v, want %v in sorted order", lists.ReadFiles, want)
	}
	// A file that was read and then changed is a changed file. Listing it as
	// read would tell the next model it is still as it was on disk.
	if len(lists.ModifiedFiles) != 1 || lists.ModifiedFiles[0] != "/changed.go" {
		t.Errorf("modified files = %v, want just /changed.go", lists.ModifiedFiles)
	}
}

// ---------------------------------------------------------------------------
// Compact
// ---------------------------------------------------------------------------

func fauxOptions(script *faux.Script) Options {
	return Options{Model: faux.Model(), Stream: script.StreamSimple, Settings: smallTail}
}

// smallTail keeps the fixtures readable. The retained tail has to be smaller
// than the conversation or there is no history left to summarize, and writing
// 20k tokens of filler into a test to reach the real default proves nothing the
// cut-point tests do not already cover.
var smallTail = Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 500}

func longSession(t *testing.T) (*session.Session, context.Context) {
	t.Helper()
	s, ctx := newSession(t)
	appendAll(t, s, ctx,
		userMsg("please write the file"),
		toolCallMsg("write", map[string]any{"path": "/made.go"}),
		toolResultMsg("write", "ok"),
		assistantMsg("done", nil),
		// The retained tail is whatever the walk backwards reaches first, so
		// the recent turn is the bulky one here.
		userMsg(strings.Repeat("what now? ", 400)),
	)
	return s, ctx
}

func TestCompactProducesASummaryWithTheFileLists(t *testing.T) {
	s, ctx := longSession(t)
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "## Goal\nship it"}}})

	prep := Prepare(branch(t, s, ctx), smallTail)
	if prep == nil {
		t.Fatal("expected a preparation")
	}
	result, err := Compact(ctx, prep, fauxOptions(script))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Summary, "ship it") {
		t.Errorf("summary missing the model's text: %q", result.Summary)
	}
	if !strings.Contains(result.Summary, "<modified-files>\n/made.go") {
		t.Errorf("summary missing the modified files block: %q", result.Summary)
	}
	if result.FirstKeptEntryID == "" {
		t.Error("result has no first kept entry")
	}
	if result.Details.ModifiedFiles[0] != "/made.go" {
		t.Errorf("details = %+v", result.Details)
	}
}

// A summary is a one-shot request whose prompt will never be sent again. A
// cache write would be billed and never read, and reusing the session's routing
// id would evict the conversation's own cached prefix.
func TestSummarizationDoesNotWriteToTheSessionCache(t *testing.T) {
	s, ctx := longSession(t)

	var seen ai.SimpleStreamOptions
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "summary"}}})
	opts := fauxOptions(script)
	opts.StreamOptions = ai.SimpleStreamOptions{
		StreamOptions: ai.StreamOptions{
			APIKey:         "k",
			CacheRetention: ai.CacheLong,
			SessionID:      "the-conversation",
		},
	}
	opts.Stream = func(c context.Context, m *ai.Model, cc ai.Context, o *ai.SimpleStreamOptions) *ai.MessageStream {
		seen = *o
		return script.StreamSimple(c, m, cc, o)
	}

	prep := Prepare(branch(t, s, ctx), smallTail)
	if _, err := Compact(ctx, prep, opts); err != nil {
		t.Fatal(err)
	}
	if seen.CacheRetention != ai.CacheNone {
		t.Errorf("cache retention = %q, want none", seen.CacheRetention)
	}
	if seen.SessionID == "the-conversation" || seen.SessionID == "" {
		t.Errorf("session id = %q, want a fresh one", seen.SessionID)
	}
	if seen.APIKey != "k" {
		t.Error("credentials should still be passed through")
	}
}

// The conversation goes in as one block of text, not as messages. Handed a
// conversation, a model continues it; handed a transcript, it summarizes.
func TestTheConversationIsSentAsASingleSerializedPrompt(t *testing.T) {
	s, ctx := longSession(t)

	var sent ai.Context
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "summary"}}})
	opts := fauxOptions(script)
	opts.Stream = func(c context.Context, m *ai.Model, cc ai.Context, o *ai.SimpleStreamOptions) *ai.MessageStream {
		sent = cc
		return script.StreamSimple(c, m, cc, o)
	}

	prep := Prepare(branch(t, s, ctx), smallTail)
	if _, err := Compact(ctx, prep, opts); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sent.Messages))
	}
	if sent.SystemPrompt != SummarizationSystemPrompt {
		t.Error("the summarization system prompt was not used")
	}
	prompt := sent.Messages[0].(ai.UserMessage).Content.Blocks[0].(ai.TextContent).Text
	if !strings.Contains(prompt, "<conversation>") {
		t.Error("the conversation should be wrapped in tags")
	}
	if !strings.Contains(prompt, "[Assistant tool calls]: write(path=\"/made.go\")") {
		t.Errorf("tool calls should be serialized into the prompt:\n%s", prompt)
	}
}

// Failing loudly beats writing a checkpoint that says "unknown error": the
// checkpoint replaces the history, so a bad one destroys it.
func TestASummarizationErrorFailsTheCompaction(t *testing.T) {
	s, ctx := longSession(t)
	script := faux.NewScript(faux.Turn{ErrorMessage: "provider exploded", Stop: ai.StopError})

	prep := Prepare(branch(t, s, ctx), smallTail)
	_, err := Compact(ctx, prep, fauxOptions(script))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "provider exploded") {
		t.Errorf("err = %v, want the provider's message", err)
	}
}

func TestCompactWithoutAStreamFunctionIsAnError(t *testing.T) {
	s, ctx := longSession(t)
	prep := Prepare(branch(t, s, ctx), smallTail)
	if _, err := Compact(ctx, prep, Options{Model: faux.Model()}); err == nil {
		t.Fatal("expected an error")
	}
}

// A split turn needs both halves described, and it costs two requests.
func TestASplitTurnSummarizesTheHistoryAndTheTurnPrefixSeparately(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx,
		userMsg("old history"),
		assistantMsg("old reply", nil),
		userMsg("the big request"),
		toolCallMsg("read", map[string]any{"path": "/a.go"}),
		toolResultMsg("read", strings.Repeat("b", 200)),
		assistantMsg(strings.Repeat("c", 80000), nil),
	)

	script := faux.NewScript(
		faux.Turn{Blocks: []faux.Block{{Text: "HISTORY SUMMARY"}}},
		faux.Turn{Blocks: []faux.Block{{Text: "PREFIX SUMMARY"}}},
	)
	prep := Prepare(branch(t, s, ctx), Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 1500})
	if prep == nil || !prep.IsSplitTurn {
		t.Fatalf("expected a split turn, got %+v", prep)
	}

	result, err := Compact(ctx, prep, fauxOptions(script))
	if err != nil {
		t.Fatal(err)
	}
	if script.Calls() != 2 {
		t.Errorf("made %d requests, want 2", script.Calls())
	}
	if !strings.Contains(result.Summary, "HISTORY SUMMARY") || !strings.Contains(result.Summary, "PREFIX SUMMARY") {
		t.Errorf("both summaries should be present: %q", result.Summary)
	}
	if !strings.Contains(result.Summary, "Turn Context (split turn)") {
		t.Error("the split should be labelled for whoever reads the checkpoint")
	}
}

// A follow-on compaction updates the previous summary rather than starting
// over, so one cumulative record survives a long session.
func TestASecondCompactionUsesTheUpdatePrompt(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx, userMsg("ancient"))
	if _, err := s.AppendCompaction(ctx, "THE EARLIER SUMMARY", 100, session.CompactionOptions{}); err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, ctx,
		userMsg("after the checkpoint"),
		assistantMsg("ok", nil),
		userMsg(strings.Repeat("recent ", 500)),
	)

	var prompt string
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "updated"}}})
	opts := fauxOptions(script)
	opts.Stream = func(c context.Context, m *ai.Model, cc ai.Context, o *ai.SimpleStreamOptions) *ai.MessageStream {
		prompt = cc.Messages[0].(ai.UserMessage).Content.Blocks[0].(ai.TextContent).Text
		return script.StreamSimple(c, m, cc, o)
	}

	prep := Prepare(branch(t, s, ctx), smallTail)
	if prep == nil {
		t.Fatal("expected a preparation")
	}
	if _, err := Compact(ctx, prep, opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "<previous-summary>\nTHE EARLIER SUMMARY") {
		t.Errorf("the earlier summary should be in the prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "PRESERVE all existing information") {
		t.Error("the update prompt should be used, not the initial one")
	}
}

func TestCustomInstructionsReachThePrompt(t *testing.T) {
	s, ctx := longSession(t)

	var prompt string
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "summary"}}})
	opts := fauxOptions(script)
	opts.CustomInstructions = "focus on the build failures"
	opts.Stream = func(c context.Context, m *ai.Model, cc ai.Context, o *ai.SimpleStreamOptions) *ai.MessageStream {
		prompt = cc.Messages[0].(ai.UserMessage).Content.Blocks[0].(ai.TextContent).Text
		return script.StreamSimple(c, m, cc, o)
	}

	prep := Prepare(branch(t, s, ctx), smallTail)
	if _, err := Compact(ctx, prep, opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Additional focus: focus on the build failures") {
		t.Errorf("custom instructions missing:\n%s", prompt)
	}
}

// The summary has to fit in the headroom the compaction was called to create.
func TestTheSummaryBudgetStaysUnderTheModelsLimit(t *testing.T) {
	s, ctx := longSession(t)

	var maxTokens int
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "summary"}}})
	opts := fauxOptions(script)
	small := faux.Model()
	small.MaxTokens = 512
	opts.Model = small
	opts.Stream = func(c context.Context, m *ai.Model, cc ai.Context, o *ai.SimpleStreamOptions) *ai.MessageStream {
		maxTokens = o.MaxTokens
		return script.StreamSimple(c, m, cc, o)
	}

	prep := Prepare(branch(t, s, ctx), smallTail)
	if _, err := Compact(ctx, prep, opts); err != nil {
		t.Fatal(err)
	}
	if maxTokens != 512 {
		t.Errorf("max tokens = %d, want the model's 512", maxTokens)
	}
}

// ---------------------------------------------------------------------------
// Serialization
// ---------------------------------------------------------------------------

// Go map iteration is random. An unsorted rendering would make two
// summarizations of the same conversation differ for no reason at all.
func TestToolCallArgumentsSerializeInAStableOrder(t *testing.T) {
	call := toolCallMsg("edit", map[string]any{"path": "/a.go", "old": "x", "new": "y"})
	first := SerializeConversation([]ai.Message{call})
	for i := 0; i < 20; i++ {
		if got := SerializeConversation([]ai.Message{call}); got != first {
			t.Fatalf("serialization is unstable:\n%s\nvs\n%s", first, got)
		}
	}
	if !strings.Contains(first, `edit(new="y", old="x", path="/a.go")`) {
		t.Errorf("unexpected rendering: %s", first)
	}
}

// A single large file read would otherwise eat the budget meant for the whole
// history.
func TestALongToolResultIsTruncatedInTheTranscript(t *testing.T) {
	out := SerializeConversation([]ai.Message{toolResultMsg("read", strings.Repeat("z", 5000))})
	if !strings.Contains(out, "[... 3000 more characters truncated]") {
		t.Errorf("expected a truncation marker:\n%s", out[len(out)-120:])
	}
	if len(out) > toolResultMaxChars+200 {
		t.Errorf("truncated output is still %d chars", len(out))
	}
}

func TestThinkingAndTextAreLabelledSeparately(t *testing.T) {
	msg := ai.AssistantMessage{Content: ai.ContentList{
		ai.ThinkingContent{Thinking: "let me see"},
		ai.TextContent{Text: "here you go"},
	}}
	out := SerializeConversation([]ai.Message{msg})
	if !strings.Contains(out, "[Assistant thinking]: let me see") {
		t.Errorf("thinking not labelled:\n%s", out)
	}
	if !strings.Contains(out, "[Assistant]: here you go") {
		t.Errorf("text not labelled:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Cases the mutation run found nothing testing
// ---------------------------------------------------------------------------

// A provider that reports usage but fills it with zeros has measured nothing.
// Believing it makes the context read as empty, and compaction — which is
// driven entirely by that number — then never fires at all.
func TestAnAllZeroUsageIsNotAMeasurement(t *testing.T) {
	messages := []ai.Message{
		userMsg(strings.Repeat("x", 40000)),
		assistantMsg("ok", &ai.Usage{}),
	}
	est := EstimateContextTokens(messages)
	if est.LastUsageIndex != -1 {
		t.Errorf("last usage index = %d, want -1: an all-zero usage measured nothing", est.LastUsageIndex)
	}
	if est.Tokens < 10000 {
		t.Errorf("tokens = %d; the estimate should have fallen back to counting characters", est.Tokens)
	}
}

// The cut snaps forward to the next legal point, never back to the previous
// one.
//
// The two differ only when the budget runs out on an entry that is not itself
// a legal cut point — a tool result. Forward means the huge result is
// discarded; backward means it and the call before it are kept, which is the
// bulk compaction was called to get rid of.
func TestTheCutSnapsForwardPastTheMessageThatBrokeTheBudget(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx,
		userMsg("the request"),
		toolCallMsg("read", map[string]any{"path": "/huge.go"}),
		toolResultMsg("read", strings.Repeat("z", 4000)), // 1000 tokens, not a cut point
		userMsg("the recent question"),
	)

	entries := branch(t, s, ctx)
	cut := FindCutPoint(entries, 0, len(entries), 1000)
	if cut.FirstKeptEntryIndex != 3 {
		t.Errorf("kept from index %d, want 3: the budget ran out on the tool result, "+
			"so the cut belongs after it, not before the call that produced it",
			cut.FirstKeptEntryIndex)
	}
}

// A checkpoint stands for the history it replaced. Feeding it back in as
// content would summarize a summary, and each pass would erode more of the
// detail the checkpoint exists to preserve.
func TestAnEarlierCheckpointIsNotSummarizedAsContent(t *testing.T) {
	s, ctx := newSession(t)
	appendAll(t, s, ctx, userMsg("ancient"))

	// A checkpoint that names an entry before itself, which is the shape Pi
	// writes: the boundary then starts before the checkpoint, so the checkpoint
	// itself falls inside the range a second compaction looks at.
	kept, err := s.AppendMessage(ctx, userMsg("the entry the checkpoint kept"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendCompaction(ctx, "THE EARLIER SUMMARY", 100, session.CompactionOptions{
		FirstKeptEntryID: kept,
	}); err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, ctx,
		userMsg("after the checkpoint"),
		assistantMsg("ok", nil),
		userMsg(strings.Repeat("recent ", 500)),
	)

	prep := Prepare(branch(t, s, ctx), smallTail)
	if prep == nil {
		t.Fatal("expected a preparation")
		return
	}
	serialized := SerializeConversation(session.ConvertToLLM(prep.MessagesToSummarize))
	if strings.Contains(serialized, "THE EARLIER SUMMARY") {
		t.Errorf("the earlier checkpoint was queued for re-summarizing:\n%s", serialized)
	}
	// It belongs in the update prompt instead, exactly once.
	if prep.PreviousSummary != "THE EARLIER SUMMARY" {
		t.Errorf("previous summary = %q", prep.PreviousSummary)
	}
}
