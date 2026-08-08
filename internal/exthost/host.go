// Package exthost runs extensions in a separate process and makes them
// indistinguishable from in-process ones.
//
// # The shape of the thing
//
// A subprocess extension is presented to the rest of tau as an ordinary
// extension.Extension whose factory registers forwarding handlers. Everything
// downstream — the composition policies, the error collection, the staleness
// guard, the tool registry — is the same code that serves a Go extension. That
// is deliberate: the per-event composition rules are the subtlest part of the
// extension system, and a second implementation of them for the wire would
// drift silently.
//
// # What is different, and why
//
// Three things cannot be the same as in-process, and each one is a decision:
//
//   - A subprocess can hang. Every request has a deadline and a cancel frame,
//     and when the grace period expires the host applies a per-event fail-safe
//     rather than waiting. tool_call fails CLOSED — a permission gate that
//     stops answering must not become a permission gate that says yes.
//
//   - A subprocess round trip costs real time. The streaming events fire per
//     delta, and awaiting one of those per token would make the agent as slow
//     as its slowest extension. Those events are conflated: the newest payload
//     replaces an unsent one, and the handler returns immediately.
//
//   - A subprocess can be sick without being dead. Three strikes — a timeout,
//     a protocol violation, a crash — suspend it. A suspended extension keeps
//     its process (so the diagnostics survive) but receives nothing more.
package exthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extension/wire"
)

// Timeouts. These are host policy, not protocol: an extension is never told
// how long it has, because a well-behaved one should not be racing a clock.
const (
	// HandshakeTimeout bounds the wait for init_result. A process that cannot
	// declare itself in this long is not going to.
	HandshakeTimeout = 10 * time.Second
	// RequestTimeout bounds a normal request. It is generous because an
	// extension may legitimately be asking the user something.
	RequestTimeout = 60 * time.Second
	// GracePeriod is how long a cancelled request has to answer before the
	// host stops waiting and applies the fail-safe.
	GracePeriod = 5 * time.Second
	// ShutdownGrace is how long a process has to exit after shutdown before
	// it is killed.
	ShutdownGrace = 3 * time.Second
	// MaxStrikes suspends an extension. Two is within the range of bad luck;
	// three is a pattern.
	MaxStrikes = 3
)

// ErrSuspended is returned by every request to a suspended extension.
var ErrSuspended = errors.New("exthost: extension suspended after repeated failures")

// ErrProtocol reports a frame that cannot be reconciled with the protocol.
var ErrProtocol = errors.New("exthost: protocol violation")

// Spec describes a process to run as an extension.
type Spec struct {
	// Name identifies the extension in diagnostics. Empty derives one from
	// Path.
	Name string
	// Path is the file or directory the extension came from, reported to the
	// extension and used in errors.
	Path string
	// Command and Args are what to execute.
	Command string
	Args    []string
	// Env is added to the child's environment.
	Env []string
	// Dir is the child's working directory. Empty inherits the host's.
	Dir string
}

// Options configures a Host.
type Options struct {
	// Cwd, Mode, Trusted, and TauVersion are reported in the handshake.
	Cwd     string
	Trusted bool
	// TauVersion lets an extension branch on host capabilities.
	TauVersion string
	// Stderr receives the child's stderr. A subprocess extension's diagnostics
	// are the only way to debug it, so they are never discarded silently.
	Stderr io.Writer
	// OnLog receives log frames.
	OnLog func(level, message string)
	// OnWarning receives handshake warnings — an API the shim stubbed out, a
	// deprecated import. Reported once, at load.
	OnWarning func(name, message string)
	// OnSuspend is called when an extension exhausts its strikes.
	OnSuspend func(name string, reason error)
	// Now is the clock, injectable for tests.
	Now func() time.Time
	// State supplies the handshake snapshot an extension reads synchronously.
	// Nil sends none, and a shim's getters start empty.
	State func() *wire.SessionState
}

// pending is one outstanding host→extension request.
type pending struct {
	ch chan wire.Result
}

