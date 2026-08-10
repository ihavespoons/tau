package export

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ihavespoons/tau/session"
	"github.com/ihavespoons/tau/theme"
)

// ErrInMemory is returned when there is no session file to export. A session
// that was never given a path exists only in this process.
var ErrInMemory = errors.New("cannot export in-memory session to HTML")

// ErrEmpty is returned when the session has no entries yet.
var ErrEmpty = errors.New("nothing to export yet - start a conversation first")

// State is the part of the agent's state worth showing alongside the
// transcript. All of it is optional: a session exported from a file has none of
// it, and the viewer hides the panels it has no data for.
type State struct {
	SystemPrompt string
	Tools        []Tool
	// RenderedTools is HTML pre-rendered by extension tool renderers, keyed by
	// tool-call id. Leave nil to let the viewer draw everything itself.
	RenderedTools map[string]RenderedTool
}

// FromSession collects a live session into exportable form.
func FromSession(ctx context.Context, sess *session.Session, state *State) (SessionData, error) {
	path := SessionFile(sess)
	if path == "" {
		return SessionData{}, ErrInMemory
	}
	entries := sess.Entries(ctx, nil)
	if len(entries) == 0 {
		return SessionData{}, ErrEmpty
	}
	leaf, err := sess.LeafID(ctx)
	if err != nil {
		return SessionData{}, fmt.Errorf("export: reading session leaf: %w", err)
	}

	data := SessionData{Header: header(sess), Entries: entries, LeafID: leaf}
	if state != nil {
		data.SystemPrompt = state.SystemPrompt
		data.Tools = state.Tools
		if len(state.RenderedTools) > 0 {
			data.RenderedTools = state.RenderedTools
		}
	}
	return data, nil
}

// FromFile reads a session file. Unlike [FromSession] there is no agent state
// to go with it, so the viewer shows the transcript alone.
func FromFile(ctx context.Context, path string) (SessionData, error) {
	resolved, err := ResolvePath(path)
	if err != nil {
		return SessionData{}, err
	}
	if _, err := os.Stat(resolved); err != nil {
		if os.IsNotExist(err) {
			return SessionData{}, fmt.Errorf("file not found: %s", resolved)
		}
		return SessionData{}, err
	}
	storage, err := session.OpenJSONLReadOnly(resolved)
	if err != nil {
		return SessionData{}, err
	}
	sess := session.NewSession(storage)
	leaf, err := sess.LeafID(ctx)
	if err != nil {
		return SessionData{}, fmt.Errorf("export: reading session leaf: %w", err)
	}
	return SessionData{
		Header:  storage.Header(),
		Entries: sess.Entries(ctx, nil),
		LeafID:  leaf,
	}, nil
}

// SessionFile returns the file backing a session, or "" when it has none.
func SessionFile(sess *session.Session) string {
	if s, ok := sess.Storage().(*session.JSONLStorage); ok {
		return s.Path()
	}
	return ""
}

func header(sess *session.Session) session.Header {
	if s, ok := sess.Storage().(*session.JSONLStorage); ok {
		return s.Header()
	}
	return session.Header{}
}

// DefaultOutputPath names the file an export lands in when the user did not
// choose one: tau-session-<session id>.html in the working directory.
func DefaultOutputPath(sessionFile string) string {
	base := strings.TrimSuffix(filepath.Base(sessionFile), ".jsonl")
	return "tau-session-" + base + ".html"
}

// ResolvePath expands a leading ~ and makes the path absolute.
func ResolvePath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	}
	return filepath.Abs(path)
}

// WriteFile renders data and writes it to outputPath, defaulting the name from
// the session file when outputPath is empty. It returns the path written.
func WriteFile(data SessionData, th *theme.Theme, sessionFile, outputPath string) (string, error) {
	html, err := Generate(data, th)
	if err != nil {
		return "", err
	}
	if outputPath == "" {
		outputPath = DefaultOutputPath(sessionFile)
	}
	resolved, err := ResolvePath(outputPath)
	if err != nil {
		return "", err
	}
	// A path into a directory that does not exist yet is a reasonable thing to
	// type, so make it rather than refusing.
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(resolved, []byte(html), 0o644); err != nil {
		return "", err
	}
	return resolved, nil
}
