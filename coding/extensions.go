package coding

import (
	"context"
	"encoding/json"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension"
)

// wireExtensions connects an extension Runner to the agent loop's hooks and
// event stream. This is the seam that lets an extension gate a tool call,
// rewrite context, or observe the run.
func wireExtensions(cfg *agent.LoopConfig, r *extension.Runner) {
	if r == nil {
		return
	}

	// tool_call: extensions may block or patch arguments. A blocked call
	// becomes an error result the model can read and react to.
	userBefore := cfg.BeforeToolCall
	cfg.BeforeToolCall = func(ctx context.Context, c agent.ToolCallContext) (*agent.BeforeToolCallResult, error) {
		if userBefore != nil {
			res, err := userBefore(ctx, c)
			if err != nil || (res != nil && res.Block) {
				return res, err
			}
		}

		args := map[string]any{}
		if len(c.Args) > 0 {
			_ = json.Unmarshal(c.Args, &args)
		}
		ev := &extension.ToolCallEvent{
			ToolCallID: c.ToolCall.ID,
			ToolName:   c.ToolCall.Name,
			Args:       args,
			Raw:        c.Args,
		}
		res := r.EmitToolCall(ctx, ev)
		if res != nil && res.Block {
			reason := res.Reason
			if reason == "" {
				reason = "blocked by an extension"
			}
			return &agent.BeforeToolCallResult{Block: true, Reason: reason}, nil
		}
		return nil, nil
	}

	// tool_result: extensions may patch a finished result before it is
	// recorded and sent to the model.
	userAfter := cfg.AfterToolCall
	cfg.AfterToolCall = func(ctx context.Context, c agent.ToolResultContext) (*agent.AfterToolCallResult, error) {
		var patch *agent.AfterToolCallResult
		if userAfter != nil {
			p, err := userAfter(ctx, c)
			if err != nil {
				return nil, err
			}
			patch = p
		}

		ev := &extension.ToolResultEvent{
			ToolCallID: c.ToolCall.ID,
			ToolName:   c.ToolCall.Name,
			Content:    c.Result.Content,
			Details:    c.Result.Details,
			IsError:    c.IsError,
			Usage:      c.Result.Usage,
		}
		res := r.EmitToolResult(ctx, ev)
		if res == nil {
			return patch, nil
		}
		if patch == nil {
			patch = &agent.AfterToolCallResult{}
		}
		if res.Content != nil {
			patch.Content = res.Content
		}
		if res.Details != nil {
			patch.Details = res.Details
		}
		if res.Usage != nil {
			patch.Usage = res.Usage
		}
		if res.IsError != nil {
			patch.IsError = res.IsError
		}
		return patch, nil
	}

	// context: extensions may rewrite the transcript before it is converted
	// for the provider.
	userTransform := cfg.TransformContext
	cfg.TransformContext = func(ctx context.Context, msgs []ai.Message) ([]ai.Message, error) {
		if userTransform != nil {
			out, err := userTransform(ctx, msgs)
			if err != nil {
				return nil, err
			}
			msgs = out
		}
		return r.EmitContext(ctx, msgs), nil
	}
}

// extensionSink forwards agent-loop events to extensions as notifications.
func extensionSink(r *extension.Runner) agent.Sink {
	if r == nil {
		return nil
	}
	return func(ctx context.Context, ev agent.Event) error {
		switch ev.Type {
		case agent.EventAgentStart:
			r.EmitAgentStart(ctx)
		case agent.EventAgentEnd:
			r.EmitAgentEnd(ctx, &extension.AgentEndEvent{Messages: ev.Messages})
			// agent_settled follows once the loop has fully unwound.
			r.EmitAgentSettled(ctx)
		case agent.EventTurnStart:
			r.EmitTurnStart(ctx)
		case agent.EventTurnEnd:
			r.EmitTurnEnd(ctx, &extension.TurnEndEvent{Message: ev.Message, ToolResults: ev.ToolResults})
		case agent.EventMessageStart:
			r.EmitMessageStart(ctx, &extension.MessageStartEvent{Message: ev.Message})
		case agent.EventMessageUpdate:
			r.EmitMessageUpdate(ctx, &extension.MessageUpdateEvent{
				Message: ev.Message, StreamEvent: ev.StreamEvent,
			})
		case agent.EventToolExecutionStart:
			r.EmitToolExecutionStart(ctx, &extension.ToolExecutionStartEvent{
				ToolCallID: ev.ToolCallID, ToolName: ev.ToolName, Args: ev.Args,
			})
		case agent.EventToolExecutionUpdate:
			r.EmitToolExecutionUpdate(ctx, &extension.ToolExecutionUpdateEvent{
				ToolCallID: ev.ToolCallID, ToolName: ev.ToolName,
				Args: ev.Args, PartialResult: ev.PartialResult,
			})
		case agent.EventToolExecutionEnd:
			r.EmitToolExecutionEnd(ctx, &extension.ToolExecutionEndEvent{
				ToolCallID: ev.ToolCallID, ToolName: ev.ToolName,
				Result: ev.Result, IsError: ev.IsError,
			})
		}
		return nil
	}
}

