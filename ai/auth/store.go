package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// ModifyFunc receives the current credential (nil when absent) and returns the
// replacement, or nil to leave the entry unchanged.
type ModifyFunc func(current *Credential) (*Credential, error)

// CredentialStore is app-owned credential storage keyed by provider id, one
// credential per provider.
//
// Modify is the only write path, so every mutation is a serialized
// read-modify-write. OAuth refresh runs inside Modify, so concurrent requests
// cannot double-refresh a rotated token.
//
// Error semantics: Read returns (nil, nil) for a missing entry. Methods error
// only on storage failure.
type CredentialStore interface {
	// Read returns the stored credential, possibly expired, or nil.
	Read(ctx context.Context, providerID string) (*Credential, error)
	// List returns credential metadata without exposing or resolving secrets.
	List(ctx context.Context) ([]CredentialInfo, error)
	// Modify performs a serialized read-modify-write, mutually exclusive per
	// provider id (cross-process too, where the backing store supports it).
	// It returns the post-write credential; a nil return from fn leaves the
	// entry unchanged and yields the current value.
	Modify(ctx context.Context, providerID string, fn ModifyFunc) (*Credential, error)
	// Delete removes a credential (logout), serialized against Modify.
	Delete(ctx context.Context, providerID string) error
}

// storeData is the auth.json document: provider id → credential.
type storeData map[string]*Credential

// MemStore is an in-memory CredentialStore for tests and defaults.
type MemStore struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
	data  storeData
}

// NewMemStore creates an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{locks: map[string]*sync.Mutex{}, data: storeData{}}
}

// NewMemStoreFromJSON creates an in-memory store seeded from an auth.json document.
func NewMemStoreFromJSON(doc []byte) (*MemStore, error) {
	s := NewMemStore()
	if len(doc) > 0 {
		if err := json.Unmarshal(doc, &s.data); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *MemStore) lockFor(providerID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[providerID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[providerID] = l
	}
	return l
}

func (s *MemStore) Read(_ context.Context, providerID string) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.data[providerID]
	if !ok || c == nil {
		return nil, nil
	}
	cl := c.Clone()
	return &cl, nil
}

func (s *MemStore) List(_ context.Context) ([]CredentialInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return infoList(s.data), nil
}

func (s *MemStore) Modify(_ context.Context, providerID string, fn ModifyFunc) (*Credential, error) {
	l := s.lockFor(providerID)
	l.Lock()
	defer l.Unlock()

	s.mu.Lock()
	cur := s.data[providerID]
	var curCopy *Credential
	if cur != nil {
		c := cur.Clone()
		curCopy = &c
	}
	s.mu.Unlock()

	next, err := fn(curCopy)
	if err != nil {
		return nil, err
	}
	if next == nil {
		return curCopy, nil
	}

	s.mu.Lock()
	stored := next.Clone()
	s.data[providerID] = &stored
	s.mu.Unlock()
	return next, nil
}

func (s *MemStore) Delete(_ context.Context, providerID string) error {
	l := s.lockFor(providerID)
	l.Lock()
	defer l.Unlock()
	s.mu.Lock()
	delete(s.data, providerID)
	s.mu.Unlock()
	return nil
}

func infoList(data storeData) []CredentialInfo {
	out := make([]CredentialInfo, 0, len(data))
	for id, c := range data {
		if c == nil {
			continue
		}
		out = append(out, CredentialInfo{ProviderID: id, Type: c.Type})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out
}

// FileStore is a CredentialStore backed by an auth.json file, byte-compatible
// with Pi's. The directory is created 0700 and the file written 0600. Writes
// are serialized in-process per provider and across processes by an advisory
// lock on <path>.lock.
type FileStore struct {
	path string

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// ConfigValue resolves a stored api-key value that is an indirection
	// (e.g. "$MY_KEY"). Nil uses DefaultConfigValue.
	ConfigValue func(value string, env ProviderEnv) string
}

// NewFileStore creates a store over the given auth.json path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, locks: map[string]*sync.Mutex{}}
}

