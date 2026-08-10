package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// JSONLStorage is a session backed by a JSONL file: a header line followed by
// one entry per line, appended and never rewritten.
type JSONLStorage struct {
	mu     sync.RWMutex
	path   string
	header Header
	ix     *Index
	// soft records recoverable problems found while loading — unknown entry
	// types or message roles. The entries are kept; these explain what was not
	// understood.
	soft []error
	// migrated reports that the file was written in an older format.
	migrated bool
}

// Migrated reports whether the session was upgraded from an older format when
// it was opened.
func (s *JSONLStorage) Migrated() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.migrated
}

var _ Storage = (*JSONLStorage)(nil)

// SoftErrors returns problems encountered while loading that did not prevent
// the session from opening.
func (s *JSONLStorage) SoftErrors() []error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]error(nil), s.soft...)
}

// Path is the session file's location.
func (s *JSONLStorage) Path() string { return s.path }

// Header returns the session header.
func (s *JSONLStorage) Header() Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.header
}

// CreateOptions configures a new JSONL session file.
type CreateOptions struct {
	Cwd               string
	SessionID         string
	ParentSessionPath string
	Metadata          map[string]any
}

// CreateJSONL writes a new session file with its header.
func CreateJSONL(path string, opts CreateOptions) (*JSONLStorage, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, errorf(CodeStorage, err, "failed to create session directory for %s", path)
	}
	header := Header{
		Version:       Version,
		ID:            opts.SessionID,
		Timestamp:     Now(),
		Cwd:           opts.Cwd,
		ParentSession: opts.ParentSessionPath,
		Metadata:      opts.Metadata,
	}
	line, err := json.Marshal(header)
	if err != nil {
		return nil, errorf(CodeStorage, err, "failed to encode session header")
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		return nil, errorf(CodeStorage, err, "failed to create session %s", path)
	}
	return &JSONLStorage{path: path, header: header, ix: NewIndex()}, nil
}

// OpenJSONL reads an existing session file, migrating it in place if it was
// written in an older format.
//
// Lines that cannot be fully understood are preserved verbatim and reported
// through SoftErrors rather than failing the open — a session written by a
// newer agent, or containing one corrupt entry, still loads. A missing or
// invalid header is fatal.
func OpenJSONL(path string) (*JSONLStorage, error) {
	return openJSONL(path, true)
}

// OpenJSONLReadOnly opens a session without ever writing to it. An older
// format is still migrated for this process's view of the file.
//
// This is what reads someone else's session — a Pi file being imported, or one
// only being inspected. Migration would otherwise rewrite the source, and an
// import that mutates its input is not an import.
func OpenJSONLReadOnly(path string) (*JSONLStorage, error) {
	return openJSONL(path, false)
}

func openJSONL(path string, rewriteOnMigrate bool) (*JSONLStorage, error) {
	lines, err := readJSONLines(path)
	if err != nil {
		return nil, err
	}

	// Migration must happen before decoding: a v1 entry has no id and no
	// parentId, so there is no tree for the decoder to build until it does.
	// The rewrite is not optional when tau will append to this file — appended
	// entries parent onto ids minted here, and minting is random, so a second
	// open of an unmigrated file would produce different ids and dangling
	// parents.
	migrated, changed, err := MigrateLines(lines)
	if err != nil {
		return nil, err
	}
	if changed {
		lines = migrated
		if rewriteOnMigrate {
			if err := rewriteJSONL(path, lines); err != nil {
				return nil, err
			}
		}
	}

	s := &JSONLStorage{path: path, ix: NewIndex(), migrated: changed}
	sawHeader := false
	for i, line := range lines {
		if !sawHeader {
			if err := json.Unmarshal(line, &s.header); err != nil {
				return nil, errorf(CodeInvalidSession, err, "invalid session file %s", path)
			}
			sawHeader = true
			continue
		}
		entry, err := UnmarshalEntry(line)
		if err != nil {
			if entry == nil {
				return nil, errorf(CodeInvalidEntry, err, "invalid session file %s: line %d", path, i+1)
			}
			s.soft = append(s.soft, errorf(CodeInvalidEntry, err, "%s line %d", path, i+1))
		}
		s.ix.AddLoaded(entry)
	}
	if !sawHeader {
		return nil, errorf(CodeInvalidSession, nil, "invalid session file %s: missing session header", path)
	}
	return s, nil
}

func readJSONLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errorf(CodeNotFound, err, "session not found: %s", path)
		}
		return nil, errorf(CodeStorage, err, "failed to read session %s", path)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Session lines carry whole messages, which can be large; raise the cap
	// well past bufio's 64 KiB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	var lines [][]byte
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lines = append(lines, []byte(line))
	}
	if err := scanner.Err(); err != nil {
		return nil, errorf(CodeStorage, err, "failed to read session %s", path)
	}
	return lines, nil
}

// rewriteJSONL replaces a session file atomically. The append-only log is
// rewritten exactly once in its life — when its format is upgraded — and a
// half-written session would take the conversation with it.
func rewriteJSONL(path string, lines [][]byte) error {
	var buf bytes.Buffer
	for _, line := range lines {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tau-session-*.jsonl")
	if err != nil {
		return errorf(CodeStorage, err, "failed to migrate session %s", path)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return errorf(CodeStorage, err, "failed to migrate session %s", path)
	}
	if err := tmp.Close(); err != nil {
		return errorf(CodeStorage, err, "failed to migrate session %s", path)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return errorf(CodeStorage, err, "failed to migrate session %s", path)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errorf(CodeStorage, err, "failed to migrate session %s", path)
	}
	return nil
}

// LoadJSONLMetadata reads only a session file's header.
func LoadJSONLMetadata(path string) (Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, errorf(CodeNotFound, err, "session not found: %s", path)
		}
		return Metadata{}, errorf(CodeStorage, err, "failed to read session header %s", path)
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReaderSize(f, 64*1024)
	var line string
	for {
		l, err := reader.ReadString('\n')
		if strings.TrimSpace(l) != "" {
			line = strings.TrimSpace(l)
			break
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return Metadata{}, errorf(CodeStorage, err, "failed to read session header %s", path)
		}
	}
	if line == "" {
		return Metadata{}, errorf(CodeInvalidSession, nil, "invalid session file %s: missing session header", path)
	}
	var header Header
	if err := json.Unmarshal([]byte(line), &header); err != nil {
		return Metadata{}, errorf(CodeInvalidSession, err, "invalid session file %s", path)
	}
	return headerMetadata(header, path), nil
}

func headerMetadata(h Header, path string) Metadata {
	return Metadata{
		ID:                h.ID,
		CreatedAt:         h.Timestamp,
		Cwd:               h.Cwd,
		Path:              path,
		ParentSessionPath: h.ParentSession,
		Metadata:          h.Metadata,
	}
}

// appendLine writes one JSONL record. Opening with O_APPEND keeps writes
// atomic for line-sized payloads on POSIX.
func (s *JSONLStorage) appendLine(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return errorf(CodeStorage, err, "failed to encode session entry")
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return errorf(CodeStorage, err, "failed to open session %s", s.path)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return errorf(CodeStorage, err, "failed to append to session %s", s.path)
	}
	return nil
}

func (s *JSONLStorage) Metadata(context.Context) (Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return headerMetadata(s.header, s.path), nil
}

func (s *JSONLStorage) LeafID(context.Context) (*string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Leaf()
}

func (s *JSONLStorage) SetLeafID(_ context.Context, leafID *string) error {
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
	if err := s.appendLine(entry); err != nil {
		return err
	}
	s.ix.Add(entry)
	return nil
}

func (s *JSONLStorage) CreateEntryID(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ix.CreateEntryID(), nil
}

func (s *JSONLStorage) AppendEntry(_ context.Context, entry Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLine(entry); err != nil {
		return err
	}
	s.ix.Add(entry)
	return nil
}

func (s *JSONLStorage) GetEntry(_ context.Context, id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Get(id)
}

func (s *JSONLStorage) FindEntries(_ context.Context, entryType string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Find(entryType)
}

func (s *JSONLStorage) Label(_ context.Context, id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Label(id)
}

func (s *JSONLStorage) SessionName(context.Context) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.SessionName()
}

func (s *JSONLStorage) Stats(context.Context) Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Stats()
}

func (s *JSONLStorage) PathToRootOrCompaction(_ context.Context, leafID *string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.PathToRootOrCompaction(leafID)
}

func (s *JSONLStorage) Entries(_ context.Context, opts *CursorOptions) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Slice(opts)
}
