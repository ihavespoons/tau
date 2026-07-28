package extension

import (
	"context"
	"encoding/json"

	"github.com/ihavespoons/tau/agent"
)

// fakeTool is a minimal Tool for registration tests.
type fakeTool struct {
	name  string
	label string
}

func (f fakeTool) Def() agent.ToolDef { return agent.ToolDef{Name: f.name, Label: f.label} }

func (f fakeTool) Execute(context.Context, string, json.RawMessage, agent.UpdateFunc) (agent.ToolResult, error) {
	return agent.Text("ok"), nil
}
