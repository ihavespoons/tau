// Package sqlite stores sessions in a SQLite database rather than one JSONL
// file per session.
//
// It exists for the cases the file layout is bad at: a server holding thousands
// of sessions, a listing that should not mean opening every file to read its
// first line, and queries from outside tau entirely. It is a satellite — the
// binary does not import it, so nothing here is linked into tau unless a
// program asks for it.
//
// The driver is modernc.org/sqlite, which is pure Go: tau builds with
// CGO_ENABLED=0 and a cgo-backed driver would end that.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"

	"github.com/ihavespoons/tau/session"
)

// Repo is a session.Repo backed by one database file.
type Repo struct {
	db   *sql.DB
	path string
}

var _ session.Repo = (*Repo)(nil)

// schema is applied on every open. Every statement is idempotent, so opening an
// existing database is the same code path as creating one.
//
// Entries are stored as the bytes they were written as. Decoding them back
// through session.UnmarshalEntry is what keeps this backend and the JSONL one
// from ever disagreeing about what an entry means — and it is what makes an
// entry written by a newer tau survive a round trip through an older one.
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    cwd         TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    parent_path TEXT NOT NULL DEFAULT '',
    metadata    TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS entries (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    entry_id   TEXT NOT NULL,
    raw        BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS entries_by_session ON entries(session_id, seq);
CREATE INDEX IF NOT EXISTS sessions_by_cwd ON sessions(cwd, created_at);
`

// Open opens or creates the database at path.
func Open(path string) (*Repo, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// WAL lets a reader — a listing, a viewer, another tau — run while a
	// session is being appended to, which is the whole reason to prefer this
	// backend over a file per session.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema in %s: %w", path, err)
	}
	return &Repo{db: db, path: path}, nil
}

// Close releases the database.
func (r *Repo) Close() error { return r.db.Close() }

// DB exposes the handle for a caller that wants to query sessions itself. That
// is the point of this backend, so it is not hidden.
func (r *Repo) DB() *sql.DB { return r.db }

// sessionPath is the identity a Metadata carries.
//
// There is no file, but Path is what pickers show and what callers hold onto to
// reopen a session, so it has to be unique and readable. The database and the
// session id together are both.
func (r *Repo) sessionPath(id string) string {
	return "sqlite:" + r.path + "#" + id
}

func (r *Repo) Create(ctx context.Context, opts session.CreateSessionOptions) (*session.Session, error) {
	id := opts.ID
	if id == "" {
		id = session.NewID()
	}
	createdAt := session.Now()

	meta := []byte("{}")
	if len(opts.Metadata) > 0 {
		encoded, err := json.Marshal(opts.Metadata)
		if err != nil {
			return nil, fmt.Errorf("encode session metadata: %w", err)
		}
		meta = encoded
	}

	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO sessions (id, cwd, created_at, parent_path, metadata) VALUES (?, ?, ?, ?, ?)`,
		id, opts.Cwd, createdAt, opts.ParentSessionPath, string(meta)); err != nil {
		return nil, fmt.Errorf("create session %s: %w", id, err)
	}

	return session.NewSession(&Storage{
		db: r.db,
		meta: session.Metadata{
			ID: id, CreatedAt: createdAt, Cwd: opts.Cwd,
			Path:              r.sessionPath(id),
			ParentSessionPath: opts.ParentSessionPath,
			Metadata:          opts.Metadata,
		},
		ix: session.NewIndex(),
	}), nil
}

func (r *Repo) Open(ctx context.Context, meta session.Metadata) (*session.Session, error) {
	id := meta.ID
	if id == "" {
		id = idFromPath(meta.Path)
	}
	loaded, err := r.load(ctx, id)
	if err != nil {
		return nil, err
	}
	return session.NewSession(loaded), nil
}

