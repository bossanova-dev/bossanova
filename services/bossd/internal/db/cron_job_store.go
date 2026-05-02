package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

var _ CronJobStore = (*SQLiteCronJobStore)(nil)

// SQLiteCronJobStore implements CronJobStore using SQLite.
type SQLiteCronJobStore struct {
	db *sql.DB
}

// NewCronJobStore creates a new SQLite-backed CronJobStore.
func NewCronJobStore(db *sql.DB) *SQLiteCronJobStore {
	return &SQLiteCronJobStore{db: db}
}

func (s *SQLiteCronJobStore) Create(ctx context.Context, params CreateCronJobParams) (*models.CronJob, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new cron job id: %w", err)
	}
	now := sqlutil.TimeNow()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, timezone, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, params.RepoID, params.Name, params.Prompt, params.Schedule, params.Timezone,
		sqlutil.BoolToInt(params.Enabled), now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert cron job: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteCronJobStore) Get(ctx context.Context, id string) (*models.CronJob, error) {
	row := s.db.QueryRowContext(ctx, cronJobSelectSQL+" WHERE id = ?", id)
	return scanCronJob(row)
}

func (s *SQLiteCronJobStore) List(ctx context.Context) ([]*models.CronJob, error) {
	rows, err := s.db.QueryContext(ctx, cronJobSelectSQL+" ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("list cron jobs: %w", err)
	}
	return collectCronJobs(rows)
}

func (s *SQLiteCronJobStore) ListByRepo(ctx context.Context, repoID string) ([]*models.CronJob, error) {
	rows, err := s.db.QueryContext(ctx,
		cronJobSelectSQL+" WHERE repo_id = ? ORDER BY name ASC", repoID)
	if err != nil {
		return nil, fmt.Errorf("list cron jobs by repo: %w", err)
	}
	return collectCronJobs(rows)
}

func (s *SQLiteCronJobStore) ListEnabled(ctx context.Context) ([]*models.CronJob, error) {
	rows, err := s.db.QueryContext(ctx,
		cronJobSelectSQL+" WHERE enabled = 1 ORDER BY created_at ASC")
	if err != nil {
		return nil, fmt.Errorf("list enabled cron jobs: %w", err)
	}
	return collectCronJobs(rows)
}

func (s *SQLiteCronJobStore) Update(ctx context.Context, id string, params UpdateCronJobParams) (*models.CronJob, error) {
	now := sqlutil.TimeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *params.Name)
	}
	if params.Prompt != nil {
		sets = append(sets, "prompt = ?")
		args = append(args, *params.Prompt)
	}
	if params.Schedule != nil {
		sets = append(sets, "schedule = ?")
		args = append(args, *params.Schedule)
	}
	if params.Timezone != nil {
		if *params.Timezone == nil {
			sets = append(sets, "timezone = NULL")
		} else {
			sets = append(sets, "timezone = ?")
			args = append(args, **params.Timezone)
		}
	}
	if params.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, sqlutil.BoolToInt(*params.Enabled))
	}
	if params.NextRunAt != nil {
		if *params.NextRunAt == nil {
			sets = append(sets, "next_run_at = NULL")
		} else {
			sets = append(sets, "next_run_at = ?")
			args = append(args, (*params.NextRunAt).UTC().Format("2006-01-02T15:04:05.000Z"))
		}
	}

	args = append(args, id)
	query := "UPDATE cron_jobs SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update cron job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteCronJobStore) MarkFireStarted(ctx context.Context, id string, sessionID string, firedAt time.Time, nextRunAt *time.Time) error {
	now := sqlutil.TimeNow()
	firedAtStr := firedAt.UTC().Format("2006-01-02T15:04:05.000Z")

	sets := []string{
		"last_run_session_id = ?",
		"last_run_at = ?",
		"updated_at = ?",
	}
	args := []any{sessionID, firedAtStr, now}

	if nextRunAt == nil {
		sets = append(sets, "next_run_at = NULL")
	} else {
		sets = append(sets, "next_run_at = ?")
		args = append(args, nextRunAt.UTC().Format("2006-01-02T15:04:05.000Z"))
	}

	args = append(args, id)
	query := "UPDATE cron_jobs SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("mark cron job fire started: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteCronJobStore) UpdateLastRun(ctx context.Context, id string, params UpdateCronJobLastRunParams) error {
	now := sqlutil.TimeNow()
	ranAt := params.RanAt.UTC().Format("2006-01-02T15:04:05.000Z")

	sets := []string{
		"last_run_at = ?",
		"last_run_outcome = ?",
		"updated_at = ?",
	}
	args := []any{ranAt, string(params.Outcome), now}

	if params.SessionID != nil {
		sets = append(sets, "last_run_session_id = ?")
		args = append(args, *params.SessionID)
	}
	if params.NextRunAt == nil {
		sets = append(sets, "next_run_at = NULL")
	} else {
		sets = append(sets, "next_run_at = ?")
		args = append(args, params.NextRunAt.UTC().Format("2006-01-02T15:04:05.000Z"))
	}

	args = append(args, id)
	query := "UPDATE cron_jobs SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update cron job last run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *SQLiteCronJobStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM cron_jobs WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete cron job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

const cronJobSelectSQL = `SELECT id, repo_id, name, prompt, schedule, timezone, enabled,
	last_run_session_id, last_run_at, last_run_outcome, next_run_at,
	created_at, updated_at
	FROM cron_jobs`

func collectCronJobs(rows *sql.Rows) ([]*models.CronJob, error) {
	defer func() { _ = rows.Close() }()
	var jobs []*models.CronJob
	for rows.Next() {
		j, err := scanCronJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func scanCronJob(s sqlutil.Scanner) (*models.CronJob, error) {
	var j models.CronJob
	var enabledInt int
	var timezone, lastRunSessionID, lastRunAt, lastRunOutcome, nextRunAt sql.NullString
	var createdAt, updatedAt string
	err := s.Scan(
		&j.ID, &j.RepoID, &j.Name, &j.Prompt, &j.Schedule,
		&timezone, &enabledInt,
		&lastRunSessionID, &lastRunAt, &lastRunOutcome, &nextRunAt,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	j.Enabled = enabledInt != 0
	if timezone.Valid {
		s := timezone.String
		j.Timezone = &s
	}
	if lastRunSessionID.Valid {
		s := lastRunSessionID.String
		j.LastRunSessionID = &s
	}
	if lastRunAt.Valid {
		t := sqlutil.ParseTime(lastRunAt.String)
		if !t.IsZero() {
			j.LastRunAt = &t
		}
	}
	if lastRunOutcome.Valid {
		o := models.CronJobOutcome(lastRunOutcome.String)
		j.LastRunOutcome = &o
	}
	if nextRunAt.Valid {
		t := sqlutil.ParseTime(nextRunAt.String)
		if !t.IsZero() {
			j.NextRunAt = &t
		}
	}
	j.CreatedAt = sqlutil.ParseTime(createdAt)
	j.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &j, nil
}
