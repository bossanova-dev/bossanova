package db

import (
	"context"
	"errors"
	"time"

	"github.com/recurser/bossalib/models"
)

// ErrCronJobLastRunSuperseded is returned by UpdateLastRun when a guarded write
// (ExpectedSessionID set) does not match: the cron job's last_run_session_id is
// no longer the expected session — a newer run already called MarkFireStarted
// and replaced the pointer — or the row is gone. Callers recording an older
// run's finalize outcome treat this as a benign skip, not a failure, so a late
// finalize can't move the pointer back to a stale session and re-enable overlap.
var ErrCronJobLastRunSuperseded = errors.New("cron job last run superseded by newer run")

// ErrStaleRepoUpdate is returned by RepoStore.Update when an optimistic-
// concurrency guard (UpdateRepoParams.ExpectedUpdatedAt set) does not match the
// repo's current updated_at: another writer advanced the row since the caller
// last read it. The write is rolled back atomically — no field is changed — so
// callers surface a conflict (connect.CodeAborted) rather than clobbering the
// concurrent edit. Distinct from sql.ErrNoRows, which means the row is gone.
var ErrStaleRepoUpdate = errors.New("repo update rejected: stale updated_at token")

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
	DisplayName            *string
	OriginURL              *string
	DefaultBaseBranch      *string
	WorktreeBaseDir        *string
	SetupScript            **string // double pointer: nil = don't update, *nil = set to NULL
	CanAutoMerge           *bool
	CanAutoMergeDependabot *bool
	CanAutoRepair          *bool
	CanAutoRotate          *bool
	// ArchiveSessionsAfterMerge toggles the post-merge auto-archive automation.
	ArchiveSessionsAfterMerge *bool
	MergeStrategy             *models.MergeStrategy
	LinearAPIKey              *string
	SentryAPIKey              *string
	SentryOrg                 *string
	// ExpectedUpdatedAt, when non-nil, enables an optimistic-concurrency guard:
	// the update is applied only while the repo's stored updated_at still matches
	// this token (compared at the stored millisecond string granularity). A
	// mismatch returns ErrStaleRepoUpdate and rolls back atomically; a missing
	// row returns sql.ErrNoRows.
	ExpectedUpdatedAt *time.Time
}

// RepoStore defines the interface for repo persistence.
type RepoStore interface {
	Create(ctx context.Context, params CreateRepoParams) (*models.Repo, error)
	Get(ctx context.Context, id string) (*models.Repo, error)
	GetByPath(ctx context.Context, localPath string) (*models.Repo, error)
	GetByOrigin(ctx context.Context, originURL string) (*models.Repo, error)
	List(ctx context.Context) ([]*models.Repo, error)
	Update(ctx context.Context, id string, params UpdateRepoParams) (*models.Repo, error)
	Delete(ctx context.Context, id string) error
}

// CreateTaskMappingParams holds the parameters for creating a new task mapping.
type CreateTaskMappingParams struct {
	ExternalID string
	PluginName string
	RepoID     string
	RetryCount int
}

// UpdateTaskMappingParams holds the fields that can be updated on a task mapping.
// Nil fields are not updated.
type UpdateTaskMappingParams struct {
	SessionID            **string // double pointer: nil = don't update, *nil = set to NULL
	Status               *models.TaskMappingStatus
	LastError            **string                   // double pointer: nil = don't update, *nil = clear
	PendingUpdateStatus  **models.TaskMappingStatus // double pointer: nil = don't update, *nil = clear
	PendingUpdateDetails **string                   // double pointer: nil = don't update, *nil = clear
	RetryCount           *int                       // nil = don't update
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
	ListRecentFailures(ctx context.Context, limit int) ([]*models.TaskMapping, error)
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
	AgentName    string // Agent plugin name; daemon callers should pass a resolved name. Empty falls back to "claude" for legacy callers.
	Model        string // Opaque agent model id; "" = plugin default.
	// AccountID binds the session to a rotation account; nil/empty = the
	// system-default account 0 (no injected env, D9).
	AccountID  *string
	PRNumber   *int
	PRURL      *string
	TrackerID  *string
	TrackerURL *string
}

