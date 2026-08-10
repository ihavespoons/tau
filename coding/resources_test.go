package coding

import (
	"context"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/prompttemplate"
	"github.com/ihavespoons/tau/skills"
	"github.com/ihavespoons/tau/slashcmd"
)

func testSession(t *testing.T) *Session {
	t.Helper()
	return &Session{
		Prompts: []prompttemplate.Template{{
			Name: "review", Description: "review a diff",
			Content: "Review $1 carefully.", FilePath: "/p/review.md", Source: "user",
		}},
		Skills: []skills.Skill{{
			Name: "deploy", Description: "how to deploy",
			FilePath: "/s/deploy/SKILL.md", BaseDir: "/s/deploy", Source: "user",
		}},
	}
}

// Loading a template or a skill is only half the job — until they are in the
// registry they are files nobody can reach.
func TestBuildCommandsRegistersResources(t *testing.T) {
	reg := testSession(t).buildCommands()

	cmd, ok := reg.Lookup("review")
	if !ok {
		t.Fatalf("prompt template not registered; have %v", reg.Names())
	}
	res, err := cmd.Run(context.Background(), "api.go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Prompt != "Review api.go carefully." {
		t.Errorf("arguments were not substituted: %q", res.Prompt)
	}
	if res.Output != "" {
		t.Errorf("a template feeds the agent, it does not print: %q", res.Output)
	}

	cmd, ok = reg.Lookup("skill:deploy")
	if !ok {
		t.Fatalf("skill command not registered; have %v", reg.Names())
	}
	res, err = cmd.Run(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	// The skill file is named, not inlined: skills can be long, and they stay
	// out of context until the model decides it needs one.
	if !strings.Contains(res.Prompt, "/s/deploy/SKILL.md") {
		t.Errorf("skill prompt should point at the file: %q", res.Prompt)
	}
}

// A resource must not be able to take over a built-in by naming itself after
// one — /help has to keep meaning /help.
func TestResourcesCannotShadowBuiltins(t *testing.T) {
	s := testSession(t)
	s.Prompts = append(s.Prompts, prompttemplate.Template{
		Name: "help", Description: "not the real help", Content: "hijacked",
	})

	reg := s.buildCommands()
	cmd, ok := reg.Lookup("help")
	if !ok {
		t.Fatal("/help disappeared")
	}
	if cmd.Info().Source == slashcmd.SourcePrompt {
		t.Error("a prompt template shadowed the built-in /help")
	}
}
