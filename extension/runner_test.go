package extension

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

func newRunner(t *testing.T, exts ...Extension) *Runner {
	t.Helper()
	r := NewRunner(RunnerOptions{Mode: ModeTUI, Cwd: "/work", Trusted: true})
	for _, e := range exts {
		_ = r.Load(e)
	}
	return r
}

// ext builds a named extension from a factory body.
func ext(name string, fn func(*API)) Extension {
	return Extension{Name: name, Path: "/ext/" + name, Factory: func(a *API) error {
		fn(a)
		return nil
	}}
}

func userMsg(s string) ai.UserMessage {
	return ai.UserMessage{Content: ai.UserContent{Text: s}, Timestamp: 1}
}

// --- tool_call: block short-circuits, failure blocks (fails closed) ---

func TestToolCallBlockShortCircuits(t *testing.T) {
	var secondRan bool
	r := newRunner(t,
		ext("gate", func(a *API) {
			a.OnToolCall(func(context.Context, *ToolCallEvent, *Context) (*ToolCallResult, error) {
				return &ToolCallResult{Block: true, Reason: "denied by policy"}, nil
			})
		}),
		ext("later", func(a *API) {
			a.OnToolCall(func(context.Context, *ToolCallEvent, *Context) (*ToolCallResult, error) {
				secondRan = true
				return nil, nil
			})
		}),
	)

	res := r.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash"})
	if res == nil || !res.Block || res.Reason != "denied by policy" {
		t.Fatalf("result = %+v, want a block", res)
	}
	if secondRan {
		t.Error("a blocking handler must short-circuit the rest")
	}
}

// A permission gate that errors must fail CLOSED. This is the one event where
// a handler failure changes behavior instead of being ignored.
func TestToolCallHandlerFailureBlocksTool(t *testing.T) {
	r := newRunner(t, ext("broken", func(a *API) {
		a.OnToolCall(func(context.Context, *ToolCallEvent, *Context) (*ToolCallResult, error) {
			return nil, errors.New("policy server unreachable")
		})
	}))

	res := r.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash"})
	if res == nil || !res.Block {
		t.Fatalf("a failing tool_call handler must block, got %+v", res)
	}
	if !strings.Contains(res.Reason, "policy server unreachable") {
		t.Errorf("reason should name the failure: %q", res.Reason)
	}
	if len(r.Errors()) != 1 {
		t.Errorf("failure should be reported, got %d errors", len(r.Errors()))
	}
}

func TestToolCallPanicBlocksTool(t *testing.T) {
	r := newRunner(t, ext("panicky", func(a *API) {
		a.OnToolCall(func(context.Context, *ToolCallEvent, *Context) (*ToolCallResult, error) {
			panic("boom")
		})
	}))
	res := r.EmitToolCall(context.Background(), &ToolCallEvent{ToolName: "bash"})
	if res == nil || !res.Block {
		t.Fatal("a panicking tool_call handler must block")
	}
}

// Args are mutable: a handler can patch arguments and later handlers see it.
func TestToolCallArgsAreMutable(t *testing.T) {
	var seen string
	r := newRunner(t,
		ext("patcher", func(a *API) {
			a.OnToolCall(func(_ context.Context, ev *ToolCallEvent, _ *Context) (*ToolCallResult, error) {
				ev.Args["path"] = "/safe/path"
				return nil, nil
			})
		}),
		ext("observer", func(a *API) {
			a.OnToolCall(func(_ context.Context, ev *ToolCallEvent, _ *Context) (*ToolCallResult, error) {
				seen, _ = ev.Args["path"].(string)
				return nil, nil
			})
		}),
	)
	ev := &ToolCallEvent{ToolName: "read", Args: map[string]any{"path": "/etc/passwd"}}
	r.EmitToolCall(context.Background(), ev)
	if seen != "/safe/path" {
		t.Errorf("later handler saw %q, want the patched value", seen)
	}
}

// --- tool_result: field-wise middleware, later sees earlier ---

