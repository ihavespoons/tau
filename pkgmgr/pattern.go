package pkgmgr

import (
	"path"
	"path/filepath"
	"strings"
)

// Pattern lists decide which of a package's resources are used.
//
// An entry is one of four things, and the prefix is the whole grammar:
//
//	pattern    include anything matching (if any plain include exists, it is
//	           the whitelist — everything unmatched is left out)
//	!pattern   exclude anything matching
//	+path      force-include exactly this path, overriding an exclude
//	-path      force-exclude exactly this path, overriding everything
//
// The two override forms match a literal path rather than a glob, which is
// what makes them usable as a per-resource on/off switch: "-skills/deploy"
// turns off one skill without disturbing the pattern that selected the rest.

// isPattern reports whether an entry is a pattern rather than a plain path.
func isPattern(s string) bool {
	return strings.HasPrefix(s, "!") || strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") ||
		strings.ContainsAny(s, "*?")
}

// isOverride reports whether an entry is one of the override forms.
func isOverride(s string) bool {
	return strings.HasPrefix(s, "!") || strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-")
}

// splitPatternEntries separates plain paths from patterns. A plain path names a
// resource directly and is resolved as a path; a pattern filters a scan.
func splitPatternEntries(entries []string) (plain, patterns []string) {
	for _, e := range entries {
		if isPattern(e) {
			patterns = append(patterns, e)
		} else {
			plain = append(plain, e)
		}
	}
	return plain, patterns
}

// matchCandidates lists the strings a pattern is tried against for one file.
//
// A pattern may name the path relative to the package, the bare file name, or
// the absolute path. For a SKILL.md the skill's directory counts too, because
// a skill is addressed by its directory everywhere else in tau and requiring
// "skills/deploy/SKILL.md" to disable it would be a trap.
func matchCandidates(filePath, baseDir string) []string {
	rel, err := filepath.Rel(baseDir, filePath)
	if err != nil {
		rel = filePath
	}
	name := filepath.Base(filePath)
	out := []string{toSlash(rel), name, toSlash(filePath)}

	if name == "SKILL.md" {
		parent := filepath.Dir(filePath)
		parentRel, err := filepath.Rel(baseDir, parent)
		if err != nil {
			parentRel = parent
		}
		out = append(out, toSlash(parentRel), filepath.Base(parent), toSlash(parent))
	}
	return out
}

// exactCandidates is matchCandidates minus the bare names, for the override
// forms. A "+" or "-" entry addresses one file, so matching it against a bare
// base name would let "-config.json" disable every theme called that.
func exactCandidates(filePath, baseDir string) []string {
	rel, err := filepath.Rel(baseDir, filePath)
	if err != nil {
		rel = filePath
	}
	out := []string{toSlash(rel), toSlash(filePath)}

	if filepath.Base(filePath) == "SKILL.md" {
		parent := filepath.Dir(filePath)
		parentRel, err := filepath.Rel(baseDir, parent)
		if err != nil {
			parentRel = parent
		}
		out = append(out, toSlash(parentRel), toSlash(parent))
	}
	return out
}

func toSlash(p string) string { return filepath.ToSlash(p) }

// matchesAny reports whether any pattern matches the file.
func matchesAny(filePath string, patterns []string, baseDir string) bool {
	candidates := matchCandidates(filePath, baseDir)
	for _, pattern := range patterns {
		normalized := toSlash(pattern)
		for _, c := range candidates {
			if globMatch(normalized, c) {
				return true
			}
		}
	}
	return false
}

// matchesAnyExact reports whether any pattern equals the file, comparing paths
// rather than globbing.
func matchesAnyExact(filePath string, patterns []string, baseDir string) bool {
	if len(patterns) == 0 {
		return false
	}
	candidates := exactCandidates(filePath, baseDir)
	for _, pattern := range patterns {
		normalized := normalizeExact(pattern)
		for _, c := range candidates {
			if normalized == c {
				return true
			}
		}
	}
	return false
}

