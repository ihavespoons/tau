package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ihavespoons/tau/ai"
)

// LoopConfig configures one agent run. The hook fields mirror Pi's
// AgentLoopConfig; every hook is optional.
//
// Hook contract: hooks must not panic. A hook returning an error aborts the
// run — for recoverable conditions, return a safe fallback value instead.
type LoopConfig struct {
	Model        *ai.Model
	Stream       ai.SimpleStreamFunc
	Reasoning    ai.ThinkingLevel
	StreamOpts   ai.StreamOptions
	ToolExecMode ExecutionMode

	// ConvertToLLM projects agent messages down to provider messages,
	// dropping any that the model must not see. Nil means pass-through.
	ConvertToLLM func(messages []ai.Message) ([]ai.Message, error)
	// TransformContext runs before ConvertToLLM, at the agent-message level
	// (context pruning, injection).
	TransformContext func(ctx context.Context, messages []ai.Message) ([]ai.Message, error)

	// GetSteeringMessages is polled at turn boundaries; returned messages are
	// injected before the next provider call. Tool calls already in flight
	// still complete.
	GetSteeringMessages func(ctx context.Context) ([]ai.Message, error)
	// GetFollowUpMessages is polled only when the agent would otherwise stop.
	GetFollowUpMessages func(ctx context.Context) ([]ai.Message, error)

	// ShouldStopAfterTurn requests a graceful stop after the current turn.
	ShouldStopAfterTurn func(ctx context.Context, t TurnContext) (bool, error)
	// PrepareNextTurn may swap context, model, or thinking level between turns.
	PrepareNextTurn func(ctx context.Context, t TurnContext) (*TurnUpdate, error)

	// BeforeToolCall runs after argument validation; Block stops execution.
	BeforeToolCall func(ctx context.Context, c ToolCallContext) (*BeforeToolCallResult, error)
	// AfterToolCall may patch a finished tool result.
	AfterToolCall func(ctx context.Context, c ToolResultContext) (*AfterToolCallResult, error)
}

// TurnContext describes a completed turn.
type TurnContext struct {
	Message     *ai.AssistantMessage
	ToolResults []ai.ToolResultMessage
	Context     *RunContext
	NewMessages []ai.Message
}

// TurnUpdate replaces per-turn state for the next turn.
type TurnUpdate struct {
	Context       *RunContext
	Model         *ai.Model
	ThinkingLevel *ai.ModelThinkingLevel
}

// ToolCallContext is passed to BeforeToolCall.
type ToolCallContext struct {
	AssistantMessage *ai.AssistantMessage
	ToolCall         ai.ToolCall
	Args             json.RawMessage
	Context          *RunContext
}

// ToolResultContext is passed to AfterToolCall.
type ToolResultContext struct {
	AssistantMessage *ai.AssistantMessage
	ToolCall         ai.ToolCall
	Args             json.RawMessage
	Result           ToolResult
	IsError          bool
	Context          *RunContext
}

// BeforeToolCallResult can block a tool call.
type BeforeToolCallResult struct {
	Block  bool
	Reason string
}

// AfterToolCallResult patches a tool result. Nil fields keep their original
// values — there is no deep merge.
type AfterToolCallResult struct {
	Content   ai.ContentList
	Details   any
	Usage     *ai.Usage
	Terminate *bool
	IsError   *bool
}

// RunContext is the mutable conversation state for a run.
type RunContext struct {
	SystemPrompt string
	Messages     []ai.Message
	Tools        []Tool
}

// Clone returns a shallow copy with its own message slice.
func (c *RunContext) Clone() *RunContext {
	if c == nil {
		return nil
	}
	msgs := make([]ai.Message, len(c.Messages))
	copy(msgs, c.Messages)
	return &RunContext{SystemPrompt: c.SystemPrompt, Messages: msgs, Tools: c.Tools}
}

