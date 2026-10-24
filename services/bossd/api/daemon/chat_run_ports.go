package server

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

// agentChatStore contains the chat operations consumed by the daemon door.
type agentChatStore interface {
	Create(context.Context, domain.CreateAgentChatParams) (*domain.AgentChat, error)
	GetByAgentSessionID(context.Context, string) (*domain.AgentChat, error)
	ListBySession(context.Context, string) ([]*domain.AgentChat, error)
	ListBySessions(context.Context, []string) (map[string][]*domain.AgentChat, error)
	UpdateTitleByAgentSessionID(context.Context, string, string) error
	UpdateTmuxSessionName(context.Context, string, *string) error
	UpdateProviderSessionID(context.Context, string, *string) error
	UpdateAgentAccountIDByAgentSessionID(context.Context, string, *string) error
	DeleteByAgentSessionID(context.Context, string) error
}

// agentRunStore records interactive launches and supplies the run-cost projection.
type agentRunReader interface {
	List(context.Context, domain.AgentRunFilter) ([]domain.AgentRun, error)
}

type agentRunStore interface {
	agentRunReader
	Create(context.Context, domain.AgentRun) (domain.AgentRun, error)
	MarkStopped(context.Context, string, string, time.Time) error
	RecordTelemetry(context.Context, string, domain.AgentRunTelemetry) error
	CreateBackfilled(context.Context, domain.AgentRunBackfillParams) (domain.AgentRunBackfillSummary, error)
}

type taskMappingFailureReader interface {
	ListByRecentFailure(context.Context, int) ([]*domain.TaskMapping, error)
}

type resumeAttemptReader interface {
	ListByBudget(context.Context, string, string) ([]*domain.ResumeAttempt, error)
}

type pendingTrackerMergeStore interface {
	Upsert(context.Context, domain.PendingTrackerMerge) error
	ListByDueAt(context.Context, time.Time) ([]domain.PendingTrackerMerge, error)
	Delete(context.Context, string) error
}
