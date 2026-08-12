package views

// One named handler per top-level message arm of ChatPickerModel.Update
// (BOS-527). Each keeps the original arm's body verbatim and the original
// (tea.Model, tea.Cmd) shape, so the value receiver still carries every
// in-flight-marker mutation forward exactly as the inline arm did.

import (
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

func (m ChatPickerModel) handleSession(msg chatPickerSessionMsg) (tea.Model, tea.Cmd) {
	m.session = msg.session
	return m, tea.Batch(m.listChats(), m.fetchRepoWebLink())
}

func (m ChatPickerModel) handleRepoWebLink(msg repoWebLinkMsg) (tea.Model, tea.Cmd) {
	// Discard a stale in-flight fetch (e.g. one started before a PR existed)
	// whose repo/PR no longer matches the current session; otherwise it could
	// overwrite a freshly installed PR link with the old plain repo URL.
	if msg.repoID != m.session.GetRepoId() || msg.prNumber != int(m.session.GetPrNumber()) {
		return m, nil
	}
	m.repoWebLink = msg.link
	return m, nil
}

func (m ChatPickerModel) handleAgents(msg agentsMsg) (tea.Model, tea.Cmd) {
	// Errors are non-fatal: an empty agent list collapses the picker
	// to its single-agent UX (skip the agent-select phase entirely).
	if msg.err == nil {
		m.agents = msg.agents
	}
	return m, nil
}

func (m ChatPickerModel) handleChatsListed(msg chatsListedMsg) (tea.Model, tea.Cmd) {
	m.loading = false
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.chats = msg.chats
	m.daemonStatuses = msg.daemonStatuses
	m.daemonLastOutput = msg.daemonLastOutput
	m.daemonWaitingReasons = msg.daemonWaitingReasons
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
		// Prefer a chat that is waiting on the user (BOS-494): a `? question`
		// chat is actionable now, so land the cursor there rather than on a
		// newer working/idle chat. Among several question chats, pick the
		// longest-waiting one (oldest chatLastActive). Only when no chat is
		// asking do we fall back to the first working/idle/limited chat. Both
		// are resolved in a single pass; the question index wins when set.
		target, fallback := -1, -1
		var oldestWait time.Time
		for i, chat := range m.chats {
			switch m.daemonStatuses[chat.AgentSessionId] {
			case statusQuestion:
				if wait := m.chatLastActive(chat); target == -1 || wait.Before(oldestWait) {
					target, oldestWait = i, wait
				}
			case statusWorking, statusIdle, statusLimited:
				if fallback == -1 {
					fallback = i
				}
			}
		}
		if target == -1 {
			target = fallback
		}
		if target >= 0 {
			m.table.SetCursor(target)
			updateCursorColumn(&m.table)
		}
	}
	return m, m.backfillTitles()
}

func (m ChatPickerModel) handleTitlesBackfilled(msg chatTitlesBackfilledMsg) (tea.Model, tea.Cmd) {
	for i, chat := range m.chats {
		if title, ok := msg.updates[chat.AgentSessionId]; ok {
			m.chats[i].Title = title
		}
	}
	m.buildTableRows()
	return m, nil
}

func (m ChatPickerModel) handleChatDeleted(msg chatDeletedMsg) (tea.Model, tea.Cmd) {
	if msg.agentSessionID == m.deletingAgentSessionID {
		m.deletingAgentSessionID = ""
	}
	if msg.err != nil {
		m.statusMsg = rpcStatusMessage("Delete failed", msg.err)
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
}

// handleChatRenamed settles an inline [r]ename. The optimistic title is already
// on the row, so success is a no-op; a failure puts the previous title back and
// reports why, rather than leaving the list showing a name the daemon rejected.
func (m ChatPickerModel) handleChatRenamed(msg chatRenamedMsg) (tea.Model, tea.Cmd) {
	if msg.err == nil {
		return m, nil
	}
	for _, chat := range m.chats {
		if chat.GetAgentSessionId() == msg.agentSessionID {
			chat.Title = msg.previousTitle
			break
		}
	}
	m.statusMsg = rpcStatusMessage("Rename failed", msg.err)
	m.buildTableRows()
	return m, nil
}

func (m ChatPickerModel) handleNewTabResult(msg newTabResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.statusMsg = rpcStatusMessage("Couldn't open new tab", msg.err)
		return m, nil
	}
	captureViewTelemetry(m.ctx, m.telemetry, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
		"action": "open",
	})
	return m, nil
}

func (m ChatPickerModel) handleWebOpenResult(msg webOpenResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		// Shared by the [g]ithub and [l]inear shortcuts, so the message
		// stays generic rather than naming a specific destination.
		m.statusMsg = rpcStatusMessage("Couldn't open browser", msg.err)
	}
	return m, nil
}

func (m ChatPickerModel) handleMergeResult(msg mergeResultMsg) (tea.Model, tea.Cmd) {
	if msg.sessionID != m.sessionID {
		return m, nil // orphan completion from a session the user navigated away from
	}
	m.merging = false
	// No tui_action here: session_merged is captured once in App.handleMergeResult
	// (app_handlers.go), which sees every mergeResultMsg whether or not this
	// picker is still on screen.
	if msg.err != nil {
		m.statusMsg = rpcStatusMessage("Couldn't merge", msg.err)
		return m, nil
	}
	// Merge succeeded — stay on the session-detail view showing merged status
	// so the user can archive in place. The server-side PR state transition
	// lands asynchronously via webhook; HomeModel renders the session as
	// MERGED optimistically until the daemon reconciles (if the user then
	// cancels or archives back to the list).
	m.merged = true
	if m.session.GetRepoShouldArchiveSessionsAfterMerge() {
		// Optimistic latch: the daemon will archive on the merge webhook.
		// Show "Archiving…" immediately, before archive_pending round-trips.
		m.optimisticArchiveLatch = true
	}
	return m, nil
}