// UpdateSessionParams holds the fields that can be updated on a session.
type UpdateSessionParams struct {
	Title                   *string
	State                   *int
	WorktreePath            *string
	BranchName              *string
	AgentSessionID          **string
	PRNumber                **int
	PRURL                   **string
	TrackerID               **string
	TrackerURL              **string
	TmuxSessionName         **string
	LastCheckState          *int
	LastObservedReviewState *int
	AutomationEnabled       *bool
	AttemptCount            *int
	BlockedReason           **string
	// LastAttemptHeadSHA follows the nullable-string double-pointer convention:
	// nil = don't touch, *nil = clear to NULL (reset on green / auto-unblock),
	// *val = set the SHA at which an attempt was just counted (BOS-235).
	LastAttemptHeadSHA **string

	// RotationAttemptCount and RotationResumeAt track account-rotation state
	// for the headless auto-rotation feature (BOS-174).
	RotationAttemptCount *int
	// RotationResumeAt follows the nullable-string double-pointer convention:
	// nil = don't touch, *nil = clear to NULL (resume due / rotation
	// complete), *val = set the ISO 8601 resume time (mirrors
	// LastAttemptHeadSHA).
	RotationResumeAt **string

	ArchivedAt **string // ISO 8601 string or nil
	CronJobID  **string
	HookToken  **string // double pointer: nil = don't update, *nil = clear (cleared on finalize success)
	// AccountID rebinds the session to a rotation account (BOS-171 manual
	// switch). Follows the nullable-string double-pointer convention: nil =
	// don't touch, *nil = clear to NULL (system-default account 0), *val =
	// bind to that account id.
	AccountID      **string
	TmuxUnattended *bool
	// QuickChat marks a visible no-worktree/branch/PR planning chat (BOS-322).
	// nil = don't touch; mirrors TmuxUnattended (set via Update after create).
	QuickChat *bool

	// Composite display fields, updated by the DisplayStatusComputer (Step 2).
	// Pointer-typed so a nil value means "don't touch" and a zero value means
	// "set to empty/zero" — matching the rest of UpdateSessionParams.
	DisplayLabel   *string
	DisplayIntent  *int32
	DisplaySpinner *bool

	// SetupError flags a non-fatal setup-script failure on the session. nil
	// means "don't touch"; a value (including "") sets/clears the column.
	SetupError *string
}

// SessionWithRepo pairs a Session with its owning repo metadata, so
// callers that need both can fetch them in a single join query rather than
// issuing a follow-up Get per session.
type SessionWithRepo struct {
	*models.Session
	RepoDisplayName string
	RepoOriginURL   string
}

// SessionStore defines the interface for session persistence.
type SessionStore interface {
	Create(ctx context.Context, params CreateSessionParams) (*models.Session, error)
	Get(ctx context.Context, id string) (*models.Session, error)
	List(ctx context.Context, repoID string) ([]*models.Session, error)
	ListByState(ctx context.Context, state int) ([]*models.Session, error)
	// ListByStates returns every session whose state is one of the given
	// states. Multi-state variant of ListByState used by the broadened
	// stranded-cron reap (BOS-333); an empty slice returns nil.
	ListByStates(ctx context.Context, states []int) ([]*models.Session, error)
	ListActive(ctx context.Context, repoID string) ([]*models.Session, error)
	ListActiveWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error)
	ListWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error)
	ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*SessionWithRepo, error)
	ListArchived(ctx context.Context, repoID string) ([]*models.Session, error)
	Update(ctx context.Context, id string, params UpdateSessionParams) (*models.Session, error)
	// UpdateStateConditional runs the conditional `UPDATE sessions SET
	// state=newState WHERE id=? AND state=expectedState` used as the
	// idempotency gate for the Stop-hook finalize endpoint. Returns true if
	// exactly one row transitioned; false if the row was already past the
	// expected state (duplicate event or stale transition).
	UpdateStateConditional(ctx context.Context, id string, newState, expectedState int) (bool, error)
	// UpdateStateConditionalFrom transitions the session to newState only when
	// its current state is one of expectedStates. From-set variant of
	// UpdateStateConditional backing the broadened stranded-cron reap
	// (BOS-333); an empty expectedStates slice is a no-op returning false.
	UpdateStateConditionalFrom(ctx context.Context, id string, newState int, expectedStates []int) (bool, error)
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

	// UpdateRepairBlocked records the blocked-refusal lane: the daemon
	// refused to START a repair chat (a FailedPrecondition displace/reclaim
	// refusal). It sets last_repair_blocked_reason + last_repair_blocked_at
	// WITHOUT touching last_repair_attempt_count or the runner/exit error
	// fields, so a start-refusal never counts as a repair failure or feeds
	// the exponential backoff. UpdateRepairDiagnostics clears the pair on
	// the next real outcome.
	UpdateRepairBlocked(ctx context.Context, sessionID string, at time.Time, reason string) error
}

