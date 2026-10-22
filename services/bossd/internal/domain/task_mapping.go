package domain

import "time"

// TaskMappingStatus represents the state of a task mapping.
type TaskMappingStatus int

const (
	TaskMappingStatusPending    TaskMappingStatus = iota // Discovered, not yet acted on
	TaskMappingStatusInProgress                          // Session created or merge in progress
	TaskMappingStatusCompleted                           // Successfully completed
	TaskMappingStatusFailed                              // Failed (session failed, merge rejected, etc.)
	TaskMappingStatusSkipped                             // Skipped (e.g. previously-rejected)
	TaskMappingStatusOrphaned                            // Driving goroutine died (e.g. daemon restart); eligible for re-pickup
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
	LastError            *string
	PendingUpdateStatus  *TaskMappingStatus
	PendingUpdateDetails *string
	// RetryCount counts how many times a terminal mapping has been
	// re-processed into a fresh repair attempt. Bounds sequential re-repairs
	// of an unfixable PR (see Orchestrator.processTask).
	RetryCount int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}
