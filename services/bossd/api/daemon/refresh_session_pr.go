package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

const refreshSessionPRTimeout = 20 * time.Second

func (s *Server) RefreshSessionPR(ctx context.Context, req *connect.Request[pb.RefreshSessionPRRequest]) (*connect.Response[pb.RefreshSessionPRResponse], error) {
	msg := req.Msg
	sess, repo, prNumber, err := s.resolveRefreshSessionPRTarget(ctx, msg)
	if err != nil {
		return nil, err
	}

	refreshCtx, cancel := context.WithTimeout(ctx, refreshSessionPRTimeout)
	defer cancel()

	prStatus, checkSet, err := s.refreshSessionPRSnapshot(refreshCtx, repo, sess, prNumber)
	if err != nil {
		return nil, err
	}
	if err := s.releaseHandedOffBlock(ctx, sess, prStatus, checkSet); err != nil {
		return nil, err
	}

	refreshed, err := s.sessions.Get(ctx, sess.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get refreshed session: %w", err))
	}
	p := s.sessionProtoWithRepo(ctx, refreshed)
	var displayEntry *status.DisplayEntry
	if s.displayTracker != nil {
		displayEntry = s.displayTracker.Get(refreshed.ID)
		HydrateDisplayEntry(p, displayEntry)
	}
	if s.onSessionUpdated != nil {
		s.onSessionUpdated(ctx, p)
	}
	// After the hook: the stream hook re-asserts the agent-observability overlay
	// in place, and the verify-park attention must rank below it (BOS-1382).
	HydrateVerifyAttention(p, displayEntry)
	return connect.NewResponse(&pb.RefreshSessionPRResponse{Session: p}), nil
}

func (s *Server) resolveRefreshSessionPRTarget(
	ctx context.Context, msg *pb.RefreshSessionPRRequest,
) (*models.Session, *models.Repo, int, error) {
	if msg == nil {
		return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id or pr_number is required"))
	}
	sessionID := strings.TrimSpace(msg.GetId())
	hasSessionID := msg != nil && msg.Id != nil && sessionID != ""
	hasPRNumber := msg != nil && msg.PrNumber != nil
	prNumber := int(msg.GetPrNumber())

	if !hasSessionID && !hasPRNumber {
		return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id or pr_number is required"))
	}
	if msg != nil && msg.Id != nil && strings.TrimSpace(msg.GetId()) == "" {
		return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id cannot be empty"))
	}
	if hasPRNumber && prNumber <= 0 {
		return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("pr_number must be positive"))
	}

	var sess *models.Session
	var err error
	if hasSessionID {
		sess, err = s.sessions.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, nil, 0, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
			}
			return nil, nil, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
		}
		if sess.PRNumber == nil || *sess.PRNumber <= 0 {
			return nil, nil, 0, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session has no linked PR"))
		}
		if hasPRNumber && *sess.PRNumber != prNumber {
			return nil, nil, 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id %q is linked to PR #%d, not PR #%d", sessionID, *sess.PRNumber, prNumber))
		}
		prNumber = *sess.PRNumber
	} else {
		sess, err = s.sessionByPRNumber(ctx, prNumber)
		if err != nil {
			return nil, nil, 0, err
		}
	}

	repo, err := s.repos.Get(ctx, sess.RepoID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, 0, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
		}
		return nil, nil, 0, connect.NewError(connect.CodeInternal, fmt.Errorf("get repo: %w", err))
	}
	if strings.TrimSpace(repo.OriginURL) == "" {
		return nil, nil, 0, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session repo has no origin URL"))
	}
	return sess, repo, prNumber, nil
}

func (s *Server) sessionByPRNumber(ctx context.Context, prNumber int) (*models.Session, error) {
	rows, err := s.sessions.ListActive(ctx, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list sessions: %w", err))
	}
	var match *models.Session
	for _, sess := range rows {
		if sess == nil || sess.PRNumber == nil || *sess.PRNumber != prNumber {
			continue
		}
		if match != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("PR #%d is linked to multiple sessions; pass id", prNumber))
		}
		match = sess
	}
	if match == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no session linked to PR #%d", prNumber))
	}
	return match, nil
}

func (s *Server) refreshSessionPRDisplay(ctx context.Context, repo *models.Repo, sess *models.Session, prNumber int) error {
	_, _, err := s.refreshSessionPRSnapshot(ctx, repo, sess, prNumber)
	return err
}

