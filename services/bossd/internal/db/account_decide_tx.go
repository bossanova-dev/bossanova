package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/rotation"
)

// SQLiteAccountStore is the real SQLite implementation of the rotation port.
var _ rotation.AccountRepository = (*SQLiteAccountStore)(nil)

// isoMillisLayout is the fixed-width UTC ISO layout the account store writes for
// nullable time columns. Fixed width keeps lexical <= comparison correct against
// the guard value in SetCooldownIfNotCooling.
const isoMillisLayout = "2006-01-02T15:04:05.000Z"

// DecideTx runs fn inside a single transaction, giving it a tx-scoped view and
// guarded writer over the accounts for provider. The transaction commits only
// when fn returns nil; any error (or a crash before Commit) rolls back, so no
// cooldown/health change is persisted. This is the atomicity contract the
// rotation engine relies on.
func (s *SQLiteAccountStore) DecideTx(ctx context.Context, provider string, fn func(tx rotation.TxAccountView) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rotation tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	view := &txAccountView{tx: tx, provider: provider}
	if err := fn(view); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rotation tx: %w", err)
	}
	return nil
}

// txAccountView is the transactional view handed to the decide closure. It reads
// and writes account rows through the same *sql.Tx so the engine's decision and
// its cooldown/health write commit atomically.
type txAccountView struct {
	tx       *sql.Tx
	provider string
}

var _ rotation.TxAccountView = (*txAccountView)(nil)

// ListByProvider returns all accounts for the view's provider (any
// health/status), ordered so the FIRST healthy row is the rotation winner:
// priority ASC -> health-rank (ok before failed) -> weekly-expiry rank -> LRU
// (NULL last_used_at first) -> id ASC. The weekly-expiry rank (BOS-429) sorts
// accounts whose usage_reset_7d is a known instant strictly after now first,
// soonest-reset first; accounts with a NULL reset (never probed) or a past
// reset (window already rolled — a fresh full week, not urgent) tie on that key
// and fall through to LRU. This mirrors FutureWeeklyReset's future-only rule so
// this SQL surface, the bind-time comparator (account.moreEligible), and the
// rotation fake never drift. now is bound in the store's fixed-width
// isoMillisLayout so the lexical `>` comparison matches chronological order. The
// trailing id tiebreak makes selection reproducible across restarts (and matches
// the fake store) when rows are otherwise fully tied.
func (v *txAccountView) ListByProvider(ctx context.Context, now time.Time) ([]*models.Account, error) {
	nowISO := now.UTC().Format(isoMillisLayout)
	rows, err := v.tx.QueryContext(ctx,
		accountSelectSQL+` WHERE provider = ?
		ORDER BY priority ASC,
		         CASE health WHEN 'ok' THEN 0 ELSE 1 END ASC,
		         CASE WHEN usage_reset_7d IS NOT NULL AND usage_reset_7d > ? THEN 0 ELSE 1 END ASC,
		         CASE WHEN usage_reset_7d IS NOT NULL AND usage_reset_7d > ? THEN usage_reset_7d END ASC,
		         (last_used_at IS NULL) DESC,
		         last_used_at ASC,
		         id ASC`,
		v.provider, nowISO, nowISO)
	if err != nil {
		return nil, fmt.Errorf("list accounts by provider (rotation): %w", err)
	}
	return collectAccounts(rows)
}

// SetCooldownIfNotCooling sets cooldown_until on accountID ONLY when the row is
// not already cooling (cooldown_until IS NULL OR <= now). Returns applied=false
// when the row was already cooling — the idempotency gate, not an error. The
// write is scoped to the view's provider (id AND provider), matching the
// provider-scoped read, so a mismatched signal can never cool another
// provider's account. until and the now guard are written in the same
// fixed-width ISO layout the store uses so the lexical <= comparison is correct.
func (v *txAccountView) SetCooldownIfNotCooling(ctx context.Context, accountID string, until, now time.Time) (bool, error) {
	res, err := v.tx.ExecContext(ctx,
		`UPDATE accounts SET cooldown_until = ?, updated_at = ?
		 WHERE id = ? AND provider = ? AND (cooldown_until IS NULL OR cooldown_until <= ?)`,
		until.UTC().Format(isoMillisLayout), sqlutil.TimeNow(), accountID, v.provider, now.UTC().Format(isoMillisLayout))
	if err != nil {
		return false, fmt.Errorf("set cooldown if not cooling: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set cooldown rows affected: %w", err)
	}
	return n > 0, nil
}

// FailHealth sets health=failed on accountID (the persistent auth-invalidated
// sink), scoped to the view's provider (id AND provider). A no-match row is fine
// (returns nil): the engine only calls this for a known capped account.
func (v *txAccountView) FailHealth(ctx context.Context, accountID string) error {
	_, err := v.tx.ExecContext(ctx,
		`UPDATE accounts SET health = ?, updated_at = ? WHERE id = ? AND provider = ?`,
		string(models.AccountHealthFailed), sqlutil.TimeNow(), accountID, v.provider)
	if err != nil {
		return fmt.Errorf("fail account health: %w", err)
	}
	return nil
}
