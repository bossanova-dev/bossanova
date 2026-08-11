package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	libvcs "github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	daemontelemetry "github.com/recurser/bossd/internal/telemetry"
)

// FinalizeResult is the outcome of a FinalizeSession call. Outcome maps 1:1
// to the cron_jobs.last_run_outcome column FinalizeSession writes. NoOp is
// true when the conditional state transition no-op'd (duplicate Stop event
// or a session that never reached ImplementingPlan) — the hook endpoint
// returns 200 either way, but the caller uses this to log the distinction.
// Deleted is true when the finalize path already removed the session row and
// worktree, so the caller must not record the deleted session ID or transition
// the (now-gone) row to Blocked.
type FinalizeResult struct {
	Outcome models.CronJobOutcome
	NoOp    bool
	Deleted bool
	// Err, when non-nil, is the underlying failure behind a *_failed outcome.
	// It is not returned as a top-level error so the hook endpoint still
	// reports success — the outcome column is already recorded.
	Err error
}

// FinalizeSession runs the Stop-hook finalize pipeline for a cron-spawned
// session. It is idempotent: duplicate Stop events no-op via a conditional
// state transition (ImplementingPlan → Finalizing) guarded by rows_affected.
//
// IMPORTANT: callers must not invoke this just because a Stop hook fired. The
// Claude Stop hook fires at the end of EVERY main-agent turn — including mid-run
// pauses (e.g. the agent yielding while a background subagent runs) — so a Stop
// is a per-turn hint, not a completion signal. Run-completion gating lives in
// CronCompletionGate (cronRunIsOver); finalizing a still-working run opens a junk
// PR and Blocks a live session. The Stop-hook path funnels through that gate and
// this entry, which accepts only ImplementingPlan. The
// recoverStrandedCronSessions sweep does NOT go through the gate: it applies its
// own liveness gate (strandedRunIsDead) and calls finalizeSessionFrom directly
// with the broadened reap set, so it can advance a session interrupted in
// PushingBranch/OpeningDraftPR/etc. — states this Stop-hook entry deliberately
// still no-ops on.
//
// Cron runs are autonomous: there is no second "Finalize" chat. After the PR
// is opened, bossd injects the PR number into the (tagless) commit subjects and
// force-pushes (injectPRTagsAndPush), the deterministic in-process replacement
// for the old /boss-finalize chat's add-pr-numbers.sh step.
//
// Outcome classification, in the order it's evaluated:
//   - pr_created               — empty git status but branch already has an open PR; lookup errors are best-effort and fall through to deleted_no_changes
//   - deleted_no_changes       — empty git status and no committed branch work → worktree + session removed
//   - cleanup_failed           — empty status but worktree removal errored
//   - pr_skipped_no_github     — changes present, origin is not GitHub:
//     cron session → hard-deleted (Archive + Delete, Deleted=true)
//     interactive  → preserved, transitions to Blocked
//   - pr_failed                — dirty output present but the PR could not be opened
//   - pr_created               — dirty output: a PR was opened (placeholder commit added when the branch had none); PR tags injected
//   - pr_failed                — clean committed branch could not open or attach a PR
//   - pr_created               — clean committed branch opened/attached a PR; PR tags injected
//
// After the outcome is classified, FinalizeSession writes
// cron_jobs.last_run_outcome (step 4) and, on the pr_created success path,
// clears session.hook_token so a replayed Stop event can no longer
// authenticate against this session (step 5). Failure outcomes also fire the
// Block state-machine event so the session shows up as attention-needed in
// the UI — the Finalizing state itself is intentionally silent per
// vcs/attention.go.
func (l *Lifecycle) FinalizeSession(ctx context.Context, sessionID string) (*FinalizeResult, error) {
	// The Stop-hook/gate path stays ImplementingPlan-only — a per-turn Stop is
	// only meaningful there. The broadened reap set lives on the sweep's own
	// entry (finalizeSessionFrom, via recoverStrandedCronSessions).
	return l.finalizeSessionFrom(ctx, sessionID, []int{int(machine.ImplementingPlan)})
}