// Host is one running subprocess extension.
type Host struct {
	spec Spec
	opts Options

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	w      *wire.Writer
	r      *wire.Reader
	closed chan struct{}

	mu      sync.Mutex
	pending map[string]*pending
	nextID  uint64
	// initWaiter is set for the duration of the handshake only. init_result
	// has no request id, so it cannot travel through the pending map.
	initWaiter chan initOutcome
	// toolUpdates routes streamed partial results to the tool call that is
	// waiting for them.
	toolUpdates map[string]func(wire.ToolUpdate)

	// decl is the extension's handshake declaration, fixed after init.
	decl wire.InitResult

	strikes  atomic.Int32
	suspend  atomic.Bool
	exitErr  atomic.Pointer[error]
	stopOnce sync.Once

	// generation is bumped when the session is replaced. A result stamped
	// with an older generation is discarded rather than applied to a session
	// it never saw.
	generation atomic.Uint64

	// api is bound once the factory has run, so incoming ui_request and
	// action frames have somewhere to go.
	apiMu sync.Mutex
	api   *extension.API

	// hot is the conflating sender for streaming events.
	hot *conflator
}

// Name is the extension's identity.
func (h *Host) Name() string {
	if h.decl.Name != "" {
		return h.decl.Name
	}
	return h.spec.Name
}

// Suspended reports whether the extension has been taken out of service.
func (h *Host) Suspended() bool { return h.suspend.Load() }

// Strikes reports the current failure count, for diagnostics and tests.
func (h *Host) Strikes() int { return int(h.strikes.Load()) }

// Declaration returns what the extension declared at handshake.
func (h *Host) Declaration() wire.InitResult { return h.decl }

// Spawn starts the process and completes the handshake.
//
// A failure here is a failure to load: the caller reports it and carries on
// without the extension, exactly as a Go factory returning an error does.
func Spawn(ctx context.Context, spec Spec, opts Options) (*Host, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if spec.Name == "" {
		spec.Name = spec.Path
	}

	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("exthost: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("exthost: stdout: %w", err)
	}
	cmd.Stderr = opts.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exthost: start %s: %w", spec.Command, err)
	}

	h := &Host{
		spec: spec, opts: opts, cmd: cmd, stdin: stdin,
		w: wire.NewWriter(stdin), r: wire.NewReader(stdout),
		closed:      make(chan struct{}),
		pending:     map[string]*pending{},
		toolUpdates: map[string]func(wire.ToolUpdate){},
	}
	h.hot = newConflator(h)

	go h.readPump()
	go h.reap()

	if err := h.handshake(ctx); err != nil {
		h.Stop("exit")
		return nil, err
	}
	return h, nil
}

func (h *Host) reap() {
	err := h.cmd.Wait()
	if err != nil {
		h.exitErr.Store(&err)
	}
	// Waking every pending caller is what turns a crash into a fail-safe
	// instead of a hang: the request's own deadline would eventually fire,
	// but a dead process is knowable now.
	h.stopOnce.Do(func() { close(h.closed) })
	h.failAllPending()
}

func (h *Host) failAllPending() {
	h.mu.Lock()
	ps := h.pending
	h.pending = map[string]*pending{}
	h.mu.Unlock()
	for id, p := range ps {
		select {
		case p.ch <- wire.Result{ID: id, Error: "extension exited"}:
		default:
		}
	}
}

