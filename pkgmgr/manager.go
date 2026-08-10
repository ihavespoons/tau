package pkgmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Scope is where a package is installed: for the user, or for one project.
type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

// ErrUntrusted is returned when a project-scoped operation is attempted in a
// directory the user has not trusted.
var ErrUntrusted = errors.New("project is not trusted")

// Runner executes an external command. It exists so tests can drive the
// manager without npm or git installed.
type Runner func(ctx context.Context, dir, name string, args ...string) (string, error)

// execRunner runs a command for real, returning its combined output so the
// failure a user sees is the one the tool actually printed.
func execRunner(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Options configures a Manager.
type Options struct {
	// AgentDir is the user-scope root (~/.tau/agent).
	AgentDir string
	// Cwd is the project directory; ProjectDir is its .tau.
	Cwd string
	// ProjectTrusted gates every project-scoped operation. Installing into a
	// repository writes into it and running its packages executes its code,
	// so an untrusted checkout gets neither.
	ProjectTrusted bool
	// Run executes npm and git; nil means run them for real.
	Run Runner
}

// Manager installs packages and resolves the resources they provide.
type Manager struct {
	opts Options
}

// New builds a Manager.
func New(opts Options) *Manager {
	if opts.Run == nil {
		opts.Run = execRunner
	}
	return &Manager{opts: opts}
}

// baseDir is the scope's root directory.
func (m *Manager) baseDir(scope Scope) (string, error) {
	switch scope {
	case ScopeProject:
		if !m.opts.ProjectTrusted {
			return "", ErrUntrusted
		}
		if m.opts.Cwd == "" {
			return "", errors.New("no project directory")
		}
		return filepath.Join(m.opts.Cwd, ".tau"), nil
	case ScopeUser:
		if m.opts.AgentDir == "" {
			return "", errors.New("no agent directory")
		}
		return m.opts.AgentDir, nil
	}
	return "", fmt.Errorf("unknown scope %q", scope)
}

// managedPath joins parts onto a root and refuses to leave it.
//
// The parts come from a package source the user typed, so this is the last
// line between "install github.com/o/r" and a write outside the install root.
// source.go rejects the obvious traversals; this catches whatever it missed.
func managedPath(root string, parts ...string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(append([]string{absRoot}, parts...)...)
	resolved, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if resolved != absRoot && !strings.HasPrefix(resolved, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to use path outside package install root: %s", resolved)
	}
	return resolved, nil
}

// InstallRoot is the directory a source's packages live under.
func (m *Manager) InstallRoot(kind Kind, scope Scope) (string, error) {
	base, err := m.baseDir(scope)
	if err != nil {
		return "", err
	}
	switch kind {
	case KindNPM:
		return filepath.Join(base, "npm"), nil
	case KindGit:
		return filepath.Join(base, "git"), nil
	}
	return "", fmt.Errorf("%s packages are not installed anywhere", kind)
}

// PackagePath is where a source's contents end up on disk.
//
// A local source is not copied — it is used where it lies, so that editing a
// package under development takes effect without reinstalling it.
func (m *Manager) PackagePath(src Source, scope Scope) (string, error) {
	switch src.Kind {
	case KindLocal:
		return m.resolveLocal(src.LocalPath)
	case KindNPM:
		root, err := m.InstallRoot(KindNPM, scope)
		if err != nil {
			return "", err
		}
		return managedPath(root, "node_modules", filepath.FromSlash(src.Name))
	case KindGit:
		root, err := m.InstallRoot(KindGit, scope)
		if err != nil {
			return "", err
		}
		return managedPath(root, src.Host, filepath.FromSlash(src.Path))
	}
	return "", fmt.Errorf("unknown source kind %q", src.Kind)
}

// resolveLocal turns a local source into an absolute path, expanding "~" and
// resolving relatives against the project directory.
func (m *Manager) resolveLocal(p string) (string, error) {
	expanded := p
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		expanded = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded), nil
	}
	base := m.opts.Cwd
	if base == "" {
		var err error
		if base, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	return filepath.Join(base, filepath.FromSlash(expanded)), nil
}

