package coding

import (
	"context"
	"fmt"
	"strings"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/session"
)

// RenderTree draws the session tree as indented text.
//
// Entries that carry no conversation — a leaf move, a label — are collapsed
// away rather than shown. They are real structure, but the question /tree
// answers is "where in the conversation can I go back to", and a tree where
// every navigation left a node would drown the answer in its own history.
func RenderTree(ctx context.Context, s *session.Session, roots []*session.TreeNode) string {
	if len(roots) == 0 {
		return "This session has no history yet."
	}

	leafID := ""
	if s != nil {
		if id, err := s.LeafID(ctx); err == nil && id != nil {
			leafID = *id
		}
	}

	var b strings.Builder
	for _, root := range roots {
		renderNode(&b, root, 0, leafID)
	}
	if b.Len() == 0 {
		return "This session has no history yet."
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderNode prints a node if it is worth showing, then its children. A node
// that is not shown does not indent its children — otherwise the depth would
// track file structure rather than conversation structure.
func renderNode(b *strings.Builder, node *session.TreeNode, depth int, leafID string) {
	summary, show := describeEntry(node.Entry)
	childDepth := depth
	if show {
		marker := "  "
		if node.Entry.Base().ID == leafID {
			marker = "> "
		}
		fmt.Fprintf(b, "%s%s%s  %s", marker, strings.Repeat("  ", depth), node.Entry.Base().ID, summary)
		if node.Label != "" {
			fmt.Fprintf(b, "  [%s]", node.Label)
		}
		b.WriteByte('\n')
		childDepth++
	}
	for _, child := range node.Children {
		renderNode(b, child, childDepth, leafID)
	}
}

// describeEntry summarizes one entry for the tree, and reports whether it is
// worth a line at all.
func describeEntry(e session.Entry) (string, bool) {
	switch entry := e.(type) {
	case *session.MessageEntry:
		switch m := entry.Message.(type) {
		case ai.UserMessage:
			return "user: " + truncateLine(firstLine(m.Content.String()), 60), true
		case ai.AssistantMessage:
			return "assistant: " + truncateLine(firstLine(assistantText(m)), 60), true
		case ai.ToolResultMessage:
			return "", false
		default:
			return entry.Message.Role(), true
		}
	case *session.CompactionEntry:
		return fmt.Sprintf("compaction: %d tokens summarized", entry.TokensBefore), true
	case *session.BranchSummaryEntry:
		return "branch summary", true
	case *session.CustomMessageEntry:
		return "custom: " + entry.CustomType, true
	case *session.ModelChangeEntry:
		return "model: " + entry.Provider + "/" + entry.ModelID, true
	default:
		return "", false
	}
}

func assistantText(m ai.AssistantMessage) string {
	for _, c := range m.Content {
		if t, ok := c.(ai.TextContent); ok && strings.TrimSpace(t.Text) != "" {
			return t.Text
		}
	}
	for _, c := range m.Content {
		if tc, ok := c.(ai.ToolCall); ok {
			return "(" + tc.Name + ")"
		}
	}
	return "(no text)"
}
