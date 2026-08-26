package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/tmux"
)

// wakeHook lets tests inject fake spawn dependencies into a single *Server
// instance. Stored on the server struct (not in a package-level var) so
// parallel tests with separate *Server values don't trample each other —
// adding t.Parallel() to a wake test is now a safe edit.
//
// Despite the name it is not wake-specific: (*Server).chatSpawnDeps applies it
// to every chat spawn, so the record path (ensureChatTmuxSession) honours the
// same overrides.
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

// ErrHeadlessRunActive is returned by WakeChatInternal when the chat's
// headless run (codex exec / claude --print) is still in progress. Such a
// run reports WORKING in the status tracker but has no tmux session, so
// waking it would spawn a second, tmux-hosted agent on the same worktree
// while the original process is still writing to it. Callers map this to a
// FailedPrecondition so clients retry once the run stops. The wrapped detail
// (see headlessRunActiveMessage) is actionable — it tells the human why the
// attach was refused and where to tail the live run instead — because every
// attach surface (TUI, CLI, web) renders the error string verbatim.
var ErrHeadlessRunActive = errors.New("headless run in progress")

// headlessRunActiveMessage builds the actionable refusal a human sees when they
// attach to a still-running detach run: attaching would duplicate the agent, so
// point them at the live agent log to tail instead. The path mirrors the daemon's
// agent-logs convention configured by the daemon at startup.
func headlessRunActiveMessage(agentLogsDir, agentSessionID string) error {
	return fmt.Errorf(
		"%w: attaching would duplicate the agent — tail the live run instead: %s",
		ErrHeadlessRunActive, bossalog.AgentLogFile(agentLogsDir, agentSessionID),
	)
}

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

	// The singleflight body names its error result so the pane rollback below can
	// observe it from a defer (BOS-845).
	v, err, _ := s.chatWakeGroup.Do(agentSessionID, func() (_ any, err error) {
		chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil || chat == nil {
			return nil, fmt.Errorf("%w: %s", ErrWakeChatNotFound, agentSessionID)
		}
		sess, err := s.sessions.Get(ctx, chat.SessionID)
		if err != nil || sess == nil {
			return nil, fmt.Errorf("%w: session for %s", ErrWakeChatNotFound, agentSessionID)
		}
		tmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)

		deps := s.chatSpawnDeps()

		// Refuse to wake a chat whose headless run (codex exec / claude
		// --print) is still active. A live headless run reports WORKING in
		// the status tracker but owns no tmux session, so spawning here would
		// put a second, tmux-hosted agent on the same worktree concurrently
		// with the original process. Gate only when there is no live tmux
		// session, so waking a genuinely asleep chat (stale/STOPPED status,
		// or one that already has a tmux session) is unaffected.
		if s.chatStatus != nil {
			if e := s.chatStatus.Get(agentSessionID); e != nil && e.Status == pb.ChatStatus_CHAT_STATUS_WORKING {
				if deps.Tmux == nil || !deps.Tmux.HasSession(ctx, tmuxName) {
					return nil, headlessRunActiveMessage(s.agentLogsDir, agentSessionID)
				}
			}
		}

		var legacyAmbiguousReason string
		if !forceFresh {
			// Detached, dedicated budget — the same treatment the record path
			// gives this scan, and for the same reasons. The legacy backfill is a
			// codex rollout scan whose cost scales with the corpus, so inheriting
			// the wake request's deadline is what produced the reported
			// DeadlineExceeded; WithoutCancel keeps the request-scoped values while
			// escaping its cancellation. The eligibility guard (codex, no provider
			// id, non-zero CreatedAt, resolver present) lives inside
			// backfillCodexProviderSessionID, which also runs the persist on a
			// context of its own so a late-but-successful scan still records what
			// it found.
			backfillCtx, cancelBackfill := context.WithTimeout(context.WithoutCancel(ctx), providerSessionIDLegacyBackfillTimeout)
			_, reason, backfillErr := s.backfillCodexProviderSessionID(backfillCtx, chat, sess.WorktreePath, deps.Resolver)
			cancelBackfill()
			// Warn and continue rather than fail the wake: a missed backfill costs
			// a correct resume id, a returned error costs the whole wake. Mirrors
			// ensureChatTmuxSession — before this, the two paths disagreed and only
			// the record one degraded gracefully.
			if backfillErr != nil {
				s.logger.Warn().Err(backfillErr).
					Str("agent_session_id", chat.AgentSessionID).
					Str("agent", chat.AgentName).
					Msg("legacy provider session id discovery failed before wake; continuing without it")
			} else {
				legacyAmbiguousReason = reason
			}
		}

		defaultAccountID := ""
		appendPrompt, promptClasses := session.BuildAppendSystemPrompt(sess, chat.AgentSessionID, chat.AgentName, session.ResolveSubagentGrantForSpawn(s.logger))
		result, err := spawnChatTmux(ctx, deps, spawnInput{
			Chat:                      chat,
			WorktreePath:              sess.WorktreePath,
			TmuxName:                  tmuxName,
			ForceFresh:                forceFresh,
			AppendSystemPrompt:        appendPrompt,
			AppendSystemPromptClasses: promptClasses,
			SessionEnvFunc: func() map[string]string {
				// The bound account's spawn env sits UNDER the managed session env
				// so a persisted account_id gets its credentials materialized on
				// the wake path, and the repo's stored LINEAR_API_KEY / SENTRY_*
				// secrets are filled beneath the worktree .env so the woken chat
				// authenticates to its OWN repo's Linear workspace rather than the
				// daemon's ambient one. Built through chatSpawnEnv, the single
				// named chain DescribeChatMCP also probes under — that shared
				// helper is what keeps a probe from describing a different
				// environment than the chat it claims to describe.
				//
				// This closure runs only after spawnChatTmux has proved a new tmux
				// session is needed, which is what keeps account last-used updates
				// off already-live wakes.
				defaultAccountID = s.defaultAccountIDForChat(ctx, sess, chat)
				return s.chatSpawnEnv(ctx, sess, chat, defaultAccountID, "wake chat", recordAccountUse)
			},
			Model:                  chat.Model,
			SessionAgentName:       sess.AgentName,
			SessionEffectiveEffort: sess.EffectiveEffort,
		})
		if err != nil {
			return nil, err
		}
		// Arm the pane rollback for the row-write window: until the chat row names
		// this pane, an error return would strand a live RemainOnExit pane that no
		// cleanup path can find (BOS-845). OutcomeAlreadyLive is deliberately not
		// armed — that pane belongs to an earlier claim — and spawnChatTmux already
		// rolled back its own failures, so the return above stays unarmed too.
		paneNeedsRollback := result.Outcome != OutcomeAlreadyLive
		defer func() {
			if err != nil && paneNeedsRollback {
				killSpawnedChatPaneBestEffort(ctx, deps.Tmux, s.logger, chat.AgentSessionID, tmuxName)
			}
		}()
		if result.Outcome != OutcomeAlreadyLive {
			s.persistDefaultAccountForChat(ctx, sess, chat, defaultAccountID)
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
		// Only write a provider session id when none is stored. Never overwrite
		// a non-nil id with a re-resolution result: codex ids are guessed from
		// disk, and overwriting on difference is exactly what stamped a sibling
		// chat's id over the correct one (BOS-290). Once bound (via fd
		// resolution or backfill), a chat's id is authoritative.
		if result.ProviderSessionID != "" && chat.ProviderSessionID == nil {
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
		// The row now names the pane, so cleanup can reach it: disarm.
		paneNeedsRollback = false
		if result.Outcome != OutcomeAlreadyLive {
			s.recordInteractiveAgentRunStart(ctx, sess, chat, result.LaunchedAt)
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
	res, ok := v.(wakeResult)
	if !ok {
		return 0, "", "", fmt.Errorf("wake chat: singleflight returned unexpected type %T", v)
	}
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
	case errors.Is(err, ErrHeadlessRunActive):
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
		case errors.Is(err, ErrHeadlessRunActive):
			code = pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION
		}
		return pb.WakeChatResult_OUTCOME_UNSPECIFIED, "", "", code, err
	}
	return outcomeAs[pb.WakeChatResult_Outcome](outcome), tmuxName, reason, pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
}