// finalizeSessionFrom runs the finalize pipeline, advancing to Finalizing only
// when the session's current state is one of expectedStates. It is the shared
// body behind both finalize entries: FinalizeSession (the Stop-hook/gate path,
// ImplementingPlan-only) and the recoverStrandedCronSessions sweep (the
// broadened reap set). Everything after step 1 — classify, record outcome,
// clear token, block-on-failure — is identical regardless of the accepted
// from-set.
func (l *Lifecycle) finalizeSessionFrom(ctx context.Context, sessionID string, expectedStates []int) (*FinalizeResult, error) {
	// Step 1: conditional state transition. The rows_affected guard is the
	// authoritative idempotency mechanism — a check-then-set Go path would
	// race with concurrent Stop events.
	advanced, err := l.sessions.UpdateStateConditionalFrom(
		ctx, sessionID, int(machine.Finalizing), expectedStates,
	)
	if err != nil {
		return nil, fmt.Errorf("advance to finalizing: %w", err)
	}
	if !advanced {
		return &FinalizeResult{NoOp: true}, nil
	}

	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("worktree", session.WorktreePath).
		Msg("finalizing session")

	// A zero-output cron session deliberately runs in the user's repository
	// checkout. Do this before classification: classifyFinalizeOutcome starts by
	// calling worktrees.Status and could otherwise inspect, push, or create a PR
	// from that checkout.
	var result *FinalizeResult
	if isCronSession(session) && session.IsQuickChat {
		repo, repoErr := l.repos.Get(ctx, session.RepoID)
		if repoErr != nil {
			return nil, fmt.Errorf("get repo for zero-output finalize: %w", repoErr)
		}
		if deleteErr := l.hardDeleteSession(ctx, session, repo); deleteErr != nil {
			result = &FinalizeResult{Outcome: models.CronJobOutcomeCleanupFailed, Err: deleteErr}
		} else {
			result = &FinalizeResult{Outcome: models.CronJobOutcomeZeroOutput, Deleted: true}
		}
	} else {
		// Step 2 + 3: classify outcome by examining the worktree.
		result = l.classifyFinalizeOutcome(ctx, session)
	}

	// Step 4: record the outcome on the cron job row. Non-cron sessions
	// (the headless-completion path in finalizeHeadlessRunIfApplicable now
	// finalizes these too) carry no CronJobID and are skipped here rather than
	// treated as errors.
	if session.CronJobID != nil && *session.CronJobID != "" && l.cronJobs != nil {
		ranAt := time.Now()

		// For deleted paths (deleted_no_changes, or cron no-github hard-delete),
		// the session row was already deleted. SQLite's ON DELETE SET NULL cascade
		// has already nulled out cron_jobs.last_run_session_id; passing the deleted
		// ID to UpdateLastRun would re-violate the FK constraint, so we leave the
		// session-ID field alone (nil = don't update).
		var recordedSessionIDPtr *string
		expectedSessionID := sessionID
		allowClearedExpectedSessionID := false
		if !result.Deleted {
			recordedSessionID := sessionID
			recordedSessionIDPtr = &recordedSessionID
		} else {
			allowClearedExpectedSessionID = true
		}

		// Guard the write against a newer run: if this session went idle before
		// finalizing, the next tick may have already fired a fresh run and moved
		// last_run_session_id forward (the lost-hook overlap case). Recording
		// this older outcome would point the job back at the now-stale session,
		// so the overlap check would inspect idle this-session and launch more
		// runs while the newer one is still working. ExpectedSessionID makes the
		// write conditional on this session still being the recorded last run.
		// deleted_no_changes leaves recordedSessionIDPtr nil because the session
		// row is gone, but still guards against a newer run. Session deletion may
		// already have cleared last_run_session_id to NULL, so that cleared value
		// is allowed only for this deleted-session path.
		if err := l.cronJobs.UpdateLastRun(ctx, *session.CronJobID, db.UpdateCronJobLastRunParams{
			SessionID:                     recordedSessionIDPtr,
			ExpectedSessionID:             &expectedSessionID,
			AllowClearedExpectedSessionID: allowClearedExpectedSessionID,
			RanAt:                         ranAt,
			Outcome:                       result.Outcome,
		}); err != nil {
			if errors.Is(err, db.ErrCronJobLastRunSuperseded) {
				// A newer run owns last_run_session_id; this older finalize must
				// not move it back. Benign — log at info and continue.
				l.logger.Info().
					Str("session", sessionID).
					Str("cronJob", *session.CronJobID).
					Str("outcome", string(result.Outcome)).
					Msg("finalize: cron last-run superseded by newer run; skipping outcome write")
			} else {
				// Outcome classification already succeeded; log and continue.
				l.logger.Error().Err(err).
					Str("session", sessionID).
					Str("cronJob", *session.CronJobID).
					Str("outcome", string(result.Outcome)).
					Msg("failed to record cron job last-run outcome")
			}
		}
	}

	// Step 5: clear hook_token on success only, so a replayed Stop event can
	// no longer authenticate as this session. The PR was already marked ready
	// by the classifier, so move the session out of Finalizing before daemon
	// startup recovery can mistake it for an interrupted finalize.
	if result.Outcome == models.CronJobOutcomePRCreated {
		var nilToken *string
		readyState := int(machine.ReadyForReview)
		if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
			HookToken: &nilToken,
			State:     &readyState,
		}); err != nil {
			l.logger.Error().Err(err).
				Str("session", sessionID).
				Msg("failed to finalize session ready state")
		}
	}

	// Step 6: on failure outcomes (except deleted paths, which removed the
	// session entirely), persist a descriptive blocked reason and then
	// transition to Blocked so the session surfaces as attention-needed in
	// the UI. Finalizing itself is suppressed in ComputeAttentionStatus, so
	// leaving a failed session there would hide the problem. Deleted sessions
	// (Deleted=true) have no row to transition — skip them regardless of outcome.
	if needsAttention(result.Outcome) && !result.Deleted {
		reason := sessionreason.FinalizeFailure(string(result.Outcome), result.Err)
		reasonPtr := &reason
		if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
			BlockedReason: &reasonPtr,
		}); err != nil {
			l.logger.Error().Err(err).Str("session", sessionID).
				Msg("failed to persist finalize blocked reason")
		}
		if _, err := l.sessions.UpdateStateConditional(
			ctx, sessionID, int(machine.Blocked), int(machine.Finalizing),
		); err != nil {
			l.logger.Error().Err(err).
				Str("session", sessionID).
				Str("outcome", string(result.Outcome)).
				Msg("failed to transition failed finalize to blocked")
		}
	}

	outcome := "error"
	switch result.Outcome {
	case models.CronJobOutcomePRCreated:
		outcome = "pr_opened"
	case models.CronJobOutcomeDeletedNoChanges,
		models.CronJobOutcomePRNoChanges,
		models.CronJobOutcomeZeroOutput,
		models.CronJobOutcomeWorktreeGone:
		outcome = "no_changes"
	case models.CronJobOutcomePRFailed,
		models.CronJobOutcomePRSkippedNoGitHub,
		models.CronJobOutcomeChatSpawnFailed,
		models.CronJobOutcomeCleanupFailed,
		models.CronJobOutcomeFailedRecovered,
		models.CronJobOutcomeFireFailed,
		models.CronJobOutcomeGated:
		outcome = "error"
	}
	daemontelemetry.Capture(ctx, l.telemetry, libtelemetry.EventSessionFinalized, map[string]any{
		"outcome": outcome, "agent": telemetryAgent(session.AgentName), "unattended": isUnattendedSession(session),
	})

	return result, nil
}

func telemetryAgent(name string) string {
	switch name {
	case "claude", "codex", "opencode":
		return name
	default:
		return "other"
	}
}

// injectPRTagsAndPush rewrites the session's tagless commit subjects to carry
// the resolved PR number and force-pushes the result. It replaces the work the
// old bossd-spawned "Finalize" chat did by running the /boss-finalize skill's
// add-pr-numbers.sh: cron runs commit tagless, and the commit-message policy
// (commitlint pr-tag rule) requires "[#<PR>]" on non-protected branches.
//
// Failures are returned to the finalize classifier. Once bossd stopped
// spawning a separate "Finalize" chat, this in-process step became the only
// place that both applies required PR tags and pushes the session branch.
func (l *Lifecycle) injectPRTagsAndPush(ctx context.Context, sessionID string) error {
	cur, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).
			Msg("finalize: refresh session before PR-tag injection failed")
		return fmt.Errorf("refresh session before PR-tag injection: %w", err)
	}
	if cur.PRNumber == nil || *cur.PRNumber <= 0 {
		l.logger.Warn().Str("session", sessionID).
			Msg("finalize: no PR number resolved; skipping PR-tag injection")
		return fmt.Errorf("no PR number resolved for PR-tag injection")
	}
	if cur.BranchName == "" || cur.BaseBranch == "" || cur.WorktreePath == "" {
		l.logger.Warn().Str("session", sessionID).
			Msg("finalize: missing branch/base/worktree; skipping PR-tag injection")
		return fmt.Errorf("missing branch/base/worktree for PR-tag injection")
	}

	// Repair a cron session still titled with the cron job name by adopting its
	// PR's GitHub title. Done before the dirty-worktree check below can fail the
	// injection, so a placeholder / pr_failed finalize still leaves a meaningful
	// session title — every other rename path is gated on PRNumber == nil and is
	// bypassed once the agent's PR already exists.
	if updatedSess, changed, syncErr := adoptPRTitleWhenCronTitleStale(
		ctx, l.sessions, l.repos, l.cronJobs, l.provider, l.logger, cur); syncErr != nil {
		l.logger.Warn().Err(syncErr).Str("session", sessionID).
			Msg("finalize: cron title sync from PR failed")
	} else if changed {
		cur = updatedSess
	}

	status, err := l.worktrees.Status(ctx, cur.WorktreePath)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).
			Msg("finalize: status before PR-tag injection failed")
		return fmt.Errorf("status before PR-tag injection: %w", err)
	}
	managedPaths := l.managedDirtyPaths(ctx, cur, "ListIgnoredDirtyFiles failed before PR-tag injection")
	if trimmed := strings.TrimSpace(stripBossdManagedFilesWith(status, managedPaths)); trimmed != "" {
		if porcelainHasTrackedChanges(trimmed) {
			l.logger.Warn().
				Str("session", sessionID).
				Str("status", trimmed).
				Msg("finalize: worktree has uncommitted tracked changes; refusing PR-tag injection")
			return fmt.Errorf("worktree has uncommitted changes before PR-tag injection")
		}
		// Only untracked scratch remains (e.g. a stray plan file the agent forgot
		// to commit). That is not implementation work, so allow PR-tag injection
		// to proceed while preserving those files in the worktree.
		l.logger.Warn().
			Str("session", sessionID).
			Str("status", trimmed).
			Msg("finalize: only untracked leftovers remain; proceeding with PR-tag injection")
	}

	// Inject against the freshly-fetched remote base so the merge-base reflects
	// the branch's true divergence point.
	if err := l.worktrees.FetchBase(ctx, cur.WorktreePath, cur.BaseBranch); err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).
			Msg("finalize: fetch base before PR-tag injection failed")
		return fmt.Errorf("fetch base before PR-tag injection: %w", err)
	}
	baseRef := "refs/remotes/origin/" + cur.BaseBranch
	if err := l.worktrees.InjectPRNumbers(ctx, cur.WorktreePath, cur.BranchName, *cur.PRNumber, baseRef); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Int("pr", *cur.PRNumber).
			Msg("finalize: PR-tag injection failed; PR commits may not satisfy commitlint")
		return fmt.Errorf("inject PR tags and push: %w", err)
	}
	l.logger.Info().
		Str("session", sessionID).
		Int("pr", *cur.PRNumber).
		Msg("finalize: injected PR number into commit subjects")
	return nil
}