// normalizeExact strips a leading "./" so a path written the way a shell
// completes it compares equal to the relative path tau computed.
func normalizeExact(pattern string) string {
	p := strings.TrimPrefix(strings.TrimPrefix(pattern, "./"), `.\`)
	return toSlash(p)
}

// globMatch matches a slash-separated path against a glob.
//
// "*" and "?" stay within one segment and "**" spans any number of them, which
// is the subset of minimatch's grammar that Pi's own patterns use. Brace
// expansion is not supported: nothing in Pi's resource lists relies on it, and
// a pattern this does not understand simply fails to match, which shows up as
// a resource that did not appear rather than one that wrongly did.
func globMatch(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, parts []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Trailing "**" swallows the rest, including nothing at all.
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(parts); i++ {
				if matchSegs(pat[1:], parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], parts[0]); err != nil || !ok {
			return false
		}
		pat, parts = pat[1:], parts[1:]
	}
	return len(parts) == 0
}

// applyPatterns filters paths down to the enabled set.
//
// Includes select, excludes remove, "+" adds back, "-" removes last. With no
// plain include everything starts selected, so a list made only of overrides
// reads as "all of them except these", which is how a user disables one skill
// out of a package.
func applyPatterns(allPaths, patterns []string, baseDir string) map[string]bool {
	var includes, excludes, forceIncludes, forceExcludes []string
	for _, p := range patterns {
		switch {
		case strings.HasPrefix(p, "+"):
			forceIncludes = append(forceIncludes, p[1:])
		case strings.HasPrefix(p, "-"):
			forceExcludes = append(forceExcludes, p[1:])
		case strings.HasPrefix(p, "!"):
			excludes = append(excludes, p[1:])
		default:
			includes = append(includes, p)
		}
	}

	result := make(map[string]bool, len(allPaths))
	for _, p := range allPaths {
		if len(includes) == 0 || matchesAny(p, includes, baseDir) {
			result[p] = true
		}
	}
	if len(excludes) > 0 {
		for p := range result {
			if matchesAny(p, excludes, baseDir) {
				delete(result, p)
			}
		}
	}
	if len(forceIncludes) > 0 {
		for _, p := range allPaths {
			if !result[p] && matchesAnyExact(p, forceIncludes, baseDir) {
				result[p] = true
			}
		}
	}
	if len(forceExcludes) > 0 {
		for p := range result {
			if matchesAnyExact(p, forceExcludes, baseDir) {
				delete(result, p)
			}
		}
	}
	return result
}

// applyAutoloadDisabledPatterns computes per-path enable/disable decisions for a
// package that autoloads everything.
//
// Unlike applyPatterns this does not select a subset: everything is loaded
// either way, and the patterns only say which of them start switched off. The
// returned map holds a verdict for the paths some pattern spoke about; a path
// nothing matched is not in it and keeps the caller's default.
func applyAutoloadDisabledPatterns(allPaths, patterns []string, baseDir string) map[string]bool {
	result := map[string]bool{}
	for _, pattern := range patterns {
		target := pattern
		if isOverride(pattern) {
			target = pattern[1:]
		}
		enabled := !strings.HasPrefix(pattern, "-") && !strings.HasPrefix(pattern, "!")
		exact := strings.HasPrefix(pattern, "+") || strings.HasPrefix(pattern, "-")

		for _, filePath := range allPaths {
			var hit bool
			if exact {
				hit = matchesAnyExact(filePath, []string{target}, baseDir)
			} else {
				hit = matchesAny(filePath, []string{target}, baseDir)
			}
			if hit {
				result[filePath] = enabled
			}
		}
	}
	return result
}

// isEnabledByOverrides applies only the override forms to one path, for
// resources that were named directly rather than discovered by a scan.
func isEnabledByOverrides(filePath string, patterns []string, baseDir string) bool {
	var excludes, forceIncludes, forceExcludes []string
	for _, p := range patterns {
		switch {
		case strings.HasPrefix(p, "!"):
			excludes = append(excludes, p[1:])
		case strings.HasPrefix(p, "+"):
			forceIncludes = append(forceIncludes, p[1:])
		case strings.HasPrefix(p, "-"):
			forceExcludes = append(forceExcludes, p[1:])
		}
	}

	enabled := true
	if len(excludes) > 0 && matchesAny(filePath, excludes, baseDir) {
		enabled = false
	}
	if len(forceIncludes) > 0 && matchesAnyExact(filePath, forceIncludes, baseDir) {
		enabled = true
	}
	if len(forceExcludes) > 0 && matchesAnyExact(filePath, forceExcludes, baseDir) {
		enabled = false
	}
	return enabled
}
