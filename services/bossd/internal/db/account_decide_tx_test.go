package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
)

// TestDecideTx_CommitPersists verifies a DecideTx that applies a cooldown and
// returns nil commits: the row reloaded via Get shows the cooldown and the
// write reports applied==true.
func TestDecideTx_CommitPersists(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "c1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	until := now.Add(time.Hour)

	var applied bool
	err = store.DecideTx(ctx, string(models.AccountProviderClaude), func(tx rotation.TxAccountView) error {
		var e error
		applied, e = tx.SetCooldownIfNotCooling(ctx, acct.ID, until, now)
		return e
	})
	if err != nil {
		t.Fatalf("DecideTx: %v", err)
	}
	if !applied {
		t.Errorf("applied = false, want true on first cooldown")
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CooldownUntil == nil || !got.CooldownUntil.Equal(until) {
		t.Errorf("cooldown_until = %v, want %v (committed)", got.CooldownUntil, until)
	}
}

// TestDecideTx_GuardBlocksSecondCool verifies the idempotency gate: a second
// SetCooldownIfNotCooling on an already-cooling row returns applied==false and
// does not change the stored cooldown_until.
func TestDecideTx_GuardBlocksSecondCool(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "c1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	first := now.Add(time.Hour)

	if err := store.DecideTx(ctx, string(models.AccountProviderClaude), func(tx rotation.TxAccountView) error {
		applied, e := tx.SetCooldownIfNotCooling(ctx, acct.ID, first, now)
		if e != nil {
			return e
		}
		if !applied {
			t.Errorf("first applied = false, want true")
		}
		return nil
	}); err != nil {
		t.Fatalf("first DecideTx: %v", err)
	}

	second := now.Add(2 * time.Hour)
	if err := store.DecideTx(ctx, string(models.AccountProviderClaude), func(tx rotation.TxAccountView) error {
		applied, e := tx.SetCooldownIfNotCooling(ctx, acct.ID, second, now)
		if e != nil {
			return e
		}
		if applied {
			t.Errorf("second applied = true, want false (already cooling)")
		}
		return nil
	}); err != nil {
		t.Fatalf("second DecideTx: %v", err)
	}

	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CooldownUntil == nil || !got.CooldownUntil.Equal(first) {
		t.Errorf("cooldown_until = %v, want unchanged %v", got.CooldownUntil, first)
	}
}

// TestDecideTx_GuardAllowsReCoolWhenExpired verifies the guard re-cools a row
// whose existing cooldown_until is already in the past.
func TestDecideTx_GuardAllowsReCoolWhenExpired(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "c1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed an already-expired cooldown via the normal store update path.
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	if _, err := store.Update(ctx, acct.ID, UpdateAccountParams{CooldownUntil: func() **time.Time { p := &past; return &p }()}); err != nil {
		t.Fatalf("seed expired cooldown: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	fresh := now.Add(time.Hour)
	var applied bool
	if err := store.DecideTx(ctx, string(models.AccountProviderClaude), func(tx rotation.TxAccountView) error {
		var e error
		applied, e = tx.SetCooldownIfNotCooling(ctx, acct.ID, fresh, now)
		return e
	}); err != nil {
		t.Fatalf("DecideTx: %v", err)
	}
	if !applied {
		t.Errorf("applied = false, want true (prior cooldown expired)")
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CooldownUntil == nil || !got.CooldownUntil.Equal(fresh) {
		t.Errorf("cooldown_until = %v, want %v", got.CooldownUntil, fresh)
	}
}

// TestDecideTx_FailHealth verifies FailHealth persists health=failed.
func TestDecideTx_FailHealth(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "c1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.DecideTx(ctx, string(models.AccountProviderClaude), func(tx rotation.TxAccountView) error {
		return tx.FailHealth(ctx, acct.ID)
	}); err != nil {
		t.Fatalf("DecideTx: %v", err)
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Health != models.AccountHealthFailed {
		t.Errorf("health = %q, want %q", got.Health, models.AccountHealthFailed)
	}
}

// TestDecideTx_Rollback verifies an fn that returns an error rolls back: the
// cooldown it wrote before returning does NOT persist.
func TestDecideTx_Rollback(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	acct, err := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "c1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	until := now.Add(time.Hour)

	sentinel := errors.New("boom")
	err = store.DecideTx(ctx, string(models.AccountProviderClaude), func(tx rotation.TxAccountView) error {
		if _, e := tx.SetCooldownIfNotCooling(ctx, acct.ID, until, now); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("DecideTx err = %v, want sentinel propagated", err)
	}
	got, err := store.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CooldownUntil != nil {
		t.Errorf("cooldown_until = %v, want nil (rolled back)", got.CooldownUntil)
	}
}

// TestDecideTx_NoMatchIsSilent locks the intentional real-adapter behavior for
// a capped account that no longer exists (e.g. deleted mid-flight): the guarded
// cooldown write reports applied==false with no error (RowsAffected 0), and
// FailHealth is a no-op nil. This is deliberately looser than the in-memory
// fake (which errors on a missing id) — the engine's caller trusts the signal's
// account id, and rotating away from a vanished account is the desired outcome.
func TestDecideTx_NoMatchIsSilent(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if err := store.DecideTx(ctx, string(models.AccountProviderClaude), func(tx rotation.TxAccountView) error {
		applied, e := tx.SetCooldownIfNotCooling(ctx, "does-not-exist", now.Add(time.Hour), now)
		if e != nil {
			return e
		}
		if applied {
			t.Errorf("SetCooldownIfNotCooling applied = true, want false for unknown id")
		}
		if e := tx.FailHealth(ctx, "does-not-exist"); e != nil {
			t.Errorf("FailHealth on unknown id = %v, want nil (no-op)", e)
		}
		return nil
	}); err != nil {
		t.Fatalf("DecideTx: %v", err)
	}
}

// TestDecideTx_ListByProviderOrdering verifies the rotation ordering
// (priority ASC -> ok-before-failed -> LRU with NULL last_used_at first) and
// that only the requested provider's rows are returned.
func TestDecideTx_ListByProviderOrdering(t *testing.T) {
	db := setupTestDB(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	setLastUsed := func(id string, t2 time.Time) {
		v := t2
		if _, err := store.Update(ctx, id, UpdateAccountParams{LastUsedAt: func() **time.Time { p := &v; return &p }()}); err != nil {
			t.Fatalf("set last_used_at: %v", err)
		}
	}
	setFailed := func(id string) {
		h := models.AccountHealthFailed
		if _, err := store.Update(ctx, id, UpdateAccountParams{Health: &h}); err != nil {
			t.Fatalf("set failed: %v", err)
		}
	}

	base := time.Now().UTC().Truncate(time.Millisecond)

	// priority 1, ok, last_used_at recent.
	p1recent, _ := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "p1-recent", Priority: 1})
	setLastUsed(p1recent.ID, base.Add(-time.Minute))
	// priority 1, ok, last_used_at NULL -> should sort BEFORE p1recent (LRU first).
	p1null, _ := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "p1-null", Priority: 1})
	// priority 2, ok.
	p2ok, _ := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "p2-ok", Priority: 2})
	// priority 1, failed -> ok-before-failed pushes it after the ok priority-1 rows.
	p1failed, _ := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderClaude, Label: "p1-failed", Priority: 1})
	setFailed(p1failed.ID)
	// Different provider row must not appear.
	codex, _ := store.Create(ctx, CreateAccountParams{Provider: models.AccountProviderCodex, Label: "codex", Priority: 1})

	var order []string
	if err := store.DecideTx(ctx, string(models.AccountProviderClaude), func(tx rotation.TxAccountView) error {
		list, e := tx.ListByProvider(ctx)
		if e != nil {
			return e
		}
		for _, a := range list {
			if a.Provider != models.AccountProviderClaude {
				t.Errorf("provider filter leaked %q (%s)", a.Provider, a.Label)
			}
			order = append(order, a.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("DecideTx: %v", err)
	}

	// Expected: p1null (NULL LRU first), p1recent, p1failed (failed after ok at
	// same priority), p2ok (higher priority number last).
	want := []string{p1null.ID, p1recent.ID, p1failed.ID, p2ok.ID}
	if len(order) != len(want) {
		t.Fatalf("order len = %d, want %d (%v)", len(order), len(want), order)
	}
	for i, id := range want {
		if order[i] != id {
			t.Errorf("order[%d] = %q, want %q", i, order[i], id)
		}
	}
	if codex.Provider != models.AccountProviderCodex {
		t.Fatalf("codex seed provider = %q", codex.Provider)
	}
}
