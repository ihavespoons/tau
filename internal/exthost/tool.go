package exthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
	"github.com/ihavespoons/tau/extension/wire"
)

// Deadlines for the two request kinds that happen on a latency-sensitive path.
const (
	// completionTimeout bounds an argument-completion request. It fires while
	// the user is typing; a slow extension must not make the editor feel
	// stuck, so a missed deadline simply yields no suggestions.
	completionTimeout = 500 * time.Millisecond
	// renderTimeout bounds a renderer request. A missed deadline falls back to
	// the built-in rendering rather than leaving a gap in the transcript.
	renderTimeout = 100 * time.Millisecond
)

// wireTool is a tool whose implementation lives in another process.
type wireTool struct {
	h    *Host
	decl wire.ToolDecl
	def  agent.ToolDef
}

func (h *Host) newWireTool(d wire.ToolDecl) agent.Tool {
	t := &wireTool{h: h, decl: d}
	t.def = agent.ToolDef{
		Name:        d.Name,
		Description: d.Description,
		Label:       d.Name,
		Parameters:  parseSchema(d.Parameters),
	}
	return t
}

// parseSchema decodes a declared JSON Schema.
//
// A tool whose schema will not parse still gets registered, with an empty
// object schema. Refusing to register it would mean the model never sees a
// tool the extension believes it published, and the failure would surface as
// a mystery — the tool simply not existing — instead of as a provider
// complaint naming the tool.
func parseSchema(raw json.RawMessage) *jsonschema.Schema {
	if len(raw) == 0 {
		return &jsonschema.Schema{Type: "object"}
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return &jsonschema.Schema{Type: "object"}
	}
	if s.Type == "" && s.Properties == nil {
		return &jsonschema.Schema{Type: "object"}
	}
	return &s
}

func (t *wireTool) Def() agent.ToolDef { return t.def }

// Execute runs the tool in the extension's process.
//
// A returned error becomes an isError result upstream, which is the same
// contract an in-process tool has and the same one Pi gives a TS tool that
// throws. That makes a failed tool and an unreachable extension look alike to
// the model, which is correct: in both cases the tool did not work, and the
// message says which it was.
func (t *wireTool) Execute(ctx context.Context, callID string, args json.RawMessage, update agent.UpdateFunc) (agent.ToolResult, error) {
	id := t.h.nextRequestID()
	stop := t.h.watchToolUpdates(id, update)
	defer stop()

	res, err := t.h.request(ctx, id, wire.ToolExecute{
		Type: wire.FrameToolExecute, ID: id, Generation: t.h.generation.Load(),
		Tool: t.decl.Name, CallID: callID, Args: args,
	})
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("tool %s: %w", t.decl.Name, err)
	}
	if res.Error != "" {
		return agent.ToolResult{}, errors.New(res.Error)
	}

	var p wire.ToolResultPayload
	if len(res.Payload) > 0 {
		if err := json.Unmarshal(res.Payload, &p); err != nil {
			return agent.ToolResult{}, fmt.Errorf("tool %s: undecodable result: %w", t.decl.Name, err)
		}
	}
	if p.IsError {
		return agent.ToolResult{}, errors.New(p.Output)
	}
	return toolResultFrom(p), nil
}

func toolResultFrom(p wire.ToolResultPayload) agent.ToolResult {
	return agent.ToolResult{
		Content: ai.ContentList{ai.TextContent{Text: p.Output}},
		Details: p.Details,
	}
}
