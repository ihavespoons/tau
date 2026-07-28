package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/config"
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

	untrusted := buildSystemPrompt(cwd, []agent.Tool{}, false, Options{})
	if strings.Contains(untrusted, "MARKER_UNTRUSTED") {
		t.Error("an untrusted project's skills leaked into the system prompt")
	}

	trusted := buildSystemPrompt(cwd, []agent.Tool{}, true, Options{})
	if !strings.Contains(trusted, "MARKER_UNTRUSTED") {
		t.Error("a trusted project's skills should load")
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
