package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/mergepolicy"
)

// prAlreadyMerged reports whether a LIVE read of the PR says it is already
// merged. Only a definitive "merged" answer returns true: on a nil provider, an
// empty origin, a provider error, or a missing PR status it returns false, so
// the caller does NOT short-circuit and proceeds down the normal merge path
// exactly as it does today. That is safe precisely because liveMergeBlock no
// longer fails open on those same uncertainties: an unreadable provider now
// blocks with gate=pending further down the path rather than merging blind.
//
// It also returns the merged PR's head SHA ("" when unknown) so a head-pinned
// caller can refuse a merge that landed at another head.
func (s *Server) prAlreadyMerged(ctx context.Context, originURL string, prNumber int) (bool, string) {
	if s.provider == nil || originURL == "" {
		return false, ""
	}
	prStatus, err := s.provider.GetPRStatus(ctx, originURL, prNumber)
	if err != nil || prStatus == nil || prStatus.State != vcs.PRStateMerged {
		return false, ""
	}
	return true, prStatus.HeadSHA
}

// prMergedUpstream is prAlreadyMerged without the head, for boolean contexts.
func (s *Server) prMergedUpstream(ctx context.Context, originURL string, prNumber int) bool {
	merged, _ := s.prAlreadyMerged(ctx, originURL, prNumber)
	return merged
}

// verifyAlreadyMergedOnBase runs the madverts-core PR #2222 base-ancestry check
// on MergeSession's idempotent short-circuit — the path taken when the provider
// already reports the PR as merged. It returns a connect error ONLY for
// ErrMergeNotOnBase; every other outcome is success.
//
// The asymmetry with the normal merge path is deliberate. Here the provider API
// has ALREADY confirmed the merge (that read is what selected this path), so a
// verification that could not COMPLETE — ErrMergeVerifyInfra from the merge-commit
// query, the fetch, or the ancestor check — tells us nothing we don't already
// know and must not strand a retry of a merge that demonstrably landed. Only the
// completed-check-with-a-negative-answer case ("the API says merged, but the
// commit is not on origin/<base>") is actionable, and that one stays a hard
// CodeInternal failure on every call, not just the first.
//
// Nil-guarded like syncBaseAfterMerge: a server without a provider or worktree
// manager cannot verify anything, so it keeps returning success.
func (s *Server) verifyAlreadyMergedOnBase(
	ctx context.Context, sessionID string, repo *models.Repo, baseBranch string, prNumber int,
) error {
	if s.provider == nil || s.worktrees == nil {
		return nil
	}
	err := mergepolicy.VerifyOnBase(ctx, s.provider, s.worktrees,
		repo.LocalPath, repo.OriginURL, baseBranch, prNumber)
	if err == nil {
		return nil
	}
	if errors.Is(err, mergepolicy.ErrMergeNotOnBase) {
		return connect.NewError(connect.CodeInternal,
			fmt.Errorf("merge verification failed: %w", err))
	}
	s.logger.Warn().Err(err).
		Str("session_id", sessionID).
		Int("pr", prNumber).
		Msg("merge already confirmed merged by the API; local re-verification could not complete")
	return nil
}

// syncBaseAfterMerge pulls the merged commit into the user's local main repo so
// subsequent worktrees, branches, and manual operations on <base> start from the
// post-merge tip. Best-effort: the remote merge is already done, so failures are
// logged rather than returned. Nil-guarded for test servers without a worktree
// manager.
func (s *Server) syncBaseAfterMerge(ctx context.Context, localPath, baseBranch string) {
	if s.worktrees == nil {
		return
	}
	if err := s.worktrees.SyncBaseBranch(ctx, localPath, baseBranch); err != nil {
		if errors.Is(err, domain.ErrLocalSyncDeferred) {
			// Non-fatal: origin/<base> is already freshened; the local
			// fast-forward is recorded and retried once the base checkout
			// is clean. The merge itself is done and verified on base.
			s.logger.Info().Err(err).
				Str("repo", localPath).
				Str("base", baseBranch).
				Msg("local base sync deferred: base checked out with uncommitted changes; will fast-forward once clean")
		} else {
			s.logger.Warn().Err(err).
				Str("repo", localPath).
				Str("base", baseBranch).
				Msg("post-merge sync of local base branch failed; run `git fetch` + fast-forward manually")
		}
	}
}

