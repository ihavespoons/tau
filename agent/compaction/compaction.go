package compaction

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/session"
)

// Options is everything a summarization call needs.
type Options struct {
	// Model summarizes. It need not be the session's model.
	Model *ai.Model
	// Stream issues the request. Injected so this package never builds a
	// provider, which is what lets every path here be tested offline.
	Stream ai.SimpleStreamFunc
	// StreamOptions carries credentials, headers and provider env. Cache and
	// session-routing fields are overwritten per request.
	StreamOptions ai.SimpleStreamOptions
	// Thinking is the reasoning level, applied only on a reasoning model.
	Thinking ai.ThinkingLevel
	// Settings bounds the request and the retained tail.
	Settings Settings
	// CustomInstructions is appended to the prompt as an additional focus.
	CustomInstructions string
}

func (o Options) settings() Settings {
	s := o.Settings
	if s.ReserveTokens <= 0 {
		s.ReserveTokens = DefaultSettings.ReserveTokens
	}
	if s.KeepRecentTokens <= 0 {
		s.KeepRecentTokens = DefaultSettings.KeepRecentTokens
	}
	return s
}

// Details is what a compaction entry carries beyond its summary.
type Details = FileLists

// Preparation is the decision of what to compact, made without a provider.
//
// It is a separate step from the summarization so an extension can inspect and
// override the plan, and so tau can tell the user what a /compact would do
// before spending a request on it.
type Preparation struct {
	// FirstKeptEntryID is the first entry that survives.
	FirstKeptEntryID string
	// MessagesToSummarize is the history being replaced.
	MessagesToSummarize []ai.Message
	// TurnPrefixMessages is the leading part of a turn that got split.
	TurnPrefixMessages []ai.Message
	// IsSplitTurn reports that the cut landed inside a turn.
	IsSplitTurn bool
	// TokensBefore is the context size the compaction was called at.
	TokensBefore int
	// PreviousSummary is the prior checkpoint's summary, updated rather than
	// replaced so a long session keeps one cumulative record.
	PreviousSummary string
	// FileOps is the file tracking gathered from what is being replaced.
	FileOps *FileOps
	// Settings are the ones the plan was made under.
	Settings Settings
}

// Result is a finished compaction, ready to append to the session.
type Result struct {
	Summary          string
	FirstKeptEntryID string
	TokensBefore     int
	Usage            *ai.Usage
	Details          Details
}

// Prepare decides what a compaction of this branch would replace.
//
// Returns nil when there is nothing to do: a branch that already ends in a
// checkpoint, or one where everything falls inside the retained tail.
func Prepare(pathEntries []session.Entry, s Settings) *Preparation {
	if len(pathEntries) == 0 {
		return nil
	}
	if _, ends := pathEntries[len(pathEntries)-1].(*session.CompactionEntry); ends {
		return nil
	}

	prevIndex := -1
	for i := len(pathEntries) - 1; i >= 0; i-- {
		if _, ok := pathEntries[i].(*session.CompactionEntry); ok {
			prevIndex = i
			break
		}
	}

	previousSummary := ""
	boundaryStart := 0
	if prevIndex >= 0 {
		prev := pathEntries[prevIndex].(*session.CompactionEntry)
		previousSummary = prev.Summary
		// Start from the checkpoint's own first kept entry, not from the
		// checkpoint: everything between them is still in context, so a second
		// compaction must be free to replace it too.
		boundaryStart = prevIndex + 1
		for i, e := range pathEntries {
			if e.Base().ID == prev.FirstKeptEntryID {
				boundaryStart = i
				break
			}
		}
	}

	tokensBefore := EstimateContextTokens(session.BuildSessionContext(pathEntries, session.BuildOptions{}).Messages).Tokens
	cut := FindCutPoint(pathEntries, boundaryStart, len(pathEntries), s.KeepRecentTokens)

	firstKept := pathEntries[cut.FirstKeptEntryIndex]
	if firstKept.Base().ID == "" {
		return nil
	}

	historyEnd := cut.FirstKeptEntryIndex
	if cut.IsSplitTurn {
		historyEnd = cut.TurnStartIndex
	}

	var toSummarize []ai.Message
	for i := boundaryStart; i < historyEnd; i++ {
		if m, ok := entryMessage(pathEntries[i]); ok {
			toSummarize = append(toSummarize, m)
		}
	}

	var turnPrefix []ai.Message
	if cut.IsSplitTurn {
		for i := cut.TurnStartIndex; i < cut.FirstKeptEntryIndex; i++ {
			if m, ok := entryMessage(pathEntries[i]); ok {
				turnPrefix = append(turnPrefix, m)
			}
		}
	}

	if len(toSummarize) == 0 && len(turnPrefix) == 0 {
		return nil
	}

	fileOps := NewFileOps()
	if prevIndex >= 0 {
		prev := pathEntries[prevIndex].(*session.CompactionEntry)
		// An extension's checkpoint carries whatever details that extension
		// chose; only tau's own shape can be folded back in.
		if !prev.FromHook {
			if lists, ok := decodeFileLists(prev.Details); ok {
				fileOps.AddLists(lists)
			}
		}
	}
	for _, m := range toSummarize {
		fileOps.AddFromMessage(m)
	}
	for _, m := range turnPrefix {
		fileOps.AddFromMessage(m)
	}

	return &Preparation{
		FirstKeptEntryID:    firstKept.Base().ID,
		MessagesToSummarize: toSummarize,
		TurnPrefixMessages:  turnPrefix,
		IsSplitTurn:         cut.IsSplitTurn,
		TokensBefore:        tokensBefore,
		PreviousSummary:     previousSummary,
		FileOps:             fileOps,
		Settings:            s,
	}
}

