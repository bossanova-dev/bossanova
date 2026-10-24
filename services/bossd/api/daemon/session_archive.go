package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

// --- MarkArchived / Resurrect ---

// ArchiveSessionAndNotify performs the full session archive (stop processes,
// kill tmux, remove worktree, set archived_at) and emits the stream update so
// the TUI/bosso read-model reflect the session immediately. It is idempotent:
// a genuinely missing session emits KIND_DELETED, while an already-archived
// session emits an update (keeping it archived/in trash). Both return nil.
// Shared by the ArchiveSession RPC and the dependabot auto-archive path (BOS-101).
func (s *Server) ArchiveSessionAndNotify(ctx context.Context, id string) error {
	if err := s.lifecycle.ArchiveSession(ctx, id); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("archive session: %w", err)
	}
	// A domain.ErrNotFound from ArchiveSession is ambiguous: the session row may be
	// gone, or it may exist but already be archived (the archived_at IS NULL
	// update matched no rows). The Get below is the source of truth — only a
	// genuinely missing row emits the delete delta. An already-archived session
	// must stay archived/in trash, not be dropped from the read model (BOS-101).
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			if s.onSessionDeleted != nil {
				s.onSessionDeleted(ctx, id)
			}
			return nil
		}
		return fmt.Errorf("get session: %w", err)
	}
	p := s.sessionProtoWithRepo(ctx, sess)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	return nil
}

// archiveRPCBudget bounds an archive the RPC has started once it is detached
// from the caller: generous enough for a slow `git worktree remove` on a large
// worktree, finite so a wedged step cannot pin the handler forever.
const archiveRPCBudget = 2 * time.Minute

