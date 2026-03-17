package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
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
	id := newID()
	now := timeNow()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repos (id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, created_at, updated_at
		 FROM repos WHERE id = ?`, id)
	return scanRepo(row)
}

func (s *SQLiteRepoStore) GetByPath(ctx context.Context, localPath string) (*models.Repo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, created_at, updated_at
		 FROM repos WHERE local_path = ?`, localPath)
	return scanRepo(row)
}

func (s *SQLiteRepoStore) List(ctx context.Context) ([]*models.Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, created_at, updated_at
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
	now := timeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.DisplayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *params.DisplayName)
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
func scanRepo(s scanner) (*models.Repo, error) {
	var r models.Repo
	var createdAt, updatedAt string
	err := s.Scan(&r.ID, &r.DisplayName, &r.LocalPath, &r.OriginURL,
		&r.DefaultBaseBranch, &r.WorktreeBaseDir, &r.SetupScript,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	r.CreatedAt = parseTime(createdAt)
	r.UpdatedAt = parseTime(updatedAt)
	return &r, nil
}

// parseTime parses an ISO 8601 timestamp string from SQLite.
func parseTime(s string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05.000Z", s)
	if t.IsZero() {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	return t
}
