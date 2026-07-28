// Package mcp is tau's bundled Model Context Protocol client, shipped as an
// extension rather than as a core feature.
//
// That choice is the point. Pi refuses MCP in its core, and tau keeps that
// core minimal for the same reasons — so MCP has to earn its place through the
// public extension API, using nothing an out-of-tree extension could not use.
// If something here needs a private hook, the extension API is too small.
package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultTimeout bounds a server's initial connection. Startup blocks on it,
// so it is short enough that a wedged server delays tau rather than hanging it.
const defaultTimeout = 20 * time.Second

// version is reported to servers during initialization.
const version = "0.1.0"

// server is one configured MCP server and its live session.
type server struct {
	name string
	cfg  ServerConfig

	mu    sync.RWMutex
	sess  *sdk.ClientSession
	tools []string
	err   error
}

func (s *server) session() *sdk.ClientSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sess
}

func (s *server) status() (connected bool, toolCount int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sess != nil, len(s.tools), s.err
}

// client holds every configured server for one session.
type client struct {
	api     *extension.API
	servers []*server
	// configPaths are the global and project files, reported by /mcp so a
	// user who expected servers knows where tau looked for them.
	configPaths [2]string
}

// New returns the bundled MCP extension.
func New() extension.Extension {
	return extension.Extension{
		Name:    "mcp",
		Hidden:  true,
		Factory: factory,
	}
}

// factory connects the configured servers and registers their tools.
//
// Connection happens here, during registration, rather than on session_start:
// tools discovered after the agent is built would miss the first turn, and the
// first turn is the one the user is waiting on.
func factory(api *extension.API) error {
	global, project := ConfigPaths(config.AgentDir(), api.Cwd(), config.DirName)
	configs, errs := Load(global, project, api.IsProjectTrusted())
	for _, err := range errs {
		api.UI().Notify(extension.Notification{
			Level: extension.NotifyWarning, Title: "mcp", Message: err.Error(),
		})
	}
	c := &client{api: api, configPaths: [2]string{global, project}}
	for _, cfg := range configs {
		c.servers = append(c.servers, &server{name: cfg.Name, cfg: cfg.ServerConfig})
	}

	// /mcp is registered even with nothing configured. "Why are there no MCP
	// tools?" is exactly the moment a user reaches for it, and a command that
	// silently does not exist answers nothing.
	api.RegisterCommand(extension.Command{
		Name:        "mcp",
		Description: "Show MCP server status",
		Handler: func(_ context.Context, _ string, _ *extension.CommandContext) error {
			api.UI().Notify(extension.Notification{
				Level: extension.NotifyInfo, Title: "mcp", Message: c.statusReport(),
			})
			return nil
		},
	})

	if len(c.servers) == 0 {
		return nil
	}

	c.connectAll(context.Background())

	api.OnSessionShutdown(func(_ context.Context, _ *extension.SessionShutdownEvent, _ *extension.Context) error {
		c.closeAll()
		return nil
	})

	return nil
}

// connectAll dials every server concurrently. One unreachable server must not
// delay the others, and must not prevent tau from starting at all.
func (c *client) connectAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, srv := range c.servers {
		wg.Add(1)
		go func(srv *server) {
			defer wg.Done()
			if err := c.connect(ctx, srv); err != nil {
				srv.mu.Lock()
				srv.err = err
				srv.mu.Unlock()
				c.api.UI().Notify(extension.Notification{
					Level:   extension.NotifyWarning,
					Title:   "mcp",
					Message: fmt.Sprintf("%s: %v", srv.name, err),
				})
			}
		}(srv)
	}
	wg.Wait()

	// Registration is deliberately serial and outside the goroutines: it
	// mutates the agent's tool set, and doing it in one place keeps the order
	// of tools deterministic across runs.
	for _, srv := range c.servers {
		c.registerTools(ctx, srv)
	}
}

