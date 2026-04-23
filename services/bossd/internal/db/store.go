package db

import (
	"context"

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
	ClaudeSessionID   **string
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
	ListActive(ctx context.Context, repoID string) ([]*models.Session, error)
	ListActiveWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error)
	ListWithRepo(ctx context.Context, repoID string) ([]*SessionWithRepo, error)
	ListArchived(ctx context.Context, repoID string) ([]*models.Session, error)
	Update(ctx context.Context, id string, params UpdateSessionParams) (*models.Session, error)
	Archive(ctx context.Context, id string) error
	Resurrect(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	AdvanceOrphanedSessions(ctx context.Context) (int64, error)
}

// CreateClaudeChatParams holds the parameters for creating a new Claude chat record.
type CreateClaudeChatParams struct {
	SessionID string
	ClaudeID  string
	Title     string
}

// ClaudeChatStore defines the interface for Claude chat persistence.
type ClaudeChatStore interface {
	Create(ctx context.Context, params CreateClaudeChatParams) (*models.ClaudeChat, error)
	GetByClaudeID(ctx context.Context, claudeID string) (*models.ClaudeChat, error)
	ListBySession(ctx context.Context, sessionID string) ([]*models.ClaudeChat, error)
	UpdateTitle(ctx context.Context, id string, title string) error
	UpdateTitleByClaudeID(ctx context.Context, claudeID string, title string) error
	UpdateTmuxSessionName(ctx context.Context, claudeID string, name *string) error
	DeleteByClaudeID(ctx context.Context, claudeID string) error
	ListWithTmuxSession(ctx context.Context) ([]*models.ClaudeChat, error)
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
