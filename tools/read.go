package tools

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env"
	"github.com/ihavespoons/tau/ai"
)

// ReadParams are the read tool's arguments.
type ReadParams struct {
	Path   string `json:"path" jsonschema:"Path to the file to read (relative or absolute)"`
	Offset int    `json:"offset,omitempty" jsonschema:"Line number to start reading from (1-indexed)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of lines to read"`
}

// ReadDetails is the read tool's structured detail payload.
type ReadDetails struct {
	Truncation *Truncation `json:"truncation,omitempty"`
}

var readDescription = fmt.Sprintf(
	"Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). "+
		"Images are sent as attachments. For text files, output is truncated to %d lines or %dKB "+
		"(whichever is hit first). Use offset/limit for large files. When you need the full file, "+
		"continue with offset until complete.",
	DefaultMaxLines, DefaultMaxBytes/1024)

// Read builds the read tool against an environment.
//
// Output is the file's raw text — Pi does not prefix line numbers, and adding
// them would corrupt the model's picture of the file's actual bytes.
func Read(e env.Env) agent.Tool {
	return agent.MustNew("read", "read", readDescription,
		func(ctx context.Context, _ string, p ReadParams, _ agent.UpdateFunc) (agent.ToolResult, error) {
			if p.Path == "" {
				return agent.ToolResult{}, fmt.Errorf("path is required")
			}

			res, err := e.Read(ctx, p.Path, env.ReadOptions{
				Offset: p.Offset, Limit: p.Limit, MaxBytes: DefaultMaxBytes,
			})
			if err != nil {
				return agent.ToolResult{}, err
			}

			if res.MimeType != "" {
				return agent.ToolResult{
					Content: ai.ContentList{
						ai.TextContent{Text: fmt.Sprintf("Read image file [%s]", res.MimeType)},
						ai.ImageContent{
							Data:     base64.StdEncoding.EncodeToString(res.Bytes),
							MimeType: res.MimeType,
						},
					},
				}, nil
			}

			if res.Binary {
				return agent.ToolResult{}, fmt.Errorf(
					"%s is a binary file and cannot be read as text", p.Path)
			}

			// The env already applied offset/limit; re-run truncation here so
			// the model gets Pi's actionable continuation notice.
			if p.Offset > 0 && p.Offset > res.TotalLines {
				return agent.ToolResult{}, fmt.Errorf(
					"offset %d is beyond end of file (%d lines total)", p.Offset, res.TotalLines)
			}

			startLine := 1
			if p.Offset > 0 {
				startLine = p.Offset
			}
			trunc := TruncateHead(res.Content, TruncateOptions{})

			var out string
			var details *ReadDetails
			switch {
			case trunc.FirstLineExceedsLimit:
				out = fmt.Sprintf(
					"[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
					startLine, FormatSize(len(res.Content)), FormatSize(DefaultMaxBytes),
					startLine, p.Path, DefaultMaxBytes)
				details = &ReadDetails{Truncation: &trunc}
			case trunc.Truncated:
				endLine := startLine + trunc.OutputLines - 1
				next := endLine + 1
				out = trunc.Content
				if trunc.TruncatedBy == "lines" {
					out += fmt.Sprintf(
						"\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]",
						startLine, endLine, res.TotalLines, next)
				} else {
					out += fmt.Sprintf(
						"\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]",
						startLine, endLine, res.TotalLines, FormatSize(DefaultMaxBytes), next)
				}
				details = &ReadDetails{Truncation: &trunc}
			case res.Truncated:
				// A user-supplied limit stopped early with more file left.
				endLine := startLine + trunc.OutputLines - 1
				remaining := res.TotalLines - endLine
				if remaining > 0 {
					out = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]",
						trunc.Content, remaining, endLine+1)
				} else {
					out = trunc.Content
				}
			default:
				out = trunc.Content
			}

			result := agent.Text("%s", out)
			if details != nil {
				result.Details = details
			}
			return result, nil
		})
}
