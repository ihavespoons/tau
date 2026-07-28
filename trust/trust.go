// Package trust gates project-local configuration — the port of Pi's
// trust-manager.ts and project-trust.ts.
//
// It is a separate package from settings on purpose: the trust decision must
// be made BEFORE project settings load, so it can depend only on the global
// scope. Folding it into settings would invite a cycle, and worse, would
// tempt a future caller to read defaultProjectTrust from the merged view —
// which would let an untrusted project authorize itself.
package trust

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// Default is the fallback applied when no saved decision matches.
type Default string

const (
	// Ask prompts the user (and denies when there is no UI).
	Ask Default = "ask"
	// Always trusts silently.
	Always Default = "always"
	// Never denies silently.
	Never Default = "never"
)

// Decision is a stored trust value: true, false, or absent.
type Decision struct {
	Path    string
	Trusted bool
}

// gatedResources are the project-local entries whose presence requires a trust
// decision (trust-manager.ts:29-37). Their existence is what makes a project
// "interesting" enough to gate.
var gatedResources = []string{
	"settings.json",
	"extensions",
	"skills",
	"prompts",
	"themes",
	"SYSTEM.md",
	"APPEND_SYSTEM.md",
}

// Store persists per-directory trust decisions in trust.json.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore opens the trust store under agentDir.
func NewStore(agentDir string) *Store {
	return &Store{path: filepath.Join(agentDir, "trust.json")}
}

// Path returns the backing file.
func (s *Store) Path() string { return s.path }

type trustFile map[string]*bool

func (s *Store) read() (trustFile, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return trustFile{}, nil
		}
		return nil, fmt.Errorf("trust: read %s: %w", s.path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return trustFile{}, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("trust: parse %s: %w", s.path, err)
	}
	data := trustFile{}
	for k, v := range raw {
		switch string(v) {
		case "true":
			t := true
			data[k] = &t
		case "false":
			f := false
			data[k] = &f
		case "null":
			data[k] = nil
		default:
			return nil, fmt.Errorf("trust: invalid %s: value for %q must be true, false, or null", s.path, k)
		}
	}
	return data, nil
}

func (s *Store) write(data trustFile) error {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("{")
	first := true
	for _, k := range keys {
		v := data[k]
		kb, err := json.Marshal(k)
		if err != nil {
			return err
		}
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString("\n  ")
		b.Write(kb)
		b.WriteString(": ")
		switch {
		case v == nil:
			b.WriteString("null")
		case *v:
			b.WriteString("true")
		default:
			b.WriteString("false")
		}
	}
	if !first {
		b.WriteString("\n")
	}
	b.WriteString("}\n")

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("trust: create %s: %w", filepath.Dir(s.path), err)
	}
	if err := os.WriteFile(s.path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("trust: write %s: %w", s.path, err)
	}
	return nil
}

func (s *Store) withLock(ctx context.Context, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("trust: create %s: %w", filepath.Dir(s.path), err)
	}
	fl := flock.New(s.path + ".lock")
	locked, err := fl.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("trust: lock %s: %w", s.path, err)
	}
	if !locked {
		return fmt.Errorf("trust: could not lock %s", s.path)
	}
	defer func() { _ = fl.Unlock() }()
	return fn()
}

// Lookup returns the nearest stored decision for cwd, walking up ancestors.
//
// The walk is what makes "trust ~/code" cover everything beneath it
// (trust-manager.ts:43-57).
func (s *Store) Lookup(cwd string) (*Decision, error) {
	data, err := s.read()
	if err != nil {
		return nil, err
	}
	return nearest(data, cwd), nil
}