func (l *Lifecycle) markFinalizePRReady(ctx context.Context, sessionID, originURL string) error {
	cur, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("refresh session before mark ready: %w", err)
	}
	if cur.PRNumber == nil || *cur.PRNumber <= 0 {
		return fmt.Errorf("no PR number resolved for mark ready")
	}
	if err := l.assertPRHasDiffBeforeMarkReady(ctx, cur); err != nil {
		return err
	}
	if err := l.provider.MarkReadyForReview(ctx, originURL, *cur.PRNumber); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Int("pr", *cur.PRNumber).
			Msg("finalize: mark PR ready failed")
		return fmt.Errorf("mark PR ready: %w", err)
	}
	l.logger.Info().
		Str("session", sessionID).
		Int("pr", *cur.PRNumber).
		Msg("finalize: marked PR ready for review")
	return nil
}

// errEmptyDiffRefusedReady marks the backstop's refusal so the finalize call
// sites can classify it as pr_no_changes rather than pr_failed. Nothing broke —
// the PR was opened and pushed fine; the run simply produced no changes — and
// pr_failed must keep meaning "something went wrong". Both outcomes are
// attention outcomes (needsAttention), so the session still Blocks either way;
// only the recorded reason differs.
var errEmptyDiffRefusedReady = errors.New("empty diff against base")

// markReadyFailureOutcome maps a markFinalizePRReady error to the outcome that
// describes it truthfully: the BOS-591 empty-diff refusal is a no-changes run,
// anything else is a genuine PR failure.
func markReadyFailureOutcome(err error) models.CronJobOutcome {
	if errors.Is(err, errEmptyDiffRefusedReady) {
		return models.CronJobOutcomePRNoChanges
	}
	return models.CronJobOutcomePRFailed
}

// assertPRHasDiffBeforeMarkReady is BOS-591's last line of defence: a PR whose
// diff against its base is empty is vacuously green (no code to fail CI), so
// marking it ready presents a failed run as merge-ready. The empty-run guard in
// attachExistingPRIfCleanBranchHasOne is the primary defence; this check sits
// inside markFinalizePRReady — the single funnel for all three of *finalize's*
// mark-ready call sites — so it holds regardless of how finalize arrived there.
// It deliberately covers cron sessions too (decision D2): what stays cron-exempt
// is the *guard*, not this backstop. A cron branch that genuinely changes
// nothing should not be advertised as reviewable. Note that widens cron's
// dirty-output-with-no-commits path from pr_created to pr_no_changes — that PR
// was exactly the vacuous-green shape this ticket exists to stop.
//
// It is NOT the only MarkReadyForReview call in bossd: Dispatcher.handleChecksPassed
// (dispatcher.go) also marks a green draft ready when repo.CanAutoMerge, and
// that path does not run this check. It is unreachable for a session this
// backstop refused — Blocked does not Permit ChecksPassed (machine.go) — but do
// not read this comment as "an empty PR can never be marked ready anywhere".
//
// The caller guarantees cur.PRNumber is non-nil and positive (markFinalizePRReady
// validates it immediately above); this function dereferences it unguarded.
//
// Known limitation: it compares against the SESSION's base branch, not the PR's
// current base on GitHub. attachExistingPRIfCleanBranchHasOne attaches whatever
// open PR GitHub reports for the branch, and libvcs.PRSummary does not carry the
// PR's base at all (decodePRList never asks `gh pr list` for baseRefName), so
// there is nothing to sync session.BaseBranch from. A PR retargeted on GitHub is
// therefore evaluated against the wrong ref, and it breaks BOTH ways: a branch
// empty against the PR's real base but differing from the session's slips
// through, and — the user-visible direction — a branch empty against the
// session's stale base but carrying real work against the PR's live base is
// wrongly refused and Blocked.
//
// This is pre-existing and consistent with injectPRTagsAndPush, which
// merge-bases against session.BaseBranch too. Closing it is not a one-line
// assignment: the live base only comes from a GetPRStatus round-trip
// (vcs.PRStatus.BaseBranch), plus a second FetchBase for a base ref that may not
// exist locally — two network hops on a path that deliberately avoids even one.
// Wider than BOS-591. Do not read this backstop as base-retarget-proof.
//
// It deliberately does NOT fetch the base: injectPRTagsAndPush runs immediately
// before markFinalizePRReady at every call site and already calls FetchBase (see
// injectPRTagsAndPush above), so refs/remotes/origin/<base> is fresh. Adding a
// second fetch here would duplicate a network round-trip on the hot path.
//
// Failure semantics are asymmetric on purpose, matching this file's convention
// that a git error never blocks a legitimate run: an EMPTY diff is definite
// evidence of a no-op run and is refused, but an UNREADABLE diff is not evidence
// of emptiness, so a HasDiffAgainstBase error warns and proceeds (fail open).
// Missing base/worktree fields are likewise skipped rather than refused — they
// have their own validation in injectPRTagsAndPush.
func (l *Lifecycle) assertPRHasDiffBeforeMarkReady(ctx context.Context, cur *models.Session) error {
	if cur.BaseBranch == "" || cur.WorktreePath == "" {
		l.logger.Warn().
			Str("session", cur.ID).
			Str("branch", cur.BranchName).
			Int("pr", *cur.PRNumber).
			Msg("finalize: missing base/worktree; skipping empty-diff check before mark ready")
		return nil
	}
	baseRef := "refs/remotes/origin/" + cur.BaseBranch
	hasDiff, err := l.worktrees.HasDiffAgainstBase(ctx, cur.WorktreePath, baseRef)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", cur.ID).
			Str("branch", cur.BranchName).
			Int("pr", *cur.PRNumber).
			Str("base_ref", baseRef).
			Msg("finalize: empty-diff check failed before mark ready; proceeding (fail open)")
		return nil
	}
	if !hasDiff {
		l.logger.Warn().
			Str("session", cur.ID).
			Str("branch", cur.BranchName).
			Int("pr", *cur.PRNumber).
			Str("base_ref", baseRef).
			Msg("finalize: refusing to mark PR ready — it has an empty diff against its base")
		return fmt.Errorf("refusing to mark PR #%d ready (%s): %w", *cur.PRNumber, baseRef, errEmptyDiffRefusedReady)
	}
	return nil
}

