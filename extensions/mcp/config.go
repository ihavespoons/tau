package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// ServerConfig describes one MCP server.
//
// The shape is the de-facto standard `mcpServers` object that every MCP host
// reads, so an existing config can be copied across unchanged rather than
// re-authored for tau.
type ServerConfig struct {
	// Command and Args launch a stdio server.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Env adds variables to the server process, on top of tau's environment.
	Env map[string]string `json:"env,omitempty"`

	// URL selects the streamable HTTP transport instead of stdio.
	URL string `json:"url,omitempty"`
	// Headers are sent with every HTTP request (bearer tokens and the like).
	Headers map[string]string `json:"headers,omitempty"`

	// Enabled defaults to true. Set it to false to keep a server configured
	// but dormant.
	Enabled *bool `json:"enabled,omitempty"`

	// TimeoutSeconds bounds the initial connection. Zero uses the default.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// Disabled reports whether this server should be skipped.
func (c ServerConfig) Disabled() bool { return c.Enabled != nil && !*c.Enabled }

// Validate checks that exactly one transport is described.
func (c ServerConfig) Validate() error {
	switch {
	case c.Command == "" && c.URL == "":
		return errors.New("needs either a command (stdio) or a url (http)")
	case c.Command != "" && c.URL != "":
		return errors.New("has both a command and a url; pick one transport")
	}
	return nil
}

// Config is an mcp.json file.
type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

// Named is a server config paired with its name, in a stable order.
type Named struct {
	Name string
	ServerConfig
}

// loadFile reads one mcp.json. A missing file is not an error — most projects
// do not have one.
func loadFile(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// Load merges the global and project configurations.
//
// The project file is only consulted when the directory is trusted: an
// untrusted project must not be able to make tau launch a process.
func Load(globalPath, projectPath string, projectTrusted bool) ([]Named, []error) {
	var errs []error
	merged := map[string]ServerConfig{}

	paths := []string{globalPath}
	if projectTrusted && projectPath != "" {
		paths = append(paths, projectPath)
	}
	for _, p := range paths {
		cfg, err := loadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if cfg == nil {
			continue
		}
		// Later scopes override earlier ones by name, matching how settings
		// merge everywhere else in tau.
		for name, sc := range cfg.Servers {
			merged[name] = sc
		}
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]Named, 0, len(names))
	for _, name := range names {
		sc := merged[name]
		if sc.Disabled() {
			continue
		}
		if err := sc.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("mcp server %q %w", name, err))
			continue
		}
		out = append(out, Named{Name: name, ServerConfig: sc})
	}
	return out, errs
}

// ConfigPaths returns the two files Load reads for a session.
func ConfigPaths(agentDir, cwd, projectDirName string) (global, project string) {
	global = filepath.Join(agentDir, "mcp.json")
	if cwd != "" {
		project = filepath.Join(cwd, projectDirName, "mcp.json")
	}
	return global, project
}
