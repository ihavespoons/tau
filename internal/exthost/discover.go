package exthost

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Kind is how an extension is launched.
type Kind string

const (
	// KindNative is an executable that speaks the wire protocol itself, in
	// whatever language it was written.
	KindNative Kind = "native"
	// KindTypeScript is a .ts/.js file or a package directory, launched under
	// the host shim.
	KindTypeScript Kind = "typescript"
)

// ShimPackage is the npm package that runs Pi-style TypeScript extensions.
const ShimPackage = "@ihavespoons/tau-pi-host"

// ShimCommand is the executable the shim installs.
const ShimCommand = "tau-pi-host"

// ErrShimMissing reports that a TypeScript extension was found but the host
// shim is not installed.
//
// tau never installs it silently. Running `npx` on an extension's behalf would
// fetch and execute code from the network as a side effect of opening a
// directory, which is precisely the thing the project-trust gate exists to
// prevent.
var ErrShimMissing = errors.New(
	"exthost: TypeScript extensions need the host shim; install it with `npm i -g " + ShimPackage + "`")

// Candidate is a discovered extension, before it is launched.
type Candidate struct {
	// Path is what was discovered: a file, or a package directory.
	Path string
	// Name is the identity shown to the user.
	Name string
	// Kind is how to launch it.
	Kind Kind
	// Source records where the entry came from, for diagnostics: "flag",
	// "settings", "user", or "project".
	Source string
	// Entry is the file the launcher should run. For a package directory it is
	// the manifest's entry point rather than the directory itself.
	Entry string
}

// manifest is the subset of package.json tau reads.
//
// The `tau` key is tau's own; `pi` is accepted as an alias so a Pi extension
// published before tau existed needs no edit to be discovered. Where both are
// present, tau's wins — an author who wrote both meant the specific one.
type manifest struct {
	Name string          `json:"name"`
	Main string          `json:"main"`
	Tau  *manifestExtras `json:"tau,omitempty"`
	Pi   *manifestExtras `json:"pi,omitempty"`
}

type manifestExtras struct {
	Extension string `json:"extension,omitempty"`
	// Native marks an executable that speaks the wire protocol directly, so
	// tau launches it instead of the TypeScript shim.
	Native string `json:"native,omitempty"`
}

func (m manifest) extras() *manifestExtras {
	if m.Tau != nil {
		return m.Tau
	}
	return m.Pi
}

// DiscoverOptions describes where to look.
type DiscoverOptions struct {
	// Flags are -e values, highest precedence.
	Flags []string
	// Settings are paths from the resolved settings.
	Settings []string
	// UserDir is ~/.tau/agent/extensions.
	UserDir string
	// ProjectDir is <cwd>/.tau/extensions. It is only read when Trusted.
	ProjectDir string
	// Trusted gates the project directory. Cloning a repository must not be
	// enough to make tau execute what is in it.
	Trusted bool
	// Cwd resolves relative paths.
	Cwd string
}

// Discover finds extensions in precedence order, nearest wins.
//
// The same path reached twice is loaded once: a path named on the command line
// and also present in settings is one extension, and loading it twice would
// dispatch every event to it twice.
func Discover(opts DiscoverOptions) ([]Candidate, []error) {
	var out []Candidate
	var errs []error
	seen := map[string]bool{}

	add := func(path, source string) {
		abs := path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(opts.Cwd, abs)
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			return
		}
		seen[abs] = true

		c, err := classify(abs, source)
		if err != nil {
			errs = append(errs, err)
			return
		}
		out = append(out, c)
	}

	for _, p := range opts.Flags {
		add(p, "flag")
	}
	for _, p := range opts.Settings {
		add(p, "settings")
	}
	for _, p := range scanDir(opts.UserDir, &errs) {
		add(p, "user")
	}
	// A project directory is only read once the user has trusted it. Without
	// the gate, `git clone && tau` would run whatever the repository shipped.
	if opts.Trusted {
		for _, p := range scanDir(opts.ProjectDir, &errs) {
			add(p, "project")
		}
	}
	return out, errs
}

