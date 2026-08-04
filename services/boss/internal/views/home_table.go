// Home's session table: row construction, status cells, and height. Split out
// of home.go (BOS-526); the declarations are unchanged.

package views

import (
	"fmt"

	"charm.land/bubbles/v2/table"
	"charm.land/lipgloss/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// homeTableBlockPadding mirrors the Padding(0, 1) renderSessionTable wraps the
// table in, so the table's own budget is the terminal width minus both sides.
// Declared here rather than inlined at the one call site so the two cannot
// drift: widen the block's padding and this is the single number to follow.
const homeTableBlockPadding = 1

// tableAvailWidth returns the rendered columns the session table may occupy —
// the terminal width less renderSessionTable's block padding on either side.
// See fitAvailWidth (theme.go) for the unknown-width and padding-swamped cases.
func (h HomeModel) tableAvailWidth() int {
	return fitAvailWidth(h.width, homeTableBlockPadding)
}

// homeNameMinWidth returns the floor fitColumns may squeeze Home's NAME column
// to at a given terminal width. NAME is priority 0 — it is never dropped,
// because a session list without session names is not a session list — so it
// is the only column that ever reaches the squeeze pass, and this is the number
// that decides how much of a title survives there.
//
// The floors are per-tier in principle because a 40-column window can live with
// a heavily truncated title while a 100-column one should not be: the narrow
// tier is already a compromise, the compact tier is a normal working terminal,
// and the full tier is a board that should read exactly as it always has.
//
// ON HOME TODAY, ONLY THE NARROW FLOOR IS REACHABLE — say so rather than let
// the paragraph above read as a live policy. fitColumns runs the drop loop to
// exhaustion before it squeezes anything, so the squeeze pass is only entered
// once the set is down to cursor + attention + NAME, which costs
// 1 + 1 + nameWidth + 3 gaps. NAME is capped at 60 (+ tableColumnSep = 61), so
// that set is over budget only while width - 2 < 66, i.e. below 68 columns; and
// the floor only BINDS (rather than merely being the clamp target) below about
// 23 columns. Both are far inside narrowWidthMax, so homeNameMinWidthCompact
// and homeNameMinWidthFull are currently inert: replacing this function with
// the constant homeNameMinWidthNarrow would render byte-identically at every
// terminal width.
//
// They are kept, and kept named, because the reachability above is a
// consequence of NAME's 60-column cap and of the drop-before-squeeze ordering,
// not a property of the layout — retune either (the BOS-570 follow-up recorded
// in buildTableRows would retune both) and the compact and full floors start
// biting immediately. A floor invented under pressure at that point would be a
// literal buried in a switch; these are a one-line edit where a reader looking
// for layout numbers will find them. An unknown width uses the full-tier floor
// for the same reason tableAvailWidth returns 0 there: assume the unfitted
// board until told otherwise.
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
	return renderDisplayStatus(sess, h.spinner)
}

// renderAttentionIndicator returns a colored "!" for sessions needing attention,
// or an empty string otherwise.
func renderAttentionIndicator(sess *pb.Session) string {
	if sess.AttentionStatus == nil || !sess.AttentionStatus.NeedsAttention {
		return ""
	}
	switch sess.AttentionStatus.Reason {
	case pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS:
		return styleStatusDanger.Render("!")
	case pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE:
		return ""
	default:
		return styleStatusWarning.Render("!")
	}
}

