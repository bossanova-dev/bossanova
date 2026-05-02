package session

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	libvcs "github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// finalizeChatSkill is the slash-skill the finalize chat is launched with.
// /boss-finalize handles end-of-run cleanup (review summary, push, status
// update) so the cron-spawned worktree closes cleanly.
const finalizeChatSkill = "/boss-finalize"

// FinalizeResult is the outcome of a FinalizeSession call. Outcome maps 1:1
// to the cron_jobs.last_run_outcome column FinalizeSession writes. NoOp is
// true when the conditional state transition no-op'd (duplicate Stop event
// or a session that never reached ImplementingPlan) — the hook endpoint
// returns 200 either way, but the caller uses this to log the distinction.
type FinalizeResult struct {
	Outcome models.CronJobOutcome
	NoOp    bool
	// Err, when non-nil, is the underlying failure behind a *_failed outcome.
	// It is not returned as a top-level error so the hook endpoint still
	// reports success — the outcome column is already recorded.
	Err error
}

// FinalizeSession runs the Stop-hook finalize pipeline for a cron-spawned
// session. It is idempotent: duplicate Stop events no-op via a conditional
// state transition (ImplementingPlan → Finalizing) guarded by rows_affected.
//
// Outcome classification, in the order it's evaluated:
//   - deleted_no_changes       — empty git status → worktree + session removed
//   - cleanup_failed           — empty status but worktree removal errored
//   - pr_skipped_no_github     — changes present, origin is not GitHub
//   - pr_failed                — EnsurePR returned an error
//   - chat_spawn_failed        — PR created but /boss-finalize chat start failed
//   - pr_created               — PR opened + finalize chat started (happy path)
//
// After the outcome is classified, FinalizeSession writes
// cron_jobs.last_run_outcome (step 4) and, on the pr_created success path,
// clears session.hook_token so a replayed Stop event can no longer
// authenticate against this session (step 5). Failure outcomes also fire the
// Block state-machine event so the session shows up as attention-needed in
// the UI — the Finalizing state itself is intentionally silent per
// vcs/attention.go.
//
// The StartFinalizeChat call is stubbed in this flight leg; FL4-3 fills in
// the real spawn logic. Until then, the pr_created branch always reports
// chat_spawn_failed — tests in FL4-2 therefore exercise pr_created by
// stubbing StartFinalizeChat through a Lifecycle method override in tests.
func (l *Lifecycle) FinalizeSession(ctx context.Context, sessionID string) (*FinalizeResult, error) {
	// Step 1: conditional state transition. The rows_affected guard is the
	// authoritative idempotency mechanism — a check-then-set Go path would
	// race with concurrent Stop events.
	advanced, err := l.sessions.UpdateStateConditional(
		ctx, sessionID, int(machine.Finalizing), int(machine.ImplementingPlan),
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

	// Step 2 + 3: classify outcome by examining the worktree.
	result := l.classifyFinalizeOutcome(ctx, session)

	// Step 4: record the outcome on the cron job row. Non-cron sessions
	// (hypothetical — the hook is only wired into cron-spawned worktrees)
	// are skipped rather than treated as errors.
	if session.CronJobID != nil && *session.CronJobID != "" && l.cronJobs != nil {
		ranAt := time.Now()

		// For deleted_no_changes, the session row was already deleted by
		// finalizeNoChanges. SQLite's ON DELETE SET NULL cascade has already
		// nulled out cron_jobs.last_run_session_id; passing the deleted ID to
		// UpdateLastRun would re-violate the FK constraint, so we leave the
		// session-ID field alone (nil = don't update).
		var recordedSessionIDPtr *string
		if result.Outcome != models.CronJobOutcomeDeletedNoChanges {
			recordedSessionID := sessionID
			recordedSessionIDPtr = &recordedSessionID
		}

		if err := l.cronJobs.UpdateLastRun(ctx, *session.CronJobID, db.UpdateCronJobLastRunParams{
			SessionID: recordedSessionIDPtr,
			RanAt:     ranAt,
			Outcome:   result.Outcome,
		}); err != nil {
			// Outcome classification already succeeded; log and continue.
			l.logger.Error().Err(err).
				Str("session", sessionID).
				Str("cronJob", *session.CronJobID).
				Str("outcome", string(result.Outcome)).
				Msg("failed to record cron job last-run outcome")
		}
	}

	// Step 5: clear hook_token on success only, so a replayed Stop event can
	// no longer authenticate as this session. Failure outcomes keep the token
	// so an operator can manually retry.
	if result.Outcome == models.CronJobOutcomePRCreated {
		var nilToken *string
		if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
			HookToken: &nilToken,
		}); err != nil {
			l.logger.Error().Err(err).
				Str("session", sessionID).
				Msg("failed to clear hook_token after finalize success")
		}
	}

	// Step 6: on failure outcomes (except deleted_no_changes, which removed
	// the session entirely), transition to Blocked so the session surfaces
	// as attention-needed in the UI. Finalizing itself is suppressed in
	// ComputeAttentionStatus, so leaving a failed session there would hide
	// the problem.
	if needsAttention(result.Outcome) {
		if _, err := l.sessions.UpdateStateConditional(
			ctx, sessionID, int(machine.Blocked), int(machine.Finalizing),
		); err != nil {
			l.logger.Error().Err(err).
				Str("session", sessionID).
				Str("outcome", string(result.Outcome)).
				Msg("failed to transition failed finalize to blocked")
		}
	}

	return result, nil
}

