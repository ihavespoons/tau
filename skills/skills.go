// Package skills discovers and parses Agent Skills (SKILL.md files) and
// renders them for the system prompt. Port of Pi's core/skills.ts.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ihavespoons/tau/frontmatter"
)

// Spec limits (skills.ts:11-14).
const (
	MaxNameLength        = 64
	MaxDescriptionLength = 1024
)

// Skill is a discovered skill.
type Skill struct {
	Name        string
	Description string
	// FilePath is the absolute path to SKILL.md; the model reads it on demand.
	FilePath string
	// BaseDir is the skill's directory — relative paths inside the skill
	// resolve against it.
	BaseDir string
	// Source is where it came from: user, project, or path.
	Source string
	// DisableModelInvocation hides the skill from the prompt; it can then
	// only be invoked explicitly via /skill:<name>.
	DisableModelInvocation bool
}

// Diagnostic is a non-fatal problem found while loading skills. A bad skill
// is reported and skipped — it never fails the run.
type Diagnostic struct {
	Type    string // "warning" | "collision"
	Message string
	Path    string
}

// Result is a load outcome.
type Result struct {
	Skills      []Skill
	Diagnostics []Diagnostic
}

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// validateName ports skills.ts:92-112.
func validateName(name string) []string {
	var errs []string
	if len(name) > MaxNameLength {
		errs = append(errs, fmt.Sprintf("name exceeds %d characters (%d)", MaxNameLength, len(name)))
	}
	if !namePattern.MatchString(name) {
		errs = append(errs, "name contains invalid characters (must be lowercase a-z, 0-9, hyphens only)")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		errs = append(errs, "name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		errs = append(errs, "name must not contain consecutive hyphens")
	}
	return errs
}

// LoadFromFile parses one SKILL.md. A missing description is fatal for that
// skill (it is what the model matches on); everything else is a warning.
func LoadFromFile(path, source string) (*Skill, []Diagnostic) {
	var diags []Diagnostic

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, []Diagnostic{{Type: "warning", Message: err.Error(), Path: path}}
	}

	fm, _, err := frontmatter.Parse(raw)
	if err != nil {
		return nil, []Diagnostic{{Type: "warning", Message: "failed to parse frontmatter: " + err.Error(), Path: path}}
	}

	dir := filepath.Dir(path)
	description, _ := fm.String("description")
	if strings.TrimSpace(description) == "" {
		diags = append(diags, Diagnostic{Type: "warning", Message: "description is required", Path: path})
		return nil, diags
	}
	if len(description) > MaxDescriptionLength {
		diags = append(diags, Diagnostic{
			Type:    "warning",
			Message: fmt.Sprintf("description exceeds %d characters (%d)", MaxDescriptionLength, len(description)),
			Path:    path,
		})
	}

	// Name falls back to the containing directory (skills.ts:296).
	name, _ := fm.String("name")
	if name == "" {
		name = filepath.Base(dir)
	}
	for _, e := range validateName(name) {
		diags = append(diags, Diagnostic{Type: "warning", Message: e, Path: path})
	}

	disabled, _ := fm.Bool("disable-model-invocation")

	return &Skill{
		Name:                   name,
		Description:            description,
		FilePath:               path,
		BaseDir:                dir,
		Source:                 source,
		DisableModelInvocation: disabled,
	}, diags
}

// LoadFromDir scans a directory for skills.
//
// Discovery rules (skills.ts:160-167): a directory containing SKILL.md is a
// skill root and is not descended into; otherwise direct .md children of the
// root are loaded and subdirectories are searched for SKILL.md.
//
// .gitignore, .ignore, and .fdignore files encountered along the way are
// honored, each governing its own subtree.
func LoadFromDir(dir, source string) Result {
	return loadFromDir(dir, source, true, &ignoreRules{}, dir)
}

func loadFromDir(dir, source string, includeRootFiles bool, rules *ignoreRules, root string) Result {
	var res Result

	entries, err := os.ReadDir(dir)
	if err != nil {
		return res
	}

	// Rules declared here apply to everything below, so they have to be read
	// before the entries are judged.
	loadIgnoreFiles(rules, dir, root)

	ignored := func(full string, isDir bool) bool {
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return false
		}
		return rules.ignores(rel, isDir)
	}

	// A SKILL.md here makes this a skill root: load it and stop.
	for _, e := range entries {
		if e.Name() != "SKILL.md" {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() || ignored(full, false) {
			continue
		}
		skill, diags := LoadFromFile(full, source)
		if skill != nil {
			res.Skills = append(res.Skills, *skill)
		}
		res.Diagnostics = append(res.Diagnostics, diags...)
		return res
	}

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := os.Stat(full) // Stat follows symlinks, as Pi does.
		if err != nil {
			continue
		}
		if ignored(full, info.IsDir()) {
			continue
		}

		if info.IsDir() {
			sub := loadFromDir(full, source, false, rules, root)
			res.Skills = append(res.Skills, sub.Skills...)
			res.Diagnostics = append(res.Diagnostics, sub.Diagnostics...)
			continue
		}
		if !includeRootFiles || !strings.HasSuffix(name, ".md") {
			continue
		}
		skill, diags := LoadFromFile(full, source)
		if skill != nil {
			res.Skills = append(res.Skills, *skill)
		}
		res.Diagnostics = append(res.Diagnostics, diags...)
	}
	return res
}

