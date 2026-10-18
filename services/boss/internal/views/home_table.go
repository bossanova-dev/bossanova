// Home's session table: row construction, status cells, and height. Split out
// of home.go (BOS-526); the declarations are unchanged.

package views

import (
	"fmt"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
func (h HomeModel) tableAvailWidth() int {
	return fitAvailWidth(h.width, homeTableBlockPadding)
}

// NAME remains selectable and visible when optional columns are dropped.
const (
	homeNameMinWidthNarrow  = 16
	homeNameMinWidthCompact = 24
	homeNameMinWidthFull    = 32
)

func homeNameMinWidth(width int) int {
	switch {
	case width <= 0 || width > compactWidthMax:
		return homeNameMinWidthFull
	case width <= narrowWidthMax:
		return homeNameMinWidthNarrow
	default:
		return homeNameMinWidthCompact
	}
}

func (h HomeModel) renderSessionStatus(sess *pb.Session) string {
	if sess != nil && h.isArchiving(sess.Id) {
		return renderArchivingStatus(h.spinner)
	}
	if h.isMerging(sess.GetId()) {
		return renderMergingStatus(h.spinner)
	}
	if displaystatus.IsStateEndingLabel(sess.GetDisplayLabel()) {
		return renderDisplayStatus(sess, h.spinner)
	}
	// A finished row reads its PR's end ("✓ merged", "closed") like the rest of
	// its struck-through cells.
	if a := sess.GetAttention(); a != nil && a.GetKind() != pb.AttentionKind_ATTENTION_KIND_DONE && (a.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !displaystatus.FixShowsLabel(sess)) {
		return h.renderAttentionStatus(a, sess.GetDisplayLabel() == statusWorking)
	}
	if sess != nil && sess.GetDisplayLabel() == statusWorking {
		if phase := h.daemonPhases[sess.GetId()]; phase != "" {
			label := phase
			if sess.GetDisplaySpinner() {
				label = h.spinner.View() + label
			}
			return styleForIntent(sess.GetDisplayIntent()).Render(ansi.Truncate(label, homeStatusWidth, "…"))
		}
	}
	return renderDisplayStatus(sess, h.spinner)
}

// buildTableRows rebuilds the table columns and rows from h.sessions.
func (h *HomeModel) buildTableRows() {
	h.sessions = attentionBoard(h.sessions)
	if len(h.sessions) == 0 {
		// Archived-only polls also hide the board after projection.
		h.filter.Reset()
	}
	sessions := h.boardSessions()
	h.filter.SetCounts(len(sessions), len(h.sessions))
	grouped, sectioned := h.hasAttentionBoard(), h.showsSections()
	if len(sessions) == 0 {
		h.table.SetRows(nil)
		h.table.SetHeight(h.tableHeight())
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
			nameWidthLabels = append(nameWidthLabels, sessionEndpointLabels(sess))
		}
		if waitingHint := waitingHintLine(h.sessionWaitingReason(sess), waitingBadgeSuperseded(sess)); sess.GetAttention() == nil && waitingHint != "" {
			nameWidthLabels = append(nameWidthLabels, waitingHint)
		}
		if sess.GetAttention() == nil {
			nameWidthLabels = append(nameWidthLabels, sessionWarningHintTexts(sess)...)
		}
		linkedNames[i] = renderTrackerLink(sess, names[i])
		if sess.PrNumber != nil {
			prLabels[i] = fmt.Sprintf("#%d", *sess.PrNumber)
			prs[i] = renderPRLink(sess)
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
	nameWidth := maxColWidth("NAME", nameWidthLabels, 60) + tableColumnSep
	if grouped && h.tableAvailWidth() > 0 {
		other := 1 + (maxColWidth("REPO", repos, 20) + tableColumnSep) + (maxColWidth("PR", prLabels, 8) + tableColumnSep) + (homeStatusWidth + tableColumnSep) + 5
		nameWidth = min(nameWidth, max(homeNameMinWidth(h.width), h.tableAvailWidth()-other))
	}
	rcols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: "REPO", Width: maxColWidth("REPO", repos, 20) + tableColumnSep}, priority: 3, minWidth: 6},
		{col: table.Column{Title: "NAME", Width: nameWidth}, priority: 0, minWidth: homeNameMinWidth(h.width)},
		{col: table.Column{Title: "PR", Width: maxColWidth("PR", prLabels, 8) + tableColumnSep}, priority: 2, minWidth: 3},
		{col: table.Column{Title: "STATUS", Width: homeStatusWidth + tableColumnSep}, priority: 1, minWidth: 8},
	}

	fitted := fitColumnsIndexed(rcols, h.tableAvailWidth())
	cols := fittedColumns(fitted)
	cursor := h.table.Cursor()

	// Project primary and auxiliary rows onto the surviving columns.
	project := func(full table.Row) table.Row { return projectRow(fitted, full) }

	mutedStrike := lipgloss.NewStyle().Foreground(colorMuted).Strikethrough(true)

	rows := make([]table.Row, 0, len(sessions)*2)
	for i, sess := range sessions {
		if h.sectionRows(sessions, i, sectioned) > 0 {
			rows = append(rows,
				project(table.Row{"", "", "", "", ""}),
				project(table.Row{"", "", h.sectionTitleCell(sessions, i), "", ""}))
		}
		rowIndex := len(rows)
		statusStyled := h.renderSessionStatus(sess)

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
			name = renderMutedTrackerLink(sess, names[i])
			pr = renderMutedPRLink(sess)
		case selected:
			repo = renderSelectedText(repos[i])
			name = renderSelectedTrackerLink(sess, names[i])
			if sess.PrNumber != nil {
				pr = renderSelectedPRLink(sess)
			} else {
				pr = renderSelectedText(prs[i]) // the "-" placeholder, kept blue
			}
		}

		indicator := ""
		if rowIndex == cursor {
			indicator = cursorChevron
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
			rows = append(rows, project(table.Row{"", "", renderSessionEndpoints(sess), "", ""}))
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
		if waitingHint := waitingHintLine(h.sessionWaitingReason(sess), waitingBadgeSuperseded(sess)); waitingHint != "" {
			rows = append(rows, project(table.Row{"", "", styleStatusInfo.Render(waitingHint), "", ""}))
		}
		// Warning sub-rows. The style is resolved PER HINT through the shared
		// BOS-855 gate, which folds in both reasons a hint goes recessive: the
		// merged/closed fade (BOS-246 — Part A clears the reason at the source, so
		// that half only paints the brief window before the next poll reconciles
		// the session) and a demotable hint on a row whose composite is a
		// live-activity label. selectedSessionWarningBlock calls the same helper,
		// so the list and the detail view cannot disagree.
		for _, hint := range sessionWarningHints(sess) {
			hintRow := project(table.Row{"", "", warningHintStyle(sess, hint).Render(hint.Text), "", ""})
			rows = append(rows, hintRow)
		}
	}

	setTableContent(&h.table, cols, rows)
	h.table.SetWidth(columnsWidth(cols))
	h.table.SetHeight(h.tableHeight())
	h.table.SetCursor(cursor)
	h.normalizeTableCursor(cursor)
	updateCursorColumn(&h.table)
}

