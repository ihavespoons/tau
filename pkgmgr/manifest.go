package pkgmgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ResourceType is a kind of resource a package can provide.
type ResourceType string

const (
	TypeExtensions ResourceType = "extensions"
	TypeSkills     ResourceType = "skills"
	TypePrompts    ResourceType = "prompts"
	TypeThemes     ResourceType = "themes"
)

// ResourceTypes is every resource type, in the order they are collected.
var ResourceTypes = []ResourceType{TypeExtensions, TypeSkills, TypePrompts, TypeThemes}

// Manifest is the resource declaration inside a package's package.json.
//
// Each field lists source entries — paths or globs relative to the package
// root — optionally mixed with the override forms from pattern.go, which let a
// package ship something disabled by default.
type Manifest struct {
	Extensions []string `json:"extensions,omitempty"`
	Skills     []string `json:"skills,omitempty"`
	Prompts    []string `json:"prompts,omitempty"`
	Themes     []string `json:"themes,omitempty"`
}

// Entries returns the manifest's declaration for one resource type. The second
// result distinguishes "declared as empty" from "not declared", which decide
// different things: an empty list ships nothing, an absent one falls back to
// the conventional directory.
func (m *Manifest) Entries(t ResourceType) ([]string, bool) {
	if m == nil {
		return nil, false
	}
	switch t {
	case TypeExtensions:
		return m.Extensions, m.Extensions != nil
	case TypeSkills:
		return m.Skills, m.Skills != nil
	case TypePrompts:
		return m.Prompts, m.Prompts != nil
	case TypeThemes:
		return m.Themes, m.Themes != nil
	}
	return nil, false
}

// Empty reports that the manifest declares nothing at all, which is treated the
// same as having no manifest.
func (m *Manifest) Empty() bool {
	return m == nil || (m.Extensions == nil && m.Skills == nil && m.Prompts == nil && m.Themes == nil)
}

// packageJSON is the sliver of package.json tau reads. The manifest key is
// "tau"; "pi" is accepted so a package written for Pi works unmodified, which
// is the whole point of matching Pi's layout.
type packageJSON struct {
	Name    string    `json:"name"`
	Version string    `json:"version"`
	Tau     *Manifest `json:"tau"`
	Pi      *Manifest `json:"pi"`
}

// ReadManifest returns a package's resource declaration, or nil if it has none.
// A package.json that is missing, unreadable, or malformed yields nil rather
// than an error: the package still works by convention, and refusing to load a
// whole package over a stray comma would be worse than ignoring the file.
func ReadManifest(packageRoot string) *Manifest {
	pkg, ok := readPackageJSON(packageRoot)
	if !ok {
		return nil
	}
	if pkg.Tau != nil {
		return pkg.Tau
	}
	return pkg.Pi
}

func readPackageJSON(packageRoot string) (packageJSON, bool) {
	data, err := os.ReadFile(filepath.Join(packageRoot, "package.json"))
	if err != nil {
		return packageJSON{}, false
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return packageJSON{}, false
	}
	return pkg, true
}

// PackageIdent reads a package's declared name and version, for listings.
func PackageIdent(packageRoot string) (name, version string) {
	pkg, ok := readPackageJSON(packageRoot)
	if !ok {
		return "", ""
	}
	return pkg.Name, pkg.Version
}

// ConventionDir is where a resource type lives when a package has no manifest.
func ConventionDir(packageRoot string, t ResourceType) string {
	return filepath.Join(packageRoot, string(t))
}

// hasGlob reports whether an entry needs expanding rather than resolving.
func hasGlob(s string) bool { return strings.ContainsAny(s, "*?") }

// CollectFiles returns the resource files a manifest entry list resolves to,
// together with the subset the package itself leaves enabled.
//
// A package speaks twice here: its source entries say what exists, and any
// override patterns among them say what starts switched off. Those are the
// package author's defaults; the user's own patterns are applied later, on top.
func CollectFiles(packageRoot string, t ResourceType) (all []string, enabled map[string]bool) {
	manifest := ReadManifest(packageRoot)
	entries, declared := manifest.Entries(t)

	if declared && len(entries) > 0 {
		all = collectFromEntries(entries, packageRoot, t)
		overrides := filterOverrides(entries)
		if len(overrides) == 0 {
			return all, allEnabled(all)
		}
		return all, applyPatterns(all, overrides, packageRoot)
	}
	if declared {
		// An explicitly empty list means the package ships none of this type.
		return nil, map[string]bool{}
	}

	dir := ConventionDir(packageRoot, t)
	if !isDir(dir) {
		return nil, map[string]bool{}
	}
	all = collectResourceFiles(dir, t)
	return all, allEnabled(all)
}

