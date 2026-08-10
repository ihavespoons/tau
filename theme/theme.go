// Package theme loads Pi-format JSON colour themes.
//
// A theme file has exactly the shape Pi's themes use, so a theme written for Pi
// loads here unmodified: a name, an optional table of colour variables, and a
// colours table carrying one entry per token in [Tokens]. A colour value is a
// "#rrggbb" hex string, a 256-colour palette index (0-255), the name of an
// entry in vars, or the empty string meaning "no colour".
//
// Ported from pi/packages/coding-agent/src/modes/interactive/theme/theme.ts.
package theme

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Token names one colour slot in a theme.
type Token string

// The colour tokens a theme file declares, in the order Pi's schema lists them.
const (
	// Core UI.
	Accent       Token = "accent"
	Border       Token = "border"
	BorderAccent Token = "borderAccent"
	BorderMuted  Token = "borderMuted"
	Success      Token = "success"
	Error        Token = "error"
	Warning      Token = "warning"
	Muted        Token = "muted"
	Dim          Token = "dim"
	Text         Token = "text"
	ThinkingText Token = "thinkingText"

	// Backgrounds and content text.
	SelectedBg         Token = "selectedBg"
	UserMessageBg      Token = "userMessageBg"
	UserMessageText    Token = "userMessageText"
	CustomMessageBg    Token = "customMessageBg"
	CustomMessageText  Token = "customMessageText"
	CustomMessageLabel Token = "customMessageLabel"
	ToolPendingBg      Token = "toolPendingBg"
	ToolSuccessBg      Token = "toolSuccessBg"
	ToolErrorBg        Token = "toolErrorBg"
	ToolTitle          Token = "toolTitle"
	ToolOutput         Token = "toolOutput"

	// Markdown.
	MdHeading         Token = "mdHeading"
	MdLink            Token = "mdLink"
	MdLinkURL         Token = "mdLinkUrl"
	MdCode            Token = "mdCode"
	MdCodeBlock       Token = "mdCodeBlock"
	MdCodeBlockBorder Token = "mdCodeBlockBorder"
	MdQuote           Token = "mdQuote"
	MdQuoteBorder     Token = "mdQuoteBorder"
	MdHr              Token = "mdHr"
	MdListBullet      Token = "mdListBullet"

	// Tool diffs.
	ToolDiffAdded   Token = "toolDiffAdded"
	ToolDiffRemoved Token = "toolDiffRemoved"
	ToolDiffContext Token = "toolDiffContext"

	// Syntax highlighting.
	SyntaxComment     Token = "syntaxComment"
	SyntaxKeyword     Token = "syntaxKeyword"
	SyntaxFunction    Token = "syntaxFunction"
	SyntaxVariable    Token = "syntaxVariable"
	SyntaxString      Token = "syntaxString"
	SyntaxNumber      Token = "syntaxNumber"
	SyntaxType        Token = "syntaxType"
	SyntaxOperator    Token = "syntaxOperator"
	SyntaxPunctuation Token = "syntaxPunctuation"

	// Thinking-level borders. ThinkingMax is the one optional token: a theme
	// that omits it inherits ThinkingXhigh.
	ThinkingOff     Token = "thinkingOff"
	ThinkingMinimal Token = "thinkingMinimal"
	ThinkingLow     Token = "thinkingLow"
	ThinkingMedium  Token = "thinkingMedium"
	ThinkingHigh    Token = "thinkingHigh"
	ThinkingXhigh   Token = "thinkingXhigh"
	ThinkingMax     Token = "thinkingMax"

	// Bash mode.
	BashMode Token = "bashMode"
)

// Tokens lists every colour token a theme may declare, in schema order. All of
// them but [ThinkingMax] are required.
var Tokens = []Token{
	Accent, Border, BorderAccent, BorderMuted, Success, Error, Warning, Muted,
	Dim, Text, ThinkingText,

	SelectedBg, UserMessageBg, UserMessageText, CustomMessageBg,
	CustomMessageText, CustomMessageLabel, ToolPendingBg, ToolSuccessBg,
	ToolErrorBg, ToolTitle, ToolOutput,

	MdHeading, MdLink, MdLinkURL, MdCode, MdCodeBlock, MdCodeBlockBorder,
	MdQuote, MdQuoteBorder, MdHr, MdListBullet,

	ToolDiffAdded, ToolDiffRemoved, ToolDiffContext,

	SyntaxComment, SyntaxKeyword, SyntaxFunction, SyntaxVariable, SyntaxString,
	SyntaxNumber, SyntaxType, SyntaxOperator, SyntaxPunctuation,

	ThinkingOff, ThinkingMinimal, ThinkingLow, ThinkingMedium, ThinkingHigh,
	ThinkingXhigh, ThinkingMax,

	BashMode,
}

