package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestSeedAccountMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	database := dbtest.New(t)
	store := db.NewAgentAccountStore(database)
	at := time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)
	for _, outcome := range []string{"healthy", "auth_invalid"} {
		account, err := store.Create(ctx, db.CreateAgentAccountParams{Provider: domain.AgentAccountProviderClaude, Label: outcome, Priority: 3, AllowedModels: []string{"sonnet"}})
		require.NoError(t, err)
		failure := ""
		if outcome == "auth_invalid" {
			failure = "credential_superseded"
		}
		want := AgentAccountSpec{ID: account.ID, Health: "failed", Tier: "max", CooldownUntil: &at, LastUsedAt: &at, LastTestOKAt: &at, LastTestError: "fixture error",
			Usage:     &UsageSpec{Util5h: .23, Util7d: .45, Reset5h: &at, Reset7d: &at, Status: "ok", PlanTier: "max", FetchedAt: &at},
			AuthCheck: &AuthCheckSpec{CheckedAt: &at, Outcome: outcome, FailureClass: failure, NextRetryAt: &at}}
		encoded, err := json.Marshal(Spec{Version: 1, Accounts: []AgentAccountSpec{want}})
		require.NoError(t, err)
		var input Spec
		require.NoError(t, json.Unmarshal(encoded, &input))
		result, err := Seed(ctx, database, input)
		require.NoError(t, err)
		require.Empty(t, result.SessionIDs)
		got, err := store.Get(ctx, account.ID)
		require.NoError(t, err)
		require.Equal(t, domain.AgentAccountHealthFailed, got.Health)
		require.Equal(t, want.Tier, got.Tier)
		require.Equal(t, want.CooldownUntil, got.CooldownUntil)
		require.Equal(t, want.LastUsedAt, got.LastUsedAt)
		require.Equal(t, want.LastTestOKAt, got.LastTestOkAt)
		require.Equal(t, want.LastTestError, got.LastTestError)
		require.Equal(t, &domain.UsageSnapshot{Util5h: .23, Util7d: .45, Reset5h: &at, Reset7d: &at, Status: "ok", PlanTier: "max", FetchedAt: &at}, got.Usage)
		require.Equal(t, domain.AuthCheck{CheckedAt: &at, Outcome: domain.AuthCheckOutcome(outcome), FailureClass: failure, NextRetryAt: &at}, got.AuthCheck)
		_, err = Seed(ctx, database, Spec{Version: 1, Accounts: []AgentAccountSpec{{ID: account.ID}}})
		require.NoError(t, err)
		cleared, err := store.Get(ctx, account.ID)
		require.NoError(t, err)
		require.Nil(t, cleared.CooldownUntil)
		require.Nil(t, cleared.LastUsedAt)
		require.Nil(t, cleared.LastTestOkAt)
		require.Empty(t, cleared.LastTestError)
		require.Empty(t, cleared.Tier)
		require.Equal(t, got.Health, cleared.Health)
		require.Equal(t, got.Usage, cleared.Usage)
		require.Equal(t, got.AuthCheck, cleared.AuthCheck)
		require.Equal(t, account.Label, cleared.Label)
		require.Equal(t, account.Provider, cleared.Provider)
		require.Equal(t, account.Status, cleared.Status)
		require.Equal(t, account.Priority, cleared.Priority)
		require.Equal(t, account.AllowedModels, cleared.AllowedModels)
	}
}

func TestSeedRejectsUnknownAccountBeforeWriting(t *testing.T) {
	ctx := context.Background()
	database := dbtest.New(t)
	store := db.NewAgentAccountStore(database)
	account, err := store.Create(ctx, db.CreateAgentAccountParams{Provider: domain.AgentAccountProviderClaude, Label: "unchanged"})
	require.NoError(t, err)
	_, err = Seed(ctx, database, Spec{Version: 1, Accounts: []AgentAccountSpec{{ID: account.ID, Tier: "changed"}, {ID: "missing"}}})
	require.Error(t, err)
	got, err := store.Get(ctx, account.ID)
	require.NoError(t, err)
	require.Empty(t, got.Tier)
}

func TestSeedAccountNullableObservations(t *testing.T) {
	ctx := context.Background()
	database := dbtest.New(t)
	store := db.NewAgentAccountStore(database)
	account, err := store.Create(ctx, db.CreateAgentAccountParams{Provider: domain.AgentAccountProviderCodex, Label: "nullable"})
	require.NoError(t, err)
	_, err = Seed(ctx, database, Spec{Version: 1, Accounts: []AgentAccountSpec{{ID: account.ID}}})
	require.NoError(t, err)
	got, err := store.Get(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, got.Usage)
	require.Equal(t, domain.AuthCheck{}, got.AuthCheck)
	_, err = Seed(ctx, database, Spec{Version: 1, Accounts: []AgentAccountSpec{{ID: account.ID, Usage: &UsageSpec{}, AuthCheck: &AuthCheckSpec{Outcome: "healthy"}}}})
	require.NoError(t, err)
	got, err = store.Get(ctx, account.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Usage)
	require.NotNil(t, got.Usage.FetchedAt)
	require.Nil(t, got.Usage.Reset5h)
	require.Nil(t, got.Usage.Reset7d)
	require.NotNil(t, got.AuthCheck.CheckedAt)
	require.Nil(t, got.AuthCheck.NextRetryAt)
	require.Equal(t, domain.AuthCheckOutcomeHealthy, got.AuthCheck.Outcome)
}
