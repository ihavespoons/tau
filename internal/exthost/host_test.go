package exthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/extension/wire"
)

// demoBin builds the test extension once per package run.
var demoBin = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "exthost-demo")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "demoext")
	out, err := exec.Command("go", "build", "-o", bin, "./testdata/demoext").CombinedOutput()
	if err != nil {
		return "", errors.New(string(out))
	}
	return bin, nil
})

type capture struct {
	mu       sync.Mutex
	logs     []string
	warnings []string
	suspends []string
	stderr   bytes.Buffer
}

func (c *capture) Logs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.logs...)
}

func (c *capture) hasLog(sub string) bool {
	for _, l := range c.Logs() {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// waitForLog polls until a log line containing sub arrives. Extension-initiated
// work runs on its own goroutines, so there is no synchronous moment to assert
// at; the bound is what turns a lost frame into a failure instead of a hang.
func (c *capture) waitForLog(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.hasLog(sub) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no log containing %q; saw %v; stderr: %s", sub, c.Logs(), c.stderr.String())
}

func spawnDemo(t *testing.T, mode string) (*Host, *capture) {
	t.Helper()
	bin, err := demoBin()
	if err != nil {
		t.Fatalf("build demoext: %v", err)
	}
	cap := &capture{}
	opts := Options{
		Cwd:     t.TempDir(),
		Trusted: true,
		Stderr:  &cap.stderr,
		OnLog: func(level, msg string) {
			cap.mu.Lock()
			cap.logs = append(cap.logs, level+": "+msg)
			cap.mu.Unlock()
		},
		OnWarning: func(name, msg string) {
			cap.mu.Lock()
			cap.warnings = append(cap.warnings, name+": "+msg)
			cap.mu.Unlock()
		},
		OnSuspend: func(name string, reason error) {
			cap.mu.Lock()
			cap.suspends = append(cap.suspends, name+": "+reason.Error())
			cap.mu.Unlock()
		},
	}
	spec := Spec{Name: "demo", Path: "testdata/demoext", Command: bin}
	if mode != "" {
		spec.Env = []string{"TAU_DEMO_MODE=" + mode}
	}
	h, err := Spawn(context.Background(), spec, opts)
	if err != nil {
		t.Fatalf("spawn: %v (stderr: %s)", err, cap.stderr.String())
	}
	t.Cleanup(func() { h.Stop("exit") })
	return h, cap
}

// loadDemo spawns the extension and loads it into a real Runner, so what the
// test drives is the same dispatch path a Go extension goes through.
func loadDemo(t *testing.T, mode string) (*Host, *extension.Runner, *capture) {
	t.Helper()
	h, cap := spawnDemo(t, mode)
	r := extension.NewRunner(extension.RunnerOptions{
		Mode: extension.ModeTUI, Cwd: ".", Trusted: true, UI: &stubUI{},
	})
	if err := r.Load(h.Extension()); err != nil {
		t.Fatalf("load: %v (stderr: %s)", err, cap.stderr.String())
	}
	r.Bind(&stubRuntime{})
	return h, r, cap
}

func TestHandshakeDeclaresEverything(t *testing.T) {
	h, cap := spawnDemo(t, "")
	d := h.Declaration()

	if h.Name() != "demoext" {
		t.Fatalf("name = %q", h.Name())
	}
	if len(d.Tools) != 1 || d.Tools[0].Name != "demo_echo" {
		t.Fatalf("tools = %+v", d.Tools)
	}
	if len(d.Commands) != 1 || !d.Commands[0].Completions {
		t.Fatalf("commands = %+v", d.Commands)
	}
	if len(d.Shortcuts) != 1 || len(d.Flags) != 1 || len(d.Renderers) != 1 {
		t.Fatalf("declaration = %+v", d)
	}
	cap.mu.Lock()
	warnings := append([]string(nil), cap.warnings...)
	cap.mu.Unlock()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "demo warning") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestHandshakeRefusalIsALoadFailure(t *testing.T) {
	bin, err := demoBin()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = Spawn(context.Background(), Spec{
		Name: "demo", Command: bin, Env: []string{"TAU_DEMO_MODE=refuse"},
	}, Options{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("an extension that declined to load was loaded anyway")
	}
	if !strings.Contains(err.Error(), "declines to load") {
		t.Fatalf("err = %v", err)
	}
}

// A version tau does not speak is refused rather than half-understood: a
// permission gate that silently stops gating is worse than one that fails.
func TestProtocolMismatchIsRefused(t *testing.T) {
	bin, err := demoBin()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = Spawn(context.Background(), Spec{
		Name: "demo", Command: bin, Env: []string{"TAU_DEMO_MODE=badversion"},
	}, Options{Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("err = %v, want a protocol complaint", err)
	}
}

func TestSpawnFailsWhenTheCommandDoesNotExist(t *testing.T) {
	_, err := Spawn(context.Background(), Spec{
		Name: "nope", Command: filepath.Join(t.TempDir(), "not-a-program"),
	}, Options{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("spawning a missing program succeeded")
	}
}

func TestSubscribedEventsRoundTrip(t *testing.T) {
	_, r, _ := loadDemo(t, "")

	res := r.EmitInput(context.Background(), "shout", nil, "tui", "")
	if res == nil || res.Action != extension.InputTransform || res.Text != "SHOUT" {
		t.Fatalf("input result = %+v", res)
	}

	res = r.EmitInput(context.Background(), "quiet", nil, "tui", "")
	if res == nil || res.Action != extension.InputContinue {
		t.Fatalf("unchanged input became %+v", res)
	}
}

// An event nobody subscribed to must never be written. The extension declares
// six; the rest have no handler at all, which is what makes the dispatch free.
func TestUnsubscribedEventsAreNeverDispatched(t *testing.T) {
	_, r, cap := loadDemo(t, "")

	// user_bash is not in the subscription list.
	if got := r.EmitUserBash(context.Background(), &extension.UserBashEvent{Command: "ls"}); got != nil {
		t.Fatalf("an unsubscribed event produced a result: %+v", got)
	}
	if errs := r.Errors(); len(errs) != 0 {
		t.Fatalf("errors = %v (stderr: %s)", errs, cap.stderr.String())
	}
}

func TestToolCallGateBlocks(t *testing.T) {
	_, r, _ := loadDemo(t, "")

	got := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c1", ToolName: "demo_echo",
		Args: map[string]any{"text": "forbidden"},
	})
	if got == nil || !got.Block {
		t.Fatalf("the gate did not block: %+v", got)
	}
	if !strings.Contains(got.Reason, "demoext says no") {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestToolCallAllows(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	got := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c1", ToolName: "demo_echo", Args: map[string]any{"text": "fine"},
	})
	if got != nil && got.Block {
		t.Fatalf("an allowed call was blocked: %+v", got)
	}
}

// In process a handler edits the shared args map. Over a wire there is no
// shared map, so the edit travels back explicitly and the host applies it.
func TestToolCallArgumentRewriteCrossesTheWire(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	ev := &extension.ToolCallEvent{
		ToolCallID: "c1", ToolName: "demo_echo", Args: map[string]any{"text": "rewrite"},
	}
	r.EmitToolCall(context.Background(), ev)
	if ev.Args["text"] != "rewritten by demoext" {
		t.Fatalf("args = %+v", ev.Args)
	}
}

// The load-bearing failure mode of the whole phase: a gate that stops
// answering must not be read as consent.
func TestToolCallFailsClosedWhenTheExtensionHangs(t *testing.T) {
	_, r, _ := loadDemo(t, "hang")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan *extension.ToolCallResult, 1)
	go func() {
		done <- r.EmitToolCall(ctx, &extension.ToolCallEvent{
			ToolCallID: "c1", ToolName: "demo_echo", Args: map[string]any{"text": "fine"},
		})
	}()

	select {
	case got := <-done:
		if got == nil || !got.Block {
			t.Fatalf("a hung gate allowed the call: %+v", got)
		}
		if !strings.Contains(got.Reason, "could not be consulted") {
			t.Fatalf("reason = %q", got.Reason)
		}
	case <-time.After(GracePeriod + 10*time.Second):
		t.Fatal("EmitToolCall never returned: the grace period does not bound the wait")
	}
}

func TestToolCallFailsClosedWhenTheExtensionCrashes(t *testing.T) {
	h, r, _ := loadDemo(t, "crash")

	got := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c1", ToolName: "demo_echo", Args: map[string]any{"text": "fine"},
	})
	if got == nil || !got.Block {
		t.Fatalf("a dead gate allowed the call: %+v", got)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the host never noticed the process had exited")
	}
}

