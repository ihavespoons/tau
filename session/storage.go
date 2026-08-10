package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/ihavespoons/tau/ai"
)

// Error codes for session failures.
const (
	CodeNotFound       = "not_found"
	CodeInvalidSession = "invalid_session"
	CodeInvalidEntry   = "invalid_entry"
	CodeInvalidFork    = "invalid_fork_target"
	CodeStorage        = "storage"
)

// Error is a session failure with a machine-readable code.
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func errorf(code string, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// IsCode reports whether err is a session *Error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Metadata describes a stored session.
type Metadata struct {
	ID                string
	CreatedAt         string
	Cwd               string
	Path              string
	ParentSessionPath string
	Metadata          map[string]any
}

// Stats summarizes a session's size and cost.
type Stats struct {
	MessageCount   int
	CachedTokens   int
	UncachedTokens int
	TotalTokens    int
	CostTotal      float64
}

// CursorOptions bounds an entry listing.
type CursorOptions struct {
	AfterEntrySeq int
	Limit         int
}

// Storage is the append-only entry log behind a session.
//
// Implementations are single-writer: one process owns a session file at a
// time, matching Pi. Concurrent appends from multiple writers are not
// coordinated and will interleave lines.
type Storage interface {
	Metadata(ctx context.Context) (Metadata, error)
	LeafID(ctx context.Context) (*string, error)
	// SetLeafID appends a leaf entry recording the move; it never rewrites.
	SetLeafID(ctx context.Context, leafID *string) error
	CreateEntryID(ctx context.Context) (string, error)
	AppendEntry(ctx context.Context, entry Entry) error
	GetEntry(ctx context.Context, id string) (Entry, bool)
	FindEntries(ctx context.Context, entryType string) []Entry
	Label(ctx context.Context, id string) (string, bool)
	SessionName(ctx context.Context) (string, bool)
	Stats(ctx context.Context) Stats
	// PathToRootOrCompaction returns the entries from leafID back to the root
	// in root-first order, stopping early at a compaction checkpoint.
	PathToRootOrCompaction(ctx context.Context, leafID *string) ([]Entry, error)
	Entries(ctx context.Context, opts *CursorOptions) []Entry
}

// Index maintains the in-memory view every storage backend answers from: entry
// lookup, label state, and the current leaf.
//
// It is exported because it is the reusable half of implementing Storage. A
// backend supplies durability — a file, a table, a network — and the semantics
// here stay identical across all of them: label last-write-wins, the leaf
// derived from the entries themselves, the path walk that stops at a
// compaction. A second backend reimplementing those in its own idiom would be a
// second set of answers to drift apart.
type Index struct {
	entries []Entry
	byID    map[string]Entry
	labels  map[string]string
	leafID  *string
}

// NewIndex returns an empty index for a backend to fill.
func NewIndex() *Index {
	return &Index{byID: map[string]Entry{}, labels: map[string]string{}}
}

// Head is the current leaf without the existence check Leaf makes, which is
// what a new entry takes as its parent: the leaf it is about to become the
// child of has just been indexed, so there is nothing yet to verify.
func (ix *Index) Head() *string { return ix.leafID }

// Add indexes an entry that was just appended.
func (ix *Index) Add(e Entry) {
	ix.entries = append(ix.entries, e)
	ix.byID[e.Base().ID] = e
	ix.updateLabel(e)
	ix.leafID = leafIDAfterEntry(e)
}

// AddLoaded indexes an entry read back from storage during open.
func (ix *Index) AddLoaded(e Entry) {
	ix.entries = append(ix.entries, e)
	ix.byID[e.Base().ID] = e
	ix.updateLabel(e)
	ix.leafID = leafIDAfterEntry(e)
}

func (ix *Index) updateLabel(e Entry) {
	l, ok := e.(*LabelEntry)
	if !ok {
		return
	}
	if l.Label != nil && strings.TrimSpace(*l.Label) != "" {
		ix.labels[l.TargetID] = strings.TrimSpace(*l.Label)
	} else {
		delete(ix.labels, l.TargetID)
	}
}

// createEntryID mints a short id from the random tail of a uuidv7. The v7
// prefix is time-derived and nearly constant between calls, so the entropy
// must come from the end.
func (ix *Index) CreateEntryID() string {
	for i := 0; i < 100; i++ {
		u, err := uuid.NewV7()
		if err != nil {
			continue
		}
		s := u.String()
		id := s[len(s)-8:]
		if _, taken := ix.byID[id]; !taken {
			return id
		}
	}
	if u, err := uuid.NewV7(); err == nil {
		return u.String()
	}
	return uuid.NewString()
}

func (ix *Index) Leaf() (*string, error) {
	if ix.leafID != nil {
		if _, ok := ix.byID[*ix.leafID]; !ok {
			return nil, errorf(CodeInvalidSession, nil, "entry %s not found", *ix.leafID)
		}
	}
	return ix.leafID, nil
}

func (ix *Index) Get(id string) (Entry, bool) {
	e, ok := ix.byID[id]
	return e, ok
}

func (ix *Index) Find(entryType string) []Entry {
	var out []Entry
	for _, e := range ix.entries {
		if e.EntryType() == entryType {
			out = append(out, e)
		}
	}
	return out
}

func (ix *Index) Label(id string) (string, bool) {
	l, ok := ix.labels[id]
	return l, ok
}

func (ix *Index) SessionName() (string, bool) {
	for i := len(ix.entries) - 1; i >= 0; i-- {
		if info, ok := ix.entries[i].(*SessionInfoEntry); ok {
			if name := strings.TrimSpace(info.Name); name != "" {
				return name, true
			}
			return "", false
		}
	}
	return "", false
}

func (ix *Index) Stats() Stats {
	var s Stats
	for _, e := range ix.entries {
		var usage *ai.Usage
		switch entry := e.(type) {
		case *MessageEntry:
			s.MessageCount++
			if am, ok := entry.Message.(ai.AssistantMessage); ok {
				usage = &am.Usage
			} else if am, ok := entry.Message.(*ai.AssistantMessage); ok {
				usage = &am.Usage
			}
		case *CompactionEntry:
			usage = entry.Usage
		case *BranchSummaryEntry:
			usage = entry.Usage
		}
		if usage == nil {
			continue
		}
		s.CachedTokens += usage.CacheRead
		s.UncachedTokens += usage.Input + usage.CacheWrite
		s.TotalTokens += usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
		s.CostTotal += usage.Cost.Total
	}
	return s
}

// pathToRootOrCompaction walks parent links from leafID to the root,
// returning entries root-first. A compaction with a retained tail is a
// self-contained checkpoint and stops the walk immediately; otherwise the walk
// continues back to the compaction's first kept entry.
func (ix *Index) PathToRootOrCompaction(leafID *string) ([]Entry, error) {
	if leafID == nil {
		return nil, nil
	}
	current, ok := ix.byID[*leafID]
	if !ok {
		return nil, errorf(CodeNotFound, nil, "entry %s not found", *leafID)
	}

	var path []Entry
	var stopAt *string
	for current != nil {
		path = append([]Entry{current}, path...)
		if stopAt != nil && current.Base().ID == *stopAt {
			break
		}
		if c, isCompaction := current.(*CompactionEntry); isCompaction {
			if c.RetainedTail != nil {
				break
			}
			if c.FirstKeptEntryID != "" {
				stopAt = &c.FirstKeptEntryID
			} else {
				stopAt = nil
			}
		}
		parentID := current.Base().ParentID
		if parentID == nil || *parentID == "" {
			break
		}
		parent, found := ix.byID[*parentID]
		if !found {
			return nil, errorf(CodeInvalidSession, nil, "entry %s not found", *parentID)
		}
		current = parent
	}
	return path, nil
}

func (ix *Index) Slice(opts *CursorOptions) []Entry {
	start := 0
	if opts != nil {
		start = opts.AfterEntrySeq
	}
	if start < 0 {
		start = 0
	}
	if start >= len(ix.entries) {
		return nil
	}
	end := len(ix.entries)
	if opts != nil && opts.Limit > 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}
	out := make([]Entry, end-start)
	copy(out, ix.entries[start:end])
	return out
}
