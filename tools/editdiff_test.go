package tools

import (
	"strings"
	"testing"
)

func TestApplyEditsSingleReplacement(t *testing.T) {
	got, err := ApplyEdits("hello world", []Edit{{OldText: "world", NewText: "there"}}, "f.txt")
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if got.NewContent != "hello there" {
		t.Errorf("newContent = %q", got.NewContent)
	}
	if got.BaseContent != "hello world" {
		t.Errorf("baseContent = %q", got.BaseContent)
	}
}

// Multiple edits all match the ORIGINAL content, never the running result.
func TestApplyEditsMultipleAgainstOriginal(t *testing.T) {
	content := "alpha\nbeta\ngamma\n"
	got, err := ApplyEdits(content, []Edit{
		{OldText: "alpha", NewText: "ALPHA"},
		{OldText: "gamma", NewText: "GAMMA"},
	}, "f.txt")
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if got.NewContent != "ALPHA\nbeta\nGAMMA\n" {
		t.Errorf("newContent = %q", got.NewContent)
	}
}

// Edits applied out of source order must still land correctly, which is what
// the reverse-offset application guarantees.
func TestApplyEditsOutOfOrder(t *testing.T) {
	got, err := ApplyEdits("one two three", []Edit{
		{OldText: "three", NewText: "3"},
		{OldText: "one", NewText: "1"},
	}, "f.txt")
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if got.NewContent != "1 two 3" {
		t.Errorf("newContent = %q", got.NewContent)
	}
}

func TestApplyEditsErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		edits   []Edit
		wantErr string
	}{
		{
			name:    "no match",
			content: "hello",
			edits:   []Edit{{OldText: "nope", NewText: "x"}},
			wantErr: "could not find the exact text",
		},
		{
			name:    "multiple matches names the count",
			content: "dup\ndup\ndup",
			edits:   []Edit{{OldText: "dup", NewText: "x"}},
			wantErr: "found 3 occurrences",
		},
		{
			name:    "empty oldText",
			content: "hello",
			edits:   []Edit{{OldText: "", NewText: "x"}},
			wantErr: "must not be empty",
		},
		{
			name:    "no change",
			content: "hello",
			edits:   []Edit{{OldText: "hello", NewText: "hello"}},
			wantErr: "no changes made",
		},
		{
			name:    "overlapping edits",
			content: "abcdef",
			edits:   []Edit{{OldText: "abcd", NewText: "x"}, {OldText: "cdef", NewText: "y"}},
			wantErr: "overlap",
		},
		{
			name:    "indexed message for multi-edit no-match",
			content: "hello",
			edits:   []Edit{{OldText: "hello", NewText: "hi"}, {OldText: "zzz", NewText: "x"}},
			wantErr: "edits[1]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyEdits(tc.content, tc.edits, "f.txt")
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// An ambiguous edit must never be applied — silently picking one occurrence is
// how files get corrupted.
func TestApplyEditsAmbiguousLeavesContentUntouched(t *testing.T) {
	content := "x = 1\ny = 2\nx = 1\n"
	if _, err := ApplyEdits(content, []Edit{{OldText: "x = 1", NewText: "x = 99"}}, "f.txt"); err == nil {
		t.Fatal("expected ambiguity error, got success")
	}
}

func TestApplyEditsFuzzyMatching(t *testing.T) {
	tests := []struct {
		name    string
		content string
		oldText string
	}{
		{"smart double quotes", "say \u201chello\u201d now", `say "hello" now`},
		{"smart single quote", "it\u2019s here", "it's here"},
		{"em dash", "a \u2014 b", "a - b"},
		{"non-breaking space", "a\u00a0b", "a b"},
		{"trailing whitespace", "line one   \nline two", "line one\nline two"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyEdits(tc.content, []Edit{{OldText: tc.oldText, NewText: "REPLACED"}}, "f.txt")
			if err != nil {
				t.Fatalf("fuzzy match failed: %v", err)
			}
			if !strings.Contains(got.NewContent, "REPLACED") {
				t.Errorf("newContent = %q", got.NewContent)
			}
		})
	}
}

func TestApplyEditsCRLFNormalization(t *testing.T) {
	// Content is normalized to LF before matching, so an LF oldText matches a
	// CRLF file.
	got, err := ApplyEdits(normalizeToLF("a\r\nb\r\nc"), []Edit{{OldText: "a\nb", NewText: "x\ny"}}, "f.txt")
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if got.NewContent != "x\ny\nc" {
		t.Errorf("newContent = %q", got.NewContent)
	}
}

func TestLineEndingHelpers(t *testing.T) {
	if got := detectLineEnding("a\r\nb"); got != "\r\n" {
		t.Errorf("detectLineEnding(CRLF) = %q", got)
	}
	if got := detectLineEnding("a\nb"); got != "\n" {
		t.Errorf("detectLineEnding(LF) = %q", got)
	}
	if got := detectLineEnding("no newline"); got != "\n" {
		t.Errorf("detectLineEnding(none) = %q", got)
	}
	if got := restoreLineEndings("a\nb", "\r\n"); got != "a\r\nb" {
		t.Errorf("restoreLineEndings = %q", got)
	}
}

func TestStripBOM(t *testing.T) {
	prefix, text := stripBOM("\ufeffhello")
	if prefix != "\ufeff" || text != "hello" {
		t.Errorf("stripBOM = (%q, %q)", prefix, text)
	}
	prefix, text = stripBOM("hello")
	if prefix != "" || text != "hello" {
		t.Errorf("stripBOM(no bom) = (%q, %q)", prefix, text)
	}
}

func TestGenerateDiff(t *testing.T) {
	got := GenerateDiff("a\nb\nc", "a\nB\nc")

	if got.FirstChangedLine != 2 {
		t.Errorf("firstChangedLine = %d, want 2", got.FirstChangedLine)
	}
	if !strings.Contains(got.Diff, "-2 b") {
		t.Errorf("diff missing removal line:\n%s", got.Diff)
	}
	if !strings.Contains(got.Diff, "+2 B") {
		t.Errorf("diff missing addition line:\n%s", got.Diff)
	}
}

func TestGenerateUnifiedPatch(t *testing.T) {
	got := GenerateUnifiedPatch("f.txt", "a\nb\nc", "a\nB\nc")

	for _, want := range []string{"--- f.txt", "+++ f.txt", "@@", "-b", "+B"} {
		if !strings.Contains(got, want) {
			t.Errorf("patch missing %q:\n%s", want, got)
		}
	}
}
