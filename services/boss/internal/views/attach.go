package views

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/google/uuid"
	"github.com/recurser/boss/internal/claude"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
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

// attachReadyMsg carries the session plus its chat list, fetched via RPC
// before launching Claude. The chat list is needed so first-attach prefill
// can be gated on len(chats) == 0 (matching the original semantics from the
// daemon-side prefill that was removed in PR #179).
type attachReadyMsg struct {
	session *pb.Session
	chats   []*pb.ClaudeChat
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
	return m.fetchAttachState()
}

func (m AttachModel) fetchAttachState() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID)
		if err != nil {
			return attachErrMsg{err: err}
		}
		// Best-effort chat list — used only to gate first-attach prefill.
		// A failure here should not block attach; treat as "unknown count"
		// (i.e. assume non-empty so we don't accidentally prefill on a
		// resumed session).
		chats, _ := m.client.ListChats(m.ctx, m.sessionID)
		return attachReadyMsg{session: sess, chats: chats}
	}
}

func (m AttachModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case attachReadyMsg:
		m.session = msg.session
		m.launching = false

		// Decide whether this is a fresh first attach to a tracker-backed
		// session that should have its plan pasted into Claude's input box.
		// Mirrors the gating that used to live in shouldConsiderPrefill on
		// the daemon side (removed in PR #179 along with the per-chat tmux
		// indirection that hosted it).
		prefillPlan := ""
		if m.resumeID == "" &&
			msg.session.GetTrackerId() != "" &&
			msg.session.GetPlan() != "" &&
			len(msg.chats) == 0 {
			prefillPlan = msg.session.GetPlan()
		}

		// Resolve the Claude session UUID and ask the daemon to record the
		// chat AND ensure a tmux session is hosting it. Doing the create on
		// the daemon side means the tmux server outlives bossd (it
		// double-forks), so the user can kill bossd, restart it, and
		// reattach to the same chat without losing state — which the pre-
		// PR-#179 architecture provided implicitly via tmux indirection
		// and the post-#179 "direct exec" replacement quietly lost.
		resume := m.resumeID != ""
		if resume {
			m.claudeID = m.resumeID
		} else {
			m.claudeID = uuid.New().String()
		}
		chat, err := m.client.RecordChat(m.ctx, m.sessionID, m.claudeID, "New chat", resume)
		if err != nil {
			m.err = fmt.Errorf("record chat: %w", err)
			return m, nil
		}
		tmuxName := chat.GetTmuxSessionName()
		if tmuxName == "" {
			m.err = fmt.Errorf("daemon did not return a tmux session name; check that tmux is installed")
			return m, nil
		}

		// Attach to the daemon-owned tmux session. Ctrl+X / Ctrl+] are
		// bound to detach-client by the daemon's bindDetachKeys, so the
		// keystroke detaches the client cleanly and the exec returns 0.
		// We distinguish "user detached" from "claude exited" in
		// claudeFinishedMsg by probing tmux has-session afterwards.
		claudeCmd := exec.Command("tmux", "attach", "-t", tmuxName)
		claudeCmd.Dir = msg.session.GetWorktreePath()

		m.manager.RegisterSession(m.claudeID, m.sessionID)
		ptycmd := bosspty.NewPTYCommand(m.manager, m.claudeID, claudeCmd)

		if prefillPlan != "" {
			go prefillClaudeInput(m.ctx, m.manager, m.claudeID, prefillPlan)
		}

		return m, tea.Exec(ptycmd, func(err error) tea.Msg {
			// Clear the primary screen buffer so Claude content doesn't
			// linger in scrollback when boss exits alt screen.
			fmt.Print("\033[2J\033[H\033[3J")
			// Treat as detach if EITHER signal fires:
			//   - bosspty intercepted Ctrl+X / Ctrl+] (user-intent signal)
			//   - the tmux session is still alive (the chat survives even
			//     if bosspty didn't see the keystroke, e.g. user typed the
			//     literal `tmux detach` prefix or detached via the menu).
			// Both should agree under normal use; a permissive OR means we
			// never strand a still-alive chat behind an "ended" UI.
			detached := ptycmd.Detached || tmuxSessionAlive(tmuxName)
			return claudeFinishedMsg{err: err, detached: detached}
		})

	case claudeFinishedMsg:
		if msg.detached {
			// User pressed Ctrl+X or Ctrl+] — process is still running in
			// the background; drop back to the caller's view.
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

// tmuxSessionAlive reports whether a tmux session by the given name is still
// running. Used by the attach flow to distinguish a Ctrl+X / Ctrl+] detach
// (session still alive on the tmux server, claude still running inside) from
// a real claude exit (session torn down by tmux when its only window's
// command exited).
func tmuxSessionAlive(name string) bool {
	if name == "" {
		return false
	}
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

// prefillClaudeInput writes the session plan into Claude's TUI input box via
// a bracketed-paste sequence so the user lands on a ready-to-refine prompt
// instead of an empty input. Best-effort and non-fatal — a missing prefill
// is a UX degradation, not a reason to abort the attach.
//
// Flow:
//  1. Wait for the bosspty.Manager to register the *Process for claudeID
//     (tea.Exec calls Manager.GetOrStart on its own goroutine, so there is a
//     small race window between this goroutine starting and the entry being
//     in the map).
//  2. Wait for Claude Code's input-box prompt indicator (❯) to appear
//     in the PTY output. If the marker never shows up within the
//     deadline, paste anyway — same fail-open policy as the daemon-side
//     prefill that this replaces. We match on the prompt character
//     rather than the default footer so users with custom statuslines
//     (which replace the footer entirely) still get the prefill.
//  3. Write \x1b[200~ + plan + \x1b[201~ to the PTY. Bracketed paste tells
//     Claude "treat this as paste, not keystrokes," so it lands in the input
//     box without auto-submitting.
func prefillClaudeInput(ctx context.Context, manager *bosspty.Manager, claudeID, plan string) {
	const (
		readyMarker      = "❯"
		recentBufferSize = 4096
		processWait      = 3 * time.Second
		readyWait        = 2 * time.Second
		pollInterval     = 100 * time.Millisecond
	)

	// Step 1: wait for the process to appear in the manager.
	var proc *bosspty.Process
	procDeadline := time.Now().Add(processWait)
	for {
		if p, ok := manager.Get(claudeID); ok {
			proc = p
			break
		}
		if time.Now().After(procDeadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Step 2: wait for Claude's input box to be ready, then paste.
	marker := []byte(readyMarker)
	readyDeadline := time.Now().Add(readyWait)
	for time.Now().Before(readyDeadline) {
		if bytes.Contains(proc.RecentOutput(recentBufferSize), marker) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Step 3: paste. Errors are silently dropped — the process may have
	// exited or detached by now; either way the user can recover by typing
	// the prompt manually.
	payload := append([]byte("\x1b[200~"), plan...)
	payload = append(payload, "\x1b[201~"...)
	_ = proc.WriteInput(payload)
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
			fmt.Sprintf("Launching Claude Code for %s...  Press Ctrl+X to detach", title)))
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