// Install fetches a package and returns where it landed.
func (m *Manager) Install(ctx context.Context, source string, scope Scope) (Source, string, error) {
	src := ParseSource(source)
	path, err := m.PackagePath(src, scope)
	if err != nil {
		return src, "", err
	}

	switch src.Kind {
	case KindLocal:
		if !isDir(path) {
			return src, "", fmt.Errorf("no such directory: %s", path)
		}
		return src, path, nil

	case KindNPM:
		root, err := m.InstallRoot(KindNPM, scope)
		if err != nil {
			return src, "", err
		}
		if err := m.ensureNPMRoot(root); err != nil {
			return src, "", err
		}
		if _, err := m.opts.Run(ctx, root, "npm", "install", "--no-audit", "--no-fund", "--save-exact", src.Spec); err != nil {
			return src, "", err
		}
		return src, path, nil

	case KindGit:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return src, "", err
		}
		if isDir(filepath.Join(path, ".git")) {
			return src, path, m.gitSync(ctx, path, src.Ref)
		}
		if _, err := m.opts.Run(ctx, filepath.Dir(path), "git", "clone", src.Repo, path); err != nil {
			return src, "", err
		}
		if src.Ref != "" {
			if _, err := m.opts.Run(ctx, path, "git", "checkout", src.Ref); err != nil {
				return src, "", err
			}
		}
		return src, path, nil
	}
	return src, "", fmt.Errorf("unknown source kind %q", src.Kind)
}

// ensureNPMRoot makes the install prefix look like a package to npm. Without a
// package.json npm walks upward looking for one and installs into whatever it
// finds, which could be the user's home directory.
func (m *Manager) ensureNPMRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	manifest := filepath.Join(root, "package.json")
	if exists(manifest) {
		return nil
	}
	body := []byte(`{
  "name": "tau-packages",
  "private": true,
  "description": "Packages installed by tau. Managed automatically; edits are not preserved."
}
`)
	return os.WriteFile(manifest, body, 0o644)
}

// gitSync brings an existing checkout up to date.
func (m *Manager) gitSync(ctx context.Context, path, ref string) error {
	if _, err := m.opts.Run(ctx, path, "git", "fetch", "--tags", "origin"); err != nil {
		return err
	}
	if ref != "" {
		_, err := m.opts.Run(ctx, path, "git", "checkout", ref)
		return err
	}
	_, err := m.opts.Run(ctx, path, "git", "pull", "--ff-only")
	return err
}

// Update refetches an installed package. A pinned source is left alone: the
// user asked for that exact version, and quietly moving it would be a lie.
func (m *Manager) Update(ctx context.Context, source string, scope Scope) (bool, error) {
	src := ParseSource(source)
	if src.Pinned {
		return false, nil
	}
	switch src.Kind {
	case KindLocal:
		return false, nil
	case KindNPM:
		root, err := m.InstallRoot(KindNPM, scope)
		if err != nil {
			return false, err
		}
		spec := src.Spec
		if src.Version == "" {
			spec = src.Name + "@latest"
		}
		if _, err := m.opts.Run(ctx, root, "npm", "install", "--no-audit", "--no-fund", spec); err != nil {
			return false, err
		}
		return true, nil
	case KindGit:
		path, err := m.PackagePath(src, scope)
		if err != nil {
			return false, err
		}
		if !isDir(path) {
			return false, fmt.Errorf("not installed: %s", source)
		}
		return true, m.gitSync(ctx, path, src.Ref)
	}
	return false, fmt.Errorf("unknown source kind %q", src.Kind)
}

