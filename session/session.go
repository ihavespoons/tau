package session

import (
	"context"
	"strings"
	"sync"

	"github.com/ihavespoons/tau/ai"
)

// ModelRef identifies the model a session is using.
type ModelRef struct {
	Provider string
	ModelID  string
}

// Context is the projection of a session branch for a model request.
type Context struct {
	Messages        []ai.Message
	ThinkingLevel   string
	Model           *ModelRef
	ActiveToolNames []string
}

// EntryTransform rewrites the entry list before it becomes messages.
type EntryTransform func(entries []Entry) []Entry

// CustomEntryProjector lets an extension give its custom entries a presence in
// model context. Custom entries project to nothing by default.
type CustomEntryProjector func(entry *CustomEntry, index int, entries []Entry) []ai.Message

// BuildOptions tunes context projection.
type BuildOptions struct {
	// EntryTransforms run after the default compaction transform.
	EntryTransforms []EntryTransform
	// EntryProjectors map a custom entry's customType to its context messages.
	EntryProjectors map[string]CustomEntryProjector
}

// Session is the tree API over a storage backend.
type Session struct {
	storage Storage
	opts    BuildOptions
	// appendMu makes an append atomic as a unit. Storage locks each call
	// separately, so without this two goroutines could read the same leaf and
	// both parent onto it, silently forking the conversation.
	appendMu sync.Mutex
}

// NewSession wraps a storage backend.
func NewSession(storage Storage, opts ...BuildOptions) *Session {
	s := &Session{storage: storage}
	if len(opts) > 0 {
		s.opts = opts[0]
	}
	return s
}

// Storage exposes the underlying entry log.
func (s *Session) Storage() Storage { return s.storage }

// Metadata describes the session.
func (s *Session) Metadata(ctx context.Context) (Metadata, error) { return s.storage.Metadata(ctx) }

// LeafID is the current position in the tree.
func (s *Session) LeafID(ctx context.Context) (*string, error) { return s.storage.LeafID(ctx) }

// Entry looks up one entry.
func (s *Session) Entry(ctx context.Context, id string) (Entry, bool) {
	return s.storage.GetEntry(ctx, id)
}

// Entries lists entries in append order.
func (s *Session) Entries(ctx context.Context, opts *CursorOptions) []Entry {
	return s.storage.Entries(ctx, opts)
}

// Label returns an entry's bookmark.
func (s *Session) Label(ctx context.Context, id string) (string, bool) {
	return s.storage.Label(ctx, id)
}

// Name returns the session's display name.
func (s *Session) Name(ctx context.Context) (string, bool) { return s.storage.SessionName(ctx) }

// Stats summarizes size and cost.
func (s *Session) Stats(ctx context.Context) Stats { return s.storage.Stats(ctx) }

// Branch returns the active path root-first, from fromID or the current leaf.
func (s *Session) Branch(ctx context.Context, fromID *string) ([]Entry, error) {
	leafID := fromID
	if leafID == nil {
		var err error
		leafID, err = s.storage.LeafID(ctx)
		if err != nil {
			return nil, err
		}
	}
	return s.storage.PathToRootOrCompaction(ctx, leafID)
}

// ContextEntries returns the branch entries that contribute to model context.
func (s *Session) ContextEntries(ctx context.Context, opts ...BuildOptions) ([]Entry, error) {
	branch, err := s.Branch(ctx, nil)
	if err != nil {
		return nil, err
	}
	return BuildContextEntries(branch, s.merge(opts...)), nil
}

// BuildContext projects the active branch into a model request context.
func (s *Session) BuildContext(ctx context.Context, opts ...BuildOptions) (Context, error) {
	branch, err := s.Branch(ctx, nil)
	if err != nil {
		return Context{}, err
	}
	return BuildSessionContext(branch, s.merge(opts...)), nil
}

func (s *Session) merge(opts ...BuildOptions) BuildOptions {
	out := BuildOptions{
		EntryTransforms: append([]EntryTransform(nil), s.opts.EntryTransforms...),
		EntryProjectors: map[string]CustomEntryProjector{},
	}
	for k, v := range s.opts.EntryProjectors {
		out.EntryProjectors[k] = v
	}
	for _, o := range opts {
		out.EntryTransforms = append(out.EntryTransforms, o.EntryTransforms...)
		for k, v := range o.EntryProjectors {
			out.EntryProjectors[k] = v
		}
	}
	return out
}

