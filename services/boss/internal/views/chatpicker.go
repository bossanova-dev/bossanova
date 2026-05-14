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
	"github.com/recurser/boss/internal/agent"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossalib/vcs"
)

// chatPickerSessionMsg carries a session fetched via RPC for the chat picker.
type chatPickerSessionMsg struct {
	session *pb.Session
}

// chatPickerErrMsg signals a fetch error in the chat picker.
type chatPickerErrMsg struct {
	err error
}

// chatsListedMsg carries the result of listing chats via RPC,
// along with daemon-side heartbeat statuses for cross-instance display.
type chatsListedMsg struct {
	chats            []*pb.ClaudeChat
	daemonStatuses   map[string]string    // agent_session_id → status string
	daemonLastOutput map[string]time.Time // agent_session_id → last PTY output time
	err              error
}

// chatTitlesBackfilledMsg carries updated titles for chats that were "New chat".
type chatTitlesBackfilledMsg struct {
	updates map[string]string // agent_session_id -> title
}

// chatPickerRefreshMsg carries refreshed session + daemon statuses for polling.
type chatPickerRefreshMsg struct {
	session          *pb.Session
	daemonStatuses   map[string]string
	daemonLastOutput map[string]time.Time
}

// chatDeletedMsg signals that a chat was deleted (or failed to delete).
type chatDeletedMsg struct {
	agentSessionID string
	err            error
}

// newTabResultMsg carries the result of an async openInNewTab call.
type newTabResultMsg struct {
	err error
}

type repoWebLink struct {
	provider string
	url      string
}

type repoWebLinkMsg struct {
	link repoWebLink
}

type webOpenResultMsg struct {
	err error
}

// mergeResultMsg carries the result of an async MergeSession RPC call.
type mergeResultMsg struct {
	err error
}

// wakeResultMsg carries the result of an async WakeChat RPC call.
type wakeResultMsg struct {
	agentSessionID string
	resp           *pb.WakeChatResponse
	err            error
}

// ChatPickerModel lets the user choose between starting a new chat or
// resuming a previous Claude Code conversation for a session.
type ChatPickerModel struct {
	client           client.BossClient
	telemetry        telemetry.Client
	ctx              context.Context
	sessionID        string
	highlightID      string               // agent session ID to auto-highlight after detach
	daemonStatuses   map[string]string    // agent_session_id → status string from daemon heartbeats
	daemonLastOutput map[string]time.Time // agent_session_id → last PTY output time from daemon

	session *pb.Session
	chats   []*pb.ClaudeChat
	table   table.Model
	spinner spinner.Model
	loading bool
	err     error
	cancel  bool
	merged  bool
	width   int
	height  int

	// newTabSupported is cached at construction so we don't re-inspect
	// env vars on every render. The [t]erminal action is hidden when
	// false — there's no recoverable path on unsupported terminals.
	newTabSupported bool

	// Transient status line (e.g. "couldn't open new tab in <term>"),
	// cleared on the next keypress.
	statusMsg   string
	repoWebLink repoWebLink

	// Remove confirmation
	confirming             bool
	deletingAgentSessionID string

	// Merge confirmation / in-progress
	mergeConfirming bool
	merging         bool

	// Agents loaded once at picker construction. Drives the per-chat
	// agent-select sub-phase shown when the user presses [n] (new chat)
	// AND more than one agent runner is loaded. Errors fall through
	// silently — empty agents collapses to single-agent UX.
	agents       []client.AgentInfo
	agentTable   table.Model
	pickingAgent bool // true while showing the one-shot agent picker
}

// SetTelemetry installs a telemetry client for successful chat-picker actions.
func (m *ChatPickerModel) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// NewChatPickerModel creates a ChatPickerModel for the given session.
// If highlightAgentSessionID is non-empty, that chat will be auto-highlighted after loading.
func NewChatPickerModel(c client.BossClient, parentCtx context.Context, sessionID, highlightAgentSessionID string) ChatPickerModel {
	return ChatPickerModel{
		client:          c,
		ctx:             parentCtx,
		sessionID:       sessionID,
		highlightID:     highlightAgentSessionID,
		spinner:         newStatusSpinner(),
		loading:         true,
		table:           newBossTable(nil, nil, 0),
		newTabSupported: hasNewTabSupport(),
	}
}