// buildTableRows rebuilds the table columns and rows from h.sessions.
func (h *HomeModel) buildTableRows() {
	if len(h.sessions) == 0 {
		h.table.SetRows(nil)
		return
	}

	repos := make([]string, len(h.sessions))
	names := make([]string, len(h.sessions))       // plain text for width calc
	linkedNames := make([]string, len(h.sessions)) // may contain OSC 8 hyperlinks
	nameWidthLabels := make([]string, 0, len(h.sessions)*2)
	prLabels := make([]string, len(h.sessions)) // visible text for width calc
	prs := make([]string, len(h.sessions))      // may contain OSC 8 hyperlinks
	for i, sess := range h.sessions {
		repos[i] = sess.RepoDisplayName
		if sess.Title != "" {
			names[i] = sess.Title
		} else {
			names[i] = sess.BranchName
		}
		nameWidthLabels = append(nameWidthLabels, names[i])
		if sessionHasEndpointRow(sess) {
			nameWidthLabels = append(nameWidthLabels, sessionEndpointLabels(sess))
		}
		if waitingHint := waitingHintLine(h.sessionWaitingReason(sess)); waitingHint != "" {
			nameWidthLabels = append(nameWidthLabels, waitingHint)
		}
		nameWidthLabels = append(nameWidthLabels, sessionWarningHints(sess)...)
		linkedNames[i] = renderTrackerLink(sess, names[i])
		if sess.PrNumber != nil {
			prLabels[i] = fmt.Sprintf("#%d", *sess.PrNumber)
			prs[i] = renderPRLink(sess)
		} else {
			prLabels[i] = "-"
			prs[i] = "-"
		}
	}

	// The declared column set, widest-first, carrying the session list's drop
	// order (BOS-572). Every width expression is unchanged from the pre-BOS-572
	// literal; only the priority/minWidth metadata is new.
	//
	// Drop order, most expendable first: REPO (the repo is usually obvious from
	// the session title and is constant down a single-repo board), then PR (the
	// number is a convenience, not an identity), then STATUS (the last thing to
	// go — it is the one cell that changes on its own). NAME and the two
	// one-column indicators are priority 0: a session list must always say
	// which session a row is, and which row the cursor is on.
	//
	// The minWidth on a droppable column is INERT today — fitColumns runs the
	// drop loop to exhaustion before it squeezes anything, so a column with
	// priority > 0 is always gone before the squeeze pass could reach it. Each
	// one is declared anyway so that re-ordering a priority later is a one-line
	// edit rather than an invitation to invent a floor under pressure.
	//
	// KNOWN LIMITATION of that drop-before-squeeze ordering, which the plan
	// specifies and this code implements faithfully: because NAME keeps its full
	// 60-column cap until every droppable column is gone, a long-titled board
	// can drop STATUS at a width where squeezing NAME by a handful of columns
	// would have kept it — leaving terminal width unused next to a column the
	// drop order calls "the last thing to go". Measured against
	// responsiveHomeSessions (a 71-character title), the board is
	// cursor+attention+NAME from 20 columns all the way to 85, and STATUS only
	// returns at 86 — so at 85 the drop leaves 17 columns unused, one short of
	// the 18 STATUS needs. The stranding is always bounded by the width of the
	// column that was dropped; it is a drop-overshoot, not unbounded dead space.
	//
	// DO NOT "fix" this by squeezing before each drop without also re-specifying
	// the acceptance criteria. That ordering is not merely a policy change: with
	// this same fixture at 72 columns, NAME has 45 columns of squeeze headroom
	// against a 32-column overflow, so a squeeze-first fit keeps ALL SIX columns
	// and the branch stops satisfying BOS-572's committed criterion "at a
	// 72-column terminal at least one of REPO / PR is absent". The follow-up that
	// re-policies this (a BOS-570 child, since the primitive is the epic's shared
	// producer) has to move the criterion and the tier constants together, and
	// re-capture the narrow proof still. Retuning the tiers above is the cheap
	// half-measure in the meantime.
	rcols := []responsiveColumn{
		{col: cursorColumn, priority: 0, minWidth: 1},
		{col: table.Column{Title: " ", Width: 1}, priority: 0, minWidth: 1},
		{col: table.Column{Title: "REPO", Width: maxColWidth("REPO", repos, 20) + tableColumnSep}, priority: 3, minWidth: 6},
		{col: table.Column{Title: "NAME", Width: maxColWidth("NAME", nameWidthLabels, 60) + tableColumnSep}, priority: 0, minWidth: homeNameMinWidth(h.width)},
		{col: table.Column{Title: "PR", Width: maxColWidth("PR", prLabels, 8) + tableColumnSep}, priority: 2, minWidth: 3},
		{col: table.Column{Title: "STATUS", Width: 16 + tableColumnSep}, priority: 1, minWidth: 8},
	}

	fitted := fitColumnsIndexed(rcols, h.tableAvailWidth())
	cols := fittedColumns(fitted)

	// project maps a row built in the FULL declared order onto the surviving
	// columns. Every row — primary, endpoint, warning — is built with all six
	// cells and then projected here, so no call site ever writes a column index
	// by hand: the NAME cell moves from index 3 to index 2 the moment REPO is
	// dropped, and a hard-coded index would silently put an endpoint list or a
	// setup-failure hint in the wrong column (or off the end of a shortened
	// row) on any narrow board.
	project := func(full table.Row) table.Row { return projectRow(fitted, full) }

	mutedStrike := lipgloss.NewStyle().Foreground(colorMuted).Strikethrough(true)

	cursor := h.table.Cursor()
	rows := make([]table.Row, 0, len(h.sessions)*2)
	for i, sess := range h.sessions {
		rowIndex := len(rows)
		statusStyled := h.renderSessionStatus(sess)

		attn := renderAttentionIndicator(sess)
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
			// The table applies the selected (blue) foreground to the whole
			// row, but a styled leading cell (the attention "!") emits a full
			// SGR reset that cancels it for the cells after it (repo/name/pr).
			// Re-assert the selection color on those cells with raw ANSI so it
			// survives regardless of the reset. See BOS-103.
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
		row := project(table.Row{indicator, attn, repo, name, pr, statusStyled})
		rows = append(rows, row)
		// The session's verified machine-local HTTP endpoints (BOS-474) sit
		// directly under the session they belong to and ABOVE any warning rows,
		// so the ":port" links read as part of the session rather than as part
		// of the warning block. Non-selectable, like the warning rows — the row
		// exists exactly when sessionHasEndpointRow says so, which is the same
		// predicate sessionSubRowCount uses, so every cursor/height helper
		// agrees with what is actually emitted here.
		if sessionHasEndpointRow(sess) {
			rows = append(rows, project(table.Row{"", "", "", renderSessionEndpoints(sess), "", ""}))
		}
		// Why a session is parked on an external event (BOS-668). Informational,
		// not a warning, so it is styled with the info color and sits above the
		// danger-styled warning block. Same emit-and-count discipline as the
		// endpoint row: the predicate here is waitingHintLine != "", which is
		// exactly what sessionSubRowCount counts.
		if waitingHint := waitingHintLine(h.sessionWaitingReason(sess)); waitingHint != "" {
			rows = append(rows, project(table.Row{"", "", "", styleStatusInfo.Render(waitingHint), "", ""}))
		}
		// A merged/closed row's warnings no longer need to alarm — dim them
		// (BOS-246). Part A clears the reason at the source, so this only paints
		// the brief window before the next poll reconciles the session.
		hintStyle := styleStatusDanger
		if merged {
			hintStyle = styleStatusDangerFaded
		}
		for _, hint := range sessionWarningHints(sess) {
			hintRow := project(table.Row{"", "", "", hintStyle.Render(hint), "", ""})
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
	if !h.confirm.active && !h.upgrading && !h.restarting {
		footerLines = h.sessionTableFooterLineCount()
	}
	overhead := bannerOverhead + 1 + actionBarPadY + footerLines // banner+newline + gap + actionbar padding + actionbar
	return clampedTableHeight(h.tableDataRowCount(), h.height, overhead)
}
