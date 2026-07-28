package tools

import (
	"context"
	"fmt"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env"
)

// WriteParams are the write tool's arguments.
type WriteParams struct {
	Path    string `json:"path" jsonschema:"Path to the file to write (relative or absolute)"`
	Content string `json:"content" jsonschema:"Content to write to the file"`
}

const writeDescription = "Write content to a file. Creates the file if it doesn't exist, " +
	"overwrites if it does. Automatically creates parent directories."

// Write builds the write tool against an environment. Writes are serialized
// per path so a concurrent edit cannot interleave with them.
func Write(e env.Env) agent.Tool {
	return agent.MustNew("write", "write", writeDescription,
		func(ctx context.Context, _ string, p WriteParams, _ agent.UpdateFunc) (agent.ToolResult, error) {
			if p.Path == "" {
				return agent.ToolResult{}, fmt.Errorf("path is required")
			}
			abs, err := e.Abs(p.Path)
			if err != nil {
				return agent.ToolResult{}, err
			}

			var result agent.ToolResult
			err = WithFileMutation(abs, func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := e.Write(ctx, p.Path, []byte(p.Content)); err != nil {
					return err
				}
				result = agent.Text("Successfully wrote %d bytes to %s", len(p.Content), p.Path)
				return nil
			})
			if err != nil {
				return agent.ToolResult{}, err
			}
			return result, nil
		})
}
