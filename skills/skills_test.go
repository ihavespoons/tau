package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validSkill = `---
name: deploy
description: How to deploy the service
---

Run make deploy.
`

func TestLoadFromFileValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy", "SKILL.md")
	writeFile(t, path, validSkill)

	s, diags := LoadFromFile(path, "user")
	if s == nil {
		t.Fatalf("expected a skill, diagnostics: %+v", diags)
	}
	if s.Name != "deploy" || s.Description != "How to deploy the service" {
		t.Errorf("got %+v", s)
	}
	if s.BaseDir != filepath.Dir(path) {
		t.Errorf("BaseDir = %q", s.BaseDir)
	}
	if len(diags) != 0 {
		t.Errorf("unexpected diagnostics: %+v", diags)
	}
}

// Name falls back to the containing directory (skills.ts:296).
func TestLoadFromFileNameFallsBackToDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "my-skill", "SKILL.md")
	writeFile(t, path, "---\ndescription: does a thing\n---\nbody\n")

	s, _ := LoadFromFile(path, "user")
	if s == nil || s.Name != "my-skill" {
		t.Fatalf("expected name from directory, got %+v", s)
	}
}

// A missing description is fatal for the skill but must not panic.
func TestLoadFromFileMissingDescriptionSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x", "SKILL.md")
	writeFile(t, path, "---\nname: x\n---\nbody\n")

	s, diags := LoadFromFile(path, "user")
	if s != nil {
		t.Error("skill without a description must not load")
	}
	if len(diags) == 0 || !strings.Contains(diags[0].Message, "description is required") {
		t.Errorf("expected a description diagnostic, got %+v", diags)
	}
}

func TestLoadFromFileMalformedFrontmatterIsSoftError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad", "SKILL.md")
	writeFile(t, path, "---\n\tname: [unclosed\n---\nbody\n")

	s, diags := LoadFromFile(path, "user")
	if s != nil {
		t.Error("malformed frontmatter should not yield a skill")
	}
	if len(diags) == 0 {
		t.Error("malformed frontmatter should produce a diagnostic")
	}
}

func TestLoadFromFileNameValidationWarnsButLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s", "SKILL.md")
	writeFile(t, path, "---\nname: Bad--Name-\ndescription: still usable\n---\nbody\n")

	s, diags := LoadFromFile(path, "user")
	if s == nil {
		t.Fatal("an invalid name should warn, not drop the skill")
	}
	if len(diags) < 3 {
		t.Errorf("expected warnings for case, consecutive hyphens, trailing hyphen; got %+v", diags)
	}
}

func TestLoadFromFileDisableModelInvocation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s", "SKILL.md")
	writeFile(t, path, "---\nname: hidden\ndescription: d\ndisable-model-invocation: true\n---\nbody\n")

	s, _ := LoadFromFile(path, "user")
	if s == nil || !s.DisableModelInvocation {
		t.Fatalf("expected DisableModelInvocation, got %+v", s)
	}
}

// A directory holding SKILL.md is a skill root and is not descended into
// (skills.ts:163-165).
func TestLoadFromDirSkillRootStopsRecursion(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "SKILL.md"), "---\nname: outer\ndescription: outer skill\n---\n")
	writeFile(t, filepath.Join(root, "nested", "SKILL.md"), "---\nname: inner\ndescription: inner skill\n---\n")

	res := LoadFromDir(root, "user")
	if len(res.Skills) != 1 || res.Skills[0].Name != "outer" {
		t.Fatalf("expected only the outer skill, got %+v", res.Skills)
	}
}

