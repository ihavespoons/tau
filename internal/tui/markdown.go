package tui

import (
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// markdown renders assistant prose. Rendering is comparatively expensive, so
// it happens exactly once per message — when the message completes and is
// flushed into scrollback. Streaming text is shown raw until then.
//
// P10 replaces glamour with a goldmark renderer built for terminals; this
// wrapper is the seam that swap happens behind.
type markdown struct {
	mu    sync.Mutex
	width int
	r     *glamour.TermRenderer
}

func newMarkdown(width int) *markdown {
	m := &markdown{}
	m.setWidth(width)
	return m
}

// compactStyle adapts glamour's defaults to a terminal transcript.
//
// Two changes matter. The document margin goes, because a coding agent's
// output is dense and shares the width with tool results. And the blanket body
// foreground goes, because glamour otherwise paints every cell — including the
// padding spaces it adds out to the wrap width — in one fixed color. That
// produces three problems at once: text that is unreadable when its guess
// about the terminal background is wrong, escape sequences around every single
// space, and trailing padding that ruins copy-paste. Leaving the color unset
// means prose renders in whatever foreground the user already chose, which is
// what every well-behaved CLI does.
//
// Emphasis, headings, links, and syntax-highlighted code keep their styling:
// only the body color is dropped.
func compactStyle() ansi.StyleConfig {
	s := styles.DarkStyleConfig
	if !lipgloss.HasDarkBackground() {
		s = styles.LightStyleConfig
	}

	zero := uint(0)
	s.Document.Margin = &zero
	s.Document.BlockPrefix = ""
	s.Document.BlockSuffix = ""
	s.CodeBlock.Margin = &zero

	s.Document.Color = nil
	s.Document.BackgroundColor = nil
	s.Text.Color = nil
	s.Paragraph.Color = nil
	s.List.Color = nil
	s.Item.Color = nil
	s.Emph.Color = nil
	s.Strong.Color = nil

	return s
}

func (m *markdown) setWidth(width int) {
	if width < 20 {
		width = 20
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.r != nil && m.width == width {
		return
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(compactStyle()),
		glamour.WithWordWrap(width),
		glamour.WithEmoji(),
	)
	if err != nil {
		m.r = nil
		return
	}
	m.width, m.r = width, r
}

// render turns markdown into styled terminal lines. On any failure it falls
// back to the raw text: a transcript that renders plainly beats one that
// disappears.
func (m *markdown) render(src string) []string {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	src = strings.TrimRight(src, "\n")
	m.mu.Lock()
	r := m.r
	m.mu.Unlock()
	if r == nil {
		return strings.Split(src, "\n")
	}
	out, err := r.Render(src)
	if err != nil {
		return strings.Split(src, "\n")
	}

	// glamour pads each line out to the wrap width. Those trailing spaces are
	// invisible on screen but very visible when the transcript is copied, so
	// they are trimmed here.
	lines := strings.Split(strings.Trim(out, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return lines
}
