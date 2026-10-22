package db

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
)

func TestAccountStore_CreateGetDefaults(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{
		Provider:      models.AgentAccountProviderClaude,
		Label:         "work",
		Priority:      5,
		Tier:          "max",
		AllowedModels: []string{"claude-opus-4-8", "claude-sonnet-5"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if acct.ID == "" {
		t.Error("id should not be empty")
	}
	if acct.Status != models.AgentAccountStatusActive {
		t.Errorf("status = %q, want %q", acct.Status, models.AgentAccountStatusActive)
	}
	if acct.Health != models.AgentAccountHealthOK {
		t.Errorf("health = %q, want %q", acct.Health, models.AgentAccountHealthOK)
	}
	if acct.CreatedAt.IsZero() || acct.UpdatedAt.IsZero() {
		t.Errorf("timestamps should be non-zero: created=%v updated=%v", acct.CreatedAt, acct.UpdatedAt)
	}
	if acct.CooldownUntil != nil || acct.LastUsedAt != nil {
		t.Errorf("cooldown/last-used should be nil on create: %v %v", acct.CooldownUntil, acct.LastUsedAt)
	}

	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Provider != models.AgentAccountProviderClaude {
		t.Errorf("provider = %q, want claude", got.Provider)
	}
	if got.Label != "work" {
		t.Errorf("label = %q, want work", got.Label)
	}
	if got.Priority != 5 || got.Tier != "max" {
		t.Errorf("priority/tier = %d/%q", got.Priority, got.Tier)
	}
	if len(got.AllowedModels) != 2 || got.AllowedModels[0] != "claude-opus-4-8" || got.AllowedModels[1] != "claude-sonnet-5" {
		t.Errorf("allowed_models = %v, want [claude-opus-4-8 claude-sonnet-5]", got.AllowedModels)
	}
}

func TestAccountStore_CreateEmptyAllowedModels(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{
		Provider: models.AgentAccountProviderCodex,
		Label:    "no-models",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.AllowedModels) != 0 {
		t.Errorf("allowed_models = %v, want empty", got.AllowedModels)
	}
}

func TestAccountStore_UpdateFields(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{
		Provider: models.AgentAccountProviderClaude,
		Label:    "upd",
		Priority: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newLabel := "renamed"
	newStatus := models.AgentAccountStatusDisabled
	newPriority := 9
	newHealth := models.AgentAccountHealthFailed
	newTier := "pro"
	newModels := []string{"gpt-5"}
	cooldown := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	lastUsed := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)

	updated, err := store.Update(ctx, acct.ID, UpdateAgentAccountParams{
		Label:         &newLabel,
		Status:        &newStatus,
		Priority:      &newPriority,
		Health:        &newHealth,
		Tier:          &newTier,
		AllowedModels: &newModels,
		CooldownUntil: func() **time.Time { p := &cooldown; return &p }(),
		LastUsedAt:    func() **time.Time { p := &lastUsed; return &p }(),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Label != newLabel {
		t.Errorf("label = %q, want %q", updated.Label, newLabel)
	}
	if updated.Status != newStatus || updated.Health != newHealth {
		t.Errorf("status/health = %q/%q", updated.Status, updated.Health)
	}
	if updated.Priority != newPriority || updated.Tier != newTier {
		t.Errorf("priority/tier = %d/%q", updated.Priority, updated.Tier)
	}
	if len(updated.AllowedModels) != 1 || updated.AllowedModels[0] != "gpt-5" {
		t.Errorf("allowed_models = %v", updated.AllowedModels)
	}
	if updated.CooldownUntil == nil || !updated.CooldownUntil.Equal(cooldown) {
		t.Errorf("cooldown_until = %v, want %v", updated.CooldownUntil, cooldown)
	}
	if updated.LastUsedAt == nil || !updated.LastUsedAt.Equal(lastUsed) {
		t.Errorf("last_used_at = %v, want %v", updated.LastUsedAt, lastUsed)
	}
	if !updated.UpdatedAt.After(acct.CreatedAt) && !updated.UpdatedAt.Equal(acct.CreatedAt) {
		t.Errorf("updated_at %v should be >= created_at %v", updated.UpdatedAt, acct.CreatedAt)
	}

	// Clear the nullable times back to NULL via the *nil double-pointer path.
	cleared, err := store.Update(ctx, acct.ID, UpdateAgentAccountParams{
		CooldownUntil: func() **time.Time { var p *time.Time; return &p }(),
		LastUsedAt:    func() **time.Time { var p *time.Time; return &p }(),
	})
	if err != nil {
		t.Fatalf("clear update: %v", err)
	}
	if cleared.CooldownUntil != nil {
		t.Errorf("cooldown_until = %v, want nil after clear", cleared.CooldownUntil)
	}
	if cleared.LastUsedAt != nil {
		t.Errorf("last_used_at = %v, want nil after clear", cleared.LastUsedAt)
	}
}

func TestAccountStore_UpdateUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	label := "x"
	if _, err := store.Update(ctx, "does-not-exist", UpdateAgentAccountParams{Label: &label}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("update unknown: got %v, want domain.ErrNotFound", err)
	}
}

func TestAccountStore_CooldownRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{
		Provider: models.AgentAccountProviderClaude,
		Label:    "cooldown",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	cases := []struct {
		name string
		val  *time.Time
	}{
		{"past", ptrTime(time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond))},
		{"future", ptrTime(time.Now().Add(24 * time.Hour).UTC().Truncate(time.Millisecond))},
		{"null", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.Update(ctx, acct.ID, UpdateAgentAccountParams{
				CooldownUntil: &tc.val,
			})
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if tc.val == nil {
				if got.CooldownUntil != nil {
					t.Errorf("cooldown_until = %v, want nil", got.CooldownUntil)
				}
				return
			}
			if got.CooldownUntil == nil || !got.CooldownUntil.Equal(*tc.val) {
				t.Errorf("cooldown_until = %v, want %v (store must not interpret expiry)", got.CooldownUntil, *tc.val)
			}
		})
	}
}

func TestAccountStore_ListOrderingAndProviderFilter(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	// Deliberately create out of priority order and across providers.
	claudeB, _ := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "claude-b", Priority: 10})
	claudeA, _ := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "claude-a", Priority: 1})
	codexA, _ := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "codex-a", Priority: 3})

	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list len = %d, want 3", len(all))
	}
	// provider ASC ('claude' < 'codex'), then priority ASC: claudeA(1), claudeB(10), codexA(3).
	wantOrder := []string{claudeA.ID, claudeB.ID, codexA.ID}
	for i, id := range wantOrder {
		if all[i].ID != id {
			t.Errorf("list[%d].ID = %q, want %q (order: provider,priority,created_at)", i, all[i].ID, id)
		}
	}

	claudeOnly, err := store.ListByProvider(ctx, models.AgentAccountProviderClaude)
	if err != nil {
		t.Fatalf("list by provider: %v", err)
	}
	if len(claudeOnly) != 2 {
		t.Fatalf("claude len = %d, want 2", len(claudeOnly))
	}
	for _, a := range claudeOnly {
		if a.Provider != models.AgentAccountProviderClaude {
			t.Errorf("provider filter leaked %q", a.Provider)
		}
	}
	if claudeOnly[0].ID != claudeA.ID || claudeOnly[1].ID != claudeB.ID {
		t.Errorf("claude order = [%q %q], want [%q %q]", claudeOnly[0].ID, claudeOnly[1].ID, claudeA.ID, claudeB.ID)
	}

	codexOnly, _ := store.ListByProvider(ctx, models.AgentAccountProviderCodex)
	if len(codexOnly) != 1 || codexOnly[0].ID != codexA.ID {
		t.Errorf("codex filter = %v, want [%q]", codexOnly, codexA.ID)
	}
}