// liveMergeBlock returns a structured merge-block reason when a fresh read of
// the PR shows a state that must block a merge (failing checks, an unresolved
// conflict, or outstanding changes-requested), or nil when the merge may
// proceed. It reads LIVE provider state (GetPRStatus + GetCheckResults +
// GetReviewComments) rather than the possibly-stale display tracker, so a stale
// Blocked (e.g. a false fix-loop-exhaustion) never wedges a genuinely
// green+mergeable PR (BOS-235 Bug 2). Only definitively-bad live states (Failing
// / Conflict / Rejected as computed by vcs.ComputeDisplayStatus) block; still-
// settling CI or a not-yet-approved PR does not veto an explicit merge request.
// When it does block, it derives the same structured reason as the get/list
// surface (vcs.DeriveMergeBlock) so the refusal carries a gate + human detail
// (BOS-239) computed from the live state, not the stale tracker, plus the head
// SHA the decision was computed against.
//
// It fails CLOSED on an unverifiable state (BOS-644): when any of the three
// reads errors against a live context, or GetPRStatus returns a nil status with
// a nil error, it returns a GATE_PENDING block naming the read that failed
// rather than nil. A read that did not happen is not evidence that nothing is
// blocking, and MergePR is not a backstop for this class — the review gate
// exists precisely because GitHub can report a PR mergeable while the daemon's
// own tracker knows it is not.
//
// A nil provider or an empty origin stay no-ops: they mean "this deployment has
// no provider / this session has no remote", not "the read failed".
//
// Pending verify claims and parks do not belong to the block set: the caller
// separately requires a passed verdict or an approval recorded at the pin.
// The provider merge carries that pin to bind these sequential reads to its head.
// The legacy stream transport remains unpinned until BOS-2064.
//
// When expectedHead is non-empty and the live PR head is known and differs, it
// returns an error wrapping vcs.ErrHeadMismatch (and no block) right after the
// GetPRStatus read, skipping the remaining reads: the gate's verdict would be
// about a different head, so the mismatch takes precedence. An empty live head
// is unknown, not mismatched; the merge proceeds and the remote pin is the
// backstop.
//
// Otherwise it returns a non-nil error, and no block, when the CALLER's context
// was canceled or timed out mid-read. A canceled read is not an unverifiable gate:
// nobody is waiting for the answer, and reporting it as GATE_PENDING would
// convert the caller's own cancellation into a FailedPrecondition telling them
// to retry once the provider is reachable — losing the Canceled /
// DeadlineExceeded semantics MergeSession preserves everywhere else. The caller
// maps the error back to the matching Connect code.
func (s *Server) liveMergeBlock(ctx context.Context, originURL string, prNumber int, expectedHead string, blocks blockingStatuses) (*vcs.MergeBlockReason, mergeVerdict, error) {
	if s.provider == nil || originURL == "" {
		return nil, mergeVerdict{}, nil
	}
	// unverifiable reports a read failure as a GATE_PENDING block, except when
	// the context died under it: then the failure is the caller going away, not
	// the provider being unreachable, so the context error is returned instead.
	unverifiable := func(read string, err error) (*vcs.MergeBlockReason, mergeVerdict, error) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, mergeVerdict{}, ctxErr
		}
		return unverifiableMergeBlock(read, err), mergeVerdict{}, nil
	}

	prStatus, err := s.provider.GetPRStatus(ctx, originURL, prNumber)
	if err != nil {
		return unverifiable("PR status", err)
	}
	if prStatus == nil {
		return unverifiable("PR status", errors.New("provider returned no PR status"))
	}
	if expectedHead != "" && prStatus.HeadSHA != "" && !strings.EqualFold(prStatus.HeadSHA, expectedHead) {
		return nil, mergeVerdict{}, fmt.Errorf("%w: expected head %s, live head %s", vcs.ErrHeadMismatch, expectedHead, prStatus.HeadSHA)
	}
	checks, err := s.provider.GetCheckResults(ctx, originURL, prNumber)
	if err != nil {
		return unverifiable("check results", err)
	}
	reviews, err := s.provider.GetReviewComments(ctx, originURL, prNumber)
	if err != nil {
		return unverifiable("review comments", err)
	}
	s.warnOnNoReviewsObserved(ctx, prStatus, originURL, prNumber)
	// The observation is a FOURTH context-bound provider read, and it runs after
	// the three guarded ones. It swallows its own errors on purpose (an
	// unreadable tally must not block a merge the gate approved) — but a dead
	// context is not an ordinary observation failure. Swallowing that one would
	// let the merge proceed on an expired context until MergePR failed and was
	// reported as CodeInternal, losing the cancellation semantics the guards
	// above exist to preserve. Ordinary failures stay ignored; only the context
	// is re-checked.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, mergeVerdict{}, ctxErr
	}
	info := vcs.ComputeDisplayStatus(prStatus, checks, reviews)
	verdict := mergeVerdict{verified: vcs.VerifyPassed(checks), info: info}
	if !blocks[info.Status] {
		return nil, verdict, nil
	}
	mb := vcs.DeriveMergeBlock(info.Status, info.HasFailures, info.ChangesRequestedBy, info.VerifyReason)
	if prStatus.HeadSHA != "" {
		mb.Detail += fmt.Sprintf("; gated at head %s", prStatus.HeadSHA)
	}
	return &mb, verdict, nil
}

