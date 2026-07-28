package tools

import (
	"path/filepath"
	"sync"
)

// mutationQueue serializes mutations targeting the same file while letting
// different files proceed in parallel. Under parallel tool execution an edit
// and a write can otherwise interleave read-modify-write cycles on one path
// and lose data (Pi's file-mutation-queue.ts).
type mutationQueue struct {
	mu    sync.Mutex
	locks map[string]*queueEntry
}

type queueEntry struct {
	mu   sync.Mutex
	refs int
}

var fileMutations = &mutationQueue{locks: map[string]*queueEntry{}}

// key normalizes a path so two spellings of the same file share a lock. Symlinks
// are resolved when possible; a path that does not exist yet (the common case
// for write) falls back to its cleaned absolute form.
func (q *mutationQueue) key(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// withFileMutation runs fn while holding the lock for path.
func (q *mutationQueue) withFileMutation(path string, fn func() error) error {
	k := q.key(path)

	q.mu.Lock()
	entry, ok := q.locks[k]
	if !ok {
		entry = &queueEntry{}
		q.locks[k] = entry
	}
	entry.refs++
	q.mu.Unlock()

	entry.mu.Lock()
	defer func() {
		entry.mu.Unlock()
		q.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(q.locks, k)
		}
		q.mu.Unlock()
	}()

	return fn()
}

// WithFileMutation serializes mutations to the same file across the process.
func WithFileMutation(path string, fn func() error) error {
	return fileMutations.withFileMutation(path, fn)
}
