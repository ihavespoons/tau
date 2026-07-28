package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/skills"
)

func snippets() map[string]string {
	return map[string]string{
		"read":  "Read file contents",
		"bash":  "Execute bash commands (ls, grep, find, etc.)",
		"edit":  "Make precise file edits",
		"write": "Create or overwrite files",
	}
}

// indexOf reports where a section starts, or -1.
func indexOf(s, sub string) int { return strings.Index(s, sub) }

func TestBuildSectionOrder(t *testing.T) {
	out := Build(Options{
		Cwd:                "/work",
		ToolSnippets:       snippets(),
		AppendSystemPrompt: "APPENDED",
		ContextFiles:       []ContextFile{{Path: "/work/AGENTS.md", Content: "project rules"}},
		Skills:             []skills.Skill{{Name: "deploy", Description: "how to deploy", FilePath: "/s/SKILL.md"}},
	})

	// Verified against system-prompt.ts:121-159.
	sections := []string{
		"You are an expert coding assistant",
		"Available tools:",
		"Guidelines:",
		"APPENDED",
		"<project_context>",
		"<available_skills>",
		"Current working directory: /work",
	}
	prev := -1
	for _, s := range sections {
		at := indexOf(out, s)
		if at == -1 {
			t.Fatalf("section %q missing from prompt:\n%s", s, out)
		}
		if at < prev {
			t.Errorf("section %q is out of order", s)
		}
		prev = at
	}
}

func TestBuildOnlyAdvertisesToolsWithSnippets(t *testing.T) {
	out := Build(Options{
		Cwd:           "/w",
		SelectedTools: []string{"read", "bash", "secret"},
		ToolSnippets:  map[string]string{"read": "Read file contents", "bash": "Run commands"},
	})
	if !strings.Contains(out, "- read: Read file contents") {
		t.Error("read should be advertised")
	}
	if strings.Contains(out, "secret") {
		t.Error("a tool without a snippet must not appear in Available tools")
	}
}

func TestBuildNoVisibleToolsSaysNone(t *testing.T) {
	out := Build(Options{Cwd: "/w", SelectedTools: []string{"read"}})
	if !strings.Contains(out, "Available tools:\n(none)") {
		t.Errorf("expected (none) placeholder, got:\n%s", out)
	}
}

