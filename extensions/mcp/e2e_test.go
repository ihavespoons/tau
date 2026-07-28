package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/coding"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
)

// buildEchoServer compiles the test MCP server so the extension launches a
// real process over real stdio framing.
func buildEchoServer(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("compiling the test server is too slow for -short")
	}

	bin := filepath.Join(t.TempDir(), "echoserver")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/echoserver")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the test MCP server: %v\n%s", err, out)
	}
	return bin
}

func writeConfig(t *testing.T, path string, cfg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// THE P4 MCP GATE: a server configured in mcp.json is launched, its tools reach
// the agent's tool set under a namespaced name, and calling one returns the
// server's answer. This is also the acceptance test for the extension API —
// the extension under test uses nothing that is not exported.
func TestMCPServerToolsReachTheAgent(t *testing.T) {
	bin := buildEchoServer(t)

	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	spec := map[string]any{"mcpServers": map[string]any{
		"demo": map[string]any{"command": bin},
	}}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	writeConfig(t, filepath.Join(agentDir, "mcp.json"), string(body))

	ctx := context.Background()
	cs, err := coding.New(ctx, coding.Options{
		Cwd:        t.TempDir(),
		NoTools:    true, // isolate the MCP tools from the built-ins
		Extensions: []extension.Extension{New()},
	})
	if err != nil {
		t.Fatalf("building session: %v", err)
	}
	defer cs.Close(ctx, "test")

	names := cs.ToolNames()
	want := ToolName("demo", "shout")
	var found agent.Tool
	for _, tool := range cs.Agent.Tools() {
		if tool.Def().Name == want {
			found = tool
		}
	}
	if found == nil {
		t.Fatalf("the MCP tool never reached the agent; tools were %v", names)
	}

	if desc := found.Def().Description; !strings.Contains(desc, "upper case") {
		t.Errorf("the server's description was lost: %q", desc)
	}
	if found.Def().Parameters == nil || found.Def().Parameters.Properties["text"] == nil {
		t.Errorf("the server's input schema was lost: %+v", found.Def().Parameters)
	}

	res, err := found.Execute(ctx, "call-1", json.RawMessage(`{"text":"hello"}`), nil)
	if err != nil {
		t.Fatalf("calling the MCP tool: %v", err)
	}
	text, _ := res.Content[0].(ai.TextContent)
	if text.Text != "HELLO!" {
		t.Errorf("expected the server's answer, got %q", text.Text)
	}

	// The status command should describe what is connected.
	out, err := cs.RunCommand(ctx, "/mcp")
	if err != nil {
		t.Fatalf("/mcp: %v", err)
	}
	_ = out
}

// A server that cannot start must be reported and skipped, leaving tau usable.
// A broken entry in a config file is not a reason to refuse to launch.
func TestUnstartableServerDoesNotBreakStartup(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	writeConfig(t, filepath.Join(agentDir, "mcp.json"),
		`{"mcpServers":{"broken":{"command":"/nonexistent/tau-mcp-test-binary"}}}`)

	ctx := context.Background()
	cs, err := coding.New(ctx, coding.Options{
		Cwd:        t.TempDir(),
		NoTools:    true,
		Extensions: []extension.Extension{New()},
	})
	if err != nil {
		t.Fatalf("an unreachable MCP server must not prevent startup: %v", err)
	}
	defer cs.Close(ctx, "test")

	for _, name := range cs.ToolNames() {
		if strings.HasPrefix(name, toolPrefix) {
			t.Errorf("a server that never started contributed a tool: %s", name)
		}
	}
}

// /mcp exists even with nothing configured — "why do I have no MCP tools?" is
// exactly when a user reaches for it.
func TestMCPCommandExistsWithoutConfiguration(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	ctx := context.Background()
	cs, err := coding.New(ctx, coding.Options{
		Cwd:        t.TempDir(),
		NoTools:    true,
		Extensions: []extension.Extension{New()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close(ctx, "test")

	if _, ok := cs.Commands.Lookup("mcp"); !ok {
		t.Fatalf("/mcp was not registered; commands were %v", cs.Commands.Names())
	}
}

// The status report has to say where tau looked, or a misplaced config file is
// undebuggable.
func TestStatusReportNamesTheConfigFiles(t *testing.T) {
	c := &client{configPaths: [2]string{"/agent/mcp.json", "/work/.tau/mcp.json"}}
	out := c.statusReport()
	if !strings.Contains(out, "/agent/mcp.json") || !strings.Contains(out, "/work/.tau/mcp.json") {
		t.Errorf("the report should name both config paths:\n%s", out)
	}
}
