package rotation

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/recurser/bossalib/models"
)

// fakeStore is an in-memory AccountRepository used by the rotation engine tests.
// It mimics SQLite's single-writer semantics: DecideTx holds a mutex for the
// whole closure, applies writes to the backing accounts in place, and rolls
// back (restores a snapshot) when the closure returns an error or when a crash
// is simulated. A write counter records cooldown/health changes so tests can
// assert idempotency ("exactly one cooldown write").
type fakeStore struct {
	mu       sync.Mutex
	accounts []*models.Account
	writes   int  // number of cooldown/health writes actually applied+committed
	txCalls  int  // number of DecideTx invocations (for the capability test)
	crash    bool // when true, DecideTx runs fn then rolls back with an error
}

func newFakeStore(accts ...*models.Account) *fakeStore {
	return &fakeStore{accounts: accts}
}

// DecideTx serializes on the store mutex, runs fn against the live accounts,
// and commits on success. On fn error (or a simulated crash) it restores the
// pre-transaction snapshot so no cooldown/health change is persisted.
func (s *fakeStore) DecideTx(ctx context.Context, provider string, fn func(tx TxAccountView) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txCalls++

	snapAccounts := make([]models.Account, len(s.accounts))
	for i, a := range s.accounts {
		snapAccounts[i] = *a
	}
	snapWrites := s.writes

	rollback := func() {
		for i := range s.accounts {
			*s.accounts[i] = snapAccounts[i]
		}
		s.writes = snapWrites
	}

	tx := &fakeTx{store: s, provider: provider}
	if err := fn(tx); err != nil {
		rollback()
		return err
	}
	if s.crash {
		rollback()
		return fmt.Errorf("simulated crash before commit")
	}
	return nil
}

// fakeTx is the transactional view. It mutates the live accounts so reads within
// the transaction observe its own writes (like a real SQLite transaction).
type fakeTx struct {
	store    *fakeStore
	provider string
}

func (tx *fakeTx) find(id string) *models.Account {
	for _, a := range tx.store.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (tx *fakeTx) ListByProvider(ctx context.Context) ([]*models.Account, error) {
	var out []*models.Account
	for _, a := range tx.store.accounts {
		if a.Provider == models.AccountProvider(tx.provider) {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i], out[j]
		if ai.Priority != aj.Priority {
			return ai.Priority < aj.Priority
		}
		ri, rj := healthRankFake(ai.Health), healthRankFake(aj.Health)
		if ri != rj {
			return ri < rj
		}
		if lastUsedLess(ai.LastUsedAt, aj.LastUsedAt) {
			return true
		}
		if lastUsedLess(aj.LastUsedAt, ai.LastUsedAt) {
			return false
		}
		return ai.ID < aj.ID // final deterministic tiebreak, mirrors the SQL ORDER BY
	})
	return out, nil
}

func (tx *fakeTx) SetCooldownIfNotCooling(ctx context.Context, accountID string, until, now time.Time) (bool, error) {
	a := tx.find(accountID)
	if a == nil {
		return false, fmt.Errorf("account %q not found", accountID)
	}
	if a.CooldownUntil != nil && a.CooldownUntil.After(now) {
		return false, nil // already cooling — idempotency gate
	}
	u := until
	a.CooldownUntil = &u
	tx.store.writes++
	return true, nil
}

func (tx *fakeTx) FailHealth(ctx context.Context, accountID string) error {
	a := tx.find(accountID)
	if a == nil {
		return fmt.Errorf("account %q not found", accountID)
	}
	if a.Health != models.AccountHealthFailed {
		a.Health = models.AccountHealthFailed
		tx.store.writes++
	}
	return nil
}

func healthRankFake(h models.AccountHealth) int {
	if h == models.AccountHealthOK {
		return 0
	}
	return 1
}

// lastUsedLess orders by LastUsedAt ascending with NULLS FIRST.
func lastUsedLess(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil:
		return true
	case b == nil:
		return false
	default:
		return a.Before(*b)
	}
}
