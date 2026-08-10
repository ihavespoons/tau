package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
)

// renderer turns messages and tool activity into the lines that get flushed
// into the terminal's scrollback.
//
// Everything printed here leaves tau's render tree for good, which is what
// keeps a long session cheap: a completed message is formatted exactly once,
// no matter how far the conversation runs.
type renderer struct {
	theme Theme
	md    *markdown
	width int
	// hideThinking mirrors the hideThinkingBlock setting.
	hideThinking bool
	// toolOutputLines caps how much of a tool result is shown inline.
	toolOutputLines int
}

func newRenderer(theme Theme, width int, hideThinking bool) *renderer {
	return &renderer{
		theme: theme, md: newMarkdown(width), width: width,
		hideThinking: hideThinking, toolOutputLines: 12,
	}
}

func (r *renderer) setWidth(w int) {
	r.width = w
	r.md.setWidth(w)
}

// toggleThinking flips thinking-block visibility for the rest of the session,
// reporting the new hidden state. It does not touch settings: this is the
// "let me see what it was reasoning about just now" key, not a preference
// change, and Pi treats it the same way.
//
// Only messages rendered after the toggle are affected. What has already been
// flushed into scrollback is the terminal's, not tau's, and reprinting the
// transcript to reveal thinking blocks would cost the very thing inline
// rendering buys.
func (r *renderer) toggleThinking() bool {
	r.hideThinking = !r.hideThinking
	return r.hideThinking
}

// user renders a submitted prompt.
func (r *renderer) user(text string) []string {
	lines := wrapBlock(strings.TrimRight(text, "\n"), r.width-2)
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		gutter := r.theme.User.Render("› ")
		if i > 0 {
			gutter = "  "
		}
		out = append(out, gutter+l)
	}
	return out
}

// assistant renders a completed assistant message: thinking, prose, and the
// tool calls it decided to make.
func (r *renderer) assistant(m ai.AssistantMessage) []string {
	var out []string

	for _, c := range m.Content {
		switch b := c.(type) {
		case ai.ThinkingContent:
			if r.hideThinking || b.Thinking == "" {
				continue
			}
			for _, l := range wrapBlock(b.Thinking, r.width-2) {
				out = append(out, r.theme.Thinking.Render("  "+l))
			}
			out = append(out, "")
		case ai.TextContent:
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			out = append(out, r.md.render(b.Text)...)
		}
	}

	if m.StopReason == ai.StopError || m.StopReason == ai.StopAborted {
		out = append(out, r.errorLine(m))
	}
	return out
}

// errorLine describes a failed or aborted turn.
func (r *renderer) errorLine(m ai.AssistantMessage) string {
	msg := m.ErrorMessage
	if msg == "" {
		msg = string(m.StopReason)
	}
	if m.StopReason == ai.StopAborted {
		return r.theme.Warning.Render("⨯ stopped: " + oneLine(msg))
	}
	return r.theme.Error.Render("⨯ " + oneLine(msg))
}

// toolCall renders the header line for a tool invocation.
func (r *renderer) toolCall(name string, args map[string]any) string {
	head := r.theme.ToolName.Render("· " + name)
	if summary := summarizeArgs(name, args); summary != "" {
		head += " " + r.theme.ToolArgs.Render(truncateCells(summary, max(10, r.width-len(name)-6)))
	}
	return head
}

// toolResult renders a finished tool's output, indented under its call and
// clipped to a readable height. The session file keeps the full text.
func (r *renderer) toolResult(res *agent.ToolResult, isError bool) []string {
	text := resultText(res)
	if strings.TrimSpace(text) == "" {
		if isError {
			return []string{r.theme.Error.Render("  ↳ failed")}
		}
		return nil
	}

	lines := wrapBlock(strings.TrimRight(text, "\n"), r.width-4)
	clipped := len(lines) - r.toolOutputLines
	if clipped > 0 {
		lines = lines[:r.toolOutputLines]
	}

	style := r.theme.ToolOut
	if isError {
		style = r.theme.Error
	}
	out := make([]string, 0, len(lines)+1)
	for _, l := range lines {
		out = append(out, "  "+style.Render(l))
	}
	if clipped > 0 {
		out = append(out, "  "+r.theme.Dim.Render(fmt.Sprintf("… %d more lines", clipped)))
	}
	return out
}

// notice renders a host message: command output, warnings, errors.
func (r *renderer) notice(text string, style func(...string) string) []string {
	var out []string
	for _, l := range wrapBlock(text, r.width) {
		out = append(out, style(l))
	}
	return out
}

// resultText flattens a tool result's text blocks.
func resultText(res *agent.ToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(ai.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// argOrder is the argument each built-in tool is best identified by. Showing
// the path for a read and the command for a bash call is the difference
// between an activity log you can skim and one you cannot.
var argOrder = map[string][]string{
	"read":  {"path"},
	"write": {"path"},
	"edit":  {"path"},
	"bash":  {"command"},
	"grep":  {"pattern", "path"},
	"find":  {"pattern", "path"},
	"ls":    {"path"},
}

// summarizeArgs renders tool arguments as a single readable line.
func summarizeArgs(tool string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for _, key := range argOrder[tool] {
		if v, ok := args[key]; ok {
			parts = append(parts, oneLine(fmt.Sprint(v)))
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}

	// Unknown tool — most likely from an extension or MCP server. Show its
	// arguments in a stable order so the line does not shuffle between calls.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := oneLine(fmt.Sprint(args[k]))
		if v == "" {
			continue
		}
		parts = append(parts, k+"="+truncateCells(v, 40))
		if len(parts) == 3 {
			break
		}
	}
	return strings.Join(parts, " ")
}
