// Package prompttemplate loads and expands markdown prompt templates.
// Port of Pi's core/prompt-templates.ts.
package prompttemplate

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/ihavespoons/tau/frontmatter"
)

// Template is a prompt template loaded from a .md file.
type Template struct {
	Name         string
	Description  string
	ArgumentHint string
	Content      string
	FilePath     string
	Source       string // "user" | "project" | "path"
}

// ParseArgs splits an argument string bash-style, honoring single and double
// quotes (prompt-templates.ts:24-55). Quotes group but are not preserved.
func ParseArgs(s string) []string {
	var args []string
	var current strings.Builder
	var quote rune

	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				current.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case unicode.IsSpace(c):
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(c)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// substitution grammar, mirroring prompt-templates.ts:74:
//
//	${N:-default} ${@:-default} ${ARGUMENTS:-default}   default when empty
//	${@:N} ${@:N:L}                                     bash-style slicing
//	$1 $@ $ARGUMENTS                                    simple
var substPattern = regexp.MustCompile(`\$\{(\d+|ARGUMENTS|@):-([^}]*)\}|\$\{@:(\d+)(?::(\d+))?\}|\$(ARGUMENTS|@|\d+)`)

// SubstituteArgs expands argument placeholders.
//
// Substitution is single-pass over the template: values that themselves look
// like placeholders are not re-expanded (prompt-templates.ts:67-69).
func SubstituteArgs(content string, args []string) string {
	all := strings.Join(args, " ")

	return substPattern.ReplaceAllStringFunc(content, func(match string) string {
		m := substPattern.FindStringSubmatch(match)
		defaultTarget, defaultValue := m[1], m[2]
		sliceStart, sliceLength := m[3], m[4]
		simple := m[5]

		if defaultTarget != "" {
			var value string
			if defaultTarget == "@" || defaultTarget == "ARGUMENTS" {
				value = all
			} else if n, err := strconv.Atoi(defaultTarget); err == nil {
				value = at(args, n-1)
			}
			if value != "" {
				return value
			}
			return defaultValue
		}

		if sliceStart != "" {
			start, _ := strconv.Atoi(sliceStart)
			start-- // user indices are 1-based
			if start < 0 {
				start = 0
			}
			if start > len(args) {
				start = len(args)
			}
			if sliceLength != "" {
				length, _ := strconv.Atoi(sliceLength)
				end := start + length
				if end > len(args) {
					end = len(args)
				}
				if end < start {
					end = start
				}
				return strings.Join(args[start:end], " ")
			}
			return strings.Join(args[start:], " ")
		}

		if simple == "ARGUMENTS" || simple == "@" {
			return all
		}
		if n, err := strconv.Atoi(simple); err == nil {
			return at(args, n-1)
		}
		return match
	})
}

func at(args []string, i int) string {
	if i < 0 || i >= len(args) {
		return ""
	}
	return args[i]
}

// LoadFromFile parses one template file. The name is the basename without
// .md; a missing description falls back to the first non-empty body line,
// truncated to 60 characters (prompt-templates.ts:111-120).
func LoadFromFile(path, source string) (*Template, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fm, body, err := frontmatter.Parse(raw)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSuffix(filepath.Base(path), ".md")
	description, _ := fm.String("description")
	if description == "" {
		for _, line := range strings.Split(body, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			description = line
			if len(line) > 60 {
				description = line[:60] + "..."
			}
			break
		}
	}
	hint, _ := fm.String("argument-hint")

	return &Template{
		Name:         name,
		Description:  description,
		ArgumentHint: hint,
		Content:      body,
		FilePath:     path,
		Source:       source,
	}, nil
}

// LoadFromDir loads .md templates from a directory, non-recursively
// (prompt-templates.ts:138-175).
func LoadFromDir(dir, source string) []Template {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Template
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if t, err := LoadFromFile(full, source); err == nil {
			out = append(out, *t)
		}
	}
	return out
}

// LoadOptions configures Load.
type LoadOptions struct {
	Cwd             string
	AgentDir        string
	Paths           []string
	IncludeDefaults bool
	ConfigDirName   string
}

// Load gathers templates from <agentDir>/prompts, <cwd>/.tau/prompts, and
// any explicit paths.
func Load(opts LoadOptions) []Template {
	configDir := opts.ConfigDirName
	if configDir == "" {
		configDir = ".tau"
	}
	userDir := filepath.Join(opts.AgentDir, "prompts")
	projectDir := filepath.Join(opts.Cwd, configDir, "prompts")

	var out []Template
	if opts.IncludeDefaults {
		out = append(out, LoadFromDir(userDir, "user")...)
		out = append(out, LoadFromDir(projectDir, "project")...)
	}
	for _, raw := range opts.Paths {
		path := raw
		if !filepath.IsAbs(path) {
			path = filepath.Join(opts.Cwd, path)
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		source := "path"
		switch {
		case strings.HasPrefix(path, userDir):
			source = "user"
		case strings.HasPrefix(path, projectDir):
			source = "project"
		}
		if info.IsDir() {
			out = append(out, LoadFromDir(path, source)...)
		} else if strings.HasSuffix(path, ".md") {
			if t, err := LoadFromFile(path, source); err == nil {
				out = append(out, *t)
			}
		}
	}
	return out
}

// Find returns the template with the given name.
func Find(list []Template, name string) (Template, bool) {
	for _, t := range list {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

var expandPattern = regexp.MustCompile(`^/([^\s]+)(?:\s+([\s\S]*))?$`)

// Expand rewrites "/name args" into the template body with arguments
// substituted. Text that is not a slash command, or names no known template,
// is returned unchanged (prompt-templates.ts:269-285).
func Expand(text string, templates []Template) string {
	if !strings.HasPrefix(text, "/") {
		return text
	}
	m := expandPattern.FindStringSubmatch(text)
	if m == nil {
		return text
	}
	t, ok := Find(templates, m[1])
	if !ok {
		return text
	}
	return SubstituteArgs(t.Content, ParseArgs(m[2]))
}
