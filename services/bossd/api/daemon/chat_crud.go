package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

func (s *Server) ListChats(ctx context.Context, req *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	if req.Msg.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}

	chats, err := s.agentChats.ListBySession(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list chats: %w", err))
	}

	pbChats := make([]*pb.AgentChat, len(chats))
	for i, c := range chats {
		pbChats[i] = agentChatToProto(c)
	}

	return connect.NewResponse(&pb.ListChatsResponse{Chats: pbChats}), nil
}

func (s *Server) UpdateChatTitle(ctx context.Context, req *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error) {
	msg := req.Msg
	if msg.AgentSessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}
	if msg.Title == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title is required"))
	}

	if err := s.agentChats.UpdateTitleByAgentSessionID(ctx, msg.AgentSessionId, msg.Title); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update chat title: %w", err))
	}

	return connect.NewResponse(&pb.UpdateChatTitleResponse{}), nil
}

func (s *Server) killChatTmuxSession(ctx context.Context, chat *domain.AgentChat) {
	if chat == nil || chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" || s.chatTmux == nil {
		return
	}

	tmuxName := *chat.TmuxSessionName
	if err := s.chatTmux.KillSession(ctx, tmuxName); err != nil {
		s.logger.Warn().Err(err).
			Str("session", chat.SessionID).
			Str("agentSessionID", chat.AgentSessionID).
			Str("tmuxSession", tmuxName).
			Msg("failed to kill chat tmux session during delete")
	}
}

func isAgentChatNotFound(err error) bool {
	return errors.Is(err, domain.ErrNotFound)
}

// deleteChatOutcome names the path a DeleteChat attempt terminated on. The
// audit line owes one record per ATTEMPT (AC5), not one per completed delete,
// and the `refused bool` this replaces could express only a single outcome —
// "the cleanup gate fired" — hardcoded into its own message. That left the
// three earlier returns in DeleteChat with no audit trace whatsoever. The
// session-scope mismatch is the one that matters: it is an authz denial, and an
// unrecorded authz denial is exactly the forensic void BOS-1299 exists to close.
type deleteChatOutcome string

const (
	// deleteChatOutcomeAllowed is the only outcome that proceeds to delete.
	deleteChatOutcomeAllowed deleteChatOutcome = "allowed"
	// deleteChatOutcomeInvalidArgument: the request named no chat at all.
	deleteChatOutcomeInvalidArgument deleteChatOutcome = "invalid-argument"
	// deleteChatOutcomeLookupFailed: the chat store could not answer.
	deleteChatOutcomeLookupFailed deleteChatOutcome = "lookup-failed"
	// deleteChatOutcomeCrossSession: the chat resolves, but to a session other
	// than the one the caller was authorized for. An authz denial.
	deleteChatOutcomeCrossSession deleteChatOutcome = "cross-session"
	// deleteChatOutcomeRefusedCrossAgent: the transcript-absent cleanup gate
	// fired against a chat recorded as another provider's.
	deleteChatOutcomeRefusedCrossAgent deleteChatOutcome = "refused-cleanup-cross-agent"
)

// auditMessage is the human half of the audit line. The machine half is the
// outcome field, so these strings stay descriptions rather than identifiers —
// grep the outcome, read the message.
func (o deleteChatOutcome) auditMessage() string {
	switch o {
	case deleteChatOutcomeInvalidArgument:
		return "delete chat rejected: agent_session_id is required"
	case deleteChatOutcomeLookupFailed:
		return "delete chat failed: chat lookup error"
	case deleteChatOutcomeCrossSession:
		return "delete chat denied: chat does not belong to the requested session"
	case deleteChatOutcomeRefusedCrossAgent:
		return "delete chat refused: cleanup reap of a non-claude chat"
	default:
		return "delete chat"
	}
}

// logDeleteChatAudit writes the one structured line every deletion attempt
// leaves behind. Before it existed, a destroyed chat had to be reconstructed
// from tmux reaper logs: the deletion itself recorded neither who asked nor
// why (BOS-1299).
//
// Redaction contract: identifiers, the recorded agent and the reason only.
// Never the chat title, never transcript content, never credentials.
//
// The reason is logged as its proto name rather than its integer, matching
// rotation/audit.go, so the line stays readable when the enum grows. Auditing
// must never fail the operation — this is a synchronous, unconditional,
// error-free call, and a Server built without a logger (zero zerolog.Logger)
// makes it a no-op rather than a panic.
func (s *Server) logDeleteChatAudit(msg *pb.DeleteChatRequest, chat *domain.AgentChat, outcome deleteChatOutcome) {
	ev := s.logger.Info()
	if outcome != deleteChatOutcomeAllowed {
		// Level is part of the contract: a quiet refusal is indistinguishable
		// from a successful no-op, which is the whole failure this replaces.
		// The rule is deliberately a single rule — Info only where the delete
		// actually proceeded, Warn on every path that ended the attempt early —
		// because "did this deletion happen?" is the question a reader of these
		// lines has, and a per-outcome level ladder answers a different one.
		// The outcome field carries the finer distinction, greppably.
		ev = s.logger.Warn()
	}

	sessionID := msg.GetSessionId()
	agentName := ""
	if chat != nil {
		sessionID = chat.SessionID
		agentName = chat.AgentName
	}

	ev = ev.
		Str("session_id", sessionID).
		Str("agent_session_id", msg.GetAgentSessionId()).
		Str("agent_name", agentName).
		Str("reason", msg.GetReason().String()).
		Str("outcome", string(outcome))
	// On the cross-session denial the owning session and the requested one
	// disagree, and WHICH session was asked for is the entire content of the
	// denial. Emitted only when the two differ, so every other line keeps the
	// shape it already had.
	if requested := msg.GetSessionId(); requested != "" && requested != sessionID {
		ev = ev.Str("requested_session_id", requested)
	}
	if chat == nil {
		ev = ev.Bool("chat_absent", true)
	} else if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
		ev = ev.Str("tmux_session", *chat.TmuxSessionName)
	}

	// No Err() on the lookup-failure path: the redaction contract above is a
	// CLOSED field set, and a store error string is not one of its members. The
	// error still reaches the caller as the RPC error.
	ev.Msg(outcome.auditMessage())
}

