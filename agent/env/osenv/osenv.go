// Package osenv implements env.Env against the local operating system:
// the real filesystem and a real bash shell. It is the port of Pi's
// NodeExecutionEnv (packages/agent/src/harness/env/nodejs.ts).
package osenv

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ihavespoons/tau/agent/env"
)

// DefaultMaxReadBytes bounds a single Read when the caller sets no limit.
// Matches Pi's DEFAULT_MAX_BYTES (core/tools/truncate.ts).
const DefaultMaxReadBytes = 50 * 1024

// Options configures an OSEnv.
type Options struct {
	// Cwd is the working directory. Defaults to the process's.
	Cwd string
	// ShellPath overrides shell discovery (Pi's settings.shellPath).
	ShellPath string
	// Env are extra variables layered over the process environment for every
	// command (KEY=VALUE).
	Env []string
	// InheritEnv passes the process environment to commands. Default true.
	InheritEnv *bool
}

// OSEnv is an env.Env backed by the local machine.
type OSEnv struct {
	cwd       string
	shellPath string
	extraEnv  []string
	inherit   bool
}

var _ env.Env = (*OSEnv)(nil)

// New builds an OSEnv. A relative or empty Cwd resolves against the process's
// working directory.
func New(opts Options) (*OSEnv, error) {
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, env.Errorf(env.CodeIO, "", "resolving working directory: %v", err)
		}
	}
	abs, err := filepath.Abs(expandHome(cwd))
	if err != nil {
		return nil, env.Errorf(env.CodeInvalidPath, cwd, "resolving working directory: %v", err)
	}
	inherit := true
	if opts.InheritEnv != nil {
		inherit = *opts.InheritEnv
	}
	return &OSEnv{cwd: abs, shellPath: opts.ShellPath, extraEnv: opts.Env, inherit: inherit}, nil
}

// Cwd implements env.FS.
func (e *OSEnv) Cwd() string { return e.cwd }

// expandHome resolves a leading ~ to the user's home directory (Pi's
// resolvePath). A bare "~" and "~/..." are handled; "~user" is not, matching Pi.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// Abs implements env.FS. Paths resolve against the environment's Cwd; ~ is
// expanded and file:// URLs are unwrapped. Pi does not confine paths to the
// working directory, and neither do we — the sandbox boundary belongs to the
// Env implementation, not to path arithmetic.
func (e *OSEnv) Abs(path string) (string, error) {
	if path == "" {
		return "", env.Errorf(env.CodeInvalidPath, path, "empty path")
	}
	p := strings.TrimPrefix(expandHome(path), "file://")
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Join(e.cwd, p), nil
}

// toError maps an OS error to a typed env.Error.
func toError(err error, path string) error {
	if err == nil {
		return nil
	}
	var typed *env.Error
	if errors.As(err, &typed) {
		return typed
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &env.Error{Code: env.CodeNotFound, Path: path, Message: "no such file or directory", Cause: err}
	case errors.Is(err, fs.ErrPermission):
		return &env.Error{Code: env.CodePermission, Path: path, Message: "permission denied", Cause: err}
	case errors.Is(err, fs.ErrExist):
		return &env.Error{Code: env.CodeAlreadyExists, Path: path, Message: "already exists", Cause: err}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &env.Error{Code: env.CodeIO, Path: path, Message: err.Error(), Cause: err}
	}
	return &env.Error{Code: env.CodeIO, Path: path, Message: err.Error(), Cause: err}
}

func toFileInfo(path string, info fs.FileInfo) env.FileInfo {
	return env.FileInfo{
		Path:    path,
		Name:    info.Name(),
		Size:    info.Size(),
		Mode:    info.Mode(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
	}
}

// Stat implements env.FS. Like Pi's fileInfo it does not follow the final
// symlink, so a dangling link reports as a link rather than not-found.
func (e *OSEnv) Stat(ctx context.Context, path string) (env.FileInfo, error) {
	abs, err := e.Abs(path)
	if err != nil {
		return env.FileInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return env.FileInfo{}, toError(err, abs)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return env.FileInfo{}, toError(err, abs)
	}
	return toFileInfo(abs, info), nil
}

// Exists implements env.FS.
func (e *OSEnv) Exists(ctx context.Context, path string) bool {
	_, err := e.Stat(ctx, path)
	return err == nil
}

// List implements env.FS.
func (e *OSEnv) List(ctx context.Context, path string) ([]env.FileInfo, error) {
	abs, err := e.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, toError(err, abs)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, toError(err, abs)
	}
	out := make([]env.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, toError(err, abs)
		}
		info, err := entry.Info()
		if err != nil {
			// A file removed mid-scan is not a failure of the listing.
			continue
		}
		out = append(out, toFileInfo(filepath.Join(abs, entry.Name()), info))
	}
	return out, nil
}

