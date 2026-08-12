package views

// The chat picker's model, its Update dispatcher, its View mode selector, the
// action predicates both the key routing and the renderers read, and the
// exported accessors App reads. Everything else the picker needs lives in a
// sibling chatpicker_*.go file (BOS-527):
//
//	chatpicker_messages.go  the *Msg types and confirmKind
//	chatpicker_cmds.go      the async tea.Cmd builders
//	chatpicker_handlers.go  one handler per top-level message arm
//	chatpicker_keys.go      key routing, the confirm overlay, startDelete
//	chatpicker_switch.go    the switch-account concern (BOS-171)
//	chatpicker_agent.go     the agent-select overlay
//	chatpicker_table.go     chat-table construction and layout arithmetic
//	chatpicker_view.go      the per-mode renderers

import (
	"context"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

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
	// daemonWaitingReasons maps agent_session_id → why that chat is parked on an
	// external event (BOS-668). Rendered as the session-detail waiting line so an
	// operator can see WHY a chat is idle-looking without opening it.
	daemonWaitingReasons map[string]string

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

	// optimisticArchiveLatch is a short-lived local latch set the moment a
	// TUI-initiated merge succeeds (mergeResultMsg) on an archive-after-merge
	// repo, so the detail view shows "Archiving…" with no flicker before the
	// daemon's authoritative archive_pending signal round-trips. The steady-state
	// signal is m.session.GetArchivePending() (daemon-driven); the two are OR'd
	// where archiving is rendered. Kept distinct from the daemon signal so a
	// steady-state poll can't overwrite the optimism. Cleared on
	// archived/navigation, when the repo disables archive-after-merge, and — the
	// handoff that bounds it — the moment a poll first reports archive_pending
	// true (the daemon signal then drives the spinner), so a daemon-side archive
	// that sets then clears archive_pending without archiving cannot leave it
	// stuck. Replaces the old inferred autoArchiving heuristic that showed a
	// permanent false spinner for resurrected merged sessions (BOS-425).
	optimisticArchiveLatch bool

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

	// preferredAgent is the configured default agent (Settings.DefaultAgent),
	// seeded by App at construction and re-seeded from each confirmed pick. It
	// takes precedence over the session's own agent as the picker's default
	// cursor: sessions.agent_name is written once at create time and never
	// updated, so seeding from it alone pinned the cursor to whatever runner
	// the session was created with forever.
	preferredAgent string

	// onAgentSelected persists confirmed picks so the next [n] — here and in
	// the new-session wizard, which shares saveDefaultAgent — opens on the
	// runner last chosen. Nil in tests that don't exercise persistence.
	onAgentSelected func(string) error

	// Switch-account flow (BOS-171). switchTargetChatID is the agent_session_id
	// of the chat captured when the operator triggered the switch; the flow reads
	// its live status back out of m.daemonStatuses to decide whether a mid-turn
	// confirm is needed. switchAccounts / switchAccountTable drive the account
	// picker (mirroring the new-session wizard's account select).
	// switchSelectedAccount is the account_id chosen in the picker ("" = system
	// default) held across the optional confirm step. switching is true while the
	// RPC is in flight; switchNotice is the passive system line rendered with the
	// daemon's NoticeText on success or its error message on failure.
	switchTargetChatID    string
	switchAccounts        []*pb.Account
	switchAccountTable    table.Model
	pickingAccount        bool
	switchSelectedAccount string
	switching             bool
	switchNotice          string

	// Inline rename (BOS-836). renaming is true while the prompt owns the
	// keyboard; renameInput holds the edited title; renameTargetChatID is the
	// agent_session_id captured when the prompt opened, so a poll that reorders
	// or re-sorts the list mid-edit cannot redirect the commit at whatever row
	// the cursor now sits on. renameErr is the inline complaint rendered beside
	// the input (an empty title), kept out of statusMsg because the prompt is
	// still open and statusMsg is cleared by the next keypress.
	renaming           bool
	renameInput        textinput.Model
	renameTargetChatID string
	renameErr          string
}

// SetTelemetry installs a telemetry client for successful chat-picker actions.
func (m *ChatPickerModel) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// SetPreferredAgent sets the default cursor in the [n]ew-chat agent picker.
// An empty name, or one naming a runner the daemon has not loaded, leaves the
// picker on its session-agent fallback.
func (m *ChatPickerModel) SetPreferredAgent(name string) {
	m.preferredAgent = name
}