func TestAccountStore_UniqueProviderLabel(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "dup"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "dup"})
	if err == nil {
		t.Error("expected UNIQUE(provider,label) violation for duplicate claude/dup")
	} else if !errors.Is(err, ErrAgentAccountExists) {
		t.Errorf("duplicate create should wrap ErrAgentAccountExists, got: %v", err)
	}
	// Same label under a different provider is allowed.
	if _, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "dup"}); err != nil {
		t.Errorf("same label under other provider should succeed: %v", err)
	}
}

func TestAccountStore_Delete(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "del"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Delete(ctx, acct.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, acct.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get after delete: got %v, want domain.ErrNotFound", err)
	}
	if err := store.Delete(ctx, acct.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second delete: got %v, want domain.ErrNotFound", err)
	}
}

// TestAccountsMigrationIdempotent asserts the accounts table exists after
// setupTestDB and that re-running the migration set is a no-op (goose version
// tracking), after which the table is still writable.
func TestAccountsMigrationIdempotent(t *testing.T) {
	// A real replay, not the shared fixture: idempotency of the migrations is
	// this test's subject, so it must not start from their captured output.
	db := dbtest.NewMigrated(t)
	ctx := context.Background()

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_accounts").Scan(&count); err != nil {
		t.Fatalf("agent_accounts table should exist: %v", err)
	}

	// Re-run migrations; goose version tracking makes this a no-op.
	if err := dbtest.Run(db, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("second migration run should be a no-op: %v", err)
	}

	store := NewAgentAccountStore(db)
	if _, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "post-migrate"}); err != nil {
		t.Fatalf("insert after second migration: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_accounts").Scan(&count); err != nil {
		t.Fatalf("select after second migration: %v", err)
	}
	if count != 1 {
		t.Errorf("agent_accounts count = %d, want 1", count)
	}
}

// TestAccountStore_TestResultColumnsDefault asserts the BOS-160 columns default
// to "never tested" on create: last_test_ok_at NULL, last_test_error "".
func TestAccountStore_TestResultColumnsDefault(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "fresh"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if acct.LastTestOkAt != nil {
		t.Errorf("last_test_ok_at = %v, want nil on create", acct.LastTestOkAt)
	}
	if acct.LastTestError != "" {
		t.Errorf("last_test_error = %q, want empty on create", acct.LastTestError)
	}
}

