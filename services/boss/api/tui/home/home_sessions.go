// Home's session-list state: cursor/row arithmetic, the archiving override
// set, the value-delivered latch, and the session-poll handlers. Split out of
// home.go (BOS-526); the declarations are unchanged.

package home

import (
	"context"
	"maps"
	"strings"
	"time"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/displaystatus"
	"github.com/recurser/bossalib/kit/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// saveSettings persists config.Settings to disk. Tests stub this to avoid
// writing real config files.
var SaveSettings = config.Save

func (h *Model) MarkArchiving(sessionID string) {
	if sessionID == "" {
		return
	}
	if h.ArchivingOverrideIDs == nil {
		h.ArchivingOverrideIDs = make(map[string]struct{})
	}
	if h.ArchiveInFlightIDs == nil {
		h.ArchiveInFlightIDs = make(map[string]struct{})
	}
	h.ArchivingOverrideIDs[sessionID] = struct{}{}
	h.ArchiveInFlightIDs[sessionID] = struct{}{}
}

// resolveArchive records an archive RPC result. Success keeps the render
// override until polling confirms the row is gone; failure drops it at once.
//
// A deferred success (BOS-1380) drops it at once too: the archive is pending
// until every chat is idle, so the row's "archiving" must come only from the
// daemon's archive_pending. Keeping the local override would render
// "archiving" forever if the pending archive is cancelled (resurrect, new
// chat), because the row would then never leave the list.
func (h *Model) ResolveArchive(sessionID string, err error, deferred bool) {
	delete(h.ArchiveInFlightIDs, sessionID)
	if err != nil || deferred {
		delete(h.ArchivingOverrideIDs, sessionID)
	}
}

func (h Model) IsArchiving(sessionID string) bool {
	_, ok := h.ArchivingOverrideIDs[sessionID]
	return ok
}

func (h Model) ArchiveInFlight(sessionID string) bool {
	_, ok := h.ArchiveInFlightIDs[sessionID]
	return ok
}

func (h *Model) MarkMerging(a ui.AttentionActionState) {
	if h.MergesInFlight == nil {
		h.MergesInFlight = make(map[string]ui.AttentionActionState)
	}
	h.MergesInFlight[a.SessionID] = a
}

// mergeInFlight returns the session's merge while its outcome is not known, so
// a reopened details view carries it.
func (h Model) MergeInFlight(sessionID string) (ui.AttentionActionState, bool) {
	m, ok := h.MergesInFlight[sessionID]
	return m, ok
}

// isMerging reports a session whose row reads "merging": no other attention
// action is offered on it until the merge settles.
func (h Model) isMerging(sessionID string) bool {
	return h.MergesInFlight[sessionID].MergeUnresolved()
}

// showActionResult puts an action's outcome on the status line and returns the
// timer that clears it. A merge's row stops reading "merging", unless the
// merge's outcome is not known yet: then the line says so until it settles.
func (h *Model) ShowActionResult(msg ui.AttentionActionResultMsg) tea.Cmd {
	if msg.Action == pb.AttentionAction_ATTENTION_ACTION_MERGE {
		if msg.Unsettled {
			h.MarkMerging(ui.AwaitingMerge(msg.SessionID, msg.Text))
		} else {
			delete(h.MergesInFlight, msg.SessionID)
		}
	}
	switch {
	case msg.Err != nil:
		return h.setStatus(ui.RPCStatusDetail(msg.Err), statusFailure)
	case msg.Unsettled:
		return h.setStatus(msg.Text, statusOngoing)
	default:
		return h.setStatus(msg.Text, statusOutcome)
	}
}

// settleUnansweredMerges ends each unanswered merge the poll shows has ended.
// A session gone from the list takes its waiting line with it: Home does not
// list archived sessions, so the poll cannot say whether it merged.
func (h *Model) settleUnansweredMerges() tea.Cmd {
	var expire tea.Cmd
	for id, m := range h.MergesInFlight {
		if m.Waiting == "" {
			continue
		}
		sess := sessionByID(h.Sessions, id)
		if sess == nil {
			if h.Status == m.Waiting {
				h.clearStatus()
			}
			delete(h.MergesInFlight, id)
			continue
		}
		status, isErr, settled := ui.SettleUnansweredMerge(sess)
		if !settled {
			continue
		}
		delete(h.MergesInFlight, id)
		kind := statusOutcome
		if isErr {
			kind = statusFailure
		}
		expire = h.setStatus(status, kind)
	}
	return expire
}

func (h *Model) reconcileArchivingSessions() {
	for sessionID := range h.ArchivingOverrideIDs {
		if !sessionsContainID(h.Sessions, sessionID) {
			delete(h.ArchivingOverrideIDs, sessionID)
			delete(h.ArchiveInFlightIDs, sessionID)
		}
	}
}

func CloneSessionIDSet(ids map[string]struct{}) map[string]struct{} {
	clone := make(map[string]struct{}, len(ids))
	for id := range ids {
		clone[id] = struct{}{}
	}
	return clone
}

func sessionNeedsAttention(sess *pb.Session) bool {
	if sess.GetAttention() != nil {
		return attention.NeedsYou(sess.GetAttention().GetKind())
	}
	return sess != nil && displaystatus.IsQuestionLabel(sess.GetDisplayLabel())
}

// newlyNeedsYouSessions identifies Needs you rising edges and changed fix alerts
// between successful polls. shownParks is rememberShownParks over the polls
// before incoming, and told is rememberToldAttention over them: a session whose
// attention the operator was already told about is not news again, however
// many polls in between showed it in progress, as a restarted daemon does
// until it has read every PR.
func newlyNeedsYouSessions(previous, incoming []*pb.Session, shownParks, told map[string]string) []*pb.Session {
	previousNeedsYou := make(map[string]*pb.Session, len(previous))
	for _, sess := range previous {
		if sessionNeedsAttention(sess) {
			previousNeedsYou[sess.GetId()] = sess
		}
	}

	newlyNeedsYou := make([]*pb.Session, 0)
	for _, sess := range incoming {
		if sessionNeedsAttention(sess) && raisesNewNeed(previousNeedsYou[sess.GetId()], sess, shownParks[sess.GetId()]) &&
			!alreadyTold(told, sess) {
			newlyNeedsYou = append(newlyNeedsYou, sess)
		}
	}
	return newlyNeedsYou
}

// raisesNewNeed reports whether sess, which needs you, asks for something the
// operator has not been told about. old is the session as the previous poll had
// it, nil when it did not need you then; shownPark is the park its row last
// showed.
//
// When a fix alert clears, by dismissal or on its own, the session falls
// through to whatever lay under it, usually a verify park. That park is news
// only if the board has not already shown it: a dismissed repair failure on a
// parked PR reveals the same park the operator saw before the failure, and
// must not raise a fresh notification right after they acted on the session.
func raisesNewNeed(old, sess *pb.Session, shownPark string) bool {
	if old == nil {
		return true
	}
	att := sess.GetAttention()
	if att.GetAlertKey() == old.GetAttention().GetAlertKey() {
		return false
	}
	if att.GetAlertKey() == "" && len(att.GetParkCodes()) > 0 {
		return parkKey(att) != shownPark
	}
	return att.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX
}

// attentionIdentity names one attention for rememberToldAttention: its kind,
// when it began, the alert and park it names and the PR head it is about. A
// question begins when it is asked, and adds its chat and words for an older
// daemon that dates every question in one state alike. Empty for a daemon that
// serves no attention.
func attentionIdentity(sess *pb.Session) string {
	att := sess.GetAttention()
	if att == nil {
		return ""
	}
	parts := []string{att.GetKind().String(), att.GetSince().AsTime().UTC().Format(time.RFC3339Nano), att.GetAlertKey(), parkKey(att), sess.GetPrDisplayHeadSha()}
	if att.GetKind() == pb.AttentionKind_ATTENTION_KIND_QUESTION {
		parts = append(parts, att.GetTargetChatId(), att.GetSentence())
	}
	return strings.Join(parts, "\x00")
}

func alreadyTold(told map[string]string, sess *pb.Session) bool {
	id := attentionIdentity(sess)
	return id != "" && told[sess.GetId()] == id
}

// rememberToldAttention returns, by session id, the identity of the last Needs
// you attention each session's row showed. A row that leaves Needs you keeps
// it, so the same attention coming back is not announced again.
func rememberToldAttention(told map[string]string, incoming []*pb.Session) map[string]string {
	next := maps.Clone(told)
	if next == nil {
		next = make(map[string]string, len(incoming))
	}
	for _, sess := range incoming {
		if sessionNeedsAttention(sess) {
			next[sess.GetId()] = attentionIdentity(sess)
		}
	}
	return next
}

// parkKey identifies the verify park an attention shows; empty when none.
func parkKey(att *pb.SessionAttention) string {
	return strings.Join(att.GetParkCodes(), ",")
}

// rememberShownParks returns, by session id, the verify park each row last
// showed. A fix alert or a question outranks a park and hides it, so while one
// is up the session keeps the park shown before it; any other attention shows
// the park it carries, or none.
func RememberShownParks(shownParks map[string]string, incoming []*pb.Session) map[string]string {
	next := make(map[string]string, len(shownParks))
	for _, sess := range incoming {
		att := sess.GetAttention()
		id := sess.GetId()
		switch {
		case att.GetAlertKey() != "" || att.GetKind() == pb.AttentionKind_ATTENTION_KIND_QUESTION:
			if park, ok := shownParks[id]; ok {
				next[id] = park
			}
		case len(att.GetParkCodes()) > 0:
			next[id] = parkKey(att)
		}
	}
	return next
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
func (h *Model) latchValueDeliveredIfNeeded() {
	if !valueDelivered(h.RepoCount, len(h.Sessions), sessionsHaveChat(h.Sessions)) {
		return
	}
	updated, dirty := h.Settings.EnsureBossCloudValueDeliveredAt(h.currentTime())
	if !dirty {
		return
	}
	if err := SaveSettings(updated); err == nil {
		h.Settings = updated
	}
	// On save error, leave h.settings unchanged so the latch retries next poll.
}

// applyMergedOptimisticOverride overrides the tracked session's display
// status to MERGED until the daemon webhook catches up. Clears the override
// once the server reports a terminal state.
func (h *Model) applyMergedOptimisticOverride() {
	if h.MergedOptimisticID == "" {
		return
	}
	for _, sess := range h.Sessions {
		if sess.Id != h.MergedOptimisticID {
			continue
		}
		switch sess.GetDisplayStatus() {
		case pb.DisplayStatus_DISPLAY_STATUS_MERGED,
			pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
			h.MergedOptimisticID = ""
		default:
			sess.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED
		}
		return
	}
}

// renameSessionCmd writes a new title for one session (BOS-837). The client,
// context, id and title are captured by value so the command is unaffected by
// anything the model does — including the 2s poll replacing h.sessions — between
// the keypress and the RPC returning.
func renameSessionCmd(c Client, ctx context.Context, sessionID, title string) tea.Cmd {
	return func() tea.Msg {
		sess, err := c.UpdateSession(ctx, &pb.UpdateSessionRequest{
			Id:    sessionID,
			Title: &title,
		})
		return SessionRenamedMsg{Session: sess, Err: err}
	}
}

// handleSessionRenamed adopts the daemon's post-write session record. The title
// comes from the response rather than from what was typed, so the board shows
// what the daemon actually stored (it may normalize).
//
// The patch is not immune to the poll: a sessionListMsg that was already in
// flight when the response landed carries the pre-rename title and replaces
// h.sessions wholesale, so the old title can reappear for up to one poll
// interval. That is cosmetic and self-healing — the daemon has already stored
// the new title, so the next poll brings it back — which is why no override is
// retained for it the way applyMergedOptimisticOverride retains merge state.
//
// A failure leaves every title untouched — nothing was written optimistically —
// and reports it on the status line in the failure colour, where it stays until
// the operator's next key.
func (h Model) handleSessionRenamed(msg SessionRenamedMsg) (tea.Model, tea.Cmd) {
	if msg.Err != nil {
		h.setStatus(ui.RPCStatusMessage("Rename failed", msg.Err), statusFailure)
		h.Table.SetHeight(h.TableHeight())
		return h, nil
	}
	renamed := msg.Session
	if renamed.GetId() == "" {
		return h, nil
	}
	for i, sess := range h.Sessions {
		if sess.GetId() != renamed.GetId() {
			continue
		}
		// Patch only the title: the response is a snapshot from the write, and
		// replacing the whole row would discard the poll-derived display state
		// (heartbeat status, waiting reason) the board has since layered on.
		//
		// The patch lands on a CLONE, and the slice entry is swapped rather
		// than written through. h.sessions holds the very pointers
		// applySessionList handed to notifyForSessions, which reads GetTitle()
		// off them from a tea.Cmd goroutine while this runs on the update loop
		// — assigning sess.Title in place would race that read. Never mutate a
		// *pb.Session already published into h.sessions.
		patched := proto.CloneOf(sess)
		patched.Title = renamed.GetTitle()
		h.Sessions[i] = patched
		h.BuildTableRows()
		break
	}
	// Outside the loop deliberately: the write already succeeded, so the
	// operator is owed the acknowledgement even when the renamed session is no
	// longer on the board — a poll can archive it, or move it to another repo's
	// filter, between the keystroke and the response. Reporting only on a
	// matched row would make exactly that case look like the rename was
	// swallowed.
	expire := h.setStatus("Renamed session to "+renamed.GetTitle(), statusOutcome)
	h.Table.SetHeight(h.TableHeight())
	return h, expire
}

// moveSessionCmd asks the daemon to move one session up or down relative to its
// neighbours (BOS-1231). Like renameSessionCmd everything is captured by value,
// so the 2s poll replacing h.sessions between the keypress and the reply cannot
// change what was requested. The neighbour arithmetic deliberately stays in the
// daemon — the TUI sends a direction, never a target rank, so two clients
// cannot derive different positions from the same list.
//
// No repo_id is set: Home renders the unfiltered cross-repo list, so the
// neighbours the daemon reasons about are exactly the rows on screen.
func moveSessionCmd(c Client, ctx context.Context, sessionID string, direction pb.MoveDirection) tea.Cmd {
	return func() tea.Msg {
		_, moved, err := c.MoveSession(ctx, &pb.MoveSessionRequest{
			Id:        sessionID,
			Direction: direction,
		})
		return sessionMovedMsg{SessionID: sessionID, Moved: moved, Err: err}
	}
}

// moveSelectedSession swaps the selected session with the neighbour in the
// given direction, records the optimistic order, and returns the RPC command.
// A move at the boundary of the list is a no-op: the caller still reports the
// key as handled, but nothing is reordered, no override is recorded and no RPC
// is sent — the daemon would answer is_moved=false to the same effect, and not
// asking keeps a held-down key from issuing a round trip per repeat.
func (h Model) moveSelectedSession(direction pb.MoveDirection) (Model, tea.Cmd) {
	if h.Filter.Engaged() {
		h.setStatus("Clear the search to reorder sessions", statusFailure)
		h.Table.SetHeight(h.TableHeight())
		return h, nil
	}

	if !sessionReorderAvailable(h.Client) {
		// Refuse BEFORE painting. Against the hosted orchestrator MoveSession is
		// a hard Unimplemented, so swapping the slice first would reorder the
		// board, report a failure, and revert on the next poll — one honest
		// message beats a flicker plus that message.
		h.setStatus("Reordering sessions is only available against a local daemon", statusFailure)
		h.Table.SetHeight(h.TableHeight())
		return h, nil
	}
	sess := h.SelectedSession()
	if sess == nil {
		return h, nil
	}
	index := -1
	for i, s := range h.Sessions {
		if s.GetId() == sess.GetId() {
			index = i
			break
		}
	}
	if index < 0 {
		return h, nil
	}
	target := index - 1
	if direction == pb.MoveDirection_MOVE_DIRECTION_DOWN {
		target = index + 1
	}
	if target < 0 || target >= len(h.Sessions) {
		// Boundary: unchanged order, unchanged cursor, nothing reported.
		return h, nil
	}

	// Copy before swapping: h.sessions is the very slice applySessionList
	// handed to notifyForSessions, which a tea.Cmd goroutine can still be
	// reading. Reordering in place would race that read.
	reordered := make([]*pb.Session, len(h.Sessions))
	copy(reordered, h.Sessions)
	reordered[index], reordered[target] = reordered[target], reordered[index]
	h.Sessions = reordered
	h.MoveOverrideOrder = SessionIDOrder(reordered)
	h.BuildTableRows()
	// Pin the cursor to the session that moved, by id rather than by row: one
	// session can render several rows (endpoint, waiting and warning sub-rows),
	// so the row it vacated is not in general the row it now occupies, and the
	// row it vacated may not even be a session's primary row. Set directly
	// rather than through restoreTableCursor, which would consume a pending
	// highlightSessionID that belongs to the chat picker's return path.
	if row, ok := h.TableCursorForSessionID(sess.GetId()); ok {
		h.Table.SetCursor(row)
		ui.UpdateCursorColumn(&h.Table)
	}
	// One request at a time. Dispatching each press as its own tea.Cmd lets
	// bubbletea run them concurrently, so the daemon can serve alt+down and
	// alt+up in the opposite order to the keypresses — the first then lands as
	// a boundary no-op and the persisted order contradicts what was typed. The
	// board has already moved above; only the RPC queues.
	queued := QueuedMove{SessionID: sess.GetId(), Direction: direction}
	if h.MoveInFlight > 0 {
		// Copy before appending: Model is passed by value, so appending in
		// place could write through a backing array this model shares with the
		// one it was copied from.
		h.MovePending = append(append([]QueuedMove(nil), h.MovePending...), queued)
		return h, nil
	}
	h.MoveInFlight++
	return h, moveSessionCmd(h.Client, h.Ctx, queued.SessionID, queued.Direction)
}

// sessionReorderCapable is the optional capability moveSelectedSession consults
// before painting an optimistic reorder. Only a client that CANNOT serve
// MoveSession implements it — the hosted orchestrator, until BOS-1232 — so
// absence means capable and LocalClient and every test double are untouched.
type sessionReorderCapable interface {
	CanMoveSession() bool
}

// sessionReorderAvailable reports whether the reorder chords can do anything at
// all against this client.
func sessionReorderAvailable(c Client) bool {
	capable, ok := c.(sessionReorderCapable)
	return !ok || capable.CanMoveSession()
}

// queuedMove is a reorder chord whose RPC has not been sent yet.
type QueuedMove struct {
	SessionID string
	Direction pb.MoveDirection
}

// moveSettled reports that no reorder work is outstanding: nothing in flight
// and nothing queued behind it. It is the condition for releasing
// moveOverrideOrder, because a queued move has not reached the daemon at all —
// every poll is necessarily older than its write.
func (h Model) moveSettled() bool {
	return h.MoveInFlight == 0 && len(h.MovePending) == 0
}

// handleSessionMoved records a MoveSession result. A successful move needs no
// patch — the optimistic order already shows it, and the poll will confirm it.
//
// A failure drops the override and says so on the status line. The locally
// reordered slice is deliberately left alone rather than un-swapped: the next
// poll is at most one interval away and carries the daemon's real order, which
// is the authority — reconstructing the pre-move order here would be a second
// guess at the same answer, and would be wrong if a poll had landed in between.
func (h Model) handleSessionMoved(msg sessionMovedMsg) (tea.Model, tea.Cmd) {
	if h.MoveInFlight > 0 {
		h.MoveInFlight--
	}
	// Each press overwrites moveOverrideOrder with the newest full order, so on
	// a SUCCESSFUL reply the override may describe a chord queued behind this
	// one rather than this one's result: clearing it there would let a poll
	// issued before that write flash the row back — the exact flicker the
	// override exists to prevent. Hence moveSettled below. A failure is
	// different: it empties the queue, so nothing is left for the override to
	// describe.
	if msg.Err != nil {
		// Requests are serialized, so this is the only one that reached the
		// daemon, and every chord queued behind it was computed against a local
		// order the daemon never accepted. Drop the queue with the override
		// rather than send requests derived from it; the next poll is the
		// authority.
		h.MovePending = nil
		h.MoveOverrideOrder = nil
		h.setStatus(ui.RPCStatusMessage("Move failed", msg.Err), statusFailure)
		h.Table.SetHeight(h.TableHeight())
		return h, nil
	}
	if !msg.Moved && h.moveSettled() {
		// A successful no-op: the daemon found the session already at the
		// boundary (the list moved under us between keypress and RPC). Drop the
		// override so the next poll's order wins; never surface an error.
		h.MoveOverrideOrder = nil
	}
	if len(h.MovePending) > 0 {
		// Release the next chord now that the daemon has answered this one, so
		// it reasons about a list that already carries the previous move.
		next := h.MovePending[0]
		h.MovePending = h.MovePending[1:]
		h.MoveInFlight++
		return h, moveSessionCmd(h.Client, h.Ctx, next.SessionID, next.Direction)
	}
	return h, nil
}

// sessionIDOrder projects a session slice onto its id sequence.
func SessionIDOrder(sessions []*pb.Session) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.GetId()
	}
	return ids
}

