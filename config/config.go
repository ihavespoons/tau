// Package config resolves tau's on-disk locations. Layout mirrors Pi's
// (~/.pi/agent/* global, <cwd>/.pi/* project) under tau's own names, so a
// Pi user's mental model transfers and the interop importer has a 1:1 map.
package config

import (
	"os"
	"path/filepath"
)

// Env var overrides, mirroring Pi's PI_CODING_AGENT_DIR / PI_CODING_AGENT_SESSION_DIR.
const (
	EnvAgentDir   = "TAU_AGENT_DIR"
	EnvSessionDir = "TAU_SESSION_DIR"
	// EnvRadiusGateway points Radius at a deployment other than the default.
	// PI_RADIUS_URL is accepted too, because a user migrating from Pi has
	// already set it for a self-hosted gateway.
	EnvRadiusGateway   = "TAU_RADIUS_GATEWAY"
	EnvPiRadiusGateway = "PI_RADIUS_URL"
)

// DirName is the config directory name used both globally (~/.tau) and
// per-project (<cwd>/.tau).
const DirName = ".tau"

// AgentDir returns the global agent directory (~/.tau/agent).
func AgentDir() string {
	if v := os.Getenv(EnvAgentDir); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(DirName, "agent")
	}
	return filepath.Join(home, DirName, "agent")
}

// SettingsPath is the global settings file.
func SettingsPath() string { return filepath.Join(AgentDir(), "settings.json") }

// AuthPath is the credential store file (0600).
func AuthPath() string { return filepath.Join(AgentDir(), "auth.json") }

// ModelsPath is the custom provider/model overlay file.
func ModelsPath() string { return filepath.Join(AgentDir(), "models.json") }

// ModelsStorePath caches the catalogs of providers that publish theirs over the
// network instead of shipping a static list. It is written by tau, not by hand
// — models.json is the file the user edits.
func ModelsStorePath() string { return filepath.Join(AgentDir(), "models-store.json") }

// RadiusGateway is the Radius deployment to talk to; empty means the default.
func RadiusGateway() string {
	if v := os.Getenv(EnvRadiusGateway); v != "" {
		return v
	}
	return os.Getenv(EnvPiRadiusGateway)
}

// SessionsDir is the root of session storage.
func SessionsDir() string {
	if v := os.Getenv(EnvSessionDir); v != "" {
		return v
	}
	return filepath.Join(AgentDir(), "sessions")
}

// BinDir holds managed tool binaries (fd, rg).
func BinDir() string { return filepath.Join(AgentDir(), "bin") }

// ExtensionsDir, SkillsDir, PromptsDir, ThemesDir are the global resource dirs.
func ExtensionsDir() string { return filepath.Join(AgentDir(), "extensions") }
func SkillsDir() string     { return filepath.Join(AgentDir(), "skills") }
func PromptsDir() string    { return filepath.Join(AgentDir(), "prompts") }
func ThemesDir() string     { return filepath.Join(AgentDir(), "themes") }

// ProjectDir returns the project-local config directory for cwd (<cwd>/.tau).
func ProjectDir(cwd string) string { return filepath.Join(cwd, DirName) }

// ProjectSettingsPath is the project-local settings file.
func ProjectSettingsPath(cwd string) string {
	return filepath.Join(ProjectDir(cwd), "settings.json")
}

// PiAgentDir returns Pi's global agent directory, for the interop importer.
func PiAgentDir() string {
	if v := os.Getenv("PI_CODING_AGENT_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".pi", "agent")
	}
	return filepath.Join(home, ".pi", "agent")
}