// SetAgentSelectionHandler registers a callback for confirmed picker choices.
func (m *ChatPickerModel) SetAgentSelectionHandler(fn func(string) error) {
	m.onAgentSelected = fn
}

// NewChatPickerModel creates a ChatPickerModel for the given session.
// If highlightAgentSessionID is non-empty, that chat will be auto-highlighted after loading.
func NewChatPickerModel(c client.BossClient, parentCtx context.Context, sessionID, highlightAgentSessionID string) ChatPickerModel {
	return ChatPickerModel{
		client:      c,
		ctx:         parentCtx,
		sessionID:   sessionID,
		highlightID: highlightAgentSessionID,
		spinner:     newStatusSpinner(),
		loading:     true,
		table:       newBossTable(nil, nil, 0),
		// [t]erminal opens the session's worktree path in a LOCAL terminal tab.
		// Under --host that path is on the remote machine, so the action would
		// open a directory that does not exist here (or, worse, a same-named one
		// that does). Fold the remote context into the same flag that already
		// gates both the key and the action-bar entry, so the affordance
		// disappears rather than silently doing the wrong thing. Opening an ssh
		// session into the remote worktree instead is the better UX and is left
		// as a follow-up.
		newTabSupported: hasNewTabSupport() && !isRemoteHost(),
		renameInput:     newChatRenameInput(),
	}
}

// newChatRenameInput builds the inline [r]ename prompt's text input. The prompt
// string is what the renderer shows, so the prompt line reads "Rename: <title>"
// on one line — matching the delete confirm's line count, which is what keeps
// tableHeight()'s reservation correct without a new term.
func newChatRenameInput() textinput.Model {
	ti := textinput.New()
	ti.Placeholder = "chat title"
	ti.Prompt = "Rename: "
	ti.SetWidth(60)
	return ti
}