// decodeFileLists reads details written by a previous compaction. They come
// back as decoded JSON rather than the struct that wrote them, so the round
// trip is explicit; anything unrecognized is ignored rather than fatal.
func decodeFileLists(details any) (FileLists, bool) {
	if details == nil {
		return FileLists{}, false
	}
	if l, ok := details.(FileLists); ok {
		return l, true
	}
	if l, ok := details.(*FileLists); ok && l != nil {
		return *l, true
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return FileLists{}, false
	}
	var out FileLists
	if err := json.Unmarshal(encoded, &out); err != nil {
		return FileLists{}, false
	}
	if out.ReadFiles == nil && out.ModifiedFiles == nil {
		return FileLists{}, false
	}
	return out, true
}

// Compact turns a preparation into a summary.
//
// A split turn costs two requests: one for the history before the turn, one for
// the part of the turn that did not survive. They are separate because they
// answer different questions — what happened before, versus what the retained
// work is a continuation of — and a single prompt for both produces a summary
// that serves neither.
func Compact(ctx context.Context, prep *Preparation, opts Options) (*Result, error) {
	if prep == nil {
		return nil, fmt.Errorf("nothing to compact")
	}
	s := prep.Settings
	if s.ReserveTokens <= 0 {
		s = opts.settings()
	}

	var summary string
	var usage ai.Usage

	if prep.IsSplitTurn && len(prep.TurnPrefixMessages) > 0 {
		historyText := "No prior history."
		haveHistory := false
		if len(prep.MessagesToSummarize) > 0 {
			text, u, err := generateSummary(ctx, prep.MessagesToSummarize, prep.PreviousSummary, s, opts)
			if err != nil {
				return nil, err
			}
			historyText, usage, haveHistory = text, u, true
		}
		prefixText, prefixUsage, err := generateTurnPrefixSummary(ctx, prep.TurnPrefixMessages, s, opts)
		if err != nil {
			return nil, err
		}
		summary = historyText + "\n\n---\n\n**Turn Context (split turn):**\n\n" + prefixText
		if haveHistory {
			usage = combineUsage(usage, prefixUsage)
		} else {
			usage = prefixUsage
		}
	} else {
		text, u, err := generateSummary(ctx, prep.MessagesToSummarize, prep.PreviousSummary, s, opts)
		if err != nil {
			return nil, err
		}
		summary, usage = text, u
	}

	lists := prep.FileOps.Lists()
	summary += FormatFileOperations(lists)

	return &Result{
		Summary:          summary,
		FirstKeptEntryID: prep.FirstKeptEntryID,
		TokensBefore:     prep.TokensBefore,
		Usage:            &usage,
		Details:          lists,
	}, nil
}

