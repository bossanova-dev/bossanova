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
// them and the log-silence sweep reaps the run, as it did before this verb.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"

	"github.com/recurser/bossd/internal/session"
)

// finishRunBudget bounds one fired finish, detached from any caller. It
// matches the cron completion gate's finalize timeout: a worktree finalize may
// push a branch and open a PR.
const finishRunBudget = 5 * time.Minute

// finishDeferral returns the server's finish deferrer, building it on first
// use so a Server assembled as a struct literal still gets one.
func (s *Server) finishDeferral() *archiveDeferrer {
	s.finishDeferrerOnce.Do(func() {
		var chats archiveChatLister
		if s.agentChats != nil {
			chats = s.agentChats
		}
		var sessions archiveSessionGetter
		if s.sessions != nil {
			sessions = s.sessions
		}
		logger := s.logger.With().Str("deferrer", "finish").Logger()
		// No archiving flag: a pending finish is not an archive, and the
		// session must not render as "Archiving…".
		d := newArchiveDeferrer(chats, s.chatStatus, nil, sessions, s.finishRunNow, logger)
		d.fireBudget = finishRunBudget
		if s.lifecycle != nil {
			d.settled = s.lifecycle.UnattendedRunQuiet
		}
		s.finishDeferrer = d
	})
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
	// Ownership guard: only a chat of this session may finish it. Any chat of
	// the session qualifies, not just Session.AgentSessionID, so a chat that
	// replaced the original after an account switch can still finish its run.
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
