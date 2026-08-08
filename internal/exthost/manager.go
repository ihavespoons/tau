package exthost

import (
	"context"
	"os/exec"
	"sync"

	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extension/wire"
)

// Manager owns the subprocess extensions a session has spawned.
//
// It exists because those processes outlive any single call: the session has
// to be able to shut them down, invalidate them when it is replaced, and
// restart them on /reload. Without an owner they would be leaked one per
// navigation.
type Manager struct {
	opts Options
	// LookPath resolves the shim executable. Injected so a test can exercise
	// the missing-shim path without depending on what is installed.
	LookPath func(string) (string, error)

	mu    sync.Mutex
	hosts []*Host
	// errs collects load failures, so one broken extension is reported rather
	// than preventing startup.
	errs []error
}

// NewManager creates a Manager.
func NewManager(opts Options) *Manager {
	return &Manager{opts: opts, LookPath: exec.LookPath}
}

// SetContext supplies the facts an extension is told at handshake. They are
// not known when the Manager is built: the trust decision comes first, and the
// working directory can change with the session.
func (m *Manager) SetContext(cwd string, trusted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts.Cwd = cwd
	m.opts.Trusted = trusted
}

// SetState supplies the handshake snapshot source.
func (m *Manager) SetState(fn func() *wire.SessionState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opts.State = fn
}

func (m *Manager) options() Options {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opts
}

// Load spawns every candidate and returns the extensions that came up.
//
// A candidate that fails to spawn is recorded and skipped. Refusing to start
// because one extension is broken would make a single bad file in
// ~/.tau/agent/extensions unbootable, and the user would have no session in
// which to fix it.
func (m *Manager) Load(ctx context.Context, cands []Candidate) []extension.Extension {
	var out []extension.Extension
	for _, c := range cands {
		spec, err := SpecFor(c, m.lookPath())
		if err != nil {
			m.addErr(err)
			continue
		}
		h, err := Spawn(ctx, spec, m.options())
		if err != nil {
			m.addErr(err)
			continue
		}
		m.mu.Lock()
		m.hosts = append(m.hosts, h)
		m.mu.Unlock()
		out = append(out, h.Extension())
	}
	return out
}

func (m *Manager) lookPath() func(string) (string, error) {
	if m.LookPath != nil {
		return m.LookPath
	}
	return exec.LookPath
}

func (m *Manager) addErr(err error) {
	m.mu.Lock()
	m.errs = append(m.errs, err)
	m.mu.Unlock()
}

// Errors returns load failures.
func (m *Manager) Errors() []error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]error{}, m.errs...)
}

// Hosts returns the running extensions.
func (m *Manager) Hosts() []*Host {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Host{}, m.hosts...)
}

// Invalidate bumps every host's generation.
//
// The processes stay alive. An extension's per-session state is set up by its
// factory, and Pi extensions capture it in `withSession` closures; killing the
// process on every navigation would re-run that setup constantly, and a
// long-lived connection (an MCP server, a language server) would be torn down
// and rebuilt for a keystroke.
func (m *Manager) Invalidate() {
	for _, h := range m.Hosts() {
		h.Invalidate()
	}
}

// Stop shuts every extension down.
func (m *Manager) Stop(reason string) {
	hosts := m.Hosts()
	m.mu.Lock()
	m.hosts = nil
	m.mu.Unlock()

	// Shutting down in parallel, because the grace periods are per process and
	// serializing them would make quitting take as long as the sum.
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h *Host) {
			defer wg.Done()
			h.Stop(reason)
		}(h)
	}
	wg.Wait()
}

// Reload stops everything and spawns the candidates again.
//
// This is the one operation that does kill the processes: /reload exists
// precisely so an author can edit an extension and see the new code run, and
// keeping the old process alive would defeat it.
func (m *Manager) Reload(ctx context.Context, cands []Candidate) []extension.Extension {
	m.Stop("reload")
	m.mu.Lock()
	m.errs = nil
	m.mu.Unlock()
	return m.Load(ctx, cands)
}

// RendererFor finds the host that claims a renderer, or nil.
func (m *Manager) RendererFor(kind, selector string) *Host {
	for _, h := range m.Hosts() {
		if !h.Suspended() && h.Renders(kind, selector) {
			return h
		}
	}
	return nil
}
