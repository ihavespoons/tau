package models

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/ihavespoons/tau/ai"
)

// CatalogStore persists the catalogs of providers that publish their models
// over the network rather than shipping a static list.
//
// It is a cache, not configuration: losing it costs one refresh. That is what
// makes it safe to read at startup and to keep out of models.json, which the
// user owns and hand-edits.
//
// The file matches Pi's ~/.pi/agent/models-store.json — a map of provider id to
// {models, checkedAt} — so an imported Pi setup starts with a warm cache.
type CatalogStore struct {
	path string
}

// NewCatalogStore returns a store backed by path.
func NewCatalogStore(path string) *CatalogStore { return &CatalogStore{path: path} }

// Path is where the store reads and writes.
func (s *CatalogStore) Path() string { return s.path }

// CatalogEntry is one provider's cached catalog.
type CatalogEntry struct {
	Models []ai.Model `json:"models"`
	// CheckedAt is when the catalog was last fetched (unix ms).
	CheckedAt int64 `json:"checkedAt"`
}

type catalogFile map[string]*CatalogEntry

// Read returns the cached catalog for a provider, or nil if there is none.
//
// A missing or unreadable file yields no entry and no error: a corrupted cache
// must not stop tau from starting, because every provider with a compiled
// catalog is unaffected by it.
func (s *CatalogStore) Read(providerID string) *CatalogEntry {
	data, err := s.read()
	if err != nil {
		return nil
	}
	return data[providerID]
}

// Write replaces one provider's cached catalog.
func (s *CatalogStore) Write(ctx context.Context, providerID string, models []ai.Model) error {
	return s.withLock(ctx, func() error {
		data, err := s.read()
		if err != nil {
			// The file is unreadable, so it is replaced rather than merged.
			// Refusing to write would leave a corrupt cache corrupt forever.
			data = catalogFile{}
		}
		data[providerID] = &CatalogEntry{Models: models, CheckedAt: time.Now().UnixMilli()}
		return s.write(data)
	})
}

// Delete drops one provider's cached catalog.
func (s *CatalogStore) Delete(ctx context.Context, providerID string) error {
	return s.withLock(ctx, func() error {
		data, err := s.read()
		if err != nil || data[providerID] == nil {
			return nil
		}
		delete(data, providerID)
		return s.write(data)
	})
}

func (s *CatalogStore) read() (catalogFile, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return catalogFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var data catalogFile
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("models: parse %s: %w", s.path, err)
	}
	if data == nil {
		data = catalogFile{}
	}
	return data, nil
}

func (s *CatalogStore) write(data catalogFile) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.path, b, 0o644); err != nil {
		return fmt.Errorf("models: write %s: %w", s.path, err)
	}
	return nil
}

// withLock holds the cross-process advisory lock for the duration of fn. Two
// tau processes can log in at once, and a lost write is a catalog that silently
// reverts to the older one's view.
func (s *CatalogStore) withLock(ctx context.Context, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	fl := flock.New(s.path + ".lock")
	locked, err := fl.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("models: lock %s: %w", s.path, err)
	}
	if !locked {
		return fmt.Errorf("models: could not lock %s", s.path)
	}
	defer func() { _ = fl.Unlock() }()
	return fn()
}
