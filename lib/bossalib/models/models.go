// Package models defines the core domain types for Bossanova.
// These are the Go-native types used throughout the application.
// Proto conversion functions bridge these with the generated protobuf types.
package models

import (
	"time"

	"github.com/recurser/bossalib/machine"
)

// MergeStrategy represents the merge method used when auto-merging PRs.
// Default is MergeStrategyMerge, matching GitHub's default.
type MergeStrategy string

const (
	MergeStrategyMerge  MergeStrategy = "merge"
	MergeStrategyRebase MergeStrategy = "rebase"
	MergeStrategySquash MergeStrategy = "squash"
)

// Repo represents a registered Git repository.
type Repo struct {
	ID                      string
	DisplayName             string
	LocalPath               string
	OriginURL               string
	DefaultBaseBranch       string
	WorktreeBaseDir         string
	SetupScript             *string
	CanAutoMerge            bool
	CanAutoMergeDependabot  bool
	CanAutoAddressReviews   bool
	CanAutoResolveConflicts bool
	MergeStrategy           MergeStrategy
	LinearAPIKey            string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Session represents a Claude coding session.
type Session struct {
	ID                string
	RepoID            string
	Title             string
	Plan              string
	WorktreePath      string
	BranchName        string
	BaseBranch        string
	State             machine.State
	ClaudeSessionID   *string
	PRNumber          *int
	PRURL             *string
	TrackerID         *string
	TrackerURL        *string
	LastCheckState    machine.CheckState
	AutomationEnabled bool
	AttemptCount      int
	BlockedReason     *string
	ArchivedAt        *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Attempt represents a fix attempt within a session.
type Attempt struct {
	ID        string
	SessionID string
	Trigger   AttemptTrigger
	Result    AttemptResult
	Error     *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ClaudeChat represents a Claude Code conversation associated with a session.
type ClaudeChat struct {
	ID        string
	SessionID string
	ClaudeID  string // Claude Code session UUID
	Title     string
	DaemonID  string // Originating daemon (empty = local)
	CreatedAt time.Time
}

// TaskMappingStatus represents the state of a task mapping.
type TaskMappingStatus int

const (
	TaskMappingStatusPending    TaskMappingStatus = iota // Discovered, not yet acted on
	TaskMappingStatusInProgress                          // Session created or merge in progress
	TaskMappingStatusCompleted                           // Successfully completed
	TaskMappingStatusFailed                              // Failed (session failed, merge rejected, etc.)
	TaskMappingStatusSkipped                             // Skipped (e.g. previously-rejected)
)

// TaskMapping tracks the relationship between an external task (e.g. a
// dependabot PR) and a bossanova session. Used for dedup and status tracking.
type TaskMapping struct {
	ID                   string
	ExternalID           string
	PluginName           string
	SessionID            *string
	RepoID               string
	Status               TaskMappingStatus
	PendingUpdateStatus  *TaskMappingStatus
	PendingUpdateDetails *string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// AttemptTrigger represents what triggered a fix attempt.
type AttemptTrigger int

const (
	AttemptTriggerUnspecified AttemptTrigger = iota
	AttemptTriggerCheckFailure
	AttemptTriggerConflict
	AttemptTriggerReviewFeedback
)

// AttemptResult represents the outcome of a fix attempt.
type AttemptResult int

const (
	AttemptResultUnspecified AttemptResult = iota
	AttemptResultSuccess
	AttemptResultFailed
	AttemptResultIncomplete
)

func (t AttemptTrigger) String() string {
	switch t {
	case AttemptTriggerCheckFailure:
		return "check_failure"
	case AttemptTriggerConflict:
		return "conflict"
	case AttemptTriggerReviewFeedback:
		return "review_feedback"
	default:
		return "unspecified"
	}
}

func (r AttemptResult) String() string {
	switch r {
	case AttemptResultSuccess:
		return "success"
	case AttemptResultFailed:
		return "failed"
	case AttemptResultIncomplete:
		return "incomplete"
	default:
		return "unspecified"
	}
}
