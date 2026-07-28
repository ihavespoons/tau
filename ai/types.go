// Package ai is a unified multi-provider LLM API — the tau port of Pi's
// @earendil-works/pi-ai (snapshot v0.82.1). Message and event shapes marshal
// to the exact JSON wire format Pi uses, so session files interoperate.
package ai

import (
	"encoding/json"
	"fmt"
)

// Api identifies a wire API implementation (e.g. "anthropic-messages").
type Api = string

// Known wire APIs.
const (
	ApiOpenAICompletions    Api = "openai-completions"
	ApiMistralConversations Api = "mistral-conversations"
	ApiOpenAIResponses      Api = "openai-responses"
	ApiAzureOpenAIResponses Api = "azure-openai-responses"
	ApiOpenAICodexResponses Api = "openai-codex-responses"
	ApiAnthropicMessages    Api = "anthropic-messages"
	ApiBedrockConverse      Api = "bedrock-converse-stream"
	ApiGoogleGenerativeAI   Api = "google-generative-ai"
	ApiGoogleVertex         Api = "google-vertex"
	ApiPiMessages           Api = "pi-messages"
)

// ProviderId identifies a provider (e.g. "anthropic", "openrouter").
type ProviderId = string

// ThinkingLevel is a pi reasoning-effort level (never "off").
type ThinkingLevel string

// ModelThinkingLevel extends ThinkingLevel with "off".
type ModelThinkingLevel string

const (
	ThinkingOff     ModelThinkingLevel = "off"
	ThinkingMinimal ThinkingLevel      = "minimal"
	ThinkingLow     ThinkingLevel      = "low"
	ThinkingMedium  ThinkingLevel      = "medium"
	ThinkingHigh    ThinkingLevel      = "high"
	ThinkingXHigh   ThinkingLevel      = "xhigh"
	ThinkingMax     ThinkingLevel      = "max"
)

// ThinkingLevelMap maps pi thinking levels to provider/model-specific values.
// A key mapped to nil (JSON null) marks the level as unsupported; a missing
// key uses the provider default. Mirrors Pi's Partial<Record<..., string|null>>.
type ThinkingLevelMap map[ModelThinkingLevel]*string

// ThinkingBudgets holds token budgets per thinking level (token-based providers only).
type ThinkingBudgets struct {
	Minimal *int `json:"minimal,omitempty"`
	Low     *int `json:"low,omitempty"`
	Medium  *int `json:"medium,omitempty"`
	High    *int `json:"high,omitempty"`
}

// StopReason mirrors Pi's StopReason union.
type StopReason string

const (
	StopPending StopReason = "pending"
	StopStop    StopReason = "stop"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "toolUse"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

// Content is a message content block: TextContent, ThinkingContent,
// ImageContent, or ToolCall.
type Content interface {
	ContentType() string
}

// TextContent is a text block.
type TextContent struct {
	Text string `json:"text"`
	// TextSignature carries provider metadata (e.g. OpenAI responses item ids).
	TextSignature string `json:"textSignature,omitempty"`
}

// ThinkingContent is a reasoning block.
type ThinkingContent struct {
	Thinking          string `json:"thinking"`
	ThinkingSignature string `json:"thinkingSignature,omitempty"`
	// Redacted marks thinking redacted by safety filters; the opaque payload
	// lives in ThinkingSignature for multi-turn continuity.
	Redacted bool `json:"redacted,omitempty"`
}

// ImageContent is a base64-encoded image block.
type ImageContent struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// ToolCall is an assistant tool invocation.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	// ThoughtSignature is Google-specific: opaque signature for reusing thought context.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

func (TextContent) ContentType() string     { return "text" }
func (ThinkingContent) ContentType() string { return "thinking" }
func (ImageContent) ContentType() string    { return "image" }
func (ToolCall) ContentType() string        { return "toolCall" }

// MarshalJSON implementations force the "type" discriminator so a zero-value
// struct can never serialize without one.

func (c TextContent) MarshalJSON() ([]byte, error) {
	type alias TextContent
	return json.Marshal(struct {
		Type string `json:"type"`
		alias
	}{"text", alias(c)})
}

func (c ThinkingContent) MarshalJSON() ([]byte, error) {
	type alias ThinkingContent
	return json.Marshal(struct {
		Type string `json:"type"`
		alias
	}{"thinking", alias(c)})
}

func (c ImageContent) MarshalJSON() ([]byte, error) {
	type alias ImageContent
	return json.Marshal(struct {
		Type string `json:"type"`
		alias
	}{"image", alias(c)})
}

func (c ToolCall) MarshalJSON() ([]byte, error) {
	type alias ToolCall
	return json.Marshal(struct {
		Type string `json:"type"`
		alias
	}{"toolCall", alias(c)})
}

// UnmarshalContent decodes a single content block by its "type" discriminator.
func UnmarshalContent(raw json.RawMessage) (Content, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	switch probe.Type {
	case "text":
		var c TextContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	case "thinking":
		var c ThinkingContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	case "image":
		var c ImageContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	case "toolCall":
		var c ToolCall
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("ai: unknown content type %q", probe.Type)
	}
}

// ContentList is a JSON-polymorphic slice of content blocks.
type ContentList []Content

func (l *ContentList) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	out := make(ContentList, 0, len(raws))
	for _, raw := range raws {
		c, err := UnmarshalContent(raw)
		if err != nil {
			return err
		}
		out = append(out, c)
	}
	*l = out
	return nil
}

