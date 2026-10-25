package chatpicker

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/recurser/boss/api/tui/ui"

	// The armed-callback context for a waiting chat (BOS-1517). A chat parked on a
	// GitHub callback has no prompt until the callback fires, so the picker names
	// what it is waiting for in the STATUS cell: a phrase for the trigger, or a
	// muted "done" when every wait left is the session's own PR merging or
	// closing. Opening the chat carries the armed line (the chat title, the
	// daemon's reason verbatim, the session's verify park when there is one, and
	// the callback's expiry) to the launch screen.
	//
	// The daemon stays the only producer of the reason (docs/features/chat-picker.md "Waiting
	// reason"). Two facts are not on the wire: the expiry, and whether the chat
	// holds callbacks besides the one the reason names. Both are read from
	// ListGitHubCallbacks — once per change in the waiting set, never per poll.
	// The expiry is matched back to the daemon's reason rather than re-derived, so
	// the TUI can never name a different callback than the daemon did. The
	// callback's message body is secret (models.proto) and is never read here.

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
)

// liveCallbackStates mirrors the daemon's nonTerminalCallbackStates
// (services/bossd/internal/callback): a triggered callback still counts because
// its prompt has not landed yet.
var liveCallbackStates = map[string]bool{"active": true, "leased": true, "triggered": true}

// callbackReason recomposes the daemon's waiting reason for cb.
func callbackReason(cb *pb.GitHubCallback) string {
	return displaystatus.CallbackWaitingReason(cb.GetTrigger(), cb.GetRepoOwner(), cb.GetRepoName(), int(cb.GetPrNumber()))
}

// callbackWaitLabels phrase what a chat armed on each trigger is waiting for.
// The bare trigger misleads in a STATUS cell: "merged" reads as a merged PR.
var callbackWaitLabels = map[models.GitHubCallbackTrigger]string{
	models.GitHubCallbackTriggerMerged:            "awaiting merge",
	models.GitHubCallbackTriggerClosed:            "awaiting close",
	models.GitHubCallbackTriggerChecksPassed:      "awaiting checks",
	models.GitHubCallbackTriggerChecksFailed:      "awaiting failure",
	models.GitHubCallbackTriggerReadyForReview:    "awaiting ready",
	models.GitHubCallbackTriggerChecksPassedReady: "awaiting green",
}

// callbackWaitLabel is the STATUS label for a chat waiting on trigger, or ""
// when there is no trigger. An unknown trigger still reads as a wait.
func callbackWaitLabel(trigger string) string {
	if trigger == "" {
		return ""
	}
	if label, ok := callbackWaitLabels[models.GitHubCallbackTrigger(trigger)]; ok {
		return label
	}
	return "awaiting " + strings.ReplaceAll(trigger, "_", " ")
}

