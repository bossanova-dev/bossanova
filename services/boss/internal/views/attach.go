package views

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// attachOutputMsg carries a line of output from the stream.
type attachOutputMsg struct {
	line string
}

// attachStateChangeMsg carries a state change event.
type attachStateChangeMsg struct {
	prev pb.SessionState
	next pb.SessionState
}

// attachEndedMsg signals the session has ended.
type attachEndedMsg struct {
	finalState pb.SessionState
	reason     string
}

// attachErrMsg signals a stream error.
type attachErrMsg struct {
	err error
}

// AttachModel displays streaming output from an attached session.
type AttachModel struct {
	client    client.BossClient
	ctx       context.Context
	cancelFn  context.CancelFunc
	sessionID string

	session  *pb.Session
	stream   client.AttachStream
	viewport viewport.Model
	lines    []string
	state    pb.SessionState
	ended    bool
	endMsg   string
	err      error
	detach   bool
	width    int
	height   int
}

// NewAttachModel creates an AttachModel for the given session.
func NewAttachModel(c client.BossClient, parentCtx context.Context, sessionID string) AttachModel {
	ctx, cancel := context.WithCancel(parentCtx)
	vp := viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	vp.SoftWrap = true

	return AttachModel{
		client:    c,
		ctx:       ctx,
		cancelFn:  cancel,
		sessionID: sessionID,
		viewport:  vp,
	}
}

func (m AttachModel) Init() tea.Cmd {
	return tea.Batch(
		m.fetchSession(),
		m.startStream(),
	)
}

func (m AttachModel) fetchSession() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID)
		if err != nil {
			return attachErrMsg{err: err}
		}
		return sessionFetchedMsg{session: sess}
	}
}

// sessionFetchedMsg carries a session fetched for the header.
type sessionFetchedMsg struct {
	session *pb.Session
}

func (m AttachModel) startStream() tea.Cmd {
	return func() tea.Msg {
		stream, err := m.client.AttachSession(m.ctx, m.sessionID)
		if err != nil {
			return attachErrMsg{err: err}
		}
		return streamConnectedMsg{stream: stream}
	}
}

// streamConnectedMsg carries the established stream.
type streamConnectedMsg struct {
	stream client.AttachStream
}

func readFromStream(stream client.AttachStream) tea.Cmd {
	return func() tea.Msg {
		if !stream.Receive() {
			if err := stream.Err(); err != nil {
				return attachErrMsg{err: err}
			}
			return attachEndedMsg{reason: "stream closed"}
		}

		ev := stream.Msg()
		if ev.OutputLine != nil {
			return attachOutputMsg{line: ev.OutputLine.Text}
		}
		if ev.StateChange != nil {
			return attachStateChangeMsg{
				prev: ev.StateChange.PreviousState,
				next: ev.StateChange.NewState,
			}
		}
		if ev.SessionEnded != nil {
			reason := ""
			if ev.SessionEnded.Reason != nil {
				reason = *ev.SessionEnded.Reason
			}
			return attachEndedMsg{
				finalState: ev.SessionEnded.FinalState,
				reason:     reason,
			}
		}
		return nil
	}
}

func (m AttachModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionFetchedMsg:
		m.session = msg.session
		m.state = msg.session.State
		return m, nil

	case streamConnectedMsg:
		m.stream = msg.stream
		return m, readFromStream(m.stream)

	case attachOutputMsg:
		m.lines = append(m.lines, msg.line)
		m.viewport.SetContent(strings.Join(m.lines, "\n"))
		m.viewport.GotoBottom()
		return m, readFromStream(m.stream)

	case attachStateChangeMsg:
		m.state = msg.next
		label := fmt.Sprintf("--- state: %s → %s ---", StateLabel(msg.prev), StateLabel(msg.next))
		m.lines = append(m.lines, label)
		m.viewport.SetContent(strings.Join(m.lines, "\n"))
		m.viewport.GotoBottom()
		return m, readFromStream(m.stream)

	case attachEndedMsg:
		m.ended = true
		if msg.reason != "" {
			m.endMsg = msg.reason
		} else {
			m.endMsg = "session ended"
		}
		return m, nil

	case attachErrMsg:
		m.err = msg.err
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		headerHeight := 4
		footerHeight := 2
		m.viewport.SetWidth(msg.Width)
		m.viewport.SetHeight(msg.Height - headerHeight - footerHeight)
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "ctrl+c":
			m.cancelFn()
			m.detach = true
			return m, nil
		case "q":
			if m.ended {
				m.detach = true
				return m, nil
			}
		}
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

// Detached returns true if the user detached or the session ended and they pressed q.
func (m AttachModel) Detached() bool { return m.detach }

func (m AttachModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			styleError.Render(fmt.Sprintf("Error: %v", m.err)) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	var b strings.Builder

	// Header
	title := m.sessionID
	branch := ""
	if m.session != nil {
		title = m.session.Title
		branch = m.session.BranchName
	}

	stateStyled := lipgloss.NewStyle().Foreground(stateColor(m.state)).Render(StateLabel(m.state))
	header := fmt.Sprintf(" %s  %s  %s", title, stateStyled, styleSubtle.Render(branch))
	b.WriteString(lipgloss.NewStyle().Bold(true).Padding(0, 1).Render(header))
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Faint(true).Render(strings.Repeat("─", max(m.width, 40))))
	b.WriteString("\n")

	// Viewport
	b.WriteString(m.viewport.View())
	b.WriteString("\n")

	// Footer
	if m.ended {
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Foreground(colorYellow).Render(
			fmt.Sprintf("Session ended: %s", m.endMsg)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[q] back"))
	} else {
		b.WriteString(styleActionBar.Render("[esc] detach"))
	}

	return tea.NewView(b.String())
}
