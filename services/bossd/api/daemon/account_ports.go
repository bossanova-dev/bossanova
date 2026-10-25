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

type accountIdentityWriter interface {
	SetIdentity(context.Context, string, domain.AgentAccountIdentity) error
}

func (s *Server) publishAgentAccountsChanged() {
	if s.agentAccountsChanged != nil {
		s.agentAccountsChanged()
	}
}

// AgentAccountCredentialStore is the credential-blob plane for accounts. The daemon
// wires a keyring-backed accountcred.Store; tests inject a fake. It is defined
// here (rather than importing accountcred) so the account RPCs depend only on
// this narrow surface and secrets stay out of SQLite. Save overwrites; Load and
// Delete return accountcred.ErrCredentialNotFound when no entry exists.
type AgentAccountCredentialStore interface {
	Save(accountID string, blob []byte) error
	Load(accountID string) ([]byte, error)
	Delete(accountID string) error
}
