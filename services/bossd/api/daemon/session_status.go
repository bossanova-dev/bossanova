package server

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Server) GetSessionStatuses(ctx context.Context, req *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error) {
	if s.chatStatus == nil {
		return connect.NewResponse(&pb.GetSessionStatusesResponse{}), nil
	}

	statuses := make([]*pb.SessionStatusEntry, 0, len(req.Msg.SessionIds))

	for _, sessionID := range req.Msg.SessionIds {
		best, reason, ok := s.chatStatusForSession(ctx, sessionID)
		if !ok {
			continue
		}

		statuses = append(statuses, &pb.SessionStatusEntry{
			SessionId:     sessionID,
			Status:        best,
			WaitingReason: reason,
			Phase:         s.phaseForSession(ctx, sessionID, best),
		})
	}

	return connect.NewResponse(&pb.GetSessionStatusesResponse{Statuses: statuses}), nil
}

// phaseForSession picks the lowest-id working chat with a phase. Attention and
// waiting statuses must not expose a working phase.
func (s *Server) phaseForSession(ctx context.Context, sessionID string, served pb.ChatStatus) string {
	if served != pb.ChatStatus_CHAT_STATUS_WORKING {
		return ""
	}
	chats, err := s.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		return ""
	}
	return s.phaseForChats(chats, served)
}

func (s *Server) phaseForChats(chats []*domain.AgentChat, served pb.ChatStatus) string {
	if served != pb.ChatStatus_CHAT_STATUS_WORKING || s.chatStatus == nil {
		return ""
	}
	var winnerID, phase string
	for _, chat := range chats {
		entry := s.chatStatus.Get(chat.AgentSessionID)
		if entry == nil {
			continue
		}
		resolved, _ := status.PromoteWaiting(entry.Status, s.chatStatus.Waiting(chat.AgentSessionID))
		// No dated API bump: session TUI, CLI and web clients, including
		// version-skewed orchestrator-proxy clients, render phase as opaque
		// display text. A hold label preserves that contract; no transform or
		// version gate is needed. See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
		candidate := s.chatStatus.DisplayPhase(chat.AgentSessionID)
		if resolved == pb.ChatStatus_CHAT_STATUS_WORKING && candidate != "" && (winnerID == "" || chat.AgentSessionID < winnerID) {
			winnerID, phase = chat.AgentSessionID, candidate
		}
	}
	return phase
}

func (s *Server) chatsBySessionForStatuses(ctx context.Context, sessionIDs []string) (map[string][]*domain.AgentChat, bool) {
	if s.chatStatus == nil || s.agentChats == nil {
		return nil, true
	}
	chatsBySession, err := s.agentChats.ListBySessions(ctx, sessionIDs)
	if err != nil {
		s.logger.Warn().Err(err).Msg("list chats for session statuses")
		return nil, false
	}
	return chatsBySession, true
}

func (s *Server) chatStatusForSession(ctx context.Context, sessionID string) (pb.ChatStatus, string, bool) {
	if s.chatStatus == nil || s.agentChats == nil {
		return pb.ChatStatus_CHAT_STATUS_STOPPED, "", true
	}
	chats, err := s.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		s.logger.Warn().Err(err).Str("session_id", sessionID).Msg("list chats for session status")
		return pb.ChatStatus_CHAT_STATUS_STOPPED, "", false
	}
	st, reason, ok, _ := s.chatStatusAndWaitingAggregate(ctx, chats, true)
	return st, reason, ok
}

// applyWaitingMarker promotes a chat status entry from WORKING to WAITING when
// the status tracker holds a waiting reason for it (BOS-668). The promotion rule
// itself lives in status.PromoteWaiting so this RPC, the stream deltas and the
// snapshot reader cannot drift apart; this wrapper only applies the result to a
// protobuf entry.
func applyWaitingMarker(entry *pb.ChatStatusEntry, reason string) {
	if entry == nil {
		return
	}
	entry.Status, entry.WaitingReason = status.PromoteWaiting(entry.GetStatus(), reason)
}

