package extension

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
)

// Factory is an extension's entry point. It registers handlers and resources
// on the supplied API and returns.
type Factory func(api *API) error

// Extension pairs a factory with an identity for diagnostics.
type Extension struct {
	// Name identifies the extension in errors and UI.
	Name string
	// Path is the source location, when the extension came from disk.
	Path string
	// Hidden omits the extension from user-facing listings (bundled ones).
	Hidden  bool
	Factory Factory
}

// ErrStale is returned when an extension uses a context captured before a
// session switch, fork, or reload. Pi throws here; Go returns an error.
var ErrStale = errors.New(
	"extension: this context belongs to a replaced session. " +
		"Do not hold on to a captured `api` or command context across " +
		"newSession/fork/switchSession/reload — request a fresh one")

// Error is a failure raised by an extension handler. Handlers never abort the
// agent: their failures are collected here and reported.
type Error struct {
	Extension string
	Event     EventType
	Err       error
}

func (e *Error) Error() string {
	return fmt.Sprintf("extension %s: %s: %v", e.Extension, e.Event, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Handler signatures. Every handler takes a cancellable context (Pi's
// ctx.signal), the typed event, and the extension context.
type (
	ProjectTrustHandler      func(context.Context, *ProjectTrustEvent, *Context) (*ProjectTrustResult, error)
	ResourcesDiscoverHandler func(context.Context, *ResourcesDiscoverEvent, *Context) (*ResourcesDiscoverResult, error)

	SessionStartHandler       func(context.Context, *SessionStartEvent, *Context) error
	SessionInfoChangedHandler func(context.Context, *SessionInfoChangedEvent, *Context) error
	SessionBeforeSwitchHandler  func(context.Context, *SessionBeforeSwitchEvent, *Context) (*SessionBeforeResult, error)
	SessionBeforeForkHandler    func(context.Context, *SessionBeforeForkEvent, *Context) (*SessionBeforeResult, error)
	SessionBeforeCompactHandler func(context.Context, *SessionBeforeCompactEvent, *Context) (*SessionBeforeResult, error)
	SessionBeforeTreeHandler    func(context.Context, *SessionBeforeTreeEvent, *Context) (*SessionBeforeResult, error)
	SessionCompactHandler       func(context.Context, *SessionCompactEvent, *Context) error
	SessionTreeHandler          func(context.Context, *SessionTreeEvent, *Context) error
	SessionShutdownHandler      func(context.Context, *SessionShutdownEvent, *Context) error

	ContextHandler               func(context.Context, *ContextEvent, *Context) (*ContextResult, error)
	BeforeProviderRequestHandler func(context.Context, *BeforeProviderRequestEvent, *Context) (*BeforeProviderRequestResult, error)
	BeforeProviderHeadersHandler func(context.Context, *BeforeProviderHeadersEvent, *Context) error
	AfterProviderResponseHandler func(context.Context, *AfterProviderResponseEvent, *Context) error

	BeforeAgentStartHandler func(context.Context, *BeforeAgentStartEvent, *Context) (*BeforeAgentStartResult, error)
	AgentStartHandler       func(context.Context, *AgentStartEvent, *Context) error
	AgentEndHandler         func(context.Context, *AgentEndEvent, *Context) error
	AgentSettledHandler     func(context.Context, *AgentSettledEvent, *Context) error
	TurnStartHandler        func(context.Context, *TurnStartEvent, *Context) error
	TurnEndHandler          func(context.Context, *TurnEndEvent, *Context) error

	MessageStartHandler  func(context.Context, *MessageStartEvent, *Context) error
	MessageUpdateHandler func(context.Context, *MessageUpdateEvent, *Context) error
	MessageEndHandler    func(context.Context, *MessageEndEvent, *Context) (*MessageEndResult, error)

	ToolExecutionStartHandler  func(context.Context, *ToolExecutionStartEvent, *Context) error
	ToolExecutionUpdateHandler func(context.Context, *ToolExecutionUpdateEvent, *Context) error
	ToolExecutionEndHandler    func(context.Context, *ToolExecutionEndEvent, *Context) error

	ModelSelectHandler         func(context.Context, *ModelSelectEvent, *Context) error
	ThinkingLevelSelectHandler func(context.Context, *ThinkingLevelSelectEvent, *Context) error

	ToolCallHandler   func(context.Context, *ToolCallEvent, *Context) (*ToolCallResult, error)
	ToolResultHandler func(context.Context, *ToolResultEvent, *Context) (*ToolResultResult, error)
	UserBashHandler   func(context.Context, *UserBashEvent, *Context) (*UserBashResult, error)
	InputHandler      func(context.Context, *InputEvent, *Context) (*InputResult, error)
)

// Command is an extension-registered slash command.
type Command struct {
	Name        string
	Description string
	// Handler runs the command. args is the rest of the line.
	Handler func(ctx context.Context, args string, cc *CommandContext) error
	// ArgumentCompletions offers completions for the argument being typed.
	ArgumentCompletions func(prefix string) []CompletionItem
}

// CompletionItem is one autocomplete suggestion.
type CompletionItem struct {
	Value       string
	Label       string
	Description string
}

// Shortcut is an extension-registered key binding.
type Shortcut struct {
	Key         string
	Description string
	Handler     func(ctx context.Context, ec *Context) error
}

// Flag is an extension-registered CLI flag.
type Flag struct {
	Name        string
	Description string
	// Type is "bool" or "string".
	Type    string
	Default any
}

// API is the surface an extension factory registers against.
type API struct {
	mu sync.Mutex

	name string
	path string

	handlers map[EventType][]any

	tools     []agent.Tool
	commands  []Command
	shortcuts []Shortcut
	flags     []Flag
	flagVals  map[string]any

	// runtime is bound by the Runner once the host is ready; action methods
	// before binding return ErrNotBound.
	runtime Runtime
}

// ErrNotBound is returned by action methods called during factory execution,
// before the host has bound a runtime.
var ErrNotBound = errors.New("extension: action unavailable during registration")

// Name returns the extension's name.
func (a *API) Name() string { return a.name }

// Path returns the extension's source path, when it has one.
func (a *API) Path() string { return a.path }

func (a *API) on(t EventType, h any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.handlers == nil {
		a.handlers = map[EventType][]any{}
	}
	a.handlers[t] = append(a.handlers[t], h)
}

// Subscribed reports whether the extension registered any handler for t. The
// subprocess host uses this to avoid shipping unsubscribed events over the
// wire.
func (a *API) Subscribed(t EventType) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.handlers[t]) > 0
}

// Typed registration — one method per event, mirroring Pi's 33 overloads.

func (a *API) OnProjectTrust(h ProjectTrustHandler)           { a.on(EventProjectTrust, h) }
func (a *API) OnResourcesDiscover(h ResourcesDiscoverHandler) { a.on(EventResourcesDiscover, h) }

func (a *API) OnSessionStart(h SessionStartHandler)       { a.on(EventSessionStart, h) }
func (a *API) OnSessionInfoChanged(h SessionInfoChangedHandler) {
	a.on(EventSessionInfoChanged, h)
}
func (a *API) OnSessionBeforeSwitch(h SessionBeforeSwitchHandler) {
	a.on(EventSessionBeforeSwitch, h)
}
func (a *API) OnSessionBeforeFork(h SessionBeforeForkHandler) { a.on(EventSessionBeforeFork, h) }
func (a *API) OnSessionBeforeCompact(h SessionBeforeCompactHandler) {
	a.on(EventSessionBeforeCompact, h)
}
func (a *API) OnSessionBeforeTree(h SessionBeforeTreeHandler) { a.on(EventSessionBeforeTree, h) }
func (a *API) OnSessionCompact(h SessionCompactHandler)       { a.on(EventSessionCompact, h) }
func (a *API) OnSessionTree(h SessionTreeHandler)             { a.on(EventSessionTree, h) }
func (a *API) OnSessionShutdown(h SessionShutdownHandler)     { a.on(EventSessionShutdown, h) }

func (a *API) OnContext(h ContextHandler) { a.on(EventContext, h) }
func (a *API) OnBeforeProviderRequest(h BeforeProviderRequestHandler) {
	a.on(EventBeforeProviderRequest, h)
}
func (a *API) OnBeforeProviderHeaders(h BeforeProviderHeadersHandler) {
	a.on(EventBeforeProviderHeaders, h)
}
func (a *API) OnAfterProviderResponse(h AfterProviderResponseHandler) {
	a.on(EventAfterProviderResponse, h)
}

func (a *API) OnBeforeAgentStart(h BeforeAgentStartHandler) { a.on(EventBeforeAgentStart, h) }
func (a *API) OnAgentStart(h AgentStartHandler)             { a.on(EventAgentStart, h) }
func (a *API) OnAgentEnd(h AgentEndHandler)                 { a.on(EventAgentEnd, h) }
func (a *API) OnAgentSettled(h AgentSettledHandler)         { a.on(EventAgentSettled, h) }
func (a *API) OnTurnStart(h TurnStartHandler)               { a.on(EventTurnStart, h) }
func (a *API) OnTurnEnd(h TurnEndHandler)                   { a.on(EventTurnEnd, h) }

func (a *API) OnMessageStart(h MessageStartHandler)   { a.on(EventMessageStart, h) }
func (a *API) OnMessageUpdate(h MessageUpdateHandler) { a.on(EventMessageUpdate, h) }
func (a *API) OnMessageEnd(h MessageEndHandler)       { a.on(EventMessageEnd, h) }

func (a *API) OnToolExecutionStart(h ToolExecutionStartHandler) {
	a.on(EventToolExecutionStart, h)
}
func (a *API) OnToolExecutionUpdate(h ToolExecutionUpdateHandler) {
	a.on(EventToolExecutionUpdate, h)
}
func (a *API) OnToolExecutionEnd(h ToolExecutionEndHandler) { a.on(EventToolExecutionEnd, h) }

func (a *API) OnModelSelect(h ModelSelectHandler) { a.on(EventModelSelect, h) }
func (a *API) OnThinkingLevelSelect(h ThinkingLevelSelectHandler) {
	a.on(EventThinkingLevelSelect, h)
}

func (a *API) OnToolCall(h ToolCallHandler)     { a.on(EventToolCall, h) }
func (a *API) OnToolResult(h ToolResultHandler) { a.on(EventToolResult, h) }
func (a *API) OnUserBash(h UserBashHandler)     { a.on(EventUserBash, h) }
func (a *API) OnInput(h InputHandler)           { a.on(EventInput, h) }

// --- registration ---

// RegisterTool adds a tool. Registering a name that already exists overrides
// the earlier tool, which is how extensions replace built-ins.
func (a *API) RegisterTool(t agent.Tool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools = append(a.tools, t)
}

// RegisterCommand adds a slash command.
func (a *API) RegisterCommand(c Command) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commands = append(a.commands, c)
}

