package tui

import (
	"strings"
	"testing"
)

// glamour pads every line out to the wrap width. Those spaces are invisible on
// screen but come along when the transcript is copied, so the renderer has to
// strip them.
func TestRenderedMarkdownHasNoTrailingPadding(t *testing.T) {
	md := newMarkdown(60)
	for _, line := range md.render("Some text\n\n- one\n- two\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line carries trailing whitespace: %q", line)
		}
	}
}

// The body must not carry a hardcoded foreground: whichever one glamour picks
// is wrong on half of all terminals, so prose inherits the user's own.
func TestBodyTextIsNotRecolored(t *testing.T) {
	md := newMarkdown(60)
	lines := md.render("plain sentence with no markup")
	if len(lines) == 0 {
		t.Fatal("nothing rendered")
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "\x1b[") {
		t.Errorf("unstyled prose should emit no escape sequences, got %q", joined)
	}
}

// Emphasis and code still have to be visible — dropping the body color must
// not flatten everything.
func TestEmphasisAndCodeAreStillStyled(t *testing.T) {
	md := newMarkdown(60)
	joined := strings.Join(md.render("a **bold** word and `code`"), "\n")
	if !strings.Contains(joined, "\x1b[") {
		t.Errorf("emphasis lost its styling: %q", joined)
	}
	if !strings.Contains(stripANSI(joined), "bold") {
		t.Errorf("emphasis lost its text: %q", joined)
	}
}

func TestRenderedMarkdownRespectsWidth(t *testing.T) {
	const width = 40
	md := newMarkdown(width)
	long := strings.Repeat("alpha beta gamma delta ", 10)
	for _, line := range md.render(long) {
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
