package acp

import "encoding/json"

// ProtocolVersion is the ACP version tau implements. It is an integer and is
// only bumped for breaking changes; everything else is negotiated through
// capabilities.
const ProtocolVersion = 1

// Method names, spelled as the schema's meta.json spells them. They are
// constants rather than literals because a typo in one is a method the client
// silently never calls.
const (
	MethodInitialize   = "initialize"
	MethodAuthenticate = "authenticate"
	MethodSessionNew   = "session/new"
	MethodSessionLoad  = "session/load"
	MethodPrompt       = "session/prompt"
	MethodCancel       = "session/cancel"

	// Sent by the agent, not received.
	MethodSessionUpdate = "session/update"
)

// StopReason values, from the schema's StopReason enum.
const (
	StopEndTurn         = "end_turn"
	StopMaxTokens       = "max_tokens"
	StopMaxTurnRequests = "max_turn_requests"
	StopRefusal         = "refusal"
	StopCancelled       = "cancelled"
)

// SessionUpdate discriminators, from the schema's SessionUpdate union.
const (
	UpdateUserMessageChunk  = "user_message_chunk"
	UpdateAgentMessageChunk = "agent_message_chunk"
	UpdateAgentThoughtChunk = "agent_thought_chunk"
	UpdateToolCall          = "tool_call"
	UpdateToolCallUpdate    = "tool_call_update"
)

// ToolCallStatus values.
const (
	ToolPending    = "pending"
	ToolInProgress = "in_progress"
	ToolCompleted  = "completed"
	ToolFailed     = "failed"
)

// ContentBlock is one piece of a message. Only the text variant is produced
// here; the schema also defines image, audio, resource_link and resource, and
// baseline support is required for text and resource_link alone.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Data and MimeType carry an image, which tau accepts inbound.
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// Implementation names a side of the connection.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// PromptCapabilities says which ContentBlock variants a client may send.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// AgentCapabilities is what tau can do. Everything defaults to false, which is
// the honest answer for a capability that is not wired.
type AgentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities PromptCapabilities `json:"promptCapabilities"`
}

// InitializeRequest opens the connection.
type InitializeRequest struct {
	ProtocolVersion    int             `json:"protocolVersion"`
	ClientCapabilities json.RawMessage `json:"clientCapabilities,omitempty"`
	ClientInfo         *Implementation `json:"clientInfo,omitempty"`
}

// InitializeResponse answers with what tau supports.
type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	AuthMethods       []json.RawMessage `json:"authMethods"`
}

// NewSessionRequest asks for a conversation. cwd is required and must be
// absolute; the protocol says so of every path it carries.
type NewSessionRequest struct {
	Cwd                   string            `json:"cwd"`
	McpServers            []json.RawMessage `json:"mcpServers"`
	AdditionalDirectories []string          `json:"additionalDirectories,omitempty"`
}

// NewSessionResponse hands back the id every later request carries.
type NewSessionResponse struct {
	SessionID string `json:"sessionId"`
}

// PromptRequest is a user turn.
type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResponse ends the turn and says why.
type PromptResponse struct {
	StopReason string `json:"stopReason"`
}

// CancelNotification asks for the current turn to stop. It is a notification:
// there is no response, and the agent confirms by answering the in-flight
// session/prompt with the cancelled stop reason.
type CancelNotification struct {
	SessionID string `json:"sessionId"`
}

// SessionNotification carries everything that happens during a turn.
type SessionNotification struct {
	SessionID string      `json:"sessionId"`
	Update    interface{} `json:"update"`
}

// ContentChunkUpdate is a piece of a message: agent text, agent thinking, or
// the user's own turn echoed back.
type ContentChunkUpdate struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
	MessageID     string       `json:"messageId,omitempty"`
}

// ToolCallUpdate reports a tool starting, progressing, or finishing.
//
// The first report for a call carries sessionUpdate "tool_call" and everything
// known about it; later ones carry "tool_call_update" and only what changed,
// which is what lets a client render a row and then fill it in.
type ToolCallUpdate struct {
	SessionUpdate string            `json:"sessionUpdate"`
	ToolCallID    string            `json:"toolCallId"`
	Title         string            `json:"title,omitempty"`
	Status        string            `json:"status,omitempty"`
	Content       []ToolCallContent `json:"content,omitempty"`
	RawInput      map[string]any    `json:"rawInput,omitempty"`
	RawOutput     json.RawMessage   `json:"rawOutput,omitempty"`
}

// ToolCallContent is what a tool produced. The schema also defines diff and
// terminal variants; tau reports its output as content.
type ToolCallContent struct {
	Type    string       `json:"type"`
	Content ContentBlock `json:"content,omitempty"`
}
