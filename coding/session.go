// Package coding wires the runtime pieces into a working coding agent:
// provider + agent loop + tools + session persistence. It is the seam the
// CLI modes sit on, and the precursor to P3's full orchestrator.
package coding

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env"
	"github.com/ihavespoons/tau/agent/env/osenv"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/ai/auth"
	"github.com/ihavespoons/tau/ai/provider"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/models"
	"github.com/ihavespoons/tau/pkgmgr"
	"github.com/ihavespoons/tau/prompt"
	"github.com/ihavespoons/tau/prompttemplate"
	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/settings"
	"github.com/ihavespoons/tau/skills"
	"github.com/ihavespoons/tau/slashcmd"
	"github.com/ihavespoons/tau/tools"
	"github.com/ihavespoons/tau/trust"
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
	// TrustOverride forces the project-trust decision (--approve/--no-approve).
	TrustOverride *bool
	// Extensions are loaded before the session starts, in order.
	Extensions []extension.Extension
	// ExternalExtensions discovers and launches extensions that live outside
	// the binary. Nil loads only the ones compiled in.
	ExternalExtensions ExtensionLoader
	// Mode is reported to extensions so they can degrade gracefully.
	Mode extension.Mode
	// Resume opens the most recent session for Cwd instead of creating one.
	Resume bool
	// SessionPath opens a specific session file.
	SessionPath string
	// UI is the host's interactive surface, handed to extensions and to the
	// built-in commands that need to ask the user something. Nil is headless.
	UI extension.UI
	// Changelog is the release notes /changelog shows, as a Keep-a-Changelog
	// document. Empty means the binary ships none, and the command says so.
	// cmd/tau passes the copy embedded at the repository root.
	Changelog string
	// Interactive supplies the built-in commands that need dialogs. The host
	// may pass a value whose session pointer is filled in after New returns —
	// nothing calls it during construction.
	Interactive slashcmd.Interactive
}

// Session is a running coding agent bound to a persisted session file.
type Session struct {
	Agent   *agent.Agent
	Env     *osenv.OSEnv
	Model   *ai.Model
	Session *session.Session
	Path    string
	// Cwd is the directory the session is bound to.
	Cwd string
	// Extensions dispatches hooks; nil when no extensions are loaded.
	Extensions *extension.Runner
	// Trust records whether project-scoped resources were allowed to load.
	Trust trust.Outcome
	// Models is the composed provider catalog: built-ins plus models.json.
	Models *models.Registry
	// Settings is the merged global+project configuration.
	Settings *settings.Resolved
	// setMgr is the scope-aware view behind Settings, kept because package
	// entries can only be read per scope.
	setMgr *settings.Manager
	// pkgs are the resource files installed packages contribute. Rescanned
	// with the rest of the resources, so installing a package and reloading
	// picks it up.
	pkgs packagePaths
	// Skills are the discovered Agent Skills. They appear in the system prompt
	// and, unless disabled, as /skill:<name> commands.
	Skills []skills.Skill
	// Prompts are the discovered prompt templates, each registered as a slash
	// command that expands to its body.
	Prompts []prompttemplate.Template
	// Commands is the slash-command registry for this session.
	Commands *slashcmd.Registry
	// UI is the host's interactive surface; never nil.
	UI extension.UI
	// Warnings are non-fatal startup problems — a malformed models.json, say.
	// The session runs regardless; the host decides whether to show them.
	Warnings []string

	// mu guards allTools, which an extension may mutate from its own
	// goroutine long after startup.
	mu sync.Mutex
	// builtinTools is the set tau itself provides, kept so a reload can
	// rebuild the tool list from a known base instead of unpicking what an
	// extension added.
	builtinTools []agent.Tool
	// allTools is the full registered set, including tools an extension has
	// deactivated — SetActiveTools selects from here.
	allTools []agent.Tool
	repo     session.Repo
	// sessionID identifies the conversation to providers that key a cache or
	// a backend affinity off it. It tracks Session across switches, so it is
	// kept here rather than read back from disk per request.
	sessionID string
	store     auth.CredentialStore
	opts      Options
}

// envExecOptions is the default shell configuration for extension-issued
// commands.
func envExecOptions() env.ExecOptions { return env.ExecOptions{Timeout: 2 * time.Minute} }

// resources are the user-authored assets discovered for a session: skills and
// prompt templates. They are loaded once, because both the system prompt and
// the command registry need them and scanning twice can disagree.
type resources struct {
	skills   []skills.Skill
	prompts  []prompttemplate.Template
	packages packagePaths
	warnings []string
}

