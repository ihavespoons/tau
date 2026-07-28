// Package env is the capability abstraction tools execute against: a
// filesystem and a shell. Swapping the implementation is how tau supports
// sandboxes, remote execution, and tests without touching tool code (Pi's
// FileSystem/Shell/ExecutionEnv split).
package env

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// Code classifies a filesystem or shell failure.
type Code string

const (
	CodeNotFound       Code = "not_found"
	CodeNotAFile       Code = "not_a_file"
	CodeNotADirectory  Code = "not_a_directory"
	CodeAlreadyExists  Code = "already_exists"
	CodePermission     Code = "permission"
	CodeInvalidPath    Code = "invalid_path"
	CodeTooLarge       Code = "too_large"
	CodeNotUTF8        Code = "not_utf8"
	CodeIO             Code = "io"
	CodeUnsupported    Code = "unsupported"
	CodeSpawnFailed    Code = "spawn_failed"
	CodeInvalidCommand Code = "invalid_command"
)

// Error is a typed filesystem/shell failure carrying the offending path.
type Error struct {
	Code    Code
	Path    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("%s: %s", e.Path, e.Message)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

// Errorf builds a typed error.
func Errorf(code Code, path string, format string, args ...any) *Error {
	return &Error{Code: code, Path: path, Message: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// FileInfo describes a filesystem entry.
type FileInfo struct {
	Path    string
	Name    string
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
	IsDir   bool
}

// ReadOptions bounds a text read.
type ReadOptions struct {
	// Offset is the 1-based first line to return (0 means from the start).
	Offset int
	// Limit caps the number of lines returned (0 means no line cap).
	Limit int
	// MaxBytes caps the bytes read (0 means the implementation default).
	MaxBytes int
}

// ReadResult is the outcome of a bounded text read.
type ReadResult struct {
	Content string
	// Truncated reports whether the content was cut short by a limit.
	Truncated bool
	// TotalLines in the file, when known.
	TotalLines int
	// Binary reports that the file is not valid UTF-8 text.
	Binary bool
	// MimeType is set for images read as bytes.
	MimeType string
	// Bytes carries raw content for binary/image reads.
	Bytes []byte
}

// FS is the filesystem capability. Implementations must not panic; failures
// come back as *Error.
type FS interface {
	// Read returns bounded text (or image bytes) for a file.
	Read(ctx context.Context, path string, opts ReadOptions) (ReadResult, error)
	// ReadFile returns a file's full contents.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// Write replaces a file's contents, creating parent directories.
	Write(ctx context.Context, path string, data []byte) error
	// Stat describes one entry.
	Stat(ctx context.Context, path string) (FileInfo, error)
	// List returns the entries of a directory (non-recursive).
	List(ctx context.Context, path string) ([]FileInfo, error)
	// Mkdir creates a directory and its parents.
	Mkdir(ctx context.Context, path string) error
	// Remove deletes a file or empty directory.
	Remove(ctx context.Context, path string) error
	// Exists reports whether a path exists.
	Exists(ctx context.Context, path string) bool
	// Abs resolves a path against the environment's working directory.
	Abs(path string) (string, error)
	// Cwd is the working directory.
	Cwd() string
}

// ExecOptions configures a shell execution.
type ExecOptions struct {
	// Cwd overrides the working directory for this command.
	Cwd string
	// Env are additional environment variables (KEY=VALUE).
	Env []string
	// Timeout bounds the command (0 means the implementation default).
	Timeout time.Duration
	// MaxOutputBytes caps captured output; excess spills to a temp file.
	MaxOutputBytes int
	// Stdin is fed to the command when non-empty.
	Stdin string
	// OnOutput streams captured output as it arrives.
	OnOutput func(chunk string)
}

// ExecResult is the outcome of a command. A non-zero exit code is data, not
// an error: only spawn/environment failures return an error.
type ExecResult struct {
	Output   string
	ExitCode int
	// TimedOut reports that the command was killed by the timeout.
	TimedOut bool
	// Cancelled reports that the context was cancelled.
	Cancelled bool
	// Truncated reports that Output was cut to MaxOutputBytes.
	Truncated bool
	// FullOutputPath is a temp file holding the complete output when truncated.
	FullOutputPath string
	Duration       time.Duration
}

// Shell is the command-execution capability.
type Shell interface {
	Exec(ctx context.Context, command string, opts ExecOptions) (ExecResult, error)
}

// Env is the full capability set a tool executes against.
type Env interface {
	FS
	Shell
}
