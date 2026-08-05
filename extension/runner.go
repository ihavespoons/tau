package extension

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
)

// Runner loads extensions and dispatches events to them.
//
// Dispatch is synchronous and ordered: extensions in load order, handlers in
// registration order within each extension. This mirrors Pi's awaited
// handlers, and means a slow handler backpressures the agent.
//
// Handler failures never abort the agent. They are collected as *Error and
// reported to the error listener. The single deliberate exception is
// tool_call, where a failing handler blocks the tool — failing closed is the
// safe direction for a permission gate.
type Runner struct {
	mu sync.Mutex

	apis []*API

	mode         Mode
	cwd          string
	trusted      bool
	systemPrompt string
	runtime      Runtime
	ui           UI

	// generation invalidates contexts when the session is replaced.
	generation uint64

	onError func(*Error)
	errs    []*Error
}

// RunnerOptions configures a Runner.
type RunnerOptions struct {
	Mode    Mode
	Cwd     string
	Trusted bool
	// UI is the host's interactive surface. Nil means headless: dialogs fail
	// with ErrNoUI instead of hanging.
	UI UI
	// OnError receives every handler failure. Nil collects them for Errors().
	OnError func(*Error)
}

// NewRunner creates a Runner with no extensions loaded.
func NewRunner(opts RunnerOptions) *Runner {
	ui := opts.UI
	if ui == nil {
		ui = NoUI{}
	}
	return &Runner{
		mode: opts.Mode, cwd: opts.Cwd, trusted: opts.Trusted,
		ui: ui, onError: opts.OnError,
	}
}

// Load runs an extension's factory and registers it. A factory that fails is
// reported and skipped: one broken extension must not prevent startup.
func (r *Runner) Load(ext Extension) error {
	r.mu.Lock()
	api := &API{
		name: ext.Name, path: ext.Path, flagVals: map[string]any{},
		ui: r.ui, cwd: r.cwd, mode: r.mode, trusted: r.trusted, runtime: r.runtime,
	}
	r.mu.Unlock()
	if err := ext.Factory(api); err != nil {
		e := &Error{Extension: ext.Name, Event: "load", Err: err}
		r.reportError(e)
		return e
	}
	r.mu.Lock()
	r.apis = append(r.apis, api)
	r.mu.Unlock()
	return nil
}

// Bind attaches the host runtime, enabling extension action methods.
func (r *Runner) Bind(rt Runtime) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runtime = rt
	for _, a := range r.apis {
		a.mu.Lock()
		a.runtime = rt
		a.mu.Unlock()
	}
}

// SetSystemPrompt records the prompt reported by Context.SystemPrompt.
func (r *Runner) SetSystemPrompt(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.systemPrompt = s
}

// Invalidate bumps the generation, making every previously issued context
// stale. Hosts call this when the session is replaced or reloaded.
func (r *Runner) Invalidate() { atomic.AddUint64(&r.generation, 1) }

// Errors returns collected handler failures.
func (r *Runner) Errors() []*Error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*Error{}, r.errs...)
}

func (r *Runner) reportError(e *Error) {
	r.mu.Lock()
	r.errs = append(r.errs, e)
	cb := r.onError
	r.mu.Unlock()
	if cb != nil {
		cb(e)
	}
}

// Tools returns every registered tool, later registrations of the same name
// overriding earlier ones (extensions override built-ins by name).
func (r *Runner) Tools() []agent.Tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	byName := map[string]int{}
	var out []agent.Tool
	for _, a := range r.apis {
		for _, t := range a.Tools() {
			name := t.Def().Name
			if i, seen := byName[name]; seen {
				out[i] = t
				continue
			}
			byName[name] = len(out)
			out = append(out, t)
		}
	}
	return out
}

// Commands returns every registered command. Duplicate names are suffixed
// (:1, :2) so both remain reachable, matching Pi.
func (r *Runner) Commands() []Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]int{}
	var out []Command
	for _, a := range r.apis {
		for _, c := range a.Commands() {
			n := seen[c.Name]
			seen[c.Name] = n + 1
			if n > 0 {
				c.Name = fmt.Sprintf("%s:%d", c.Name, n)
			}
			out = append(out, c)
		}
	}
	return out
}