func (m ChatPickerModel) Init() tea.Cmd {
	return tea.Batch(m.fetchSession(), fetchAgents(m.client, m.ctx), m.spinner.Tick, tickCmd())
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

// parseChatStatuses fetches daemon-side heartbeat statuses and converts them
// into maps keyed by Claude ID.
func parseChatStatuses(c client.BossClient, ctx context.Context, sessionID string) (map[string]string, map[string]time.Time) {
	entries, err := c.GetChatStatuses(ctx, sessionID)
	if err != nil {
		return nil, nil
	}
	statuses := make(map[string]string, len(entries))
	lastOutput := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		statuses[e.AgentSessionId] = chatStatusString(e.Status)
		if e.LastOutputAt != nil {
			lastOutput[e.AgentSessionId] = e.LastOutputAt.AsTime()
		}
	}
	return statuses, lastOutput
}

func (m ChatPickerModel) listChats() tea.Cmd {
	return func() tea.Msg {
		chats, err := m.client.ListChats(m.ctx, m.sessionID)
		if err != nil {
			return chatsListedMsg{err: err}
		}
		statuses, lastOutput := parseChatStatuses(m.client, m.ctx, m.sessionID)
		return chatsListedMsg{chats: chats, daemonStatuses: statuses, daemonLastOutput: lastOutput}
	}
}

func (m ChatPickerModel) fetchRepoWebLink() tea.Cmd {
	if m.session == nil || m.session.GetRepoId() == "" {
		return nil
	}
	repoID := m.session.GetRepoId()
	prNumber := int(m.session.GetPrNumber())
	return func() tea.Msg {
		repos, err := m.client.ListRepos(m.ctx)
		if err != nil {
			return repoWebLinkMsg{}
		}
		for _, repo := range repos {
			if repo.GetId() != repoID {
				continue
			}
			if provider, webURL, ok := vcs.PullRequestWebLink(repo.GetOriginUrl(), prNumber); ok {
				return repoWebLinkMsg{link: repoWebLink{provider: provider, url: webURL}}
			}
			provider, webURL, ok := vcs.RepoWebLink(repo.GetOriginUrl())
			if !ok {
				return repoWebLinkMsg{}
			}
			return repoWebLinkMsg{link: repoWebLink{provider: provider, url: webURL}}
		}
		return repoWebLinkMsg{}
	}
}

// refreshStatuses fetches the latest session (for PR status) and daemon
// chat statuses without re-listing all chats.
func (m ChatPickerModel) refreshStatuses() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID)
		if err != nil {
			return chatPickerRefreshMsg{}
		}
		statuses, lastOutput := parseChatStatuses(m.client, m.ctx, m.sessionID)
		return chatPickerRefreshMsg{
			session:          sess,
			daemonStatuses:   statuses,
			daemonLastOutput: lastOutput,
		}
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
			title := agent.ChatTitle(worktreePath, c.AgentSessionId)
			if title != "" {
				updates[c.AgentSessionId] = title
				_ = m.client.UpdateChatTitle(m.ctx, c.AgentSessionId, title)
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
	createds := make([]string, len(m.chats))
	actives := make([]string, len(m.chats))
	agents := make([]string, len(m.chats))
	showAgentColumn := len(m.agents) > 1
	for i, chat := range m.chats {
		t := chat.Title
		if t == "" {
			t = "New chat"
		}
		titles[i] = t
		createds[i] = RelativeTime(chat.CreatedAt.AsTime())
		actives[i] = RelativeTime(m.chatLastActive(chat))
		agents[i] = m.chatAgentName(chat)
	}

	titleWidth := maxColWidth("CHAT", titles, 60)
	agentWidth := maxColWidth("AGENT", agents, 12)
	createdWidth := maxColWidth("CREATED", createds, 12)
	activeWidth := maxColWidth("ACTIVE", actives, 12)
	statusWidth := 12 // enough for spinner + "working"

	cols := []table.Column{
		cursorColumn,
		{Title: "CHAT", Width: titleWidth + tableColumnSep},
	}
	if showAgentColumn {
		cols = append(cols, table.Column{Title: "AGENT", Width: agentWidth + tableColumnSep})
	}
	cols = append(cols,
		table.Column{Title: "CREATED", Width: createdWidth + tableColumnSep},
		table.Column{Title: "ACTIVE", Width: activeWidth + tableColumnSep},
		table.Column{Title: "STATUS", Width: statusWidth + tableColumnSep},
	)

	cursor := m.table.Cursor()
	rows := make([]table.Row, len(m.chats))
	for i, chat := range m.chats {
		daemon := m.daemonStatuses[chat.AgentSessionId]
		statusStr := renderClaudeStatus(daemon, m.spinner)
		if chat.AgentSessionId != "" && chat.AgentSessionId == m.deletingAgentSessionID {
			statusStr = renderRowPendingStatus(m.spinner, "deleting")
		}
		createdStr := styleSubtle.Render(createds[i])
		activeStr := styleSubtle.Render(actives[i])
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		row := table.Row{indicator, titles[i]}
		if showAgentColumn {
			row = append(row, agents[i])
		}
		row = append(row, createdStr, activeStr, statusStr)
		rows[i] = row
	}
	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	m.table.SetWidth(columnsWidth(cols))
	m.table.SetHeight(m.tableHeight())
	m.table.SetCursor(cursor)
}

