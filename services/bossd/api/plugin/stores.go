package plugin

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

type RepoStore interface {
	Get(context.Context, string) (*domain.Repo, error)
	List(context.Context) ([]*domain.Repo, error)
}
type SessionStore interface {
	Get(context.Context, string) (*domain.Session, error)
	ListActive(context.Context, string) ([]*domain.Session, error)
	ListByRepoAndPR(context.Context, string, int) ([]*domain.SessionWithRepo, error)
	ListActiveWithRepo(context.Context, string) ([]*domain.SessionWithRepo, error)
	Update(context.Context, string, domain.UpdateSessionParams) (*domain.Session, error)
	UpdateRepairBlocked(context.Context, string, time.Time, string) error
	UpdateRepairDiagnostics(context.Context, domain.UpdateRepairDiagnosticsParams) error
}
type AgentChatStore interface {
	ListBySession(context.Context, string) ([]*domain.AgentChat, error)
	GetByAgentSessionID(context.Context, string) (*domain.AgentChat, error)
	DeleteByAgentSessionID(context.Context, string) error
	UpdateProviderSessionID(context.Context, string, *string) error
}
type AgentRunStore interface {
	Create(context.Context, domain.AgentRun) (domain.AgentRun, error)
	MarkStoppedByAgentSessionID(context.Context, string, string, time.Time) error
	RecordTelemetry(context.Context, string, domain.AgentRunTelemetry) error
	RecordTelemetryByAgentSessionID(context.Context, string, domain.AgentRunTelemetry) error
	List(context.Context, domain.AgentRunFilter) ([]domain.AgentRun, error)
}
