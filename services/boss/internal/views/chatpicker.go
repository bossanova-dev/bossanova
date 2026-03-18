package views

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/claude"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
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
	client      client.BossClient
	ctx         context.Context
	manager     *bosspty.Manager
	sessionID   string
	highlightID string // Claude ID to auto-highlight after detach

	session *pb.Session
	chats   []*pb.ClaudeChat
	table   table.Model
	spinner spinner.Model
	loading bool
	err     error
	cancel  bool
	width   int
	height  int

	// Remove confirmation
	confirming bool
}

// NewChatPickerModel creates a ChatPickerModel for the given session.
// If highlightClaudeID is non-empty, that chat will be auto-highlighted after loading.
func NewChatPickerModel(c client.BossClient, parentCtx context.Context, manager *bosspty.Manager, sessionID, highlightClaudeID string) ChatPickerModel {
	return ChatPickerModel{
		client:      c,
		ctx:         parentCtx,
		manager:     manager,
		sessionID:   sessionID,
		highlightID: highlightClaudeID,
		spinner:     newStatusSpinner(),
		loading:     true,
		table:       newBossTable(nil, nil, 0),
	}
}

func (m ChatPickerModel) Init() tea.Cmd {
	return tea.Batch(m.fetchSession(), m.spinner.Tick)
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

// buildTableRows rebuilds the table rows from m.chats.
func (m *ChatPickerModel) buildTableRows() {
	if len(m.chats) == 0 {
		m.table.SetRows(nil)
		return
	}

	titles := make([]string, len(m.chats))
	actives := make([]string, len(m.chats))
	for i, chat := range m.chats {
		t := chat.Title
		if t == "" {
			t = "New chat"
		}
		titles[i] = t
		actives[i] = relativeTime(m.chatLastActive(chat))
	}

	titleWidth := maxColWidth("CHAT", titles, 60)
	activeWidth := maxColWidth("ACTIVE", actives, 12)
	statusWidth := 12 // enough for spinner + "working"

	cols := []table.Column{
		cursorColumn,
		{Title: "CHAT", Width: titleWidth},
		{Title: "ACTIVE", Width: activeWidth},
		{Title: "STATUS", Width: statusWidth},
	}

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.chats))
	for i, chat := range m.chats {
		status := bosspty.StatusStopped
		if m.manager != nil {
			status = m.manager.ProcessStatus(chat.ClaudeId)
		}
		statusStr := renderStatus(status, m.spinner)
		activeStr := styleSubtle.Render(actives[i])
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, titles[i], activeStr, statusStr}
	}
	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	m.table.SetWidth(columnsWidth(cols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
}

// selectedChat returns the chat at the current table cursor, or nil if empty.
func (m ChatPickerModel) selectedChat() *pb.ClaudeChat {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.chats) {
		return nil
	}
	return m.chats[idx]
}

func (m ChatPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		// Rebuild rows to animate spinner frames.
		if len(m.chats) > 0 {
			m.buildTableRows()
		}
		return m, cmd

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
		// Sort chats by last activity (most recent first).
		sort.Slice(m.chats, func(i, j int) bool {
			return m.chatLastActive(m.chats[i]).After(m.chatLastActive(m.chats[j]))
		})
		m.buildTableRows()
		// Auto-highlight the chat the user just left, or the first running chat.
		if m.highlightID != "" {
			for i, chat := range m.chats {
				if chat.ClaudeId == m.highlightID {
					m.table.SetCursor(i)
					break
				}
			}
		} else if m.manager != nil {
			for i, chat := range m.chats {
				if m.manager.IsRunning(chat.ClaudeId) {
					m.table.SetCursor(i)
					break
				}
			}
		}
		return m, m.backfillTitles()

	case chatTitlesBackfilledMsg:
		for i, chat := range m.chats {
			if title, ok := msg.updates[chat.ClaudeId]; ok {
				m.chats[i].Title = title
			}
		}
		m.buildTableRows()
		return m, nil

	case chatDeletedMsg:
		if msg.err == nil {
			for i, chat := range m.chats {
				if chat.ClaudeId == msg.claudeID {
					m.chats = append(m.chats[:i], m.chats[i+1:]...)
					break
				}
			}
			m.buildTableRows()
			if m.table.Cursor() >= len(m.chats) && len(m.chats) > 0 {
				m.table.SetCursor(len(m.chats) - 1)
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
		m.table.SetHeight(m.tableHeight())
		m.table.SetWidth(msg.Width)
		return m, nil

	case tea.KeyMsg:
		if m.confirming {
			return m.updateDeleteConfirm(msg)
		}

		switch msg.String() {
		case "esc", "q":
			m.cancel = true
			return m, nil
		case "n":
			return m, func() tea.Msg {
				return switchViewMsg{
					view:      ViewAttach,
					sessionID: m.sessionID,
				}
			}
		case "d":
			if chat := m.selectedChat(); chat != nil {
				m.confirming = true
			}
			return m, nil
		case "enter":
			if chat := m.selectedChat(); chat != nil {
				resumeID := chat.ClaudeId
				return m, func() tea.Msg {
					return switchViewMsg{
						view:      ViewAttach,
						sessionID: m.sessionID,
						resumeID:  resumeID,
					}
				}
			}
			return m, nil
		}

		// Forward navigation keys to the table.
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		updateCursorColumn(&m.table)
		return m, cmd
	}

	return m, nil
}

func (m ChatPickerModel) updateDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.confirming = false
		chat := m.selectedChat()
		if chat == nil {
			return m, nil
		}
		claudeID := chat.ClaudeId
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

// tableHeight returns the height to pass to table.SetHeight.
// Capped at row count + 1 so the table doesn't expand beyond its content.
func (m ChatPickerModel) tableHeight() int {
	needed := len(m.chats) + 1 // header + chat rows
	if m.height <= 0 {
		return needed
	}
	avail := m.height - 4 // title(1) + blank(1) + blank(1) + action bar(1)
	if avail < 1 {
		avail = 1
	}
	if needed < avail {
		return needed
	}
	return avail
}

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

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.confirming {
		chat := m.selectedChat()
		if chat != nil {
			chatTitle := chat.Title
			if chatTitle == "" {
				chatTitle = "New chat"
			}
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
				fmt.Sprintf("Remove %q?", chatTitle)))
			b.WriteString("\n")
			b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
		}
	} else {
		actionBar := "[n]ew chat  [esc] back"
		if m.selectedChat() != nil {
			actionBar = "[n]ew chat  [enter] select  [d] remove  [esc] back"
		}
		b.WriteString(styleActionBar.Render(actionBar))
	}

	return tea.NewView(b.String())
}

// chatLastActive returns the most recent activity time for a chat.
// For running processes this is the last PTY output time; otherwise created_at.
func (m ChatPickerModel) chatLastActive(chat *pb.ClaudeChat) time.Time {
	if m.manager != nil {
		if lw := m.manager.ProcessLastWrite(chat.ClaudeId); !lw.IsZero() {
			return lw
		}
	}
	return chat.CreatedAt.AsTime()
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