// ReadFile implements env.FS.
func (e *OSEnv) ReadFile(ctx context.Context, path string) ([]byte, error) {
	abs, err := e.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, toError(err, abs)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, toError(err, abs)
	}
	return data, nil
}

// Write implements env.FS, creating parent directories first.
func (e *OSEnv) Write(ctx context.Context, path string, data []byte) error {
	abs, err := e.Abs(path)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return toError(err, abs)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return toError(err, abs)
	}
	if err := ctx.Err(); err != nil {
		return toError(err, abs)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return toError(err, abs)
	}
	return nil
}

// Mkdir implements env.FS.
func (e *OSEnv) Mkdir(ctx context.Context, path string) error {
	abs, err := e.Abs(path)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return toError(err, abs)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return toError(err, abs)
	}
	return nil
}

// Remove implements env.FS. It deletes a file or an empty directory; removing
// a populated directory is an error, matching Pi's non-recursive default.
func (e *OSEnv) Remove(ctx context.Context, path string) error {
	abs, err := e.Abs(path)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return toError(err, abs)
	}
	if err := os.Remove(abs); err != nil {
		return toError(err, abs)
	}
	return nil
}

// Read implements env.FS: a bounded read that classifies its own content.
//
// Images are returned as raw bytes with a MimeType; non-UTF-8 files are
// reported with Binary set rather than as mangled text; text is windowed by
// Offset/Limit and capped by MaxBytes on a line boundary.
func (e *OSEnv) Read(ctx context.Context, path string, opts env.ReadOptions) (env.ReadResult, error) {
	abs, err := e.Abs(path)
	if err != nil {
		return env.ReadResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return env.ReadResult{}, toError(err, abs)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return env.ReadResult{}, toError(err, abs)
	}
	if info.IsDir() {
		return env.ReadResult{}, env.Errorf(env.CodeNotAFile, abs, "is a directory")
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return env.ReadResult{}, toError(err, abs)
	}

	sniff := data
	if len(sniff) > imageSniffBytes {
		sniff = sniff[:imageSniffBytes]
	}
	if mime := DetectImageMimeType(sniff); mime != "" {
		return env.ReadResult{MimeType: mime, Bytes: data}, nil
	}

	if !utf8.Valid(data) {
		return env.ReadResult{Binary: true, Bytes: data}, nil
	}

	return windowText(string(data), opts), nil
}

// windowText applies Offset/Limit/MaxBytes to text content. Byte capping stops
// at a line boundary so callers never see a half-line, except when the first
// selected line alone exceeds the cap.
func windowText(text string, opts env.ReadOptions) env.ReadResult {
	lines := strings.Split(text, "\n")
	total := len(lines)

	start := 0
	if opts.Offset > 0 {
		start = opts.Offset - 1
	}
	if start >= total {
		return env.ReadResult{TotalLines: total}
	}

	end := total
	truncated := false
	if opts.Limit > 0 && start+opts.Limit < end {
		end = start + opts.Limit
		truncated = true
	}
	selected := lines[start:end]

	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxReadBytes
	}
	kept := make([]string, 0, len(selected))
	used := 0
	for i, line := range selected {
		cost := len(line)
		if i > 0 {
			cost++ // newline
		}
		if used+cost > maxBytes {
			truncated = true
			break
		}
		kept = append(kept, line)
		used += cost
	}

	return env.ReadResult{
		Content:    strings.Join(kept, "\n"),
		Truncated:  truncated,
		TotalLines: total,
	}
}
