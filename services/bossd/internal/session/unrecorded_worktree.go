package session

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/recurser/bossalib/models"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
)

// WorktreeCurrentPaths inventories live working directories; errors fail closed.
type WorktreeCurrentPaths interface {
	CurrentPaths(context.Context) ([]string, error)
}

// SetUnrecordedWorktreePaths supplies process and tmux liveness inventories.
func (l *Lifecycle) SetUnrecordedWorktreePaths(processes, panes WorktreeCurrentPaths) {
	l.unrecordedProcesses = processes
	l.unrecordedPanes = panes
}

// UnrecordedWorktreeReport summarizes the reconciliation, with holds by reason.
type UnrecordedWorktreeReport struct {
	Removed, ResidueRemoved int
	Held                    map[string]int
}

// ReconcileUnrecordedWorktrees reclaims safe, unreferenced worktrees and hook residue.
func (l *Lifecycle) ReconcileUnrecordedWorktrees(ctx context.Context) (UnrecordedWorktreeReport, error) {
	return l.reconcileUnrecordedWorktrees(ctx, time.Now())
}

func (l *Lifecycle) reconcileUnrecordedWorktrees(ctx context.Context, now time.Time) (UnrecordedWorktreeReport, error) {
	report := UnrecordedWorktreeReport{Held: make(map[string]int)}
	defer func() {
		l.logger.Info().Int("removed", report.Removed).Int("residue_removed", report.ResidueRemoved).Interface("held", report.Held).Msg("unrecorded worktree reconcile")
	}()
	repos, err := l.repos.List(ctx)
	if err != nil {
		return report, fmt.Errorf("list repos: %w", err)
	}
	if l.unrecordedProcesses == nil || l.unrecordedPanes == nil {
		return report, fmt.Errorf("worktree liveness inventory unavailable")
	}
	processes, pErr := l.unrecordedProcesses.CurrentPaths(ctx)
	panes, tErr := l.unrecordedPanes.CurrentPaths(ctx)

	scan := unrecordedWorktreeScan{processes: processes, panes: panes, processErr: pErr, paneErr: tErr, now: now}
	for _, registered := range repos {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if registered.WorktreeBaseDir == "" {
			continue
		}
		repo := *registered
		if resolved, err := filepath.EvalSymlinks(repo.WorktreeBaseDir); err == nil {
			repo.WorktreeBaseDir = resolved
		}
		l.reconcileUnrecordedRepo(ctx, &repo, scan, &report)
	}

	return report, nil
}

type unrecordedWorktreeScan struct {
	processes, panes    []string
	processErr, paneErr error
	now                 time.Time
}

func (l *Lifecycle) reconcileUnrecordedRepo(ctx context.Context, repo *models.Repo, scan unrecordedWorktreeScan, report *UnrecordedWorktreeReport) {
	base := gitpkg.WorktreeRepoBase(repo.WorktreeBaseDir, repo.DisplayName)
	entries, err := gitpkg.ListWorktrees(ctx, repo.LocalPath)
	if err != nil {
		l.holdUnrecorded(report, repo.LocalPath, "inventory-error")
		return
	}
	release, _, err := acquireStartPathWithin(ctx, repo.ID, TargetReapLockTimeout)
	if err != nil {
		l.holdUnrecorded(report, base, "start-lock")
		return
	}
	defer release()
	rows, err := l.sessions.List(ctx, "")
	if err != nil {
		l.holdUnrecorded(report, base, "session-inventory-error")
		return
	}
	for _, entry := range entries {
		if !gitpkg.WorktreeWithin(base, entry.Path) || sameKeptWorktreePath(repo.LocalPath, entry.Path) || worktreeRecorded(rows, entry.Path) {
			continue
		}
		l.reconcileUnrecordedCandidate(ctx, repo, entry.Path, scan, report)
	}
	// Inventory failures prohibit residue removal too: an in-flight hook writer
	// can have a cwd in a directory no longer registered with git.
	if scan.processErr == nil && scan.paneErr == nil {
		l.reconcileWorktreeResidue(ctx, repo, base, entries, scan.processes, scan.panes, report)
	}
}

func (scan unrecordedWorktreeScan) liveHold(path string) string {
	switch {
	case scan.processErr != nil:
		return "process-inventory-error"
	case scan.paneErr != nil:
		return "pane-inventory-error"
	case worktreeUsed(scan.processes, path):
		return "process"
	case worktreeUsed(scan.panes, path):
		return "pane"
	default:
		return ""
	}
}

