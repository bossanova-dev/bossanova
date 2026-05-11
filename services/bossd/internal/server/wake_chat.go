package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/tmux"
)

// wakeHook lets tests inject fake spawn dependencies into a single *Server
// instance. Stored on the server struct (not in a package-level var) so
// parallel tests with separate *Server values don't trample each other —
// adding t.Parallel() to a wake test is now a safe edit.
type wakeHook struct {
	spawner     tmuxSpawner
	transcripts transcriptOracle
	argv        argvBuilder
	resolver    interactiveSessionResolver
}

// ErrWakeChatNotFound is returned by WakeChatInternal when the named chat
// (or its owning session) cannot be found. Callers map this to whichever
// transport-specific not-found code makes sense (CodeNotFound for connect,
// "chat not found" string for the stream CommandResult).
var ErrWakeChatNotFound = errors.New("chat not found")

// WakeChatInternal is the transport-agnostic body of the WakeChat RPC.
// Both the connect handler (browser/CLI direct path) and the stream
// dispatcher (browser → bosso → stream → daemon) call it. Returns the
// resolved Outcome, the persisted tmux session name, and an error.
//
// Errors are plain Go errors, not connect errors — callers translate.
//
//   - ErrWakeChatNotFound       chat or session missing
//   - ErrWorktreeMissing        worktree directory deleted out of band
//   - any other error           spawn failure (claude binary missing, tmux exec, etc.)
func (s *Server) WakeChatInternal(ctx context.Context, agentSessionID string, forceFresh bool) (Outcome, string, string, error) {
	if agentSessionID == "" {
		return 0, "", "", errors.New("agent_session_id is required")
	}

	type wakeResult struct {
		outcome  Outcome
		tmuxName string
		reason   string
	}

	v, err, _ := s.chatWakeGroup.Do(agentSessionID, func() (any, error) {
		chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil || chat == nil {
			return nil, fmt.Errorf("%w: %s", ErrWakeChatNotFound, agentSessionID)
		}
		sess, err := s.sessions.Get(ctx, chat.SessionID)
		if err != nil || sess == nil {
			return nil, fmt.Errorf("%w: session for %s", ErrWakeChatNotFound, agentSessionID)
		}
		tmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)

		deps := spawnDeps{
			Transcripts: liveTranscriptOracle{clients: s.agentClients},
			Argv:        liveArgvBuilder{clients: s.agentClients},
		}
		if s.tmux != nil {
			deps.Tmux = liveTmuxSpawner{c: s.tmux}
		}
		if s.agentClients != nil {
			deps.Resolver = liveInteractiveSessionResolver{clients: s.agentClients}
		}
		if s.wakeHook.spawner != nil {
			deps.Tmux = s.wakeHook.spawner
		}
		if s.wakeHook.transcripts != nil {
			deps.Transcripts = s.wakeHook.transcripts
		}
		if s.wakeHook.argv != nil {
			deps.Argv = s.wakeHook.argv
		}
		if s.wakeHook.resolver != nil {
			deps.Resolver = s.wakeHook.resolver
		}

		var legacyAmbiguousReason string
		if !forceFresh && chat.AgentName == "codex" && chat.ProviderSessionID == nil && !chat.CreatedAt.IsZero() && deps.Resolver != nil {
			legacyLaunchedAfter := chat.CreatedAt.Add(-5 * time.Minute)
			resolution, err := deps.Resolver.ResolveInteractiveSessionID(ctx, chat.AgentName, sess.WorktreePath, chat.AgentSessionID, legacyLaunchedAfter, chat.CreatedAt, true)
			if err != nil {
				return nil, err
			}
			if resolution.SessionID != "" {
				providerID := resolution.SessionID
				if err := s.agentChats.UpdateProviderSessionID(ctx, chat.AgentSessionID, &providerID); err != nil {
					return nil, fmt.Errorf("persist provider session id: %w", err)
				}
				chat.ProviderSessionID = &providerID
			} else if resolution.Ambiguous {
				legacyAmbiguousReason = resolution.Reason
			}
		}

		result, err := spawnChatTmux(ctx, deps, spawnInput{
			Chat:         chat,
			WorktreePath: sess.WorktreePath,
			TmuxName:     tmuxName,
			ForceFresh:   forceFresh,
		})
		if err != nil {
			return nil, err
		}
		outcome := result.Outcome
		reason := result.FallbackReason

		canUseLegacyAmbiguous := result.FallbackReason == "" || result.FallbackReason == WakeFallbackReasonProviderIDMissing
		if result.Outcome == OutcomeFreshFallback && result.ProviderSessionID == "" && legacyAmbiguousReason != "" && canUseLegacyAmbiguous && !result.DiscoveryAmbiguous && result.DiscoveryReason == "" {
			reason = WakeFallbackReasonLegacyProviderIDDiscoveryAmbiguous
			result.FallbackReason = reason
			result.DiscoveryAmbiguous = true
			result.DiscoveryReason = legacyAmbiguousReason
		}

		if result.Outcome == OutcomeFreshFallback && result.FallbackReason != "" {
			providerSessionID := result.ProviderSessionID
			if chat.ProviderSessionID != nil {
				providerSessionID = *chat.ProviderSessionID
			}
			ev := s.logger.Warn().
				Str("reason", result.FallbackReason).
				Str("agent_session_id", chat.AgentSessionID).
				Str("provider_session_id", providerSessionID).
				Str("agent_name", chat.AgentName).
				Str("worktree", sess.WorktreePath).
				Str("tmux_session", tmuxName)
			if result.DiscoveryAmbiguous {
				ev = ev.Bool("ambiguous", true)
			}
			if result.DiscoveryReason != "" {
				ev = ev.Str("discovery_reason", result.DiscoveryReason)
			}
			ev.Msg("wake chat fresh fallback")
		}
		if result.ProviderSessionID != "" && (chat.ProviderSessionID == nil || *chat.ProviderSessionID != result.ProviderSessionID) {
			providerID := result.ProviderSessionID
			if err := s.agentChats.UpdateProviderSessionID(ctx, chat.AgentSessionID, &providerID); err != nil {
				return nil, fmt.Errorf("persist provider session id: %w", err)
			}
			chat.ProviderSessionID = &providerID
		}

		// Persist the tmux name so list/kill paths see the live session.
		if chat.TmuxSessionName == nil || *chat.TmuxSessionName != tmuxName {
			if err := s.agentChats.UpdateTmuxSessionName(ctx, chat.AgentSessionID, &tmuxName); err != nil {
				return nil, fmt.Errorf("persist tmux name: %w", err)
			}
		}

		// Register with the poller so the next status snapshot reflects
		// the revived session immediately rather than waiting for the
		// next bootstrap cycle to discover it.
		if s.tmuxPoller != nil {
			s.tmuxPoller.RegisterChat(chat.AgentSessionID)
		}

		return wakeResult{outcome: outcome, tmuxName: tmuxName, reason: reason}, nil
	})
	if err != nil {
		return 0, "", "", err
	}
	res := v.(wakeResult)
	return res.outcome, res.tmuxName, res.reason, nil
}

