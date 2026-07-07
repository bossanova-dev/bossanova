// Package accountwiring adapts the bossd daemon's concrete stores and plugin
// clients to the degrade-safe account.Resolver seams (Registry, Materializer)
// and exposes the session-aware spawn-env resolver consumed by the session
// Lifecycle and the plugin HostService.
//
// It lives outside internal/account so the resolver stays a pure library with
// no dependency on db, agent, or the generated protobufs; this package holds
// all the daemon-specific plumbing. It is imported only by cmd/main.go.
//
// Nothing here ever logs a credential blob or a materialized env value.
package accountwiring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// AccountStore is the narrow slice of db.AccountStore the adapters read. The
// daemon's *db.SQLiteAccountStore satisfies it.
type AccountStore interface {
	List(ctx context.Context) ([]*models.Account, error)
	Get(ctx context.Context, id string) (*models.Account, error)
	Update(ctx context.Context, id string, params db.UpdateAccountParams) (*models.Account, error)
}

// CredentialLoader loads an account's opaque credential blob. Satisfied by the
// keyring-backed accountcred.Store. Blobs are never logged.
type CredentialLoader interface {
	Load(accountID string) ([]byte, error)
}

// --- Registry adapter -----------------------------------------------------

type registryAdapter struct{ store AccountStore }

// NewRegistry adapts an AccountStore to account.Registry. A nil store yields a
// registry that reports no accounts (the resolver then degrades to account 0).
func NewRegistry(store AccountStore) account.Registry { return &registryAdapter{store: store} }

func (a *registryAdapter) List(ctx context.Context) ([]account.AccountMeta, error) {
	if a.store == nil {
		return nil, nil
	}
	rows, err := a.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]account.AccountMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMeta(r))
	}
	return out, nil
}

func (a *registryAdapter) Get(ctx context.Context, id string) (account.AccountMeta, bool, error) {
	if a.store == nil {
		return account.AccountMeta{}, false, nil
	}
	r, err := a.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return account.AccountMeta{}, false, nil
		}
		return account.AccountMeta{}, false, err
	}
	return toMeta(r), true, nil
}

func (a *registryAdapter) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	if a.store == nil {
		return nil
	}
	// db.UpdateAccountParams.LastUsedAt is **time.Time: a non-nil outer pointer
	// selects the column, and the inner pointer's value is written.
	tp := &at
	_, err := a.store.Update(ctx, id, db.UpdateAccountParams{LastUsedAt: &tp})
	return err
}

func toMeta(a *models.Account) account.AccountMeta {
	return account.AccountMeta{
		ID:           a.ID,
		Provider:     string(a.Provider),
		Label:        a.Label,
		Status:       string(a.Status),
		Health:       string(a.Health),
		Priority:     a.Priority,
		CoolingUntil: a.CooldownUntil,
		LastUsedAt:   a.LastUsedAt,
	}
}

// --- Materializer adapter -------------------------------------------------

type materializerAdapter struct {
	clients map[string]agent.AgentRunnerClient
	store   AccountStore
	creds   CredentialLoader
	log     zerolog.Logger
}

// NewMaterializer adapts the per-provider agent-runner clients, the account
// store, and the credential loader to account.Materializer. Any of them may be
// nil; the adapter then degrades to no-rotation (nil env), never erroring on a
// spawn.
func NewMaterializer(
	clients map[string]agent.AgentRunnerClient,
	store AccountStore,
	creds CredentialLoader,
	log zerolog.Logger,
) account.Materializer {
	return &materializerAdapter{clients: clients, store: store, creds: creds, log: log}
}

func (m *materializerAdapter) client(provider string) (agent.AgentRunnerClient, bool) {
	c, ok := m.clients[provider]
	return c, ok && c != nil
}

// SupportsRotation reports whether provider's plugin can materialize
// credentials. A missing client, codes.Unimplemented, or any other RPC error
// all degrade to (false, nil): an unreachable plugin must never fail a spawn.
func (m *materializerAdapter) SupportsRotation(ctx context.Context, provider string) (bool, error) {
	client, ok := m.client(provider)
	if !ok {
		return false, nil
	}
	resp, err := client.RotationCapability(ctx, &bossanovav1.RotationCapabilityRequest{})
	if err != nil {
		if grpcstatus.Code(err) != codes.Unimplemented {
			m.log.Warn().Err(err).Str("provider", provider).
				Msg("account: RotationCapability failed; treating provider as no-rotation")
		}
		return false, nil
	}
	return resp.GetSupportsRotation(), nil
}

