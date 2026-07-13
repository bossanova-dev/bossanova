package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
)

func TestAccountStore_CreateGetDefaults(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{
		Provider:      models.AccountProviderClaude,
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
	if acct.Status != models.AccountStatusActive {
		t.Errorf("status = %q, want %q", acct.Status, models.AccountStatusActive)
	}
	if acct.Health != models.AccountHealthOK {
		t.Errorf("health = %q, want %q", acct.Health, models.AccountHealthOK)
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
	if got.Provider != models.AccountProviderClaude {
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
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{
		Provider: models.AccountProviderCodex,
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
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{
		Provider: models.AccountProviderClaude,
		Label:    "upd",
		Priority: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	newLabel := "renamed"
	newStatus := models.AccountStatusDisabled
	newPriority := 9
	newHealth := models.AccountHealthFailed
	newTier := "pro"
	newModels := []string{"gpt-5"}
	cooldown := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	lastUsed := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)

	updated, err := store.Update(ctx, acct.ID, UpdateAccountParams{
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
	cleared, err := store.Update(ctx, acct.ID, UpdateAccountParams{
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
	store := NewAccountStore(db)
	ctx := context.Background()

	label := "x"
	if _, err := store.Update(ctx, "does-not-exist", UpdateAccountParams{Label: &label}); err != sql.ErrNoRows {
		t.Errorf("update unknown: got %v, want sql.ErrNoRows", err)
	}
}

func TestAccountStore_CooldownRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{
		Provider: models.AccountProviderClaude,
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
			got, err := store.Update(ctx, acct.ID, UpdateAccountParams{
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
	store := NewAccountStore(db)
	ctx := context.Background()

	// Deliberately create out of priority order and across providers.
	claudeB, _ := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "claude-b", Priority: 10})
	claudeA, _ := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "claude-a", Priority: 1})
	codexA, _ := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderCodex, Label: "codex-a", Priority: 3})

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

	claudeOnly, err := store.ListByProvider(ctx, models.AccountProviderClaude)
	if err != nil {
		t.Fatalf("list by provider: %v", err)
	}
	if len(claudeOnly) != 2 {
		t.Fatalf("claude len = %d, want 2", len(claudeOnly))
	}
	for _, a := range claudeOnly {
		if a.Provider != models.AccountProviderClaude {
			t.Errorf("provider filter leaked %q", a.Provider)
		}
	}
	if claudeOnly[0].ID != claudeA.ID || claudeOnly[1].ID != claudeB.ID {
		t.Errorf("claude order = [%q %q], want [%q %q]", claudeOnly[0].ID, claudeOnly[1].ID, claudeA.ID, claudeB.ID)
	}

	codexOnly, _ := store.ListByProvider(ctx, models.AccountProviderCodex)
	if len(codexOnly) != 1 || codexOnly[0].ID != codexA.ID {
		t.Errorf("codex filter = %v, want [%q]", codexOnly, codexA.ID)
	}
}

func TestAccountStore_UniqueProviderLabel(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	if _, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "dup"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "dup"})
	if err == nil {
		t.Error("expected UNIQUE(provider,label) violation for duplicate claude/dup")
	} else if !errors.Is(err, ErrAccountExists) {
		t.Errorf("duplicate create should wrap ErrAccountExists, got: %v", err)
	}
	// Same label under a different provider is allowed.
	if _, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderCodex, Label: "dup"}); err != nil {
		t.Errorf("same label under other provider should succeed: %v", err)
	}
}

func TestAccountStore_Delete(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "del"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Delete(ctx, acct.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, acct.ID); err != sql.ErrNoRows {
		t.Errorf("get after delete: got %v, want sql.ErrNoRows", err)
	}
	if err := store.Delete(ctx, acct.ID); err != sql.ErrNoRows {
		t.Errorf("second delete: got %v, want sql.ErrNoRows", err)
	}
}

// TestAccountsMigrationIdempotent asserts the accounts table exists after
// setupTestDB and that re-running the migration set is a no-op (goose version
// tracking), after which the table is still writable.
func TestAccountsMigrationIdempotent(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts").Scan(&count); err != nil {
		t.Fatalf("accounts table should exist: %v", err)
	}

	// Re-run migrations; goose version tracking makes this a no-op.
	if err := migrate.Run(db, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("second migration run should be a no-op: %v", err)
	}

	store := NewAccountStore(db)
	if _, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "post-migrate"}); err != nil {
		t.Fatalf("insert after second migration: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts").Scan(&count); err != nil {
		t.Fatalf("select after second migration: %v", err)
	}
	if count != 1 {
		t.Errorf("accounts count = %d, want 1", count)
	}
}

// TestAccountStore_TestResultColumnsDefault asserts the BOS-160 columns default
// to "never tested" on create: last_test_ok_at NULL, last_test_error "".
func TestAccountStore_TestResultColumnsDefault(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "fresh"})
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
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "rec"})
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
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "susp"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed a passing test so we can prove suspension clears last_test_ok_at.
	okAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	if err := store.RecordTestResult(ctx, acct.ID, ptrTime(okAt), ""); err != nil {
		t.Fatalf("seed test result: %v", err)
	}

	const reason = "account suspended: organization disabled Claude subscription access"
	if err := store.MarkAccountSuspended(ctx, acct.ID, reason); err != nil {
		t.Fatalf("mark suspended: %v", err)
	}

	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Health != models.AccountHealthFailed {
		t.Errorf("health = %q, want %q", got.Health, models.AccountHealthFailed)
	}
	if got.LastTestError != reason {
		t.Errorf("last_test_error = %q, want %q", got.LastTestError, reason)
	}
	if got.LastTestOkAt != nil {
		t.Errorf("last_test_ok_at = %v, want nil (cleared)", got.LastTestOkAt)
	}
	// Status is left untouched — an operator re-enables via a separate action.
	if got.Status != models.AccountStatusActive {
		t.Errorf("status = %q, want unchanged active", got.Status)
	}
}

func TestAccountStore_MarkAccountSuspendedUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	if err := store.MarkAccountSuspended(ctx, "nope", "x"); err != sql.ErrNoRows {
		t.Errorf("mark unknown: got %v, want sql.ErrNoRows", err)
	}
}

func TestAccountStore_RecordTestResultUnknownID(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	if err := store.RecordTestResult(ctx, "nope", nil, "x"); err != sql.ErrNoRows {
		t.Errorf("record unknown: got %v, want sql.ErrNoRows", err)
	}
}

func TestAccountStore_RecordUsageProbeRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "usage"})
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

	empty, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderCodex, Label: "never-probed"})
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
	store := NewAccountStore(db)
	ctx := context.Background()

	if err := store.RecordUsageProbe(ctx, "nope", models.UsageSnapshot{}); err != sql.ErrNoRows {
		t.Errorf("record usage unknown: got %v, want sql.ErrNoRows", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