// RunLoop executes the agent loop: prompts are appended to the context, then
// turns run until there are no tool calls, no steering, and no follow-up
// messages. It returns the messages produced by the run.
//
// Like Pi's loop, provider failures are not errors here — they arrive as an
// assistant message with a terminal stop reason and end the run cleanly. An
// error return means the harness itself failed (a hook or sink errored).
func RunLoop(ctx context.Context, prompts []ai.Message, rc *RunContext, cfg LoopConfig, sinks ...Sink) ([]ai.Message, error) {
	newMessages := append([]ai.Message{}, prompts...)
	cur := rc.Clone()
	cur.Messages = append(cur.Messages, prompts...)

	emit := func(ev Event) error { return emitTo(ctx, sinks, ev) }

	if err := emit(Event{Type: EventAgentStart}); err != nil {
		return newMessages, err
	}
	if err := emit(Event{Type: EventTurnStart}); err != nil {
		return newMessages, err
	}
	for _, p := range prompts {
		if err := emit(Event{Type: EventMessageStart, Message: p}); err != nil {
			return newMessages, err
		}
		if err := emit(Event{Type: EventMessageEnd, Message: p}); err != nil {
			return newMessages, err
		}
	}

	return runLoop(ctx, cur, newMessages, cfg, emit, true)
}

// RunLoopContinue resumes from the existing context without adding a prompt
// (used for retries, where the transcript already ends in a user or tool
// result message).
func RunLoopContinue(ctx context.Context, rc *RunContext, cfg LoopConfig, sinks ...Sink) ([]ai.Message, error) {
	if len(rc.Messages) == 0 {
		return nil, errors.New("agent: cannot continue with no messages")
	}
	if _, isAssistant := rc.Messages[len(rc.Messages)-1].(ai.AssistantMessage); isAssistant {
		return nil, errors.New("agent: cannot continue from an assistant message")
	}

	emit := func(ev Event) error { return emitTo(ctx, sinks, ev) }
	if err := emit(Event{Type: EventAgentStart}); err != nil {
		return nil, err
	}
	if err := emit(Event{Type: EventTurnStart}); err != nil {
		return nil, err
	}
	return runLoop(ctx, rc.Clone(), nil, cfg, emit, true)
}

type emitFunc func(Event) error

func runLoop(ctx context.Context, cur *RunContext, newMessages []ai.Message, cfg LoopConfig, emit emitFunc, firstTurn bool) ([]ai.Message, error) {
	// Poll steering before the first turn: the user may have typed while the
	// previous run was settling.
	pending, err := poll(ctx, cfg.GetSteeringMessages)
	if err != nil {
		return newMessages, err
	}

	for { // outer: resumes when follow-up messages arrive after a would-be stop
		hasMoreToolCalls := true

		for hasMoreToolCalls || len(pending) > 0 {
			if !firstTurn {
				if err := emit(Event{Type: EventTurnStart}); err != nil {
					return newMessages, err
				}
			}
			firstTurn = false

			for _, m := range pending {
				if err := emit(Event{Type: EventMessageStart, Message: m}); err != nil {
					return newMessages, err
				}
				if err := emit(Event{Type: EventMessageEnd, Message: m}); err != nil {
					return newMessages, err
				}
				cur.Messages = append(cur.Messages, m)
				newMessages = append(newMessages, m)
			}
			// pending is not cleared here: every path below either returns or
			// refreshes it from the steering poll at the bottom of the loop.

			msg, err := streamAssistant(ctx, cur, cfg, emit)
			if err != nil {
				return newMessages, err
			}
			newMessages = append(newMessages, *msg)

			// A terminal provider failure ends the run without an error: the
			// stop reason on the message carries the diagnosis.
			if msg.StopReason == ai.StopError || msg.StopReason == ai.StopAborted {
				if err := emit(Event{Type: EventTurnEnd, Message: *msg}); err != nil {
					return newMessages, err
				}
				return newMessages, emit(Event{Type: EventAgentEnd, Messages: newMessages})
			}

			toolCalls := toolCallsOf(msg)
			var toolResults []ai.ToolResultMessage
			hasMoreToolCalls = false

			if len(toolCalls) > 0 {
				var batch executedBatch
				if msg.StopReason == ai.StopLength {
					// The message was cut off by the token limit, so every
					// tool call may carry silently truncated arguments —
					// none are safe to run.
					batch, err = failTruncatedToolCalls(toolCalls, emit)
				} else {
					batch, err = executeToolCalls(ctx, cur, msg, toolCalls, cfg, emit)
				}
				if err != nil {
					return newMessages, err
				}
				toolResults = batch.messages
				hasMoreToolCalls = !batch.terminate
				for _, r := range toolResults {
					cur.Messages = append(cur.Messages, r)
					newMessages = append(newMessages, r)
				}
			}

			if err := emit(Event{Type: EventTurnEnd, Message: *msg, ToolResults: toolResults}); err != nil {
				return newMessages, err
			}

			turn := TurnContext{Message: msg, ToolResults: toolResults, Context: cur, NewMessages: newMessages}

			if cfg.PrepareNextTurn != nil {
				upd, err := cfg.PrepareNextTurn(ctx, turn)
				if err != nil {
					return newMessages, err
				}
				if upd != nil {
					if upd.Context != nil {
						cur = upd.Context
					}
					if upd.Model != nil {
						cfg.Model = upd.Model
					}
					if upd.ThinkingLevel != nil {
						if *upd.ThinkingLevel == ai.ThinkingOff {
							cfg.Reasoning = ""
						} else {
							cfg.Reasoning = ai.ThinkingLevel(*upd.ThinkingLevel)
						}
					}
				}
			}

			if cfg.ShouldStopAfterTurn != nil {
				stop, err := cfg.ShouldStopAfterTurn(ctx, turn)
				if err != nil {
					return newMessages, err
				}
				if stop {
					return newMessages, emit(Event{Type: EventAgentEnd, Messages: newMessages})
				}
			}

			pending, err = poll(ctx, cfg.GetSteeringMessages)
			if err != nil {
				return newMessages, err
			}
		}

		followUps, err := poll(ctx, cfg.GetFollowUpMessages)
		if err != nil {
			return newMessages, err
		}
		if len(followUps) > 0 {
			pending = followUps
			continue
		}
		break
	}

	return newMessages, emit(Event{Type: EventAgentEnd, Messages: newMessages})
}

