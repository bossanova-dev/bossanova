package models

import "time"

// CronJobOutcome is the result recorded after a cron job's session is finalized.
type CronJobOutcome string

const (
	CronJobOutcomeDeletedNoChanges  CronJobOutcome = "deleted_no_changes"
	CronJobOutcomePRCreated         CronJobOutcome = "pr_created"
	CronJobOutcomePRSkippedNoGitHub CronJobOutcome = "pr_skipped_no_github"
	CronJobOutcomePRFailed          CronJobOutcome = "pr_failed"
	CronJobOutcomeChatSpawnFailed   CronJobOutcome = "chat_spawn_failed"
	CronJobOutcomeCleanupFailed     CronJobOutcome = "cleanup_failed"
	CronJobOutcomeFailedRecovered   CronJobOutcome = "failed_recovered"
	// CronJobOutcomeFireFailed records a fire that never reached a session —
	// e.g. the scheduler's CreateSession call returned an error. Distinct
	// from the later-pipeline failure outcomes (PRFailed, ChatSpawnFailed).
	CronJobOutcomeFireFailed CronJobOutcome = "fire_failed"
)

// CronJob represents a scheduled prompt that fires on a cron expression.
type CronJob struct {
	ID               string
	RepoID           string
	Name             string
	Prompt           string
	Schedule         string
	Timezone         *string // IANA name; nil = daemon-local
	Enabled          bool
	LastRunSessionID *string
	LastRunAt        *time.Time
	LastRunOutcome   *CronJobOutcome
	NextRunAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
