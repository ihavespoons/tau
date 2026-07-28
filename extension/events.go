// Package extension is tau's in-process extension API — the Go port of Pi's
// ExtensionAPI. Extensions observe and shape the agent's behavior through 33
// typed events, and register tools, commands, and renderers.
//
// The composition rules differ per event and are ported from Pi's runner:
// some chain, some accumulate, some take the first or last non-nil result,
// and some short-circuit. Getting these wrong is silent and subtle, so each
// policy is documented on its emitter in runner.go and pinned by a test.
package extension

import (
	"encoding/json"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
)

// EventType names an extension hook.
type EventType string

const (
	EventProjectTrust     EventType = "project_trust"
	EventResourcesDiscover EventType = "resources_discover"

	EventSessionStart         EventType = "session_start"
	EventSessionInfoChanged   EventType = "session_info_changed"
	EventSessionBeforeSwitch  EventType = "session_before_switch"
	EventSessionBeforeFork    EventType = "session_before_fork"
	EventSessionBeforeCompact EventType = "session_before_compact"
	EventSessionCompact       EventType = "session_compact"
	EventSessionShutdown      EventType = "session_shutdown"
	EventSessionBeforeTree    EventType = "session_before_tree"
	EventSessionTree          EventType = "session_tree"

	EventContext               EventType = "context"
	EventBeforeProviderRequest EventType = "before_provider_request"
	EventBeforeProviderHeaders EventType = "before_provider_headers"
	EventAfterProviderResponse EventType = "after_provider_response"

	EventBeforeAgentStart EventType = "before_agent_start"
	EventAgentStart       EventType = "agent_start"
	EventAgentEnd         EventType = "agent_end"
	EventAgentSettled     EventType = "agent_settled"
	EventTurnStart        EventType = "turn_start"
	EventTurnEnd          EventType = "turn_end"

	EventMessageStart  EventType = "message_start"
	EventMessageUpdate EventType = "message_update"
	EventMessageEnd    EventType = "message_end"

	EventToolExecutionStart  EventType = "tool_execution_start"
	EventToolExecutionUpdate EventType = "tool_execution_update"
	EventToolExecutionEnd    EventType = "tool_execution_end"

	EventModelSelect         EventType = "model_select"
	EventThinkingLevelSelect EventType = "thinking_level_select"

	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventUserBash   EventType = "user_bash"
	EventInput      EventType = "input"
)

// AllEventTypes lists every hook, in Pi's declaration order.
var AllEventTypes = []EventType{
	EventProjectTrust, EventResourcesDiscover,
	EventSessionStart, EventSessionInfoChanged, EventSessionBeforeSwitch,
	EventSessionBeforeFork, EventSessionBeforeCompact, EventSessionCompact,
	EventSessionShutdown, EventSessionBeforeTree, EventSessionTree,
	EventContext, EventBeforeProviderRequest, EventBeforeProviderHeaders,
	EventAfterProviderResponse, EventBeforeAgentStart, EventAgentStart,
	EventAgentEnd, EventAgentSettled, EventTurnStart, EventTurnEnd,
	EventMessageStart, EventMessageUpdate, EventMessageEnd,
	EventToolExecutionStart, EventToolExecutionUpdate, EventToolExecutionEnd,
	EventModelSelect, EventThinkingLevelSelect,
	EventToolCall, EventToolResult, EventUserBash, EventInput,
}

// --- trust & resources ---

// TrustDecision is an extension's vote on whether a project is trusted.
type TrustDecision string

const (
	TrustYes       TrustDecision = "yes"
	TrustNo        TrustDecision = "no"
	TrustUndecided TrustDecision = "undecided"
)

// ProjectTrustEvent asks whether the project directory should be trusted.
type ProjectTrustEvent struct {
	Cwd string `json:"cwd"`
}

// ProjectTrustResult carries a trust vote. Undecided falls through to the
// next extension, then to tau's own trust store.
type ProjectTrustResult struct {
	Decision TrustDecision `json:"decision"`
	Reason   string        `json:"reason,omitempty"`
}

// ResourcesDiscoverEvent asks extensions to contribute resource paths.
type ResourcesDiscoverEvent struct {
	Cwd    string `json:"cwd"`
	Reason string `json:"reason"` // "startup" | "reload"
}

// ResourcesDiscoverResult contributes skill/prompt/theme directories.
type ResourcesDiscoverResult struct {
	SkillPaths  []string `json:"skillPaths,omitempty"`
	PromptPaths []string `json:"promptPaths,omitempty"`
	ThemePaths  []string `json:"themePaths,omitempty"`
}

