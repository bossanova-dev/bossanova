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

// accountCredentialLocker is implemented by accountcred.Store. It is kept
// optional so custom credential stores and existing test fakes do not need to
// provide locking, while production refreshes share the materializer's
// per-account load-merge-save lock.
type accountCredentialLocker interface {
	WithCredentialLock(accountID string, fn func() error) error
}

// AgentAccountSmokeRunner runs a trivial live provider invocation to prove a
// credential works (claude -p "ok" / codex equivalent). It is injected and
// nil-safe: when absent, TestAgentAccount records an unavailable smoke result.
type AgentAccountSmokeRunner interface {
	Smoke(ctx context.Context, accountID, provider string, blob []byte) error
}

// UsageProbeRecorder probes and caches usage metadata for one account. It is
// fail-soft at call sites: refresh errors never make ListAgentAccounts fail.
type UsageProbeRecorder interface {
	RecordUsageProbe(ctx context.Context, accountID string) error
}

// AgentAccountMaterializations purges the on-disk credential materialization for one
// account (the plaintext tree an agent runner reads, e.g. codex's auth.json).
// Optional: a nil capability leaves RemoveAgentAccount behaving as before. The
// implementation is provider-aware — the RPC always calls it and lets the
// adapter no-op for providers that materialize nothing.
type AgentAccountMaterializations interface {
	RemoveMaterialization(ctx context.Context, provider, accountID string) error
}

func (s *Server) publishAccountCredential(ctx context.Context, a *domain.AgentAccount) {
	if s.credentialPublisher == nil || a == nil || a.SyncedAgentAccountID == "" {
		return
	}
	if err := s.credentialPublisher(ctx, a.SyncedAgentAccountID, a.ID); err != nil {
		s.logger.Warn().Err(err).Str("account_id", a.ID).Msg("credential distribution deferred")
	}
}
