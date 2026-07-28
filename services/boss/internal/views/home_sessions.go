// Home's session-list state: cursor/row arithmetic, the archiving override
// set, the value-delivered latch, and the session-poll handlers. Split out of
// home.go (BOS-526); the declarations are unchanged.

package views

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// saveSettings persists config.Settings to disk. Tests stub this to avoid
// writing real config files.
var saveSettings = config.Save

func (h *HomeModel) markArchiving(sessionID string) {
	if sessionID == "" {
		return
	}
	if h.archivingOverrideIDs == nil {
		h.archivingOverrideIDs = make(map[string]struct{})
	}
	if h.archiveInFlightIDs == nil {
		h.archiveInFlightIDs = make(map[string]struct{})
	}
	h.archivingOverrideIDs[sessionID] = struct{}{}
	h.archiveInFlightIDs[sessionID] = struct{}{}
}

// resolveArchive records an archive RPC result. Success keeps the render
// override until polling confirms the row is gone; failure drops it at once.
func (h *HomeModel) resolveArchive(sessionID string, err error) {
	delete(h.archiveInFlightIDs, sessionID)
	if err != nil {
		delete(h.archivingOverrideIDs, sessionID)
	}
}

func (h HomeModel) isArchiving(sessionID string) bool {
	_, ok := h.archivingOverrideIDs[sessionID]
	return ok
}

func (h HomeModel) archiveInFlight(sessionID string) bool {
	_, ok := h.archiveInFlightIDs[sessionID]
	return ok
}

func (h *HomeModel) reconcileArchivingSessions() {
	for sessionID := range h.archivingOverrideIDs {
		if !sessionsContainID(h.sessions, sessionID) {
			delete(h.archivingOverrideIDs, sessionID)
			delete(h.archiveInFlightIDs, sessionID)
		}
	}
}

func cloneSessionIDSet(ids map[string]struct{}) map[string]struct{} {
	clone := make(map[string]struct{}, len(ids))
	for id := range ids {
		clone[id] = struct{}{}
	}
	return clone
}

func sessionNeedsAttention(sess *pb.Session) bool {
	return sess != nil && displaystatus.IsQuestionLabel(sess.GetDisplayLabel())
}

// newlyQuestionSessions identifies question-state rising edges between two
// successful session polls. An empty prior slice deliberately makes the first
// successful poll notify for already-waiting questions.
func newlyQuestionSessions(previous, incoming []*pb.Session) []*pb.Session {
	previousQuestions := make(map[string]bool, len(previous))
	for _, sess := range previous {
		if sessionNeedsAttention(sess) {
			previousQuestions[sess.GetId()] = true
		}
	}

	newlyQuestion := make([]*pb.Session, 0)
	for _, sess := range incoming {
		if sessionNeedsAttention(sess) && !previousQuestions[sess.GetId()] {
			newlyQuestion = append(newlyQuestion, sess)
		}
	}
	return newlyQuestion
}

// valueDelivered reports whether the user has received real value: a repo,
// a session, and a chat.
func valueDelivered(repoCount, sessionCount int, hasChat bool) bool {
	return repoCount > 0 && sessionCount > 0 && hasChat
}

// sessionsHaveChat reports whether any session has an active chat. has_active_chat
// (heartbeat-tracked) is the cleanest available "has started a chat" signal on the
// session list — no extra RPC.
func sessionsHaveChat(sessions []*pb.Session) bool {
	for _, s := range sessions {
		if s.GetHasActiveChat() {
			return true
		}
	}
	return false
}

// latchValueDeliveredIfNeeded sets the one-time BossCloudValueDeliveredAt milestone
// the first time the user has a repo + session + chat, and persists it. Set-once:
// the timestamp never moves, so the promo stays eligible even if the repo is later
// deleted. Persist failures are non-fatal — the latch retries on the next poll.
func (h *HomeModel) latchValueDeliveredIfNeeded() {
	if !valueDelivered(h.repoCount, len(h.sessions), sessionsHaveChat(h.sessions)) {
		return
	}
	updated, dirty := h.settings.EnsureBossCloudValueDeliveredAt(h.currentTime())
	if !dirty {
		return
	}
	if err := saveSettings(updated); err == nil {
		h.settings = updated
	}
	// On save error, leave h.settings unchanged so the latch retries next poll.
}

// applyMergedOptimisticOverride overrides the tracked session's display
// status to MERGED until the daemon webhook catches up. Clears the override
// once the server reports a terminal state.
func (h *HomeModel) applyMergedOptimisticOverride() {
	if h.mergedOptimisticID == "" {
		return
	}
	for _, sess := range h.sessions {
		if sess.Id != h.mergedOptimisticID {
			continue
		}
		switch sess.GetDisplayStatus() {
		case pb.DisplayStatus_DISPLAY_STATUS_MERGED,
			pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
			h.mergedOptimisticID = ""
		default:
			sess.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
		}
		return
	}
}

