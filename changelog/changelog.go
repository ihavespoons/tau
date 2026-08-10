// Package changelog parses a Keep-a-Changelog document into per-release
// entries and renders it for display.
//
// It is a port of Pi's utils/changelog.ts. Pi reads the file off disk from
// inside its installed npm package; tau has no install tree, so the caller
// passes the document in — see the root tau package for the embedded copy.
package changelog

import (
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// repo is where relative links in the changelog resolve to.
const repo = "ihavespoons/tau"

// linkBasePath is the directory relative links are relative to. Pi needs
// "packages/coding-agent" because its changelog sits inside one package of a
// monorepo; tau is a single module rooted at the repository, so links already
// mean what they say.
const linkBasePath = ""

var (
	// versionRE matches Pi's header pattern, which accepts both the bracketed
	// Keep-a-Changelog style and a bare version.
	versionRE = regexp.MustCompile(`##\s+\[?(\d+)\.(\d+)\.(\d+)\]?`)
	// inlineLinkRE matches an inline markdown link or image, capturing the
	// target separately from the optional title that may follow it.
	inlineLinkRE = regexp.MustCompile(`(!?\[[^\]\n]+\]\()([^\s)]+)((?:\s+[^)]*)?\))`)
	schemeRE     = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:`)
	// escapeRE matches a percent-escape that is already well formed.
	escapeRE = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
)

// Entry is one release's section of the changelog.
type Entry struct {
	Major, Minor, Patch int
	// Content is the section including its own `##` header line, trimmed.
	Content string
}

// Version renders the entry's version without a leading v.
func (e Entry) Version() string {
	return strconv.Itoa(e.Major) + "." + strconv.Itoa(e.Minor) + "." + strconv.Itoa(e.Patch)
}

// Tag renders the entry's git tag.
func (e Entry) Tag() string { return "v" + e.Version() }

// Parse splits a changelog into entries, in document order.
//
// A `## ` line starts a section and ends the previous one. A `## ` line with no
// version in it drops what follows until the next parseable header — that is
// how the document's own title and any prose between releases stay out of the
// output.
func Parse(markdown string) []Entry {
	var (
		entries []Entry
		current *Entry
		lines   []string
	)
	flush := func() {
		if current != nil && len(lines) > 0 {
			current.Content = strings.TrimSpace(strings.Join(lines, "\n"))
			entries = append(entries, *current)
		}
	}

	for _, line := range strings.Split(markdown, "\n") {
		if !strings.HasPrefix(line, "## ") {
			if current != nil {
				lines = append(lines, line)
			}
			continue
		}
		flush()

		m := versionRE.FindStringSubmatch(line)
		if m == nil {
			current, lines = nil, nil
			continue
		}
		current = &Entry{Major: atoi(m[1]), Minor: atoi(m[2]), Patch: atoi(m[3])}
		lines = []string{line}
	}
	flush()

	return entries
}

// Compare orders two entries by version: negative if a precedes b, zero if
// they are the same release, positive if a follows b.
func Compare(a, b Entry) int {
	if a.Major != b.Major {
		return a.Major - b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor - b.Minor
	}
	return a.Patch - b.Patch
}

// Newer returns the entries released after version, which may carry a leading
// v and may be truncated ("0.19" means 0.19.0). An unparseable component reads
// as zero, so a garbage version yields everything rather than nothing.
func Newer(entries []Entry, version string) []Entry {
	last := parseVersion(version)
	var out []Entry
	for _, e := range entries {
		if Compare(e, last) > 0 {
			out = append(out, e)
		}
	}
	return out
}

// Render turns a whole changelog into the text /changelog shows.
//
// Entries come out oldest first, because the newest release is the one the
// reader cares about and the terminal puts the bottom of the output nearest
// their eye. Links are rewritten to point at each entry's own tag, so a line
// written about 0.5.0 still resolves to the file as it stood then.
func Render(markdown string) string {
	entries := Parse(markdown)
	if len(entries) == 0 {
		return "No changelog entries found."
	}
	parts := make([]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		parts = append(parts, NormalizeLinks(entries[i].Content, entries[i].Tag()))
	}
	return strings.Join(parts, "\n\n")
}

// NormalizeLinks rewrites the inline links in markdown so they resolve from
// anywhere: relative paths become GitHub URLs pinned to tag, and URLs already
// pointing at a moving branch are pinned to it too.
func NormalizeLinks(markdown, tag string) string {
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return inlineLinkRE.ReplaceAllStringFunc(markdown, func(match string) string {
		m := inlineLinkRE.FindStringSubmatch(match)
		return m[1] + normalizeTarget(m[2], tag) + m[3]
	})
}

func normalizeTarget(target, tag string) string {
	repoURL := "https://github.com/" + repo

	// A link into main or master says "the current file", which stops being
	// true the moment the file changes. The entry knows which release it
	// describes, so pin it there.
	canonical := target
	for _, route := range []string{"blob", "tree"} {
		for _, branch := range []string{"main", "master"} {
			prefix := repoURL + "/" + route + "/" + branch + "/"
			if rest, ok := strings.CutPrefix(canonical, prefix); ok {
				canonical = repoURL + "/" + route + "/" + tag + "/" + rest
			}
		}
	}

	// Anything already addressed absolutely, or aimed at the rendered page
	// itself, is left alone.
	if strings.HasPrefix(canonical, "#") || strings.HasPrefix(canonical, "//") || schemeRE.MatchString(canonical) {
		return canonical
	}

	pathPart, query, fragment := splitTarget(canonical)
	if pathPart == "" {
		return canonical
	}
	repoPath, ok := resolveRepoPath(pathPart)
	if !ok {
		return canonical
	}
	route := "blob"
	if isDirectory(pathPart, repoPath) {
		route = "tree"
	}
	return repoURL + "/" + route + "/" + tag + "/" + escapePath(repoPath) + query + fragment
}

// splitTarget divides a link target into its path, query and fragment, each
// keeping its leading punctuation so concatenation rebuilds the original.
func splitTarget(target string) (pathPart, query, fragment string) {
	before := target
	if i := strings.Index(target, "#"); i >= 0 {
		before, fragment = target[:i], target[i:]
	}
	if i := strings.Index(before, "?"); i >= 0 {
		return before[:i], before[i:], fragment
	}
	return before, "", fragment
}

// resolveRepoPath maps a link's path onto a path inside the repository, or
// reports that it escapes the repository and should be left alone.
func resolveRepoPath(target string) (string, bool) {
	target = strings.ReplaceAll(target, "\\", "/")

	var joined string
	if strings.HasPrefix(target, "/") {
		joined = path.Clean(strings.TrimLeft(target, "/"))
	} else {
		joined = path.Clean(path.Join(linkBasePath, target))
	}
	if joined == "." || joined == ".." || strings.HasPrefix(joined, "../") {
		return "", false
	}
	// path.Clean drops a trailing slash where Node's posix normalize — what
	// this is ported from — keeps it. The slash is the strongest signal that
	// the link means a directory, so it has to survive.
	if strings.HasSuffix(target, "/") && !strings.HasSuffix(joined, "/") {
		joined += "/"
	}
	return joined, true
}

// isDirectory picks between GitHub's blob and tree routes. A trailing slash
// settles it; otherwise a basename without a dot is taken for a directory,
// which is the same guess Pi makes and is wrong only for extensionless files.
func isDirectory(original, repoPath string) bool {
	if strings.HasSuffix(original, "/") {
		return true
	}
	return !strings.Contains(path.Base(repoPath), ".")
}

// escapePath percent-encodes a repository path, leaving its separators intact
// — which url.PathEscape would not — and leaving any escape already written in
// the link alone. Pi reaches for encodeURI here, which turns a correct %20
// into %2520 and breaks the link it was asked to fix.
func escapePath(p string) string {
	var b strings.Builder
	last := 0
	for _, loc := range escapeRE.FindAllStringIndex(p, -1) {
		b.WriteString(escapeRun(p[last:loc[0]]))
		b.WriteString(p[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(escapeRun(p[last:]))
	return b.String()
}

// escapeRun escapes a stretch of path known to contain no escapes. A lone %
// still gets encoded, because at that point it is a literal.
func escapeRun(s string) string {
	return (&url.URL{Path: s}).EscapedPath()
}

func parseVersion(v string) Entry {
	var e Entry
	fields := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	dst := []*int{&e.Major, &e.Minor, &e.Patch}
	for i, f := range fields {
		*dst[i] = atoi(f)
	}
	return e
}

// atoi reads a leading run of digits, ignoring whatever follows. Version
// strings arrive with build metadata attached often enough to matter, and the
// regex has already vouched for the changelog's own headers.
func atoi(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}
