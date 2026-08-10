package tui

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/charmbracelet/lipgloss"

	"github.com/ihavespoons/tau/theme"
)

// highlightCode renders a code block as styled lines, using the loaded theme's
// syntax colours rather than a palette of chroma's own. A theme that declares
// no syntax colours produces plain text, which is the correct outcome: the user
// asked for those colours and got them.
//
// An unknown language is not an error. Chroma guesses from the content, and a
// failed guess falls through to unstyled code — a block that renders plainly
// beats one that does not render.
func highlightCode(src, lang string, styles map[theme.Token]lipgloss.Style) []string {
	src = strings.TrimRight(src, "\n")

	plain := func() []string {
		style := styles[theme.MdCodeBlock]
		lines := strings.Split(src, "\n")
		for i, l := range lines {
			lines[i] = style.Render(l)
		}
		return lines
	}

	lexer := lexers.Get(lang)
	if lexer == nil && lang == "" {
		lexer = lexers.Analyse(src)
	}
	if lexer == nil {
		return plain()
	}
	iter, err := chroma.Coalesce(lexer).Tokenise(nil, src)
	if err != nil {
		return plain()
	}

	var (
		out  []string
		line strings.Builder
	)
	for tok := iter(); tok != chroma.EOF; tok = iter() {
		style := styles[syntaxToken(tok.Type)]
		// Walked by index rather than split on newlines: most tokens contain
		// none, and splitting every one of them allocated a slice per token.
		for rest := tok.Value; ; {
			nl := strings.IndexByte(rest, '\n')
			if nl < 0 {
				writeStyled(&line, style, rest)
				break
			}
			// Each piece is styled on its own so no escape sequence spans a
			// line break: the transcript wraps and is copied line by line.
			writeStyled(&line, style, rest[:nl])
			out = append(out, line.String())
			line.Reset()
			rest = rest[nl+1:]
		}
	}
	return append(out, line.String())
}

// writeStyled appends a run, leaving whitespace unstyled. A foreground colour
// on a space is invisible, so styling one only adds escape sequences — to the
// terminal's work, to the transcript's size, and to what lands on the clipboard
// when the code is copied.
func writeStyled(b *strings.Builder, style lipgloss.Style, s string) {
	if s == "" {
		return
	}
	if strings.TrimSpace(s) == "" {
		b.WriteString(s)
		return
	}
	b.WriteString(style.Render(s))
}

// paletteTokens are every colour the markdown renderer paints with.
var paletteTokens = []theme.Token{
	theme.MdHeading, theme.MdLink, theme.MdLinkURL, theme.MdCode,
	theme.MdCodeBlock, theme.MdCodeBlockBorder, theme.MdQuote,
	theme.MdQuoteBorder, theme.MdHr, theme.MdListBullet, theme.Border,

	theme.SyntaxComment, theme.SyntaxKeyword, theme.SyntaxFunction,
	theme.SyntaxVariable, theme.SyntaxString, theme.SyntaxNumber,
	theme.SyntaxType, theme.SyntaxOperator, theme.SyntaxPunctuation,
}

// paletteFor resolves the theme's colours into styles once, when the renderer
// is built. Resolving them per node meant rebuilding a style for every heading,
// bullet and fence in the transcript.
//
// A token the theme does not declare is absent from the map, and a missing key
// yields the zero style — which renders plainly, exactly as intended.
func paletteFor(th Theme) map[theme.Token]lipgloss.Style {
	out := make(map[theme.Token]lipgloss.Style, len(paletteTokens))
	for _, t := range paletteTokens {
		out[t] = tokenStyle(th, t)
	}
	return out
}

// syntaxToken maps chroma's token tree onto the nine syntax colours a theme
// declares. Pi's themes carry exactly these, so a theme written for Pi colours
// tau's code blocks the way its author intended.
//
// The specific types are checked before their categories, because a keyword
// that names a type and a name that names a function both want their own
// colour and would otherwise be swallowed by the general case.
func syntaxToken(t chroma.TokenType) theme.Token {
	switch t {
	case chroma.KeywordType:
		return theme.SyntaxType
	case chroma.NameFunction, chroma.NameFunctionMagic:
		return theme.SyntaxFunction
	case chroma.NameClass, chroma.NameBuiltin, chroma.NameException, chroma.NameNamespace:
		return theme.SyntaxType
	}

	switch t.Category() {
	case chroma.Comment:
		return theme.SyntaxComment
	case chroma.Keyword:
		return theme.SyntaxKeyword
	case chroma.Name:
		return theme.SyntaxVariable
	case chroma.Operator:
		return theme.SyntaxOperator
	case chroma.Punctuation:
		return theme.SyntaxPunctuation
	case chroma.Literal:
		switch t.SubCategory() {
		case chroma.LiteralString:
			return theme.SyntaxString
		case chroma.LiteralNumber:
			return theme.SyntaxNumber
		}
		return theme.SyntaxString
	}
	return theme.MdCodeBlock
}

// tokenStyle is the palette lookup for callers outside a render pass.
func tokenStyle(th Theme, tok theme.Token) lipgloss.Style {
	s := lipgloss.NewStyle()
	if th.Colors == nil {
		return s
	}
	if c := th.Colors.Color(tok); c != "" {
		s = s.Foreground(lipgloss.Color(c))
	}
	return s
}