// OwnedPath is a discovered path tagged with the extension that supplied it.
type OwnedPath struct {
	Path      string `json:"path"`
	Extension string `json:"extension"`
}

// DiscoveredResources aggregates every extension's contributions.
type DiscoveredResources struct {
	SkillPaths  []OwnedPath `json:"skillPaths"`
	PromptPaths []OwnedPath `json:"promptPaths"`
	ThemePaths  []OwnedPath `json:"themePaths"`
}

// --- session lifecycle ---

// SessionStartEvent fires once the session is open and bound.
type SessionStartEvent struct {
	SessionPath string `json:"sessionPath,omitempty"`
	Cwd         string `json:"cwd"`
	Resumed     bool   `json:"resumed"`
}

// SessionInfoChangedEvent fires when session metadata changes (e.g. name).
type SessionInfoChangedEvent struct {
	Name string `json:"name,omitempty"`
}

// SessionBeforeSwitchEvent precedes switching to another session file.
type SessionBeforeSwitchEvent struct {
	TargetPath string `json:"targetPath"`
}

// SessionBeforeForkEvent precedes forking the session.
type SessionBeforeForkEvent struct {
	EntryID string `json:"entryId,omitempty"`
}

// SessionBeforeCompactEvent precedes compaction.
type SessionBeforeCompactEvent struct {
	CustomInstructions string `json:"customInstructions,omitempty"`
}

// SessionBeforeTreeEvent precedes tree navigation.
type SessionBeforeTreeEvent struct {
	TargetID string `json:"targetId,omitempty"`
}

// SessionBeforeResult can cancel a pending session operation. It is shared by
// all four session_before_* events.
type SessionBeforeResult struct {
	Cancel bool   `json:"cancel,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// SessionCompactEvent fires after compaction completes.
type SessionCompactEvent struct {
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore"`
}

// SessionTreeEvent fires after tree navigation completes.
type SessionTreeEvent struct {
	TargetID string `json:"targetId"`
}

// SessionShutdownEvent fires as the session closes.
type SessionShutdownEvent struct {
	Reason string `json:"reason"` // "exit" | "reload" | "switch"
}

// --- provider & context ---

// ContextEvent offers the message list before it becomes a provider request.
type ContextEvent struct {
	Messages []ai.Message `json:"messages"`
}

// ContextResult replaces the message list.
type ContextResult struct {
	Messages []ai.Message `json:"messages,omitempty"`
}

// BeforeProviderRequestEvent exposes the raw provider payload.
type BeforeProviderRequestEvent struct {
	Payload any `json:"payload"`
}

// BeforeProviderRequestResult replaces the payload.
type BeforeProviderRequestResult struct {
	Payload any `json:"payload,omitempty"`
}

// BeforeProviderHeadersEvent exposes the outgoing headers for in-place
// mutation. A nil map value deletes a header.
type BeforeProviderHeadersEvent struct {
	Headers map[string]*string `json:"headers"`
}

// AfterProviderResponseEvent reports the provider's response metadata.
type AfterProviderResponseEvent struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
}

// --- agent lifecycle ---

// BeforeAgentStartEvent precedes a run and can inject messages or replace the
// system prompt.
type BeforeAgentStartEvent struct {
	Prompt       string           `json:"prompt"`
	Images       []ai.ImageContent `json:"images,omitempty"`
	SystemPrompt string           `json:"systemPrompt"`
}

// BeforeAgentStartResult injects a message and/or replaces the system prompt.
// Messages accumulate across extensions; the system prompt chains.
type BeforeAgentStartResult struct {
	Message      ai.Message `json:"message,omitempty"`
	SystemPrompt *string    `json:"systemPrompt,omitempty"`
}

// BeforeAgentStartCombined is the aggregate of every extension's contribution.
type BeforeAgentStartCombined struct {
	Messages     []ai.Message
	SystemPrompt *string
}

// AgentStartEvent fires when a run begins.
type AgentStartEvent struct{}

// AgentEndEvent fires when the loop finishes.
type AgentEndEvent struct {
	Messages []ai.Message `json:"messages"`
}

// AgentSettledEvent fires once the run and its listeners have settled.
type AgentSettledEvent struct{}

// TurnStartEvent fires at the start of each turn.
type TurnStartEvent struct{}

