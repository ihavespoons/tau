package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ihavespoons/tau/config"
)

// SocketPath is where the server listens by default.
//
// A Unix socket rather than a port, for the same reason Pi uses one: the
// default deployment is one server for one person on one machine, and a socket
// is reachable by them and by nothing on the network. A port is available with
// --listen for the cases that need it.
func SocketPath() string { return filepath.Join(config.AgentDir(), "server.sock") }

// Listen opens a listener for an address.
//
// An address with a slash in it is a Unix socket path; anything else is handed
// to TCP. A stale socket file from a server that was killed is removed first —
// bind fails on an existing path, and refusing to start because of a crash that
// happened days ago would be its own problem.
func Listen(addr string) (net.Listener, error) {
	if addr == "" {
		addr = SocketPath()
	}
	if !strings.Contains(addr, "/") {
		return net.Listen("tcp", addr)
	}

	if err := os.MkdirAll(filepath.Dir(addr), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	if info, err := os.Stat(addr); err == nil && info.Mode()&os.ModeSocket != 0 {
		// Only remove it if nothing is listening: taking the socket out from
		// under a running server would be worse than failing to start.
		if conn, derr := net.DialTimeout("unix", addr, 200*time.Millisecond); derr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("a server is already listening on %s", addr)
		}
		_ = os.Remove(addr)
	}

	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	// The socket carries full control of every agent the server owns, so it is
	// the owner's alone.
	if err := os.Chmod(addr, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("secure socket: %w", err)
	}
	return ln, nil
}

// Serve runs the HTTP API until ctx is cancelled, then stops every instance.
//
// The agents go down with the server on purpose. They are subprocesses of it,
// and leaving them running would leave sessions being written to by processes
// nothing is left to address.
func Serve(ctx context.Context, s *Supervisor, ln net.Listener) error {
	srv := &http.Server{
		Handler: Handler(s),
		// No write timeout: an event stream is meant to stay open for as long
		// as the agent is working, and a deadline here would cut it off
		// mid-turn.
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()

	select {
	case err := <-errs:
		_ = s.Shutdown(context.Background())
		return err
	case <-ctx.Done():
	}

	// Shut the HTTP side first so no new commands arrive, then the agents.
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(stopCtx)
	agentErr := s.Shutdown(stopCtx)
	return errors.Join(shutdownErr, agentErr, <-errs)
}
