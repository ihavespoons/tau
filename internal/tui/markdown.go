package tui

import (
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	emoji "github.com/yuin/goldmark-emoji"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gmext "github.com/yuin/goldmark/extension"
	xast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"

	"github.com/ihavespoons/tau/theme"
)

// hrMaxWidth caps a horizontal rule. A rule spanning a wide terminal reads as
// a page break rather than a separator (markdown.ts:464).
const hrMaxWidth = 80

// codeIndent is the gutter under a fenced code block's opening fence
// (markdown.ts:379).
const codeIndent = "  "

// markdown renders assistant prose. Rendering is comparatively expensive, so it
// happens exactly once per message — when the message completes and is flushed
// into scrollback. Streaming text is shown raw until then.
//
// The renderer walks goldmark's AST and paints with the loaded theme's md* and
// syntax* tokens, which is the whole reason it exists: glamour carried its own
// palette and ignored the user's theme entirely.
type markdown struct {
	mu    sync.Mutex
	width int
	th    Theme
	md    goldmark.Markdown
}

func newMarkdown(width int) *markdown { return newThemedMarkdown(width, DefaultTheme()) }

func newThemedMarkdown(width int, th Theme) *markdown {
	m := &markdown{
		th: th,
		// GFM for tables, strikethrough and task lists; emoji for the
		// shortcodes models reach for unprompted.
		md: goldmark.New(goldmark.WithExtensions(gmext.GFM, emoji.Emoji)),
	}
	m.setWidth(width)
	return m
}

