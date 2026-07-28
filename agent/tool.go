// Package agent is tau's agent runtime — the port of Pi's
// @earendil-works/pi-agent-core. It owns the turn loop, the event protocol,
// and the tool contract.
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/ai"
)

// ExecutionMode controls whether a tool may run concurrently with others in
// the same assistant message.
type ExecutionMode string

const (
	// ExecutionParallel is the default: prepare sequentially, execute concurrently.
	ExecutionParallel ExecutionMode = "parallel"
	// ExecutionSequential forces the whole batch to run one at a time.
	ExecutionSequential ExecutionMode = "sequential"
)

// ToolResult is the outcome of one tool execution.
type ToolResult struct {
	// Content is what the model sees: text and images.
	Content ai.ContentList `json:"content"`
	// Details is arbitrary structured data for logs and UI rendering. It is
	// not sent to the model.
	Details any `json:"details,omitempty"`
	// Usage from the tool execution itself (e.g. a sub-agent tool). Not part
	// of main context accounting.
	Usage *ai.Usage `json:"usage,omitempty"`
	// AddedToolNames are tools that became available from this point onward.
	AddedToolNames []string `json:"addedToolNames,omitempty"`
	// Terminate hints that the agent should stop after this batch. The loop
	// only terminates when every finalized result in the batch sets it.
	Terminate bool `json:"terminate,omitempty"`
}

// Text builds a text-only tool result.
func Text(format string, args ...any) ToolResult {
	s := format
	if len(args) > 0 {
		s = fmt.Sprintf(format, args...)
	}
	return ToolResult{Content: ai.ContentList{ai.TextContent{Text: s}}}
}

// UpdateFunc streams partial results during execution. Calls made after
// Execute returns are ignored.
type UpdateFunc func(ToolResult)

// ToolDef is a tool's LLM-facing definition plus runtime metadata.
type ToolDef struct {
	Name        string
	Description string
	// Label is a human-readable name for UI display.
	Label string
	// Parameters is the JSON Schema for the tool's arguments.
	Parameters *jsonschema.Schema
	// ExecutionMode overrides the loop's default for this tool.
	ExecutionMode ExecutionMode
	// ConstrainedSampling optionally requests provider-side output constraints.
	ConstrainedSampling *ai.ConstrainedSampling
}

// Tool is an executable tool.
//
// Contract: Execute reports failure by returning an error. The loop converts
// it into an error tool result the model can read and retry — a tool must not
// encode failures in Content itself (Pi's throw-on-failure contract).
type Tool interface {
	Def() ToolDef
	Execute(ctx context.Context, callID string, args json.RawMessage, update UpdateFunc) (ToolResult, error)
}

// PrepareArguments is an optional escape hatch for tools that must massage
// raw model output before schema validation.
type PrepareArguments interface {
	PrepareArguments(raw json.RawMessage) (json.RawMessage, error)
}

// AITool renders a tool definition for the provider request.
func AITool(t Tool) ai.Tool {
	d := t.Def()
	return ai.Tool{
		Name:                d.Name,
		Description:         d.Description,
		Parameters:          d.Parameters,
		ConstrainedSampling: d.ConstrainedSampling,
	}
}

// AITools renders a tool set for the provider request.
func AITools(tools []Tool) []ai.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ai.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, AITool(t))
	}
	return out
}

// FindTool returns the tool with the given name, or nil.
func FindTool(tools []Tool, name string) Tool {
	for _, t := range tools {
		if t.Def().Name == name {
			return t
		}
	}
	return nil
}

// typedTool adapts a typed execute function to the Tool interface.
type typedTool[P any] struct {
	def ToolDef
	fn  func(ctx context.Context, callID string, params P, update UpdateFunc) (ToolResult, error)
}

func (t *typedTool[P]) Def() ToolDef { return t.def }

func (t *typedTool[P]) Execute(ctx context.Context, callID string, args json.RawMessage, update UpdateFunc) (ToolResult, error) {
	var params P
	if len(args) > 0 {
		if err := json.Unmarshal(args, &params); err != nil {
			return ToolResult{}, fmt.Errorf("invalid arguments for %s: %w", t.def.Name, err)
		}
	}
	if err := Validate(t.def.Parameters, args); err != nil {
		return ToolResult{}, err
	}
	return t.fn(ctx, callID, params, update)
}

// New builds a Tool from a typed execute function, inferring the parameter
// schema from P.
func New[P any](name, label, description string, fn func(ctx context.Context, callID string, params P, update UpdateFunc) (ToolResult, error)) (Tool, error) {
	schema, err := jsonschema.For[P](nil)
	if err != nil {
		return nil, fmt.Errorf("tool %s: deriving schema: %w", name, err)
	}
	return &typedTool[P]{
		def: ToolDef{Name: name, Label: label, Description: description, Parameters: schema},
		fn:  fn,
	}, nil
}

// MustNew is New, panicking on schema derivation failure. Intended for
// package-level tool definitions.
func MustNew[P any](name, label, description string, fn func(ctx context.Context, callID string, params P, update UpdateFunc) (ToolResult, error)) Tool {
	t, err := New(name, label, description, fn)
	if err != nil {
		panic(err)
	}
	return t
}

// Validate checks raw arguments against a schema. A nil schema accepts
// anything, mirroring Pi's behavior for tools registered without one.
func Validate(schema *jsonschema.Schema, args json.RawMessage) error {
	if schema == nil {
		return nil
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("resolving schema: %w", err)
	}
	var v any
	if len(args) == 0 {
		v = map[string]any{}
	} else if err := json.Unmarshal(args, &v); err != nil {
		return fmt.Errorf("invalid JSON arguments: %w", err)
	}
	if err := resolved.Validate(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