func poll(ctx context.Context, fn func(context.Context) ([]ai.Message, error)) ([]ai.Message, error) {
	if fn == nil {
		return nil, nil
	}
	return fn(ctx)
}

func toolCallsOf(msg *ai.AssistantMessage) []ai.ToolCall {
	var out []ai.ToolCall
	for _, c := range msg.Content {
		if tc, ok := c.(ai.ToolCall); ok {
			out = append(out, tc)
		}
	}
	return out
}

// streamAssistant runs one provider call, mutating the live transcript entry
// as deltas arrive so subscribers always see the partial message in place.
func streamAssistant(ctx context.Context, cur *RunContext, cfg LoopConfig, emit emitFunc) (*ai.AssistantMessage, error) {
	messages := cur.Messages
	if cfg.TransformContext != nil {
		transformed, err := cfg.TransformContext(ctx, messages)
		if err != nil {
			return nil, err
		}
		messages = transformed
	}

	llmMessages := messages
	if cfg.ConvertToLLM != nil {
		converted, err := cfg.ConvertToLLM(messages)
		if err != nil {
			return nil, err
		}
		llmMessages = converted
	}

	llmCtx := ai.Context{
		SystemPrompt: cur.SystemPrompt,
		Messages:     ai.MessageList(llmMessages),
		Tools:        AITools(cur.Tools),
	}

	opts := &ai.SimpleStreamOptions{StreamOptions: cfg.StreamOpts, Reasoning: cfg.Reasoning}
	stream := cfg.Stream(ctx, cfg.Model, llmCtx, opts)

	added := false
	for ev := range stream.Events() {
		switch ev.Type {
		case ai.EventStart:
			cur.Messages = append(cur.Messages, *ev.Partial)
			added = true
			if err := emit(Event{Type: EventMessageStart, Message: *ev.Partial}); err != nil {
				return nil, err
			}
		case ai.EventDone, ai.EventError:
			// Fall through to the terminal handling below.
		default:
			if added && ev.Partial != nil {
				cur.Messages[len(cur.Messages)-1] = *ev.Partial
				streamEvent := ev
				if err := emit(Event{
					Type:        EventMessageUpdate,
					Message:     *ev.Partial,
					StreamEvent: &streamEvent,
				}); err != nil {
					return nil, err
				}
			}
		}
	}

	final := stream.Result()
	if final == nil {
		final = &ai.AssistantMessage{
			Content: ai.ContentList{}, Api: cfg.Model.Api, Provider: cfg.Model.Provider,
			Model: cfg.Model.ID, StopReason: ai.StopError,
			ErrorMessage: "provider stream ended without a terminal event",
			Timestamp:    time.Now().UnixMilli(),
		}
	}
	if added {
		cur.Messages[len(cur.Messages)-1] = *final
	} else {
		cur.Messages = append(cur.Messages, *final)
		if err := emit(Event{Type: EventMessageStart, Message: *final}); err != nil {
			return nil, err
		}
	}
	if err := emit(Event{Type: EventMessageEnd, Message: *final}); err != nil {
		return nil, err
	}
	return final, nil
}

