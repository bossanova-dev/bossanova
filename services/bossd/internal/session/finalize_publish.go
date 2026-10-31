package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

// finalizeTrigger identifies the entry that requested the finalize pipeline.
type finalizeTrigger uint8

const (
	finalizeTriggerRunEnd finalizeTrigger = iota
	finalizeTriggerStrandedRecovery
	finalizeTriggerFinishRequest
	finalizeTriggerBlockedFinish
	finalizeTriggerArchive
)

func finalizePRBody(session *models.Session, trigger finalizeTrigger) string {
	reasons := [...]string{
		"run end",
		"stranded-session recovery (the run's completion signal never reached bossd)",
		"the run's own finish request",
		"blocked-run finish",
		"archive (zero-output)",
	}
	body := fmt.Sprintf("bossd opened this PR while finalizing session `%s`.\n\nTrigger: %s.\n\nThe agent's run ended without opening its own PR.\n", session.ID, reasons[trigger])
	// Verify ties a PR to its ticket only through this first line
	// (parseTicketLine in skills-toolbox/verify/verify-admission.mjs); without it
	// a merged recovery PR leaves its claimed ticket In Progress.
	if session.TrackerURL != nil && *session.TrackerURL != "" {
		return withTrackerLine(session.TrackerURL, body)
	}
	if session.TrackerID != nil && *session.TrackerID != "" {
		body += fmt.Sprintf("\nTracker ticket: %s\n", *session.TrackerID)
	}
	return body
}

// publishDirtyOutput handles GitHub output after managed files and non-GitHub
// origins have been routed away. Tracked edits must never reach publication.
func (l *Lifecycle) publishDirtyOutput(ctx context.Context, session *models.Session, repo *models.Repo, status string, trigger finalizeTrigger) *FinalizeResult {
	if porcelainHasTrackedChanges(status) {
		l.logger.Warn().Str("session", session.ID).Str("status", strings.TrimSpace(status)).
			Msg("finalize: worktree has uncommitted tracked changes; nothing was pushed or opened")
		if session.PRNumber != nil {
			if _, _, err := adoptPRTitleWhenCronTitleStale(ctx, l.sessions, l.repos, l.cronJobs, l.provider, l.logger, session); err != nil {
				l.logger.Warn().Err(err).Str("session", session.ID).Msg("finalize: cron title sync from PR failed")
			}
		}
		return &FinalizeResult{Outcome: domain.CronJobOutcomePRFailed, Err: fmt.Errorf("worktree has uncommitted tracked changes; nothing was pushed or opened")}
	}

	// Only untracked leftovers reach publication. Preserve them for inspection;
	// add a placeholder when GitHub would otherwise reject a zero-commit branch.
	hasCommittedWork, err := l.cleanBranchHasCommittedWork(ctx, session, true)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("finalize: committed-work check failed for dirty cron output; preserving worktree")
		return &FinalizeResult{
			Outcome: domain.CronJobOutcomePRFailed,
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
				Outcome: domain.CronJobOutcomePRFailed,
				Err:     err,
			}
		}
	}

	// EnsurePR is idempotent: it no-ops when the session already has a PR,
	// and otherwise pushes the branch and opens a draft PR.
	if err := l.ensureFinalizePR(ctx, session, repo, trigger); err != nil {
		return &FinalizeResult{Outcome: domain.CronJobOutcomePRFailed, Err: err}
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
		return &FinalizeResult{Outcome: domain.CronJobOutcomePRFailed, Err: err}
	}
	if err := l.markFinalizePRReady(ctx, session, repo.OriginURL); err != nil {
		return &FinalizeResult{Outcome: markReadyFailureOutcome(err), Err: err}
	}

	return &FinalizeResult{Outcome: domain.CronJobOutcomePRCreated}
}

// ensureFinalizePR recovers a PR made visible despite an EnsurePR failure,
// preserving the association race handling on both publish paths.
func (l *Lifecycle) ensureFinalizePR(ctx context.Context, session *models.Session, repo *models.Repo, trigger finalizeTrigger) error {
	if err := l.EnsurePR(ctx, session.ID, finalizePRBody(session, trigger)); err != nil {
		if attached, attachErr := l.attachBranchPRAfterEnsurePRError(ctx, session, repo); attached {
			l.logger.Warn().Err(err).Str("session", session.ID).
				Msg("finalize: EnsurePR failed after branch PR became visible; continuing")
		} else {
			if attachErr != nil {
				err = fmt.Errorf("%w; attach branch PR after EnsurePR failure: %v", err, attachErr)
			}
			l.logger.Warn().Err(err).Str("session", session.ID).
				Msg("finalize: EnsurePR failed; preserving worktree")
			return err
		}
	}
	return nil
}
