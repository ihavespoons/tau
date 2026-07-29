package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/agent/env"
	"github.com/ihavespoons/tau/ai"
)

const (
	grepDefaultLimit = 100
	findDefaultLimit = 1000
	lsDefaultLimit   = 500
	// grepMaxLineLength keeps one long minified line from filling the whole
	// result with a single match.
	grepMaxLineLength = 500
)

// GrepParams are the grep tool's arguments.
type GrepParams struct {
	Pattern    string `json:"pattern" jsonschema:"Search pattern (regex or literal string)"`
	Path       string `json:"path,omitempty" jsonschema:"Directory or file to search (default: current directory)"`
	Glob       string `json:"glob,omitempty" jsonschema:"Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'"`
	IgnoreCase bool   `json:"ignoreCase,omitempty" jsonschema:"Case-insensitive search (default: false)"`
	Literal    bool   `json:"literal,omitempty" jsonschema:"Treat pattern as a literal string instead of a regex (default: false)"`
	Context    int    `json:"context,omitempty" jsonschema:"Number of lines to show before and after each match (default: 0)"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum number of matches to return (default: 100)"`
}

// SearchDetails is the structured detail payload the search tools return.
type SearchDetails struct {
	Truncation   *Truncation `json:"truncation,omitempty"`
	LimitReached int         `json:"limitReached,omitempty"`
	LinesCut     bool        `json:"linesTruncated,omitempty"`
}

var grepDescription = fmt.Sprintf(
	"Search file contents for a pattern. Returns matching lines with file paths and line numbers. "+
		"Respects .gitignore. Output is truncated to %d matches or %dKB (whichever is hit first). "+
		"Long lines are truncated to %d chars.",
	grepDefaultLimit, DefaultMaxBytes/1024, grepMaxLineLength)

// Grep builds the grep tool.
//
// It shells out to ripgrep rather than walking the tree here. Respecting
// .gitignore, skipping binaries, and staying fast on a large repository is a
// great deal of behaviour to approximate, and approximating it badly would
// show up as searches that miss files or take a minute.
func Grep(e env.Env) agent.Tool {
	return agent.MustNew("grep", "grep", grepDescription,
		func(ctx context.Context, _ string, p GrepParams, _ agent.UpdateFunc) (agent.ToolResult, error) {
			if p.Pattern == "" {
				return agent.ToolResult{}, fmt.Errorf("pattern is required")
			}

			rg, err := ensureBinary(ctx, "rg")
			if err != nil {
				return agent.ToolResult{}, err
			}

			searchPath := resolveToCwd(p.Path, e.Cwd())
			info, err := os.Stat(searchPath)
			if err != nil {
				return agent.ToolResult{}, fmt.Errorf("path not found: %s", searchPath)
			}

			limit := p.Limit
			if limit < 1 {
				limit = grepDefaultLimit
			}

			args := []string{"--json", "--line-number", "--color=never", "--hidden"}
			if p.IgnoreCase {
				args = append(args, "--ignore-case")
			}
			if p.Literal {
				args = append(args, "--fixed-strings")
			}
			if p.Glob != "" {
				args = append(args, "--glob", p.Glob)
			}
			args = append(args, "--", p.Pattern, searchPath)

			matches, err := runRipgrep(ctx, rg, args, limit)
			if err != nil {
				return agent.ToolResult{}, err
			}

			return formatGrepResult(matches, searchPath, info.IsDir(), p.Context, limit), nil
		})
}

// grepMatch is one hit from ripgrep's JSON stream.
type grepMatch struct {
	Path string
	Line int
}