// connect establishes one session.
func (c *client) connect(ctx context.Context, srv *server) error {
	timeout := defaultTimeout
	if srv.cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(srv.cfg.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport, err := c.transport(srv)
	if err != nil {
		return err
	}

	impl := &sdk.Implementation{Name: "tau", Version: version}
	cl := sdk.NewClient(impl, &sdk.ClientOptions{
		ToolListChangedHandler: func(_ context.Context, _ *sdk.ToolListChangedRequest) {
			// The server has changed its tool set. Re-registering is legal at
			// any time, so pick the new list up without restarting.
			go c.registerTools(context.Background(), srv)
		},
	})

	sess, err := cl.Connect(ctx, transport, nil)
	if err != nil {
		return err
	}

	srv.mu.Lock()
	srv.sess = sess
	srv.err = nil
	srv.mu.Unlock()
	return nil
}

// transport builds the connection for a server config.
func (c *client) transport(srv *server) (sdk.Transport, error) {
	if srv.cfg.URL != "" {
		httpClient := http.DefaultClient
		if len(srv.cfg.Headers) > 0 {
			httpClient = &http.Client{Transport: headerTransport{
				headers: srv.cfg.Headers, base: http.DefaultTransport,
			}}
		}
		return &sdk.StreamableClientTransport{Endpoint: srv.cfg.URL, HTTPClient: httpClient}, nil
	}

	cmd := exec.Command(srv.cfg.Command, srv.cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range srv.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// A server's stderr is its log; letting it reach tau's stderr would
	// scribble over the TUI, so it is discarded.
	cmd.Stderr = nil
	return &sdk.CommandTransport{Command: cmd}, nil
}

// registerTools lists a server's tools and registers each one.
func (c *client) registerTools(ctx context.Context, srv *server) {
	sess := srv.session()
	if sess == nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		srv.mu.Lock()
		srv.err = fmt.Errorf("listing tools: %w", err)
		srv.mu.Unlock()
		return
	}

	var names []string
	for _, t := range res.Tools {
		tool, terr := newRemoteTool(srv, t)
		if terr != nil {
			c.api.UI().Notify(extension.Notification{
				Level:   extension.NotifyWarning,
				Title:   "mcp",
				Message: fmt.Sprintf("%s: %v", srv.name, terr),
			})
			continue
		}
		c.api.RegisterTool(tool)
		names = append(names, t.Name)
	}

	sort.Strings(names)
	srv.mu.Lock()
	srv.tools = names
	srv.mu.Unlock()
}

// statusReport renders the /mcp output.
func (c *client) statusReport() string {
	var b strings.Builder
	for _, srv := range c.servers {
		connected, count, err := srv.status()
		switch {
		case err != nil:
			fmt.Fprintf(&b, "%s  error: %v\n", srv.name, err)
		case !connected:
			fmt.Fprintf(&b, "%s  not connected\n", srv.name)
		default:
			fmt.Fprintf(&b, "%s  connected, %d tool(s)\n", srv.name, count)
			srv.mu.RLock()
			for _, t := range srv.tools {
				fmt.Fprintf(&b, "    %s\n", ToolName(srv.name, t))
			}
			srv.mu.RUnlock()
		}
	}
	if b.Len() == 0 {
		fmt.Fprintf(&b, "no MCP servers configured\n  add them to %s", c.configPaths[0])
		if c.configPaths[1] != "" {
			fmt.Fprintf(&b, "\n  or, in a trusted project, %s", c.configPaths[1])
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// closeAll shuts every session down.
func (c *client) closeAll() {
	for _, srv := range c.servers {
		srv.mu.Lock()
		sess := srv.sess
		srv.sess = nil
		srv.mu.Unlock()
		if sess != nil {
			_ = sess.Close()
		}
	}
}

// headerTransport adds configured headers to every HTTP request.
type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The request must be cloned: RoundTrip is forbidden from modifying the
	// one it is given.
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	return t.base.RoundTrip(clone)
}
