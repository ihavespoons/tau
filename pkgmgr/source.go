// Package pkgmgr installs and resolves resource packages: bundles of skills,
// prompt templates, themes and extensions distributed over npm, git, or a local
// directory.
//
// A package is just a directory. What makes it a package is that tau knows how
// to fetch it, where it put it, and which files inside it count as resources —
// either because a manifest says so or because they sit where resources live.
package pkgmgr

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind is how a source is fetched.
type Kind string

const (
	// KindNPM is an npm package, installed with npm into a managed prefix.
	KindNPM Kind = "npm"
	// KindGit is a git repository, cloned into a managed directory.
	KindGit Kind = "git"
	// KindLocal is a directory already on disk, used where it lies.
	KindLocal Kind = "local"
)

// Source is a parsed package source.
type Source struct {
	Kind Kind
	// Raw is the string the user wrote, kept verbatim so settings round-trip.
	Raw string

	// Spec, Name and Version describe an npm source. Spec is what npm is
	// asked to install ("pkg@^1.2.0"); Name is the bare package name, which
	// is also the directory it lands in.
	Spec    string
	Name    string
	Version string

	// Repo, Host, Path and Ref describe a git source. Repo is a URL git can
	// clone; Host and Path decide where the clone lands.
	Repo string
	Host string
	Path string
	Ref  string

	// LocalPath is the directory of a local source, as written.
	LocalPath string

	// Pinned reports that the source names an exact version or ref, so
	// updating it would contradict what the user asked for.
	Pinned bool
}

// npmSpecPattern splits "@scope/name@version" and "name@version". The leading
// "@" of a scope must not be read as the version separator, which is the only
// reason this is a regexp rather than a LastIndex.
var npmSpecPattern = regexp.MustCompile(`^(@?[^@]+(?:/[^@]+)?)(?:@(.+))?$`)

// exactVersion matches a fully-specified semver, which is what "pinned" means
// for npm. A range like "^1.2.0" is not pinned: the user asked for whatever
// satisfies it, and that changes over time.
var exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// ParseSource classifies a package source string.
//
// The rules are Pi's: an "npm:" prefix is npm, an obvious filesystem path is
// local, a parseable git URL is git, and anything left over is local. The last
// case is deliberate — a bare name that is not a URL is far more likely to be a
// mistyped directory than a repository nobody can reach.
func ParseSource(s string) Source {
	trimmed := strings.TrimSpace(s)

	if rest, ok := strings.CutPrefix(trimmed, "npm:"); ok {
		spec := strings.TrimSpace(rest)
		name, version := parseNPMSpec(spec)
		return Source{
			Kind: KindNPM, Raw: trimmed,
			Spec: spec, Name: name, Version: version,
			Pinned: exactVersion.MatchString(version),
		}
	}

	if isLocalPath(trimmed) {
		return Source{Kind: KindLocal, Raw: trimmed, LocalPath: trimmed}
	}

	if git, ok := ParseGitURL(trimmed); ok {
		git.Raw = trimmed
		return git
	}

	return Source{Kind: KindLocal, Raw: trimmed, LocalPath: trimmed}
}

// parseNPMSpec splits a spec into name and version.
func parseNPMSpec(spec string) (name, version string) {
	m := npmSpecPattern.FindStringSubmatch(spec)
	if m == nil {
		return spec, ""
	}
	return m[1], m[2]
}

// isLocalPath reports whether a source is meant as a filesystem path.
func isLocalPath(s string) bool {
	switch {
	case s == "." || s == "..":
		return true
	case strings.HasPrefix(s, "./"), strings.HasPrefix(s, "../"):
		return true
	case strings.HasPrefix(s, ".\\"), strings.HasPrefix(s, "..\\"):
		return true
	case strings.HasPrefix(s, "~"):
		return true
	case filepath.IsAbs(s):
		return true
	}
	// A Windows drive letter is absolute even when tau is not running on
	// Windows: the string came from settings that may have been written there.
	return len(s) > 2 && s[1] == ':' && (s[2] == '\\' || s[2] == '/')
}

// scpLike matches git's abbreviated ssh syntax, git@host:owner/repo, which is
// not a URL and so cannot be handed to a URL parser.
var scpLike = regexp.MustCompile(`^git@([^:]+):(.+)$`)

// explicitScheme matches the URL forms accepted without a "git:" prefix.
var explicitScheme = regexp.MustCompile(`(?i)^(https?|ssh|git)://`)