// ExportKey names a colour used by the HTML export rather than the terminal.
// All three are optional.
type ExportKey string

const (
	PageBg ExportKey = "pageBg"
	CardBg ExportKey = "cardBg"
	InfoBg ExportKey = "infoBg"
)

// ExportKeys lists every export colour a theme may declare.
var ExportKeys = []ExportKey{PageBg, CardBg, InfoBg}

func optional(tok Token) bool { return tok == ThinkingMax }

// Value is a single colour: a hex string, a variable reference, the empty
// string ("no colour"), or a 256-colour palette index.
type Value struct {
	// Str holds the string form — "#rrggbb", a variable name, or "". Empty
	// when IsIndex is set.
	Str string
	// Idx holds the palette index when IsIndex is set.
	Idx int
	// IsIndex reports whether the value was written as a number.
	IsIndex bool
}

// Str returns a string-form value.
func Str(s string) Value { return Value{Str: s} }

// Index returns a 256-colour palette value.
func Index(i int) Value { return Value{Idx: i, IsIndex: true} }

// IsEmpty reports whether the value means "no colour".
func (v Value) IsEmpty() bool { return !v.IsIndex && v.Str == "" }

// String renders the value the way a terminal wants it: "#rrggbb" for a hex
// colour, a decimal index for a palette colour, "" for no colour. Only
// meaningful once variable references have been resolved — an unresolved
// reference stringifies as its own name.
func (v Value) String() string {
	if v.IsIndex {
		return strconv.Itoa(v.Idx)
	}
	return v.Str
}

// terminal reports whether the value stands on its own rather than naming a
// variable. Mirrors the first branch of Pi's resolveVarRefs.
func (v Value) terminal() bool {
	return v.IsIndex || v.Str == "" || strings.HasPrefix(v.Str, "#")
}

// UnmarshalJSON accepts either a string or an integer in 0-255.
func (v *Value) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*v = Value{Str: s}
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("colour must be a string or an integer 0-255, got %s", trimmed)
	}
	i, err := strconv.Atoi(n.String())
	if err != nil {
		return fmt.Errorf("colour index must be a whole number, got %s", n.String())
	}
	if i < 0 || i > 255 {
		return fmt.Errorf("colour index %d out of range 0-255", i)
	}
	*v = Value{Idx: i, IsIndex: true}
	return nil
}

// MarshalJSON writes the value back in the form it was read.
func (v Value) MarshalJSON() ([]byte, error) {
	if v.IsIndex {
		return json.Marshal(v.Idx)
	}
	return json.Marshal(v.Str)
}

// File is the on-disk shape of a theme.
type File struct {
	Schema string           `json:"$schema,omitempty"`
	Name   string           `json:"name"`
	Vars   map[string]Value `json:"vars,omitempty"`
	Colors map[Token]Value  `json:"colors"`
	Export *ExportSection   `json:"export,omitempty"`
}

// ExportSection carries the HTML-export-only colours.
type ExportSection struct {
	PageBg *Value `json:"pageBg,omitempty"`
	CardBg *Value `json:"cardBg,omitempty"`
	InfoBg *Value `json:"infoBg,omitempty"`
}

func (e *ExportSection) get(k ExportKey) *Value {
	if e == nil {
		return nil
	}
	switch k {
	case PageBg:
		return e.PageBg
	case CardBg:
		return e.CardBg
	case InfoBg:
		return e.InfoBg
	}
	return nil
}

// Theme is a parsed theme with every variable reference already resolved.
type Theme struct {
	// Name is the theme's declared name, and how it is selected.
	Name string
	// Path is the file the theme was read from, empty for a built-in.
	Path string

	colors map[Token]Value
	export map[ExportKey]Value
}

