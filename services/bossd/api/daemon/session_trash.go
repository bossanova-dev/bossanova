package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (s *Server) EmptyTrash(ctx context.Context, req *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error) {
	repoID := "" // all repos
	archived, err := s.sessions.ListArchived(ctx, repoID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list archived: %w", err))
	}

	olderThan := protoToTimestamp(req.Msg.OlderThan)

	// Collect branches to delete, grouped by repo, and delete DB records.
	repoBranches := make(map[string][]string) // repoID -> unique branch names
	repoBranchSeen := make(map[string]map[string]struct{})
	deleted := int32(0)
	for _, sess := range archived {
		sess = s.trashSessionForDeletion(ctx, sess, olderThan)
		if sess == nil {
			continue
		}
		if sess.RepoID != "" && sess.BranchName != "" {
			if repoBranchSeen[sess.RepoID] == nil {
				repoBranchSeen[sess.RepoID] = make(map[string]struct{})
			}
			if _, seen := repoBranchSeen[sess.RepoID][sess.BranchName]; !seen {
				repoBranches[sess.RepoID] = append(repoBranches[sess.RepoID], sess.BranchName)
				repoBranchSeen[sess.RepoID][sess.BranchName] = struct{}{}
			}
		}
		if err := s.sessions.Delete(ctx, sess.ID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete archived session %s: %w", sess.ID, err))
		}
		if s.onSessionDeleted != nil {
			s.onSessionDeleted(ctx, sess.ID)
		}
		deleted++
	}

	// Best-effort git cleanup: delete branches and prune worktrees per repo. The
	// rows are already gone by here, so a refused reap (see RemoveSession) leaves
	// branches nothing names — log it rather than discarding it.
	for repoID, branches := range repoBranches {
		branches = s.unsharedBranches(ctx, repoID, "", branches)
		if len(branches) == 0 {
			continue
		}
		repo, err := s.repos.Get(ctx, repoID)
		if err != nil {
			continue
		}
		if !domain.CloudRepoAllowed(ctx, repoID) || !s.trashCleanupAllowed(ctx, repoID) {
			continue
		}
		if reapErr := s.worktrees.ReapLocalBranches(ctx, repo.LocalPath, branches); reapErr != nil {
			s.logger.Warn().Err(reapErr).
				Str("repo", repo.LocalPath).
				Strs("branches", branches).
				Msg("empty trash: could not delete local branches; they are left behind")
		}
	}

	return connect.NewResponse(&pb.EmptyTrashResponse{DeletedCount: deleted}), nil
}

// unsharedBranches drops each branch that a session row in repoID other than
// removedID still names. Two sessions can share a branch (a PR linked across
// sessions before link-pr refused it), and deleting the local ref for one would
// take the other's unpushed commits with it. A failed read keeps every branch:
// a skipped delete only leaves a branch behind.
func (s *Server) unsharedBranches(ctx context.Context, repoID, removedID string, branches []string) []string {
	rows, err := s.sessions.List(ctx, repoID)
	if err != nil {
		s.logger.Warn().Err(err).Str("repo_id", repoID).Strs("branches", branches).
			Msg("could not check which sessions name these branches; keeping them")
		return nil
	}
	owner := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ID != removedID && row.BranchName != "" {
			owner[row.BranchName] = row.ID
		}
	}
	var unshared []string
	for _, branch := range branches {
		if other, ok := owner[branch]; ok {
			s.logger.Info().Str("repo_id", repoID).Str("branch", branch).Str("session", other).
				Msg("keeping local branch another session still names")
			continue
		}
		unshared = append(unshared, branch)
	}
	return unshared
}

func (s *Server) trashCleanupAllowed(ctx context.Context, repoID string) bool {
	if !domain.IsCloudCommand(ctx) {
		return true
	}
	repo, err := s.repos.Get(ctx, repoID)
	if err != nil {
		return false
	}
	key, err := trashStorageKey(repo.LocalPath)
	if err != nil {
		return false
	}
	// Git storage belongs to registrations, including those with only active
	// sessions or no sessions. Archived rows cannot describe that boundary.
	repos, err := s.repos.List(ctx)
	if err != nil {
		return false
	}
	for _, protected := range repos {
		if domain.CloudRepoAllowed(ctx, protected.ID) {
			continue
		}
		other, err := trashStorageKey(protected.LocalPath)
		if err != nil || key == other {
			return false
		}
	}
	return true
}

func trashStorageKey(repoPath string) (string, error) {
	dir := filepath.Join(repoPath, ".git")
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		data, err := os.ReadFile(dir)
		if err != nil {
			return "", err
		}
		value, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
		if !ok {
			return "", errors.New("unrecognized git directory")
		}
		dir = strings.TrimSpace(value)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(repoPath, dir)
		}
	}
	common, err := os.ReadFile(filepath.Join(dir, "commondir"))
	if err == nil {
		value := strings.TrimSpace(string(common))
		if value == "" {
			return "", errors.New("empty git common directory")
		}
		if filepath.IsAbs(value) {
			dir = value
		} else {
			dir = filepath.Join(dir, value)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.EvalSymlinks(dir)
}

func (s *Server) trashSessionForDeletion(ctx context.Context, sess *models.Session, olderThan *time.Time) *models.Session {
	if olderThan != nil && sess.ArchivedAt != nil && sess.ArchivedAt.After(*olderThan) {
		return nil
	}
	if domain.IsCloudCommand(ctx) {
		current, err := s.sessions.Get(ctx, sess.ID)
		if err != nil || current == nil || current.RepoID != sess.RepoID || current.ArchivedAt == nil {
			return nil
		}
		sess = current
	}
	if !domain.CloudRepoAllowed(ctx, sess.RepoID) {
		return nil
	}
	return sess
}