// UserContent is Pi's `string | (TextContent | ImageContent)[]`. When Blocks
// is nil it marshals as a plain JSON string.
type UserContent struct {
	Text   string
	Blocks ContentList
}

// String flattens the content to text (blocks joined by newline, images skipped).
func (u UserContent) String() string {
	if u.Blocks == nil {
		return u.Text
	}
	s := ""
	for _, b := range u.Blocks {
		if t, ok := b.(TextContent); ok {
			if s != "" {
				s += "\n"
			}
			s += t.Text
		}
	}
	return s
}

func (u UserContent) MarshalJSON() ([]byte, error) {
	if u.Blocks == nil {
		return json.Marshal(u.Text)
	}
	return json.Marshal(u.Blocks)
}

func (u *UserContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*u = UserContent{Text: s}
		return nil
	}
	var blocks ContentList
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	*u = UserContent{Blocks: blocks}
	return nil
}

// Cost is the dollar cost breakdown of a Usage.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// Usage is token and cost accounting for one assistant response.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	// CacheWrite1h is the subset of CacheWrite written with 1h retention.
	// Only Anthropic reports this split.
	CacheWrite1h *int `json:"cacheWrite1h,omitempty"`
	// Reasoning tokens, when the provider reports them. A subset of Output.
	Reasoning   *int `json:"reasoning,omitempty"`
	TotalTokens int  `json:"totalTokens"`
	Cost        Cost `json:"cost"`
}

// DiagnosticErrorInfo mirrors Pi's redacted error info.
type DiagnosticErrorInfo struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	Code    any    `json:"code,omitempty"`
}

// Diagnostic is a redacted provider/runtime diagnostic attached to an
// AssistantMessage on failures and recoveries.
type Diagnostic struct {
	Type      string               `json:"type"`
	Timestamp int64                `json:"timestamp"`
	Error     *DiagnosticErrorInfo `json:"error,omitempty"`
	Details   map[string]any       `json:"details,omitempty"`
}

// UserMessage is a user turn.
type UserMessage struct {
	Content   UserContent `json:"content"`
	Timestamp int64       `json:"timestamp"` // unix ms
}

// AssistantMessage is an assistant turn. Content holds TextContent,
// ThinkingContent, and ToolCall blocks.
type AssistantMessage struct {
	Content       ContentList  `json:"content"`
	Api           Api          `json:"api"`
	Provider      ProviderId   `json:"provider"`
	Model         string       `json:"model"`
	ResponseModel string       `json:"responseModel,omitempty"`
	ResponseID    string       `json:"responseId,omitempty"`
	Diagnostics   []Diagnostic `json:"diagnostics,omitempty"`
	Usage         Usage        `json:"usage"`
	StopReason    StopReason   `json:"stopReason"`
	ErrorMessage  string       `json:"errorMessage,omitempty"`
	Timestamp     int64        `json:"timestamp"` // unix ms
}

// ToolResultMessage is the result of executing a tool call.
type ToolResultMessage struct {
	ToolCallID string      `json:"toolCallId"`
	ToolName   string      `json:"toolName"`
	Content    ContentList `json:"content"` // text and images
	Details    any         `json:"details,omitempty"`
	// Usage from the tool execution itself; not part of main context accounting.
	Usage *Usage `json:"usage,omitempty"`
	// AddedToolNames are names from Context.Tools that became available after
	// this result (deferred tool loading).
	AddedToolNames []string `json:"addedToolNames,omitempty"`
	IsError        bool     `json:"isError"`
	Timestamp      int64    `json:"timestamp"` // unix ms
}

// Message is a UserMessage, AssistantMessage, or ToolResultMessage.
type Message interface {
	Role() string
}

func (UserMessage) Role() string       { return "user" }
func (AssistantMessage) Role() string  { return "assistant" }
func (ToolResultMessage) Role() string { return "toolResult" }

// The role discriminator is injected/stripped by the marshal helpers below so
// the struct definitions stay free of a mutable Role field.

func (m UserMessage) MarshalJSON() ([]byte, error) {
	type alias UserMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{"user", alias(m)})
}

func (m AssistantMessage) MarshalJSON() ([]byte, error) {
	type alias AssistantMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{"assistant", alias(m)})
}

func (m ToolResultMessage) MarshalJSON() ([]byte, error) {
	type alias ToolResultMessage
	return json.Marshal(struct {
		Role string `json:"role"`
		alias
	}{"toolResult", alias(m)})
}

// UnmarshalMessage decodes a message by its "role" discriminator.
func UnmarshalMessage(raw json.RawMessage) (Message, error) {
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	switch probe.Role {
	case "user":
		var m UserMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case "assistant":
		var m AssistantMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	case "toolResult":
		var m ToolResultMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		return m, nil
	default:
		return nil, fmt.Errorf("ai: unknown message role %q", probe.Role)
	}
}

// MessageList is a JSON-polymorphic slice of messages.
type MessageList []Message

func (l *MessageList) UnmarshalJSON(data []byte) error {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return err
	}
	out := make(MessageList, 0, len(raws))
	for _, raw := range raws {
		m, err := UnmarshalMessage(raw)
		if err != nil {
			return err
		}
		out = append(out, m)
	}
	*l = out
	return nil
}

// Context is the LLM request context.
type Context struct {
	SystemPrompt string      `json:"systemPrompt,omitempty"`
	Messages     MessageList `json:"messages"`
	Tools        []Tool      `json:"tools,omitempty"`
}