func TestLoadFromDirRecursesAndLoadsRootMarkdown(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "loose.md"), "---\nname: loose\ndescription: a loose file\n---\n")
	writeFile(t, filepath.Join(root, "sub", "SKILL.md"), "---\nname: sub\ndescription: nested skill\n---\n")
	writeFile(t, filepath.Join(root, "sub", "extra.md"), "---\nname: extra\ndescription: should be ignored\n---\n")
	writeFile(t, filepath.Join(root, ".hidden", "SKILL.md"), "---\nname: hidden\ndescription: skipped\n---\n")
	writeFile(t, filepath.Join(root, "node_modules", "SKILL.md"), "---\nname: dep\ndescription: skipped\n---\n")

	got := map[string]bool{}
	for _, s := range LoadFromDir(root, "user").Skills {
		got[s.Name] = true
	}
	if !got["loose"] || !got["sub"] {
		t.Errorf("expected loose and sub, got %v", got)
	}
	// extra.md is a non-root .md inside a subdirectory: not loaded.
	if got["extra"] {
		t.Error("non-root .md files in subdirectories should be ignored")
	}
	if got["hidden"] || got["dep"] {
		t.Error("dot-directories and node_modules must be skipped")
	}
}

func TestLoadFromDirMissingDirIsEmpty(t *testing.T) {
	res := LoadFromDir(filepath.Join(t.TempDir(), "nope"), "user")
	if len(res.Skills) != 0 || len(res.Diagnostics) != 0 {
		t.Errorf("missing dir should load nothing, got %+v", res)
	}
}

// User skills are loaded first, so a project skill of the same name loses and
// is reported as a collision (skills.ts:410-422).
func TestLoadCollisionKeepsFirst(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "project")
	writeFile(t, filepath.Join(agentDir, "skills", "dup", "SKILL.md"), "---\nname: dup\ndescription: user version\n---\n")
	writeFile(t, filepath.Join(cwd, ".tau", "skills", "dup", "SKILL.md"), "---\nname: dup\ndescription: project version\n---\n")

	res := Load(LoadOptions{Cwd: cwd, AgentDir: agentDir, IncludeDefaults: true})
	if len(res.Skills) != 1 {
		t.Fatalf("expected 1 skill after collision, got %+v", res.Skills)
	}
	if res.Skills[0].Description != "user version" {
		t.Errorf("user skill should win, got %q", res.Skills[0].Description)
	}
	found := false
	for _, d := range res.Diagnostics {
		if d.Type == "collision" {
			found = true
		}
	}
	if !found {
		t.Error("expected a collision diagnostic")
	}
}

func TestLoadExplicitPaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "extra", "SKILL.md"), "---\nname: extra\ndescription: from a path\n---\n")

	res := Load(LoadOptions{Cwd: root, AgentDir: filepath.Join(root, "agent"), Paths: []string{"extra"}})
	if len(res.Skills) != 1 || res.Skills[0].Name != "extra" {
		t.Fatalf("expected the explicit skill, got %+v", res.Skills)
	}
	if res.Skills[0].Source != "path" {
		t.Errorf("source = %q, want path", res.Skills[0].Source)
	}
}

func TestLoadMissingPathWarns(t *testing.T) {
	root := t.TempDir()
	res := Load(LoadOptions{Cwd: root, AgentDir: root, Paths: []string{"nope"}})
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Message, "does not exist") {
		t.Errorf("expected a missing-path warning, got %+v", res.Diagnostics)
	}
}

func TestFormatForPrompt(t *testing.T) {
	out := FormatForPrompt([]Skill{
		{Name: "deploy", Description: "How to <deploy> & ship", FilePath: "/s/deploy/SKILL.md"},
		{Name: "hidden", Description: "no", FilePath: "/s/h/SKILL.md", DisableModelInvocation: true},
	})
	if !strings.Contains(out, "<available_skills>") || !strings.Contains(out, "</available_skills>") {
		t.Error("missing wrapper element")
	}
	if !strings.Contains(out, "<name>deploy</name>") {
		t.Error("missing skill name")
	}
	if !strings.Contains(out, "How to &lt;deploy&gt; &amp; ship") {
		t.Errorf("description should be XML-escaped:\n%s", out)
	}
	if strings.Contains(out, "hidden") {
		t.Error("DisableModelInvocation skills must not appear in the prompt")
	}
}