// ParseGitURL parses a git source, returning false if the string is not one.
//
// With a "git:" prefix, shorthand is accepted: host/owner/repo and git@host:…
// as well as full URLs. Without one, only an explicit scheme counts, because
// "some/path" has to stay a directory rather than silently becoming a clone of
// somebody's repository.
//
// A ref may be appended as "@ref" or "#ref". Pi resolves shorthand through
// hosted-git-info, which additionally knows a handful of host aliases; tau
// covers the aliases it documents (github:, gitlab:, bitbucket:) and otherwise
// requires a real hostname. The cost of the gap is that an undocumented alias
// is treated as a path, which fails loudly at install time rather than quietly
// fetching the wrong thing.
func ParseGitURL(source string) (Source, bool) {
	trimmed := strings.TrimSpace(source)
	hasPrefix := strings.HasPrefix(trimmed, "git:") && !strings.HasPrefix(trimmed, "git://")
	raw := trimmed
	if hasPrefix {
		raw = strings.TrimSpace(trimmed[len("git:"):])
	}

	if host, rest, ok := hostAlias(raw); ok {
		repoPath, ref := splitRef(rest)
		return buildGitSource("https://"+host+"/"+repoPath, host, repoPath, ref)
	}

	if !hasPrefix && !explicitScheme.MatchString(raw) {
		return Source{}, false
	}

	repo, ref := splitRef(raw)

	if m := scpLike.FindStringSubmatch(repo); m != nil {
		return buildGitSource(repo, m[1], m[2], ref)
	}

	if explicitScheme.MatchString(repo) {
		u, err := url.Parse(repo)
		if err != nil {
			return Source{}, false
		}
		return buildGitSource(repo, u.Hostname(), strings.TrimLeft(u.Path, "/"), ref)
	}

	host, path, ok := strings.Cut(repo, "/")
	if !ok {
		return Source{}, false
	}
	// Without a dot there is no reason to believe this is a hostname, and
	// "owner/repo" alone does not say which forge it lives on.
	if !strings.Contains(host, ".") && host != "localhost" {
		return Source{}, false
	}
	return buildGitSource("https://"+repo, host, path, ref)
}

// hostAlias expands the forge shorthands Pi's docs advertise.
func hostAlias(s string) (host, rest string, ok bool) {
	for prefix, h := range map[string]string{
		"github:":    "github.com",
		"gitlab:":    "gitlab.com",
		"bitbucket:": "bitbucket.org",
	} {
		if r, found := strings.CutPrefix(s, prefix); found {
			return h, r, true
		}
	}
	return "", "", false
}

// splitRef separates a trailing ref from a repository.
//
// Both "@ref" and "#ref" are accepted. The "@" form is searched after the
// host so that git@host:owner/repo does not lose its user, and the search is
// for the first "@" in the path, matching Pi.
func splitRef(u string) (repo, ref string) {
	if base, r, ok := strings.Cut(u, "#"); ok && base != "" && r != "" {
		return base, r
	}

	if m := scpLike.FindStringSubmatch(u); m != nil {
		path, r, ok := strings.Cut(m[2], "@")
		if !ok || path == "" || r == "" {
			return u, ""
		}
		return "git@" + m[1] + ":" + path, r
	}

	if idx := strings.Index(u, "://"); idx >= 0 {
		head := u[:idx+3]
		rest := u[idx+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return u, ""
		}
		path, r, ok := strings.Cut(rest[slash:], "@")
		if !ok || r == "" || path == "/" {
			return u, ""
		}
		return head + rest[:slash] + path, r
	}

	slash := strings.Index(u, "/")
	if slash < 0 {
		return u, ""
	}
	path, r, ok := strings.Cut(u[slash:], "@")
	if !ok || r == "" || path == "/" {
		return u, ""
	}
	return u[:slash] + path, r
}

// buildGitSource validates the pieces and assembles a git source.
func buildGitSource(repo, host, path, ref string) (Source, bool) {
	path = strings.TrimSuffix(strings.TrimLeft(path, "/"), ".git")
	if host == "" || path == "" || len(strings.Split(path, "/")) < 2 {
		return Source{}, false
	}
	if unsafePathPart(host, false) || unsafePathPart(path, true) {
		return Source{}, false
	}
	return Source{
		Kind: KindGit, Raw: repo,
		Repo: repo, Host: host, Path: path, Ref: ref,
		Pinned: ref != "",
	}, true
}

// unsafePathPart rejects components that would escape the install root.
//
// Host and path go straight into a directory name, so a "..", a backslash, or
// a percent-encoded one is a path traversal with a clone in front of it. The
// decoded form is checked as well as the literal, because the filesystem sees
// whichever the shell hands it.
func unsafePathPart(value string, allowSlash bool) bool {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return true
	}
	for _, candidate := range []string{value, decoded} {
		if strings.ContainsAny(candidate, "\x00\\") || strings.HasPrefix(candidate, "/") {
			return true
		}
		if !allowSlash && strings.Contains(candidate, "/") {
			return true
		}
		for _, part := range strings.Split(candidate, "/") {
			if part == ".." {
				return true
			}
		}
	}
	return false
}
