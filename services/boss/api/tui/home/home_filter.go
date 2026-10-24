package home

import (
	"fmt"
	"strings"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// boardSessions derives the visible subset without narrowing poll/notification state.
func (h Model) BoardSessions() []*pb.Session {
	if h.Filter.Query() == "" {
		return h.Sessions
	}
	sessions := make([]*pb.Session, 0, len(h.Sessions))
	for _, sess := range h.Sessions {
		if h.Filter.Matches(homeFilterHaystack(sess)) {
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
			status = ansi.Strip((Model{}).renderAttentionStatus(att, false))
		}
	}
	fields := []string{sess.GetRepoDisplayName(), sess.GetTitle(), sess.GetBranchName(), sess.GetTrackerId(), status}
	if sess.PrNumber != nil {
		fields = append(fields, fmt.Sprintf("#%d", sess.GetPrNumber()))
	}
	return strings.Join(fields, " ")
}

func (h Model) startFilter() (Model, tea.Cmd, bool) {
	if h.Err != nil || h.Loading || len(h.Sessions) == 0 {
		return h, nil, true
	}
	cmd := h.Filter.Activate()
	h.clearStatus()
	h.Table.SetHeight(h.TableHeight())
	return h, cmd, true
}

func (h *Model) rebuildFilteredBoard() {
	h.BuildTableRows()
	if rows := h.primarySessionRows(); len(rows) > 0 {
		h.Table.SetCursor(rows[0])
		ui.UpdateCursorColumn(&h.Table)
	}
}

func (h Model) updateFilter(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd, changed := h.Filter.Update(msg)
	if changed {
		h.rebuildFilteredBoard()
	}
	return h, cmd
}

func (h Model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		if !h.Filter.Commit() {
			h.Filter.Dismiss()
		}
		h.BuildTableRows()
		return h, nil
	case "esc":
		h.Filter.Reset()
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
