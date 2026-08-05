package compaction

import (
	"context"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/faux"
	"github.com/ihavespoons/tau/session"
)

// forked builds a session that explored one branch and is about to go back to
// an earlier point: the shape /tree navigation produces.
func forked(t *testing.T) (*session.Session, context.Context, string, string) {
	t.Helper()
	s, ctx := newSession(t)

	shared, err := s.AppendMessage(ctx, userMsg("the shared beginning"))
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, ctx,
		userMsg("try the risky approach"),
		toolCallMsg("edit", map[string]any{"path": "/risky.go"}),
		toolResultMsg("edit", "ok"),
	)
	explored, err := s.AppendMessage(ctx, assistantMsg("that did not work", nil))
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, shared, explored
}

// The summary must cover exactly what the new position cannot see. Including
// the shared history would repeat context the conversation already has.
func TestOnlyTheAbandonedEntriesAreCollected(t *testing.T) {
	s, ctx, shared, explored := forked(t)

	got, err := CollectBranchEntries(ctx, s, explored, shared)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommonAncestorID != shared {
		t.Errorf("ancestor = %q, want %q", got.CommonAncestorID, shared)
	}
	if len(got.Entries) != 4 {
		t.Fatalf("collected %d entries, want the 4 after the fork point", len(got.Entries))
	}
	for _, e := range got.Entries {
		if e.Base().ID == shared {
			t.Error("the shared ancestor should not be summarized")
		}
	}
	// Chronological, because that is the order a summary reads in.
	if got.Entries[len(got.Entries)-1].Base().ID != explored {
		t.Error("entries should end at the position being left")
	}
}

func TestNavigatingFromNowhereCollectsNothing(t *testing.T) {
	s, ctx, shared, _ := forked(t)
	got, err := CollectBranchEntries(ctx, s, "", shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 0 {
		t.Errorf("collected %d entries from an empty position", len(got.Entries))
	}
}

// Moving forward along the same line abandons nothing — everything the old
// position saw, the new one still sees.
func TestNavigatingAlongTheSameLineCollectsNothing(t *testing.T) {
	s, ctx := newSession(t)
	first, err := s.AppendMessage(ctx, userMsg("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AppendMessage(ctx, userMsg("two"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := CollectBranchEntries(ctx, s, first, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 0 {
		t.Errorf("collected %d entries, want none", len(got.Entries))
	}
}

// A tool result adds nothing the call that produced it did not already say,
// and it is the bulkiest thing in a branch.
func TestToolResultsAreLeftOutOfTheBranchSummary(t *testing.T) {
	entries := []session.Entry{
		&session.MessageEntry{EntryBase: session.EntryBase{ID: "a"}, Message: toolCallMsg("read", map[string]any{"path": "/x.go"})},
		&session.MessageEntry{EntryBase: session.EntryBase{ID: "b"}, Message: toolResultMsg("read", strings.Repeat("z", 5000))},
	}
	prep := PrepareBranchEntries(entries, 0)
	if len(prep.Messages) != 1 {
		t.Fatalf("kept %d messages, want just the tool call", len(prep.Messages))
	}
	if _, isResult := prep.Messages[0].(ai.ToolResultMessage); isResult {
		t.Error("a tool result should not be summarized")
	}
	// The call's file tracking still has to survive.
	if !prep.FileOps.Read["/x.go"] {
		t.Error("the read was not tracked")
	}
}

// A branch too long to summarize whole is best described by where it got to.
func TestABranchOverBudgetKeepsItsNewestEntries(t *testing.T) {
	var entries []session.Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, &session.MessageEntry{
			EntryBase: session.EntryBase{ID: string(rune('a' + i))},
			Message:   userMsg(strings.Repeat("x", 4000)),
		})
	}
	entries = append(entries, &session.MessageEntry{
		EntryBase: session.EntryBase{ID: "newest"},
		Message:   userMsg("the last thing that happened"),
	})

	prep := PrepareBranchEntries(entries, 3000)
	if len(prep.Messages) == 0 || len(prep.Messages) == len(entries) {
		t.Fatalf("kept %d of %d messages; expected a partial tail", len(prep.Messages), len(entries))
	}
	last := prep.Messages[len(prep.Messages)-1].(ai.UserMessage)
	if last.Content.Text != "the last thing that happened" {
		t.Errorf("the newest entry was dropped: %q", last.Content.Text)
	}
	if prep.TotalTokens > 3000 {
		t.Errorf("kept %d tokens for a 3000 budget", prep.TotalTokens)
	}
}

// Which files a branch touched is short enough to always keep, and it is the
// one part of a summary that must not be paraphrased away.
func TestFileTrackingSurvivesEntriesThatDidNotFit(t *testing.T) {
	entries := []session.Entry{
		&session.MessageEntry{
			EntryBase: session.EntryBase{ID: "old"},
			Message:   toolCallMsg("edit", map[string]any{"path": "/early.go"}),
		},
	}
	for i := 0; i < 10; i++ {
		entries = append(entries, &session.MessageEntry{
			EntryBase: session.EntryBase{ID: string(rune('a' + i))},
			Message:   userMsg(strings.Repeat("x", 4000)),
		})
	}

	prep := PrepareBranchEntries(entries, 2000)
	if !prep.FileOps.Edited["/early.go"] {
		t.Error("an edit outside the token budget was forgotten")
	}
}