// refreshSessionPRSnapshot re-reads the PR into the display tracker and returns
// the PR status and check set it read.
func (s *Server) refreshSessionPRSnapshot(ctx context.Context, repo *models.Repo, sess *models.Session, prNumber int) (*vcs.PRStatus, vcs.CheckSet, error) {
	if s.provider == nil {
		return nil, vcs.CheckSet{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("refresh session PR: VCS provider is unavailable"))
	}
	if s.displayTracker == nil {
		return nil, vcs.CheckSet{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("refresh session PR: display tracker is unavailable"))
	}

	prStatus, err := s.provider.GetPRStatus(ctx, repo.OriginURL, prNumber)
	if err != nil {
		return nil, vcs.CheckSet{}, providerRefreshError(ctx, "PR status", err)
	}
	if prStatus == nil {
		return nil, vcs.CheckSet{}, connect.NewError(connect.CodeUnavailable, fmt.Errorf("refresh session PR: provider returned no PR status"))
	}

	var checkSet vcs.CheckSet
	var reviews []vcs.ReviewComment
	if prStatus.State != vcs.PRStateMerged && prStatus.State != vcs.PRStateClosed {
		checkSet, err = vcs.ReadCheckSet(ctx, s.provider, repo.OriginURL, prNumber)
		if err != nil {
			return nil, vcs.CheckSet{}, providerRefreshError(ctx, "check results", err)
		}
		if !prStatus.Draft {
			reviews, err = s.provider.GetReviewComments(ctx, repo.OriginURL, prNumber)
			if err != nil {
				return nil, vcs.CheckSet{}, providerRefreshError(ctx, "review comments", err)
			}
		}
	}

	info := vcs.ComputeDisplayStatusWith(prStatus, checkSet.Checks, reviews, vcs.DisplayOptions{DraftHeldByBossd: sess.State.HoldsBossdDraft(), Required: s.requiredChecksFor(ctx, repo.LocalPath)})
	info.HasBuildReceipt = checkSet.HasBuildReceipt
	info.BuildReceiptSeen = checkSet.BuildReceiptSeen
	info.HeadSHA = prStatus.HeadSHA
	if prStatus.State == vcs.PRStateOpen {
		info.IsPartial = vcs.IsPartialMarked(prStatus.Title, prStatus.Body)
	}
	info.Mergeable = prStatus.Mergeable
	info.AddedLineCount = prStatus.AddedLineCount
	info.DeletedLineCount = prStatus.DeletedLineCount
	info.ChangedFileCount = prStatus.ChangedFileCount
	s.displayTracker.Set(sess.ID, info)
	return prStatus, checkSet, nil
}

// handedOffPR reports whether the PR shows a finished build hand-off on its
// current head: open, out of draft, a boss/build success receipt and a
// boss/verify status in any state. The check set is read on the head, so a
// receipt or verify status left on an earlier head does not count.
func handedOffPR(prStatus *vcs.PRStatus, checkSet vcs.CheckSet) bool {
	if prStatus == nil || prStatus.State != vcs.PRStateOpen || prStatus.Draft || !checkSet.HasBuildReceipt {
		return false
	}
	for _, c := range checkSet.Checks {
		if c.Name == vcs.VerifyStatusContext {
			return true
		}
	}
	return false
}

// handOffEvents walk a Blocked session with a PR to ReadyForReview, the state
// finalize gives a handed-off PR. The machine has no direct edge from Blocked,
// so the walk is Unblock (to AwaitingChecks), ChecksPassed (to GreenDraft) and
// PlanComplete (out of draft, to ReadyForReview). Leaving Blocked clears the
// block reason and attempt count.
var handOffEvents = []machine.Event{machine.Unblock, machine.ChecksPassed, machine.PlanComplete}

// releaseHandedOffBlock moves a Blocked session whose PR shows a finished
// hand-off (handedOffPR) to ReadyForReview and clears its block metadata. A
// later bossd pass, such as a finalize that failed on incidental worktree
// churn, can block a session after its build handed the PR off, and nothing
// else clears that block while the PR stays open. Any other session, and a
// Blocked one whose PR lacks the evidence, is left exactly as it is.
//
// Blocked released the PR and branch, so the release runs under the
// start-path lock and is skipped when another live session has taken them.
func (s *Server) releaseHandedOffBlock(ctx context.Context, sess *models.Session, prStatus *vcs.PRStatus, checkSet vcs.CheckSet) error {
	if sess.State != machine.Blocked || !handedOffPR(prStatus, checkSet) {
		return nil
	}
	fresh, release, err := session.AcquireRevival(ctx, s.sessions, sess, session.RevivalLockTimeout, func(f *models.Session) bool {
		return f.State == machine.Blocked
	})
	if err != nil {
		s.logger.Warn().Err(err).Str("session", sess.ID).Msg("refresh session PR: handed-off PR, but the block was not released")
		return nil
	}
	defer release()

	sctx := &machine.SessionContext{AttemptCount: fresh.AttemptCount, MaxAttempts: machine.MaxAttempts, HasPR: true}
	if fresh.BlockedReason != nil {
		sctx.BlockedReason = *fresh.BlockedReason
	}
	sm := machine.NewWithContext(machine.Blocked, sctx)
	for _, event := range handOffEvents {
		if err := sm.FireCtx(ctx, event); err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("release handed-off block: fire %v: %w", event, err))
		}
	}

	state := int(sm.State())
	attempts := sm.Context().AttemptCount
	var clearReason, clearHeadSHA *string
	if _, err := s.sessions.Update(ctx, fresh.ID, domain.UpdateSessionParams{
		State:              &state,
		AttemptCount:       &attempts,
		BlockedReason:      &clearReason,
		LastAttemptHeadSHA: &clearHeadSHA,
	}); err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("release handed-off block: %w", err))
	}
	s.logger.Info().
		Str("session", fresh.ID).
		Str("new_state", sm.State().String()).
		Msg("refresh session PR: handed-off PR released a stale block")
	return nil
}

func providerRefreshError(ctx context.Context, read string, err error) error {
	code := connect.CodeUnavailable
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
		code = connect.CodeCanceled
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			code = connect.CodeDeadlineExceeded
		}
	}
	return connect.NewError(code, fmt.Errorf("refresh session PR: fetch %s: %w", read, err))
}