func (m *ChatPickerModel) chatAgentName(chat *pb.ClaudeChat) string {
	if chat.GetAgentName() != "" {
		return chat.GetAgentName()
	}
	if m.session != nil && m.session.GetAgentName() != "" {
		return m.session.GetAgentName()
	}
	return "-"
}

// canMerge reports whether the [m]erge action should be available for the
// current session — only when the session has an open PR and its display
// status is "passing".
func (m ChatPickerModel) canMerge() bool {
	return m.session != nil &&
		m.session.GetPrNumber() != 0 &&
		m.session.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_PASSING
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
		return m, tea.Batch(m.listChats(), m.fetchRepoWebLink())

	case repoWebLinkMsg:
		m.repoWebLink = msg.link
		return m, nil

	case agentsMsg:
		// Errors are non-fatal: an empty agent list collapses the picker
		// to its single-agent UX (skip the agent-select phase entirely).
		if msg.err == nil {
			m.agents = msg.agents
		}
		return m, nil

	case chatsListedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.chats = msg.chats
		m.daemonStatuses = msg.daemonStatuses
		m.daemonLastOutput = msg.daemonLastOutput
		// Sort chats by creation time (newest first).
		sort.Slice(m.chats, func(i, j int) bool {
			return m.chats[i].CreatedAt.AsTime().After(m.chats[j].CreatedAt.AsTime())
		})
		m.buildTableRows()
		// Auto-highlight the chat the user just left, or the first running chat.
		if m.highlightID != "" {
			for i, chat := range m.chats {
				if chat.AgentSessionId == m.highlightID {
					m.table.SetCursor(i)
					updateCursorColumn(&m.table)
					break
				}
			}
		} else if m.daemonStatuses != nil {
			for i, chat := range m.chats {
				if s := m.daemonStatuses[chat.AgentSessionId]; s == statusWorking || s == statusIdle || s == statusQuestion {
					m.table.SetCursor(i)
					updateCursorColumn(&m.table)
					break
				}
			}
		}
		return m, m.backfillTitles()

	case chatTitlesBackfilledMsg:
		for i, chat := range m.chats {
			if title, ok := msg.updates[chat.AgentSessionId]; ok {
				m.chats[i].Title = title
			}
		}
		m.buildTableRows()
		return m, nil

	case chatDeletedMsg:
		if msg.agentSessionID == m.deletingAgentSessionID {
			m.deletingAgentSessionID = ""
		}
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Delete failed: %v", msg.err)
			m.buildTableRows()
			return m, nil
		}
		for i, chat := range m.chats {
			if chat.AgentSessionId == msg.agentSessionID {
				m.chats = append(m.chats[:i], m.chats[i+1:]...)
				break
			}
		}
		m.buildTableRows()
		if m.table.Cursor() >= len(m.chats) && len(m.chats) > 0 {
			m.table.SetCursor(len(m.chats) - 1)
		}
		return m, nil

	case newTabResultMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Couldn't open new tab: %v", msg.err)
			return m, nil
		}
		captureViewTelemetry(m.ctx, m.telemetry, telemetry.EventChatAttached, map[string]any{
			"source": "tui",
			"action": "open",
		})
		return m, nil

	case webOpenResultMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Couldn't open GitHub: %v", msg.err)
		}
		return m, nil

	case mergeResultMsg:
		m.merging = false
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Couldn't merge: %v", msg.err)
			return m, nil
		}
		// Merge succeeded — signal App to return to the session list. The
		// server-side PR state transition lands asynchronously via webhook;
		// HomeModel renders the session as MERGED optimistically until the
		// daemon reconciles.
		m.merged = true
		return m, nil

	case wakeResultMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Wake failed: %v", msg.err)
			return m, nil
		}
		switch msg.resp.GetOutcome() {
		case pb.WakeChatResponse_OUTCOME_ALREADY_LIVE:
			m.statusMsg = "Already live"
		case pb.WakeChatResponse_OUTCOME_RESUMED:
			m.statusMsg = "Resumed"
		case pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK:
			m.statusMsg = wakeFreshFallbackStatus(msg.resp.GetReason())
		default:
			m.statusMsg = "Woken"
		}
		// Refresh statuses so the chat's STATUS column flips off "stopped"
		// without waiting for the next tick.
		return m, m.refreshStatuses()

	case tickMsg:
		return m, tea.Batch(m.refreshStatuses(), tickCmd())

	case chatPickerRefreshMsg:
		if msg.session != nil {
			m.session = msg.session
		}
		if msg.daemonStatuses != nil {
			m.daemonStatuses = msg.daemonStatuses
		}
		if msg.daemonLastOutput != nil {
			m.daemonLastOutput = msg.daemonLastOutput
		}
		if len(m.chats) > 0 {
			m.buildTableRows()
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
		// While a merge is in flight the View hides all action bars and
		// confirmation prompts behind the spinner. Swallow key input so the
		// user can't invisibly enter `d`/`m` confirm state and then confirm
		// it with `y` against a prompt they can't see.
		if m.merging {
			return m, nil
		}
		if m.pickingAgent {
			return m.updateAgentSelect(msg)
		}
		if m.confirming {
			return m.updateDeleteConfirm(msg)
		}
		if m.mergeConfirming {
			return m.updateMergeConfirm(msg)
		}

		m.statusMsg = ""

		switch msg.String() {
		case "esc":
			m.cancel = true
			return m, nil
		case "n":
			if len(m.agents) > 1 {
				m.pickingAgent = true
				m.buildAgentTable()
				return m, nil
			}
			return m, func() tea.Msg {
				return switchViewMsg{
					view:      ViewAttach,
					sessionID: m.sessionID,
				}
			}
		case "s":
			return m, func() tea.Msg {
				return switchViewMsg{
					view:      ViewSessionSettings,
					sessionID: m.sessionID,
				}
			}
		case "t":
			if !m.newTabSupported || m.session == nil {
				return m, nil
			}
			path := m.session.GetWorktreePath()
			if path == "" {
				return m, nil
			}
			return m, func() tea.Msg {
				return newTabResultMsg{err: openInNewTab(path)}
			}
		case "g":
			if m.repoWebLink.provider == "github" && m.repoWebLink.url != "" {
				repoURL := m.repoWebLink.url
				return m, func() tea.Msg {
					return webOpenResultMsg{err: openURLFunc(repoURL)}
				}
			}
		case "m":
			if !m.canMerge() {
				return m, nil
			}
			m.mergeConfirming = true
			return m, nil
		case "w":
			chat := m.selectedChat()
			if chat == nil {
				return m, nil
			}
			// Only fire WakeChat for chats whose daemon-reported status is
			// "stopped". For any other status (working, idle, question, or
			// unknown) the wake call would be a no-op (OUTCOME_ALREADY_LIVE)
			// but firing it is misleading UX — a transient "Waking..." flash
			// for a chat that's already healthy.
			if m.daemonStatuses[chat.AgentSessionId] != statusStopped {
				return m, nil
			}
			m.statusMsg = "Waking..."
			sessionID := m.sessionID
			agentSessionID := chat.AgentSessionId
			c := m.client
			ctx := m.ctx
			return m, func() tea.Msg {
				resp, err := c.WakeChat(ctx, sessionID, agentSessionID, false)
				return wakeResultMsg{
					agentSessionID: agentSessionID,
					resp:           resp,
					err:            err,
				}
			}
		case "d":
			if chat := m.selectedChat(); chat != nil && m.deletingAgentSessionID == "" {
				m.confirming = true
			}
			return m, nil
		case "enter":
			if chat := m.selectedChat(); chat != nil {
				resumeID := chat.AgentSessionId
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

func (m ChatPickerModel) updateMergeConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.mergeConfirming = false
		m.merging = true
		client := m.client
		ctx := m.ctx
		id := m.sessionID
		return m, func() tea.Msg {
			_, err := client.MergeSession(ctx, id)
			return mergeResultMsg{err: err}
		}
	case "n", "esc":
		m.mergeConfirming = false
	}
	return m, nil
}

// buildAgentTable populates m.agentTable from m.agents. Single AGENT
// column, mirrors the new-session wizard's agent select shape.
func (m *ChatPickerModel) buildAgentTable() {
	names := make([]string, len(m.agents))
	for i, a := range m.agents {
		names[i] = a.Name
	}
	preferred := ""
	if m.session != nil {
		preferred = m.session.AgentName
	}
	cursor := agentIndex(m.agents, preferred)
	if cursor < 0 {
		cursor = 0
	}
	cols := []table.Column{
		cursorColumn,
		{Title: "AGENT", Width: maxColWidth("AGENT", names, 20) + tableColumnSep},
	}
	rows := make([]table.Row, len(m.agents))
	for i := range m.agents {
		indicator := ""
		if i == cursor {
			indicator = cursorChevron
		}
		rows[i] = table.Row{indicator, names[i]}
	}
	m.agentTable = newBossTable(cols, rows, len(m.agents)+1)
	m.agentTable.SetCursor(cursor)
	m.agentTable.SetWidth(columnsWidth(cols))
}

// updateAgentSelect handles key input while the agent-select sub-phase is
// showing. Esc cancels back to the chat picker; Enter confirms the
// selection and transitions to ViewAttach with the chosen agent override.
func (m ChatPickerModel) updateAgentSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.pickingAgent = false
		return m, nil
	case "enter", " ", "space":
		idx := m.agentTable.Cursor()
		if idx < 0 || idx >= len(m.agents) {
			return m, nil
		}
		agentName := m.agents[idx].Name
		m.pickingAgent = false
		sessionID := m.sessionID
		return m, func() tea.Msg {
			return switchViewMsg{
				view:      ViewAttach,
				sessionID: sessionID,
				agentName: agentName,
			}
		}
	default:
		var cmd tea.Cmd
		m.agentTable, cmd = m.agentTable.Update(msg)
		updateCursorColumn(&m.agentTable)
		return m, cmd
	}
}