func (m ChatPickerModel) Init() tea.Cmd {
	return tea.Batch(m.fetchSession(), fetchAgents(m.client, m.ctx), m.spinner.Tick, tickCmd())
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
// "passing" or "approved" and the merge has not already completed (merged
// sessions no longer offer merge; they show the merged status in place instead).
//
// This intentionally mirrors what the backend MergeSession RPC accepts: it
// performs an immediate merge and rejects any PR whose tracked display status
// is not passing with "merge blocked: gate=..." (services/bossd .../server.go). A
// PR still CHECKING — even with no failures yet — would be rejected, so
// offering [m]erge in that state only leads the user into a confirm dialog
// that errors. There is no auto-merge/merge-when-ready queue today, so the
// affordance stays gated on the green states the backend will actually accept.
func (m ChatPickerModel) canMerge() bool {
	// A merge already in flight hides the [m]erge affordance: m.merging is set
	// when the user confirms a merge and cleared when the mergeResultMsg lands,
	// so the "Merging PR #N..." feedback line stands in until the RPC resolves.
	if m.merging {
		return false
	}
	if m.merged || m.session == nil || m.session.GetPrNumber() == 0 {
		return false
	}
	status := m.session.GetDisplayStatus()
	return status == pb.DisplayStatus_DISPLAY_STATUS_PASSING ||
		status == pb.DisplayStatus_DISPLAY_STATUS_APPROVED
}

// canRename reports whether the [r]ename action should be available — a chat is
// selected and no other flow already owns the keyboard or is mutating that row.
// Both the action bar and the `r` key read this single predicate, so the key can
// never do something the bar does not advertise (and vice versa).
func (m ChatPickerModel) canRename() bool {
	if m.loading || m.confirm != confirmNone {
		return false
	}
	if m.merging || m.archiving || m.switching || m.deletingAgentSessionID != "" {
		return false
	}
	return m.selectedChat() != nil
}

// textEntryActive reports whether the inline rename prompt is focused, so App
// leaves ctrl+x alone rather than aliasing it onto Esc (BOS-660): inside a text
// field Esc cancels the edit, and ctrl+x is a line-editing key.
func (m ChatPickerModel) textEntryActive() bool { return m.renaming }

// selectedChat returns the chat at the current table cursor, or nil if empty.
func (m ChatPickerModel) selectedChat() *pb.ClaudeChat {
	idx := m.table.Cursor()
	if idx < 0 || idx >= len(m.chats) {
		return nil
	}
	return m.chats[idx]
}

// Update dispatches each message to its named handler. The arm order is the
// original inline order and every handler keeps the value receiver, so the
// in-flight markers merge/archive/wake/switch-account set and clear still
// travel back out on the returned model.
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
		return m.handleSession(msg)

	case repoWebLinkMsg:
		return m.handleRepoWebLink(msg)

	case agentsMsg:
		return m.handleAgents(msg)

	case chatsListedMsg:
		return m.handleChatsListed(msg)

	case chatTitlesBackfilledMsg:
		return m.handleTitlesBackfilled(msg)

	case chatDeletedMsg:
		return m.handleChatDeleted(msg)

	case chatRenamedMsg:
		return m.handleChatRenamed(msg)

	case newTabResultMsg:
		return m.handleNewTabResult(msg)

	case webOpenResultMsg:
		return m.handleWebOpenResult(msg)

	case mergeResultMsg:
		return m.handleMergeResult(msg)

	case archiveResultMsg:
		return m.handleArchiveResult(msg)

	case wakeResultMsg:
		return m.handleWakeResult(msg)

	case switchAccountsLoadedMsg:
		return m.handleSwitchAccountsLoaded(msg)

	case switchAccountResultMsg:
		return m.handleSwitchAccountResult(msg)

	case tickMsg:
		return m, tea.Batch(m.refreshStatuses(), tickCmd())

	case chatPickerRefreshMsg:
		return m.handleRefresh(msg)

	case chatPickerErrMsg:
		return m.handleErr(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Refit whichever table is currently visible before sizing the main
		// table viewport below. Without this, a picker opened on a wide terminal
		// keeps its old column set until a later poll or spinner tick rebuilds it.
		switch {
		case m.pickingAgent:
			cursor := m.agentTable.Cursor()
			m.buildAgentTable()
			if rows := m.agentTable.Rows(); len(rows) > 0 {
				m.agentTable.SetCursor(min(max(cursor, 0), len(rows)-1))
				updateCursorColumn(&m.agentTable)
			}
		case m.pickingAccount:
			cursor := m.switchAccountTable.Cursor()
			m.buildSwitchAccountTable()
			if rows := m.switchAccountTable.Rows(); len(rows) > 0 {
				m.switchAccountTable.SetCursor(min(max(cursor, 0), len(rows)-1))
				updateCursorColumn(&m.switchAccountTable)
			}
		case len(m.chats) > 0:
			m.buildTableRows()
		}
		// Order-independent since BOS-532: SetHeight's reservation now derives
		// from blockWrapWidth() (m.width, assigned above, and the table's
		// columns) rather than from the table's own width, which SetWidth is
		// the only remaining writer of.
		//
		// Sized to the room left by the block padding, not the raw terminal
		// width: the table's viewport pads every visible line out to its width,
		// and View renders it inside chatPickerContentBlock, so SetWidth(msg.Width) drew
		// msg.Width+2 columns and soft-wrapped every row until the next chat
		// poll reset the width to columnsWidth. That double-counted against the
		// rows tableHeight had reserved.
		m.table.SetWidth(msg.Width - chatPickerBlockPadding*2)
		m.table.SetHeight(m.tableHeight())
		return m, nil

	// Bracketed paste is not a KeyMsg, so it must be forwarded explicitly or a
	// pasted title never reaches the input. Placed ahead of the KeyMsg arm for
	// the same reason the trash view does it: so paste can never be mistaken for
	// a list shortcut. Inert unless the rename prompt is open.
	case tea.PasteMsg:
		if !m.renaming {
			return m, nil
		}
		var cmd tea.Cmd
		m.renameInput, cmd = m.renameInput.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

// View selects the renderer for the picker's current mode. The precedence —
// error, loading, agent overlay, account overlay, then the chat list — is the
// original inline order and is behaviour.
func (m ChatPickerModel) View() tea.View {
	switch {
	case m.err != nil:
		return tea.NewView(m.renderErrState())
	case m.loading:
		return tea.NewView(m.renderLoading())
	case m.pickingAgent:
		return tea.NewView(m.renderAgentSelect())
	case m.pickingAccount:
		return tea.NewView(m.renderSwitchAccounts())
	}
	return tea.NewView(m.renderChatList())
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
