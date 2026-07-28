package session

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ihavespoons/tau/ai"
)

// Synthetic-message prefixes and suffixes, applied when flattening session
// messages for the model. Copied verbatim from Pi's harness/messages.ts —
// note the branch suffix has no leading newline while the compaction one does.
const (
	CompactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	CompactionSummarySuffix = "\n</summary>"
	BranchSummaryPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	BranchSummarySuffix     = "</summary>"
)

// Message roles beyond the three the provider layer defines.
const (
	RoleBashExecution     = "bashExecution"
	RoleCustom            = "custom"
	RoleBranchSummary     = "branchSummary"
	RoleCompactionSummary = "compactionSummary"
)

// MessageDecoder builds a message from its raw JSON.
type MessageDecoder func(raw json.RawMessage) (ai.Message, error)

var (
	decodersMu sync.RWMutex
	decoders   = map[string]MessageDecoder{}
)

// RegisterMessageDecoder teaches the session loader about a message role
// beyond the built-ins. The coding layer uses this to persist its own message
// types without the session package depending on it.
//
// Registering a role that already has a decoder replaces it. Safe for
// concurrent use, though registration normally happens at init.
func RegisterMessageDecoder(role string, fn MessageDecoder) {
	decodersMu.Lock()
	defer decodersMu.Unlock()
	decoders[role] = fn
}

func lookupDecoder(role string) (MessageDecoder, bool) {
	decodersMu.RLock()
	defer decodersMu.RUnlock()
	fn, ok := decoders[role]
	return fn, ok
}

func init() {
	RegisterMessageDecoder(RoleBashExecution, func(raw json.RawMessage) (ai.Message, error) {
		var m BashExecutionMessage
		return &m, json.Unmarshal(raw, &m)
	})
	RegisterMessageDecoder(RoleCustom, func(raw json.RawMessage) (ai.Message, error) {
		var m CustomMessage
		return &m, json.Unmarshal(raw, &m)
	})
	RegisterMessageDecoder(RoleBranchSummary, func(raw json.RawMessage) (ai.Message, error) {
		var m BranchSummaryMessage
		return &m, json.Unmarshal(raw, &m)
	})
	RegisterMessageDecoder(RoleCompactionSummary, func(raw json.RawMessage) (ai.Message, error) {
		var m CompactionSummaryMessage
		return &m, json.Unmarshal(raw, &m)
	})
}

// BashExecutionMessage records a shell command the user ran directly.
type BashExecutionMessage struct {
	Command            string `json:"command"`
	Output             string `json:"output"`
	ExitCode           *int   `json:"exitCode"`
	Cancelled          bool   `json:"cancelled"`
	Truncated          bool   `json:"truncated"`
	FullOutputPath     string `json:"fullOutputPath,omitempty"`
	ExcludeFromContext bool   `json:"excludeFromContext,omitempty"`
	Timestamp          int64  `json:"timestamp"`
}

// CustomMessage is an extension-authored message that participates in context.
type CustomMessage struct {
	CustomType string         `json:"customType"`
	Content    ai.UserContent `json:"content"`
	Display    bool           `json:"display"`
	Details    any            `json:"details,omitempty"`
	Timestamp  int64          `json:"timestamp"`
}

// BranchSummaryMessage summarizes a branch the conversation returned from.
type BranchSummaryMessage struct {
	Summary   string `json:"summary"`
	FromID    string `json:"fromId"`
	Timestamp int64  `json:"timestamp"`
}

// CompactionSummaryMessage summarizes history replaced by a compaction.
type CompactionSummaryMessage struct {
	Summary      string `json:"summary"`
	TokensBefore int    `json:"tokensBefore"`
	Timestamp    int64  `json:"timestamp"`
}

// OpaqueMessage preserves a message whose role this build does not recognize.
// It replays its source bytes on marshal so the session round-trips intact.
type OpaqueMessage struct {
	RoleName string
	Raw      json.RawMessage
}

func (*BashExecutionMessage) Role() string     { return RoleBashExecution }
func (*CustomMessage) Role() string            { return RoleCustom }
func (*BranchSummaryMessage) Role() string     { return RoleBranchSummary }
func (*CompactionSummaryMessage) Role() string { return RoleCompactionSummary }
func (m *OpaqueMessage) Role() string          { return m.RoleName }

func marshalMessage(role string, v any) ([]byte, error) {
	return withDiscriminator("role", role, v)
}

func (m *BashExecutionMessage) MarshalJSON() ([]byte, error) {
	type alias BashExecutionMessage
	return marshalMessage(RoleBashExecution, alias(*m))
}

func (m *CustomMessage) MarshalJSON() ([]byte, error) {
	type alias CustomMessage
	return marshalMessage(RoleCustom, alias(*m))
}

