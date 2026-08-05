package session

import (
	"context"
	"testing"
)

func treeSession(t *testing.T) (*Session, context.Context) {
	t.Helper()
	return newTestSession(t), context.Background()
}

// The tree is what /tree navigates. A conversation that forked must come back
// as a fork, not as whichever branch happens to be current.
func TestForkedBranchesBothAppearInTheTree(t *testing.T) {
	s, ctx := treeSession(t)

	root, err := s.AppendMessage(ctx, userMsg("root"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.AppendMessage(ctx, userMsg("first branch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTo(ctx, &root, nil); err != nil {
		t.Fatal(err)
	}
	second, err := s.AppendMessage(ctx, userMsg("second branch"))
	if err != nil {
		t.Fatal(err)
	}

	children := s.Children(ctx, root)
	if len(children) != 2 {
		t.Fatalf("root has %d children, want 2", len(children))
	}
	if children[0].Base().ID != first || children[1].Base().ID != second {
		t.Errorf("children = %s, %s; want %s, %s in append order",
			children[0].Base().ID, children[1].Base().ID, first, second)
	}

	roots := s.Tree(ctx)
	if len(roots) != 1 {
		t.Fatalf("got %d roots, want 1", len(roots))
	}
	if roots[0].Entry.Base().ID != root {
		t.Errorf("root = %s, want %s", roots[0].Entry.Base().ID, root)
	}
	if len(roots[0].Children) != 2 {
		t.Errorf("root node has %d children, want 2", len(roots[0].Children))
	}
}

// A leaf move is itself an entry, and it parents onto wherever the leaf was.
// That is real structure, not noise: it is the record of the navigation.
func TestTheTreeReachesEveryEntryFromARoot(t *testing.T) {
	s, ctx := treeSession(t)

	root, err := s.AppendMessage(ctx, userMsg("root"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, userMsg("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTo(ctx, &root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, userMsg("b")); err != nil {
		t.Fatal(err)
	}

	all := s.Entries(ctx, nil)
	seen := map[string]bool{}
	Walk(s.Tree(ctx), func(node *TreeNode, _ int) { seen[node.Entry.Base().ID] = true })
	for _, e := range all {
		if !seen[e.Base().ID] {
			t.Errorf("entry %s (%s) is not reachable in the tree", e.Base().ID, e.EntryType())
		}
	}
}

func TestLabelsAreResolvedOntoTreeNodes(t *testing.T) {
	s, ctx := treeSession(t)

	target, err := s.AppendMessage(ctx, userMsg("interesting"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendLabel(ctx, target, ptr("checkpoint")); err != nil {
		t.Fatal(err)
	}

	var found *TreeNode
	Walk(s.Tree(ctx), func(node *TreeNode, _ int) {
		if node.Entry.Base().ID == target {
			found = node
		}
	})
	if found == nil {
		t.Fatal("labelled entry not in tree")
	}
	if found.Label != "checkpoint" {
		t.Errorf("label = %q, want checkpoint", found.Label)
	}
	if found.LabelTimestamp == "" {
		t.Error("a label should carry when it was set")
	}
}

// Clearing a label is a new entry, not a deletion. Replaying in file order is
// what makes the last write win.
func TestAClearedLabelDoesNotSurviveOnTheNode(t *testing.T) {
	s, ctx := treeSession(t)

	target, err := s.AppendMessage(ctx, userMsg("interesting"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendLabel(ctx, target, ptr("checkpoint")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendLabel(ctx, target, nil); err != nil {
		t.Fatal(err)
	}

	Walk(s.Tree(ctx), func(node *TreeNode, _ int) {
		if node.Entry.Base().ID == target && node.Label != "" {
			t.Errorf("label = %q, want it cleared", node.Label)
		}
	})
}

// A parent that is not in the file cannot be linked to. Dropping the entry
// would hide history; surfacing it as a root shows exactly what is there.
func TestAnOrphanedEntryIsReturnedAsARoot(t *testing.T) {
	missing := "nosuchid"
	entries := []Entry{
		&MessageEntry{EntryBase: EntryBase{ID: "aaaa", Timestamp: "2024-01-01T00:00:01.000Z"}, Message: userMsg("root")},
		&MessageEntry{EntryBase: EntryBase{ID: "bbbb", ParentID: &missing, Timestamp: "2024-01-01T00:00:02.000Z"}, Message: userMsg("orphan")},
	}
	roots := BuildTree(entries)
	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2", len(roots))
	}
	if roots[1].Entry.Base().ID != "bbbb" {
		t.Errorf("second root = %s, want bbbb", roots[1].Entry.Base().ID)
	}
}

// An entry naming itself as its own parent would be its own subtree and never
// appear under any root — indistinguishable from having lost it.
func TestASelfParentedEntryIsStillReachable(t *testing.T) {
	self := "aaaa"
	entries := []Entry{
		&MessageEntry{EntryBase: EntryBase{ID: self, ParentID: &self, Timestamp: "2024-01-01T00:00:01.000Z"}, Message: userMsg("loop")},
	}
	roots := BuildTree(entries)
	if len(roots) != 1 {
		t.Fatalf("got %d roots, want 1", len(roots))
	}
	if len(roots[0].Children) != 0 {
		t.Errorf("a self-parented entry should not be its own child")
	}
}

// Children come back oldest first so the newest branch sits at the bottom of
// the picker, where the eye already is.
func TestChildrenAreOrderedOldestFirst(t *testing.T) {
	parent := "p"
	entries := []Entry{
		&MessageEntry{EntryBase: EntryBase{ID: "p", Timestamp: "2024-01-01T00:00:00.000Z"}, Message: userMsg("p")},
		&MessageEntry{EntryBase: EntryBase{ID: "late", ParentID: &parent, Timestamp: "2024-01-01T00:00:09.000Z"}, Message: userMsg("late")},
		&MessageEntry{EntryBase: EntryBase{ID: "early", ParentID: &parent, Timestamp: "2024-01-01T00:00:02.000Z"}, Message: userMsg("early")},
	}
	roots := BuildTree(entries)
	kids := roots[0].Children
	if len(kids) != 2 {
		t.Fatalf("got %d children, want 2", len(kids))
	}
	if kids[0].Entry.Base().ID != "early" || kids[1].Entry.Base().ID != "late" {
		t.Errorf("children = %s, %s; want early, late", kids[0].Entry.Base().ID, kids[1].Entry.Base().ID)
	}
}

func TestChildrenOfAnEntryWithNoneIsEmpty(t *testing.T) {
	s, ctx := treeSession(t)
	id, err := s.AppendMessage(ctx, userMsg("only"))
	if err != nil {
		t.Fatal(err)
	}
	if kids := s.Children(ctx, id); len(kids) != 0 {
		t.Errorf("got %d children, want 0", len(kids))
	}
}
