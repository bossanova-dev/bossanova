package views

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/claude"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// claudeFinishedMsg signals that the interactive Claude process has exited or
// the user has detached from it.
type claudeFinishedMsg struct {
	err      error
	detached bool
}

// chatTitleUpdatedMsg signals a best-effort title update completed (ignored).
type chatTitleUpdatedMsg struct{}

// sessionFetchedMsg carries a session fetched via RPC.
type sessionFetchedMsg struct {
	session *pb.Session
}

// tmuxEnsuredMsg signals that the daemon has created/returned a tmux session.
type tmuxEnsuredMsg struct {
	tmuxSessionName string
	claudeID        string
}

// attachErrMsg signals a fetch or launch error.
type attachErrMsg struct {
	err error
}

// AttachModel launches an interactive Claude Code TUI in the session's worktree.
type AttachModel struct {
	client    client.BossClient
	ctx       context.Context
	sessionID string
	resumeID  string // Claude Code session UUID to resume (empty = new chat)
	claudeID  string // The Claude Code session UUID actually launched

	session   *pb.Session
	launching bool  // true while fetching session / ensuring tmux
	returned  bool  // true after claude exits
	claudeErr error // error from claude process (if any)
	detach    bool
	err       error
	width     int
	height    int
}

// NewAttachModel creates an AttachModel for the given session.
// If resumeID is non-empty, Claude Code will be launched with --resume.
func NewAttachModel(c client.BossClient, parentCtx context.Context, sessionID, resumeID string) AttachModel {
	return AttachModel{
		client:    c,
		ctx:       parentCtx,
		sessionID: sessionID,
		resumeID:  resumeID,
		launching: true,
	}
}

func (m AttachModel) Init() tea.Cmd {
	return m.fetchSession()
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

func (m AttachModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case sessionFetchedMsg:
		m.session = msg.session

		// Determine mode for EnsureTmuxSession RPC.
		mode := "new"
		claudeID := ""
		if m.resumeID != "" {
			mode = "resume"
			claudeID = m.resumeID
		}

		return m, m.ensureTmuxSession(mode, claudeID)

	case tmuxEnsuredMsg:
		m.claudeID = msg.claudeID
		m.launching = false

		sessionName := msg.tmuxSessionName
		cmd := exec.Command("tmux", "attach-session", "-t", sessionName)
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
			// Clear the primary screen buffer so tmux content
			// doesn't linger in scrollback when boss exits alt screen.
			fmt.Print("\033[2J\033[H\033[3J")
			// tmux attach exits 0 for both detach and session end;
			// check if the session still exists to distinguish the two.
			detached := exec.Command("tmux", "has-session", "-t", sessionName).Run() == nil
			return claudeFinishedMsg{err: err, detached: detached}
		})

	case claudeFinishedMsg:
		if msg.detached {
			// User detached from tmux (Ctrl+B d) — process is still running in background.
			m.detach = true
			return m, nil
		}
		m.returned = true
		m.claudeErr = msg.err
		// Auto-detach back to home screen.
		m.detach = true
		// Best-effort: update chat title from JSONL and report stopped status.
		return m, tea.Batch(m.updateChatTitle(), m.reportStopped())

	case chatTitleUpdatedMsg:
		// Ignored — fire-and-forget.
		return m, nil

	case attachErrMsg:
		m.err = msg.err
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.detach = true
			return m, nil
		}
	}

	return m, nil
}

// ensureTmuxSession calls the daemon RPC to create or reuse a tmux session.
func (m AttachModel) ensureTmuxSession(mode, claudeID string) tea.Cmd {
	return func() tea.Msg {
		tmuxName, returnedClaudeID, err := m.client.EnsureTmuxSession(m.ctx, m.sessionID, mode, claudeID)
		if err != nil {
			return attachErrMsg{err: fmt.Errorf("ensure tmux session: %w", err)}
		}
		return tmuxEnsuredMsg{tmuxSessionName: tmuxName, claudeID: returnedClaudeID}
	}
}

// updateChatTitle reads the Claude JSONL file and updates the chat title via RPC.
// If the session was never used (no real user message), it deletes the orphan record.
func (m AttachModel) updateChatTitle() tea.Cmd {
	if m.claudeID == "" || m.session == nil {
		return nil
	}
	claudeID := m.claudeID
	worktreePath := m.session.GetWorktreePath()
	return func() tea.Msg {
		title := claude.ChatTitle(worktreePath, claudeID)
		if title != "" {
			_ = m.client.UpdateChatTitle(m.ctx, claudeID, title)
		} else {
			// No real user message — session was never used. Remove the orphan.
			_ = m.client.DeleteChat(m.ctx, claudeID)
		}
		return chatTitleUpdatedMsg{}
	}
}

// reportStopped sends a fire-and-forget stopped status to the daemon so other
// clients see the change immediately.
func (m AttachModel) reportStopped() tea.Cmd {
	if m.claudeID == "" {
		return nil
	}
	claudeID := m.claudeID
	return func() tea.Msg {
		_ = m.client.ReportChatStatus(m.ctx, []*pb.ChatStatusReport{{
			ClaudeId:     claudeID,
			Status:       pb.ChatStatus_CHAT_STATUS_STOPPED,
			LastOutputAt: timestamppb.Now(),
		}})
		return nil
	}
}

// Detached returns true if the user should return to the home screen.
func (m AttachModel) Detached() bool { return m.detach }

// SessionID returns the session ID for navigation after detach.
func (m AttachModel) SessionID() string { return m.sessionID }

// ClaudeID returns the Claude Code session UUID for navigation after detach.
func (m AttachModel) ClaudeID() string { return m.claudeID }

func (m AttachModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.launching {
		var b strings.Builder
		title := m.sessionID
		if m.session != nil {
			title = m.session.Title
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			fmt.Sprintf("Launching Claude Code for %s...  Press Ctrl+B d to detach", title)))
		return tea.NewView(b.String())
	}

	if m.returned {
		var b strings.Builder
		if m.claudeErr != nil {
			b.WriteString(renderError(fmt.Sprintf("Claude Code exited with error: %v", m.claudeErr), m.width))
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Claude Code session ended."))
		}
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	return tea.NewView("")
}