// deriveState reads model, thinking level, and active tools off the full path.
// These come from the whole branch, not just the post-compaction slice.
func deriveState(path []Entry) Context {
	state := Context{ThinkingLevel: "off"}
	for _, e := range path {
		switch entry := e.(type) {
		case *ThinkingLevelChangeEntry:
			state.ThinkingLevel = entry.ThinkingLevel
		case *ModelChangeEntry:
			state.Model = &ModelRef{Provider: entry.Provider, ModelID: entry.ModelID}
		case *ActiveToolsChangeEntry:
			state.ActiveToolNames = append([]string(nil), entry.ActiveToolNames...)
		case *MessageEntry:
			switch m := entry.Message.(type) {
			case ai.AssistantMessage:
				state.Model = &ModelRef{Provider: m.Provider, ModelID: m.Model}
			case *ai.AssistantMessage:
				state.Model = &ModelRef{Provider: m.Provider, ModelID: m.Model}
			}
		}
	}
	return state
}

// DefaultContextEntryTransform applies compaction to a branch.
//
// With no compaction the branch passes through. Otherwise the last compaction
// on the path leads, followed by everything after it. A compaction carrying a
// retained tail is self-contained; an older one instead replays entries from
// its first kept entry up to the compaction.
func DefaultContextEntryTransform(path []Entry) []Entry {
	compactionIdx := -1
	var compaction *CompactionEntry
	for i, e := range path {
		if c, ok := e.(*CompactionEntry); ok {
			compaction = c
			compactionIdx = i
		}
	}
	if compaction == nil {
		return append([]Entry(nil), path...)
	}

	entries := []Entry{compaction}
	if compaction.RetainedTail != nil {
		return append(entries, path[compactionIdx+1:]...)
	}
	if compaction.FirstKeptEntryID != "" {
		found := false
		for i := 0; i < compactionIdx; i++ {
			if path[i].Base().ID == compaction.FirstKeptEntryID {
				found = true
			}
			if found {
				entries = append(entries, path[i])
			}
		}
	}
	return append(entries, path[compactionIdx+1:]...)
}

// BuildContextEntries applies the default compaction transform then any
// caller-supplied transforms.
func BuildContextEntries(path []Entry, opts BuildOptions) []Entry {
	entries := DefaultContextEntryTransform(path)
	for _, transform := range opts.EntryTransforms {
		entries = transform(entries)
	}
	return entries
}

// EntryToContextMessages projects one entry into model context.
//
// A message entry yields its message; compaction and branch summaries become
// synthetic summary messages (a compaction also replays its retained tail);
// custom entries yield nothing unless a projector is registered for them.
func EntryToContextMessages(entry Entry, index int, entries []Entry, opts BuildOptions) []ai.Message {
	switch e := entry.(type) {
	case *MessageEntry:
		if e.Message == nil {
			return nil
		}
		return []ai.Message{e.Message}
	case *CustomMessageEntry:
		return []ai.Message{&CustomMessage{
			CustomType: e.CustomType,
			Content:    e.Content,
			Display:    e.Display,
			Details:    e.Details,
			Timestamp:  parseTimestamp(e.Timestamp),
		}}
	case *CompactionEntry:
		out := []ai.Message{&CompactionSummaryMessage{
			Summary:      e.Summary,
			TokensBefore: e.TokensBefore,
			Timestamp:    parseTimestamp(e.Timestamp),
		}}
		return append(out, e.RetainedTail...)
	case *BranchSummaryEntry:
		if e.Summary == "" {
			return nil
		}
		return []ai.Message{&BranchSummaryMessage{
			Summary:   e.Summary,
			FromID:    e.FromID,
			Timestamp: parseTimestamp(e.Timestamp),
		}}
	case *CustomEntry:
		if project, ok := opts.EntryProjectors[e.CustomType]; ok && project != nil {
			return project(e, index, entries)
		}
		return nil
	default:
		return nil
	}
}

// BuildSessionContext projects a branch into a model request context.
func BuildSessionContext(path []Entry, opts BuildOptions) Context {
	state := deriveState(path)
	entries := BuildContextEntries(path, opts)
	for i, entry := range entries {
		state.Messages = append(state.Messages, EntryToContextMessages(entry, i, entries, opts)...)
	}
	return state
}

// appendEntry stamps an entry with a fresh id, the current leaf as parent, and
// the current time, then writes it. The whole sequence is atomic so concurrent
// callers extend one chain instead of forking it.
func (s *Session) appendEntry(ctx context.Context, build func(base EntryBase) Entry) (string, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	id, err := s.storage.CreateEntryID(ctx)
	if err != nil {
		return "", err
	}
	parent, err := s.storage.LeafID(ctx)
	if err != nil {
		return "", err
	}
	entry := build(EntryBase{ID: id, ParentID: parent, Timestamp: Now()})
	if err := s.storage.AppendEntry(ctx, entry); err != nil {
		return "", err
	}
	return id, nil
}

