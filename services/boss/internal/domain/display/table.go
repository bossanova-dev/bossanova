package display

import (
	"strings"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// ellipsis is the house truncation/progress marker (U+2026). One cell and
// one rune wide, so a cell or rune budget reserves exactly one for it.
const Ellipsis = "…"

// maxColWidth returns the maximum width needed for a column, given its header
// and a set of values, capped at cap.
func MaxColWidth(header string, values []string, cap int) int {
	w := lipgloss.Width(header)
	for _, v := range values {
		if vw := lipgloss.Width(v); vw > w {
			w = vw
		}
	}
	if cap > 0 && w > cap {
		return cap
	}
	return w
}

// CLITableStyles returns table styles for non-interactive CLI output.
// Bold header, no selection highlighting needed.
func CLITableStyles() table.Styles {
	return table.Styles{
		Header:   lipgloss.NewStyle().Bold(true).Padding(0, 1),
		Cell:     lipgloss.NewStyle().Padding(0, 1),
		Selected: lipgloss.NewStyle(),
	}
}

// CLIColumnsWidth returns the total rendered width for a set of columns,
// including cell padding (1 char each side per column).
func CLIColumnsWidth(cols []table.Column) int {
	w := 0
	for _, c := range cols {
		w += c.Width + 2
	}
	return w
}

// RenderCLITable renders non-interactive CLI tables with compact, deterministic
// spacing. Bubble table adds per-cell padding that can make adjacent text-heavy
// columns look separated by a variable gap.
func RenderCLITable(cols []table.Column, rows []table.Row) string {
	var b strings.Builder
	headers := make(table.Row, len(cols))
	for i, col := range cols {
		headers[i] = col.Title
	}
	writeCLITableRow(&b, cols, headers)
	for _, row := range rows {
		b.WriteByte('\n')
		writeCLITableRow(&b, cols, row)
	}
	return b.String()
}

func writeCLITableRow(b *strings.Builder, cols []table.Column, cells table.Row) {
	for i, col := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		cell := ""
		if i < len(cells) {
			cell = TruncateDisplay(cells[i], col.Width)
		}
		b.WriteString(cell)
		if pad := col.Width - lipgloss.Width(cell); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
	}
}

// truncateDisplay caps a SINGLE-LINE s at maxWidth display cells, appending the
// ellipsis marker when it has to cut. ansi.Truncate measures in cells and
// subtracts the marker's own width itself, so maxWidth is the whole budget:
// there is no separate reservation to keep in sync, and it never byte-slices a
// styled string into mojibake or a stranded SGR escape.
//
// The single-line precondition is load bearing, because the two width functions
// in this file disagree about newlines: ansi.StringWidth (what ansi.Truncate
// measures with) SUMS across lines, while lipgloss.Width (what
// writeCLITableRow's padding measures with) returns the WIDEST line. So on
// multi-line input this budgets the total rather than the visual width —
// truncateDisplay("aaaa\nbbbb", 6) is "aaaa\nb…", not the untouched input a
// widest-line reading would give — and the padding below would then disagree
// with the cut. Every caller today satisfies the precondition: maskTestError
// collapses whitespace runs to single spaces before calling
// (account_health.go), accountLastTestCell truncates that same masked value
// (accounts_list.go), and writeCLITableRow feeds one table cell at a time.
// Keep it that way, or measure in one unit rather than two.
func TruncateDisplay(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return s
	}
	return ansi.Truncate(s, maxWidth, Ellipsis)
}