// generateSummary produces or updates the history summary.
func generateSummary(ctx context.Context, messages []ai.Message, previousSummary string, s Settings, opts Options) (string, ai.Usage, error) {
	instructions := summarizationPrompt
	if previousSummary != "" {
		instructions = updateSummarizationPrompt
	}
	if opts.CustomInstructions != "" {
		instructions += "\n\nAdditional focus: " + opts.CustomInstructions
	}

	prompt := "<conversation>\n" + SerializeConversation(session.ConvertToLLM(messages)) + "\n</conversation>\n\n"
	if previousSummary != "" {
		prompt += "<previous-summary>\n" + previousSummary + "\n</previous-summary>\n\n"
	}
	prompt += instructions

	// Four fifths of the reserve: the summary has to fit inside the headroom
	// the compaction was called to create, with room left for the prompt.
	maxTokens := boundedTokens(s.ReserveTokens*8/10, opts.Model)
	return complete(ctx, prompt, maxTokens, opts, "summarization")
}

// generateTurnPrefixSummary summarizes the discarded head of a split turn on a
// smaller budget — it is context for the retained work, not a record in itself.
func generateTurnPrefixSummary(ctx context.Context, messages []ai.Message, s Settings, opts Options) (string, ai.Usage, error) {
	prompt := "<conversation>\n" + SerializeConversation(session.ConvertToLLM(messages)) +
		"\n</conversation>\n\n" + turnPrefixSummarizationPrompt
	maxTokens := boundedTokens(s.ReserveTokens/2, opts.Model)
	return complete(ctx, prompt, maxTokens, opts, "turn prefix summarization")
}

func boundedTokens(want int, model *ai.Model) int {
	if model != nil && model.MaxTokens > 0 && want > model.MaxTokens {
		return model.MaxTokens
	}
	if want < 1 {
		return 1
	}
	return want
}

// complete runs one summarization request and returns its text.
//
// Cache retention is off and the session id is fresh: a summary is a one-shot
// request whose prompt will never be seen again, so a cache write would be paid
// for and never read, and reusing the session's routing id would let the
// summarization displace the conversation's own cached prefix.
func complete(ctx context.Context, prompt string, maxTokens int, opts Options, what string) (string, ai.Usage, error) {
	if opts.Stream == nil {
		return "", ai.Usage{}, fmt.Errorf("%s needs a stream function", what)
	}
	if opts.Model == nil {
		return "", ai.Usage{}, fmt.Errorf("%s needs a model", what)
	}

	streamOpts := opts.StreamOptions
	streamOpts.MaxTokens = maxTokens
	streamOpts.CacheRetention = ai.CacheNone
	streamOpts.SessionID = newRequestID()
	streamOpts.Reasoning = ""
	if opts.Model.Reasoning && opts.Thinking != "" && opts.Thinking != ai.ThinkingLevel(ai.ThinkingOff) {
		streamOpts.Reasoning = opts.Thinking
	}

	reqContext := ai.Context{
		SystemPrompt: SummarizationSystemPrompt,
		Messages: ai.MessageList{ai.UserMessage{
			Content: ai.UserContent{Blocks: ai.ContentList{ai.TextContent{Text: prompt}}},
		}},
	}

	result := opts.Stream(ctx, opts.Model, reqContext, &streamOpts).Result()
	if result == nil {
		return "", ai.Usage{}, fmt.Errorf("%s produced no result", what)
	}
	switch result.StopReason {
	case ai.StopError:
		return "", ai.Usage{}, fmt.Errorf("%s failed: %s", what, errorText(result))
	case ai.StopAborted:
		return "", ai.Usage{}, context.Canceled
	}
	return contentText(result.Content), result.Usage, nil
}

func errorText(m *ai.AssistantMessage) string {
	if m.ErrorMessage != "" {
		return m.ErrorMessage
	}
	return "unknown error"
}

func newRequestID() string {
	if u, err := uuid.NewV7(); err == nil {
		return u.String()
	}
	return uuid.NewString()
}
