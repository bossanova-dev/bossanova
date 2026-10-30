package home

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

func (h Model) SelectedSession() *pb.Session {
	if len(h.Sessions) == 0 {
		return nil
	}
	if h.hasTableLayout() {
		row := h.Table.Cursor()
		if row >= 0 && row < len(h.layout.rowSession) {
			if i := h.layout.rowSession[row]; i >= 0 {
				return h.layout.sessions[i]
			}
		}
	}
	sessions := h.BoardSessions()
	idx, ok := h.sessionIndexForTableCursor(h.Table.Cursor())
	if !ok {
		return nil
	}
	return sessions[idx]
}

func (h Model) sessionIndexForTableCursor(cursor int) (int, bool) {
	sessions := h.BoardSessions()
	sectioned := h.showsSections()
	if cursor < 0 {
		return 0, false
	}
	row := 0
	for i, sess := range sessions {
		row += h.sectionRows(sessions, i, sectioned)
		next := row + 1 + h.SubRowCount(sess)
		if cursor >= row && cursor < next {
			return i, true
		}
		row = next
	}
	return 0, false
}

func (h Model) primarySessionRows() []int {
	if h.hasTableLayout() {
		return h.layout.primaryRows
	}
	sessions := h.BoardSessions()
	sectioned := h.showsSections()
	rows := make([]int, 0, len(sessions))
	row := 0
	for i, sess := range sessions {
		row += h.sectionRows(sessions, i, sectioned)
		rows = append(rows, row)
		row += 1 + h.SubRowCount(sess)
	}
	return rows
}

func (h Model) TableDataRowCount() int {
	if h.hasTableLayout() {
		return len(h.layout.rowSession)
	}
	sessions := h.BoardSessions()
	sectioned := h.showsSections()
	rows := len(sessions)
	for i, sess := range sessions {
		rows += h.sectionRows(sessions, i, sectioned)
		rows += h.SubRowCount(sess)
	}
	return rows
}

func (h Model) TableCursorForSessionIndex(sessionIndex int) int {
	if h.hasTableLayout() {
		if sessionIndex < 0 || sessionIndex >= len(h.layout.primaryRows) {
			return -1
		}
		return h.layout.primaryRows[sessionIndex]
	}
	sessions := h.BoardSessions()
	sectioned := h.showsSections()
	row := 0
	for i, sess := range sessions {
		row += h.sectionRows(sessions, i, sectioned)
		if i == sessionIndex {
			return row
		}
		row += 1 + h.SubRowCount(sess)
	}
	return -1
}

// tableCursorForSessionID returns the table cursor row for the session with the
// given id, or (-1, false) when no session matches (including the empty id).
func (h Model) TableCursorForSessionID(id string) (int, bool) {
	if id == "" {
		return -1, false
	}
	if h.hasTableLayout() {
		row, ok := h.layout.sessionRows[id]
		if !ok {
			return -1, false
		}
		return row, true
	}
	sessions := h.BoardSessions()
	for i, sess := range sessions {
		if sess.Id == id {
			return h.TableCursorForSessionIndex(i), true
		}
	}
	return -1, false
}

func (h *Model) normalizeTableCursor(previousCursor int) {
	if h.hasTableLayout() {
		h.moveCursor(h.layout.normalizedCursor(h.Table.Cursor(), previousCursor))
		return
	}
	rows := h.primarySessionRows()
	if len(rows) == 0 {
		return
	}
	cursor := h.Table.Cursor()
	for _, row := range rows {
		if cursor == row {
			return
		}
	}
	if cursor > previousCursor {
		for _, row := range rows {
			if row > cursor {
				h.moveCursor(row)
				return
			}
		}
	} else if cursor < previousCursor {
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i] < cursor {
				h.moveCursor(rows[i])
				return
			}
		}
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i] <= cursor {
			h.moveCursor(rows[i])
			return
		}
	}
	h.moveCursor(rows[0])
}