// applyMoveOverride re-imposes the locally chosen order on a fresh poll, and
// reports whether the override is still needed.
//
// It permutes only the positions held by sessions the override names: every
// other session keeps exactly the slot the daemon gave it, so a session created
// since the chord was pressed lands where the daemon put it rather than being
// shuffled by stale local state.
//
// The override is dropped — "the server's own answer supersedes it" — on the
// first poll that arrives with no move RPC outstanding, WHATEVER order that
// poll carries. Agreement is deliberately not the release condition: the
// daemon's move is multi-position in general, so an override released only on
// exact agreement would never be released at all after one. Holding it while an
// RPC is in flight is what stops a poll issued before the write from flashing
// the row back to where it started.
func (h Model) applyMoveOverride(incoming []*pb.Session) ([]*pb.Session, []string) {
	if len(h.MoveOverrideOrder) == 0 {
		return incoming, nil
	}
	rank := make(map[string]int, len(h.MoveOverrideOrder))
	for i, id := range h.MoveOverrideOrder {
		rank[id] = i
	}

	// The slots the override is allowed to permute, and the sessions in them.
	slots := make([]int, 0, len(incoming))
	known := make([]*pb.Session, 0, len(incoming))
	for i, sess := range incoming {
		if _, ok := rank[sess.GetId()]; ok {
			slots = append(slots, i)
			known = append(known, sess)
		}
	}
	if len(known) == 0 {
		// Every session the override named is gone; it can say nothing about
		// this list.
		return incoming, nil
	}

	ordered := make([]*pb.Session, len(known))
	copy(ordered, known)
	sortSessionsByRank(ordered, rank)

	if h.moveSettled() {
		// Nothing is outstanding, so this poll already carries the daemon's own
		// answer to the move — and the daemon is the authority even when that
		// answer is NOT the adjacent swap the chord optimistically rendered. A
		// rank move is multi-position in both directions (an unranked row joins
		// the END of the ranked block on the way up, so from the third row it
		// rises to the top; the last ranked row clears its rank and falls to its
		// created_at slot on the way down — see db.ComputeListRankMove).
		// Releasing the override only when the incoming order happened to match
		// it would re-impose a wrong local order on every future poll, forever,
		// and mask every later reorder from any source.
		return incoming, nil
	}

	reordered := make([]*pb.Session, len(incoming))
	copy(reordered, incoming)
	for i, slot := range slots {
		reordered[slot] = ordered[i]
	}
	return reordered, h.MoveOverrideOrder
}

