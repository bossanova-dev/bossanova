package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/mergepolicy"
)

func (s *Server) MergeSession(ctx context.Context, req *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	// Head pin (BOS-1381): when set, the merge is refused with a HEAD_MISMATCH
	// FailedPrecondition unless the PR head is exactly this commit. Validated
	// before any read so a malformed pin never reaches the provider.
	var pin string
	if raw := req.Msg.GetExpectedHeadSha(); raw != "" {
		normalized, ok := vcs.NormalizeHeadSHA(raw)
		if !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("expected_head_sha must be a 40-character hex commit SHA"))
		}
		pin = normalized
	}

	return s.mergeSession(ctx, req.Msg.Id, mergeGate{pin: pin})
}

type mergeGate struct {
	pin, approvedHead string
}

func (s *Server) mergeSession(ctx context.Context, id string, g mergeGate) (*connect.Response[pb.MergeSessionResponse], error) {
	pin := g.pin

	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	// A local-only branch merge has no remote head to pin against. Fail closed
	// rather than merge unpinned; the message deliberately omits the
	// HEAD_MISMATCH token because nothing moved.
	if pin != "" && sess.PRNumber == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("expected_head_sha is only supported for PR merges"))
	}

	if sess.PRNumber != nil && pin == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("EXPECTED_HEAD_REQUIRED: pin the verified head with boss merge --match-head or MCP match_head"))
	}

	repo, err := s.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get repo: %w", err))
	}

	// Surface the transient blue "merging" display status for the full duration
	// of the blocking merge (gate + provider merge + verification). scheduleRecompute
	// is synchronous, so clients see "merging" before any blocking work starts;
	// the defer releases it on every return path (blocked gate, local/PR merge
	// success, or any error). Nil-guarded like lifecycle.go's SetSettingUp for
	// test servers without a display tracker.
	if s.displayTracker != nil {
		// The refresh below is unconditional within this block (which a server
		// wired without a display tracker never enters at all): a merge boss
		// just performed must not need a webhook or the next display-poller
		// tick to reflect its own action. (That tick is DisplayPollInterval —
		// 2 minutes by default, whatever poll_interval_seconds is set to
		// otherwise, and 5 minutes while the session is in webhook-healthy
		// backoff. This refresh deliberately does NOT arm that backoff: see
		// RefreshPRWithoutWebhookCredit.) Whether a polled
		// PR-status entry already existed or not, the pre-merge label the
		// tracker holds (e.g. "✓ passing") is stale the instant the merge
		// lands, so re-polling on every return path is the only way the display
		// the client goes on to read agrees with what just happened. On the
		// merge-only path (no prior entry — e.g. the post-restart window) the
		// re-poll gives the merge-only entry HoldMerging created a real PR status
		// before the release; if it fails, the release drops that entry, which
		// would otherwise leave recompute on the "stopped" fallback.
		//
		// The refresh runs under the hold and the release comes last, so clients
		// read "merging" until the re-polled label lands, never the pre-merge
		// one in between. The hold counts: an operator merge holding the same
		// session keeps "merging" past this release.
		//
		// The refresh runs synchronously inside this defer (not fired off
		// asynchronously) so the tracker can never be updated after this RPC
		// has already returned. Be aware of what that costs before adding to
		// it: the refresh runs a full pollSession, so on the success path it sees
		// the PR merged and reconciles the session's persisted state to a
		// terminal one, which notifies the task orchestrator and can
		// synchronously dequeue the repo's next task (up to a whole
		// CreateSession). Only an already-terminal tracker entry is a true
		// no-op — display_poller.go skips those; on the still-open blocked-gate
		// and error paths pollSession makes its status + checks + reviews reads
		// (one for a draft PR, which short-circuits; a fourth when it probes a
		// rebase-blocked PR's repairable-conflict strategy). Do
		// not try to bound this with a shorter context either: the orchestrator
		// hands this same ctx to CreateSession, so cancelling it mid-flight
		// would strand a half-created session.
		//
		// It also fires on the blocked-gate and error returns, deliberately: a
		// failed or blocked merge attempt can itself change what's true (e.g. a
		// check just failed, a review just landed), and the session stays
		// active on those paths, so it must show a true label rather than the
		// one from before the attempt. A refresh error here must never be
		// promoted to a merge failure — it only means the next scheduled poll
		// repairs the label instead of this one.
		//
		// Capture the PR number now rather than reading sess.PRNumber inside
		// the defer: the re-fetch near the end of this function REASSIGNS sess,
		// leaving it nil when that read fails, and the deferred deref would
		// then panic on a merge that already landed. Capturing also pins the PR
		// the merge actually operated on.
		var refreshPRNumber *int
		if sess.PRNumber != nil {
			prNumber := *sess.PRNumber
			refreshPRNumber = &prNumber
		}
		releaseMerging := s.displayTracker.HoldMerging(id)
		defer func() {
			defer releaseMerging()
			if s.prRefresher != nil && refreshPRNumber != nil {
				if err := s.prRefresher.RefreshPRWithoutWebhookCredit(ctx, repo.OriginURL, *refreshPRNumber); err != nil {
					s.logger.Debug().Err(err).
						Str("session_id", id).
						Msg("merge: post-merge pr refresh failed; next poll will repair")
				}
			}
		}()
	}

	// Serialize user-initiated merges per repo (BOS-439): concurrent merges on
	// the same repo share one local clone and would race on .git/index.lock and
	// refs/heads/<base>. Acquire AFTER HoldMerging (a queued merge still shows the
	// "merging" status while it waits) but BEFORE the live gate, so a merge that
	// queued behind another re-reads the PR fresh once the base has advanced.
	// A cancelled/timed-out queued merge bails here without merging.
	release, err := s.acquireRepoMerge(ctx, sess.RepoID)
	if err != nil {
		// Only reachable when the caller's ctx carries a deadline/cancel (a
		// direct-connect/TUI client) and it fires while queued. The reverse-stream
		// (web) path runs this handler on the long-lived stream ctx, so bosso's
		// mergeCommandDeadline bounds only bosso's wait for the reply, not this
		// ctx. Preserve timeout-vs-cancel semantics for the callers that do carry
		// a deadline: map DeadlineExceeded distinctly from a client cancel.
		code := connect.CodeCanceled
		if errors.Is(err, context.DeadlineExceeded) {
			code = connect.CodeDeadlineExceeded
		}
		return nil, connect.NewError(code,
			fmt.Errorf("merge abandoned while queued: %w", err))
	}
	defer release()

	// Sync the branch the PR actually targets, which may differ from the
	// repo's default branch (e.g. a PR into `develop`).
	baseBranch := sess.BaseBranch
	if baseBranch == "" {
		baseBranch = repo.DefaultBaseBranch
	}

	// Record how far this branch had drifted BEFORE the merge lands (BOS-521
	// acceptance metric) — afterwards the branch is on the base and the answer
	// is always 0.
	s.logCommitsBehindBaseAtMerge(ctx, repo, sess, baseBranch)

	// mergeDetail carries a human-readable note about HOW the merge was
	// performed (e.g. a strategy substitution). Empty when the merge ran
	// exactly as configured.
	var mergeDetail string

	// alreadyMerged short-circuits the merge+verify work below when the PR is
	// already merged upstream.
	var alreadyMerged bool

	if sess.PRNumber != nil {
		// Idempotency: a merge that already landed remotely but whose RPC
		// failed afterwards (a verification infra error, a dropped connection)
		// must stay retryable. Re-reading the PR here — after acquireRepoMerge,
		// so a merge queued behind another re-reads fresh — turns that retry
		// into a no-op success instead of a second MergePR the remote rejects.
		// A stale "merged" read is safe (merged is terminal); a stale "open"
		// read just proceeds to MergePR exactly as today.
		if merged, mergedHead := s.prAlreadyMerged(ctx, repo.OriginURL, *sess.PRNumber); merged {
			// A pinned caller must not be told its head merged when the PR
			// landed at another head. An unknown head keeps the idempotent
			// success; a matching head is the stranded-retry case.
			if pin != "" && mergedHead != "" && !strings.EqualFold(mergedHead, pin) {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					fmt.Errorf("%w: expected head %s, merged head %s", vcs.ErrHeadMismatch, pin, mergedHead))
			}
			alreadyMerged = true
			s.logger.Info().
				Str("session_id", id).
				Int("pr", *sess.PRNumber).
				Msg("merge: PR is already merged upstream; skipping the merge call (idempotent retry of a stranded merge)")
			// The stranded attempt may have died before its local sync, so
			// still run it best-effort.
			s.syncBaseAfterMerge(ctx, repo.LocalPath, baseBranch)
		} else {
			// Guard against merging a truly red/conflicted/rejected PR, but gate on a
			// LIVE read of the PR rather than the possibly-stale display tracker. A
			// stale Blocked (e.g. a false "fix loop exhausted") must never veto a
			// genuinely green+mergeable PR (BOS-235 Bug 2). A live read that shows
			// failing checks, an unresolved conflict, or outstanding changes-requested
			// rejects here; so does a read that could not be performed at all, which
			// blocks with gate=pending rather than merging an unverified state
			// (BOS-644).
			if err := s.checkMergeSessionGate(ctx, repo, sess, *sess.PRNumber, g); err != nil {
				return nil, err
			}
		}
	}

	switch {
	case alreadyMerged:
		// The remote merge and the local sync are done, but the #2222
		// base-ancestry guard must still run. Otherwise it survives only the
		// FIRST call: a merge that landed and then had its base rewritten
		// hard-fails with CodeInternal, drivers and the repair loop retry
		// CodeInternal, and the retry would short-circuit straight to success —
		// reporting a merge that did not land.
		if err := s.verifyAlreadyMergedOnBase(ctx, id, repo, baseBranch, *sess.PRNumber); err != nil {
			return nil, err
		}

	case sess.PRNumber == nil:
		// Local-only merge path: no PR on a remote, just a local session
		// branch that needs to land on the repo's base. The session's
		// branch name is authoritative.
		if sess.BranchName == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("session has no PR and no branch name to merge"))
		}
		if err := s.worktrees.MergeLocalBranch(ctx, repo.LocalPath, baseBranch, sess.BranchName, string(repo.MergeStrategy)); err != nil {
			if errors.Is(err, domain.ErrBaseBranchNotReady) {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					fmt.Errorf("merge blocked: gate=base_sync; %w", err))
			}
			if errors.Is(err, domain.ErrMergeConflict) {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					fmt.Errorf("merge blocked: gate=conflict; %w", err))
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("local merge: %w", err))
		}

	default:
		// The GitHub merge always completes regardless of the operator's local
		// checkout state; the local fast-forward of refs/heads/<base> is a
		// best-effort, deferrable step handled after the merge (see the
		// syncBaseAfterMerge call below), never a pre-merge gate.
		//
		// s.worktrees doubles as the merge-commit counter: rebase-merge is
		// structurally refused by GitHub on a branch containing merge commits,
		// so the resolver downgrades rebase to squash before we ever call
		// MergePR. A nil s.worktrees is a nil interface value and the resolver
		// fails open on it — no special case needed here.
		strategy, substituted, err := mergepolicy.ResolveStrategyForBranch(
			ctx, s.provider, repo.OriginURL, string(repo.MergeStrategy),
			s.worktrees, repo.LocalPath, baseBranch, sess.BranchName,
		)
		if err != nil {
			// ErrMergeStrategyIncompatible must map to FailedPrecondition, NOT
			// CodeInternal: drivers and the repair loop treat CodeInternal as
			// retryable and would loop forever on a combination that can never
			// succeed. Wrapping the error unmodified keeps the
			// MERGE_STRATEGY_INCOMPATIBLE token (and the merge-commit count) in
			// the message so callers can distinguish it. Every other resolver
			// error (e.g. ErrMergeStrategyDisallowed) keeps its existing
			// FailedPrecondition mapping.
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		if substituted != "" {
			mergeDetail = substituted
			s.logger.Warn().
				Str("session_id", id).
				Int("pr", *sess.PRNumber).
				Str("strategy", strategy).
				Str("reason", substituted).
				Msg("merge: substituted squash for rebase")
		}

		if err := s.provider.MergePR(ctx, repo.OriginURL, *sess.PRNumber, vcs.MergePROpts{Strategy: strategy, ExpectedHeadSHA: pin}); err != nil {
			// The remote refused a head-pinned merge because the PR head moved
			// (or has not yet settled after a push equal to the pin). Terminal:
			// never fall back to squash and never surface it as CodeInternal,
			// which drivers and the repair loop retry blindly.
			if errors.Is(err, vcs.ErrHeadMismatch) {
				return nil, s.remoteHeadMismatch(ctx, repo.OriginURL, *sess.PRNumber, pin, err)
			}
			// Backstop for a race the pre-check can miss (e.g. CountMergeCommits
			// failed open, or a merge commit landed between the count and the
			// merge): GitHub refused the rebase-merge itself. The policy for
			// what to do about that lives in mergepolicy, NOT here — a second
			// copy in this handler is exactly the drift the package exists to
			// prevent. Retry ONCE with squash when the policy says so; never loop.
			fallback, reason, policyErr := mergepolicy.SquashFallback(
				ctx, s.provider, repo.OriginURL, strategy, err,
			)
			switch {
			case policyErr != nil:
				// Same terminal-vs-retryable reasoning as the pre-check: no
				// fallback exists, so this is FailedPrecondition carrying both
				// the MERGE_STRATEGY_INCOMPATIBLE token and the provider's reason.
				return nil, connect.NewError(connect.CodeFailedPrecondition, policyErr)
			case fallback == "":
				// Not a rebase refusal, or the fallback could not be confirmed
				// (reason explains which). Either way, surface the original
				// merge failure — today's behaviour is the safe default.
				if reason != "" {
					// A non-empty reason with no fallback is SquashFallback's
					// documented ("", reason, nil) case, whose only cause today
					// is a failed allowed-strategies read — hence this message.
					// Keep it in sync if that contract grows a second cause.
					//
					// No Err() field here: the only error in scope is the
					// merge failure that is returned on the next line, and
					// logging it as THE error of this line would shadow the
					// actual cause, which reason carries.
					s.logger.Debug().
						Str("session_id", id).
						Str("reason", reason).
						Msg("merge: could not read allowed merge strategies for the squash fallback")
				}
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("merge PR: %w", err))
			}
			s.logger.Warn().Err(err).
				Str("session_id", id).
				Int("pr", *sess.PRNumber).
				Str("strategy", fallback).
				Msg("merge: rebase merge refused by the remote; retrying once with squash")
			if retryErr := s.provider.MergePR(ctx, repo.OriginURL, *sess.PRNumber, vcs.MergePROpts{Strategy: fallback, ExpectedHeadSHA: pin}); retryErr != nil {
				if errors.Is(retryErr, vcs.ErrHeadMismatch) {
					return nil, s.remoteHeadMismatch(ctx, repo.OriginURL, *sess.PRNumber, pin, retryErr)
				}
				// Surface BOTH failures: the retry error alone hides why a
				// second merge was attempted at all, which made the original
				// incident hard to diagnose from logs.
				return nil, connect.NewError(connect.CodeInternal,
					fmt.Errorf("merge PR: rebase refused (%v); %s retry: %w", err, fallback, retryErr))
			}
			mergeDetail = reason
		}

		// Verify the merge actually landed on origin/<base>. Catches
		// post-merge history rewrites, branch-protection races, and any
		// case where gh reported "merged" but the commit isn't where we
		// expect — the class of bug that previously went undetected for
		// days (madverts-core PR #2222).
		if err := mergepolicy.VerifyOnBase(ctx, s.provider, s.worktrees,
			repo.LocalPath, repo.OriginURL, baseBranch, *sess.PRNumber,
		); err != nil {
			// ErrMergeNotOnBase is a COMPLETED check with a negative answer —
			// the madverts-core PR #2222 incident class. This guard MUST NOT be
			// weakened: it stays a hard failure even when the API says merged.
			// Tested first, deliberately, so that guarantee never depends on
			// how the infra sentinel happens to wrap things.
			switch {
			case errors.Is(err, mergepolicy.ErrMergeNotOnBase):
				return nil, connect.NewError(connect.CodeInternal,
					fmt.Errorf("merge verification failed: %w", err))
			case errors.Is(err, mergepolicy.ErrMergeVerifyInfra) &&
				s.prMergedUpstream(ctx, repo.OriginURL, *sess.PRNumber):
				// The check could not COMPLETE (provider query, fetch, or git
				// invocation failed), but the API confirms the PR merged. Don't
				// strand a merge that demonstrably landed.
				s.logger.Warn().Err(err).
					Str("session_id", id).
					Int("pr", *sess.PRNumber).
					Msg("merge verified via API; local fetch failed")
			default:
				return nil, connect.NewError(connect.CodeInternal,
					fmt.Errorf("merge verification failed: %w", err))
			}
		}

		// Pull the merged commit into the user's local main repo so subsequent
		// worktrees, branches, and manual operations on <base> start from the
		// post-merge tip. Sync failures are logged — the server-side merge
		// is already verified to be on origin/<base>.
		s.syncBaseAfterMerge(ctx, repo.LocalPath, baseBranch)

		// The base just advanced: rebase the repo's other in-flight branches
		// onto it so drift never accumulates into an end-of-run conflict
		// (BOS-521). Opt-in per repo, and asynchronous — it queues behind this
		// handler's own hold on the per-repo merge gate.
		s.enqueueKeepCurrentSweep(repo, baseBranch, id)
	}

	// Re-fetch so callers see any server-side side effects. The state
	// transition to Merged is NOT among them: it arrives either from the
	// PR-merged webhook handled by session.Dispatcher, or from this handler's
	// own deferred display refresh — and that defer runs AFTER this read, so
	// the Session returned below can lag the display tracker by exactly that
	// one transition. Note this reassigns sess to nil on error, which is why
	// the deferred refresh above captured the PR number instead of reading it
	// off sess.
	sess, err = s.sessions.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	return connect.NewResponse(&pb.MergeSessionResponse{
		Session: SessionToProto(sess),
		Detail:  mergeDetail,
	}), nil
}
