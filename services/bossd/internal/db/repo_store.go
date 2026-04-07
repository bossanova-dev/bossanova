package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ RepoStore = (*SQLiteRepoStore)(nil)

// SQLiteRepoStore implements RepoStore using SQLite.
type SQLiteRepoStore struct {
	db *sql.DB
}

// NewRepoStore creates a new SQLite-backed RepoStore.
func NewRepoStore(db *sql.DB) *SQLiteRepoStore {
	return &SQLiteRepoStore{db: db}
}

func (s *SQLiteRepoStore) Create(ctx context.Context, params CreateRepoParams) (*models.Repo, error) {
	id := sqlutil.NewID()
	now := sqlutil.TimeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repos (id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_address_reviews, can_auto_resolve_conflicts, merge_strategy, linear_api_key, linear_team_key, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, 1, 1, 1, 'merge', '', '', ?, ?)`,
		id, params.DisplayName, params.LocalPath, params.OriginURL,
		params.DefaultBaseBranch, params.WorktreeBaseDir, params.SetupScript, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert repo: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteRepoStore) Get(ctx context.Context, id string) (*models.Repo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_address_reviews, can_auto_resolve_conflicts, merge_strategy, linear_api_key, linear_team_key, created_at, updated_at
		 FROM repos WHERE id = ?`, id)
	return scanRepo(row)
}

func (s *SQLiteRepoStore) GetByPath(ctx context.Context, localPath string) (*models.Repo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_address_reviews, can_auto_resolve_conflicts, merge_strategy, linear_api_key, linear_team_key, created_at, updated_at
		 FROM repos WHERE local_path = ?`, localPath)
	return scanRepo(row)
}

func (s *SQLiteRepoStore) List(ctx context.Context) ([]*models.Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_address_reviews, can_auto_resolve_conflicts, merge_strategy, linear_api_key, linear_team_key, created_at, updated_at
		 FROM repos ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var repos []*models.Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

func (s *SQLiteRepoStore) Update(ctx context.Context, id string, params UpdateRepoParams) (*models.Repo, error) {
	now := sqlutil.TimeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.DisplayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *params.DisplayName)
	}
	if params.OriginURL != nil {
		sets = append(sets, "origin_url = ?")
		args = append(args, *params.OriginURL)
	}
	if params.DefaultBaseBranch != nil {
		sets = append(sets, "default_base_branch = ?")
		args = append(args, *params.DefaultBaseBranch)
	}
	if params.WorktreeBaseDir != nil {
		sets = append(sets, "worktree_base_dir = ?")
		args = append(args, *params.WorktreeBaseDir)
	}
	if params.SetupScript != nil {
		sets = append(sets, "setup_script = ?")
		args = append(args, *params.SetupScript)
	}
	if params.CanAutoMerge != nil {
		sets = append(sets, "can_auto_merge = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoMerge))
	}
	if params.CanAutoMergeDependabot != nil {
		sets = append(sets, "can_auto_merge_dependabot = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoMergeDependabot))
	}
	if params.CanAutoAddressReviews != nil {
		sets = append(sets, "can_auto_address_reviews = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoAddressReviews))
	}
	if params.CanAutoResolveConflicts != nil {
		sets = append(sets, "can_auto_resolve_conflicts = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoResolveConflicts))
	}
	if params.MergeStrategy != nil {
		sets = append(sets, "merge_strategy = ?")
		args = append(args, string(*params.MergeStrategy))
	}
	if params.LinearAPIKey != nil {
		sets = append(sets, "linear_api_key = ?")
		args = append(args, *params.LinearAPIKey)
	}
	if params.LinearTeamKey != nil {
		sets = append(sets, "linear_team_key = ?")
		args = append(args, *params.LinearTeamKey)
	}

	args = append(args, id)
	query := "UPDATE repos SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update repo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteRepoStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// scanRepo scans a repo from any scanner (*sql.Row or *sql.Rows).
func scanRepo(s sqlutil.Scanner) (*models.Repo, error) {
	var r models.Repo
	var createdAt, updatedAt string
	var canAutoMerge, canAutoMergeDependabot, canAutoAddressReviews, canAutoResolveConflicts int
	var mergeStrategy string
	err := s.Scan(&r.ID, &r.DisplayName, &r.LocalPath, &r.OriginURL,
		&r.DefaultBaseBranch, &r.WorktreeBaseDir, &r.SetupScript,
		&canAutoMerge, &canAutoMergeDependabot, &canAutoAddressReviews, &canAutoResolveConflicts,
		&mergeStrategy,
		&r.LinearAPIKey, &r.LinearTeamKey,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	r.CanAutoMerge = canAutoMerge != 0
	r.CanAutoMergeDependabot = canAutoMergeDependabot != 0
	r.CanAutoAddressReviews = canAutoAddressReviews != 0
	r.CanAutoResolveConflicts = canAutoResolveConflicts != 0
	r.MergeStrategy = models.MergeStrategy(mergeStrategy)
	r.CreatedAt = sqlutil.ParseTime(createdAt)
	r.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &r, nil
}
