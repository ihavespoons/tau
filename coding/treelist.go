package coding

import (
	"context"
	"fmt"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/session"
)

// TreeEntryKind classifies an entry for the tree picker's filters.
//
// The filters are over what an entry *is*, not over what it says, so the
// classification has to survive out of this package. A picker handed nothing but
// rendered strings would have to guess a message's role back out of its prefix.
type TreeEntryKind string

const (
	TreeUser          TreeEntryKind = "user"
	TreeAssistant     TreeEntryKind = "assistant"
	TreeToolResult    TreeEntryKind = "toolResult"
	TreeCompaction    TreeEntryKind = "compaction"
	TreeBranchSummary TreeEntryKind = "branchSummary"
	TreeCustom        TreeEntryKind = "custom"
	// TreeBookkeeping is an entry that records a setting rather than a turn: a
	// label, a model switch, a leaf move. It is real structure — the log is
	// append-only and these are how state changes are persisted — but it is
	// noise in every view except the one that asks for everything.
	TreeBookkeeping TreeEntryKind = "bookkeeping"
)

// TreeEntry is one row of the session tree.
type TreeEntry struct {
	ID       string
	ParentID string
	// Depth indents branch structure rather than message count: an entry
	// indents its children only where it has more than one. A linear
	// conversation — which is what most sessions are — therefore stays flat
	// instead of marching off the right edge one message at a time.
	Depth int
	Kind  TreeEntryKind
	// Summary is the single line describing this entry.
	Summary string
	// Label is the user's bookmark, empty when there is none.
	Label string
	// LabelTimestamp is when that bookmark was last set.
	LabelTimestamp string
	// Current marks the session's leaf — where the next message will attach.
	Current bool
	// ToolOnly marks an assistant turn that produced tool calls and no prose.
	// There is nothing to read on such a row, so a picker can drop it without
	// hiding anything the user wrote or was told.
	ToolOnly bool
	// HasChildren reports whether anything hangs off this entry, which is what
	// makes it worth folding.
	HasChildren bool
}

// TreeEntries flattens the whole session tree, depth-first in append order.
//
// This is deliberately unfiltered — every entry, bookkeeping included. Deciding
// what to show belongs to the caller, because the picker's filters change that
// answer on every keystroke and a listing that had already dropped rows could
// not answer them. RenderTree is the opinionated view; this is the raw one.
func (s *Session) TreeEntries(ctx context.Context) ([]TreeEntry, error) {
	if s.Session == nil {
		return nil, ErrNoSession
	}
	leafID := ""
	if id, err := s.Session.LeafID(ctx); err == nil && id != nil {
		leafID = *id
	}

	var out []TreeEntry
	for _, root := range s.Session.Tree(ctx) {
		flattenTreeNode(&out, root, 0, leafID)
	}
	return out, nil
}

// SetEntryLabel bookmarks any entry rather than the current position. An empty
// label clears one.
//
// /label answers "remember where I am"; this answers "remember that", pointed
// at a row someone is looking at in the tree picker.
func (s *Session) SetEntryLabel(ctx context.Context, entryID, label string) error {
	if s.Session == nil {
		return ErrNoSession
	}
	var value *string
	if label != "" {
		value = &label
	}
	_, err := s.Session.AppendLabel(ctx, entryID, value)
	return err
}

func flattenTreeNode(out *[]TreeEntry, node *session.TreeNode, depth int, leafID string) {
	base := node.Entry.Base()
	parent := ""
	if base.ParentID != nil {
		parent = *base.ParentID
	}
	kind, summary := classifyEntry(node.Entry)

	*out = append(*out, TreeEntry{
		ID:             base.ID,
		ParentID:       parent,
		Depth:          depth,
		Kind:           kind,
		Summary:        summary,
		Label:          node.Label,
		LabelTimestamp: node.LabelTimestamp,
		Current:        base.ID == leafID,
		ToolOnly:       toolOnlyTurn(node.Entry),
		HasChildren:    len(node.Children) > 0,
	})

	childDepth := depth
	if len(node.Children) > 1 {
		childDepth++
	}
	for _, child := range node.Children {
		flattenTreeNode(out, child, childDepth, leafID)
	}
}

// classifyEntry names an entry's kind and describes it in one line.
func classifyEntry(e session.Entry) (TreeEntryKind, string) {
	switch entry := e.(type) {
	case *session.MessageEntry:
		switch m := entry.Message.(type) {
		case ai.UserMessage:
			return TreeUser, "user: " + truncateLine(firstLine(m.Content.String()), 60)
		case ai.AssistantMessage:
			return TreeAssistant, "assistant: " + truncateLine(firstLine(assistantText(m)), 60)
		case ai.ToolResultMessage:
			return TreeToolResult, "tool: " + m.ToolName
		default:
			return TreeBookkeeping, entry.Message.Role()
		}
	case *session.CompactionEntry:
		return TreeCompaction, fmt.Sprintf("compaction: %d tokens summarized", entry.TokensBefore)
	case *session.BranchSummaryEntry:
		return TreeBranchSummary, "branch summary"
	case *session.CustomMessageEntry:
		return TreeCustom, "custom: " + entry.CustomType
	case *session.CustomEntry:
		return TreeBookkeeping, "extension state: " + entry.CustomType
	case *session.ModelChangeEntry:
		return TreeBookkeeping, "model: " + entry.Provider + "/" + entry.ModelID
	case *session.ThinkingLevelChangeEntry:
		return TreeBookkeeping, "thinking: " + entry.ThinkingLevel
	case *session.ActiveToolsChangeEntry:
		return TreeBookkeeping, fmt.Sprintf("tools: %d enabled", len(entry.ActiveToolNames))
	case *session.LabelEntry:
		if entry.Label == nil {
			return TreeBookkeeping, "label cleared"
		}
		return TreeBookkeeping, "label: " + truncateLine(*entry.Label, 40)
	case *session.SessionInfoEntry:
		return TreeBookkeeping, "named: " + entry.Name
	case *session.LeafEntry:
		return TreeBookkeeping, "moved"
	default:
		return TreeBookkeeping, e.EntryType()
	}
}

// toolOnlyTurn reports an assistant message that says nothing a reader could
// read. An error or an abort is exempt: an empty turn that failed is exactly
// the row someone scrolling back is looking for.
func toolOnlyTurn(e session.Entry) bool {
	me, ok := e.(*session.MessageEntry)
	if !ok {
		return false
	}
	am, ok := me.Message.(ai.AssistantMessage)
	if !ok {
		return false
	}
	switch am.StopReason {
	case "", ai.StopStop, ai.StopToolUse, ai.StopPending:
	default:
		return false
	}
	for _, c := range am.Content {
		if t, ok := c.(ai.TextContent); ok && strings.TrimSpace(t.Text) != "" {
			return false
		}
	}
	return true
}
