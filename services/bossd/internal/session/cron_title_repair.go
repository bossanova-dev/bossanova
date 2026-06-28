package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

// adoptPRTitleWhenCronTitleStale renames a cron session to its PR's current
// GitHub title when the stored title is still the cron job's name.
//
// Background: every PR-title rename path is one-shot-gated on PRNumber == nil
// (EnsurePR, the finalize attach safety-net, the reconciler, LinkPR). Once a
// cron PR is attached, the placeholder / pr_failed finalize branches leave the
// session title at the cron job name ("Bossanova auto-implement") even though
// the agent set a meaningful title on the GitHub PR. This is the repair: it is
// keyed on "the title is STILL the cron default", which is exactly as safe
// against clobbering a deliberately-set title as the PRNumber == nil guard —
// any title the agent or a human chose is, by definition, not the default.
//
// It returns the updated session (so callers can publish a change event) and
// whether a rename happened. Any lookup failure is non-fatal: the title repair
// must never block finalize or the reconcile loop.
func adoptPRTitleWhenCronTitleStale(
	ctx context.Context,
	sessions db.SessionStore,
	repos db.RepoStore,
	cronJobs db.CronJobStore,
	provider vcs.Provider,
	logger zerolog.Logger,
	session *models.Session,
) (*models.Session, bool, error) {
	if session == nil || cronJobs == nil || provider == nil {
		return nil, false, nil
	}
	if session.CronJobID == nil || *session.CronJobID == "" || session.PRNumber == nil {
		return nil, false, nil
	}

	job, err := cronJobs.Get(ctx, *session.CronJobID)
	if err != nil {
		return nil, false, fmt.Errorf("get cron job %q: %w", *session.CronJobID, err)
	}
	if job == nil || !isCronDefaultTitle(session.Title, job.Name) {
		return nil, false, nil
	}

	repo, err := repos.Get(ctx, session.RepoID)
	if err != nil {
		return nil, false, fmt.Errorf("get repo %q: %w", session.RepoID, err)
	}
	if strings.TrimSpace(repo.OriginURL) == "" {
		return nil, false, nil
	}

	status, err := provider.GetPRStatus(ctx, repo.OriginURL, *session.PRNumber)
	if err != nil {
		return nil, false, fmt.Errorf("get PR #%d status: %w", *session.PRNumber, err)
	}
	if status == nil {
		return nil, false, nil
	}
	prTitle := strings.TrimSpace(status.Title)
	// Only adopt a genuinely better title: non-empty, not itself the cron
	// default, and actually different from what's stored.
	if prTitle == "" || isCronDefaultTitle(prTitle, job.Name) || prTitle == strings.TrimSpace(session.Title) {
		return nil, false, nil
	}

	updated, err := sessions.Update(ctx, session.ID, db.UpdateSessionParams{Title: &prTitle})
	if err != nil {
		return nil, false, fmt.Errorf("update session title: %w", err)
	}
	logger.Info().
		Str("session", session.ID).
		Int("pr", *session.PRNumber).
		Str("old_title", session.Title).
		Str("new_title", prTitle).
		Msg("repaired stale cron session title from PR title")
	return updated, true, nil
}

// isCronDefaultTitle reports whether title is still the cron job's name, either
// verbatim or in its PR-normalized form (the two values a never-renamed cron
// session can carry: scheduler sets job.Name, draftPRTitle normalizes it).
func isCronDefaultTitle(title, jobName string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return false
	}
	name := strings.TrimSpace(jobName)
	if name == "" {
		return false
	}
	return t == name || t == normalizeCronPRTitle(name)
}
