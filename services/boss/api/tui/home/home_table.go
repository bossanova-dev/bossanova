// Home's session table: row construction, status cells, and height. Split out
// of home.go (BOS-526); the declarations are unchanged.

package home

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"

	"charm.land/bubbles/v2/table"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/display"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// homeTableBlockPadding mirrors the Padding(0, 1) renderSessionTable wraps the
// table in, so the table's own budget is the terminal width minus both sides.
// Declared here rather than inlined at the one call site so the two cannot
// drift: widen the block's padding and this is the single number to follow.
const homeTableBlockPadding = 1

const homeStatusWidth = 16

// tableAvailWidth returns the rendered columns the session table may occupy —
// the terminal width less renderSessionTable's block padding on either side.
// See fitAvailWidth (theme.go) for the unknown-width and padding-swamped cases.
func (h Model) tableAvailWidth() int {
	return ui.FitAvailWidth(h.Width, homeTableBlockPadding)
}

// NAME remains selectable and visible when optional columns are dropped.
const (
	homeNameMinWidthNarrow  = 16
	homeNameMinWidthCompact = 24
	homeNameMinWidthFull    = 32
)

func homeNameMinWidth(width int) int {
	switch {
	case width <= 0 || width > ui.CompactWidthMax:
		return homeNameMinWidthFull
	case width <= ui.NarrowWidthMax:
		return homeNameMinWidthNarrow
	default:
		return homeNameMinWidthCompact
	}
}

func (h Model) RenderSessionStatus(sess *pb.Session) string {
	if sess != nil && h.IsArchiving(sess.Id) {
		return ui.RenderArchivingStatus(h.Spinner)
	}
	if h.isMerging(sess.GetId()) {
		return ui.RenderMergingStatus(h.Spinner)
	}
	if displaystatus.IsStateEndingLabel(sess.GetDisplayLabel()) {
		return ui.RenderDisplayStatus(sess, h.Spinner)
	}
	// A finished row reads its PR's end ("✓ merged", "closed") like the rest of
	// its struck-through cells.
	if a := sess.GetAttention(); a != nil && a.GetKind() != pb.AttentionKind_ATTENTION_KIND_DONE && (a.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !displaystatus.FixShowsLabel(sess)) {
		return h.renderAttentionStatus(a, sess.GetDisplayLabel() == ui.StatusWorking)
	}
	if sess != nil && sess.GetDisplayLabel() == ui.StatusWorking {
		if phase := h.DaemonPhases[sess.GetId()]; phase != "" {
			label := phase
			if sess.GetDisplaySpinner() {
				label = h.Spinner.View() + label
			}
			return ui.StyleForIntent(sess.GetDisplayIntent()).Render(ansi.Truncate(label, homeStatusWidth, "…"))
		}
	}
	return ui.RenderDisplayStatus(sess, h.Spinner)
}