func (h *Host) handshake(ctx context.Context) error {
	init := wire.Init{
		Type: wire.FrameInit, Protocol: wire.Protocol,
		Name: h.spec.Name, Path: h.spec.Path,
		Cwd: h.opts.Cwd, Mode: "rpc", Trusted: h.opts.Trusted,
		Generation: h.generation.Load(), TauVersion: h.opts.TauVersion,
	}
	if h.opts.State != nil {
		init.State = h.opts.State()
	}
	if err := h.w.Write(init); err != nil {
		return fmt.Errorf("exthost: %s: send init: %w", h.spec.Name, err)
	}

	select {
	case res := <-h.initCh():
		if res.err != nil {
			return res.err
		}
		h.decl = res.decl
	case <-time.After(HandshakeTimeout):
		return fmt.Errorf("exthost: %s: no init_result within %s", h.spec.Name, HandshakeTimeout)
	case <-ctx.Done():
		return ctx.Err()
	case <-h.closed:
		return fmt.Errorf("exthost: %s: exited during handshake%s", h.spec.Name, h.exitDetail())
	}

	if h.decl.Error != "" {
		return fmt.Errorf("exthost: %s: %s", h.spec.Name, h.decl.Error)
	}
	// A version mismatch is refused rather than negotiated. Half-understanding
	// a protocol is how a permission gate silently stops gating.
	if h.decl.Protocol != wire.Protocol {
		return fmt.Errorf("exthost: %s: speaks protocol %d, tau speaks %d",
			h.spec.Name, h.decl.Protocol, wire.Protocol)
	}
	for _, warn := range h.decl.Warnings {
		if h.opts.OnWarning != nil {
			h.opts.OnWarning(h.Name(), warn)
		}
	}
	return nil
}

type initOutcome struct {
	decl wire.InitResult
	err  error
}

func (h *Host) initCh() chan initOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan initOutcome, 1)
	h.initWaiter = ch
	return ch
}

// readPump owns the child's stdout. It routes results to their waiters and
// hands everything else to a goroutine, because a ui_request blocks on the
// user and a pump that waited for one would stop delivering every other
// extension's answers too.
func (h *Host) readPump() {
	for {
		env, raw, err := h.r.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				h.strike(fmt.Errorf("%w: %v", ErrProtocol, err))
			}
			h.stopOnce.Do(func() { close(h.closed) })
			h.failAllPending()
			return
		}
		h.route(env, raw)
	}
}

func (h *Host) route(env wire.Envelope, raw []byte) {
	switch env.Type {
	case wire.FrameInitResult:
		var res wire.InitResult
		if err := json.Unmarshal(raw, &res); err != nil {
			h.deliverInit(initOutcome{err: fmt.Errorf("exthost: bad init_result: %w", err)})
			return
		}
		h.deliverInit(initOutcome{decl: res})

	case wire.FrameResult:
		var res wire.Result
		if err := json.Unmarshal(raw, &res); err != nil {
			h.strike(fmt.Errorf("%w: bad result: %v", ErrProtocol, err))
			return
		}
		h.deliver(res)

	case wire.FrameToolUpdate:
		var up wire.ToolUpdate
		if err := json.Unmarshal(raw, &up); err != nil {
			h.strike(fmt.Errorf("%w: bad tool_update: %v", ErrProtocol, err))
			return
		}
		h.deliverToolUpdate(up)

	case wire.FrameUIRequest:
		var req wire.UIRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			h.strike(fmt.Errorf("%w: bad ui_request: %v", ErrProtocol, err))
			return
		}
		go h.serveUI(req)

	case wire.FrameAction:
		var act wire.Action
		if err := json.Unmarshal(raw, &act); err != nil {
			h.strike(fmt.Errorf("%w: bad action: %v", ErrProtocol, err))
			return
		}
		go h.serveAction(act)

	case wire.FrameLog:
		var lg wire.Log
		if err := json.Unmarshal(raw, &lg); err != nil {
			return
		}
		if h.opts.OnLog != nil {
			h.opts.OnLog(lg.Level, lg.Message)
		}

	default:
		// A host-originated type arriving from the extension means one side
		// has the direction wrong; anything unrecognized means a version skew
		// the handshake should have caught.
		h.strike(fmt.Errorf("%w: unexpected frame %q from extension", ErrProtocol, env.Type))
	}
}

func (h *Host) deliverInit(out initOutcome) {
	h.mu.Lock()
	ch := h.initWaiter
	h.initWaiter = nil
	h.mu.Unlock()
	if ch != nil {
		ch <- out
	}
}

func (h *Host) deliver(res wire.Result) {
	h.mu.Lock()
	p := h.pending[res.ID]
	delete(h.pending, res.ID)
	h.mu.Unlock()
	if p == nil {
		// A result for a request the host stopped waiting for. That is the
		// normal end of a timed-out request, not a fault.
		return
	}
	select {
	case p.ch <- res:
	default:
	}
}