// sortSessionsByRank orders sessions by their position in rank. Insertion sort
// keeps it stable and allocation-free; the session list is tens of rows.
func sortSessionsByRank(sessions []*pb.Session, rank map[string]int) {
	for i := 1; i < len(sessions); i++ {
		for j := i; j > 0 && rank[sessions[j].GetId()] < rank[sessions[j-1].GetId()]; j-- {
			sessions[j], sessions[j-1] = sessions[j-1], sessions[j]
		}
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
func (h Model) SelectedSessionID() string {
	sess := h.SelectedSession()
	if sess == nil {
		return ""
	}
	return sess.Id
}

func (h Model) SelectedSession() *pb.Session {
	sessions := h.BoardSessions()
	idx, ok := h.sessionIndexForTableCursor(h.Table.Cursor())
	if !ok {
		return nil
	}
	return sessions[idx]
}

func (h Model) sessionIndexForTableCursor(cursor int) (int, bool) {
	sessions := h.BoardSessions()
	sectioned := h.showsSections()
	if cursor < 0 {
		return 0, false
	}
	row := 0
	for i, sess := range sessions {
		row += h.sectionRows(sessions, i, sectioned)
		next := row + 1 + h.SubRowCount(sess)
		if cursor >= row && cursor < next {
			return i, true
		}
		row = next
	}
	return 0, false
}

func (h Model) primarySessionRows() []int {
	sessions := h.BoardSessions()
	sectioned := h.showsSections()
	rows := make([]int, 0, len(sessions))
	row := 0
	for i, sess := range sessions {
		row += h.sectionRows(sessions, i, sectioned)
		rows = append(rows, row)
		row += 1 + h.SubRowCount(sess)
	}
	return rows
}

func (h Model) TableDataRowCount() int {
	sessions := h.BoardSessions()
	sectioned := h.showsSections()
	rows := len(sessions)
	for i, sess := range sessions {
		rows += h.sectionRows(sessions, i, sectioned)
		rows += h.SubRowCount(sess)
	}
	return rows
}

// neighborSessionID returns the ID of the session that should receive the
// cursor once the session with removedID leaves the list (e.g. on archive). It
// prefers the next session down so the cursor stays at the same visual position
// after the list closes the gap; if the removed session was last, it falls back
// to the previous session. Returns "" when there is no suitable neighbor (the
// removed session was the only one, or is not in the current list).
func (h Model) NeighborSessionID(removedID string) string {
	sessions := h.BoardSessions()
	idx := -1
	for i, sess := range sessions {
		if sess.Id == removedID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ""
	}
	if idx+1 < len(sessions) {
		return sessions[idx+1].Id
	}
	if idx-1 >= 0 {
		return sessions[idx-1].Id
	}
	return ""
}

func (h Model) TableCursorForSessionIndex(sessionIndex int) int {
	sessions := h.BoardSessions()
	sectioned := h.showsSections()
	row := 0
	for i, sess := range sessions {
		row += h.sectionRows(sessions, i, sectioned)
		if i == sessionIndex {
			return row
		}
		row += 1 + h.SubRowCount(sess)
	}
	return -1
}

// tableCursorForSessionID returns the table cursor row for the session with the
// given id, or (-1, false) when no session matches (including the empty id).
func (h Model) TableCursorForSessionID(id string) (int, bool) {
	sessions := h.BoardSessions()
	if id == "" {
		return -1, false
	}
	for i, sess := range sessions {
		if sess.Id == id {
			return h.TableCursorForSessionIndex(i), true
		}
	}
	return -1, false
}

func (h *Model) normalizeTableCursor(previousCursor int) {
	rows := h.primarySessionRows()
	if len(rows) == 0 {
		return
	}
	cursor := h.Table.Cursor()
	for _, row := range rows {
		if cursor == row {
			return
		}
	}
	if cursor > previousCursor {
		for _, row := range rows {
			if row > cursor {
				h.Table.SetCursor(row)
				return
			}
		}
	} else if cursor < previousCursor {
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i] < cursor {
				h.Table.SetCursor(rows[i])
				return
			}
		}
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i] <= cursor {
			h.Table.SetCursor(rows[i])
			return
		}
	}
	h.Table.SetCursor(rows[0])
}

