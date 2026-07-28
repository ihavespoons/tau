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
	ix   *index
}

var _ Storage = (*MemStorage)(nil)

// NewMemStorage creates an empty in-memory session.
func NewMemStorage(meta Metadata) *MemStorage {
	return &MemStorage{meta: meta, ix: newIndex()}
}

func (s *MemStorage) Metadata(context.Context) (Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta, nil
}

func (s *MemStorage) LeafID(context.Context) (*string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.leaf()
}

func (s *MemStorage) SetLeafID(ctx context.Context, leafID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if leafID != nil {
		if _, ok := s.ix.get(*leafID); !ok {
			return errorf(CodeNotFound, nil, "entry %s not found", *leafID)
		}
	}
	entry := &LeafEntry{
		EntryBase: EntryBase{ID: s.ix.createEntryID(), ParentID: s.ix.leafID, Timestamp: Now()},
		TargetID:  leafID,
	}
	s.ix.add(entry)
	return nil
}

func (s *MemStorage) CreateEntryID(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ix.createEntryID(), nil
}

func (s *MemStorage) AppendEntry(_ context.Context, entry Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ix.add(entry)
	return nil
}

func (s *MemStorage) GetEntry(_ context.Context, id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.get(id)
}

func (s *MemStorage) FindEntries(_ context.Context, entryType string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.find(entryType)
}

func (s *MemStorage) Label(_ context.Context, id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.label(id)
}

func (s *MemStorage) SessionName(context.Context) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.sessionName()
}

func (s *MemStorage) Stats(context.Context) Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.stats()
}

func (s *MemStorage) PathToRootOrCompaction(_ context.Context, leafID *string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.pathToRootOrCompaction(leafID)
}

func (s *MemStorage) Entries(_ context.Context, opts *CursorOptions) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.slice(opts)
}