// bossdManagedWorktreeFiles lists files bossd writes into the worktree
// during session setup. They appear in `git status --porcelain` (typically
// untracked) but are NOT Claude-authored changes and must not influence
// the finalize outcome. The Stop-hook config additionally contains a
// bearer token, so misclassifying it as a Claude change would risk
// pushing credentials to the remote via EnsurePR.
//
// A parallel global-gitignore effort is plumbing these paths through a
// shared excludesFile per worktree; this filter is the in-process
// belt-and-suspenders so a regression there can't re-introduce the
// pr_failed → Blocked failure observed for "do nothing" cron runs.
var bossdManagedWorktreeFiles = []string{
	".claude/settings.local.json",
	".claude/scheduled_tasks.lock",
}

func (l *Lifecycle) managedDirtyPaths(ctx context.Context, session *models.Session, warnMsg string) []string {
	managedPaths := append([]string(nil), bossdManagedWorktreeFiles...)
	client, err := l.agentClientFor(session)
	if err != nil {
		return managedPaths
	}
	resp, rpcErr := client.ListIgnoredDirtyFiles(ctx, &bossanovav1.ListIgnoredDirtyFilesRequest{
		WorkDir: session.WorktreePath,
	})
	if rpcErr != nil {
		l.logger.Warn().Err(rpcErr).Msg(warnMsg)
		return managedPaths
	}
	if resp != nil {
		managedPaths = append(managedPaths, resp.Paths...)
	}
	return managedPaths
}

// stripBossdManagedFilesWith removes porcelain entries for any of the given
// managedPaths before the empty-status check. Lines are "XY path" (two
// status chars, a space, then the pathspec); we slice past the 3-char
// prefix and trim trailing whitespace before comparing. Rename entries
// ("R  old -> new") are rare and never originate from bossd, so are left
// untouched.
//
// Callers should pass the union of bossd-owned paths and any agent-plugin-
// contributed paths returned by AgentRunnerService.ListIgnoredDirtyFiles.
func stripBossdManagedFilesWith(porcelain string, managedPaths []string) string {
	if porcelain == "" {
		return ""
	}
	managed := make(map[string]struct{}, len(managedPaths))
	for _, p := range managedPaths {
		managed[p] = struct{}{}
	}
	var kept []string
	for line := range strings.SplitSeq(porcelain, "\n") {
		if len(line) >= 4 {
			path := strings.TrimSpace(line[3:])
			if _, drop := managed[path]; drop {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// porcelainHasTrackedChanges reports whether any line of `git status
// --porcelain` output represents a change to a tracked file (modification,
// staged add, delete, rename, etc.) as opposed to a purely untracked entry.
// Untracked entries are prefixed with "??"; everything else touches tracked
// state. Used by the finalize guard to distinguish genuine incomplete work
// (tracked changes — must block) from stray scratch artifacts (untracked-only —
// safe to skip past).
func porcelainHasTrackedChanges(porcelain string) bool {
	for line := range strings.SplitSeq(porcelain, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "??") {
			return true
		}
	}
	return false
}

// worktreeIsMissing reports whether a failed `git status` was caused by the
// worktree path being gone (the session's worktree was archived/removed) rather
// than a genuine git failure against a live worktree. It is deliberately
// conservative — a genuine git failure at a still-present path must NOT be
// swallowed as benign:
//   - if os.Stat confirms the path is PRESENT, the failure is treated as a live
//     (non-benign) failure regardless of the git error text — a corrupt or
//     de-initialized worktree (e.g. a deleted .git that makes git report "not a
//     git repository") must surface as pr_failed, not be reclassified as gone;
//   - os.Stat reporting the path does not exist is the authoritative "gone"
//     signal;
//   - only when the path cannot be confirmed present (empty path, or os.Stat
//     itself errored for another reason) does the git error text naming a
//     missing repository/path stand in as a fallback, for the race where the
//     directory vanished mid-finalize.
func worktreeIsMissing(path string, statusErr error) bool {
	if path != "" {
		switch _, err := os.Stat(path); {
		case err == nil:
			// The worktree path is present on disk, so a failed `git status`
			// here is a genuine failure against a live worktree. Do not let the
			// error-text fallback below (which matches common strings like "no
			// such file or directory") reclassify a confirmed-present path as
			// gone.
			return false
		case os.IsNotExist(err):
			return true
		}
		// Stat errored for another reason (e.g. permission); fall through to the
		// error-text fallback so a race where the path can't be inspected is
		// still handled.
	}
	if statusErr == nil {
		return false
	}
	msg := strings.ToLower(statusErr.Error())
	return strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "no such file or directory")
}

// classifyFinalizeOutcome runs steps 2 and 3 of the finalize pipeline: it
// inspects the worktree and routes to the cleanup, no-github, PR-failed, or
// PR-created branch. It never returns an error — unrecoverable failures are
// folded into the Outcome column so the caller always records something.
func (l *Lifecycle) classifyFinalizeOutcome(ctx context.Context, session *models.Session) *FinalizeResult {
	status, err := l.worktrees.Status(ctx, session.WorktreePath)
	if err != nil {
		if worktreeIsMissing(session.WorktreePath, err) {
			// The worktree is already gone — e.g. the session was
			// archived/removed (ArchiveSession deletes the worktree but leaves
			// the row in an implementing state) before a late Stop hook or a
			// stranded-cron sweep reached finalize. There is nothing left to
			// finalize; this is a benign no-op, not a PR failure. Record it as
			// worktree_gone so the session is NOT surfaced as attention-needed
			// with a scary pr_failed blocked reason.
			return &FinalizeResult{
				Outcome: models.CronJobOutcomeWorktreeGone,
				Err:     fmt.Errorf("worktree gone: %w", err),
			}
		}
		// The path exists but git status failed for some other reason; we can't
		// tell whether there were changes, so treat as recoverable pr_failed so
		// the user can investigate the worktree manually.
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     fmt.Errorf("git status: %w", err),
		}
	}

	// Drop bossd-owned and agent-plugin-owned files from the porcelain
	// output before the empty-check. The agent plugin declares which paths
	// it manages (e.g. .claude/settings.local.json) via ListIgnoredDirtyFiles;
	// we fall back to the hardcoded list when no client is loaded for this
	// session's agent (e.g. legacy rows whose agent_name doesn't match a
	// loaded plugin).
	status = stripBossdManagedFilesWith(status, l.managedDirtyPaths(ctx, session, "ListIgnoredDirtyFiles failed"))

	if strings.TrimSpace(status) == "" {
		if result := l.attachExistingPRIfCleanBranchHasOne(ctx, session); result != nil {
			return result
		}
		if result := l.createPRIfCleanBranchHasCommittedWork(ctx, session); result != nil {
			return result
		}
		return l.finalizeNoChanges(ctx, session)
	}

	// Changes exist — route on GitHub linkage.
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     fmt.Errorf("get repo: %w", err),
		}
	}
	if !libvcs.IsGitHubURL(repo.OriginURL) {
		// cron session finalize
		//   ├─ no commits ──────────────► finalizeNoChanges(): Archive + Delete            [EXISTING]
		//   ├─ commits, GitHub origin ──► EnsurePR() → pr_created / pr_failed(keep,Blocked) [EXISTING]
		//   └─ commits, NO GitHub origin─► cron: hard-delete (Archive+Delete);  [NEW]
		//                                  interactive: preserve (pr_skipped_no_github)
		if isCronSession(session) {
			l.logger.Info().
				Str("session", session.ID).
				Str("origin", repo.OriginURL).
				Msg("finalize: cron session has changes but origin is not GitHub; hard-deleting")
			if err := l.hardDeleteSession(ctx, session, repo); err != nil {
				return &FinalizeResult{
					Outcome: models.CronJobOutcomeCleanupFailed,
					Err:     err,
				}
			}
			return &FinalizeResult{Outcome: models.CronJobOutcomePRSkippedNoGitHub, Deleted: true}
		}
		l.logger.Info().
			Str("session", session.ID).
			Str("origin", repo.OriginURL).
			Msg("finalize: changes present but origin is not GitHub; preserving worktree")
		return &FinalizeResult{Outcome: models.CronJobOutcomePRSkippedNoGitHub}
	}

	// Cron sessions defer PR creation at start (opts.DeferPR), so dirty output
	// can reach here with no PR, and the branch may have no commits beyond base
	// when the agent left only uncommitted changes. EnsurePR can't open a PR
	// for a zero-commit branch (GitHub rejects it with "no commits between")
	// and deliberately makes no placeholder commit. Add one in that case —
	// mirroring the session-start draft PR path — so the run still gets a PR.
	//
	// The agent's own uncommitted changes are intentionally NOT committed here:
	// under BOSS_CRON the agent is told to commit its own work, so leftover
	// uncommitted changes mean an incomplete run. They are preserved in the
	// worktree for inspection rather than auto-committed with a synthetic
	// message.
	hasCommittedWork, err := l.cleanBranchHasCommittedWork(ctx, session, true)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: committed-work check failed for dirty cron output; preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     err,
		}
	}
	if !hasCommittedWork {
		l.logger.Warn().
			Str("session", session.ID).
			Msg("finalize: cron run left uncommitted changes but committed nothing; opening placeholder PR and preserving the worktree changes for inspection")
		if err := l.worktrees.EmptyCommit(ctx, session.WorktreePath, draftPRPlaceholderCommitSubject); err != nil {
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: placeholder commit failed for dirty cron output; preserving worktree")
			return &FinalizeResult{
				Outcome: models.CronJobOutcomePRFailed,
				Err:     err,
			}
		}
	}

	// EnsurePR is idempotent: it no-ops when the session already has a PR,
	// and otherwise pushes the branch and opens a draft PR.
	if err := l.EnsurePR(ctx, session.ID); err != nil {
		if attached, attachErr := l.attachBranchPRAfterEnsurePRError(ctx, session, repo); attached {
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: EnsurePR failed after branch PR became visible; continuing")
		} else {
			if attachErr != nil {
				err = fmt.Errorf("%w; attach branch PR after EnsurePR failure: %v", err, attachErr)
			}
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: EnsurePR failed for dirty cron output; preserving worktree")
			return &FinalizeResult{
				Outcome: models.CronJobOutcomePRFailed,
				Err:     err,
			}
		}
	}

	// Safety net: EnsurePR may have created the PR on GitHub but failed to
	// persist the number (network drop after create, race with a concurrent
	// opener, etc.). Re-fetch the session and, if PRNumber is still unset,
	// fall back to an active branch-PR lookup so the UI shows "#N" not "-".
	if cur, err := l.sessions.Get(ctx, session.ID); err == nil && cur.PRNumber == nil {
		if _, attachErr := l.attachOpenPRForBranch(ctx, session.ID, cur, repo); attachErr != nil {
			l.logger.Warn().Err(attachErr).Str("session", session.ID).
				Msg("finalize: post-EnsurePR PR association failed; poller will retry")
		}
	}

	if err := l.injectPRTagsAndPush(ctx, session.ID); err != nil {
		return &FinalizeResult{Outcome: models.CronJobOutcomePRFailed, Err: err}
	}
	if err := l.markFinalizePRReady(ctx, session.ID, repo.OriginURL); err != nil {
		return &FinalizeResult{Outcome: markReadyFailureOutcome(err), Err: err}
	}

	return &FinalizeResult{Outcome: models.CronJobOutcomePRCreated}
}