// OrphanHeadlessRunStore atomically stamps the daemon-restart orphan reason
// while moving a headless run from ImplementingPlan to Orphaned.
type OrphanHeadlessRunStore interface {
	OrphanHeadlessRun(ctx context.Context, id, reason string) (bool, error)
}

// UnarchivedOrphanClaimStore atomically claims an unarchived Orphaned session
// with its expected daemon-restart marker for auto-resume, allowing an archive
// or marker change to win over a stale sweep snapshot.
type UnarchivedOrphanClaimStore interface {
	ClaimUnarchivedOrphan(ctx context.Context, id, reason string) (bool, error)
}

// OrphanResumeStore owns the complete conditional handoff for an orphaned
// headless run. Each operation checks the claim shape in SQL and returns its
// affected-row result without a follow-up read, so a concurrent completion or
// archive cannot be overwritten after the initial claim.
type OrphanResumeStore interface {
	UnarchivedOrphanClaimStore
	CommitOrphanResume(ctx context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error)
	ReleaseOrphanResumeClaim(ctx context.Context, id, reason string, priorAgentSession *string) (bool, error)
	ReparkOrphanResume(ctx context.Context, id, reason string, priorAgentSession *string, newAgentSessionID string) (bool, error)
}

// UpdateRepairDiagnosticsParams carries the per-attempt outcome that the
// repair plugin reports via host.RecordRepairOutcome.
type UpdateRepairDiagnosticsParams struct {
	SessionID         string
	StartedAt         time.Time
	RunnerError       string
	ExitError         string
	HeadSHA           string
	DisplayStatus     int32
	ReviewFingerprint *string
}

// CreateAgentChatParams holds the parameters for creating a new agent chat record.
type CreateAgentChatParams struct {
	SessionID         string
	AgentSessionID    string
	ProviderSessionID *string
	AgentName         string // Agent plugin name; empty falls back to "claude".
	Title             string
	// AccountID binds the chat to a rotation account; nil/empty = the
	// system-default account 0 (no injected env, D9).
	AccountID *string
	// Model is the per-chat agent model id; empty inherits the agent CLI
	// default (BOS-381). bossd never enumerates valid models.
	Model string
}

// AgentChatStore defines the interface for agent chat persistence.
type AgentChatStore interface {
	Create(ctx context.Context, params CreateAgentChatParams) (*models.AgentChat, error)
	GetByAgentSessionID(ctx context.Context, agentSessionID string) (*models.AgentChat, error)
	ListBySession(ctx context.Context, sessionID string) ([]*models.AgentChat, error)
	ListBySessions(ctx context.Context, sessionIDs []string) (map[string][]*models.AgentChat, error)
	UpdateTitle(ctx context.Context, id string, title string) error
	UpdateTitleByAgentSessionID(ctx context.Context, agentSessionID string, title string) error
	UpdateAgentSessionID(ctx context.Context, id, oldAgentSessionID, newAgentSessionID string) error
	UpdateTmuxSessionName(ctx context.Context, agentSessionID string, name *string) error
	UpdateProviderSessionID(ctx context.Context, agentSessionID string, providerSessionID *string) error
	UpdateAccountIDByAgentSessionID(ctx context.Context, agentSessionID string, accountID *string) error
	// MarkStartFailed records a short human-readable reason that the
	// agent never came up for this chat, AND clears tmux_session_name
	// in the same statement so the chat is no longer treated as
	// resumable. Used by StartTmuxChat's failure paths (SendPlan
	// timeout, hook misconfiguration, post-create errors) instead of
	// DeleteByAgentSessionID: the row is preserved so the chat list
	// can surface the failed attempt.
	MarkStartFailed(ctx context.Context, agentSessionID, reason string) error
	DeleteByAgentSessionID(ctx context.Context, agentSessionID string) error
	ListWithTmuxSession(ctx context.Context) ([]*models.AgentChat, error)
	// ListRoutableChats returns chats the daemon can route to for the
	// upstream reconnect snapshot: tmux-hosted chats plus headless runs
	// (which have no tmux session name), excluding failed-start rows.
	ListRoutableChats(ctx context.Context) ([]*models.AgentChat, error)
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
	RepoID          string
	Name            string
	Prompt          string
	Schedule        string
	Timezone        *string
	AgentName       string // Agent plugin name; empty falls back to "claude".
	Model           string // Opaque agent model id; "" = plugin default.
	Enabled         bool
	GateCommand     string // shell command to run before firing; "" = no gate
	RunSetupCommand bool   // whether to run the repo setup script before the agent session
}