// load reads a session and replays its entries into a fresh index.
//
// The replay is deliberate. SQL could answer "what is the current leaf" or
// "walk back to the compaction" directly, but those answers have subtleties —
// label last-write-wins, a leaf entry that moves the head, a path that stops
// early — and a second implementation of them in SQL would be a second thing to
// keep correct. The database is the durable log; session.Index is the meaning.
func (r *Repo) load(ctx context.Context, id string) (*Storage, error) {
	var (
		cwd, createdAt, parent, meta string
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT cwd, created_at, parent_path, metadata FROM sessions WHERE id = ?`, id).
		Scan(&cwd, &createdAt, &parent, &meta)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("session %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("read session %s: %w", id, err)
	}

	s := &Storage{
		db: r.db,
		meta: session.Metadata{
			ID: id, CreatedAt: createdAt, Cwd: cwd,
			Path:              r.sessionPath(id),
			ParentSessionPath: parent,
			Metadata:          decodeMetadata(meta),
		},
		ix: session.NewIndex(),
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT seq, raw FROM entries WHERE session_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("read entries of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var seq int64
		var raw []byte
		if err := rows.Scan(&seq, &raw); err != nil {
			return nil, fmt.Errorf("read entry of %s: %w", id, err)
		}
		entry, err := session.UnmarshalEntry(raw)
		if err != nil {
			s.soft = append(s.soft, fmt.Errorf("session %s entry %d: %w", id, seq, err))
			// An entry of a type this build does not know still decodes
			// opaquely, keeping its bytes and its tree links, so it is kept —
			// dropping it would sever the path between the entries either side.
			// One that did not decode at all is skipped: the rest of the
			// conversation is still there. Same rule as the JSONL backend.
			if entry == nil {
				continue
			}
		}
		s.ix.AddLoaded(entry)
		s.lastSeq = seq
	}
	return s, rows.Err()
}

// List returns sessions for one working directory, or all of them when cwd is
// empty. Newest first, matching the JSONL repo.
//
// This is one query. The file backend has to open every session to answer it,
// which is the cost this backend exists to remove.
func (r *Repo) List(ctx context.Context, cwd string) ([]session.Metadata, error) {
	query := `SELECT id, cwd, created_at, parent_path, metadata FROM sessions`
	args := []any{}
	if cwd != "" {
		query += ` WHERE cwd = ?`
		args = append(args, cwd)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []session.Metadata
	for rows.Next() {
		var id, dir, createdAt, parent, meta string
		if err := rows.Scan(&id, &dir, &createdAt, &parent, &meta); err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		out = append(out, session.Metadata{
			ID: id, CreatedAt: createdAt, Cwd: dir,
			Path:              r.sessionPath(id),
			ParentSessionPath: parent,
			Metadata:          decodeMetadata(meta),
		})
	}
	return out, rows.Err()
}

func (r *Repo) Delete(ctx context.Context, meta session.Metadata) error {
	id := meta.ID
	if id == "" {
		id = idFromPath(meta.Path)
	}
	// Entries go with it through the foreign key, which is why the pragma is
	// set on every connection rather than assumed.
	if _, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session %s: %w", id, err)
	}
	return nil
}

// Fork copies a prefix of one session into a new one whose row points back at
// the source. Copied entries keep their original bytes.
func (r *Repo) Fork(ctx context.Context, source session.Metadata, opts session.CreateSessionOptions, fork session.ForkOptions) (*session.Session, error) {
	src, err := r.Open(ctx, source)
	if err != nil {
		return nil, err
	}
	entries, err := session.EntriesToFork(ctx, src.Storage(), fork)
	if err != nil {
		return nil, err
	}

	if opts.ParentSessionPath == "" {
		opts.ParentSessionPath = source.Path
	}
	if opts.Metadata == nil {
		opts.Metadata = source.Metadata
	}
	if opts.Cwd == "" {
		opts.Cwd = source.Cwd
	}

	dst, err := r.Create(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := dst.Storage().AppendEntry(ctx, e); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

func decodeMetadata(s string) map[string]any {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// idFromPath recovers a session id from the URI Path form.
func idFromPath(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '#' {
			return path[i+1:]
		}
	}
	return ""
}

// Storage is one session's append-only log inside the database.
//
// Single-writer per session, the same contract the JSONL backend has. What is
// different is that two sessions can be appended to at once without either
// holding a lock the other wants.
type Storage struct {
	mu      sync.RWMutex
	db      *sql.DB
	meta    session.Metadata
	ix      *session.Index
	lastSeq int64
	// soft collects entries that did not decode on load. They are lost to this
	// process but still in the database, which is where a fix would look.
	soft []error
}

var _ session.Storage = (*Storage)(nil)

// SoftErrors reports entries that could not be decoded when the session was
// opened.
func (s *Storage) SoftErrors() []error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]error(nil), s.soft...)
}

func (s *Storage) Metadata(context.Context) (session.Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta, nil
}

func (s *Storage) LeafID(context.Context) (*string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Leaf()
}

func (s *Storage) SetLeafID(ctx context.Context, leafID *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if leafID != nil {
		if _, ok := s.ix.Get(*leafID); !ok {
			return fmt.Errorf("entry %s not found", *leafID)
		}
	}
	entry := &session.LeafEntry{
		EntryBase: session.EntryBase{
			ID: s.ix.CreateEntryID(), ParentID: s.ix.Head(), Timestamp: session.Now(),
		},
		TargetID: leafID,
	}
	return s.append(ctx, entry)
}

func (s *Storage) CreateEntryID(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ix.CreateEntryID(), nil
}

func (s *Storage) AppendEntry(ctx context.Context, entry session.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(ctx, entry)
}

// append writes an entry and indexes it, in that order: an entry the index
// knows about but the database does not would come back missing on the next
// open, which is worse than a write that failed and said so.
func (s *Storage) append(ctx context.Context, entry session.Entry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode entry: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO entries (session_id, entry_id, raw) VALUES (?, ?, ?)`,
		s.meta.ID, entry.Base().ID, raw)
	if err != nil {
		return fmt.Errorf("append entry to %s: %w", s.meta.ID, err)
	}
	if seq, err := res.LastInsertId(); err == nil {
		s.lastSeq = seq
	}
	s.ix.Add(entry)
	return nil
}

func (s *Storage) GetEntry(_ context.Context, id string) (session.Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Get(id)
}

func (s *Storage) FindEntries(_ context.Context, entryType string) []session.Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Find(entryType)
}

func (s *Storage) Label(_ context.Context, id string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Label(id)
}

func (s *Storage) SessionName(context.Context) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.SessionName()
}

func (s *Storage) Stats(context.Context) session.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Stats()
}

func (s *Storage) PathToRootOrCompaction(_ context.Context, leafID *string) ([]session.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.PathToRootOrCompaction(leafID)
}

func (s *Storage) Entries(_ context.Context, opts *session.CursorOptions) []session.Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ix.Slice(opts)
}
