// Package session implements tau's session store: an append-only JSONL tree
// (format version 3) that is byte-compatible with Pi's, so sessions written by
// either agent can be read by the other.
//
// Each file is a header line followed by one entry per line. Entries link via
// id/parentId to form a tree, which is what makes branching possible without
// rewriting history — the "current conversation" is the path from the active
// leaf back to the root.
package session

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ihavespoons/tau/ai"
)

// Version is the only session format version tau reads or writes. Pi
// auto-migrates v1/v2 on load; tau rejects them (the P7 interop importer
// handles migration instead).
const Version = 3

// Entry type discriminators.
const (
	TypeMessage             = "message"
	TypeThinkingLevelChange = "thinking_level_change"
	TypeModelChange         = "model_change"
	TypeActiveToolsChange   = "active_tools_change"
	TypeCompaction          = "compaction"
	TypeBranchSummary       = "branch_summary"
	TypeCustom              = "custom"
	TypeCustomMessage       = "custom_message"
	TypeLabel               = "label"
	TypeSessionInfo         = "session_info"
	TypeLeaf                = "leaf"
)

// Timestamp renders t in the ISO-8601 form Pi writes (JS toISOString).
func Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// Now is the current timestamp in session format.
func Now() string { return Timestamp(time.Now()) }

// parseTimestamp converts a session timestamp to unix milliseconds. Invalid
// timestamps yield 0 rather than an error: a bad timestamp must not make an
// otherwise-readable entry unloadable.
func parseTimestamp(s string) int64 {
	for _, layout := range []string{"2006-01-02T15:04:05.000Z07:00", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// Header is the first line of a session file. It is metadata only and is not
// part of the entry tree (no id/parentId linkage).
type Header struct {
	Version       int            `json:"version"`
	ID            string         `json:"id"`
	Timestamp     string         `json:"timestamp"`
	Cwd           string         `json:"cwd"`
	ParentSession string         `json:"parentSession,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`

	raw json.RawMessage
}

func (h Header) MarshalJSON() ([]byte, error) {
	if len(h.raw) > 0 {
		return h.raw, nil
	}
	type alias Header
	return json.Marshal(struct {
		Type string `json:"type"`
		alias
	}{"session", alias(h)})
}

// UnmarshalJSON validates the header the way Pi does: the version must be
// exactly 3, and id/timestamp/cwd must be present.
func (h *Header) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type          string           `json:"type"`
		Version       *int             `json:"version"`
		ID            string           `json:"id"`
		Timestamp     string           `json:"timestamp"`
		Cwd           string           `json:"cwd"`
		ParentSession *json.RawMessage `json:"parentSession"`
		Metadata      *json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("first line is not a valid session header: %w", err)
	}
	if probe.Type != "session" {
		return fmt.Errorf("first line is not a valid session header")
	}
	if probe.Version == nil || *probe.Version != Version {
		got := "missing"
		if probe.Version != nil {
			got = fmt.Sprintf("%d", *probe.Version)
		}
		return fmt.Errorf("unsupported session version (want %d, got %s)", Version, got)
	}
	if probe.ID == "" {
		return fmt.Errorf("session header is missing id")
	}
	if probe.Timestamp == "" {
		return fmt.Errorf("session header is missing timestamp")
	}
	if probe.Cwd == "" {
		return fmt.Errorf("session header is missing cwd")
	}

	h.Version = *probe.Version
	h.ID = probe.ID
	h.Timestamp = probe.Timestamp
	h.Cwd = probe.Cwd

	if probe.ParentSession != nil {
		var s string
		if err := json.Unmarshal(*probe.ParentSession, &s); err != nil {
			return fmt.Errorf("session header parentSession must be a string")
		}
		h.ParentSession = s
	}
	if probe.Metadata != nil {
		var m map[string]any
		if err := json.Unmarshal(*probe.Metadata, &m); err != nil {
			return fmt.Errorf("session header metadata must be an object")
		}
		h.Metadata = m
	}
	h.raw = append(json.RawMessage(nil), data...)
	return nil
}

// EntryBase carries the fields every tree entry shares.
//
// raw holds the verbatim line an entry was decoded from. Entries are
// append-only and never mutated after write, so re-marshalling a decoded entry
// reproduces its source bytes exactly — that is what makes fork byte-lossless
// and preserves fields written by a newer Pi (or tau) than this build knows.
// Constructing an entry in code leaves raw empty, so it marshals from struct.
type EntryBase struct {
	ID        string  `json:"id"`
	ParentID  *string `json:"parentId"`
	Timestamp string  `json:"timestamp"`

	raw json.RawMessage
}

