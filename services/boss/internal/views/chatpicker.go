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
	// repoID and prNumber tag the session state this link was computed for, so
	// the handler can discard a slow in-flight fetch (e.g. one started before a
	// PR existed) that resolves after a newer fetch already installed the right
	// link.
	repoID   string
	prNumber int
}

type webOpenResultMsg struct {
	err error
}

// mergeResultMsg carries the result of an async MergeSession RPC call.
// sessionID tags the session this merge was issued for so a completion that
// resolves after the user navigated to a different session is discarded rather
// than acted on.
type mergeResultMsg struct {
	sessionID string
	err       error
}

// archiveResultMsg carries the result of an async ArchiveSession RPC call.
// sessionID tags the session this archive was issued for so a completion that
// resolves after the user navigated to a different session is discarded rather
// than acted on.
type archiveResultMsg struct {
	sessionID string
	err       error
}

// confirmKind identifies which destructive action a y/n confirmation prompt
// is gating. Stored as a plain enum on the model (NOT a pointer/closure) so it
// is safe under Bubble Tea's value-copy of models between updates.
type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmDelete
	confirmMerge
	confirmArchive
)

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

	// Active y/n confirmation (confirmNone when no prompt is showing).
	confirm                confirmKind
	deletingAgentSessionID string

	// In-flight spinners for the async RPCs.
	merging   bool
	archiving bool
	archived  bool

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
		tag := repoWebLinkMsg{repoID: repoID, prNumber: prNumber}
		repos, err := m.client.ListRepos(m.ctx)
		if err != nil {
			return tag
		}
		for _, repo := range repos {
			if repo.GetId() != repoID {
				continue
			}
			if provider, webURL, ok := vcs.PullRequestWebLink(repo.GetOriginUrl(), prNumber); ok {
				tag.link = repoWebLink{provider: provider, url: webURL}
				return tag
			}
			provider, webURL, ok := vcs.RepoWebLink(repo.GetOriginUrl())
			if !ok {
				return tag
			}
			tag.link = repoWebLink{provider: provider, url: webURL}
			return tag
		}
		return tag
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
		// A chat with start_error set never came up — the agent_chats
		// row was created but StartTmuxChat hit a failure (e.g.
		// SendPlan timeout from claude's broken --print regression
		// pre-#350). The row is preserved so the operator can see the
		// attempt; surface that explicitly here instead of letting the
		// row look like a fresh "stopped" chat.
		if chat.GetStartError() != "" {
			statusStr = renderChatStartFailed()
		}
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

// hasPR reports whether the current session has a known pull-request number.
func (m ChatPickerModel) hasPR() bool {
	return m.session != nil && m.session.GetPrNumber() != 0
}

// canOpenGitHub reports whether the [g]ithub button and the g shortcut should
// be available — only when the repo web-link is a GitHub link and the session
// has a known PR number (so the button opens the PR, not just the repo).
func (m ChatPickerModel) canOpenGitHub() bool {
	return m.repoWebLink.provider == "github" && m.repoWebLink.url != "" && m.hasPR()
}

// canOpenTracker reports whether the [l]inear button and the l shortcut should
// be available — only when the session has a non-empty tracker URL.
func (m ChatPickerModel) canOpenTracker() bool {
	return m.session != nil && m.session.GetTrackerUrl() != ""
}

