package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
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

	// Intercept a SUBMITTED single-line "/boss switch" (or "/switch") control
	// command before any tmux delivery: parse it mechanically, run the existing
	// account-switch primitive, and return the outcome in notice_text WITHOUT ever
	// waking, pasting into, or submitting to the pane — so a credit-exhausted chat
	// can switch accounts with zero LLM calls. Everything that is not a bare
	// single-line "/boss switch" (prose, multi-line input, any other command) falls
	// through to normal delivery below, unchanged.
	//
	// The interception is gated on submit: a submit=false request explicitly asks
	// to "only prefill the composer" (see BOS-242), so honoring that intent, a
	// prefilled "/boss switch" is staged into the composer verbatim rather than
	// executed — the switch fires only when the caller actually submits the command
	// (the CLI's default; an explicit submit=true for MCP/web). This keeps submit's
	// meaning consistent: it governs whether the user's directive is acted on now.
	//
	// Scope boundary: this guards only the RPC send path. Raw SSH keystrokes typed
	// directly into the tmux pane bypass this RPC entirely and still reach the agent
	// (→ 401 on an exhausted account); guarding the raw pane is deliberately out of
	// scope here and is covered by S3 (auto-rotate on 401).
	if req.Msg.GetSubmit() {
		if cmd, ok := session.ParseBossControlCommand(req.Msg.GetMessage()); ok {
			return s.handleBossSwitchInterception(ctx, chat, sess, cmd)
		}
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

// handleBossSwitchInterception executes a mechanically-parsed "/boss switch"
// control command and returns the outcome as a SendChatMessageResponse whose
// Delivered is false and whose NoticeText carries the human-readable result. The
// message is never delivered to the agent, so the switch costs zero LLM calls.
//
// Account resolution mirrors the SwitchSessionAccount RPC so the two paths behave
// identically:
//   - Named target (cmd.Account != ""): resolveSessionAccount does id-or-label,
//     provider-scoped, eligibility-checked resolution — an ambiguous label is
//     InvalidArgument and an ineligible (disabled/failed/cooling) named target is
//     InvalidArgument, both surfaced verbatim (identical to SwitchSessionAccount).
//   - Unnamed target: the S1 util-aware resolver picks the next eligible account
//     EXCLUDING the current one. When no other account is available it returns a
//     friendly notice rather than an error or a silent no-op.
//
// The switch itself runs through executeAccountSwitch (Auto:false), so mid-turn
// refusal (→ FailedPrecondition naming --force) and the stream publish are shared.
func (s *Server) handleBossSwitchInterception(ctx context.Context, chat *models.AgentChat, sess *models.Session, cmd *session.BossControlCommand) (*connect.Response[pb.SendChatMessageResponse], error) {
	// The switch's account picker is scoped to the CHAT's provider (a claude
	// account must not bind a codex chat), falling back to the session's agent for
	// a legacy chat with no AgentName — the same resolution switchTargetAgentName
	// performs, but chat and sess are already in scope so no extra fetch is needed.
	agentName := chat.AgentName
	if agentName == "" {
		agentName = sess.AgentName
	}

	var targetAccountID string
	var err error
	if cmd.Account != "" {
		requested := cmd.Account
		targetAccountID, err = s.resolveSessionAccount(ctx, &requested, agentName)
		if err != nil {
			return nil, err
		}
	} else {
		if s.resolver == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("no account registry configured; cannot pick a switch target"))
		}
		current := derefAccountID(sess.AccountID)
		targetAccountID, err = s.resolver.DefaultAccountIDExcluding(ctx, accountAgentName(agentName), current, time.Now())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("select switch target: %w", err))
		}
		// An empty pick (or the current account echoed back) means there is no
		// other eligible account to move to. Report it as a benign notice on the
		// undelivered response, not an error — the user asked a reasonable question
		// and there is simply nowhere to go.
		if targetAccountID == "" || targetAccountID == current {
			return connect.NewResponse(&pb.SendChatMessageResponse{
				Delivered:  false,
				NoticeText: "no other account available to switch to",
			}), nil
		}
	}

	res, err := s.executeAccountSwitch(ctx, chat.SessionID, chat.AgentSessionID, targetAccountID, cmd.Force)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&pb.SendChatMessageResponse{
		Delivered:  false,
		NoticeText: res.NoticeText,
	}), nil
}
