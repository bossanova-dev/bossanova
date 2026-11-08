// Package reviewqueue displays the cross-repository review queue.
package reviewqueue

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/telemetry"
	queue "github.com/recurser/bossalib/reviewqueue"
)

// Client is the queue screen's consumer contract.
type Client interface {
	ListReviewQueueRepos(context.Context) ([]*pb.Repo, error)
	ListReviewQueue(context.Context, *pb.Repo, pb.ReviewQueueSort) (*pb.ListReviewQueueResponse, error)
	GetFactoryHealth(context.Context, string) (*pb.GetFactoryHealthResponse, error)
	GetSession(context.Context, string, rpc.SessionReadOptions) (*pb.Session, error)
	ui.AttentionClient
	ui.BlockerAcceptClient
}

// LoadedMsg contains a complete poll, including partial repository failures.
type LoadedMsg struct {
	generation     *queueGeneration
	Rows           []*pb.ReviewQueueRow
	Counts         map[pb.ConfidenceLevel]int32
	Health         []string
	RepoErrs       []string
	Err            error
	WaitingSession *pb.Session
}
type queueGeneration struct{ _ byte }
type tickMsg struct{ generation *queueGeneration }
type openResultMsg struct{ Err error }
type mergeRequest struct{ _ byte }
type mergeSessionMsg struct {
	request *mergeRequest
	Session *pb.Session
	Head    string
	PR      int
	Err     error
}

// Model owns the queue and its action flows.
type Model struct {
	generation                 *queueGeneration
	client                     Client
	ctx                        context.Context
	telemetry                  telemetry.Client
	rows                       []*pb.ReviewQueueRow
	counts                     map[pb.ConfidenceLevel]int32
	health, repoErrs           []string
	table                      ui.Table
	order                      pb.ReviewQueueSort
	width, height              int
	returnView                 ui.View
	openURL                    func(string) error
	action                     ui.AttentionActionState
	accept                     ui.AcceptBlockerFlow
	status                     string
	err                        error
	loading, focused, fetching bool
	lastPoll                   time.Time
	refreshPending             bool
	mergePending               *mergeRequest
}

// NewModel creates a queue with an injected URL opener.
func NewModel(c Client, ctx context.Context, openURL func(string) error) Model {
	return Model{generation: &queueGeneration{}, client: c, ctx: ctx, openURL: openURL, focused: true, loading: true, fetching: true, lastPoll: time.Now(), order: pb.ReviewQueueSort_REVIEW_QUEUE_SORT_PRIORITY, table: ui.NewBossTable(nil, nil, 0)}
}
func (m *Model) SetTelemetry(c telemetry.Client) { m.telemetry = c }
func (m *Model) SetSize(w, h int)                { m.width = w; m.height = h; m.buildTable() }
func (m *Model) SetHeight(h int)                 { m.height = h; m.buildTable() }
func (m Model) Height() int                      { return m.height }
func (m Model) Width() int                       { return m.width }
func (m *Model) Table() *ui.Table                { return &m.table }
func (m Model) TableHeight() int                 { return m.tableHeight() }
func (m *Model) SetReturnView(v ui.View)         { m.returnView = v }
func (m Model) TextEntryActive() bool            { return m.accept.TextEntryActive() }
func (m Model) Order() pb.ReviewQueueSort        { return m.order }
func (m *Model) SetOrder(o pb.ReviewQueueSort) {
	if o != 0 {
		m.order = o
	}
	queue.Sort(m.rows, m.order)
	m.buildTable()
}
func (m Model) Init() tea.Cmd { return tea.Batch(m.load(), m.tick()) }
func (m Model) tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{generation: m.generation} })
}
func (m Model) selected() *pb.ReviewQueueRow {
	i := m.table.Cursor()
	if i < 0 || i >= len(m.rows) {
		return nil
	}
	return m.rows[i]
}

func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
	case tea.FocusMsg:
		m.focused = true
	case tea.BlurMsg:
		m.focused = false
	case tickMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		if !m.fetching && ui.PollDue(m.focused, m.lastPoll, time.Now()) {
			m.fetching = true
			m.lastPoll = time.Now()
			return m, tea.Batch(m.load(), m.tick())
		}
		return m, m.tick()
	case LoadedMsg:
		if msg.generation != m.generation {
			return m, nil
		}
		selected := m.selected().GetSession().GetId()
		m.rows, m.counts, m.health, m.repoErrs, m.err = msg.Rows, msg.Counts, msg.Health, msg.RepoErrs, msg.Err
		m.loading, m.fetching = false, false
		queue.Sort(m.rows, m.order)
		m.buildTable()
		m.selectID(selected)
		if m.action.Waiting != "" && msg.WaitingSession != nil {
			if status, _, settled := ui.SettleUnansweredMerge(msg.WaitingSession); settled {
				m.status = status
				m.action = ui.AttentionActionState{}
				m.buildTable()
			}
		}
		if m.refreshPending {
			m.refreshPending = false
			return m.refresh()
		}
	case mergeSessionMsg:
		return m.mergeSession(msg)
	case openResultMsg:
		if msg.Err != nil {
			m.status = msg.Err.Error()
		}
		return m.refresh()
	case ui.AttentionActionResultMsg:
		m.action = ui.AttentionActionState{}
		m.status = msg.Text
		if msg.Err != nil {
			m.status = msg.Err.Error()
		}
		if msg.Unsettled {
			m.action = ui.AwaitingMerge(msg.SessionID, msg.Text)
		}
		return m.refresh()
	case ui.AcceptBlockerResultMsg:
		m.accept, _ = m.accept.Update(msg)
		m.status = msg.Text
		if msg.Err != nil {
			m.status = msg.Err.Error()
		}
		return m.refresh()
	}
	if m.accept.Active() {
		var cmd tea.Cmd
		m.accept, cmd = m.accept.Update(msg)
		return m, cmd
	}
	if m.action.Active() {
		var cmd tea.Cmd
		m.action, cmd = m.action.Update(msg)
		m.buildTable()
		return m, cmd
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		return m.key(key)
	}
	return m, nil
}

func (m *Model) selectID(id string) {
	for i, r := range m.rows {
		if r.GetSession().GetId() == id {
			m.table.SetCursor(i)
			break
		}
	}
}

// IsAsyncMsg identifies queue work that must finish while a modal covers the screen.
func IsAsyncMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case tickMsg, LoadedMsg, mergeSessionMsg, openResultMsg, ui.AttentionActionResultMsg, ui.AcceptBlockerResultMsg:
		return true
	}
	return false
}