func (l *Lifecycle) attachBranchPRAfterEnsurePRError(ctx context.Context, session *models.Session, repo *models.Repo) (bool, error) {
	cur, err := l.sessions.Get(ctx, session.ID)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: failed to refresh session before post-EnsurePR branch PR lookup")
		cur = session
	}
	if cur.PRNumber != nil {
		return true, nil
	}

	return l.attachOpenPRForBranch(ctx, session.ID, cur, repo)
}

// isPlanningOnlyNoChangeSession reports whether a session is an explicit
// planning-only quick chat (BOS-322). Such sessions open no worktree/branch/PR
// and are expected to make no repository changes, so finalize must treat their
// no-change result as a benign success rather than a failed implementation run.
// Keyed on the persisted IsQuickChat flag — never on title/prompt text, which are
// user-editable and would make the gate fragile.
func isPlanningOnlyNoChangeSession(session *models.Session) bool {
	return session != nil && session.IsQuickChat
}

// attachExistingPRIfCleanBranchHasOne attaches a pre-existing open PR for a
// clean branch when one can be found. Initial lookup errors are non-fatal, but
// attachment/update/persistence errors after a PR match are pr_failed outcomes.
func (l *Lifecycle) attachExistingPRIfCleanBranchHasOne(ctx context.Context, session *models.Session) *FinalizeResult {
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     fmt.Errorf("get repo: %w", err),
		}
	}
	if !libvcs.IsGitHubURL(repo.OriginURL) {
		return nil
	}

	found, err := l.attachOpenPRForBranch(ctx, session.ID, session, repo)
	if err != nil {
		// Best-effort lookup only. A clean worktree means the run made no
		// changes; if the initial PR list request fails (transient gh/GitHub
		// error, wrong-account token), fall through so the no-committed-work
		// path reaches finalizeNoChanges (deleted_no_changes).
		if !errors.Is(err, errListOpenPRs) {
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Str("branch", session.BranchName).
				Msg("finalize: existing-PR attachment failed for clean worktree; preserving session")
			return &FinalizeResult{
				Outcome: models.CronJobOutcomePRFailed,
				Err:     err,
			}
		}
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("branch", session.BranchName).
			Msg("finalize: existing-PR lookup failed for clean worktree; treating as no-changes")
		return nil
	}
	if !found {
		return nil
	}

	// A clean branch whose only commit is the empty draft-PR bootstrap commit
	// did no real work, even though a PR exists. For a non-cron (headless
	// detach) run — the /boss-epic fan-out path — do not attach + mark it ready as
	// a green PR: record a no-changes outcome so the session Blocks and a
	// headless driver fail-isolates it instead of merging an empty PR. Cron
	// finalize is left byte-identical: its no-op runs are already handled by the
	// deleted_no_changes / placeholder-commit paths, and this gate is skipped
	// for cron sessions. A git error fails open toward "real work" so a
	// transient failure never wrongly blocks a legitimate run.
	if !isCronSession(session) {
		// Every branch of this guard logs its decision — including the
		// real-work path. BOS-591's incident was undiagnosable after the fact
		// precisely because the "this run did real work" outcome was silent,
		// making a bypassed guard indistinguishable from a guard that never ran.
		if hasReal, realCount, realErr := l.branchHasRealCommits(ctx, session); realErr != nil {
			l.logger.Warn().Err(realErr).
				Str("session", session.ID).
				Str("branch", session.BranchName).
				Msg("finalize: real-commit check failed for clean branch with PR; treating as real work")
		} else if hasReal {
			l.logger.Info().
				Str("session", session.ID).
				Str("branch", session.BranchName).
				Int("real_commits", realCount).
				Msg("finalize: clean branch has an existing PR with real commits; proceeding to mark it ready")
		} else {
			// Planning-only sessions (quick chats: recon, plan review, visible
			// /boss-plan) are expected to produce no repository changes, so a
			// no-real-commits result is a benign success — not a failed
			// implementation run. Divert them to the deleted_no_changes cleanup
			// path instead of pr_no_changes/Blocked (BOS-322). Real quick chats
			// skip finalize entirely; this is the defensive backstop for any that
			// reach it. True empty /boss-build runs (IsQuickChat false) still
			// fall through to pr_no_changes and Block.
			if isPlanningOnlyNoChangeSession(session) {
				l.logger.Info().
					Str("session", session.ID).
					Str("branch", session.BranchName).
					Msg("finalize: planning-only no-change session completed without implementation output")
				return l.finalizeNoChanges(ctx, session)
			}
			l.logger.Info().
				Str("session", session.ID).
				Str("branch", session.BranchName).
				Msg("finalize: clean branch has an existing PR but no real commits; recording no-op headless run")
			return &FinalizeResult{
				Outcome: models.CronJobOutcomePRNoChanges,
				Err:     errors.New("headless run produced no changes beyond the empty draft-PR bootstrap commit"),
			}
		}
	}

	l.logger.Info().
		Str("session", session.ID).
		Str("branch", session.BranchName).
		Msg("finalize: attached existing branch PR for clean worktree")
	if err := l.injectPRTagsAndPush(ctx, session.ID); err != nil {
		return &FinalizeResult{Outcome: models.CronJobOutcomePRFailed, Err: err}
	}
	if err := l.markFinalizePRReady(ctx, session.ID, repo.OriginURL); err != nil {
		return &FinalizeResult{Outcome: markReadyFailureOutcome(err), Err: err}
	}
	return &FinalizeResult{Outcome: models.CronJobOutcomePRCreated}
}