func (s *Server) DeleteChat(ctx context.Context, req *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	if req.Msg.AgentSessionId == "" {
		s.logDeleteChatAudit(req.Msg, nil, deleteChatOutcomeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}

	chat, err := s.agentChats.GetByAgentSessionID(ctx, req.Msg.AgentSessionId)
	if err != nil && !isAgentChatNotFound(err) {
		s.logDeleteChatAudit(req.Msg, nil, deleteChatOutcomeLookupFailed)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get chat: %w", err))
	}
	// Enforce session scoping when the caller provided a session_id: the chat
	// must belong to that session. This makes bosso's session-level authz
	// (it routes by the session's owning daemon) actually binding rather than
	// advisory — without it the daemon would delete any chat with this
	// agent_session_id regardless of which session was authorized. A nil chat
	// is left to the idempotent no-op delete below.
	if req.Msg.SessionId != "" && chat != nil && chat.SessionID != req.Msg.SessionId {
		// Audited before the return: this is the authz denial, and an authz
		// denial that leaves no record is the forensic void this ticket closes.
		s.logDeleteChatAudit(req.Msg, chat, deleteChatOutcomeCrossSession)
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat does not belong to session %q", req.Msg.SessionId))
	}

	// Fail closed on a cleanup reap aimed at another provider's chat. The
	// CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT reason means the caller found no
	// *Claude* transcript for this chat — a provider-specific, absence-based
	// inference. Letting it authorise an irreversible delete against a Codex
	// chat is what destroyed one (#2617 fixed the TUI side; this backstops any
	// other caller whose reason reaches the daemon intact). Scope: that means
	// LOCAL callers only. ProxyDeleteChatRequest carries no reason field, so a
	// remote cleanup reap is re-stamped USER_REQUESTED by CommandHandlerAdapter
	// and is NOT gated here — see the note on RemoteClient.DeleteChat.
	//
	// The gate keys on the explicitly-set reap reason, never on "not one of the
	// permitted values": every other reason, including the zero value every
	// pre-existing caller sends, deletes exactly as before. That is what keeps
	// this change free of a dated API bump — the refusal is unreachable for a
	// client that cannot set the field. If a future change ever makes this fire
	// on UNSPECIFIED, that argument collapses and a dated bump plus an
	// ErrorTransform becomes owed.
	//
	// A nil chat is left to the idempotent no-op delete below: there is no row
	// to protect, so refusing would convert a harmless no-op into an error.
	//
	// Normalising the recorded agent through accountAgentName is load-bearing,
	// not cosmetic. An empty agent_chats.agent_name is a row that predates the
	// column (the SQLite column itself defaults to "claude"), so it MEANS
	// claude rather than "unknown" — the same ""→claude fall-through
	// spawn_chat_domain.go applies in liveInteractiveSessionResolver,
	// liveTranscriptOracle and liveArgvBuilder. A raw
	// `chat.AgentName != defaultLegacyAgent` comparison looks correct and
	// passes every gate test that supplies an explicit agent, but it refuses
	// the reap for every legacy row — turning a safety gate into a leak that
	// strands those rows forever.
	refused := chat != nil &&
		req.Msg.GetReason() == pb.DeleteChatRequest_DELETION_REASON_CLEANUP_LOCAL_CLAUDE_TRANSCRIPT_ABSENT &&
		accountAgentName(chat.AgentName) != defaultLegacyAgent

	outcome := deleteChatOutcomeAllowed
	if refused {
		outcome = deleteChatOutcomeRefusedCrossAgent
	}
	s.logDeleteChatAudit(req.Msg, chat, outcome)

	if refused {
		// Return before killChatTmuxSession. Preserving the row while killing
		// its pane would leave a still-pointed row with a dead pane, which the
		// session guard misreads as "the agent exited, finalize".
		//
		// FailedPrecondition, not NotFound: the row is intact and resolvable,
		// and the caller asked for something not permitted in this state.
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"refusing %s of chat recorded as agent %q: only a claude chat may be reaped on an absent claude transcript",
			req.Msg.GetReason().String(), chat.AgentName))
	}

	s.killChatTmuxSession(ctx, chat)

	// Clear cached status while the chat row is still present. The status
	// tracker's auth-change hook resolves agent_session_id -> session through the
	// chat store; doing this after deletion would prevent a reverse-stream clear
	// delta for a prior AGENT_AUTH_FAILED overlay.
	if s.chatStatus != nil && chat != nil {
		s.chatStatus.Remove(req.Msg.AgentSessionId)
	}

	if err := s.agentChats.DeleteByAgentSessionID(ctx, req.Msg.AgentSessionId); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete chat: %w", err))
	}

	// Also clean up any cached status for idempotent deletes where the chat row
	// was already gone before this request.
	if s.chatStatus != nil && chat == nil {
		s.chatStatus.Remove(req.Msg.AgentSessionId)
	}

	return connect.NewResponse(&pb.DeleteChatResponse{}), nil
}

// --- Chat Status ---

// SetChatPhase records the ephemeral stage a skill reports for its own chat.
