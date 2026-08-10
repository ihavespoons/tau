package coding

import (
	"context"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func entryByID(t *testing.T, rows []TreeEntry, id string) TreeEntry {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("entry %s is not in the listing", id)
	return TreeEntry{}
}

// The whole point of the depth rule: a session is mostly a straight line, and
// indenting every message under the one before it would push a long
// conversation off the right edge without saying anything about its shape.
func TestALinearConversationStaysFlat(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs, user("one"), assistant("two"), user("three"), assistant("four"))

	rows, err := cs.TreeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("listing has %d rows, want 4", len(rows))
	}
	for _, r := range rows {
		if r.Depth != 0 {
			t.Errorf("%q is indented to depth %d in a conversation that never branched", r.Summary, r.Depth)
		}
	}
}

func TestAForkIndentsBothBranchesUnderTheirSharedParent(t *testing.T) {
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

	rows, err := cs.TreeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	shared := entryByID(t, rows, root)
	if shared.Depth != 0 {
		t.Errorf("the branch point sits at depth %d, want 0", shared.Depth)
	}
	if !shared.HasChildren {
		t.Error("the branch point does not report children, so it could not be folded")
	}

	branches := 0
	for _, r := range rows {
		if r.ParentID != root {
			continue
		}
		branches++
		if r.Depth != 1 {
			t.Errorf("%q sits at depth %d, want 1", r.Summary, r.Depth)
		}
	}
	// The leaf move is an entry too, so this counts the messages rather than
	// asserting a total.
	if branches < 2 {
		t.Errorf("only %d children hang off the branch point", branches)
	}
}

func TestTheListingKeepsEveryEntryIncludingBookkeeping(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs, user("do it"),
		ai.ToolResultMessage{ToolCallID: "c1", ToolName: "read",
			Content: ai.ContentList{ai.TextContent{Text: "file contents"}}})

	rows, err := cs.TreeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[TreeEntryKind]int{}
	for _, r := range rows {
		kinds[r.Kind]++
	}
	// RenderTree drops tool results; this listing must not, or the picker's
	// "show everything" filter would have nothing to show.
	if kinds[TreeToolResult] != 1 {
		t.Errorf("the tool result is missing from the listing: %v", kinds)
	}
	if kinds[TreeUser] != 1 {
		t.Errorf("the user message is missing: %v", kinds)
	}
}

func TestTheCurrentLeafIsMarked(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs, user("one"), assistant("two"))

	rows, err := cs.TreeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current := 0
	for _, r := range rows {
		if r.Current {
			current++
			if r.Summary != "assistant: two" {
				t.Errorf("the leaf is %q, want the last message", r.Summary)
			}
		}
	}
	if current != 1 {
		t.Errorf("%d rows claim to be the current position", current)
	}
}

// A turn that only called tools has nothing to read, so the picker hides it —
// but a turn that failed with nothing to read is exactly what someone
// scrolling back is looking for.
func TestToolOnlyTurnsAreMarkedButFailuresAreNot(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)

	silent := ai.AssistantMessage{
		Content:    ai.ContentList{ai.ToolCall{ID: "c1", Name: "read"}},
		StopReason: ai.StopToolUse,
	}
	failed := ai.AssistantMessage{
		Content:      ai.ContentList{},
		StopReason:   ai.StopError,
		ErrorMessage: "the provider hung up",
	}
	addHistory(t, cs, user("go"), silent, failed)

	rows, err := cs.TreeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sawSilent, sawFailed bool
	for _, r := range rows {
		if r.Kind != TreeAssistant {
			continue
		}
		if r.ToolOnly {
			sawSilent = true
		} else {
			sawFailed = true
		}
	}
	if !sawSilent {
		t.Error("a turn with only tool calls was not marked tool-only")
	}
	if !sawFailed {
		t.Error("a failed turn was marked tool-only, so the picker would hide it")
	}
}

func TestLabelsReachTheListing(t *testing.T) {
	ctx := context.Background()
	url, _ := summarizerServer(t, "x")
	cs := persistedSession(t, url, 200000)
	addHistory(t, cs, user("worth remembering"))

	id, err := cs.Session.LeafID(ctx)
	if err != nil || id == nil {
		t.Fatal("no leaf to label")
	}
	label := "before the refactor"
	if _, err := cs.Session.AppendLabel(ctx, *id, &label); err != nil {
		t.Fatal(err)
	}

	rows, err := cs.TreeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryByID(t, rows, *id).Label; got != label {
		t.Errorf("label = %q, want %q", got, label)
	}
}
