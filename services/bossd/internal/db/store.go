package db

import (
	"context"
	"time"

	"github.com/recurser/bossalib/models"
)

// CreateRepoParams holds the parameters for creating a new repo.
type CreateRepoParams struct {
	DisplayName       string
	LocalPath         string
	OriginURL         string
	DefaultBaseBranch string
	WorktreeBaseDir   string
	SetupScript       *string
}

// UpdateRepoParams holds the fields that can be updated on a repo.
// Nil fields are not updated.
type UpdateRepoParams struct {
	DisplayName             *string
	OriginURL               *string
	DefaultBaseBranch       *string
	WorktreeBaseDir         *string
	SetupScript             **string // double pointer: nil = don't update, *nil = set to NULL
	CanAutoMerge            *bool
	CanAutoMergeDependabot  *bool
	CanAutoAddressReviews   *bool
	CanAutoResolveConflicts *bool
	MergeStrategy           *models.MergeStrategy
	LinearAPIKey            *string
}

// RepoStore defines the interface for repo persistence.
type RepoStore interface {
	Create(ctx context.Context, params CreateRepoParams) (*models.Repo, error)
	Get(ctx context.Context, id string) (*models.Repo, error)
	GetByPath(ctx context.Context, localPath string) (*models.Repo, error)
	List(ctx context.Context) ([]*models.Repo, error)
	Update(ctx context.Context, id string, params UpdateRepoParams) (*models.Repo, error)
	Delete(ctx context.Context, id string) error
}

// CreateTaskMappingParams holds the parameters for creating a new task mapping.
type CreateTaskMappingParams struct {
	ExternalID string
	PluginName string
	RepoID     string
}

// UpdateTaskMappingParams holds the fields that can be updated on a task mapping.
// Nil fields are not updated.
type UpdateTaskMappingParams struct {
	SessionID            **string // double pointer: nil = don't update, *nil = set to NULL
	Status               *models.TaskMappingStatus
	PendingUpdateStatus  **models.TaskMappingStatus // double pointer: nil = don't update, *nil = clear
	PendingUpdateDetails **string                   // double pointer: nil = don't update, *nil = clear
}

// TaskMappingStore defines the interface for task mapping persistence.
type TaskMappingStore interface {
	Create(ctx context.Context, params CreateTaskMappingParams) (*models.TaskMapping, error)
	Get(ctx context.Context, id string) (*models.TaskMapping, error)
	GetByExternalID(ctx context.Context, externalID string) (*models.TaskMapping, error)
	GetBySessionID(ctx context.Context, sessionID string) (*models.TaskMapping, error)
	Update(ctx context.Context, id string, params UpdateTaskMappingParams) (*models.TaskMapping, error)
	Delete(ctx context.Context, id string) error
	ListPending(ctx context.Context) ([]*models.TaskMapping, error)
	FailOrphanedMappings(ctx context.Context) (int64, error)
}

// CreateSessionParams holds the parameters for creating a new session.
type CreateSessionParams struct {
	RepoID       string
	Title        string
	Plan         string
	WorktreePath string
	BranchName   string
	BaseBranch   string
	AgentName    string // Agent plugin name; empty falls back to "claude".
	PRNumber     *int
	PRURL        *string
	TrackerID    *string
	TrackerURL   *string
}

// UpdateSessionParams holds the fields that can be updated on a session.
type UpdateSessionParams struct {
	Title             *string
	State             *int
	WorktreePath      *string
	BranchName        *string
	AgentSessionID    **string
	PRNumber          **int
	PRURL             **string
	TrackerID         **string
	TrackerURL        **string
	TmuxSessionName   **string
	LastCheckState    *int
	AutomationEnabled *bool
	AttemptCount      *int
	BlockedReason     **string
	ArchivedAt        **string // ISO 8601 string or nil
	CronJobID         **string
	HookToken         **string // double pointer: nil = don't update, *nil = clear (cleared on finalize success)

	// Composite display fields, updated by the DisplayStatusComputer (Step 2).
	// Pointer-typed so a nil value means "don't touch" and a zero value means
	// "set to empty/zero" — matching the rest of UpdateSessionParams.
	DisplayLabel   *string
	DisplayIntent  *int32
	DisplaySpinner *bool
}

