package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/claude"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// chatPickerSessionMsg carries a session fetched via RPC for the chat picker.
type chatPickerSessionMsg struct {
	session *pb.Session
}

// chatPickerErrMsg signals a fetch error in the chat picker.
type chatPickerErrMsg struct {
	err error
}

// chatsListedMsg carries the result of listing chats via RPC.
type chatsListedMsg struct {
	chats []*pb.ClaudeChat
	err   error
}

// chatTitlesBackfilledMsg carries updated titles for chats that were "New chat".
type chatTitlesBackfilledMsg struct {
	updates map[string]string // claude_id -> title
}

// chatDeletedMsg signals that a chat was deleted (or failed to delete).
type chatDeletedMsg struct {
	claudeID string
	err      error
}

// ChatPickerModel lets the user choose between starting a new chat or
// resuming a previous Claude Code conversation for a session.
type ChatPickerModel struct {
	client    client.BossClient
	ctx       context.Context
	sessionID string

	session *pb.Session
	chats   []*pb.ClaudeChat
	cursor  int
	loading bool
	err     error
	cancel  bool
	width   int
	height  int

	// Remove confirmation
	confirming bool
}

// NewChatPickerModel creates a ChatPickerModel for the given session.
func NewChatPickerModel(c client.BossClient, parentCtx context.Context, sessionID string) ChatPickerModel {
	return ChatPickerModel{
		client:    c,
		ctx:       parentCtx,
		sessionID: sessionID,
		loading:   true,
	}
}

func (m ChatPickerModel) Init() tea.Cmd {
	return m.fetchSession()
}

func (m ChatPickerModel) fetchSession() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID)
		if err != nil {
			return chatPickerErrMsg{err: err}
		}
		return chatPickerSessionMsg{session: sess}
	}
}

func (m ChatPickerModel) listChats() tea.Cmd {
	return func() tea.Msg {
		chats, err := m.client.ListChats(m.ctx, m.sessionID)
		return chatsListedMsg{chats: chats, err: err}
	}
}

// backfillTitles reads JSONL files for chats still titled "New chat" and
// updates their titles via RPC. This is best-effort and non-blocking.
func (m ChatPickerModel) backfillTitles() tea.Cmd {
	if m.session == nil {
		return nil
	}
	var needsUpdate []*pb.ClaudeChat
	for _, c := range m.chats {
		if c.Title == "" || c.Title == "New chat" {
			needsUpdate = append(needsUpdate, c)
		}
	}
	if len(needsUpdate) == 0 {
		return nil
	}
	worktreePath := m.session.GetWorktreePath()
	return func() tea.Msg {
		updates := make(map[string]string)
		for _, c := range needsUpdate {
			title := claude.ChatTitle(worktreePath, c.ClaudeId)
			if title != "" {
				updates[c.ClaudeId] = title
				_ = m.client.UpdateChatTitle(m.ctx, c.ClaudeId, title)
			}
		}
		return chatTitlesBackfilledMsg{updates: updates}
	}
}

func (m ChatPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case chatPickerSessionMsg:
		m.session = msg.session
		return m, m.listChats()

	case chatsListedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.chats = msg.chats
		return m, m.backfillTitles()

	case chatTitlesBackfilledMsg:
		for i, chat := range m.chats {
			if title, ok := msg.updates[chat.ClaudeId]; ok {
				m.chats[i].Title = title
			}
		}
		return m, nil

	case chatDeletedMsg:
		if msg.err == nil {
			// Remove the deleted chat from the list.
			for i, chat := range m.chats {
				if chat.ClaudeId == msg.claudeID {
					m.chats = append(m.chats[:i], m.chats[i+1:]...)
					break
				}
			}
			// Adjust cursor if it's now out of bounds.
			if m.cursor > len(m.chats) {
				m.cursor = len(m.chats)
			}
		}
		return m, nil

	case chatPickerErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		if m.confirming {
			return m.updateDeleteConfirm(msg)
		}

		switch msg.String() {
		case "esc", "q":
			m.cancel = true
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down", "j":
			// cursor 0 = "New chat", cursor 1..N = previous chats
			maxCursor := len(m.chats) // 0-based: 0 to len(chats)
			if m.cursor < maxCursor {
				m.cursor++
			}
			return m, nil
		case "d":
			if m.cursor > 0 && m.cursor <= len(m.chats) {
				m.confirming = true
			}
			return m, nil
		case "enter":
			var resumeID string
			if m.cursor > 0 && m.cursor <= len(m.chats) {
				resumeID = m.chats[m.cursor-1].ClaudeId
			}
			return m, func() tea.Msg {
				return switchViewMsg{
					view:      ViewAttach,
					sessionID: m.sessionID,
					resumeID:  resumeID,
				}
			}
		}
	}

	return m, nil
}

