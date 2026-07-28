// Package frontmatter parses YAML frontmatter from markdown files, shared by
// skills and prompt templates. Port of Pi's utils/frontmatter.ts.
package frontmatter

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// Map is a parsed frontmatter block. Absent frontmatter yields an empty Map
// rather than an error — a plain markdown file is valid input.
type Map map[string]any

// String returns a string-valued key.
func (m Map) String(key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// Bool returns a bool-valued key.
func (m Map) Bool(key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// Parse splits a document into frontmatter and body.
//
// Frontmatter must open with "---" on the first line and close with a line
// starting "\n---"; anything else is treated as an all-body document
// (frontmatter.ts:10-26). Line endings are normalized to \n first, so CRLF
// files parse identically.
func Parse(content []byte) (Map, string, error) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(string(content), "\r\n", "\n"), "\r", "\n")

	if !strings.HasPrefix(normalized, "---") {
		return Map{}, normalized, nil
	}
	end := strings.Index(normalized[3:], "\n---")
	if end == -1 {
		return Map{}, normalized, nil
	}
	end += 3

	yamlText := normalized[4:end]
	body := strings.TrimSpace(normalized[end+4:])

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(yamlText), &parsed); err != nil {
		return Map{}, body, err
	}
	if parsed == nil {
		return Map{}, body, nil
	}
	return Map(parsed), body, nil
}
