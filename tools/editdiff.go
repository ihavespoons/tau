package tools

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Edit is one exact-text replacement.
type Edit struct {
	OldText string `json:"oldText" jsonschema:"Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."`
	NewText string `json:"newText" jsonschema:"Replacement text for this targeted edit."`
}

// bom is the UTF-8 byte order mark (U+FEFF), written as an escape because a
// literal BOM mid-file is not valid Go source.
const bom = "\ufeff"

// stripBOM splits a leading UTF-8 BOM off the content. The model never
// includes an invisible BOM in oldText, so matching happens without it and it
// is restored on write.
func stripBOM(content string) (prefix, text string) {
	if strings.HasPrefix(content, bom) {
		return bom, strings.TrimPrefix(content, bom)
	}
	return "", content
}

// detectLineEnding reports the file's dominant line ending by looking at which
// style appears first.
func detectLineEnding(content string) string {
	crlf := strings.Index(content, "\r\n")
	lf := strings.Index(content, "\n")
	if lf == -1 || crlf == -1 {
		return "\n"
	}
	if crlf < lf {
		return "\r\n"
	}
	return "\n"
}

func normalizeToLF(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

func restoreLineEndings(text, ending string) string {
	if ending == "\r\n" {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

// normalizeForFuzzyMatch makes text robust to the cosmetic differences that
// cause spurious edit failures: NFKC form, trailing whitespace, smart quotes,
// Unicode dashes, and exotic spaces. Port of Pi's normalizeForFuzzyMatch.
func normalizeForFuzzyMatch(text string) string {
	text = norm.NFKC.String(text)

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	text = strings.Join(lines, "\n")

	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch r {
		case '\u2018', '\u2019', '\u201a', '\u201b':
			b.WriteByte('\'')
		case '\u201c', '\u201d', '\u201e', '\u201f':
			b.WriteByte('"')
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			b.WriteByte('-')
		case '\u00a0', '\u202f', '\u205f', '\u3000':
			b.WriteByte(' ')
		default:
			if r >= '\u2002' && r <= '\u200a' {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

type matchedEdit struct {
	editIndex   int
	matchIndex  int
	matchLength int
	newText     string
}

// AppliedEdits is the before/after pair produced by applying edits.
type AppliedEdits struct {
	BaseContent string
	NewContent  string
}

// ApplyEdits applies exact-text replacements to LF-normalized content.
//
// Every edit is matched against the *same original content*, never against the
// result of earlier edits, then replacements are applied in reverse offset
// order so positions stay valid. An edit that matches zero or more than one
// place is an error naming the count — silently applying an ambiguous edit is
// how files get corrupted.
func ApplyEdits(normalizedContent string, edits []Edit, path string) (AppliedEdits, error) {
	total := len(edits)
	normalized := make([]Edit, total)
	for i, e := range edits {
		normalized[i] = Edit{OldText: normalizeToLF(e.OldText), NewText: normalizeToLF(e.NewText)}
		if normalized[i].OldText == "" {
			return AppliedEdits{}, emptyOldTextError(path, i, total)
		}
	}

	// If any edit only matches after fuzzy normalization, the whole operation
	// runs in normalized space so offsets stay consistent.
	usedFuzzy := false
	for _, e := range normalized {
		if !strings.Contains(normalizedContent, e.OldText) {
			usedFuzzy = true
			break
		}
	}
	base := normalizedContent
	if usedFuzzy {
		base = normalizeForFuzzyMatch(normalizedContent)
	}

	matches := make([]matchedEdit, 0, total)
	for i, e := range normalized {
		needle := e.OldText
		if usedFuzzy {
			needle = normalizeForFuzzyMatch(needle)
		}
		idx := strings.Index(base, needle)
		if idx == -1 {
			return AppliedEdits{}, notFoundError(path, i, total)
		}
		if occurrences := strings.Count(base, needle); occurrences > 1 {
			return AppliedEdits{}, duplicateError(path, i, total, occurrences)
		}
		matches = append(matches, matchedEdit{
			editIndex: i, matchIndex: idx, matchLength: len(needle), newText: e.NewText,
		})
	}

	sort.Slice(matches, func(a, b int) bool { return matches[a].matchIndex < matches[b].matchIndex })
	for i := 1; i < len(matches); i++ {
		prev, cur := matches[i-1], matches[i]
		if prev.matchIndex+prev.matchLength > cur.matchIndex {
			return AppliedEdits{}, fmt.Errorf(
				"edits[%d] and edits[%d] overlap in %s. Merge them into one edit or target disjoint regions",
				prev.editIndex, cur.editIndex, path)
		}
	}

	newContent := applyReplacements(base, matches)
	if normalizedContent == newContent {
		return AppliedEdits{}, noChangeError(path, total)
	}
	return AppliedEdits{BaseContent: normalizedContent, NewContent: newContent}, nil
}

// applyReplacements rewrites matches back-to-front so earlier offsets stay valid.
func applyReplacements(content string, matches []matchedEdit) string {
	var b strings.Builder
	prev := 0
	for _, m := range matches {
		b.WriteString(content[prev:m.matchIndex])
		b.WriteString(m.newText)
		prev = m.matchIndex + m.matchLength
	}
	b.WriteString(content[prev:])
	return b.String()
}

func notFoundError(path string, i, total int) error {
	if total == 1 {
		return fmt.Errorf("could not find the exact text in %s. The old text must match exactly including all whitespace and newlines", path)
	}
	return fmt.Errorf("could not find edits[%d] in %s. The oldText must match exactly including all whitespace and newlines", i, path)
}

func duplicateError(path string, i, total, occurrences int) error {
	if total == 1 {
		return fmt.Errorf("found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique", occurrences, path)
	}
	return fmt.Errorf("found %d occurrences of edits[%d] in %s. Each oldText must be unique. Please provide more context to make it unique", occurrences, i, path)
}

func emptyOldTextError(path string, i, total int) error {
	if total == 1 {
		return fmt.Errorf("oldText must not be empty in %s", path)
	}
	return fmt.Errorf("edits[%d].oldText must not be empty in %s", i, path)
}

func noChangeError(path string, total int) error {
	if total == 1 {
		return fmt.Errorf("no changes made to %s. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected", path)
	}
	return fmt.Errorf("no changes made to %s. The replacements produced identical content", path)
}

// DiffResult is a rendered diff plus the first changed line in the new file.
type DiffResult struct {
	Diff             string `json:"diff"`
	FirstChangedLine int    `json:"firstChangedLine,omitempty"`
}

const diffContextLines = 4

// GenerateDiff renders a line-numbered diff with bounded context, matching
// Pi's generateDiffString: "+N ", "-N ", and " N " prefixes, with elided
// context marked by "...".
func GenerateDiff(oldContent, newContent string) DiffResult {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	parts := diffLines(oldLines, newLines)

	width := len(fmt.Sprint(max(len(oldLines), len(newLines))))
	pad := func(n int) string { return fmt.Sprintf("%*d", width, n) }
	blank := strings.Repeat(" ", width)

	var out []string
	oldNum, newNum := 1, 1
	firstChanged := 0
	lastWasChange := false

	for i, part := range parts {
		if part.kind != diffEqual {
			if firstChanged == 0 {
				firstChanged = newNum
			}
			for _, line := range part.lines {
				if part.kind == diffAdd {
					out = append(out, fmt.Sprintf("+%s %s", pad(newNum), line))
					newNum++
				} else {
					out = append(out, fmt.Sprintf("-%s %s", pad(oldNum), line))
					oldNum++
				}
			}
			lastWasChange = true
			continue
		}

		nextIsChange := i < len(parts)-1 && parts[i+1].kind != diffEqual
		emit := func(lines []string) {
			for _, line := range lines {
				out = append(out, fmt.Sprintf(" %s %s", pad(oldNum), line))
				oldNum++
				newNum++
			}
		}
		skip := func(n int) {
			out = append(out, fmt.Sprintf(" %s ...", blank))
			oldNum += n
			newNum += n
		}

		switch {
		case lastWasChange && nextIsChange:
			if len(part.lines) <= diffContextLines*2 {
				emit(part.lines)
			} else {
				emit(part.lines[:diffContextLines])
				skip(len(part.lines) - diffContextLines*2)
				emit(part.lines[len(part.lines)-diffContextLines:])
			}
		case lastWasChange:
			shown := min(diffContextLines, len(part.lines))
			emit(part.lines[:shown])
			if rest := len(part.lines) - shown; rest > 0 {
				skip(rest)
			}
		case nextIsChange:
			skipped := max(0, len(part.lines)-diffContextLines)
			if skipped > 0 {
				skip(skipped)
			}
			emit(part.lines[skipped:])
		default:
			oldNum += len(part.lines)
			newNum += len(part.lines)
		}
		lastWasChange = false
	}

	return DiffResult{Diff: strings.Join(out, "\n"), FirstChangedLine: firstChanged}
}

type diffKind int

const (
	diffEqual diffKind = iota
	diffAdd
	diffRemove
)

type diffPart struct {
	kind  diffKind
	lines []string
}

// diffLines is a line-level LCS diff, grouped into runs of equal/added/removed.
func diffLines(a, b []string) []diffPart {
	// LCS table. Files reaching here are single source files, so the O(n*m)
	// table is acceptable; guard pathological sizes by degrading to a
	// whole-file replacement.
	const maxCells = 4_000_000
	if len(a)*len(b) > maxCells {
		var parts []diffPart
		if len(a) > 0 {
			parts = append(parts, diffPart{kind: diffRemove, lines: a})
		}
		if len(b) > 0 {
			parts = append(parts, diffPart{kind: diffAdd, lines: b})
		}
		return parts
	}

	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var parts []diffPart
	appendLine := func(kind diffKind, line string) {
		if n := len(parts); n > 0 && parts[n-1].kind == kind {
			parts[n-1].lines = append(parts[n-1].lines, line)
			return
		}
		parts = append(parts, diffPart{kind: kind, lines: []string{line}})
	}

	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			appendLine(diffEqual, a[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			appendLine(diffRemove, a[i])
			i++
		default:
			appendLine(diffAdd, b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		appendLine(diffRemove, a[i])
	}
	for ; j < len(b); j++ {
		appendLine(diffAdd, b[j])
	}
	return parts
}

// GenerateUnifiedPatch renders a standard unified diff for the edit details.
func GenerateUnifiedPatch(path, oldContent, newContent string) string {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	parts := diffLines(oldLines, newLines)

	var body []string
	oldStart, newStart := 0, 0
	oldNum, newNum := 1, 1
	oldCount, newCount := 0, 0
	flush := func() []string {
		if len(body) == 0 {
			return nil
		}
		header := fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldStart, oldCount, newStart, newCount)
		return append([]string{header}, body...)
	}

	var hunks []string
	for i, part := range parts {
		switch part.kind {
		case diffEqual:
			ctxBefore := 0
			if i < len(parts)-1 && parts[i+1].kind != diffEqual {
				ctxBefore = min(diffContextLines, len(part.lines))
			}
			ctxAfter := 0
			if len(body) > 0 {
				ctxAfter = min(diffContextLines, len(part.lines))
			}
			if ctxAfter > 0 {
				for _, line := range part.lines[:ctxAfter] {
					body = append(body, " "+line)
					oldCount++
					newCount++
				}
			}
			if len(part.lines) > ctxAfter+ctxBefore || (ctxBefore == 0 && len(body) > 0) {
				hunks = append(hunks, flush()...)
				body = nil
				oldCount, newCount = 0, 0
			}
			oldNum += len(part.lines) - ctxBefore
			newNum += len(part.lines) - ctxBefore
			if ctxBefore > 0 {
				if len(body) == 0 {
					oldStart, newStart = oldNum, newNum
				}
				for _, line := range part.lines[len(part.lines)-ctxBefore:] {
					body = append(body, " "+line)
					oldCount++
					newCount++
				}
				oldNum += ctxBefore
				newNum += ctxBefore
			}
		case diffAdd:
			if len(body) == 0 {
				oldStart, newStart = oldNum, newNum
			}
			for _, line := range part.lines {
				body = append(body, "+"+line)
				newCount++
				newNum++
			}
		case diffRemove:
			if len(body) == 0 {
				oldStart, newStart = oldNum, newNum
			}
			for _, line := range part.lines {
				body = append(body, "-"+line)
				oldCount++
				oldNum++
			}
		}
	}
	hunks = append(hunks, flush()...)

	if len(hunks) == 0 {
		return ""
	}
	head := fmt.Sprintf("--- %s\n+++ %s\n", path, path)
	return head + strings.Join(hunks, "\n") + "\n"
}
