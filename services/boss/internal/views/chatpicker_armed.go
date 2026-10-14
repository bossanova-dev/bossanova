package views

// The armed-callback context for a waiting chat (BOS-1517). A chat parked on a
// GitHub callback has no prompt until the callback fires, so the picker names
// what it is waiting for: the trigger in the STATUS cell, and one line under the
// table carrying the chat title, the daemon's reason verbatim, the session's
// verify park when there is one, and the callback's expiry.
//
// The daemon stays the only producer of the reason (CONCEPTS.md "Waiting
// reason"). The expiry is the one fact not on the wire, so it is read from
// ListGithubCallbacks — once per change in the waiting set, never per poll —
// and matched back to the daemon's reason rather than re-derived, so the TUI
// can never name a different callback than the daemon did. The callback's
// message body is secret (models.proto) and is never read here.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// liveCallbackStates mirrors the daemon's nonTerminalCallbackStates
// (services/bossd/internal/callback): a triggered callback still counts because
// its prompt has not landed yet.
var liveCallbackStates = map[string]bool{"active": true, "leased": true, "triggered": true}

// callbackReason recomposes the daemon's waiting reason for cb.
func callbackReason(cb *pb.GithubCallback) string {
	return displaystatus.CallbackWaitingReason(cb.GetTrigger(), cb.GetRepoOwner(), cb.GetRepoName(), int(cb.GetPrNumber()))
}

// armedCallbackFor returns the callback the daemon's reason describes: live,
// unexpired as of now, and recomposing to exactly reason. Ties go to the oldest
// created_at, then the smallest id — callback.ActiveInterestForChat's order.
func armedCallbackFor(reason string, cbs []*pb.GithubCallback, now time.Time) *pb.GithubCallback {
	if reason == "" {
		return nil
	}
	var best *pb.GithubCallback
	for _, cb := range cbs {
		if cb == nil || !liveCallbackStates[cb.GetState()] || !callbackUnexpired(cb, now) {
			continue
		}
		if callbackReason(cb) != reason {
			continue
		}
		if best == nil || callbackSortsBefore(cb, best) {
			best = cb
		}
	}
	return best
}

func callbackUnexpired(cb *pb.GithubCallback, now time.Time) bool {
	return cb.GetExpiresAt() != nil && now.Before(cb.GetExpiresAt().AsTime())
}

func callbackSortsBefore(a, b *pb.GithubCallback) bool {
	ac, bc := a.GetCreatedAt().AsTime(), b.GetCreatedAt().AsTime()
	if !ac.Equal(bc) {
		return ac.Before(bc)
	}
	return a.GetId() < b.GetId()
}

// expiresInLabel renders a future duration at the coarsest whole unit, floored:
// "in 6d", "in 5h", "in 12m", "in <1m". shortDuration only spans minutes and
// seconds, which would print a seven-day expiry as "10080m".
func expiresInLabel(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("in %dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("in %dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("in %dm", int(d/time.Minute))
	default:
		return "in <1m"
	}
}

// waitKey is one waiting (chat, reason) pair. The set of them decides whether
// the armed callbacks need fetching again.
type waitKey struct {
	chatID string
	reason string
}

// waitingKeys returns the waiting chats that carry a reason, ordered by chat id
// so two polls of the same set compare equal.
func (m ChatPickerModel) waitingKeys() []waitKey {
	var keys []waitKey
	for _, chat := range m.chats {
		if id := chat.GetAgentSessionId(); m.isWaitingWithReason(id) {
			keys = append(keys, waitKey{chatID: id, reason: m.daemonWaitingReasons[id]})
		}
	}
	slices.SortFunc(keys, func(a, b waitKey) int { return strings.Compare(a.chatID, b.chatID) })
	return keys
}

func (m ChatPickerModel) isWaitingWithReason(chatID string) bool {
	return m.daemonStatuses[chatID] == statusWaiting && m.daemonWaitingReasons[chatID] != ""
}

// isStandbyWait reports whether chatID is waiting only on a standby
// (contingency) callback — checks_failed or closed (BOS-1531). Such a chat is
// not what the PR is waiting on: it renders muted, and the waiting line
// describes a blocking chat ahead of it. The daemon's reason decides, so the
// answer never waits on the callback fetch.
func (m ChatPickerModel) isStandbyWait(chatID string) bool {
	return m.isWaitingWithReason(chatID) && displaystatus.IsStandbyWaitingReason(m.daemonWaitingReasons[chatID])
}

// syncArmedCallbacks drops armed entries whose chat stopped waiting on that
// reason (the same rule daemonWaitingReasons follows, BOS-668) and returns a
// fetch only when the waiting set differs from the one last fetched for.
func (m *ChatPickerModel) syncArmedCallbacks() tea.Cmd {
	keys := m.waitingKeys()
	if len(m.armedCallbacks) > 0 {
		kept := make(map[string]*pb.GithubCallback, len(keys))
		for _, k := range keys {
			if cb := m.armedCallbacks[k.chatID]; cb != nil && callbackReason(cb) == k.reason {
				kept[k.chatID] = cb
			}
		}
		m.armedCallbacks = kept
	}
	if slices.Equal(keys, m.armedKeys) {
		return nil
	}
	m.armedKeys = keys
	if len(keys) == 0 || m.client == nil {
		return nil
	}
	return fetchArmedCallbacks(m.client, m.ctx, keys)
}

