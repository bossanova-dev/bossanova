package ui

import (
	"slices"
	"sync"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Table renders its visible rows on demand. Content and layout snapshots are
// immutable, so copying a Bubble Tea model cannot invalidate another copy.
// Rows returns an owned snapshot; change content with SetRows or SetRow.
type Table struct {
	KeyMap                        table.KeyMap
	Help                          help.Model
	cols                          []table.Column
	rows                          []*tableRow
	cursor, offset, width, height int
	focus                         bool
	styles                        table.Styles
	cells                         []lipgloss.Style
	layout                        *tableLayout
	diagnostic                    *tableDiagnostic
}

type tableDiagnostic struct {
	sync.Mutex
	rendered int
}
type tableLayout byte
type tableRow struct {
	values table.Row
	sync.Mutex
	layout                  *tableLayout
	plain, cursor           string
	plainValid, cursorValid bool
}

func newTable(cols []table.Column, rows []table.Row, height int) Table {
	m := Table{KeyMap: bossKeyMap(), Help: help.New(), focus: true, styles: bossTableStyles(), diagnostic: &tableDiagnostic{}}
	m.SetColumns(cols)
	m.SetRows(rows)
	m.SetHeight(height)
	return m
}

func (m *Table) SetColumns(cols []table.Column) {
	m.cols = slices.Clone(cols)
	m.cells = make([]lipgloss.Style, len(cols))
	for i, c := range cols {
		m.cells[i] = lipgloss.NewStyle().Width(c.Width).MaxWidth(c.Width).Inline(true)
	}
	m.layout = new(tableLayout)
}
func (m *Table) SetRows(rows []table.Row) {
	m.rows = make([]*tableRow, len(rows))
	for i, row := range rows {
		m.rows[i] = &tableRow{values: slices.Clone(row)}
	}
	m.keepVisible()
}

// SetRow replaces one row without invalidating any other cached row.
func (m *Table) SetRow(i int, row table.Row) {
	if i < 0 || i >= len(m.rows) {
		return
	}
	m.rows = slices.Clone(m.rows)
	m.rows[i] = &tableRow{values: slices.Clone(row)}
}
func (m *Table) SetStyles(styles table.Styles) { m.styles = styles; m.layout = new(tableLayout) }
func (m *Table) SetWidth(w int)                { m.width = w; m.layout = new(tableLayout) }
func (m *Table) SetHeight(h int) {
	// The public height includes the header, as in bubbles. Header height is
	// determined by style geometry without rendering cells in a setter.
	m.height = max(0, h-m.headerHeight())
	m.keepVisible()
}
func (m Table) headerHeight() int {
	if len(m.cols) == 0 {
		return 1
	}
	return 1 + m.styles.Header.GetVerticalFrameSize()
}
func (m Table) Height() int      { return m.height }
func (m Table) Width() int       { return m.width }
func (m Table) Cursor() int      { return m.cursor }
func (m Table) Focused() bool    { return m.focus }
func (m *Table) Focus()          { m.focus = true }
func (m *Table) Blur()           { m.focus = false }
func (m *Table) SetCursor(n int) { m.cursor = n; m.keepVisible() }
func (m *Table) MoveUp(n int) {
	start, end := m.cursorWindow()
	relative := m.offset - start
	m.cursor = min(max(m.cursor-n, 0), max(0, len(m.rows)-1))
	switch {
	case start == 0:
		relative = min(max(relative, 0), m.cursor)
	case start < m.height:
		relative = min(min(max(relative+n, 0), m.cursor), m.height)
	case relative >= 1:
		relative = min(max(relative+n, 1), m.height)
	}
	relative = min(max(relative, 0), max(0, end-start-m.height))
	m.placeWindow(relative)
}
func (m *Table) MoveDown(n int) {
	oldStart, _ := m.cursorWindow()
	relative := m.offset - oldStart
	m.cursor = min(max(m.cursor+n, 0), max(0, len(m.rows)-1))
	start, end := m.cursorWindow()
	relative = min(max(relative, 0), max(0, end-start-m.height))
	switch {
	case end == len(m.rows) && relative > 0:
		relative = min(max(relative-n, 1), m.height)
	case m.cursor > (end-start)/2 && relative > 0:
		relative = min(max(relative-n, 1), m.cursor)
	case relative > 1:
	case m.cursor > relative+m.height-1:
		relative = min(max(relative+1, 0), 1)
	}
	m.placeWindow(relative)
}
func (m *Table) GotoTop()    { m.SetCursor(0) }
func (m *Table) GotoBottom() { m.SetCursor(len(m.rows) - 1) }

// cursorWindow is bubbles' buffered window expressed only as indices. Keeping
// its relative scroll arithmetic preserves byte-compatible line navigation
// (notably moving up from the bottom), without rendering the buffered rows.
func (m Table) cursorWindow() (int, int) {
	return max(0, m.cursor-m.height), min(len(m.rows), m.cursor+m.height)
}
func (m *Table) placeWindow(relative int) {
	start, end := m.cursorWindow()
	m.offset = start + min(max(relative, 0), max(0, end-start-m.height))
	m.keepVisible()
}

func (m *Table) keepVisible() {
	m.cursor = min(max(m.cursor, 0), max(0, len(m.rows)-1))
	m.offset = min(max(m.offset, 0), max(0, len(m.rows)-m.height))
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.height > 0 && m.cursor >= m.offset+m.height {
		m.offset = m.cursor - m.height + 1
	}
}

// VisibleRange returns the half-open range of rows drawn by View.
func (m Table) VisibleRange() (start, end int) { return m.offset, min(len(m.rows), m.offset+m.height) }
func (m Table) Columns() []table.Column        { return slices.Clone(m.cols) }
func (m Table) hasCursorColumn() bool          { return len(m.cols) > 0 && m.cols[0] == CursorColumn }
func (m Table) rowValues(i int) table.Row {
	row := slices.Clone(m.rows[i].values)
	if m.hasCursorColumn() && len(row) > 0 {
		row[0] = ""
		if i == m.cursor {
			row[0] = CursorChevron
		}
	}
	return row
}
func (m Table) Rows() []table.Row {
	rows := make([]table.Row, len(m.rows))
	for i := range rows {
		rows[i] = m.rowValues(i)
	}
	return rows
}
func (m Table) SelectedRow() table.Row {
	if len(m.rows) == 0 {
		return nil
	}
	return m.rowValues(m.cursor)
}
func (m Table) HelpView() string { return m.Help.View(m.KeyMap) }

// RenderedRows is a cumulative diagnostic count of row cache misses. Copies
// share the counter, while their immutable content and layout remain independent.
func (m Table) RenderedRows() int {
	if m.diagnostic == nil {
		return 0
	}
	m.diagnostic.Lock()
	defer m.diagnostic.Unlock()
	return m.diagnostic.rendered
}
func (m Table) Update(msg tea.Msg) (Table, tea.Cmd) {
	if !m.focus {
		return m, nil
	}
	if msg, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(msg, m.KeyMap.LineUp):
			m.MoveUp(1)
		case key.Matches(msg, m.KeyMap.LineDown):
			m.MoveDown(1)
		case key.Matches(msg, m.KeyMap.PageUp):
			m.MoveUp(m.height)
		case key.Matches(msg, m.KeyMap.PageDown):
			m.MoveDown(m.height)
		case key.Matches(msg, m.KeyMap.HalfPageUp):
			m.MoveUp(m.height / 2)
		case key.Matches(msg, m.KeyMap.HalfPageDown):
			m.MoveDown(m.height / 2)
		case key.Matches(msg, m.KeyMap.GotoTop):
			m.GotoTop()
		case key.Matches(msg, m.KeyMap.GotoBottom):
			m.GotoBottom()
		}
	}
	return m, nil
}
func (m Table) headersView() string {
	cells := make([]string, 0, len(m.cols))
	for i, col := range m.cols {
		if col.Width <= 0 {
			continue
		}
		cells = append(cells, m.styles.Header.Render(m.cells[i].Render(ansi.Truncate(col.Title, col.Width, "…"))))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cells...)
}
func (m Table) renderRow(i int) string {
	row := m.rows[i]
	row.Lock()
	defer row.Unlock()
	if row.layout != m.layout {
		row.layout = m.layout
		row.plainValid = false
		row.cursorValid = false
	}
	selected := i == m.cursor
	cached, valid := row.plain, row.plainValid
	if selected {
		cached, valid = row.cursor, row.cursorValid
	}
	if !valid {
		cells := make([]string, 0, len(m.cols))
		for c, value := range row.values {
			if c >= len(m.cols) {
				break
			}
			if m.cols[c].Width <= 0 {
				continue
			}
			if c == 0 && m.hasCursorColumn() {
				value = ""
				if selected {
					value = CursorChevron
				}
			}
			cells = append(cells, m.styles.Cell.Render(m.cells[c].Render(ansi.Truncate(value, m.cols[c].Width, "…"))))
		}
		cached = lipgloss.JoinHorizontal(lipgloss.Top, cells...)
		if selected {
			row.cursor = cached
			row.cursorValid = true
		} else {
			row.plain = cached
			row.plainValid = true
		}
		if m.diagnostic != nil {
			m.diagnostic.Lock()
			m.diagnostic.rendered++
			m.diagnostic.Unlock()
		}
	}
	if selected {
		return m.styles.Selected.Render(cached)
	}
	return cached
}
func (m Table) View() string {
	start, end := m.VisibleRange()
	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		lines = append(lines, m.renderRow(i))
	}
	// Keep bubbles' viewport framing (ANSI-aware clipping, width padding and
	// blank height filling), without its eager table rendering on every setter.
	v := viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(m.height))
	v.SetContent(lipgloss.JoinVertical(lipgloss.Left, lines...))
	return m.headersView() + "\n" + v.View()
}
