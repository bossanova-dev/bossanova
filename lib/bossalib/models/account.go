package models

import "time"

// AccountProvider identifies which coding-agent CLI an account authenticates.
type AccountProvider string

const (
	AccountProviderClaude AccountProvider = "claude"
	AccountProviderCodex  AccountProvider = "codex"
)

// AccountStatus is the enabled/disabled state of an account.
type AccountStatus string

const (
	AccountStatusActive   AccountStatus = "active"
	AccountStatusDisabled AccountStatus = "disabled"
)

// AccountHealth is the last-known health of an account.
type AccountHealth string

const (
	AccountHealthOK     AccountHealth = "ok"
	AccountHealthFailed AccountHealth = "failed"
)

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
// services/bossd/internal/accountcred, keyed by this Account.ID. SQLite holds
// metadata only (locked decision D3).
//
// D9 (system-default account 0 is NOT a row): the machine's existing
// ~/.claude / ~/.codex login is an implicit account 0 — never imported, never a
// row here. Absence of an account binding means "use the machine login, inject
// nothing". This package therefore adds no default row and no import path.
type Account struct {
	ID            string
	Provider      AccountProvider
	Label         string
	AccountEmail  string
	Status        AccountStatus
	Priority      int // sort order; lower = preferred
	Health        AccountHealth
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
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