func (m ChatPickerModel) updateDeleteConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.confirming = false
		chat := m.selectedChat()
		if chat == nil {
			return m, nil
		}
		agentSessionID := chat.AgentSessionId
		m.deletingAgentSessionID = agentSessionID
		m.buildTableRows()
		return m, func() tea.Msg {
			err := m.client.DeleteChat(m.ctx, agentSessionID)
			return chatDeletedMsg{agentSessionID: agentSessionID, err: err}
		}
	case "n", "esc":
		m.confirming = false
	}
	return m, nil
}

// Cancelled returns true if the user cancelled the chat picker.
func (m ChatPickerModel) Cancelled() bool { return m.cancel }

// Merged returns true if the user just completed a successful merge from
// this session. App uses this signal to return to the home view.
func (m ChatPickerModel) Merged() bool { return m.merged }

// Session returns the active session, or nil if it has not been fetched yet.
// Used by the top-level App to attach session context to a bug report.
func (m ChatPickerModel) Session() *pb.Session { return m.session }

// DaemonStatuses returns the per-chat daemon heartbeat statuses, keyed by
// Claude ID. Used by the top-level App to attach diagnostic context to a
// bug report.
func (m ChatPickerModel) DaemonStatuses() map[string]string { return m.daemonStatuses }

