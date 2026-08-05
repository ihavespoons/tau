// Package pi reads a Pi installation so tau can adopt it.
//
// Everything here is read-only with respect to ~/.pi. tau writes its own copy
// under ~/.tau and never edits, moves or deletes the source — the two agents
// must be able to coexist while a user decides, and an import that damages
// what it imported is not one you would run twice.
//
// The formats are the same by construction: tau's session entries, settings,
// credentials and model overlay were all ported from Pi's, so the import is a
// copy plus a format migration rather than a translation.
package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ihavespoons/tau/session"
)

// Snapshot is what a Pi installation contains.
type Snapshot struct {
	// AgentDir is where it was found.
	AgentDir string
	// Sessions found under the sessions directory.
	Sessions []SessionFile
	// SettingsPath is the settings file, empty when there is none.
	SettingsPath string
	// AuthPath is the credential file, empty when there is none.
	AuthPath string
	// AuthProviders are the provider names with stored credentials. Names
	// only — the file itself is never read into anything that gets printed.
	AuthProviders []string
	// ModelsPath is the custom provider overlay, empty when there is none.
	ModelsPath string
	// ModelsStorePath is the cached dynamic catalog, empty when there is none.
	ModelsStorePath string
	// Resources are the resource directories that exist (skills, prompts…).
	Resources []string
	// Problems are things that were found but could not be read. They do not
	// stop an import; they say what will not come across.
	Problems []string
}

// SessionFile is one Pi session on disk.
type SessionFile struct {
	// Path is the source file.
	Path string
	// RelPath is its location under the sessions directory, which tau
	// reproduces verbatim: the cwd encoding is byte-identical between the two.
	RelPath string
	// Cwd is the working directory the session belongs to.
	Cwd string
	// ID is the session id from the header.
	ID string
	// CreatedAt is the header timestamp.
	CreatedAt string
	// Version is the on-disk format version. Anything below the current one is
	// migrated on import.
	Version int
	// Entries counts the records after the header.
	Entries int
	// Name is the user-set display name, if any.
	Name string
}

// resourceDirs are the subdirectories worth reporting on. They are copied
// wholesale rather than parsed: their contents are the user's own files.
var resourceDirs = []string{"skills", "prompts", "themes", "extensions"}

// Inspect reads a Pi agent directory without changing anything.
//
// A missing directory is not an error — it is the answer to "is there a Pi
// installation here", and the caller wants to say so rather than fail.
func Inspect(agentDir string) (*Snapshot, error) {
	if agentDir == "" {
		return nil, errors.New("no Pi directory to inspect")
	}
	info, err := os.Stat(agentDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Snapshot{AgentDir: agentDir}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", agentDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", agentDir)
	}

	snap := &Snapshot{AgentDir: agentDir}

	for _, name := range []struct {
		file string
		dest *string
	}{
		{"settings.json", &snap.SettingsPath},
		{"auth.json", &snap.AuthPath},
		{"models.json", &snap.ModelsPath},
		{"models-store.json", &snap.ModelsStorePath},
	} {
		path := filepath.Join(agentDir, name.file)
		if fileExists(path) {
			*name.dest = path
		}
	}

	if snap.AuthPath != "" {
		providers, err := authProviders(snap.AuthPath)
		if err != nil {
			snap.Problems = append(snap.Problems, "auth.json: "+err.Error())
		} else {
			snap.AuthProviders = providers
		}
	}

	for _, dir := range resourceDirs {
		if dirExists(filepath.Join(agentDir, dir)) {
			snap.Resources = append(snap.Resources, dir)
		}
	}

	sessions, problems := scanSessions(filepath.Join(agentDir, "sessions"))
	snap.Sessions = sessions
	snap.Problems = append(snap.Problems, problems...)

	return snap, nil
}

