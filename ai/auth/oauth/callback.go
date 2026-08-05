package oauth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// A browser-redirect OAuth flow needs somewhere for the provider to send the
// user back to. This is that somewhere: a single-shot local HTTP server that
// captures one authorization code and stops.

type callbackResult struct {
	Code  string
	State string
	// Err reports a failure the browser already saw, so the terminal can say
	// the same thing rather than timing out on a login that is already over.
	Err error
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
// For most providers the port and path are not tau's choice: they are
// registered with the provider as part of the redirect URI, so a login fails
// outright if the server listens anywhere else. OpenRouter is the exception —
// it takes the callback URL as a request parameter, so the port is ephemeral
// and the path is a fresh random value each login.
type callbackConfig struct {
	// Port is the fixed registered port, or 0 to take any free one.
	Port     int
	Path     string
	Provider string
	// ExpectedState is the CSRF value the redirect must carry back. Empty
	// means the provider round-trips no state, and an unguessable callback
	// path is doing that job instead.
	ExpectedState string
	// Exchange, when set, runs inside the request handler before the browser
	// is answered, so a failed token exchange is reported on the page the user
	// is looking at rather than only in a terminal they may have left.
	Exchange func(ctx context.Context, code string) error
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
			cs.fail(fmt.Errorf("%s authorization failed: %s", cfg.Provider, errParam))
		case code == "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, ErrorHTML("Missing code parameter.", ""))
		case cfg.ExpectedState != "" && state == "":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, ErrorHTML("Missing state parameter.", ""))
		case cfg.ExpectedState != "" && state != cfg.ExpectedState:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, ErrorHTML("State mismatch.", ""))
		default:
			if cfg.Exchange != nil {
				if err := cfg.Exchange(r.Context(), code); err != nil {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, ErrorHTML(cfg.Provider+" key exchange failed.", err.Error()))
					cs.fail(err)
					return
				}
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, SuccessHTML(cfg.Provider+" authentication completed. You can close this window."))
			cs.settle(callbackResult{Code: code, State: state})
		}
	})
	cs.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = cs.srv.Serve(ln) }()
	return cs, nil
}

// URL returns the address the provider should redirect to. It is only
// meaningful for a server on an ephemeral port, where the port is not known
// until it is listening.
func (c *callbackServer) URL(path string) string {
	addr, ok := c.ln.Addr().(*net.TCPAddr)
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s%s", net.JoinHostPort(callbackHost(), strconv.Itoa(addr.Port)), path)
}

func (c *callbackServer) settle(res callbackResult) {
	c.once.Do(func() { c.ch <- res })
}

// fail settles the wait with an error the browser has already been shown.
func (c *callbackServer) fail(err error) { c.settle(callbackResult{Err: err}) }

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
