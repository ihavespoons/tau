package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ihavespoons/tau/agent"
	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/export"
	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/theme"
)

// ExportSession writes the conversation to a file and returns the path.
//
// The format follows the extension: a .jsonl path writes the current branch as
// a session file that tau can open again, anything else renders the
// self-contained HTML page. An empty path takes the default name in the
// working directory.
func (s *Session) ExportSession(ctx context.Context, path string) (string, error) {
	if strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return s.exportJSONL(ctx, path)
	}
	data, err := export.FromSession(ctx, s.Session, s.exportState())
	if err != nil {
		return "", err
	}
	return export.WriteFile(data, s.exportTheme(), export.SessionFile(s.Session), path)
}

// exportState collects what the viewer shows beside the transcript: the system
// prompt the session is running under and the tools it can call.
//
// Tool output is left for the viewer to render. Pre-rendering a tool the way
// the TUI draws it needs the entry renderers, which are registered and
// forwarded but not yet drawn from (ledger B73), so custom and extension tools
// fall back to the viewer's generic view.
func (s *Session) exportState() *export.State {
	if s.Agent == nil {
		return nil
	}
	tools := s.Agent.Tools()
	st := &export.State{
		SystemPrompt: s.Agent.SystemPrompt(),
		Tools:        make([]export.Tool, 0, len(tools)),
	}
	for _, t := range tools {
		st.Tools = append(st.Tools, exportTool(t))
	}
	return st
}

func exportTool(t agent.Tool) export.Tool {
	d := t.Def()
	return export.Tool{Name: d.Name, Description: d.Description, Parameters: d.Parameters}
}

// exportTheme resolves the configured theme for the page.
//
// The terminal is not asked which background it has: an export is a file, and
// the answer would be about the terminal that happened to run the command
// rather than the machine that opens the page. An unqualified setting is
// resolved from the environment alone.
func (s *Session) exportTheme() *theme.Theme {
	setting := ""
	if s.Settings != nil {
		setting = s.Settings.ThemeSetting()
	}
	set := theme.Discover(theme.Options{Dir: config.ThemesDir(), Paths: s.ThemePaths()})
	th, ok := set.Resolve(setting, theme.DetectBackground(nil).Mode)
	if !ok {
		return nil // Generate falls back to the built-in dark theme.
	}
	return th
}

// exportJSONL writes the current branch as a standalone session file.
//
// The header is the session's own, so the copy keeps its id, version and
// directory; only the entries are rewritten, and only their parent links.
func (s *Session) exportJSONL(ctx context.Context, path string) (string, error) {
	sessionFile := export.SessionFile(s.Session)
	if sessionFile == "" {
		return "", export.ErrInMemory
	}
	entries, err := branchToRoot(ctx, s.Session)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", export.ErrEmpty
	}

	head, err := json.Marshal(headerOf(s.Session))
	if err != nil {
		return "", fmt.Errorf("export: encoding session header: %w", err)
	}
	lines := [][]byte{head}
	rest, err := rechain(entries)
	if err != nil {
		return "", err
	}
	lines = append(lines, rest...)

	if path == "" {
		path = strings.TrimSuffix(export.DefaultOutputPath(sessionFile), ".html") + ".jsonl"
	}
	resolved, err := export.ResolvePath(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", err
	}
	body := append(bytesJoin(lines), '\n')
	if err := os.WriteFile(resolved, body, 0o644); err != nil {
		return "", err
	}
	return resolved, nil
}

func bytesJoin(lines [][]byte) []byte {
	var out []byte
	for i, l := range lines {
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, l...)
	}
	return out
}

func headerOf(sess *session.Session) session.Header {
	if s, ok := sess.Storage().(*session.JSONLStorage); ok {
		return s.Header()
	}
	return session.Header{}
}

// branchToRoot walks parent links from the leaf all the way back, root-first.
//
// Session.Branch stops at a compaction because that is what belongs in the
// model's context. An exported file is the transcript, so the history a
// compaction summarized is part of it.
func branchToRoot(ctx context.Context, sess *session.Session) ([]session.Entry, error) {
	leaf, err := sess.LeafID(ctx)
	if err != nil {
		return nil, err
	}
	var path []session.Entry
	id := leaf
	for id != nil && *id != "" {
		e, ok := sess.Entry(ctx, *id)
		if !ok {
			return nil, fmt.Errorf("export: entry %s not found", *id)
		}
		path = append([]session.Entry{e}, path...)
		id = e.Base().ParentID
	}
	return path, nil
}

// rechain renders entries as JSONL lines whose parent links form one straight
// chain. Walking to the root already produces that, so in practice every line
// is the verbatim bytes tau read from disk; the rewrite is the fallback for a
// branch that somehow arrives with a gap in it.
func rechain(entries []session.Entry) ([][]byte, error) {
	var prev *string
	out := make([][]byte, 0, len(entries))
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("export: encoding entry %s: %w", e.Base().ID, err)
		}
		if !sameID(e.Base().ParentID, prev) {
			if line, err = patchParentID(line, prev); err != nil {
				return nil, err
			}
		}
		out = append(out, line)
		id := e.Base().ID
		prev = &id
	}
	return out, nil
}

func sameID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// patchParentID rewrites one field of an already-encoded entry. The other
// fields keep their values but not their order, which JSON does not promise
// and no reader of a session file depends on.
func patchParentID(line []byte, parent *string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return nil, fmt.Errorf("export: re-reading entry: %w", err)
	}
	v, err := json.Marshal(parent)
	if err != nil {
		return nil, err
	}
	fields["parentId"] = v
	return json.Marshal(fields)
}