// Base returns the shared fields.
func (b *EntryBase) Base() *EntryBase { return b }

// Raw returns the verbatim source line, or nil for a constructed entry.
func (b *EntryBase) Raw() json.RawMessage { return b.raw }

// Detach drops the retained source bytes so subsequent field changes are
// reflected on marshal. Call it before mutating a decoded entry.
func (b *EntryBase) Detach() { b.raw = nil }

// Entry is one line of the session tree.
type Entry interface {
	EntryType() string
	Base() *EntryBase
	json.Marshaler
}

// MessageEntry stores a conversation message.
type MessageEntry struct {
	EntryBase
	Message ai.Message `json:"message"`
}

// ThinkingLevelChangeEntry records a thinking/reasoning level switch.
type ThinkingLevelChangeEntry struct {
	EntryBase
	ThinkingLevel string `json:"thinkingLevel"`
}

// ModelChangeEntry records a model switch.
type ModelChangeEntry struct {
	EntryBase
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// ActiveToolsChangeEntry records a change to the enabled tool set.
type ActiveToolsChangeEntry struct {
	EntryBase
	ActiveToolNames []string `json:"activeToolNames"`
}

// CompactionEntry checkpoints the conversation with a summary of everything
// before it. RetainedTail makes the entry self-contained; FirstKeptEntryID is
// the older form, kept for sessions written before RetainedTail existed.
type CompactionEntry struct {
	EntryBase
	Summary          string       `json:"summary"`
	FirstKeptEntryID string       `json:"firstKeptEntryId,omitempty"`
	TokensBefore     int          `json:"tokensBefore"`
	RetainedTail     []ai.Message `json:"retainedTail,omitempty"`
	Details          any          `json:"details,omitempty"`
	Usage            *ai.Usage    `json:"usage,omitempty"`
	FromHook         bool         `json:"fromHook,omitempty"`
}

// BranchSummaryEntry captures context from a branch the conversation left.
type BranchSummaryEntry struct {
	EntryBase
	FromID   string    `json:"fromId"`
	Summary  string    `json:"summary"`
	Details  any       `json:"details,omitempty"`
	Usage    *ai.Usage `json:"usage,omitempty"`
	FromHook bool      `json:"fromHook,omitempty"`
}

// CustomEntry persists extension state. It never enters model context.
type CustomEntry struct {
	EntryBase
	CustomType string `json:"customType"`
	Data       any    `json:"data,omitempty"`
}

// CustomMessageEntry is an extension-injected message that does enter model
// context.
type CustomMessageEntry struct {
	EntryBase
	CustomType string         `json:"customType"`
	Content    ai.UserContent `json:"content"`
	Details    any            `json:"details,omitempty"`
	Display    bool           `json:"display"`
}

// LabelEntry sets or clears a user bookmark on another entry. A nil Label
// clears it.
type LabelEntry struct {
	EntryBase
	TargetID string  `json:"targetId"`
	Label    *string `json:"label"`
}

// SessionInfoEntry carries session metadata, currently the display name.
// The type name is Pi's legacy spelling.
type SessionInfoEntry struct {
	EntryBase
	Name string `json:"name,omitempty"`
}

// LeafEntry records a move of the active leaf. Appending one is how branching
// is persisted — the log is never rewritten.
type LeafEntry struct {
	EntryBase
	TargetID *string `json:"targetId"`
}

// OpaqueEntry preserves an entry whose type this build does not recognize, so
// a session written by a newer agent still loads and round-trips intact.
type OpaqueEntry struct {
	EntryBase
	Type string
}

func (*MessageEntry) EntryType() string             { return TypeMessage }
func (*ThinkingLevelChangeEntry) EntryType() string { return TypeThinkingLevelChange }
func (*ModelChangeEntry) EntryType() string         { return TypeModelChange }
func (*ActiveToolsChangeEntry) EntryType() string   { return TypeActiveToolsChange }
func (*CompactionEntry) EntryType() string          { return TypeCompaction }
func (*BranchSummaryEntry) EntryType() string       { return TypeBranchSummary }
func (*CustomEntry) EntryType() string              { return TypeCustom }
func (*CustomMessageEntry) EntryType() string       { return TypeCustomMessage }
func (*LabelEntry) EntryType() string               { return TypeLabel }
func (*SessionInfoEntry) EntryType() string         { return TypeSessionInfo }
func (*LeafEntry) EntryType() string                { return TypeLeaf }
func (e *OpaqueEntry) EntryType() string            { return e.Type }

// withDiscriminator marshals v and splices a discriminator in as the first
// field, which is where Pi writes it.
func withDiscriminator(key, value string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) < 2 || b[0] != '{' {
		return nil, fmt.Errorf("session: cannot add %q to non-object JSON", key)
	}
	head, err := json.Marshal(map[string]string{key: value})
	if err != nil {
		return nil, err
	}
	if len(b) == 2 { // "{}" — nothing to join
		return head, nil
	}
	out := make([]byte, 0, len(head)+len(b))
	out = append(out, head[:len(head)-1]...) // drop closing brace
	out = append(out, ',')
	out = append(out, b[1:]...) // drop opening brace
	return out, nil
}