// SessionWithRepo pairs a Session with its owning repo's display name, so
// callers that need both can fetch them in a single join query rather than
// issuing a follow-up Get per session.
type SessionWithRepo struct {
	*models.Session
	RepoDisplayName string
}

// SessionStore defines the interface for session persistence.
type SessionStore interface {
	Create(ctx context.Context, params CreateSessionParams) (*models.Session, error)
	Get(ctx context.Context, id string) (*models.Session, error)
	List(ctx context.Context, repoID string) ([]*models.Session, error)
	ListByState(ctx context.Context, state int) ([]*models.Session, error)
	ListActive(ctx context.Context, repoID string) ([]*models.Session, error)
	ListActiveWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error)
	ListWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error)
	ListArchived(ctx context.Context, repoID string) ([]*models.Session, error)
	Update(ctx context.Context, id string, params UpdateSessionParams) (*models.Session, error)
	// UpdateStateConditional runs the conditional `UPDATE sessions SET
	// state=newState WHERE id=? AND state=expectedState` used as the
	// idempotency gate for the Stop-hook finalize endpoint. Returns true if
	// exactly one row transitioned; false if the row was already past the
	// expected state (duplicate event or stale transition).
	UpdateStateConditional(ctx context.Context, id string, newState, expectedState int) (bool, error)
	Archive(ctx context.Context, id string) error
	Resurrect(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	AdvanceOrphanedSessions(ctx context.Context) (int64, error)

	// UpdateRepairDiagnostics writes the last_repair_* columns atomically.
	// last_repair_attempt_count tracks consecutive failures: a clean run
	// (both error fields empty) resets it to 0; a failed run bumps it by
	// one. The TUI uses that count to render the "(N×)" suffix, which
	// would otherwise overcount a fail → succeed → fail sequence.
	UpdateRepairDiagnostics(ctx context.Context, params UpdateRepairDiagnosticsParams) error
}

// UpdateRepairDiagnosticsParams carries the per-attempt outcome that the
// repair plugin reports via host.RecordRepairOutcome.
type UpdateRepairDiagnosticsParams struct {
	SessionID     string
	StartedAt     time.Time
	RunnerError   string
	ExitError     string
	HeadSHA       string
	DisplayStatus int32
}

// CreateAgentChatParams holds the parameters for creating a new agent chat record.
type CreateAgentChatParams struct {
	SessionID         string
	AgentSessionID    string
	ProviderSessionID *string
	AgentName         string // Agent plugin name; empty falls back to "claude".
	Title             string
}

// AgentChatStore defines the interface for agent chat persistence.
type AgentChatStore interface {
	Create(ctx context.Context, params CreateAgentChatParams) (*models.AgentChat, error)
	GetByAgentSessionID(ctx context.Context, agentSessionID string) (*models.AgentChat, error)
	ListBySession(ctx context.Context, sessionID string) ([]*models.AgentChat, error)
	UpdateTitle(ctx context.Context, id string, title string) error
	UpdateTitleByAgentSessionID(ctx context.Context, agentSessionID string, title string) error
	UpdateTmuxSessionName(ctx context.Context, agentSessionID string, name *string) error
	UpdateProviderSessionID(ctx context.Context, agentSessionID string, providerSessionID *string) error
	DeleteByAgentSessionID(ctx context.Context, agentSessionID string) error
	ListWithTmuxSession(ctx context.Context) ([]*models.AgentChat, error)
}

// CreateAttemptParams holds the parameters for creating a new attempt.
type CreateAttemptParams struct {
	SessionID string
	Trigger   int
}