func TestToolResultPatchesAreCumulative(t *testing.T) {
	r := newRunner(t,
		ext("a", func(a *API) {
			a.OnToolResult(func(context.Context, *ToolResultEvent, *Context) (*ToolResultResult, error) {
				return &ToolResultResult{Content: ai.ContentList{ai.TextContent{Text: "first"}}}, nil
			})
		}),
		ext("b", func(a *API) {
			a.OnToolResult(func(_ context.Context, ev *ToolResultEvent, _ *Context) (*ToolResultResult, error) {
				// Must observe extension a's patch.
				if got := ev.Content[0].(ai.TextContent).Text; got != "first" {
					t.Errorf("second handler saw %q, want the first handler's patch", got)
				}
				isErr := true
				return &ToolResultResult{IsError: &isErr}, nil
			})
		}),
	)

	res := r.EmitToolResult(context.Background(), &ToolResultEvent{
		ToolName: "bash", Content: ai.ContentList{ai.TextContent{Text: "original"}},
	})
	if res == nil {
		t.Fatal("expected a patched result")
	}
	if res.Content[0].(ai.TextContent).Text != "first" {
		t.Errorf("content = %v", res.Content)
	}
	if res.IsError == nil || !*res.IsError {
		t.Error("isError patch was lost")
	}
}

func TestToolResultUnmodifiedReturnsNil(t *testing.T) {
	r := newRunner(t, ext("noop", func(a *API) {
		a.OnToolResult(func(context.Context, *ToolResultEvent, *Context) (*ToolResultResult, error) {
			return nil, nil
		})
	}))
	if res := r.EmitToolResult(context.Background(), &ToolResultEvent{ToolName: "x"}); res != nil {
		t.Errorf("unmodified result should be nil, got %+v", res)
	}
}

// --- message_end: chains, rejects role changes ---

func TestMessageEndChainsReplacements(t *testing.T) {
	r := newRunner(t,
		ext("a", func(a *API) {
			a.OnMessageEnd(func(_ context.Context, ev *MessageEndEvent, _ *Context) (*MessageEndResult, error) {
				return &MessageEndResult{Message: userMsg("first")}, nil
			})
		}),
		ext("b", func(a *API) {
			a.OnMessageEnd(func(_ context.Context, ev *MessageEndEvent, _ *Context) (*MessageEndResult, error) {
				if got := ev.Message.(ai.UserMessage).Content.Text; got != "first" {
					t.Errorf("second handler saw %q, want chained value", got)
				}
				return &MessageEndResult{Message: userMsg("second")}, nil
			})
		}),
	)
	out := r.EmitMessageEnd(context.Background(), userMsg("original"))
	if out == nil || out.(ai.UserMessage).Content.Text != "second" {
		t.Errorf("out = %#v", out)
	}
}

// A role change would corrupt the transcript, so it is rejected.
func TestMessageEndRejectsRoleChange(t *testing.T) {
	r := newRunner(t, ext("bad", func(a *API) {
		a.OnMessageEnd(func(context.Context, *MessageEndEvent, *Context) (*MessageEndResult, error) {
			return &MessageEndResult{Message: ai.AssistantMessage{StopReason: ai.StopStop}}, nil
		})
	}))
	if out := r.EmitMessageEnd(context.Background(), userMsg("original")); out != nil {
		t.Errorf("role-changing replacement must be ignored, got %#v", out)
	}
	if len(r.Errors()) != 1 || !strings.Contains(r.Errors()[0].Err.Error(), "must keep role") {
		t.Errorf("expected a role-mismatch error, got %v", r.Errors())
	}
}

func TestMessageEndUnmodifiedReturnsNil(t *testing.T) {
	r := newRunner(t, ext("noop", func(a *API) {
		a.OnMessageEnd(func(context.Context, *MessageEndEvent, *Context) (*MessageEndResult, error) {
			return nil, nil
		})
	}))
	if out := r.EmitMessageEnd(context.Background(), userMsg("x")); out != nil {
		t.Errorf("unmodified should be nil, got %#v", out)
	}
}

// --- before_agent_start: messages accumulate, system prompt chains ---