// UpdateCronJobParams holds the fields that can be updated on a cron job.
// Nil fields are not updated.
type UpdateCronJobParams struct {
	Name            *string
	Prompt          *string
	Schedule        *string
	Timezone        **string // double pointer: nil = don't update, *nil = set to NULL
	AgentName       *string
	Model           *string // nil = don't update; "" is a real value (plugin default)
	Enabled         *bool
	NextRunAt       **time.Time // double pointer: nil = don't update, *nil = clear
	GateCommand     *string     // nil = don't update; "" = clear gate
	RunSetupCommand *bool       // nil = don't update
}

// UpdateCronJobLastRunParams records the outcome of a cron job fire.
type UpdateCronJobLastRunParams struct {
	SessionID *string // nil = don't update; otherwise set even if empty string
	// ExpectedSessionID, when non-nil, guards the write: the row is updated only
	// while cron_jobs.last_run_session_id still equals this value. A newer run
	// that already called MarkFireStarted will have replaced the pointer, in
	// which case this (older) run's finalize outcome must not clobber it; the
	// guarded no-match is reported as ErrCronJobLastRunSuperseded.
	ExpectedSessionID *string
	// AllowClearedExpectedSessionID permits a guarded update when the expected
	// session was deleted and session deletion already cleared last_run_session_id
	// to NULL. A non-NULL different session still counts as superseded.
	AllowClearedExpectedSessionID bool
	RanAt                         time.Time
	Outcome                       models.CronJobOutcome
	NextRunAt                     *time.Time // nil = clear (job disabled or schedule invalid)
}

// CreateAccountParams holds the fields required to create an account.
// Status defaults to active and Health to ok on create.
type CreateAccountParams struct {
	Provider      models.AccountProvider
	Label         string
	Priority      int
	Tier          string
	AllowedModels []string
}

// UpdateAccountParams holds updatable account fields. Nil fields are not
// updated. Double-pointer fields clear to NULL when *field == nil.
type UpdateAccountParams struct {
	Label         *string
	Status        *models.AccountStatus
	Priority      *int
	Health        *models.AccountHealth
	CooldownUntil **time.Time // nil = don't update; *nil = set NULL
	LastUsedAt    **time.Time // nil = don't update; *nil = set NULL
	Tier          *string
	AllowedModels *[]string
}

// AccountStore persists account-registry metadata (no secrets — see accountcred).
type AccountStore interface {
	Create(ctx context.Context, params CreateAccountParams) (*models.Account, error)
	Get(ctx context.Context, id string) (*models.Account, error)
	List(ctx context.Context) ([]*models.Account, error) // ORDER BY provider, priority, created_at
	ListByProvider(ctx context.Context, p models.AccountProvider) ([]*models.Account, error)
	Update(ctx context.Context, id string, params UpdateAccountParams) (*models.Account, error)
	Delete(ctx context.Context, id string) error
	// RecordTestResult updates only the last-test bookkeeping columns
	// (last_test_ok_at, last_test_error) for a row. okAt nil clears
	// last_test_ok_at to NULL; testErr is written verbatim ("" = no error).
	// Returns sql.ErrNoRows when the account does not exist.
	RecordTestResult(ctx context.Context, id string, okAt *time.Time, testErr string) error
	// RecordUsageProbe overwrites only cached usage-snapshot metadata for a row
	// and bumps updated_at. The snapshot never carries credentials. Returns
	// sql.ErrNoRows when the account does not exist.
	RecordUsageProbe(ctx context.Context, id string, snap models.UsageSnapshot) error
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
