package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/prompttemplate"
	"github.com/ihavespoons/tau/trust"
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
		mgr, set, err := loadSettings(cwd, trusted)
		if err != nil {
			t.Fatal(err)
		}
		res := loadResources(cwd, trusted, mgr, set, Options{})
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
		mgr, set, err := loadSettings(cwd, trusted)
		if err != nil {
			t.Fatal(err)
		}
		return loadResources(cwd, trusted, mgr, set, Options{}).prompts
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

	out := resolveTrust(cwd, false, nil, nil)
	if out.Trusted {
		t.Errorf("undecided project should be denied without a UI: %+v", out)
	}
}

// A directory with nothing to gate needs no decision.
func TestTrustGrantedWhenNothingIsGated(t *testing.T) {
	cwd := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	out := resolveTrust(cwd, false, nil, nil)
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
	if out := resolveTrust(cwd, false, &yes, nil); !out.Trusted {
		t.Errorf("--approve should grant trust: %+v", out)
	}
	if out := resolveTrust(cwd, true, &no, nil); out.Trusted {
		t.Errorf("--no-approve should deny trust: %+v", out)
	}
}

// The extension vote is taken between the cheap checks and the saved
// decision, so it beats a stored "no" and is beaten by an explicit override.
func TestTrustVoteOrdering(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	writeProjectSkill(t, cwd, "MARKER")

	vote := func(v *trust.Verdict) func(string) *trust.Verdict {
		return func(string) *trust.Verdict { return v }
	}

	// Undecided falls through to the store's answer, which is untrusted here.
	if out := resolveTrust(cwd, false, nil, vote(nil)); out.Trusted {
		t.Errorf("an undecided vote should not grant trust: %+v", out)
	}
	if out := resolveTrust(cwd, false, nil, vote(&trust.Verdict{Trusted: true})); !out.Trusted {
		t.Errorf("a yes vote should grant trust: %+v", out)
	}

	// A saved yes loses to a no vote: the vote is asked first.
	no := false
	if err := trust.NewStore(agentDir).Set(ctx, cwd, boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	if out := resolveTrust(cwd, false, nil, vote(&trust.Verdict{Trusted: no})); out.Trusted {
		t.Errorf("a no vote should beat a saved yes: %+v", out)
	}

	// The override beats everything, including the vote.
	yes := true
	if out := resolveTrust(cwd, false, &yes, vote(&trust.Verdict{Trusted: false})); !out.Trusted {
		t.Errorf("--approve should beat a no vote: %+v", out)
	}
}

func boolPtr(b bool) *bool { return &b }

// voteLoader is an extension loader whose one extension votes on project
// trust. The second Load returns nothing, mirroring a real loader skipping
// what it has already spawned.
type voteLoader struct {
	decision extension.TrustDecision
	votes    int
	requests []LoadRequest
}

func (l *voteLoader) Load(_ context.Context, req LoadRequest) ([]extension.Extension, []string) {
	l.requests = append(l.requests, req)
	if len(l.requests) > 1 {
		return nil, nil
	}
	return []extension.Extension{{
		Name: "voter",
		Factory: func(api *extension.API) error {
			api.OnProjectTrust(func(context.Context, *extension.ProjectTrustEvent, *extension.Context) (*extension.ProjectTrustResult, error) {
				l.votes++
				return &extension.ProjectTrustResult{Decision: l.decision}, nil
			})
			return nil
		},
	}}, nil
}

func (l *voteLoader) Invalidate() {}
func (l *voteLoader) Stop(string) {}
func (l *voteLoader) Reload(context.Context, LoadRequest) ([]extension.Extension, []string) {
	return nil, nil
}

// An extension's vote decides trust for the whole session, which means project
// resources load or do not load on its say-so.
func TestProjectTrustHookDecidesTheSession(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision extension.TrustDecision
		want     bool
	}{
		{"yes", extension.TrustYes, true},
		{"no", extension.TrustNo, false},
		// Undecided falls through, and an undecided project with no UI is
		// denied.
		{"undecided", extension.TrustUndecided, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			writeProjectSkill(t, cwd, "MARKER")

			loader := &voteLoader{decision: tc.decision}
			cs := newTestSession(t, Options{Cwd: cwd, ExternalExtensions: loader})

			if loader.votes != 1 {
				t.Errorf("the hook fired %d times, want 1", loader.votes)
			}
			if cs.Trust.Trusted != tc.want {
				t.Errorf("trusted = %v, want %v (%s)", cs.Trust.Trusted, tc.want, cs.Trust.Reason)
			}

			// The decision has to reach the project resources, not just the
			// outcome struct.
			hasSkill := false
			for _, s := range cs.Skills {
				if strings.Contains(s.Name, "injected") {
					hasSkill = true
				}
			}
			if hasSkill != tc.want {
				t.Errorf("project skill loaded = %v, want %v", hasSkill, tc.want)
			}
		})
	}
}

// The voting extensions load before the answer exists, so they are asked with
// the project untrusted; the rest load with the real decision.
func TestProjectTrustHookLoadsInTwoPhases(t *testing.T) {
	cwd := t.TempDir()
	writeProjectSkill(t, cwd, "MARKER")

	loader := &voteLoader{decision: extension.TrustYes}
	cs := newTestSession(t, Options{Cwd: cwd, ExternalExtensions: loader})

	if len(loader.requests) != 2 {
		t.Fatalf("the loader was called %d times, want 2", len(loader.requests))
	}
	if loader.requests[0].Trusted {
		t.Error("the voting phase must not claim the project is trusted — nothing has decided yet")
	}
	if loader.requests[1].Trusted != cs.Trust.Trusted {
		t.Errorf("the second phase loaded with trusted=%v, want %v",
			loader.requests[1].Trusted, cs.Trust.Trusted)
	}

	// And the decision reaches the extensions that were already running.
	if cs.Extensions == nil {
		t.Fatal("the voting runner was discarded")
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

	if out := resolveTrust(cwd, false, nil, nil); out.Trusted {
		t.Errorf("a project authorized itself via its own settings: %+v", out)
	}
}
