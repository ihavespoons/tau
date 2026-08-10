package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// colorEnabled reports whether the terminal takes styling at all. When output
// is piped — or run under `go test` — lipgloss detects no terminal and strips
// every style, and attributes written by hand have to follow the same rule or
// they end up as escape sequences in a file meant to be plain.
func colorEnabled() bool { return lipgloss.ColorProfile() != termenv.Ascii }

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

// Terminal attributes tau sets itself rather than through lipgloss.
const (
	sgrReset        = "\x1b[0m"
	sgrUnderlineOn  = "\x1b[4m"
	sgrUnderlineOff = "\x1b[24m"
	sgrStrikeOn     = "\x1b[9m"
	sgrStrikeOff    = "\x1b[29m"
)

// underlineANSI and strikeANSI turn an attribute on for a whole string.
//
// lipgloss is not used for these two: it applies them per character, re-emitting
// the entire colour sequence for every rune, which turns a five-letter heading
// into ten escape sequences. That is size the wrapper has to parse, the block
// cache has to hold, and the terminal has to decode, on every line of every
// heading and every piece of struck-through text.
func underlineANSI(s string) string { return sgrSpan(s, sgrUnderlineOn, sgrUnderlineOff) }
func strikeANSI(s string) string    { return sgrSpan(s, sgrStrikeOn, sgrStrikeOff) }

// sgrSpan opens an attribute, re-opens it after every reset the inner styling
// emitted, and closes it at the end. Without the re-opening, the first nested
// colour run would cancel the attribute for the rest of the string.
func sgrSpan(s, on, off string) string {
	if s == "" || !colorEnabled() {
		return s
	}
	return on + strings.ReplaceAll(s, sgrReset, sgrReset+on) + off
}

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