// LoadOptions configures Load.
type LoadOptions struct {
	Cwd      string
	AgentDir string
	// Paths are explicit extra skill files or directories.
	Paths []string
	// IncludeDefaults scans <agentDir>/skills and <cwd>/.tau/skills.
	IncludeDefaults bool
	// ConfigDirName defaults to ".tau".
	ConfigDirName string
}

// Load gathers skills from every configured location.
//
// First registration of a name wins; later ones are reported as collisions
// (skills.ts:410-422), so a project skill cannot silently shadow a user one.
func Load(opts LoadOptions) Result {
	configDir := opts.ConfigDirName
	if configDir == "" {
		configDir = ".tau"
	}

	byName := map[string]Skill{}
	realPaths := map[string]bool{}
	var order []string
	var diags, collisions []Diagnostic

	add := func(r Result) {
		diags = append(diags, r.Diagnostics...)
		for _, s := range r.Skills {
			// Two paths that resolve to the same file are one skill, not a
			// collision. Symlinking a skill directory into a second location is
			// the normal way to share skills between checkouts, and reporting
			// that as a name clash would be reporting a skill against itself.
			real := canonicalPath(s.FilePath)
			if realPaths[real] {
				continue
			}
			if existing, dup := byName[s.Name]; dup {
				collisions = append(collisions, Diagnostic{
					Type:    "collision",
					Message: fmt.Sprintf("name %q collision (kept %s)", s.Name, existing.FilePath),
					Path:    s.FilePath,
				})
				continue
			}
			realPaths[real] = true
			byName[s.Name] = s
			order = append(order, s.Name)
		}
	}

	userDir := filepath.Join(opts.AgentDir, "skills")
	projectDir := filepath.Join(opts.Cwd, configDir, "skills")
	if opts.IncludeDefaults {
		add(LoadFromDir(userDir, "user"))
		add(LoadFromDir(projectDir, "project"))
	}

	for _, raw := range opts.Paths {
		path := raw
		if !filepath.IsAbs(path) {
			path = filepath.Join(opts.Cwd, path)
		}
		info, err := os.Stat(path)
		if err != nil {
			diags = append(diags, Diagnostic{Type: "warning", Message: "skill path does not exist", Path: path})
			continue
		}
		source := sourceFor(path, userDir, projectDir)
		switch {
		case info.IsDir():
			add(LoadFromDir(path, source))
		case strings.HasSuffix(path, ".md"):
			skill, d := LoadFromFile(path, source)
			if skill != nil {
				add(Result{Skills: []Skill{*skill}, Diagnostics: d})
			} else {
				diags = append(diags, d...)
			}
		default:
			diags = append(diags, Diagnostic{Type: "warning", Message: "skill path is not a markdown file", Path: path})
		}
	}

	out := make([]Skill, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return Result{Skills: out, Diagnostics: append(diags, collisions...)}
}

func sourceFor(path, userDir, projectDir string) string {
	switch {
	case isUnder(path, userDir):
		return "user"
	case isUnder(path, projectDir):
		return "project"
	default:
		return "path"
	}
}

// canonicalPath resolves symlinks so the same file reached two ways compares
// equal. An unresolvable path is its own answer — a skill that cannot be
// stat'd is a problem for the caller, not for deduplication.
func canonicalPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

func isUnder(target, root string) bool {
	if target == root {
		return true
	}
	return strings.HasPrefix(target, root+string(filepath.Separator))
}

// Find returns the skill with the given name.
func Find(list []Skill, name string) (Skill, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// FormatForPrompt renders skills for the system prompt in the Agent Skills
// XML shape (skills.ts:335-361). Skills with DisableModelInvocation are
// excluded — they are reachable only via /skill:<name>.
func FormatForPrompt(list []Skill) string {
	var visible []Skill
	for _, s := range list {
		if !s.DisableModelInvocation {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return ""
	}

	lines := []string{
		"\n\nThe following skills provide specialized instructions for specific tasks.",
		"Use the read tool to load a skill's file when the task matches its description.",
		"When a skill file references a relative path, resolve it against the skill directory (parent of SKILL.md / dirname of the path) and use that absolute path in tool commands.",
		"",
		"<available_skills>",
	}
	for _, s := range visible {
		lines = append(lines,
			"  <skill>",
			"    <name>"+escapeXML(s.Name)+"</name>",
			"    <description>"+escapeXML(s.Description)+"</description>",
			"    <location>"+escapeXML(s.FilePath)+"</location>",
			"  </skill>",
		)
	}
	lines = append(lines, "</available_skills>")
	return strings.Join(lines, "\n")
}

func escapeXML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
