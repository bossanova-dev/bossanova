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
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
