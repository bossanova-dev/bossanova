package home

import (
	"maps"
	"slices"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const spinnerProbe = "\ue000"

// homeTableLayout is an immutable snapshot, replaced only by BuildTableRows.
// project closes over ui's private fitted-column projection.
type homeTableLayout struct {
	statusVisible                         bool
	source                                []*pb.Session
	query                                 string
	sessions                              []*pb.Session
	project                               func(table.Row) table.Row
	rowSession, primaryRows, animatedRows []int
	previousRows, nextRows                []int
	sessionRows                           map[string]int
	warningRows                           map[int]homeWarningRow
}

type homeWarningRow struct{ sessionIndex, hintIndex int }

// hasTableLayout also supports callers that seed exported Sessions directly
// before asking for heights, as the router's reserved-height fixture does.
func (h Model) hasTableLayout() bool {
	if h.layout == nil || len(h.Sessions) != len(h.layout.source) || h.Filter.Query() != h.layout.query {
		return false
	}
	return len(h.Sessions) == 0 || &h.Sessions[0] == &h.layout.source[0]
}

func (l *homeTableLayout) recordRows(count int) {
	l.rowSession = make([]int, count)
	l.previousRows = make([]int, count)
	l.nextRows = make([]int, count)
	for row := range l.rowSession {
		l.rowSession[row] = -1
	}
	for i, row := range l.primaryRows {
		l.rowSession[row] = i
	}
	previous := -1
	for row := range count {
		if l.rowSession[row] >= 0 {
			previous = row
		}
		l.previousRows[row] = previous
	}
	next := -1
	for row := count - 1; row >= 0; row-- {
		if l.rowSession[row] >= 0 {
			next = row
		}
		l.nextRows[row] = next
	}
}

func (l *homeTableLayout) normalizedCursor(row, previous int) int {
	if len(l.primaryRows) == 0 {
		return 0
	}
	row = min(max(row, 0), len(l.rowSession)-1)
	if l.rowSession[row] >= 0 {
		return row
	}
	if row > previous && l.nextRows[row] >= 0 {
		return l.nextRows[row]
	}
	if l.previousRows[row] >= 0 {
		return l.previousRows[row]
	}
	return l.primaryRows[0]
}

func (h Model) primaryRow(sess *pb.Session, _ int, selected bool) table.Row {
	repo := sess.GetRepoDisplayName()
	title := sess.GetTitle()
	if title == "" {
		title = sess.GetBranchName()
	}
	name, pr := ui.RenderTrackerLink(sess, title), "-"
	if sess.PrNumber != nil {
		pr = ui.RenderPRLink(sess)
	}
	switch {
	case sess.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_MERGED || sess.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
		repo = lipgloss.NewStyle().Foreground(ui.ColorMuted).Strikethrough(true).Render(repo)
		name, pr = ui.RenderMutedTrackerLink(sess, title), ui.RenderMutedPRLink(sess)
	case selected:
		repo = ui.RenderSelectedText(repo)
		name = ui.RenderSelectedTrackerLink(sess, title)
		if sess.PrNumber != nil {
			pr = ui.RenderSelectedPRLink(sess)
		} else {
			pr = ui.RenderSelectedText(pr)
		}
	}
	indicator := ""
	if selected {
		indicator = ui.CursorChevron
	}
	return h.layout.project(table.Row{indicator, repo, name, pr, h.RenderSessionStatus(sess)})
}

// moveCursor keeps the embedded link colours with the selection, including
// error and attention rows (BOS-103). The table's wrapper cannot style OSC 8
// content whose own SGR resets override that wrapper.
func (h *Model) moveCursor(row int) {
	previous := h.selectedRow
	h.Table.SetCursor(row)
	current := h.Table.Cursor()
	if !h.hasTableLayout() || previous == current {
		return
	}
	for _, r := range []int{previous, current} {
		if r >= 0 && r < len(h.layout.rowSession) {
			if i := h.layout.rowSession[r]; i >= 0 {
				h.Table.SetRow(r, h.primaryRow(h.layout.sessions[i], r, r == current))
			}
		}
	}
	h.selectedRow = current
}

func (h *Model) refreshSpinnerRows() {
	if !h.hasTableLayout() {
		return
	}
	start, end := h.Table.VisibleRange()
	for _, row := range h.layout.animatedRows {
		if row >= start && row < end {
			i := h.layout.rowSession[row]
			h.Table.SetRow(row, h.primaryRow(h.layout.sessions[i], row, row == h.Table.Cursor()))
		}
	}
	h.refreshRuntimeSpinnerRows(start, end)
	// Repair retries and exhausted-account hints contain countdowns. Refresh
	// visible warning slots without remeasuring the board or touching stable rows.
	// Expired hints leave an empty slot until the next data rebuild compacts it.
	copied := false
	for row := start; row < end; row++ {
		warning, ok := h.layout.warningRows[row]
		if !ok {
			continue
		}
		sess := h.layout.sessions[warning.sessionIndex]
		hints := ui.SessionWarningHints(sess)
		cell := ""
		if warning.hintIndex < len(hints) {
			hint := hints[warning.hintIndex]
			cell = ui.WarningHintStyle(sess, hint).Render(hint.Text)
		}
		if cell == h.warningCells[row] {
			continue
		}
		if !copied {
			h.warningCells = maps.Clone(h.warningCells)
			copied = true
		}
		h.warningCells[row] = cell
		h.Table.SetRow(row, h.layout.project(table.Row{"", "", cell, "", ""}))
	}
}

// Runtime overrides can start after a static row was built. Overlay their
// membership on the immutable layout, without refreshing a row twice.
func (h *Model) refreshRuntimeSpinnerRows(start, end int) {
	if !h.layout.statusVisible {
		return
	}
	refresh := func(id string) {
		row, ok := h.layout.sessionRows[id]
		if !ok || row < start || row >= end {
			return
		}
		if _, animated := slices.BinarySearch(h.layout.animatedRows, row); animated {
			return
		}
		h.refreshStatusRow(id)
	}
	for id := range h.ArchivingOverrideIDs {
		refresh(id)
	}
	for id := range h.MergesInFlight {
		if !h.IsArchiving(id) && h.isMerging(id) {
			refresh(id)
		}
	}
}

// Settling an override must also clear its cached cell when it stops animating.
func (h *Model) refreshStatusRow(id string) {
	if !h.hasTableLayout() || !h.layout.statusVisible {
		return
	}
	if row, ok := h.layout.sessionRows[id]; ok {
		i := h.layout.rowSession[row]
		h.Table.SetRow(row, h.primaryRow(h.layout.sessions[i], row, row == h.Table.Cursor()))
	}
}