func TestToolCallFailsClosedWhenTheHandlerErrors(t *testing.T) {
	_, r, _ := loadDemo(t, "handlererror")
	got := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c1", ToolName: "demo_echo", Args: map[string]any{"text": "fine"},
	})
	if got == nil || !got.Block {
		t.Fatalf("a failing gate allowed the call: %+v", got)
	}
}

// Every other event fails OPEN: a broken extension must not stop the agent.
func TestNonGateEventsFailOpen(t *testing.T) {
	h, r, _ := loadDemo(t, "crash")

	// Kill it first — this mode exits when the gate is consulted.
	r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c", ToolName: "demo_echo", Args: map[string]any{"text": "fine"},
	})
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the extension did not exit")
	}

	msgs := []ai.Message{ai.UserMessage{Content: ai.UserContent{Text: "hi"}, Timestamp: 1}}
	got := r.EmitContext(context.Background(), msgs)
	if len(got) != 1 {
		t.Fatalf("a dead extension changed the context: %+v", got)
	}
	if len(r.Errors()) == 0 {
		t.Fatal("a dead extension's failure was not reported")
	}
}

// An empty result and no result are different answers. Confusing them here
// would silently erase the conversation.
func TestNoOpinionIsNotAnEmptyContext(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	msgs := []ai.Message{
		ai.UserMessage{Content: ai.UserContent{Text: "one"}, Timestamp: 1},
		ai.UserMessage{Content: ai.UserContent{Text: "two"}, Timestamp: 2},
	}
	got := r.EmitContext(context.Background(), msgs)
	if len(got) != 2 {
		t.Fatalf("silence was read as an empty message list: %d messages left", len(got))
	}
}