// AppendMessage records a conversation message.
func (s *Session) AppendMessage(ctx context.Context, message ai.Message) (string, error) {
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &MessageEntry{EntryBase: base, Message: message}
	})
}

// AppendThinkingLevelChange records a thinking level switch.
func (s *Session) AppendThinkingLevelChange(ctx context.Context, level string) (string, error) {
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &ThinkingLevelChangeEntry{EntryBase: base, ThinkingLevel: level}
	})
}

// AppendModelChange records a model switch.
func (s *Session) AppendModelChange(ctx context.Context, provider, modelID string) (string, error) {
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &ModelChangeEntry{EntryBase: base, Provider: provider, ModelID: modelID}
	})
}

// AppendActiveToolsChange records a change to the enabled tool set.
func (s *Session) AppendActiveToolsChange(ctx context.Context, names []string) (string, error) {
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &ActiveToolsChangeEntry{EntryBase: base, ActiveToolNames: append([]string(nil), names...)}
	})
}

// CompactionOptions carries the optional parts of a compaction checkpoint.
type CompactionOptions struct {
	FirstKeptEntryID string
	Details          any
	Usage            *ai.Usage
	FromHook         bool
	RetainedTail     []ai.Message
}

// AppendCompaction records a compaction checkpoint.
func (s *Session) AppendCompaction(ctx context.Context, summary string, tokensBefore int, opts CompactionOptions) (string, error) {
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &CompactionEntry{
			EntryBase:        base,
			Summary:          summary,
			FirstKeptEntryID: opts.FirstKeptEntryID,
			TokensBefore:     tokensBefore,
			RetainedTail:     opts.RetainedTail,
			Details:          opts.Details,
			Usage:            opts.Usage,
			FromHook:         opts.FromHook,
		}
	})
}

// AppendCustomEntry persists extension state outside model context.
func (s *Session) AppendCustomEntry(ctx context.Context, customType string, data any) (string, error) {
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &CustomEntry{EntryBase: base, CustomType: customType, Data: data}
	})
}

// AppendCustomMessage records an extension message that enters model context.
func (s *Session) AppendCustomMessage(ctx context.Context, customType string, content ai.UserContent, display bool, details any) (string, error) {
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &CustomMessageEntry{
			EntryBase:  base,
			CustomType: customType,
			Content:    content,
			Display:    display,
			Details:    details,
		}
	})
}

// AppendLabel sets or clears a bookmark on an entry. A nil label clears it.
func (s *Session) AppendLabel(ctx context.Context, targetID string, label *string) (string, error) {
	if _, ok := s.storage.GetEntry(ctx, targetID); !ok {
		return "", errorf(CodeNotFound, nil, "entry %s not found", targetID)
	}
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &LabelEntry{EntryBase: base, TargetID: targetID, Label: label}
	})
}

// AppendName sets the session's display name. Newlines are collapsed so the
// name stays a single line in pickers.
func (s *Session) AppendName(ctx context.Context, name string) (string, error) {
	sanitized := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(name))
	return s.appendEntry(ctx, func(base EntryBase) Entry {
		return &SessionInfoEntry{EntryBase: base, Name: sanitized}
	})
}

// BranchSummary describes context carried over when leaving a branch.
type BranchSummary struct {
	Summary  string
	Details  any
	Usage    *ai.Usage
	FromHook bool
}

// MoveTo repositions the leaf, optionally recording a summary of the branch
// being left. Returns the branch-summary entry id when one was written.
func (s *Session) MoveTo(ctx context.Context, entryID *string, summary *BranchSummary) (string, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()

	if entryID != nil {
		if _, ok := s.storage.GetEntry(ctx, *entryID); !ok {
			return "", errorf(CodeNotFound, nil, "entry %s not found", *entryID)
		}
	}
	if err := s.storage.SetLeafID(ctx, entryID); err != nil {
		return "", err
	}
	if summary == nil {
		return "", nil
	}

	id, err := s.storage.CreateEntryID(ctx)
	if err != nil {
		return "", err
	}
	fromID := "root"
	if entryID != nil {
		fromID = *entryID
	}
	entry := &BranchSummaryEntry{
		EntryBase: EntryBase{ID: id, ParentID: entryID, Timestamp: Now()},
		FromID:    fromID,
		Summary:   summary.Summary,
		Details:   summary.Details,
		Usage:     summary.Usage,
		FromHook:  summary.FromHook,
	}
	if err := s.storage.AppendEntry(ctx, entry); err != nil {
		return "", err
	}
	return id, nil
}