// armedCallbackFor returns the callback the daemon's reason describes: live,
// unexpired as of now, and recomposing to exactly reason. Ties go to the oldest
// created_at, then the smallest id — callback.ActiveInterestForChat's order.
func armedCallbackFor(reason string, cbs []*pb.GitHubCallback, now time.Time) *pb.GitHubCallback {
	if reason == "" {
		return nil
	}
	var best *pb.GitHubCallback
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

func callbackUnexpired(cb *pb.GitHubCallback, now time.Time) bool {
	return cb.GetExpiresAt() != nil && now.Before(cb.GetExpiresAt().AsTime())
}

func callbackSortsBefore(a, b *pb.GitHubCallback) bool {
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
func (m Model) waitingKeys() []waitKey {
	var keys []waitKey
	for _, chat := range m.chats {
		if id := chat.GetAgentSessionId(); m.isWaitingWithReason(id) {
			keys = append(keys, waitKey{chatID: id, reason: m.daemonWaitingReasons[id]})
		}
	}
	slices.SortFunc(keys, func(a, b waitKey) int { return strings.Compare(a.chatID, b.chatID) })
	return keys
}

func (m Model) isWaitingWithReason(chatID string) bool {
	return m.daemonStatuses[chatID] == ui.StatusWaiting && m.daemonWaitingReasons[chatID] != ""
}

// chatWait classifies what a waiting chat is parked on.
type chatWait int

const (
	// waitNone: the chat is not waiting with a reason.
	waitNone chatWait = iota
	// waitProgress: the chat wakes to act on the PR's progress.
	waitProgress
	// waitStandby: a contingency watch (checks_failed, or closed on a PR other
	// than the session's own). Not what the PR is waiting on (BOS-1531).
	waitStandby
	// waitDone: every wait left is the session's own PR merging or closing.
	// The chat's work is finished; it is parked for post-merge cleanup.
	waitDone
)

// chatWait classifies chatID's wait. A reason that reads as done is confirmed
// against every callback the chat holds: boss-build arms checks_passed,
// checks_failed and merged independently and re-arms only the missing ones, so
// after a push the daemon names the older merged while a newer checks_passed
// still holds the chat. Until the callbacks are listed such a chat is a
// progress wait, so a done is never shown and then taken back.
func (m Model) chatWait(chatID string) chatWait {
	if !m.isWaitingWithReason(chatID) {
		return waitNone
	}
	wait := m.reasonWait(m.daemonWaitingReasons[chatID])
	if wait != waitDone {
		return wait
	}
	if listed, ok := m.listedFor(chatID); !ok || m.progressCallback(listed) != nil {
		return waitProgress
	}
	return waitDone
}

// reasonWait classifies one canonical waiting reason on its own. A merged or
// closed wait on another PR is progress: an orchestrating chat waiting for a
// child PR to merge still has work to do when it fires.
func (m Model) reasonWait(reason string) chatWait {
	trigger := models.GitHubCallbackTrigger(displaystatus.CallbackWaitingTrigger(reason))
	switch {
	case (trigger == models.GitHubCallbackTriggerMerged || trigger == models.GitHubCallbackTriggerClosed) && m.isOwnPRReason(reason):
		return waitDone
	case trigger.IsStandby():
		return waitStandby
	default:
		return waitProgress
	}
}

// isOwnPRReason reports whether a canonical waiting reason ("awaiting <trigger>
// on owner/repo#N") names the session's own PR: the same owner/repo and number.
// GitHub names are case-insensitive. A session that names no repo is matched
// on its PR number alone. A session without a PR owns no PR.
func (m Model) isOwnPRReason(reason string) bool {
	n := int(m.session.GetPrNumber())
	trigger := displaystatus.CallbackWaitingTrigger(reason)
	owner, repo := m.sessionRepo()
	if owner == "" {
		return n > 0 && trigger != "" && strings.HasSuffix(reason, fmt.Sprintf("#%d", n))
	}
	own := displaystatus.CallbackWaitingReason(trigger, owner, repo, n)
	return own != "" && strings.EqualFold(reason, own)
}

// sessionRepo is the GitHub owner and repo of the session's PR, read from its
// pr_url or, failing that, its repo_origin_url. Both are "" when neither names
// one.
func (m Model) sessionRepo() (owner, repo string) {
	nwo := vcs.GitHubNWO(m.session.GetPrUrl())
	if nwo == "" {
		nwo = vcs.GitHubNWO(m.session.GetRepoOriginUrl())
	}
	owner, repo, _ = strings.Cut(nwo, "/")
	return owner, repo
}

// maxPRRefRepoCells bounds the owner/repo part of a cross-repo PR reference in
// the STATUS cell, so a long repo name cannot crowd out the PR number.
const maxPRRefRepoCells = 24

// prRef names the PR a wait on another PR is parked on: "#N" in the session's
// repo (or when the session names no repo), otherwise "owner/repo#N" with the
// repo cut from the left so the number always shows.
func (m Model) prRef(cb *pb.GitHubCallback) string {
	number := fmt.Sprintf("#%d", cb.GetPrNumber())
	owner, repo := m.sessionRepo()
	if owner == "" || (strings.EqualFold(cb.GetRepoOwner(), owner) && strings.EqualFold(cb.GetRepoName(), repo)) {
		return number
	}
	nwo := cb.GetRepoOwner() + "/" + cb.GetRepoName()
	if w := ansi.StringWidth(nwo); w > maxPRRefRepoCells {
		nwo = ansi.TruncateLeft(nwo, w-maxPRRefRepoCells+1, "…")
	}
	return nwo + number
}

// listedCallbacks is one waiting chat's live callbacks as ListGitHubCallbacks
// last returned them, with the daemon reason they were listed under.
type listedCallbacks struct {
	reason string
	live   []*pb.GitHubCallback
}

// listedFor returns chatID's listed callbacks, provided they were listed under
// the daemon's current reason.
func (m Model) listedFor(chatID string) (listedCallbacks, bool) {
	listed, ok := m.armedCallbacks[chatID]
	return listed, ok && listed.reason == m.daemonWaitingReasons[chatID]
}

// progressCallback returns the oldest unexpired callback in listed that is a
// progress wait on its own, or nil. Oldest first mirrors the daemon's reason
// selection.
func (m Model) progressCallback(listed listedCallbacks) *pb.GitHubCallback {
	now := time.Now()
	var best *pb.GitHubCallback
	for _, cb := range listed.live {
		reason := callbackReason(cb)
		if reason == "" || !callbackUnexpired(cb, now) || m.reasonWait(reason) != waitProgress {
			continue
		}
		if best == nil || callbackSortsBefore(cb, best) {
			best = cb
		}
	}
	return best
}

// statusCallback is the callback a progress wait's STATUS cell names: the one
// the daemon's reason describes, or, when that reason is the session's own PR
// merging or closing, the progress callback that keeps the chat waiting.
func (m Model) statusCallback(chat *pb.AgentChat) *pb.GitHubCallback {
	id := chat.GetAgentSessionId()
	if m.reasonWait(m.daemonWaitingReasons[id]) != waitDone {
		return m.armedCallback(chat)
	}
	if listed, ok := m.listedFor(id); ok {
		return m.progressCallback(listed)
	}
	return nil
}

// armedDoneRelist is how often a steady waiting set is re-listed while any of
// its chats reads "done". A newer checks_passed armed under an unchanged reason
// (the daemon still names the older merged wait) changes no waiting key, so
// without this the chat would keep showing a stale done.
const armedDoneRelist = 30 * time.Second

func (m Model) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// showsDone reports whether any waiting chat currently reads "done".
func (m Model) showsDone(keys []waitKey) bool {
	return slices.ContainsFunc(keys, func(k waitKey) bool { return m.chatWait(k.chatID) == waitDone })
}

// syncArmedCallbacks drops armed entries whose chat stopped waiting on that
// reason (the same rule daemonWaitingReasons follows, BOS-668) and returns a
// fetch when the waiting set differs from the one last fetched for, or when a
// chat of a steady set has read "done" for armedDoneRelist.
func (m *Model) syncArmedCallbacks() tea.Cmd {
	keys := m.waitingKeys()
	if len(m.armedCallbacks) > 0 {
		kept := make(map[string]listedCallbacks, len(keys))
		for _, k := range keys {
			if listed, ok := m.armedCallbacks[k.chatID]; ok && listed.reason == k.reason {
				kept[k.chatID] = listed
			}
		}
		m.armedCallbacks = kept
	}
	now := m.clock()
	if slices.Equal(keys, m.armedKeys) && (now.Sub(m.armedFetchedAt) < armedDoneRelist || !m.showsDone(keys)) {
		return nil
	}
	m.armedKeys = keys
	if len(keys) == 0 || m.client == nil {
		return nil
	}
	m.armedFetchedAt = now
	return fetchArmedCallbacks(m.client, m.ctx, keys)
}

// fetchArmedCallbacks lists each waiting chat's live callbacks by target chat
// id off the UI loop. Best effort: a failed list leaves that chat out of the
// result, so both surfaces fall back to the plain waiting text.
func fetchArmedCallbacks(c Client, ctx context.Context, keys []waitKey) tea.Cmd {
	keys = slices.Clone(keys)
	return func() tea.Msg {
		byChat := make(map[string]listedCallbacks, len(keys))
		failed := false
		for _, k := range keys {
			chatID := k.chatID
			cbs, err := c.ListGitHubCallbacks(ctx, &pb.ListGitHubCallbacksRequest{TargetChatId: &chatID})
			if err != nil {
				failed = true
				continue
			}
			listed := listedCallbacks{reason: k.reason}
			for _, cb := range cbs {
				if cb != nil && liveCallbackStates[cb.GetState()] {
					listed.live = append(listed.live, cb)
				}
			}
			byChat[chatID] = listed
		}
		return armedCallbacksMsg{byChat: byChat, keys: keys, failed: failed}
	}
}

// handleArmedCallbacks stores a fetch result only while its keys still match
// the waiting set, so a stale reply cannot attach an old expiry to a new wait.
func (m Model) handleArmedCallbacks(msg armedCallbacksMsg) (tea.Model, tea.Cmd) {
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
func (m Model) armedCallback(chat *pb.AgentChat) *pb.GitHubCallback {
	id := chat.GetAgentSessionId()
	if !m.isWaitingWithReason(id) {
		return nil
	}
	listed, ok := m.listedFor(id)
	if !ok {
		return nil
	}
	return armedCallbackFor(listed.reason, listed.live, time.Now())
}

// armedParkSummary is the verify park the daemon already hydrates onto the
// session, linked to the waiting chat only while the session is NEEDS_HUMAN.
func armedParkSummary(sess *pb.Session) string {
	if sess.GetDisplayStatus() != pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN {
		return ""
	}
	return ui.AttentionWarningHint(sess)
}

// armedLine composes the full, untruncated armed line the launch screen shows
// for chat, or "" when the chat is not waiting with a reason. The park precedes
// the expiry so a truncated line drops the expiry first. A standby reason is
// prefixed "standby: " (never "waiting: "), so the line never presents a
// contingency watch as what the chat is waiting on (BOS-1531).
func (m Model) armedLine(chat *pb.AgentChat) string {
	id := chat.GetAgentSessionId()
	if !m.isWaitingWithReason(id) {
		return ""
	}
	reason := m.daemonWaitingReasons[id]
	switch {
	case m.chatWait(id) == waitStandby:
		reason = displaystatus.StandbyLabel + ": " + reason
	case ui.WaitingBadgeSuperseded(m.session):
		reason = ui.StatusWaiting + ": " + reason
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

// chatDisplayTitle is the title a chat row shows.
func chatDisplayTitle(chat *pb.AgentChat) string {
	if t := chat.GetTitle(); t != "" {
		return t
	}
	return "New chat"
}