func nearest(data trustFile, cwd string) *Decision {
	current := normalize(cwd)
	for {
		if v, ok := data[current]; ok && v != nil {
			return &Decision{Path: current, Trusted: *v}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// Set records a decision for a directory. A nil decision removes the entry.
func (s *Store) Set(ctx context.Context, dir string, trusted *bool) error {
	return s.SetMany(ctx, map[string]*bool{dir: trusted})
}

// SetMany applies several decisions atomically.
func (s *Store) SetMany(ctx context.Context, updates map[string]*bool) error {
	return s.withLock(ctx, func() error {
		data, err := s.read()
		if err != nil {
			return err
		}
		for dir, trusted := range updates {
			key := normalize(dir)
			if trusted == nil {
				delete(data, key)
			} else {
				data[key] = trusted
			}
		}
		return s.write(data)
	})
}

// normalize canonicalizes a directory for use as a store key.
func normalize(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

// HasGatedResources reports whether cwd carries project-local resources that
// require a trust decision. A project with none is trusted implicitly, because
// there is nothing to gate (trust-manager.ts:184-206).
//
// The user's own ~/.agents/skills is a user resource, not a project one, and
// is excluded even when cwd is the home directory.
func HasGatedResources(cwd, configDirName, homeDir string) bool {
	current := normalize(cwd)
	configDir := filepath.Join(current, configDirName)
	for _, entry := range gatedResources {
		if _, err := os.Stat(filepath.Join(configDir, entry)); err == nil {
			return true
		}
	}

	userSkills := ""
	if homeDir != "" {
		userSkills = filepath.Join(normalize(homeDir), ".agents", "skills")
	}
	for {
		agentSkills := filepath.Join(current, ".agents", "skills")
		if agentSkills != userSkills {
			if _, err := os.Stat(agentSkills); err == nil {
				return true
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

// Request describes a trust decision to make.
type Request struct {
	Cwd string
	// Override short-circuits everything (the --approve/--no-approve flags).
	Override *bool
	// Default is the fallback from GLOBAL settings only.
	Default Default
	// HasUI reports whether a prompt is possible. Without one, an undecided
	// project is denied — trust fails closed.
	HasUI bool
	// ConfigDirName is the project config directory (".tau").
	ConfigDirName string
	// HomeDir is used to exclude the user's global skills directory.
	HomeDir string
}

// Outcome is the result of a trust decision.
type Outcome struct {
	// Trusted reports whether project resources may load.
	Trusted bool
	// NeedsPrompt reports that the caller should ask the user; Trusted is
	// false until they answer.
	NeedsPrompt bool
	// Reason explains the decision, for diagnostics.
	Reason string
}

// Decide resolves project trust without any terminal interaction.
//
// Order (project-trust.ts:46-96):
//  1. an explicit override wins
//  2. no gated resources means nothing to gate — trusted
//  3. a stored decision (nearest ancestor) wins
//  4. the global default: always/never decide, ask continues
//  5. no UI denies; otherwise the caller must prompt
//
// The extension `project_trust` hook sits between 2 and 3 in Pi; that hook
// arrives with the extension system, and this ordering leaves room for it.
func Decide(store *Store, req Request) (Outcome, error) {
	if req.Override != nil {
		return Outcome{Trusted: *req.Override, Reason: "override"}, nil
	}
	configDir := req.ConfigDirName
	if configDir == "" {
		configDir = ".tau"
	}
	if !HasGatedResources(req.Cwd, configDir, req.HomeDir) {
		return Outcome{Trusted: true, Reason: "no project resources require trust"}, nil
	}
	if store != nil {
		decision, err := store.Lookup(req.Cwd)
		if err != nil {
			return Outcome{}, err
		}
		if decision != nil {
			return Outcome{
				Trusted: decision.Trusted,
				Reason:  fmt.Sprintf("saved decision for %s", decision.Path),
			}, nil
		}
	}
	switch req.Default {
	case Always:
		return Outcome{Trusted: true, Reason: "defaultProjectTrust=always"}, nil
	case Never:
		return Outcome{Trusted: false, Reason: "defaultProjectTrust=never"}, nil
	}
	if !req.HasUI {
		// Non-interactive modes cannot ask, so they must not trust.
		return Outcome{Trusted: false, Reason: "no UI to prompt; defaulting to untrusted"}, nil
	}
	return Outcome{Trusted: false, NeedsPrompt: true, Reason: "awaiting user decision"}, nil
}

// Option is a choice offered by the trust prompt.
type Option struct {
	Label   string
	Trusted bool
	// Updates are the store writes to apply when chosen; empty means
	// session-only.
	Updates map[string]*bool
}

// Options builds the prompt choices for cwd (trust-manager.ts:65-95).
func Options(cwd string, includeSessionOnly bool) []Option {
	path := normalize(cwd)
	yes, no := true, false

	opts := []Option{
		{Label: "Trust", Trusted: true, Updates: map[string]*bool{path: &yes}},
	}
	if parent := filepath.Dir(path); parent != path {
		opts = append(opts, Option{
			Label:   fmt.Sprintf("Trust parent folder (%s)", parent),
			Trusted: true,
			Updates: map[string]*bool{parent: &yes, path: nil},
		})
	}
	if includeSessionOnly {
		opts = append(opts, Option{Label: "Trust (this session only)", Trusted: true})
	}
	opts = append(opts, Option{
		Label: "Do not trust", Trusted: false, Updates: map[string]*bool{path: &no},
	})
	if includeSessionOnly {
		opts = append(opts, Option{Label: "Do not trust (this session only)", Trusted: false})
	}
	return opts
}