// sessionByID returns the session with the given id, or nil when none matches
// (including the empty id). Callers pass the result to nil-safe helpers.
func sessionByID(sessions []*pb.Session, id string) *pb.Session {
	if id == "" {
		return nil
	}
	for _, s := range sessions {
		if s.GetId() == id {
			return s
		}
	}
	return nil
}

// sessionsContainID reports whether any session in the slice has the given id.
func sessionsContainID(sessions []*pb.Session, id string) bool {
	for _, s := range sessions {
		if s.Id == id {
			return true
		}
	}
	return false
}

// selectedSessionID returns the ID of the currently highlighted session,
// or "" if no session is selected.
func (h HomeModel) selectedSessionID() string {
	sess := h.selectedSession()
	if sess == nil {
		return ""
	}
	return sess.Id
}

func (h HomeModel) selectedSession() *pb.Session {
	idx, ok := h.sessionIndexForTableCursor(h.table.Cursor())
	if !ok {
		return nil
	}
	return h.sessions[idx]
}

func (h HomeModel) sessionIndexForTableCursor(cursor int) (int, bool) {
	if cursor < 0 {
		return 0, false
	}
	row := 0
	for i, sess := range h.sessions {
		next := row + 1 + sessionSubRowCount(sess)
		if cursor >= row && cursor < next {
			return i, true
		}
		row = next
	}
	return 0, false
}

func (h HomeModel) primarySessionRows() []int {
	rows := make([]int, 0, len(h.sessions))
	row := 0
	for _, sess := range h.sessions {
		rows = append(rows, row)
		row += 1 + sessionSubRowCount(sess)
	}
	return rows
}

func (h HomeModel) tableDataRowCount() int {
	rows := len(h.sessions)
	for _, sess := range h.sessions {
		rows += sessionSubRowCount(sess)
	}
	return rows
}

// neighborSessionID returns the ID of the session that should receive the
// cursor once the session with removedID leaves the list (e.g. on archive). It
// prefers the next session down so the cursor stays at the same visual position
// after the list closes the gap; if the removed session was last, it falls back
// to the previous session. Returns "" when there is no suitable neighbor (the
// removed session was the only one, or is not in the current list).
func (h HomeModel) neighborSessionID(removedID string) string {
	idx := -1
	for i, sess := range h.sessions {
		if sess.Id == removedID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ""
	}
	if idx+1 < len(h.sessions) {
		return h.sessions[idx+1].Id
	}
	if idx-1 >= 0 {
		return h.sessions[idx-1].Id
	}
	return ""
}

func (h HomeModel) tableCursorForSessionIndex(sessionIndex int) int {
	row := 0
	for i, sess := range h.sessions {
		if i == sessionIndex {
			return row
		}
		row += 1 + sessionSubRowCount(sess)
	}
	return -1
}

// tableCursorForSessionID returns the table cursor row for the session with the
// given id, or (-1, false) when no session matches (including the empty id).
func (h HomeModel) tableCursorForSessionID(id string) (int, bool) {
	if id == "" {
		return -1, false
	}
	for i, sess := range h.sessions {
		if sess.Id == id {
			return h.tableCursorForSessionIndex(i), true
		}
	}
	return -1, false
}

func (h *HomeModel) normalizeTableCursor(previousCursor int) {
	rows := h.primarySessionRows()
	if len(rows) == 0 {
		return
	}
	cursor := h.table.Cursor()
	for _, row := range rows {
		if cursor == row {
			return
		}
	}
	if cursor > previousCursor {
		for _, row := range rows {
			if row > cursor {
				h.table.SetCursor(row)
				return
			}
		}
	} else if cursor < previousCursor {
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i] < cursor {
				h.table.SetCursor(rows[i])
				return
			}
		}
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i] <= cursor {
			h.table.SetCursor(rows[i])
			return
		}
	}
	h.table.SetCursor(rows[0])
}

// handleSessionList applies a session poll result: the generation and poll-ID
// guards first, then either the failure debounce or the successful merge.
func (h HomeModel) handleSessionList(msg sessionListMsg) (tea.Model, tea.Cmd) {
	if msg.homeGeneration != 0 && msg.homeGeneration != h.generation {
		return h, nil
	}
	if msg.pollID != 0 {
		if msg.pollID < h.latestSessionPollID {
			return h, nil
		}
		h.latestSessionPollID = msg.pollID
	}
	h.loading = false
	if msg.err != nil {
		return h.handleSessionListError(msg.err)
	}
	return h.applySessionList(msg)
}

