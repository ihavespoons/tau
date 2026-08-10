package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ihavespoons/tau/theme"
)

// forceColor makes lipgloss emit escape sequences under `go test`, which has no
// terminal to detect and so strips every style. Without it, a test asserting
// that emphasis is styled would pass or fail on whether the suite happened to
// run attached to a terminal.
func forceColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// Trailing spaces are invisible on screen but come along when the transcript is
// copied, so the renderer has to strip them.
func TestRenderedMarkdownHasNoTrailingPadding(t *testing.T) {
	md := newMarkdown(60)
	for _, line := range md.render("Some text\n\n- one\n- two\n\n| a | b |\n|---|---|\n| 1 | 2 |\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line carries trailing whitespace: %q", line)
		}
	}
}

// Body text must not carry a hardcoded foreground: whichever one the renderer
// picks is wrong on half of all terminals, so prose inherits the user's own.
func TestBodyTextIsNotRecolored(t *testing.T) {
	forceColor(t)
	md := newMarkdown(60)
	lines := md.render("plain sentence with no markup")
	if len(lines) == 0 {
		t.Fatal("nothing rendered")
	}
	if joined := strings.Join(lines, "\n"); strings.Contains(joined, "\x1b[") {
		t.Errorf("unstyled prose should emit no escape sequences, got %q", joined)
	}
}

func TestEmphasisAndCodeAreStillStyled(t *testing.T) {
	forceColor(t)
	md := newMarkdown(60)
	joined := strings.Join(md.render("a **bold** word and `code`"), "\n")
	if !strings.Contains(joined, "\x1b[") {
		t.Errorf("emphasis lost its styling: %q", joined)
	}
	if plain := stripANSI(joined); !strings.Contains(plain, "bold") || !strings.Contains(plain, "code") {
		t.Errorf("emphasis lost its text: %q", plain)
	}
}

func TestRenderedMarkdownRespectsWidth(t *testing.T) {
	const width = 40
	md := newMarkdown(width)

	src := strings.Repeat("alpha beta gamma delta ", 10) +
		"\n\n- " + strings.Repeat("nested item text ", 8) +
		"\n\n> " + strings.Repeat("quoted words ", 8) +
		"\n\n| one | two |\n|---|---|\n| " + strings.Repeat("cell ", 10) + " | x |\n"

	for _, line := range md.render(src) {
		if w := displayWidth(line); w > width {
			t.Errorf("line is %d columns, wrap width is %d: %q", w, width, line)
		}
	}
}

func TestEmptyMarkdownRendersNothing(t *testing.T) {
	if got := newMarkdown(60).render("   \n\n"); len(got) != 0 {
		t.Errorf("blank input should render nothing, got %q", got)
	}
}

// The whole point of replacing glamour: a loaded theme's markdown colours have
// to actually reach the screen.
func TestMarkdownUsesTheLoadedTheme(t *testing.T) {
	forceColor(t)

	th := DefaultTheme()
	heading := th.Colors.Color(theme.MdHeading)
	if heading == "" {
		t.Fatal("the built-in theme declares no mdHeading colour")
	}

	md := newThemedMarkdown(60, th)
	out := strings.Join(md.render("# Title\n\nbody\n"), "\n")
	if !strings.Contains(out, ansiParams(t, heading)) {
		t.Errorf("the heading was not painted with the theme's mdHeading colour:\n%q", out)
	}
}

// A theme with no colours at all must render plainly rather than fall back to
// something invented by the renderer.
func TestMarkdownWithoutColorsRendersPlainly(t *testing.T) {
	forceColor(t)

	md := newThemedMarkdown(60, Theme{Colors: &theme.Theme{}})
	out := strings.Join(md.render("# Title\n\n```go\nvar x = 1\n```\n"), "\n")
	if strings.Contains(out, "38;2") {
		t.Errorf("a colourless theme produced colour: %q", out)
	}
	if !strings.Contains(stripANSI(out), "var x = 1") {
		t.Errorf("the code was lost: %q", out)
	}
}

func TestHeadingsShowTheirLevel(t *testing.T) {
	md := newMarkdown(60)

	// One and two are distinguished by weight and underline; from three down
	// the hashes are all that is left to carry depth.
	if out := stripANSI(strings.Join(md.render("## Two"), "\n")); strings.Contains(out, "#") {
		t.Errorf("a level-two heading should not show its hashes: %q", out)
	}
	if out := stripANSI(strings.Join(md.render("### Three"), "\n")); !strings.HasPrefix(out, "### Three") {
		t.Errorf("a level-three heading should show its hashes: %q", out)
	}
}

func TestCodeBlocksKeepTheirFences(t *testing.T) {
	md := newMarkdown(60)
	lines := md.render("```go\nfunc main() {}\n```\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = stripANSI(l)
	}

	if len(plain) < 3 {
		t.Fatalf("expected fences around the body, got %q", plain)
	}
	if plain[0] != "```go" {
		t.Errorf("the opening fence should name the language, got %q", plain[0])
	}
	if plain[len(plain)-1] != "```" {
		t.Errorf("the closing fence is missing, got %q", plain)
	}
	if !strings.Contains(plain[1], "func main() {}") {
		t.Errorf("the code is missing: %q", plain[1])
	}
	if !strings.HasPrefix(plain[1], codeIndent) {
		t.Errorf("code should be indented under the fence: %q", plain[1])
	}
}