// Color returns the terminal form of a token's colour: "#rrggbb", a decimal
// palette index, or "" for no colour. An unknown token returns "".
func (t *Theme) Color(tok Token) string { return t.colors[tok].String() }

// Value returns a token's resolved value.
func (t *Theme) Value(tok Token) Value { return t.colors[tok] }

// ExportColor returns an export colour as hex, converting a palette index the
// way Pi's getThemeExportColors does. Returns "" when the theme leaves it unset.
func (t *Theme) ExportColor(k ExportKey) string {
	v, ok := t.export[k]
	if !ok || v.IsEmpty() {
		return ""
	}
	if v.IsIndex {
		return ANSI256ToHex(v.Idx)
	}
	return v.Str
}

// Parse reads a theme from JSON. path is recorded on the result and used in
// error messages; pass "" for a theme that has no file.
func Parse(data []byte, path string) (*Theme, error) {
	var f File
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", describe(path), err)
	}
	return FromFile(&f, path)
}

// FromFile validates and resolves an already-decoded theme.
func FromFile(f *File, path string) (*Theme, error) {
	if strings.TrimSpace(f.Name) == "" {
		return nil, fmt.Errorf("%s: theme has no name", describe(path))
	}
	if err := ValidateName(f.Name); err != nil {
		return nil, fmt.Errorf("%s: %w", describe(path), err)
	}

	var missing []string
	for _, tok := range Tokens {
		if optional(tok) {
			continue
		}
		if _, ok := f.Colors[tok]; !ok {
			missing = append(missing, string(tok))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%s: theme %q is missing %d colour(s): %s",
			describe(path), f.Name, len(missing), strings.Join(missing, ", "))
	}

	known := make(map[Token]bool, len(Tokens))
	for _, tok := range Tokens {
		known[tok] = true
	}
	var unknown []string
	for tok := range f.Colors {
		if !known[tok] {
			unknown = append(unknown, string(tok))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: theme %q declares unknown colour(s): %s",
			describe(path), f.Name, strings.Join(unknown, ", "))
	}

	t := &Theme{
		Name:   f.Name,
		Path:   path,
		colors: make(map[Token]Value, len(Tokens)),
		export: make(map[ExportKey]Value, len(ExportKeys)),
	}

	// thinkingMax falls back to thinkingXhigh — Pi's withThemeColorFallbacks.
	colors := make(map[Token]Value, len(f.Colors)+1)
	for tok, v := range f.Colors {
		colors[tok] = v
	}
	if _, ok := colors[ThinkingMax]; !ok {
		colors[ThinkingMax] = colors[ThinkingXhigh]
	}

	for tok, v := range colors {
		resolved, err := resolveVar(v, f.Vars, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: theme %q colour %q: %w", describe(path), f.Name, tok, err)
		}
		t.colors[tok] = resolved
	}

	for _, k := range ExportKeys {
		v := f.Export.get(k)
		if v == nil {
			continue
		}
		resolved, err := resolveVar(*v, f.Vars, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: theme %q export %q: %w", describe(path), f.Name, k, err)
		}
		t.export[k] = resolved
	}

	return t, nil
}

// ValidateName rejects theme names that cannot be selected. A "/" is reserved
// for the automatic light/dark form of the theme setting, so a theme carrying
// one could never be named in settings.
func ValidateName(name string) error {
	if strings.Contains(name, "/") {
		return fmt.Errorf("invalid theme name %q: theme names cannot contain %q because it is reserved for automatic light/dark theme settings", name, "/")
	}
	return nil
}

// resolveVar walks variable references to a terminal value, refusing cycles.
// Verbatim port of Pi's resolveVarRefs, including its error strings.
func resolveVar(v Value, vars map[string]Value, visited map[string]bool) (Value, error) {
	if v.terminal() {
		return v, nil
	}
	if visited[v.Str] {
		return Value{}, fmt.Errorf("circular variable reference detected: %s", v.Str)
	}
	next, ok := vars[v.Str]
	if !ok {
		return Value{}, fmt.Errorf("variable reference not found: %s", v.Str)
	}
	if visited == nil {
		visited = make(map[string]bool)
	}
	visited[v.Str] = true
	return resolveVar(next, vars, visited)
}

func describe(path string) string {
	if path == "" {
		return "theme"
	}
	return path
}