func (m ChatPickerModel) updateDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.confirming = false
		claudeID := m.chats[m.cursor-1].ClaudeId
		return m, func() tea.Msg {
			err := m.client.DeleteChat(m.ctx, claudeID)
			return chatDeletedMsg{claudeID: claudeID, err: err}
		}
	case "n", "esc":
		m.confirming = false
	}
	return m, nil
}

// Cancelled returns true if the user cancelled the chat picker.
func (m ChatPickerModel) Cancelled() bool { return m.cancel }

func (m ChatPickerModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.loading {
		title := m.sessionID
		if m.session != nil {
			title = m.session.Title
		}
		return tea.NewView(
			lipgloss.NewStyle().Padding(1, 2).Render(
				fmt.Sprintf("Loading chats for %s...", title)),
		)
	}

	var b strings.Builder

	// Header with session title.
	title := m.sessionID
	if m.session != nil {
		title = m.session.Title
	}
	b.WriteString(styleTitle.Render(title))
	b.WriteString("\n\n")

	// "New chat" option (always cursor 0).
	newChatLine := "  New chat"
	if m.cursor == 0 {
		newChatLine = "> New chat"
		newChatLine = styleSelected.Render(newChatLine)
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(newChatLine))
	b.WriteString("\n")

	if len(m.chats) > 0 {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Faint(true).Render("Previous chats"))
		b.WriteString("\n\n")

		for i, chat := range m.chats {
			cursor := "  "
			if m.cursor == i+1 {
				cursor = "> "
			}

			chatTitle := chat.Title
			if chatTitle == "" {
				chatTitle = "New chat"
			}

			timeStr := relativeTime(chat.CreatedAt.AsTime())

			// Show "(remote)" for chats that originated on another daemon.
			suffix := ""
			if chat.DaemonId != "" {
				suffix = " (remote)"
			}

			line := fmt.Sprintf("%s%s%s  %s", cursor, chatTitle, suffix, styleSubtle.Render(timeStr))
			if m.cursor == i+1 {
				line = styleSelected.Render(fmt.Sprintf("%s%s%s", cursor, chatTitle, suffix)) +
					"  " + styleSubtle.Render(timeStr)
			}

			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
			b.WriteString("\n")
		}
	}

	if m.confirming && m.cursor > 0 && m.cursor <= len(m.chats) {
		chatTitle := m.chats[m.cursor-1].Title
		if chatTitle == "" {
			chatTitle = "New chat"
		}
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorRed).Render(
			fmt.Sprintf("Remove %q?", chatTitle)))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		actionBar := "[enter] select  [esc] back"
		if m.cursor > 0 && m.cursor <= len(m.chats) {
			actionBar = "[enter] select  [d] remove  [esc] back"
		}
		b.WriteString(styleActionBar.Render(actionBar))
	}

	return tea.NewView(b.String())
}

// relativeTime formats a time as a human-readable relative string.
func relativeTime(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dh ago", h)
	case d < 14*24*time.Hour:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd ago", days)
	default:
		weeks := int(d.Hours() / 24 / 7)
		return fmt.Sprintf("%dw ago", weeks)
	}
}
