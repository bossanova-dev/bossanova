package views

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/google/uuid"
	"github.com/recurser/boss/internal/claude"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
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

// attachErrMsg signals a fetch or launch error.
type attachErrMsg struct {
	err error
}

// AttachModel launches an interactive Claude Code TUI in the session's worktree.
type AttachModel struct {
	client    client.BossClient
	ctx       context.Context
	manager   *bosspty.Manager
	sessionID string
	resumeID  string // Claude Code session UUID to resume (empty = new chat)
	claudeID  string // The Claude Code session UUID actually launched

	session   *pb.Session
	launching bool  // true while fetching session
	returned  bool  // true after claude exits
	claudeErr error // error from claude process (if any)
	detach    bool
	err       error
	width     int
	height    int
}

// NewAttachModel creates an AttachModel for the given session.
// If resumeID is non-empty, Claude Code will be launched with --resume.
func NewAttachModel(c client.BossClient, parentCtx context.Context, manager *bosspty.Manager, sessionID, resumeID string) AttachModel {
	return AttachModel{
		client:    c,
		ctx:       parentCtx,
		manager:   manager,
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
		m.launching = false

		// Launch interactive Claude in the session's worktree.
		cfg, _ := config.Load()
		var args []string
		if m.resumeID != "" {
			// Resume an existing Claude Code session.
			m.claudeID = m.resumeID
			args = append(args, "--resume", m.resumeID)
		} else {
			// New chat: generate UUID, record it, and launch with --session-id.
			newID := uuid.New().String()
			if _, err := m.client.RecordChat(m.ctx, m.sessionID, newID, "New chat"); err != nil {
				m.err = fmt.Errorf("record chat: %w", err)
				return m, nil
			}
			m.claudeID = newID
			args = append(args, "--session-id", newID)
		}
		if cfg.DangerouslySkipPermissions {
			args = append(args, "--dangerously-skip-permissions")
		}
		claudeCmd := exec.Command("claude", args...)
		claudeCmd.Dir = msg.session.GetWorktreePath()

		m.manager.RegisterSession(m.claudeID, m.sessionID)
		ptycmd := bosspty.NewPTYCommand(m.manager, m.claudeID, claudeCmd)
		return m, tea.Exec(ptycmd, func(err error) tea.Msg {
			// Clear the primary screen buffer so Claude's logo/intro
			// text doesn't linger in scrollback when boss exits alt screen.
			fmt.Print("\033[2J\033[H\033[3J")
			return claudeFinishedMsg{err: err, detached: ptycmd.Detached}
		})

	case claudeFinishedMsg:
		if msg.detached {
			// User pressed Ctrl+] — process is still running in background.
			m.detach = true
			return m, nil
		}
		m.returned = true
		m.claudeErr = msg.err
		// Auto-detach back to home screen.
		m.detach = true
		// Best-effort: update chat title from JSONL.
		return m, m.updateChatTitle()

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
		case "esc", "q":
			m.detach = true
			return m, nil
		}
	}

	return m, nil
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
		b.WriteString(lipgloss.NewStyle().Padding(1, 2).Render(
			fmt.Sprintf("Launching Claude Code for %s...  Press Ctrl+] to detach", title)))
		return tea.NewView(b.String())
	}

	if m.returned {
		var b strings.Builder
		if m.claudeErr != nil {
			b.WriteString(renderError(fmt.Sprintf("Claude Code exited with error: %v", m.claudeErr), m.width))
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(1, 2).Render("Claude Code session ended."))
		}
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[q] back"))
		return tea.NewView(b.String())
	}

	return tea.NewView("")
}