// applyLivenessMarkers projects the poller's spinner-aware liveness reading
// (BOS-805) onto a chat status entry. Called for BOTH kinds of entry
// GetChatStatuses builds — the ones backed by a tracker heartbeat and the
// synthetic ones added for a tmux-alive chat that has none — so a consumer never
// has to guess which flavour it received. A chat the tracker knows nothing about
// simply reports the zero values: no spinner, no substantive timestamp, and
// last_output_seeded=false, which is honest because such an entry carries no
// last_output_at to mislabel in the first place.
func applyLivenessMarkers(entry *pb.ChatStatusEntry, spinnerPresent bool, lastSubstantiveOutputAt time.Time, lastOutputSeeded bool) {
	if entry == nil {
		return
	}
	entry.SpinnerPresent = spinnerPresent
	entry.LastOutputSeeded = lastOutputSeeded
	if !lastSubstantiveOutputAt.IsZero() {
		entry.LastSubstantiveOutputAt = timestamppb.New(lastSubstantiveOutputAt)
	}
}

// sessionChatStatusRank orders chat statuses for the session-level aggregate:
// question > limited > working > waiting > idle > stopped. WAITING sits below
// WORKING on purpose — a session with a genuinely working chat in it is working,
// even if a sibling is parked on a callback — and above IDLE, because a parked
// chat is more informative than one merely sitting at a prompt. Any status not
// listed ranks with STOPPED, matching the pre-BOS-668 fall-through.
func sessionChatStatusRank(st pb.ChatStatus) int {
	switch st {
	case pb.ChatStatus_CHAT_STATUS_QUESTION:
		return 5
	case pb.ChatStatus_CHAT_STATUS_LIMITED:
		return 4
	case pb.ChatStatus_CHAT_STATUS_WORKING:
		return 3
	case pb.ChatStatus_CHAT_STATUS_WAITING:
		return 2
	case pb.ChatStatus_CHAT_STATUS_IDLE:
		return 1
	default:
		return 0
	}
}

