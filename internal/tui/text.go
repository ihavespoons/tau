package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// wrapBreakpoints are the characters wrapping may break after when a word is
// longer than the line. Paths and identifiers dominate a coding transcript,
// so breaking at separators keeps long arguments readable.
const wrapBreakpoints = " -/\\_.,;:"

// wrapANSI wraps a possibly styled line to width, returning display lines.
// Styling survives the wrap: escape sequences are reopened on each line.
func wrapANSI(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	if s == "" {
		return []string{""}
	}
	return strings.Split(ansi.Wrap(s, width, wrapBreakpoints), "\n")
}

// wrapBlock wraps every line of a multi-line string.
func wrapBlock(s string, width int) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, wrapANSI(line, width)...)
	}
	return out
}

// displayWidth is the rendered column count of a styled string.
func displayWidth(s string) int { return ansi.StringWidth(s) }

// truncateCells shortens a styled string to width columns, appending an
// ellipsis when it had to cut.
func truncateCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if displayWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// oneLine flattens whitespace so a value can sit on a single status or
// activity line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// lastLines returns the final n lines, prefixed with an elision marker when
// content was dropped.
func lastLines(lines []string, n int, marker string) []string {
	if n <= 0 {
		return nil
	}
	if len(lines) <= n {
		return lines
	}
	out := make([]string, 0, n)
	out = append(out, marker)
	return append(out, lines[len(lines)-n+1:]...)
}
