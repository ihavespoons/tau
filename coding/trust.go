package coding

import (
	"context"
	"os"

	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/settings"
	"github.com/ihavespoons/tau/trust"
)

// resolveTrust decides whether project-scoped resources (.tau/settings.json,
// .tau/skills, .tau/extensions) may load for this directory.
//
// The default comes from GLOBAL settings only. Reading it from the merged
// view would let an untrusted project authorize itself by writing
// "defaultProjectTrust": "always" into its own .tau/settings.json.
//
// Without a UI to prompt with, an undecided project is denied: trust fails
// closed.
// agentDir is tau's global state directory.
func agentDir() string { return config.AgentDir() }

// vote is consulted between the cheap checks and the saved decision. Nil skips
// the extension round entirely.
func resolveTrust(cwd string, hasUI bool, override *bool, vote func(string) *trust.Verdict) trust.Outcome {
	agentDir := config.AgentDir()

	def := trust.Ask
	if mgr, err := settings.Load(settings.Options{Cwd: cwd, AgentDir: agentDir}); err == nil {
		switch mgr.DefaultProjectTrust() {
		case settings.TrustAlways:
			def = trust.Always
		case settings.TrustNever:
			def = trust.Never
		}
	}

	home, _ := os.UserHomeDir()
	outcome, err := trust.Decide(trust.NewStore(agentDir), trust.Request{
		Cwd:           cwd,
		Override:      override,
		Default:       def,
		HasUI:         hasUI,
		ConfigDirName: config.DirName,
		HomeDir:       home,
		Vote:          vote,
	})
	if err != nil {
		// A broken trust store must not silently grant trust.
		return trust.Outcome{Trusted: false, Reason: "trust store unreadable: " + err.Error()}
	}
	return outcome
}

// ResolveTrust answers the same question for callers outside a session — the
// package subcommands, which install into .tau and must not do so for a
// checkout the user has not trusted.
//
// There is no prompt on this path, so an undecided project is denied and the
// user reaches it with --approve. A decision saved by a session is honored
// here, and one saved here is honored by the next session.
func ResolveTrust(cwd string, override *bool) trust.Outcome {
	return resolveTrust(cwd, false, override, nil)
}

// votingExtensions starts the extensions entitled to a project_trust vote and
// returns a runner holding them.
//
// The vote is taken before the saved decision (project-trust.ts:53-68), so
// these have to be running before the answer exists — which limits the
// electorate to extensions reachable without trusting the project: the -e
// flags, ~/.tau/agent/extensions, and the paths named in GLOBAL settings.
// Reading the project's settings here would let it choose who decides its own
// trust.
//
// Compiled-in extensions are deliberately not among them. They load once the
// decision is made, because tau's own bundled extensions read the answer in
// their factory — the MCP client uses it to decide whether .tau/mcp.json may
// launch a process — and voting is not a thing any of them want to do.
//
// The processes are told "untrusted" at handshake, which is true at that
// moment. The decision reaches them afterwards through the per-event context.
func votingExtensions(ctx context.Context, cwd string, mode extension.Mode, ui extension.UI, opts Options) (*extension.Runner, []string) {
	if opts.ExternalExtensions == nil {
		return nil, nil
	}

	var paths []string
	if mgr, err := settings.Load(settings.Options{Cwd: cwd, AgentDir: config.AgentDir()}); err == nil {
		if set, rerr := mgr.Resolve(); rerr == nil {
			paths = set.ExtensionPaths()
		}
	}

	exts, warnings := opts.ExternalExtensions.Load(ctx, LoadRequest{
		Cwd: cwd, Trusted: false, SettingsPaths: paths, Mode: mode,
	})
	if len(exts) == 0 {
		return nil, warnings
	}

	r := extension.NewRunner(extension.RunnerOptions{
		Mode: mode, Cwd: cwd, Trusted: false, UI: ui,
	})
	for _, e := range exts {
		_ = r.Load(e)
	}
	return r, warnings
}

// trustVote adapts the extension hook to the trust package's contract. An
// undecided vote, or no handler at all, returns nil and falls through to the
// saved decision.
func trustVote(ctx context.Context, r *extension.Runner) func(string) *trust.Verdict {
	if r == nil {
		return nil
	}
	return func(cwd string) *trust.Verdict {
		res := r.EmitProjectTrust(ctx, &extension.ProjectTrustEvent{Cwd: cwd})
		if res == nil {
			return nil
		}
		switch res.Decision {
		case extension.TrustYes:
			return &trust.Verdict{Trusted: true}
		case extension.TrustNo:
			return &trust.Verdict{Trusted: false}
		}
		// Pi's result also carries a "remember" flag that writes the decision
		// to the trust store. tau's does not: an extension quietly persisting
		// trust for a directory is not a power worth handing out for the
		// convenience of not being asked twice.
		return nil
	}
}