func TestBeforeAgentStartAccumulatesMessagesAndChainsPrompt(t *testing.T) {
	r := newRunner(t,
		ext("a", func(a *API) {
			a.OnBeforeAgentStart(func(_ context.Context, ev *BeforeAgentStartEvent, _ *Context) (*BeforeAgentStartResult, error) {
				sp := ev.SystemPrompt + "\n[a]"
				return &BeforeAgentStartResult{Message: userMsg("from a"), SystemPrompt: &sp}, nil
			})
		}),
		ext("b", func(a *API) {
			a.OnBeforeAgentStart(func(_ context.Context, ev *BeforeAgentStartEvent, ec *Context) (*BeforeAgentStartResult, error) {
				// Both the event field and ctx.SystemPrompt must show a's edit.
				if !strings.HasSuffix(ev.SystemPrompt, "[a]") {
					t.Errorf("event prompt not chained: %q", ev.SystemPrompt)
				}
				if !strings.HasSuffix(ec.SystemPrompt(), "[a]") {
					t.Errorf("ctx.SystemPrompt not chained: %q", ec.SystemPrompt())
				}
				sp := ev.SystemPrompt + "\n[b]"
				return &BeforeAgentStartResult{Message: userMsg("from b"), SystemPrompt: &sp}, nil
			})
		}),
	)

	out := r.EmitBeforeAgentStart(context.Background(), "do it", nil, "base")
	if out == nil {
		t.Fatal("expected a combined result")
	}
	if len(out.Messages) != 2 {
		t.Errorf("messages should accumulate, got %d", len(out.Messages))
	}
	if out.SystemPrompt == nil || *out.SystemPrompt != "base\n[a]\n[b]" {
		t.Errorf("system prompt = %q, want the chained value", *out.SystemPrompt)
	}
}

func TestBeforeAgentStartNoContributionsReturnsNil(t *testing.T) {
	r := newRunner(t, ext("noop", func(a *API) {
		a.OnBeforeAgentStart(func(context.Context, *BeforeAgentStartEvent, *Context) (*BeforeAgentStartResult, error) {
			return nil, nil
		})
	}))
	if out := r.EmitBeforeAgentStart(context.Background(), "p", nil, "s"); out != nil {
		t.Errorf("expected nil, got %+v", out)
	}
}

// --- input: handled short-circuits, transform chains ---

func TestInputHandledShortCircuits(t *testing.T) {
	var laterRan bool
	r := newRunner(t,
		ext("consumer", func(a *API) {
			a.OnInput(func(context.Context, *InputEvent, *Context) (*InputResult, error) {
				return &InputResult{Action: InputHandled}, nil
			})
		}),
		ext("later", func(a *API) {
			a.OnInput(func(context.Context, *InputEvent, *Context) (*InputResult, error) {
				laterRan = true
				return nil, nil
			})
		}),
	)
	res := r.EmitInput(context.Background(), "hi", nil, "tui", "")
	if res.Action != InputHandled {
		t.Errorf("action = %v", res.Action)
	}
	if laterRan {
		t.Error("handled must short-circuit")
	}
}

func TestInputTransformChains(t *testing.T) {
	r := newRunner(t,
		ext("a", func(a *API) {
			a.OnInput(func(_ context.Context, ev *InputEvent, _ *Context) (*InputResult, error) {
				return &InputResult{Action: InputTransform, Text: ev.Text + " one"}, nil
			})
		}),
		ext("b", func(a *API) {
			a.OnInput(func(_ context.Context, ev *InputEvent, _ *Context) (*InputResult, error) {
				return &InputResult{Action: InputTransform, Text: ev.Text + " two"}, nil
			})
		}),
	)
	res := r.EmitInput(context.Background(), "start", nil, "tui", "")
	if res.Action != InputTransform || res.Text != "start one two" {
		t.Errorf("res = %+v, want chained transform", res)
	}
}