// loadResources discovers skills and prompt templates.
//
// Project-scoped discovery is behind the trust gate. A skill injects text into
// the system prompt and a prompt template becomes a slash command that expands
// to arbitrary instructions, so an untrusted checkout gets neither — otherwise
// cloning a repository would be enough to steer the agent.
func loadResources(cwd string, trusted bool, mgr *settings.Manager, set *settings.Resolved, opts Options) resources {
	scanCwd := cwd
	if !trusted {
		scanCwd = ""
	}

	res := resources{packages: loadPackages(mgr, cwd, trusted)}
	res.warnings = append(res.warnings, res.packages.warnings...)

	if !opts.NoSkills {
		loaded := skills.Load(skills.LoadOptions{
			Cwd: scanCwd, AgentDir: config.AgentDir(), IncludeDefaults: true,
			// Paths declared in settings come first: first registration wins a
			// name collision, so what the user wrote by hand beats what a
			// package happened to ship under the same name.
			Paths: append(set.SkillPaths(), res.packages.get(pkgmgr.TypeSkills)...),
		})
		res.skills = loaded.Skills
		for _, d := range loaded.Diagnostics {
			res.warnings = append(res.warnings, fmt.Sprintf("skill %s: %s", d.Path, d.Message))
		}
	}

	res.prompts = prompttemplate.Load(prompttemplate.LoadOptions{
		Cwd: scanCwd, AgentDir: config.AgentDir(), IncludeDefaults: true,
		Paths: append(set.PromptPaths(), res.packages.get(pkgmgr.TypePrompts)...),
	})
	return res
}

// refreshResources re-scans skills and prompt templates and rebuilds anything
// derived from them: the command registry and the system prompt's skill list.
//
// The trust decision is not revisited. It was made for this directory at
// startup, and a rescan is not the moment to re-ask a question the user has
// already answered.
func (s *Session) refreshResources() {
	res := loadResources(s.Cwd, s.Trust.Trusted, s.setMgr, s.Settings, s.opts)
	s.Skills, s.Prompts, s.pkgs = res.skills, res.prompts, res.packages
	s.Warnings = append(s.Warnings, res.warnings...)

	s.mu.Lock()
	toolset := append([]agent.Tool{}, s.allTools...)
	s.mu.Unlock()

	// Rebuilt rather than patched, for the same reason the tool list is: a
	// command whose skill file was deleted has to stop being callable.
	s.Commands = s.buildCommands()

	if s.Agent != nil {
		sp := buildSystemPrompt(s.Cwd, toolset, res.skills, s.opts)
		s.Agent.SetSystemPrompt(sp)
		if s.Extensions != nil {
			s.Extensions.SetSystemPrompt(sp)
		}
	}
}

// ExtensionPaths are the extension entry points to load: the ones named in
// settings first, then the ones installed packages contribute.
func (s *Session) ExtensionPaths() []string {
	return append(s.Settings.ExtensionPaths(), s.pkgs.get(pkgmgr.TypeExtensions)...)
}

// ThemePaths are the theme files to load, from settings and from packages.
func (s *Session) ThemePaths() []string {
	return append(s.Settings.ThemePaths(), s.pkgs.get(pkgmgr.TypeThemes)...)
}