// WakeChat brings a chat's tmux+agent (claude, codex, …) back to life.
// Connect entrypoint; see WakeChatInternal for the actual logic.
func (s *Server) WakeChat(ctx context.Context, req *connect.Request[pb.WakeChatRequest]) (*connect.Response[pb.WakeChatResponse], error) {
	outcome, tmuxName, reason, err := s.WakeChatInternal(ctx, req.Msg.GetAgentSessionId(), req.Msg.GetForceFresh())
	if err != nil {
		return nil, wakeChatErrorToConnect(err)
	}
	return connect.NewResponse(&pb.WakeChatResponse{
		Outcome:         outcomeAs[pb.WakeChatResponse_Outcome](outcome),
		TmuxSessionName: tmuxName,
		Reason:          reason,
	}), nil
}

// wakeChatErrorToConnect maps WakeChatInternal errors to connect codes.
func wakeChatErrorToConnect(err error) error {
	switch {
	case errors.Is(err, ErrWakeChatNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, ErrWorktreeMissing):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		// Special-case the empty agent_session_id check so the connect
		// handler returns CodeInvalidArgument instead of CodeInternal.
		if err.Error() == "agent_session_id is required" {
			return connect.NewError(connect.CodeInvalidArgument, err)
		}
		return connect.NewError(connect.CodeInternal, fmt.Errorf("spawn: %w", err))
	}
}

// wakeOutcomeEnum is satisfied by the two proto enum types that mirror the
// internal Outcome ordering: WakeChatResponse_Outcome (the connect RPC
// response) and WakeChatResult_Outcome (the reverse-stream payload). Both
// share the same wire numeric values (0..3); the generic mapper below
// relies on that invariant — if either enum is reordered or extended, the
// existing wake_chat_test cases catch it via the proto-level assertions.
type wakeOutcomeEnum interface {
	~int32
	pb.WakeChatResponse_Outcome | pb.WakeChatResult_Outcome
}

// outcomeAs converts the internal Outcome to whichever proto enum the
// caller wants, replacing the two near-identical switch statements that
// existed before.
func outcomeAs[T wakeOutcomeEnum](o Outcome) T {
	switch o {
	case OutcomeAlreadyLive:
		return T(1)
	case OutcomeResumed:
		return T(2)
	case OutcomeFreshFallback:
		return T(3)
	default:
		return T(0)
	}
}

// WakeChatStream is the reverse-stream entrypoint that satisfies
// upstream.ChatWaker. Returns proto-level enums directly so the upstream
// package can build a CommandResult payload without importing this
// package's internal Outcome type. The returned errorCode classifies the
// failure for the dispatcher (NOT_FOUND / FAILED_PRECONDITION / unspecified)
// so bosso can map back to the right ConnectRPC code without inspecting
// the human-readable error string.
func (s *Server) WakeChatStream(ctx context.Context, agentSessionID string, forceFresh bool) (pb.WakeChatResult_Outcome, string, string, pb.CommandResult_ErrorCode, error) {
	outcome, tmuxName, reason, err := s.WakeChatInternal(ctx, agentSessionID, forceFresh)
	if err != nil {
		code := pb.CommandResult_ERROR_CODE_UNSPECIFIED
		switch {
		case errors.Is(err, ErrWakeChatNotFound):
			code = pb.CommandResult_ERROR_CODE_NOT_FOUND
		case errors.Is(err, ErrWorktreeMissing):
			code = pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION
		}
		return pb.WakeChatResult_OUTCOME_UNSPECIFIED, "", "", code, err
	}
	return outcomeAs[pb.WakeChatResult_Outcome](outcome), tmuxName, reason, pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
}