// chatStatusAndWaitingAggregate reduces a session's chats to the one status the
// session list renders, plus the waiting reason to surface when that status is
// WAITING. The reduction walks the ordered chats slice rather than the GetBatch
// map so the winning chat — and therefore the reason — is a function of the
// data, never of Go's randomized map iteration.
// It also returns the order-independent status.IdleWaitingAggregate (BOS-1269's
// all-idle rule and the working-over-waiting mark). Only the ListSessions
// display recompute reads that fourth value; every other caller discards it.
//
// A WORKING session surfaces a reason only when every working chat carries an
// armed callback (WorkingOverWaiting), which is when it folded to WAITING
// before; a working chat with no callback means plain live work.
//
// The aggregate is deliberately NOT derived from the winning chat. The winner
// is decided by the lexicographic tie-break below, so a status-shaped carrier
// would make the session's label depend on how its agent-session uuids happened
// to sort. status.IdleWaitingAggregate is a conjunction, which cannot.
func (s *Server) chatStatusAndWaitingAggregate(ctx context.Context, chats []*domain.AgentChat, loaded bool) (pb.ChatStatus, string, bool, status.IdleWaitingAggregate) {
	var waitingIdle status.IdleWaitingAggregate
	if s.chatStatus == nil || s.agentChats == nil {
		return pb.ChatStatus_CHAT_STATUS_STOPPED, "", true, waitingIdle
	}
	if !loaded {
		return pb.ChatStatus_CHAT_STATUS_STOPPED, "", false, waitingIdle
	}
	if len(chats) == 0 {
		return pb.ChatStatus_CHAT_STATUS_STOPPED, "", true, waitingIdle
	}

	agentSessionIDs := make([]string, len(chats))
	for i, c := range chats {
		agentSessionIDs[i] = c.AgentSessionID
	}
	entries := s.chatStatus.GetBatch(agentSessionIDs)

	// Compute best status: question > limited > working > waiting > idle >
	// stopped. LIMITED ranks just below QUESTION, matching displaystatus.Compute
	// and the DisplayStatusComputer aggregation so the session list and chat
	// picker agree.
	best := pb.ChatStatus_CHAT_STATUS_STOPPED
	bestReason := ""
	bestChatID := ""
	for _, c := range chats {
		e, ok := entries[c.AgentSessionID]
		if !ok || e == nil {
			continue
		}
		st, reason := status.PromoteWaiting(e.Status, s.chatStatus.Waiting(c.AgentSessionID))
		// BOS-1269: accumulate the reported status PromoteWaiting is about to
		// discard, in the same loop that already visits every chat.
		waitingIdle.Observe(e.Status, st, reason)
		rank := sessionChatStatusRank(st)
		bestRank := sessionChatStatusRank(best)
		// Strictly better wins; an equal-ranked chat only wins the tie under
		// winsWaitingTie, so several parked chats always yield the same reason.
		if rank > bestRank || (rank == bestRank && bestChatID != "" && winsWaitingTie(reason, c.AgentSessionID, bestReason, bestChatID)) {
			best, bestReason, bestChatID = st, reason, c.AgentSessionID
		}
	}
	// When heartbeats report stopped, fall back to per-chat tmux liveness.
	// Default to IDLE (not WORKING) — the poller will upgrade to WORKING
	// within 3s if Claude is actually producing output.
	if best == pb.ChatStatus_CHAT_STATUS_STOPPED && s.lifecycle != nil {
		for _, c := range chats {
			if c.TmuxSessionName != nil && *c.TmuxSessionName != "" &&
				s.lifecycle.IsTmuxSessionAlive(ctx, *c.TmuxSessionName) {
				best = pb.ChatStatus_CHAT_STATUS_IDLE
				break
			}
		}
	}
	if best == pb.ChatStatus_CHAT_STATUS_WORKING && !waitingIdle.WorkingOverWaiting(best) {
		bestReason = ""
	}
	return best, bestReason, true, waitingIdle
}

// winsWaitingTie reports whether a candidate chat beats the current winner of
// an equal-ranked fold step. A blocking (non-standby) waiting reason beats a
// standby one (displaystatus.IsStandbyWaitingReason: a checks_failed/closed
// contingency watch), so the session names the wait actually holding the PR
// rather than a finished stage's standby watch (BOS-1531); otherwise the
// smaller agent-session id wins. Both keys are total, so the result is
// independent of chat order. Non-WAITING chats carry no reason and fall
// straight through to the id rule.
func winsWaitingTie(reason, chatID, bestReason, bestChatID string) bool {
	if standby, bestStandby := displaystatus.IsStandbyWaitingReason(reason), displaystatus.IsStandbyWaitingReason(bestReason); standby != bestStandby {
		return bestStandby
	}
	return chatID < bestChatID
}

// agentAuthFailedBlockedReason is the stable, machine-matchable blocked_reason
// stamped on a session whose agent pane shows the login-required terminal shape.
// It is the shared sessionreason value, so the daemon's persisted block of an
// auth-failed stranded run (BOS-1530) and this overlay read as one reason.
var agentAuthFailedBlockedReason = sessionreason.AgentAuthFailed()

// agentAuthFailedSummary is the human-readable attention summary for a
// login-required (auth-failed) session.
const agentAuthFailedSummary = "agent not logged in — run /login to re-authenticate"

// agentStalledBlockedReason is the stable, machine-matchable blocked_reason
// stamped on a session whose agent claims to be working but has made no
// transcript progress for longer than its phase's stall threshold (BOS-667).
const agentStalledBlockedReason = "agent-stalled"

// agentStalledSummary is the human-readable attention summary for a stalled
// agent. It deliberately does not name a cause: the marker is raised from
// progress liveness alone, and the observed causes range from a silently
// dropped auth token to a wedged model request.
const agentStalledSummary = "agent reports working but has made no progress — check the pane"
