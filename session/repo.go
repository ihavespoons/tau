package session

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// ForkOptions selects how much of a source session a fork copies.
type ForkOptions struct {
	// EntryID bounds the copy. Empty copies the whole session.
	EntryID string
	// Position "before" (default) cuts just before a user message so the fork
	// can replay it differently; "at" keeps the entry itself.
	Position string
	// ID overrides the new session's id.
	ID string
}

// Repo stores and enumerates sessions.
type Repo interface {
	Create(ctx context.Context, opts CreateSessionOptions) (*Session, error)
	Open(ctx context.Context, meta Metadata) (*Session, error)
	List(ctx context.Context, cwd string) ([]Metadata, error)
	Delete(ctx context.Context, meta Metadata) error
	Fork(ctx context.Context, source Metadata, opts CreateSessionOptions, fork ForkOptions) (*Session, error)
}

// CreateSessionOptions configures a new session.
type CreateSessionOptions struct {
	Cwd               string
	ID                string
	ParentSessionPath string
	Metadata          map[string]any
}

// JSONLRepo stores sessions as JSONL files under a root directory, laid out
// per working directory.
type JSONLRepo struct {
	root string
}

var _ Repo = (*JSONLRepo)(nil)

// NewJSONLRepo creates a repo rooted at the given sessions directory.
func NewJSONLRepo(root string) *JSONLRepo { return &JSONLRepo{root: root} }

// Root is the sessions directory.
func (r *JSONLRepo) Root() string { return r.root }

// EncodeCwd renders a working directory as its session subdirectory name.
// Byte-identical to Pi: strip a leading separator, replace `/`, `\`, and `:`
// with `-`, then wrap in double dashes.
//
//	/Users/ben/Code/tau  ->  --Users-ben-Code-tau--
func EncodeCwd(cwd string) string {
	// Exactly one leading separator is stripped, matching Pi's `^[/\\]`.
	trimmed := cwd
	if len(trimmed) > 0 && (trimmed[0] == '/' || trimmed[0] == '\\') {
		trimmed = trimmed[1:]
	}
	replaced := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '-'
		}
		return r
	}, trimmed)
	return "--" + replaced + "--"
}

// sessionFileName renders a session's file name: the creation timestamp with
// `:` and `.` replaced by `-`, then the session id.
func sessionFileName(timestamp, sessionID string) string {
	safe := strings.Map(func(r rune) rune {
		if r == ':' || r == '.' {
			return '-'
		}
		return r
	}, timestamp)
	return safe + "_" + sessionID + ".jsonl"
}

func (r *JSONLRepo) dirFor(cwd string) string {
	return filepath.Join(r.root, EncodeCwd(cwd))
}

func (r *JSONLRepo) Create(_ context.Context, opts CreateSessionOptions) (*Session, error) {
	id := opts.ID
	if id == "" {
		id = NewID()
	}
	createdAt := Now()
	path := filepath.Join(r.dirFor(opts.Cwd), sessionFileName(createdAt, id))
	storage, err := CreateJSONL(path, CreateOptions{
		Cwd:               opts.Cwd,
		SessionID:         id,
		ParentSessionPath: opts.ParentSessionPath,
		Metadata:          opts.Metadata,
	})
	if err != nil {
		return nil, err
	}
	return NewSession(storage), nil
}

func (r *JSONLRepo) Open(_ context.Context, meta Metadata) (*Session, error) {
	storage, err := OpenJSONL(meta.Path)
	if err != nil {
		return nil, err
	}
	return NewSession(storage), nil
}

// List returns sessions for one working directory, or every session when cwd
// is empty. Newest first. Files that are not valid sessions are skipped.
func (r *JSONLRepo) List(_ context.Context, cwd string) ([]Metadata, error) {
	var dirs []string
	if cwd != "" {
		dirs = []string{r.dirFor(cwd)}
	} else {
		entries, err := os.ReadDir(r.root)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, errorf(CodeStorage, err, "failed to list sessions root %s", r.root)
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(r.root, e.Name()))
			}
		}
	}

	var out []Metadata
	for _, dir := range dirs {
		files, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, errorf(CodeStorage, err, "failed to list sessions in %s", dir)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			meta, err := LoadJSONLMetadata(filepath.Join(dir, f.Name()))
			if err != nil {
				// A stray or half-written file should not break listing.
				if IsCode(err, CodeInvalidSession) || IsCode(err, CodeNotFound) {
					continue
				}
				return nil, err
			}
			out = append(out, meta)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

func (r *JSONLRepo) Delete(_ context.Context, meta Metadata) error {
	if err := os.Remove(meta.Path); err != nil && !os.IsNotExist(err) {
		return errorf(CodeStorage, err, "failed to delete session %s", meta.Path)
	}
	return nil
}

// Fork copies a prefix of one session into a new file whose header points back
// at the source. Copied entries keep their original bytes.
func (r *JSONLRepo) Fork(ctx context.Context, source Metadata, opts CreateSessionOptions, fork ForkOptions) (*Session, error) {
	src, err := r.Open(ctx, source)
	if err != nil {
		return nil, err
	}
	entries, err := EntriesToFork(ctx, src.Storage(), fork)
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

// EntriesToFork selects the entries a fork copies.
//
// Exported for the same reason Index is: the rules here — "at" keeps the target
// entry, "before" requires a user message and takes its parent, and either way
// the walk stops at a compaction — are session semantics, not file semantics. A
// second backend restating them would be a second thing to get right.
func EntriesToFork(ctx context.Context, storage Storage, fork ForkOptions) ([]Entry, error) {
	if fork.EntryID == "" {
		return storage.Entries(ctx, nil), nil
	}
	target, ok := storage.GetEntry(ctx, fork.EntryID)
	if !ok {
		return nil, errorf(CodeInvalidFork, nil, "entry %s not found", fork.EntryID)
	}
	var leafID *string
	if fork.Position == "at" {
		id := target.Base().ID
		leafID = &id
	} else {
		msg, isMessage := target.(*MessageEntry)
		if !isMessage || msg.Message == nil || msg.Message.Role() != "user" {
			return nil, errorf(CodeInvalidFork, nil, "entry %s is not a user message", fork.EntryID)
		}
		leafID = target.Base().ParentID
	}
	return storage.PathToRootOrCompaction(ctx, leafID)
}

// NewID mints a session id: a UUIDv7, so ids sort by creation time.
func NewID() string {
	if u, err := uuid.NewV7(); err == nil {
		return u.String()
	}
	return uuid.NewString()
}
