// Package wire is the subprocess extension protocol: the frames tau and an
// out-of-process extension exchange over stdio.
//
// # Why a wire protocol at all
//
// tau's in-process extension API (the parent package) is Go-only. Pi's
// extensions are TypeScript, and there are ~76 of them. Running them means
// running a Node process and talking to it. The same protocol lets an
// extension be written in any language that can read and write lines.
//
// # Framing
//
// LF-only JSONL. One JSON value per line, terminated by U+000A, and nothing
// else counts as a terminator. A JSON string may legally contain U+2028 and
// U+2029, and a JavaScript peer using JSON.stringify will emit them raw, so a
// reader that treats them as line breaks corrupts the frame. Go's encoder
// escapes them, which makes tau's own output safe either way; the reader is
// where the rule has to be enforced.
//
// # These types are the single source of truth
//
// cmd/gen-wire-ts regenerates the TypeScript declarations the host shim
// compiles against directly from this file's structs. CI fails if the
// checked-in output drifts, so the two surfaces cannot disagree.
package wire

import "encoding/json"

// Protocol is the wire version tau speaks. An extension declaring a different
// major version is refused rather than half-understood.
const Protocol = 1

// FrameType discriminates a frame. Host-originated and extension-originated
// types are disjoint, so a frame arriving from the wrong direction is a
// protocol error rather than an ambiguity.
type FrameType string

const (
	// --- host → extension ---

	// FrameInit opens the connection and asks the extension to declare
	// itself. It is the only frame sent before the handshake completes.
	FrameInit FrameType = "init"
	// FrameEvent dispatches one extension event. Only events the extension
	// subscribed to are ever sent.
	FrameEvent FrameType = "event"
	// FrameToolExecute runs a tool the extension registered.
	FrameToolExecute FrameType = "tool_execute"
	// FrameCommand runs a slash command the extension registered.
	FrameCommand FrameType = "command"
	// FrameCompletions asks for argument completions for a command.
	FrameCompletions FrameType = "completions"
	// FrameShortcut fires a key binding the extension registered.
	FrameShortcut FrameType = "shortcut"
	// FrameRender asks a registered renderer for its lines.
	FrameRender FrameType = "render"
	// FrameCancel abandons an in-flight request. The extension should stop
	// work and reply; the host has already moved on if the grace period
	// expired.
	FrameCancel FrameType = "cancel"
	// FrameReply answers an extension-originated ui_request or action.
	FrameReply FrameType = "reply"
	// FrameShutdown asks the extension to exit. The host closes stdin after
	// sending it and kills the process if it outlives the grace period.
	FrameShutdown FrameType = "shutdown"

	// --- extension → host ---

	// FrameInitResult completes the handshake.
	FrameInitResult FrameType = "init_result"
	// FrameResult answers a host request. Which payload shape it carries is
	// determined by the request the id belongs to.
	FrameResult FrameType = "result"
	// FrameToolUpdate streams a partial tool result. It carries no reply.
	FrameToolUpdate FrameType = "tool_update"
	// FrameUIRequest asks the host to interact with the user.
	FrameUIRequest FrameType = "ui_request"
	// FrameAction asks the host to act on the session.
	FrameAction FrameType = "action"
	// FrameLog writes a diagnostic line. It carries no reply.
	FrameLog FrameType = "log"
)

// Envelope is enough of a frame to route it. The full frame is decoded again
// into its concrete type once the direction and kind are known.
type Envelope struct {
	Type FrameType `json:"type"`
	// ID correlates a request with its result. Frames that are neither carry
	// none.
	ID string `json:"id,omitempty"`
}

// --- handshake ---

