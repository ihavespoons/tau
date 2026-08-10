package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	emojiast "github.com/yuin/goldmark-emoji/ast"
	"github.com/yuin/goldmark/ast"
	xast "github.com/yuin/goldmark/extension/ast"

	"github.com/ihavespoons/tau/theme"
)

// inline renders a block's inline children into one string, which the caller
// wraps.
//
// base is the style the surrounding block established — a heading's colour, a
// quote's italics — and every nested style derives from it rather than
// replacing it, so bold inside a heading stays the heading's colour. Blocks
// with nothing to say pass a plain style, and plain prose then emits no escape
// sequences at all: body text inherits whatever foreground the user's terminal
// already uses, which is the one colour guaranteed to be readable.
func (r *mdRender) inline(parent ast.Node, base lipgloss.Style) string {
	var b strings.Builder
	r.inlineInto(&b, parent, base)
	return b.String()
}

func (r *mdRender) inlineInto(b *strings.Builder, parent ast.Node, base lipgloss.Style) {
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		switch n := n.(type) {
		case *ast.Text:
			b.WriteString(base.Render(string(n.Segment.Value(r.src))))
			switch {
			case n.HardLineBreak():
				b.WriteString("\n")
			case n.SoftLineBreak():
				// A source line break inside a paragraph is a space: the
				// renderer re-wraps to the terminal's width, so honouring the
				// author's line endings would wrap twice.
				b.WriteString(" ")
			}

		case *ast.String:
			b.WriteString(base.Render(string(n.Value)))

		case *ast.Emphasis:
			style := base.Italic(true)
			if n.Level >= 2 {
				style = base.Bold(true)
			}
			r.inlineInto(b, n, style)

		case *xast.Strikethrough:
			var seg strings.Builder
			r.inlineInto(&seg, n, base)
			b.WriteString(strikeANSI(seg.String()))

		case *ast.CodeSpan:
			b.WriteString(r.style(theme.MdCode).Render(r.plainText(n)))

		case *ast.Link:
			r.link(b, n, string(n.Destination), base)

		case *ast.AutoLink:
			url := string(n.URL(r.src))
			b.WriteString(underlineANSI(r.style(theme.MdLink).Render(url)))

		case *ast.Image:
			// The terminal cannot show it here, so it is named instead. Image
			// protocols are a separate feature with its own capability check.
			b.WriteString(r.style(theme.MdLink).Render("[image: " + r.plainText(n) + "]"))
			r.appendURL(b, string(n.Destination), "")

		case *ast.RawHTML:
			b.WriteString(base.Render(r.rawInline(n)))

		case *emojiast.Emoji:
			if n.Value != nil {
				b.WriteString(base.Render(string(n.Value.Unicode)))
				continue
			}
			b.WriteString(base.Render(string(n.ShortName)))

		case *xast.TaskCheckBox:
			// Already drawn as the list item's marker.

		default:
			r.inlineInto(b, n, base)
		}
	}
}

// link shows the text and, when they differ, the destination after it. A
// terminal hyperlink would hide the URL behind the text, and a transcript that
// hides where a link goes is one the reader cannot audit.
func (r *mdRender) link(b *strings.Builder, n ast.Node, dest string, base lipgloss.Style) {
	text := r.plainText(n)
	if text == "" {
		b.WriteString(underlineANSI(r.style(theme.MdLink).Render(dest)))
		return
	}
	var seg strings.Builder
	r.inlineInto(&seg, n, base.Foreground(r.color(theme.MdLink)))
	b.WriteString(underlineANSI(seg.String()))
	r.appendURL(b, dest, text)
}

// appendURL writes " (dest)" unless the destination is what the text already
// said.
func (r *mdRender) appendURL(b *strings.Builder, dest, text string) {
	if dest == "" || dest == text {
		return
	}
	b.WriteString(r.style(theme.MdLinkURL).Render(" (" + dest + ")"))
}

// color is the raw colour for a token, for cases that need to set a foreground
// on an inherited style rather than start a new one.
func (r *mdRender) color(tok theme.Token) lipgloss.TerminalColor {
	if r.th.Colors == nil {
		return lipgloss.NoColor{}
	}
	if c := r.th.Colors.Color(tok); c != "" {
		return lipgloss.Color(c)
	}
	return lipgloss.NoColor{}
}

// plainText is a node's text with no styling, for the places that need the
// characters rather than their appearance.
func (r *mdRender) plainText(parent ast.Node) string {
	var b strings.Builder
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		switch n := n.(type) {
		case *ast.Text:
			b.Write(n.Segment.Value(r.src))
			if n.SoftLineBreak() || n.HardLineBreak() {
				b.WriteString(" ")
			}
		case *ast.String:
			b.Write(n.Value)
		case *ast.RawHTML:
			b.WriteString(r.rawInline(n))
		case *emojiast.Emoji:
			if n.Value != nil {
				b.WriteString(string(n.Value.Unicode))
			}
		default:
			b.WriteString(r.plainText(n))
		}
	}
	return b.String()
}

// rawInline is an inline HTML node's source.
func (r *mdRender) rawInline(n *ast.RawHTML) string {
	var b strings.Builder
	for i := 0; i < n.Segments.Len(); i++ {
		seg := n.Segments.At(i)
		b.Write(seg.Value(r.src))
	}
	return b.String()
}