// authProviders lists the provider names in an auth file.
//
// Names only, deliberately. The values are live credentials, and nothing in
// this package should be able to put one in a log line or an error message.
func authProviders(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("not valid JSON")
	}
	out := make([]string, 0, len(raw))
	for name := range raw {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// scanSessions walks the sessions tree, reading each file's header and
// counting its entries.
func scanSessions(root string) ([]SessionFile, []string) {
	if !dirExists(root) {
		return nil, nil
	}

	var out []SessionFile
	var problems []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			problems = append(problems, path+": "+err.Error())
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = d.Name()
		}

		file, readErr := readSessionFile(path, rel)
		if readErr != nil {
			problems = append(problems, rel+": "+readErr.Error())
			return nil
		}
		out = append(out, file)
		return nil
	})
	if err != nil {
		problems = append(problems, err.Error())
	}

	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, problems
}

// readSessionFile reads one session's header and counts its entries.
//
// It goes through the session package's own migration so the reported version
// and entry count describe what an import would produce, not what the file
// happens to say — a v1 file reports as v1 here but is countable because
// migration ran first.
func readSessionFile(path, rel string) (SessionFile, error) {
	ctx := context.Background()
	storage, err := session.OpenJSONLReadOnly(path)
	if err != nil {
		return SessionFile{}, err
	}
	header := storage.Header()

	file := SessionFile{
		Path:      path,
		RelPath:   rel,
		Cwd:       header.Cwd,
		ID:        header.ID,
		CreatedAt: header.Timestamp,
		Version:   header.Version,
		Entries:   len(storage.Entries(ctx, nil)),
	}
	if storage.Migrated() {
		// The header now reads as current because migration rewrote it in
		// memory; the file on disk is still the old format.
		file.Version = onDiskVersion(path)
	}
	if name, ok := storage.SessionName(ctx); ok {
		file.Name = name
	}
	return file, nil
}

// onDiskVersion reads the version straight off the first line, bypassing the
// in-memory migration, so a report can say what the file actually is.
func onDiskVersion(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	line := data
	if i := strings.IndexByte(string(data), '\n'); i >= 0 {
		line = data[:i]
	}
	var probe struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return 0
	}
	if probe.Version == nil {
		// v1 predates the field entirely.
		return 1
	}
	return *probe.Version
}

// Describe renders a snapshot for a human.
func (s *Snapshot) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Pi installation: %s\n", s.AgentDir)
	if s.Empty() {
		b.WriteString("  nothing to import\n")
		return b.String()
	}

	if len(s.Sessions) > 0 {
		byCwd := map[string]int{}
		needMigration := 0
		for _, f := range s.Sessions {
			byCwd[f.Cwd]++
			if f.Version < session.Version {
				needMigration++
			}
		}
		fmt.Fprintf(&b, "  sessions   %d across %d directories", len(s.Sessions), len(byCwd))
		if needMigration > 0 {
			fmt.Fprintf(&b, " (%d need migrating to v%d)", needMigration, session.Version)
		}
		b.WriteByte('\n')
	}
	if s.SettingsPath != "" {
		b.WriteString("  settings   settings.json\n")
	}
	if len(s.AuthProviders) > 0 {
		fmt.Fprintf(&b, "  auth       %s\n", strings.Join(s.AuthProviders, ", "))
	} else if s.AuthPath != "" {
		b.WriteString("  auth       auth.json\n")
	}
	if s.ModelsPath != "" {
		b.WriteString("  models     models.json\n")
	}
	if s.ModelsStorePath != "" {
		b.WriteString("  catalogs   models-store.json\n")
	}
	if len(s.Resources) > 0 {
		fmt.Fprintf(&b, "  resources  %s\n", strings.Join(s.Resources, ", "))
	}
	for _, p := range s.Problems {
		fmt.Fprintf(&b, "  ! %s\n", p)
	}
	return b.String()
}

// Empty reports whether there is anything to import.
func (s *Snapshot) Empty() bool {
	return len(s.Sessions) == 0 && s.SettingsPath == "" && s.AuthPath == "" &&
		s.ModelsPath == "" && s.ModelsStorePath == "" && len(s.Resources) == 0
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
