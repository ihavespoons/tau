package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ihavespoons/tau/export"
)

// shareViewerEnv names a page that renders an exported session from a gist id.
//
// Pi points this at pi.dev by default. tau has no such service and does not
// silently hand someone's transcript to a third party, so an unset variable
// means the gist link is the whole answer: the gist holds the finished page,
// and `gh gist view --web` or a download opens it.
const shareViewerEnv = "TAU_SHARE_VIEWER_URL"

// ShareSession uploads the exported page as a secret GitHub gist and returns
// the URLs to show the user.
//
// A secret gist is unlisted, not private: anyone holding the link can read the
// whole transcript, including any file contents and command output in it.
func (s *Session) ShareSession(ctx context.Context) (string, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("GitHub CLI (gh) is not installed — install it from https://cli.github.com/")
	}
	if _, err := exec.CommandContext(ctx, gh, "auth", "status").CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("GitHub CLI is not logged in — run 'gh auth login' first")
	}

	data, err := export.FromSession(ctx, s.Session, s.exportState())
	if err != nil {
		return "", err
	}
	// Named for the gist rather than for this machine: the file name becomes
	// the gist's title, and a directory built per share keeps concurrent
	// shares from overwriting each other's upload.
	dir, err := os.MkdirTemp("", "tau-share-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	tmpFile := filepath.Join(dir, "session.html")
	if _, err := export.WriteFile(data, s.exportTheme(), export.SessionFile(s.Session), tmpFile); err != nil {
		return "", err
	}

	out, err := exec.CommandContext(ctx, gh, "gist", "create", "--public=false", tmpFile).Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("failed to create gist: %s", ghError(err))
	}

	gistURL := lastURL(string(out))
	if gistURL == "" {
		return "", errors.New("failed to parse the gist URL from gh output")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Gist: %s", gistURL)
	if viewer := strings.TrimSpace(os.Getenv(shareViewerEnv)); viewer != "" {
		if id := gistID(gistURL); id != "" {
			fmt.Fprintf(&b, "\nShare URL: %s#%s", viewer, id)
		}
	}
	b.WriteString("\nThe gist is secret but not private: anyone with the link can read the transcript.")
	return b.String(), nil
}

// ghError prefers what gh wrote to stderr over Go's "exit status 1".
func ghError(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return msg
		}
	}
	return err.Error()
}

// lastURL picks the gist URL out of gh's output, which prints the URL on its
// own line but may print other lines around it.
func lastURL(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") {
			return line
		}
	}
	return ""
}

func gistID(url string) string {
	parts := strings.Split(strings.TrimSuffix(url, "/"), "/")
	return parts[len(parts)-1]
}