// buildSystemPrompt assembles the run's system prompt from the active tools'
// declared snippets and guidelines, the project's AGENTS.md/CLAUDE.md files,
// and any discovered skills.
func buildSystemPrompt(cwd string, toolset []agent.Tool, skillList []skills.Skill, opts Options) string {
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
	po.Skills = skillList

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

	ui := opts.UI
	if ui == nil {
		ui = extension.NoUI{}
	}
	mode := opts.Mode
	if mode == "" {
		mode = extension.ModePrint
	}

	// Trust is decided before anything project-scoped is read, because the
	// project's own settings must not be able to influence the decision.
	//
	// Extensions vote first, so the ones entitled to vote are started first;
	// everything else waits for the answer. See votingExtensions.
	extRunner, voteWarnings := votingExtensions(ctx, cwd, mode, ui, opts)
	tr := resolveTrust(cwd, mode == extension.ModeTUI, opts.TrustOverride, trustVote(ctx, extRunner))
	if extRunner != nil {
		extRunner.SetTrusted(tr.Trusted)
	}

	setMgr, set, err := loadSettings(cwd, tr.Trusted)
	if err != nil {
		return nil, err
	}

	store := auth.NewFileStore(config.AuthPath())
	reg, modelWarnings, err := BuildRegistry(store)
	if err != nil {
		return nil, err
	}

	model, thinking, err := resolveModel(reg, opts, set)
	if err != nil {
		return nil, err
	}

	// The session exists before the tools do, because the bash tool reads the
	// session's own metadata (id, file, model) back out of it at call time.
	cs := &Session{
		Env: e, Model: model, Trust: tr, Cwd: cwd,
		Models: reg, Settings: set, setMgr: setMgr, store: store, opts: opts,
	}

	var toolset []agent.Tool
	if !opts.NoTools {
		toolset = tools.CodingTools(e, cs.sessionEnv)
	}

	res := loadResources(cwd, tr.Trusted, setMgr, set, opts)
	systemPrompt := buildSystemPrompt(cwd, toolset, res.skills, opts)

	cs.UI = ui
	cs.Extensions = extRunner
	cs.Skills, cs.Prompts, cs.pkgs = res.skills, res.prompts, res.packages
	cs.Warnings = append(modelWarnings, res.warnings...)
	cs.Warnings = append(cs.Warnings, voteWarnings...)

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
		cs.sessionID = meta.ID

		if opts.Resume || opts.SessionPath != "" {
			sctx, berr := sess.BuildContext(ctx)
			if berr != nil {
				return nil, berr
			}
			restored = session.ConvertToLLM(sctx.Messages)
		}
	}

	// Load the rest of the extensions, then let them contribute tools before
	// the agent is built. A failing extension is reported but never blocks
	// startup.
	//
	// "The rest" because the ones entitled to a project_trust vote are already
	// running: the loader skips what it has spawned, so this call adds the
	// project's extensions and nothing else runs twice.
	cs.builtinTools = append([]agent.Tool{}, toolset...)

	loadedExts := append([]extension.Extension{}, opts.Extensions...)
	if opts.ExternalExtensions != nil {
		// The snapshot is partial here on purpose: extensions load before the
		// agent and the command registry exist, because they contribute tools
		// and commands to both. What is knowable now is sent, and the shim's
		// mirror catches up from the events that follow.
		snap := Snapshot{ThinkingLevel: string(thinking)}
		if model != nil {
			snap.ModelID, snap.ModelProvider = model.ID, string(model.Provider)
			snap.ContextWindow, snap.MaxTokens = model.ContextWindow, model.MaxTokens
		}
		ext, warnings := opts.ExternalExtensions.Load(ctx, LoadRequest{
			Cwd: cwd, Trusted: tr.Trusted, SettingsPaths: cs.ExtensionPaths(),
			Mode: mode, Snapshot: snap,
		})
		loadedExts = append(loadedExts, ext...)
		cs.Warnings = append(cs.Warnings, warnings...)
	}
	if len(loadedExts) > 0 {
		if cs.Extensions == nil {
			cs.Extensions = extension.NewRunner(extension.RunnerOptions{
				Mode: mode, Cwd: cwd, Trusted: tr.Trusted, UI: ui,
			})
		}
		for _, e := range loadedExts {
			_ = cs.Extensions.Load(e)
		}
	}
	if cs.Extensions != nil {
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
		ThinkingLevel: thinking,
		Tools:         toolset,
		Messages:      restored,
		Stream:        cs.stream,
		Config:        loopCfg,
	})
	cs.Agent.SteeringMode = agent.QueueMode(set.SteeringMode())
	cs.Agent.FollowUpMode = agent.QueueMode(set.FollowUpMode())

	// Commands need the built agent, so the registry is assembled last.
	cs.Commands = cs.buildCommands()

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

// Close emits session shutdown to extensions and stops the ones running in
// their own processes. Skipping the second half would leak a process per
// session.
func (s *Session) Close(ctx context.Context, reason string) {
	if s.Extensions != nil {
		s.Extensions.EmitSessionShutdown(ctx, &extension.SessionShutdownEvent{Reason: reason})
	}
	if s.opts.ExternalExtensions != nil {
		s.opts.ExternalExtensions.Stop(reason)
	}
}

// DefaultModel is the model tau picks when neither the caller nor settings
// name one.
//
// It is provider-qualified deliberately. A bare id is ambiguous once the
// compiled catalog spans every provider — a dozen of them resell
// claude-sonnet-5 — and the resolver's tie-break is a sort over ids, which
// would silently start the session on whichever reseller happened to sort
// highest. A user typing a bare id still gets that behaviour, which is Pi's;
// tau's own default must not depend on it.
const DefaultModel = "anthropic/claude-sonnet-5"