func (h *Host) nextRequestID() string {
	return strconv.FormatUint(atomic.AddUint64(&h.nextID, 1), 10)
}

// request sends a frame and waits for its result.
//
// The wait ends in one of four ways, and each has a different meaning:
// an answer, the caller's context ending, the process dying, or the deadline.
// Only the last two are the extension's fault, so only they take a strike.
func (h *Host) request(ctx context.Context, id string, frame any) (wire.Result, error) {
	if h.suspend.Load() {
		return wire.Result{}, ErrSuspended
	}

	ch := make(chan wire.Result, 1)
	h.mu.Lock()
	h.pending[id] = &pending{ch: ch}
	h.mu.Unlock()

	if err := h.w.Write(frame); err != nil {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		h.strike(err)
		return wire.Result{}, err
	}

	timer := time.NewTimer(RequestTimeout)
	defer timer.Stop()

	select {
	case res := <-ch:
		return res, nil

	case <-h.closed:
		h.abandon(id)
		return wire.Result{}, fmt.Errorf("exthost: %s: exited%s", h.Name(), h.exitDetail())

	case <-ctx.Done():
		// The caller gave up. The extension may still be mid-dialog, so it is
		// told to stop and given a grace period to answer anyway — a cancelled
		// request that returns a real answer is still a real answer.
		return h.cancelAndWait(id, ch, ctx.Err())

	case <-timer.C:
		res, err := h.cancelAndWait(id, ch, fmt.Errorf(
			"exthost: %s: no response within %s", h.Name(), RequestTimeout))
		if err != nil {
			h.strike(err)
		}
		return res, err
	}
}

// cancelAndWait sends a cancel frame and gives the extension GracePeriod to
// answer before giving up on it.
func (h *Host) cancelAndWait(id string, ch chan wire.Result, cause error) (wire.Result, error) {
	_ = h.w.Write(wire.Cancel{Type: wire.FrameCancel, ID: id})

	grace := time.NewTimer(GracePeriod)
	defer grace.Stop()
	select {
	case res := <-ch:
		return res, nil
	case <-h.closed:
		h.abandon(id)
		return wire.Result{}, cause
	case <-grace.C:
		h.abandon(id)
		return wire.Result{}, cause
	}
}

func (h *Host) abandon(id string) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

func (h *Host) exitDetail() string {
	if e := h.exitErr.Load(); e != nil && *e != nil {
		return ": " + (*e).Error()
	}
	return ""
}

// strike records a failure and suspends the extension once they accumulate.
//
// Suspension keeps the process alive. Killing it would take its stderr with
// it, and the reason it misbehaved is usually in there.
func (h *Host) strike(cause error) {
	if h.suspend.Load() {
		return
	}
	n := h.strikes.Add(1)
	if int(n) < MaxStrikes {
		return
	}
	if h.suspend.CompareAndSwap(false, true) {
		if h.opts.OnSuspend != nil {
			h.opts.OnSuspend(h.Name(), cause)
		}
		_ = h.w.Write(wire.Shutdown{Type: wire.FrameShutdown, Reason: "suspended"})
	}
}

// Invalidate bumps the generation so results referring to the previous session
// are discarded. The process stays alive: an extension's `withSession` closures
// are set up in its factory, and killing it would mean re-running the factory
// on every navigation.
func (h *Host) Invalidate() uint64 { return h.generation.Add(1) }

// Generation is the current session generation.
func (h *Host) Generation() uint64 { return h.generation.Load() }

// Stop shuts the extension down: a shutdown frame, then a closed stdin, then a
// kill if it is still running.
func (h *Host) Stop(reason string) {
	_ = h.w.Write(wire.Shutdown{Type: wire.FrameShutdown, Reason: reason})
	_ = h.stdin.Close()

	select {
	case <-h.closed:
	case <-time.After(ShutdownGrace):
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill()
		}
		<-h.closed
	}
	h.hot.stop()
}

// Done is closed when the process has exited.
func (h *Host) Done() <-chan struct{} { return h.closed }