// UpdateAttemptParams holds the fields that can be updated on an attempt.
type UpdateAttemptParams struct {
	Result *int
	Error  **string
}

// AttemptStore defines the interface for attempt persistence.
type AttemptStore interface {
	Create(ctx context.Context, params CreateAttemptParams) (*models.Attempt, error)
	Get(ctx context.Context, id string) (*models.Attempt, error)
	ListBySession(ctx context.Context, sessionID string) ([]*models.Attempt, error)
	Update(ctx context.Context, id string, params UpdateAttemptParams) (*models.Attempt, error)
	Delete(ctx context.Context, id string) error
}

// CreateWorkflowParams holds the parameters for creating a new workflow.
type CreateWorkflowParams struct {
	SessionID      string
	RepoID         string
	PlanPath       string
	MaxLegs        int
	StartCommitSHA *string
	ConfigJSON     *string
}

// UpdateWorkflowParams holds the fields that can be updated on a workflow.
// Nil fields are not updated.
type UpdateWorkflowParams struct {
	Status      *string
	CurrentStep *string
	FlightLeg   *int
	LastError   **string // double pointer: nil = don't update, *nil = clear
}

// WorkflowStore defines the interface for workflow persistence.
type WorkflowStore interface {
	Create(ctx context.Context, params CreateWorkflowParams) (*models.Workflow, error)
	Get(ctx context.Context, id string) (*models.Workflow, error)
	Update(ctx context.Context, id string, params UpdateWorkflowParams) (*models.Workflow, error)
	List(ctx context.Context) ([]*models.Workflow, error)
	ListByStatus(ctx context.Context, status string) ([]*models.Workflow, error)
	ListActiveBySessionIDs(ctx context.Context, sessionIDs []string) ([]*models.Workflow, error)
	FailOrphaned(ctx context.Context) (int64, error)
}

// CreateCronJobParams holds the parameters for creating a new cron job.
type CreateCronJobParams struct {
	RepoID   string
	Name     string
	Prompt   string
	Schedule string
	Timezone *string
	Enabled  bool
}

// UpdateCronJobParams holds the fields that can be updated on a cron job.
// Nil fields are not updated.
type UpdateCronJobParams struct {
	Name      *string
	Prompt    *string
	Schedule  *string
	Timezone  **string // double pointer: nil = don't update, *nil = set to NULL
	Enabled   *bool
	NextRunAt **time.Time // double pointer: nil = don't update, *nil = clear
}

// UpdateCronJobLastRunParams records the outcome of a cron job fire.
type UpdateCronJobLastRunParams struct {
	SessionID *string // nil = don't update; otherwise set even if empty string
	RanAt     time.Time
	Outcome   models.CronJobOutcome
	NextRunAt *time.Time // nil = clear (job disabled or schedule invalid)
}

// CronJobStore defines the interface for cron job persistence.
type CronJobStore interface {
	Create(ctx context.Context, params CreateCronJobParams) (*models.CronJob, error)
	Get(ctx context.Context, id string) (*models.CronJob, error)
	List(ctx context.Context) ([]*models.CronJob, error)
	ListByRepo(ctx context.Context, repoID string) ([]*models.CronJob, error)
	ListEnabled(ctx context.Context) ([]*models.CronJob, error)
	Update(ctx context.Context, id string, params UpdateCronJobParams) (*models.CronJob, error)
	// MarkFireStarted records that a cron job has fired and spawned a session.
	// It updates last_run_session_id, last_run_at, and next_run_at but does
	// NOT touch last_run_outcome — outcome is written later by the finalize
	// pipeline via UpdateLastRun. Use this at fire time; use UpdateLastRun
	// for terminal outcomes.
	MarkFireStarted(ctx context.Context, id string, sessionID string, firedAt time.Time, nextRunAt *time.Time) error
	UpdateLastRun(ctx context.Context, id string, params UpdateCronJobLastRunParams) error
	Delete(ctx context.Context, id string) error
}