func TestInputUnchangedReturnsContinue(t *testing.T) {
	r := newRunner(t, ext("noop", func(a *API) {
		a.OnInput(func(context.Context, *InputEvent, *Context) (*InputResult, error) { return nil, nil })
	}))
	if res := r.EmitInput(context.Background(), "x", nil, "cli", ""); res.Action != InputContinue {
		t.Errorf("action = %v, want continue", res.Action)
	}
}

// --- user_bash: FIRST non-nil wins ---

func TestUserBashFirstResultWins(t *testing.T) {
	var secondRan bool
	r := newRunner(t,
		ext("first", func(a *API) {
			a.OnUserBash(func(context.Context, *UserBashEvent, *Context) (*UserBashResult, error) {
				return &UserBashResult{Output: "handled by first"}, nil
			})
		}),
		ext("second", func(a *API) {
			a.OnUserBash(func(context.Context, *UserBashEvent, *Context) (*UserBashResult, error) {
				secondRan = true
				return &UserBashResult{Output: "second"}, nil
			})
		}),
	)
	res := r.EmitUserBash(context.Background(), &UserBashEvent{Command: "ls"})
	if res == nil || res.Output != "handled by first" {
		t.Errorf("res = %+v", res)
	}
	if secondRan {
		t.Error("first non-nil result must short-circuit")
	}
}

// --- session_before_*: cancel short-circuits, else last wins ---

func TestSessionBeforeCancelShortCircuits(t *testing.T) {
	var laterRan bool
	r := newRunner(t,
		ext("blocker", func(a *API) {
			a.OnSessionBeforeCompact(func(context.Context, *SessionBeforeCompactEvent, *Context) (*SessionBeforeResult, error) {
				return &SessionBeforeResult{Cancel: true, Reason: "not now"}, nil
			})
		}),
		ext("later", func(a *API) {
			a.OnSessionBeforeCompact(func(context.Context, *SessionBeforeCompactEvent, *Context) (*SessionBeforeResult, error) {
				laterRan = true
				return nil, nil
			})
		}),
	)
	res := r.EmitSessionBeforeCompact(context.Background(), &SessionBeforeCompactEvent{})
	if res == nil || !res.Cancel {
		t.Fatalf("res = %+v", res)
	}
	if laterRan {
		t.Error("cancel must short-circuit")
	}
}

// --- project_trust: first decisive vote wins, undecided falls through ---

func TestProjectTrustUndecidedFallsThrough(t *testing.T) {
	r := newRunner(t,
		ext("abstain", func(a *API) {
			a.OnProjectTrust(func(context.Context, *ProjectTrustEvent, *Context) (*ProjectTrustResult, error) {
				return &ProjectTrustResult{Decision: TrustUndecided}, nil
			})
		}),
		ext("decider", func(a *API) {
			a.OnProjectTrust(func(context.Context, *ProjectTrustEvent, *Context) (*ProjectTrustResult, error) {
				return &ProjectTrustResult{Decision: TrustNo, Reason: "untrusted origin"}, nil
			})
		}),
	)
	res := r.EmitProjectTrust(context.Background(), &ProjectTrustEvent{Cwd: "/work"})
	if res == nil || res.Decision != TrustNo {
		t.Errorf("res = %+v, want the decisive vote", res)
	}
}

func TestProjectTrustNoVotesReturnsNil(t *testing.T) {
	r := newRunner(t)
	if res := r.EmitProjectTrust(context.Background(), &ProjectTrustEvent{}); res != nil {
		t.Errorf("res = %+v, want nil", res)
	}
}

// --- context: chained, and the caller's slice is not corrupted ---

func TestContextChainsAndCopiesInput(t *testing.T) {
	original := []ai.Message{userMsg("a"), userMsg("b")}
	r := newRunner(t, ext("trimmer", func(a *API) {
		a.OnContext(func(_ context.Context, ev *ContextEvent, _ *Context) (*ContextResult, error) {
			return &ContextResult{Messages: ev.Messages[:1]}, nil
		})
	}))
	out := r.EmitContext(context.Background(), original)
	if len(out) != 1 {
		t.Errorf("out = %d messages, want 1", len(out))
	}
	if len(original) != 2 {
		t.Error("the caller's slice must not be modified")
	}
}