// buildTableRows rebuilds the table columns and rows from h.sessions.
func (h *Model) BuildTableRows() {
	h.Sessions = attentionBoard(h.Sessions)
	if len(h.Sessions) == 0 {
		// Archived-only polls also hide the board after projection.
		h.Filter.Reset()
	}
	sessions := h.BoardSessions()
	h.Filter.SetCounts(len(sessions), len(h.Sessions))
	h.layout = &homeTableLayout{source: h.Sessions, query: h.Filter.Query(), sessions: sessions, sessionRows: make(map[string]int)}
	h.warningCells = nil
	grouped, sectioned := h.hasAttentionBoard(), h.showsSections()
	if len(sessions) == 0 {
		h.Table.SetRows(nil)
		h.Table.SetHeight(h.TableHeight())
		return
	}

	repos := make([]string, len(sessions))
	names := make([]string, len(sessions)) // plain text for width calc
	nameWidthLabels := make([]string, 0, len(sessions)*2)
	prLabels := make([]string, len(sessions)) // visible text for width calc
	for i, sess := range sessions {
		repos[i] = sess.RepoDisplayName
		if sess.Title != "" {
			names[i] = sess.Title
		} else {
			names[i] = sess.BranchName
		}
		nameWidthLabels = append(nameWidthLabels, names[i])
		if h.hasEndpointRow(sess) {
			nameWidthLabels = append(nameWidthLabels, ui.SessionEndpointLabels(sess))
		}
		if waitingHint := ui.WaitingHintLine(h.sessionWaitingReason(sess), ui.WaitingBadgeSuperseded(sess)); sess.GetAttention() == nil && waitingHint != "" {
			nameWidthLabels = append(nameWidthLabels, waitingHint)
		}
		if sess.GetAttention() == nil {
			nameWidthLabels = append(nameWidthLabels, ui.SessionWarningHintTexts(sess)...)
		}
		if sess.PrNumber != nil {
			prLabels[i] = fmt.Sprintf("#%d", *sess.PrNumber)
		} else {
			prLabels[i] = "-"
		}
	}

	// Keep the attention board's STATUS visible at ordinary terminal widths;
	// legacy boards retain the existing drop-before-squeeze fit.
	if grouped {
		nameWidthLabels = append(nameWidthLabels, "IN PROGRESS")
	}
	nameWidth := display.MaxColWidth("NAME", nameWidthLabels, 60) + ui.TableColumnSep
	if grouped && h.tableAvailWidth() > 0 {
		other := 1 + (display.MaxColWidth("REPO", repos, 20) + ui.TableColumnSep) + (display.MaxColWidth("PR", prLabels, 8) + ui.TableColumnSep) + (homeStatusWidth + ui.TableColumnSep) + 5
		nameWidth = min(nameWidth, max(homeNameMinWidth(h.Width), h.tableAvailWidth()-other))
	}
	rcols := []ui.ResponsiveColumn{
		{Col: ui.CursorColumn, Priority: 0, MinWidth: 1},
		{Col: table.Column{Title: "REPO", Width: display.MaxColWidth("REPO", repos, 20) + ui.TableColumnSep}, Priority: 3, MinWidth: 6},
		{Col: table.Column{Title: "NAME", Width: nameWidth}, Priority: 0, MinWidth: homeNameMinWidth(h.Width)},
		{Col: table.Column{Title: "PR", Width: display.MaxColWidth("PR", prLabels, 8) + ui.TableColumnSep}, Priority: 2, MinWidth: 3},
		{Col: table.Column{Title: "STATUS", Width: homeStatusWidth + ui.TableColumnSep}, Priority: 1, MinWidth: 8},
	}

	fitted := ui.FitColumnsIndexed(rcols, h.tableAvailWidth())
	cols := ui.FittedColumns(fitted)
	statusVisible := false
	for _, col := range cols {
		if col.Title == "STATUS" {
			statusVisible = true
			break
		}
	}
	h.layout.statusVisible = statusVisible
	cursor := h.Table.Cursor()

	// Project primary and auxiliary rows onto the surviving columns.
	project := func(full table.Row) table.Row { return ui.ProjectRow(fitted, full) }
	h.layout.project = project
	h.selectedRow = cursor

	rows := make([]table.Row, 0, len(sessions)*2)
	for i, sess := range sessions {
		if h.sectionRows(sessions, i, sectioned) > 0 {
			rows = append(rows,
				project(table.Row{"", "", "", "", ""}),
				project(table.Row{"", "", h.sectionTitleCell(sessions, i), "", ""}))
		}
		rowIndex := len(rows)
		h.layout.primaryRows = append(h.layout.primaryRows, rowIndex)
		h.layout.sessionRows[sess.GetId()] = rowIndex
		rows = append(rows, h.primaryRow(sess, rowIndex, rowIndex == cursor))
		probe := *h
		probe.Spinner = spinner.New(spinner.WithSpinner(spinner.Spinner{Frames: []string{spinnerProbe}}))
		if statusVisible && strings.Contains(probe.RenderSessionStatus(sess), spinnerProbe) {
			h.layout.animatedRows = append(h.layout.animatedRows, rowIndex)
		}

		// The session's verified machine-local HTTP endpoints (BOS-474) sit
		// directly under the session they belong to and ABOVE any warning rows,
		// so the ":port" links read as part of the session rather than as part
		// of the warning block. Non-selectable, like the warning rows — the row
		// exists exactly when hasEndpointRow says so, which is the same
		// predicate sessionSubRowCount uses, so every cursor/height helper
		// agrees with what is actually emitted here.
		if h.hasEndpointRow(sess) {
			rows = append(rows, project(table.Row{"", "", ui.RenderSessionEndpoints(sess), "", ""}))
		}
		if sess.GetAttention() != nil {
			// The list shows no sentence; details and notifications do.
			continue
		}
		// Why a session is parked on an external event (BOS-668). Informational,
		// not a warning, so it is styled with the info color and sits above the
		// warning block. Same emit-and-count discipline as the
		// endpoint row: the predicate here is waitingHintLine != "", which is
		// exactly what sessionSubRowCount counts.
		if waitingHint := ui.WaitingHintLine(h.sessionWaitingReason(sess), ui.WaitingBadgeSuperseded(sess)); waitingHint != "" {
			rows = append(rows, project(table.Row{"", "", ui.StyleStatusInfo.Render(waitingHint), "", ""}))
		}
		// Warning sub-rows. The style is resolved PER HINT through the shared
		// BOS-855 gate, which folds in both reasons a hint goes recessive: the
		// merged/closed fade (BOS-246 — Part A clears the reason at the source, so
		// that half only paints the brief window before the next poll reconciles
		// the session) and a demotable hint on a row whose composite is a
		// live-activity label. selectedSessionWarningBlock calls the same helper,
		// so the list and the detail view cannot disagree.
		for hintIndex, hint := range ui.SessionWarningHints(sess) {
			if h.layout.warningRows == nil {
				h.layout.warningRows = make(map[int]homeWarningRow)
				h.warningCells = make(map[int]string)
			}
			row := len(rows)
			cell := ui.WarningHintStyle(sess, hint).Render(hint.Text)
			h.layout.warningRows[row] = homeWarningRow{sessionIndex: i, hintIndex: hintIndex}
			h.warningCells[row] = cell
			rows = append(rows, project(table.Row{"", "", cell, "", ""}))
		}
	}

	h.layout.recordRows(len(rows))
	ui.SetTableContent(&h.Table, cols, rows)
	h.Table.SetWidth(ui.ColumnsWidth(cols))
	h.Table.SetHeight(h.TableHeight())
	h.normalizeTableCursor(cursor)
}

// tableHeight returns the height to pass to table.SetHeight.
func (h Model) TableHeight() int {
	footerLines := 1
	switch {
	case h.AttentionAction.Active():
		footerLines = h.AttentionAction.LineCount(h.Width)
	case h.Rename.Active():
		// The rename editor is two content lines (input + key hints) where the
		// action bar it replaces is one at a wide terminal, so reserving the
		// default would leave the board a row too tall (BOS-837).
		footerLines = h.Rename.LineCount()
	case !h.Confirm.Active && !h.Upgrading && !h.Restarting:
		footerLines = h.SessionTableFooterLineCount()
	}
	// banner+newline + gap + actionbar padding + status + actionbar
	overhead := ui.BannerOverhead + 1 + ui.ActionBarPadY + h.statusHeight() + footerLines + h.Filter.Height()
	return ui.ClampedTableHeight(h.TableDataRowCount(), h.Height, overhead)
}