func (h HomeModel) handleSessionListError(err error) (tea.Model, tea.Cmd) {
	if h.restarting {
		h.buildTableRows()
		return h, nil
	}
	// Debounce transient poll failures: keep the last-good session list
	// on screen and only surface the "Cannot connect to daemon" view
	// after several consecutive failures. This stops the constant
	// flashing when the daemon is briefly unreachable (e.g. busy).
	h.pollFailures++
	if h.pollFailures >= pollFailureThreshold {
		h.err = err
		h.daemonRemediation = daemonDownRemediation()
	}
	h.buildTableRows()
	return h, nil
}

// applySessionList merges a successful poll into the model.
func (h HomeModel) applySessionList(msg sessionListMsg) (tea.Model, tea.Cmd) {
	// Successful poll: clear the failure streak and any error screen.
	h.pollFailures = 0
	h.err = nil
	h.daemonRemediation = ""
	// Capture the session under the cursor before the list is replaced so we
	// can keep it selected across the poll even if rows above it disappear
	// (e.g. a sibling session finished archiving). Empty when nothing is
	// selected. See BOS-367.
	selectedID := h.selectedSessionID()
	notifyCmd := h.notifyNewQuestions(msg.sessions)
	h.sessions = msg.sessions
	h.latchValueDeliveredIfNeeded()
	h.daemonStatuses = msg.daemonStatuses
	h.applyMergedOptimisticOverride()
	h.reconcileArchivingSessions()
	h.buildTableRows()
	h.restoreTableCursor(selectedID)
	return h, notifyCmd
}

// notifyNewQuestions returns the OS-notification command for sessions that just
// entered the question state, and records the most recent one when boss is in
// the background. Called before h.sessions is replaced, so it compares the
// previous poll against the incoming one.
func (h *HomeModel) notifyNewQuestions(incoming []*pb.Session) tea.Cmd {
	var notifyCmd tea.Cmd
	if config.NotificationsEnabled(h.settings) {
		if newlyQuestion := newlyQuestionSessions(h.sessions, incoming); len(newlyQuestion) > 0 {
			notifyCmd = notifyForSessions(newlyQuestion)
			// Remember the most recent question only when boss is in the
			// background, so a subsequent focus (e.g. the user clicking the OS
			// notification) can jump straight into it. If boss is focused the
			// user is already here — don't hijack their view.
			if !h.focused {
				h.pendingAttentionSessionID = newlyQuestion[len(newlyQuestion)-1].GetId()
			}
		}
	}
	return notifyCmd
}

// restoreTableCursor places the cursor after a poll rebuilt the rows: an
// explicit highlight request wins, then the session that was selected before
// the poll, then a normalized fallback.
func (h *HomeModel) restoreTableCursor(selectedID string) {
	if h.highlightSessionID != "" {
		if row, ok := h.tableCursorForSessionID(h.highlightSessionID); ok {
			h.table.SetCursor(row)
			updateCursorColumn(&h.table)
		}
		h.highlightSessionID = ""
	} else if row, ok := h.tableCursorForSessionID(selectedID); ok {
		// Keep the same session under the cursor across the poll, even when
		// rows above it were removed (BOS-367) — otherwise the cursor holds
		// its row number and slides onto the next session down.
		h.table.SetCursor(row)
		updateCursorColumn(&h.table)
	} else if len(h.sessions) > 0 {
		h.normalizeTableCursor(h.table.Cursor())
		updateCursorColumn(&h.table)
	}
}

func fetchSessions(c client.BossClient, ctx context.Context, homeGeneration, pollID uint64) tea.Cmd {
	return func() tea.Msg {
		sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{}, client.SessionReadOptions{IncludeLocalHTTPEndpoints: true})
		if err != nil {
			return sessionListMsg{homeGeneration: homeGeneration, pollID: pollID, err: err}
		}

		// Fetch daemon-side heartbeat statuses for cross-instance display.
		var daemonStatuses map[string]string
		if len(sessions) > 0 {
			ids := make([]string, len(sessions))
			for i, s := range sessions {
				ids[i] = s.Id
			}
			entries, sErr := c.GetSessionStatuses(ctx, ids)
			if sErr == nil {
				daemonStatuses = make(map[string]string, len(entries))
				for _, e := range entries {
					daemonStatuses[e.SessionId] = chatStatusString(e.Status)
				}
			}
		}

		return sessionListMsg{
			homeGeneration: homeGeneration,
			pollID:         pollID,
			sessions:       sessions,
			daemonStatuses: daemonStatuses,
		}
	}
}
