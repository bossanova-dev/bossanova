package session

import (
	"context"
	"fmt"
	"strings"
)

// injectPRTagsAndPush rewrites the session's tagless commit subjects to carry
// the resolved PR number and force-pushes the result. It replaces the work the
// old bossd-spawned "Finalize" chat did by running the /boss-finalize skill's
// add-pr-numbers.sh: cron runs commit tagless, and the commit-message policy
// (commitlint pr-tag rule) requires "[#<PR>]" on non-protected branches.
//
// The publish guard runs earlier; the tracked-change check here also guards
// clean-branch paths against edits racing with their initial status read.
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

	// Refresh the base for tag injection and the subsequent readiness diff,
	// including Dependabot branches that preserve their commit subjects.
	if err := l.worktrees.FetchBase(ctx, cur.WorktreePath, cur.BaseBranch); err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).
			Msg("finalize: fetch base before PR-tag injection failed")
		return fmt.Errorf("fetch base before PR-tag injection: %w", err)
	}

	if IsDependabotBranch(cur.BranchName) {
		// Dependabot subjects are exempt from PR tags. Publish committed repairs
		// without rewriting history; a diverged remote rejects the normal push.
		if err := l.worktrees.Push(ctx, cur.WorktreePath, cur.BranchName); err != nil {
			return fmt.Errorf("push dependabot repairs: %w", err)
		}
		l.logger.Info().Str("session", sessionID).
			Msg("finalize: pushed dependabot branch without PR-tag injection")
		return nil
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
