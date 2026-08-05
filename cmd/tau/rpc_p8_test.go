package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The P8 gate, half of it: an rpc client drives tau. Everything here goes
// through the compiled binary over real pipes, because the thing being tested
// is a protocol, and a protocol tested in-process is a function call.

// rpcClient is the other end of `tau --mode rpc`.
type rpcClient struct {
	t    *testing.T
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	errs strings.Builder

	mu    sync.Mutex
	lines []map[string]any
	// cursor is how far await has consumed. Records arrive faster than a test
	// asks for them, so a match already in the buffer has to count — and must
	// not be matched twice.
	cursor int
}

func startRPC(t *testing.T, cwd string, env ...string) *rpcClient {
	t.Helper()
	return startRPCWith(t, cwd, nil, env...)
}

func startRPCWith(t *testing.T, cwd string, args []string, env ...string) *rpcClient {
	t.Helper()
	bin := buildTau(t)

	cmd := exec.Command(bin, append([]string{"--mode", "rpc"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)

	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c := &rpcClient{t: t, cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20)}
	cmd.Stderr = &syncWriter{w: &c.errs}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return c
}

type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (c *rpcClient) send(v any) {
	c.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.in.Write(append(raw, '\n')); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// next reads one record. Framing is LF-only on purpose, so this reads the same
// way a client in any language would have to.
func (c *rpcClient) next(timeout time.Duration) (map[string]any, error) {
	type result struct {
		m   map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := c.out.ReadString('\n')
		if err != nil && line == "" {
			ch <- result{nil, err}
			return
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &m); err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{m, nil}
	}()
	select {
	case r := <-ch:
		if r.m != nil {
			c.mu.Lock()
			c.lines = append(c.lines, r.m)
			c.mu.Unlock()
		}
		return r.m, r.err
	case <-time.After(timeout):
		return nil, errors.New("timed out waiting for a record")
	}
}

// await reads until a record satisfies match, so a test never has to know how
// many events precede the one it cares about.
func (c *rpcClient) await(timeout time.Duration, match func(map[string]any) bool) map[string]any {
	c.t.Helper()

	c.mu.Lock()
	for c.cursor < len(c.lines) {
		m := c.lines[c.cursor]
		c.cursor++
		if match(m) {
			c.mu.Unlock()
			return m
		}
	}
	c.mu.Unlock()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m, err := c.next(time.Until(deadline))
		if err != nil {
			c.t.Fatalf("waiting for a record: %v\nseen: %s\nstderr: %s", err, c.seen(), c.stderr())
		}
		c.mu.Lock()
		c.cursor = len(c.lines)
		c.mu.Unlock()
		if match(m) {
			return m
		}
	}
	c.t.Fatalf("no matching record within %s\nseen: %s\nstderr: %s", timeout, c.seen(), c.stderr())
	return nil
}

func (c *rpcClient) awaitType(timeout time.Duration, want string) map[string]any {
	c.t.Helper()
	return c.await(timeout, func(m map[string]any) bool { return m["type"] == want })
}

func (c *rpcClient) awaitResponse(timeout time.Duration, command string) map[string]any {
	c.t.Helper()
	return c.await(timeout, func(m map[string]any) bool {
		return m["type"] == "response" && m["command"] == command
	})
}

// waitForText polls everything received so far for a substring, without
// consuming it. Ordering between a response and the events around it is not
// guaranteed, so an assertion about "did this ever appear" must not depend on
// where the consuming cursor happens to be.
func (c *rpcClient) waitForText(timeout time.Duration, sub string) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(c.seen(), sub) {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("never saw %q\nseen: %s\nstderr: %s", sub, c.seen(), c.stderr())
		}
		if _, err := c.next(time.Until(deadline)); err != nil {
			c.t.Fatalf("never saw %q: %v\nseen: %s\nstderr: %s", sub, err, c.seen(), c.stderr())
		}
	}
}

func (c *rpcClient) seen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, m := range c.lines {
		raw, _ := json.Marshal(m)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

func (c *rpcClient) stderr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errs.String()
}

