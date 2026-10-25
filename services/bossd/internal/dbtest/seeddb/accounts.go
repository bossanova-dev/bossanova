package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

// AgentAccountSpec supplies display metadata unavailable through account RPCs. The
// account must already exist; credentials and identity are created through the API.
type AgentAccountSpec struct {
	ID            string         `json:"id"`
	Health        string         `json:"health"`
	Tier          string         `json:"tier"`
	CooldownUntil *time.Time     `json:"cooldown_until,omitempty"`
	LastUsedAt    *time.Time     `json:"last_used_at,omitempty"`
	LastTestOKAt  *time.Time     `json:"last_test_ok_at,omitempty"`
	LastTestError string         `json:"last_test_error"`
	Usage         *UsageSpec     `json:"usage,omitempty"`
	AuthCheck     *AuthCheckSpec `json:"auth_check,omitempty"`
}

// UsageSpec is one cached, credential-free usage observation.
type UsageSpec struct {
	Util5h    float64    `json:"util_5h"`
	Util7d    float64    `json:"util_7d"`
	Reset5h   *time.Time `json:"reset_5h,omitempty"`
	Reset7d   *time.Time `json:"reset_7d,omitempty"`
	Status    string     `json:"status"`
	PlanTier  string     `json:"plan_tier"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
}

// AuthCheckSpec is one redacted credential-verification observation.
type AuthCheckSpec struct {
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	Outcome      string     `json:"outcome"`
	FailureClass string     `json:"failure_class"`
	NextRetryAt  *time.Time `json:"next_retry_at,omitempty"`
}

func validateSeedAccounts(ctx context.Context, database *sql.DB, accounts []AgentAccountSpec) error {
	store := db.NewAgentAccountStore(database)
	for _, account := range accounts {
		if _, err := store.Get(ctx, account.ID); err != nil {
			return fmt.Errorf("seed account %q: %w", account.ID, err)
		}
		if account.Health != "" && account.Health != string(domain.AgentAccountHealthOK) && account.Health != string(domain.AgentAccountHealthFailed) {
			return fmt.Errorf("seed account %q: unknown health %q", account.ID, account.Health)
		}
	}
	return nil
}

func seedAccounts(ctx context.Context, database *sql.DB, accounts []AgentAccountSpec) error {
	for _, account := range accounts {
		if err := seedAccount(ctx, database, account); err != nil {
			return fmt.Errorf("seed account %q: %w", account.ID, err)
		}
	}
	return nil
}

func seedAccount(ctx context.Context, database *sql.DB, account AgentAccountSpec) error {
	store := db.NewAgentAccountStore(database)
	update := db.UpdateAgentAccountParams{Tier: &account.Tier, CooldownUntil: &account.CooldownUntil, LastUsedAt: &account.LastUsedAt}
	if account.Health != "" {
		health := domain.AgentAccountHealth(account.Health)
		update.Health = &health
	}
	if _, err := store.Update(ctx, account.ID, update); err != nil {
		return err
	}
	if err := store.RecordTestResult(ctx, account.ID, account.LastTestOKAt, account.LastTestError); err != nil {
		return err
	}
	if usage := account.Usage; usage != nil {
		if err := store.RecordUsageProbe(ctx, account.ID, domain.UsageSnapshot{
			Util5h: usage.Util5h, Util7d: usage.Util7d, Reset5h: usage.Reset5h, Reset7d: usage.Reset7d,
			Status: usage.Status, PlanTier: usage.PlanTier, FetchedAt: usage.FetchedAt,
		}); err != nil {
			return err
		}
	}
	if check := account.AuthCheck; check != nil {
		if err := store.RecordAuthCheck(ctx, account.ID, domain.AuthCheck{
			CheckedAt: check.CheckedAt, Outcome: domain.AuthCheckOutcome(check.Outcome), FailureClass: check.FailureClass, NextRetryAt: check.NextRetryAt,
		}); err != nil {
			return err
		}
	}
	return nil
}
