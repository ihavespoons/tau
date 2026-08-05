// Package compaction shortens a session that no longer fits the model's
// context window.
//
// Port of Pi's core/compaction (compaction.ts, branch-summarization.ts,
// utils.ts). The work splits in two: deciding *where* to cut a conversation,
// which is arithmetic over the entries and needs no provider, and *summarizing*
// what was cut, which needs one. Everything here keeps that split, so the cut
// point is testable without a model and the summary is testable with a scripted
// one.
package compaction

import (
	"encoding/json"
	"math"

	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/session"
)

// Settings controls when compaction triggers and how much survives it.
type Settings struct {
	// Enabled turns automatic compaction on.
	Enabled bool
	// ReserveTokens is headroom kept below the context window, and the budget
	// the summarization request itself is sized against.
	ReserveTokens int
	// KeepRecentTokens is roughly how much recent conversation survives.
	KeepRecentTokens int
}

// DefaultSettings matches Pi's DEFAULT_COMPACTION_SETTINGS.
var DefaultSettings = Settings{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000}

// ContextTokens is how much of the window a response consumed.
//
// The provider's own total is preferred when it reports one; the sum of the
// components is the fallback, because a window is filled by everything sent,
// cache hits included.
func ContextTokens(u ai.Usage) int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// ShouldCompact reports whether the context has grown past its reserve.
func ShouldCompact(contextTokens, contextWindow int, s Settings) bool {
	if !s.Enabled {
		return false
	}
	return contextTokens > contextWindow-s.ReserveTokens
}

// assistantUsage returns a message's usage if it is trustworthy.
//
// An aborted or errored turn has usage that does not describe a full context,
// and an all-zero one describes nothing at all. Believing either would make the
// estimate jump backwards and cancel a compaction that was due.
func assistantUsage(m ai.Message) (ai.Usage, bool) {
	var am *ai.AssistantMessage
	switch v := m.(type) {
	case ai.AssistantMessage:
		am = &v
	case *ai.AssistantMessage:
		am = v
	default:
		return ai.Usage{}, false
	}
	if am.StopReason == ai.StopAborted || am.StopReason == ai.StopError {
		return ai.Usage{}, false
	}
	if ContextTokens(am.Usage) <= 0 {
		return ai.Usage{}, false
	}
	return am.Usage, true
}

// LastAssistantUsage finds the most recent trustworthy usage on a branch.
func LastAssistantUsage(entries []session.Entry) (ai.Usage, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		me, ok := entries[i].(*session.MessageEntry)
		if !ok {
			continue
		}
		if u, ok := assistantUsage(me.Message); ok {
			return u, true
		}
	}
	return ai.Usage{}, false
}

// ContextEstimate is how full the window is believed to be.
type ContextEstimate struct {
	// Tokens is the estimate: measured usage plus everything after it.
	Tokens int
	// UsageTokens is the part the provider actually measured.
	UsageTokens int
	// TrailingTokens is the estimated part.
	TrailingTokens int
	// LastUsageIndex is the message the measurement came from, or -1.
	LastUsageIndex int
}

// EstimateContextTokens sizes a conversation.
//
// The provider's last reported usage is the ground truth for everything up to
// the message that carried it; only what came after is guessed. That keeps the
// error bounded to the tail rather than compounding over the whole history.
func EstimateContextTokens(messages []ai.Message) ContextEstimate {
	last := -1
	var usage ai.Usage
	for i := len(messages) - 1; i >= 0; i-- {
		if u, ok := assistantUsage(messages[i]); ok {
			usage, last = u, i
			break
		}
	}

	if last < 0 {
		total := 0
		for _, m := range messages {
			total += EstimateTokens(m)
		}
		return ContextEstimate{Tokens: total, TrailingTokens: total, LastUsageIndex: -1}
	}

	trailing := 0
	for i := last + 1; i < len(messages); i++ {
		trailing += EstimateTokens(messages[i])
	}
	measured := ContextTokens(usage)
	return ContextEstimate{
		Tokens:         measured + trailing,
		UsageTokens:    measured,
		TrailingTokens: trailing,
		LastUsageIndex: last,
	}
}

