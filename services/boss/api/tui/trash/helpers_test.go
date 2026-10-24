package trash

import (
	"testing"

	"charm.land/bubbles/v2/table"
)

func assertTableRowsMatchColumns(t *testing.T, cols []table.Column, rows []table.Row) {
	t.Helper()
	for i, row := range rows {
		if len(row) != len(cols) {
			t.Fatalf("row %d has %d cells for %d columns", i, len(row), len(cols))
		}
	}
}
func columnTitles(cols []table.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Title
	}
	return out
}