type executedBatch struct {
	messages  []ai.ToolResultMessage
	terminate bool
}

type finalized struct {
	toolCall ai.ToolCall
	result   ToolResult
	isError  bool
}

// failTruncatedToolCalls reports every tool call in a token-limited message as
// an error so the model re-issues them with complete arguments.
func failTruncatedToolCalls(toolCalls []ai.ToolCall, emit emitFunc) (executedBatch, error) {
	var msgs []ai.ToolResultMessage
	for _, tc := range toolCalls {
		if err := emit(Event{
			Type: EventToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments,
		}); err != nil {
			return executedBatch{}, err
		}
		f := finalized{
			toolCall: tc,
			result: errorResult(fmt.Sprintf(
				"Tool call %q was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.",
				tc.Name)),
			isError: true,
		}
		if err := emitToolEnd(f, emit); err != nil {
			return executedBatch{}, err
		}
		m := toolResultMessage(f)
		if err := emitToolResultMessage(m, emit); err != nil {
			return executedBatch{}, err
		}
		msgs = append(msgs, m)
	}
	return executedBatch{messages: msgs}, nil
}

func executeToolCalls(ctx context.Context, cur *RunContext, msg *ai.AssistantMessage, toolCalls []ai.ToolCall, cfg LoopConfig, emit emitFunc) (executedBatch, error) {
	sequential := cfg.ToolExecMode == ExecutionSequential
	if !sequential {
		for _, tc := range toolCalls {
			if t := FindTool(cur.Tools, tc.Name); t != nil && t.Def().ExecutionMode == ExecutionSequential {
				sequential = true
				break
			}
		}
	}
	if sequential {
		return executeSequential(ctx, cur, msg, toolCalls, cfg, emit)
	}
	return executeParallel(ctx, cur, msg, toolCalls, cfg, emit)
}

func executeSequential(ctx context.Context, cur *RunContext, msg *ai.AssistantMessage, toolCalls []ai.ToolCall, cfg LoopConfig, emit emitFunc) (executedBatch, error) {
	var all []finalized
	var msgs []ai.ToolResultMessage

	for _, tc := range toolCalls {
		if err := emit(Event{
			Type: EventToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments,
		}); err != nil {
			return executedBatch{}, err
		}

		prep, immediate := prepareToolCall(ctx, cur, msg, tc, cfg)
		var f finalized
		if immediate != nil {
			f = *immediate
		} else {
			res, isErr := executePrepared(ctx, prep, emit)
			f = finalizeToolCall(ctx, cur, msg, prep, res, isErr, cfg)
		}

		if err := emitToolEnd(f, emit); err != nil {
			return executedBatch{}, err
		}
		m := toolResultMessage(f)
		if err := emitToolResultMessage(m, emit); err != nil {
			return executedBatch{}, err
		}
		all = append(all, f)
		msgs = append(msgs, m)

		if ctx.Err() != nil {
			break
		}
	}
	return executedBatch{messages: msgs, terminate: shouldTerminate(all)}, nil
}

