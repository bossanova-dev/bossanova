package models

import "time"

// WorkflowStatus represents the state of an autopilot workflow.
type WorkflowStatus string

const (
	WorkflowStatusPending   WorkflowStatus = "pending"
	WorkflowStatusRunning   WorkflowStatus = "running"
	WorkflowStatusPaused    WorkflowStatus = "paused"
	WorkflowStatusCompleted WorkflowStatus = "completed"
	WorkflowStatusFailed    WorkflowStatus = "failed"
	WorkflowStatusCancelled WorkflowStatus = "cancelled"
)

// WorkflowStep represents the current step of an autopilot workflow.
type WorkflowStep string

const (
	WorkflowStepPlan      WorkflowStep = "plan"
	WorkflowStepImplement WorkflowStep = "implement"
	WorkflowStepHandoff   WorkflowStep = "handoff"
	WorkflowStepResume    WorkflowStep = "resume"
	WorkflowStepVerify    WorkflowStep = "verify"
	WorkflowStepLand      WorkflowStep = "land"
)

// Workflow represents an autopilot workflow execution.
type Workflow struct {
	ID             string
	SessionID      string
	RepoID         string
	PlanPath       string
	Status         WorkflowStatus
	CurrentStep    WorkflowStep
	FlightLeg      int
	MaxLegs        int
	LastError      *string
	StartCommitSHA *string
	ConfigJSON     *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
