package slashcmd

import (
	"context"
	"fmt"

	"github.com/ihavespoons/tau/prompttemplate"
	"github.com/ihavespoons/tau/skills"
)

// RegisterTemplates exposes prompt templates as slash commands. Running one
// returns its expanded body as a Prompt for the agent, not as Output.
func RegisterTemplates(r *Registry, templates []prompttemplate.Template) {
	for _, t := range templates {
		tmpl := t
		r.Register(New(Info{
			Name:         tmpl.Name,
			Description:  tmpl.Description,
			ArgumentHint: tmpl.ArgumentHint,
			Source:       SourcePrompt,
			SourceInfo:   tmpl.FilePath,
		}, func(_ context.Context, args string) (Result, error) {
			return Result{
				Prompt: prompttemplate.SubstituteArgs(tmpl.Content, prompttemplate.ParseArgs(args)),
			}, nil
		}))
	}
}

// RegisterSkills exposes skills as /skill:<name> commands.
//
// Invoking one asks the agent to read the skill file rather than inlining its
// contents: skills can be long, and Pi's design keeps them out of context
// until the model actually needs them.
func RegisterSkills(r *Registry, list []skills.Skill) {
	for _, s := range list {
		skill := s
		r.Register(New(Info{
			Name:        "skill:" + skill.Name,
			Description: skill.Description,
			Source:      SourceSkill,
			SourceInfo:  skill.FilePath,
		}, func(_ context.Context, args string) (Result, error) {
			prompt := fmt.Sprintf(
				"Use the %s skill. Read %s for its instructions and follow them.\nResolve any relative paths in that file against %s.",
				skill.Name, skill.FilePath, skill.BaseDir)
			if args != "" {
				prompt += "\n\n" + args
			}
			return Result{Prompt: prompt}, nil
		}))
	}
}
