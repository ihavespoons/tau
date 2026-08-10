package coding

import (
	"context"
	"errors"
	"fmt"

	"github.com/ihavespoons/tau/session"
)

// DeleteSession removes a session file.
//
// The current session is refused: deleting the file being appended to would
// leave the agent writing into nothing, and "start a new one first" is a
// better answer than a session that half exists.
func (s *Session) DeleteSession(ctx context.Context, meta session.Metadata) (string, error) {
	if s.repo == nil {
		return "", errors.New("this session does not persist, so there is nothing to delete")
	}
	if meta.Path == "" {
		return "", errors.New("no session to delete")
	}
	if meta.Path == s.Path {
		return "", errors.New("that is the session you are in — start a new one first")
	}
	if err := s.repo.Delete(ctx, meta); err != nil {
		return "", err
	}
	return "Deleted " + meta.Path, nil
}

// RenameSession names a session other than the one in progress.
//
// The name is appended to the session's own file rather than held anywhere
// central, which is what makes it survive being read by Pi: a name is an entry
// in the transcript, not a property of a listing.
func (s *Session) RenameSession(ctx context.Context, meta session.Metadata, name string) (string, error) {
	if s.repo == nil {
		return "", errors.New("this session does not persist, so it cannot be named")
	}
	if meta.Path == "" {
		return "", errors.New("no session to rename")
	}

	// The session in progress is already open; naming it through a second
	// handle would append behind the open one's back.
	if meta.Path == s.Path {
		if s.Session == nil {
			return "", ErrNoSession
		}
		if _, err := s.Session.AppendName(ctx, name); err != nil {
			return "", err
		}
		return "Named this session " + name, nil
	}

	sess, err := s.repo.Open(ctx, meta)
	if err != nil {
		return "", err
	}
	if _, err := sess.AppendName(ctx, name); err != nil {
		return "", fmt.Errorf("naming %s: %w", meta.Path, err)
	}
	return "Named " + meta.Path + " " + name, nil
}