// message_end replacement is applied by the orchestrator rather than the sink,
// because a sink cannot alter the message the loop recorded. runtimeAdapter
// exposes the coding session to extensions.
type runtimeAdapter struct{ s *Session }

func (a runtimeAdapter) SendMessage(msg ai.Message, deliverAs string) error {
	switch deliverAs {
	case "steer":
		a.s.Agent.Steer(msg)
	default:
		a.s.Agent.FollowUp(msg)
	}
	return nil
}

func (a runtimeAdapter) SetSessionName(name string) error {
	if a.s.Session == nil {
		return nil
	}
	_, err := a.s.Session.AppendName(context.Background(), name)
	return err
}

func (a runtimeAdapter) SessionName() string {
	if a.s.Session == nil {
		return ""
	}
	name, _ := a.s.Session.Name(context.Background())
	return name
}

func (a runtimeAdapter) Exec(ctx context.Context, command string) (string, int, error) {
	res, err := a.s.Env.Exec(ctx, command, envExecOptions())
	if err != nil {
		return "", 0, err
	}
	return res.Output, res.ExitCode, nil
}

func (a runtimeAdapter) ActiveToolNames() []string { return a.s.ToolNames() }

func (a runtimeAdapter) SetActiveTools(names []string) error {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	a.s.mu.Lock()
	all := append([]agent.Tool{}, a.s.allTools...)
	a.s.mu.Unlock()

	var keep []agent.Tool
	for _, t := range all {
		if want[t.Def().Name] {
			keep = append(keep, t)
		}
	}
	a.s.Agent.SetTools(keep)
	return nil
}

// RegisterTools adds tools to the live set. A tool registered after startup —
// an MCP server announcing tools/list_changed, say — has to reach both the
// registry and the agent's active set, or it exists but is never offered.
func (a runtimeAdapter) RegisterTools(ts []agent.Tool) error {
	if len(ts) == 0 {
		return nil
	}

	a.s.mu.Lock()
	for _, t := range ts {
		name := t.Def().Name
		replaced := false
		for i, existing := range a.s.allTools {
			if existing.Def().Name == name {
				a.s.allTools[i] = t
				replaced = true
				break
			}
		}
		if !replaced {
			a.s.allTools = append(a.s.allTools, t)
		}
	}
	a.s.mu.Unlock()

	// Mirror the change into the active set, preserving order and honoring
	// any deactivation an extension has already made.
	active := a.s.Agent.Tools()
	for _, t := range ts {
		name := t.Def().Name
		replaced := false
		for i, existing := range active {
			if existing.Def().Name == name {
				active[i] = t
				replaced = true
				break
			}
		}
		if !replaced {
			active = append(active, t)
		}
	}
	a.s.Agent.SetTools(active)
	return nil
}

func (a runtimeAdapter) Model() *ai.Model { return a.s.Agent.Model() }

func (a runtimeAdapter) SetModel(m *ai.Model) error {
	a.s.Agent.SetModel(m)
	return nil
}

func (a runtimeAdapter) ThinkingLevel() ai.ModelThinkingLevel { return a.s.Agent.ThinkingLevel() }

func (a runtimeAdapter) SetThinkingLevel(l ai.ModelThinkingLevel) error {
	a.s.Agent.SetThinkingLevel(l)
	return nil
}
