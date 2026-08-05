package coding

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ihavespoons/tau/agent/compaction"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/session"
)

// Compaction, tree navigation and forking: the operations that change which
// part of the session the conversation is looking at.
//
// All of them end the same way — the session file gains an entry, and the
// agent's in-memory message list is rebuilt from the file. Rebuilding rather
// than patching is what keeps the two from drifting: the file is the truth, and
// after any of these the in-memory list is no longer a suffix of it.

// ErrNoSession is returned by operations that need a persisted session.
var ErrNoSession = errors.New("this session is not persisted, so it has no history to work with")

// compactionSettings reads the merged configuration.
func (s *Session) compactionSettings() compaction.Settings {
	out := compaction.DefaultSettings
	if s.Settings == nil {
		return out
	}
	out.Enabled = s.Settings.CompactionEnabled()
	if v := s.Settings.CompactionReserveTokens(); v > 0 {
		out.ReserveTokens = v
	}
	if v := s.Settings.CompactionKeepRecentTokens(); v > 0 {
		out.KeepRecentTokens = v
	}
	return out
}

// compactionOptions builds the summarization request configuration.
//
// The session's own stream function is used, so the summary goes through the
// same provider resolution, retries and headers as a normal turn — a summary
// that fails for a reason the conversation would have survived is a bug the
// user cannot diagnose.
func (s *Session) compactionOptions(instructions string) compaction.Options {
	return compaction.Options{
		Model:              s.Model,
		Stream:             s.stream,
		Thinking:           ai.ThinkingLevel(s.Agent.ThinkingLevel()),
		Settings:           s.compactionSettings(),
		CustomInstructions: strings.TrimSpace(instructions),
	}
}

// PrepareCompaction reports what a compaction would replace, without spending
// a request. Returns nil when there is nothing to compact.
func (s *Session) PrepareCompaction(ctx context.Context) (*compaction.Preparation, error) {
	if s.Session == nil {
		return nil, ErrNoSession
	}
	entries, err := s.Session.Branch(ctx, nil)
	if err != nil {
		return nil, err
	}
	return compaction.Prepare(entries, s.compactionSettings()), nil
}

// Compact summarizes the older part of the conversation and checkpoints it.
//
// instructions is an optional focus for the summary. Returns nil when there was
// nothing to compact, which is not an error: /compact on a short session should
// say so rather than fail.
func (s *Session) Compact(ctx context.Context, instructions string) (*compaction.Result, error) {
	prep, err := s.PrepareCompaction(ctx)
	if err != nil || prep == nil {
		return nil, err
	}

	// Asked before the request is spent, because an extension that vetoes a
	// compaction should not be billed for the summary first.
	if s.Extensions != nil {
		if res := s.Extensions.EmitSessionBeforeCompact(ctx, &extension.SessionBeforeCompactEvent{
			CustomInstructions: instructions,
		}); res != nil && res.Cancel {
			return nil, cancelledBy("compaction", res.Reason)
		}
	}

	result, err := compaction.Compact(ctx, prep, s.compactionOptions(instructions))
	if err != nil {
		return nil, err
	}

	// The retained tail is captured now, from the branch as it stands, so the
	// checkpoint is self-contained. Reconstructing it later from
	// firstKeptEntryId would depend on the tree still having the same shape,
	// and a fork of this session is free to change it.
	tail, err := s.retainedTail(ctx, result.FirstKeptEntryID)
	if err != nil {
		return nil, err
	}

	if _, err := s.Session.AppendCompaction(ctx, result.Summary, result.TokensBefore, session.CompactionOptions{
		FirstKeptEntryID: result.FirstKeptEntryID,
		Details:          result.Details,
		Usage:            result.Usage,
		RetainedTail:     tail,
	}); err != nil {
		return nil, err
	}

	if err := s.reloadContext(ctx); err != nil {
		return nil, err
	}
	if s.Extensions != nil {
		s.Extensions.EmitSessionCompact(ctx, &extension.SessionCompactEvent{
			Summary:      result.Summary,
			TokensBefore: result.TokensBefore,
		})
	}
	return result, nil
}