// StartFinalizeChat spawns a new Claude chat running the /boss-finalize skill
// against the session's worktree. The prior implementing-plan chat is left
// running — the existing idle poller reaps it once it stops producing output —
// so the user can switch back to it if the finalize chat fails.
//
// On success, the new claude process ID is recorded in claude_chats so the UI
// lists both conversations under the same session row. The session's primary
// ClaudeSessionID is intentionally NOT overwritten: the implementing chat
// remains the canonical "main" chat for the session, and the finalize chat is
// a sibling.
func (l *Lifecycle) StartFinalizeChat(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if session.WorktreePath == "" {
		return fmt.Errorf("session %s has no worktree path", sessionID)
	}

	claudeID, err := l.claude.Start(ctx, session.WorktreePath, finalizeChatSkill, nil, "")
	if err != nil {
		return fmt.Errorf("start finalize claude: %w", err)
	}

	if l.claudeChats != nil {
		if _, err := l.claudeChats.Create(ctx, db.CreateClaudeChatParams{
			SessionID: sessionID,
			ClaudeID:  claudeID,
			Title:     "Finalize",
		}); err != nil {
			// The claude process is already running; failing to record the
			// chat row would orphan it from the UI but the run itself can
			// still complete. Surface as an error so FinalizeSession reports
			// chat_spawn_failed and the session lands in Blocked for an
			// operator to investigate.
			return fmt.Errorf("create finalize chat row: %w", err)
		}
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeID", claudeID).
		Msg("finalize chat started")

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
}

// stripBossdManagedFiles removes porcelain entries for bossd-owned paths
// before the empty-status check. Lines are "XY path" (two status chars,
// a space, then the pathspec); we slice past the 3-char prefix and trim
// trailing whitespace before comparing. Rename entries ("R  old -> new")
// are rare and never originate from bossd, so are left untouched.
func stripBossdManagedFiles(porcelain string) string {
	if porcelain == "" {
		return ""
	}
	managed := make(map[string]struct{}, len(bossdManagedWorktreeFiles))
	for _, p := range bossdManagedWorktreeFiles {
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

// classifyFinalizeOutcome runs steps 2 and 3 of the finalize pipeline: it
// inspects the worktree and routes to the cleanup, no-github, PR-failed, or
// PR-created branch. It never returns an error — unrecoverable failures are
// folded into the Outcome column so the caller always records something.
func (l *Lifecycle) classifyFinalizeOutcome(ctx context.Context, session *models.Session) *FinalizeResult {
	status, err := l.worktrees.Status(ctx, session.WorktreePath)
	if err != nil {
		// Can't tell whether there were changes; treat as recoverable pr_failed
		// so the user can investigate the worktree manually.
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     fmt.Errorf("git status: %w", err),
		}
	}

	// Drop bossd-owned files (the Stop-hook config) from the porcelain
	// output before the empty-check — see stripBossdManagedFiles.
	status = stripBossdManagedFiles(status)

	if strings.TrimSpace(status) == "" {
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
		l.logger.Info().
			Str("session", session.ID).
			Str("origin", repo.OriginURL).
			Msg("finalize: changes present but origin is not GitHub; preserving worktree")
		return &FinalizeResult{Outcome: models.CronJobOutcomePRSkippedNoGitHub}
	}

	if err := l.EnsurePR(ctx, session.ID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: EnsurePR failed; preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomePRFailed,
			Err:     err,
		}
	}

	if err := l.StartFinalizeChat(ctx, session.ID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: chat spawn failed; PR already created, preserving worktree")
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeChatSpawnFailed,
			Err:     err,
		}
	}

	return &FinalizeResult{Outcome: models.CronJobOutcomePRCreated}
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

	if session.WorktreePath != "" && session.WorktreePath != repo.LocalPath {
		if err := l.worktrees.Archive(ctx, session.WorktreePath); err != nil {
			return &FinalizeResult{
				Outcome: models.CronJobOutcomeCleanupFailed,
				Err:     fmt.Errorf("archive worktree: %w", err),
			}
		}
	}

	// Tear down any per-chat tmux sessions BEFORE deleting the session row.
	// claude_chats.session_id has ON DELETE CASCADE, so once the row is gone
	// we lose the tmux_session_name needed to find and kill the tmux session
	// — leaving a stranded `claude` process with no DB pointer back to it.
	// Cron-spawned sessions always have a tmux-hosted chat in this branch
	// (per startCronTmuxChat); interactive sessions never reach finalize, so
	// this is effectively the cron cleanup path.
	l.killAllChatTmuxSessions(ctx, session.ID)

	if err := l.sessions.Delete(ctx, session.ID); err != nil {
		return &FinalizeResult{
			Outcome: models.CronJobOutcomeCleanupFailed,
			Err:     fmt.Errorf("delete session: %w", err),
		}
	}

	return &FinalizeResult{Outcome: models.CronJobOutcomeDeletedNoChanges}
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
		if sess.CronJobID != nil && *sess.CronJobID != "" && l.cronJobs != nil {
			recordedID := sess.ID
			if err := l.cronJobs.UpdateLastRun(ctx, *sess.CronJobID, db.UpdateCronJobLastRunParams{
				SessionID: &recordedID,
				RanAt:     time.Now(),
				Outcome:   models.CronJobOutcomeFailedRecovered,
			}); err != nil {
				l.logger.Error().Err(err).
					Str("session", sess.ID).
					Str("cronJob", *sess.CronJobID).
					Msg("recover: failed to record failed_recovered outcome")
			}
		}

		if _, err := l.sessions.UpdateStateConditional(
			ctx, sess.ID, int(machine.Blocked), int(machine.Finalizing),
		); err != nil {
			l.logger.Error().Err(err).
				Str("session", sess.ID).
				Msg("recover: failed to transition stuck Finalizing session to Blocked")
			continue
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
// latter has no row to transition. failed_recovered and fire_failed are
// recorded by other code paths (RecoverFinalizingSessions and the
// scheduler respectively) and never flow through FinalizeSession's
// needsAttention check, but they're listed here to keep the switch
// exhaustive.
func needsAttention(o models.CronJobOutcome) bool {
	switch o {
	case models.CronJobOutcomePRFailed,
		models.CronJobOutcomePRSkippedNoGitHub,
		models.CronJobOutcomeChatSpawnFailed,
		models.CronJobOutcomeCleanupFailed:
		return true
	case models.CronJobOutcomeDeletedNoChanges,
		models.CronJobOutcomePRCreated,
		models.CronJobOutcomeFailedRecovered,
		models.CronJobOutcomeFireFailed:
		return false
	}
	return false
}
