package tui

import (
	"strings"
	"testing"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/ai"
)

func plain(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(stripANSI(l))
		b.WriteString("\n")
	}
	return b.String()
}

// stripANSI removes styling so assertions are about content, not colors.
func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case inEscape:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		case r == 0x1b:
			inEscape = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestRenderUserMessage(t *testing.T) {
	r := newRenderer(DefaultTheme(), 60, false)
	out := plain(r.user("fix the parser"))
	if !strings.Contains(out, "› fix the parser") {
		t.Errorf("user message lost its gutter:\n%s", out)
	}
}

func TestRenderAssistantText(t *testing.T) {
	r := newRenderer(DefaultTheme(), 60, false)
	out := plain(r.assistant(ai.AssistantMessage{
		Content: ai.ContentList{ai.TextContent{Text: "Here is **the** answer."}},
	}))
	if !strings.Contains(out, "the") || !strings.Contains(out, "answer") {
		t.Errorf("assistant prose was lost:\n%s", out)
	}
}

// Thinking is shown by default and suppressed when the setting says so; a
// transcript that silently leaked reasoning the user asked to hide would be a
// privacy surprise.
func TestThinkingRespectsTheSetting(t *testing.T) {
	msg := ai.AssistantMessage{
		Content: ai.ContentList{
			ai.ThinkingContent{Thinking: "SECRET REASONING"},
			ai.TextContent{Text: "the answer"},
		},
	}

	shown := plain(newRenderer(DefaultTheme(), 60, false).assistant(msg))
	if !strings.Contains(shown, "SECRET REASONING") {
		t.Errorf("thinking should render by default:\n%s", shown)
	}

	hidden := plain(newRenderer(DefaultTheme(), 60, true).assistant(msg))
	if strings.Contains(hidden, "SECRET REASONING") {
		t.Errorf("hideThinkingBlock was ignored:\n%s", hidden)
	}
	if !strings.Contains(hidden, "the answer") {
		t.Errorf("hiding thinking must not hide the answer:\n%s", hidden)
	}
}

func TestRenderErrorAndAbortedTurns(t *testing.T) {
	r := newRenderer(DefaultTheme(), 60, false)

	failed := plain(r.assistant(ai.AssistantMessage{
		StopReason: ai.StopError, ErrorMessage: "rate limited",
	}))
	if !strings.Contains(failed, "rate limited") {
		t.Errorf("a failed turn must say why:\n%s", failed)
	}

	stopped := plain(r.assistant(ai.AssistantMessage{StopReason: ai.StopAborted}))
	if !strings.Contains(stopped, "stopped") {
		t.Errorf("an aborted turn should read as stopped, not failed:\n%s", stopped)
	}
}

func TestToolCallLineNamesTheImportantArgument(t *testing.T) {
	r := newRenderer(DefaultTheme(), 80, false)

	read := stripANSI(r.toolCall("read", map[string]any{"path": "main.go", "offset": 10}))
	if !strings.Contains(read, "read") || !strings.Contains(read, "main.go") {
		t.Errorf("read should be identified by its path: %q", read)
	}
	if strings.Contains(read, "offset") {
		t.Errorf("the summary should stay focused, got %q", read)
	}

	bash := stripANSI(r.toolCall("bash", map[string]any{"command": "go test ./..."}))
	if !strings.Contains(bash, "go test ./...") {
		t.Errorf("bash should be identified by its command: %q", bash)
	}
}

// A tool tau does not know — from an extension or an MCP server — still has to
// produce a stable, readable line.
func TestUnknownToolArgsAreOrderedAndBounded(t *testing.T) {
	args := map[string]any{"zeta": "1", "alpha": "2", "mid": "3", "omega": "4"}
	first := summarizeArgs("mcp__github__search", args)
	second := summarizeArgs("mcp__github__search", args)

	if first != second {
		t.Errorf("the summary must be stable across calls: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, "alpha=") {
		t.Errorf("arguments should be ordered, got %q", first)
	}
	if strings.Count(first, "=") > 3 {
		t.Errorf("the summary should be bounded, got %q", first)
	}
}

func TestToolResultIsClippedWithACount(t *testing.T) {
	r := newRenderer(DefaultTheme(), 80, false)
	r.toolOutputLines = 3

	var lines []string
	for i := range 20 {
		lines = append(lines, "output line "+string(rune('a'+i)))
	}
	res := agent.Text("%s", strings.Join(lines, "\n"))

	out := plain(r.toolResult(&res, false))
	if !strings.Contains(out, "more lines") {
		t.Errorf("long output should report what was clipped:\n%s", out)
	}
	if strings.Count(out, "\n") > 5 {
		t.Errorf("output was not clipped:\n%s", out)
	}
}

func TestToolResultMarksErrors(t *testing.T) {
	r := newRenderer(DefaultTheme(), 80, false)
	res := agent.Text("permission denied")
	out := plain(r.toolResult(&res, true))
	if !strings.Contains(out, "permission denied") {
		t.Errorf("an error result must show its message:\n%s", out)
	}

	empty := agent.ToolResult{}
	if out := plain(r.toolResult(&empty, true)); !strings.Contains(out, "failed") {
		t.Errorf("a silent failure still needs to be visible:\n%s", out)
	}
	if out := r.toolResult(&empty, false); len(out) != 0 {
		t.Errorf("a silent success should print nothing, got %v", out)
	}
}

// Rendered output must never be wider than the terminal, or the live region
// wraps and the editor jumps around.
func TestRenderedLinesRespectWidth(t *testing.T) {
	const width = 40
	r := newRenderer(DefaultTheme(), width, false)

	long := strings.Repeat("supercalifragilistic ", 20)
	for _, lines := range [][]string{
		r.user(long),
		r.assistant(ai.AssistantMessage{Content: ai.ContentList{ai.TextContent{Text: long}}}),
	} {
		for _, l := range lines {
			if w := displayWidth(l); w > width {
				t.Errorf("line is %d columns wide, terminal is %d: %q", w, width, stripANSI(l))
			}
		}
	}
}

func TestLastLinesKeepsTheTail(t *testing.T) {
	lines := []string{"1", "2", "3", "4", "5"}
	got := lastLines(lines, 3, "…")
	if len(got) != 3 || got[0] != "…" || got[2] != "5" {
		t.Errorf("expected an elided tail, got %v", got)
	}
	if got := lastLines(lines, 10, "…"); len(got) != 5 {
		t.Errorf("a short list should pass through, got %v", got)
	}
}

func TestTruncateCells(t *testing.T) {
	if got := truncateCells("hello world", 5); displayWidth(got) > 5 {
		t.Errorf("truncation exceeded the budget: %q", got)
	}
	if got := truncateCells("short", 20); got != "short" {
		t.Errorf("a fitting string should be untouched, got %q", got)
	}
}
