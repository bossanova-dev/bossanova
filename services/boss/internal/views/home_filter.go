package views

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// boardSessions derives the visible subset without narrowing poll/notification state.
func (h HomeModel) boardSessions() []*pb.Session {
	if h.filter.Query() == "" {
		return h.sessions
	}
	sessions := make([]*pb.Session, 0, len(h.sessions))
	for _, sess := range h.sessions {
		if h.filter.Matches(homeFilterHaystack(sess)) {
			sessions = append(sessions, sess)
		}
	}
	return sessions
}

func homeFilterHaystack(sess *pb.Session) string {
	status := sess.GetDisplayLabel()
	if att := sess.GetAttention(); att != nil && !displaystatus.IsStateEndingLabel(status) && (att.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !displaystatus.FixShowsLabel(sess)) {
		// Progress labels are stable; the rendered spinner is deliberately excluded,
		// and a merge cell is matched by its resting text.
		if att.GetKind() == pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
			status = att.GetProgressLabel()
		} else {
			status = ansi.Strip((HomeModel{}).renderAttentionStatus(att, false))
		}
	}
	fields := []string{sess.GetRepoDisplayName(), sess.GetTitle(), sess.GetBranchName(), sess.GetTrackerId(), status}
	if sess.PrNumber != nil {
		fields = append(fields, fmt.Sprintf("#%d", sess.GetPrNumber()))
	}
	return strings.Join(fields, " ")
}

func (h HomeModel) startFilter() (HomeModel, tea.Cmd, bool) {
	if h.err != nil || h.loading || len(h.sessions) == 0 {
		return h, nil, true
	}
	cmd := h.filter.Activate()
	h.clearStatus()
	h.table.SetHeight(h.tableHeight())
	return h, cmd, true
}

func (h *HomeModel) rebuildFilteredBoard() {
	h.buildTableRows()
	if rows := h.primarySessionRows(); len(rows) > 0 {
		h.table.SetCursor(rows[0])
		updateCursorColumn(&h.table)
	}
}

func (h HomeModel) updateFilter(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd, changed := h.filter.Update(msg)
	if changed {
		h.rebuildFilteredBoard()
	}
	return h, cmd
}

func (h HomeModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		if !h.filter.Commit() {
			h.filter.Dismiss()
		}
		h.buildTableRows()
		return h, nil
	case "esc":
		h.filter.Reset()
		h.rebuildFilteredBoard()
		return h, nil
	case "up", "down", "ctrl+p", "ctrl+n", "ctrl+d", "ctrl+u":
		return h.handleTableKey(msg)
	case "alt+up":
		return h.moveSelectedSession(pb.MoveDirection_MOVE_DIRECTION_UP)
	case "alt+down":
		return h.moveSelectedSession(pb.MoveDirection_MOVE_DIRECTION_DOWN)
	default:
		return h.updateFilter(msg)
	}
}
