package oauth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// A browser-redirect OAuth flow needs somewhere for the provider to send the
// user back to. This is that somewhere: a single-shot local HTTP server that
// captures one authorization code and stops.

type callbackResult struct {
	Code  string
	State string
}

type callbackServer struct {
	srv      *http.Server
	ln       net.Listener
	ch       chan callbackResult
	once     sync.Once
	closeOne sync.Once
}

// callbackConfig describes one provider's redirect endpoint.
//
// The port and path are not tau's choice: they are registered with the
// provider as part of the redirect URI, so a login fails outright if the
// server listens anywhere else.
type callbackConfig struct {
	Port          int
	Path          string
	Provider      string
	ExpectedState string
}

func startCallbackServer(cfg callbackConfig) (*callbackServer, error) {
	addr := net.JoinHostPort(callbackHost(), fmt.Sprintf("%d", cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	cs := &callbackServer{ln: ln, ch: make(chan callbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cfg.Path {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, ErrorHTML("Callback route not found.", ""))
			return
		}
		q := r.URL.Query()
		code, state, errParam := q.Get("code"), q.Get("state"), q.Get("error")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case errParam != "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, ErrorHTML(cfg.Provider+" authentication did not complete.", "Error: "+errParam))
		case code == "" || state == "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, ErrorHTML("Missing code or state parameter.", ""))
		case state != cfg.ExpectedState:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, ErrorHTML("State mismatch.", ""))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, SuccessHTML(cfg.Provider+" authentication completed. You can close this window."))
			cs.settle(callbackResult{Code: code, State: state})
		}
	})
	cs.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = cs.srv.Serve(ln) }()
	return cs, nil
}

func (c *callbackServer) settle(res callbackResult) {
	c.once.Do(func() { c.ch <- res })
}

// Wait returns a channel that yields the captured code, or a zero value if the
// wait is cancelled.
func (c *callbackServer) Wait() <-chan callbackResult { return c.ch }

// CancelWait unblocks Wait with an empty result.
func (c *callbackServer) CancelWait() { c.settle(callbackResult{}) }

func (c *callbackServer) Close() {
	c.closeOne.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.srv.Shutdown(ctx)
	})
}
