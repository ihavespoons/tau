package tools

import (
	"context"
	"encoding/json"

	"github.com/ihavespoons/tau/agent"
)

// withPrompt attaches system-prompt metadata to a tool.
//
// agent.MustNew derives a ToolDef from the parameter type and has no slot for
// PromptSnippet/PromptGuidelines, so the built-ins wrap themselves here
// rather than hand-rolling the Tool interface four times. Snippets and
// guidelines are copied verbatim from Pi's tool definitions — this text is
// what the model reads about each tool, so wording changes are behavior
// changes.
type withPrompt struct {
	inner      agent.Tool
	snippet    string
	guidelines []string
}

func promptMeta(t agent.Tool, snippet string, guidelines ...string) agent.Tool {
	return &withPrompt{inner: t, snippet: snippet, guidelines: guidelines}
}

func (w *withPrompt) Def() agent.ToolDef {
	def := w.inner.Def()
	def.PromptSnippet = w.snippet
	def.PromptGuidelines = w.guidelines
	return def
}

func (w *withPrompt) Execute(ctx context.Context, callID string, args json.RawMessage, update agent.UpdateFunc) (agent.ToolResult, error) {
	return w.inner.Execute(ctx, callID, args, update)
}

// PrepareArguments forwards to the wrapped tool when it needs one (edit).
func (w *withPrompt) PrepareArguments(raw json.RawMessage) (json.RawMessage, error) {
	if p, ok := w.inner.(agent.PrepareArguments); ok {
		return p.PrepareArguments(raw)
	}
	return raw, nil
}