// newContext issues a context stamped with the current generation.
func (r *Runner) newContext() *Context {
	return &Context{runner: r, generation: atomic.LoadUint64(&r.generation)}
}

// NewCommandContext issues the context a command handler receives. Hosts call
// this when dispatching an extension command; the context goes stale as soon
// as the session is replaced, so it must be issued per invocation rather than
// captured.
func (r *Runner) NewCommandContext() *CommandContext {
	return &CommandContext{Context: r.newContext()}
}

// handlersFor collects (api, handler) pairs in dispatch order.
func (r *Runner) handlersFor(t EventType) []struct {
	api *API
	h   any
} {
	r.mu.Lock()
	apis := append([]*API{}, r.apis...)
	r.mu.Unlock()

	var out []struct {
		api *API
		h   any
	}
	for _, a := range apis {
		a.mu.Lock()
		hs := append([]any{}, a.handlers[t]...)
		a.mu.Unlock()
		for _, h := range hs {
			out = append(out, struct {
				api *API
				h   any
			}{a, h})
		}
	}
	return out
}

// safely runs fn, converting a panic into an error so one bad extension
// cannot take down the agent.
func safely(fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return fn()
}

// dispatch runs fn for each handler of t, reporting failures. It is the
// shared shell for the notification-only events.
func (r *Runner) dispatch(t EventType, fn func(h any, ec *Context) error) {
	ec := r.newContext()
	for _, e := range r.handlersFor(t) {
		if err := safely(func() error { return fn(e.h, ec) }); err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: t, Err: err})
		}
	}
}

// --- notification-only events: results ignored, failures reported ---

