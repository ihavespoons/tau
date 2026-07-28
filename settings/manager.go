package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/ihavespoons/tau/config"
)

// LoadError records a settings file that failed to load. A bad file degrades
// to empty rather than aborting startup (Pi's tryLoadFromStorage), so the
// error is surfaced here instead of returned.
type LoadError struct {
	Scope Scope
	Path  string
	Err   error
}

func (e LoadError) Error() string {
	return fmt.Sprintf("settings: %s scope (%s): %v", e.Scope, e.Path, e.Err)
}

func (e LoadError) Unwrap() error { return e.Err }

// Manager owns the two settings scopes and their merged view.
//
// Reads are served from memory. Writes are read-modify-write under an advisory
// file lock, touching only the key being set so unknown keys survive.
type Manager struct {
	mu sync.RWMutex

	globalPath  string
	projectPath string

	global  map[string]json.RawMessage
	project map[string]json.RawMessage
	merged  map[string]json.RawMessage

	projectTrusted bool
	errs           []LoadError
}

// Options configures Load.
type Options struct {
	// Cwd is the project directory; "" uses the process working directory.
	Cwd string
	// AgentDir overrides the global settings directory (tests).
	AgentDir string
	// ProjectTrusted gates the project scope. An untrusted project's
	// settings.json is not read at all.
	ProjectTrusted bool
}

// Load reads both scopes and computes the merged view. It returns a Manager
// even when a file is malformed; inspect Errors for per-scope failures.
func Load(opts Options) (*Manager, error) {
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("settings: resolve cwd: %w", err)
		}
	}
	globalPath := config.SettingsPath()
	if opts.AgentDir != "" {
		globalPath = filepath.Join(opts.AgentDir, "settings.json")
	}

	m := &Manager{
		globalPath:     globalPath,
		projectPath:    config.ProjectSettingsPath(cwd),
		projectTrusted: opts.ProjectTrusted,
		global:         map[string]json.RawMessage{},
		project:        map[string]json.RawMessage{},
	}

	if raw, err := readRaw(m.globalPath); err != nil {
		m.errs = append(m.errs, LoadError{Scope: Global, Path: m.globalPath, Err: err})
	} else {
		m.global = migrate(raw)
	}

	// An untrusted project contributes nothing — this is the trust gate's
	// entire purpose, so the file is never even read.
	if m.projectTrusted {
		if raw, err := readRaw(m.projectPath); err != nil {
			m.errs = append(m.errs, LoadError{Scope: Project, Path: m.projectPath, Err: err})
		} else {
			m.project = migrate(raw)
		}
	}

	m.merged = merge(m.global, m.project)
	return m, nil
}

// Errors returns load failures encountered for either scope.
func (m *Manager) Errors() []LoadError {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]LoadError{}, m.errs...)
}

// ProjectTrusted reports whether the project scope was loaded.
func (m *Manager) ProjectTrusted() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.projectTrusted
}

// Path returns the file backing a scope.
func (m *Manager) Path(scope Scope) string {
	if scope == Global {
		return m.globalPath
	}
	return m.projectPath
}

// Settings returns the merged, typed settings.
func (m *Manager) Settings() (Settings, error) { return m.decode(m.rawMerged()) }

// Scoped returns one scope's typed settings.
func (m *Manager) Scoped(scope Scope) (Settings, error) { return m.decode(m.rawScope(scope)) }

func (m *Manager) rawMerged() map[string]json.RawMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneRaw(m.merged)
}

func (m *Manager) rawScope(scope Scope) map[string]json.RawMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if scope == Global {
		return cloneRaw(m.global)
	}
	return cloneRaw(m.project)
}

func (m *Manager) decode(raw map[string]json.RawMessage) (Settings, error) {
	b, err := marshalStable(raw)
	if err != nil {
		return Settings{}, err
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return Settings{}, fmt.Errorf("settings: decode: %w", err)
	}
	return s, nil
}

// Origin reports which scope supplied a top-level key.
func (m *Manager) Origin(key string) (Scope, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.project[key]; ok {
		return Project, true
	}
	if _, ok := m.global[key]; ok {
		return Global, true
	}
	return "", false
}

// Get returns the raw JSON for a merged key. Dotted paths reach one level into
// a nested object ("compaction.enabled").
func (m *Manager) Get(key string) (json.RawMessage, bool) {
	raw := m.rawMerged()
	return lookup(raw, key)
}

func lookup(raw map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	top, nested, isNested := strings.Cut(key, ".")
	v, ok := raw[top]
	if !ok || !isNested {
		return v, ok
	}
	obj, isObj := asObject(v)
	if !isObj {
		return nil, false
	}
	nv, ok := obj[nested]
	return nv, ok
}