// fetchArmedCallbacks lists each waiting chat's callbacks by target chat id off
// the UI loop. Best effort: a failed list leaves that chat out of the result, so
// both surfaces fall back to the plain waiting text.
func fetchArmedCallbacks(c client.BossClient, ctx context.Context, keys []waitKey) tea.Cmd {
	keys = slices.Clone(keys)
	return func() tea.Msg {
		now := time.Now()
		byChat := make(map[string]*pb.GithubCallback, len(keys))
		failed := false
		for _, k := range keys {
			chatID := k.chatID
			cbs, err := c.ListGithubCallbacks(ctx, &pb.ListGithubCallbacksRequest{TargetChatId: &chatID})
			if err != nil {
				failed = true
				continue
			}
			if cb := armedCallbackFor(k.reason, cbs, now); cb != nil {
				byChat[chatID] = cb
			}
		}
		return armedCallbacksMsg{byChat: byChat, keys: keys, failed: failed}
	}
}

// handleArmedCallbacks stores a fetch result only while its keys still match
// the waiting set, so a stale reply cannot attach an old expiry to a new wait.
func (m ChatPickerModel) handleArmedCallbacks(msg armedCallbacksMsg) (tea.Model, tea.Cmd) {
	if !slices.Equal(msg.keys, m.waitingKeys()) {
		return m, nil
	}
	m.armedCallbacks = msg.byChat
	// A failed lookup is retried once per waiting set: forgetting armedKeys
	// makes the next poll fetch again, and armedRetriedKeys stops a persistent
	// error from re-fetching on every poll.
	if msg.failed && !slices.Equal(msg.keys, m.armedRetriedKeys) {
		m.armedRetriedKeys = msg.keys
		m.armedKeys = nil
	}
	if len(m.chats) > 0 {
		m.buildTableRows()
	}
	return m, nil
}

// armedCallback returns the known callback chat is parked on, or nil when the
// chat is not waiting, the fetch has not answered, or the callback has since
// stopped matching the daemon's reason or expired.
func (m ChatPickerModel) armedCallback(chat *pb.ClaudeChat) *pb.GithubCallback {
	id := chat.GetAgentSessionId()
	if !m.isWaitingWithReason(id) {
		return nil
	}
	cb := m.armedCallbacks[id]
	if cb == nil || callbackReason(cb) != m.daemonWaitingReasons[id] || !callbackUnexpired(cb, time.Now()) {
		return nil
	}
	return cb
}

// armedParkSummary is the verify park the daemon already hydrates onto the
// session, linked to the waiting chat only while the session is NEEDS_HUMAN.
func armedParkSummary(sess *pb.Session) string {
	if sess.GetDisplayStatus() != pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN {
		return ""
	}
	return attentionWarningHint(sess)
}

// armedLine composes the full, untruncated waiting line for chat, or "" when
// the chat is not waiting with a reason. The park precedes the expiry so a
// truncated line drops the expiry first. A standby reason is prefixed
// "standby: " (never "waiting: "), so the line never presents a contingency
// watch as what the chat is waiting on (BOS-1531).
func (m ChatPickerModel) armedLine(chat *pb.ClaudeChat) string {
	id := chat.GetAgentSessionId()
	if !m.isWaitingWithReason(id) {
		return ""
	}
	reason := m.daemonWaitingReasons[id]
	switch {
	case m.isStandbyWait(id):
		reason = displaystatus.StandbyLabel + ": " + reason
	case waitingBadgeSuperseded(m.session):
		reason = statusWaiting + ": " + reason
	}
	parts := []string{chatDisplayTitle(chat), reason}
	if park := armedParkSummary(m.session); park != "" {
		parts = append(parts, park)
	}
	if cb := m.armedCallback(chat); cb != nil {
		parts = append(parts, "expires "+expiresInLabel(time.Until(cb.GetExpiresAt().AsTime())))
	}
	return strings.Join(parts, " · ")
}

// waitingLineChat picks the chat the waiting line describes, blocking waits
// first (BOS-1531): the cursor chat when it is a blocking wait, then the first
// blocking wait in table order, then the cursor chat when it is a standby wait,
// then the first standby wait.
func (m ChatPickerModel) waitingLineChat() *pb.ClaudeChat {
	selected := m.selectedChat()
	for _, standby := range []bool{false, true} {
		matches := func(chat *pb.ClaudeChat) bool {
			id := chat.GetAgentSessionId()
			return m.isWaitingWithReason(id) && m.isStandbyWait(id) == standby
		}
		if selected != nil && matches(selected) {
			return selected
		}
		for _, chat := range m.chats {
			if matches(chat) {
				return chat
			}
		}
	}
	return nil
}

// chatDisplayTitle is the title a chat row shows.
func chatDisplayTitle(chat *pb.ClaudeChat) string {
	if t := chat.GetTitle(); t != "" {
		return t
	}
	return "New chat"
}