// Path returns the backing file path.
func (s *FileStore) Path() string { return s.path }

func (s *FileStore) lockFor(providerID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[providerID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[providerID] = l
	}
	return l
}

func (s *FileStore) ensureDir() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create %s: %w", dir, err)
	}
	return nil
}

func (s *FileStore) readFile() (storeData, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return storeData{}, nil
		}
		return nil, fmt.Errorf("auth: read %s: %w", s.path, err)
	}
	if len(b) == 0 {
		return storeData{}, nil
	}
	var data storeData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("auth: parse %s: %w", s.path, err)
	}
	if data == nil {
		data = storeData{}
	}
	return data, nil
}

func (s *FileStore) writeFile(data storeData) error {
	// Pi writes JSON.stringify(data, null, 2).
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		return fmt.Errorf("auth: write %s: %w", s.path, err)
	}
	// WriteFile honors the mode only on create; enforce it for existing files.
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("auth: chmod %s: %w", s.path, err)
	}
	return nil
}

// withFileLock runs fn while holding the cross-process advisory lock.
func (s *FileStore) withFileLock(ctx context.Context, fn func() error) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	fl := flock.New(s.path + ".lock")
	locked, err := fl.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("auth: lock %s: %w", s.path, err)
	}
	if !locked {
		return fmt.Errorf("auth: could not lock %s", s.path)
	}
	defer func() { _ = fl.Unlock() }()
	return fn()
}

func (s *FileStore) configValue(value string, env ProviderEnv) string {
	if value == "" {
		return value
	}
	if s.ConfigValue != nil {
		return s.ConfigValue(value, env)
	}
	return DefaultConfigValue(value, env)
}

func (s *FileStore) Read(_ context.Context, providerID string) (*Credential, error) {
	data, err := s.readFile()
	if err != nil {
		return nil, err
	}
	c, ok := data[providerID]
	if !ok || c == nil {
		return nil, nil
	}
	out := c.Clone()
	// Pi resolves configured key indirections on read (auth-storage.ts).
	if out.Type == CredentialAPIKey && out.Key != "" {
		out.Key = s.configValue(out.Key, out.Env)
	}
	return &out, nil
}

func (s *FileStore) List(_ context.Context) ([]CredentialInfo, error) {
	data, err := s.readFile()
	if err != nil {
		return nil, err
	}
	return infoList(data), nil
}

func (s *FileStore) Modify(ctx context.Context, providerID string, fn ModifyFunc) (*Credential, error) {
	l := s.lockFor(providerID)
	l.Lock()
	defer l.Unlock()

	var result *Credential
	err := s.withFileLock(ctx, func() error {
		data, err := s.readFile()
		if err != nil {
			return err
		}
		var cur *Credential
		if c, ok := data[providerID]; ok && c != nil {
			cl := c.Clone()
			cur = &cl
		}
		next, err := fn(cur)
		if err != nil {
			return err
		}
		if next == nil {
			result = cur
			return nil
		}
		data[providerID] = next
		if err := s.writeFile(data); err != nil {
			return err
		}
		result = next
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *FileStore) Delete(ctx context.Context, providerID string) error {
	l := s.lockFor(providerID)
	l.Lock()
	defer l.Unlock()

	return s.withFileLock(ctx, func() error {
		data, err := s.readFile()
		if err != nil {
			return err
		}
		delete(data, providerID)
		return s.writeFile(data)
	})
}

// ReadStoredCredential is a one-off read of a provider's credential from an
// auth.json file without instantiating a store or resolving indirections.
// It returns nil on any failure, mirroring Pi's readStoredCredential.
func ReadStoredCredential(providerID, authPath string) *Credential {
	b, err := os.ReadFile(authPath)
	if err != nil {
		return nil
	}
	var data storeData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil
	}
	c, ok := data[providerID]
	if !ok || c == nil {
		return nil
	}
	out := c.Clone()
	return &out
}