func TestFormatForPromptEmpty(t *testing.T) {
	if got := FormatForPrompt(nil); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
	hidden := []Skill{{Name: "h", DisableModelInvocation: true}}
	if got := FormatForPrompt(hidden); got != "" {
		t.Errorf("all-hidden should render empty, got %q", got)
	}
}

func TestFind(t *testing.T) {
	list := []Skill{{Name: "a"}, {Name: "b"}}
	if s, ok := Find(list, "b"); !ok || s.Name != "b" {
		t.Error("expected to find b")
	}
	if _, ok := Find(list, "z"); ok {
		t.Error("should not find z")
	}
}

// A skills directory checked into a repository sits beside build output and
// vendored trees. Scanning it must honor the ignore files already there.
func TestLoadFromDirHonoursIgnoreFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "dist/\nscratch\n")
	writeFile(t, filepath.Join(dir, "keep", "SKILL.md"), "---\ndescription: kept\n---\nbody\n")
	writeFile(t, filepath.Join(dir, "dist", "SKILL.md"), "---\ndescription: built\n---\nbody\n")
	writeFile(t, filepath.Join(dir, "scratch", "SKILL.md"), "---\ndescription: scratch\n---\nbody\n")

	res := LoadFromDir(dir, "user")
	if len(res.Skills) != 1 || res.Skills[0].Name != "keep" {
		t.Fatalf("expected only the un-ignored skill, got %+v", res.Skills)
	}
}

// A nested ignore file governs its own subtree and nothing above it, and a
// negation re-includes what an earlier pattern dropped.
func TestLoadFromDirNestedIgnoreAndNegation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "vendor", ".ignore"), "*\n!wanted\n")
	writeFile(t, filepath.Join(dir, "vendor", "wanted", "SKILL.md"), "---\ndescription: wanted\n---\nbody\n")
	writeFile(t, filepath.Join(dir, "vendor", "junk", "SKILL.md"), "---\ndescription: junk\n---\nbody\n")
	// The same name outside vendor/ must be unaffected by vendor's rules.
	writeFile(t, filepath.Join(dir, "junk", "SKILL.md"), "---\nname: outer\ndescription: outer\n---\nbody\n")

	var names []string
	for _, s := range LoadFromDir(dir, "user").Skills {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "outer,wanted" {
		t.Fatalf("got %v", names)
	}
}

// Symlinking a skill directory into a second location is how skills get shared
// between checkouts. The same file reached twice is one skill, not a clash.
func TestLoadDeduplicatesSymlinkedSkills(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	writeFile(t, filepath.Join(agentDir, "skills", "deploy", "SKILL.md"), validSkill)

	cwd := filepath.Join(root, "project")
	link := filepath.Join(cwd, ".tau", "skills", "deploy")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(agentDir, "skills", "deploy"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := Load(LoadOptions{Cwd: cwd, AgentDir: agentDir, IncludeDefaults: true})
	if len(res.Skills) != 1 {
		t.Fatalf("expected one skill, got %+v", res.Skills)
	}
	for _, d := range res.Diagnostics {
		if d.Type == "collision" {
			t.Errorf("a skill linked to itself is not a collision: %+v", d)
		}
	}
}

// Two genuinely different files sharing a name is still a collision.
func TestLoadDistinctFilesStillCollide(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	cwd := filepath.Join(root, "project")
	writeFile(t, filepath.Join(agentDir, "skills", "deploy", "SKILL.md"), validSkill)
	writeFile(t, filepath.Join(cwd, ".tau", "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: a different deploy\n---\nbody\n")

	res := Load(LoadOptions{Cwd: cwd, AgentDir: agentDir, IncludeDefaults: true})
	if len(res.Skills) != 1 {
		t.Fatalf("expected one skill, got %+v", res.Skills)
	}
	var collisions int
	for _, d := range res.Diagnostics {
		if d.Type == "collision" {
			collisions++
		}
	}
	if collisions != 1 {
		t.Errorf("expected one collision diagnostic, got %d: %+v", collisions, res.Diagnostics)
	}
}
