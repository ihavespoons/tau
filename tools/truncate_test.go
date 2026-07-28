package tools

import (
	"strings"
	"testing"
)

func TestTruncateHeadKeepsFirstLines(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5"
	got := TruncateHead(content, TruncateOptions{MaxLines: 2})

	if got.Content != "l1\nl2" {
		t.Errorf("content = %q, want the FIRST 2 lines", got.Content)
	}
	if !got.Truncated || got.TruncatedBy != "lines" {
		t.Errorf("truncated=%v by=%q", got.Truncated, got.TruncatedBy)
	}
	if got.TotalLines != 5 || got.OutputLines != 2 {
		t.Errorf("totalLines=%d outputLines=%d", got.TotalLines, got.OutputLines)
	}
}

func TestTruncateTailKeepsLastLines(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5"
	got := TruncateTail(content, TruncateOptions{MaxLines: 2})

	if got.Content != "l4\nl5" {
		t.Errorf("content = %q, want the LAST 2 lines", got.Content)
	}
	if !got.Truncated || got.TruncatedBy != "lines" {
		t.Errorf("truncated=%v by=%q", got.Truncated, got.TruncatedBy)
	}
}

func TestTruncateNoOpWhenUnderLimits(t *testing.T) {
	content := "a\nb\nc"
	for name, got := range map[string]Truncation{
		"head": TruncateHead(content, TruncateOptions{}),
		"tail": TruncateTail(content, TruncateOptions{}),
	} {
		if got.Truncated {
			t.Errorf("%s: unexpectedly truncated", name)
		}
		if got.Content != content {
			t.Errorf("%s: content = %q, want %q", name, got.Content, content)
		}
	}
}

func TestTruncateHeadByBytesStopsOnLineBoundary(t *testing.T) {
	content := "aaaa\nbbbb\ncccc"
	got := TruncateHead(content, TruncateOptions{MaxBytes: 7})

	if got.Content != "aaaa" {
		t.Errorf("content = %q, want a whole-line cut", got.Content)
	}
	if got.TruncatedBy != "bytes" {
		t.Errorf("truncatedBy = %q, want bytes", got.TruncatedBy)
	}
	if strings.Contains(got.Content, "bb") {
		t.Error("byte truncation must not emit a partial line")
	}
}

func TestTruncateHeadFirstLineTooLarge(t *testing.T) {
	got := TruncateHead(strings.Repeat("x", 100)+"\nshort", TruncateOptions{MaxBytes: 10})

	if !got.FirstLineExceedsLimit {
		t.Error("expected FirstLineExceedsLimit")
	}
	if got.Content != "" {
		t.Errorf("content = %q, want empty", got.Content)
	}
}

// Tail truncation is the one place a partial line is allowed: a single
// oversized final line still has to show its most recent bytes.
func TestTruncateTailPartialLastLine(t *testing.T) {
	got := TruncateTail(strings.Repeat("x", 100), TruncateOptions{MaxBytes: 10})

	if !got.LastLinePartial {
		t.Error("expected LastLinePartial")
	}
	if len(got.Content) > 10 {
		t.Errorf("content = %d bytes, want <= 10", len(got.Content))
	}
}

func TestTruncateTrailingNewlineNotCountedAsLine(t *testing.T) {
	got := TruncateHead("a\nb\n", TruncateOptions{})
	if got.TotalLines != 2 {
		t.Errorf("totalLines = %d, want 2 (trailing newline is not a line)", got.TotalLines)
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int]string{
		512:             "512B",
		1024:            "1.0KB",
		50 * 1024:       "50.0KB",
		2 * 1024 * 1024: "2.0MB",
	}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}