func TestHeaderPatchMerges(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	keep := "keep"
	drop := "drop"
	headers := map[string]*string{"x-keep": &keep, "x-drop-me": &drop}

	r.EmitBeforeProviderHeaders(context.Background(), headers)

	if headers["x-demo"] == nil || *headers["x-demo"] != "1" {
		t.Fatalf("added header missing: %+v", headers)
	}
	if headers["x-keep"] == nil || *headers["x-keep"] != "keep" {
		t.Fatal("an untouched header was lost")
	}
	if v, ok := headers["x-drop-me"]; !ok || v != nil {
		t.Fatalf("a null header should mean delete, got %v", v)
	}
}

func TestWireToolExecutes(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	tool := findTool(t, r, "demo_echo")

	var partials []string
	res, err := tool.Execute(context.Background(), "call-1",
		json.RawMessage(`{"text":"hello"}`),
		func(p agent.ToolResult) { partials = append(partials, textOf(p)) })
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := textOf(res); got != "echo: hello" {
		t.Fatalf("output = %q", got)
	}
	if len(partials) != 1 || partials[0] != "working on hello" {
		t.Fatalf("partials = %v", partials)
	}
	details, _ := res.Details.(map[string]any)
	if details["callId"] != "call-1" {
		t.Fatalf("details = %+v", res.Details)
	}
}

