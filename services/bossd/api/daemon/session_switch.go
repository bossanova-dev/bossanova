package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/internal/detach"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

// SwitchSessionAgentAccount stops the session's live chat, rebinds the session to the
// chosen rotation account, and brings the chat back up under it (session core
// owns the stop/rebind/resume-or-fresh orchestration). When agent_session_id is
// omitted the handler resolves the session's primary live chat — the most
// recently created chat that still has a tmux session. A mid-turn chat is
// refused unless force is set, so callers surface a confirmation first.
func (s *Server) SwitchSessionAgentAccount(ctx context.Context, req *connect.Request[pb.SwitchSessionAgentAccountRequest]) (*connect.Response[pb.SwitchSessionAgentAccountResponse], error) {
	msg := req.Msg
	if strings.TrimSpace(msg.SessionId) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}

	agentSessionID := strings.TrimSpace(msg.GetAgentSessionId())
	if agentSessionID == "" {
		resolved, err := s.resolvePrimaryLiveChat(ctx, msg.SessionId)
		if err != nil {
			return nil, err
		}
		agentSessionID = resolved
	}

	// Resolve the switch target through the same id/label + eligibility validation
	// as session creation. account_id is advertised as an account id OR a
	// provider-scoped label ("id or label"), and an explicitly-targeted account
	// must match the chat's provider and be eligible (active, healthy, not
	// cooling). An empty target is the explicit "system default (account 0)"
	// choice and skips validation. Scope to the CHAT's agent (the account picker
	// is scoped to the chat's provider), falling back to the session's agent for a
	// legacy chat with no AgentName.
	agentName, err := s.switchTargetAgentName(ctx, msg.SessionId, agentSessionID)
	if err != nil {
		return nil, err
	}
	requested := msg.AccountId
	targetAccountID, err := s.resolveSessionAccount(ctx, &requested, agentName, "")
	if err != nil {
		return nil, err
	}

	res, err := s.executeAccountSwitch(ctx, msg.SessionId, agentSessionID, targetAccountID, msg.Force)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&pb.SwitchSessionAgentAccountResponse{
		Resumed:     res.Resumed,
		TargetLabel: res.TargetLabel,
		NoticeText:  res.NoticeText,
	}), nil
}

// switchRespawnBudget resolves the deadline executeAccountSwitch applies to the
// switch primitive, derived from the composer-readiness deadline this daemon's
// tmux client is actually configured with (BOS-948).
//
// It has to be derived rather than fixed. The budget's whole sizing argument is
// stated against session_start_ready_deadline_seconds, and that setting is
// deliberately unclamped — the operator docs tell slow-host operators to raise
// it. A compiled constant stopped funding one full readiness attempt past
// roughly 88 configured seconds, and the shortening was silent: the wait simply
// ran for less than the operator had asked for.
//
// The resolved value is read from the tmux client rather than from
// config.Load(). The client already holds the setting as resolved at daemon
// start, so reading it here costs nothing, cannot disagree with the wait it is
// sizing, and preserves the "takes effect on the next daemon restart" contract
// the setting documents — a per-switch re-read would quietly break it.
//
// A nil client is the test and not-yet-wired case; SwitchRespawnBudgetFor
// floors a non-positive readiness to the default, so it yields the same budget
// the constant used to.
func (s *Server) switchRespawnBudget() time.Duration {
	if s.switchRespawnBudgetOverride > 0 {
		return s.switchRespawnBudgetOverride
	}
	if s.tmux == nil {
		return config.SwitchRespawnBudgetFor(0)
	}
	return config.SwitchRespawnBudgetFor(s.tmux.SessionStartReadyDeadline())
}