func TestGuidelinesOrderAndDedupe(t *testing.T) {
	got := guidelines([]string{"bash", "read"}, []string{
		"Custom one",
		"Custom one", // duplicate
		"  ",         // blank after trim
		"Be concise in your responses", // collides with a built-in
	})
	want := []string{
		"- Use bash for file operations like ls, rg, find",
		"- Custom one",
		"- Be concise in your responses",
		"- Show file paths clearly when working with files",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d guidelines, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("guideline[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The bash-fallback guideline is suppressed when a dedicated search tool
// exists (system-prompt.ts:104).
func TestGuidelinesBashFallbackSuppressed(t *testing.T) {
	for _, tool := range []string{"grep", "find", "ls"} {
		got := strings.Join(guidelines([]string{"bash", tool}, nil), "\n")
		if strings.Contains(got, "Use bash for file operations") {
			t.Errorf("with %s present, the bash fallback guideline should be dropped", tool)
		}
	}
	got := strings.Join(guidelines([]string{"read"}, nil), "\n")
	if strings.Contains(got, "Use bash for file operations") {
		t.Error("without bash, the fallback guideline should be absent")
	}
}

func TestBuildCustomPromptSkipsToolsAndGuidelines(t *testing.T) {
	out := Build(Options{
		Cwd:          "/w",
		CustomPrompt: "You are a haiku bot.",
		ToolSnippets: snippets(),
		ContextFiles: []ContextFile{{Path: "/w/AGENTS.md", Content: "rules"}},
	})
	if !strings.HasPrefix(out, "You are a haiku bot.") {
		t.Error("custom prompt should lead")
	}
	for _, banned := range []string{"Available tools:", "Guidelines:", "expert coding assistant"} {
		if strings.Contains(out, banned) {
			t.Errorf("custom prompt must not include %q", banned)
		}
	}
	// Later sections still apply.
	if !strings.Contains(out, "<project_context>") {
		t.Error("context files should still be appended after a custom prompt")
	}
	if !strings.Contains(out, "Current working directory: /w") {
		t.Error("cwd should still be appended")
	}
}

// Skills are gated on the read tool, since reading is how a skill is loaded
// (system-prompt.ts:155).
func TestBuildSkillsRequireReadTool(t *testing.T) {
	sk := []skills.Skill{{Name: "deploy", Description: "d", FilePath: "/s/SKILL.md"}}

	with := Build(Options{Cwd: "/w", SelectedTools: []string{"read", "bash"}, Skills: sk})
	if !strings.Contains(with, "<available_skills>") {
		t.Error("skills should render when read is available")
	}
	without := Build(Options{Cwd: "/w", SelectedTools: []string{"bash"}, Skills: sk})
	if strings.Contains(without, "<available_skills>") {
		t.Error("skills must be omitted without the read tool")
	}
}

func TestBuildDocsSectionOptional(t *testing.T) {
	without := Build(Options{Cwd: "/w", ToolSnippets: snippets()})
	if strings.Contains(without, "tau documentation") {
		t.Error("docs section should be omitted when no paths are configured")
	}
	with := Build(Options{Cwd: "/w", ToolSnippets: snippets(), Docs: Docs{
		Readme: "/opt/tau/README.md", Docs: "/opt/tau/docs", Examples: "/opt/tau/examples",
	}})
	if !strings.Contains(with, "/opt/tau/README.md") {
		t.Error("docs section should appear when configured")
	}
	// It belongs inside the base prompt, before anything appended.
	if indexOf(with, "tau documentation") > indexOf(with, "Current working directory") {
		t.Error("docs section is misplaced")
	}
}

func TestBuildNormalizesWindowsCwd(t *testing.T) {
	out := Build(Options{Cwd: `C:\Users\ben\code`})
	if !strings.Contains(out, "Current working directory: C:/Users/ben/code") {
		t.Errorf("backslashes should be normalized, got tail: %q", out[len(out)-60:])
	}
}

func TestBuildMultipleContextFiles(t *testing.T) {
	out := Build(Options{Cwd: "/w", ContextFiles: []ContextFile{
		{Path: "/AGENTS.md", Content: "root"},
		{Path: "/w/AGENTS.md", Content: "leaf"},
	}})
	rootAt := indexOf(out, `path="/AGENTS.md"`)
	leafAt := indexOf(out, `path="/w/AGENTS.md"`)
	if rootAt == -1 || leafAt == -1 {
		t.Fatal("both context files should be present")
	}
	if rootAt > leafAt {
		t.Error("context files should keep the order given (outermost first)")
	}
}

func TestLoadContextFilesPrecedence(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	project := filepath.Join(root, "repo", "sub")
	for _, d := range []string{agentDir, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(agentDir, "AGENTS.md", "global")
	write(filepath.Join(root, "repo"), "AGENTS.md", "repo root")
	write(project, "CLAUDE.md", "leaf")

	files := LoadContextFiles(project, agentDir)
	if len(files) != 3 {
		t.Fatalf("got %d context files, want 3: %+v", len(files), files)
	}
	if files[0].Content != "global" {
		t.Errorf("global agent-dir file should come first, got %q", files[0].Content)
	}
	if files[1].Content != "repo root" {
		t.Errorf("ancestors should be outermost-first, got %q", files[1].Content)
	}
	if files[2].Content != "leaf" {
		t.Errorf("nearest file should be last, got %q", files[2].Content)
	}
}

// Within one directory the first candidate wins; CLAUDE.md is not also read.
func TestLoadContextFilesFirstCandidateWinsPerDir(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := LoadContextFiles(dir, "")
	if len(files) != 1 || !strings.HasSuffix(files[0].Path, "AGENTS.md") {
		t.Errorf("expected only AGENTS.md, got %+v", files)
	}
}

func TestLoadContextFilesNoneFound(t *testing.T) {
	if files := LoadContextFiles(t.TempDir(), ""); len(files) != 0 {
		t.Errorf("expected no context files, got %+v", files)
	}
}

// A directory in place of a context file must not crash discovery.
func TestLoadContextFilesIgnoresDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if files := LoadContextFiles(dir, ""); len(files) != 0 {
		t.Errorf("a directory named AGENTS.md should be skipped, got %+v", files)
	}
}