func (r *Runner) EmitSessionStart(ctx context.Context, ev *SessionStartEvent) {
	r.dispatch(EventSessionStart, func(h any, ec *Context) error {
		return h.(SessionStartHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitSessionInfoChanged(ctx context.Context, ev *SessionInfoChangedEvent) {
	r.dispatch(EventSessionInfoChanged, func(h any, ec *Context) error {
		return h.(SessionInfoChangedHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitSessionCompact(ctx context.Context, ev *SessionCompactEvent) {
	r.dispatch(EventSessionCompact, func(h any, ec *Context) error {
		return h.(SessionCompactHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitSessionTree(ctx context.Context, ev *SessionTreeEvent) {
	r.dispatch(EventSessionTree, func(h any, ec *Context) error {
		return h.(SessionTreeHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitSessionShutdown(ctx context.Context, ev *SessionShutdownEvent) {
	r.dispatch(EventSessionShutdown, func(h any, ec *Context) error {
		return h.(SessionShutdownHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitAgentStart(ctx context.Context) {
	ev := &AgentStartEvent{}
	r.dispatch(EventAgentStart, func(h any, ec *Context) error {
		return h.(AgentStartHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitAgentEnd(ctx context.Context, ev *AgentEndEvent) {
	r.dispatch(EventAgentEnd, func(h any, ec *Context) error {
		return h.(AgentEndHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitAgentSettled(ctx context.Context) {
	ev := &AgentSettledEvent{}
	r.dispatch(EventAgentSettled, func(h any, ec *Context) error {
		return h.(AgentSettledHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitTurnStart(ctx context.Context) {
	ev := &TurnStartEvent{}
	r.dispatch(EventTurnStart, func(h any, ec *Context) error {
		return h.(TurnStartHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitTurnEnd(ctx context.Context, ev *TurnEndEvent) {
	r.dispatch(EventTurnEnd, func(h any, ec *Context) error {
		return h.(TurnEndHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitMessageStart(ctx context.Context, ev *MessageStartEvent) {
	r.dispatch(EventMessageStart, func(h any, ec *Context) error {
		return h.(MessageStartHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitMessageUpdate(ctx context.Context, ev *MessageUpdateEvent) {
	r.dispatch(EventMessageUpdate, func(h any, ec *Context) error {
		return h.(MessageUpdateHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitToolExecutionStart(ctx context.Context, ev *ToolExecutionStartEvent) {
	r.dispatch(EventToolExecutionStart, func(h any, ec *Context) error {
		return h.(ToolExecutionStartHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitToolExecutionUpdate(ctx context.Context, ev *ToolExecutionUpdateEvent) {
	r.dispatch(EventToolExecutionUpdate, func(h any, ec *Context) error {
		return h.(ToolExecutionUpdateHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitToolExecutionEnd(ctx context.Context, ev *ToolExecutionEndEvent) {
	r.dispatch(EventToolExecutionEnd, func(h any, ec *Context) error {
		return h.(ToolExecutionEndHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitModelSelect(ctx context.Context, ev *ModelSelectEvent) {
	r.dispatch(EventModelSelect, func(h any, ec *Context) error {
		return h.(ModelSelectHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitThinkingLevelSelect(ctx context.Context, ev *ThinkingLevelSelectEvent) {
	r.dispatch(EventThinkingLevelSelect, func(h any, ec *Context) error {
		return h.(ThinkingLevelSelectHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitAfterProviderResponse(ctx context.Context, ev *AfterProviderResponseEvent) {
	r.dispatch(EventAfterProviderResponse, func(h any, ec *Context) error {
		return h.(AfterProviderResponseHandler)(ctx, ev, ec)
	})
}

// --- result-bearing events, each with its own composition policy ---

// EmitProjectTrust asks extensions whether the project is trusted.
// Policy: the first decisive vote (yes or no) wins; undecided falls through.
func (r *Runner) EmitProjectTrust(ctx context.Context, ev *ProjectTrustEvent) *ProjectTrustResult {
	ec := r.newContext()
	for _, e := range r.handlersFor(EventProjectTrust) {
		var res *ProjectTrustResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(ProjectTrustHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventProjectTrust, Err: err})
			continue
		}
		if res != nil && (res.Decision == TrustYes || res.Decision == TrustNo) {
			return res
		}
	}
	return nil
}

// EmitResourcesDiscover collects resource paths from every extension.
// Policy: concatenate all contributions, tagged with the owning extension.
func (r *Runner) EmitResourcesDiscover(ctx context.Context, ev *ResourcesDiscoverEvent) DiscoveredResources {
	var out DiscoveredResources
	ec := r.newContext()
	for _, e := range r.handlersFor(EventResourcesDiscover) {
		var res *ResourcesDiscoverResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(ResourcesDiscoverHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventResourcesDiscover, Err: err})
			continue
		}
		if res == nil {
			continue
		}
		for _, p := range res.SkillPaths {
			out.SkillPaths = append(out.SkillPaths, OwnedPath{Path: p, Extension: e.api.path})
		}
		for _, p := range res.PromptPaths {
			out.PromptPaths = append(out.PromptPaths, OwnedPath{Path: p, Extension: e.api.path})
		}
		for _, p := range res.ThemePaths {
			out.ThemePaths = append(out.ThemePaths, OwnedPath{Path: p, Extension: e.api.path})
		}
	}
	return out
}

// emitSessionBefore is the shared policy for the four session_before_* events:
// any handler cancelling short-circuits immediately; otherwise the last
// non-nil result wins.
func (r *Runner) emitSessionBefore(t EventType, call func(h any, ec *Context) (*SessionBeforeResult, error)) *SessionBeforeResult {
	ec := r.newContext()
	var last *SessionBeforeResult
	for _, e := range r.handlersFor(t) {
		var res *SessionBeforeResult
		err := safely(func() error {
			var herr error
			res, herr = call(e.h, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: t, Err: err})
			continue
		}
		if res != nil {
			last = res
			if res.Cancel {
				return res
			}
		}
	}
	return last
}

func (r *Runner) EmitSessionBeforeSwitch(ctx context.Context, ev *SessionBeforeSwitchEvent) *SessionBeforeResult {
	return r.emitSessionBefore(EventSessionBeforeSwitch, func(h any, ec *Context) (*SessionBeforeResult, error) {
		return h.(SessionBeforeSwitchHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitSessionBeforeFork(ctx context.Context, ev *SessionBeforeForkEvent) *SessionBeforeResult {
	return r.emitSessionBefore(EventSessionBeforeFork, func(h any, ec *Context) (*SessionBeforeResult, error) {
		return h.(SessionBeforeForkHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitSessionBeforeCompact(ctx context.Context, ev *SessionBeforeCompactEvent) *SessionBeforeResult {
	return r.emitSessionBefore(EventSessionBeforeCompact, func(h any, ec *Context) (*SessionBeforeResult, error) {
		return h.(SessionBeforeCompactHandler)(ctx, ev, ec)
	})
}

func (r *Runner) EmitSessionBeforeTree(ctx context.Context, ev *SessionBeforeTreeEvent) *SessionBeforeResult {
	return r.emitSessionBefore(EventSessionBeforeTree, func(h any, ec *Context) (*SessionBeforeResult, error) {
		return h.(SessionBeforeTreeHandler)(ctx, ev, ec)
	})
}

// EmitContext offers the message list to extensions.
// Policy: chained replacement — each handler sees the previous handler's
// output. The input slice is copied first so a handler cannot corrupt the
// caller's transcript.
func (r *Runner) EmitContext(ctx context.Context, messages []ai.Message) []ai.Message {
	cur := append([]ai.Message{}, messages...)
	ec := r.newContext()
	for _, e := range r.handlersFor(EventContext) {
		ev := &ContextEvent{Messages: cur}
		var res *ContextResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(ContextHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventContext, Err: err})
			continue
		}
		if res != nil && res.Messages != nil {
			cur = res.Messages
		}
	}
	return cur
}

// EmitBeforeProviderRequest offers the raw provider payload.
// Policy: chained replacement; a nil result keeps the current payload.
func (r *Runner) EmitBeforeProviderRequest(ctx context.Context, payload any) any {
	cur := payload
	ec := r.newContext()
	for _, e := range r.handlersFor(EventBeforeProviderRequest) {
		ev := &BeforeProviderRequestEvent{Payload: cur}
		var res *BeforeProviderRequestResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(BeforeProviderRequestHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventBeforeProviderRequest, Err: err})
			continue
		}
		if res != nil && res.Payload != nil {
			cur = res.Payload
		}
	}
	return cur
}

// EmitBeforeProviderHeaders offers the outgoing headers.
// Policy: handlers mutate the map in place and their return value is ignored.
func (r *Runner) EmitBeforeProviderHeaders(ctx context.Context, headers map[string]*string) map[string]*string {
	ev := &BeforeProviderHeadersEvent{Headers: headers}
	r.dispatch(EventBeforeProviderHeaders, func(h any, ec *Context) error {
		return h.(BeforeProviderHeadersHandler)(ctx, ev, ec)
	})
	return headers
}

// EmitBeforeAgentStart offers the prompt and system prompt before a run.
// Policy: injected messages ACCUMULATE across extensions, while the system
// prompt CHAINS — each handler observes the previous handler's replacement,
// including through Context.SystemPrompt.
func (r *Runner) EmitBeforeAgentStart(ctx context.Context, prompt string, images []ai.ImageContent, systemPrompt string) *BeforeAgentStartCombined {
	cur := systemPrompt
	modified := false
	var messages []ai.Message

	ec := r.newContext()
	ec.systemPrompt = &cur

	for _, e := range r.handlersFor(EventBeforeAgentStart) {
		ev := &BeforeAgentStartEvent{Prompt: prompt, Images: images, SystemPrompt: cur}
		var res *BeforeAgentStartResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(BeforeAgentStartHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventBeforeAgentStart, Err: err})
			continue
		}
		if res == nil {
			continue
		}
		if res.Message != nil {
			messages = append(messages, res.Message)
		}
		if res.SystemPrompt != nil {
			cur = *res.SystemPrompt
			modified = true
		}
	}

	if len(messages) == 0 && !modified {
		return nil
	}
	out := &BeforeAgentStartCombined{Messages: messages}
	if modified {
		out.SystemPrompt = &cur
	}
	return out
}

// EmitMessageEnd offers a finished message for replacement.
// Policy: chained — each handler sees the previous replacement. A replacement
// whose role differs is rejected as an extension error and ignored, because a
// role change would corrupt the transcript. Returns nil when unmodified.
func (r *Runner) EmitMessageEnd(ctx context.Context, message ai.Message) ai.Message {
	cur := message
	modified := false
	ec := r.newContext()

	for _, e := range r.handlersFor(EventMessageEnd) {
		ev := &MessageEndEvent{Message: cur}
		var res *MessageEndResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(MessageEndHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventMessageEnd, Err: err})
			continue
		}
		if res == nil || res.Message == nil {
			continue
		}
		if res.Message.Role() != cur.Role() {
			r.reportError(&Error{
				Extension: e.api.name, Event: EventMessageEnd,
				Err: fmt.Errorf("replacement message must keep role %q, got %q", cur.Role(), res.Message.Role()),
			})
			continue
		}
		cur = res.Message
		modified = true
	}

	if !modified {
		return nil
	}
	return cur
}

// EmitToolCall offers a validated tool call before execution.
//
// Policy: a handler setting Block short-circuits immediately; otherwise the
// last non-nil result wins. Uniquely among the events, a handler that FAILS
// blocks the tool rather than being ignored — a permission gate that errors
// must fail closed, not silently allow.
func (r *Runner) EmitToolCall(ctx context.Context, ev *ToolCallEvent) *ToolCallResult {
	ec := r.newContext()
	var last *ToolCallResult
	for _, e := range r.handlersFor(EventToolCall) {
		var res *ToolCallResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(ToolCallHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventToolCall, Err: err})
			return &ToolCallResult{Block: true, Reason: fmt.Sprintf("extension %s failed: %v", e.api.name, err)}
		}
		if res != nil {
			last = res
			if res.Block {
				return res
			}
		}
	}
	return last
}

// EmitToolResult offers a finished tool result for patching.
// Policy: field-wise middleware — each handler patches individual fields and
// later handlers observe earlier patches. Returns nil when unmodified.
func (r *Runner) EmitToolResult(ctx context.Context, ev *ToolResultEvent) *ToolResultResult {
	cur := *ev
	modified := false
	ec := r.newContext()

	for _, e := range r.handlersFor(EventToolResult) {
		var res *ToolResultResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(ToolResultHandler)(ctx, &cur, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventToolResult, Err: err})
			continue
		}
		if res == nil {
			continue
		}
		if res.Content != nil {
			cur.Content = res.Content
			modified = true
		}
		if res.Details != nil {
			cur.Details = res.Details
			modified = true
		}
		if res.IsError != nil {
			cur.IsError = *res.IsError
			modified = true
		}
		if res.Usage != nil {
			cur.Usage = res.Usage
			modified = true
		}
	}

	if !modified {
		return nil
	}
	isErr := cur.IsError
	return &ToolResultResult{Content: cur.Content, Details: cur.Details, IsError: &isErr, Usage: cur.Usage}
}

// EmitUserBash offers a user-issued shell command.
// Policy: the FIRST non-nil result wins and short-circuits.
func (r *Runner) EmitUserBash(ctx context.Context, ev *UserBashEvent) *UserBashResult {
	ec := r.newContext()
	for _, e := range r.handlersFor(EventUserBash) {
		var res *UserBashResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(UserBashHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventUserBash, Err: err})
			continue
		}
		if res != nil {
			return res
		}
	}
	return nil
}

// EmitInput offers user input before it becomes a prompt.
// Policy: "handled" short-circuits immediately; "transform" chains text and
// images through subsequent handlers. Returns a transform result only when
// something actually changed.
func (r *Runner) EmitInput(ctx context.Context, text string, images []ai.ImageContent, source string, streamingBehavior string) *InputResult {
	curText, curImages := text, images
	changed := false
	ec := r.newContext()

	for _, e := range r.handlersFor(EventInput) {
		ev := &InputEvent{Text: curText, Images: curImages, Source: source, StreamingBehavior: streamingBehavior}
		var res *InputResult
		err := safely(func() error {
			var herr error
			res, herr = e.h.(InputHandler)(ctx, ev, ec)
			return herr
		})
		if err != nil {
			r.reportError(&Error{Extension: e.api.name, Event: EventInput, Err: err})
			continue
		}
		if res == nil {
			continue
		}
		switch res.Action {
		case InputHandled:
			return res
		case InputTransform:
			curText = res.Text
			if res.Images != nil {
				curImages = res.Images
			}
			changed = true
		}
	}

	if !changed {
		return &InputResult{Action: InputContinue}
	}
	return &InputResult{Action: InputTransform, Text: curText, Images: curImages}
}

// Names lists the loaded extensions in load order.
func (r *Runner) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.apis))
	for _, a := range r.apis {
		out = append(out, a.name)
	}
	return out
}