// handleSessionList applies a session poll result: the generation and poll-ID
// guards first, then either the failure debounce or the successful merge.
func (h Model) HandleSessionList(msg SessionListMsg) (tea.Model, tea.Cmd) {
	if msg.HomeGeneration != 0 && msg.HomeGeneration != h.Generation {
		return h, nil
	}
	if msg.PollID != 0 {
		if msg.PollID < h.latestSessionPollID {
			return h, nil
		}
		h.latestSessionPollID = msg.PollID
	}
	h.Loading = false
	if msg.Err != nil {
		return h.handleSessionListError(msg.Err)
	}
	return h.ApplySessionList(msg)
}

func (h Model) handleSessionListError(err error) (tea.Model, tea.Cmd) {
	if h.Restarting {
		h.BuildTableRows()
		return h, nil
	}
	// Debounce transient poll failures: keep the last-good session list
	// on screen and only surface the "Cannot connect to daemon" view
	// after several consecutive failures. This stops the constant
	// flashing when the daemon is briefly unreachable (e.g. busy).
	h.PollFailures++
	if h.PollFailures >= pollFailureThreshold {
		h.Err = err
		h.daemonRemediation = daemonDownRemediation()
	}
	h = h.dismissInputsIfHidden()
	h.BuildTableRows()
	return h, nil
}

