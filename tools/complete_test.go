package tools

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func completionTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{
		"main.go",
		"README.md",
		".hidden",
		"internal/parser.go",
		"internal/parser_test.go",
		"internal/deep/nested.go",
	} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The result differs depending on whether fd is installed — recursive with it,
// one directory without — so the assertions are about what must be true either
// way rather than the exact list.
func TestFileMatchesFindsWhatWasTyped(t *testing.T) {
	root := completionTree(t)

	got := FileMatches(context.Background(), root, "main", 20)
	if !slices.Contains(got, "main.go") {
		t.Errorf("completions for \"main\" = %v, want main.go among them", got)
	}
}

func TestFileMatchesCompletesInsideADirectory(t *testing.T) {
	root := completionTree(t)

	got := FileMatches(context.Background(), root, "internal/pars", 20)
	if !slices.Contains(got, "internal/parser.go") {
		t.Errorf("completions = %v, want internal/parser.go", got)
	}
	for _, p := range got {
		if p == "main.go" {
			t.Errorf("a path outside the typed directory came back: %v", got)
		}
	}
}

func TestAnEmptyPrefixListsTheRoot(t *testing.T) {
	root := completionTree(t)

	got := FileMatches(context.Background(), root, "", 20)
	if len(got) == 0 {
		t.Fatal("an empty prefix offered nothing")
	}
	if !slices.Contains(got, "README.md") && !slices.Contains(got, "main.go") {
		t.Errorf("completions = %v, want the files in the root", got)
	}
}

// Every completion in a repository root would otherwise start with .git.
func TestDotfilesNeedAskingFor(t *testing.T) {
	root := completionTree(t)

	if got := dirMatches(root, "", 20); slices.Contains(got, ".hidden") {
		t.Errorf("a dotfile was offered unasked: %v", got)
	}
	if got := dirMatches(root, ".hid", 20); !slices.Contains(got, ".hidden") {
		t.Errorf("completions for \".hid\" = %v, want the dotfile", got)
	}
}

func TestDirectoriesAreMarkedSoTheNextSegmentCanBeTyped(t *testing.T) {
	root := completionTree(t)

	got := dirMatches(root, "inter", 20)
	if !slices.Contains(got, "internal/") {
		t.Errorf("completions = %v, want internal/ with its slash", got)
	}
}

// What was typed is usually the start of a file's name, and a reader should not
// have to walk past three deep matches to reach it.
func TestRankingPutsNamePrefixesFirst(t *testing.T) {
	got := rank([]string{
		"internal/deep/xparser.go",
		"parser.go",
		"internal/parser_test.go",
	}, "parser", 10)

	if got[0] != "parser.go" {
		t.Errorf("ranked %v, want the name-prefix match first", got)
	}
	if got[len(got)-1] != "internal/deep/xparser.go" {
		t.Errorf("ranked %v, want the mid-name match last", got)
	}
}

func TestSplittingATypedPath(t *testing.T) {
	for _, tc := range []struct{ in, dir, base string }{
		{"main", "", "main"},
		{"internal/pars", "internal/", "pars"},
		{"internal/", "internal/", ""},
	} {
		dir, base := path2(tc.in)
		if dir != tc.dir || base != tc.base {
			t.Errorf("path2(%q) = %q,%q want %q,%q", tc.in, dir, base, tc.dir, tc.base)
		}
	}
}