// ExpireSwitchBudgetForTest drives executeAccountSwitch until its real
// detach.Flight budget expires. It is exported only for cross-service tests
// that cannot import this internal package directly.
func ExpireSwitchBudgetForTest(ctx context.Context, budget time.Duration) error {
	s := &Server{
		logger:                      zerolog.Nop(),
		switchRespawnBudgetOverride: budget,
	}
	s.switchAccountFn = func(ctx context.Context, _ session.SwitchAgentAccountParams) (session.SwitchAgentAccountResult, error) {
		<-ctx.Done()
		return session.SwitchAgentAccountResult{}, ctx.Err()
	}
	_, err := s.executeAccountSwitch(ctx, "sess-switch-budget-expiry", "agent-switch-budget-expiry", "acct-switch-budget-expiry", false)
	return err
}

// executeAccountSwitch runs the manual (Auto:false) account switch primitive for
// an already-resolved target account id and publishes the rebound session. It is
// the shared body of the SwitchSessionAgentAccount RPC and the SendChatMessage
// "/boss switch" interception, so both behave identically: same typed-error →
// connect-code mapping and same best-effort stream publish. Callers own account
// resolution (resolveSessionAccount / DefaultAgentAccountIDExcluding) and the outer
// response shape; targetAccountID is "" for the system-default (account 0).
//
// The returned error is already a connect error with the right code; callers
// return it verbatim.
//
// # Budget and serialization (BOS-897, BOS-948)
//
// The whole primitive runs as one detach.Flight on chatSwitchGroup under the
// budget switchRespawnBudget resolves — derived from this daemon's configured
// composer-readiness deadline rather than compiled (BOS-948), so it is read per
// call rather than referenced as a constant. detach.Flight owns the mechanics —
// key validation, the dead-caller fence, panic recovery, the detached budget
// context, and the caller-owned wait — so what remains here is why THIS call
// site wants them:
//
//   - The budget is the only deadline the switch's respawn ever sees. bosso's
//     per-command relay bounds only bosso's own wait for the CommandResult; the
//     command reaches the daemon on the long-lived stream context, so without
//     this the readiness wait inside StartTmuxChat runs its full per-attempt
//     budget twice with nothing above it to stop.
//   - The group is what makes a losing attempt harmless — see chatSwitchGroup's
//     own comment for the teardown race it removes.
//
// A joiner that abandons leaves the leader running, deliberately: the leader's
// pane is the one that should survive. The cost is that a caller can be told
// DeadlineExceeded while the switch subsequently succeeds, which is strictly
// better than the pane being destroyed and matches how WakeChat already
// behaves. Keying on agentSessionID also coalesces two switches on one chat to
// DIFFERENT target accounts inside one budget — the joiner receives the
// leader's result. Rare and non-destructive, and pinned by test so it is a
// chosen semantic rather than a discovered one.
func (s *Server) executeAccountSwitch(ctx context.Context, sessionID, agentSessionID, targetAccountID string, force bool) (session.SwitchAgentAccountResult, error) {
	switchFn := s.switchAccountFn
	if switchFn == nil {
		switchFn = s.lifecycle.SwitchAgentAccount
	}

	// Built only when the test-only seam is set, so production passes no option
	// at all and the flight below is the unmodified primitive. The hook's
	// parameter is detach.Flight's own flight key, which on this route IS
	// agentSessionID — the same value passed as the key below — so the field is
	// handed over directly rather than re-closed over.
	var opts []detach.FlightOption
	if s.switchFlightAttached != nil {
		opts = append(opts, detach.WithAttachHook(s.switchFlightAttached))
	}

	// The flight key must never be empty. Both production callers resolve a real
	// chat first — SwitchSessionAgentAccount through resolvePrimaryLiveChat, which
	// errors rather than yielding "", and the "/boss switch" interception from an
	// already-loaded chat row — so ErrEmptyKey below is an assertion, not a
	// reachable path. detach.Flight refuses it because the failure it guards is
	// silent and cross-session: on an empty key EVERY concurrent switch would
	// coalesce into one flight regardless of chat or session, and a switch for
	// session B would return session A's result having never run its own.
	//
	// detach.Flight also refuses an ALREADY-dead caller before seeding a flight,
	// returning its ctx.Err() verbatim so the arms below can split Canceled from
	// DeadlineExceeded, and recovers any panic in the body — which on a DoChan
	// group is otherwise unrecoverable and takes the whole daemon down.
	res, err := detach.Flight(ctx, &s.chatSwitchGroup, s.logger, agentSessionID, s.switchRespawnBudget(),
		func(budgetCtx context.Context) (session.SwitchAgentAccountResult, error) {
			// budgetCtx is detached from whichever caller happened to WIN the
			// race to be leader and bounded only by the budget, so no caller's
			// departure can cancel work a healthy joiner is still waiting on.
			flightRes, switchErr := switchFn(budgetCtx, session.SwitchAgentAccountParams{
				SessionID:       sessionID,
				AgentSessionID:  agentSessionID,
				TargetAccountID: targetAccountID,
				Force:           force,
			})
			if switchErr != nil {
				return session.SwitchAgentAccountResult{}, switchErr
			}
			// Publish from INSIDE the flight, on the flight's own budget,
			// because this is the only place that runs exactly once per switch
			// and only when the switch actually happened.
			//
			// Outside the flight it would run neither reliably nor once. A
			// caller that leaves gets its own context error back, so a switch
			// whose callers all left — reachable precisely because the flight is
			// detached and outlives them — would change the durable
			// sessions.account_id and publish nothing, leaving the web account
			// badge stale until the next full daemon snapshot. And every joiner
			// receiving the shared result would publish the same delta again, so
			// N callers meant N identical publishes. budgetCtx is also the only
			// context here guaranteed live: the caller's may already be dead,
			// which is exactly the case that leaks.
			s.publishReboundSession(budgetCtx, sessionID)
			return flightRes, nil
		}, opts...)
	if err != nil {
		switch {
		// Preserves the pre-detach.Flight message and code exactly; the guard
		// itself now lives in the helper so a future call site cannot omit it.
		case errors.Is(err, detach.ErrEmptyKey):
			return session.SwitchAgentAccountResult{}, connect.NewError(connect.CodeInvalidArgument,
				errors.New("switch session account: agent_session_id is required"))
		case errors.Is(err, session.ErrChatMidTurn):
			return session.SwitchAgentAccountResult{}, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("chat is mid-turn; confirm the switch (force) to interrupt it: %w", err))
		// ErrAgentAccountAuthInvalid belongs here with the other target-eligibility
		// refusals, not on the default arm. The Lifecycle reloads the target
		// after resolveSessionAccount already prechecked it, so credential
		// maintenance can bench it in between; that is a routine refusal about
		// the target's state, and answering it with CodeInternal reports a
		// server fault (and, through the hosted classifier, an unmapped code)
		// for a request the daemon handled exactly as designed.
		case errors.Is(err, session.ErrAgentAccountCooling), errors.Is(err, session.ErrAgentAccountDisabled),
			errors.Is(err, session.ErrAgentAccountFailed), errors.Is(err, session.ErrAgentAccountAuthInvalid):
			return session.SwitchAgentAccountResult{}, connect.NewError(connect.CodeFailedPrecondition, err)
		case errors.Is(err, domain.ErrNotFound):
			return session.SwitchAgentAccountResult{}, connect.NewError(connect.CodeNotFound, err)
		// Ahead of the default arm on purpose (BOS-747): a request that ended
		// because a context ran out must not be answered with a code that
		// invites a retry, and CodeInternal is what the default would give it.
		// Reachable only when a context genuinely expired — a readiness wait
		// that merely exhausts its OWN budget returns readyMarkerTimeoutErr,
		// which wraps no context error, so the diagnostic timeout still lands
		// on the default arm with its pane snapshot intact.
		case errors.Is(err, context.DeadlineExceeded):
			return session.SwitchAgentAccountResult{}, connect.NewError(connect.CodeDeadlineExceeded,
				fmt.Errorf("switch session account: %w", err))
		case errors.Is(err, context.Canceled):
			return session.SwitchAgentAccountResult{}, connect.NewError(connect.CodeCanceled,
				fmt.Errorf("switch session account: %w", err))
		default:
			// A switch that rebound the chat and then could not inject the target
			// account's credentials refuses the respawn (BOS-1142). That refusal
			// travels the whole way here: StartTmuxChat wraps it with
			// redactedInjection (masked message, Unwrap intact) and SwitchAgentAccount
			// re-wraps it with %w, so the typed outcome is still readable at this
			// edge. It is a precondition on the target account exactly like the
			// eligibility arms above, not a daemon fault — and the create, wake and
			// resurrect paths already answer the same refusal with
			// FailedPrecondition, so leaving the switch on the default arm would
			// make one spawn edge disagree with the other four. The dispatcher leg
			// needs no arm of its own: classifySwitchCommandError maps
			// CodeFailedPrecondition to ERROR_CODE_FAILED_PRECONDITION already.
			if refusal, ok := injectionRefusalConnectError(err, "switch session account"); ok {
				return session.SwitchAgentAccountResult{}, refusal
			}
			return session.SwitchAgentAccountResult{}, connect.NewError(connect.CodeInternal, fmt.Errorf("switch session account: %w", err))
		}
	}

	return res, nil
}