// TurnEndEvent fires after a turn's tool calls complete.
type TurnEndEvent struct {
	Message     ai.Message             `json:"message"`
	ToolResults []ai.ToolResultMessage `json:"toolResults,omitempty"`
}

// --- messages ---

// MessageStartEvent fires when a message begins.
type MessageStartEvent struct {
	Message ai.Message `json:"message"`
}

// MessageUpdateEvent fires for each streaming delta.
type MessageUpdateEvent struct {
	Message     ai.Message `json:"message"`
	StreamEvent *ai.Event  `json:"streamEvent,omitempty"`
}

// MessageEndEvent fires when a message is final and may replace it.
type MessageEndEvent struct {
	Message ai.Message `json:"message"`
}

// MessageEndResult replaces the message. The replacement must keep the same
// role; a mismatch is reported as an extension error and ignored.
type MessageEndResult struct {
	Message ai.Message `json:"message,omitempty"`
}

// --- tool execution ---

// ToolExecutionStartEvent fires as a tool begins.
type ToolExecutionStartEvent struct {
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Args       map[string]any `json:"args,omitempty"`
}

// ToolExecutionUpdateEvent carries a streamed partial result.
type ToolExecutionUpdateEvent struct {
	ToolCallID    string             `json:"toolCallId"`
	ToolName      string             `json:"toolName"`
	Args          map[string]any     `json:"args,omitempty"`
	PartialResult *agent.ToolResult  `json:"partialResult,omitempty"`
}

// ToolExecutionEndEvent fires when a tool finishes.
type ToolExecutionEndEvent struct {
	ToolCallID string            `json:"toolCallId"`
	ToolName   string            `json:"toolName"`
	Result     *agent.ToolResult `json:"result,omitempty"`
	IsError    bool              `json:"isError"`
}

// ToolCallEvent fires after argument validation and before execution. Args is
// mutable: writing to it patches the arguments the tool receives, and later
// handlers observe earlier edits.
type ToolCallEvent struct {
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Args       map[string]any `json:"args"`
	Raw        json.RawMessage `json:"-"`
}

// ToolCallResult can block a tool call.
type ToolCallResult struct {
	Block  bool   `json:"block,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ToolResultEvent fires after execution and before the result is recorded.
type ToolResultEvent struct {
	ToolCallID string         `json:"toolCallId"`
	ToolName   string         `json:"toolName"`
	Content    ai.ContentList `json:"content,omitempty"`
	Details    any            `json:"details,omitempty"`
	IsError    bool           `json:"isError"`
	Usage      *ai.Usage      `json:"usage,omitempty"`
}

// ToolResultResult patches a tool result field-wise. Nil fields keep their
// current value, and later handlers see earlier patches.
type ToolResultResult struct {
	Content ai.ContentList `json:"content,omitempty"`
	Details any            `json:"details,omitempty"`
	IsError *bool          `json:"isError,omitempty"`
	Usage   *ai.Usage      `json:"usage,omitempty"`
}

// --- selection & input ---

// ModelSelectEvent fires when the active model changes.
type ModelSelectEvent struct {
	Model *ai.Model `json:"model"`
}

// ThinkingLevelSelectEvent fires when the thinking level changes.
type ThinkingLevelSelectEvent struct {
	Level ai.ModelThinkingLevel `json:"level"`
}

// UserBashEvent fires for a user-issued `!` shell command.
type UserBashEvent struct {
	Command string `json:"command"`
}

// UserBashResult replaces the command's execution. The first non-nil result
// wins.
type UserBashResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
}

// InputAction is what an input handler decided.
type InputAction string

const (
	// InputContinue passes the input through unchanged.
	InputContinue InputAction = "continue"
	// InputTransform rewrites the input for downstream handlers.
	InputTransform InputAction = "transform"
	// InputHandled consumes the input entirely, short-circuiting.
	InputHandled InputAction = "handled"
)

// InputEvent fires for user input before it becomes a prompt.
type InputEvent struct {
	Text   string            `json:"text"`
	Images []ai.ImageContent `json:"images,omitempty"`
	Source string            `json:"source"` // "tui" | "cli" | "rpc" | "extension"
	// StreamingBehavior is "steer" or "followUp" when input arrives mid-run.
	StreamingBehavior string `json:"streamingBehavior,omitempty"`
}

// InputResult is an input handler's decision.
type InputResult struct {
	Action InputAction       `json:"action"`
	Text   string            `json:"text,omitempty"`
	Images []ai.ImageContent `json:"images,omitempty"`
}
