package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/prompttemplate"
)

// writeProjectSkill plants a project-local skill that injects a marker into
// the system prompt if it is loaded.
func writeProjectSkill(t *testing.T, cwd, marker string) {
	t.Helper()
	dir := filepath.Join(cwd, ".tau", "skills", "injected")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: injected\ndescription: " + marker + "\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An untrusted project must not get its skills into the system prompt: that
// is instruction injection from a directory the user has not vouched for.
func TestUntrustedProjectSkillsAreNotLoaded(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	writeProjectSkill(t, cwd, "MARKER_UNTRUSTED")

	prompt := func(trusted bool) string {
		set, err := loadSettings(cwd, trusted)
		if err != nil {
			t.Fatal(err)
		}
		res := loadResources(cwd, trusted, set, Options{})
		return buildSystemPrompt(cwd, []agent.Tool{}, res.skills, Options{})
	}

	if strings.Contains(prompt(false), "MARKER_UNTRUSTED") {
		t.Error("an untrusted project's skills leaked into the system prompt")
	}
	if !strings.Contains(prompt(true), "MARKER_UNTRUSTED") {
		t.Error("a trusted project's skills should load")
	}
}

// The same gate covers prompt templates: a template is a slash command that
// expands to whatever the project wrote, which is instruction injection with
// an extra keystroke in front of it.
func TestUntrustedProjectPromptsAreNotLoaded(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	dir := filepath.Join(cwd, ".tau", "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\ndescription: injected\n---\n\nMARKER_PROMPT\n"
	if err := os.WriteFile(filepath.Join(dir, "injected.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	load := func(trusted bool) []prompttemplate.Template {
		set, err := loadSettings(cwd, trusted)
		if err != nil {
			t.Fatal(err)
		}
		return loadResources(cwd, trusted, set, Options{}).prompts
	}

	if got := load(false); len(got) != 0 {
		t.Errorf("an untrusted project's prompt templates were loaded: %+v", got)
	}
	if got := load(true); len(got) != 1 || got[0].Name != "injected" {
		t.Errorf("a trusted project's prompt templates should load, got %+v", got)
	}
}

// Without a UI to prompt with, a project carrying gated resources is denied.
func TestTrustFailsClosedWithoutUI(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	writeProjectSkill(t, cwd, "MARKER")

	out := resolveTrust(cwd, false, nil)
	if out.Trusted {
		t.Errorf("undecided project should be denied without a UI: %+v", out)
	}
}

// A directory with nothing to gate needs no decision.
func TestTrustGrantedWhenNothingIsGated(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	out := resolveTrust(cwd, false, nil)
	if !out.Trusted {
		t.Errorf("a project with no gated resources should be trusted: %+v", out)
	}
}

// The override short-circuits the decision, in both directions.
func TestTrustOverride(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	writeProjectSkill(t, cwd, "MARKER")

	yes, no := true, false
	if out := resolveTrust(cwd, false, &yes); !out.Trusted {
		t.Errorf("--approve should grant trust: %+v", out)
	}
	if out := resolveTrust(cwd, true, &no); out.Trusted {
		t.Errorf("--no-approve should deny trust: %+v", out)
	}
}

// An untrusted project must not be able to authorize itself by writing
// defaultProjectTrust into its own settings file.
func TestProjectCannotSelfAuthorize(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	writeProjectSkill(t, cwd, "MARKER")

	projectSettings := filepath.Join(cwd, ".tau", "settings.json")
	if err := os.MkdirAll(filepath.Dir(projectSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectSettings, []byte(`{"defaultProjectTrust":"always"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if out := resolveTrust(cwd, false, nil); out.Trusted {
		t.Errorf("a project authorized itself via its own settings: %+v", out)
	}
}
