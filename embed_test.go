package tau_test

import (
	"strings"
	"testing"

	"github.com/ihavespoons/tau"
	"github.com/ihavespoons/tau/changelog"
)

// The changelog is only useful if it parses, and it is edited by hand at every
// release. Checking it here turns a malformed header into a failing build
// rather than an empty /changelog.
func TestEmbeddedChangelogParses(t *testing.T) {
	entries := changelog.Parse(tau.Changelog)
	if len(entries) == 0 {
		t.Fatal("no entries parsed out of CHANGELOG.md")
	}

	// Newest first in the file, so each entry must precede the one before it.
	for i := 1; i < len(entries); i++ {
		if changelog.Compare(entries[i-1], entries[i]) <= 0 {
			t.Errorf("entry %s is not newer than the one after it, %s",
				entries[i-1].Version(), entries[i].Version())
		}
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Content, "## ") {
			t.Errorf("%s: content should start with its header, got %q", e.Version(), e.Content)
		}
		_, body, _ := strings.Cut(e.Content, "\n")
		if strings.TrimSpace(body) == "" {
			t.Errorf("%s has a header but nothing under it", e.Version())
		}
	}
}