func (l *Lifecycle) createPRIfCleanBranchHasCommittedWork(ctx context.Context, session *models.Session) *FinalizeResult {
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     fmt.Errorf("get repo: %w", err),
		}
	}
	originURL, err := l.originURLIfConfigured(ctx, repo)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("branch", session.BranchName).
			Msg("finalize: origin lookup failed for clean committed branch; preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     err,
		}
	}
	isGitHubOrigin := libvcs.IsGitHubURL(originURL)
	hasCommittedWork, err := l.cleanBranchHasCommittedWork(ctx, session, isGitHubOrigin)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("branch", session.BranchName).
			Msg("finalize: committed work lookup failed; preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     err,
		}
	}
	if !hasCommittedWork {
		return nil
	}

	// NOTE: the empty-vs-real-work gate lives only in the sibling
	// attachExistingPRIfCleanBranchHasOne path, not here. A no-op headless
	// /boss-epic run always carries bossd's bootstrap draft PR, so it is classified
	// by that attach path (checked first in classifyFinalizeOutcome) before this
	// no-existing-PR path is ever reached; a branch that is HEAD-ahead-of-base
	// by only the empty bootstrap commit but has no PR is not a shape session
	// start produces (the bootstrap commit and PR are created together). Keeping
	// the gate out of here also preserves this path's existing behavior for
	// interactive non-GitHub sessions byte-identical.

	if !isGitHubOrigin {
		// Same routing as the dirty-output no-github branch in
		// classifyFinalizeOutcome: a cron session whose committed work can never
		// become a PR is hard-deleted; interactive sessions are preserved.
		if isCronSession(session) {
			l.logger.Info().
				Str("session", session.ID).
				Str("branch", session.BranchName).
				Str("origin", originURL).
				Msg("finalize: clean branch has committed work but origin is not GitHub; hard-deleting cron session")
			if err := l.hardDeleteSession(ctx, session, repo); err != nil {
				return &FinalizeResult{
					Outcome: models.CronJobOutcomeCleanupFailed,
					Err:     err,
				}
			}
			return &FinalizeResult{Outcome: models.CronJobOutcomePRSkippedNoGitHub, Deleted: true}
		}
		l.logger.Info().
			Str("session", session.ID).
			Str("branch", session.BranchName).
			Str("origin", originURL).
			Msg("finalize: clean branch has committed work but origin is not GitHub; preserving worktree")
		return &FinalizeResult{Outcome: models.CronJobOutcomePRSkippedNoGitHub}
	}

	l.logger.Info().
		Str("session", session.ID).
		Str("branch", session.BranchName).
		Msg("finalize: clean branch has committed work; creating PR")
	if err := l.EnsurePR(ctx, session.ID); err != nil {
		if attached, attachErr := l.attachBranchPRAfterEnsurePRError(ctx, session, repo); attached {
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: EnsurePR failed after branch PR became visible; continuing finalization")
		} else {
			if attachErr != nil {
				err = fmt.Errorf("%w; attach branch PR after EnsurePR failure: %v", err, attachErr)
			}
			l.logger.Warn().Err(err).
				Str("session", session.ID).
				Msg("finalize: EnsurePR failed for clean committed branch; preserving worktree")
			return &FinalizeResult{
				Outcome: models.CronJobOutcomePRFailed,
				Err:     err,
			}
		}
	}

	// Safety net: same race as the dirty-output path — EnsurePR may have
	// opened the PR on GitHub but the PRNumber write was dropped. Re-fetch
	// and attach if still unset so the UI always shows the PR number.
	if cur, err := l.sessions.Get(ctx, session.ID); err == nil && cur.PRNumber == nil {
		if _, attachErr := l.attachOpenPRForBranch(ctx, session.ID, cur, repo); attachErr != nil {
			l.logger.Warn().Err(attachErr).Str("session", session.ID).
				Msg("finalize: post-EnsurePR PR association failed; poller will retry")
		}
	}

	if err := l.injectPRTagsAndPush(ctx, session.ID); err != nil {
		return &FinalizeResult{Outcome: models.CronJobOutcomePRFailed, Err: err}
	}
	if err := l.markFinalizePRReady(ctx, session.ID, repo.OriginURL); err != nil {
		return &FinalizeResult{Outcome: markReadyFailureOutcome(err), Err: err}
	}

	return &FinalizeResult{Outcome: models.CronJobOutcomePRCreated}
}

