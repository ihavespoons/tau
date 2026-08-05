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