func TestRPCModeRunsATurn(t *testing.T) {
	url, calls := fakeAnthropic(t, writeStream(textStream("hello from the model")))
	_, env := p7Env(t, url, "")

	c := startRPC(t, t.TempDir(), env...)

	ready := c.awaitType(10*time.Second, "ready")
	if ready["sessionPath"] == "" {
		t.Fatalf("ready carried no session path: %v", ready)
	}

	c.send(map[string]any{"id": "1", "type": "prompt", "message": "say hi"})
	res := c.awaitResponse(10*time.Second, "prompt")
	if res["success"] != true {
		t.Fatalf("prompt failed: %v", res)
	}

	// The answer arrives as events, not in the response: a prompt is
	// asynchronous so the connection stays usable during the turn.
	c.await(20*time.Second, func(m map[string]any) bool {
		return m["type"] == "message_end" &&
			strings.Contains(string(mustJSON(m["message"])), "hello from the model")
	})
	c.awaitType(10*time.Second, "agent_settled")

	if got := calls.Load(); got != 1 {
		t.Errorf("provider requests = %d, want 1", got)
	}
}

func TestRPCModeAnswersStateAndCommands(t *testing.T) {
	url, _ := fakeAnthropic(t)
	_, env := p7Env(t, url, "")
	c := startRPC(t, t.TempDir(), env...)
	c.awaitType(10*time.Second, "ready")

	c.send(map[string]any{"id": "s", "type": "get_state"})
	res := c.awaitResponse(10*time.Second, "get_state")
	data, _ := res["data"].(map[string]any)
	if data == nil || data["sessionId"] == "" {
		t.Fatalf("get_state = %v", res)
	}
	if data["isStreaming"] != false {
		t.Fatalf("idle session reported as streaming: %v", data)
	}

	c.send(map[string]any{"id": "c", "type": "get_commands"})
	res = c.awaitResponse(10*time.Second, "get_commands")
	body := string(mustJSON(res["data"]))
	for _, want := range []string{`"compact"`, `"tree"`, `"reload"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("get_commands is missing %s: %s", want, body)
		}
	}
}

// An unknown command must be answered, not ignored. A client waiting on an id
// that never comes back is indistinguishable from a hung agent.
func TestRPCModeRejectsUnknownCommands(t *testing.T) {
	url, _ := fakeAnthropic(t)
	_, env := p7Env(t, url, "")
	c := startRPC(t, t.TempDir(), env...)
	c.awaitType(10*time.Second, "ready")

	c.send(map[string]any{"id": "x", "type": "definitely_not_a_command"})
	res := c.awaitResponse(10*time.Second, "definitely_not_a_command")
	if res["success"] != false || !strings.Contains(res["error"].(string), "unknown command") {
		t.Fatalf("response = %v", res)
	}
	if res["id"] != "x" {
		t.Fatalf("the response lost the command id: %v", res)
	}
}

func TestRPCModeSurvivesMalformedInput(t *testing.T) {
	url, _ := fakeAnthropic(t)
	_, env := p7Env(t, url, "")
	c := startRPC(t, t.TempDir(), env...)
	c.awaitType(10*time.Second, "ready")

	if _, err := c.in.Write([]byte("this is not json\n")); err != nil {
		t.Fatal(err)
	}
	c.await(10*time.Second, func(m map[string]any) bool {
		return m["type"] == "response" && m["success"] == false
	})

	// Still alive and answering.
	c.send(map[string]any{"id": "after", "type": "get_state"})
	if res := c.awaitResponse(10*time.Second, "get_state"); res["success"] != true {
		t.Fatalf("the server died on bad input: %v", res)
	}
}

// The P8 gate, whole: an extension in its own process asks a question, tau
// relays it to an rpc client in a third process, the client answers, and the
// answer decides whether a tool runs. Three processes, two protocols, nothing
// mocked.
func TestRPCModeProxiesASubprocessExtensionDialog(t *testing.T) {
	url, _ := fakeAnthropic(t, writeStream(toolCallStream("echo the-tool-ran")))
	_, env := p7Env(t, url, "")
	ext := buildAskingExtension(t)

	c := startRPCWith(t, t.TempDir(), []string{"-e", ext}, env...)
	c.awaitType(15*time.Second, "ready")

	// The extension asked, and the question came out on the client's stream in
	// Pi's own request shape.
	req := c.awaitType(15*time.Second, "extension_ui_request")
	if req["method"] != "confirm" || req["title"] != "Allow tools?" {
		t.Fatalf("ui request = %v", req)
	}
	id, _ := req["id"].(string)
	if id == "" {
		t.Fatalf("ui request carried no id: %v", req)
	}

	c.send(map[string]any{"type": "extension_ui_response", "id": id, "confirmed": true})
	// The extension says so itself, on a stream a program can read.
	c.await(15*time.Second, func(m map[string]any) bool {
		return m["type"] == "extension_log" && strings.Contains(toS(m["delta"]), "answered=true")
	})

	c.send(map[string]any{"id": "1", "type": "prompt", "message": "run something"})
	end := c.awaitType(20*time.Second, "tool_execution_end")
	if end["isError"] == true {
		t.Fatalf("the gate blocked the call after the user allowed it: %v", end)
	}
	if !strings.Contains(string(mustJSON(end["result"])), "the-tool-ran") {
		t.Fatalf("tool result = %s", mustJSON(end["result"]))
	}
}

// The same path, refused. A client that declines must leave the gate closed:
// "no answer" and "yes" have to stay different all the way down.
func TestRPCModeDialogCancellationLeavesTheGateClosed(t *testing.T) {
	url, _ := fakeAnthropic(t, writeStream(toolCallStream("echo the-tool-ran")))
	_, env := p7Env(t, url, "")
	ext := buildAskingExtension(t)

	c := startRPCWith(t, t.TempDir(), []string{"-e", ext}, env...)
	c.awaitType(15*time.Second, "ready")

	req := c.awaitType(15*time.Second, "extension_ui_request")
	id, _ := req["id"].(string)
	// Dismissing the dialog is a "no", not a third outcome: an unanswered
	// permission question must not be readable as consent.
	c.send(map[string]any{"type": "extension_ui_response", "id": id, "cancelled": true})
	c.await(15*time.Second, func(m map[string]any) bool {
		return m["type"] == "extension_log" && strings.Contains(toS(m["delta"]), "answered=false")
	})

	c.send(map[string]any{"id": "1", "type": "prompt", "message": "run something"})
	end := c.awaitType(20*time.Second, "tool_execution_end")
	if end["isError"] != true {
		t.Fatalf("an unanswered gate let the tool run: %v", end)
	}
	if !strings.Contains(string(mustJSON(end["result"])), "no permission yet") {
		t.Fatalf("the block reason did not reach the model: %s", mustJSON(end["result"]))
	}
}

// A slash command reaches an extension through a prompt, which is what
// get_commands means by "available for invocation".
func TestRPCModePromptDispatchesSlashCommands(t *testing.T) {
	url, _ := fakeAnthropic(t)
	_, env := p7Env(t, url, "")
	ext := buildAskingExtension(t)

	c := startRPCWith(t, t.TempDir(), []string{"-e", ext}, env...)
	c.awaitType(15*time.Second, "ready")

	c.send(map[string]any{"id": "1", "type": "prompt", "message": "/askstate"})
	res := c.awaitResponse(15*time.Second, "prompt")
	if res["success"] != true {
		t.Fatalf("the command failed: %v", res)
	}
	c.waitForText(15*time.Second, `"allowed=`)
}

func toS(v any) string {
	s, _ := v.(string)
	return s
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// buildAskingExtension compiles the wire-protocol extension that opens a
// dialog as soon as the session starts.
func buildAskingExtension(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "askext")
	out, err := exec.Command("go", "build", "-o", bin, "./testdata/askext").CombinedOutput()
	if err != nil {
		t.Fatalf("build askext: %s", out)
	}
	return bin
}