func (m *markdown) setWidth(width int) {
	if width < 20 {
		width = 20
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.width = width
}

// render turns markdown into styled terminal lines. Anything the renderer
// cannot make sense of comes through as its own source text: a transcript that
// renders plainly beats one that disappears.
func (m *markdown) render(src string) []string {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	src = strings.TrimRight(src, "\n")

	m.mu.Lock()
	width, th, md := m.width, m.th, m.md
	m.mu.Unlock()

	buf := []byte(src)
	r := &mdRender{th: th, src: buf}
	doc := md.Parser().Parse(text.NewReader(buf))
	lines := r.blocks(doc, width)

	// Trailing spaces are invisible on screen and very visible when the
	// transcript is copied.
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// mdRender holds what every node needs: the palette and the source the AST
// points into.
type mdRender struct {
	th  Theme
	src []byte
}

// style is the palette lookup. An absent token yields a plain style, so a theme
// missing a colour renders in the terminal's own foreground rather than in
// something invented here.
func (r *mdRender) style(tok theme.Token) lipgloss.Style {
	s := lipgloss.NewStyle()
	if r.th.Colors == nil {
		return s
	}
	if c := r.th.Colors.Color(tok); c != "" {
		s = s.Foreground(lipgloss.Color(c))
	}
	return s
}

// blocks renders a node's block children, separated the way Pi separates them:
// one blank line between blocks, except between a paragraph and the list it
// introduces (markdown.ts:364-371).
func (r *mdRender) blocks(parent ast.Node, width int) []string {
	var out []string
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		lines := r.block(n, width)
		if len(lines) == 0 {
			continue
		}
		if len(out) > 0 && !joined(n.PreviousSibling(), n) {
			out = append(out, "")
		}
		out = append(out, lines...)
	}
	return out
}

// joined reports whether two adjacent blocks run together with no blank line.
func joined(prev, next ast.Node) bool {
	if prev == nil {
		return false
	}
	_, wasParagraph := prev.(*ast.Paragraph)
	_, isList := next.(*ast.List)
	return wasParagraph && isList
}

func (r *mdRender) block(n ast.Node, width int) []string {
	switch n := n.(type) {
	case *ast.Heading:
		return r.heading(n, width)
	case *ast.Paragraph, *ast.TextBlock:
		return wrapBlock(r.inline(n, lipgloss.NewStyle()), width)
	case *ast.FencedCodeBlock:
		return r.code(n, string(n.Language(r.src)))
	case *ast.CodeBlock:
		return r.code(n, "")
	case *ast.List:
		return r.list(n, 0, width)
	case *ast.Blockquote:
		return r.quote(n, width)
	case *ast.ThematicBreak:
		return []string{r.style(theme.MdHr).Render(strings.Repeat("─", min(width, hrMaxWidth)))}
	case *xast.Table:
		return r.table(n, width)
	case *ast.HTMLBlock:
		// Raw HTML has no terminal equivalent. Showing the source beats
		// showing nothing, which is what dropping the node would do.
		return wrapBlock(strings.TrimRight(r.rawText(n), "\n"), width)
	default:
		return wrapBlock(r.inline(n, lipgloss.NewStyle()), width)
	}
}

// heading styles the whole line, and shows the leading hashes only from level
// three down — by then the size difference has run out and the marker is the
// only thing left to carry depth (markdown.ts:336-360).
func (r *mdRender) heading(n *ast.Heading, width int) []string {
	style := r.style(theme.MdHeading).Bold(true)
	body := r.inline(n, style)
	if n.Level >= 3 {
		body = style.Render(strings.Repeat("#", n.Level)+" ") + body
	}
	if n.Level == 1 {
		body = underlineANSI(body)
	}
	return wrapBlock(body, width)
}

// code renders a fenced block: the fences stay visible, because knowing where
// a block starts and what language it claims is worth two lines.
func (r *mdRender) code(n ast.Node, lang string) []string {
	border := r.style(theme.MdCodeBlockBorder)
	out := []string{border.Render("```" + lang)}
	for _, l := range highlightCode(r.rawText(n), lang, r.th) {
		out = append(out, codeIndent+l)
	}
	return append(out, border.Render("```"))
}

// quote prefixes each line with a border, so a wrapped quote still reads as one
// (markdown.ts:414-460).
func (r *mdRender) quote(n ast.Node, width int) []string {
	bar := r.style(theme.MdQuoteBorder).Render("│ ")
	body := r.blocks(n, max(1, width-2))

	out := make([]string, 0, len(body))
	for _, l := range body {
		out = append(out, bar+l)
	}
	return out
}

// list renders items with their markers, indenting nested lists by four columns
// per level and aligning wrapped text under the first character of the item
// rather than under its bullet (markdown.ts:604-660).
func (r *mdRender) list(n *ast.List, depth, width int) []string {
	indent := strings.Repeat("    ", depth)
	bullet := r.style(theme.MdListBullet)
	number := n.Start
	if number == 0 {
		number = 1
	}

	var out []string
	for item := n.FirstChild(); item != nil; item = item.NextSibling() {
		marker := "- "
		if n.IsOrdered() {
			marker = itoa(number) + ". "
			number++
		}
		if box, ok := taskBox(item); ok {
			marker += box
		}

		first := indent + bullet.Render(marker)
		cont := indent + strings.Repeat(" ", displayWidth(marker))
		body := r.itemBody(item, depth, max(1, width-displayWidth(first)))

		if len(body) == 0 {
			out = append(out, first)
		}
		for i, l := range body {
			if i == 0 {
				out = append(out, first+l)
				continue
			}
			out = append(out, cont+l)
		}
		if !n.IsTight && item.NextSibling() != nil {
			out = append(out, "")
		}
	}
	return out
}

// itemBody renders one item's contents. A nested list is rendered at the outer
// width and depth+1 so its own indent lands where it should, rather than being
// indented twice.
func (r *mdRender) itemBody(item ast.Node, depth, width int) []string {
	var out []string
	for n := item.FirstChild(); n != nil; n = n.NextSibling() {
		var lines []string
		if sub, ok := n.(*ast.List); ok {
			lines = r.list(sub, depth+1, width)
		} else {
			lines = r.block(n, width)
		}
		if len(lines) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, lines...)
	}
	return out
}

// taskBox reports the checkbox a GFM task item starts with. The node stays in
// the tree; inline rendering skips it so the marker is not printed twice.
func taskBox(item ast.Node) (string, bool) {
	para := item.FirstChild()
	if para == nil {
		return "", false
	}
	box, ok := para.FirstChild().(*xast.TaskCheckBox)
	if !ok {
		return "", false
	}
	if box.IsChecked {
		return "[x] ", true
	}
	return "[ ] ", true
}

// rawText is a block node's source lines, verbatim.
func (r *mdRender) rawText(n ast.Node) string {
	var b strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(r.src))
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
