package server

// FinishUnattendedRun (BOS-1508).
//
// A finished unattended run had no way to tell bossd it was done, so bossd
// inferred completion from its pane log going quiet for fifteen minutes, and
// the lingering session blocked the next cron fire. FinishUnattendedRun is the
// explicit verb: the run's own chat asks, bossd records the request, and the
// run's normal finalize (the stranded-run sweep's pipeline) fires once the
// session has really settled.
//
// "Settled" is the archive deferrer's idle check (every chat idle, the
// requesting chat past requesterSettle) plus one predicate the archive path
// does not need: every agent log of the session quiet for finishQuietWindow.
// The tracker reads a chat idle as soon as its turn ends, even while background
// subagents keep running, so tracker-idle alone would finalize a live run.
//
// Pending finishes are in memory only, like pending archives: a restart drops
// them. The log-silence sweep then reaps a run that was not Blocked, as it did
// before this verb. Nothing reaps a Blocked run, so its dropped finish leaves
// it Blocked with its old reason until its chat finishes again.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/safego"

	"github.com/recurser/bossd/internal/session"
)

// finishRunBudget bounds one fired finish, detached from any caller. It
// matches the cron completion gate's finalize timeout: a worktree finalize may
// push a branch and open a PR.
const finishRunBudget = 5 * time.Minute

func (s *Server) finishDeferral() *archiveDeferrer {
	return s.finishDeferrer
}

// finishRunNow finalizes sessionID through the unattended-run pipeline. A row
// that is already gone (the sweep or Stop gate hard-deleted it first) is done,
// not an error; hard-delete already notified stream clients.
func (s *Server) finishRunNow(ctx context.Context, sessionID string) error {
	if s.lifecycle == nil {
		return errors.New("finish unattended run: lifecycle unavailable")
	}
	res, err := s.lifecycle.FinishUnattendedRun(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("finish unattended run: %w", err)
	}
	evt := s.logger.Info().Str("session", sessionID)
	if res != nil {
		evt = evt.Bool("noop", res.NoOp).Str("outcome", string(res.Outcome)).Bool("deleted", res.Deleted)
		if res.Err != nil {
			evt = evt.AnErr("finalize_err", res.Err)
		}
	}
	evt.Msg("unattended run finished")
	if res != nil && res.Deleted {
		return nil
	}
	// The row survives a non-deleting outcome (a PR was opened, or a failure
	// blocked it): push its new state to stream clients.
	if s.sessions == nil {
		return nil
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("get session: %w", err)
	}
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, s.sessionProtoWithRepo(ctx, sess))
	}
	return nil
}

