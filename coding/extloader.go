package coding

import (
	"context"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/extension"
)

// ExtensionLoader discovers and launches extensions that live outside the
// binary.
//
// It is an interface rather than a direct call into the subprocess host so
// that `coding` stays a library: an embedder can supply its own loader, or
// none, and the package does not drag a process supervisor in with it.
//
// Load runs after the trust decision has been made and settings have been
// merged, and never before: a project's extensions must not be launched in a
// directory the user has not trusted, and the settings that name them are
// themselves project-scoped.
type ExtensionLoader interface {
	// Load returns the extensions to register, plus any non-fatal problems to
	// show the user. A loader that cannot start one extension reports it and
	// returns the rest — a single bad file must not cost the user their
	// session.
	Load(ctx context.Context, req LoadRequest) ([]extension.Extension, []string)
	// Invalidate marks every loaded extension's captured session state stale,
	// without stopping anything.
	Invalidate()
	// Stop shuts the extensions down. reason is "exit", "reload", or "switch".
	Stop(reason string)
	// Reload stops everything and loads again, so an author can edit an
	// extension and see the new code run.
	Reload(ctx context.Context, req LoadRequest) ([]extension.Extension, []string)
}

// LoadRequest is what a loader needs to know to find extensions.
type LoadRequest struct {
	// Cwd is the session's working directory.
	Cwd string
	// Trusted reports the project-trust decision. A loader must not read the
	// project's extension directory when it is false.
	Trusted bool
	// SettingsPaths are the extension paths named in the merged settings.
	SettingsPaths []string
	// Mode is the host's UI mode, for diagnostics.
	Mode extension.Mode
	// Snapshot is the session state an out-of-process extension needs to be
	// able to answer synchronously. Pi's ExtensionAPI getters return values,
	// not promises, and an extension puts them straight into a message; a
	// loader that omits this leaves every one of them empty.
	Snapshot Snapshot
}

// Snapshot is the session state handed to an out-of-process extension at load.
type Snapshot struct {
	SessionName   string
	ModelID       string
	ModelProvider string
	ContextWindow int
	MaxTokens     int
	ThinkingLevel string
	ActiveTools   []string
	Commands      []SnapshotCommand
}

// SnapshotCommand is one entry in the command list Pi exposes via getCommands.
type SnapshotCommand struct {
	Name        string
	Description string
	Source      string
}

// ReloadExtensions restarts the out-of-process extensions and rebuilds the
// dispatch runner around them.
//
// The generation bump is the load-bearing part: every context an extension
// captured before this point now belongs to a session that is gone, and a
// handler still holding one gets ErrStale rather than a live view of state the
// old code was reasoning about.
//
// Bundled extensions are re-registered too. They cannot have changed on disk,
// but they share the runner with the reloaded ones, and rebuilding half of it
// would leave the surviving half bound to a runner nothing dispatches through.
func (s *Session) ReloadExtensions(ctx context.Context) error {
	// Resources are re-scanned whether or not extensions are configured. The
	// reason to run /reload is that something on disk changed, and an edited
	// skill is as much a reason as an edited extension.
	defer s.refreshResources()

	if s.opts.ExternalExtensions == nil && len(s.opts.Extensions) == 0 {
		return nil
	}

	if s.Extensions != nil {
		s.Extensions.EmitSessionShutdown(ctx, &extension.SessionShutdownEvent{Reason: "reload"})
		s.Extensions.Invalidate()
	}

	loaded := append([]extension.Extension{}, s.opts.Extensions...)
	if s.opts.ExternalExtensions != nil {
		req := LoadRequest{
			Cwd: s.Cwd, Trusted: s.Trust.Trusted,
			SettingsPaths: s.Settings.ExtensionPaths(), Mode: s.opts.Mode,
			Snapshot: s.Snapshot(),
		}
		ext, warnings := s.opts.ExternalExtensions.Reload(ctx, req)
		loaded = append(loaded, ext...)
		s.Warnings = append(s.Warnings, warnings...)
	}

	runner := extension.NewRunner(extension.RunnerOptions{
		Mode: s.opts.Mode, Cwd: s.Cwd, Trusted: s.Trust.Trusted, UI: s.UI,
	})
	for _, e := range loaded {
		_ = runner.Load(e)
	}
	runner.Bind(runtimeAdapter{s})
	s.Extensions = runner

	// Tools are rebuilt from the built-in set rather than patched. A tool the
	// old extension registered must disappear when the new code no longer
	// registers it, and patching the live list would leave it callable by a
	// process that has exited.
	s.mu.Lock()
	s.allTools = append(append([]agent.Tool{}, s.builtinTools...), runner.Tools()...)
	next := append([]agent.Tool{}, s.allTools...)
	s.mu.Unlock()

	s.Agent.SetTools(next)
	runner.EmitSessionStart(ctx, &extension.SessionStartEvent{
		SessionPath: s.Path, Cwd: s.Cwd, Resumed: true,
	})
	return nil
}

// ExtensionNames lists the loaded extensions, for /reload and diagnostics.
func (s *Session) ExtensionNames() []string {
	if s.Extensions == nil {
		return nil
	}
	return s.Extensions.Names()
}

// Snapshot describes the session for an out-of-process extension.
//
// It is taken at the moment it is asked for rather than cached: a reload
// happens mid-session, and handing the new process the state from startup
// would make its first answers wrong in a way nothing would report.
func (s *Session) Snapshot() Snapshot {
	snap := Snapshot{}
	if s.Model != nil {
		snap.ModelID = s.Model.ID
		snap.ModelProvider = string(s.Model.Provider)
		snap.ContextWindow = s.Model.ContextWindow
		snap.MaxTokens = s.Model.MaxTokens
	}
	if s.Agent != nil {
		snap.ThinkingLevel = string(s.Agent.ThinkingLevel())
		snap.ActiveTools = s.ToolNames()
	}
	if s.Session != nil {
		snap.SessionName = (runtimeAdapter{s}).SessionName()
	}
	if s.Commands != nil {
		for _, info := range s.Commands.List() {
			snap.Commands = append(snap.Commands, SnapshotCommand{
				Name: info.Name, Description: info.Description, Source: string(info.Source),
			})
		}
	}
	return snap
}