// applySessionList merges a successful poll into the model.
func (h Model) ApplySessionList(msg SessionListMsg) (tea.Model, tea.Cmd) {
	// Successful poll: clear the failure streak and any error screen.
	h.PollFailures = 0
	h.Err = nil
	h.daemonRemediation = ""
	// Capture the session under the cursor before the list is replaced so we
	// can keep it selected across the poll even if rows above it disappear
	// (e.g. a sibling session finished archiving). Empty when nothing is
	// selected. See BOS-367.
	selectedID := h.SelectedSessionID()
	notifyCmd := h.notifyNewNeedsYou(msg.Sessions)
	// Re-impose any order the reorder chords chose locally, before the rows are
	// built from it. applyMoveOverride returns the override it wants retained,
	// which is nil once the daemon's own order has caught up (BOS-1231).
	h.Sessions, h.MoveOverrideOrder = h.applyMoveOverride(msg.Sessions)
	h.latchValueDeliveredIfNeeded()
	h.HeartbeatStatuses = msg.HeartbeatStatuses
	h.DaemonWaitingReasons = msg.DaemonWaitingReasons
	h.DaemonPhases = msg.DaemonPhases
	// A poll that succeeded is the whole truth about which organizations are
	// readable right now, so replace rather than merge: an organization that
	// recovered must stop being reported.
	h.SessionReadFailures = msg.SessionReadFailures
	h.applyMergedOptimisticOverride()
	h.reconcileArchivingSessions()
	expire := h.settleUnansweredMerges()
	h = h.dismissInputsIfHidden()
	h.BuildTableRows()
	h.restoreTableCursor(selectedID)
	return h, tea.Batch(notifyCmd, expire)
}