// --- before_provider_headers: in-place mutation, return ignored ---

func TestBeforeProviderHeadersMutatesInPlace(t *testing.T) {
	r := newRunner(t, ext("hdr", func(a *API) {
		a.OnBeforeProviderHeaders(func(_ context.Context, ev *BeforeProviderHeadersEvent, _ *Context) error {
			v := "custom"
			ev.Headers["x-tau"] = &v
			ev.Headers["x-drop"] = nil // nil deletes
			return nil
		})
	}))
	headers := map[string]*string{}
	out := r.EmitBeforeProviderHeaders(context.Background(), headers)
	if out["x-tau"] == nil || *out["x-tau"] != "custom" {
		t.Errorf("headers = %v", out)
	}
	if v, ok := out["x-drop"]; !ok || v != nil {
		t.Error("nil value should be preserved as a delete marker")
	}
}

// --- resources_discover: concatenates, tagged by owner ---

func TestResourcesDiscoverConcatenates(t *testing.T) {
	r := newRunner(t,
		ext("one", func(a *API) {
			a.OnResourcesDiscover(func(context.Context, *ResourcesDiscoverEvent, *Context) (*ResourcesDiscoverResult, error) {
				return &ResourcesDiscoverResult{SkillPaths: []string{"/a/skills"}}, nil
			})
		}),
		ext("two", func(a *API) {
			a.OnResourcesDiscover(func(context.Context, *ResourcesDiscoverEvent, *Context) (*ResourcesDiscoverResult, error) {
				return &ResourcesDiscoverResult{SkillPaths: []string{"/b/skills"}, ThemePaths: []string{"/b/themes"}}, nil
			})
		}),
	)
	out := r.EmitResourcesDiscover(context.Background(), &ResourcesDiscoverEvent{Cwd: "/work"})
	if len(out.SkillPaths) != 2 || len(out.ThemePaths) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out.SkillPaths[0].Extension != "/ext/one" {
		t.Errorf("path not tagged with its owner: %+v", out.SkillPaths[0])
	}
}

// --- failures are contained everywhere except tool_call ---

func TestNotificationHandlerFailureIsContained(t *testing.T) {
	var secondRan bool
	r := newRunner(t,
		ext("broken", func(a *API) {
			a.OnSessionStart(func(context.Context, *SessionStartEvent, *Context) error {
				return errors.New("kaboom")
			})
		}),
		ext("healthy", func(a *API) {
			a.OnSessionStart(func(context.Context, *SessionStartEvent, *Context) error {
				secondRan = true
				return nil
			})
		}),
	)
	r.EmitSessionStart(context.Background(), &SessionStartEvent{Cwd: "/work"})
	if !secondRan {
		t.Error("one extension's failure must not stop the others")
	}
	if len(r.Errors()) != 1 {
		t.Errorf("errors = %v", r.Errors())
	}
}

func TestPanicInNotificationHandlerIsContained(t *testing.T) {
	r := newRunner(t, ext("panicky", func(a *API) {
		a.OnAgentStart(func(context.Context, *AgentStartEvent, *Context) error { panic("boom") })
	}))
	r.EmitAgentStart(context.Background()) // must not panic
	if len(r.Errors()) != 1 {
		t.Errorf("panic should be reported as an error, got %v", r.Errors())
	}
}

func TestFactoryFailureIsReportedAndSkipped(t *testing.T) {
	r := NewRunner(RunnerOptions{})
	err := r.Load(Extension{Name: "bad", Factory: func(*API) error { return errors.New("nope") }})
	if err == nil {
		t.Error("expected an error from a failing factory")
	}
	// A broken extension must not prevent others from loading.
	if err := r.Load(ext("good", func(a *API) {})); err != nil {
		t.Errorf("second load failed: %v", err)
	}
}

// --- registration ---

