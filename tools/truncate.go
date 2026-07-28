package tools

import (
	"fmt"
	"strings"
)

// Truncation limits, ported from Pi's core/tools/truncate.ts. Two independent
// limits apply and whichever is hit first wins.
const (
	DefaultMaxLines = 2000
	DefaultMaxBytes = 50 * 1024
)

// Truncation describes the outcome of a truncation pass. It is surfaced in
// tool Details so the UI can explain what was cut.
type Truncation struct {
	Content   string `json:"-"`
	Truncated bool   `json:"truncated"`
	// TruncatedBy is "lines", "bytes", or "" when nothing was cut.
	TruncatedBy string `json:"truncatedBy,omitempty"`
	TotalLines  int    `json:"totalLines"`
	TotalBytes  int    `json:"totalBytes"`
	OutputLines int    `json:"outputLines"`
	OutputBytes int    `json:"outputBytes"`
	// LastLinePartial marks the tail-truncation edge case where a single
	// oversized line had to be cut mid-line.
	LastLinePartial bool `json:"lastLinePartial,omitempty"`
	// FirstLineExceedsLimit marks head truncation where line one alone blows
	// the byte budget, so nothing could be returned.
	FirstLineExceedsLimit bool `json:"firstLineExceedsLimit,omitempty"`
	MaxLines              int  `json:"maxLines"`
	MaxBytes              int  `json:"maxBytes"`
}

// TruncateOptions overrides the default limits.
type TruncateOptions struct {
	MaxLines int
	MaxBytes int
}

func (o TruncateOptions) limits() (int, int) {
	maxLines, maxBytes := o.MaxLines, o.MaxBytes
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return maxLines, maxBytes
}

// splitLinesForCounting splits content into lines, ignoring a single trailing
// newline so "a\n" counts as one line rather than two.
func splitLinesForCounting(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// FormatSize renders a byte count the way Pi does in truncation notices.
func FormatSize(bytes int) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	}
}

// TruncateHead keeps the FIRST N lines/bytes — the right choice for file
// reads, where the beginning is what you want. Never returns a partial line:
// if line one alone exceeds the byte limit it returns empty content with
// FirstLineExceedsLimit set.
func TruncateHead(content string, opts TruncateOptions) Truncation {
	maxLines, maxBytes := opts.limits()
	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	base := Truncation{
		TotalLines: totalLines, TotalBytes: totalBytes,
		MaxLines: maxLines, MaxBytes: maxBytes,
	}

	if totalLines <= maxLines && totalBytes <= maxBytes {
		base.Content = content
		base.OutputLines = totalLines
		base.OutputBytes = totalBytes
		return base
	}

	if totalLines > 0 && len(lines[0]) > maxBytes {
		base.Truncated = true
		base.TruncatedBy = "bytes"
		base.FirstLineExceedsLimit = true
		return base
	}

	kept := make([]string, 0, min(totalLines, maxLines))
	used := 0
	truncatedBy := "lines"
	for i := 0; i < len(lines) && i < maxLines; i++ {
		cost := len(lines[i])
		if i > 0 {
			cost++ // newline
		}
		if used+cost > maxBytes {
			truncatedBy = "bytes"
			break
		}
		kept = append(kept, lines[i])
		used += cost
	}
	if len(kept) >= maxLines && used <= maxBytes {
		truncatedBy = "lines"
	}

	out := strings.Join(kept, "\n")
	base.Content = out
	base.Truncated = true
	base.TruncatedBy = truncatedBy
	base.OutputLines = len(kept)
	base.OutputBytes = len(out)
	return base
}

// TruncateTail keeps the LAST N lines/bytes — the right choice for command
// output, where errors and final results live at the end. May return a partial
// first line when the final line alone exceeds the byte limit.
func TruncateTail(content string, opts TruncateOptions) Truncation {
	maxLines, maxBytes := opts.limits()
	totalBytes := len(content)
	lines := splitLinesForCounting(content)
	totalLines := len(lines)

	base := Truncation{
		TotalLines: totalLines, TotalBytes: totalBytes,
		MaxLines: maxLines, MaxBytes: maxBytes,
	}

	if totalLines <= maxLines && totalBytes <= maxBytes {
		base.Content = content
		base.OutputLines = totalLines
		base.OutputBytes = totalBytes
		return base
	}

	var kept []string
	used := 0
	truncatedBy := "lines"
	lastLinePartial := false
	for i := len(lines) - 1; i >= 0 && len(kept) < maxLines; i-- {
		cost := len(lines[i])
		if len(kept) > 0 {
			cost++ // newline
		}
		if used+cost > maxBytes {
			truncatedBy = "bytes"
			// Edge case: nothing kept yet and this line alone is too big —
			// keep its tail so the caller still sees the most recent output.
			if len(kept) == 0 {
				partial := tailBytes(lines[i], maxBytes)
				kept = append([]string{partial}, kept...)
				used = len(partial)
				lastLinePartial = true
			}
			break
		}
		kept = append([]string{lines[i]}, kept...)
		used += cost
	}
	if len(kept) >= maxLines && used <= maxBytes {
		truncatedBy = "lines"
	}

	out := strings.Join(kept, "\n")
	base.Content = out
	base.Truncated = true
	base.TruncatedBy = truncatedBy
	base.OutputLines = len(kept)
	base.OutputBytes = len(out)
	base.LastLinePartial = lastLinePartial
	return base
}

// tailBytes keeps the last maxBytes of s, advancing to a UTF-8 boundary so a
// multi-byte rune is never split.
func tailBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && s[start]&0xc0 == 0x80 {
		start++
	}
	return s[start:]
}