// RegisterShortcut adds a key binding.
func (a *API) RegisterShortcut(s Shortcut) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.shortcuts = append(a.shortcuts, s)
}

// RegisterFlag adds a CLI flag.
func (a *API) RegisterFlag(f Flag) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flags = append(a.flags, f)
}

// Flag returns a parsed flag value supplied by the host.
func (a *API) Flag(name string) any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.flagVals[name]
}

// Tools returns the tools this extension registered.
func (a *API) Tools() []agent.Tool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.Tool{}, a.tools...)
}

// Commands returns the commands this extension registered.
func (a *API) Commands() []Command {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Command{}, a.commands...)
}

// Shortcuts returns the shortcuts this extension registered.
func (a *API) Shortcuts() []Shortcut {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Shortcut{}, a.shortcuts...)
}

// Flags returns the flags this extension declared.
func (a *API) Flags() []Flag {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Flag{}, a.flags...)
}

// --- actions (delegated to the host runtime) ---

// Runtime is what the host provides so extensions can act on the session. It
// is bound after all factories have run.
type Runtime interface {
	SendMessage(msg ai.Message, deliverAs string) error
	SetSessionName(name string) error
	SessionName() string
	Exec(ctx context.Context, command string) (string, int, error)
	ActiveToolNames() []string
	SetActiveTools(names []string) error
	Model() *ai.Model
	SetModel(m *ai.Model) error
	ThinkingLevel() ai.ModelThinkingLevel
	SetThinkingLevel(l ai.ModelThinkingLevel) error
}

func (a *API) runtimeOrErr() (Runtime, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runtime == nil {
		return nil, ErrNotBound
	}
	return a.runtime, nil
}

// SendMessage delivers a message into the conversation. deliverAs is
// "steer", "followUp", or "" for the next turn.
func (a *API) SendMessage(msg ai.Message, deliverAs string) error {
	rt, err := a.runtimeOrErr()
	if err != nil {
		return err
	}
	return rt.SendMessage(msg, deliverAs)
}

// SetSessionName renames the session.
func (a *API) SetSessionName(name string) error {
	rt, err := a.runtimeOrErr()
	if err != nil {
		return err
	}
	return rt.SetSessionName(name)
}

// Exec runs a shell command in the session's environment.
func (a *API) Exec(ctx context.Context, command string) (string, int, error) {
	rt, err := a.runtimeOrErr()
	if err != nil {
		return "", 0, err
	}
	return rt.Exec(ctx, command)
}

// SetActiveTools replaces the active tool set by name.
func (a *API) SetActiveTools(names []string) error {
	rt, err := a.runtimeOrErr()
	if err != nil {
		return err
	}
	return rt.SetActiveTools(names)
}