func (m ChatPickerModel) handleArchiveResult(msg archiveResultMsg) (tea.Model, tea.Cmd) {
	if msg.sessionID != m.sessionID {
		return m, nil // orphan completion from a session the user navigated away from
	}
	m.archiving = false
	// No tui_action here: session_archived is captured once in
	// App.handleArchiveResult (app_handlers.go), which sees every
	// archiveResultMsg whether or not this picker is still on screen.
	if msg.err != nil {
		m.statusMsg = rpcStatusMessage("Couldn't archive session", msg.err)
		return m, nil
	}
	m.archived = true
	m.optimisticArchiveLatch = false // archived; drop the optimistic latch
	return m, nil
}

func (m ChatPickerModel) handleWakeResult(msg wakeResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.statusMsg = rpcStatusMessage("Wake failed", msg.err)
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
}

func (m ChatPickerModel) handleSwitchAccountsLoaded(msg switchAccountsLoadedMsg) (tea.Model, tea.Cmd) {
	// Load errors are non-fatal — open the picker with just the System
	// default row so the operator can still switch to account 0.
	if msg.err != nil {
		m.switchAccounts = nil
	} else {
		m.switchAccounts = msg.accounts
	}
	m.pickingAccount = true
	m.buildSwitchAccountTable()
	return m, nil
}

func (m ChatPickerModel) handleSwitchAccountResult(msg switchAccountResultMsg) (tea.Model, tea.Cmd) {
	m.switching = false
	// No tui_action here: account_switched is captured once in
	// App.handleSwitchAccountResult (app_handlers.go), which sees every
	// switchAccountResultMsg whether or not this picker is still on screen —
	// and Esc during an in-flight switch deliberately leaves it.
	if msg.err != nil {
		// Surface the daemon's human-readable error (cooling/disabled/other)
		// on the same notice line used for success. rpcStatusDetail leaves that
		// answer verbatim and substitutes only when the tunnel carrying it is
		// the thing that failed.
		m.switchNotice = rpcStatusDetail(msg.err)
		return m, nil
	}
	m.switchNotice = msg.resp.GetNoticeText()
	// Reload the chat list AND refresh statuses. A fresh-fallback switch
	// (resumed=false) respawns under a NEW agent_session_id, so it creates a
	// new agent_chats row; refreshing statuses alone would leave m.chats stale
	// and the newly spawned pane unselectable until the picker is reopened.
	// listChats picks up the new row; refreshStatuses keeps the session/PR
	// refresh and the STATUS column current.
	return m, tea.Batch(m.listChats(), m.refreshStatuses())
}

func (m ChatPickerModel) handleRefresh(msg chatPickerRefreshMsg) (tea.Model, tea.Cmd) {
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
		if m.session.GetArchivedAt() != nil {
			// The session was archived out from under us (BOS-46 archive-after-merge,
			// an external merge, or a manual archive elsewhere). Flip the same flag a
			// TUI-initiated archive sets so the app loop returns us to the session list.
			m.archived = true
			m.optimisticArchiveLatch = false // archived; drop the optimistic latch
		}
		// Whether an archive is actually in flight is now the daemon-driven
		// m.session.GetArchivePending() (hydrated from the DisplayTracker), read
		// live off every poll — no longer inferred from MERGED + repo flag, which
		// stayed true forever for a resurrected merged session (BOS-425). Also
		// drop the optimistic merge latch if the repo disabled archive-after-merge
		// while this picker is open, so the latch can't get stuck showing
		// "Archiving…" for an archive the daemon will now skip.
		if !m.session.GetRepoShouldArchiveSessionsAfterMerge() {
			m.optimisticArchiveLatch = false
		}
		// Hand off from the optimistic bridge to the authoritative daemon
		// signal: once the daemon reports the archive actually in flight, drop
		// the latch. isArchiving() keeps rendering the spinner via
		// GetArchivePending() while the archive runs, and when the daemon clears
		// archive_pending (archive completed OR failed — the defer in
		// Lifecycle.ArchiveSession fires on both), the spinner stops from the
		// daemon signal alone. This bounds the latch to the pre-signal window it
		// exists for, so a daemon-side archive that sets then clears
		// archive_pending without archiving (a failed ArchiveSession) can no
		// longer leave a permanently-stuck "Archiving…" spinner (BOS-425).
		if m.session.GetArchivePending() {
			m.optimisticArchiveLatch = false
		}
		if m.session.GetPrNumber() != prevPR {
			m.repoWebLink = repoWebLink{}
			refreshWebLink = m.fetchRepoWebLink()
		}
	}
	if msg.daemonStatuses != nil {
		m.daemonStatuses = msg.daemonStatuses
		// Assigned in the same branch, deliberately: parseChatStatuses returns
		// both maps together or neither, and a chat whose callback just fired
		// drops out of the reasons map entirely. Gating on the reasons map being
		// non-empty would leave a stale reason on screen forever (BOS-668).
		m.daemonWaitingReasons = msg.daemonWaitingReasons
	}
	if msg.daemonLastOutput != nil {
		m.daemonLastOutput = msg.daemonLastOutput
	}
	if len(m.chats) > 0 {
		m.buildTableRows()
	}
	return m, refreshWebLink
}

func (m ChatPickerModel) handleErr(msg chatPickerErrMsg) (tea.Model, tea.Cmd) {
	m.loading = false
	m.err = msg.err
	return m, nil
}