// marshalEntry emits the retained source bytes when present, otherwise the
// struct with its type discriminator injected.
func marshalEntry(base *EntryBase, typ string, v any) ([]byte, error) {
	if len(base.raw) > 0 {
		return base.raw, nil
	}
	return withDiscriminator("type", typ, v)
}

func (e *MessageEntry) MarshalJSON() ([]byte, error) {
	type alias MessageEntry
	return marshalEntry(&e.EntryBase, TypeMessage, alias(*e))
}

func (e *ThinkingLevelChangeEntry) MarshalJSON() ([]byte, error) {
	type alias ThinkingLevelChangeEntry
	return marshalEntry(&e.EntryBase, TypeThinkingLevelChange, alias(*e))
}

func (e *ModelChangeEntry) MarshalJSON() ([]byte, error) {
	type alias ModelChangeEntry
	return marshalEntry(&e.EntryBase, TypeModelChange, alias(*e))
}

func (e *ActiveToolsChangeEntry) MarshalJSON() ([]byte, error) {
	type alias ActiveToolsChangeEntry
	return marshalEntry(&e.EntryBase, TypeActiveToolsChange, alias(*e))
}

func (e *CompactionEntry) MarshalJSON() ([]byte, error) {
	type alias CompactionEntry
	return marshalEntry(&e.EntryBase, TypeCompaction, alias(*e))
}

func (e *BranchSummaryEntry) MarshalJSON() ([]byte, error) {
	type alias BranchSummaryEntry
	return marshalEntry(&e.EntryBase, TypeBranchSummary, alias(*e))
}

func (e *CustomEntry) MarshalJSON() ([]byte, error) {
	type alias CustomEntry
	return marshalEntry(&e.EntryBase, TypeCustom, alias(*e))
}

func (e *CustomMessageEntry) MarshalJSON() ([]byte, error) {
	type alias CustomMessageEntry
	return marshalEntry(&e.EntryBase, TypeCustomMessage, alias(*e))
}

func (e *LabelEntry) MarshalJSON() ([]byte, error) {
	type alias LabelEntry
	return marshalEntry(&e.EntryBase, TypeLabel, alias(*e))
}

func (e *SessionInfoEntry) MarshalJSON() ([]byte, error) {
	type alias SessionInfoEntry
	return marshalEntry(&e.EntryBase, TypeSessionInfo, alias(*e))
}

func (e *LeafEntry) MarshalJSON() ([]byte, error) {
	type alias LeafEntry
	return marshalEntry(&e.EntryBase, TypeLeaf, alias(*e))
}

// MarshalJSON on an opaque entry always replays its source bytes; there is no
// struct form to fall back to.
func (e *OpaqueEntry) MarshalJSON() ([]byte, error) {
	if len(e.raw) > 0 {
		return e.raw, nil
	}
	return nil, fmt.Errorf("session: opaque entry %q has no source bytes", e.Type)
}

