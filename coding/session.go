// Package coding wires the runtime pieces into a working coding agent:
// provider + agent loop + tools + session persistence. It is the seam the
// CLI modes sit on, and the precursor to P3's full orchestrator.
package coding

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env"
	"github.com/ihavespoons/tau/agent/env/osenv"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/ai/provider"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/prompt"
	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/skills"
	"github.com/ihavespoons/tau/tools"
)

// Options configures a coding session.
type Options struct {
	Cwd           string
	ModelID       string
	ThinkingLevel ai.ModelThinkingLevel
	SystemPrompt  string
	// NoTools disables tool use entirely.
	NoTools bool
	// NoSession skips persistence (useful for one-shot runs).
	NoSession bool
	// AppendSystemPrompt is appended after the built system prompt.
	AppendSystemPrompt string
	// NoSkills disables skill discovery.
	NoSkills bool
	// Extensions are loaded before the session starts, in order.
	Extensions []extension.Extension
	// Mode is reported to extensions so they can degrade gracefully.
	Mode extension.Mode
	// Resume opens the most recent session for Cwd instead of creating one.
	Resume bool
	// SessionPath opens a specific session file.
	SessionPath string
}

// Session is a running coding agent bound to a persisted session file.
type Session struct {
	Agent   *agent.Agent
	Env     *osenv.OSEnv
	Model   *ai.Model
	Session *session.Session
	Path    string
	// Extensions dispatches hooks; nil when no extensions are loaded.
	Extensions *extension.Runner

	// allTools is the full registered set, including tools an extension has
	// deactivated — SetActiveTools selects from here.
	allTools []agent.Tool
	repo     session.Repo
}

// envExecOptions is the default shell configuration for extension-issued
// commands.
func envExecOptions() env.ExecOptions { return env.ExecOptions{Timeout: 2 * time.Minute} }

// buildSystemPrompt assembles the run's system prompt from the active tools'
// declared snippets and guidelines, the project's AGENTS.md/CLAUDE.md files,
// and any discovered skills.
func buildSystemPrompt(cwd string, toolset []agent.Tool, opts Options) string {
	po := prompt.Options{
		CustomPrompt:       opts.SystemPrompt,
		AppendSystemPrompt: opts.AppendSystemPrompt,
		Cwd:                cwd,
		ToolSnippets:       map[string]string{},
	}
	for _, t := range toolset {
		d := t.Def()
		po.SelectedTools = append(po.SelectedTools, d.Name)
		if d.PromptSnippet != "" {
			po.ToolSnippets[d.Name] = d.PromptSnippet
		}
		po.PromptGuidelines = append(po.PromptGuidelines, d.PromptGuidelines...)
	}

	po.ContextFiles = prompt.LoadContextFiles(cwd, config.AgentDir())

	if !opts.NoSkills {
		res := skills.Load(skills.LoadOptions{
			Cwd: cwd, AgentDir: config.AgentDir(), IncludeDefaults: true,
		})
		po.Skills = res.Skills
	}

	return prompt.Build(po)
}

