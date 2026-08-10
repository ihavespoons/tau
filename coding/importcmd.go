package coding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ihavespoons/tau/config"
	"github.com/ihavespoons/tau/session"
)

// ImportSession adopts a session file and continues the conversation in it.
//
// The file is copied into this directory's session folder before it is opened,
// which is what makes an imported transcript show up in /resume afterwards.
// It also means further turns are appended to tau's copy rather than to the
// file that was handed over — importing something out of a shared directory
// should not start writing to it.
//
// This is the other half of `/export <path>.jsonl`. `tau import` is a
// different thing entirely: that adopts a whole Pi installation.
func (s *Session) ImportSession(ctx context.Context, path string) (string, error) {
	if s.repo == nil {
		return "", errors.New("this session does not persist, so there is nowhere to import into")
	}

	src, err := s.resolveImportPath(path)
	if err != nil {
		return "", err
	}
	// Read the header first. A file that is not a session has to be refused
	// before anything has been copied anywhere.
	if _, err := session.LoadJSONLMetadata(src); err != nil {
		return "", fmt.Errorf("%s is not a session file: %w", src, err)
	}

	dst := filepath.Join(config.SessionsDir(), session.EncodeCwd(s.Cwd), filepath.Base(src))
	copied := false
	if dst != src {
		if _, err := os.Stat(dst); err == nil {
			return "", fmt.Errorf("%s already holds a session by that name — rename the file being imported", dst)
		}
		if err := copyFile(src, dst); err != nil {
			return "", err
		}
		copied = true
	}

	err = s.replaceSession(ctx, func() (*session.Session, session.Metadata, error) {
		sess, oerr := s.repo.Open(ctx, session.Metadata{Path: dst, Cwd: s.Cwd})
		if oerr != nil {
			return nil, session.Metadata{}, oerr
		}
		meta, merr := sess.Metadata(ctx)
		if merr != nil {
			return nil, session.Metadata{}, merr
		}
		if meta.Path == "" {
			meta.Path = dst
		}
		return sess, meta, nil
	}, dst)
	if err != nil {
		// A half-import would leave the next attempt failing on a name that
		// only exists because this one did not work.
		if copied {
			_ = os.Remove(dst)
		}
		return "", err
	}

	out := "Imported " + src
	if copied {
		out += "\nCopied to " + s.Path
	}
	return out, nil
}

// resolveImportPath turns what was typed into an absolute path. A relative one
// is read against the session's directory rather than the process's, which are
// the same thing until an embedder makes them different.
func (s *Session) resolveImportPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("/import needs a path to a session .jsonl file")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path[1:], "/"))
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.Cwd, path)
	}
	path = filepath.Clean(path)

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a session file", path)
	}
	return path, nil
}

// copyFile writes src to dst, creating the parent directory. It refuses to
// overwrite: the caller has already decided the destination is free, and a
// session file is not something to clobber on a race.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copying to %s: %w", dst, err)
	}
	return out.Close()
}