// A branch that itself contains a checkpoint has that checkpoint's file list
// as part of its own history; restarting the tracking would lose it.
func TestANestedBranchSummarysFilesCarryForward(t *testing.T) {
	entries := []session.Entry{
		&session.BranchSummaryEntry{
			EntryBase: session.EntryBase{ID: "bs"},
			FromID:    "x",
			Summary:   "an earlier exploration",
			Details:   FileLists{ReadFiles: []string{"/nested-read.go"}, ModifiedFiles: []string{"/nested-edit.go"}},
		},
	}
	prep := PrepareBranchEntries(entries, 0)
	lists := prep.FileOps.Lists()
	if len(lists.ReadFiles) != 1 || lists.ReadFiles[0] != "/nested-read.go" {
		t.Errorf("read files = %v", lists.ReadFiles)
	}
	if len(lists.ModifiedFiles) != 1 || lists.ModifiedFiles[0] != "/nested-edit.go" {
		t.Errorf("modified files = %v", lists.ModifiedFiles)
	}
}

func TestGenerateBranchSummaryFramesTheResult(t *testing.T) {
	s, ctx, shared, explored := forked(t)
	collected, err := CollectBranchEntries(ctx, s, explored, shared)
	if err != nil {
		t.Fatal(err)
	}

	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "## Goal\ntried the risky thing"}}})
	result, err := GenerateBranchSummary(ctx, collected.Entries, BranchOptions{
		Options: Options{Model: faux.Model(), Stream: script.StreamSimple, Settings: DefaultSettings},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Without the preamble the summary reads as work that was done, when it is
	// work that was abandoned.
	if !strings.HasPrefix(result.Summary, "The user explored a different conversation branch") {
		t.Errorf("summary is not framed as an abandoned branch: %q", result.Summary)
	}
	if !strings.Contains(result.Summary, "tried the risky thing") {
		t.Errorf("summary missing the model's text: %q", result.Summary)
	}
	if len(result.ModifiedFiles) != 1 || result.ModifiedFiles[0] != "/risky.go" {
		t.Errorf("modified files = %v, want /risky.go", result.ModifiedFiles)
	}
}

func TestABranchWithNothingInItNeedsNoRequest(t *testing.T) {
	script := faux.NewScript()
	result, err := GenerateBranchSummary(context.Background(), nil, BranchOptions{
		Options: Options{Model: faux.Model(), Stream: script.StreamSimple},
	})
	if err != nil {
		t.Fatal(err)
	}
	if script.Calls() != 0 {
		t.Errorf("made %d requests for an empty branch", script.Calls())
	}
	if result.Summary != "No content to summarize" {
		t.Errorf("summary = %q", result.Summary)
	}
}

// An extension replacing the instructions must get its prompt used verbatim,
// not appended to tau's.
func TestReplacedInstructionsAreTheWholePrompt(t *testing.T) {
	s, ctx, shared, explored := forked(t)
	collected, err := CollectBranchEntries(ctx, s, explored, shared)
	if err != nil {
		t.Fatal(err)
	}

	var prompt string
	script := faux.NewScript(faux.Turn{Blocks: []faux.Block{{Text: "ok"}}})
	_, err = GenerateBranchSummary(ctx, collected.Entries, BranchOptions{
		ReplaceInstructions: true,
		Options: Options{
			Model:              faux.Model(),
			CustomInstructions: "JUST LIST THE FILES",
			Stream: func(c context.Context, m *ai.Model, cc ai.Context, o *ai.SimpleStreamOptions) *ai.MessageStream {
				prompt = cc.Messages[0].(ai.UserMessage).Content.Blocks[0].(ai.TextContent).Text
				return script.StreamSimple(c, m, cc, o)
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "JUST LIST THE FILES") {
		t.Errorf("custom prompt missing:\n%s", prompt)
	}
	if strings.Contains(prompt, "## Constraints & Preferences") {
		t.Error("the default prompt should have been replaced, not appended to")
	}
	if strings.Contains(prompt, "Additional focus") {
		t.Error("replaced instructions should not also be appended")
	}
}

// An abort is the user pressing Escape. It is not a failure worth an error
// dialog, but it must not produce a summary either.
func TestAnAbortedBranchSummaryIsReportedAsCancellation(t *testing.T) {
	s, ctx, shared, explored := forked(t)
	collected, err := CollectBranchEntries(ctx, s, explored, shared)
	if err != nil {
		t.Fatal(err)
	}

	script := faux.NewScript(faux.Turn{ErrorMessage: "aborted", Stop: ai.StopAborted})
	_, err = GenerateBranchSummary(ctx, collected.Entries, BranchOptions{
		Options: Options{Model: faux.Model(), Stream: script.StreamSimple},
	})
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// The common ancestor has to be the DEEPEST entry both positions share, not
// the shallowest. Taking the root instead would put the whole shared history
// into the "abandoned" summary — repeating context the conversation already
// has, and inflating a checkpoint into a transcript.
func TestTheCommonAncestorIsTheDeepestSharedEntry(t *testing.T) {
	ctx := context.Background()
	s, _ := newSession(t)

	if _, err := s.AppendMessage(ctx, userMsg("the shared root")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, userMsg("more shared history")); err != nil {
		t.Fatal(err)
	}
	forkPoint, err := s.AppendMessage(ctx, userMsg("the fork point"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, userMsg("down the branch")); err != nil {
		t.Fatal(err)
	}
	explored, err := s.AppendMessage(ctx, assistantMsg("that did not work", nil))
	if err != nil {
		t.Fatal(err)
	}

	got, err := CollectBranchEntries(ctx, s, explored, forkPoint)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommonAncestorID != forkPoint {
		t.Errorf("ancestor = %q, want the fork point %q", got.CommonAncestorID, forkPoint)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("collected %d entries, want the 2 after the fork point", len(got.Entries))
	}
	for _, e := range got.Entries {
		if me, ok := e.(*session.MessageEntry); ok {
			if um, ok := me.Message.(ai.UserMessage); ok && strings.Contains(um.Content.Text, "shared") {
				t.Errorf("shared history was collected as abandoned: %q", um.Content.Text)
			}
		}
	}
}