// notifyNewNeedsYou returns the OS-notification command for sessions that just
// entered Needs you, and records the most recent one when boss is in
// the background. Called before h.sessions is replaced, so it compares the
// previous poll against the incoming one. The first poll only records what is
// already waiting: the board shows it, and announcing it again on every boss
// launch is noise.
func (h *Model) notifyNewNeedsYou(incoming []*pb.Session) tea.Cmd {
	var notifyCmd tea.Cmd
	shownParks := h.ShownParks
	h.ShownParks = RememberShownParks(shownParks, incoming)
	told := h.ToldAttention
	h.ToldAttention = rememberToldAttention(told, incoming)
	if told != nil && config.NotificationsEnabled(h.Settings) {
		if newlyNeedsYou := newlyNeedsYouSessions(h.Sessions, incoming, shownParks, told); len(newlyNeedsYou) > 0 {
			notifyCmd = notifyForSessions(newlyNeedsYou)
			// Remember the most recent Needs you session only when boss is in the
			// background, so a subsequent focus (e.g. the user clicking the OS
			// notification) can jump straight into it. If boss is focused the
			// user is already here — don't hijack their view.
			if !h.Focused {
				h.PendingAttentionSessionID = newlyNeedsYou[len(newlyNeedsYou)-1].GetId()
			}
		}
	}
	return notifyCmd
}

