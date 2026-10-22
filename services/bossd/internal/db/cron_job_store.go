package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ CronJobStore = (*SQLiteCronJobStore)(nil)

// SQLiteCronJobStore implements CronJobStore using SQLite.
type SQLiteCronJobStore struct{ db *sql.DB }

// NewCronJobStore creates a new SQLite-backed CronJobStore.
func NewCronJobStore(db *sql.DB) *SQLiteCronJobStore { return &SQLiteCronJobStore{db: db} }

func normalizeCronJobAgentName(agentName string) string {
	if v := strings.TrimSpace(agentName); v != "" {
		return v
	}
	return "claude"
}

func (s *SQLiteCronJobStore) Create(ctx context.Context, params CreateCronJobParams) (*domain.CronJob, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new cron job id: %w", err)
	}
	now := sqlutil.TimeNow()
	policy := params.ConcurrencyPolicy
	if policy == "" {
		policy = models.CronJobConcurrencyPolicySkip
	}
	row, err := sqlcgen.New(s.db).CreateCronJob(ctx, sqlcgen.CreateCronJobParams{
		ID: id, RepoID: params.RepoID, Name: params.Name, Prompt: params.Prompt, Schedule: params.Schedule,
		Timezone: params.Timezone, AgentName: normalizeCronJobAgentName(params.AgentName), Model: params.Model,
		IsEnabled: params.IsEnabled, GateCommand: optionalText(params.GateCommand), ShouldRunSetupCommand: params.ShouldRunSetupCommand,
		IsZeroOutput: params.IsZeroOutput, ConcurrencyPolicy: string(policy), ManagedBy: params.ManagedBy, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return nil, fmt.Errorf("insert cron job: %w", err)
	}
	return cronJobFromRow(row), nil
}

func (s *SQLiteCronJobStore) Get(ctx context.Context, id string) (*domain.CronJob, error) {
	row, err := sqlcgen.New(s.db).GetCronJob(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return cronJobFromRow(row), nil
}

func (s *SQLiteCronJobStore) List(ctx context.Context) ([]*domain.CronJob, error) {
	rows, err := sqlcgen.New(s.db).ListCronJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list cron jobs: %w", err)
	}
	return mapRows(rows, cronJobFromRow), nil
}

func (s *SQLiteCronJobStore) ListByRepo(ctx context.Context, repoID string) ([]*domain.CronJob, error) {
	rows, err := sqlcgen.New(s.db).ListCronJobsByRepo(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("list cron jobs by repo: %w", err)
	}
	return mapRows(rows, cronJobFromRow), nil
}

func (s *SQLiteCronJobStore) ListByEnabled(ctx context.Context) ([]*domain.CronJob, error) {
	rows, err := sqlcgen.New(s.db).ListCronJobsByEnabled(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled cron jobs: %w", err)
	}
	return mapRows(rows, cronJobFromRow), nil
}

func (s *SQLiteCronJobStore) Update(ctx context.Context, id string, params UpdateCronJobParams) (*domain.CronJob, error) {
	arg := sqlcgen.UpdateCronJobParams{
		ID: id, Name: params.Name, Prompt: params.Prompt, Schedule: params.Schedule, Model: params.Model,
		IsEnabled: params.IsEnabled, GateCommand: params.GateCommand, ShouldRunSetupCommand: params.ShouldRunSetupCommand,
		IsZeroOutput: params.IsZeroOutput, ManagedBy: params.ManagedBy, UpdatedAt: sqlutil.TimeNow(),
		SetTimezone: params.Timezone != nil, SetNextRunAt: params.NextRunAt != nil,
	}
	if params.Timezone != nil {
		arg.Timezone = *params.Timezone
	}
	if params.NextRunAt != nil {
		arg.NextRunAt = timeText(*params.NextRunAt)
	}
	if params.AgentName != nil {
		value := normalizeCronJobAgentName(*params.AgentName)
		arg.AgentName = &value
	}
	if params.ConcurrencyPolicy != nil {
		value := string(*params.ConcurrencyPolicy)
		arg.ConcurrencyPolicy = &value
	}
	row, err := sqlcgen.New(s.db).UpdateCronJob(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("update cron job: %w", notFound(err))
	}
	return cronJobFromRow(row), nil
}

func (s *SQLiteCronJobStore) MarkFireStarted(ctx context.Context, id, sessionID, agentName string, firedAt time.Time, nextRunAt *time.Time) error {
	agentName = strings.TrimSpace(agentName)
	n, err := sqlcgen.New(s.db).MarkCronJobFireStarted(ctx, sqlcgen.MarkCronJobFireStartedParams{
		ID: id, LastRunSessionID: &sessionID, LastRunAgentName: &agentName, LastRunAt: timeText(&firedAt), NextRunAt: timeText(nextRunAt), UpdatedAt: sqlutil.TimeNow(),
	})
	if err != nil {
		return fmt.Errorf("mark cron job fire started: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

func (s *SQLiteCronJobStore) UpdateLastRun(ctx context.Context, id string, params UpdateCronJobLastRunParams) error {
	outcome := string(params.Outcome)
	arg := sqlcgen.UpdateCronJobLastRunParams{ID: id, LastRunAt: timeText(&params.RanAt), LastRunOutcome: &outcome,
		UpdatedAt: sqlutil.TimeNow(), LastRunSessionID: params.SessionID, SetNextRunAt: params.NextRunAt != nil,
		ExpectedSessionID: params.ExpectedSessionID, AllowClearedExpectedSessionID: params.AllowClearedExpectedSessionID}
	if params.NextRunAt != nil {
		arg.NextRunAt = timeText(*params.NextRunAt)
	}
	n, err := sqlcgen.New(s.db).UpdateCronJobLastRun(ctx, arg)
	if err != nil {
		return fmt.Errorf("update cron job last run: %w", err)
	}
	if n == 0 {
		if params.ExpectedSessionID != nil {
			return ErrCronJobLastRunSuperseded
		}
		return notFound(sql.ErrNoRows)
	}
	return nil
}

func (s *SQLiteCronJobStore) Delete(ctx context.Context, id string) error {
	n, err := sqlcgen.New(s.db).DeleteCronJob(ctx, id)
	if err != nil {
		return fmt.Errorf("delete cron job: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

func cronJobFromRow(row sqlcgen.CronJob) *domain.CronJob {
	job := &domain.CronJob{
		ID: row.ID, RepoID: row.RepoID, Name: row.Name, Prompt: row.Prompt, Schedule: row.Schedule, Timezone: row.Timezone,
		AgentName: normalizeCronJobAgentName(row.AgentName), Model: row.Model, IsEnabled: row.IsEnabled,
		GateCommand: textValue(row.GateCommand), ShouldRunSetupCommand: row.ShouldRunSetupCommand, IsZeroOutput: row.IsZeroOutput,
		ConcurrencyPolicy: models.ParseCronJobConcurrencyPolicy(row.ConcurrencyPolicy), ManagedBy: row.ManagedBy,
		LastRunSessionID: row.LastRunSessionID, LastRunAgentName: textValue(row.LastRunAgentName), LastRunAt: optionalTextTime(row.LastRunAt),
		NextRunAt: optionalTextTime(row.NextRunAt), CreatedAt: textTime(row.CreatedAt), UpdatedAt: textTime(row.UpdatedAt),
	}
	if row.LastRunOutcome != nil {
		value := models.CronJobOutcome(*row.LastRunOutcome)
		job.LastRunOutcome = &value
	}
	return job
}
