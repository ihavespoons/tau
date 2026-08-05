// Package rpc is the protocol for `tau --mode rpc`: a headless tau driven over
// stdio by another program.
//
// # Framing
//
// LF-only JSONL, in both directions. Commands arrive on stdin, one JSON value
// per line; responses and events go out on stdout the same way. Nothing but
// U+000A terminates a record — a payload string may legally contain U+2028 or
// U+2029, and a client that splits on those will tear a record in half.
//
// # Shapes are Pi's
//
// The command names, response envelopes, and extension-UI request shapes are
// taken verbatim from Pi's rpc-types.ts. A client written against Pi drives tau
// without a translation layer, which is the whole point of having a protocol
// rather than an API.
package rpc

import (
	"encoding/json"

	"github.com/ihavespoons/tau/ai"
)

// Command is one line on stdin.
//
// It is a single struct rather than a union because Go has no unions and a
// per-command type would need a two-pass decode anyway: the fields are
// disjoint by command, and a field that does not apply is simply absent.
type Command struct {
	// ID correlates a command with its response. A command without one still
	// runs; its response just cannot be matched to it.
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`

	// Prompting.
	Message string            `json:"message,omitempty"`
	Images  []ai.ImageContent `json:"images,omitempty"`
	// StreamingBehavior is "steer" or "followUp" when a prompt arrives while
	// the agent is already running.
	StreamingBehavior string `json:"streamingBehavior,omitempty"`
	ParentSession     string `json:"parentSession,omitempty"`

	// Model and thinking.
	Provider string `json:"provider,omitempty"`
	ModelID  string `json:"modelId,omitempty"`
	Level    string `json:"level,omitempty"`

	// Queue modes: "all" or "one-at-a-time".
	Mode string `json:"mode,omitempty"`

	// Compaction.
	CustomInstructions string `json:"customInstructions,omitempty"`
	// Enabled is a pointer so that `false` is distinguishable from absent:
	// set_auto_compaction with no field is a malformed command, not a request
	// to turn it off.
	Enabled *bool `json:"enabled,omitempty"`

	// Bash.
	Command            string `json:"command,omitempty"`
	ExcludeFromContext bool   `json:"excludeFromContext,omitempty"`

	// Session.
	SessionPath string `json:"sessionPath,omitempty"`
	EntryID     string `json:"entryId,omitempty"`
	Since       string `json:"since,omitempty"`
	OutputPath  string `json:"outputPath,omitempty"`
	Name        string `json:"name,omitempty"`
}

// Response is a command's acknowledgement.
type Response struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"` // always "response"
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// SessionState answers get_state.
type SessionState struct {
	Model                 *ModelInfo `json:"model,omitempty"`
	ThinkingLevel         string     `json:"thinkingLevel"`
	IsStreaming           bool       `json:"isStreaming"`
	IsCompacting          bool       `json:"isCompacting"`
	SteeringMode          string     `json:"steeringMode"`
	FollowUpMode          string     `json:"followUpMode"`
	SessionFile           string     `json:"sessionFile,omitempty"`
	SessionID             string     `json:"sessionId"`
	SessionName           string     `json:"sessionName,omitempty"`
	AutoCompactionEnabled bool       `json:"autoCompactionEnabled"`
	MessageCount          int        `json:"messageCount"`
	PendingMessageCount   int        `json:"pendingMessageCount"`
}

// ModelInfo describes a model in a response.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Provider      string `json:"provider"`
	API           string `json:"api,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	MaxTokens     int    `json:"maxTokens,omitempty"`
	Reasoning     bool   `json:"reasoning,omitempty"`
}

// SlashCommand describes a command available for invocation via prompt.
type SlashCommand struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Source is "builtin", "extension", "prompt", or "skill".
	Source     string `json:"source"`
	SourceInfo string `json:"sourceInfo,omitempty"`
}

// BashResult answers the bash command.
type BashResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	// Aborted reports that the command was cut short rather than finishing.
	Aborted bool `json:"aborted,omitempty"`
}

// CompactionResult answers compact.
type CompactionResult struct {
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore"`
	TokensAfter  int    `json:"tokensAfter"`
	Cancelled    bool   `json:"cancelled,omitempty"`
}

// CancelledResult is the answer to the session operations an extension may
// veto. Cancellation is an outcome, not a failure: an extension declining a
// fork is the system working.
type CancelledResult struct {
	Cancelled bool   `json:"cancelled"`
	Reason    string `json:"reason,omitempty"`
}

// TreeNode is one entry in the get_tree answer.
type TreeNode struct {
	ID       string     `json:"id"`
	ParentID string     `json:"parentId,omitempty"`
	Kind     string     `json:"kind"`
	Label    string     `json:"label,omitempty"`
	Summary  string     `json:"summary,omitempty"`
	Children []TreeNode `json:"children,omitempty"`
}

// ForkMessage is one place a fork can start from.
type ForkMessage struct {
	EntryID string `json:"entryId"`
	Text    string `json:"text"`
}

// --- events (stdout) ---

// Event is anything tau emits that is not a response to a command.
//
// Every event carries `type`, and a client is expected to ignore types it does
// not know: new event types are added over time and a client that fails on an
// unrecognized one breaks on upgrade.
type Event struct {
	Type string `json:"type"`

	Message   json.RawMessage `json:"message,omitempty"`
	Delta     string          `json:"delta,omitempty"`
	DeltaKind string          `json:"deltaKind,omitempty"`

	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       map[string]any  `json:"args,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`

	Usage *ai.Usage `json:"usage,omitempty"`
	Error string    `json:"error,omitempty"`

	SessionPath string `json:"sessionPath,omitempty"`
	Model       string `json:"model,omitempty"`

	// ExtensionPath and Event name an extension failure.
	ExtensionPath string `json:"extensionPath,omitempty"`
	EventName     string `json:"event,omitempty"`
}

// ExtensionUIRequest is emitted when an extension needs the user.
//
// The field names are Pi's RpcExtensionUIRequest, verbatim. The same shapes are
// used by the subprocess extension protocol, which is what lets an extension's
// dialog reach a client through two hops without either end translating.
type ExtensionUIRequest struct {
	Type   string `json:"type"` // always "extension_ui_request"
	ID     string `json:"id"`
	Method string `json:"method"`

	Title       string   `json:"title,omitempty"`
	Message     string   `json:"message,omitempty"`
	Options     []string `json:"options,omitempty"`
	Timeout     int      `json:"timeout,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Prefill     string   `json:"prefill,omitempty"`
	NotifyType  string   `json:"notifyType,omitempty"`

	StatusKey  string  `json:"statusKey,omitempty"`
	StatusText *string `json:"statusText,omitempty"`

	WidgetKey       string   `json:"widgetKey,omitempty"`
	WidgetLines     []string `json:"widgetLines,omitempty"`
	WidgetPlacement string   `json:"widgetPlacement,omitempty"`

	Text string `json:"text,omitempty"`
}

// ExtensionUIResponse is the client's answer, arriving on stdin.
//
// Cancelled is how a client declines without it being an error: the user
// dismissed the dialog, and the extension is told so rather than being handed
// a value nobody chose.
type ExtensionUIResponse struct {
	Type      string `json:"type"` // always "extension_ui_response"
	ID        string `json:"id"`
	Value     string `json:"value,omitempty"`
	Confirmed *bool  `json:"confirmed,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}