// publishReboundSession streams the rebound session so the cloud/web read model
// reflects the new account immediately, mirroring update/archive/resurrect. The
// switch changed the durable sessions.account_id; without this delta
// ProxyGetSession and the stream-driven read model keep showing the old account
// badge until the next daemon snapshot.
//
// Best-effort: the switch already succeeded by the time this runs, so a
// load/publish failure must never fail the RPC. Call it on the switch's own
// budget context, not a caller's — see the call site for why.
func (s *Server) publishReboundSession(ctx context.Context, sessionID string) {
	if s.onSessionUpdated == nil {
		return
	}
	sess, gerr := s.sessions.Get(ctx, sessionID)
	if gerr != nil {
		s.logger.Warn().Err(gerr).Str("session", sessionID).
			Msg("switch account: failed to load session for stream update")
		return
	}
	p := s.sessionProtoWithRepo(ctx, sess)
	// Re-source provider/account from the primary chat (BOS-381 authority)
	// so the streamed delta matches the Get/List read paths.
	s.withPrimaryChatIdentity(ctx, p, sess)
	// Hydrate the non-secret account label so the streamed delta matches
	// the Get/List read paths; otherwise the web AccountBadge shows the
	// raw account id until the next full snapshot.
	s.withAccountLabel(ctx, p, sess)
	s.withRotationEvents(ctx, p, sess)
	s.onSessionUpdated(ctx, p)
}