// blockingStatuses is the set of live display statuses a merge gate refuses.
type blockingStatuses map[vcs.DisplayStatus]bool

// mergeSessionBlocks refuses definitive failures. Checking, Verifying and
// NeedsHuman require a verdict or a head-bound approval in the caller.
var mergeSessionBlocks = blockingStatuses{
	vcs.DisplayStatusFailing:  true,
	vcs.DisplayStatusConflict: true,
	vcs.DisplayStatusRejected: true,
}

// operatorApprovalBlocks is what the operator merge refuses before it records
// an approval: everything MergeSession refuses, a draft, which GitHub will not
// merge, and checks still running, which must pass before anyone signs off.
var operatorApprovalBlocks = blockingStatuses{
	vcs.DisplayStatusFailing:  true,
	vcs.DisplayStatusConflict: true,
	vcs.DisplayStatusRejected: true,
	vcs.DisplayStatusDraft:    true,
	vcs.DisplayStatusChecking: true,
}

// remoteHeadMismatch maps a remote refusal of a head-pinned merge (an error
// wrapping vcs.ErrHeadMismatch from MergePR) to the typed HEAD_MISMATCH
// FailedPrecondition. It re-reads the PR head once, best-effort, so the caller
// can tell a real move from GitHub's few-second lag after a push that equals
// the pin. The daemon never retries: the caller re-verifies.
func (s *Server) remoteHeadMismatch(ctx context.Context, originURL string, prNumber int, pin string, mergeErr error) error {
	live := "unknown"
	if st, err := s.provider.GetPRStatus(ctx, originURL, prNumber); err == nil && st != nil && st.HeadSHA != "" {
		live = st.HeadSHA
	}
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("%w: expected head %s, live head %s; remote refused the merge: %v",
			vcs.ErrHeadMismatch, pin, live, mergeErr))
}

// unverifiableMergeBlock builds the block returned when the live merge gate
// could not read the state it must judge. GATE_PENDING already means
// "mergeability unknown", which is exactly this condition. Nothing retries it
// automatically: the merge is held until the caller retries.
func unverifiableMergeBlock(read string, err error) *vcs.MergeBlockReason {
	return &vcs.MergeBlockReason{
		Gate:   vcs.MergeGatePending,
		Detail: fmt.Sprintf("the live merge gate could not read the %s (%v), so the PR's mergeability is unverified; retry once the provider is reachable", read, err),
	}
}

