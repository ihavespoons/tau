package changelog

import (
	"strings"
	"testing"
)

const sample = `# Changelog

Prose above the first release, which belongs to no entry.

## [0.2.0] - 2026-08-10

- Second release.
- See [the tools package](tools/) and [bash.go](tools/bash.go).

## [0.1.0] - 2026-07-29

- First release.
`

func TestParseCollectsReleasesInDocumentOrder(t *testing.T) {
	entries := Parse(sample)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	if got := entries[0].Version(); got != "0.2.0" {
		t.Errorf("first entry = %s, want 0.2.0 (document order, newest first)", got)
	}
	if got := entries[1].Version(); got != "0.1.0" {
		t.Errorf("second entry = %s, want 0.1.0", got)
	}
	if !strings.HasPrefix(entries[0].Content, "## [0.2.0]") {
		t.Errorf("content should keep its header line, got %q", entries[0].Content)
	}
	if strings.Contains(entries[0].Content, "Prose above") {
		t.Error("prose under the document title leaked into an entry")
	}
	if strings.HasSuffix(entries[1].Content, "\n") {
		t.Error("entry content should be trimmed")
	}
}

// A `##` line that is not a version ends the entry before it and swallows what
// follows, which is what keeps unrelated sections out of the output.
func TestParseDropsSectionsWithoutAVersion(t *testing.T) {
	entries := Parse("## [1.0.0]\n- shipped\n\n## Notes\n- not a release\n")
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if strings.Contains(entries[0].Content, "not a release") {
		t.Errorf("the Notes section leaked in: %q", entries[0].Content)
	}
}

func TestParseAcceptsUnbracketedHeaders(t *testing.T) {
	entries := Parse("## 1.2.3 — a release\n- ok\n")
	if len(entries) != 1 || entries[0].Version() != "1.2.3" {
		t.Fatalf("got %+v, want a single 1.2.3 entry", entries)
	}
}

func TestParseIgnoresAnEmptyDocument(t *testing.T) {
	if got := Parse(""); len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

func TestCompareOrdersByComponent(t *testing.T) {
	cases := []struct {
		a, b string
		want int // sign
	}{
		{"1.0.0", "0.9.9", +1},
		{"0.9.9", "1.0.0", -1},
		{"0.19.0", "0.9.0", +1},
		{"0.1.2", "0.1.2", 0},
		{"0.1.10", "0.1.9", +1},
	}
	for _, c := range cases {
		got := Compare(parseVersion(c.a), parseVersion(c.b))
		if sign(got) != c.want {
			t.Errorf("Compare(%s, %s) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNewerFiltersReleasedSince(t *testing.T) {
	entries := Parse(sample)

	got := Newer(entries, "0.1.0")
	if len(got) != 1 || got[0].Version() != "0.2.0" {
		t.Errorf("Newer(0.1.0) = %+v, want just 0.2.0", got)
	}
	if got := Newer(entries, "v0.2.0"); len(got) != 0 {
		t.Errorf("Newer(v0.2.0) = %+v, want nothing (leading v accepted)", got)
	}
	// A truncated version reads its missing components as zero.
	if got := Newer(entries, "0.1"); len(got) != 1 {
		t.Errorf("Newer(0.1) = %+v, want 0.2.0", got)
	}
}

func TestRenderPutsTheNewestReleaseLast(t *testing.T) {
	out := Render(sample)
	if strings.Index(out, "0.1.0") > strings.Index(out, "0.2.0") {
		t.Errorf("the newest release should end up at the bottom:\n%s", out)
	}
}

func TestRenderSaysSoWhenThereIsNothing(t *testing.T) {
	if got := Render("# Changelog\n\nNothing yet.\n"); got != "No changelog entries found." {
		t.Errorf("Render = %q", got)
	}
}

func TestRenderPinsLinksToTheirOwnRelease(t *testing.T) {
	out := Render(sample)
	if !strings.Contains(out, "https://github.com/ihavespoons/tau/blob/v0.2.0/tools/bash.go") {
		t.Errorf("a file link was not pinned to its entry's tag:\n%s", out)
	}
	if !strings.Contains(out, "https://github.com/ihavespoons/tau/tree/v0.2.0/tools/") {
		t.Errorf("a directory link should use the tree route:\n%s", out)
	}
}

func TestNormalizeLinks(t *testing.T) {
	const tag = "v0.5.0"
	base := "https://github.com/ihavespoons/tau"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "relative file",
			in:   "[x](coding/session.go)",
			want: "[x](" + base + "/blob/" + tag + "/coding/session.go)",
		},
		{
			name: "repository-absolute path",
			in:   "[x](/README.md)",
			want: "[x](" + base + "/blob/" + tag + "/README.md)",
		},
		{
			name: "directory by trailing slash",
			in:   "[x](tools/)",
			want: "[x](" + base + "/tree/" + tag + "/tools/)",
		},
		{
			name: "extensionless path is taken for a directory",
			in:   "[x](extensions/mcp)",
			want: "[x](" + base + "/tree/" + tag + "/extensions/mcp)",
		},
		{
			name: "a branch URL is pinned to the tag",
			in:   "[x](" + base + "/blob/main/tools/bash.go)",
			want: "[x](" + base + "/blob/" + tag + "/tools/bash.go)",
		},
		{
			name: "an external URL is untouched",
			in:   "[x](https://example.com/a.go)",
			want: "[x](https://example.com/a.go)",
		},
		{
			name: "a page anchor is untouched",
			in:   "[x](#usage)",
			want: "[x](#usage)",
		},
		{
			name: "a path escaping the repository is untouched",
			in:   "[x](../../etc/passwd)",
			want: "[x](../../etc/passwd)",
		},
		{
			name: "query and fragment survive",
			in:   "[x](coding/session.go?plain=1#L12)",
			want: "[x](" + base + "/blob/" + tag + "/coding/session.go?plain=1#L12)",
		},
		{
			name: "a link title survives",
			in:   `[x](README.md "the readme")`,
			want: `[x](` + base + `/blob/` + tag + `/README.md "the readme")`,
		},
		{
			name: "images are rewritten too",
			in:   "![x](docs/shot.png)",
			want: "![x](" + base + "/blob/" + tag + "/docs/shot.png)",
		},
		{
			// Markdown ends the target at the first space and reads the rest
			// as a title, so a path with a space in it never reaches the
			// rewriter whole. Nothing to fix — just worth pinning.
			name: "a space ends the target",
			in:   "[x](docs/a b.md)",
			want: "[x](" + base + "/tree/" + tag + "/docs/a b.md)",
		},
		{
			name: "an already-escaped path is left as it is",
			in:   "[x](docs/a%20b.md)",
			want: "[x](" + base + "/blob/" + tag + "/docs/a%20b.md)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeLinks(c.in, tag); got != c.want {
				t.Errorf("NormalizeLinks(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// The tag argument is accepted with or without its v, because callers hold
// versions in both shapes.
func TestNormalizeLinksAcceptsABareVersion(t *testing.T) {
	got := NormalizeLinks("[x](README.md)", "0.5.0")
	if !strings.Contains(got, "/blob/v0.5.0/") {
		t.Errorf("got %q, want a v-prefixed tag", got)
	}
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}
