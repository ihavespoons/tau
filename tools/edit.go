package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env"
)

// EditParams are the edit tool's arguments.
type EditParams struct {
	Path  string `json:"path" jsonschema:"Path to the file to edit (relative or absolute)"`
	Edits []Edit `json:"edits" jsonschema:"One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead."`
}

// EditDetails is the edit tool's structured detail payload.
type EditDetails struct {
	// Diff is a display-oriented, line-numbered diff.
	Diff string `json:"diff"`
	// Patch is a standard unified patch.
	Patch string `json:"patch"`
	// FirstChangedLine locates the first change in the new file, for editors.
	FirstChangedLine int `json:"firstChangedLine,omitempty"`
}

const editDescription = "Edit a single file using exact text replacement. Every edits[].oldText " +
	"must match a unique, non-overlapping region of the original file. If two changes affect the " +
	"same block or nearby lines, merge them into one edit instead of emitting overlapping edits. " +
	"Do not include large unchanged regions just to connect distant changes."

// Edits builds the edit tool against an environment.
//
// The whole read-modify-write runs under the path's mutation lock so a
// concurrent write cannot land between the read and the write.
func Edits(e env.Env) agent.Tool {
	return &editTool{inner: agent.MustNew("edit", "edit", editDescription,
		func(ctx context.Context, _ string, p EditParams, _ agent.UpdateFunc) (agent.ToolResult, error) {
			if p.Path == "" {
				return agent.ToolResult{}, fmt.Errorf("path is required")
			}
			if len(p.Edits) == 0 {
				return agent.ToolResult{}, fmt.Errorf(
					"edit tool input is invalid. edits must contain at least one replacement")
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
				raw, err := e.ReadFile(ctx, p.Path)
				if err != nil {
					return fmt.Errorf("could not edit file: %s. %w", p.Path, err)
				}

				// Match without the BOM (the model never includes one), then
				// restore both the BOM and the file's original line endings.
				bomPrefix, content := stripBOM(string(raw))
				ending := detectLineEnding(content)
				applied, err := ApplyEdits(normalizeToLF(content), p.Edits, p.Path)
				if err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}

				final := bomPrefix + restoreLineEndings(applied.NewContent, ending)
				if err := e.Write(ctx, p.Path, []byte(final)); err != nil {
					return err
				}

				diff := GenerateDiff(applied.BaseContent, applied.NewContent)
				result = agent.Text("Successfully replaced %d block(s) in %s.", len(p.Edits), p.Path)
				result.Details = &EditDetails{
					Diff:             diff.Diff,
					Patch:            GenerateUnifiedPatch(p.Path, applied.BaseContent, applied.NewContent),
					FirstChangedLine: diff.FirstChangedLine,
				}
				return nil
			})
			if err != nil {
				return agent.ToolResult{}, err
			}
			return result, nil
		})}
}

// editTool wraps the generated tool to normalize the argument shapes models
// actually emit before schema validation runs.
type editTool struct{ inner agent.Tool }

func (t *editTool) Def() agent.ToolDef {
	def := t.inner.Def()
	// Go's nil-able slice makes the derived schema `["null","array"]`, but Pi
	// (and provider strict modes) expect a plain `array`. Fix it in place so
	// the wire schema matches.
	if def.Parameters != nil {
		if edits, ok := def.Parameters.Properties["edits"]; ok && edits.Type == "" && len(edits.Types) == 2 {
			edits.Types = nil
			edits.Type = "array"
		}
	}
	return def
}

func (t *editTool) Execute(ctx context.Context, callID string, args json.RawMessage, update agent.UpdateFunc) (agent.ToolResult, error) {
	return t.inner.Execute(ctx, callID, normalizeEditArgs(args), update)
}

// normalizeEditArgs repairs the two malformed shapes Pi handles in
// prepareEditArguments: `edits` arriving as a JSON *string* (Opus 4.6, GLM-5.1
// do this), and legacy top-level oldText/newText instead of an edits array.
// Anything it cannot repair passes through untouched for the validator to
// reject with a proper message.
func normalizeEditArgs(args json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return args
	}
	changed := false

	if raw, ok := obj["edits"]; ok {
		var asString string
		if json.Unmarshal(raw, &asString) == nil {
			var parsed []Edit
			if json.Unmarshal([]byte(asString), &parsed) == nil {
				if encoded, err := json.Marshal(parsed); err == nil {
					obj["edits"] = encoded
					changed = true
				}
			}
		}
	}

	oldRaw, hasOld := obj["oldText"]
	newRaw, hasNew := obj["newText"]
	if hasOld && hasNew {
		var oldText, newText string
		if json.Unmarshal(oldRaw, &oldText) == nil && json.Unmarshal(newRaw, &newText) == nil {
			var edits []Edit
			if raw, ok := obj["edits"]; ok {
				_ = json.Unmarshal(raw, &edits)
			}
			edits = append(edits, Edit{OldText: oldText, NewText: newText})
			if encoded, err := json.Marshal(edits); err == nil {
				obj["edits"] = encoded
				delete(obj, "oldText")
				delete(obj, "newText")
				changed = true
			}
		}
	}

	if !changed {
		return args
	}
	if encoded, err := json.Marshal(obj); err == nil {
		return encoded
	}
	return args
}
