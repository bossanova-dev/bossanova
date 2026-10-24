package server

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

// agentAccountStore contains the registry operations used by account RPCs.
type agentAccountStore interface {
	Create(context.Context, domain.CreateAgentAccountParams) (*domain.AgentAccount, error)
	Get(context.Context, string) (*domain.AgentAccount, error)
	List(context.Context) ([]*domain.AgentAccount, error)
	ListByProvider(context.Context, domain.AgentAccountProvider) ([]*domain.AgentAccount, error)
	Update(context.Context, string, domain.UpdateAgentAccountParams) (*domain.AgentAccount, error)
	Delete(context.Context, string) error
	RecordTestResult(context.Context, string, *time.Time, string) error
}

// authCheckClearer is the optional credential-verification withdrawal operation.
type authCheckClearer interface {
	MarkAuthCheckCleared(context.Context, string) error
}
