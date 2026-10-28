package chatpicker

import (
	"strings"

	"github.com/recurser/boss/api/tui/ui"

	// Key routing for the chat picker (BOS-527). handleKey is the single entry
	// point for tea.KeyMsg: it reproduces, in order, the precedence the inline
	// Update arm had — error mode, in-flight swallow, agent overlay, account
	// overlay, confirm overlay, then the list-mode keys behind handleListKey.
	// That ordering is behaviour; do not reorder these checks.

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// handleKey dispatches a key press to whichever mode currently owns input.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A confirmed action runs on without the operator: Esc leaves, and Home
	// carries a merge's progress and outcome.
	if m.attentionAction.Busy && msg.String() == "esc" {
		m.cancel = true
		return m, nil
	}
	if m.attentionAction.Active() {
		var cmd tea.Cmd
		m.attentionAction, cmd = m.attentionAction.Update(msg)
		m.table.SetHeight(m.tableHeight())
		return m, cmd
	}
	if m.err != nil {
		return m.handleErrModeKey(msg)
	}
	// While a merge/archive is in flight, swallow key input so the user
	// can't invisibly enter another confirm state before the async result
	// routes the picker. Esc is the exception: it navigates away while
	// allowing the RPC to finish in the background.
	if m.merging || m.archiving || m.switching {
		if msg.String() == "esc" {
			m.cancel = true
		}
		return m, nil
	}
	if m.pickingAgent {
		return m.updateAgentSelect(msg)
	}
	if m.pickingAccount {
		return m.updateSwitchAccountSelect(msg)
	}
	// The inline rename prompt owns the keyboard while it is open, ahead of the
	// confirm overlay: canRename() refuses to open it while a confirm is up, so
	// the two can never both be live, and typing must never reach the list keys.
	if m.renaming {
		return m.updateRename(msg)
	}
	if m.confirm != confirmNone {
		return m.updateConfirm(msg)
	}

	m.statusMsg = ""
	m.statusNote = ""

	return m.handleListKey(msg)
}

// handleErrModeKey handles key input once chat listing has failed.
//
// Chat listing failed, but the session itself loaded, so the session can still
// be archived — and archiveCmd is now the only TUI archive path. Keep the
// archive flow reachable here; swallow everything else.
func (m Model) handleErrModeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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

// handleListKey handles key input in the chat picker's default list mode —
// every key reachable once no overlay is up and no RPC is in flight. Unhandled
// keys fall through to the table's own navigation bindings.
func (m Model) handleListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var handled bool
	if !m.attentionAction.MergeUnresolved() {
		m.attentionAction, cmd, handled = m.attentionAction.Start(m.ctx, m.client, m.session, msg.String(), true, m.external.OpenURL)
	}
	if handled {
		m.table.SetHeight(m.tableHeight())
		return m, cmd
	}
	switch msg.String() {
	case "esc":
		m.cancel = true
		return m, nil
	case "n":
		return m.keyNewChat()
	case "s":
		return m, func() tea.Msg {
			return ui.SwitchViewMsg{
				View:      ui.ViewSessionSettings,
				SessionID: m.sessionID,
			}
		}
	case "t":
		return m.keyNewTab()
	case "g":
		return m.keyOpenGitHub()
	case "l":
		return m.keyOpenTracker()
	case "m":
		return m.keyMerge()
	case "a":
		return m.keyArchive()
	case "w":
		return m.keyWake()
	case "c":
		return m.keySwitchAccount()
	case "d":
		return m.keyDelete()
	case "r":
		return m.keyRename()
	case "enter":
		return m.keyOpenChat()
	}

	// Forward navigation keys to the table.
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// keyNewChat handles [n]ew chat: open the agent picker when more than one
// runner is loaded, otherwise go straight to a fresh attach.
func (m Model) keyNewChat() (tea.Model, tea.Cmd) {
	if len(m.agents) > 1 {
		m.pickingAgent = true
		m.buildAgentTable()
		return m, nil
	}
	return m, func() tea.Msg {
		return ui.SwitchViewMsg{
			View:      ui.ViewAttach,
			SessionID: m.sessionID,
		}
	}
}