// A tool that fails is a result the model can read, not a transport failure.
func TestWireToolFailureIsAnError(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	tool := findTool(t, r, "demo_echo")

	_, err := tool.Execute(context.Background(), "c", json.RawMessage(`{"text":"fail"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "the tool refused") {
		t.Fatalf("err = %v", err)
	}
}

func TestWireToolCancellationReachesTheExtension(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	tool := findTool(t, r, "demo_echo")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	done := make(chan agent.ToolResult, 1)
	go func() {
		res, _ := tool.Execute(ctx, "c", json.RawMessage(`{"text":"hang"}`), nil)
		done <- res
	}()

	select {
	case res := <-done:
		// The extension answers inside the grace period, so the cancelled call
		// still returns its answer rather than a fail-safe.
		if got := textOf(res); got != "cancelled" {
			t.Fatalf("output = %q, want the extension's own cancellation answer", got)
		}
	case <-time.After(GracePeriod + 5*time.Second):
		t.Fatal("a cancelled tool call never returned")
	}
}

func TestToolSchemaSurvivesTheWire(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	def := findTool(t, r, "demo_echo").Def()
	if def.Parameters == nil || def.Parameters.Type != "object" {
		t.Fatalf("schema = %+v", def.Parameters)
	}
	if _, ok := def.Parameters.Properties["text"]; !ok {
		t.Fatalf("schema lost its properties: %+v", def.Parameters)
	}
	if len(def.Parameters.Required) != 1 || def.Parameters.Required[0] != "text" {
		t.Fatalf("required = %v", def.Parameters.Required)
	}
}

func TestCommandRuns(t *testing.T) {
	_, r, cap := loadDemo(t, "")
	cmds := r.Commands()
	if len(cmds) != 1 || cmds[0].Name != "demo" {
		t.Fatalf("commands = %+v", cmds)
	}
	if err := cmds[0].Handler(context.Background(), "hello", r.NewCommandContext()); err != nil {
		t.Fatalf("command: %v", err)
	}
	cap.waitForLog(t, "command ran with args hello")
}

func TestCommandFailureSurfaces(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	err := r.Commands()[0].Handler(context.Background(), "boom", r.NewCommandContext())
	if err == nil || !strings.Contains(err.Error(), "the command failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestCompletionsRoundTrip(t *testing.T) {
	_, r, _ := loadDemo(t, "")
	items := r.Commands()[0].ArgumentCompletions("ab")
	if len(items) != 2 || items[0].Value != "ab-one" {
		t.Fatalf("items = %+v", items)
	}
}

// Completions run while the user types. A slow extension must not make the
// editor feel stuck, so the deadline is short and a miss yields nothing.
func TestSlowCompletionsGiveUpQuickly(t *testing.T) {
	_, r, _ := loadDemo(t, "slowcompletions")

	start := time.Now()
	items := r.Commands()[0].ArgumentCompletions("ab")
	elapsed := time.Since(start)

	if items != nil {
		t.Fatalf("a slow extension produced completions: %+v", items)
	}
	if elapsed > GracePeriod+2*time.Second {
		t.Fatalf("waited %s for completions", elapsed)
	}
}

func TestRendererRoundTrip(t *testing.T) {
	h, _, _ := loadDemo(t, "")
	if !h.Renders("message", "assistant") {
		t.Fatal("declared renderer not reported")
	}
	if h.Renders("entry", "") {
		t.Fatal("an undeclared renderer was reported")
	}
	lines, err := h.Render(context.Background(), "message", 80, map[string]any{"role": "assistant"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "width 80") {
		t.Fatalf("lines = %v", lines)
	}
}

func TestSlowRendererFallsBack(t *testing.T) {
	h, _, _ := loadDemo(t, "slowrender")
	start := time.Now()
	_, err := h.Render(context.Background(), "message", 80, map[string]any{})
	if err == nil {
		t.Fatal("a slow renderer blocked the draw path instead of failing")
	}
	if elapsed := time.Since(start); elapsed > GracePeriod+2*time.Second {
		t.Fatalf("waited %s to give up on a renderer", elapsed)
	}
}

func TestUIRequestsAndActionsReachTheHost(t *testing.T) {
	_, r, cap := loadDemo(t, "askui")
	r.EmitSessionStart(context.Background(), &extension.SessionStartEvent{Cwd: "."})

	cap.waitForLog(t, "asked everything")
	for _, want := range []string{`confirm={"confirmed":true}`, `select={"value":"a"}`, `"exitCode":0`, `"id":"stub-model"`} {
		if !cap.hasLog(want) {
			t.Fatalf("missing %q in %v", want, cap.Logs())
		}
	}
}

// Streaming events must not put a subprocess round trip in the agent loop.
func TestHotEventsDoNotBlockTheAgent(t *testing.T) {
	_, r, _ := loadDemo(t, "")

	msg := ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "x"}}, Timestamp: 1}
	start := time.Now()
	const n = 500
	for i := 0; i < n; i++ {
		r.EmitMessageUpdate(context.Background(), &extension.MessageUpdateEvent{Message: msg})
	}
	elapsed := time.Since(start)

	// 500 synchronous round trips through another process would take orders of
	// magnitude longer than this; the assertion is about the shape, not the
	// number. Whether any payload is actually conflated depends on how fast
	// the pipe drains, which is why that is asserted separately below.
	if elapsed > 2*time.Second {
		t.Fatalf("%d hot events took %s: they are being awaited", n, elapsed)
	}
	if errs := r.Errors(); len(errs) != 0 {
		t.Fatalf("hot events reported errors: %v", errs)
	}
}

// blockedWriter stalls its first write until released, which is what makes
// conflation observable: a payload can only be replaced while an earlier one
// is still unsent.
type blockedWriter struct {
	release chan struct{}
	once    sync.Once

	mu     sync.Mutex
	writes int
}

func (b *blockedWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { <-b.release })
	b.mu.Lock()
	b.writes++
	b.mu.Unlock()
	return len(p), nil
}

func (b *blockedWriter) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writes
}

func TestConflatorReplacesUnsentPayloads(t *testing.T) {
	bw := &blockedWriter{release: make(chan struct{})}
	h := &Host{w: wire.NewWriter(bw), closed: make(chan struct{})}
	defer close(h.closed)
	c := newConflator(h)
	defer c.stop()

	const n = 8
	for i := 0; i < n; i++ {
		c.send(wire.Event{
			Type: wire.FrameEvent, ID: "id", Event: "message_update", NoReply: true,
		})
	}

	// The sender is parked inside the first write, so everything after the one
	// it took collapses into a single pending payload.
	deadline := time.Now().Add(2 * time.Second)
	for c.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Dropped() == 0 {
		t.Fatal("nothing was conflated while a write was outstanding")
	}
	if got := c.Dropped(); got > n-1 {
		t.Fatalf("dropped %d of %d: more than could have been queued", got, n)
	}

	close(bw.release)

	// At most two frames reach the wire: the one in flight and the survivor.
	deadline = time.Now().Add(2 * time.Second)
	for bw.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := bw.count(); got > 2 {
		t.Fatalf("%d frames written for %d sends: conflation did not hold", got, n)
	}
}

// Two different hot events must not evict each other: conflation is per event
// type, and a message_update replacing a tool_execution_update would lose one
// stream to keep the other.
func TestConflationIsPerEventType(t *testing.T) {
	bw := &blockedWriter{release: make(chan struct{})}
	h := &Host{w: wire.NewWriter(bw), closed: make(chan struct{})}
	defer close(h.closed)
	c := newConflator(h)
	defer c.stop()

	// Park the sender on a first frame that neither of the two below can
	// replace, so both stay queued together.
	c.send(wire.Event{Type: wire.FrameEvent, Event: "session_start", NoReply: true})
	time.Sleep(50 * time.Millisecond)

	c.send(wire.Event{Type: wire.FrameEvent, Event: "message_update", NoReply: true})
	c.send(wire.Event{Type: wire.FrameEvent, Event: "tool_execution_update", NoReply: true})

	if got := c.Dropped(); got != 0 {
		t.Fatalf("dropped %d: different event types evicted each other", got)
	}

	close(bw.release)
	deadline := time.Now().Add(2 * time.Second)
	for bw.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := bw.count(); got != 3 {
		t.Fatalf("%d frames written, want 3", got)
	}
}

// A result about a session that no longer exists must not be applied to the
// one that replaced it.
func TestStaleGenerationResultsAreDiscarded(t *testing.T) {
	h, r, _ := loadDemo(t, "slowinput")

	done := make(chan *extension.InputResult, 1)
	go func() { done <- r.EmitInput(context.Background(), "shout", nil, "tui", "") }()

	// The extension takes 300ms to answer; replacing the session well inside
	// that leaves the answer in flight when the generation changes.
	time.Sleep(50 * time.Millisecond)
	h.Invalidate()

	select {
	case res := <-done:
		if res == nil {
			t.Fatal("no result at all")
		}
		if res.Action == extension.InputTransform {
			t.Fatalf("a decision about the previous session was applied to this one: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("EmitInput never returned")
	}
	if h.Generation() == 0 {
		t.Fatal("generation never advanced")
	}
}

// The same request, with no session change, must still be applied. Otherwise
// the test above would pass against code that discards everything.
func TestFreshGenerationResultsAreApplied(t *testing.T) {
	_, r, _ := loadDemo(t, "slowinput")
	res := r.EmitInput(context.Background(), "shout", nil, "tui", "")
	if res == nil || res.Action != extension.InputTransform || res.Text != "SHOUT" {
		t.Fatalf("result = %+v", res)
	}
}

func TestSuspensionAfterRepeatedFailures(t *testing.T) {
	h, _, cap := loadDemo(t, "hang")

	for i := 0; i < MaxStrikes; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		id := h.nextRequestID()
		_, _ = h.request(ctx, id, map[string]any{"type": "event", "id": id, "event": "noop"})
		cancel()
		// A caller-cancelled request is not the extension's fault, so drive
		// the strike directly: this asserts the policy, not the accounting.
		h.strike(errors.New("synthetic"))
	}
	if !h.Suspended() {
		t.Fatalf("still live after %d strikes", MaxStrikes)
	}
	cap.mu.Lock()
	n := len(cap.suspends)
	cap.mu.Unlock()
	if n != 1 {
		t.Fatalf("suspend reported %d times", n)
	}
}

func TestSuspendedExtensionAnswersNothingAndBlocksTools(t *testing.T) {
	h, r, _ := loadDemo(t, "")
	for i := 0; i < MaxStrikes; i++ {
		h.strike(errors.New("synthetic"))
	}

	got := r.EmitToolCall(context.Background(), &extension.ToolCallEvent{
		ToolCallID: "c", ToolName: "demo_echo", Args: map[string]any{"text": "fine"},
	})
	if got == nil || !got.Block {
		t.Fatalf("a suspended gate allowed the call: %+v", got)
	}

	res := r.EmitInput(context.Background(), "shout", nil, "tui", "")
	if res == nil || res.Action != extension.InputContinue {
		t.Fatalf("a suspended extension still transformed input: %+v", res)
	}
}

func TestStopIsIdempotentAndTerminates(t *testing.T) {
	h, _ := spawnDemo(t, "")
	h.Stop("exit")
	h.Stop("exit")
	select {
	case <-h.Done():
	default:
		t.Fatal("Stop returned before the process exited")
	}
}

// --- helpers ---

func findTool(t *testing.T, r *extension.Runner, name string) agent.Tool {
	t.Helper()
	for _, tool := range r.Tools() {
		if tool.Def().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not registered; have %d", name, len(r.Tools()))
	return nil
}

func textOf(r agent.ToolResult) string {
	var sb strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(ai.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func fmtSscan(s string, out *int) (int, error) {
	n := 0
	i := 0
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		n = n*10 + int(s[i]-'0')
	}
	*out = n
	return i, nil
}

type stubUI struct{}

func (stubUI) Confirm(context.Context, extension.ConfirmRequest) (bool, error) { return true, nil }
func (stubUI) Select(context.Context, extension.SelectRequest) (int, error)    { return 0, nil }
func (stubUI) Input(context.Context, extension.InputRequest) (string, error)   { return "typed", nil }
func (stubUI) Notify(extension.Notification)                                   {}
func (stubUI) SetStatus(string)                                                {}
func (stubUI) SetTitle(string)                                                 {}
func (stubUI) SetWidget(string, extension.WidgetPosition, extension.Widget)    {}

type stubRuntime struct {
	mu   sync.Mutex
	name string
}

func (s *stubRuntime) RegisterTools([]agent.Tool) error { return nil }
func (s *stubRuntime) SendMessage(ai.Message, string) error {
	return nil
}
func (s *stubRuntime) SetSessionName(n string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.name = n
	return nil
}
func (s *stubRuntime) SessionName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}
func (s *stubRuntime) Exec(_ context.Context, cmd string) (string, int, error) {
	return "ran: " + cmd, 0, nil
}
func (s *stubRuntime) ActiveToolNames() []string     { return []string{"demo_echo"} }
func (s *stubRuntime) SetActiveTools([]string) error { return nil }
func (s *stubRuntime) Model() *ai.Model {
	return &ai.Model{ID: "stub-model", Provider: "stub", Name: "Stub", ContextWindow: 1000}
}
func (s *stubRuntime) SetModel(*ai.Model) error                     { return nil }
func (s *stubRuntime) ThinkingLevel() ai.ModelThinkingLevel         { return "off" }
func (s *stubRuntime) SetThinkingLevel(ai.ModelThinkingLevel) error { return nil }
