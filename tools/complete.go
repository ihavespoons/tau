package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FileMatchLimit is how many paths FileMatches returns at most. A completion
// list is read at a glance, not scrolled.
const FileMatchLimit = 20

// FileMatches lists paths under root for an autocomplete prompt.
//
// It prefers fd, for the same reason the find tool does: the answer has to skip
// what .gitignore skips, and a completion list with node_modules in it is not a
// completion list.
//
// It never downloads. Autocomplete runs on a keystroke, and a first press that
// stalled on a network fetch would be worse than no completion at all — so when
// fd is not on disk this falls back to reading the one directory the prefix
// names, which is bounded and instant. The find tool fetches fd the first time
// it runs, and completions get better afterwards without anything being said.
func FileMatches(ctx context.Context, root, prefix string, limit int) []string {
	if limit < 1 {
		limit = FileMatchLimit
	}
	if root == "" {
		root = "."
	}
	if fd, ok := binaryPath("fd"); ok {
		if out := fdMatches(ctx, fd, root, prefix, limit); len(out) > 0 {
			return out
		}
	}
	return dirMatches(root, prefix, limit)
}

func fdMatches(ctx context.Context, fd, root, prefix string, limit int) []string {
	args := []string{"--color=never", "--hidden", "--full-path"}
	if !insideGitRepo(root) {
		args = append(args, "--no-require-git")
	}
	// Over-fetch so the ranking below has something to choose between: fd
	// returns in traversal order, which is not the order a reader wants.
	args = append(args, "--max-results", strconv.Itoa(limit*5))
	args = append(args, "--", regexp.QuoteMeta(prefix), root)

	out, err := exec.CommandContext(ctx, fd, args...).Output()
	if err != nil && len(out) == 0 {
		return nil
	}

	var paths []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		if rel, err := filepath.Rel(root, line); err == nil {
			line = rel
		}
		paths = append(paths, filepath.ToSlash(line))
	}
	return rank(paths, prefix, limit)
}

// dirMatches completes within a single directory, the way a shell does.
func dirMatches(root, prefix string, limit int) []string {
	dir, base := path2(prefix)
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return nil
	}

	needle := strings.ToLower(base)
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), needle) {
			continue
		}
		// A leading dot is only offered when it was asked for: otherwise every
		// completion in a repository root would start with .git.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		p := dir + name
		if e.IsDir() {
			p += "/"
		}
		out = append(out, p)
		if len(out) == limit {
			break
		}
	}
	sort.Strings(out)
	return out
}

// path2 splits a typed prefix into the directory part, which is already
// decided, and the fragment still being typed.
func path2(prefix string) (dir, base string) {
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		return prefix[:i+1], prefix[i+1:]
	}
	return "", prefix
}

// rank puts the likeliest answers first: what the reader typed is usually the
// start of a file's name, then part of its name, and only then part of the
// directories above it.
func rank(paths []string, prefix string, limit int) []string {
	_, base := path2(prefix)
	needle := strings.ToLower(base)

	score := func(p string) int {
		name := strings.ToLower(filepath.Base(p))
		switch {
		case needle == "":
			return 1
		case strings.HasPrefix(name, needle):
			return 0
		case strings.Contains(name, needle):
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(paths, func(i, j int) bool {
		si, sj := score(paths[i]), score(paths[j])
		if si != sj {
			return si < sj
		}
		// Shorter paths are nearer the root and are what a short prefix
		// usually meant.
		return len(paths[i]) < len(paths[j])
	})
	if len(paths) > limit {
		paths = paths[:limit]
	}
	return paths
}