// keyNewTab handles [t]erminal: open the worktree in a new terminal tab.
func (m Model) keyNewTab() (tea.Model, tea.Cmd) {
	if !m.newTabSupported || m.session == nil {
		return m, nil
	}
	path := m.session.GetWorktreePath()
	if path == "" {
		return m, nil
	}
	return m, func() tea.Msg {
		return newTabResultMsg{err: m.external.OpenNewTab(path)}
	}
}

// keyOpenGitHub handles the [g]ithub action key. When the action is hidden,
// swallow it (like the m/[m]erge key) rather than letting it fall through to
// the table's go-to-top binding and silently move the cursor — a hidden
// shortcut must do nothing. (home still goes to the top.)
func (m Model) keyOpenGitHub() (tea.Model, tea.Cmd) {
	if !m.canOpenGitHub() {
		return m, nil
	}
	repoURL := m.repoWebLink.url
	return m, func() tea.Msg {
		return webOpenResultMsg{err: m.external.OpenURL(repoURL)}
	}
}

// keyOpenTracker handles the [l]inear action key. When the action is hidden,
// swallow it to avoid unintended side-effects — a hidden shortcut must do
// nothing.
func (m Model) keyOpenTracker() (tea.Model, tea.Cmd) {
	if !m.canOpenTracker() {
		return m, nil
	}
	trackerURL := m.session.GetTrackerUrl()
	return m, func() tea.Msg {
		return webOpenResultMsg{err: m.external.OpenURL(trackerURL)}
	}
}

// keyMerge handles [m]: on a verify-parked PR it raises the confirmation that
// approves the park so verify merges, otherwise the merge confirmation when
// merge is available.
func (m Model) keyMerge() (tea.Model, tea.Cmd) {
	if m.canMerge() {
		m.confirm = confirmMerge
	}
	return m, nil
}

// keyArchive handles [a]rchive: raise the archive confirmation.
func (m Model) keyArchive() (tea.Model, tea.Cmd) {
	if m.sessionID != "" && !m.loading && m.confirm == confirmNone && !m.merging && !m.archiving {
		m.confirm = confirmArchive
	}
	return m, nil
}

// keyWake handles [w]ake for the selected chat.
func (m Model) keyWake() (tea.Model, tea.Cmd) {
	chat := m.selectedChat()
	if chat == nil {
		return m, nil
	}
	// Only fire WakeChat for chats whose daemon-reported status is
	// "stopped". For any other status (working, idle, question, or
	// unknown) the wake call would be a no-op (OUTCOME_ALREADY_LIVE)
	// but firing it is misleading UX — a transient "Waking…" flash
	// for a chat that's already healthy.
	if m.daemonStatuses[chat.AgentSessionId] != ui.StatusStopped {
		return m, nil
	}
	m.statusMsg = "Waking…"
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
}

// keySwitchAccount handles swit[c]h account (BOS-171): stop → swap → resume the
// selected chat under a chosen rotation account. Open the account picker scoped
// to the chat's provider; the mid-turn confirm (if WORKING) comes after the
// account is chosen.
func (m Model) keySwitchAccount() (tea.Model, tea.Cmd) {
	chat := m.selectedChat()
	if chat == nil {
		return m, nil
	}
	m.switchTargetChatID = chat.AgentSessionId
	m.switchNotice = ""
	return m, m.loadSwitchAccounts(m.chatAgentName(chat))
}

// keyDelete handles [d]elete: raise the delete confirmation for the selected
// chat when no delete is already in flight.
func (m Model) keyDelete() (tea.Model, tea.Cmd) {
	if chat := m.selectedChat(); chat != nil && m.deletingAgentSessionID == "" {
		m.confirm = confirmDelete
	}
	return m, nil
}

// keyRename handles [r]ename: open the inline title prompt over the selected
// row, prefilled with the current title. When the action is hidden, swallow the
// key (like [g]ithub and [m]erge) rather than letting it fall through to the
// table, whose default keymap could otherwise move the cursor — a hidden
// shortcut must do nothing.
func (m Model) keyRename() (tea.Model, tea.Cmd) {
	if !m.canRename() {
		return m, nil
	}
	chat := m.selectedChat()
	m.renaming = true
	m.renameTargetChatID = chat.AgentSessionId
	m.renameErr = ""
	m.renameInput.SetValue(chat.GetTitle())
	m.renameInput.CursorEnd()
	return m, m.renameInput.Focus()
}