func TestAccountStore_RecordTestResult(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "rec"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	okAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)

	cases := []struct {
		name       string
		okAt       *time.Time
		testErr    string
		wantOK     bool // whether last_test_ok_at should be set afterward
		wantErrStr string
	}{
		{"pass", ptrTime(okAt), "", true, ""},
		{"fail_clears_ok", nil, "smoke failed: boom", false, "smoke failed: boom"},
		{"pass_clears_err", ptrTime(okAt), "", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.RecordTestResult(ctx, acct.ID, tc.okAt, tc.testErr); err != nil {
				t.Fatalf("record: %v", err)
			}
			got, err := store.Get(ctx, acct.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if tc.wantOK {
				if got.LastTestOkAt == nil || !got.LastTestOkAt.Equal(okAt) {
					t.Errorf("last_test_ok_at = %v, want %v", got.LastTestOkAt, okAt)
				}
			} else if got.LastTestOkAt != nil {
				t.Errorf("last_test_ok_at = %v, want nil", got.LastTestOkAt)
			}
			if got.LastTestError != tc.wantErrStr {
				t.Errorf("last_test_error = %q, want %q", got.LastTestError, tc.wantErrStr)
			}
		})
	}
}

func TestAccountStore_MarkAccountSuspended(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "susp"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed a passing test so we can prove suspension clears last_test_ok_at.
	okAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	if err := store.RecordTestResult(ctx, acct.ID, ptrTime(okAt), ""); err != nil {
		t.Fatalf("seed test result: %v", err)
	}

	const reason = "account suspended: organization disabled Claude subscription access"
	if err := store.MarkSuspended(ctx, acct.ID, reason); err != nil {
		t.Fatalf("mark suspended: %v", err)
	}

	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Health != models.AgentAccountHealthFailed {
		t.Errorf("health = %q, want %q", got.Health, models.AgentAccountHealthFailed)
	}
	if got.LastTestError != reason {
		t.Errorf("last_test_error = %q, want %q", got.LastTestError, reason)
	}
	if got.LastTestOkAt != nil {
		t.Errorf("last_test_ok_at = %v, want nil (cleared)", got.LastTestOkAt)
	}
	// Status is left untouched — an operator re-enables via a separate action.
	if got.Status != models.AgentAccountStatusActive {
		t.Errorf("status = %q, want unchanged active", got.Status)
	}
}

func TestAccountStore_MarkAccountSuspendedUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	if err := store.MarkSuspended(ctx, "nope", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("mark unknown: got %v, want domain.ErrNotFound", err)
	}
}

func TestAccountStore_RecordTestResultUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	if err := store.RecordTestResult(ctx, "nope", nil, "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record unknown: got %v, want domain.ErrNotFound", err)
	}
}

func TestAccountStore_RecordUsageProbeRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "usage"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if acct.Usage != nil {
		t.Fatalf("fresh account Usage = %#v, want nil", acct.Usage)
	}
	before := acct.UpdatedAt
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	fetchedAt := time.Now().UTC().Truncate(time.Millisecond)
	snap := models.UsageSnapshot{
		Util5h:    0.42,
		Util7d:    0.73,
		Reset5h:   &reset5h,
		Reset7d:   &reset7d,
		Status:    "warning",
		PlanTier:  "max",
		FetchedAt: &fetchedAt,
	}

	if err := store.RecordUsageProbe(ctx, acct.ID, snap); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Usage == nil {
		t.Fatal("Usage = nil, want populated snapshot")
	}
	if got.Usage.Util5h != snap.Util5h || got.Usage.Util7d != snap.Util7d {
		t.Errorf("utils = %v/%v, want %v/%v", got.Usage.Util5h, got.Usage.Util7d, snap.Util5h, snap.Util7d)
	}
	if got.Usage.Reset5h == nil || !got.Usage.Reset5h.Equal(reset5h) {
		t.Errorf("reset_5h = %v, want %v", got.Usage.Reset5h, reset5h)
	}
	if got.Usage.Reset7d == nil || !got.Usage.Reset7d.Equal(reset7d) {
		t.Errorf("reset_7d = %v, want %v", got.Usage.Reset7d, reset7d)
	}
	if got.Usage.Status != snap.Status || got.Usage.PlanTier != snap.PlanTier {
		t.Errorf("status/plan = %q/%q, want %q/%q", got.Usage.Status, got.Usage.PlanTier, snap.Status, snap.PlanTier)
	}
	if got.Usage.FetchedAt == nil || !got.Usage.FetchedAt.Equal(fetchedAt) {
		t.Errorf("fetched_at = %v, want %v", got.Usage.FetchedAt, fetchedAt)
	}
	if got.UpdatedAt.Before(before) {
		t.Errorf("updated_at = %v, want >= %v", got.UpdatedAt, before)
	}

	empty, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "never-probed"})
	if err != nil {
		t.Fatalf("create never-probed: %v", err)
	}
	empty, err = store.Get(ctx, empty.ID)
	if err != nil {
		t.Fatalf("get never-probed: %v", err)
	}
	if empty.Usage != nil {
		t.Errorf("never-probed Usage = %#v, want nil", empty.Usage)
	}
}

