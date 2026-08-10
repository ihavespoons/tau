package session

import (
	"context"
	"sync"
)

// MemStorage keeps a session entirely in memory. It backs `--no-session` runs
// and tests.
type MemStorage struct {
	mu   sync.RWMutex
	meta Metadata
	ix   *Index
}

var _ Storage = (*MemStorage)(nil)

// NewMemStorage creates an empty in-memory session.
func NewMemStorage(meta Metadata) *MemStorage {
	return &MemStorage{meta: meta, ix: NewIndex()}
}

func (s *MemStorage) Metadata(context.Context) (Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta, nil
}

func (s *MemStorage) LeafID(context.Context) (*string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Leaf()
}

func (s *MemStorage) SetLeafID(ctx context.Context, leafID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if leafID != nil {
		if _, ok := s.ix.Get(*leafID); !ok {
			return errorf(CodeNotFound, nil, "entry %s not found", *leafID)
		}
	}
	entry := &LeafEntry{
		EntryBase: EntryBase{ID: s.ix.CreateEntryID(), ParentID: s.ix.Head(), Timestamp: Now()},
		TargetID:  leafID,
	}
	s.ix.Add(entry)
	return nil
}

func (s *MemStorage) CreateEntryID(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ix.CreateEntryID(), nil
}

func (s *MemStorage) AppendEntry(_ context.Context, entry Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ix.Add(entry)
	return nil
}

func (s *MemStorage) GetEntry(_ context.Context, id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Get(id)
}

func (s *MemStorage) FindEntries(_ context.Context, entryType string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Find(entryType)
}

func (s *MemStorage) Label(_ context.Context, id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Label(id)
}

func (s *MemStorage) SessionName(context.Context) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.SessionName()
}

func (s *MemStorage) Stats(context.Context) Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Stats()
}

func (s *MemStorage) PathToRootOrCompaction(_ context.Context, leafID *string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.PathToRootOrCompaction(leafID)
}

func (s *MemStorage) Entries(_ context.Context, opts *CursorOptions) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Slice(opts)
}