func (m *BranchSummaryMessage) MarshalJSON() ([]byte, error) {
	type alias BranchSummaryMessage
	return marshalMessage(RoleBranchSummary, alias(*m))
}

func (m *CompactionSummaryMessage) MarshalJSON() ([]byte, error) {
	type alias CompactionSummaryMessage
	return marshalMessage(RoleCompactionSummary, alias(*m))
}

func (m *OpaqueMessage) MarshalJSON() ([]byte, error) {
	if len(m.Raw) == 0 {
		return nil, fmt.Errorf("session: opaque message %q has no source bytes", m.RoleName)
	}
	return m.Raw, nil
}

// decodeAgentMessage decodes a message by role: registered decoders first,
// then the provider-layer roles. An unknown role yields an OpaqueMessage and a
// non-nil error, so the caller can keep the message and still report it.
func decodeAgentMessage(raw json.RawMessage) (ai.Message, error) {
	var probe struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return &OpaqueMessage{Raw: append(json.RawMessage(nil), raw...)},
			fmt.Errorf("message is not a JSON object: %w", err)
	}
	if fn, ok := lookupDecoder(probe.Role); ok {
		msg, err := fn(raw)
		if err != nil {
			return &OpaqueMessage{RoleName: probe.Role, Raw: append(json.RawMessage(nil), raw...)},
				fmt.Errorf("decoding %q message: %w", probe.Role, err)
		}
		return msg, nil
	}
	switch probe.Role {
	case "user", "assistant", "toolResult":
		msg, err := ai.UnmarshalMessage(raw)
		if err != nil {
			return &OpaqueMessage{RoleName: probe.Role, Raw: append(json.RawMessage(nil), raw...)},
				fmt.Errorf("decoding %q message: %w", probe.Role, err)
		}
		return msg, nil
	default:
		return &OpaqueMessage{RoleName: probe.Role, Raw: append(json.RawMessage(nil), raw...)},
			fmt.Errorf("unknown message role %q", probe.Role)
	}
}

// BashExecutionText renders a bash execution for the model.
func BashExecutionText(m *BashExecutionMessage) string {
	text := fmt.Sprintf("Ran `%s`\n", m.Command)
	if m.Output != "" {
		text += fmt.Sprintf("```\n%s\n```", m.Output)
	} else {
		text += "(no output)"
	}
	if m.Cancelled {
		text += "\n\n(command cancelled)"
	} else if m.ExitCode != nil && *m.ExitCode != 0 {
		text += fmt.Sprintf("\n\nCommand exited with code %d", *m.ExitCode)
	}
	if m.Truncated && m.FullOutputPath != "" {
		text += fmt.Sprintf("\n\n[Output truncated. Full output: %s]", m.FullOutputPath)
	}
	return text
}

// ConvertToLLM flattens session messages into the three roles a provider
// accepts. Synthetic roles become user messages; messages that carry no model
// context (an excluded bash run, an unknown role) are dropped.
func ConvertToLLM(messages []ai.Message) []ai.Message {
	out := make([]ai.Message, 0, len(messages))
	for _, m := range messages {
		switch msg := m.(type) {
		case *BashExecutionMessage:
			if msg.ExcludeFromContext {
				continue
			}
			out = append(out, ai.UserMessage{
				Content:   ai.UserContent{Blocks: ai.ContentList{ai.TextContent{Text: BashExecutionText(msg)}}},
				Timestamp: msg.Timestamp,
			})
		case *CustomMessage:
			out = append(out, ai.UserMessage{Content: msg.Content, Timestamp: msg.Timestamp})
		case *BranchSummaryMessage:
			out = append(out, ai.UserMessage{
				Content: ai.UserContent{Blocks: ai.ContentList{
					ai.TextContent{Text: BranchSummaryPrefix + msg.Summary + BranchSummarySuffix},
				}},
				Timestamp: msg.Timestamp,
			})
		case *CompactionSummaryMessage:
			out = append(out, ai.UserMessage{
				Content: ai.UserContent{Blocks: ai.ContentList{
					ai.TextContent{Text: CompactionSummaryPrefix + msg.Summary + CompactionSummarySuffix},
				}},
				Timestamp: msg.Timestamp,
			})
		case ai.UserMessage, ai.AssistantMessage, ai.ToolResultMessage:
			out = append(out, msg)
		case *ai.UserMessage:
			out = append(out, *msg)
		case *ai.AssistantMessage:
			out = append(out, *msg)
		case *ai.ToolResultMessage:
			out = append(out, *msg)
		default:
			// Unknown roles carry no model context.
			continue
		}
	}
	return out
}
