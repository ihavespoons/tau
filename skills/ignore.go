package skills

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ignoreFileNames are the ignore files honored while scanning a skill tree
// (skills.ts:16). A skills directory checked into a repository often sits
// beside build output and vendored trees; without this, scanning one means
// walking all of it.
var ignoreFileNames = []string{".gitignore", ".ignore", ".fdignore"}

// ignoreRules is a gitignore-style matcher accumulated over a walk.
//
// This is deliberately a subset of git's format — the shapes that appear in
// real ignore files (globs, negation, directory-only, anchoring) — rather than
// a full implementation. A skills tree is not a repository, and a pattern this
// misses costs a wasted directory read, not a wrong answer about a file.
type ignoreRules struct {
	rules []ignoreRule
}

type ignoreRule struct {
	segs     []string
	negated  bool
	dirOnly  bool
	anchored bool
}

// add parses ignore-file lines, anchoring each one under prefix — the path of
// the file's own directory relative to the root of the walk. A nested
// .gitignore governs its own subtree and nothing above it.
func (r *ignoreRules) add(lines []string, prefix string) {
	for _, line := range lines {
		rule, ok := parseIgnoreLine(line, prefix)
		if ok {
			r.rules = append(r.rules, rule)
		}
	}
}

func parseIgnoreLine(line, prefix string) (ignoreRule, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ignoreRule{}, false
	}
	// An escaped hash is a literal name; an unescaped one is a comment.
	if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, `\#`) {
		return ignoreRule{}, false
	}

	pattern := trimmed
	var negated bool
	switch {
	case strings.HasPrefix(pattern, "!"):
		negated = true
		pattern = pattern[1:]
	case strings.HasPrefix(pattern, `\`):
		pattern = pattern[1:]
	}

	// A leading slash anchors the pattern to the directory holding the ignore
	// file, which is exactly where the prefix puts it.
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")

	dirOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return ignoreRule{}, false
	}

	// A slash anywhere in the pattern anchors it too, per gitignore.
	if strings.Contains(pattern, "/") {
		anchored = true
	}
	if prefix != "" {
		pattern = prefix + pattern
		anchored = true
	}

	return ignoreRule{
		segs:     strings.Split(pattern, "/"),
		negated:  negated,
		dirOnly:  dirOnly,
		anchored: anchored,
	}, true
}

// ignores reports whether a path relative to the root of the walk is excluded.
// Later rules win, so a negation can re-include what an earlier rule dropped.
func (r *ignoreRules) ignores(rel string, isDir bool) bool {
	if r == nil || len(r.rules) == 0 || rel == "" {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	ignored := false
	for _, rule := range r.rules {
		if rule.matches(parts, isDir) {
			ignored = !rule.negated
		}
	}
	return ignored
}

func (r ignoreRule) matches(parts []string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if r.anchored {
		return matchSegments(r.segs, parts)
	}
	// An unanchored pattern matches at any depth.
	for i := range parts {
		if matchSegments(r.segs, parts[i:]) {
			return true
		}
	}
	return false
}

// matchSegments reports whether the pattern consumes the path exactly, with
// "**" standing in for any number of segments.
func matchSegments(segs, parts []string) bool {
	if len(segs) == 0 {
		return len(parts) == 0
	}
	if segs[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchSegments(segs[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	if ok, err := path.Match(segs[0], parts[0]); err != nil || !ok {
		return false
	}
	return matchSegments(segs[1:], parts[1:])
}

// loadIgnoreFiles adds the rules declared in dir, if any. prefix is dir's path
// relative to the root of the walk.
func loadIgnoreFiles(r *ignoreRules, dir, root string) {
	prefix := ""
	if rel, err := filepath.Rel(root, dir); err == nil && rel != "." {
		prefix = filepath.ToSlash(rel) + "/"
	}
	for _, name := range ignoreFileNames {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		r.add(strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n"), prefix)
	}
}
