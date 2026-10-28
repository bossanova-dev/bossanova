// Home's session table: row construction, status cells, and height. Split out
// of home.go (BOS-526); the declarations are unchanged.

package home

import (
	"fmt"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
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
	grouped, sectioned := h.hasAttentionBoard(), h.showsSections()
	if len(sessions) == 0 {
		h.Table.SetRows(nil)
		h.Table.SetHeight(h.TableHeight())
		return
	}

	repos := make([]string, len(sessions))
	names := make([]string, len(sessions))       // plain text for width calc
	linkedNames := make([]string, len(sessions)) // may contain OSC 8 hyperlinks
	nameWidthLabels := make([]string, 0, len(sessions)*2)
	prLabels := make([]string, len(sessions)) // visible text for width calc
	prs := make([]string, len(sessions))      // may contain OSC 8 hyperlinks
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
		linkedNames[i] = ui.RenderTrackerLink(sess, names[i])
		if sess.PrNumber != nil {
			prLabels[i] = fmt.Sprintf("#%d", *sess.PrNumber)
			prs[i] = ui.RenderPRLink(sess)
		} else {
			prLabels[i] = "-"
			prs[i] = "-"
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
	cursor := h.Table.Cursor()

	// Project primary and auxiliary rows onto the surviving columns.
	project := func(full table.Row) table.Row { return ui.ProjectRow(fitted, full) }

	mutedStrike := lipgloss.NewStyle().Foreground(ui.ColorMuted).Strikethrough(true)

	rows := make([]table.Row, 0, len(sessions)*2)
	for i, sess := range sessions {
		if h.sectionRows(sessions, i, sectioned) > 0 {
			rows = append(rows,
				project(table.Row{"", "", "", "", ""}),
				project(table.Row{"", "", h.sectionTitleCell(sessions, i), "", ""}))
		}
		rowIndex := len(rows)
		statusStyled := h.RenderSessionStatus(sess)

		repo, name, pr := repos[i], linkedNames[i], prs[i]
		selected := rowIndex == cursor
		merged := sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_MERGED ||
			sess.DisplayStatus == pb.DisplayStatus_DISPLAY_STATUS_CLOSED
		switch {
		case merged:
			repo = mutedStrike.Render(repos[i])
			// renderMutedTrackerLink styles the full title with raw ANSI and
			// wraps the tracker ID in OSC 8; do NOT wrap its output with
			// lipgloss — that strips the hyperlink envelope.
			name = ui.RenderMutedTrackerLink(sess, names[i])
			pr = ui.RenderMutedPRLink(sess)
		case selected:
			repo = ui.RenderSelectedText(repos[i])
			name = ui.RenderSelectedTrackerLink(sess, names[i])
			if sess.PrNumber != nil {
				pr = ui.RenderSelectedPRLink(sess)
			} else {
				pr = ui.RenderSelectedText(prs[i]) // the "-" placeholder, kept blue
			}
		}

		indicator := ""
		if rowIndex == cursor {
			indicator = ui.CursorChevron
		}
		row := project(table.Row{indicator, repo, name, pr, statusStyled})
		rows = append(rows, row)
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
		for _, hint := range ui.SessionWarningHints(sess) {
			hintRow := project(table.Row{"", "", ui.WarningHintStyle(sess, hint).Render(hint.Text), "", ""})
			rows = append(rows, hintRow)
		}
	}

	ui.SetTableContent(&h.Table, cols, rows)
	h.Table.SetWidth(ui.ColumnsWidth(cols))
	h.Table.SetHeight(h.TableHeight())
	h.Table.SetCursor(cursor)
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