func allEnabled(paths []string) map[string]bool {
	m := make(map[string]bool, len(paths))
	for _, p := range paths {
		m[p] = true
	}
	return m
}

func filterOverrides(entries []string) []string {
	var out []string
	for _, e := range entries {
		if isOverride(e) {
			out = append(out, e)
		}
	}
	return out
}

// collectFromEntries expands the non-override entries into concrete files.
func collectFromEntries(entries []string, root string, t ResourceType) []string {
	var resolved []string
	for _, entry := range entries {
		if isOverride(entry) {
			continue
		}
		if hasGlob(entry) {
			resolved = append(resolved, expandGlob(root, entry)...)
			continue
		}
		resolved = append(resolved, filepath.Join(root, filepath.FromSlash(entry)))
	}
	return collectFromPaths(resolved, t)
}

// collectFromPaths turns resolved entries into files: a directory contributes
// whatever resources it holds, a file contributes itself.
func collectFromPaths(paths []string, t ResourceType) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		var files []string
		if info.IsDir() {
			files = collectResourceFiles(p, t)
		} else {
			files = []string{p}
		}
		for _, f := range files {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

// expandGlob walks root and returns the paths matching a glob pattern.
//
// Go's filepath.Glob has no "**", so this walks and matches with the same
// matcher the enable/disable patterns use. That keeps one glob dialect in the
// package rather than two that differ in the corners.
func expandGlob(root, pattern string) []string {
	normalized := toSlash(pattern)
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if globMatch(normalized, toSlash(rel)) {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// collectResourceFiles finds the resources of one type inside a directory.
//
// Each type is discovered the way it is written: a skill is a directory holding
// SKILL.md, an extension is an entry point that may be a file or a directory
// with an index, and prompts and themes are just files with the right suffix.
func collectResourceFiles(dir string, t ResourceType) []string {
	switch t {
	case TypeSkills:
		return collectSkillEntries(dir, dir)
	case TypeExtensions:
		return collectExtensionEntries(dir)
	case TypePrompts:
		return collectFilesWithSuffix(dir, ".md")
	case TypeThemes:
		return collectFilesWithSuffix(dir, ".json")
	}
	return nil
}

// collectSkillEntries finds SKILL.md files, descending until it finds one.
//
// A directory holding a SKILL.md is a skill and is not descended into, so a
// skill's own bundled files cannot masquerade as further skills. At the top
// level a loose .md file is a skill too, which is how single-file skills are
// written.
func collectSkillEntries(dir, root string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	for _, e := range entries {
		if e.Name() != "SKILL.md" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if isFile(p) {
			return []string{p}
		}
	}

	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		p := filepath.Join(dir, name)
		if dir == root && isFile(p) && strings.HasSuffix(name, ".md") {
			out = append(out, p)
			continue
		}
		if isDir(p) {
			out = append(out, collectSkillEntries(p, root)...)
		}
	}
	return out
}

// collectExtensionEntries finds extension entry points one level down.
//
// It does not recurse: an extension's own source tree is full of .ts files that
// are not extensions, and the entry point is either the directory's index or
// what its package.json names.
func collectExtensionEntries(dir string) []string {
	if entries := resolveExtensionEntries(dir); entries != nil {
		return entries
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []string
	for _, e := range dirEntries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		p := filepath.Join(dir, name)
		switch {
		case isFile(p) && (strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".js")):
			out = append(out, p)
		case isDir(p):
			out = append(out, resolveExtensionEntries(p)...)
		}
	}
	return out
}

// resolveExtensionEntries returns a directory's declared entry points, or nil
// if it does not look like an extension directory.
func resolveExtensionEntries(dir string) []string {
	if manifest := ReadManifest(dir); manifest != nil && len(manifest.Extensions) > 0 {
		var out []string
		for _, entry := range manifest.Extensions {
			p := filepath.Join(dir, filepath.FromSlash(entry))
			if exists(p) {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	for _, index := range []string{"index.ts", "index.js"} {
		p := filepath.Join(dir, index)
		if exists(p) {
			return []string{p}
		}
	}
	return nil
}

// collectFilesWithSuffix walks a directory for files with a given suffix.
func collectFilesWithSuffix(dir, suffix string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if path != dir && (strings.HasPrefix(name, ".") || name == "node_modules") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(name, suffix) {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