func (l *Lifecycle) cleanBranchHasCommittedWork(ctx context.Context, session *models.Session, fetchBase bool) (bool, error) {
	if session.BranchName == "" {
		return false, nil
	}
	if session.BaseBranch == "" {
		return false, fmt.Errorf("base branch is empty for clean branch %q", session.BranchName)
	}

	baseRef := session.BaseBranch
	if fetchBase {
		if err := l.worktrees.FetchBase(ctx, session.WorktreePath, session.BaseBranch); err != nil {
			return false, fmt.Errorf("fetch base branch: %w", err)
		}
		baseRef = "refs/remotes/origin/" + session.BaseBranch
	}
	headInBase, err := l.worktrees.IsAncestor(ctx, session.WorktreePath, "HEAD", baseRef)
	if err != nil {
		return false, fmt.Errorf("check branch commits against %s: %w", baseRef, err)
	}
	return !headInBase, nil
}

// branchHasRealCommits reports whether the session's branch carries any commit
// beyond bossd's empty draft-PR bootstrap commit — i.e. the run produced real
// work rather than leaving only the placeholder. It reuses realCommitSubjects
// (the same placeholder filter the cron PR-title path uses), but — unlike
// finalizeTitle's use of CommitSubjects — against the freshly-fetched
// remote-tracking base ref (refs/remotes/origin/<base>), not the bare local
// base branch name. A freshly-created worktree's local base branch can be
// stale (behind the remote), so `git log <local-base>..HEAD` would also list
// every commit merged to the real base since that local ref was last updated —
// making a bootstrap-only branch look like it did real work and defeating this
// guard. The non-cron (headless detach) finalize paths use it to refuse to
// surface a no-op run as a green PR: a bootstrap-only branch is
// HEAD-ahead-of-base (so cleanBranchHasCommittedWork and an attached bootstrap
// PR both read as "work"), yet its diff vs the true base is empty. A
// legitimate tiny/docs change carries a real commit and still passes. A
// FetchBase failure is returned as an error (never silently ignored, and never
// falls back to the stale local ref) so the caller's fail-open contract kicks
// in and treats the run as real work rather than risk a false "no changes".
// realCount is the number of non-placeholder commits the check counted; the
// caller logs it so a future guard bypass is diagnosable from the log alone.
func (l *Lifecycle) branchHasRealCommits(ctx context.Context, session *models.Session) (hasReal bool, realCount int, err error) {
	if session.BaseBranch == "" {
		return false, 0, fmt.Errorf("base branch is empty for branch %q", session.BranchName)
	}
	// NOTE: this now starts with FetchBase, and a fetch is a ref WRITE. An empty
	// worktree path is rejected by git.Manager.FetchBase itself (guarded there
	// so every call site is covered, not just this one); the error surfaces
	// here and the caller's fail-open contract treats the run as real work.
	if err := l.worktrees.FetchBase(ctx, session.WorktreePath, session.BaseBranch); err != nil {
		return false, 0, fmt.Errorf("fetch base branch: %w", err)
	}
	baseRef := "refs/remotes/origin/" + session.BaseBranch
	subjects, err := l.worktrees.CommitSubjects(ctx, session.WorktreePath, baseRef)
	if err != nil {
		return false, 0, fmt.Errorf("read commit subjects: %w", err)
	}
	realCount = len(realCommitSubjects(subjects))
	return realCount > 0, realCount, nil
}

func (l *Lifecycle) originURLIfConfigured(ctx context.Context, repo *models.Repo) (string, error) {
	if repo.OriginURL != "" {
		return repo.OriginURL, nil
	}

	url, err := l.worktrees.DetectOriginURL(ctx, repo.LocalPath)
	if err != nil {
		return "", fmt.Errorf("detect origin URL: %w", err)
	}
	if url == "" {
		return "", nil
	}

	if _, err := l.repos.Update(ctx, repo.ID, db.UpdateRepoParams{
		OriginURL: &url,
	}); err != nil {
		return "", fmt.Errorf("persist origin URL: %w", err)
	}

	l.logger.Info().
		Str("repo", repo.ID).
		Str("originURL", url).
		Msg("re-detected and persisted origin URL")

	repo.OriginURL = url
	return url, nil
}

// hardDeleteSession performs the Archive → killAllChatTmuxSessions → Delete →
// notifier sequence shared by the no-changes and no-github-cron paths. It
// returns nil on success. On failure the caller should demote the outcome to
// cleanup_failed and preserve the session row for investigation.
//
// The worktree guard (WorktreePath != "" && != repo.LocalPath) is applied here
// so both callers get identical protection. The no-github caller is cron-gated;
// finalizeNoChanges is not — since the headless-completion path routes non-cron
// `--detach` runs through FinalizeSession, a clean-worktree interactive session
// reaches it too (which is why the draft-PR stop above is load-bearing).
func (l *Lifecycle) hardDeleteSession(ctx context.Context, session *models.Session, repo *models.Repo) error {
	// Load-bearing, not defensive (BOS-540). Only the second caller (the
	// no-GitHub-origin branch) is cron-gated; finalizeNoChanges is reached by
	// ANY clean-worktree session, including a non-cron `--detach` run, which
	// runs with DeferPR=false and therefore DOES have a background draft-PR step
	// in flight. Without this the step would go on to push the branch and open a
	// PR for a session this function has just deleted.
	l.StopBackgroundDraftPR(ctx, session.ID)
	// Re-read after the join, as ArchiveSession and RemoveSession do: the step
	// can correct a drifted BranchName on its way out, and the reap below is
	// unconditional, so a stale name would delete nothing and leak the real
	// branch.
	if fresh, freshErr := l.sessions.Get(ctx, session.ID); freshErr == nil {
		session = fresh
	}

	if session.WorktreePath != "" && session.WorktreePath != repo.LocalPath {
		if err := l.worktrees.Archive(ctx, session.WorktreePath); err != nil {
			return fmt.Errorf("archive worktree: %w", err)
		}

		// BOS-424: reap the session's LOCAL branch on hard-delete, regardless of
		// repo.CanAutoDeleteBranches. A no-change cron run is hard-deleted here
		// (finalizeNoChanges), never through ArchiveSession, so BOS-180's reap
		// never runs and the orphaned cron-* branch leaks. The session row is
		// already being deleted — there is nothing to resurrect — so reap it
		// unconditionally. The BranchSafeToDelete guard inside reapSafeLocalBranch
		// still protects the shared commits-no-origin caller: an unmerged branch
		// (commits ahead of base) reads as not-safe and is kept. Best-effort:
		// the worktree is already gone, so a delete failure must not fail the
		// hard-delete (outcome stays deleted_no_changes).
		l.reapSafeLocalBranch(ctx, session.ID, repo, session)
	}

	// Tear down any per-chat tmux sessions BEFORE deleting the session row.
	// agent_chats.session_id has ON DELETE CASCADE, so once the row is gone
	// we lose the tmux_session_name needed to find and kill the tmux session
	// — leaving a stranded `claude` process with no DB pointer back to it.
	l.killAllChatTmuxSessions(ctx, session.ID)

	if err := l.sessions.Delete(ctx, session.ID); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	if l.sessionDeletedNotifier != nil {
		l.sessionDeletedNotifier(ctx, session.ID)
	}
	return nil
}