// estimatedImageChars is what one image is treated as weighing. Images have no
// character count, and pretending they weigh nothing would let a conversation
// of screenshots overflow the window while the estimate read near zero.
const estimatedImageChars = 4800

// EstimateTokens approximates a message at four characters per token.
//
// Deliberately an overestimate: compacting slightly early costs one extra
// summary, and compacting slightly late costs the turn.
func EstimateTokens(m ai.Message) int {
	switch msg := m.(type) {
	case ai.UserMessage:
		return tokensFromChars(userContentChars(msg.Content))
	case *ai.UserMessage:
		return tokensFromChars(userContentChars(msg.Content))
	case ai.AssistantMessage:
		return tokensFromChars(assistantChars(msg.Content))
	case *ai.AssistantMessage:
		return tokensFromChars(assistantChars(msg.Content))
	case ai.ToolResultMessage:
		return tokensFromChars(blockChars(msg.Content))
	case *ai.ToolResultMessage:
		return tokensFromChars(blockChars(msg.Content))
	case *session.CustomMessage:
		return tokensFromChars(userContentChars(msg.Content))
	case *session.BashExecutionMessage:
		return tokensFromChars(len(msg.Command) + len(msg.Output))
	case *session.BranchSummaryMessage:
		return tokensFromChars(len(msg.Summary))
	case *session.CompactionSummaryMessage:
		return tokensFromChars(len(msg.Summary))
	default:
		return 0
	}
}

func tokensFromChars(chars int) int { return int(math.Ceil(float64(chars) / 4)) }

func userContentChars(c ai.UserContent) int {
	if c.Blocks == nil {
		return len(c.Text)
	}
	return blockChars(c.Blocks)
}

func blockChars(blocks ai.ContentList) int {
	chars := 0
	for _, b := range blocks {
		switch block := b.(type) {
		case ai.TextContent:
			chars += len(block.Text)
		case *ai.TextContent:
			chars += len(block.Text)
		case ai.ImageContent, *ai.ImageContent:
			chars += estimatedImageChars
		}
	}
	return chars
}

func assistantChars(blocks ai.ContentList) int {
	chars := 0
	for _, b := range blocks {
		switch block := b.(type) {
		case ai.TextContent:
			chars += len(block.Text)
		case *ai.TextContent:
			chars += len(block.Text)
		case ai.ThinkingContent:
			chars += len(block.Thinking)
		case *ai.ThinkingContent:
			chars += len(block.Thinking)
		case ai.ToolCall:
			chars += len(block.Name) + jsonLen(block.Arguments)
		case *ai.ToolCall:
			chars += len(block.Name) + jsonLen(block.Arguments)
		}
	}
	return chars
}

func jsonLen(v any) int {
	encoded, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(encoded)
}

// combineUsage adds two summarization calls together. A split turn costs two
// requests, and reporting only one of them would undercount the session.
func combineUsage(a, b ai.Usage) ai.Usage {
	out := ai.Usage{
		Input:       a.Input + b.Input,
		Output:      a.Output + b.Output,
		CacheRead:   a.CacheRead + b.CacheRead,
		CacheWrite:  a.CacheWrite + b.CacheWrite,
		TotalTokens: a.TotalTokens + b.TotalTokens,
		Cost: ai.Cost{
			Input:      a.Cost.Input + b.Cost.Input,
			Output:     a.Cost.Output + b.Cost.Output,
			CacheRead:  a.Cost.CacheRead + b.Cost.CacheRead,
			CacheWrite: a.Cost.CacheWrite + b.Cost.CacheWrite,
			Total:      a.Cost.Total + b.Cost.Total,
		},
	}
	if a.CacheWrite1h != nil || b.CacheWrite1h != nil {
		out.CacheWrite1h = ptr(deref(a.CacheWrite1h) + deref(b.CacheWrite1h))
	}
	if a.Reasoning != nil || b.Reasoning != nil {
		out.Reasoning = ptr(deref(a.Reasoning) + deref(b.Reasoning))
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
