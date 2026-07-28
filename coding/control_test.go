package coding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/extension"
	"github.com/ihavespoons/tau/slashcmd"
)

// newTestSession builds a real coding session in temporary directories, so the
// control surface is exercised against the same construction path the CLI uses.
func newTestSession(t *testing.T, opts Options) *Session {
	t.Helper()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)

	if opts.Cwd == "" {
		opts.Cwd = t.TempDir()
	}
	// Without tools the session needs no shell, and these tests are about
	// control flow rather than execution.
	opts.NoTools = true

	cs, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("building session: %v", err)
	}
	t.Cleanup(func() { cs.Close(context.Background(), "test") })
	return cs
}

func TestDefaultModelComesFromSettings(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"),
		[]byte(`{"defaultModel":"claude-opus-4-8","defaultThinkingLevel":"low"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cs, err := New(context.Background(), Options{Cwd: t.TempDir(), NoTools: true})
	if err != nil {
		t.Fatalf("building session: %v", err)
	}
	t.Cleanup(func() { cs.Close(context.Background(), "test") })

	if !strings.Contains(cs.Model.ID, "opus-4-8") {
		t.Errorf("settings did not choose the model, got %q", cs.Model.ID)
	}
	if cs.ThinkingLevel() != "low" {
		t.Errorf("settings did not choose the thinking level, got %q", cs.ThinkingLevel())
	}
}

// An explicit flag beats settings, which beat tau's built-in default.
func TestModelOptionOverridesSettings(t *testing.T) {
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"),
		[]byte(`{"defaultModel":"claude-opus-4-8"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cs, err := New(context.Background(), Options{
		Cwd: t.TempDir(), NoTools: true, ModelID: "claude-sonnet-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(context.Background(), "test") })

	if !strings.Contains(cs.Model.ID, "sonnet-5") {
		t.Errorf("the explicit model should win, got %q", cs.Model.ID)
	}
}

func TestSetModelSwitchesAndRecords(t *testing.T) {
	cs := newTestSession(t, Options{})
	before := cs.Model.ID

	target := ""
	for _, m := range cs.AvailableModels() {
		if m.ID != before {
			target = m.ID
			break
		}
	}
	if target == "" {
		t.Skip("only one model is configured")
	}

	m, err := cs.SetModel(context.Background(), target)
	if err != nil {
		t.Fatalf("switching model: %v", err)
	}
	if m.ID != target || cs.Agent.Model().ID != target {
		t.Errorf("model did not switch: session=%s agent=%s", cs.Model.ID, cs.Agent.Model().ID)
	}
}

func TestSetModelRejectsUnknownSpec(t *testing.T) {
	cs := newTestSession(t, Options{})
	if _, err := cs.SetModel(context.Background(), "not-a-real-model"); err == nil {
		t.Error("an unknown model must be rejected rather than silently ignored")
	}
}

// Cycling wraps at both ends and always lands on a real model.
func TestCycleModelWraps(t *testing.T) {
	cs := newTestSession(t, Options{})
	set := cs.CycleModels()
	if len(set) < 2 {
		t.Skip("need at least two models to cycle")
	}

	start := cs.Model.ID
	for range len(set) {
		if cs.CycleModel(context.Background(), 1) == nil {
			t.Fatal("cycling produced no model")
		}
	}
	if cs.Model.ID != start {
		t.Errorf("a full cycle should return to the start: %s -> %s", start, cs.Model.ID)
	}

	cs.CycleModel(context.Background(), -1)
	if cs.Model.ID == start {
		t.Error("cycling backwards did nothing")
	}
}

// The thinking level is clamped to what the model actually supports, so the UI
// never advertises a level that will not be requested.
func TestThinkingLevelIsClamped(t *testing.T) {
	cs := newTestSession(t, Options{})
	got := cs.SetThinkingLevel(context.Background(), "max")
	supported := ai.SupportedThinkingLevels(cs.Model)

	found := false
	for _, l := range supported {
		if l == got {
			found = true
		}
	}
	if !found {
		t.Errorf("level %q is not among the model's supported levels %v", got, supported)
	}
}

func TestCycleThinkingLevelAdvances(t *testing.T) {
	cs := newTestSession(t, Options{})
	if len(ai.SupportedThinkingLevels(cs.Model)) < 2 {
		t.Skip("model has a single thinking level")
	}
	before := cs.ThinkingLevel()
	if after := cs.CycleThinkingLevel(context.Background(), 1); after == before {
		t.Errorf("cycling did not change the level (%q)", after)
	}
}

// /new must produce a different session file and an empty transcript, or
// "start fresh" silently keeps talking to the old conversation.
func TestStartSessionReplacesTheTranscript(t *testing.T) {
	cs := newTestSession(t, Options{})
	first := cs.Path

	cs.Agent.SetMessages([]ai.Message{
		ai.UserMessage{Content: ai.UserContent{Text: "old conversation"}},
	})

	if err := cs.StartSession(context.Background()); err != nil {
		t.Fatalf("starting a new session: %v", err)
	}
	if cs.Path == first {
		t.Error("a new session should live in a new file")
	}
	if len(cs.Agent.Messages()) != 0 {
		t.Errorf("the old transcript survived: %v", cs.Agent.Messages())
	}
}

func TestSwitchSessionRestoresTheTranscript(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{})

	// Record a message in the first session, then move away and back.
	if _, err := cs.Session.AppendMessage(ctx, ai.UserMessage{
		Content: ai.UserContent{Text: "remember me"}, Timestamp: 1,
	}); err != nil {
		t.Fatal(err)
	}
	original := cs.Path

	if err := cs.StartSession(ctx); err != nil {
		t.Fatal(err)
	}

	metas, err := cs.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var target *string
	for i := range metas {
		if metas[i].Path == original {
			target = &metas[i].Path
			if err := cs.SwitchSession(ctx, metas[i]); err != nil {
				t.Fatalf("switching back: %v", err)
			}
			break
		}
	}
	if target == nil {
		t.Fatalf("the original session was not listed: %+v", metas)
	}

	msgs := cs.Agent.Messages()
	if len(msgs) == 0 {
		t.Fatal("switching back restored nothing")
	}
	if !strings.Contains(msgs[0].(ai.UserMessage).Content.String(), "remember me") {
		t.Errorf("restored the wrong transcript: %+v", msgs)
	}
}

// A session swap must invalidate contexts extensions captured earlier, or a
// handler could mutate a conversation that no longer exists.
func TestSwitchSessionInvalidatesExtensionContexts(t *testing.T) {
	var captured *extension.Context
	ext := extension.Extension{
		Name: "capture",
		Factory: func(api *extension.API) error {
			api.OnSessionStart(func(_ context.Context, _ *extension.SessionStartEvent, ec *extension.Context) error {
				if captured == nil {
					captured = ec
				}
				return nil
			})
			return nil
		},
	}

	cs := newTestSession(t, Options{Extensions: []extension.Extension{ext}})
	if captured == nil {
		t.Fatal("the extension never received a context")
	}
	if err := captured.Err(); err != nil {
		t.Fatalf("the context was stale before anything changed: %v", err)
	}

	if err := cs.StartSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := captured.Err(); err == nil {
		t.Error("a context captured before the swap must go stale")
	}
}

// Live registration is what makes an MCP server announcing tools/list_changed
// work: a tool added after startup has to reach the model, not just the
// registry.
func TestLiveToolRegistrationReachesTheAgent(t *testing.T) {
	late := agent.MustNew("late_tool", "late", "registered after startup",
		func(_ context.Context, _ string, _ struct{}, _ agent.UpdateFunc) (agent.ToolResult, error) {
			return agent.Text("ok"), nil
		})

	var api *extension.API
	ext := extension.Extension{
		Name: "later",
		Factory: func(a *extension.API) error {
			api = a
			return nil
		},
	}

	cs := newTestSession(t, Options{Extensions: []extension.Extension{ext}})
	for _, name := range cs.ToolNames() {
		if name == "late_tool" {
			t.Fatal("the tool was registered before the test asked for it")
		}
	}

	api.RegisterTool(late)

	found := false
	for _, name := range cs.ToolNames() {
		if name == "late_tool" {
			found = true
		}
	}
	if !found {
		t.Errorf("a late registration never reached the agent: %v", cs.ToolNames())
	}
	if _, ok := cs.ToolsByName()["late_tool"]; !ok {
		t.Error("a late registration never reached the registry")
	}
}

// Re-registering the same name replaces rather than duplicates it — otherwise
// a server re-announcing its tools would offer each one twice.
func TestReRegisteringAToolReplacesIt(t *testing.T) {
	build := func(desc string) agent.Tool {
		return agent.MustNew("dup", "dup", desc,
			func(_ context.Context, _ string, _ struct{}, _ agent.UpdateFunc) (agent.ToolResult, error) {
				return agent.Text("ok"), nil
			})
	}

	var api *extension.API
	cs := newTestSession(t, Options{Extensions: []extension.Extension{{
		Name:    "dup",
		Factory: func(a *extension.API) error { api = a; return nil },
	}}})

	api.RegisterTool(build("first"))
	api.RegisterTool(build("second"))

	count := 0
	for _, name := range cs.ToolNames() {
		if name == "dup" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected one registration, found %d: %v", count, cs.ToolNames())
	}
	if got := cs.ToolsByName()["dup"].Def().Description; got != "second" {
		t.Errorf("the later registration should win, got %q", got)
	}
}

func TestRunCommandDispatches(t *testing.T) {
	cs := newTestSession(t, Options{})

	res, err := cs.RunCommand(context.Background(), "/help")
	if err != nil {
		t.Fatalf("/help: %v", err)
	}
	if !strings.Contains(res.Output, "/model") {
		t.Errorf("/help should list commands:\n%s", res.Output)
	}

	if _, err := cs.RunCommand(context.Background(), "/definitely-not-a-command"); err == nil {
		t.Error("an unknown command must report an error")
	}
}

// Without a UI, the commands that need dialogs stay advertised but refuse to
// run, rather than half-working or crashing.
func TestInteractiveCommandsAreUnavailableHeadless(t *testing.T) {
	cs := newTestSession(t, Options{})

	if _, ok := cs.Commands.Lookup("resume"); !ok {
		t.Fatal("/resume should still be advertised headless")
	}
	_, err := cs.RunCommand(context.Background(), "/resume")
	if err == nil || !strings.Contains(err.Error(), slashcmd.ErrNotImplemented.Error()) {
		t.Errorf("expected ErrNotImplemented, got %v", err)
	}
}

func TestSessionSummaryReportsTheEssentials(t *testing.T) {
	cs := newTestSession(t, Options{})
	out := cs.SessionSummary()
	for _, want := range []string{"model", "cwd", "session", "usage"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
}

func TestSaveTrustRoundTrips(t *testing.T) {
	cs := newTestSession(t, Options{})
	ctx := context.Background()

	if _, err := cs.SaveTrust(ctx, "always"); err != nil {
		t.Fatalf("saving trust: %v", err)
	}
	if _, err := cs.SaveTrust(ctx, "never"); err != nil {
		t.Fatalf("saving distrust: %v", err)
	}
	if _, err := cs.SaveTrust(ctx, "nonsense"); err == nil {
		t.Error("an unknown decision must be rejected")
	}
}

func TestLastAssistantText(t *testing.T) {
	cs := newTestSession(t, Options{})
	if got := cs.LastAssistantText(); got != "" {
		t.Errorf("an empty session has nothing to copy, got %q", got)
	}

	cs.Agent.SetMessages([]ai.Message{
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "first"}}},
		ai.UserMessage{Content: ai.UserContent{Text: "question"}},
		ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: "second"}}},
	})
	if got := cs.LastAssistantText(); got != "second" {
		t.Errorf("expected the most recent reply, got %q", got)
	}
}