func TestListsRenderMarkersAndNest(t *testing.T) {
	md := newMarkdown(60)
	out := stripANSI(strings.Join(md.render("- one\n- two\n    - deep\n\n1. first\n2. second\n"), "\n"))

	for _, want := range []string{"- one", "- two", "    - deep", "1. first", "2. second"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestTaskListsShowTheirBoxes(t *testing.T) {
	md := newMarkdown(60)
	out := stripANSI(strings.Join(md.render("- [x] done\n- [ ] todo\n"), "\n"))

	if !strings.Contains(out, "- [x] done") {
		t.Errorf("a checked box is missing:\n%s", out)
	}
	if !strings.Contains(out, "- [ ] todo") {
		t.Errorf("an unchecked box is missing:\n%s", out)
	}
}

// A wrapped list item lines up under its own text, not under its bullet.
func TestListContinuationLinesAlign(t *testing.T) {
	md := newMarkdown(30)
	lines := md.render("- " + strings.Repeat("word ", 20))
	if len(lines) < 2 {
		t.Fatalf("expected the item to wrap, got %q", lines)
	}
	if !strings.HasPrefix(stripANSI(lines[1]), "  ") {
		t.Errorf("continuation is not indented past the bullet: %q", lines[1])
	}
}

func TestBlockquotesArePrefixed(t *testing.T) {
	md := newMarkdown(60)
	for _, line := range md.render("> quoted\n> lines\n") {
		if !strings.HasPrefix(stripANSI(line), "│ ") {
			t.Errorf("quote line has no border: %q", stripANSI(line))
		}
	}
}

// A rule spanning a wide terminal reads as a page break rather than a
// separator.
func TestHorizontalRuleIsCapped(t *testing.T) {
	md := newMarkdown(200)
	lines := md.render("---\n")
	if len(lines) != 1 {
		t.Fatalf("expected one line, got %q", lines)
	}
	if w := displayWidth(lines[0]); w != hrMaxWidth {
		t.Errorf("rule is %d columns, want %d", w, hrMaxWidth)
	}
}

func TestTablesAlignColumns(t *testing.T) {
	md := newMarkdown(60)
	lines := md.render("| name | count |\n|---|---:|\n| a | 1 |\n| longer | 22 |\n")
	if len(lines) < 4 {
		t.Fatalf("expected a header, a rule and two rows, got %q", lines)
	}

	plain := stripANSI(lines[1])
	if !strings.Contains(plain, "┼") {
		t.Errorf("the header rule is missing: %q", plain)
	}
	// The right-aligned column puts its short value under the long one's end.
	if !strings.Contains(stripANSI(lines[2]), " 1") {
		t.Errorf("right alignment was not applied: %q", stripANSI(lines[2]))
	}
}

func TestLinksShowWhereTheyGo(t *testing.T) {
	md := newMarkdown(80)
	out := stripANSI(strings.Join(md.render("see [the docs](https://example.com/x)"), "\n"))

	if !strings.Contains(out, "the docs") {
		t.Errorf("link text is missing: %q", out)
	}
	if !strings.Contains(out, "https://example.com/x") {
		t.Errorf("a transcript must show where a link goes: %q", out)
	}
}

// Syntax highlighting has to come from the theme, not from chroma's own
// palette — that is the whole reason for mapping the tokens by hand.
func TestCodeHighlightingUsesTheThemesColors(t *testing.T) {
	forceColor(t)

	th := DefaultTheme()
	keyword := th.Colors.Color(theme.SyntaxKeyword)
	if keyword == "" {
		t.Fatal("the built-in theme declares no syntaxKeyword colour")
	}

	out := strings.Join(highlightCode("func main() {}", "go", th), "\n")
	if !strings.Contains(out, ansiParams(t, keyword)) {
		t.Errorf("the keyword was not painted with the theme's colour:\n%q", out)
	}
}

// An unknown language must not lose the code.
func TestCodeHighlightingSurvivesAnUnknownLanguage(t *testing.T) {
	out := strings.Join(highlightCode("some text\nmore text", "nosuchlang", DefaultTheme()), "\n")
	if stripANSI(out) != "some text\nmore text" {
		t.Errorf("code was mangled: %q", stripANSI(out))
	}
}

// Escape sequences must not span a line break: the transcript is wrapped and
// copied line by line.
func TestHighlightedCodeClosesStylesPerLine(t *testing.T) {
	forceColor(t)
	for _, line := range highlightCode("func a() {}\nfunc b() {}", "go", DefaultTheme()) {
		if strings.Contains(line, "\n") {
			t.Errorf("a highlighted line contains a newline: %q", line)
		}
	}
}

// ansiParams is the colour portion of the sequence lipgloss emits for a hex
// colour — "38;2;240;198;116" and the like. Tests match on that rather than a
// whole sequence, because the real one also carries bold or underline and their
// order is lipgloss's business, not the test's.
func ansiParams(t *testing.T, color string) string {
	t.Helper()
	rendered := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render("x")
	open := strings.Index(rendered, "\x1b[")
	end := strings.Index(rendered, "m")
	if open < 0 || end <= open {
		t.Fatalf("lipgloss emitted no sequence for %s: %q", color, rendered)
	}
	return rendered[open+2 : end]
}