func TestToolNameOverrideReplacesEarlier(t *testing.T) {
	r := newRunner(t,
		ext("builtin", func(a *API) { a.RegisterTool(fakeTool{name: "bash", label: "original"}) }),
		ext("override", func(a *API) { a.RegisterTool(fakeTool{name: "bash", label: "sandboxed"}) }),
	)
	tools := r.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1 (override replaces)", len(tools))
	}
	if tools[0].Def().Label != "sandboxed" {
		t.Errorf("label = %q, want the override", tools[0].Def().Label)
	}
}

func TestDuplicateCommandNamesAreSuffixed(t *testing.T) {
	r := newRunner(t,
		ext("a", func(a *API) { a.RegisterCommand(Command{Name: "deploy"}) }),
		ext("b", func(a *API) { a.RegisterCommand(Command{Name: "deploy"}) }),
	)
	cmds := r.Commands()
	got := []string{cmds[0].Name, cmds[1].Name}
	want := []string{"deploy", "deploy:1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
}

func TestSubscribedReportsHandlerPresence(t *testing.T) {
	var api *API
	r := NewRunner(RunnerOptions{})
	_ = r.Load(Extension{Name: "x", Factory: func(a *API) error {
		api = a
		a.OnToolCall(func(context.Context, *ToolCallEvent, *Context) (*ToolCallResult, error) { return nil, nil })
		return nil
	}})
	if !api.Subscribed(EventToolCall) {
		t.Error("should report subscribed for a registered event")
	}
	if api.Subscribed(EventTurnStart) {
		t.Error("should not report subscribed for an unregistered event")
	}
}

// --- staleness guard ---

func TestContextGoesStaleAfterInvalidate(t *testing.T) {
	var captured *Context
	r := newRunner(t, ext("capturer", func(a *API) {
		a.OnSessionStart(func(_ context.Context, _ *SessionStartEvent, ec *Context) error {
			captured = ec
			return nil
		})
	}))
	r.EmitSessionStart(context.Background(), &SessionStartEvent{Cwd: "/work"})
	if captured == nil {
		t.Fatal("handler did not run")
	}
	if err := captured.Err(); err != nil {
		t.Fatalf("context should be fresh: %v", err)
	}
	if captured.Cwd() != "/work" {
		t.Errorf("cwd = %q", captured.Cwd())
	}

	r.Invalidate() // session replaced

	if err := captured.Err(); !errors.Is(err, ErrStale) {
		t.Errorf("captured context should be stale, got %v", err)
	}
	if captured.Cwd() != "" {
		t.Error("a stale context must not report live state")
	}
}

func TestActionsBeforeBindReturnNotBound(t *testing.T) {
	var api *API
	r := NewRunner(RunnerOptions{})
	_ = r.Load(Extension{Name: "x", Factory: func(a *API) error { api = a; return nil }})
	if err := api.SetSessionName("nope"); !errors.Is(err, ErrNotBound) {
		t.Errorf("err = %v, want ErrNotBound", err)
	}
	_ = r
}

// --- ordering ---

func TestDispatchFollowsLoadAndRegistrationOrder(t *testing.T) {
	var order []string
	r := newRunner(t,
		ext("first", func(a *API) {
			a.OnTurnStart(func(context.Context, *TurnStartEvent, *Context) error {
				order = append(order, "first.1")
				return nil
			})
			a.OnTurnStart(func(context.Context, *TurnStartEvent, *Context) error {
				order = append(order, "first.2")
				return nil
			})
		}),
		ext("second", func(a *API) {
			a.OnTurnStart(func(context.Context, *TurnStartEvent, *Context) error {
				order = append(order, "second.1")
				return nil
			})
		}),
	)
	r.EmitTurnStart(context.Background())
	want := []string{"first.1", "first.2", "second.1"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestAllEventTypesCovered(t *testing.T) {
	if len(AllEventTypes) != 33 {
		t.Errorf("AllEventTypes has %d entries, want 33 (Pi's hook count)", len(AllEventTypes))
	}
	seen := map[EventType]bool{}
	for _, e := range AllEventTypes {
		if seen[e] {
			t.Errorf("duplicate event type %q", e)
		}
		seen[e] = true
	}
}
