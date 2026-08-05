package session

import (
	"context"
	"sort"
)

// TreeNode is one entry with its children, as produced by Tree.
//
// It is a view over the log, not part of it: the tree is derived from parent
// links every time it is asked for, because the log is append-only and a
// materialized tree would only ever be a cache to invalidate.
type TreeNode struct {
	Entry    Entry
	Children []*TreeNode
	// Label is the entry's bookmark, empty when it has none.
	Label string
	// LabelTimestamp is when the label was last set.
	LabelTimestamp string
}

// Children returns the direct children of an entry, oldest first.
func (s *Session) Children(ctx context.Context, parentID string) []Entry {
	return childrenOf(s.storage.Entries(ctx, nil), parentID)
}

func childrenOf(entries []Entry, parentID string) []Entry {
	var out []Entry
	for _, e := range entries {
		p := e.Base().ParentID
		if p != nil && *p == parentID {
			out = append(out, e)
		}
	}
	sortByTimestamp(out)
	return out
}

// Tree returns the session's roots with children attached, oldest first.
//
// A well-formed session has exactly one root. An entry whose parent is missing
// is returned as a root too rather than dropped: a truncated or hand-edited
// file should still be navigable, and silently hiding entries would make the
// tree disagree with the file it came from.
func (s *Session) Tree(ctx context.Context) []*TreeNode {
	return BuildTree(s.storage.Entries(ctx, nil))
}

// BuildTree assembles the tree from a flat entry list in append order.
func BuildTree(entries []Entry) []*TreeNode {
	labels, times := labelState(entries)

	nodes := make(map[string]*TreeNode, len(entries))
	for _, e := range entries {
		id := e.Base().ID
		nodes[id] = &TreeNode{Entry: e, Label: labels[id], LabelTimestamp: times[id]}
	}

	var roots []*TreeNode
	for _, e := range entries {
		id := e.Base().ID
		node := nodes[id]
		parentID := e.Base().ParentID
		// A self-parented entry would otherwise be its own subtree and never
		// appear under any root, which is indistinguishable from data loss.
		if parentID == nil || *parentID == "" || *parentID == id {
			roots = append(roots, node)
			continue
		}
		if parent, ok := nodes[*parentID]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			roots = append(roots, node)
		}
	}

	sortNodes(roots)
	for _, n := range nodes {
		sortNodes(n.Children)
	}
	return roots
}

// labelState replays label entries in file order; the last write for a target
// wins and an empty label clears it.
func labelState(entries []Entry) (map[string]string, map[string]string) {
	labels := map[string]string{}
	times := map[string]string{}
	for _, e := range entries {
		l, ok := e.(*LabelEntry)
		if !ok {
			continue
		}
		if l.Label != nil && *l.Label != "" {
			labels[l.TargetID] = *l.Label
			times[l.TargetID] = l.Timestamp
		} else {
			delete(labels, l.TargetID)
			delete(times, l.TargetID)
		}
	}
	return labels, times
}

func sortNodes(nodes []*TreeNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		return parseTimestamp(nodes[i].Entry.Base().Timestamp) < parseTimestamp(nodes[j].Entry.Base().Timestamp)
	})
}

func sortByTimestamp(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return parseTimestamp(entries[i].Base().Timestamp) < parseTimestamp(entries[j].Base().Timestamp)
	})
}

// Walk visits every node depth-first, parents before children.
func Walk(roots []*TreeNode, visit func(node *TreeNode, depth int)) {
	var walk func(n *TreeNode, depth int)
	walk = func(n *TreeNode, depth int) {
		visit(n, depth)
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
}