func TestAccountStore_RecordUsageProbeUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	if err := store.RecordUsageProbe(ctx, "nope", models.UsageSnapshot{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record usage unknown: got %v, want domain.ErrNotFound", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// --- BOS-973: credential-injection failure health ---------------------------

func TestAccountStore_RecordInjectionFailure(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "inject"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed a passing test so we can prove the injection failure clears it.
	okAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	if err := store.RecordTestResult(ctx, acct.ID, ptrTime(okAt), ""); err != nil {
		t.Fatalf("seed test result: %v", err)
	}

	const reason = "materialize codex account: project codex base home: boom"
	if err := store.RecordInjectionFailure(ctx, acct.ID, reason); err != nil {
		t.Fatalf("record injection failure: %v", err)
	}

	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Health != models.AgentAccountHealthFailed {
		t.Errorf("health = %q, want %q", got.Health, models.AgentAccountHealthFailed)
	}
	want := InjectionFailureReasonPrefix + reason
	if got.LastTestError != want {
		t.Errorf("last_test_error = %q, want %q", got.LastTestError, want)
	}
	// last_test_ok_at is deliberately PRESERVED. An injection failure is a
	// local materialization failure, not a credential failure, so the
	// last-verified-good timestamp is still true — and clearing it here would
	// be the one part of the record MarkInjectionFailureCleared cannot withdraw,
	// leaving a self-healed account rendering as never tested.
	if got.LastTestOkAt == nil {
		t.Fatal("last_test_ok_at was cleared; an injection failure must leave the last verified-good test intact")
	}
	if !got.LastTestOkAt.Equal(okAt) {
		t.Errorf("last_test_ok_at = %v, want %v (unchanged)", got.LastTestOkAt, okAt)
	}

	// …and the withdrawal leaves the row exactly as the failure found it.
	if err := store.MarkInjectionFailureCleared(ctx, acct.ID); err != nil {
		t.Fatalf("clear injection failure: %v", err)
	}
	cleared, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if cleared.Health != models.AgentAccountHealthOK || cleared.LastTestError != "" {
		t.Errorf("after clear: health=%q last_test_error=%q, want ok and empty", cleared.Health, cleared.LastTestError)
	}
	if cleared.LastTestOkAt == nil || !cleared.LastTestOkAt.Equal(okAt) {
		t.Errorf("after clear: last_test_ok_at = %v, want %v — a self-healed account must not render as never tested",
			cleared.LastTestOkAt, okAt)
	}
}

// MarkSuspended keeps clearing last_test_ok_at: a suspension means the
// credential itself can no longer serve requests, so the last-verified-good
// timestamp is a stale claim. Pinned here because RecordInjectionFailure is the
// near-identical write that must NOT clear it — the two are one edit away from
// each other.
func TestAccountStore_MarkAccountSuspendedStillClearsLastTestOkAt(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "suspend"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	okAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	if err := store.RecordTestResult(ctx, acct.ID, ptrTime(okAt), ""); err != nil {
		t.Fatalf("seed test result: %v", err)
	}
	if err := store.MarkSuspended(ctx, acct.ID, "billing suspended"); err != nil {
		t.Fatalf("mark suspended: %v", err)
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Health != models.AgentAccountHealthFailed {
		t.Errorf("health = %q, want %q", got.Health, models.AgentAccountHealthFailed)
	}
	if got.LastTestOkAt != nil {
		t.Errorf("last_test_ok_at = %v, want nil (cleared by a suspension)", got.LastTestOkAt)
	}
}

func TestAccountStore_MarkInjectionFailureClearedRestoresHealth(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "inject"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.RecordInjectionFailure(ctx, acct.ID, "transient"); err != nil {
		t.Fatalf("record injection failure: %v", err)
	}
	if err := store.MarkInjectionFailureCleared(ctx, acct.ID); err != nil {
		t.Fatalf("clear injection failure: %v", err)
	}

	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Health != models.AgentAccountHealthOK {
		t.Errorf("health = %q, want %q", got.Health, models.AgentAccountHealthOK)
	}
	if got.LastTestError != "" {
		t.Errorf("last_test_error = %q, want empty", got.LastTestError)
	}
}

// TestAccountStore_MarkInjectionFailureClearedLeavesGenuineTestFailure is the whole
// point of the prefix guard: a successful materialize must never erase a real
// `boss account test` failure an operator still needs to see.
// A healthy row routinely carries a STALE last_test_error: restoreAccountHealth
// sets health=ok without clearing the reason. If RecordInjectionFailure preserved
// that string, the row would end up failed with a reason MarkInjectionFailureCleared can
// never match (it is prefix-scoped), so every later successful injection would be a
// no-op and the account would stay out of rotation permanently — exactly the
// stuck-account class this change exists to remove. A reason on a HEALTHY row is
// history, not protection, so it must be replaced.
func TestAccountStore_RecordInjectionFailureReplacesStaleReasonOnHealthyRow(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "inject"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A real failure, then a refresh that restores health but leaves the reason.
	const staleErr = "401 invalid_grant: refresh token rejected"
	if err := store.RecordTestResult(ctx, acct.ID, nil, staleErr); err != nil {
		t.Fatalf("record test result: %v", err)
	}
	healthy := models.AgentAccountHealthOK
	if _, err := store.Update(ctx, acct.ID, UpdateAgentAccountParams{Health: &healthy}); err != nil {
		t.Fatalf("restore health: %v", err)
	}

	if err := store.RecordInjectionFailure(ctx, acct.ID, "project codex base home: boom"); err != nil {
		t.Fatalf("record injection failure: %v", err)
	}

	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Health != models.AgentAccountHealthFailed {
		t.Errorf("health = %q, want %q", got.Health, models.AgentAccountHealthFailed)
	}
	if !strings.HasPrefix(got.LastTestError, InjectionFailureReasonPrefix) {
		t.Fatalf("last_test_error = %q, want the injection prefix (a stale reason on a healthy row is history, not protection)", got.LastTestError)
	}

	// The whole point: the record must remain self-clearing.
	if err := store.MarkInjectionFailureCleared(ctx, acct.ID); err != nil {
		t.Fatalf("clear injection failure: %v", err)
	}
	cleared, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if cleared.Health != models.AgentAccountHealthOK {
		t.Errorf("health after clear = %q, want %q (the account would otherwise be stuck out of rotation)", cleared.Health, models.AgentAccountHealthOK)
	}
	if cleared.LastTestError != "" {
		t.Errorf("last_test_error after clear = %q, want empty", cleared.LastTestError)
	}
}

func TestAccountStore_MarkInjectionFailureClearedLeavesGenuineTestFailure(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "inject"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const testErr = "401 invalid_grant: refresh token rejected"
	if err := store.RecordTestResult(ctx, acct.ID, nil, testErr); err != nil {
		t.Fatalf("record test result: %v", err)
	}
	if err := store.MarkSuspended(ctx, acct.ID, testErr); err != nil {
		t.Fatalf("fail health: %v", err)
	}

	if err := store.MarkInjectionFailureCleared(ctx, acct.ID); err != nil {
		t.Fatalf("clear injection failure: %v", err)
	}

	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Health != models.AgentAccountHealthFailed {
		t.Errorf("health = %q, want %q (a genuine test failure must survive)", got.Health, models.AgentAccountHealthFailed)
	}
	if got.LastTestError != testErr {
		t.Errorf("last_test_error = %q, want %q (unchanged)", got.LastTestError, testErr)
	}
}

// A clear against a healthy row matches nothing and must be a silent no-op —
// it runs on EVERY successful spawn, so domain.ErrNotFound there would be noise.
func TestAccountStore_MarkInjectionFailureClearedNoopOnHealthyRow(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "healthy"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.MarkInjectionFailureCleared(ctx, acct.ID); err != nil {
		t.Fatalf("clear on healthy row: %v", err)
	}
	if err := store.MarkInjectionFailureCleared(ctx, "nope"); err != nil {
		t.Fatalf("clear on unknown row: %v", err)
	}
}

func TestAccountStore_RecordInjectionFailureUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	if err := store.RecordInjectionFailure(ctx, "nope", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record unknown: got %v, want domain.ErrNotFound", err)
	}
}

// TestAccountStore_RecordInjectionFailurePreservesForeignReason closes the
// round-2 hole: an injection failure recorded ON TOP of a protected failure
// must not convert that failure into a self-clearing one. Materialization is a
// local filesystem operation that never re-validates the credential, so a later
// successful materialize is not evidence a live credential failure is resolved
// — and if the record had overwritten the reason, MarkInjectionFailureCleared would
// then have matched and returned a known-bad account to rotation as healthy.
func TestAccountStore_RecordInjectionFailurePreservesForeignReason(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "clobber"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const genuine = "401 invalid_grant: refresh token rejected"
	if err := store.RecordTestResult(ctx, acct.ID, nil, genuine); err != nil {
		t.Fatalf("record test result: %v", err)
	}
	if err := store.MarkSuspended(ctx, acct.ID, genuine); err != nil {
		t.Fatalf("mark suspended: %v", err)
	}

	// The plan's own Risks section notes DefaultAgentAccountID tier 2 deliberately
	// binds a failed-health active account when it is the only active one, so
	// this ordering is reachable in production, not hypothetical.
	if err := store.RecordInjectionFailure(ctx, acct.ID, "project codex base home: boom"); err != nil {
		t.Fatalf("record injection failure: %v", err)
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastTestError != genuine {
		t.Fatalf("last_test_error = %q, want the genuine failure %q preserved", got.LastTestError, genuine)
	}
	if got.Health != models.AgentAccountHealthFailed {
		t.Errorf("health = %q, want %q", got.Health, models.AgentAccountHealthFailed)
	}

	// …and the withdrawal therefore still matches nothing.
	if err := store.MarkInjectionFailureCleared(ctx, acct.ID); err != nil {
		t.Fatalf("clear injection failure: %v", err)
	}
	after, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if after.Health != models.AgentAccountHealthFailed || after.LastTestError != genuine {
		t.Fatalf("after clear: health=%q last_test_error=%q — a protected failure must survive a successful materialize",
			after.Health, after.LastTestError)
	}
}

// A blank last_test_error is not a foreign reason: an account that is simply
// healthy (or whose reason was emptied by a passing test) must still receive
// the injection reason, or the failure would be recorded with no explanation.
func TestAccountStore_RecordInjectionFailureWritesOverBlankReason(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "blank"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	const reason = "project codex base home: boom"
	if err := store.RecordInjectionFailure(ctx, acct.ID, reason); err != nil {
		t.Fatalf("record injection failure: %v", err)
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if want := InjectionFailureReasonPrefix + reason; got.LastTestError != want {
		t.Fatalf("last_test_error = %q, want %q", got.LastTestError, want)
	}

	// A SECOND injection failure replaces the first: both are this record's own
	// reason, so there is nothing to protect and the newest cause is the useful
	// one.
	const newer = "project codex base home: different entry"
	if err := store.RecordInjectionFailure(ctx, acct.ID, newer); err != nil {
		t.Fatalf("record second injection failure: %v", err)
	}
	got, err = store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if want := InjectionFailureReasonPrefix + newer; got.LastTestError != want {
		t.Fatalf("last_test_error = %q, want the newer reason %q", got.LastTestError, want)
	}
}

// TestIsSelfClearingInjectionFailure pins the predicate against the UPDATE it
// claims to mirror: for each seeded row it asserts the in-memory answer matches
// what MarkInjectionFailureCleared actually does to that row. If the two ever drift,
// a caller that exempts "self-clearing" rows would be exempting rows the clear
// path will never heal.
func TestIsSelfClearingInjectionFailure(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		seed func(t *testing.T, store *SQLiteAgentAccountStore, id string)
		want bool
	}{
		{"healthy", func(*testing.T, *SQLiteAgentAccountStore, string) {}, false},
		{"injection failure", func(t *testing.T, store *SQLiteAgentAccountStore, id string) {
			t.Helper()
			if err := store.RecordInjectionFailure(ctx, id, "keyring unavailable"); err != nil {
				t.Fatalf("record injection failure: %v", err)
			}
		}, true},
		{"suspension", func(t *testing.T, store *SQLiteAgentAccountStore, id string) {
			t.Helper()
			if err := store.MarkSuspended(ctx, id, "billing suspended"); err != nil {
				t.Fatalf("mark suspended: %v", err)
			}
		}, false},
		{"operator test failure", func(t *testing.T, store *SQLiteAgentAccountStore, id string) {
			t.Helper()
			if err := store.RecordTestResult(ctx, id, nil, "401 unauthorized"); err != nil {
				t.Fatalf("record test result: %v", err)
			}
			failed := models.AgentAccountHealthFailed
			if _, err := store.Update(ctx, id, UpdateAgentAccountParams{Health: &failed}); err != nil {
				t.Fatalf("update health: %v", err)
			}
		}, false},
		{"failed with no reason", func(t *testing.T, store *SQLiteAgentAccountStore, id string) {
			t.Helper()
			failed := models.AgentAccountHealthFailed
			if _, err := store.Update(ctx, id, UpdateAgentAccountParams{Health: &failed}); err != nil {
				t.Fatalf("update health: %v", err)
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewAgentAccountStore(setupTestDB(t))
			acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderClaude, Label: "work"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			tc.seed(t, store, acct.ID)

			before := mustGet(t, store, acct.ID)
			if got := IsSelfClearingInjectionFailure(before); got != tc.want {
				t.Fatalf("IsSelfClearingInjectionFailure = %v, want %v (health %q, reason %q)",
					got, tc.want, before.Health, before.LastTestError)
			}

			// The predicate's whole claim is that it answers "would
			// MarkInjectionFailureCleared heal this row?", so verify against the real
			// UPDATE rather than restating the prefix rule.
			if err := store.MarkInjectionFailureCleared(ctx, acct.ID); err != nil {
				t.Fatalf("clear injection failure: %v", err)
			}
			after := mustGet(t, store, acct.ID)
			cleared := before.Health != after.Health || before.LastTestError != after.LastTestError
			if cleared != tc.want {
				t.Errorf("MarkInjectionFailureCleared changed the row = %v, want %v (predicate said %v)",
					cleared, tc.want, tc.want)
			}
		})
	}

	if IsSelfClearingInjectionFailure(nil) {
		t.Error("IsSelfClearingInjectionFailure(nil) = true, want false")
	}
}

// --- BOS-1141: durable, redacted credential-verification state --------------

func TestAccountStore_AuthCheckColumnsDefault(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "fresh"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, got := range []*models.AgentAccount{acct, mustGet(t, store, acct.ID)} {
		if got.AuthCheck.CheckedAt != nil {
			t.Errorf("auth_checked_at = %v, want nil on create", got.AuthCheck.CheckedAt)
		}
		if got.AuthCheck.Outcome != models.AuthCheckOutcomeUnknown {
			t.Errorf("auth_check_outcome = %q, want empty on create", got.AuthCheck.Outcome)
		}
		if got.AuthCheck.FailureClass != "" {
			t.Errorf("auth_check_failure_class = %q, want empty on create", got.AuthCheck.FailureClass)
		}
		if got.AuthCheck.NextRetryAt != nil {
			t.Errorf("auth_check_next_retry_at = %v, want nil on create", got.AuthCheck.NextRetryAt)
		}
		if got.AuthCheck.IsAuthInvalid() {
			t.Error("a never-checked account must not read as auth-invalid")
		}
	}
}

func TestAccountStore_RecordAuthCheckRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "verify"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	checkedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	retryAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Millisecond)

	cases := []struct {
		name  string
		check models.AuthCheck
	}{
		{"auth_invalid", models.AuthCheck{
			CheckedAt:    ptrTime(checkedAt),
			Outcome:      models.AuthCheckOutcomeAuthInvalid,
			FailureClass: "auth_invalidated",
		}},
		{"transient_with_retry", models.AuthCheck{
			CheckedAt:    ptrTime(checkedAt),
			Outcome:      models.AuthCheckOutcomeTransient,
			FailureClass: "transient_provider",
			NextRetryAt:  ptrTime(retryAt),
		}},
		{"healthy_clears", models.AuthCheck{
			CheckedAt: ptrTime(checkedAt),
			Outcome:   models.AuthCheckOutcomeHealthy,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.RecordAuthCheck(ctx, acct.ID, tc.check); err != nil {
				t.Fatalf("record: %v", err)
			}
			got := mustGet(t, store, acct.ID)
			if got.AuthCheck.Outcome != tc.check.Outcome {
				t.Errorf("outcome = %q, want %q", got.AuthCheck.Outcome, tc.check.Outcome)
			}
			if got.AuthCheck.FailureClass != tc.check.FailureClass {
				t.Errorf("failure_class = %q, want %q", got.AuthCheck.FailureClass, tc.check.FailureClass)
			}
			if got.AuthCheck.CheckedAt == nil || !got.AuthCheck.CheckedAt.Equal(checkedAt) {
				t.Errorf("checked_at = %v, want %v", got.AuthCheck.CheckedAt, checkedAt)
			}
			if tc.check.NextRetryAt == nil {
				if got.AuthCheck.NextRetryAt != nil {
					t.Errorf("next_retry_at = %v, want nil", got.AuthCheck.NextRetryAt)
				}
			} else if got.AuthCheck.NextRetryAt == nil || !got.AuthCheck.NextRetryAt.Equal(retryAt) {
				t.Errorf("next_retry_at = %v, want %v", got.AuthCheck.NextRetryAt, retryAt)
			}
			if got.AuthCheck.IsAuthInvalid() != (tc.check.Outcome == models.AuthCheckOutcomeAuthInvalid) {
				t.Errorf("IsAuthInvalid = %v for outcome %q", got.AuthCheck.IsAuthInvalid(), tc.check.Outcome)
			}
		})
	}
}

func TestAccountStore_RecordAuthCheckDefaultsCheckedAt(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "default-time"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.RecordAuthCheck(ctx, acct.ID, models.AuthCheck{Outcome: models.AuthCheckOutcomeHealthy}); err != nil {
		t.Fatalf("record: %v", err)
	}
	got := mustGet(t, store, acct.ID)
	if got.AuthCheck.CheckedAt == nil {
		t.Fatal("checked_at = nil, want the write time")
	}
	if d := time.Since(*got.AuthCheck.CheckedAt); d < 0 || d > time.Minute {
		t.Errorf("checked_at = %v, want approximately now", got.AuthCheck.CheckedAt)
	}
}

func TestAccountStore_RecordAuthCheckUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)

	if err := store.RecordAuthCheck(context.Background(), "nope", models.AuthCheck{}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("record auth check unknown: got %v, want domain.ErrNotFound", err)
	}
}

