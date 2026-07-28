// Package prompt builds tau's system prompt and discovers the project
// context files that feed it. Port of Pi's core/system-prompt.ts and the
// context-file half of core/resource-loader.ts.
package prompt

import (
	"sort"
	"strings"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/skills"
)

// ContextFile is a project instruction file (AGENTS.md / CLAUDE.md) loaded
// into the system prompt.
type ContextFile struct {
	Path    string
	Content string
}

// Docs points at tau's own documentation so the agent can answer questions
// about tau itself. When Readme is empty the whole section is omitted.
//
// Pi always emits this block because its docs ship inside the npm package.
// tau's binary has no bundled docs yet, so pointing the model at paths that
// do not exist would just make it issue failing reads.
type Docs struct {
	Readme   string
	Docs     string
	Examples string
}

// Options mirrors Pi's BuildSystemPromptOptions.
type Options struct {
	// CustomPrompt replaces the default base prompt entirely. Tools,
	// guidelines, and the docs block are all skipped when it is set.
	CustomPrompt string
	// SelectedTools names the tools available this run. Nil means Pi's
	// default set (read, bash, edit, write).
	SelectedTools []string
	// ToolSnippets maps tool name to its one-line description. A tool
	// without a snippet is callable but never advertised.
	ToolSnippets map[string]string
	// PromptGuidelines are extra bullets from tools and extensions.
	PromptGuidelines []string
	// AppendSystemPrompt is appended verbatim after the base prompt.
	AppendSystemPrompt string
	// Cwd is the working directory reported to the model.
	Cwd string
	// ContextFiles are pre-loaded project instruction files.
	ContextFiles []ContextFile
	// Skills are pre-loaded skills, rendered only when the read tool exists.
	Skills []skills.Skill
	// Docs optionally points at tau's own documentation.
	Docs Docs
}

// DefaultTools is Pi's default selected-tool set (system-prompt.ts:81).
var DefaultTools = []string{"read", "bash", "edit", "write"}

const basePrompt = `You are an expert coding assistant operating inside tau, a coding agent harness. You help users by reading files, executing commands, editing code, and writing new files.`

// Build assembles the system prompt.
//
// Section order, verified against system-prompt.ts:
//
//	base prompt (incl. Available tools, Guidelines, tau docs)  [:121-138]
//	append system prompt                                       [:140-142]
//	<project_context> instruction files                        [:144-152]
//	skills, only when the read tool is present                 [:154-157]
//	Current working directory                                  [:159]
//
// A CustomPrompt short-circuits the first section: tools, guidelines, and
// the docs block are dropped, but everything after still applies [:46-72].
func Build(opts Options) string {
	promptCwd := strings.ReplaceAll(opts.Cwd, `\`, "/")

	var b strings.Builder
	if opts.CustomPrompt != "" {
		b.WriteString(opts.CustomPrompt)
	} else {
		b.WriteString(buildBase(opts))
	}

	if opts.AppendSystemPrompt != "" {
		b.WriteString("\n\n")
		b.WriteString(opts.AppendSystemPrompt)
	}

	if len(opts.ContextFiles) > 0 {
		b.WriteString("\n\n<project_context>\n\n")
		b.WriteString("Project-specific instructions and guidelines:\n\n")
		for _, f := range opts.ContextFiles {
			b.WriteString(`<project_instructions path="`)
			b.WriteString(f.Path)
			b.WriteString("\">\n")
			b.WriteString(f.Content)
			b.WriteString("\n</project_instructions>\n\n")
		}
		b.WriteString("</project_context>\n")
	}

	// With a custom prompt, a nil SelectedTools means "unknown, assume read
	// is present" — Pi: `!selectedTools || selectedTools.includes("read")`.
	hasRead := opts.SelectedTools == nil
	if opts.CustomPrompt == "" {
		hasRead = contains(selectedTools(opts), "read")
	} else if opts.SelectedTools != nil {
		hasRead = contains(opts.SelectedTools, "read")
	}
	if hasRead && len(opts.Skills) > 0 {
		b.WriteString(skills.FormatForPrompt(opts.Skills))
	}

	b.WriteString("\nCurrent working directory: ")
	b.WriteString(promptCwd)
	return b.String()
}