// runRipgrep collects matches, stopping once the limit is reached.
func runRipgrep(ctx context.Context, rg string, args []string, limit int) ([]grepMatch, error) {
	cmd := exec.CommandContext(ctx, rg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var matches []grepMatch
	scanner := bufio.NewScanner(stdout)
	// A single JSON line can be long when the matched line is; the default
	// 64KB token limit would abort the scan partway through a search.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
			Data struct {
				Path       struct{ Text string } `json:"path"`
				LineNumber int                   `json:"line_number"`
			} `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type != "match" || event.Data.Path.Text == "" || event.Data.LineNumber == 0 {
			continue
		}
		matches = append(matches, grepMatch{Path: event.Data.Path.Text, Line: event.Data.LineNumber})
		if len(matches) >= limit {
			break
		}
	}

	// Draining and killing rather than waiting: once the limit is reached
	// ripgrep may still have a great deal left to search, and the caller has
	// everything it asked for.
	if len(matches) >= limit {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()

	// Exit status 1 means no matches, which is a result and not a failure.
	if len(matches) == 0 && stderr.Len() > 0 && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() > 1 {
		return nil, fmt.Errorf("ripgrep: %s", strings.TrimSpace(stderr.String()))
	}
	return matches, nil
}

// formatGrepResult renders matches with their context lines.
func formatGrepResult(matches []grepMatch, searchPath string, isDir bool, contextLines, limit int) agent.ToolResult {
	if len(matches) == 0 {
		return agent.ToolResult{Content: ai.ContentList{ai.TextContent{Text: "No matches found"}}}
	}

	details := &SearchDetails{}
	if len(matches) >= limit {
		details.LimitReached = limit
	}

	cache := map[string][]string{}
	var out []string
	for _, m := range matches {
		rel := displayPath(m.Path, searchPath, isDir)
		lines := fileLines(cache, m.Path)
		if len(lines) == 0 {
			out = append(out, fmt.Sprintf("%s:%d: (unable to read file)", rel, m.Line))
			continue
		}

		start, end := m.Line, m.Line
		if contextLines > 0 {
			start = max(1, m.Line-contextLines)
			end = min(len(lines), m.Line+contextLines)
		}
		for n := start; n <= end && n <= len(lines); n++ {
			text, cut := truncateLine(strings.ReplaceAll(lines[n-1], "\r", ""))
			if cut {
				details.LinesCut = true
			}
			// A match line is marked with a colon and a context line with a
			// dash, which is grep's own convention and what the model expects.
			sep := "-"
			if n == m.Line {
				sep = ":"
			}
			out = append(out, fmt.Sprintf("%s%s%d%s %s", rel, sep, n, sep, text))
		}
	}

	text := strings.Join(out, "\n")
	truncation := TruncateHead(text, TruncateOptions{})
	if truncation.Truncated {
		details.Truncation = &truncation
	}

	return agent.ToolResult{
		Content: ai.ContentList{ai.TextContent{Text: truncation.Content}},
		Details: details,
	}
}

func fileLines(cache map[string][]string, path string) []string {
	if lines, ok := cache[path]; ok {
		return lines
	}
	content, err := os.ReadFile(path)
	if err != nil {
		cache[path] = nil
		return nil
	}
	normalized := strings.ReplaceAll(strings.ReplaceAll(string(content), "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(normalized, "\n")
	cache[path] = lines
	return lines
}

func truncateLine(s string) (string, bool) {
	if len(s) <= grepMaxLineLength {
		return s, false
	}
	return s[:grepMaxLineLength] + "…", true
}

// displayPath shortens a result path relative to the search root, so output
// is readable rather than a column of absolute paths.
func displayPath(path, searchPath string, isDir bool) string {
	if isDir {
		if rel, err := filepath.Rel(searchPath, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(path)
}

// FindParams are the find tool's arguments.
type FindParams struct {
	Pattern string `json:"pattern" jsonschema:"Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'"`
	Path    string `json:"path,omitempty" jsonschema:"Directory to search in (default: current directory)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum number of results (default: 1000)"`
}

var findDescription = fmt.Sprintf(
	"Search for files by glob pattern. Returns matching file paths relative to the search directory. "+
		"Respects .gitignore. Output is truncated to %d results or %dKB (whichever is hit first).",
	findDefaultLimit, DefaultMaxBytes/1024)

// Find builds the find tool, on fd for the same reasons grep is on ripgrep.
func Find(e env.Env) agent.Tool {
	return agent.MustNew("find", "find", findDescription,
		func(ctx context.Context, _ string, p FindParams, _ agent.UpdateFunc) (agent.ToolResult, error) {
			if p.Pattern == "" {
				return agent.ToolResult{}, fmt.Errorf("pattern is required")
			}

			fd, err := ensureBinary(ctx, "fd")
			if err != nil {
				return agent.ToolResult{}, err
			}

			searchPath := resolveToCwd(p.Path, e.Cwd())
			if info, err := os.Stat(searchPath); err != nil || !info.IsDir() {
				return agent.ToolResult{}, fmt.Errorf("path not found: %s", searchPath)
			}

			limit := p.Limit
			if limit < 1 {
				limit = findDefaultLimit
			}

			args := []string{"--glob", "--color=never", "--hidden"}
			if !insideGitRepo(searchPath) {
				// Outside a repository fd refuses to apply ignore files at all
				// unless told it is allowed to.
				args = append(args, "--no-require-git")
			}
			args = append(args, "--max-results", fmt.Sprint(limit))
			// A pattern naming a directory has to match against the whole path
			// rather than each file's base name.
			if strings.ContainsAny(p.Pattern, "/\\") {
				args = append(args, "--full-path")
			}
			args = append(args, "--", p.Pattern, searchPath)

			output, err := exec.CommandContext(ctx, fd, args...).Output()
			if err != nil {
				// fd exits non-zero on no matches, which is a result.
				if len(output) == 0 {
					return agent.ToolResult{
						Content: ai.ContentList{ai.TextContent{Text: "No files found"}},
					}, nil
				}
			}

			var results []string
			for _, line := range strings.Split(strings.TrimRight(string(output), "\n"), "\n") {
				if line == "" {
					continue
				}
				results = append(results, displayPath(line, searchPath, true))
			}
			if len(results) == 0 {
				return agent.ToolResult{Content: ai.ContentList{ai.TextContent{Text: "No files found"}}}, nil
			}

			details := &SearchDetails{}
			if len(results) >= limit {
				details.LimitReached = limit
			}
			truncation := TruncateHead(strings.Join(results, "\n"), TruncateOptions{})
			if truncation.Truncated {
				details.Truncation = &truncation
			}

			return agent.ToolResult{
				Content: ai.ContentList{ai.TextContent{Text: truncation.Content}},
				Details: details,
			}, nil
		})
}

// insideGitRepo reports whether the path sits under a git working tree.
func insideGitRepo(path string) bool {
	for dir := path; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// LsParams are the ls tool's arguments.
type LsParams struct {
	Path  string `json:"path,omitempty" jsonschema:"Directory to list (default: current directory)"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of entries to return (default: 500)"`
}

var lsDescription = fmt.Sprintf(
	"List the contents of a directory. Directories are suffixed with /. "+
		"Output is truncated to %d entries.", lsDefaultLimit)

// Ls builds the ls tool.
//
// This one needs no external binary: it is a single directory read, and
// shelling out would cost a process for something the standard library does
// directly.
func Ls(e env.Env) agent.Tool {
	return agent.MustNew("ls", "ls", lsDescription,
		func(ctx context.Context, _ string, p LsParams, _ agent.UpdateFunc) (agent.ToolResult, error) {
			if err := ctx.Err(); err != nil {
				return agent.ToolResult{}, err
			}

			dir := resolveToCwd(p.Path, e.Cwd())
			info, err := os.Stat(dir)
			if err != nil {
				return agent.ToolResult{}, fmt.Errorf("path not found: %s", dir)
			}
			if !info.IsDir() {
				return agent.ToolResult{}, fmt.Errorf("not a directory: %s", dir)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				return agent.ToolResult{}, fmt.Errorf("cannot read directory: %w", err)
			}

			limit := p.Limit
			if limit < 1 {
				limit = lsDefaultLimit
			}

			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() {
					name += "/"
				}
				names = append(names, name)
			}
			// Case-insensitive, so a listing reads the way a person would sort
			// it rather than putting every capitalised name first.
			sort.Slice(names, func(i, j int) bool {
				return strings.ToLower(names[i]) < strings.ToLower(names[j])
			})

			details := &SearchDetails{}
			if len(names) > limit {
				names = names[:limit]
				details.LimitReached = limit
			}
			if len(names) == 0 {
				return agent.ToolResult{
					Content: ai.ContentList{ai.TextContent{Text: "(empty directory)"}},
				}, nil
			}

			return agent.ToolResult{
				Content: ai.ContentList{ai.TextContent{Text: strings.Join(names, "\n")}},
				Details: details,
			}, nil
		})
}

// resolveToCwd resolves a possibly-relative path against the session's working
// directory rather than the process's, which may differ.
func resolveToCwd(path, cwd string) string {
	if path == "" {
		path = "."
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(cwd, path))
}