// executeParallel prepares calls sequentially (so hooks observe source order),
// runs them concurrently, then emits results in assistant source order.
func executeParallel(ctx context.Context, cur *RunContext, msg *ai.AssistantMessage, toolCalls []ai.ToolCall, cfg LoopConfig, emit emitFunc) (executedBatch, error) {
	type slot struct {
		f     finalized
		ready bool
		run   func() finalized
	}
	slots := make([]*slot, 0, len(toolCalls))

	// Serialize update events from concurrent tools: emit is not safe for
	// concurrent use by design (sinks see a single ordered stream).
	var emitMu sync.Mutex
	safeEmit := func(ev Event) error {
		emitMu.Lock()
		defer emitMu.Unlock()
		return emit(ev)
	}

	for _, tc := range toolCalls {
		if err := emit(Event{
			Type: EventToolExecutionStart, ToolCallID: tc.ID, ToolName: tc.Name, Args: tc.Arguments,
		}); err != nil {
			return executedBatch{}, err
		}
		prep, immediate := prepareToolCall(ctx, cur, msg, tc, cfg)
		if immediate != nil {
			slots = append(slots, &slot{f: *immediate, ready: true})
		} else {
			p := prep
			slots = append(slots, &slot{run: func() finalized {
				res, isErr := executePrepared(ctx, p, safeEmit)
				return finalizeToolCall(ctx, cur, msg, p, res, isErr, cfg)
			}})
		}
		if ctx.Err() != nil {
			break
		}
	}

	var wg sync.WaitGroup
	for _, s := range slots {
		if s.ready {
			continue
		}
		wg.Add(1)
		go func(s *slot) {
			defer wg.Done()
			s.f = s.run()
		}(s)
	}
	wg.Wait()

	var all []finalized
	var msgs []ai.ToolResultMessage
	for _, s := range slots {
		if err := emitToolEnd(s.f, emit); err != nil {
			return executedBatch{}, err
		}
		all = append(all, s.f)
	}
	for _, f := range all {
		m := toolResultMessage(f)
		if err := emitToolResultMessage(m, emit); err != nil {
			return executedBatch{}, err
		}
		msgs = append(msgs, m)
	}
	return executedBatch{messages: msgs, terminate: shouldTerminate(all)}, nil
}

type prepared struct {
	toolCall ai.ToolCall
	tool     Tool
	args     json.RawMessage
}

// prepareToolCall resolves and validates a call. It returns either a prepared
// call or an immediate (failed/blocked) outcome — never both.
func prepareToolCall(ctx context.Context, cur *RunContext, msg *ai.AssistantMessage, tc ai.ToolCall, cfg LoopConfig) (prepared, *finalized) {
	tool := FindTool(cur.Tools, tc.Name)
	if tool == nil {
		return prepared{}, &finalized{
			toolCall: tc, result: errorResult(fmt.Sprintf("Tool %s not found", tc.Name)), isError: true,
		}
	}

	args, err := json.Marshal(tc.Arguments)
	if err != nil {
		return prepared{}, &finalized{
			toolCall: tc, result: errorResult(err.Error()), isError: true,
		}
	}
	if pa, ok := tool.(PrepareArguments); ok {
		prepArgs, err := pa.PrepareArguments(args)
		if err != nil {
			return prepared{}, &finalized{toolCall: tc, result: errorResult(err.Error()), isError: true}
		}
		args = prepArgs
	}
	if err := Validate(tool.Def().Parameters, args); err != nil {
		return prepared{}, &finalized{toolCall: tc, result: errorResult(err.Error()), isError: true}
	}

	if cfg.BeforeToolCall != nil {
		res, err := cfg.BeforeToolCall(ctx, ToolCallContext{
			AssistantMessage: msg, ToolCall: tc, Args: args, Context: cur,
		})
		if err != nil {
			return prepared{}, &finalized{toolCall: tc, result: errorResult(err.Error()), isError: true}
		}
		if ctx.Err() != nil {
			return prepared{}, &finalized{toolCall: tc, result: errorResult("Operation aborted"), isError: true}
		}
		if res != nil && res.Block {
			reason := res.Reason
			if reason == "" {
				reason = "Tool execution was blocked"
			}
			return prepared{}, &finalized{toolCall: tc, result: errorResult(reason), isError: true}
		}
	}
	if ctx.Err() != nil {
		return prepared{}, &finalized{toolCall: tc, result: errorResult("Operation aborted"), isError: true}
	}

	return prepared{toolCall: tc, tool: tool, args: args}, nil
}

