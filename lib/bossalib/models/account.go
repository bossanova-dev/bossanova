package models

import (
	"time"

	"github.com/recurser/bossalib/agenterr"
)

// AgentAccountProvider identifies which coding-agent CLI an account authenticates.
type AgentAccountProvider string

const (
	AgentAccountProviderClaude AgentAccountProvider = "claude"
	AgentAccountProviderCodex  AgentAccountProvider = "codex"
)

// AgentAccountStatus is the enabled/disabled state of an account.
type AgentAccountStatus string

const (
	AgentAccountStatusActive   AgentAccountStatus = "active"
	AgentAccountStatusDisabled AgentAccountStatus = "disabled"
)

// AgentAccountHealth is the last-known health of an account.
type AgentAccountHealth string

const (
	AgentAccountHealthOK     AgentAccountHealth = "ok"
	AgentAccountHealthFailed AgentAccountHealth = "failed"
)

// AuthCheckOutcome classifies the result of a daemon-owned credential
// verification (BOS-1141). It is a closed set of redacted classification
// tokens: it never carries provider text, log output, or credential material.
type AuthCheckOutcome string

const (
	// AuthCheckOutcomeUnknown is the zero value: no verification has been
	// recorded for this account yet.
	AuthCheckOutcomeUnknown AuthCheckOutcome = ""
	// AuthCheckOutcomeHealthy means the last verification completed cleanly.
	AuthCheckOutcomeHealthy AuthCheckOutcome = "healthy"
	// AuthCheckOutcomeAuthInvalid means the provider confirmed the stored
	// credential is no longer usable and re-authentication is required. This
	// is the only outcome that removes an account from selection.
	AuthCheckOutcomeAuthInvalid AuthCheckOutcome = "auth_invalid"
	// AuthCheckOutcomeTransient means verification failed for a reason that
	// is expected to clear (provider 5xx, throttling, timeout, or an
	// unrecognized failure). Eligibility is preserved.
	AuthCheckOutcomeTransient AuthCheckOutcome = "transient"
	// AuthCheckOutcomeUnavailable means verification could not be performed
	// at all (no agent runner loaded, verification disabled). It says nothing
	// about the credential, so eligibility is preserved.
	AuthCheckOutcomeUnavailable AuthCheckOutcome = "unavailable"
	// AuthCheckOutcomeRefreshChainUnproven means the verification itself ran
	// cleanly — the provider answered — but on a credential whose own access
	// token says a token refresh should already have happened, and the run
	// observed no credential write. That is the shape of a live access token
	// sitting on top of a DEAD refresh chain (BOS-1174): the check keeps
	// passing until the access token expires, then hard-fails with no warning.
	//
	// It DOES NOT remove the account from selection, and IsAuthInvalid stays
	// false for it. This is an unproven liveness signal, not a confirmed
	// provider rejection: the credential demonstrably still works, so
	// condemning on it would bench an account that is doing its job. Report
	// before condemning — escalating a repeated observation into a verdict
	// needs persisted streak state this record does not carry.
	AuthCheckOutcomeRefreshChainUnproven AuthCheckOutcome = "refresh_chain_unproven"
)

// AuthCheck is the durable, redacted record of the last credential
// verification for one account. Every field is metadata: a timestamp, a
// closed-set outcome, a closed-set failure-class token, and the instant the
// account next becomes eligible for another check. Nothing here is derived
// from credential bytes.
type AuthCheck struct {
	// CheckedAt is when the verification completed (nil = never checked).
	CheckedAt *time.Time
	// Outcome is the classification of that verification.
	Outcome AuthCheckOutcome
	// FailureClass is a short redacted token naming why a non-healthy
	// verification failed (for example "auth_invalidated", "rate_limited",
	// "transient_provider", "runner_unavailable"). Empty when healthy.
	FailureClass string
	// NextRetryAt is the earliest instant the scheduler should verify this
	// account again (nil = follow the ordinary periodic cadence).
	NextRetryAt *time.Time
}

// IsAuthInvalid reports whether durable state says the credential itself was
// rejected. Callers use it to bench an account before it reaches worktree or
// credential materialization.
func (c AuthCheck) IsAuthInvalid() bool { return c.Outcome == AuthCheckOutcomeAuthInvalid }

// IsAuthInvalid reports whether durable verification state benches this
// account. It is nil-safe so it can be dropped into selection loops that
// already tolerate a nil element.
//
// It exists so eligibility predicates test the account rather than reaching
// through to AuthCheck: recording an auth-invalid verdict deliberately leaves
// Health untouched, so any predicate that checks only status and health will
// happily select a credential that is known to be rejected.
func (a *AgentAccount) IsAuthInvalid() bool { return a != nil && a.AuthCheck.IsAuthInvalid() }

// IsSuspended reports whether the provider refused this account outright, for
// example because the organization disabled subscription access. The refusal is
// recorded as failed health plus the refusal text in LastTestError.
func (a *AgentAccount) IsSuspended() bool {
	if a == nil || a.Health != AgentAccountHealthFailed {
		return false
	}
	_, suspended := agenterr.SuspensionReason(a.LastTestError)
	return suspended
}

// IsOutOfService reports whether no launch may run on this account: the
// operator disabled it, or the provider suspended it. A binding to such an
// account is stale and must be replaced, never honoured.
func (a *AgentAccount) IsOutOfService() bool {
	return a != nil && (a.Status == AgentAccountStatusDisabled || a.IsSuspended())
}

// UsageSnapshot is a cached rate-limit usage reading for one account.
// It carries metadata only: utilization fractions, reset instants, status,
// plan tier, and fetch time. It never contains a token or credential.
type UsageSnapshot struct {
	Util5h    float64
	Util7d    float64
	Reset5h   *time.Time // nullable
	Reset7d   *time.Time // nullable
	Status    string
	PlanTier  string
	FetchedAt *time.Time // nil = never probed
}

// Account is registry metadata for one provider login used by account rotation.
// Credential blobs are NEVER stored here — they live in the OS keyring via
// services/bossd/internal/adapter/accountcred, keyed by this Account.ID. SQLite holds
// metadata only (locked decision D3).
//
// D9 (system-default account 0 is NOT a row): the machine's existing
// ~/.claude / ~/.codex login is an implicit account 0 — never imported, never a
// row here. Absence of an account binding means "use the machine login, inject
// nothing". This package therefore adds no default row and no import path.
type AgentAccount struct {
	ID            string
	Provider      AgentAccountProvider
	Label         string
	Status        AgentAccountStatus
	Priority      int // sort order; lower = preferred
	Health        AgentAccountHealth
	CooldownUntil *time.Time // nullable
	LastUsedAt    *time.Time // nullable
	Tier          string     // plan/tier metadata, free-form (e.g. "max", "pro")
	AllowedModels []string   // model-affinity metadata; empty = unspecified
	// Test bookkeeping (BOS-160). LastTestOkAt is the time the last credential
	// test passed (nil = never passed). LastTestError holds the most recent
	// test failure detail ("" = no error / last test passed).
	LastTestOkAt  *time.Time // nullable
	LastTestError string
	Usage         *UsageSnapshot // nil = never probed; metadata only, never a credential
	// AuthCheck is the daemon-owned credential-verification record (BOS-1141).
	// It is deliberately distinct from LastTestOkAt/LastTestError (manual
	// TestAgentAccount bookkeeping) and from Usage, so a scheduled verification can
	// never overwrite either. Redacted metadata only.
	AuthCheck AuthCheck
	CreatedAt time.Time
	UpdatedAt time.Time
}
