package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/tmux"
)

// SendChatMessage delivers a user message into a chat's live agent, optionally
// waking an asleep session first.
func (s *Server) SendChatMessage(ctx context.Context, req *connect.Request[pb.SendChatMessageRequest]) (*connect.Response[pb.SendChatMessageResponse], error) {
	chat, err := s.agentChats.GetByAgentSessionID(ctx, req.Msg.GetAgentSessionId())
	if err != nil || chat == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found: %s", req.Msg.GetAgentSessionId()))
	}

	sess, err := s.sessions.Get(ctx, chat.SessionID)
	if err != nil || sess == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found for chat %s", req.Msg.GetAgentSessionId()))
	}

	// Prefer the tmux session name persisted on the chat row: a legacy or
	// relocated chat may be live under a name that differs from the deterministic
	// one, and checking/sending to the recomputed name would miss the running
	// session (and, on the wake path, spawn a duplicate). Fall back to the
	// deterministic name when none has been persisted yet. This mirrors the kill
	// and liveness paths (killChatTmuxSession, tmux_poller, liveness).
	tmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
		tmuxName = *chat.TmuxSessionName
	}

	// Resolve the tmux interface: prefer the test hook, fall back to the live client.
	var spawner tmuxSpawner
	if s.wakeHook.spawner != nil {
		spawner = s.wakeHook.spawner
	} else if s.tmux != nil {
		spawner = liveTmuxSpawner{c: s.tmux}
	}

	isLive := spawner != nil && spawner.HasSession(ctx, tmuxName)

	if !isLive {
		if !req.Msg.GetWakeIfAsleep() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("chat %s is not live and wake_if_asleep is false", req.Msg.GetAgentSessionId()))
		}
		_, wokenName, _, wakeErr := s.WakeChatInternal(ctx, req.Msg.GetAgentSessionId(), false)
		if wakeErr != nil {
			if errors.Is(wakeErr, ErrWorktreeMissing) || errors.Is(wakeErr, ErrHeadlessRunActive) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, wakeErr)
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("wake chat: %w", wakeErr))
		}
		// Wake spawns under (and persists) the canonical name; send there rather
		// than to a stale persisted name we may have just superseded.
		if wokenName != "" {
			tmuxName = wokenName
		}
	}

	if spawner == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("tmux not available"))
	}

	// Rewrite an installed custom skill command ("/boss-repair watch",
	// "/api-review") to the chat agent's command prefix before delivery,
	// mirroring the plan-launch render path: a raw "/boss-repair" reaches codex
	// verbatim and its CLI rejects it as unrecognized (codex custom commands use
	// "$"). The rewrite is scoped to installed skill names, so a codex user's
	// native built-in ("/model", "/status"), free text, and multi-line input all
	// pass through unchanged.
	message := session.RenderBossCommandPrefix(req.Msg.GetMessage(), chatCommandPrefix(chat.AgentName), sess.WorktreePath)

	// submit routes the delivery: true submits a single-line message (Enter +
	// BOS-228 verifier) and pastes-only a multi-line one; false (default)
	// prefills the composer. The verifier fails toward "still pending", so a
	// swallowed Enter surfaces as an error here rather than a silent false
	// "submitted". The ready marker is resolved from the chat's agent so the
	// submit path waits for the correct composer glyph (claude "❯", codex "›").
	if err := spawner.SendMessage(ctx, tmuxName, message, req.Msg.GetSubmit(), chatReadyMarker(chat.AgentName)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send message: %w", err))
	}

	return connect.NewResponse(&pb.SendChatMessageResponse{
		TmuxSessionName: tmuxName,
		Delivered:       true,
	}), nil
}