// Init opens the connection.
type Init struct {
	Type FrameType `json:"type"`
	// Protocol is the host's wire version.
	Protocol int `json:"protocol"`
	// Name is the extension's identity in diagnostics, derived from its path.
	Name string `json:"name"`
	// Path is the file or directory the extension was loaded from.
	Path string `json:"path"`
	// Cwd is the session's working directory.
	Cwd string `json:"cwd"`
	// Mode is always "rpc" for a subprocess extension, whatever the host's
	// own mode is. Pi extensions branch on ctx.mode !== "tui" to decide
	// whether a UI exists, and over a wire it does not in the sense they
	// mean: there is no component tree to mount into, only the request
	// shapes in this protocol.
	Mode string `json:"mode"`
	// Trusted reports whether project-scoped resources were allowed to load.
	// An extension reading files from the project must respect it.
	Trusted bool `json:"trusted"`
	// Generation is the session generation this connection starts at.
	Generation uint64 `json:"generation"`
	// Flags carries values for flags the extension declared on a previous
	// run. It is empty on a first connection, since flags are not known until
	// the extension declares them.
	Flags map[string]any `json:"flags,omitempty"`
	// TauVersion lets an extension branch on host capabilities.
	TauVersion string `json:"tauVersion,omitempty"`
	// State is a snapshot of what the extension can otherwise only learn by
	// asking.
	//
	// It exists because Pi's ExtensionAPI getters are synchronous — an
	// extension writes `pi.getSessionName()` and puts the string straight into
	// a message. Nothing can be synchronous across a pipe, so the shim mirrors
	// this and keeps it current from the events that change it. Without the
	// seed, every getter would return empty until the first such event, which
	// for a session name is never.
	State *SessionState `json:"state,omitempty"`
}

// SessionState is the mirrored view an out-of-process extension reads
// synchronously.
type SessionState struct {
	SessionName   string     `json:"sessionName,omitempty"`
	Model         *ModelInfo `json:"model,omitempty"`
	ThinkingLevel string     `json:"thinkingLevel,omitempty"`
	ActiveTools   []string   `json:"activeTools,omitempty"`
	// Commands is the host's slash-command list, which Pi exposes through
	// getCommands().
	Commands []CommandInfo `json:"commands,omitempty"`
}

// CommandInfo describes a command the host offers.
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Source is "builtin", "extension", "prompt", or "skill".
	Source string `json:"source,omitempty"`
}

// ToolDecl is a tool the extension registers.
type ToolDecl struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is a JSON Schema object. It is passed through to the
	// provider unchanged, exactly as an in-process tool's schema is.
	Parameters json.RawMessage `json:"parameters,omitempty"`
	// Streaming declares that the extension emits tool_update frames while
	// the tool runs. Hosts that cannot show partial results ignore it.
	Streaming bool `json:"streaming,omitempty"`
}

// CommandDecl is a slash command the extension registers.
type CommandDecl struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Completions declares that the extension answers completions frames for
	// this command. Without it the host never asks.
	Completions bool `json:"completions,omitempty"`
}

// ShortcutDecl is a key binding the extension registers.
type ShortcutDecl struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
}

// FlagDecl is a CLI flag the extension registers.
type FlagDecl struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Type is "bool" or "string".
	Type    string `json:"type"`
	Default any    `json:"default,omitempty"`
}

// RendererDecl is a renderer the extension registers.
type RendererDecl struct {
	// Kind is "message" or "entry".
	Kind string `json:"kind"`
	// Selector narrows what the renderer claims: a message role, or an entry
	// type. Empty claims everything of that kind.
	Selector string `json:"selector,omitempty"`
}

// InitResult completes the handshake.
//
// It is declarative on purpose. The host learns everything it needs to route
// work in one round trip, and an event nobody subscribed to is never
// serialized, never written, and never woken up for.
type InitResult struct {
	Type FrameType `json:"type"`
	// Protocol is the extension's wire version.
	Protocol int `json:"protocol"`
	// Name overrides the host's guess at the extension's name.
	Name string `json:"name,omitempty"`
	// Subscriptions lists the event types the extension handles. Anything not
	// listed is never sent.
	Subscriptions []string `json:"subscriptions,omitempty"`

	Tools     []ToolDecl     `json:"tools,omitempty"`
	Commands  []CommandDecl  `json:"commands,omitempty"`
	Shortcuts []ShortcutDecl `json:"shortcuts,omitempty"`
	Flags     []FlagDecl     `json:"flags,omitempty"`
	Renderers []RendererDecl `json:"renderers,omitempty"`

	// Warnings are non-fatal load complaints — an unsupported API the shim
	// stubbed out, a deprecated import. The host surfaces them once.
	Warnings []string `json:"warnings,omitempty"`
	// Error fails the handshake. The extension is not loaded.
	Error string `json:"error,omitempty"`
}

// --- host requests ---

// Event dispatches one extension event.
type Event struct {
	Type FrameType `json:"type"`
	ID   string    `json:"id"`
	// Event is the event type name, matching extension.EventType.
	Event string `json:"event"`
	// Generation stamps the session this event belongs to. A result arriving
	// with a stale generation is discarded: the session it referred to is
	// gone, and applying its decision to the current one would be wrong.
	Generation uint64 `json:"generation"`
	// Payload is the event struct, marshalled with its own JSON tags.
	Payload json.RawMessage `json:"payload,omitempty"`
	// NoReply marks an event whose result the host will not wait for. Used
	// for the notification-only events, where the extension's answer cannot
	// change anything and waiting would put a subprocess round trip in the
	// middle of the agent loop.
	NoReply bool `json:"noReply,omitempty"`
}