// resolvePrimaryLiveChat returns the agent_session_id of the session's primary
// live chat — the most recently created chat that still hosts a tmux session —
// when the caller did not name a specific chat. It returns CodeFailedPrecondition
// when the session has no live chat to switch.
func (s *Server) resolvePrimaryLiveChat(ctx context.Context, sessionID string) (string, error) {
	chats, err := s.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("list chats for session %s: %w", sessionID, err))
	}
	var primary *domain.AgentChat
	for _, c := range chats {
		if c == nil || c.TmuxSessionName == nil || *c.TmuxSessionName == "" {
			continue
		}
		if primary == nil || c.CreatedAt.After(primary.CreatedAt) {
			primary = c
		}
	}
	if primary == nil {
		return "", connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no live chat to switch for session %s", sessionID))
	}
	return primary.AgentSessionID, nil
}

// switchTargetAgentName returns the agent (provider) the switch target should be
// validated against: the selected chat's own AgentName, falling back to the
// session's AgentName for a legacy chat with none. The switch's account picker is
// scoped to the chat's provider, so account id/label resolution and eligibility
// must be scoped the same way (a claude account must not bind a codex chat).
func (s *Server) switchTargetAgentName(ctx context.Context, sessionID, agentSessionID string) (string, error) {
	chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("get chat %s: %w", agentSessionID, err))
	}
	if chat == nil {
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found for agent_session_id %q", agentSessionID))
	}
	if chat.AgentName != "" {
		return chat.AgentName, nil
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("get session %s: %w", sessionID, err))
	}
	return sess.AgentName, nil
}