// loadSettings reads the merged configuration. The project scope is gated on
// the trust decision: an untrusted directory's settings.json is not read.
//
// The manager is returned alongside the merged view because package entries
// have to be read per scope — the merge cannot say which file an entry came
// from, and that is what decides where its package lives on disk.
func loadSettings(cwd string, trusted bool) (*settings.Manager, *settings.Resolved, error) {
	mgr, err := settings.Load(settings.Options{
		Cwd: cwd, AgentDir: config.AgentDir(), ProjectTrusted: trusted,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("loading settings: %w", err)
	}
	res, err := mgr.Resolve()
	if err != nil {
		return nil, nil, err
	}
	return mgr, res, nil
}

// BuildRegistry composes the compiled provider catalog with ~/.tau/models.json.
//
// A missing or malformed models.json is not fatal: the built-ins still work,
// which matters because the file is hand-edited and losing every provider over
// one stray comma would be the worse outcome. The problem is returned as a
// warning rather than printed, so the caller decides where it surfaces.
func BuildRegistry(store auth.CredentialStore) (*models.Registry, []string, error) {
	builtins := provider.Builtins(store, auth.OSContext{})
	builtins = append(builtins, radiusProvider(store))

	var warnings []string
	cfg, err := models.LoadConfig(config.ModelsPath())
	if err != nil {
		warnings = append(warnings, err.Error())
		cfg = nil
	}

	// The deps are what let a models.json-declared provider authenticate and
	// stream, rather than merely appear in the catalog.
	reg, err := models.NewRegistry(builtins, cfg, models.Deps{
		Store: store, Env: auth.OSContext{},
	})
	return reg, warnings, err
}

// radiusProvider builds the Radius gateway provider from its cached catalog.
//
// Radius publishes no static model list — what a user can reach is a property
// of their account — so tau carries whatever the last successful fetch found
// and RefreshRadiusCatalog updates it. Before the first login the provider
// exists with an empty catalog, which is honest: `tau models` shows the
// provider, and there is nothing under it until you sign in.
func radiusProvider(store auth.CredentialStore) *provider.Provider {
	opts := provider.RadiusOptions{Gateway: config.RadiusGateway()}
	if entry := models.NewCatalogStore(config.ModelsStorePath()).Read(provider.RadiusProviderID); entry != nil {
		opts.Models = entry.Models
	}
	return provider.Radius(store, auth.OSContext{}, opts)
}

// RefreshRadiusCatalog fetches the gateway's model list and caches it.
//
// It runs after a login rather than at startup: it is a network round trip, and
// making every `tau` invocation wait on a gateway would slow down sessions that
// have nothing to do with Radius. The moment a user signs in is when they are
// online, waiting, and expecting setup to happen.
func RefreshRadiusCatalog(ctx context.Context, store auth.CredentialStore) (*provider.RadiusCatalog, error) {
	opts := provider.RadiusOptions{Gateway: config.RadiusGateway()}

	// The token is what makes the catalog the user's own; without one the
	// gateway may still publish a public list, so a missing credential is not
	// a reason to skip the fetch.
	apiKey := ""
	if cred, err := store.Read(ctx, provider.RadiusProviderID); err == nil && cred != nil {
		if cred.OAuth != nil {
			apiKey = cred.OAuth.Access
		} else {
			apiKey = cred.Key
		}
	}

	catalog, err := provider.FetchRadiusCatalog(ctx, opts, apiKey)
	if err != nil {
		return nil, err
	}
	if err := models.NewCatalogStore(config.ModelsStorePath()).
		Write(ctx, provider.RadiusProviderID, catalog.Models); err != nil {
		return catalog, err
	}
	return catalog, nil
}

// resolveModel picks the model and thinking level for the run: explicit
// option, then settings, then tau's default.
func resolveModel(reg *models.Registry, opts Options, set *settings.Resolved) (*ai.Model, ai.ModelThinkingLevel, error) {
	spec := opts.ModelID
	if spec == "" {
		spec = set.DefaultModel()
	}
	if spec == "" {
		spec = DefaultModel
	}

	match, err := reg.Resolve(spec)
	if err != nil {
		return nil, "", fmt.Errorf("%w (try `tau models`)", err)
	}

	// Precedence for thinking: the flag, then a ":level" suffix on the model
	// spec, then settings.
	level := opts.ThinkingLevel
	if level == "" {
		level = ai.ModelThinkingLevel(match.ThinkingLevel)
	}
	if level == "" {
		level = ai.ModelThinkingLevel(set.DefaultThinkingLevel())
	}
	return match.Model, ai.ClampThinkingLevel(match.Model, level), nil
}

// stream dispatches to whichever provider serves the model in play. Resolving
// per call rather than binding one provider at construction is what makes
// mid-session model switching work across providers.
func (s *Session) stream(ctx context.Context, model *ai.Model, c ai.Context, opts *ai.SimpleStreamOptions) *ai.MessageStream {
	p := s.Models.ProviderFor(model)
	if p == nil || p.StreamSimple == nil {
		return errorStream(model, fmt.Errorf("no provider configured for %s", model.Provider))
	}
	// Attached here rather than at construction because a session switch
	// changes it: providers use this to key a prompt cache and to pin a
	// conversation to one backend, and a stale id silently costs cache hits.
	if opts != nil && opts.SessionID == "" {
		opts.SessionID = s.sessionID
	}
	return p.StreamSimple(ctx, model, c, opts)
}

// errorStream honors the never-throw contract: a configuration failure is a
// terminal error event, not an out-of-band error.
func errorStream(model *ai.Model, err error) *ai.MessageStream {
	st := ai.NewMessageStream()
	msg := &ai.AssistantMessage{Content: ai.ContentList{}}
	if model != nil {
		msg.Api, msg.Provider, msg.Model = model.Api, model.Provider, model.ID
	}
	st.Push(ai.Event{
		Type:   ai.EventError,
		Reason: ai.StopError,
		Error:  ai.ErrorMessage(msg, ai.StopError, err.Error()),
	})
	return st
}

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

// Prompt runs one agent loop, compacting around it if the context needs it.
//
// The check happens twice, because there are two ways to be over the window and
// only one of them is visible in advance. Before the turn, the running estimate
// says whether the conversation has outgrown its reserve. After it, a provider
// that rejected the request for length says so in a way no estimate could have
// predicted — a cache-heavy turn, an unusually long tool result — and that
// rejection is recoverable exactly once.
func (s *Session) Prompt(ctx context.Context, text string) ([]ai.Message, error) {
	// A compaction that fails is reported through the warnings, not by
	// refusing the turn: the provider's own answer is a better diagnosis than
	// tau declining pre-emptively, and it may well succeed.
	if _, err := s.MaybeCompact(ctx); err != nil {
		s.Warnings = append(s.Warnings, "compaction failed: "+err.Error())
	}

	msg := ai.UserMessage{Content: ai.UserContent{Text: text}, Timestamp: nowMillis()}
	out, err := s.Agent.Prompt(ctx, msg)
	if err != nil || !s.overflowed(out) {
		return out, err
	}

	// The turn was rejected for length. Compacting and retrying is the whole
	// reason overflow is detected at all; if the compaction itself fails there
	// is nothing further to try, so the original rejection is what the user
	// should see.
	compacted, cerr := s.Compact(ctx, "")
	if cerr != nil || compacted == nil {
		return out, err
	}
	return s.Agent.Prompt(ctx, ai.UserMessage{
		Content:   ai.UserContent{Text: text},
		Timestamp: nowMillis(),
	})
}

// overflowed reports whether a turn ended because the request did not fit.
func (s *Session) overflowed(messages []ai.Message) bool {
	window := 0
	if s.Model != nil {
		window = s.Model.ContextWindow
	}
	for i := len(messages) - 1; i >= 0; i-- {
		switch m := messages[i].(type) {
		case ai.AssistantMessage:
			return ai.IsContextOverflow(&m, window)
		case *ai.AssistantMessage:
			return ai.IsContextOverflow(m, window)
		}
	}
	return false
}

// FormatUsage renders a usage summary for a human.
//
// The cache figures appear only when there are any, but when there are they
// matter more than the input count: on a cached turn the input number is the
// small remainder, and printing it alone makes a working cache look like a
// broken token counter.
func FormatUsage(u ai.Usage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d in / %d out tokens", u.Input, u.Output)
	if u.CacheRead > 0 || u.CacheWrite > 0 {
		fmt.Fprintf(&b, " (cache %d read / %d write)", u.CacheRead, u.CacheWrite)
	}
	fmt.Fprintf(&b, ", $%.4f", u.Cost.Total)
	return b.String()
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
