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
	// CronJobOutcomeGated records a fire that was skipped because the job's
	// gate_command exited non-zero, indicating the gate condition was not met.
	CronJobOutcomeGated CronJobOutcome = "gated"
	// CronJobOutcomePRNoChanges records a headless (detach) run that finished
	// clean but produced no real work — the branch carries only the empty
	// draft-PR bootstrap commit, so any attached PR is a no-op. It is an
	// attention outcome (see needsAttention): the session is Blocked rather than
	// surfaced as a green ready-for-review PR, so a headless /boss-epic driver
	// fail-isolates the dead session instead of merging an empty PR.
	CronJobOutcomePRNoChanges CronJobOutcome = "pr_no_changes"
)

// CronJob represents a scheduled prompt that fires on a cron expression.
type CronJob struct {
	ID               string
	RepoID           string
	Name             string
	Prompt           string
	Schedule         string
	Timezone         *string // IANA name; nil = daemon-local
	AgentName        string
	Model            string // opaque agent model id; "" = plugin default.
	Enabled          bool
	GateCommand      string // shell command run before firing; non-zero exit skips the run
	RunSetupCommand  bool   // whether to run the repo setup script before the agent session
	LastRunSessionID *string
	LastRunAt        *time.Time
	LastRunOutcome   *CronJobOutcome
	NextRunAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}