// Remove deletes an installed package. A local source is never deleted — tau
// did not put it there, and removing it means removing the user's own work.
func (m *Manager) Remove(ctx context.Context, source string, scope Scope) error {
	src := ParseSource(source)
	switch src.Kind {
	case KindLocal:
		return nil
	case KindNPM:
		root, err := m.InstallRoot(KindNPM, scope)
		if err != nil {
			return err
		}
		if !exists(filepath.Join(root, "package.json")) {
			return nil
		}
		_, err = m.opts.Run(ctx, root, "npm", "uninstall", "--no-audit", "--no-fund", src.Name)
		return err
	case KindGit:
		root, err := m.InstallRoot(KindGit, scope)
		if err != nil {
			return err
		}
		path, err := m.PackagePath(src, scope)
		if err != nil {
			return err
		}
		if !isDir(path) {
			return nil
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		pruneEmptyDirs(filepath.Dir(path), root)
		return nil
	}
	return fmt.Errorf("unknown source kind %q", src.Kind)
}

// pruneEmptyDirs removes now-empty parents of a deleted checkout, so removing
// the only package from a host does not leave its directory behind.
//
// It stops at the install root and never removes it: the root is tau's, not the
// package's, and removing the last package must not take .tau with it.
func pruneEmptyDirs(dir, stop string) {
	if abs, err := filepath.Abs(stop); err == nil {
		stop = abs
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for dir != "" && dir != stop && dir != filepath.Dir(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// Entry is a package as named in settings: either a bare source string or an
// object that also says which of the package's resources to use.
type Entry struct {
	Source string `json:"source"`
	// Autoload false means nothing is loaded unless a pattern names it.
	Autoload *bool `json:"autoload,omitempty"`

	Extensions []string `json:"extensions,omitempty"`
	Skills     []string `json:"skills,omitempty"`
	Prompts    []string `json:"prompts,omitempty"`
	Themes     []string `json:"themes,omitempty"`
}

// Patterns returns the entry's filter for one resource type, and whether the
// entry mentioned that type at all.
func (e Entry) Patterns(t ResourceType) ([]string, bool) {
	switch t {
	case TypeExtensions:
		return e.Extensions, e.Extensions != nil
	case TypeSkills:
		return e.Skills, e.Skills != nil
	case TypePrompts:
		return e.Prompts, e.Prompts != nil
	case TypeThemes:
		return e.Themes, e.Themes != nil
	}
	return nil, false
}

// ParseEntry reads a settings package entry in either form.
func ParseEntry(raw json.RawMessage) (Entry, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return Entry{}, err
		}
		return Entry{Source: s}, nil
	}
	var e Entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return Entry{}, err
	}
	if e.Source == "" {
		return Entry{}, errors.New("package entry has no source")
	}
	return e, nil
}

// ParseEntries reads a settings packages list, returning the entries it could
// parse and a warning for each it could not. One malformed entry must not cost
// the user every other package they configured.
func ParseEntries(raws []json.RawMessage) ([]Entry, []string) {
	var entries []Entry
	var warnings []string
	for _, raw := range raws {
		e, err := ParseEntry(raw)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("package entry ignored: %v", err))
			continue
		}
		entries = append(entries, e)
	}
	return entries, warnings
}

// Resource is one file a package provides.
type Resource struct {
	Path     string
	Type     ResourceType
	Enabled  bool
	Package  string // the source string, for diagnostics
	Root     string // the package directory the path is relative to
	Scope    Scope
	Priority int // lower wins on a name collision
}

// Resolution is everything the configured packages provide.
type Resolution struct {
	Resources []Resource
	Warnings  []string
}

// Enabled returns the enabled paths of one type, in resolution order.
func (r Resolution) Enabled(t ResourceType) []string {
	var out []string
	for _, res := range r.Resources {
		if res.Type == t && res.Enabled {
			out = append(out, res.Path)
		}
	}
	return out
}

// Resolve turns configured package entries into resource paths.
//
// Entries are resolved in the order given, project scope before user scope, so
// that a project's choice of package wins a collision — the same precedence
// the rest of tau's settings use.
func (m *Manager) Resolve(entries []Entry, scope Scope) Resolution {
	var out Resolution
	priority := 0
	if scope == ScopeUser {
		priority = 2
	}

	for _, entry := range entries {
		src := ParseSource(entry.Source)
		root, err := m.PackagePath(src, scope)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("package %s: %v", entry.Source, err))
			continue
		}
		if !isDir(root) {
			out.Warnings = append(out.Warnings,
				fmt.Sprintf("package %s is not installed (run `tau install %s`)", entry.Source, entry.Source))
			continue
		}
		for _, t := range ResourceTypes {
			for _, r := range m.resolveType(entry, src, root, t, scope, priority) {
				out.Resources = append(out.Resources, r)
			}
		}
	}
	return out
}

