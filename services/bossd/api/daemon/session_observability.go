package server

import (
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/contract/daemon/alertstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// latestAgentActivity returns the newest captured-pane activity time across the
// session's chats, read from the status tracker's LastOutputAt. LastOutputAt
// advances ONLY on captured-pane CONTENT changes (see tmux_poller's
// captureChanged guard), NOT pipe-pane mtime, so a zombie pane with a fresh
// mtime but static content does not advance it; STOPPED chats are skipped
// below. ok is false when no chat has an observed activity time (leave
// last_agent_activity_at absent).
//
// Caveat: LastOutputAt reflects any pane-content change, which includes a
// submit=false composer prefill (typed/pasted input the agent has not yet
// consumed), not only agent-produced output. The primary recovery consumer
// (boss-epic) only ever submits (never prefills the chats it monitors), so this
// does not affect its stale-vs-live signal. Excluding composer-only edits needs
// composer-region-aware diffing in the shared status poller (which also feeds
// get_chat_statuses) and is tracked as a follow-up, not scoped to BOS-242.
func latestAgentActivity(tracker *status.Tracker, chats []*domain.AgentChat) (time.Time, bool) {
	if tracker == nil {
		return time.Time{}, false
	}
	var latest time.Time
	for _, chat := range chats {
		e := tracker.Get(chat.AgentSessionID)
		if e == nil {
			continue
		}
		// A STOPPED chat's LastOutputAt is stamped to now by the poller when its
		// tmux session disappears (tmux_poller: Update(..., STOPPED, now)) — that
		// is a liveness heartbeat for the display label, NOT genuine agent output.
		// Counting it would make a dead/crashed chat advance last_agent_activity_at
		// on every poll and read as freshly active, defeating the stale-vs-live
		// signal this field exists to provide. Skip stopped entries.
		if e.Status == pb.ChatStatus_CHAT_STATUS_STOPPED {
			continue
		}
		if e.LastOutputAt.After(latest) {
			latest = e.LastOutputAt
		}
	}
	if latest.IsZero() {
		return time.Time{}, false
	}
	return latest, true
}

// sessionAuthFailed reports whether any of the session's chats currently shows
// the login-required terminal shape (a fresh, debounced auth-failed marker in
// the tracker).
func sessionAuthFailed(tracker *status.Tracker, chats []*domain.AgentChat) bool {
	if tracker == nil {
		return false
	}
	for _, chat := range chats {
		if tracker.AuthFailed(chat.AgentSessionID) {
			return true
		}
	}
	return false
}

func sessionAuthInvalidationCorroborated(tracker *status.Tracker, p *pb.Session, chats []*domain.AgentChat) bool {
	if tracker == nil || p == nil {
		return false
	}
	failedChats := make(map[string]time.Time)
	for _, chat := range chats {
		if since, ok := tracker.AuthFailedSince(chat.AgentSessionID); ok {
			failedChats[chat.AgentSessionID] = rotationEventComparableTime(since)
		}
	}
	if len(failedChats) == 0 {
		return false
	}
	seenChat := make(map[string]struct{})
	for _, ev := range p.GetRotationEvents() {
		if ev.GetTrigger() != pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED {
			continue
		}
		since, ok := failedChats[ev.GetChatId()]
		if !ok {
			continue
		}
		if _, seen := seenChat[ev.GetChatId()]; seen {
			continue
		}
		if ev.GetCreatedAt() == nil || ev.GetCreatedAt().AsTime().Before(since) {
			continue
		}
		seenChat[ev.GetChatId()] = struct{}{}
		if ev.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED &&
			ev.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_DISABLED {
			return true
		}
	}
	return false
}

func rotationEventComparableTime(t time.Time) time.Time {
	return sqlutil.ParseTime(sqlutil.FormatTime(t))
}

// sessionAgentStalled reports whether any of the session's chats currently
// carries a fresh agent-stalled marker (BOS-667). Like AuthFailed the marker
// self-heals: the tracker treats one older than status.StaleThreshold as absent,
// so a poller that stopped ticking cannot leave a permanent banner behind.
func sessionAgentStalled(tracker *status.Tracker, chats []*domain.AgentChat) bool {
	if tracker == nil {
		return false
	}
	for _, chat := range chats {
		if tracker.Stalled(chat.AgentSessionID) {
			return true
		}
	}
	return false
}

// HydrateBaseAttention populates attention_status from the session's VCS state
// for the blocked/orphaned reasons that would otherwise be clobbered by the
// AGENT_AUTH_FAILED overlay on the reverse stream.
//
// The reverse-stream projection (cmd/main.go) builds its Session protos straight
// from SessionToProto, which carries the persisted blocked_reason but never
// attention_status. HydrateAgentObservability only overlays AGENT_AUTH_FAILED
// when attention_status is nil, so without applying the base attention first a
// blocked/orphaned session that ALSO has an auth-failed chat would have its real
// blocked_reason overwritten by the low-priority auth overlay — and bosso's
// full-replacement read model would lose the actionable failure reason. Call
// this before HydrateAgentObservability on every reverse-stream session. repo
// may be nil (no-op); best-effort enrichment, never a hard dependency.
//
// Scope is deliberately limited to Blocked/Orphaned. ComputeAttentionStatus also
// emits ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE for a FixingChecks session
// in a CanAutoRepair=false repo, but that reason is only correct while the
// display tracker still reads DISPLAY_STATUS_CONFLICT: the local ListSessions
// path clears it via suppressStaleConflictAttention once the tracker moves out
// of conflict. Reverse-stream protos carry no hydrated display status to run
// that suppression against, so emitting the conflict reason here would resurrect
// a stale conflict attention AND (being non-nil) block the auth overlay. Merge
// conflicts already reach the read model through the display-status path, so
// they need no base-attention hydration here.
func HydrateBaseAttention(p *pb.Session, session *models.Session, repo *models.Repo, display *status.DisplayEntry) {
	if p == nil || session == nil || repo == nil {
		return
	}
	switch session.State {
	case machine.Blocked, machine.Orphaned:
		p.AttentionStatus = baseAttentionStatus(session, repo, display)
	default:
		// All other states (including FixingChecks) carry no base attention on the
		// reverse stream; see the doc comment above for why merge-conflict attention
		// is deliberately excluded.
	}
}

// baseAttentionStatus is the legacy attention_status for sess. A Blocked
// session whose alert has ended or is suspended (alertstate.BlockedAlertCurrent,
// the rule Session.attention follows: its run's agent is working, or its PR is
// green and mergeable) carries none, so no client keeps a blocked hint under a
// row that does not need the operator. display is the last-polled entry, nil
// when never polled. apiversion.EndedBlockHintChange gives pins before
// V20260923 the hint back.
func baseAttentionStatus(sess *models.Session, repo *models.Repo, display *status.DisplayEntry) *pb.AttentionStatus {
	if sess.State == machine.Blocked && !alertstate.BlockedAlertCurrent(session.BlockedAlertFacts(sess, display)) {
		return nil
	}
	return attentionStatusToProto(vcs.ComputeAttentionStatus(sess, repo))
}

// HydrateAgentObservability sets the liveness heartbeat (last_agent_activity_at)
// from real agent output and, when the agent pane persistently shows the
// login-required shape, rotation audit corroborates an invalidated auth signal,
// AND the session has no other attention reason, overlays the AGENT_AUTH_FAILED
// attention reason plus a stable blocked_reason.
//
// It is a standalone (tracker-parameterized) function, not a Server method, so
// BOTH the local GetSession/ListSessions RPC read paths AND the reverse-stream
// snapshot/delta projection (wired in cmd/main.go) produce identical
// observability fields. The cloud/web read model is fed ONLY by the reverse
// stream, so without applying this there it never sees last_agent_activity_at or
// AGENT_AUTH_FAILED. Because bosso applies session deltas as full replacements,
// every reverse-stream session UPDATED delta must carry this overlay too, or an
// unrelated recompute would clobber a live AGENT_AUTH_FAILED back off.
//
// It also overlays ATTENTION_REASON_AGENT_STALLED (BOS-667) on the same terms,
// ranked just below auth — see the switch in the body.
//
// Priority: auth is the LOWEST-priority reason — it is overlaid only where
// ComputeAttentionStatus returned no attention. A session already Blocked or
// Orphaned keeps its own reason untouched. This is also what makes the
// down-convert faithful: AGENT_AUTH_FAILED fires exactly where there was
// previously no attention, so neutralizing it for older clients restores the
// prior "just went quiet" behavior. Fails toward NOT flagging: no marker → no
// change. Fails toward NOT flagging: no marker or no corroborating rotation
// audit row for the currently auth-failed chat -> no change.
func HydrateAgentObservability(tracker *status.Tracker, p *pb.Session, chats []*domain.AgentChat) {
	HydrateAgentObservabilityWithAuthCorroboration(tracker, p, chats, false)
}

func HydrateAgentObservabilityWithAuthCorroboration(tracker *status.Tracker, p *pb.Session, chats []*domain.AgentChat, authInvalidationCorroborated bool) {
	if p == nil || len(chats) == 0 {
		return
	}
	if latest, ok := latestAgentActivity(tracker, chats); ok {
		p.LastAgentActivityAt = timestamppb.New(latest)
	}
	if p.GetAttentionStatus() != nil {
		return // keep an existing, unrelated attention reason
	}
	// Auth outranks stalled: an auth-failed pane IS a stalled agent, but only the
	// auth reason names the fix (/login). Raising the vaguer AGENT_STALLED over it
	// would replace an actionable banner with a symptom.
	var reason pb.AttentionReason
	blockedReason, summary := "", ""
	switch {
	case sessionAuthFailed(tracker, chats) && (authInvalidationCorroborated || sessionAuthInvalidationCorroborated(tracker, p, chats)):
		reason, blockedReason, summary = pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED, agentAuthFailedBlockedReason, agentAuthFailedSummary
	case sessionAgentStalled(tracker, chats):
		reason, blockedReason, summary = pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED, agentStalledBlockedReason, agentStalledSummary
	default:
		return
	}
	p.BlockedReason = &blockedReason
	p.AttentionStatus = &pb.AttentionStatus{
		NeedsAttention: true,
		Reason:         reason,
		Summary:        summary,
		Since:          p.GetUpdatedAt(),
	}
}