// New builds a coding session.
func New(ctx context.Context, opts Options) (*Session, error) {
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolving working directory: %w", err)
		}
	}

	e, err := osenv.New(osenv.Options{Cwd: cwd})
	if err != nil {
		return nil, err
	}

	store := auth.NewFileStore(config.AuthPath())
	p := provider.Anthropic(store, auth.OSContext{})
	modelID := opts.ModelID
	if modelID == "" {
		modelID = DefaultModel
	}
	model := p.Model(modelID)
	if model == nil {
		return nil, fmt.Errorf("unknown model %q (try `tau models`)", modelID)
	}

	var toolset []agent.Tool
	if !opts.NoTools {
		toolset = tools.CodingTools(e)
	}

	systemPrompt := buildSystemPrompt(cwd, toolset, opts)

	cs := &Session{Env: e, Model: model}

	// Restore transcript from disk when resuming.
	var restored []ai.Message
	if !opts.NoSession {
		repo := session.NewJSONLRepo(config.SessionsDir())
		cs.repo = repo

		var sess *session.Session
		var meta session.Metadata
		switch {
		case opts.SessionPath != "":
			meta = session.Metadata{Path: opts.SessionPath, Cwd: cwd}
			sess, err = repo.Open(ctx, meta)
		case opts.Resume:
			metas, lerr := repo.List(ctx, cwd)
			if lerr != nil {
				return nil, lerr
			}
			if len(metas) == 0 {
				return nil, fmt.Errorf("no previous session for %s", cwd)
			}
			meta = metas[0] // most recent first
			sess, err = repo.Open(ctx, meta)
		default:
			sess, err = repo.Create(ctx, session.CreateSessionOptions{Cwd: cwd})
			if sess != nil {
				meta, _ = sess.Metadata(ctx)
			}
		}
		if err != nil {
			return nil, err
		}
		cs.Session = sess
		cs.Path = meta.Path

		if opts.Resume || opts.SessionPath != "" {
			sctx, berr := sess.BuildContext(ctx)
			if berr != nil {
				return nil, berr
			}
			restored = session.ConvertToLLM(sctx.Messages)
		}
	}

	// Load extensions, then let them contribute tools before the agent is
	// built. A failing extension is reported but never blocks startup.
	mode := opts.Mode
	if mode == "" {
		mode = extension.ModePrint
	}
	if len(opts.Extensions) > 0 {
		cs.Extensions = extension.NewRunner(extension.RunnerOptions{
			Mode: mode, Cwd: cwd, Trusted: true,
		})
		for _, e := range opts.Extensions {
			_ = cs.Extensions.Load(e)
		}
		toolset = append(toolset, cs.Extensions.Tools()...)
	}
	cs.allTools = toolset

	loopCfg := agent.LoopConfig{
		ConvertToLLM: func(msgs []ai.Message) ([]ai.Message, error) {
			return session.ConvertToLLM(msgs), nil
		},
	}
	wireExtensions(&loopCfg, cs.Extensions)

	cs.Agent = agent.NewAgent(agent.Options{
		SystemPrompt:  systemPrompt,
		Model:         model,
		ThinkingLevel: opts.ThinkingLevel,
		Tools:         toolset,
		Messages:      restored,
		Stream:        p.StreamSimple,
		Config:        loopCfg,
	})

	// Persist every message the loop produces.
	if cs.Session != nil {
		cs.Agent.Subscribe(cs.persistSink)
	}

	if cs.Extensions != nil {
		cs.Extensions.Bind(runtimeAdapter{cs})
		cs.Extensions.SetSystemPrompt(systemPrompt)
		if sink := extensionSink(cs.Extensions); sink != nil {
			cs.Agent.Subscribe(sink)
		}
		cs.Extensions.EmitSessionStart(ctx, &extension.SessionStartEvent{
			SessionPath: cs.Path, Cwd: cwd, Resumed: opts.Resume || opts.SessionPath != "",
		})
	}

	return cs, nil
}

// Close emits session shutdown to extensions.
func (s *Session) Close(ctx context.Context, reason string) {
	if s.Extensions != nil {
		s.Extensions.EmitSessionShutdown(ctx, &extension.SessionShutdownEvent{Reason: reason})
	}
}

// DefaultModel is tau's default until settings land in P3.
const DefaultModel = "claude-sonnet-5"

// persistSink appends completed messages to the session file. Streaming
// updates are skipped: only message_end carries a final message.
func (s *Session) persistSink(ctx context.Context, ev agent.Event) error {
	if ev.Type != agent.EventMessageEnd || ev.Message == nil {
		return nil
	}
	if _, err := s.Session.AppendMessage(ctx, ev.Message); err != nil {
		return fmt.Errorf("persisting %s message: %w", ev.Message.Role(), err)
	}
	return nil
}

// Prompt runs one agent loop.
func (s *Session) Prompt(ctx context.Context, text string) ([]ai.Message, error) {
	return s.Agent.Prompt(ctx, ai.UserMessage{
		Content:   ai.UserContent{Text: text},
		Timestamp: nowMillis(),
	})
}

// Usage sums token usage across the transcript.
func (s *Session) Usage() ai.Usage {
	var total ai.Usage
	for _, m := range s.Agent.Messages() {
		am, ok := m.(ai.AssistantMessage)
		if !ok {
			continue
		}
		total.Input += am.Usage.Input
		total.Output += am.Usage.Output
		total.CacheRead += am.Usage.CacheRead
		total.CacheWrite += am.Usage.CacheWrite
		total.TotalTokens += am.Usage.TotalTokens
		total.Cost.Input += am.Usage.Cost.Input
		total.Cost.Output += am.Usage.Cost.Output
		total.Cost.CacheRead += am.Usage.Cost.CacheRead
		total.Cost.CacheWrite += am.Usage.Cost.CacheWrite
		total.Cost.Total += am.Usage.Cost.Total
	}
	return total
}

// ToolNames lists the active tools.
func (s *Session) ToolNames() []string {
	var names []string
	for _, t := range s.Agent.Tools() {
		names = append(names, t.Def().Name)
	}
	return names
}

// Describe renders a one-line summary for status output.
func (s *Session) Describe() string {
	parts := []string{s.Model.ID}
	if names := s.ToolNames(); len(names) > 0 {
		parts = append(parts, "tools: "+strings.Join(names, ","))
	}
	if s.Path != "" {
		parts = append(parts, "session: "+s.Path)
	}
	return strings.Join(parts, "  ")
}

func nowMillis() int64 { return time.Now().UnixMilli() }