// restoreTableCursor places the cursor after a poll rebuilt the rows: an
// explicit highlight request wins, then the session that was selected before
// the poll, then a normalized fallback.
func (h *Model) restoreTableCursor(selectedID string) {
	if h.HighlightSessionID != "" {
		if row, ok := h.TableCursorForSessionID(h.HighlightSessionID); ok {
			h.Table.SetCursor(row)
			ui.UpdateCursorColumn(&h.Table)
		}
		h.HighlightSessionID = ""
	} else if row, ok := h.TableCursorForSessionID(selectedID); ok {
		// Keep the same session under the cursor across the poll, even when
		// rows above it were removed (BOS-367) — otherwise the cursor holds
		// its row number and slides onto the next session down.
		h.Table.SetCursor(row)
		ui.UpdateCursorColumn(&h.Table)
	} else if len(h.Sessions) > 0 {
		h.normalizeTableCursor(h.Table.Cursor())
		ui.UpdateCursorColumn(&h.Table)
	}
}

func FetchSessions(c Client, ctx context.Context, homeGeneration, pollID uint64, includeEndpoints bool) tea.Cmd {
	return func() tea.Msg {
		sessions, readFailures, err := c.ListSessionsWithReadFailures(ctx, &pb.ListSessionsRequest{}, rpc.SessionReadOptions{IncludeLocalHTTPEndpoints: includeEndpoints})
		if err != nil {
			return SessionListMsg{HomeGeneration: homeGeneration, PollID: pollID, Err: err}
		}

		// Fetch daemon-side heartbeat statuses for cross-instance display.
		var daemonStatuses map[string]string
		// Reasons are sparse — only a session parked on an external event carries
		// one — so the map is only populated for the entries that have one, and
		// stays nil against a daemon too old to stamp the field (BOS-668).
		var daemonWaitingReasons map[string]string
		var daemonPhases map[string]string
		if len(sessions) > 0 {
			ids := make([]string, len(sessions))
			for i, s := range sessions {
				ids[i] = s.Id
			}
			entries, sErr := c.GetSessionStatuses(ctx, ids)
			if sErr == nil {
				daemonStatuses = make(map[string]string, len(entries))
				for _, e := range entries {
					daemonStatuses[e.SessionId] = ui.ChatStatusString(e.Status)
					if phase := e.GetPhase(); phase != "" {
						if daemonPhases == nil {
							daemonPhases = make(map[string]string)
						}
						daemonPhases[e.SessionId] = phase
					}
					if reason := e.GetWaitingReason(); reason != "" {
						if daemonWaitingReasons == nil {
							daemonWaitingReasons = make(map[string]string, 1)
						}
						daemonWaitingReasons[e.SessionId] = reason
					}
				}
			}
		}

		return SessionListMsg{
			HomeGeneration:       homeGeneration,
			PollID:               pollID,
			Sessions:             sessions,
			HeartbeatStatuses:    daemonStatuses,
			DaemonWaitingReasons: daemonWaitingReasons,
			DaemonPhases:         daemonPhases,
			SessionReadFailures:  readFailures,
		}
	}
}
