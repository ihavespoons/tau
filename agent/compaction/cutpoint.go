package compaction

import (
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/session"
)

// entryMessages projects one entry to the messages it contributes.
//
// A compaction entry projects to nothing here even though it does in a normal
// context build. Compaction is looking at what to *replace*; a previous
// checkpoint's summary is carried forward through the update prompt instead,
// and counting it as content would summarize a summary of a summary.
func entryMessages(e session.Entry) []ai.Message {
	if _, isCompaction := e.(*session.CompactionEntry); isCompaction {
		return nil
	}
	return session.EntryToContextMessages(e, 0, nil, session.BuildOptions{})
}

// entryMessage is the single message an entry contributes, if any.
func entryMessage(e session.Entry) (ai.Message, bool) {
	msgs := entryMessages(e)
	if len(msgs) == 0 {
		return nil, false
	}
	return msgs[0], true
}

// isCutPointMessage reports whether the conversation can be severed before
// this message.
//
// Everything but a tool result qualifies. A tool result is not a message the
// conversation could start from — it answers a tool call, and a provider handed
// a result with no matching call rejects the request outright.
func isCutPointMessage(m ai.Message) bool {
	switch m.(type) {
	case ai.ToolResultMessage, *ai.ToolResultMessage:
		return false
	default:
		return true
	}
}

// isTurnStartMessage reports whether a message begins a turn — that is, whether
// it came from the user's side rather than the model's.
func isTurnStartMessage(m ai.Message) bool {
	switch m.(type) {
	case ai.UserMessage, *ai.UserMessage:
		return true
	case *session.BashExecutionMessage, *session.CustomMessage:
		return true
	case *session.BranchSummaryMessage, *session.CompactionSummaryMessage:
		return true
	default:
		return false
	}
}

func isTurnStartEntry(e session.Entry) bool {
	for _, m := range entryMessages(e) {
		if isTurnStartMessage(m) {
			return true
		}
	}
	return false
}

func findValidCutPoints(entries []session.Entry, start, end int) []int {
	var out []int
	for i := start; i < end; i++ {
		for _, m := range entryMessages(entries[i]) {
			if isCutPointMessage(m) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// FindTurnStartIndex walks back to the user message that opened the turn
// containing entryIndex, or -1 if there is none within start.
func FindTurnStartIndex(entries []session.Entry, entryIndex, start int) int {
	for i := entryIndex; i >= start; i-- {
		if isTurnStartEntry(entries[i]) {
			return i
		}
	}
	return -1
}

// CutPoint is where a branch gets severed.
type CutPoint struct {
	// FirstKeptEntryIndex is the first entry that survives.
	FirstKeptEntryIndex int
	// TurnStartIndex is the user message that opened the turn being split, or
	// -1 when the cut lands on a turn boundary.
	TurnStartIndex int
	// IsSplitTurn reports that the cut lands inside a turn.
	IsSplitTurn bool
}

// FindCutPoint picks the entry to keep from, aiming to retain about
// keepRecentTokens of recent conversation.
//
// The walk goes backwards from the newest entry accumulating sizes, then snaps
// forward to the nearest legal cut point. Snapping forward rather than back
// means the kept tail is never larger than asked for — overshooting would keep
// the context above the window the compaction was called to get under.
//
// Only entries in [start, end) are considered; start is the previous
// checkpoint's boundary, since nothing before it is in context to begin with.
func FindCutPoint(entries []session.Entry, start, end, keepRecentTokens int) CutPoint {
	cutPoints := findValidCutPoints(entries, start, end)
	if len(cutPoints) == 0 {
		return CutPoint{FirstKeptEntryIndex: start, TurnStartIndex: -1}
	}

	accumulated := 0
	cutIndex := cutPoints[0]

	for i := end - 1; i >= start; i-- {
		tokens := 0
		for _, m := range entryMessages(entries[i]) {
			tokens += EstimateTokens(m)
		}
		if tokens == 0 {
			continue
		}
		accumulated += tokens
		if accumulated < keepRecentTokens {
			continue
		}
		for _, c := range cutPoints {
			if c >= i {
				cutIndex = c
				break
			}
		}
		break
	}

	// Pull in the metadata entries immediately before the cut — a model change,
	// a thinking-level change. They contribute no messages, so keeping them
	// costs nothing, and leaving them on the far side would drop the settings
	// the retained conversation was produced under.
	for cutIndex > start {
		prev := entries[cutIndex-1]
		if _, isCompaction := prev.(*session.CompactionEntry); isCompaction {
			break
		}
		if len(entryMessages(prev)) > 0 {
			break
		}
		cutIndex--
	}

	startsTurn := isTurnStartEntry(entries[cutIndex])
	turnStart := -1
	if !startsTurn {
		turnStart = FindTurnStartIndex(entries, cutIndex, start)
	}
	return CutPoint{
		FirstKeptEntryIndex: cutIndex,
		TurnStartIndex:      turnStart,
		IsSplitTurn:         !startsTurn && turnStart != -1,
	}
}
