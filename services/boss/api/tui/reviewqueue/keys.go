package reviewqueue

import (
	"fmt"
	"net/url"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	queue "github.com/recurser/bossalib/reviewqueue"
)

var orders = []pb.ReviewQueueSort{pb.ReviewQueueSort_REVIEW_QUEUE_SORT_PRIORITY, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_MOST_CONFIDENT, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_LEAST_CONFIDENT, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_SMALLEST, pb.ReviewQueueSort_REVIEW_QUEUE_SORT_OLDEST}

func (m Model) key(k tea.KeyMsg) (Model, tea.Cmd) {
	row := m.selected()
	s := row.GetSession()
	switch k.String() {
	case "esc":
		return m, func() tea.Msg { return ui.SwitchViewMsg{View: m.returnView} }
	case "q":
		return m, tea.Quit
	case "o":
		id := s.GetId()
		for i, o := range orders {
			if o == m.order {
				m.order = orders[(i+1)%len(orders)]
				break
			}
		}
		queue.Sort(m.rows, m.order)
		m.buildTable()
		m.selectID(id)
		return m, nil
	case "enter":
		if row != nil {
			return m, func() tea.Msg { return ui.SwitchViewMsg{View: ui.ViewChatPicker, SessionID: s.GetId()} }
		}
	case "p", "g":
		if row == nil {
			return m, nil
		}
		target, missing := s.GetPrUrl(), "No PR for #%d"
		if k.String() == "p" {
			target, missing = queue.Park(s).GetProofUrl(), "No proof page for #%d"
		}
		if !webURL(target) {
			m.status = fmt.Sprintf(missing, s.GetPrNumber())
			return m, nil
		}
		return m, func() tea.Msg { return openResultMsg{Err: m.openURL(target)} }
	case "m":
		if row == nil || row.GetIsAuditSample() || m.action.MergeUnresolved() || m.mergePending != nil {
			return m, nil
		}
		if _, ok := ui.AttentionBinding(s, "m", false); !ok {
			return m, nil
		}
		m.mergePending = &mergeRequest{}
		return m, func() tea.Msg {
			fresh, err := m.getSession(s.GetId())
			return mergeSessionMsg{request: m.mergePending, Session: fresh, Head: s.GetPrDisplayHeadSha(), PR: int(s.GetPrNumber()), Err: err}
		}
	case "a":
		if row == nil || row.GetConfidenceLevel() != pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED {
			return m, nil
		}
		var cmd tea.Cmd
		m.accept, cmd = m.accept.Start(m.ctx, m.client, ui.AcceptBlockerTarget{SessionID: s.GetId(), PR: int(s.GetPrNumber())})
		return m, cmd
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(k)
	return m, cmd
}
func webURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func (m Model) mergeSession(msg mergeSessionMsg) (Model, tea.Cmd) {
	if msg.request == nil || msg.request != m.mergePending {
		return m, nil
	}
	m.mergePending = nil
	if m.action.Active() || m.accept.Active() {
		return m, nil
	}
	if msg.Err != nil {
		m.status = msg.Err.Error()
		return m.refresh()
	}
	if msg.Session.GetPrDisplayHeadSha() != msg.Head {
		m.status = ui.HeadMovedError(msg.PR).Error()
		return m.refresh()
	}
	var cmd tea.Cmd
	m.action, cmd, _ = m.action.Start(m.ctx, m.client, msg.Session, "m", false, m.openURL)
	m.buildTable()
	return m, cmd
}