// warnOnNoReviewsObserved logs when the live merge gate evaluated a non-draft PR
// and observed no bot review at all. It runs before the block decision, so it
// also fires on an evaluation that then blocks for some other reason (failing
// CI, a conflict) — the absent reviewer is worth surfacing either way. The gate
// can only block on evidence an
// external reviewer produced, so an uninstalled, rate-limited, or silent review
// bot makes the safety net empty — indistinguishable, in the logs, from a PR
// that passed a real review. It keys on *bot* reviews rather than all reviews on
// purpose: a lone human approval with no bot review is precisely the silent-loss
// case this warning exists to surface. Log-only — MergeSessionResponse.detail is
// discarded by every RPC consumer.
func (s *Server) warnOnNoReviewsObserved(ctx context.Context, prStatus *vcs.PRStatus, originURL string, prNumber int) {
	if prStatus.Draft {
		return
	}
	// The tally MUST come from the raw, pre-filtering observation, never from
	// the []ReviewComment the gate judges. GetReviewComments deliberately drops
	// a bot's COMMENTED review once that bot's threads are all resolved, so
	// counting bots in its result reports "no reviewer observed" on exactly the
	// addressed, ready-to-merge PRs where the reviewer did run — firing on the
	// healthy path and never distinguishing the absent-reviewer condition this
	// warning exists to surface.
	observer, ok := s.provider.(vcs.ReviewObserver)
	if !ok {
		// This provider does no actionable filtering to see behind, so there is
		// no raw observation to consult and nothing trustworthy to warn about.
		return
	}
	obs, err := observer.GetReviewObservation(ctx, originURL, prNumber)
	if err != nil {
		// Observability only: an unreadable tally means "we cannot tell whether
		// a reviewer ran", which is not evidence that none did. Note it and move
		// on — this read is NOT part of the fail-closed gate decision, so it
		// must never block a merge the three gate reads approved.
		s.logger.Debug().Err(err).
			Int("pr", prNumber).
			Msg("merge gate: could not read the raw review observation; skipping the no-reviews-observed check")
		return
	}
	if obs.Bot > 0 {
		return
	}
	s.logger.Warn().
		Str("review_gate", "no_reviews_observed").
		Int("pr", prNumber).
		Int("reviews_total", obs.Total).
		Int("bot_reviews_total", obs.Bot).
		Msg("merge gate: no external bot review observed on this PR; the review gate had nothing to block on")
}

// mergeVerdict is derived from the same check read as the live block decision.
type mergeVerdict struct {
	verified bool
	info     vcs.DisplayInfo
}

func noMergeVerdict(pin string, pr int, info vcs.DisplayInfo) error {
	var detail string
	switch info.Status {
	case vcs.DisplayStatusNeedsHuman:
		detail = "; needs human: " + info.VerifyReason
	case vcs.DisplayStatusVerifying:
		detail = "; this head is being verified"
	default:
		detail = ""
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%w: no verdict on %s: boss/verify has not passed this head%s; run verify (/boss-verify %d) or approve and merge it (boss merge --approve <session>, or [m]erge on its TUI row)", vcs.ErrNoVerdict, pin, detail, pr))
}

func (s *Server) checkMergeSessionGate(ctx context.Context, repo *models.Repo, sess *models.Session, pr int, g mergeGate) error {
	id := sess.ID
	refreshCtx, cancel := context.WithTimeout(ctx, refreshSessionPRTimeout)
	if err := s.refreshSessionPRDisplay(refreshCtx, repo, sess, pr); err != nil {
		s.logger.Debug().Err(err).
			Str("session_id", id).
			Int("pr", pr).
			Msg("merge: pre-gate PR refresh failed; live gate will re-read")
	}
	cancel()
	mb, verdict, err := s.liveMergeBlock(ctx, repo.OriginURL, pr, g.pin, mergeSessionBlocks)
	if err != nil {
		// A known head mismatch is a terminal refusal, not a dead
		// context: test it first so it maps to FailedPrecondition.
		if errors.Is(err, vcs.ErrHeadMismatch) {
			return connect.NewError(connect.CodeFailedPrecondition, err)
		}
		// The gate read died with the caller's context, not against an
		// unreachable provider. Preserve the cancellation semantics the
		// queued-merge wait above already preserves, rather than telling
		// a caller who hung up to retry when the provider recovers.
		code := connect.CodeCanceled
		if errors.Is(err, context.DeadlineExceeded) {
			code = connect.CodeDeadlineExceeded
		}
		return connect.NewError(code,
			fmt.Errorf("merge abandoned while checking the live merge gate: %w", err))
	}
	if mb != nil {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("merge blocked: gate=%s; %s", mb.Gate.Slug(), mb.Detail))
	}
	if !verdict.verified && g.approvedHead != g.pin {
		return noMergeVerdict(g.pin, pr, verdict.info)
	}
	return nil
}
