package domain

import (
	"errors"
	"time"

	"github.com/recurser/bossd/internal/domain/agenttelemetry"
)

// AgentRun is one daemon-observed AgentRunner.StartAgentRun lifecycle.
type AgentRun struct {
	ID                   string
	SessionID            string
	AgentSessionID       string
	AgentName            string
	Model                string
	Effort               string
	StartedAt            time.Time
	StoppedAt            *time.Time
	StopReason           string
	ParentModelCallCount int64
	ChildModelCallCount  int64
	ToolCallCount        int64
	SubagentCount        int64
	DirectSubagentCount  int64
	OutputTokenCount     *int64
	ReasoningTokenCount  *int64
	// ReviewerDispatchCount is how many reviewer subagents the run dispatched —
	// worker prompts leading with the `[bs-reviewer-dispatch]` marker.
	ReviewerDispatchCount int64
	// TerminalState is the boss-build terminal state the run ended in, or "".
	TerminalState   string
	IsBackfilled    bool
	RepoDisplayName string
	Children        []AgentRunChild
}

// AgentRunChild is one child span attributed to an agent run.
type AgentRunChild struct {
	ID                  string
	AgentRunID          string
	AgentSessionID      string
	ParentAgentID       string
	SpawnDepth          int32
	StartedAt           time.Time
	StoppedAt           *time.Time
	ModelCallCount      int64
	ToolCallCount       int64
	OutputTokenCount    *int64
	ReasoningTokenCount *int64
}

type AgentRunTelemetry struct {
	ParentModelCallCount  int64
	ChildModelCallCount   int64
	ToolCallCount         int64
	SubagentCount         int64
	DirectSubagentCount   int64
	OutputTokenCount      *int64
	ReasoningTokenCount   *int64
	ReviewerDispatchCount int64
	TerminalState         string
	Children              []AgentRunChild
}

type AgentRunFilter struct {
	SessionID         string
	AgentName         string
	RepoDisplayName   string
	Since             *time.Time
	Until             *time.Time
	IncludeOpen       bool
	IncludeAllReasons bool
	IncludeBackfilled bool
	OmitChildren      bool
}

// AgentRunBackfillParams bounds a transcript replay.
type AgentRunBackfillParams struct {
	Since *time.Time
	Until *time.Time
}

type AgentRunBackfillSummary struct {
	InsertedCount int64
	SkippedCount  int64
}

// AgentRunTelemetryFromCounts translates transcript counts into persisted run telemetry.
func AgentRunTelemetryFromCounts(counts agenttelemetry.Counts) AgentRunTelemetry {
	out := AgentRunTelemetry{
		ParentModelCallCount: counts.ParentModelCallCount,
		ChildModelCallCount:  counts.ChildModelCallCount,
		ToolCallCount:        counts.ToolCallCount,
		SubagentCount:        counts.SubagentCount,
		DirectSubagentCount:  counts.DirectSubagentCount,
		OutputTokenCount:     counts.OutputTokenCount,
		ReasoningTokenCount:  counts.ReasoningTokenCount,

		ReviewerDispatchCount: counts.ReviewerDispatchCount,
		TerminalState:         counts.TerminalState,
	}
	for _, child := range counts.Children {
		out.Children = append(out.Children, AgentRunChild{
			AgentSessionID:      child.AgentSessionID,
			ParentAgentID:       child.ParentAgentID,
			SpawnDepth:          child.SpawnDepth,
			StartedAt:           child.StartedAt,
			StoppedAt:           child.StoppedAt,
			ModelCallCount:      child.ModelCallCount,
			ToolCallCount:       child.ToolCallCount,
			OutputTokenCount:    child.OutputTokenCount,
			ReasoningTokenCount: child.ReasoningTokenCount,
		})
	}
	return out
}

// ErrAuthMismatch indicates a bearer token does not match its registered run.
var ErrAuthMismatch = errors.New("agent run auth mismatch")

// ErrAgentRunNotFound indicates an agent session has no registered run.
var ErrAgentRunNotFound = errors.New("agent run not found")

const (
	AgentRunStopClean          = "clean"
	AgentRunStopStopped        = "stopped"
	AgentRunStopUsageExhausted = "usage_exhausted"
	AgentRunStopRateLimited    = "rate_limited"
	AgentRunStopDaemonRestart  = "daemon_restart"
	AgentRunStopUnknown        = "unknown"
)

const (
	AgentRunTerminalReviewReady = "REVIEW_READY"
	AgentRunTerminalPartial     = "PARTIAL"
	AgentRunTerminalBlocked     = "BLOCKED"
	AgentRunTerminalNoChange    = "NO_CHANGE"
	AgentRunTerminalUnrecorded  = ""
)