func (l *Lifecycle) reconcileUnrecordedCandidate(ctx context.Context, repo *models.Repo, path string, scan unrecordedWorktreeScan, report *UnrecordedWorktreeReport) {
	reason := scan.liveHold(path)
	if reason == "" {
		var exists bool
		reason, exists = refreshedWorktreeHold(ctx, repo.LocalPath, path, scan.now)
		if !exists {
			return
		}
	}
	if reason != "" {
		l.holdUnrecorded(report, path, reason)
		return
	}
	rows, err := l.sessions.List(ctx, "")
	if err != nil {
		l.holdUnrecorded(report, path, "session-inventory-error")
		return
	}
	if worktreeRecorded(rows, path) {
		return
	}
	if reason := l.freshWorktreeLiveHold(ctx, path); reason != "" {
		l.holdUnrecorded(report, path, reason)
		return
	}
	if err := l.worktrees.Archive(ctx, repo.DisplayName, repo.WorktreeBaseDir, path); err != nil {
		l.holdUnrecorded(report, path, "archive-error")
		return
	}
	report.Removed++
}

// freshWorktreeLiveHold refreshes both inventories at each deletion boundary.
func (l *Lifecycle) freshWorktreeLiveHold(ctx context.Context, path string) string {
	processes, pErr := l.unrecordedProcesses.CurrentPaths(ctx)
	panes, tErr := l.unrecordedPanes.CurrentPaths(ctx)
	return (unrecordedWorktreeScan{processes: processes, panes: panes, processErr: pErr, paneErr: tErr}).liveHold(path)
}

func (l *Lifecycle) holdUnrecorded(report *UnrecordedWorktreeReport, path, reason string) {
	report.Held[reason]++
	l.logger.Info().Str("worktree", path).Str("reason", reason).Msg("unrecorded worktree held")
}

func worktreeRecorded(rows []*models.Session, path string) bool {
	for _, row := range rows {
		if sameKeptWorktreePath(row.WorktreePath, path) {
			return true
		}
	}
	return false
}
func worktreeUsed(paths []string, path string) bool {
	for _, used := range paths {
		if sameKeptWorktreePath(used, path) || gitpkg.WorktreeWithin(path, used) {
			return true
		}
	}
	return false
}

func (l *Lifecycle) reconcileWorktreeResidue(ctx context.Context, repo *models.Repo, base string, entries []gitpkg.WorktreeEntry, processes, panes []string, report *UnrecordedWorktreeReport) {
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return filepath.SkipDir
		}
		if !d.IsDir() || path == base {
			return nil
		}
		if !gitpkg.WorktreeWithin(base, path) || sameKeptWorktreePath(path, repo.LocalPath) {
			return filepath.SkipDir
		}
		for _, entry := range entries {
			if sameKeptWorktreePath(path, entry.Path) {
				return filepath.SkipDir
			}
		}
		if !gitpkg.IsWorktreeResidue(path) {
			return nil
		}
		rows, err := l.sessions.List(ctx, "")
		if err != nil {
			return filepath.SkipDir
		}
		protected := worktreeUsed(processes, path) || worktreeUsed(panes, path) || residueRecorded(rows, entries, path)
		if !protected {
			if reason := l.freshWorktreeLiveHold(ctx, path); reason != "" {
				l.holdUnrecorded(report, path, reason)
				return filepath.SkipDir
			}
			removed, err := gitpkg.RemoveWorktreeResidue(base, path)
			if err != nil {
				l.holdUnrecorded(report, path, "residue-error")
			} else if removed {
				report.ResidueRemoved++
			}
		}
		return filepath.SkipDir
	})
}

func refreshedWorktreeHold(ctx context.Context, repo, path string, now time.Time) (string, bool) {
	fresh, err := gitpkg.ListWorktrees(ctx, repo)
	if err != nil {
		return "inventory-error", true
	}
	for _, entry := range fresh {
		if !sameKeptWorktreePath(entry.Path, path) {
			continue
		}
		reason, err := gitpkg.UnrecordedWorktreeHold(ctx, entry, now)
		if err != nil {
			return "git-state-error", true
		}
		return reason, true
	}
	return "", false
}

func residueRecorded(rows []*models.Session, entries []gitpkg.WorktreeEntry, path string) bool {
	for _, row := range rows {
		if row.WorktreePath == "" {
			continue
		}
		if sameKeptWorktreePath(path, row.WorktreePath) || gitpkg.WorktreeWithin(path, row.WorktreePath) || gitpkg.WorktreeWithin(row.WorktreePath, path) {
			return true
		}
	}
	for _, entry := range entries {
		if gitpkg.WorktreeWithin(path, entry.Path) {
			return true
		}
	}
	return false
}
