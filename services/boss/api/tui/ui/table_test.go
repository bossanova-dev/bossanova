package ui

import (
	"fmt"
	"testing"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
)

func lazyTableFixture(n int) ([]table.Column, []table.Row) {
	cols := []table.Column{CursorColumn, {Title: "NAME", Width: 12}, {Title: "STATUS", Width: 8}}
	rows := make([]table.Row, n)
	for i := range rows {
		rows[i] = table.Row{"", fmt.Sprintf("row-%02d 日本語 long", i), "running"}
	}
	return cols, rows
}

func newVisibleTestTable(cols []table.Column, rows []table.Row, height int) Table {
	m := NewBossTable(cols, rows, height)
	m.SetWidth(ColumnsWidth(cols))
	return m
}

func bubblesTableOracle(cols []table.Column, rows []table.Row, height int) table.Model {
	return table.New(table.WithColumns(cols), table.WithRows(rows), table.WithHeight(height), table.WithWidth(ColumnsWidth(cols)),
		table.WithKeyMap(bossKeyMap()), table.WithStyles(bossTableStyles()), table.WithFocused(true))
}

func normalizeOracleChevron(m *table.Model) {
	rows := make([]table.Row, len(m.Rows()))
	for i, row := range m.Rows() {
		rows[i] = append(table.Row(nil), row...)
		rows[i][0] = ""
		if i == m.Cursor() {
			rows[i][0] = CursorChevron
		}
	}
	m.SetRows(rows)
}

func TestTableMatchesBubbles(t *testing.T) {
	cols, rows := lazyTableFixture(24)
	got := newVisibleTestTable(cols, rows, 7)
	want := bubblesTableOracle(cols, rows, 7)
	check := func(step string) {
		t.Helper()
		normalizeOracleChevron(&want)
		if actual, expected := got.View(), want.View(); actual != expected {
			t.Fatalf("%s: view differs\ngot:  %q\nwant: %q", step, actual, expected)
		}
	}
	check("initial")
	for _, code := range []rune{tea.KeyDown, tea.KeyDown, tea.KeyUp, tea.KeyDown, tea.KeyDown,
		tea.KeyDown, tea.KeyDown, tea.KeyDown, tea.KeyDown, tea.KeyUp} {
		msg := tea.KeyPressMsg{Code: code}
		got, _ = got.Update(msg)
		want, _ = want.Update(msg)
		check(fmt.Sprintf("key %d cursor %d", code, got.Cursor()))
	}
	got.GotoBottom()
	want.GotoBottom()
	check("bottom")
	got.MoveUp(1)
	want.MoveUp(1)
	check("up from bottom")
	got.GotoTop()
	want.GotoTop()
	check("top")
	_, replacement := lazyTableFixture(12)
	replacement[2][1] = "changed"
	got.SetRows(replacement)
	want.SetRows(replacement)
	check("rebuild")
	got.GotoBottom()
	want.GotoBottom()
	check("rebuilt bottom")
}

func TestTableMatchesBubblesFraming(t *testing.T) {
	for _, count := range []int{0, 2, 12} {
		for _, width := range []int{0, 18, 50} {
			t.Run(fmt.Sprintf("rows=%d/width=%d", count, width), func(t *testing.T) {
				cols, rows := lazyTableFixture(count)
				if count > 0 {
					rows[0][1] = "\x1b[4mlink\x1b[0m 日本語"
				}
				got := newVisibleTestTable(cols, rows, 7)
				want := bubblesTableOracle(cols, rows, 7)
				got.SetWidth(width)
				want.SetWidth(width)
				normalizeOracleChevron(&want)
				if actual, expected := got.View(), want.View(); actual != expected {
					t.Fatalf("framing differs\ngot:  %q\nwant: %q", actual, expected)
				}
			})
		}
	}
}

func TestTableRendersOnlyVisibleRowsLazily(t *testing.T) {
	cols, rows := lazyTableFixture(100)
	m := newVisibleTestTable(cols, rows, 7)
	m.SetColumns(cols)
	m.SetRows(rows)
	m.SetStyles(bossTableStyles())
	m.SetWidth(ColumnsWidth(cols))
	m.SetHeight(7)
	m.SetCursor(1)
	m.MoveDown(1)
	m.MoveUp(1)
	m.GotoBottom()
	m.GotoTop()
	m.Blur()
	m.Focus()
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if n := m.RenderedRows(); n != 0 {
		t.Fatalf("setters rendered %d rows before View", n)
	}
	_ = m.View()
	start, end := m.VisibleRange()
	if n := m.RenderedRows(); n <= 0 || n > end-start {
		t.Fatalf("cold View rendered %d rows, visible range [%d,%d)", n, start, end)
	}
	for _, cursor := range []int{2, 6, 7} {
		before := m.RenderedRows()
		m.SetCursor(cursor)
		if m.RenderedRows() != before {
			t.Fatal("cursor movement rendered before View")
		}
		_ = m.View()
		if delta := m.RenderedRows() - before; delta > 3 {
			t.Fatalf("cursor %d rendered %d rows, want at most old/new selection plus entering row", cursor, delta)
		}
	}
}