// StateLabel returns a short human-readable label for a session state.
func StateLabel(state pb.SessionState) string {
	switch state {
	case pb.SessionState_SESSION_STATE_CREATING_WORKTREE:
		return "creating"
	case pb.SessionState_SESSION_STATE_STARTING_AGENT:
		return "starting"
	case pb.SessionState_SESSION_STATE_PUSHING_BRANCH:
		return "pushing"
	case pb.SessionState_SESSION_STATE_OPENING_DRAFT_PR:
		return "opening PR"
	case pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN:
		return "implementing"
	case pb.SessionState_SESSION_STATE_AWAITING_CHECKS:
		return "checks"
	case pb.SessionState_SESSION_STATE_FIXING_CHECKS:
		return "fixing"
	case pb.SessionState_SESSION_STATE_GREEN_DRAFT:
		return "green"
	case pb.SessionState_SESSION_STATE_READY_FOR_REVIEW:
		return "review"
	case pb.SessionState_SESSION_STATE_BLOCKED:
		return "blocked"
	case pb.SessionState_SESSION_STATE_MERGED:
		return "✓ merged"
	case pb.SessionState_SESSION_STATE_CLOSED:
		return "closed"
	case pb.SessionState_SESSION_STATE_ORPHANED:
		return "orphaned"
	default:
		return "unknown"
	}
}

// tableHeight returns the height to pass to table.SetHeight.
func (h HomeModel) tableHeight() int {
	footerLines := 1
	switch {
	case h.attentionAction.active():
		footerLines = h.attentionAction.lineCount(h.width)
	case h.rename.Active():
		// The rename editor is two content lines (input + key hints) where the
		// action bar it replaces is one at a wide terminal, so reserving the
		// default would leave the board a row too tall (BOS-837).
		footerLines = h.rename.lineCount()
	case !h.confirm.active && !h.upgrading && !h.restarting:
		footerLines = h.sessionTableFooterLineCount()
	}
	// banner+newline + gap + actionbar padding + status + actionbar
	overhead := bannerOverhead + 1 + actionBarPadY + h.statusHeight() + footerLines + h.filter.Height()
	return clampedTableHeight(h.tableDataRowCount(), h.height, overhead)
}