// finalizeNoChanges handles the deleted_no_changes branch: remove the
// worktree and delete the session row. Any error demotes the outcome to
// cleanup_failed and preserves the session row so the user can investigate.
func (l *Lifecycle) finalizeNoChanges(ctx context.Context, session *models.Session) *FinalizeResult {
	l.logger.Info().
		Str("session", session.ID).
		Msg("finalize: no changes; removing worktree and session")

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     fmt.Errorf("get repo: %w", err),
		}
	}

	if err := l.hardDeleteSession(ctx, session, repo); err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     err,
		}
	}

	return &FinalizeResult{Outcome: models.CronJobOutcomeDeletedNoChanges, Deleted: true}
}

// RecoverFinalizingSessions handles daemon-startup recovery: any session left
// in Finalizing from a previous crash can't be safely re-driven (we don't
// know whether EnsurePR ran, or whether the finalize chat was spawned), so
// we record failed_recovered on the cron_job, transition the session to
// Blocked so it surfaces in the UI as needs-attention, and preserve the
// worktree for the operator to investigate.
//
// hook_token is intentionally NOT cleared — if the operator manually
// re-fires the cron job, the new run gets a fresh session row and the
// stranded Finalizing → Blocked one stays around as evidence.
//
// Returns the number of sessions recovered. Errors on individual sessions
// are logged but do not abort the loop — startup must complete even if a
// row's outcome write fails.
func (l *Lifecycle) RecoverFinalizingSessions(ctx context.Context) (int, error) {
	stuck, err := l.sessions.ListByState(ctx, int(machine.Finalizing))
	if err != nil {
		return 0, fmt.Errorf("list finalizing sessions: %w", err)
	}

	recovered := 0
	for _, sess := range stuck {
		// Skip archived sessions, symmetric with the recoverStrandedCronSessions
		// guard. ListByState returns rows regardless of archived status
		// (session_store.go), and a benign worktree_gone finalize (archived /
		// removed session, worktree deleted) leaves the row in Finalizing —
		// steps 5 and 6 of FinalizeSession are both skipped for that outcome. If
		// this recovery pass reclassified such a row it would record
		// failed_recovered and transition it to Blocked on the next daemon
		// restart, resurrecting the exact scary pr_failed-style framing BOS-384
		// kills. There is nothing to recover for an archived session.
		if sess.ArchivedAt != nil {
			continue
		}
		if sess.CronJobID != nil && *sess.CronJobID != "" && l.cronJobs != nil {
			recordedID := sess.ID
			// Guard against a newer run, same as FinalizeSession: while this
			// session sat stranded in Finalizing, the scheduler may have fired a
			// fresh run (RunActive treats Finalizing as inactive) and moved
			// last_run_session_id forward via MarkFireStarted. An unguarded write
			// here would point the job back at this now-Blocked session, so the
			// overlap check would inspect it instead of the newer active run and
			// launch more runs concurrently. ExpectedSessionID keeps the write
			// conditional on this session still being the recorded last run; a
			// superseded no-match is a benign skip (the outcome belongs to the
			// newer run). The Blocked transition below is independent of this.
			if err := l.cronJobs.UpdateLastRun(ctx, *sess.CronJobID, db.UpdateCronJobLastRunParams{
				SessionID:         &recordedID,
				ExpectedSessionID: &recordedID,
				RanAt:             time.Now(),
				Outcome:           models.CronJobOutcomeFailedRecovered,
			}); err != nil {
				if errors.Is(err, db.ErrCronJobLastRunSuperseded) {
					l.logger.Info().
						Str("session", sess.ID).
						Str("cronJob", *sess.CronJobID).
						Msg("recover: cron last-run superseded by newer run; skipping failed_recovered outcome")
				} else {
					l.logger.Error().Err(err).
						Str("session", sess.ID).
						Str("cronJob", *sess.CronJobID).
						Msg("recover: failed to record failed_recovered outcome")
				}
			}
		}

		transitioned, err := l.sessions.UpdateStateConditional(
			ctx, sess.ID, int(machine.Blocked), int(machine.Finalizing),
		)
		if err != nil {
			l.logger.Error().Err(err).
				Str("session", sess.ID).
				Msg("recover: failed to transition stuck Finalizing session to Blocked")
			continue
		}
		if !transitioned {
			continue
		}

		recoverReason := sessionreason.FinalizeRecovered()
		recoverReasonPtr := &recoverReason
		if _, err := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{
			BlockedReason: &recoverReasonPtr,
		}); err != nil {
			l.logger.Error().Err(err).Str("session", sess.ID).
				Msg("recover: failed to persist blocked reason")
		}

		l.logger.Warn().
			Str("session", sess.ID).
			Msg("recovered session stuck in Finalizing from previous daemon run")
		recovered++
	}

	return recovered, nil
}

// needsAttention reports whether a finalize outcome should drop the session
// into the Blocked state so it surfaces as attention-needed in the UI. The
// happy path (pr_created) and the session-deleted path (deleted_no_changes)
// both return false — the former continues under the finalize chat, the
// latter has no row to transition. failed_recovered, fire_failed, and gated
// are recorded by other code paths (RecoverFinalizingSessions and the
// scheduler respectively) and never flow through FinalizeSession's
// needsAttention check, but they're listed here to keep the switch
// exhaustive. (gated blocks the fire before any session exists, so there is
// no session to mark attention-needed.)
//
// Note pr_skipped_no_github returns true here for the preserved (interactive)
// case, but a cron session on that path is hard-deleted (FinalizeResult.Deleted)
// — the deletion is suppressed by the caller's `&& !result.Deleted` guard in
// FinalizeSession step 6, not by this function. The outcome alone does not gate
// the Blocked transition.
func needsAttention(o models.CronJobOutcome) bool {
	switch o {
	case models.CronJobOutcomePRFailed,
		models.CronJobOutcomePRSkippedNoGitHub,
		models.CronJobOutcomeChatSpawnFailed,
		models.CronJobOutcomeCleanupFailed,
		models.CronJobOutcomePRNoChanges:
		return true
	case models.CronJobOutcomeDeletedNoChanges,
		models.CronJobOutcomeZeroOutput,
		models.CronJobOutcomePRCreated,
		models.CronJobOutcomeFailedRecovered,
		models.CronJobOutcomeFireFailed,
		models.CronJobOutcomeGated,
		// worktree_gone: finalize ran against an already-removed worktree
		// (archived/deleted session). A benign no-op, not a housekeeping
		// failure — so the session must NOT be routed to Blocked.
		models.CronJobOutcomeWorktreeGone:
		return false
	}
	return false
}