// TestAccountStore_MarkAuthCheckClearedResetsDurableState covers the credential-
// replacement path: clearing withdraws the whole auth-check record so a
// replaced credential is neither benched by a verdict about bytes that are no
// longer stored, nor left waiting for a staleness window to expire.
func TestAccountStore_MarkAuthCheckClearedResetsDurableState(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "verify"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	checkedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	retryAt := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Millisecond)
	if err := store.RecordAuthCheck(ctx, acct.ID, models.AuthCheck{
		CheckedAt:    ptrTime(checkedAt),
		Outcome:      models.AuthCheckOutcomeAuthInvalid,
		FailureClass: "auth_invalidated",
		NextRetryAt:  ptrTime(retryAt),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if !mustGet(t, store, acct.ID).AuthCheck.IsAuthInvalid() {
		t.Fatal("precondition: account should be auth-invalid before the clear")
	}

	if err := store.MarkAuthCheckCleared(ctx, acct.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got := mustGet(t, store, acct.ID)
	// checked_at must be NULL, not "now". A cleared row has to be immediately
	// due for verification; stamping the current time would satisfy the
	// maintainer's staleness gate and leave the replacement credential
	// unverified for a full interval — the exact bench this clear removes.
	if got.AuthCheck.CheckedAt != nil {
		t.Errorf("checked_at = %v, want nil so the account is immediately due", got.AuthCheck.CheckedAt)
	}
	if got.AuthCheck.Outcome != "" {
		t.Errorf("outcome = %q, want empty", got.AuthCheck.Outcome)
	}
	if got.AuthCheck.FailureClass != "" {
		t.Errorf("failure_class = %q, want empty", got.AuthCheck.FailureClass)
	}
	if got.AuthCheck.NextRetryAt != nil {
		t.Errorf("next_retry_at = %v, want nil", got.AuthCheck.NextRetryAt)
	}
	if got.AuthCheck.IsAuthInvalid() {
		t.Error("IsAuthInvalid = true after clear; the account must be selectable again")
	}
}

// TestAccountStore_MarkAuthCheckClearedPreservesTestAndUsage holds the clear to the
// same no-cross-contamination rule RecordAuthCheck obeys: it owns the four
// auth_check_* columns and nothing else.
func TestAccountStore_MarkAuthCheckClearedPreservesTestAndUsage(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "verify"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	okAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Millisecond)
	if err := store.RecordTestResult(ctx, acct.ID, &okAt, ""); err != nil {
		t.Fatalf("record test result: %v", err)
	}
	fetchedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	if err := store.RecordUsageProbe(ctx, acct.ID, models.UsageSnapshot{
		Status:    "active",
		PlanTier:  "pro",
		FetchedAt: &fetchedAt,
	}); err != nil {
		t.Fatalf("record usage probe: %v", err)
	}
	if err := store.RecordAuthCheck(ctx, acct.ID, models.AuthCheck{
		CheckedAt: ptrTime(time.Now().UTC()),
		Outcome:   models.AuthCheckOutcomeAuthInvalid,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := store.MarkAuthCheckCleared(ctx, acct.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}

	got := mustGet(t, store, acct.ID)
	if got.LastTestOkAt == nil || !got.LastTestOkAt.Equal(okAt) {
		t.Errorf("last_test_ok_at = %v, want %v (clear must not touch manual test state)", got.LastTestOkAt, okAt)
	}
	if got.Usage.Status != "active" || got.Usage.PlanTier != "pro" {
		t.Errorf("usage snapshot disturbed by clear: %+v", got.Usage)
	}
}

func TestAccountStore_MarkAuthCheckClearedUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)

	if err := store.MarkAuthCheckCleared(context.Background(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("clear auth check unknown: got %v, want domain.ErrNotFound", err)
	}
}

// TestAccountStore_AuthCheckDoesNotContaminateTestOrUsage is the plan's
// no-cross-contamination requirement in both directions: a verification write
// must not disturb manual TestAgentAccount bookkeeping or the usage snapshot, and
// neither of those may disturb the auth-check columns.
func TestAccountStore_AuthCheckDoesNotContaminateTestOrUsage(t *testing.T) {
	db := setupTestDB(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "isolation"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	okAt := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Millisecond)
	fetchedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	snap := models.UsageSnapshot{Util5h: 0.5, Util7d: 0.25, Status: "ok", PlanTier: "pro", FetchedAt: &fetchedAt}

	if err := store.RecordTestResult(ctx, acct.ID, ptrTime(okAt), ""); err != nil {
		t.Fatalf("record test result: %v", err)
	}
	if err := store.RecordUsageProbe(ctx, acct.ID, snap); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	// A verification write leaves the manual-test and usage fields alone.
	checkedAt := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.RecordAuthCheck(ctx, acct.ID, models.AuthCheck{
		CheckedAt:    ptrTime(checkedAt),
		Outcome:      models.AuthCheckOutcomeAuthInvalid,
		FailureClass: "auth_invalidated",
	}); err != nil {
		t.Fatalf("record auth check: %v", err)
	}
	got := mustGet(t, store, acct.ID)
	if got.LastTestOkAt == nil || !got.LastTestOkAt.Equal(okAt) {
		t.Errorf("auth check clobbered last_test_ok_at: %v, want %v", got.LastTestOkAt, okAt)
	}
	if got.LastTestError != "" {
		t.Errorf("auth check clobbered last_test_error: %q", got.LastTestError)
	}
	if got.Usage == nil || got.Usage.Util5h != snap.Util5h || got.Usage.Status != snap.Status {
		t.Errorf("auth check clobbered usage snapshot: %#v", got.Usage)
	}
	if got.Health != models.AgentAccountHealthOK {
		t.Errorf("auth check changed health to %q; verification must not bench an account through health", got.Health)
	}

	// Manual-test and usage writes leave the verification record alone.
	if err := store.RecordTestResult(ctx, acct.ID, nil, "manual smoke failed"); err != nil {
		t.Fatalf("record test result 2: %v", err)
	}
	if err := store.RecordUsageProbe(ctx, acct.ID, models.UsageSnapshot{Util5h: 0.9, Status: "warning"}); err != nil {
		t.Fatalf("record usage 2: %v", err)
	}
	got = mustGet(t, store, acct.ID)
	if got.AuthCheck.Outcome != models.AuthCheckOutcomeAuthInvalid {
		t.Errorf("usage/test write clobbered auth outcome: %q", got.AuthCheck.Outcome)
	}
	if got.AuthCheck.FailureClass != "auth_invalidated" {
		t.Errorf("usage/test write clobbered failure class: %q", got.AuthCheck.FailureClass)
	}
	if got.AuthCheck.CheckedAt == nil || !got.AuthCheck.CheckedAt.Equal(checkedAt) {
		t.Errorf("usage/test write clobbered checked_at: %v", got.AuthCheck.CheckedAt)
	}
}

func mustGet(t *testing.T, store *SQLiteAgentAccountStore, id string) *models.AgentAccount {
	t.Helper()
	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return got
}

// TestAccountStore_AuthCheckSurvivesMigrationReplay is AC-6's "across
// restart": the daemon re-runs migrations on every boot, so the record must
// still read back after a replay.
func TestAccountStore_AuthCheckSurvivesMigrationReplay(t *testing.T) {
	db := dbtest.NewMigrated(t)
	store := NewAgentAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAgentAccountParams{Provider: models.AgentAccountProviderCodex, Label: "restart"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	checkedAt := time.Now().UTC().Truncate(time.Millisecond)
	retryAt := checkedAt.Add(time.Hour)
	if err := store.RecordAuthCheck(ctx, acct.ID, models.AuthCheck{
		CheckedAt:    ptrTime(checkedAt),
		Outcome:      models.AuthCheckOutcomeTransient,
		FailureClass: "transient_provider",
		NextRetryAt:  ptrTime(retryAt),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := dbtest.Run(db, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	got := mustGet(t, store, acct.ID)
	if got.AuthCheck.Outcome != models.AuthCheckOutcomeTransient ||
		got.AuthCheck.FailureClass != "transient_provider" ||
		got.AuthCheck.CheckedAt == nil || !got.AuthCheck.CheckedAt.Equal(checkedAt) ||
		got.AuthCheck.NextRetryAt == nil || !got.AuthCheck.NextRetryAt.Equal(retryAt) {
		t.Fatalf("auth check did not survive migration replay: %+v", got.AuthCheck)
	}
}