// UnmarshalJSON decodes the wrapped agent message via the role registry.
func (e *MessageEntry) UnmarshalJSON(data []byte) error {
	var probe struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	type alias MessageEntry
	var a struct {
		alias
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = MessageEntry(a.alias)
	if len(probe.Message) == 0 {
		return fmt.Errorf("message entry is missing message")
	}
	msg, err := decodeAgentMessage(probe.Message)
	e.Message = msg
	return err
}

// UnmarshalJSON decodes retainedTail through the role registry so synthetic
// and custom roles survive a compaction checkpoint.
func (e *CompactionEntry) UnmarshalJSON(data []byte) error {
	type alias CompactionEntry
	var a struct {
		alias
		RetainedTail []json.RawMessage `json:"retainedTail"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = CompactionEntry(a.alias)
	if a.RetainedTail == nil {
		return nil
	}
	e.RetainedTail = make([]ai.Message, 0, len(a.RetainedTail))
	var firstErr error
	for _, raw := range a.RetainedTail {
		msg, err := decodeAgentMessage(raw)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		e.RetainedTail = append(e.RetainedTail, msg)
	}
	return firstErr
}

// UnmarshalEntry decodes one entry line.
//
// It returns a usable Entry even for input this build does not fully
// understand: an unrecognized entry type or message role yields an opaque
// passthrough plus a non-nil error describing what was not understood. Callers
// loading a file treat that error as a soft failure and keep the entry, so one
// unknown line never makes a user's history unreadable.
func UnmarshalEntry(data []byte) (Entry, error) {
	var probe struct {
		Type      string           `json:"type"`
		ID        string           `json:"id"`
		ParentID  *json.RawMessage `json:"parentId"`
		Timestamp string           `json:"timestamp"`
		TargetID  *json.RawMessage `json:"targetId"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("is not valid JSON: %w", err)
	}
	if probe.Type == "" {
		return nil, fmt.Errorf("is missing entry type")
	}
	if probe.ID == "" {
		return nil, fmt.Errorf("is missing entry id")
	}
	if probe.Timestamp == "" {
		return nil, fmt.Errorf("is missing timestamp")
	}
	var parentID *string
	if probe.ParentID != nil {
		if err := json.Unmarshal(*probe.ParentID, &parentID); err != nil {
			return nil, fmt.Errorf("has invalid parentId")
		}
	}
	if probe.Type == TypeLeaf && probe.TargetID != nil {
		var s *string
		if err := json.Unmarshal(*probe.TargetID, &s); err != nil {
			return nil, fmt.Errorf("has invalid targetId")
		}
	}

	raw := append(json.RawMessage(nil), data...)

	// An opaque entry still carries its tree links, so an unrecognized line
	// never severs the path between the entries around it.
	opaque := func() *OpaqueEntry {
		return &OpaqueEntry{
			EntryBase: EntryBase{ID: probe.ID, ParentID: parentID, Timestamp: probe.Timestamp, raw: raw},
			Type:      probe.Type,
		}
	}

	decode := func(e Entry) (Entry, error) {
		if err := json.Unmarshal(data, e); err != nil {
			// A structurally valid line whose payload we cannot map keeps its
			// bytes rather than failing the load.
			return opaque(), fmt.Errorf("entry %s (%s): %w", probe.ID, probe.Type, err)
		}
		e.Base().raw = raw
		return e, nil
	}

	switch probe.Type {
	case TypeMessage:
		e := &MessageEntry{}
		// A message entry decodes even when its role is unknown; the error is
		// soft and the entry is kept.
		err := json.Unmarshal(data, e)
		e.Base().raw = raw
		if err != nil {
			return e, fmt.Errorf("entry %s (message): %w", probe.ID, err)
		}
		return e, nil
	case TypeThinkingLevelChange:
		return decode(&ThinkingLevelChangeEntry{})
	case TypeModelChange:
		return decode(&ModelChangeEntry{})
	case TypeActiveToolsChange:
		return decode(&ActiveToolsChangeEntry{})
	case TypeCompaction:
		e := &CompactionEntry{}
		err := json.Unmarshal(data, e)
		e.Base().raw = raw
		if err != nil {
			return e, fmt.Errorf("entry %s (compaction): %w", probe.ID, err)
		}
		return e, nil
	case TypeBranchSummary:
		return decode(&BranchSummaryEntry{})
	case TypeCustom:
		return decode(&CustomEntry{})
	case TypeCustomMessage:
		return decode(&CustomMessageEntry{})
	case TypeLabel:
		return decode(&LabelEntry{})
	case TypeSessionInfo:
		return decode(&SessionInfoEntry{})
	case TypeLeaf:
		return decode(&LeafEntry{})
	default:
		return opaque(), fmt.Errorf("entry %s: unknown entry type %q", probe.ID, probe.Type)
	}
}

// leafIDAfterEntry is the active leaf once entry has been appended: a leaf
// entry redirects to its target, anything else becomes the leaf itself.
func leafIDAfterEntry(e Entry) *string {
	if l, ok := e.(*LeafEntry); ok {
		return l.TargetID
	}
	id := e.Base().ID
	return &id
}

func ptr[T any](v T) *T { return &v }
