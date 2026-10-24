package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/recurser/bossalib/models"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
)

const keptWorktreeTTL = 24 * time.Hour

// AgentStartFailedError preserves the retry budget and the final launch cause.
type AgentStartFailedError struct {
	Cause    error
	Attempts int
}

func (e *AgentStartFailedError) Error() string {
	return fmt.Sprintf("agent start failed after %d attempts: %v", e.Attempts, e.Cause)
}
func (e *AgentStartFailedError) Unwrap() error   { return e.Cause }
func (e *AgentStartFailedError) Transient() bool { return isTransientAgentStartError(e.Cause) }

// PreparedWorktreeKeptError names the durable artifacts available for a retry.
type PreparedWorktreeKeptError struct {
	Cause                error
	WorktreePath, Branch string
	Attempts             int
	KeptUntil            time.Time
}

func (e *PreparedWorktreeKeptError) Error() string {
	return fmt.Sprintf("agent start failed under host load after %d attempts; prepared worktree kept at %s until %s; re-run `boss new` for branch %s to reuse it", e.Attempts, e.WorktreePath, e.KeptUntil.Format(time.RFC3339), e.Branch)
}
func (e *PreparedWorktreeKeptError) Unwrap() error { return e.Cause }

func (l *Lifecycle) keepPreparedWorktree(ctx context.Context, sessionID string, cause error) error {
	var alreadyKept *PreparedWorktreeKeptError
	if errors.As(cause, &alreadyKept) {
		return cause
	}
	var failed *AgentStartFailedError
	if !errors.As(cause, &failed) || !failed.Transient() {
		return cause
	}
	var kept *PreparedWorktreeKeptError
	err := runCleanupStep(ctx, func(step context.Context) error {
		sess, err := l.sessions.Get(step, sessionID)
		if err != nil {
			return err
		}
		if sess == nil || sess.WorktreeCreatedAt == nil || sess.WorktreePath == "" {
			return fmt.Errorf("worktree ownership unavailable")
		}
		head, err := gitpkg.PreparedWorktreeHead(step, sess.WorktreePath)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		reason := "agent start failed under host load"
		marker := gitpkg.PreparedWorktreeMarker{Version: 1, SessionID: sess.ID, RepoID: sess.RepoID, Branch: sess.BranchName, IsBranchCreated: sess.IsBranchCreated, HeadSHA: head, BaseBranch: sess.BaseBranch, KeptAt: now, KeptUntil: now.Add(keptWorktreeTTL), Reason: reason}
		if err := gitpkg.WritePreparedWorktreeMarker(step, sess.WorktreePath, marker); err != nil {
			return err
		}
		kept = &PreparedWorktreeKeptError{Cause: cause, WorktreePath: sess.WorktreePath, Branch: sess.BranchName, Attempts: failed.Attempts, KeptUntil: marker.KeptUntil}
		if !preparedMarkerMatches(step, sess, kept) {
			kept = nil
			return fmt.Errorf("prepared worktree marker readback failed")
		}
		return nil
	})
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Msg("could not keep prepared worktree; normal cleanup applies")
		return cause
	}
	return kept
}

func preparedMarkerMatches(ctx context.Context, sess *models.Session, kept *PreparedWorktreeKeptError) bool {
	valid := false
	_ = runCleanupStep(ctx, func(step context.Context) error {
		marker, err := gitpkg.ReadPreparedWorktreeMarker(step, sess.WorktreePath)
		valid = err == nil && marker.SessionID == sess.ID && marker.RepoID == sess.RepoID && marker.Branch == sess.BranchName && marker.IsBranchCreated == sess.IsBranchCreated && kept.WorktreePath == sess.WorktreePath && kept.Branch == sess.BranchName && marker.KeptUntil.Equal(kept.KeptUntil)
		return err
	})
	return valid
}

// ReapExpiredKeptWorktrees reclaims unadopted prepared worktrees after their TTL.
func (l *Lifecycle) ReapExpiredKeptWorktrees(ctx context.Context) (int, error) {
	return l.reapExpiredKeptWorktrees(ctx, time.Now())
}
func (l *Lifecycle) reapExpiredKeptWorktrees(ctx context.Context, now time.Time) (int, error) {
	repos, err := l.repos.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list repos for kept-worktree sweep: %w", err)
	}
	reaped := 0
	for _, repo := range repos {
		paths, err := gitpkg.ListWorktreePaths(ctx, repo.LocalPath)
		if err != nil {
			l.logger.Warn().Err(err).Str("repo", repo.ID).Msg("list kept worktrees failed")
			continue
		}
		for _, path := range paths {
			marker, err := gitpkg.ReadPreparedWorktreeMarker(ctx, path)
			if err != nil || marker.RepoID != repo.ID || now.Before(marker.KeptUntil) {
				continue
			}
			release, ok := acquireTargetForReap(ctx, repo.ID, marker.Branch, nil)
			if !ok {
				continue
			}
			purged := func() bool {
				defer release()
				// PR creates use a different target key. Serialize their row insertion
				// with the check and purge, and skip rows whose bootstrap already began.
				releaseStart, _, err := acquireStartPathWithin(ctx, repo.ID, TargetReapLockTimeout)
				if err != nil {
					return false
				}
				defer releaseStart()
				rows, err := l.sessions.List(ctx, "")
				if err != nil {
					return false
				}
				for _, row := range rows {
					if sameKeptWorktreePath(row.WorktreePath, path) ||
						(row.RepoID == repo.ID && (row.BranchName == marker.Branch ||
							(row.PRNumber != nil && row.BranchName == "" && row.WorktreePath == ""))) {
						return false
					}
				}
				// A failed create may have refreshed retention before this target lock
				// became available. Never purge using the pre-lock snapshot.
				fresh, err := gitpkg.ReadPreparedWorktreeMarker(ctx, path)
				if err != nil || fresh.RepoID != repo.ID || fresh.Branch != marker.Branch || now.Before(fresh.KeptUntil) {
					return false
				}
				marker = fresh
				if err := runCleanupStep(ctx, func(step context.Context) error {
					return l.worktrees.PurgeWorktree(step, repo.LocalPath, repo.DisplayName, repo.WorktreeBaseDir, path)
				}); err != nil {
					return false
				}
				if marker.IsBranchCreated {
					if err := runCleanupStep(ctx, func(step context.Context) error {
						return l.worktrees.ReapLocalBranches(step, repo.LocalPath, []string{marker.Branch})
					}); err != nil {
						l.logger.Warn().Err(err).Str("branch", marker.Branch).Msg("expired kept-worktree branch reap failed")
					}
				}
				return true
			}()
			if purged {
				reaped++
			}
		}
	}
	return reaped, nil
}

func sameKeptWorktreePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// Marker cleanup must happen at the launch boundary, before later bootstrap
// bookkeeping can fail. Idle interactive sessions keep their reservation.
func (l *Lifecycle) consumePreparedWorktree(ctx context.Context, result *gitpkg.CreateResult) {
	if !result.Reused {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeRecordTimeout)
	defer cancel()
	if err := gitpkg.ConsumePreparedWorktreeMarker(cleanupCtx, result.WorktreePath); err != nil {
		l.logger.Warn().Err(err).Str("worktree", result.WorktreePath).Msg("could not consume prepared worktree marker after agent start")
	}
}
