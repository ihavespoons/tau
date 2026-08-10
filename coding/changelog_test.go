package coding

import (
	"context"
	"strings"
	"testing"
)

func TestChangelogCommandRendersWhatTheBinaryCarries(t *testing.T) {
	ctx := context.Background()
	cs := newTestSession(t, Options{Changelog: "# Changelog\n\n## [0.2.0] - 2026-08-10\n\n- newer\n\n## [0.1.0] - 2026-07-29\n\n- older\n"})

	res, err := cs.RunCommand(ctx, "/changelog")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "newer") || !strings.Contains(res.Output, "older") {
		t.Errorf("both releases should be shown:\n%s", res.Output)
	}
	// Oldest first, so the release the user just upgraded to sits closest to
	// the prompt.
	if strings.Index(res.Output, "older") > strings.Index(res.Output, "newer") {
		t.Errorf("the newest release should come last:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "# Changelog") {
		t.Errorf("the document title is not a release:\n%s", res.Output)
	}
}

// A binary built without release notes still answers the command rather than
// failing: /changelog is a question, and "none" is an answer.
func TestChangelogCommandWithoutOne(t *testing.T) {
	cs := newTestSession(t, Options{})

	res, err := cs.RunCommand(context.Background(), "/changelog")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "No changelog entries found." {
		t.Errorf("output = %q", res.Output)
	}
}