// MaterializeAccount loads the account's credential blob from the keyring and
// asks the provider plugin to turn it into spawn env. Any failure returns
// (nil, err) so the resolver logs and degrades. The blob and the returned env
// are never logged.
func (m *materializerAdapter) MaterializeAccount(ctx context.Context, accountID string) (map[string]string, error) {
	if m.store == nil || m.creds == nil {
		return nil, nil
	}
	acct, err := m.store.Get(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("lookup account for materialize: %w", err)
	}
	client, ok := m.client(string(acct.Provider))
	if !ok {
		return nil, nil
	}
	blob, err := m.creds.Load(accountID)
	if err != nil {
		return nil, fmt.Errorf("load account credential: %w", err)
	}
	resp, err := client.MaterializeAccount(ctx, &bossanovav1.MaterializeAccountRequest{CredentialBlob: blob})
	if err != nil {
		return nil, fmt.Errorf("plugin MaterializeAccount: %w", err)
	}
	return resp.GetEnv(), nil
}

// --- Lifecycle rotation adapters -----------------------------------------

// LifecycleMaterializer adapts account.Materializer to the session Lifecycle's
// rotate-and-restart materializer seam.
type LifecycleMaterializer struct {
	materializer account.Materializer
}

// NewLifecycleMaterializer wraps materializer. A nil materializer yields nil
// env, preserving the lifecycle's fail-safe behavior.
func NewLifecycleMaterializer(materializer account.Materializer) *LifecycleMaterializer {
	return &LifecycleMaterializer{materializer: materializer}
}

// Materialize returns the spawn env for account. It never logs env values.
func (m *LifecycleMaterializer) Materialize(ctx context.Context, account *models.Account) (map[string]string, error) {
	if m == nil || m.materializer == nil || account == nil {
		return nil, nil
	}
	return m.materializer.MaterializeAccount(ctx, account.ID)
}

// RotationBindingResolver adapts the persisted session AccountID binding into
// the rotation signal shape consumed by session.Lifecycle.
type RotationBindingResolver struct {
	registry     account.Registry
	materializer account.Materializer
}

// NewRotationBindingResolver wraps the account registry/materializer for
// lifecycle rotation decisions. Missing deps degrade to unbound/no-capability.
func NewRotationBindingResolver(registry account.Registry, materializer account.Materializer) *RotationBindingResolver {
	return &RotationBindingResolver{registry: registry, materializer: materializer}
}

// CurrentBinding reports the account currently bound to session. It returns
// bound=false for account 0, missing sessions, missing registry, or a stale
// AccountID that no longer exists.
func (r *RotationBindingResolver) CurrentBinding(ctx context.Context, sess *models.Session) (session.RotationBinding, bool, error) {
	if r == nil || sess == nil || sess.AccountID == nil || *sess.AccountID == "" || r.registry == nil {
		return session.RotationBinding{}, false, nil
	}
	accountID := *sess.AccountID
	acct, ok, err := r.registry.Get(ctx, accountID)
	if err != nil {
		return session.RotationBinding{}, false, err
	}
	if !ok {
		return session.RotationBinding{}, false, nil
	}

	provider := acct.Provider
	if provider == "" {
		provider = sess.AgentName
	}
	capable := false
	if r.materializer != nil {
		supported, err := r.materializer.SupportsRotation(ctx, provider)
		if err != nil {
			return session.RotationBinding{}, false, err
		}
		capable = supported
	}
	return session.RotationBinding{
		CappedAccountID: accountID,
		Provider:        provider,
		RotationCapable: capable,
	}, true, nil
}

// --- Session-aware spawn-env resolver -------------------------------------

// SpawnEnvResolver adapts *account.Resolver to the session-aware spawn-env seam
// (Resolve(ctx, sess) map[string]string) consumed by the session Lifecycle and
// the plugin HostService. It never logs env values.
type SpawnEnvResolver struct {
	resolver *account.Resolver
	log      zerolog.Logger
}

// NewSpawnEnvResolver wraps resolver. A nil resolver yields an adapter whose
// Resolve always returns nil (system-default binding).
func NewSpawnEnvResolver(resolver *account.Resolver, log zerolog.Logger) *SpawnEnvResolver {
	return &SpawnEnvResolver{resolver: resolver, log: log}
}

// Resolve returns the per-account spawn env for sess, or nil for the
// system-default (account 0) binding. It degrades to nil on any resolver error
// so a materialize failure never blocks a spawn.
func (r *SpawnEnvResolver) Resolve(ctx context.Context, sess *models.Session) map[string]string {
	if r == nil || r.resolver == nil || sess == nil {
		return nil
	}
	env, err := r.resolver.ResolveSpawnEnv(ctx, deref(sess.AccountID), sess.AgentName, time.Now())
	if err != nil {
		r.log.Warn().Err(err).Str("agent", sess.AgentName).
			Msg("account: resolve spawn env failed; using system default")
		return nil
	}
	return env
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
