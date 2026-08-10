package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	xast "github.com/yuin/goldmark/extension/ast"

	"github.com/ihavespoons/tau/theme"
)

// minColumnWidth is how narrow a column may be squeezed before the table stops
// giving ground. Below this a wrapped cell is one character per line, which
// tells the reader nothing.
const minColumnWidth = 3

// table renders a GFM table to fit the available width, wrapping cells rather
// than truncating them: a table in a coding transcript usually holds paths and
// error messages, and the end of those is where the answer is.
func (r *mdRender) table(n *xast.Table, width int) []string {
	rows, aligns := r.tableRows(n)
	if len(rows) == 0 {
		return nil
	}

	cols := 0
	for _, row := range rows {
		cols = max(cols, len(row))
	}
	if cols == 0 {
		return nil
	}
	widths := fitColumns(rows, cols, width)

	border := r.style(theme.Border)
	sep := border.Render(" │ ")

	var out []string
	for i, row := range rows {
		out = append(out, renderRow(row, widths, aligns, sep)...)
		if i == 0 && n.FirstChild() != nil {
			rules := make([]string, len(widths))
			for c, w := range widths {
				rules[c] = strings.Repeat("─", w)
			}
			out = append(out, border.Render(strings.Join(rules, "─┼─")))
		}
	}
	return out
}

// tableRows renders every cell's inline content and reports the column
// alignments. The header is row zero.
func (r *mdRender) tableRows(n *xast.Table) ([][]string, []xast.Alignment) {
	var (
		rows   [][]string
		aligns []xast.Alignment
	)
	for row := n.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		bold := row.Kind() == xast.KindTableHeader
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			style := lipgloss.NewStyle().Bold(bold)
			cells = append(cells, r.inline(cell, style))
			if c, ok := cell.(*xast.TableCell); ok && bold {
				aligns = append(aligns, c.Alignment)
			}
		}
		rows = append(rows, cells)
	}
	return rows, aligns
}

// fitColumns gives every column its natural width when the table fits, and
// takes the excess off the widest columns first when it does not — narrow
// columns holding a flag or a number keep their shape, and the prose column
// absorbs the wrapping.
func fitColumns(rows [][]string, cols, width int) []int {
	widths := make([]int, cols)
	for _, row := range rows {
		for c, cell := range row {
			widths[c] = max(widths[c], displayWidth(cell))
		}
	}

	budget := width - 3*(cols-1)
	if budget < cols*minColumnWidth {
		budget = cols * minColumnWidth
	}
	for total(widths) > budget {
		widest, at := 0, -1
		for c, w := range widths {
			if w > widest {
				widest, at = w, c
			}
		}
		if at < 0 || widest <= minColumnWidth {
			break
		}
		widths[at]--
	}
	return widths
}

func total(ns []int) int {
	sum := 0
	for _, n := range ns {
		sum += n
	}
	return sum
}

// renderRow lays one row out, growing to as many display lines as its tallest
// wrapped cell needs.
func renderRow(cells []string, widths []int, aligns []xast.Alignment, sep string) []string {
	wrapped := make([][]string, len(widths))
	height := 1
	for c := range widths {
		cell := ""
		if c < len(cells) {
			cell = cells[c]
		}
		wrapped[c] = wrapANSI(cell, widths[c])
		height = max(height, len(wrapped[c]))
	}

	out := make([]string, 0, height)
	for line := 0; line < height; line++ {
		parts := make([]string, len(widths))
		for c := range widths {
			text := ""
			if line < len(wrapped[c]) {
				text = wrapped[c][line]
			}
			parts[c] = pad(text, widths[c], alignAt(aligns, c))
		}
		out = append(out, strings.TrimRight(strings.Join(parts, sep), " "))
	}
	return out
}

func alignAt(aligns []xast.Alignment, c int) xast.Alignment {
	if c < len(aligns) {
		return aligns[c]
	}
	return xast.AlignNone
}

// pad fills a cell to its column width. Padding is plain spaces rather than
// styled ones, so a copied table carries no escape sequences in its gutters.
func pad(s string, width int, align xast.Alignment) string {
	gap := width - displayWidth(s)
	if gap <= 0 {
		return s
	}
	switch align {
	case xast.AlignRight:
		return strings.Repeat(" ", gap) + s
	case xast.AlignCenter:
		left := gap / 2
		return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
	default:
		return s + strings.Repeat(" ", gap)
	}
}
