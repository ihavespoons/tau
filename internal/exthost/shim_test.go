package exthost

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extension/wire"
)

// The P8 dogfood gate: a real Pi TypeScript extension runs on tau unchanged.
//
// The fixtures under testdata/piexts are copied byte-for-byte from Pi v0.82.1's
// own examples directory. Nothing about them was adapted, which is the whole
// claim being tested — an extension someone already has in ~/.pi should work.

// shimPath is the checked-in host shim's CLI.
func shimPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../shim/bin/tau-pi-host.mjs")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// requireNode skips when there is no Node to run the shim under. The shim is a
// Node program; a machine without one cannot exercise it, and pretending
// otherwise would make the suite lie about coverage.
func requireNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the TypeScript host shim cannot be exercised")
	}
	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Skipf("node is not runnable: %v", err)
	}
	// Native type stripping landed in 22.18. Below that the shim would need a
	// transpiler, which it deliberately does not carry.
	v := strings.TrimSpace(string(out))
	if !nodeAtLeast(v, 22, 18) {
		t.Skipf("node %s is too old for native TypeScript stripping (need >= 22.18)", v)
	}
	return node
}

func nodeAtLeast(version string, major, minor int) bool {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	maj, min := 0, 0
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return false
		}
		maj = maj*10 + int(c-'0')
	}
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			break
		}
		min = min*10 + int(c-'0')
	}
	return maj > major || (maj == major && min >= minor)
}

// spawnPiExtension runs one of the checked-in Pi extensions under the shim.
func spawnPiExtension(t *testing.T, name string) (*Host, *extension.Runner, *capture) {
	t.Helper()
	node := requireNode(t)

	entry, err := filepath.Abs(filepath.Join("testdata", "piexts", name))
	if err != nil {
		t.Fatal(err)
	}

	cap := &capture{}
	opts := Options{
		Cwd: t.TempDir(), Trusted: true, Stderr: &cap.stderr,
		OnLog: func(level, msg string) {
			cap.mu.Lock()
			cap.logs = append(cap.logs, level+": "+msg)
			cap.mu.Unlock()
		},
		OnWarning: func(n, m string) {
			cap.mu.Lock()
			cap.warnings = append(cap.warnings, n+": "+m)
			cap.mu.Unlock()
		},
		State: func() *wire.SessionState {
			return &wire.SessionState{
				SessionName:   "seeded name",
				ThinkingLevel: "off",
				ActiveTools:   []string{"read", "write"},
				Commands:      []wire.CommandInfo{{Name: "help", Source: "builtin"}},
			}
		},
	}

	h, err := Spawn(context.Background(), Spec{
		Name: strings.TrimSuffix(name, ".ts"), Path: entry,
		Command: node, Args: []string{shimPath(t), entry},
	}, opts)
	if err != nil {
		t.Fatalf("spawn %s: %v\nstderr: %s", name, err, cap.stderr.String())
	}
	t.Cleanup(func() { h.Stop("exit") })

	r := extension.NewRunner(extension.RunnerOptions{
		Mode: extension.ModeTUI, Cwd: ".", Trusted: true, UI: &stubUI{},
	})
	if err := r.Load(h.Extension()); err != nil {
		t.Fatalf("load %s: %v\nstderr: %s", name, err, cap.stderr.String())
	}
	r.Bind(&stubRuntime{})
	return h, r, cap
}

// hello.ts registers a tool with a TypeBox schema. It is the smallest complete
// Pi extension, and it exercises the two things every extension needs: the
// module aliases resolving, and a schema surviving the wire.
func TestPiHelloExtensionRunsUnchanged(t *testing.T) {
	h, r, cap := spawnPiExtension(t, "hello.ts")

	decl := h.Declaration()
	if len(decl.Tools) != 1 || decl.Tools[0].Name != "hello" {
		t.Fatalf("tools = %+v\nstderr: %s", decl.Tools, cap.stderr.String())
	}

	tool := findTool(t, r, "hello")
	def := tool.Def()
	if def.Parameters == nil || def.Parameters.Type != "object" {
		t.Fatalf("schema = %+v", def.Parameters)
	}
	if _, ok := def.Parameters.Properties["name"]; !ok {
		t.Fatalf("the TypeBox schema lost its property: %+v", def.Parameters)
	}
	// TypeBox makes properties required unless wrapped in Optional, which is
	// the opposite of most builders and what the extension author relied on.
	if len(def.Parameters.Required) != 1 || def.Parameters.Required[0] != "name" {
		t.Fatalf("required = %v, want [name]", def.Parameters.Required)
	}

	res, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"name":"world"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, cap.stderr.String())
	}
	if got := textOf(res); got != "Hello, world!" {
		t.Fatalf("output = %q", got)
	}
	details, _ := res.Details.(map[string]any)
	if details["greeted"] != "world" {
		t.Fatalf("details = %+v", res.Details)
	}
}