// cancelledBy reports an extension's veto of a session operation.
func cancelledBy(what, reason string) error {
	if reason == "" {
		return fmt.Errorf("%s was cancelled by an extension", what)
	}
	return fmt.Errorf("%s was cancelled by an extension: %s", what, reason)
}

// retainedTail is the messages from the first kept entry onwards.
func (s *Session) retainedTail(ctx context.Context, firstKeptEntryID string) ([]ai.Message, error) {
	entries, err := s.Session.Branch(ctx, nil)
	if err != nil {
		return nil, err
	}
	start := -1
	for i, e := range entries {
		if e.Base().ID == firstKeptEntryID {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("the entry to keep from (%s) is not on this branch", firstKeptEntryID)
	}

	var tail []ai.Message
	kept := entries[start:]
	for i, e := range kept {
		tail = append(tail, session.EntryToContextMessages(e, i, kept, session.BuildOptions{})...)
	}
	return tail, nil
}

// MaybeCompact compacts if the context has outgrown its reserve.
//
// Reports whether it compacted. A failure is returned rather than swallowed,
// but the caller is expected to keep going: a turn that cannot be compacted is
// still worth attempting, and the provider's own error is a clearer diagnosis
// than tau refusing pre-emptively.
func (s *Session) MaybeCompact(ctx context.Context) (bool, error) {
	set := s.compactionSettings()
	if !set.Enabled || s.Session == nil || s.Model == nil || s.Model.ContextWindow <= 0 {
		return false, nil
	}

	entries, err := s.Session.Branch(ctx, nil)
	if err != nil {
		return false, err
	}
	sctx := session.BuildSessionContext(entries, session.BuildOptions{})
	estimate := compaction.EstimateContextTokens(sctx.Messages)
	if !compaction.ShouldCompact(estimate.Tokens, s.Model.ContextWindow, set) {
		return false, nil
	}

	result, err := s.Compact(ctx, "")
	if err != nil {
		return false, err
	}
	return result != nil, nil
}

// reloadContext rebuilds the agent's message list from the session file.
func (s *Session) reloadContext(ctx context.Context) error {
	if s.Session == nil {
		return nil
	}
	sctx, err := s.Session.BuildContext(ctx)
	if err != nil {
		return err
	}
	s.Agent.SetMessages(session.ConvertToLLM(sctx.Messages))
	return nil
}

// MoveTo repositions the conversation at another entry in the tree.
//
// summarize asks for a summary of the branch being left. It costs a request, so
// it is the caller's decision; without one the abandoned work simply drops out
// of context, which is sometimes exactly what the user wants.
func (s *Session) MoveTo(ctx context.Context, entryID string, summarize bool) (*compaction.BranchResult, error) {
	if s.Session == nil {
		return nil, ErrNoSession
	}
	if _, ok := s.Session.Entry(ctx, entryID); !ok {
		return nil, fmt.Errorf("no entry %s in this session", entryID)
	}
	if s.Extensions != nil {
		if res := s.Extensions.EmitSessionBeforeTree(ctx, &extension.SessionBeforeTreeEvent{
			TargetID: entryID,
		}); res != nil && res.Cancel {
			return nil, cancelledBy("navigation", res.Reason)
		}
	}

	var summary *session.BranchSummary
	var result *compaction.BranchResult

	if summarize {
		leaf, err := s.Session.LeafID(ctx)
		if err != nil {
			return nil, err
		}
		if leaf != nil && *leaf != entryID {
			collected, err := compaction.CollectBranchEntries(ctx, s.Session, *leaf, entryID)
			if err != nil {
				return nil, err
			}
			if len(collected.Entries) > 0 {
				opts := compaction.BranchOptions{Options: s.compactionOptions("")}
				if s.Settings != nil {
					if v := s.Settings.BranchSummaryReserveTokens(); v > 0 {
						opts.Settings.ReserveTokens = v
					}
				}
				result, err = compaction.GenerateBranchSummary(ctx, collected.Entries, opts)
				if err != nil {
					return nil, err
				}
				summary = &session.BranchSummary{
					Summary: result.Summary,
					Details: compaction.FileLists{
						ReadFiles:     result.ReadFiles,
						ModifiedFiles: result.ModifiedFiles,
					},
					Usage: result.Usage,
				}
			}
		}
	}

	if _, err := s.Session.MoveTo(ctx, &entryID, summary); err != nil {
		return nil, err
	}
	if err := s.reloadContext(ctx); err != nil {
		return nil, err
	}
	if s.Extensions != nil {
		s.Extensions.EmitSessionTree(ctx, &extension.SessionTreeEvent{TargetID: entryID})
	}
	return result, nil
}

// Fork copies this session up to entryID into a new session file and switches
// to it, leaving the original untouched.
//
// entryID names a user message, and the copy stops just before it: the point of
// forking at a request is to make it differently. An empty entryID copies the
// whole session, which is /clone.
func (s *Session) Fork(ctx context.Context, entryID string) error {
	if s.repo == nil || s.Session == nil {
		return ErrNoSession
	}
	if s.Agent.IsRunning() {
		return ErrRunning
	}
	if entryID != "" {
		if _, ok := s.Session.Entry(ctx, entryID); !ok {
			return fmt.Errorf("no entry %s in this session", entryID)
		}
	}
	if s.Extensions != nil {
		if res := s.Extensions.EmitSessionBeforeFork(ctx, &extension.SessionBeforeForkEvent{
			EntryID: entryID,
		}); res != nil && res.Cancel {
			return cancelledBy("fork", res.Reason)
		}
	}

	meta, err := s.Session.Metadata(ctx)
	if err != nil {
		return err
	}

	// replaceSession runs the fork inside its own veto/shutdown/announce
	// sequence, so the new file is only created once the switch is agreed to.
	return s.replaceSession(ctx, func() (*session.Session, session.Metadata, error) {
		forked, err := s.repo.Fork(ctx, meta, session.CreateSessionOptions{
			Cwd:               s.Cwd,
			ParentSessionPath: meta.Path,
		}, session.ForkOptions{EntryID: entryID})
		if err != nil {
			return nil, session.Metadata{}, err
		}
		newMeta, err := forked.Metadata(ctx)
		if err != nil {
			return nil, session.Metadata{}, err
		}
		return forked, newMeta, nil
	}, "")
}

// TreeNodes returns the session tree for a navigation UI.
func (s *Session) TreeNodes(ctx context.Context) ([]*session.TreeNode, error) {
	if s.Session == nil {
		return nil, ErrNoSession
	}
	return s.Session.Tree(ctx), nil
}

// UserPrompts lists the user messages on the current branch, oldest first.
//
// This is what /fork offers to branch from: a fork point is only meaningful at
// a request the user made, because that is the decision being re-taken.
func (s *Session) UserPrompts(ctx context.Context) ([]PromptPoint, error) {
	if s.Session == nil {
		return nil, ErrNoSession
	}
	entries, err := s.Session.Branch(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []PromptPoint
	for _, e := range entries {
		me, ok := e.(*session.MessageEntry)
		if !ok {
			continue
		}
		um, ok := me.Message.(ai.UserMessage)
		if !ok {
			continue
		}
		out = append(out, PromptPoint{
			EntryID:   me.ID,
			Timestamp: me.Timestamp,
			Text:      firstLine(um.Content.String()),
		})
	}
	return out, nil
}

// PromptPoint is a place the conversation can be forked or rewound to.
type PromptPoint struct {
	EntryID   string
	Timestamp string
	Text      string
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