// scanDir lists the immediate children of an extensions directory. It does not
// recurse: a nested tree is a package's own business, and walking into
// node_modules would find hundreds of files that are not extensions.
func scanDir(dir string, errs *[]error) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			*errs = append(*errs, fmt.Errorf("exthost: read %s: %w", dir, err))
		}
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		if e.IsDir() {
			out = append(out, filepath.Join(dir, name))
			continue
		}
		if isExtensionFile(name) {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.Strings(out)
	return out
}

func isExtensionFile(name string) bool {
	switch filepath.Ext(name) {
	case ".ts", ".mts", ".cts", ".js", ".mjs", ".cjs":
		return true
	}
	return false
}

// classify decides how a discovered path is launched.
func classify(path, source string) (Candidate, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Candidate{}, fmt.Errorf("exthost: %s: %w", path, err)
	}

	c := Candidate{Path: path, Source: source, Name: baseName(path)}

	if info.IsDir() {
		m, err := readManifest(path)
		if err != nil {
			return Candidate{}, err
		}
		if m.Name != "" {
			c.Name = m.Name
		}
		if x := m.extras(); x != nil && x.Native != "" {
			c.Kind = KindNative
			c.Entry = filepath.Join(path, x.Native)
			return c, nil
		}
		entry := ""
		if x := m.extras(); x != nil && x.Extension != "" {
			entry = x.Extension
		} else if m.Main != "" {
			entry = m.Main
		}
		if entry == "" {
			return Candidate{}, fmt.Errorf(
				"exthost: %s: package.json names no entry point (set \"tau\": {\"extension\": \"...\"} or \"main\")", path)
		}
		c.Kind = KindTypeScript
		c.Entry = filepath.Join(path, entry)
		return c, nil
	}

	// A Go plugin is never loaded. Go's plugin support requires the host and
	// the plugin to be built by the same toolchain against identical
	// dependency versions, which cannot be promised across a release boundary,
	// and the failure mode is a crash rather than a diagnostic.
	if ext := filepath.Ext(path); ext == ".so" || ext == ".dylib" || ext == ".dll" {
		return Candidate{}, fmt.Errorf(
			"exthost: %s: native shared libraries are not loadable; build it as an executable that speaks the wire protocol", path)
	}

	if isExtensionFile(path) {
		c.Kind, c.Entry = KindTypeScript, path
		return c, nil
	}
	if isExecutable(info) {
		c.Kind, c.Entry = KindNative, path
		return c, nil
	}
	return Candidate{}, fmt.Errorf(
		"exthost: %s: not a TypeScript file, a package directory, or an executable", path)
}

func baseName(path string) string {
	name := filepath.Base(path)
	if ext := filepath.Ext(name); ext != "" && isExtensionFile(name) {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

func isExecutable(info fs.FileInfo) bool {
	if info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		switch strings.ToLower(filepath.Ext(info.Name())) {
		case ".exe", ".bat", ".cmd", ".com":
			return true
		}
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

func readManifest(dir string) (manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return manifest{}, fmt.Errorf("exthost: %s: a directory extension needs a package.json", dir)
		}
		return manifest{}, fmt.Errorf("exthost: %s: %w", dir, err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return manifest{}, fmt.Errorf("exthost: %s/package.json: %w", dir, err)
	}
	return m, nil
}

// SpecFor turns a candidate into a launch spec.
//
// lookPath is injected so the caller can test the missing-shim path without
// depending on what happens to be installed.
func SpecFor(c Candidate, lookPath func(string) (string, error)) (Spec, error) {
	switch c.Kind {
	case KindNative:
		return Spec{Name: c.Name, Path: c.Path, Command: c.Entry}, nil

	case KindTypeScript:
		shim, err := lookPath(ShimCommand)
		if err != nil {
			return Spec{}, fmt.Errorf("%w (needed for %s)", ErrShimMissing, c.Path)
		}
		return Spec{
			Name: c.Name, Path: c.Path,
			Command: shim, Args: []string{c.Entry},
			Dir: filepath.Dir(c.Entry),
		}, nil
	}
	return Spec{}, fmt.Errorf("exthost: %s: unknown kind %q", c.Path, c.Kind)
}