// canMerge reports whether the [m]erge action should be available for the
// current session — when the session has an open PR whose display status is
// "passing" and the merge has not already completed (merged sessions no longer
// offer merge; they show the merged status in place instead).
//
// This intentionally mirrors what the backend MergeSession RPC accepts: it
// performs an immediate merge and rejects any PR whose tracked display status
// is not passing with "PR is not passing" (services/bossd .../server.go). A
// PR still CHECKING — even with no failures yet — would be rejected, so
// offering [m]erge in that state only leads the user into a confirm dialog
// that errors. There is no auto-merge/merge-when-ready queue today, so the
// affordance stays gated on the passing state the backend will actually accept.
func (m ChatPickerModel) canMerge() bool {
	if m.merged || m.session == nil || m.session.GetPrNumber() == 0 {
		return false
	}
	return m.session.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_PASSING
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
		// Discard a stale in-flight fetch (e.g. one started before a PR existed)
		// whose repo/PR no longer matches the current session; otherwise it could
		// overwrite a freshly installed PR link with the old plain repo URL.
		if msg.repoID != m.session.GetRepoId() || msg.prNumber != int(m.session.GetPrNumber()) {
			return m, nil
		}
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
			// Shared by the [g]ithub and [l]inear shortcuts, so the message
			// stays generic rather than naming a specific destination.
			m.statusMsg = fmt.Sprintf("Couldn't open browser: %v", msg.err)
		}
		return m, nil

	case mergeResultMsg:
		if msg.sessionID != m.sessionID {
			return m, nil // orphan completion from a session the user navigated away from
		}
		m.merging = false
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Couldn't merge: %v", msg.err)
			return m, nil
		}
		// Merge succeeded — stay on the session-detail view showing merged status
		// so the user can archive in place. The server-side PR state transition
		// lands asynchronously via webhook; HomeModel renders the session as
		// MERGED optimistically until the daemon reconciles (if the user then
		// cancels or archives back to the list).
		m.merged = true
		return m, nil

	case archiveResultMsg:
		if msg.sessionID != m.sessionID {
			return m, nil // orphan completion from a session the user navigated away from
		}
		m.archiving = false
		if msg.err != nil {
			m.statusMsg = "Couldn't archive session: " + msg.err.Error()
			return m, nil
		}
		m.archived = true
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
		var refreshWebLink tea.Cmd
		if msg.session != nil {
			// A PR number appearing (or changing) after the picker opened means
			// the cached repoWebLink still points at the plain repo (or old-PR)
			// URL. Clear it before kicking off the async re-fetch so canOpenGitHub
			// hides [g]ithub during the RPC latency rather than advertising a
			// button that opens the stale page; the refetch reinstalls the link
			// pointing at the new PR.
			prevPR := m.session.GetPrNumber()
			m.session = msg.session
			if m.session.GetPrNumber() != prevPR {
				m.repoWebLink = repoWebLink{}
				refreshWebLink = m.fetchRepoWebLink()
			}
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
		return m, refreshWebLink

	case chatPickerErrMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Width first: tableHeight reserves the session warning block, whose
		// wrapped line count depends on the current table width.
		m.table.SetWidth(msg.Width)
		m.table.SetHeight(m.tableHeight())
		return m, nil

	case tea.KeyMsg:
		if m.err != nil {
			// Chat listing failed, but the session itself loaded, so the
			// session can still be archived — and archiveCmd is now the only
			// TUI archive path. Keep the archive flow reachable here; swallow
			// everything else.
			if m.archiving {
				// Allow esc to navigate away even while archiving — the RPC
				// continues in the background.
				if msg.String() == "esc" {
					m.cancel = true
				}
				return m, nil
			}
			if m.confirm == confirmArchive {
				return m.updateConfirm(msg)
			}
			switch msg.String() {
			case "esc":
				m.cancel = true
			case "a":
				if m.sessionID != "" {
					m.confirm = confirmArchive
				}
			}
			return m, nil
		}
		// While a merge/archive is in flight, swallow key input so the user
		// can't invisibly enter another confirm state before the async result
		// routes the picker. Esc is the exception: it navigates away while
		// allowing the RPC to finish in the background.
		if m.merging || m.archiving {
			if msg.String() == "esc" {
				m.cancel = true
			}
			return m, nil
		}
		if m.pickingAgent {
			return m.updateAgentSelect(msg)
		}
		if m.confirm != confirmNone {
			return m.updateConfirm(msg)
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
			// g is the [g]ithub action key. When the action is hidden, swallow
			// it (like the m/[m]erge key) rather than letting it fall through to
			// the table's go-to-top binding and silently move the cursor — a
			// hidden shortcut must do nothing. (home still goes to the top.)
			if !m.canOpenGitHub() {
				return m, nil
			}
			repoURL := m.repoWebLink.url
			return m, func() tea.Msg {
				return webOpenResultMsg{err: openURLFunc(repoURL)}
			}
		case "l":
			// l is the [l]inear action key. When the action is hidden, swallow
			// it to avoid unintended side-effects — a hidden shortcut must do
			// nothing.
			if !m.canOpenTracker() {
				return m, nil
			}
			trackerURL := m.session.GetTrackerUrl()
			return m, func() tea.Msg {
				return webOpenResultMsg{err: openURLFunc(trackerURL)}
			}
		case "m":
			if !m.canMerge() {
				return m, nil
			}
			m.confirm = confirmMerge
			return m, nil
		case "a":
			if m.sessionID != "" && !m.loading && m.confirm == confirmNone && !m.merging && !m.archiving {
				m.confirm = confirmArchive
			}
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
				m.confirm = confirmDelete
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

// updateConfirm handles the y/n keys for whichever confirmKind is active.
// It clears the prompt, flips any in-flight spinner, and returns the RPC cmd.
func (m ChatPickerModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		k := m.confirm
		m.confirm = confirmNone
		switch k {
		case confirmMerge:
			m.merging = true
			return m, m.mergeCmd()
		case confirmArchive:
			m.archiving = true
			return m, m.archiveCmd()
		case confirmDelete:
			return m.startDelete()
		case confirmNone:
			return m, nil
		}
	case "n", "esc":
		m.confirm = confirmNone
	}
	return m, nil
}

// mergeCmd runs MergeSession off the update loop. Captures everything by value.
func (m ChatPickerModel) mergeCmd() tea.Cmd {
	client := m.client
	ctx := m.ctx
	id := m.sessionID
	return func() tea.Msg {
		_, err := client.MergeSession(ctx, id)
		return mergeResultMsg{sessionID: id, err: err}
	}
}

func (m ChatPickerModel) archiveCmd() tea.Cmd {
	client := m.client
	ctx := m.ctx
	id := m.sessionID
	return func() tea.Msg {
		_, err := client.ArchiveSession(ctx, id)
		return archiveResultMsg{sessionID: id, err: err}
	}
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

// startDelete records the in-flight chat id (drives the table badge) and
// returns the DeleteChat command. Returns (model, cmd) because it mutates the
// model, unlike mergeCmd/archiveCmd which only read it.
func (m ChatPickerModel) startDelete() (tea.Model, tea.Cmd) {
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
}

// ArchivingSessionID returns the session id being archived if an archive is in
// flight, else "". Lets app.go carry archiving state across the home rebuild.
func (m ChatPickerModel) ArchivingSessionID() string {
	if m.archiving {
		return m.sessionID
	}
	return ""
}

// Cancelled returns true if the user cancelled the chat picker.
func (m ChatPickerModel) Cancelled() bool { return m.cancel }

// Merged returns true if the user just completed a successful merge from
// this session. App uses this signal to return to the home view.
func (m ChatPickerModel) Merged() bool { return m.merged }

func (m ChatPickerModel) Archived() bool { return m.archived }

// Session returns the active session, or nil if it has not been fetched yet.
// Used by the top-level App to attach session context to a bug report.
func (m ChatPickerModel) Session() *pb.Session { return m.session }

// DaemonStatuses returns the per-chat daemon heartbeat statuses, keyed by
// Claude ID. Used by the top-level App to attach diagnostic context to a
// bug report.
func (m ChatPickerModel) DaemonStatuses() map[string]string { return m.daemonStatuses }

// tableHeight returns the height to pass to table.SetHeight.
func (m ChatPickerModel) tableHeight() int {
	// gap + actionbar padding + actionbar, plus the session warning block
	// (below the header, above the chat list). Reserving its lines shrinks the
	// chat table rather than letting the block push the table off-screen.
	overhead := bannerOverhead + 1 + actionBarPadY + 1 + m.warningBlockHeight()
	return clampedTableHeight(len(m.chats), m.height, overhead)
}

// warningBlockHeight returns the number of vertical lines the session warning
// block occupies above the chat list (0 when the session has no
// finalize/repair hints), including the single blank line below it that View
// renders. There is no blank line above: the header banner already renders
// one below the worktree-path line.
func (m ChatPickerModel) warningBlockHeight() int {
	block := selectedSessionWarningBlock(m.session, m.table.Width())
	if block == "" {
		return 0
	}
	return lipgloss.Height(block) + 1
}

func (m ChatPickerModel) View() tea.View {
	if m.err != nil {
		body := renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n"
		switch {
		case m.archiving:
			body += lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
				m.spinner.View() + "Archiving session...")
		case m.confirm == confirmArchive:
			body += lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render("Archive this session?") + "\n" +
				styleActionBar.Render("[y/enter] confirm  [n/esc] cancel")
		default:
			// Surface a failed archive (stored in statusMsg) so the user isn't
			// dropped back to the bare error screen with no feedback.
			if m.statusMsg != "" {
				body += lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.statusMsg) + "\n"
			}
			if m.sessionID != "" {
				body += actionBar([]string{"[a]rchive"}, []string{"[esc] back"})
			} else {
				body += styleActionBar.Render("[esc] back")
			}
		}
		return tea.NewView(body)
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

	// Surface the session's full finalize/repair error below the header and
	// above the chat list. The header banner already renders one blank line
	// below the worktree-path line, so we add none above here; the trailing
	// "\n\n" yields the single blank line below, before the chat list.
	if block := selectedSessionWarningBlock(m.session, m.table.Width()); block != "" {
		b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(block))
		b.WriteString("\n\n")
	}

	b.WriteString(lipgloss.NewStyle().Padding(0, 1).Render(m.table.View()))
	b.WriteString("\n")

	if m.merging {
		label := "Merging PR..."
		if n := m.session.GetPrNumber(); n != 0 {
			label = fmt.Sprintf("Merging PR #%d...", n)
		}
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + label))
	} else if m.archiving {
		b.WriteString(lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(
			m.spinner.View() + "Archiving session..."))
	} else if m.confirm == confirmDelete {
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
	} else if m.confirm == confirmMerge {
		prompt := "Merge PR?"
		if n := m.session.GetPrNumber(); n != 0 {
			prompt = fmt.Sprintf("Merge PR #%d?", n)
		}
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render(prompt))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else if m.confirm == confirmArchive {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorWarning).Render("Archive this session?"))
		b.WriteString("\n")
		b.WriteString(styleActionBar.Render("[y/enter] confirm  [n/esc] cancel"))
	} else {
		if m.statusMsg != "" {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorDanger).Render(m.statusMsg))
			b.WriteString("\n")
		}
		if m.merged {
			mergedLabel := "✓ merged"
			if n := m.session.GetPrNumber(); n != 0 {
				mergedLabel = fmt.Sprintf("✓ PR #%d merged", n)
			}
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(styleStatusMuted.Render(mergedLabel)))
			b.WriteString("\n")
		}
		middle := []string{"[n]ew chat", "[s]ettings"}
		if m.newTabSupported {
			middle = append(middle, "[t]erminal")
		}
		if m.canOpenGitHub() {
			middle = append(middle, "[g]ithub")
		}
		if m.canOpenTracker() {
			middle = append(middle, "[l]inear")
		}
		if m.canMerge() {
			middle = append(middle, "[m]erge")
		}
		if m.sessionID != "" {
			middle = append(middle, "[a]rchive")
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
