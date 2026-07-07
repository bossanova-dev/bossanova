// Package account resolves which registry account a session should run under
// and produces the per-account spawn environment for that account.
//
// The resolver is deliberately degrade-safe: an empty registry, a missing
// account, or an agent-runner plugin that does not support credential rotation
// all collapse to "account 0" (the system default), meaning no per-account
// environment is injected and the CLI's own login is used. It never panics on
// nil dependencies so it can be constructed before the DB/plugin adapters are
// wired.
package account

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// SystemDefaultAccountID is the "account 0" sentinel: the empty string. A
// session bound to it uses the agent CLI's ambient login with no per-account
// environment injection.
const SystemDefaultAccountID = ""

// AccountMeta is the subset of the account registry this resolver reads.
type AccountMeta struct {
	ID           string
	Provider     string // "claude" | "codex" — matches Session.AgentName
	Label        string
	Status       string     // "active" | "disabled"
	Health       string     // "ok" | "failed" — only "ok" is selectable
	Priority     int        // sort order; lower = preferred (matches models.Account)
	CoolingUntil *time.Time // non-nil & future ⇒ cooling
	LastUsedAt   *time.Time // for stable LRU tie-break
}

// Registry is the account-metadata source the resolver reads. Implementations
// adapt the bossd account store. List returns an empty slice when no registry
// or no accounts exist.
type Registry interface {
	List(ctx context.Context) ([]AccountMeta, error)
	Get(ctx context.Context, id string) (AccountMeta, bool, error)
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
}

// Materializer turns a bound account into concrete spawn environment via the
// agent-runner plugin. SupportsRotation reports whether the provider's plugin
// can materialize credentials at all; when false the resolver degrades to
// status-only binding (no env injected).
type Materializer interface {
	SupportsRotation(ctx context.Context, provider string) (bool, error)
	MaterializeAccount(ctx context.Context, accountID string) (map[string]string, error)
}

// Resolver is the degrade-safe "brain" consumed by spawn paths and account
// pickers. It holds no state beyond its dependencies.
type Resolver struct {
	reg Registry
	mat Materializer
	log zerolog.Logger
}

// NewResolver builds a Resolver. reg and mat may be nil; the resolver tolerates
// nil dependencies and degrades to account 0 in that case.
func NewResolver(reg Registry, mat Materializer, log zerolog.Logger) *Resolver {
	return &Resolver{reg: reg, mat: mat, log: log}
}

// DefaultAccountID picks the default account for provider at time now. It keeps
// active, healthy, non-cooling accounts of the given provider, then picks the
// most preferred by the same ordering the rotation engine uses: lowest Priority
// (lower = preferred), then least-recently-used (never-used first), then lexical
// ID. Returns "" (system default) when nothing qualifies or the registry is nil.
func (r *Resolver) DefaultAccountID(ctx context.Context, provider string, now time.Time) (string, error) {
	if r == nil || r.reg == nil {
		return SystemDefaultAccountID, nil
	}
	all, err := r.reg.List(ctx)
	if err != nil {
		return "", err
	}

	best := AccountMeta{}
	found := false
	for _, a := range all {
		// Mirror the rotation engine's selectable predicate (engine.go
		// isSelectable): only active, healthy accounts are eligible. A
		// failed-health account must never be picked as a default — its
		// credentials are known-bad and would be materialized again immediately.
		if a.Provider != provider || a.Status != "active" || a.Health != "ok" {
			continue
		}
		if a.CoolingUntil != nil && a.CoolingUntil.After(now) {
			continue
		}
		if !found || moreEligible(a, best) {
			best = a
			found = true
		}
	}
	if !found {
		return SystemDefaultAccountID, nil
	}
	return best.ID, nil
}

// moreEligible reports whether candidate should beat current, mirroring the
// rotation engine's ORDER BY (account_decide_tx.go ListByProvider): lower
// Priority wins, then least-recently-used (a nil LastUsedAt sorts as the zero
// time, so never-used accounts win the tie), then lexically smaller ID.
func moreEligible(candidate, current AccountMeta) bool {
	if candidate.Priority != current.Priority {
		return candidate.Priority < current.Priority
	}
	if !lastUsed(candidate).Equal(lastUsed(current)) {
		return lastUsed(candidate).Before(lastUsed(current))
	}
	return candidate.ID < current.ID
}

func lastUsed(a AccountMeta) time.Time {
	if a.LastUsedAt == nil {
		return time.Time{}
	}
	return *a.LastUsedAt
}

// ResolveSpawnEnv produces the per-account spawn environment for accountID.
//
// Degrade-safe rules:
//   - accountID == "" (account 0): return (nil, nil) immediately, with NO
//     registry or materializer calls.
//   - account not found in the registry: log once and treat as account 0.
//   - provider plugin does not support rotation (or materializer is nil): log
//     the degrade once and return (nil, nil) — status-only binding.
//   - otherwise materialize the account's env, best-effort bump last-used
//     (log-and-continue on error), and return the env.
func (r *Resolver) ResolveSpawnEnv(ctx context.Context, accountID, provider string, now time.Time) (map[string]string, error) {
	if accountID == SystemDefaultAccountID {
		return nil, nil
	}
	if r == nil {
		return nil, nil
	}
	if r.reg == nil {
		r.logDebug().Str("account_id", accountID).Msg("account: no registry; using system default")
		return nil, nil
	}

	acct, ok, err := r.reg.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !ok {
		r.logWarn().Str("account_id", accountID).Msg("account: bound account not found; using system default")
		return nil, nil
	}

	if r.mat == nil {
		r.logDebug().Str("account_id", accountID).Str("provider", provider).
			Msg("account: no materializer; status-only binding")
		return nil, nil
	}
	supports, err := r.mat.SupportsRotation(ctx, provider)
	if err != nil {
		return nil, err
	}
	if !supports {
		r.logDebug().Str("account_id", accountID).Str("provider", provider).
			Msg("account: plugin lacks rotation; status-only binding")
		return nil, nil
	}

	env, err := r.mat.MaterializeAccount(ctx, acct.ID)
	if err != nil {
		return nil, err
	}
	if touchErr := r.reg.TouchLastUsed(ctx, acct.ID, now); touchErr != nil {
		r.logWarn().Err(touchErr).Str("account_id", acct.ID).
			Msg("account: failed to record last-used; continuing")
	}
	return env, nil
}

// Label returns a human-friendly label for accountID. "" ⇒ "Unmanaged local credentials".
// A known account returns its Label. When the registry is unreachable or the
// account is unknown it falls back to a short ID prefix so it is never empty.
func (r *Resolver) Label(ctx context.Context, accountID string) (string, error) {
	if accountID == SystemDefaultAccountID {
		return UnmanagedLocalCredentialsLabel, nil
	}
	if r == nil || r.reg == nil {
		return shortID(accountID), nil
	}
	acct, ok, err := r.reg.Get(ctx, accountID)
	if err != nil || !ok || acct.Label == "" {
		return shortID(accountID), nil
	}
	return acct.Label, nil
}

// shortID returns the first 8 characters of id (or the whole id if shorter).
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (r *Resolver) logDebug() *zerolog.Event { return r.log.Debug() }
func (r *Resolver) logWarn() *zerolog.Event  { return r.log.Warn() }