// tableHeight returns the height to pass to table.SetHeight.
func (m ChatPickerModel) tableHeight() int {
	return clampedTableHeight(len(m.chats), m.height, bannerOverhead+1+actionBarPadY+1) // gap + actionbar padding + actionbar
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
			lipgloss.NewStyle().Padding(0, 2).Render(
				fmt.Sprintf("Loading chats for %s...", title)),
		)
	}

	if m.pickingAgent {
		var b strings.Builder
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render(
			"Pick an agent for this new chat."))
		b.WriteString("\n\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.agentTable.View()))
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[enter] select"}, []string{"[esc] cancel"}))
		return tea.NewView(b.String())
	}

	var b strings.Builder

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.merging {
		label := "Merging PR..."
		if n := m.session.GetPrNumber(); n != 0 {
			label = fmt.Sprintf("Merging PR #%d...", n)
		}
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + label))
	} else if m.confirming {
		chat := m.selectedChat()
		if chat != nil {
			chatTitle := chat.Title
			if chatTitle == "" {
				chatTitle = "New chat"
			}
			b.WriteString("\n")
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(
				fmt.Sprintf("Delete %q?", chatTitle)))
			b.WriteString("\n")
			b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
		}
	} else if m.mergeConfirming {
		prompt := "Merge PR?"
		if n := m.session.GetPrNumber(); n != 0 {
			prompt = fmt.Sprintf("Merge PR #%d?", n)
		}
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(prompt))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		if m.statusMsg != "" {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.statusMsg))
			b.WriteString("\n")
		}
		middle := []string{"[n]ew chat", "[s]ettings"}
		if m.newTabSupported {
			middle = append(middle, "[t]erminal")
		}
		if m.repoWebLink.provider == "github" && m.repoWebLink.url != "" {
			middle = append(middle, "[g]ithub")
		}
		if m.canMerge() {
			middle = append(middle, "[m]erge")
		}
		if chat := m.selectedChat(); chat != nil {
			left := []string{"[enter] select", "[d]elete"}
			// Only advertise [w]ake when the highlighted chat is actually
			// stopped — for any other status the keypress is a no-op, so
			// dangling the action in the bar would mislead users.
			if m.daemonStatuses[chat.AgentSessionId] == statusStopped {
				left = append(left, "[w]ake")
			}
			b.WriteString(actionBar(
				left,
				middle,
				[]string{"[esc] back"},
			))
		} else {
			b.WriteString(actionBar(
				middle,
				[]string{"[esc] back"},
			))
		}
	}

	return tea.NewView(b.String())
}

func wakeFreshFallbackStatus(reason string) string {
	switch reason {
	case "transcript_missing":
		return "Started fresh: transcript missing"
	case "provider_id_missing":
		return "Started fresh: provider session was not discovered yet"
	case "provider_id_discovery_timeout":
		return "Started fresh: provider session is still being discovered"
	case "legacy_provider_id_discovery_ambiguous":
		return "Started fresh: legacy backfill matched multiple provider sessions"
	case "provider_id_discovery_ambiguous":
		return "Started fresh: provider session discovery matched multiple candidates"
	default:
		return "Started fresh"
	}
}

// chatLastActive returns the most recent activity time for a chat.
// Prefers daemon-reported output time, then created_at.
func (m ChatPickerModel) chatLastActive(chat *pb.ClaudeChat) time.Time {
	if t, ok := m.daemonLastOutput[chat.AgentSessionId]; ok && !t.IsZero() {
		return t
	}
	return chat.CreatedAt.AsTime()
}

// RelativeTime formats a time as a human-readable relative string.
func RelativeTime(t time.Time) string {
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
