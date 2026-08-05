package compaction

import (
	"context"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/session"
)

// Branch summarization: what happens when the user navigates away from where
// they were working.
//
// Compaction replaces history the conversation is finished with. This replaces
// history the conversation is *leaving* — a branch that stays in the file and
// stays reachable, but is about to stop being in context. Without the summary
// the next turn has no idea the exploration happened.

// BranchCollection is the stretch of tree being left behind.
type BranchCollection struct {
	// Entries are the ones only the old position could see, oldest first.
	Entries []session.Entry
	// CommonAncestorID is where the two positions rejoin, empty if nowhere.
	CommonAncestorID string
}

// CollectBranchEntries gathers what is visible from oldLeafID but not from
// targetID.
//
// It walks back from the old position to the deepest entry both positions
// share. Compaction checkpoints along the way are included rather than treated
// as a boundary: their summaries are the branch's own history, and stopping at
// one would summarize only what happened since the last checkpoint.
func CollectBranchEntries(ctx context.Context, s *session.Session, oldLeafID, targetID string) (BranchCollection, error) {
	if oldLeafID == "" {
		return BranchCollection{}, nil
	}

	oldPath, err := s.Branch(ctx, &oldLeafID)
	if err != nil {
		return BranchCollection{}, err
	}
	onOldPath := make(map[string]bool, len(oldPath))
	for _, e := range oldPath {
		onOldPath[e.Base().ID] = true
	}

	targetPath, err := s.Branch(ctx, &targetID)
	if err != nil {
		return BranchCollection{}, err
	}

	ancestor := ""
	for i := len(targetPath) - 1; i >= 0; i-- {
		if onOldPath[targetPath[i].Base().ID] {
			ancestor = targetPath[i].Base().ID
			break
		}
	}

	var entries []session.Entry
	current := oldLeafID
	for current != "" && current != ancestor {
		entry, ok := s.Entry(ctx, current)
		if !ok {
			break
		}
		entries = append(entries, entry)
		parent := entry.Base().ParentID
		if parent == nil {
			break
		}
		current = *parent
	}

	// Collected leaf-first; the summarizer reads chronologically.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return BranchCollection{Entries: entries, CommonAncestorID: ancestor}, nil
}

// BranchPreparation is the material for one branch summary.
type BranchPreparation struct {
	Messages    []ai.Message
	FileOps     *FileOps
	TotalTokens int
}

// branchMessage projects an entry for branch summarization.
//
// Unlike compaction this keeps compaction entries — a summary of the branch has
// to include the branch's own checkpoints — and drops tool results, whose
// content is already implied by the call that produced them.
func branchMessage(e session.Entry) (ai.Message, bool) {
	if me, ok := e.(*session.MessageEntry); ok {
		switch me.Message.(type) {
		case ai.ToolResultMessage, *ai.ToolResultMessage:
			return nil, false
		}
	}
	msgs := session.EntryToContextMessages(e, 0, nil, session.BuildOptions{})
	if len(msgs) == 0 {
		return nil, false
	}
	return msgs[0], true
}

// PrepareBranchEntries picks what fits in the budget, newest first.
//
// Newest first because a branch too long to summarize whole is still best
// described by where it got to. A budget of zero means no limit.
func PrepareBranchEntries(entries []session.Entry, tokenBudget int) BranchPreparation {
	fileOps := NewFileOps()

	// File tracking is gathered from every entry, including the ones that will
	// not fit. A path is a few dozen bytes and it is the one part of a summary
	// that must survive verbatim — a file the branch edited stays edited on
	// disk whether or not the conversation about it fit in the budget.
	//
	// Pi intends this (its comment says "even if they don't fit") but only
	// applies it to nested summaries; tool calls beyond the budget are dropped
	// there. tau tracks both, which can list more files than Pi would for the
	// same branch.
	for _, e := range entries {
		if bs, ok := e.(*session.BranchSummaryEntry); ok && !bs.FromHook {
			if lists, ok := decodeFileLists(bs.Details); ok {
				fileOps.AddLists(lists)
			}
		}
		if message, ok := branchMessage(e); ok {
			fileOps.AddFromMessage(message)
		}
	}

	var messages []ai.Message
	total := 0
	for i := len(entries) - 1; i >= 0; i-- {
		message, ok := branchMessage(entries[i])
		if !ok {
			continue
		}
		tokens := EstimateTokens(message)

		if tokenBudget > 0 && total+tokens > tokenBudget {
			// A summary entry is worth squeezing in over the line: it stands
			// for far more conversation than it costs.
			switch entries[i].(type) {
			case *session.CompactionEntry, *session.BranchSummaryEntry:
				if total < tokenBudget*9/10 {
					messages = append([]ai.Message{message}, messages...)
					total += tokens
				}
			}
			break
		}

		messages = append([]ai.Message{message}, messages...)
		total += tokens
	}

	return BranchPreparation{Messages: messages, FileOps: fileOps, TotalTokens: total}
}

// BranchOptions tunes a branch summary.
type BranchOptions struct {
	Options
	// ReplaceInstructions makes CustomInstructions the whole prompt rather than
	// an addition to it.
	ReplaceInstructions bool
}

// BranchResult is a finished branch summary.
type BranchResult struct {
	Summary       string
	Usage         *ai.Usage
	ReadFiles     []string
	ModifiedFiles []string
}

// GenerateBranchSummary summarizes the entries a navigation is leaving behind.
//
// The budget is the whole context window less the reserve, not the compaction
// tail size: this is a one-shot request that has no conversation to make room
// for, so there is no reason to keep it small.
func GenerateBranchSummary(ctx context.Context, entries []session.Entry, opts BranchOptions) (*BranchResult, error) {
	s := opts.settings()

	contextWindow := 128000
	if opts.Model != nil && opts.Model.ContextWindow > 0 {
		contextWindow = opts.Model.ContextWindow
	}
	budget := contextWindow - s.ReserveTokens
	if budget < 0 {
		budget = 0
	}

	prep := PrepareBranchEntries(entries, budget)
	if len(prep.Messages) == 0 {
		return &BranchResult{Summary: "No content to summarize"}, nil
	}

	instructions := branchSummaryPrompt
	switch {
	case opts.ReplaceInstructions && opts.CustomInstructions != "":
		instructions = opts.CustomInstructions
	case opts.CustomInstructions != "":
		instructions += "\n\nAdditional focus: " + opts.CustomInstructions
	}

	prompt := "<conversation>\n" + SerializeConversation(session.ConvertToLLM(prep.Messages)) +
		"\n</conversation>\n\n" + instructions

	// The branch summary itself is short by construction — it is a checkpoint,
	// not a transcript — so it gets a fixed small budget rather than a share of
	// the reserve.
	text, usage, err := complete(ctx, prompt, boundedTokens(2048, opts.Model), opts.Options, "branch summarization")
	if err != nil {
		return nil, err
	}

	summary := branchSummaryPreamble + text
	if summary == branchSummaryPreamble {
		summary = branchSummaryPreamble + "No summary generated"
	}
	lists := prep.FileOps.Lists()
	summary += FormatFileOperations(lists)

	return &BranchResult{
		Summary:       summary,
		Usage:         &usage,
		ReadFiles:     lists.ReadFiles,
		ModifiedFiles: lists.ModifiedFiles,
	}, nil
}
