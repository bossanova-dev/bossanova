package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

// --- Claude Chat Tracking ---

// recordChatRole is the role a new RecordChat row gets. A caller that names no
// role is a person, or a tool acting for one, so the chat is the operator's.
func recordChatRole(r pb.ChatRole) domain.AgentChatRole {
	if role := agentChatRoleFromProto(r); role != domain.AgentChatRoleUnknown {
		return role
	}
	return domain.AgentChatRoleOperator
}

// errResumeChatGone refuses a resume of a chat that has no row, no live pane
// and no saved conversation, usually one deleted after the caller listed it.
// Recording it would start a brand-new agent under the old id; a caller that
// wants a new chat records one with resume unset.
//
// No dated apiversion bump, and why: RecordChat's resume callers are the boss
// TUI's attach and `boss chat` (through the local client, co-deployed with
// bossd, or through bosso's ProxyRecordChat when the client is remote, which
// is version-skewed), the local MCP socket backend (released with bossd), and
// the hosted MCP gateway through bosso (version-skewed). The web only records
// new chats (resume unset), so it never reaches this refusal. Every caller
// already surfaces RecordChat's NotFound, which a chat under another session
// gets. The old answer created that brand-new chat, which is the defect, so no
// down-convert or handler gate could restore it for an older client without
// restoring the defect. Re-adopting a live pane is the answer every client got
// before the refusal existed. See
// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
var errResumeChatGone = errors.New("chat no longer exists")

// resumable reports whether a chat with no row can still be resumed: its tmux
// pane is still live (a DeleteChat whose kill failed leaves one) or it has a
// saved conversation, as when a lost chat is recorded again by its id.
func (s *Server) resumable(ctx context.Context, agentName, sessionID, agentSessionID string) bool {
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil || sess == nil {
		return false
	}
	deps := s.chatSpawnDeps()
	if deps.Tmux != nil && deps.Tmux.Available(ctx) && deps.Tmux.HasSession(ctx, domain.ChatSessionName(sess.RepoID, agentSessionID)) {
		return true
	}
	return deps.Transcripts.TranscriptExists(ctx, agentName, sess.WorktreePath, agentSessionID)
}

func (s *Server) RecordChat(ctx context.Context, req *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	msg := req.Msg
	if msg.SessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}
	if msg.AgentSessionId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("agent_session_id is required"))
	}
	// The agent-independent half of the shape check runs before the lookup;
	// the per-agent half needs the agent, so it runs before the insert below.
	if err := validateAgentSessionID("", msg.AgentSessionId); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Idempotent: reuse the existing row if one already exists for this
	// agent_session_id (resume case), otherwise insert a new one.
	chat, err := s.agentChats.GetByAgentSessionID(ctx, msg.AgentSessionId)
	if err == nil && chat != nil && chat.SessionID != msg.SessionId {
		// The chat exists but under a different session than the caller
		// authorized. Reject rather than resume it into the wrong session —
		// the bosso proxy routes by the session's owning daemon, so a
		// mismatch here means the requested session_id never owned this chat.
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat does not belong to session %q", msg.SessionId))
	}
	if err == nil && chat != nil {
		agentName := chat.AgentName
		if agentName == "" {
			agentName = "claude"
		}
		if err := validateAgentSessionID(agentName, msg.AgentSessionId); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	if err != nil {
		// Prefer an explicit override on the request, falling back to the
		// parent session's agent_name. Without the fallback, the SQLite
		// store would default to "claude" regardless of which agent
		// actually owns the session. Empty override + missing session →
		// the default Claude agent.
		agentName := msg.GetAgentName()
		if agentName == "" {
			if sess, sErr := s.sessions.Get(ctx, msg.SessionId); sErr == nil {
				agentName = sess.AgentName
			}
		}
		if agentName == "" {
			agentName = "claude"
		}
		// Existing clients already use canonical Claude/Codex UUIDs. This safety
		// check cannot be version-gated: transcript plugins reject unsafe ids for
		// every client. See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
		if err := validateAgentSessionID(agentName, msg.AgentSessionId); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if msg.GetResume() && !s.resumable(ctx, agentName, msg.SessionId, msg.AgentSessionId) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%w: %s", errResumeChatGone, msg.AgentSessionId))
		}
		chat, err = s.agentChats.Create(ctx, domain.CreateAgentChatParams{
			SessionID:      msg.SessionId,
			AgentSessionID: msg.AgentSessionId,
			AgentName:      agentName,
			Title:          msg.Title,
			Role:           recordChatRole(msg.GetRole()),
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("record chat: %w", err))
		}
		// Only a NEW chat row cancels: resuming, prompts and callbacks keep
		// an existing chat busy, so the pending request keeps waiting.
		s.cancelPendingForNewChat(msg.SessionId)
	}

	// Ensure a tmux session is alive to host the chat. The tmux server is
	// independent of bossd, so this lets the chat survive bossd restarts —
	// reattaching just reconnects to the existing tmux session.
	if err := s.ensureChatTmuxSession(ctx, chat, msg.GetResume()); err != nil {
		s.logger.Warn().Err(err).
			Str("agentSessionID", chat.AgentSessionID).
			Msg("failed to ensure tmux session for chat")
		if refusal, ok := injectionRefusalConnectError(err, "ensure chat tmux session"); ok {
			return nil, refusal
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ensure chat tmux session: %w", err))
	} else {
		// Refetch so the response carries the persisted tmux_session_name.
		if refreshed, refreshErr := s.agentChats.GetByAgentSessionID(ctx, chat.AgentSessionID); refreshErr == nil {
			chat = refreshed
		}
	}

	return connect.NewResponse(&pb.RecordChatResponse{Chat: agentChatToProto(chat)}), nil
}