// Set writes a value into one scope, then recomputes the merged view.
//
// Only the addressed key is written: the file is re-read under lock and every
// other key — including ones tau does not model — is carried across untouched.
// A dotted key merges into the existing nested object rather than replacing it,
// matching Pi's modifiedNestedFields behavior.
//
// Writing to an untrusted project scope is refused.
func (m *Manager) Set(ctx context.Context, scope Scope, key string, value any) error {
	if key == "" {
		return fmt.Errorf("settings: empty key")
	}
	m.mu.RLock()
	trusted := m.projectTrusted
	m.mu.RUnlock()
	if scope == Project && !trusted {
		return fmt.Errorf("settings: project is not trusted; refusing to write %s", m.projectPath)
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("settings: encode %s: %w", key, err)
	}

	path := m.Path(scope)
	if err := withFileLock(ctx, path, func() error {
		current, rerr := readRaw(path)
		if rerr != nil {
			return rerr
		}
		if current == nil {
			current = map[string]json.RawMessage{}
		}
		if err := applyKey(current, key, encoded); err != nil {
			return err
		}
		return writeRaw(path, current)
	}); err != nil {
		return err
	}

	// Re-read so in-memory state reflects what is actually on disk, including
	// any concurrent change another process made.
	updated, err := readRaw(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if scope == Global {
		m.global = migrate(updated)
	} else {
		m.project = migrate(updated)
	}
	m.merged = merge(m.global, m.project)
	return nil
}

// applyKey sets a top-level or one-level-nested key in place.
func applyKey(dst map[string]json.RawMessage, key string, encoded json.RawMessage) error {
	top, nested, isNested := strings.Cut(key, ".")
	if !isNested {
		dst[top] = encoded
		return nil
	}
	if strings.Contains(nested, ".") {
		return fmt.Errorf("settings: %q nests deeper than one level", key)
	}
	obj := map[string]json.RawMessage{}
	if existing, ok := dst[top]; ok {
		if parsed, isObj := asObject(existing); isObj {
			obj = parsed
		}
	}
	obj[nested] = encoded
	merged, err := marshalStable(obj)
	if err != nil {
		return err
	}
	dst[top] = merged
	return nil
}

// Unset removes a key from a scope.
func (m *Manager) Unset(ctx context.Context, scope Scope, key string) error {
	m.mu.RLock()
	trusted := m.projectTrusted
	m.mu.RUnlock()
	if scope == Project && !trusted {
		return fmt.Errorf("settings: project is not trusted; refusing to write %s", m.projectPath)
	}

	path := m.Path(scope)
	if err := withFileLock(ctx, path, func() error {
		current, rerr := readRaw(path)
		if rerr != nil {
			return rerr
		}
		if current == nil {
			return nil
		}
		top, nested, isNested := strings.Cut(key, ".")
		if !isNested {
			delete(current, top)
		} else if existing, ok := current[top]; ok {
			if obj, isObj := asObject(existing); isObj {
				delete(obj, nested)
				merged, err := marshalStable(obj)
				if err != nil {
					return err
				}
				current[top] = merged
			}
		}
		return writeRaw(path, current)
	}); err != nil {
		return err
	}

	updated, err := readRaw(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if scope == Global {
		m.global = migrate(updated)
	} else {
		m.project = migrate(updated)
	}
	m.merged = merge(m.global, m.project)
	return nil
}

// Reload re-reads both scopes from disk.
func (m *Manager) Reload() error {
	g, gerr := readRaw(m.globalPath)
	if gerr != nil {
		return gerr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.global = migrate(g)
	if m.projectTrusted {
		p, perr := readRaw(m.projectPath)
		if perr != nil {
			return perr
		}
		m.project = migrate(p)
	} else {
		m.project = map[string]json.RawMessage{}
	}
	m.merged = merge(m.global, m.project)
	return nil
}

func cloneRaw(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func readRaw(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, fmt.Errorf("settings: read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("settings: parse %s: %w", path, err)
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	return raw, nil
}

func writeRaw(path string, raw map[string]json.RawMessage) error {
	compact, err := marshalStable(raw)
	if err != nil {
		return err
	}
	pretty, err := indent(compact)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("settings: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, append(pretty, '\n'), 0o644); err != nil {
		return fmt.Errorf("settings: write %s: %w", path, err)
	}
	return nil
}

// withFileLock serializes writers across processes, mirroring ai/auth's
// FileStore locking.
func withFileLock(ctx context.Context, path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("settings: create %s: %w", filepath.Dir(path), err)
	}
	fl := flock.New(path + ".lock")
	locked, err := fl.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("settings: lock %s: %w", path, err)
	}
	if !locked {
		return fmt.Errorf("settings: could not lock %s", path)
	}
	defer func() { _ = fl.Unlock() }()
	return fn()
}