// protected-paths.ts is a permission gate written for Pi. It reads
// `event.input`, which tau calls `args` — the translation the shim does is the
// difference between this extension working and silently allowing everything.
func TestPiProtectedPathsExtensionGatesTools(t *testing.T) {
	_, r, cap := spawnPiExtension(t, "protected-paths.ts")

	blocked := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c1", ToolName: "write",
		Args: map[string]any{"path": "/repo/.env", "content": "secret"},
	})
	if blocked == nil || !blocked.Block {
		t.Fatalf("a write to .env was allowed: %+v\nstderr: %s", blocked, cap.stderr.String())
	}
	if !strings.Contains(blocked.Reason, "protected") {
		t.Fatalf("reason = %q", blocked.Reason)
	}

	allowed := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c2", ToolName: "write",
		Args: map[string]any{"path": "/repo/src/main.go"},
	})
	if allowed != nil && allowed.Block {
		t.Fatalf("an ordinary write was blocked: %+v", allowed)
	}

	// The extension only claims write and edit; anything else must pass.
	other := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c3", ToolName: "read",
		Args: map[string]any{"path": "/repo/.env"},
	})
	if other != nil && other.Block {
		t.Fatalf("a read was blocked by a write gate: %+v", other)
	}
}

// session-name.ts calls pi.getSessionName() synchronously and puts the result
// straight into a notification. Nothing can be synchronous across a pipe, so
// this is the test that the mirrored state actually works.
func TestPiSessionNameExtensionUsesSynchronousGetters(t *testing.T) {
	h, r, cap := spawnPiExtension(t, "session-name.ts")

	cmds := r.Commands()
	if len(cmds) != 1 || cmds[0].Name != "session-name" {
		t.Fatalf("commands = %+v\nstderr: %s", cmds, cap.stderr.String())
	}

	// With no argument it reads the name back. A Promise here would reach the
	// user as "[object Promise]".
	ui := &recordingUI{}
	rr := extension.NewRunner(extension.RunnerOptions{
		Mode: extension.ModeTUI, Cwd: ".", Trusted: true, UI: ui,
	})
	if err := rr.Load(h.Extension()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rr.Bind(&stubRuntime{})

	if err := rr.Commands()[0].Handler(context.Background(), "", rr.NewCommandContext()); err != nil {
		t.Fatalf("command: %v\nstderr: %s", err, cap.stderr.String())
	}
	msg := ui.waitForNotify(t, 5*time.Second)
	if !strings.Contains(msg, "seeded name") {
		t.Fatalf("notification = %q, want the seeded session name", msg)
	}
	if strings.Contains(msg, "Promise") {
		t.Fatalf("a synchronous getter returned a promise: %q", msg)
	}
}

// A tool that throws in Pi becomes an isError result the model can read, not a
// transport failure. The shim has to preserve that or a failing tool looks
// like a broken extension.
func TestPiToolThrowBecomesAnErrorResult(t *testing.T) {
	_, r, _ := spawnPiExtension(t, "throwing.ts")
	tool := findTool(t, r, "boom")

	_, err := tool.Execute(context.Background(), "c", json.RawMessage(`{}`), nil)
	if err == nil || !strings.Contains(err.Error(), "deliberate failure") {
		t.Fatalf("err = %v", err)
	}
}

// stdout is the protocol. An extension that prints must not corrupt it.
func TestPiExtensionStdoutDoesNotCorruptTheProtocol(t *testing.T) {
	h, r, cap := spawnPiExtension(t, "noisy.ts")

	if h.Suspended() {
		t.Fatalf("a printing extension was suspended: %s", cap.stderr.String())
	}
	tool := findTool(t, r, "quiet")
	res, err := tool.Execute(context.Background(), "c", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, cap.stderr.String())
	}
	if got := textOf(res); got != "still working" {
		t.Fatalf("output = %q", got)
	}
	if !strings.Contains(cap.stderr.String(), "this should not corrupt the stream") {
		t.Fatalf("the extension's stdout was lost instead of redirected: %q", cap.stderr.String())
	}
}

// recordingUI captures what an extension asked the UI to show.
type recordingUI struct {
	mu      sync.Mutex
	notices []string
}

func (u *recordingUI) Confirm(context.Context, extension.ConfirmRequest) (bool, error) {
	return true, nil
}
func (u *recordingUI) Select(context.Context, extension.SelectRequest) (int, error) { return 0, nil }
func (u *recordingUI) Input(context.Context, extension.InputRequest) (string, error) {
	return "", nil
}
func (u *recordingUI) Notify(n extension.Notification) {
	u.mu.Lock()
	u.notices = append(u.notices, n.Message)
	u.mu.Unlock()
}
func (u *recordingUI) SetStatus(string)                                             {}
func (u *recordingUI) SetTitle(string)                                              {}
func (u *recordingUI) SetWidget(string, extension.WidgetPosition, extension.Widget) {}

func (u *recordingUI) waitForNotify(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		n := len(u.notices)
		var last string
		if n > 0 {
			last = u.notices[n-1]
		}
		u.mu.Unlock()
		if n > 0 {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the extension never notified anything")
	return ""
}

var _ agent.Tool = (*wireTool)(nil)