// ensureChatTmuxSession guarantees that a `tmux` session named per
// domain.ChatSessionName exists and is hosting the chat's agent (claude,
// codex, …) for the given chat. Idempotent: a no-op if the session is
// already alive. Persists the resolved name onto the chat row so kill/list
// paths can find it later.
func (s *Server) ensureChatTmuxSession(ctx context.Context, chat *domain.AgentChat, resume bool) (err error) {
	if s.chatTmux == nil || !s.chatTmux.Available(ctx) {
		return nil
	}
	sess, err := s.sessions.Get(ctx, chat.SessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	tmuxName := domain.ChatSessionName(sess.RepoID, chat.AgentSessionID)

	deps := s.chatSpawnDeps()
	if resume {
		// Detached, dedicated budget: the legacy backfill is a whole-corpus codex
		// rollout scan, so it must not inherit the RecordChat request's deadline
		// (the source of the observed DeadlineExceeded) — WithoutCancel keeps the
		// request-scoped values while escaping its cancellation, matching
		// proxy_server.go's failover-commit idiom.
		backfillCtx, cancelBackfill := context.WithTimeout(context.WithoutCancel(ctx), providerSessionIDLegacyBackfillTimeout)
		_, reason, backfillErr := s.backfillCodexProviderSessionID(backfillCtx, chat, sess.WorktreePath,
			panePIDForChat(ctx, deps.Tmux, tmuxName), deps.Resolver)
		cancelBackfill()
		// Keep an already-live pane attachable even if discovery fails. If a
		// Codex pane needs respawning, RequireResume below preserves the chat
		// and refuses to replace unavailable history with an empty conversation.
		if backfillErr != nil {
			s.logger.Warn().Err(backfillErr).
				Str("agent_session_id", chat.AgentSessionID).
				Str("agent", chat.AgentName).
				Msg("legacy provider session id discovery failed before attach; continuing without it")
		} else if reason != "" {
			s.logger.Warn().
				Str("agent_session_id", chat.AgentSessionID).
				Str("agent", chat.AgentName).
				Str("reason", reason).
				Msg("legacy provider session id discovery ambiguous before attach")
		}
	}
	defaultAccountID := ""
	sessionEnvFunc := func() (map[string]string, error) {
		defaultAccountID = s.defaultAccountIDForChat(ctx, sess, chat)
		// Merge the bound account's spawn env UNDER the managed session env so a
		// persisted account_id gets its credentials materialized on the attach path,
		// mirroring the lifecycle/host spawn paths. Managed BOSS_* keys always win on
		// conflict; a nil resolver or account 0 leaves SessionEnv byte-identical to
		// the ambient-login behavior. Values are never logged. The account is
		// resolved for the CHAT's agent (spawnChatTmux launches chat.AgentName), not
		// the parent session's, so cross-agent chats transparently receive their own
		// provider's default account env. This closure runs only after spawnChatTmux
		// has proved a new tmux session is needed, avoiding account last-used updates
		// on already-live attaches.
		//
		// BOS-1142: a bound account whose credentials cannot be injected refuses
		// the attach instead of downgrading it to the ambient CLI login. The
		// closure runs before argv is built, so nothing has been created yet.
		accountEnv, err := s.resolveChatAccountEnvForSpawn(ctx, sess, chat, defaultAccountID, recordAccountUse)
		if err != nil {
			return nil, err
		}
		return mergeManagedOverAccount(
			session.ManagedSessionEnv(sess, chat.AgentSessionID, chat.AgentName),
			accountEnv,
		), nil
	}
	// One repo lookup per spawn: the trust decision (Repo) and the env overlay
	// below both read this value (BOS-1562).
	repo := session.RepoForSessionEnv(ctx, s.repos, sess.RepoID, sess.ID, "attach chat", s.logger)
	appendPrompt, promptClasses := session.BuildAppendSystemPrompt(sess, chat.AgentSessionID, chat.AgentName, session.ResolveSubagentGrantForSpawn(s.logger))
	result, err := spawnChatTmux(ctx, deps, spawnInput{
		Chat:                      chat,
		WorktreePath:              sess.WorktreePath,
		TmuxName:                  tmuxName,
		ForceFresh:                !resume,
		RequireResume:             resume && chat.AgentName == "codex",
		AppendSystemPrompt:        appendPrompt,
		AppendSystemPromptClasses: promptClasses,
		Repo:                      repo,
		SessionEnvFunc: func() (map[string]string, error) {
			// Fill the repo's stored LINEAR_API_KEY / SENTRY_* secrets beneath the
			// worktree .env so the attached chat authenticates to its own repo's
			// Linear workspace, not the daemon's ambient one. A missing/failed
			// repo lookup is non-fatal (OverlayWithRepo still guarantees
			// LINEAR_API_KEY is present). A credential-injection refusal is
			// fatal, though — see sessionEnvFunc.
			base, err := sessionEnvFunc()
			if err != nil {
				return nil, err
			}
			return s.environmentOverlay(base, sess.WorktreePath, repo), nil
		},
		Model:                  chat.Model,
		SessionAgentName:       sess.AgentName,
		SessionEffectiveEffort: sess.EffectiveEffort,
	})
	if err != nil {
		return err
	}
	// Arm the pane rollback for the row-write window. From here until the chat
	// row names this pane, an error return would strand a live RemainOnExit pane
	// that no cleanup path can find, because they all key off
	// agent_chats.tmux_session_name (BOS-845). Not armed for OutcomeAlreadyLive:
	// that pane belongs to an earlier successful claim and killing it would
	// destroy a live chat. spawnChatTmux already rolled back its own failures, so
	// the error return above must not be armed either — hence arming here rather
	// than before the call.
	paneNeedsRollback := result.Outcome != OutcomeAlreadyLive
	defer func() {
		if err != nil && paneNeedsRollback {
			killSpawnedChatPaneBestEffort(ctx, deps.Tmux, s.logger, chat.AgentSessionID, tmuxName)
		}
	}()
	if result.Outcome != OutcomeAlreadyLive {
		s.persistDefaultAccountForChat(ctx, sess, chat, defaultAccountID)
	}

	if result.Outcome == OutcomeFreshFallback && !result.LaunchedAt.IsZero() && result.ProviderSessionID == "" {
		ev := s.logger.Warn().
			Str("agent_session_id", chat.AgentSessionID).
			Str("agent", chat.AgentName)
		if result.DiscoveryAmbiguous {
			ev = ev.Bool("ambiguous", true)
		}
		if result.DiscoveryReason != "" {
			ev = ev.Str("reason", result.DiscoveryReason)
		}
		ev.Msg("interactive provider session id not discovered after launch")
		if deps.Resolver != nil && !result.DiscoveryAmbiguous {
			s.discoverProviderSessionIDInBackground(chat, sess.WorktreePath, tmuxName, result.LaunchedAt, deps.Resolver, deps.Tmux)
		}
	}
	// Only write a provider session id when none is stored. A codex id is
	// guessed from disk (fd resolution or time-window scan); overwriting a
	// non-nil id on difference is exactly what stamped a sibling chat's id over
	// the correct one (BOS-290). Once bound, a chat's id is authoritative.
	if result.ProviderSessionID != "" && chat.ProviderSessionID == nil {
		providerID := result.ProviderSessionID
		if err := s.agentChats.UpdateProviderSessionID(ctx, chat.AgentSessionID, &providerID); err != nil {
			return fmt.Errorf("persist provider session id: %w", err)
		}
		chat.ProviderSessionID = &providerID
	}

	if chat.TmuxSessionName == nil || *chat.TmuxSessionName != tmuxName {
		if err := s.agentChats.UpdateTmuxSessionName(ctx, chat.AgentSessionID, &tmuxName); err != nil {
			return fmt.Errorf("persist tmux session name: %w", err)
		}
	}
	// The row now names the pane, so cleanup can reach it: disarm.
	paneNeedsRollback = false
	if result.Outcome != OutcomeAlreadyLive {
		s.recordInteractiveAgentRunStart(ctx, sess, chat, result.LaunchedAt)
	}
	return nil
}
