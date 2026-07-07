package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossalib/vcs"
)

// ErrAmbiguousOrigin is returned by GetByOrigin when the canonical-URL
// fallback finds more than one repo whose stored origin maps to the same
// canonical web URL. Webhook routing must fail loud here rather than pick
// an arbitrary repo and silently refresh the wrong sessions.
var ErrAmbiguousOrigin = errors.New("multiple repos match canonical origin URL")

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
	if err := s.ensureUniqueCanonicalOrigin(ctx, params.OriginURL, ""); err != nil {
		return nil, err
	}

	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new repo id: %w", err)
	}
	now := sqlutil.TimeNow()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO repos (id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_repair, can_auto_rotate, merge_strategy, linear_api_key, sentry_api_key, sentry_org, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, 1, 1, 1, 'merge', '', '', '', ?, ?)`,
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
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_repair, can_auto_rotate, merge_strategy, linear_api_key, sentry_api_key, sentry_org, created_at, updated_at
		 FROM repos WHERE id = ?`, id)
	return scanRepo(row)
}

func (s *SQLiteRepoStore) GetByPath(ctx context.Context, localPath string) (*models.Repo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_repair, can_auto_rotate, merge_strategy, linear_api_key, sentry_api_key, sentry_org, created_at, updated_at
		 FROM repos WHERE local_path = ?`, localPath)
	return scanRepo(row)
}

func (s *SQLiteRepoStore) GetByOrigin(ctx context.Context, originURL string) (*models.Repo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_repair, can_auto_rotate, merge_strategy, linear_api_key, sentry_api_key, sentry_org, created_at, updated_at
		 FROM repos WHERE origin_url = ?`, originURL)
	repo, err := scanRepo(row)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	targetWebURL := vcs.NormalizeRepoURL(originURL)
	if targetWebURL == "" {
		if err == nil {
			return repo, nil
		}
		return nil, sql.ErrNoRows
	}

	repos, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var match *models.Repo
	for _, repo := range repos {
		repoWebURL := vcs.NormalizeRepoURL(repo.OriginURL)
		if repoWebURL == "" || repoWebURL != targetWebURL {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("%w: %q", ErrAmbiguousOrigin, targetWebURL)
		}
		match = repo
	}
	if match == nil {
		if repo != nil {
			return repo, nil
		}
		return nil, sql.ErrNoRows
	}
	return match, nil
}

func (s *SQLiteRepoStore) List(ctx context.Context) ([]*models.Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_repair, can_auto_rotate, merge_strategy, linear_api_key, sentry_api_key, sentry_org, created_at, updated_at
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
	if params.OriginURL != nil {
		if err := s.ensureUniqueCanonicalOrigin(ctx, *params.OriginURL, id); err != nil {
			return nil, err
		}
	}

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
	if params.CanAutoRepair != nil {
		sets = append(sets, "can_auto_repair = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoRepair))
	}
	if params.CanAutoRotate != nil {
		sets = append(sets, "can_auto_rotate = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoRotate))
	}
	if params.MergeStrategy != nil {
		sets = append(sets, "merge_strategy = ?")
		args = append(args, string(*params.MergeStrategy))
	}
	if params.LinearAPIKey != nil {
		sets = append(sets, "linear_api_key = ?")
		args = append(args, *params.LinearAPIKey)
	}
	if params.SentryAPIKey != nil {
		sets = append(sets, "sentry_api_key = ?")
		args = append(args, *params.SentryAPIKey)
	}
	if params.SentryOrg != nil {
		sets = append(sets, "sentry_org = ?")
		args = append(args, *params.SentryOrg)
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

func (s *SQLiteRepoStore) ensureUniqueCanonicalOrigin(ctx context.Context, originURL string, excludeID string) error {
	targetWebURL := vcs.NormalizeRepoURL(originURL)
	if targetWebURL == "" {
		return nil
	}

	repos, err := s.List(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if repo.ID == excludeID {
			continue
		}
		repoWebURL := vcs.NormalizeRepoURL(repo.OriginURL)
		if repoWebURL == "" || repoWebURL != targetWebURL {
			continue
		}
		return fmt.Errorf("%w: %q", ErrAmbiguousOrigin, targetWebURL)
	}
	return nil
}

func (s *SQLiteRepoStore) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM session_check_snapshots WHERE session_id IN (SELECT id FROM sessions WHERE repo_id = ?)`, id); err != nil {
		return fmt.Errorf("delete check snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE repo_id = ?`, id); err != nil {
		return fmt.Errorf("delete workflows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_mappings WHERE repo_id = ?`, id); err != nil {
		return fmt.Errorf("delete task mappings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attempts WHERE session_id IN (SELECT id FROM sessions WHERE repo_id = ?)`, id); err != nil {
		return fmt.Errorf("delete attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_chats WHERE session_id IN (SELECT id FROM sessions WHERE repo_id = ?)`, id); err != nil {
		return fmt.Errorf("delete agent chats: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cron_jobs SET last_run_session_id = NULL WHERE last_run_session_id IN (SELECT id FROM sessions WHERE repo_id = ?)`, id); err != nil {
		return fmt.Errorf("detach cron job last run sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cron_jobs WHERE repo_id = ?`, id); err != nil {
		return fmt.Errorf("delete cron jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE repo_id = ?`, id); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// scanRepo scans a repo from any scanner (*sql.Row or *sql.Rows).
func scanRepo(s sqlutil.Scanner) (*models.Repo, error) {
	var r models.Repo
	var createdAt, updatedAt string
	var canAutoMerge, canAutoMergeDependabot, canAutoRepair, canAutoRotate int
	var mergeStrategy string
	err := s.Scan(&r.ID, &r.DisplayName, &r.LocalPath, &r.OriginURL,
		&r.DefaultBaseBranch, &r.WorktreeBaseDir, &r.SetupScript,
		&canAutoMerge, &canAutoMergeDependabot, &canAutoRepair, &canAutoRotate,
		&mergeStrategy,
		&r.LinearAPIKey,
		&r.SentryAPIKey, &r.SentryOrg,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	r.CanAutoMerge = canAutoMerge != 0
	r.CanAutoMergeDependabot = canAutoMergeDependabot != 0
	r.CanAutoRepair = canAutoRepair != 0
	r.CanAutoRotate = canAutoRotate != 0
	r.MergeStrategy = models.MergeStrategy(mergeStrategy)
	r.CreatedAt = sqlutil.ParseTime(createdAt)
	r.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &r, nil
}