// executePrepared runs a tool, converting a returned error into an error
// result (Pi's throw-to-fail contract) and recovering panics so one bad tool
// cannot take down the agent.
func executePrepared(ctx context.Context, p prepared, emit emitFunc) (res ToolResult, isError bool) {
	var updateMu sync.Mutex
	accepting := true
	update := func(partial ToolResult) {
		updateMu.Lock()
		defer updateMu.Unlock()
		if !accepting {
			return
		}
		pr := partial
		_ = emit(Event{
			Type: EventToolExecutionUpdate, ToolCallID: p.toolCall.ID, ToolName: p.toolCall.Name,
			Args: p.toolCall.Arguments, PartialResult: &pr,
		})
	}
	stopUpdates := func() {
		updateMu.Lock()
		accepting = false
		updateMu.Unlock()
	}

	defer func() {
		if r := recover(); r != nil {
			stopUpdates()
			res = errorResult(fmt.Sprintf("tool %s panicked: %v", p.toolCall.Name, r))
			isError = true
		}
	}()

	out, err := p.tool.Execute(ctx, p.toolCall.ID, p.args, update)
	stopUpdates()
	if err != nil {
		return errorResult(err.Error()), true
	}
	return out, false
}

// finalizeToolCall applies the AfterToolCall hook. Nil fields on the hook
// result keep their original values; there is no deep merge.
func finalizeToolCall(ctx context.Context, cur *RunContext, msg *ai.AssistantMessage, p prepared, res ToolResult, isError bool, cfg LoopConfig) finalized {
	if cfg.AfterToolCall == nil {
		return finalized{toolCall: p.toolCall, result: res, isError: isError}
	}
	patch, err := cfg.AfterToolCall(ctx, ToolResultContext{
		AssistantMessage: msg, ToolCall: p.toolCall, Args: p.args,
		Result: res, IsError: isError, Context: cur,
	})
	if err != nil {
		return finalized{toolCall: p.toolCall, result: errorResult(err.Error()), isError: true}
	}
	if patch != nil {
		if patch.Content != nil {
			res.Content = patch.Content
		}
		if patch.Details != nil {
			res.Details = patch.Details
		}
		if patch.Usage != nil {
			res.Usage = patch.Usage
		}
		if patch.Terminate != nil {
			res.Terminate = *patch.Terminate
		}
		if patch.IsError != nil {
			isError = *patch.IsError
		}
	}
	return finalized{toolCall: p.toolCall, result: res, isError: isError}
}

func shouldTerminate(all []finalized) bool {
	if len(all) == 0 {
		return false
	}
	for _, f := range all {
		if !f.result.Terminate {
			return false
		}
	}
	return true
}

func errorResult(message string) ToolResult {
	return ToolResult{Content: ai.ContentList{ai.TextContent{Text: message}}}
}

func emitToolEnd(f finalized, emit emitFunc) error {
	r := f.result
	return emit(Event{
		Type: EventToolExecutionEnd, ToolCallID: f.toolCall.ID, ToolName: f.toolCall.Name,
		Result: &r, IsError: f.isError,
	})
}

func toolResultMessage(f finalized) ai.ToolResultMessage {
	content := f.result.Content
	if content == nil {
		content = ai.ContentList{}
	}
	return ai.ToolResultMessage{
		ToolCallID: f.toolCall.ID, ToolName: f.toolCall.Name,
		Content: content, Details: f.result.Details, Usage: f.result.Usage,
		AddedToolNames: f.result.AddedToolNames, IsError: f.isError,
		Timestamp: time.Now().UnixMilli(),
	}
}

func emitToolResultMessage(m ai.ToolResultMessage, emit emitFunc) error {
	if err := emit(Event{Type: EventMessageStart, Message: m}); err != nil {
		return err
	}
	return emit(Event{Type: EventMessageEnd, Message: m})
}