// resolveType applies one entry's filter for one resource type.
func (m *Manager) resolveType(entry Entry, src Source, root string, t ResourceType, scope Scope, priority int) []Resource {
	all, enabledByPackage := CollectFiles(root, t)
	patterns, declared := entry.Patterns(t)

	var verdict map[string]bool
	switch {
	case entry.Autoload != nil && !*entry.Autoload:
		// Nothing loads by default; only what a pattern names appears, which
		// is how a user takes one skill from a package they otherwise ignore.
		verdict = applyAutoloadDisabledPatterns(all, patterns, root)
		return collectVerdict(all, verdict, root, entry.Source, t, scope, priority, false)
	case declared && len(patterns) == 0:
		// An explicitly empty list is a deliberate "none of these".
		return nil
	case declared:
		verdict = applyPatterns(all, patterns, root)
	default:
		verdict = enabledByPackage
	}
	return collectVerdict(all, verdict, root, entry.Source, t, scope, priority, true)
}

// collectVerdict turns a per-path decision into resources. When includeAll is
// false, paths no pattern spoke about are left out entirely rather than being
// included as disabled.
func collectVerdict(all []string, verdict map[string]bool, root, source string, t ResourceType, scope Scope, priority int, includeAll bool) []Resource {
	var out []Resource
	for _, p := range all {
		enabled, mentioned := verdict[p]
		if !mentioned && !includeAll {
			continue
		}
		out = append(out, Resource{
			Path: p, Type: t, Enabled: enabled,
			Package: source, Root: root, Scope: scope, Priority: priority,
		})
	}
	return out
}

// Installed describes a package found on disk.
type Installed struct {
	Source  string
	Kind    Kind
	Scope   Scope
	Path    string
	Name    string
	Version string
}

// List returns the packages installed in a scope, whether or not settings
// mention them — so a package installed and then unconfigured is still visible
// and can still be removed.
func (m *Manager) List(scope Scope) ([]Installed, error) {
	var out []Installed

	if root, err := m.InstallRoot(KindNPM, scope); err == nil {
		modules := filepath.Join(root, "node_modules")
		for _, dir := range npmPackageDirs(modules) {
			name, version := PackageIdent(dir)
			if name == "" {
				name, _ = filepath.Rel(modules, dir)
				name = toSlash(name)
			}
			out = append(out, Installed{
				Source: "npm:" + name, Kind: KindNPM, Scope: scope,
				Path: dir, Name: name, Version: version,
			})
		}
	}

	if root, err := m.InstallRoot(KindGit, scope); err == nil {
		for _, dir := range gitCheckoutDirs(root) {
			rel, relErr := filepath.Rel(root, dir)
			if relErr != nil {
				continue
			}
			name, version := PackageIdent(dir)
			out = append(out, Installed{
				Source: "git:" + toSlash(rel), Kind: KindGit, Scope: scope,
				Path: dir, Name: name, Version: version,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out, nil
}

// npmPackageDirs lists installed packages, descending one level into scopes so
// that "@scope/name" is reported whole rather than as a directory called
// "@scope".
func npmPackageDirs(modules string) []string {
	entries, err := os.ReadDir(modules)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(modules, name)
		if !isDir(p) {
			continue
		}
		if strings.HasPrefix(name, "@") {
			scoped, err := os.ReadDir(p)
			if err != nil {
				continue
			}
			for _, s := range scoped {
				if sp := filepath.Join(p, s.Name()); isDir(sp) {
					out = append(out, sp)
				}
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

// gitCheckoutDirs finds clones under the git root, which is nested by host and
// repository path and so has no fixed depth.
func gitCheckoutDirs(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if isDir(filepath.Join(path, ".git")) {
			out = append(out, path)
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

// FilterPaths applies a user's enable/disable patterns to already-discovered
// resource paths.
//
// This is the half of per-resource control that has nothing to do with
// packages: skills and prompts found in ~/.tau or the project can be switched
// off the same way, by naming them in settings with a "-" or "!".
func FilterPaths(paths, patterns []string, baseDir string) []string {
	if len(patterns) == 0 {
		return paths
	}
	var out []string
	for _, p := range paths {
		if isEnabledByOverrides(p, patterns, baseDir) {
			out = append(out, p)
		}
	}
	return out
}

// SplitEntries separates plain resource paths from enable/disable patterns, so
// a settings list can hold both.
func SplitEntries(entries []string) (paths, patterns []string) {
	return splitPatternEntries(entries)
}