// ToolExecute runs a registered tool.
type ToolExecute struct {
	Type       FrameType       `json:"type"`
	ID         string          `json:"id"`
	Generation uint64          `json:"generation"`
	Tool       string          `json:"tool"`
	CallID     string          `json:"callId"`
	Args       json.RawMessage `json:"args,omitempty"`
}

// Command runs a registered slash command.
type Command struct {
	Type       FrameType `json:"type"`
	ID         string    `json:"id"`
	Generation uint64    `json:"generation"`
	Name       string    `json:"name"`
	Args       string    `json:"args,omitempty"`
}

// Completions asks for argument completions.
type Completions struct {
	Type   FrameType `json:"type"`
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Prefix string    `json:"prefix,omitempty"`
}

// Shortcut fires a registered key binding.
type Shortcut struct {
	Type       FrameType `json:"type"`
	ID         string    `json:"id"`
	Generation uint64    `json:"generation"`
	Key        string    `json:"key"`
}

// Render asks a renderer for its lines.
type Render struct {
	Type FrameType `json:"type"`
	ID   string    `json:"id"`
	// Kind is "message" or "entry".
	Kind string `json:"kind"`
	// Width is the available column count.
	Width int `json:"width"`
	// Payload is the message or entry to render.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Cancel abandons an in-flight request.
type Cancel struct {
	Type FrameType `json:"type"`
	ID   string    `json:"id"`
}

// Shutdown asks the extension to exit.
type Shutdown struct {
	Type FrameType `json:"type"`
	// Reason is "exit", "reload", or "suspended".
	Reason string `json:"reason,omitempty"`
}

// --- extension results ---

// Result answers a host request.
//
// The payload shape follows from the request: an Event's result is the event's
// own result struct, a ToolExecute's is a ToolResult, a Completions' is a
// CompletionItem list, a Render's is a line list. A Command and a Shortcut
// carry no payload.
//
// A nil Payload means "no opinion" and is not the same as an empty object: for
// most events the composition policy distinguishes them, and collapsing the
// two would silently turn "I did not decide" into "I decided the zero value".
type Result struct {
	Type    FrameType       `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Error reports a handler failure, and what the host does with it depends
	// on the event.
	//
	// tool_call fails CLOSED: an error, a timeout, a crash, or a suspended
	// extension all block the call. A permission gate that stops answering
	// must not be read as consent, so silence is a refusal.
	//
	// Every other event fails open: the failure is reported to the user and
	// the agent carries on as though the extension had no opinion. A broken
	// extension must not be able to stop the agent.
	Error string `json:"error,omitempty"`
}

// ToolUpdate streams a partial tool result. It expects no reply.
type ToolUpdate struct {
	Type FrameType `json:"type"`
	// ID is the ToolExecute request being updated.
	ID      string          `json:"id"`
	Partial json.RawMessage `json:"partial,omitempty"`
}

// Log writes a diagnostic line. It expects no reply.
type Log struct {
	Type FrameType `json:"type"`
	// Level is "debug", "info", "warn", or "error".
	Level   string `json:"level,omitempty"`
	Message string `json:"message"`
}

// --- extension-originated requests ---

// UIRequest asks the host to interact with the user.
//
// The field names are Pi's, verbatim from its RpcExtensionUIRequest. A Pi
// extension running under the shim produces exactly these shapes already, and
// an rpc client written against Pi understands them without a translation
// layer on either side.
type UIRequest struct {
	Type FrameType `json:"type"`
	ID   string    `json:"id"`
	// Method is one of: select, confirm, input, editor, notify, setStatus,
	// setWidget, setTitle, set_editor_text.
	Method string `json:"method"`

	Title   string   `json:"title,omitempty"`
	Message string   `json:"message,omitempty"`
	Options []string `json:"options,omitempty"`
	// Timeout is in milliseconds. Zero waits indefinitely.
	Timeout     int    `json:"timeout,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Prefill     string `json:"prefill,omitempty"`
	// NotifyType is "info", "warning", or "error".
	NotifyType string `json:"notifyType,omitempty"`

	StatusKey string `json:"statusKey,omitempty"`
	// StatusText is a pointer so that clearing a status (null) is
	// distinguishable from setting it to the empty string.
	StatusText *string `json:"statusText,omitempty"`

	WidgetKey string `json:"widgetKey,omitempty"`
	// WidgetLines nil removes the widget.
	WidgetLines []string `json:"widgetLines,omitempty"`
	// WidgetPlacement is "aboveEditor" or "belowEditor".
	WidgetPlacement string `json:"widgetPlacement,omitempty"`

	Text string `json:"text,omitempty"`
}

// Action asks the host to act on the session.
type Action struct {
	Type FrameType `json:"type"`
	ID   string    `json:"id"`
	// Method is one of: sendMessage, setSessionName, getSessionName, exec,
	// getActiveTools, setActiveTools, getModel, setModel, getThinkingLevel,
	// setThinkingLevel, registerTool, unregisterTool, emit.
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Reply answers a UIRequest or an Action.
type Reply struct {
	Type    FrameType       `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Cancelled reports that the user dismissed a dialog. It is distinct from
	// an error: nothing went wrong, the answer is simply absent.
	Cancelled bool   `json:"cancelled,omitempty"`
	Error     string `json:"error,omitempty"`
}

// --- action payloads ---

// SendMessageParams delivers a message into the conversation.
type SendMessageParams struct {
	Message json.RawMessage `json:"message"`
	// DeliverAs is "steer", "followUp", or empty for the next turn.
	DeliverAs string `json:"deliverAs,omitempty"`
}

// ExecParams runs a shell command in the session's environment.
type ExecParams struct {
	Command string `json:"command"`
}

// ExecResult is the outcome of an Exec action. A non-zero exit code is data,
// not an error — the same contract the built-in bash tool has.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
}

// NameParams carries a single name, for setSessionName.
type NameParams struct {
	Name string `json:"name"`
}

// ToolNamesParams carries a tool-name list, for setActiveTools.
type ToolNamesParams struct {
	Names []string `json:"names"`
}

// ModelParams selects a model by provider and id.
type ModelParams struct {
	Provider string `json:"provider"`
	ModelID  string `json:"modelId"`
}

// ThinkingParams sets the thinking level.
type ThinkingParams struct {
	Level string `json:"level"`
}

// RegisterToolParams adds a tool after the handshake. An MCP server that
// announces tools/list_changed mid-session needs this; so does any extension
// whose tool set depends on something it learns later.
type RegisterToolParams struct {
	Tool ToolDecl `json:"tool"`
}

// --- reply payloads ---

// UIValue answers select, input, and editor. Pi's client sends exactly this
// shape back, so a client written against Pi needs no translation.
type UIValue struct {
	Value string `json:"value"`
}

// UIConfirmed answers confirm.
type UIConfirmed struct {
	Confirmed bool `json:"confirmed"`
}

// ModelInfo describes the active model in an action reply.
type ModelInfo struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	// ContextWindow and MaxTokens let an extension budget its own additions
	// to the context without asking the host to do the arithmetic.
	ContextWindow int  `json:"contextWindow,omitempty"`
	MaxTokens     int  `json:"maxTokens,omitempty"`
	Reasoning     bool `json:"reasoning,omitempty"`
}

// StringsResult answers the actions that return a list of names.
type StringsResult struct {
	Values []string `json:"values,omitempty"`
}

// StringResult answers the actions that return one string.
type StringResult struct {
	Value string `json:"value,omitempty"`
}

// --- result payloads ---

// CompletionItem is one autocomplete suggestion.
type CompletionItem struct {
	Value       string `json:"value"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// CompletionsResult answers a Completions request.
type CompletionsResult struct {
	Items []CompletionItem `json:"items,omitempty"`
}

// RenderResult answers a Render request. Lines may carry ANSI styling; the
// host does not re-wrap them.
type RenderResult struct {
	Lines []string `json:"lines,omitempty"`
}

// ToolResultPayload answers a ToolExecute request.
type ToolResultPayload struct {
	// Output is the text handed back to the model.
	Output string `json:"output"`
	// Details is arbitrary structured data kept in the session entry and
	// offered to renderers. It never reaches the model.
	Details any `json:"details,omitempty"`
	// IsError marks a failed execution. Following the same contract as an
	// in-process tool, it is a result rather than a transport failure: the
	// model sees the message and can react.
	IsError bool `json:"isError,omitempty"`
}
