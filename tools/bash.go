package tools

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env"
	"github.com/ihavespoons/tau/ai"
)

// BashParams are the bash tool's arguments.
type BashParams struct {
	Command string  `json:"command" jsonschema:"Bash command to execute"`
	Timeout float64 `json:"timeout,omitempty" jsonschema:"Timeout in seconds (optional, no default timeout)"`
}

// BashDetails is the bash tool's structured detail payload.
type BashDetails struct {
	Truncation     *Truncation `json:"truncation,omitempty"`
	FullOutputPath string      `json:"fullOutputPath,omitempty"`
	ExitCode       int         `json:"exitCode"`
	DurationMS     int64       `json:"durationMs"`
}

var bashDescription = fmt.Sprintf(
	"Execute a bash command in the current working directory. Returns stdout and stderr. "+
		"Output is truncated to last %d lines or %dKB (whichever is hit first). If truncated, "+
		"full output is saved to a temp file. Optionally provide a timeout in seconds.",
	DefaultMaxLines, DefaultMaxBytes/1024)

// bashUpdateThrottle bounds how often streaming updates are emitted, matching
// Pi's BASH_UPDATE_THROTTLE_MS.
const bashUpdateThrottle = 100 * time.Millisecond

// maxTimeoutSeconds mirrors Pi's ceiling (Node's max 32-bit timer).
const maxTimeoutSeconds = 2147483.647

// Bash builds the bash tool against an environment.
//
// A non-zero exit is an *error* at the tool level even though env.Exec treats
// it as data: the model needs a failed command to read as failure. Output
// truncation keeps the tail, since errors and results land at the end.
func Bash(e env.Env) agent.Tool {
	return agent.MustNew("bash", "bash", bashDescription,
		func(ctx context.Context, _ string, p BashParams, update agent.UpdateFunc) (agent.ToolResult, error) {
			if p.Command == "" {
				return agent.ToolResult{}, fmt.Errorf("command is required")
			}
			var timeout time.Duration
			if p.Timeout != 0 {
				if p.Timeout < 0 || p.Timeout > maxTimeoutSeconds {
					return agent.ToolResult{}, fmt.Errorf(
						"invalid timeout: must be a positive number of seconds up to %.0f", maxTimeoutSeconds)
				}
				timeout = time.Duration(p.Timeout * float64(time.Second))
			}

			// Streaming updates are throttled and serialized: env.Exec calls
			// OnOutput from the stdout/stderr pump goroutines.
			var (
				mu         sync.Mutex
				buf        []byte
				lastUpdate time.Time
			)
			onOutput := func(chunk string) {
				if update == nil {
					return
				}
				mu.Lock()
				buf = append(buf, chunk...)
				if time.Since(lastUpdate) < bashUpdateThrottle {
					mu.Unlock()
					return
				}
				lastUpdate = time.Now()
				snapshot := TruncateTail(string(buf), TruncateOptions{}).Content
				mu.Unlock()
				update(agent.ToolResult{Content: ai.ContentList{ai.TextContent{Text: snapshot}}})
			}

			res, err := e.Exec(ctx, p.Command, env.ExecOptions{
				Timeout:        timeout,
				MaxOutputBytes: DefaultMaxBytes,
				OnOutput:       onOutput,
			})
			if err != nil {
				return agent.ToolResult{}, err
			}

			trunc := TruncateTail(res.Output, TruncateOptions{})
			text := trunc.Content
			details := &BashDetails{
				ExitCode:   res.ExitCode,
				DurationMS: res.Duration.Milliseconds(),
			}
			if res.Truncated || trunc.Truncated {
				merged := trunc
				merged.Truncated = true
				details.Truncation = &merged
				details.FullOutputPath = res.FullOutputPath

				start := merged.TotalLines - merged.OutputLines + 1
				end := merged.TotalLines
				switch {
				case merged.LastLinePartial:
					text += fmt.Sprintf("\n\n[Showing last %s of line %d. Full output: %s]",
						FormatSize(merged.OutputBytes), end, res.FullOutputPath)
				case merged.TruncatedBy == "lines":
					text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]",
						start, end, merged.TotalLines, res.FullOutputPath)
				default:
					text += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]",
						start, end, merged.TotalLines, FormatSize(DefaultMaxBytes), res.FullOutputPath)
				}
			}

			appendStatus := func(base, status string) string {
				if base == "" {
					return status
				}
				return base + "\n\n" + status
			}

			switch {
			case res.Cancelled:
				return agent.ToolResult{}, fmt.Errorf("%s", appendStatus(text, "Command aborted"))
			case res.TimedOut:
				return agent.ToolResult{}, fmt.Errorf("%s",
					appendStatus(text, fmt.Sprintf("Command timed out after %g seconds", p.Timeout)))
			case res.ExitCode != 0:
				return agent.ToolResult{}, fmt.Errorf("%s",
					appendStatus(text, fmt.Sprintf("Command exited with code %d", res.ExitCode)))
			}

			if text == "" {
				text = "(no output)"
			}
			result := agent.Text("%s", text)
			result.Details = details
			return result, nil
		})
}