// updateRename handles keys while the inline rename prompt is open: enter
// commits, esc cancels, everything else edits the title.
func (m Model) updateRename(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return m.commitRename()
	case "esc":
		return m.cancelRename(), nil
	}

	var cmd tea.Cmd
	// Normalized for the same reason the list filter normalizes: a synthesized
	// KeyPressMsg carrying only Code (as tests and some terminals produce) has
	// an empty Text, and textinput inserts Text.
	m.renameInput, cmd = m.renameInput.Update(ui.NormalizePrintableKey(msg))
	m.renameErr = ""
	return m, cmd
}

// commitRename validates the edited title, applies it optimistically so the row
// updates immediately, and dispatches the UpdateChatTitle RPC. The RPC is
// addressed at renameTargetChatID — the chat captured when the prompt opened —
// so a poll that reorders the list mid-edit cannot retarget the write.
func (m Model) commitRename() (tea.Model, tea.Cmd) {
	title := strings.TrimSpace(m.renameInput.Value())
	if title == "" {
		// Keep the prompt open: an empty title is a correctable mistake, and
		// closing it would discard what the operator typed.
		m.renameErr = "Title cannot be empty"
		return m, nil
	}

	target := m.renameTargetChatID
	var previous string
	for _, chat := range m.chats {
		if chat.GetAgentSessionId() == target {
			previous = chat.GetTitle()
			chat.Title = title
			break
		}
	}

	m.renaming = false
	m.renameErr = ""
	m.renameInput.Blur()
	m.buildTableRows()
	return m, m.renameChatCmd(target, title, previous)
}

// cancelRename closes the prompt and discards the edit, leaving the title as it
// was. It deliberately does NOT set m.cancel — esc inside a text field cancels
// the edit, not the view.
func (m Model) cancelRename() Model {
	m.renaming = false
	m.renameErr = ""
	m.renameInput.Blur()
	return m
}

// keyOpenChat handles [enter]: attach to the selected chat. A waiting chat
// carries its armed line, untruncated, so the launching view can say what the
// chat is parked on (BOS-1517).
func (m Model) keyOpenChat() (tea.Model, tea.Cmd) {
	if chat := m.selectedChat(); chat != nil {
		resumeID := chat.AgentSessionId
		armedNote := m.armedLine(chat)
		return m, func() tea.Msg {
			return ui.SwitchViewMsg{
				View:      ui.ViewAttach,
				SessionID: m.sessionID,
				ResumeID:  resumeID,
				ArmedNote: armedNote,
			}
		}
	}
	return m, nil
}

// updateConfirm handles the y/n keys for whichever confirmKind is active.
// It clears the prompt, flips any in-flight spinner, and returns the RPC cmd.
func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		case confirmSwitch:
			// Confirmed against a mid-turn chat — force the interrupt.
			m.switching = true
			return m, m.switchAccountCmd(true)
		case confirmNone:
			return m, nil
		}
	case "n", "esc":
		m.confirm = confirmNone
	}
	return m, nil
}

// startDelete records the in-flight chat id (drives the table badge) and
// returns the DeleteChat command. Returns (model, cmd) because it mutates the
// model, unlike mergeCmd/archiveCmd which only read it.
func (m Model) startDelete() (tea.Model, tea.Cmd) {
	chat := m.selectedChat()
	if chat == nil {
		return m, nil
	}
	agentSessionID := chat.AgentSessionId
	m.deletingAgentSessionID = agentSessionID
	m.buildTableRows()
	return m, func() tea.Msg {
		// The user pressed delete on a chat they selected: never gated, and
		// never the cleanup reason, whatever the chat's agent is.
		err := m.client.DeleteChat(m.ctx, agentSessionID, pb.DeleteChatRequest_DELETION_REASON_USER_REQUESTED)
		return chatDeletedMsg{agentSessionID: agentSessionID, err: err}
	}
}
