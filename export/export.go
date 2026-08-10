// Package export renders a session to a single self-contained HTML page.
//
// The page carries its own viewer, stylesheet, markdown renderer and syntax
// highlighter, so an exported session opens from file:// with no network
// access and nothing to install. This is a port of Pi's
// packages/coding-agent/src/core/export-html; the viewer assets themselves are
// vendored verbatim (see assets/README.md).
package export

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	_ "embed"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/theme"
)

//go:embed assets/template.html
var templateHTML string

//go:embed assets/template.css
var templateCSS string

//go:embed assets/template.js
var templateJS string

//go:embed assets/vendor/marked.min.js
var markedJS string

//go:embed assets/vendor/highlight.min.js
var highlightJS string

// SessionData is what the viewer reads out of the page: the whole session tree
// plus enough of the agent's state to explain it. Field names are the viewer's
// contract — it destructures exactly these keys.
type SessionData struct {
	Header  session.Header  `json:"header"`
	Entries []session.Entry `json:"entries"`
	// LeafID is the branch the viewer opens on. Deliberately not omitempty: the
	// viewer distinguishes "no leaf" (null) from a missing key.
	LeafID       *string `json:"leafId"`
	SystemPrompt string  `json:"systemPrompt,omitempty"`
	Tools        []Tool  `json:"tools,omitempty"`
	// RenderedTools holds HTML pre-rendered by a tool's own renderer, keyed by
	// tool-call id. The viewer draws bash/read/write/edit/ls itself and falls
	// back to a generic view for anything absent here.
	RenderedTools map[string]RenderedTool `json:"renderedTools,omitempty"`
}

// Tool is a tool definition as the viewer's "Available Tools" panel wants it.
type Tool struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Parameters  *jsonschema.Schema `json:"parameters,omitempty"`
}

// RenderedTool is pre-rendered HTML for one tool call and its result.
type RenderedTool struct {
	CallHTML            string `json:"callHtml,omitempty"`
	ResultHTMLCollapsed string `json:"resultHtmlCollapsed,omitempty"`
	ResultHTMLExpanded  string `json:"resultHtmlExpanded,omitempty"`
}

// TemplateRenderedTools names the tools the HTML template draws from their raw
// arguments and output. Everything else needs pre-rendering to look like it
// does in the TUI.
var TemplateRenderedTools = map[string]bool{
	"bash": true, "read": true, "write": true, "edit": true, "ls": true,
}

// Generate renders the page. th supplies the colours; pass nil for the
// built-in dark theme.
func Generate(data SessionData, th *theme.Theme) (string, error) {
	if th == nil {
		var ok bool
		th, ok = theme.Builtin("dark")
		if !ok {
			return "", fmt.Errorf("export: the built-in dark theme is missing")
		}
	}

	payload, err := encodeSessionData(data)
	if err != nil {
		return "", err
	}

	colors := resolveColors(th)
	derived := deriveExportColors(baseColor(colors))
	css := templateCSS
	css = replaceOnce(css, "{{THEME_VARS}}", themeVars(colors, th, derived))
	css = replaceOnce(css, "{{BODY_BG}}", pick(th, theme.PageBg, derived.pageBg))
	css = replaceOnce(css, "{{CONTAINER_BG}}", pick(th, theme.CardBg, derived.cardBg))
	css = replaceOnce(css, "{{INFO_BG}}", pick(th, theme.InfoBg, derived.infoBg))

	html := templateHTML
	html = replaceOnce(html, "{{CSS}}", css)
	html = replaceOnce(html, "{{JS}}", templateJS)
	html = replaceOnce(html, "{{SESSION_DATA}}", payload)
	html = replaceOnce(html, "{{MARKED_JS}}", markedJS)
	html = replaceOnce(html, "{{HIGHLIGHT_JS}}", highlightJS)
	return html, nil
}

// replaceOnce substitutes a placeholder exactly once, matching the single-shot
// String.replace Pi uses. Substituting globally would risk a placeholder-shaped
// string inside injected content being rewritten.
func replaceOnce(s, placeholder, value string) string {
	return strings.Replace(s, placeholder, value, 1)
}

// encodeSessionData base64s the session JSON so no amount of markup, quoting or
// script-tag lookalike in the transcript can break out of the page.
func encodeSessionData(data SessionData) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Entries carry the raw bytes tau read from disk; HTML-escaping them here
	// would rewrite session content that is already safe inside base64.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return "", fmt.Errorf("export: encoding session data: %w", err)
	}
	return base64.StdEncoding.EncodeToString(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