func (s *Server) ArchiveSession(ctx context.Context, req *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	// An archive that has started must finish even if the caller goes away
	// (BOS-1372). It kills every chat's tmux pane before removing the worktree
	// and marking the row, so a session archiving itself kills its own client
	// mid-call; on the request context the remaining steps would then fail with
	// context.Canceled, leaving dead panes on an unarchived session.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), archiveRPCBudget)
	defer cancel()
	// Route through the deferral seam (BOS-1380): a session with a working
	// chat is archived once every chat is idle rather than now.
	outcome, err := s.RequestArchive(ctx, req.Msg.Id, ArchiveRequest{
		Force:                   req.Msg.GetShouldForce(),
		RequesterAgentSessionID: req.Msg.GetRequesterAgentSessionId(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &pb.ArchiveSessionResponse{
		Session:                    &pb.Session{Id: req.Msg.Id},
		IsDeferred:                 outcome.Pending,
		BlockingAgentSessionId:     outcome.BlockingAgentSessionID,
		BlockingChatStalledSeconds: int32(outcome.BlockingStalledFor / time.Second),
	}
	if sess, err := s.sessions.Get(ctx, req.Msg.Id); err == nil {
		resp.Session = s.sessionProtoWithRepo(ctx, sess)
	}
	// archive_pending lives only in the display tracker. Without it a client
	// cannot tell a deferred archive from a finished one: the web detail page
	// would leave a session whose archive is still waiting on a chat.
	if s.displayTracker != nil {
		HydrateDisplayEntry(resp.Session, s.displayTracker.Get(req.Msg.Id))
	}
	return connect.NewResponse(resp), nil
}

// ResurrectSession is the thin ConnectRPC handler: it delegates to the
// streaming core StreamResurrectSession, wiring the connect server stream's
// Send as the emit sink. Same split as CreateSession, and for the same reason
// — the reverse-stream path (bosso -> daemon ResurrectSessionCommand) reuses
// the core without a non-mockable *connect.ServerStream.
func (s *Server) ResurrectSession(ctx context.Context, req *connect.Request[pb.ResurrectSessionRequest], stream *connect.ServerStream[pb.ResurrectSessionResponse]) error {
	return s.StreamResurrectSession(ctx, req.Msg, stream.Send)
}

// StreamResurrectSession holds the full ResurrectSession body. Progress frames
// are emitted as the repo's setup script produces output, then exactly one
// terminal SessionResurrected frame.
//
// Streaming is what makes a slow setup survivable at all (BOS-984): a unary
// response could not outlast the daemon's own 120s http.Server WriteTimeout,
// while the setup script it is waiting on is allowed five minutes. The write
// deadline for this procedure is cleared in Listen (see
// writeDeadlineExemptProcedures) — without that, streaming alone would not help.
func (s *Server) StreamResurrectSession(ctx context.Context, msg *pb.ResurrectSessionRequest, emit func(*pb.ResurrectSessionResponse) error) error {
	if msg.GetId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Un-archiving a session whose archive is only pending (BOS-1380) cancels
	// the pending archive. The session was never archived, so there is no
	// setup to run and Lifecycle.ResurrectSession would refuse it.
	if handled, err := s.cancelPendingArchiveForResurrect(ctx, msg.GetId(), emit); handled {
		return err
	}

	bs, err := s.bootstrapRunner.StartResurrect(msg.GetId(), session.ResurrectSessionOpts{}, session.ResurrectHooks{
		OnSettled: s.onResurrectSettled,
	})
	if err != nil {
		if errors.Is(err, session.ErrBootstrapInFlight) {
			return connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return resurrectSessionConnectError(err)
	}
	lines, cancel := bs.Subscribe()
	defer cancel()
	if err := relaySetupOutput(ctx, lines, bs.Done(), func(line string) error {
		return emit(&pb.ResurrectSessionResponse{Event: &pb.ResurrectSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: line}}})
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bs.Done():
	}
	if err := bs.Err(); err != nil {
		return resurrectSessionConnectError(err)
	}
	result := bs.ResurrectResult()

	sess, err := s.sessions.Get(ctx, msg.GetId())
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	p := s.sessionProtoWithRepo(ctx, sess)

	setupError := ""
	if result != nil && result.SetupErr != nil {
		setupError = result.SetupErr.Error()
	}
	return emit(&pb.ResurrectSessionResponse{
		Event: &pb.ResurrectSessionResponse_SessionResurrected{
			SessionResurrected: &pb.SessionResurrected{Session: p, SetupError: setupError},
		},
	})
}

// cancelPendingArchiveForResurrect handles a resurrect of a session that is not
// archived but has a pending archive: it cancels the pending archive and emits
// the terminal SessionResurrected frame with the current session. handled is
// false when there is nothing pending (or the session is already archived), in
// which case the caller runs the ordinary resurrect.
func (s *Server) cancelPendingArchiveForResurrect(ctx context.Context, id string, emit func(*pb.ResurrectSessionResponse) error) (bool, error) {
	if !s.archiveDeferral().has(id) {
		return false, nil
	}
	sess, err := s.sessions.Get(ctx, id)
	if err != nil || sess == nil || sess.ArchivedAt != nil {
		return false, nil
	}
	// Report only the cancel that happened: if the sweep claimed the archive
	// since has(), fall through to the ordinary resurrect path.
	if !s.CancelPendingArchive(id) {
		return false, nil
	}
	p := s.sessionProtoWithRepo(ctx, sess)
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	return true, emit(&pb.ResurrectSessionResponse{
		Event: &pb.ResurrectSessionResponse_SessionResurrected{
			SessionResurrected: &pb.SessionResurrected{Session: p},
		},
	})
}

func (s *Server) onResurrectSettled(ctx context.Context, id string, _ *session.ResurrectSessionResult, err error) {
	if err != nil || s.onSessionUpdated == nil {
		return
	}
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		s.logger.Error().Err(err).Str("session", id).Msg("get resurrected session for update")
		return
	}
	s.onSessionUpdated(ctx, s.sessionProtoWithRepo(ctx, sess))
}

// resurrectSessionConnectError maps a resurrect failure onto the Connect code
// that describes it, mirroring createSessionConnectError's role on the create
// path.
//
// It exists because BOS-984 gave resurrect an internal bound of its own
// (Lifecycle.ResurrectSession wraps the whole call in bootstrapDeadline). Left
// in the generic CodeInternal bucket that bound is indistinguishable from a
// worktree failure — which contradicts the three-way distinction
// ResurrectSessionResponse's own comment promises: a setup failure arrives as
// setup_error on a successful stream, a worktree failure is CodeInternal with
// no terminal frame, and a timeout is CodeDeadlineExceeded with no terminal
// frame. Without this mapping the daemon-side bound reported the middle case
// for the last one.
func resurrectSessionConnectError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("resurrect session: %w", err))
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, fmt.Errorf("resurrect session: %w", err))
	default:
		if refusal, ok := injectionRefusalConnectError(err, "resurrect session"); ok {
			return refusal
		}
		return connect.NewError(connect.CodeInternal, fmt.Errorf("resurrect session: %w", err))
	}
}

func (s *Server) sessionProtoWithRepo(ctx context.Context, sess *models.Session) *pb.Session {
	p := SessionToProto(sess)
	if sess.RepoID != "" {
		if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil && repo != nil {
			p.RepoDisplayName = repo.DisplayName
			p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
		}
	}
	return p
}
