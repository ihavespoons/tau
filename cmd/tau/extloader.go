package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extension/wire"
	"github.com/ihavespoons/tau/internal/exthost"
)

// subprocessLoader launches the extensions that are not compiled in.
//
// It is the one place in the binary that knows how an extension on disk turns
// into a running process, which keeps `coding` a library rather than a process
// supervisor.
type subprocessLoader struct {
	// flags are -e values, which take precedence over everything discovered.
	flags []string
	mgr   *exthost.Manager
	// diagnostics collects load failures and extension warnings so the host
	// can show them once the session is up.
	diagnostics []string
	// onLog forwards an extension's diagnostics somewhere a caller can see
	// them. Nil sends them to stderr, which is right for a terminal and wrong
	// for a program driving tau over a pipe.
	onLog func(name, level, message string)
}

// SetLogSink routes extension log frames to fn instead of stderr.
func (l *subprocessLoader) SetLogSink(fn func(name, level, message string)) {
	l.onLog = fn
}

func newSubprocessLoader(flags []string) *subprocessLoader {
	l := &subprocessLoader{flags: flags}
	l.mgr = exthost.NewManager(exthost.Options{
		TauVersion: version,
		// An extension's stderr is the only way to debug it. Passing it
		// through would corrupt the transcript, so it goes where a redirect
		// can catch it.
		Stderr: os.Stderr,
		OnLog: func(level, msg string) {
			if l.onLog != nil {
				l.onLog("extension", level, msg)
				return
			}
			fmt.Fprintf(os.Stderr, "tau: extension [%s] %s\n", level, msg)
		},
		OnWarning: func(name, msg string) {
			l.diagnostics = append(l.diagnostics, fmt.Sprintf("extension %s: %s", name, msg))
		},
		OnSuspend: func(name string, reason error) {
			l.diagnostics = append(l.diagnostics,
				fmt.Sprintf("extension %s suspended after repeated failures: %v", name, reason))
		},
	})
	return l
}

func (l *subprocessLoader) candidates(req coding.LoadRequest) []exthost.Candidate {
	cands, errs := exthost.Discover(exthost.DiscoverOptions{
		Flags:      l.flags,
		Settings:   req.SettingsPaths,
		UserDir:    config.ExtensionsDir(),
		ProjectDir: config.ProjectExtensionsDir(req.Cwd),
		Trusted:    req.Trusted,
		Cwd:        req.Cwd,
	})
	for _, err := range errs {
		l.diagnostics = append(l.diagnostics, err.Error())
	}
	return cands
}

// snapshot converts the session view into the handshake's shape.
func snapshot(s coding.Snapshot) *wire.SessionState {
	out := &wire.SessionState{
		SessionName:   s.SessionName,
		ThinkingLevel: s.ThinkingLevel,
		ActiveTools:   s.ActiveTools,
	}
	if s.ModelID != "" {
		out.Model = &wire.ModelInfo{
			ID: s.ModelID, Provider: s.ModelProvider,
			ContextWindow: s.ContextWindow, MaxTokens: s.MaxTokens,
		}
	}
	for _, c := range s.Commands {
		out.Commands = append(out.Commands, wire.CommandInfo{
			Name: c.Name, Description: c.Description, Source: c.Source,
		})
	}
	return out
}

func (l *subprocessLoader) Load(ctx context.Context, req coding.LoadRequest) ([]extension.Extension, []string) {
	// The handshake tells the extension where it is and whether the project is
	// trusted, so the manager's options are completed here rather than at
	// construction: neither is known until the trust decision has been made.
	l.mgr.SetContext(req.Cwd, req.Trusted)
	l.mgr.SetState(func() *wire.SessionState { return snapshot(req.Snapshot) })

	exts := l.mgr.Load(ctx, l.candidates(req))
	for _, err := range l.mgr.Errors() {
		l.diagnostics = append(l.diagnostics, err.Error())
	}
	out := l.diagnostics
	l.diagnostics = nil
	return exts, out
}

func (l *subprocessLoader) Reload(ctx context.Context, req coding.LoadRequest) ([]extension.Extension, []string) {
	l.mgr.SetContext(req.Cwd, req.Trusted)
	l.mgr.SetState(func() *wire.SessionState { return snapshot(req.Snapshot) })
	exts := l.mgr.Reload(ctx, l.candidates(req))
	for _, err := range l.mgr.Errors() {
		l.diagnostics = append(l.diagnostics, err.Error())
	}
	out := l.diagnostics
	l.diagnostics = nil
	return exts, out
}

func (l *subprocessLoader) Invalidate()        { l.mgr.Invalidate() }
func (l *subprocessLoader) Stop(reason string) { l.mgr.Stop(reason) }

// repeatedFlag collects a flag given more than once, so `-e a -e b` loads both
// rather than the last one.
type repeatedFlag []string

func (f *repeatedFlag) String() string { return fmt.Sprint(*f) }

func (f *repeatedFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}
