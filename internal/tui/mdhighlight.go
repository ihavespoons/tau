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
func highlightCode(src, lang string, th Theme) []string {
	src = strings.TrimRight(src, "\n")

	plain := func() []string {
		style := tokenStyle(th, theme.MdCodeBlock)
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

	styles := syntaxStyles(th)
	var (
		out  []string
		line strings.Builder
	)
	for tok := iter(); tok != chroma.EOF; tok = iter() {
		style := styles[syntaxToken(tok.Type)]
		// Each piece is styled on its own so no escape sequence spans a line
		// break: the transcript wraps and is copied line by line.
		parts := strings.Split(tok.Value, "\n")
		for i, part := range parts {
			if i > 0 {
				out = append(out, line.String())
				line.Reset()
			}
			if part == "" {
				continue
			}
			line.WriteString(style.Render(part))
		}
	}
	return append(out, line.String())
}

// syntaxStyles builds the palette once per block rather than once per token.
func syntaxStyles(th Theme) map[theme.Token]lipgloss.Style {
	toks := []theme.Token{
		theme.SyntaxComment, theme.SyntaxKeyword, theme.SyntaxFunction,
		theme.SyntaxVariable, theme.SyntaxString, theme.SyntaxNumber,
		theme.SyntaxType, theme.SyntaxOperator, theme.SyntaxPunctuation,
		theme.MdCodeBlock,
	}
	out := make(map[theme.Token]lipgloss.Style, len(toks))
	for _, t := range toks {
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