// FinishUnattendedRun records that an unattended run is done and finalizes it
// once the session settles. The requester must be one of the session's chats;
// it counts as busy for requesterSettle, so the caller's own request always
// comes back deferred and the RPC never finalizes its own caller synchronously.
func (s *Server) FinishUnattendedRun(ctx context.Context, req *connect.Request[pb.FinishUnattendedRunRequest]) (*connect.Response[pb.FinishUnattendedRunResponse], error) {
	id := req.Msg.GetSessionId()
	requester := req.Msg.GetRequesterAgentSessionId()
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("session_id is required"))
	}
	if requester == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("requester_agent_session_id is required"))
	}
	if s.sessions == nil || s.agentChats == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("session stores unavailable"))
	}
	sess, err := s.sessions.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", id))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}
	if !session.IsUnattendedSession(sess) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session %q is not an unattended run", id))
	}
	if sess.ArchivedAt != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session %q is archived", id))
	}
	// Ownership guard: only a chat of this session may finish it. Outside
	// Blocked any chat of the session qualifies, not just
	// Session.AgentSessionID, so a chat that replaced the original after an
	// account switch can still finish its run. A Blocked run takes only
	// Session.AgentSessionID (AcceptBlockedRunFinish below), so that
	// replacement is refused there.
	chats, err := s.agentChats.ListBySession(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list chats: %w", err))
	}
	owned := false
	for _, c := range chats {
		if c != nil && c.AgentSessionID == requester {
			owned = true
			break
		}
	}
	if !owned {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("chat %q is not a chat of session %q", requester, id))
	}
	if (sess.State == machine.Blocked || req.Msg.GetIsBlocked()) && s.lifecycle == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("lifecycle unavailable"))
	}
	// A Blocked run moves only on its own chat's claim: a finish from that chat
	// finalizes it from Blocked, a finish --blocked re-Blocks it with the new
	// reason. Any other requester is refused, so another chat cannot ready the
	// draft. No dated apiversion bump: FinishUnattendedRun is a DaemonService
	// procedure whose one client is the boss CLI run inside the session,
	// installed with the daemon, and nothing a transform could restore exists
	// in its empty response (the old answer from Blocked was a deferred no-op or
	// FailedPrecondition). See
	// docs/api-versioning.md#skipping-a-bump-justify-it-in-the-diff.
	//
	// From Blocked a finish --blocked's reason rides on the claim and reaches
	// the row only when the finish fires: nothing else finalizes a Blocked
	// run, and a finish dropped before it fires must leave no reason behind.
	//
	// A finish of this run that is already executing has taken, or is about to
	// take, the claim it fired on. Joining it would answer success for a claim
	// nobody reads, so a request that disagrees with it (a finish --blocked
	// after a plain finish, or the reverse) would vanish. It is refused instead,
	// atomically with storing the claim, so a finish that starts afterwards
	// takes this request's claim.
	if sess.State == machine.Blocked {
		var report *string
		if req.Msg.GetIsBlocked() {
			reason := req.Msg.GetBlockedReason()
			report = &reason
		}
		err := s.finishDeferral().unlessInFlight(id, func() error {
			return s.lifecycle.AcceptBlockedRunFinish(sess, requester, report)
		})
		switch {
		case errors.Is(err, errInFlight):
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("a finish of session %q is already running; retry once it ends", id))
		case errors.Is(err, session.ErrNotRunChat):
			return nil, connect.NewError(connect.CodePermissionDenied, err)
		case err != nil:
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	} else if req.Msg.GetIsBlocked() {
		// Persist a blocked report before deferring: whichever finalize
		// reaches the run next (this finish, the Stop-hook gate, the stranded
		// sweep) reads it from the session row.
		if err := s.lifecycle.ReportRunBlocked(ctx, id, req.Msg.GetBlockedReason()); err != nil {
			if errors.Is(err, session.ErrRunAlreadyFinalized) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, err)
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	// Detach from the caller like ArchiveSession: a finish that has started
	// must complete even if the requesting chat goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishRunBudget)
	defer cancel()
	outcome, err := s.finishDeferral().request(ctx, id, ArchiveRequest{RequesterAgentSessionID: requester})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pb.FinishUnattendedRunResponse{
		IsDeferred:             outcome.Pending,
		BlockingAgentSessionId: outcome.BlockingAgentSessionID,
	}), nil
}

// NudgePendingFinish asks the finish deferrer to re-evaluate now. It is called
// from the chat-status hook beside NudgePendingArchive.
func (s *Server) NudgePendingFinish(sessionID string) {
	d := s.finishDeferral()
	if sessionID != "" && !d.has(sessionID) {
		return
	}
	d.signal()
}

// CancelPendingFinish drops a pending finish for sessionID. It reports whether
// one was pending. A new chat row in the session calls it: new work means the
// run is not done.
func (s *Server) CancelPendingFinish(sessionID string) bool {
	return s.finishDeferral().cancel(sessionID)
}

// RunFinishDeferrer starts the finish deferrer loop on ctx and returns its
// done channel. Finishes it fires are handed to track, so main.go joins them
// with the archive drain.
func (s *Server) RunFinishDeferrer(ctx context.Context, track session.ArchiveWorkerTracker) <-chan struct{} {
	d := s.finishDeferral()
	return safego.Go(s.logger, func() { d.run(ctx, track) })
}

// cancelPendingForNewChat cancels a pending archive (BOS-1380) and unattended-run
// finish (BOS-1508): a new chat means there is new work and the run is not done.
func (s *Server) cancelPendingForNewChat(sessionID string) {
	s.CancelPendingArchive(sessionID)
	s.CancelPendingFinish(sessionID)
}