func selectedTools(opts Options) []string {
	if opts.SelectedTools != nil {
		return opts.SelectedTools
	}
	return DefaultTools
}

func buildBase(opts Options) string {
	tools := selectedTools(opts)

	// Only tools with a snippet are advertised (system-prompt.ts:82).
	var visible []string
	for _, name := range tools {
		if s := opts.ToolSnippets[name]; s != "" {
			visible = append(visible, "- "+name+": "+s)
		}
	}
	toolsList := "(none)"
	if len(visible) > 0 {
		toolsList = strings.Join(visible, "\n")
	}

	var b strings.Builder
	b.WriteString(basePrompt)
	b.WriteString("\n\nAvailable tools:\n")
	b.WriteString(toolsList)
	b.WriteString("\n\nIn addition to the tools above, you may have access to other custom tools depending on the project.")
	b.WriteString("\n\nGuidelines:\n")
	b.WriteString(strings.Join(guidelines(tools, opts.PromptGuidelines), "\n"))

	if opts.Docs.Readme != "" {
		b.WriteString("\n\ntau documentation (read only when the user asks about tau itself, its SDK, extensions, themes, skills, or TUI):")
		b.WriteString("\n- Main documentation: " + opts.Docs.Readme)
		if opts.Docs.Docs != "" {
			b.WriteString("\n- Additional docs: " + opts.Docs.Docs)
		}
		if opts.Docs.Examples != "" {
			b.WriteString("\n- Examples: " + opts.Docs.Examples + " (extensions, custom tools, SDK)")
		}
		b.WriteString("\n- When reading tau docs or examples, resolve docs/... under Additional docs and examples/... under Examples, not the current working directory")
		b.WriteString("\n- When working on tau topics, read the docs and examples, and follow .md cross-references before implementing")
		b.WriteString("\n- Always read tau .md files completely and follow links to related docs")
	}
	return b.String()
}

// guidelines assembles the deduped bullet list. Order matters and is Pi's
// (system-prompt.ts:103-119): the bash fallback first, then caller-supplied
// guidelines, then the two unconditional ones.
func guidelines(tools []string, extra []string) []string {
	var list []string
	seen := map[string]bool{}
	add := func(g string) {
		if g == "" || seen[g] {
			return
		}
		seen[g] = true
		list = append(list, "- "+g)
	}

	hasBash := contains(tools, "bash")
	if hasBash && !contains(tools, "grep") && !contains(tools, "find") && !contains(tools, "ls") {
		add("Use bash for file operations like ls, rg, find")
	}
	for _, g := range extra {
		add(strings.TrimSpace(g))
	}
	add("Be concise in your responses")
	add("Show file paths clearly when working with files")
	return list
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// FromTools derives SelectedTools, ToolSnippets, and PromptGuidelines from a
// live tool set, so callers do not have to keep three lists in sync.
//
// Guidelines are emitted in tool order; Build dedupes them.
func FromTools(tools []agent.Tool) (names []string, snippets map[string]string, guides []string) {
	snippets = map[string]string{}
	for _, t := range tools {
		d := t.Def()
		names = append(names, d.Name)
		if d.PromptSnippet != "" {
			snippets[d.Name] = d.PromptSnippet
		}
		guides = append(guides, d.PromptGuidelines...)
	}
	return names, snippets, guides
}

// SortedToolNames returns tool names in a stable order, for tests and
// deterministic output.
func SortedToolNames(tools []agent.Tool) []string {
	names, _, _ := FromTools(tools)
	sort.Strings(names)
	return names
}