func TestTableSetRowInvalidatesOnlyChangedRow(t *testing.T) {
	cols, rows := lazyTableFixture(8)
	m := newVisibleTestTable(cols, rows, 10)
	_ = m.View()
	before := m.RenderedRows()
	m.SetRow(3, table.Row{"", "replacement", "done"})
	if m.RenderedRows() != before {
		t.Fatal("SetRow rendered before View")
	}
	actual := m.View()
	// The selected row is rendered fresh on each View in addition to the edit.
	if delta := m.RenderedRows() - before; delta < 1 || delta > 2 {
		t.Fatalf("SetRow rendered %d rows, want changed row plus at most selection", delta)
	}
	fresh := newVisibleTestTable(cols, m.Rows(), 10)
	if actual != fresh.View() {
		t.Fatal("SetRow view differs from an uncached render")
	}
}

func TestTableCopiesKeepIndependentCachedViews(t *testing.T) {
	for _, mutation := range []struct {
		name  string
		apply func(*Table)
	}{
		{"row", func(m *Table) { m.SetRow(2, table.Row{"", "edited", "done"}) }},
		{"rows", func(m *Table) { _, rows := lazyTableFixture(4); m.SetRows(rows) }},
		{"columns", func(m *Table) {
			cols, _ := lazyTableFixture(8)
			cols[1].Width = 5
			m.SetColumns(cols)
		}},
		{"width", func(m *Table) { m.SetWidth(18) }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			cols, rows := lazyTableFixture(8)
			original := newVisibleTestTable(cols, rows, 10)
			before := original.View()
			copyOfTable := original
			mutation.apply(&copyOfTable)
			fresh := newVisibleTestTable(copyOfTable.Columns(), copyOfTable.Rows(), 10)
			fresh.SetWidth(copyOfTable.Width())
			expected := fresh.View()
			for range 3 {
				if copyOfTable.View() != expected {
					t.Fatal("mutated copy reused stale cached content")
				}
				if original.View() != before {
					t.Fatal("mutating a copy changed the original view")
				}
			}
		})
	}
}

func assertTableCursorVisible(t *testing.T, m Table) {
	t.Helper()
	start, end := m.VisibleRange()
	if start < 0 || end > len(m.Rows()) || m.Cursor() < start || m.Cursor() >= end {
		t.Fatalf("cursor %d outside visible range [%d,%d) of %d rows", m.Cursor(), start, end, len(m.Rows()))
	}
}

func TestTableKeepsSelectionVisibleThroughRebuilds_MultiSurfaceJourneys_AC5(t *testing.T) {
	cols, rows := lazyTableFixture(100)
	m := newVisibleTestTable(cols, rows, 10)
	m.SetCursor(80)
	assertTableCursorVisible(t, m)
	before, _ := m.VisibleRange()
	m.SetRows(rows)
	start, _ := m.VisibleRange()
	if start != before || start == 0 {
		t.Fatalf("rebuild offset = %d, want retained offset %d", start, before)
	}
	m.SetHeight(3)
	assertTableCursorVisible(t, m)
	narrowRows := make([]table.Row, len(rows))
	for i, row := range rows {
		narrowRows[i] = append(table.Row(nil), row[:2]...)
	}
	SetTableContent(&m, cols[:2], narrowRows)
	assertTableCursorVisible(t, m)
	m.SetRows(narrowRows[:20])
	assertTableCursorVisible(t, m)
	for _, code := range []rune{tea.KeyPgUp, tea.KeyPgDown, tea.KeyPgDown} {
		m, _ = m.Update(tea.KeyPressMsg{Code: code})
		assertTableCursorVisible(t, m)
	}
}

func TestTableOwnsCursorChevron_MultiSurfaceJourneys_AC5(t *testing.T) {
	cols, rows := lazyTableFixture(15)
	m := newVisibleTestTable(cols, rows, 5)
	check := func() {
		t.Helper()
		for i, row := range m.Rows() {
			want := ""
			if i == m.Cursor() {
				want = CursorChevron
			}
			if row[0] != want {
				t.Fatalf("cursor %d: row %d chevron = %q, want %q", m.Cursor(), i, row[0], want)
			}
		}
	}
	check()
	for _, cursor := range []int{4, 11, 2} {
		m.SetCursor(cursor)
		check()
		m.MoveDown(1)
		check()
		m.MoveUp(1)
		check()
	}
	for i := range rows {
		rows[i][0] = CursorChevron
	}
	m.SetRows(rows)
	check()
	m.GotoBottom()
	check()
	m.GotoTop()
	check()
}
